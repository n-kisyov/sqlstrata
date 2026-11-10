package data

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/ncruces/go-sqlite3"

	"github.com/sqlstrata/sqlstrata/internal/edit"
	"github.com/sqlstrata/sqlstrata/internal/engine"
)

// CountState describes how trustworthy RowCount is.
type CountState uint8

const (
	CountUnknown  CountState = iota
	CountEstimate            // from sqlite_stat1 or rowid range
	CountExact
)

// CellDetail holds the full (or large-window) value of an inspected cell.
type CellDetail struct {
	Row        int64
	Rowid      int64
	Offset     int64
	Col        int
	Kind       Kind
	TotalBytes int64
	Data       []byte
	Truncated  bool
}

// TableSource pages through a table or view for the grid.
//
// All exported methods are safe to call from the UI goroutine and never block
// on SQLite: missing pages are requested and the invalidate callback fires once
// they arrive.
type TableSource struct {
	db         *engine.DB
	obj        *engine.Object
	cols       []Column
	from       string // quoted object name
	selectList string
	byRowid    bool
	changes    *edit.ChangeSet
	invalidate func()

	ctx    context.Context
	cancel context.CancelFunc
	wake   chan struct{}

	mu         sync.Mutex
	gen        uint64
	sortCol    int
	sortDesc   bool
	filterRaw  string
	filterSQL  string
	pages      map[int64]*page
	cacheBytes int
	budget     int
	tick       uint64
	requested   map[int64]struct{}
	inflight    int64 // page being fetched, -1 if none
	inflightGen uint64
	fetchStop   context.CancelFunc
	countStop  context.CancelFunc
	visFirst   int64
	visLast    int64
	count      int64
	countState CountState
	anchors    []int64
	anchorsOK  bool
	err        error
}

// SourceOptions configures a TableSource.
type SourceOptions struct {
	// CacheBudget bounds the bytes of row data held in memory.
	CacheBudget int
}

// NewTableSource starts paging obj. invalidate is called (from any goroutine)
// whenever new data is available.
func NewTableSource(db *engine.DB, obj *engine.Object, opt SourceOptions, invalidate func()) *TableSource {
	if opt.CacheBudget <= 0 {
		opt.CacheBudget = 32 << 20
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &TableSource{
		db:         db,
		obj:        obj,
		from:       "main." + engine.QuoteIdent(obj.Name),
		byRowid:    obj.HasRowid(),
		invalidate: invalidate,
		ctx:        ctx,
		cancel:     cancel,
		wake:       make(chan struct{}, 1),
		sortCol:    -1,
		pages:      map[int64]*page{},
		requested:  map[int64]struct{}{},
		inflight:   -1,
		budget:     opt.CacheBudget,
		visLast:    PageRows,
	}
	if s.byRowid && !db.ReadOnly {
		s.changes = edit.NewChangeSet(obj.Name, obj.Columns)
	}
	var sel strings.Builder
	if s.byRowid {
		sel.WriteString("rowid")
	} else {
		sel.WriteString("NULL")
	}
	for _, c := range obj.Columns {
		s.cols = append(s.cols, Column{Name: c.Name, Type: c.Type, PK: c.PK > 0})
		q := engine.QuoteIdent(c.Name)
		// typeof() and length() on blobs read only the record header, so huge
		// values cost almost nothing to display.
		fmt.Fprintf(&sel, ", typeof(%[1]s), CASE typeof(%[1]s) WHEN 'blob' THEN length(%[1]s) WHEN 'text' THEN substr(%[1]s, 1, %[2]d) ELSE %[1]s END", q, PreviewBytes)
	}
	s.selectList = sel.String()

	go s.fetchLoop()
	s.startCount(0)
	return s
}

// Close stops background work and releases cached pages.
func (s *TableSource) Close() {
	s.cancel()
	s.mu.Lock()
	if s.countStop != nil {
		s.countStop()
	}
	s.pages = map[int64]*page{}
	s.cacheBytes = 0
	s.mu.Unlock()
}

// Object returns the schema object being paged.
func (s *TableSource) Object() *engine.Object { return s.obj }

// Columns returns the column descriptions.
func (s *TableSource) Columns() []Column { return s.cols }

// Editable reports whether staged row editing is supported on this source.
func (s *TableSource) Editable() bool { return s.changes != nil }

// Changes returns the staged ChangeSet, or nil if the source is read-only / a view.
func (s *TableSource) Changes() *edit.ChangeSet { return s.changes }

// RowCount returns the best known row count (including staged inserted rows).
func (s *TableSource) RowCount() int64 {
	s.mu.Lock()
	base := s.count
	s.mu.Unlock()
	if s.changes != nil {
		base += int64(len(s.changes.Inserts()))
	}
	return base
}

// Count returns the row count and how reliable it is.
func (s *TableSource) Count() (int64, CountState) {
	s.mu.Lock()
	n, st := s.count, s.countState
	s.mu.Unlock()
	if s.changes != nil {
		n += int64(len(s.changes.Inserts()))
	}
	return n, st
}

// Err returns the last fetch error, if any.
func (s *TableSource) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// CacheBytes reports the memory held by cached pages.
func (s *TableSource) CacheBytes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cacheBytes
}

// Sort returns the current sort column (-1 if none) and direction.
func (s *TableSource) Sort() (col int, desc bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sortCol, s.sortDesc
}

// SetSort sets the active sort column (-1 for default order) and reloads pages.
func (s *TableSource) SetSort(col int, desc bool) {
	s.mu.Lock()
	if col < -1 || col >= len(s.cols) {
		col = -1
	}
	if s.sortCol == col && s.sortDesc == desc {
		s.mu.Unlock()
		return
	}
	s.sortCol, s.sortDesc = col, desc
	gen := s.resetPagesLocked(false)
	s.mu.Unlock()
	s.startCount(gen)
	s.invalidate()
}

// Filter returns the raw filter text.
func (s *TableSource) Filter() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.filterRaw
}

// SetFilter applies a filter expression or search query and reloads pages.
func (s *TableSource) SetFilter(raw string) {
	raw = strings.TrimSpace(raw)
	s.mu.Lock()
	if s.filterRaw == raw {
		s.mu.Unlock()
		return
	}
	s.filterRaw = raw
	s.filterSQL = buildFilterSQL(raw, s.obj.Columns)
	gen := s.resetPagesLocked(true)
	s.mu.Unlock()
	s.startCount(gen)
	s.invalidate()
}

// Reload drops cached pages and re-fetches visible data and counts.
func (s *TableSource) Reload() {
	s.mu.Lock()
	gen := s.resetPagesLocked(true)
	s.mu.Unlock()
	s.startCount(gen)
	s.invalidate()
}

// ExportSQL returns the full SELECT statement matching the table's current
// filter and sort order (with un-truncated column values) for streaming export.
func (s *TableSource) ExportSQL() string {
	s.mu.Lock()
	filterSQL := s.filterSQL
	sortCol := s.sortCol
	sortDesc := s.sortDesc
	s.mu.Unlock()

	cols := make([]string, len(s.cols))
	for i, c := range s.cols {
		cols[i] = engine.QuoteIdent(c.Name)
	}
	q := "SELECT " + strings.Join(cols, ", ") + " FROM " + s.from
	if filterSQL != "" {
		q += " WHERE (" + filterSQL + ")"
	}
	if sortCol >= 0 && sortCol < len(s.cols) {
		dir := "ASC"
		if sortDesc {
			dir = "DESC"
		}
		q += " ORDER BY " + engine.QuoteIdent(s.cols[sortCol].Name) + " " + dir
		if s.byRowid {
			q += ", rowid " + dir
		}
	} else if s.byRowid {
		q += " ORDER BY rowid"
	}
	return q
}

func (s *TableSource) resetPagesLocked(resetCount bool) uint64 {
	s.gen++
	s.pages = map[int64]*page{}
	s.requested = map[int64]struct{}{}
	s.cacheBytes = 0
	s.err = nil
	if s.fetchStop != nil {
		s.fetchStop()
		s.fetchStop = nil
	}
	s.inflight = -1
	if resetCount {
		s.countState = CountUnknown
		s.anchors = nil
		s.anchorsOK = false
	}
	for p := s.visFirst / PageRows; p <= s.visLast/PageRows; p++ {
		s.requestLocked(p)
	}
	return s.gen
}

func buildFilterSQL(raw string, cols []engine.Column) string {
	if raw == "" {
		return ""
	}
	// Treat raw as a SQL WHERE predicate when it contains SQL operators/parens or
	// multi-token SQL keyword clauses (so bare search words like "in" or "or"
	// still perform a substring search).
	up := " " + strings.ToUpper(raw) + " "
	hasOp := strings.ContainsAny(raw, "=<>!()")
	hasKeywordClause := len(strings.Fields(raw)) >= 2 && (strings.Contains(up, " LIKE ") ||
		strings.Contains(up, " IS ") ||
		strings.Contains(up, " IN ") ||
		strings.Contains(up, " AND ") ||
		strings.Contains(up, " OR ") ||
		strings.Contains(up, " BETWEEN ") ||
		strings.Contains(up, " GLOB "))
	if hasOp || hasKeywordClause {
		return raw
	}
	// Otherwise treat raw as a substring search across all non-blob columns.
	escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(raw)
	pat := sqlite3.Quote("%" + escaped + "%")
	var terms []string
	for _, c := range cols {
		if strings.Contains(strings.ToUpper(c.Type), "BLOB") {
			continue
		}
		terms = append(terms, fmt.Sprintf("CAST(%s AS TEXT) LIKE %s ESCAPE '\\'", engine.QuoteIdent(c.Name), pat))
	}
	if len(terms) == 0 {
		return ""
	}
	return strings.Join(terms, " OR ")
}

// SetVisible tells the source which rows are on screen, so it can prioritize
// and prefetch pages and drop requests that scrolled away.
func (s *TableSource) SetVisible(first, last int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if first == s.visFirst && last == s.visLast {
		return
	}
	s.visFirst, s.visLast = first, last
	lo, hi := s.window()
	for p := range s.requested {
		if p < lo || p > hi {
			delete(s.requested, p)
		}
	}
	if s.inflight >= 0 && (s.inflight < lo || s.inflight > hi) && s.fetchStop != nil {
		s.fetchStop()
	}
	for p := first / PageRows; p <= last/PageRows; p++ {
		s.requestLocked(p)
	}
	s.requestLocked(first/PageRows - 1)
	s.requestLocked(last/PageRows + 1)
}

func (s *TableSource) window() (lo, hi int64) {
	return s.visFirst/PageRows - 2, s.visLast/PageRows + 2
}

// Cell returns the display text and kind of a cell (including any staged edit).
// ok is false while the row is still loading.
func (s *TableSource) Cell(row int64, col int) (text string, kind Kind, trunc bool, ok bool) {
	s.mu.Lock()
	baseCount := s.count
	s.mu.Unlock()

	if s.changes != nil && row >= baseCount {
		ins := s.changes.Inserts()
		idx := int(row - baseCount)
		if idx >= 0 && idx < len(ins) {
			v, ok := ins[idx].Values[col]
			if !ok || v.Null {
				return "", KindNull, false, true
			}
			return v.Text, inferKind(v.Text, s.cols[col].Type), false, true
		}
	}

	rawTxt, rawKind, rawTrunc, rowid, loaded := s.rawCell(row, col)
	if !loaded {
		return "", KindLoading, false, false
	}
	if s.changes != nil && s.byRowid {
		if v, edited, _ := s.changes.LookupKey(edit.RowKey{ID: rowid}, col); edited {
			if v.Null {
				return "", KindNull, false, true
			}
			if v.Kind == edit.KindBlob || (v.Kind == edit.KindAuto && v.Blob != nil) {
				return "BLOB " + FormatSize(int64(len(v.Blob))), KindBlob, false, true
			}
			return v.Text, inferKind(v.Text, s.cols[col].Type), false, true
		}
	}
	return rawTxt, rawKind, rawTrunc, true
}

// CellStatus reports staged-edit flags for (row, col).
func (s *TableSource) CellStatus(row int64, col int) (edited, rowDeleted, rowInserted bool) {
	if s.changes == nil {
		return false, false, false
	}
	key, ok := s.RowKey(row)
	if !ok {
		return false, false, false
	}
	if key.Inserted {
		return true, false, true
	}
	_, edited, rowDeleted = s.changes.LookupKey(key, col)
	return edited, rowDeleted, false
}

// StageCellEdit stages an edit to (row, col), capturing the original database
// value from the loaded grid page. For truncated text cells (> PreviewBytes),
// returns false so callers supply the full original value via StageCellEditWithOrig.
func (s *TableSource) StageCellEdit(row int64, col int, newVal edit.Value) bool {
	return s.StageCellEditWithOrig(row, col, edit.Value{}, false, newVal)
}

// StageCellEditWithOrig stages an edit to (row, col), using fullOrig if supplied
// or falling back to the cached page cell when untruncated.
func (s *TableSource) StageCellEditWithOrig(row int64, col int, fullOrig edit.Value, hasOrig bool, newVal edit.Value) bool {
	if s.changes == nil || col < 0 || col >= len(s.cols) {
		return false
	}
	key, ok := s.RowKey(row)
	if !ok {
		return false
	}
	var orig edit.Value
	if !key.Inserted {
		if hasOrig {
			orig = fullOrig
		} else {
			rawTxt, rawKind, rawTrunc, _, loaded := s.rawCell(row, col)
			if !loaded || rawTrunc || rawKind == KindBlob {
				return false
			}
			orig = rawCellToEditValue(rawKind, rawTxt)
		}
	}
	s.changes.SetCellKey(key, col, orig, newVal)
	s.invalidate()
	return true
}

func rawCellToEditValue(k Kind, txt string) edit.Value {
	switch k {
	case KindNull:
		return edit.NullValue()
	case KindInteger:
		return edit.ExactValue(edit.KindInt, txt, nil)
	case KindReal:
		return edit.ExactValue(edit.KindReal, txt, nil)
	default:
		return edit.ExactValue(edit.KindText, txt, nil)
	}
}

func inferKind(txt, declType string) Kind {
	up := strings.ToUpper(declType)
	if strings.Contains(up, "INT") {
		if _, err := strconv.ParseInt(strings.TrimSpace(txt), 10, 64); err == nil {
			return KindInteger
		}
	}
	if strings.Contains(up, "REAL") || strings.Contains(up, "FLOA") || strings.Contains(up, "DOUB") || strings.Contains(up, "NUM") {
		if _, err := strconv.ParseFloat(strings.TrimSpace(txt), 64); err == nil {
			return KindReal
		}
	}
	return KindText
}

func (s *TableSource) rawCell(row int64, col int) (txt string, kind Kind, trunc bool, rowid int64, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.pages[row/PageRows]
	if p == nil {
		s.requestLocked(row / PageRows)
		return "", KindLoading, false, 0, false
	}
	r := int(row % PageRows)
	if r >= p.nrows || col >= p.ncols {
		return "", KindLoading, false, 0, false
	}
	s.tick++
	p.lastUse = s.tick
	t, k, tr := p.text(r, col)
	var key int64
	if p.keys != nil {
		key = p.keys[r]
	}
	return t, k, tr, key, true
}

// RowKey returns the edit.RowKey of a loaded database row (with any int64 rowid,
// including <= 0) or a staged inserted row.
func (s *TableSource) RowKey(row int64) (edit.RowKey, bool) {
	s.mu.Lock()
	baseCount := s.count
	p := s.pages[row/PageRows]
	s.mu.Unlock()

	if s.changes != nil && row >= baseCount {
		ins := s.changes.Inserts()
		idx := int(row - baseCount)
		if idx >= 0 && idx < len(ins) {
			return edit.RowKey{ID: ins[idx].ID, Inserted: true}, true
		}
		return edit.RowKey{}, false
	}
	if p == nil || p.keys == nil || int(row%PageRows) >= p.nrows {
		return edit.RowKey{}, false
	}
	return edit.RowKey{ID: p.keys[row%PageRows]}, true
}

// FetchCellDetail loads up to maxBytes starting at offset 0 for (row, col).
func (s *TableSource) FetchCellDetail(row int64, col int, maxBytes int, cb func(CellDetail, error)) {
	s.FetchCellDetailAt(row, col, 0, maxBytes, cb)
}

// FetchCellDetailAt loads up to maxBytes of the cell at (row, col) starting from
// byte offset (using SQLite incremental BLOB I/O for BLOB columns) on the
// foreground connection and delivers it to cb.
func (s *TableSource) FetchCellDetailAt(row int64, col int, offset int64, maxBytes int, cb func(CellDetail, error)) {
	if col < 0 || col >= len(s.cols) {
		return
	}
	if offset < 0 {
		offset = 0
	}
	if maxBytes <= 0 {
		maxBytes = 64 * 1024
	}
	key, hasKey := s.RowKey(row)
	if s.changes != nil && hasKey {
		if v, edited, _ := s.changes.LookupKey(key, col); edited {
			k := inferKind(v.Text, s.cols[col].Type)
			var payload []byte
			if v.Null {
				k = KindNull
			} else if v.Kind == edit.KindBlob || (v.Kind == edit.KindAuto && v.Blob != nil) {
				k = KindBlob
				payload = slices.Clone(v.Blob)
			} else {
				payload = []byte(v.Text)
			}
			total := int64(len(payload))
			start := min(total, offset)
			end := min(total, start+int64(maxBytes))
			cb(CellDetail{
				Row:        row,
				Rowid:      key.ID,
				Offset:     start,
				Col:        col,
				Kind:       k,
				TotalBytes: total,
				Data:       payload[start:end],
				Truncated:  end < total || start > 0,
			}, nil)
			return
		}
	}

	colName := s.cols[col].Name
	colIdent := engine.QuoteIdent(colName)
	from := s.from
	byRowid := s.byRowid && hasKey && !key.Inserted
	canOpenBlob := byRowid && s.obj.Type == engine.TypeTable && !s.obj.WithoutRowid

	s.db.Fg.Go(s.ctx, func(db *sqlite3.Conn) error {
		// Step 1: check storage class cleanly.
		var typQ string
		if byRowid {
			typQ = fmt.Sprintf("SELECT typeof(%s) FROM %s WHERE rowid = ?", colIdent, from)
		} else {
			typQ = fmt.Sprintf("SELECT typeof(%s) FROM %s LIMIT 1 OFFSET ?", colIdent, from)
		}
		stmtTyp, _, err := db.Prepare(typQ)
		if err != nil {
			return err
		}
		if byRowid {
			_ = stmtTyp.BindInt64(1, key.ID)
		} else {
			_ = stmtTyp.BindInt64(1, row)
		}
		if !stmtTyp.Step() {
			err := stmtTyp.Err()
			_ = stmtTyp.Close()
			return err
		}
		typStr := stmtTyp.ColumnText(0)
		if err := stmtTyp.Close(); err != nil {
			return err
		}

		det := CellDetail{Row: row, Rowid: key.ID, Offset: offset, Col: col}
		switch typStr {
		case "null":
			det.Kind = KindNull
			cb(det, nil)
			return nil
		case "integer":
			det.Kind = KindInteger
		case "real":
			det.Kind = KindReal
		case "text":
			det.Kind = KindText
		case "blob":
			det.Kind = KindBlob
		}

		// Step 2: use SQLite incremental BLOB I/O for BLOBs when possible.
		if det.Kind == KindBlob && canOpenBlob {
			if b, berr := db.OpenBlob("main", s.obj.Name, colName, key.ID, false); berr == nil {
				defer b.Close()
				det.TotalBytes = b.Size()
				if offset < det.TotalBytes {
					if _, serr := b.Seek(offset, io.SeekStart); serr != nil {
						return serr
					}
					want := int(min(int64(maxBytes), det.TotalBytes-offset))
					buf := make([]byte, want)
					n, rerr := io.ReadFull(b, buf)
					if rerr != nil && rerr != io.EOF && rerr != io.ErrUnexpectedEOF {
						return rerr
					}
					det.Data = buf[:n]
				}
				det.Truncated = offset+int64(len(det.Data)) < det.TotalBytes || offset > 0
				cb(det, nil)
				return nil
			}
		}

		// Step 3: text/numeric or non-rowid fallback.
		var q string
		if byRowid {
			if offset > 0 {
				q = fmt.Sprintf("SELECT length(CAST(%[1]s AS BLOB)), substr(%[1]s, ?, ?) FROM %[2]s WHERE rowid = ?", colIdent, from)
			} else {
				q = fmt.Sprintf("SELECT length(CAST(%[1]s AS BLOB)), substr(%[1]s, 1, ?) FROM %[2]s WHERE rowid = ?", colIdent, from)
			}
		} else if offset > 0 {
			q = fmt.Sprintf("SELECT length(CAST(%[1]s AS BLOB)), substr(%[1]s, ?, ?) FROM %[2]s LIMIT 1 OFFSET ?", colIdent, from)
		} else {
			q = fmt.Sprintf("SELECT length(CAST(%[1]s AS BLOB)), substr(%[1]s, 1, ?) FROM %[2]s LIMIT 1 OFFSET ?", colIdent, from)
		}
		stmt, _, err := db.Prepare(q)
		if err != nil {
			return err
		}
		defer stmt.Close()
		switch {
		case byRowid && offset > 0:
			_ = stmt.BindInt64(1, offset+1)
			_ = stmt.BindInt(2, maxBytes)
			_ = stmt.BindInt64(3, key.ID)
		case byRowid:
			_ = stmt.BindInt(1, maxBytes)
			_ = stmt.BindInt64(2, key.ID)
		case offset > 0:
			_ = stmt.BindInt64(1, offset+1)
			_ = stmt.BindInt(2, maxBytes)
			_ = stmt.BindInt64(3, row)
		default:
			_ = stmt.BindInt(1, maxBytes)
			_ = stmt.BindInt64(2, row)
		}
		if !stmt.Step() {
			return stmt.Err()
		}
		raw := stmt.ColumnRawBlob(1)
		det.Data = slices.Clone(raw)
		det.TotalBytes = stmt.ColumnInt64(0)
		det.Truncated = offset+int64(len(det.Data)) < det.TotalBytes || offset > 0
		cb(det, nil)
		return nil
	}, func(err error) {
		if err != nil {
			cb(CellDetail{Row: row, Col: col}, err)
		}
	})
}

// ExportCellTo streams the full value of cell (row, col) to w (honoring any staged
// edits and using SQLite incremental BLOB I/O when available).
func (s *TableSource) ExportCellTo(row int64, col int, w io.Writer) (int64, error) {
	if col < 0 || col >= len(s.cols) {
		return 0, fmt.Errorf("column %d out of range", col)
	}
	key, hasKey := s.RowKey(row)
	if s.changes != nil && hasKey {
		if v, edited, _ := s.changes.LookupKey(key, col); edited {
			if v.Null {
				return 0, nil
			}
			var payload []byte
			if v.Blob != nil {
				payload = v.Blob
			} else {
				payload = []byte(v.Text)
			}
			n, err := w.Write(payload)
			return int64(n), err
		}
	}

	colName := s.cols[col].Name
	colIdent := engine.QuoteIdent(colName)
	from := s.from
	byRowid := s.byRowid && hasKey && !key.Inserted
	canOpenBlob := byRowid && s.obj.Type == engine.TypeTable && !s.obj.WithoutRowid

	var written int64
	err := s.db.Fg.Do(s.ctx, func(db *sqlite3.Conn) error {
		if canOpenBlob {
			if b, berr := db.OpenBlob("main", s.obj.Name, colName, key.ID, false); berr == nil {
				defer b.Close()
				buf := make([]byte, 64*1024)
				n, cerr := io.CopyBuffer(w, b, buf)
				written = n
				return cerr
			}
		}
		var q string
		if byRowid {
			q = fmt.Sprintf("SELECT %s FROM %s WHERE rowid = ?", colIdent, from)
		} else {
			q = fmt.Sprintf("SELECT %s FROM %s LIMIT 1 OFFSET ?", colIdent, from)
		}
		stmt, _, err := db.Prepare(q)
		if err != nil {
			return err
		}
		defer stmt.Close()
		if byRowid {
			_ = stmt.BindInt64(1, key.ID)
		} else {
			_ = stmt.BindInt64(1, row)
		}
		if !stmt.Step() {
			return stmt.Err()
		}
		if stmt.ColumnType(0) == sqlite3.NULL {
			return nil
		}
		raw := stmt.ColumnRawBlob(0)
		n, werr := w.Write(raw)
		written = int64(n)
		return werr
	})
	return written, err
}

func (s *TableSource) requestLocked(p int64) {
	if p < 0 {
		return
	}
	if s.countState == CountExact && p*PageRows >= s.count {
		return
	}
	if _, ok := s.pages[p]; ok || (p == s.inflight && s.inflightGen == s.gen) {
		return
	}
	if _, ok := s.requested[p]; ok {
		return
	}
	s.requested[p] = struct{}{}
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *TableSource) next() (int64, uint64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.requested) == 0 {
		return 0, 0, false
	}
	mid := (s.visFirst + s.visLast) / 2 / PageRows
	best, bestD := int64(-1), int64(math.MaxInt64)
	for p := range s.requested {
		d := p - mid
		if d < 0 {
			d = -d
		}
		if d < bestD {
			best, bestD = p, d
		}
	}
	delete(s.requested, best)
	s.inflight = best
	s.inflightGen = s.gen
	return best, s.gen, true
}

func (s *TableSource) fetchLoop() {
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-s.wake:
		}
		for {
			p, gen, ok := s.next()
			if !ok {
				break
			}
			ctx, stop := context.WithCancel(s.ctx)
			s.mu.Lock()
			if gen != s.gen {
				stop()
				s.mu.Unlock()
				continue
			}
			s.fetchStop = stop
			plan := s.planLocked(p)
			s.mu.Unlock()

			var pg *page
			err := s.db.Fg.Do(ctx, func(db *sqlite3.Conn) error {
				var err error
				pg, err = s.fetch(db, p, plan)
				return err
			})
			stop()

			s.mu.Lock()
			if s.inflight == p && s.inflightGen == gen {
				s.inflight = -1
				s.fetchStop = nil
			}
			if gen != s.gen {
				s.mu.Unlock()
				continue
			}
			switch {
			case err == nil:
				s.storeLocked(pg)
				s.err = nil
			case errors.Is(err, context.Canceled) || errors.Is(err, sqlite3.INTERRUPT):
			default:
				s.err = err
			}
			s.mu.Unlock()
			if err == nil || !errors.Is(err, context.Canceled) {
				s.invalidate()
			}
		}
	}
}

type fetchPlan struct {
	after      *int64
	before     *int64
	anchor     *int64
	offset     int64
	rowidDesc  bool
	whereSQL   string
	orderBySQL string
}

// fastRowidOrder reports whether the current sort/filter allows O(1) rowid
// keyset pagination.
func (s *TableSource) fastRowidOrderLocked() (ok bool, desc bool) {
	if !s.byRowid || s.filterSQL != "" {
		return false, false
	}
	if s.sortCol < 0 {
		return true, false
	}
	if s.obj.RowidAlias != "" && strings.EqualFold(s.cols[s.sortCol].Name, s.obj.RowidAlias) {
		return true, s.sortDesc
	}
	return false, false
}

func (s *TableSource) planLocked(p int64) fetchPlan {
	start := p * PageRows
	fast, desc := s.fastRowidOrderLocked()
	if fast && !desc {
		if p == 0 {
			return fetchPlan{offset: 0}
		}
		if prev := s.pages[p-1]; prev != nil && prev.nrows == PageRows {
			k := prev.keys[prev.nrows-1]
			return fetchPlan{after: &k}
		}
		if nxt := s.pages[p+1]; nxt != nil && nxt.nrows > 0 {
			k := nxt.keys[0]
			return fetchPlan{before: &k}
		}
		if s.anchorsOK {
			i := start / AnchorStride
			if i < int64(len(s.anchors)) {
				a := s.anchors[i]
				return fetchPlan{anchor: &a, offset: start - i*AnchorStride}
			}
		}
		return fetchPlan{offset: start}
	}
	if fast && desc {
		if p == 0 {
			return fetchPlan{offset: 0, rowidDesc: true}
		}
		if prev := s.pages[p-1]; prev != nil && prev.nrows == PageRows {
			k := prev.keys[prev.nrows-1]
			return fetchPlan{before: &k, rowidDesc: true}
		}
		if nxt := s.pages[p+1]; nxt != nil && nxt.nrows > 0 {
			k := nxt.keys[0]
			return fetchPlan{after: &k, rowidDesc: true}
		}
		if s.anchorsOK && s.countState == CountExact && s.count > 0 && len(s.anchors) > 0 {
			rem := (s.count - 1) - start
			if rem > 0 {
				k := (rem + AnchorStride - 1) / AnchorStride
				if k < int64(len(s.anchors)) {
					a := s.anchors[k]
					descPos := (s.count - 1) - k*AnchorStride
					if start >= descPos {
						return fetchPlan{anchor: &a, offset: start - descPos, rowidDesc: true}
					}
				}
			}
		}
		return fetchPlan{offset: start, rowidDesc: true}
	}

	var orderBy string
	if s.sortCol >= 0 && s.sortCol < len(s.cols) {
		dir := "ASC"
		if s.sortDesc {
			dir = "DESC"
		}
		orderBy = engine.QuoteIdent(s.cols[s.sortCol].Name) + " " + dir
		if s.byRowid {
			orderBy += ", rowid " + dir
		}
	} else if s.byRowid {
		orderBy = "rowid"
	}
	return fetchPlan{
		offset:     start,
		whereSQL:   s.filterSQL,
		orderBySQL: orderBy,
	}
}

func (s *TableSource) fetch(db *sqlite3.Conn, p int64, plan fetchPlan) (*page, error) {
	q := "SELECT " + s.selectList + " FROM " + s.from
	var args []int64
	reverse := false
	switch {
	case !plan.rowidDesc && plan.after != nil:
		q += " WHERE rowid > ? ORDER BY rowid LIMIT " + strconv.Itoa(PageRows)
		args = append(args, *plan.after)
	case !plan.rowidDesc && plan.before != nil:
		q += " WHERE rowid < ? ORDER BY rowid DESC LIMIT " + strconv.Itoa(PageRows)
		args = append(args, *plan.before)
		reverse = true
	case !plan.rowidDesc && plan.anchor != nil:
		q += " WHERE rowid >= ? ORDER BY rowid LIMIT " + strconv.Itoa(PageRows) + " OFFSET ?"
		args = append(args, *plan.anchor, plan.offset)
	case plan.rowidDesc && plan.before != nil:
		q += " WHERE rowid < ? ORDER BY rowid DESC LIMIT " + strconv.Itoa(PageRows)
		args = append(args, *plan.before)
	case plan.rowidDesc && plan.after != nil:
		q += " WHERE rowid > ? ORDER BY rowid ASC LIMIT " + strconv.Itoa(PageRows)
		args = append(args, *plan.after)
		reverse = true
	case plan.rowidDesc && plan.anchor != nil:
		q += " WHERE rowid <= ? ORDER BY rowid DESC LIMIT " + strconv.Itoa(PageRows) + " OFFSET ?"
		args = append(args, *plan.anchor, plan.offset)
	case plan.rowidDesc:
		q += " ORDER BY rowid DESC LIMIT " + strconv.Itoa(PageRows) + " OFFSET ?"
		args = append(args, plan.offset)
	default:
		if plan.whereSQL != "" {
			q += " WHERE (" + plan.whereSQL + ")"
		}
		if plan.orderBySQL != "" {
			q += " ORDER BY " + plan.orderBySQL
		}
		q += " LIMIT " + strconv.Itoa(PageRows) + " OFFSET ?"
		args = append(args, plan.offset)
	}

	stmt, _, err := db.Prepare(q)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()
	for i, a := range args {
		if err := stmt.BindInt64(i+1, a); err != nil {
			return nil, err
		}
	}

	ncols := len(s.cols)
	pg := &page{index: p, ncols: ncols, buf: make([]byte, 0, PageRows*ncols*12)}
	pg.cells = make([]cell, 0, PageRows*ncols)
	if s.byRowid {
		pg.keys = make([]int64, 0, PageRows)
	}
	for stmt.Step() {
		if s.byRowid {
			pg.keys = append(pg.keys, stmt.ColumnInt64(0))
		}
		for c := range ncols {
			ti, vi := 1+2*c, 2+2*c
			var ce cell
			switch string(stmt.ColumnRawText(ti)) {
			case "null":
				ce.kind = KindNull
			case "integer":
				ce.kind = KindInteger
			case "real":
				ce.kind = KindReal
			case "text":
				ce.kind = KindText
			case "blob":
				ce.kind = KindBlob
			}
			ce.off = uint32(len(pg.buf))
			switch ce.kind {
			case KindNull:
			case KindBlob:
				pg.buf = append(pg.buf, "BLOB "...)
				pg.buf = append(pg.buf, FormatSize(stmt.ColumnInt64(vi))...)
			default:
				v := stmt.ColumnRawText(vi)
				ce.trunc = ce.kind == KindText && len(v) >= PreviewBytes
				pg.buf = append(pg.buf, v...)
			}
			ce.n = uint32(len(pg.buf)) - ce.off
			pg.cells = append(pg.cells, ce)
		}
		pg.nrows++
	}
	if err := stmt.Err(); err != nil {
		return nil, err
	}
	if reverse {
		reversePage(pg)
	}
	pg.buf = slices.Clip(pg.buf)
	return pg, nil
}

func reversePage(pg *page) {
	slices.Reverse(pg.keys)
	n := pg.ncols
	for i, j := 0, pg.nrows-1; i < j; i, j = i+1, j-1 {
		for c := range n {
			pg.cells[i*n+c], pg.cells[j*n+c] = pg.cells[j*n+c], pg.cells[i*n+c]
		}
	}
}

func (s *TableSource) storeLocked(pg *page) {
	if pg.nrows < PageRows {
		end := pg.index*PageRows + int64(pg.nrows)
		switch {
		case pg.nrows > 0 || pg.index == 0:
			s.count, s.countState = end, CountExact
		case s.countState != CountExact && s.count > end:
			s.count = end
		}
	}
	s.tick++
	pg.lastUse = s.tick
	if old := s.pages[pg.index]; old != nil {
		s.cacheBytes -= old.size()
	}
	s.pages[pg.index] = pg
	s.cacheBytes += pg.size()
	s.evictLocked()
}

func (s *TableSource) evictLocked() {
	lo, hi := s.window()
	for s.cacheBytes > s.budget {
		var victim *page
		for _, p := range s.pages {
			if p.index >= lo && p.index <= hi {
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

func (s *TableSource) startCount(gen uint64) {
	s.mu.Lock()
	if s.countStop != nil {
		s.countStop()
	}
	if s.countState == CountExact && s.filterSQL == "" && s.anchorsOK {
		s.mu.Unlock()
		return
	}
	ctx, stop := context.WithCancel(s.ctx)
	s.countStop = stop
	whereSQL := s.filterSQL
	s.mu.Unlock()

	go s.countLoop(ctx, gen, whereSQL)
}

func (s *TableSource) countLoop(ctx context.Context, gen uint64, whereSQL string) {
	if whereSQL == "" {
		_ = s.db.Bg.Do(ctx, func(db *sqlite3.Conn) error {
			if n, ok := estimateCount(db, s.obj, s.from, s.byRowid); ok {
				s.mu.Lock()
				if s.gen == gen && s.countState == CountUnknown {
					s.count, s.countState = n, CountEstimate
				}
				s.mu.Unlock()
				s.invalidate()
			}
			return nil
		})
	}

	err := s.db.Bg.Do(ctx, func(db *sqlite3.Conn) error {
		if !s.byRowid || whereSQL != "" {
			q := "SELECT count(*) FROM " + s.from
			if whereSQL != "" {
				q += " WHERE (" + whereSQL + ")"
			}
			stmt, _, err := db.Prepare(q)
			if err != nil {
				return err
			}
			defer stmt.Close()
			if stmt.Step() {
				n := stmt.ColumnInt64(0)
				s.mu.Lock()
				if s.gen == gen {
					s.count, s.countState = n, CountExact
				}
				s.mu.Unlock()
			}
			return stmt.Err()
		}
		stmt, _, err := db.Prepare("SELECT rowid FROM " + s.from + " ORDER BY rowid")
		if err != nil {
			return err
		}
		defer stmt.Close()
		anchors := make([]int64, 0, 1024)
		var n int64
		for stmt.Step() {
			if n%AnchorStride == 0 {
				anchors = append(anchors, stmt.ColumnInt64(0))
			}
			n++
		}
		if err := stmt.Err(); err != nil {
			return err
		}
		s.mu.Lock()
		if s.gen == gen {
			s.anchors, s.anchorsOK = slices.Clip(anchors), true
			s.count, s.countState = n, CountExact
		}
		s.mu.Unlock()
		return nil
	})
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, sqlite3.INTERRUPT) {
		s.mu.Lock()
		if s.gen == gen {
			s.err = err
		}
		s.mu.Unlock()
	}
	s.invalidate()
}

func estimateCount(db *sqlite3.Conn, obj *engine.Object, from string, byRowid bool) (int64, bool) {
	if stmt, _, err := db.Prepare(`SELECT stat FROM main.sqlite_stat1 WHERE tbl = ? AND idx IS NOT NULL LIMIT 1`); err == nil {
		defer stmt.Close()
		_ = stmt.BindText(1, obj.Name)
		if stmt.Step() {
			f := strings.Fields(stmt.ColumnText(0))
			if len(f) > 0 {
				if n, err := strconv.ParseInt(f[0], 10, 64); err == nil {
					return n, true
				}
			}
		}
	}
	if byRowid {
		stmt, _, err := db.Prepare("SELECT max(rowid) - min(rowid) + 1 FROM " + from)
		if err == nil {
			defer stmt.Close()
			if stmt.Step() && stmt.ColumnType(0) != sqlite3.NULL {
				return stmt.ColumnInt64(0), true
			}
		}
	}
	return 0, false
}
