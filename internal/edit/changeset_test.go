package edit

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/ncruces/go-sqlite3"

	"github.com/sqlstrata/sqlstrata/internal/engine"
)

func TestChangeSetUndoRedoAndPreview(t *testing.T) {
	cols := []engine.Column{
		{Name: "id", Type: "INTEGER", PK: 1},
		{Name: "name", Type: "TEXT", NotNull: true},
		{Name: "score", Type: "REAL"},
	}
	cs := NewChangeSet("users", cols)

	if cs.HasChanges() {
		t.Fatal("expected empty changeset")
	}

	// Stage cell edit on rowid 10, col 1 ("name").
	cs.SetCell(10, 1, TextValue("Alice"), TextValue("Alicia"))
	if !cs.HasChanges() || cs.Count() != 1 {
		t.Fatalf("expected 1 change, got %d", cs.Count())
	}
	if v, edited, del := cs.Lookup(10, 1); !edited || del || v.Text != "Alicia" {
		t.Fatalf("unexpected lookup: %+v edited=%v del=%v", v, edited, del)
	}

	// Overwrite same cell: Orig must remain "Alice".
	cs.SetCell(10, 1, TextValue("ignored"), TextValue("Alice B."))
	sql := cs.PreviewSQL()
	if !strings.Contains(sql, `SET "name" = 'Alice B.' WHERE rowid = 10 AND "name" IS 'Alice'`) {
		t.Fatalf("unexpected preview SQL:\n%s", sql)
	}

	// Undo back to "Alicia", then undo back to clean.
	if !cs.Undo() {
		t.Fatal("undo 1 failed")
	}
	if v, edited, _ := cs.Lookup(10, 1); !edited || v.Text != "Alicia" {
		t.Fatalf("after undo 1: %+v edited=%v", v, edited)
	}
	if !cs.Undo() {
		t.Fatal("undo 2 failed")
	}
	if cs.HasChanges() {
		t.Fatal("expected clean changeset after full undo")
	}

	// Redo once -> "Alicia".
	if !cs.Redo() {
		t.Fatal("redo failed")
	}
	if v, edited, _ := cs.Lookup(10, 1); !edited || v.Text != "Alicia" {
		t.Fatalf("after redo: %+v edited=%v", v, edited)
	}
}

func TestChangeSetApplyAndConflict(t *testing.T) {
	path := filepath.Join(t.TempDir(), "edit.db")
	db, err := sqlite3.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if err := db.Exec(`
		CREATE TABLE items (
			id INTEGER PRIMARY KEY,
			title TEXT NOT NULL,
			price REAL
		);
		INSERT INTO items VALUES (1, 'Keyboard', 99.5), (2, 'Mouse', 49.0);
	`); err != nil {
		t.Fatal(err)
	}

	schema, err := engine.LoadSchema(db)
	if err != nil {
		t.Fatal(err)
	}
	obj := schema.Find("items")
	cs := NewChangeSet(obj.Name, obj.Columns)

	// 1. Update row 1, delete row 2, insert a new row.
	cs.SetCell(1, 1, TextValue("Keyboard"), TextValue("Mech Keyboard"))
	cs.SetCell(1, 2, TextValue("99.5"), TextValue("129.0"))
	cs.ToggleDelete(2)
	insID := cs.InsertRow()
	cs.SetCell(insID, 1, NullValue(), TextValue("Monitor"))
	cs.SetCell(insID, 2, NullValue(), TextValue("299.99"))

	if err := cs.Apply(db); err != nil {
		t.Fatalf("Apply failed: %v", err)
	}
	if cs.HasChanges() {
		t.Fatal("expected changeset to be cleared after Apply")
	}

	// Verify DB state.
	stmt, _, err := db.Prepare(`SELECT id, title, price FROM items ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer stmt.Close()
	type row struct {
		id    int64
		title string
		price float64
	}
	var got []row
	for stmt.Step() {
		got = append(got, row{stmt.ColumnInt64(0), stmt.ColumnText(1), stmt.ColumnFloat(2)})
	}
	if len(got) != 2 || got[0].title != "Mech Keyboard" || got[0].price != 129.0 || got[1].title != "Monitor" {
		t.Fatalf("unexpected rows after Apply: %+v", got)
	}

	// 2. Test optimistic concurrency conflict detection: stage an update with a
	// stale original value and verify Apply rolls back and reports a conflict.
	cs.SetCell(1, 1, TextValue("Stale Value"), TextValue("Should Fail"))
	err = cs.Apply(db)
	if err == nil || !strings.Contains(err.Error(), "conflict") {
		t.Fatalf("expected concurrency conflict error, got: %v", err)
	}
}

