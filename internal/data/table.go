package data

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/ncruces/go-sqlite3"

	"github.com/sqlstrata/sqlstrata/internal/engine"
)

// CountState describes how trustworthy RowCount is.
type CountState uint8

const (
	CountUnknown  CountState = iota
	CountEstimate            // from sqlite_stat1 or rowid range
	CountExact
)

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
	invalidate func()

	ctx    context.Context
	cancel context.CancelFunc
	wake   chan struct{}

	mu         sync.Mutex
	pages      map[int64]*page
	cacheBytes int
	budget     int
	tick       uint64
	requested  map[int64]struct{}
	inflight   int64 // page being fetched, -1 if none
	fetchStop  context.CancelFunc
	visFirst   int64
	visLast    int64
	count      int64
	countState CountState
	anchors    []int64
	anchorsOK  bool
	err        error
}

// Options for a TableSource.
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
		pages:      map[int64]*page{},
		requested:  map[int64]struct{}{},
		inflight:   -1,
		budget:     opt.CacheBudget,
		visLast:    PageRows,
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
	go s.countLoop()
	return s
}

// Close stops background work and releases cached pages.
func (s *TableSource) Close() {
	s.cancel()
	s.mu.Lock()
	s.pages = map[int64]*page{}
	s.cacheBytes = 0
	s.mu.Unlock()
}

// Object returns the schema object being paged.
func (s *TableSource) Object() *engine.Object { return s.obj }

// Columns returns the column descriptions.
func (s *TableSource) Columns() []Column { return s.cols }

// RowCount returns the best known row count.
func (s *TableSource) RowCount() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.count
}

// Count returns the row count and how reliable it is.
func (s *TableSource) Count() (int64, CountState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.count, s.countState
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
	// Abort a slow fetch (e.g. a large OFFSET) that is no longer useful.
	if s.inflight >= 0 && (s.inflight < lo || s.inflight > hi) && s.fetchStop != nil {
		s.fetchStop()
	}
	for p := first / PageRows; p <= last/PageRows; p++ {
		s.requestLocked(p)
	}
	// Prefetch one page on each side.
	s.requestLocked(first/PageRows - 1)
	s.requestLocked(last/PageRows + 1)
}

// window returns the range of pages worth keeping requested.
func (s *TableSource) window() (lo, hi int64) {
	return s.visFirst/PageRows - 2, s.visLast/PageRows + 2
}

// Cell returns the display text and kind of a cell. ok is false while the
// row is still loading.
func (s *TableSource) Cell(row int64, col int) (text string, kind Kind, trunc bool, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.pages[row/PageRows]
	if p == nil {
		s.requestLocked(row / PageRows)
		return "", KindLoading, false, false
	}
	r := int(row % PageRows)
	if r >= p.nrows || col >= p.ncols {
		return "", KindLoading, false, false
	}
	s.tick++
	p.lastUse = s.tick
	t, k, tr := p.text(r, col)
	return t, k, tr, true
}

// RowKey returns the rowid of a loaded row.
func (s *TableSource) RowKey(row int64) (int64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.pages[row/PageRows]
	if p == nil || p.keys == nil || int(row%PageRows) >= p.nrows {
		return 0, false
	}
	return p.keys[row%PageRows], true
}

func (s *TableSource) requestLocked(p int64) {
	if p < 0 {
		return
	}
	if s.countState == CountExact && p*PageRows >= s.count {
		return
	}
	if _, ok := s.pages[p]; ok || p == s.inflight {
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

// next picks the requested page closest to the middle of the viewport.
func (s *TableSource) next() (int64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.requested) == 0 {
		return 0, false
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
	return best, true
}

func (s *TableSource) fetchLoop() {
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-s.wake:
		}
		for {
			p, ok := s.next()
			if !ok {
				break
			}
			ctx, stop := context.WithCancel(s.ctx)
			s.mu.Lock()
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
			s.inflight = -1
			s.fetchStop = nil
			switch {
			case err == nil:
				s.storeLocked(pg)
				s.err = nil
			case errors.Is(err, context.Canceled) || errors.Is(err, sqlite3.INTERRUPT):
				// Scrolled away or closed; nothing to report.
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

// fetchPlan is a positioning strategy for one page, chosen under the lock.
type fetchPlan struct {
	after  *int64 // keyset: rowid > after
	before *int64 // reverse keyset: rowid < before
	anchor *int64 // rowid >= anchor OFFSET offset
	offset int64
}

func (s *TableSource) planLocked(p int64) fetchPlan {
	start := p * PageRows
	if !s.byRowid || p == 0 {
		return fetchPlan{offset: start}
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

func (s *TableSource) fetch(db *sqlite3.Conn, p int64, plan fetchPlan) (*page, error) {
	q := "SELECT " + s.selectList + " FROM " + s.from
	var args []int64
	reverse := false
	switch {
	case plan.after != nil:
		q += " WHERE rowid > ? ORDER BY rowid LIMIT " + strconv.Itoa(PageRows)
		args = append(args, *plan.after)
	case plan.before != nil:
		q += " WHERE rowid < ? ORDER BY rowid DESC LIMIT " + strconv.Itoa(PageRows)
		args = append(args, *plan.before)
		reverse = true
	case plan.anchor != nil:
		q += " WHERE rowid >= ? ORDER BY rowid LIMIT " + strconv.Itoa(PageRows) + " OFFSET ?"
		args = append(args, *plan.anchor, plan.offset)
	default:
		if s.byRowid {
			q += " ORDER BY rowid"
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
	// A short page means we found the end of the table: the count is exact.
	if pg.nrows < PageRows {
		end := pg.index*PageRows + int64(pg.nrows)
		switch {
		case pg.nrows > 0 || pg.index == 0:
			s.count, s.countState = end, CountExact
		case s.countState != CountExact && s.count > end:
			// Past the end: the estimate was too high, at least shrink it.
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

// countLoop estimates the row count quickly, then computes it exactly while
// building the anchor index, on the background connection.
func (s *TableSource) countLoop() {
	_ = s.db.Bg.Do(s.ctx, func(db *sqlite3.Conn) error {
		if n, ok := estimateCount(db, s.obj, s.from, s.byRowid); ok {
			s.mu.Lock()
			if s.countState == CountUnknown {
				s.count, s.countState = n, CountEstimate
			}
			s.mu.Unlock()
			s.invalidate()
		}
		return nil
	})

	err := s.db.Bg.Do(s.ctx, func(db *sqlite3.Conn) error {
		if !s.byRowid {
			stmt, _, err := db.Prepare("SELECT count(*) FROM " + s.from)
			if err != nil {
				return err
			}
			defer stmt.Close()
			if stmt.Step() {
				n := stmt.ColumnInt64(0)
				s.mu.Lock()
				s.count, s.countState = n, CountExact
				s.mu.Unlock()
			}
			return stmt.Err()
		}
		// SQLite scans the smallest covering index for this query.
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
		s.anchors, s.anchorsOK = slices.Clip(anchors), true
		s.count, s.countState = n, CountExact
		s.mu.Unlock()
		return nil
	})
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, sqlite3.INTERRUPT) {
		s.mu.Lock()
		s.err = err
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
