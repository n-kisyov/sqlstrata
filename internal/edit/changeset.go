// Package edit implements staged, transactional table editing with undo/redo
// and optimistic concurrency checks.
package edit

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/ncruces/go-sqlite3"

	"github.com/sqlstrata/sqlstrata/internal/engine"
)

// Value represents a literal SQL cell value in a staged edit.
type Value struct {
	Null bool
	Text string // raw text representation (numbers, strings, or hex for blobs)
	Blob []byte // non-nil only when setting a binary blob
}

// NullValue returns a NULL Value.
func NullValue() Value { return Value{Null: true} }

// TextValue returns a non-null text/numeric Value.
func TextValue(s string) Value { return Value{Text: s} }

// CellEdit records the original and new value of a single cell on an existing row.
type CellEdit struct {
	Col  int
	Orig Value
	New  Value
}

// NewRow represents a staged INSERT row.
type NewRow struct {
	ID     int64 // temporary negative ID (-1, -2, ...)
	Values map[int]Value
}

type opKind uint8

const (
	opUpdateCell opKind = iota
	opDeleteRow
	opUndeleteRow
	opInsertRow
)

type undoEntry struct {
	kind    opKind
	rowid   int64
	col     int
	hadPrev bool
	prev    CellEdit
	next    CellEdit
	newRow  NewRow
}

// ChangeSet holds all uncommitted edits for a single table.
type ChangeSet struct {
	mu      sync.Mutex
	table   string
	cols    []engine.Column
	updates map[int64]map[int]CellEdit // rowid -> col -> edit
	deletes map[int64]bool             // rowids marked for deletion
	inserts []NewRow
	nextID  int64
	undo    []undoEntry
	redo    []undoEntry
}

// NewChangeSet creates an empty ChangeSet for table.
func NewChangeSet(table string, cols []engine.Column) *ChangeSet {
	return &ChangeSet{
		table:   table,
		cols:    cols,
		updates: map[int64]map[int]CellEdit{},
		deletes: map[int64]bool{},
		nextID:  -1,
	}
}

// HasChanges reports whether any edits are staged.
func (cs *ChangeSet) HasChanges() bool {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return len(cs.updates) > 0 || len(cs.deletes) > 0 || len(cs.inserts) > 0
}

// Count returns the total number of staged row/cell modifications.
func (cs *ChangeSet) Count() int {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	n := len(cs.deletes) + len(cs.inserts)
	for _, m := range cs.updates {
		n += len(m)
	}
	return n
}

// Clear discards all staged edits and undo history.
func (cs *ChangeSet) Clear() {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.updates = map[int64]map[int]CellEdit{}
	cs.deletes = map[int64]bool{}
	cs.inserts = nil
	cs.undo = nil
	cs.redo = nil
}

// SetCell stages an update to (rowid, col). If rowid < 0, it updates the
// corresponding staged inserted row.
func (cs *ChangeSet) SetCell(rowid int64, col int, orig, val Value) {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	if rowid < 0 {
		for i := range cs.inserts {
			if cs.inserts[i].ID == rowid {
				cs.inserts[i].Values[col] = val
				return
			}
		}
		return
	}

	rowMap := cs.updates[rowid]
	prev, hadPrev := rowMap[col]
	if hadPrev {
		orig = prev.Orig
	}
	next := CellEdit{Col: col, Orig: orig, New: val}

	cs.undo = append(cs.undo, undoEntry{
		kind:    opUpdateCell,
		rowid:   rowid,
		col:     col,
		hadPrev: hadPrev,
		prev:    prev,
		next:    next,
	})
	cs.redo = nil
	cs.applyCellLocked(rowid, col, orig, val)
}

func (cs *ChangeSet) applyCellLocked(rowid int64, col int, orig, val Value) {
	if orig.Null == val.Null && orig.Text == val.Text && string(orig.Blob) == string(val.Blob) {
		if m := cs.updates[rowid]; m != nil {
			delete(m, col)
			if len(m) == 0 {
				delete(cs.updates, rowid)
			}
		}
		return
	}
	m := cs.updates[rowid]
	if m == nil {
		m = map[int]CellEdit{}
		cs.updates[rowid] = m
	}
	m[col] = CellEdit{Col: col, Orig: orig, New: val}
}

// ToggleDelete marks or unmarks rowid for deletion. If rowid < 0, it removes
// the staged inserted row.
func (cs *ChangeSet) ToggleDelete(rowid int64) {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	if rowid < 0 {
		for i := range cs.inserts {
			if cs.inserts[i].ID == rowid {
				cs.inserts = slices.Delete(cs.inserts, i, i+1)
				return
			}
		}
		return
	}
	if cs.deletes[rowid] {
		delete(cs.deletes, rowid)
		cs.undo = append(cs.undo, undoEntry{kind: opUndeleteRow, rowid: rowid})
	} else {
		cs.deletes[rowid] = true
		cs.undo = append(cs.undo, undoEntry{kind: opDeleteRow, rowid: rowid})
	}
	cs.redo = nil
}

// InsertRow stages a new blank row and returns its temporary negative ID.
func (cs *ChangeSet) InsertRow() int64 {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	id := cs.nextID
	cs.nextID--
	nr := NewRow{ID: id, Values: map[int]Value{}}
	for i, c := range cs.cols {
		if !c.NotNull || c.HasDflt || c.PK > 0 {
			nr.Values[i] = NullValue()
		} else {
			nr.Values[i] = TextValue("")
		}
	}
	cs.inserts = append(cs.inserts, nr)
	cs.undo = append(cs.undo, undoEntry{kind: opInsertRow, rowid: id, newRow: nr})
	cs.redo = nil
	return id
}

// Inserts returns a snapshot of staged inserted rows.
func (cs *ChangeSet) Inserts() []NewRow {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	out := make([]NewRow, len(cs.inserts))
	for i, r := range cs.inserts {
		cp := map[int]Value{}
		for k, v := range r.Values {
			cp[k] = v
		}
		out[i] = NewRow{ID: r.ID, Values: cp}
	}
	return out
}

// Lookup returns any staged override for (rowid, col) and whether the row is
// marked for deletion.
func (cs *ChangeSet) Lookup(rowid int64, col int) (val Value, edited bool, deleted bool) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if rowid < 0 {
		for _, r := range cs.inserts {
			if r.ID == rowid {
				v, ok := r.Values[col]
				if !ok {
					return NullValue(), true, false
				}
				return v, true, false
			}
		}
		return NullValue(), false, false
	}
	deleted = cs.deletes[rowid]
	if m := cs.updates[rowid]; m != nil {
		if e, ok := m[col]; ok {
			return e.New, true, deleted
		}
	}
	return Value{}, false, deleted
}

// IsRowDeleted reports whether rowid is staged for deletion.
func (cs *ChangeSet) IsRowDeleted(rowid int64) bool {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.deletes[rowid]
}

// Undo reverts the most recent edit.
func (cs *ChangeSet) Undo() bool {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if len(cs.undo) == 0 {
		return false
	}
	u := cs.undo[len(cs.undo)-1]
	cs.undo = cs.undo[:len(cs.undo)-1]
	switch u.kind {
	case opUpdateCell:
		if u.hadPrev {
			cs.applyCellLocked(u.rowid, u.col, u.prev.Orig, u.prev.New)
		} else {
			cs.applyCellLocked(u.rowid, u.col, u.next.Orig, u.next.Orig)
		}
	case opDeleteRow:
		delete(cs.deletes, u.rowid)
	case opUndeleteRow:
		cs.deletes[u.rowid] = true
	case opInsertRow:
		for i := range cs.inserts {
			if cs.inserts[i].ID == u.rowid {
				u.newRow = cs.inserts[i]
				cs.inserts = slices.Delete(cs.inserts, i, i+1)
				break
			}
		}
	}
	cs.redo = append(cs.redo, u)
	return true
}

// Redo re-applies the most recently undone edit.
func (cs *ChangeSet) Redo() bool {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if len(cs.redo) == 0 {
		return false
	}
	u := cs.redo[len(cs.redo)-1]
	cs.redo = cs.redo[:len(cs.redo)-1]
	switch u.kind {
	case opUpdateCell:
		cs.applyCellLocked(u.rowid, u.col, u.next.Orig, u.next.New)
	case opDeleteRow:
		cs.deletes[u.rowid] = true
	case opUndeleteRow:
		delete(cs.deletes, u.rowid)
	case opInsertRow:
		cs.inserts = append(cs.inserts, u.newRow)
	}
	cs.undo = append(cs.undo, u)
	return true
}

// PreviewSQL renders the human-readable transactional SQL script that Commit
// will execute.
func (cs *ChangeSet) PreviewSQL() string {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	var b strings.Builder
	tbl := "main." + engine.QuoteIdent(cs.table)
	b.WriteString("BEGIN IMMEDIATE;\n")

	// 1. Updates in deterministic rowid order.
	rowids := make([]int64, 0, len(cs.updates))
	for id := range cs.updates {
		if !cs.deletes[id] {
			rowids = append(rowids, id)
		}
	}
	slices.Sort(rowids)
	for _, id := range rowids {
		m := cs.updates[id]
		cols := make([]int, 0, len(m))
		for c := range m {
			cols = append(cols, c)
		}
		slices.Sort(cols)
		var sets, guards []string
		for _, c := range cols {
			ed := m[c]
			cname := engine.QuoteIdent(cs.cols[c].Name)
			sets = append(sets, fmt.Sprintf("%s = %s", cname, formatLiteral(ed.New, cs.cols[c].Type)))
			guards = append(guards, fmt.Sprintf("%s IS %s", cname, formatLiteral(ed.Orig, cs.cols[c].Type)))
		}
		fmt.Fprintf(&b, "UPDATE %s SET %s WHERE rowid = %d AND %s;\n",
			tbl, strings.Join(sets, ", "), id, strings.Join(guards, " AND "))
	}

	// 2. Deletes in deterministic rowid order.
	delIDs := make([]int64, 0, len(cs.deletes))
	for id := range cs.deletes {
		delIDs = append(delIDs, id)
	}
	slices.Sort(delIDs)
	for _, id := range delIDs {
		fmt.Fprintf(&b, "DELETE FROM %s WHERE rowid = %d;\n", tbl, id)
	}

	// 3. Inserts.
	for _, ins := range cs.inserts {
		var colNames, vals []string
		for i, c := range cs.cols {
			if c.Hidden >= 2 { // generated column
				continue
			}
			v, ok := ins.Values[i]
			if !ok || (v.Null && (c.PK > 0 || c.HasDflt)) {
				continue
			}
			colNames = append(colNames, engine.QuoteIdent(c.Name))
			vals = append(vals, formatLiteral(v, c.Type))
		}
		if len(colNames) == 0 {
			fmt.Fprintf(&b, "INSERT INTO %s DEFAULT VALUES;\n", tbl)
		} else {
			fmt.Fprintf(&b, "INSERT INTO %s (%s) VALUES (%s);\n",
				tbl, strings.Join(colNames, ", "), strings.Join(vals, ", "))
		}
	}

	b.WriteString("COMMIT;")
	return b.String()
}

// Apply executes all staged changes inside a single transaction on db, checking
// that every UPDATE and DELETE affects exactly 1 row (optimistic concurrency).
func (cs *ChangeSet) Apply(db *sqlite3.Conn) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	if err := db.Exec("BEGIN IMMEDIATE"); err != nil {
		return err
	}
	rollback := func(err error) error {
		_ = db.Exec("ROLLBACK")
		return err
	}

	tbl := "main." + engine.QuoteIdent(cs.table)

	rowids := make([]int64, 0, len(cs.updates))
	for id := range cs.updates {
		if !cs.deletes[id] {
			rowids = append(rowids, id)
		}
	}
	slices.Sort(rowids)
	for _, id := range rowids {
		m := cs.updates[id]
		cols := make([]int, 0, len(m))
		for c := range m {
			cols = append(cols, c)
		}
		slices.Sort(cols)
		var sets, guards []string
		for _, c := range cols {
			ed := m[c]
			cname := engine.QuoteIdent(cs.cols[c].Name)
			sets = append(sets, fmt.Sprintf("%s = %s", cname, formatLiteral(ed.New, cs.cols[c].Type)))
			guards = append(guards, fmt.Sprintf("%s IS %s", cname, formatLiteral(ed.Orig, cs.cols[c].Type)))
		}
		q := fmt.Sprintf("UPDATE %s SET %s WHERE rowid = %d AND %s",
			tbl, strings.Join(sets, ", "), id, strings.Join(guards, " AND "))
		if err := db.Exec(q); err != nil {
			return rollback(err)
		}
		if db.Changes() == 0 {
			return rollback(fmt.Errorf("conflict on rowid %d: row was modified or deleted concurrently", id))
		}
	}

	delIDs := make([]int64, 0, len(cs.deletes))
	for id := range cs.deletes {
		delIDs = append(delIDs, id)
	}
	slices.Sort(delIDs)
	for _, id := range delIDs {
		q := fmt.Sprintf("DELETE FROM %s WHERE rowid = %d", tbl, id)
		if err := db.Exec(q); err != nil {
			return rollback(err)
		}
		if db.Changes() == 0 {
			return rollback(fmt.Errorf("conflict deleting rowid %d: row no longer exists", id))
		}
	}

	for _, ins := range cs.inserts {
		var colNames, vals []string
		for i, c := range cs.cols {
			if c.Hidden >= 2 {
				continue
			}
			v, ok := ins.Values[i]
			if !ok || (v.Null && (c.PK > 0 || c.HasDflt)) {
				continue
			}
			colNames = append(colNames, engine.QuoteIdent(c.Name))
			vals = append(vals, formatLiteral(v, c.Type))
		}
		var q string
		if len(colNames) == 0 {
			q = fmt.Sprintf("INSERT INTO %s DEFAULT VALUES", tbl)
		} else {
			q = fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)",
				tbl, strings.Join(colNames, ", "), strings.Join(vals, ", "))
		}
		if err := db.Exec(q); err != nil {
			return rollback(err)
		}
	}

	if err := db.Exec("COMMIT"); err != nil {
		return rollback(err)
	}

	cs.updates = map[int64]map[int]CellEdit{}
	cs.deletes = map[int64]bool{}
	cs.inserts = nil
	cs.undo = nil
	cs.redo = nil
	return nil
}

func formatLiteral(v Value, declType string) string {
	if v.Null {
		return "NULL"
	}
	if v.Blob != nil {
		return sqlite3.Quote(v.Blob)
	}
	up := strings.ToUpper(declType)
	if strings.Contains(up, "INT") {
		if n, err := strconv.ParseInt(strings.TrimSpace(v.Text), 10, 64); err == nil {
			return strconv.FormatInt(n, 10)
		}
	}
	if strings.Contains(up, "REAL") || strings.Contains(up, "FLOA") || strings.Contains(up, "DOUB") || strings.Contains(up, "NUM") {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v.Text), 64); err == nil {
			return strconv.FormatFloat(f, 'g', -1, 64)
		}
	}
	return sqlite3.Quote(v.Text)
}
