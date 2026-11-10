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

// ValueKind identifies the explicit SQLite storage class of a Value.
type ValueKind uint8

const (
	KindAuto ValueKind = iota // coerce using column declared type
	KindNull
	KindInt
	KindReal
	KindText
	KindBlob
)

// RowKey uniquely identifies either an existing database row (Inserted == false,
// where ID is the SQLite rowid, which may be negative, zero, or positive) or
// a staged inserted row (Inserted == true, where ID is an allocator sequence).
type RowKey struct {
	ID       int64
	Inserted bool
}

// Value represents a literal SQL cell value in a staged edit.
type Value struct {
	Null bool
	Kind ValueKind
	Text string // raw text representation (numbers, strings, or hex for blobs)
	Blob []byte // non-nil when Kind == KindBlob or Blob != nil
}

// NullValue returns a NULL Value.
func NullValue() Value { return Value{Null: true, Kind: KindNull} }

// TextValue returns a Value whose literal coercion follows the column's
// declared type (or TEXT if non-numeric).
func TextValue(s string) Value { return Value{Text: s} }

// ExactValue returns a Value preserving an exact SQLite storage class for
// optimistic concurrency comparisons.
func ExactValue(kind ValueKind, s string, blob []byte) Value {
	if kind == KindNull {
		return NullValue()
	}
	return Value{Kind: kind, Text: s, Blob: blob}
}

// CellEdit records the original and new value of a single cell on an existing row.
type CellEdit struct {
	Col  int
	Orig Value
	New  Value
}

// NewRow represents a staged INSERT row.
type NewRow struct {
	ID     int64
	Values map[int]Value
}

type opKind uint8

const (
	opUpdateCell opKind = iota
	opDeleteRow
	opUndeleteRow
	opInsertRow
	opUpdateInsertCell
	opDeleteInsertRow
)

type undoEntry struct {
	kind    opKind
	key     RowKey
	col     int
	hadPrev bool
	prevVal Value
	newVal  Value
	prev    CellEdit
	next    CellEdit
	insIdx  int
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
		nextID:  1,
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

// SetCell stages an update to existing database rowid and col.
func (cs *ChangeSet) SetCell(rowid int64, col int, orig, val Value) {
	cs.SetCellKey(RowKey{ID: rowid}, col, orig, val)
}

// SetCellKey stages an update to key (existing DB row or staged insert row).
func (cs *ChangeSet) SetCellKey(key RowKey, col int, orig, val Value) {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	if key.Inserted {
		for i := range cs.inserts {
			if cs.inserts[i].ID == key.ID {
				prevVal, hadPrev := cs.inserts[i].Values[col]
				cs.inserts[i].Values[col] = val
				cs.undo = append(cs.undo, undoEntry{
					kind:    opUpdateInsertCell,
					key:     key,
					col:     col,
					hadPrev: hadPrev,
					prevVal: prevVal,
					newVal:  val,
				})
				cs.redo = nil
				return
			}
		}
		return
	}

	rowMap := cs.updates[key.ID]
	prev, hadPrev := rowMap[col]
	if hadPrev {
		orig = prev.Orig
	}
	next := CellEdit{Col: col, Orig: orig, New: val}

	cs.undo = append(cs.undo, undoEntry{
		kind:    opUpdateCell,
		key:     key,
		col:     col,
		hadPrev: hadPrev,
		prev:    prev,
		next:    next,
	})
	cs.redo = nil
	cs.applyCellLocked(key.ID, col, orig, val)
}

func (cs *ChangeSet) applyCellLocked(rowid int64, col int, orig, val Value) {
	if valuesEqual(orig, val) {
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

func valuesEqual(a, b Value) bool {
	if a.Null || b.Null {
		return a.Null && b.Null
	}
	if a.Kind == KindBlob || b.Kind == KindBlob {
		return a.Kind == b.Kind && string(a.Blob) == string(b.Blob)
	}
	if a.Kind != KindAuto && b.Kind != KindAuto && a.Kind != b.Kind {
		return false
	}
	return a.Text == b.Text && string(a.Blob) == string(b.Blob)
}

// ToggleDelete marks or unmarks existing database rowid for deletion.
func (cs *ChangeSet) ToggleDelete(rowid int64) {
	cs.ToggleDeleteKey(RowKey{ID: rowid})
}

// ToggleDeleteKey marks or unmarks key for deletion (removing staged inserts
// with full undo support).
func (cs *ChangeSet) ToggleDeleteKey(key RowKey) {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	if key.Inserted {
		for i := range cs.inserts {
			if cs.inserts[i].ID == key.ID {
				snap := cloneNewRow(cs.inserts[i])
				cs.inserts = slices.Delete(cs.inserts, i, i+1)
				cs.undo = append(cs.undo, undoEntry{
					kind:   opDeleteInsertRow,
					key:    key,
					insIdx: i,
					newRow: snap,
				})
				cs.redo = nil
				return
			}
		}
		return
	}
	if cs.deletes[key.ID] {
		delete(cs.deletes, key.ID)
		cs.undo = append(cs.undo, undoEntry{kind: opUndeleteRow, key: key})
	} else {
		cs.deletes[key.ID] = true
		cs.undo = append(cs.undo, undoEntry{kind: opDeleteRow, key: key})
	}
	cs.redo = nil
}

// InsertRow stages a new blank row and returns its RowKey.
func (cs *ChangeSet) InsertRow() int64 {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	id := cs.nextID
	cs.nextID++
	nr := NewRow{ID: id, Values: map[int]Value{}}
	for i, c := range cs.cols {
		if !c.NotNull || c.HasDflt || (c.PK > 0 && strings.EqualFold(c.Type, "INTEGER")) {
			nr.Values[i] = NullValue()
		} else {
			nr.Values[i] = TextValue("")
		}
	}
	cs.inserts = append(cs.inserts, nr)
	cs.undo = append(cs.undo, undoEntry{
		kind:   opInsertRow,
		key:    RowKey{ID: id, Inserted: true},
		insIdx: len(cs.inserts) - 1,
		newRow: cloneNewRow(nr),
	})
	cs.redo = nil
	return id
}

func cloneNewRow(nr NewRow) NewRow {
	cp := make(map[int]Value, len(nr.Values))
	for k, v := range nr.Values {
		if v.Blob != nil {
			v.Blob = slices.Clone(v.Blob)
		}
		cp[k] = v
	}
	return NewRow{ID: nr.ID, Values: cp}
}

// Inserts returns a snapshot of staged inserted rows.
func (cs *ChangeSet) Inserts() []NewRow {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	out := make([]NewRow, len(cs.inserts))
	for i, r := range cs.inserts {
		out[i] = cloneNewRow(r)
	}
	return out
}

// Lookup returns any staged override for existing DB rowid and col.
func (cs *ChangeSet) Lookup(rowid int64, col int) (val Value, edited bool, deleted bool) {
	return cs.LookupKey(RowKey{ID: rowid}, col)
}

// LookupKey returns any staged override for key and col.
func (cs *ChangeSet) LookupKey(key RowKey, col int) (val Value, edited bool, deleted bool) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if key.Inserted {
		for _, r := range cs.inserts {
			if r.ID == key.ID {
				v, ok := r.Values[col]
				if !ok {
					return NullValue(), true, false
				}
				return v, true, false
			}
		}
		return NullValue(), false, false
	}
	deleted = cs.deletes[key.ID]
	if m := cs.updates[key.ID]; m != nil {
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
			cs.applyCellLocked(u.key.ID, u.col, u.prev.Orig, u.prev.New)
		} else {
			cs.applyCellLocked(u.key.ID, u.col, u.next.Orig, u.next.Orig)
		}
	case opDeleteRow:
		delete(cs.deletes, u.key.ID)
	case opUndeleteRow:
		cs.deletes[u.key.ID] = true
	case opInsertRow:
		for i := range cs.inserts {
			if cs.inserts[i].ID == u.key.ID {
				u.insIdx = i
				u.newRow = cloneNewRow(cs.inserts[i])
				cs.inserts = slices.Delete(cs.inserts, i, i+1)
				break
			}
		}
	case opUpdateInsertCell:
		for i := range cs.inserts {
			if cs.inserts[i].ID == u.key.ID {
				if u.hadPrev {
					cs.inserts[i].Values[u.col] = u.prevVal
				} else {
					delete(cs.inserts[i].Values, u.col)
				}
				break
			}
		}
	case opDeleteInsertRow:
		idx := max(0, min(len(cs.inserts), u.insIdx))
		cs.inserts = slices.Insert(cs.inserts, idx, cloneNewRow(u.newRow))
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
		cs.applyCellLocked(u.key.ID, u.col, u.next.Orig, u.next.New)
	case opDeleteRow:
		cs.deletes[u.key.ID] = true
	case opUndeleteRow:
		delete(cs.deletes, u.key.ID)
	case opInsertRow:
		idx := max(0, min(len(cs.inserts), u.insIdx))
		cs.inserts = slices.Insert(cs.inserts, idx, cloneNewRow(u.newRow))
	case opUpdateInsertCell:
		for i := range cs.inserts {
			if cs.inserts[i].ID == u.key.ID {
				cs.inserts[i].Values[u.col] = u.newVal
				break
			}
		}
	case opDeleteInsertRow:
		for i := range cs.inserts {
			if cs.inserts[i].ID == u.key.ID {
				cs.inserts = slices.Delete(cs.inserts, i, i+1)
				break
			}
		}
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
	b.WriteString("BEGIN IMMEDIATE;\n")
	for _, q := range cs.buildStatementsLocked() {
		b.WriteString(q)
		b.WriteString(";\n")
	}
	b.WriteString("COMMIT;")
	return b.String()
}

type stmtOp struct {
	sql         string
	expectRow   bool
	conflictMsg string
}

func (cs *ChangeSet) buildStatementsLocked() []string {
	tbl := "main." + engine.QuoteIdent(cs.table)
	var out []string
	for _, op := range cs.buildOpsLocked() {
		out = append(out, op.sql)
	}
	_ = tbl
	return out
}

func (cs *ChangeSet) buildOpsLocked() []stmtOp {
	tbl := "main." + engine.QuoteIdent(cs.table)
	var ops []stmtOp

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
		ops = append(ops, stmtOp{
			sql:         fmt.Sprintf("UPDATE %s SET %s WHERE rowid = %d AND %s", tbl, strings.Join(sets, ", "), id, strings.Join(guards, " AND ")),
			expectRow:   true,
			conflictMsg: fmt.Sprintf("conflict on rowid %d: row was modified or deleted concurrently", id),
		})
	}

	// 2. Deletes in deterministic rowid order.
	delIDs := make([]int64, 0, len(cs.deletes))
	for id := range cs.deletes {
		delIDs = append(delIDs, id)
	}
	slices.Sort(delIDs)
	for _, id := range delIDs {
		ops = append(ops, stmtOp{
			sql:         fmt.Sprintf("DELETE FROM %s WHERE rowid = %d", tbl, id),
			expectRow:   true,
			conflictMsg: fmt.Sprintf("conflict deleting rowid %d: row no longer exists", id),
		})
	}

	// 3. Inserts.
	for _, ins := range cs.inserts {
		var colNames, vals []string
		for i, c := range cs.cols {
			if c.Hidden >= 2 { // generated column
				continue
			}
			v, ok := ins.Values[i]
			autoPK := c.PK > 0 && strings.EqualFold(c.Type, "INTEGER")
			if !ok || (v.Null && (autoPK || c.HasDflt)) {
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
		ops = append(ops, stmtOp{sql: q})
	}
	return ops
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

	for _, op := range cs.buildOpsLocked() {
		if err := db.Exec(op.sql); err != nil {
			return rollback(err)
		}
		if op.expectRow && db.Changes() == 0 {
			return rollback(fmt.Errorf("%s", op.conflictMsg))
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
	if v.Null || v.Kind == KindNull {
		return "NULL"
	}
	if v.Kind == KindBlob || (v.Kind == KindAuto && v.Blob != nil) {
		return sqlite3.Quote(v.Blob)
	}
	switch v.Kind {
	case KindText:
		return sqlite3.Quote(v.Text)
	case KindInt:
		if n, err := strconv.ParseInt(strings.TrimSpace(v.Text), 10, 64); err == nil {
			return strconv.FormatInt(n, 10)
		}
		return sqlite3.Quote(v.Text)
	case KindReal:
		if f, err := strconv.ParseFloat(strings.TrimSpace(v.Text), 64); err == nil {
			return strconv.FormatFloat(f, 'g', -1, 64)
		}
		return sqlite3.Quote(v.Text)
	}

	// KindAuto: coerce new text input according to column declared affinity.
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
