package iofmt

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/ncruces/go-sqlite3"

	"github.com/sqlstrata/sqlstrata/internal/sqltok"
)

func TestDumpDatabaseSQLRoundTrip(t *testing.T) {
	src, err := sqlite3.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()

	setupSQL := `
		PRAGMA foreign_keys = ON;
		CREATE TABLE departments (
			id INTEGER PRIMARY KEY,
			code TEXT NOT NULL UNIQUE
		);
		CREATE TABLE employees (
			id INTEGER PRIMARY KEY,
			dept_id INTEGER REFERENCES departments(id),
			name TEXT NOT NULL,
			salary REAL,
			badge BLOB,
			upper_name TEXT GENERATED ALWAYS AS (upper(name)) VIRTUAL
		);
		CREATE INDEX idx_emp_dept ON employees(dept_id);
		CREATE VIEW high_paid AS SELECT id, name, salary FROM employees WHERE salary >= 100.0;
		CREATE TABLE audit_log (msg TEXT);
		CREATE TRIGGER trg_emp_insert AFTER INSERT ON employees BEGIN
			INSERT INTO audit_log(msg) VALUES ('added:' || NEW.name);
		END;

		INSERT INTO departments (id, code) VALUES (10, 'ENG');
		INSERT INTO employees (id, dept_id, name, salary, badge) VALUES
			(1, 10, 'O''Connor' || char(10) || 'Line2', 125.75, X'DEADBEEF'),
			(2, 10, 'Bob', NULL, NULL);
	`
	if err := src.Exec(setupSQL); err != nil {
		t.Fatalf("setup failed: %v", err)
	}

	var dump bytes.Buffer
	dumpedRows, err := DumpDatabaseSQL(context.Background(), src, &dump)
	if err != nil {
		t.Fatalf("DumpDatabaseSQL failed: %v", err)
	}
	if dumpedRows < 3 {
		t.Fatalf("expected >= 3 dumped rows, got %d", dumpedRows)
	}

	// Restore dump into a completely fresh database via StreamStatements.
	dst, err := sqlite3.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()

	if err := sqltok.StreamStatements(bytes.NewReader(dump.Bytes()), func(st sqltok.Statement) error {
		return dst.Exec(st.SQL)
	}); err != nil {
		t.Fatalf("restore dump failed: %v\nSQL:\n%s", err, dump.String())
	}

	// Verify restored data including BLOB, escaped quote+newline, generated column, index, view, and integrity.
	chk, _, err := dst.Prepare(`SELECT name, salary, hex(badge), upper_name FROM employees WHERE id = 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer chk.Close()
	if !chk.Step() {
		t.Fatalf("employee 1 missing after restore: %v", chk.Err())
	}
	wantName := "O'Connor\nLine2"
	if chk.ColumnText(0) != wantName || chk.ColumnFloat(1) != 125.75 || chk.ColumnText(2) != "DEADBEEF" || chk.ColumnText(3) != strings.ToUpper(wantName) {
		t.Fatalf("unexpected restored row: name=%q sal=%v badge=%q upper=%q",
			chk.ColumnText(0), chk.ColumnFloat(1), chk.ColumnText(2), chk.ColumnText(3))
	}

	vStmt, _, err := dst.Prepare(`SELECT COUNT(*) FROM high_paid`)
	if err != nil {
		t.Fatal(err)
	}
	defer vStmt.Close()
	if !vStmt.Step() || vStmt.ColumnInt64(0) != 1 {
		t.Fatalf("expected 1 row in restored view high_paid, got %d", vStmt.ColumnInt64(0))
	}
}

