package data

import (
	"bufio"
	"encoding/binary"
	"io"
	"os"
	"sync"

	"github.com/ncruces/go-sqlite3"
)

const spoolMaxCellBytes = 4096

// ResultSpool streams arbitrarily large SQL query result sets into a compact
// temporary binary file on disk with an in-memory row-offset index and a tiny
// LRU page cache in RAM. It implements a random-access data source without
// buffering the full result set in memory.
type ResultSpool struct {
	cols     []Column
	path     string
	file     *os.File
	bw       *bufio.Writer
	written  int64
	offsets  []int64
	lenBuf   [6]byte // kind (1) + trunc (1) + payload len (4)
	closed   bool
	finished bool

	mu         sync.Mutex
	pages      map[int64]*page
	cacheBytes int
	budget     int
	tick       uint64
}

// NewResultSpool creates a temp-file-backed ResultSpool for the given columns.
// Call Finish once all rows have been appended, and Close when the spool is no
// longer needed to remove the temporary file.
func NewResultSpool(cols []Column) (*ResultSpool, error) {
	f, err := os.CreateTemp("", "sqlstrata-spool-*.bin")
	if err != nil {
		return nil, err
	}
	return &ResultSpool{
		cols:   cols,
		path:   f.Name(),
		file:   f,
		bw:     bufio.NewWriterSize(f, 64*1024),
		pages:  map[int64]*page{},
		budget: 4 << 20, // 4 MB page cache budget for query spool
	}, nil
}

// AppendRowFromStmt reads the current row from an executed sqlite3.Stmt and
// writes it to the temp file without keeping the row in Go heap memory.
func (s *ResultSpool) AppendRowFromStmt(stmt *sqlite3.Stmt) error {
	if s.closed || s.finished {
		return os.ErrClosed
	}
	s.offsets = append(s.offsets, s.written)
	ncols := len(s.cols)
	for i := range ncols {
		var kind Kind
		var trunc bool
		var payload []byte

		switch stmt.ColumnType(i) {
		case sqlite3.NULL:
			kind = KindNull
		case sqlite3.INTEGER:
			kind = KindInteger
			payload = stmt.ColumnRawText(i)
		case sqlite3.FLOAT:
			kind = KindReal
			payload = stmt.ColumnRawText(i)
		case sqlite3.TEXT:
			kind = KindText
			payload = stmt.ColumnRawText(i)
			if len(payload) > spoolMaxCellBytes {
				payload = payload[:spoolMaxCellBytes]
				trunc = true
			}
		case sqlite3.BLOB:
			kind = KindBlob
			payload = []byte("BLOB " + FormatSize(int64(len(stmt.ColumnRawBlob(i)))))
		}
		if err := s.writeCell(kind, trunc, payload); err != nil {
			return err
		}
	}
	return nil
}

// AppendCellBytes writes a single pre-formatted cell. Rows must complete ncols
// cells in order.
func (s *ResultSpool) AppendRowValues(kinds []Kind, vals [][]byte) error {
	if s.closed || s.finished {
		return os.ErrClosed
	}
	s.offsets = append(s.offsets, s.written)
	for i := range s.cols {
		k := kinds[i]
		v := vals[i]
		trunc := false
		if len(v) > spoolMaxCellBytes && k == KindText {
			v = v[:spoolMaxCellBytes]
			trunc = true
		}
		if err := s.writeCell(k, trunc, v); err != nil {
			return err
		}
	}
	return nil
}

func (s *ResultSpool) writeCell(kind Kind, trunc bool, payload []byte) error {
	s.lenBuf[0] = byte(kind)
	if trunc {
		s.lenBuf[1] = 1
	} else {
		s.lenBuf[1] = 0
	}
	n := len(payload)
	binary.LittleEndian.PutUint32(s.lenBuf[2:6], uint32(n))
	if _, err := s.bw.Write(s.lenBuf[:]); err != nil {
		return err
	}
	s.written += 6
	if n > 0 {
		if _, err := s.bw.Write(payload); err != nil {
			return err
		}
		s.written += int64(n)
	}
	return nil
}

// Finish flushes buffered writes to the temp file so random-access reads can begin.
func (s *ResultSpool) Finish() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.finished {
		return nil
	}
	s.finished = true
	return s.bw.Flush()
}

// Close closes and removes the backing temporary file and drops cached pages.
func (s *ResultSpool) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	s.pages = nil
	s.cacheBytes = 0
	err := s.file.Close()
	if remErr := os.Remove(s.path); err == nil && remErr != nil && !os.IsNotExist(remErr) {
		err = remErr
	}
	return err
}

func (s *ResultSpool) Columns() []Column            { return s.cols }
func (s *ResultSpool) RowCount() int64              { return int64(len(s.offsets)) }
func (s *ResultSpool) SetVisible(first, last int64) {}

// Cell returns the value at (row, col), faulting its 256-row page from disk
// on a cache miss and maintaining the LRU budget.
func (s *ResultSpool) Cell(row int64, col int) (string, Kind, bool, bool) {
	if row < 0 || row >= int64(len(s.offsets)) || col < 0 || col >= len(s.cols) {
		return "", KindNull, false, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || !s.finished {
		return "", KindNull, false, false
	}
	pIdx := row / PageRows
	pg := s.pages[pIdx]
	if pg == nil {
		var err error
		pg, err = s.readPageLocked(pIdx)
		if err != nil {
			return "", KindNull, false, false
		}
		s.pages[pIdx] = pg
		s.cacheBytes += pg.size()
		s.evictLocked(pIdx)
	}
	s.tick++
	pg.lastUse = s.tick
	r := int(row % PageRows)
	if r >= pg.nrows || col >= pg.ncols {
		return "", KindNull, false, false
	}
	txt, kind, trunc := pg.text(r, col)
	return txt, kind, trunc, true
}

func (s *ResultSpool) readPageLocked(pIdx int64) (*page, error) {
	firstRow := pIdx * PageRows
	endRow := min(int64(len(s.offsets)), firstRow+PageRows)
	nrows := int(endRow - firstRow)
	ncols := len(s.cols)

	startOff := s.offsets[firstRow]
	var endOff int64
	if endRow < int64(len(s.offsets)) {
		endOff = s.offsets[endRow]
	} else {
		endOff = s.written
	}

	raw := make([]byte, endOff-startOff)
	if _, err := io.NewSectionReader(s.file, startOff, int64(len(raw))).Read(raw); err != nil && err != io.EOF {
		return nil, err
	}

	pg := &page{
		index: pIdx,
		nrows: nrows,
		ncols: ncols,
		buf:   make([]byte, 0, len(raw)),
		cells: make([]cell, 0, nrows*ncols),
	}
	pos := 0
	for r := 0; r < nrows; r++ {
		for c := 0; c < ncols; c++ {
			if pos+6 > len(raw) {
				return pg, io.ErrUnexpectedEOF
			}
			kind := Kind(raw[pos])
			trunc := raw[pos+1] != 0
			n := int(binary.LittleEndian.Uint32(raw[pos+2 : pos+6]))
			pos += 6
			if pos+n > len(raw) {
				return pg, io.ErrUnexpectedEOF
			}
			off := uint32(len(pg.buf))
			pg.buf = append(pg.buf, raw[pos:pos+n]...)
			pos += n
			pg.cells = append(pg.cells, cell{
				off:   off,
				n:     uint32(n),
				kind:  kind,
				trunc: trunc,
			})
		}
	}
	return pg, nil
}

func (s *ResultSpool) evictLocked(keepPage int64) {
	for s.cacheBytes > s.budget && len(s.pages) > 1 {
		var victim *page
		for idx, p := range s.pages {
			if idx == keepPage {
				continue
			}
			if victim == nil || p.lastUse < victim.lastUse {
				victim = p
			}
		}
		if victim == nil {
			return
		}
		delete(s.pages, victim.index)
		s.cacheBytes -= victim.size()
	}
}

