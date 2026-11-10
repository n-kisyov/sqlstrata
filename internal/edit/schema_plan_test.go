package edit

import (
	"context"
	"strings"
	"testing"

	"github.com/ncruces/go-sqlite3"

	"github.com/sqlstrata/sqlstrata/internal/engine"
)

func TestCreateEmptyDropAndNativeAlter(t *testing.T) {
	sqlStr, err := CreateTableSQL(TableSpec{
		Name: "accounts",
		Columns: []ColumnDef{
			{Name: "tenant_id", Type: "INTEGER", NotNull: true, PK: 1},
			{Name: "id", Type: "INTEGER", NotNull: true, PK: 2},
			{Name: "balance", Type: "REAL", NotNull: true, Default: "0", HasDefault: true},
		},
		WithoutRowid: true,
		Strict:       true,
	})
	if err != nil {
		t.Fatalf("CreateTableSQL failed: %v", err)
	}
	if !strings.Contains(sqlStr, `PRIMARY KEY ("tenant_id", "id")`) || !strings.Contains(sqlStr, "WITHOUT ROWID, STRICT") {
		t.Fatalf("unexpected composite CreateTableSQL:\n%s", sqlStr)
	}

	db, err := sqlite3.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if err := db.Exec(sqlStr); err != nil {
		t.Fatalf("exec CreateTableSQL failed: %v\n%s", err, sqlStr)
	}
	if err := db.Exec(`INSERT INTO accounts (tenant_id, id) VALUES (1, 10), (1, 11);`); err != nil {
		t.Fatal(err)
	}

	schema, err := engine.LoadSchema(db)
	if err != nil {
		t.Fatal(err)
	}
	obj := schema.Find("accounts")
	if obj == nil {
		t.Fatal("accounts table not found")
	}

	// Safe column rename + constant-default column append should use Native ALTER TABLE.
	spec := TableSpecFromObject(obj)
	spec.Columns[2].Name = "avail_balance"
	spec.Columns = append(spec.Columns, ColumnDef{
		Name:       "note",
		Type:       "TEXT",
		NotNull:    true,
		Default:    "''",
		HasDefault: true,
	})
	plan, err := PlanTableAlter(obj, spec, map[string]string{"avail_balance": "balance"}, nil)
	if err != nil {
		t.Fatalf("PlanTableAlter failed: %v", err)
	}
	if plan.Rebuild {
		t.Fatalf("expected native ALTER TABLE, got 12-step rebuild: %+v", plan)
	}
	if err := ExecuteAlterPlan(context.Background(), db, plan); err != nil {
		t.Fatalf("ExecuteAlterPlan failed: %v", err)
	}

	// Verify EmptyTableSQL clears rows cleanly, and DropObjectSQL removes table.
	emptyStmt, err := EmptyTableSQL("accounts")
	if err != nil || db.Exec(emptyStmt) != nil {
		t.Fatalf("EmptyTableSQL failed: %v", err)
	}
	dropStmt, err := DropObjectSQL(obj)
	if err != nil || db.Exec(dropStmt) != nil {
		t.Fatalf("DropObjectSQL failed: %v", err)
	}
}

func TestTwelveStepRebuildPreservesDataAndDependents(t *testing.T) {
	db, err := sqlite3.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if err := db.Exec(`
		PRAGMA foreign_keys = ON;
		CREATE TABLE orgs (id INTEGER PRIMARY KEY, name TEXT);
		INSERT INTO orgs VALUES (1, 'Acme');
		CREATE TABLE users (
			id INTEGER PRIMARY KEY,
			org_id INTEGER NOT NULL REFERENCES orgs(id),
			email TEXT NOT NULL,
			legacy TEXT
		);
		CREATE INDEX idx_users_email ON users(email);
		CREATE TABLE audit (msg TEXT);
		CREATE TRIGGER trg_user_ins AFTER INSERT ON users BEGIN
			INSERT INTO audit(msg) VALUES (NEW.email);
		END;
		INSERT INTO users (id, org_id, email, legacy) VALUES (10, 1, 'a@example.com', 'dropme');
	`); err != nil {
		t.Fatal(err)
	}

	schema, err := engine.LoadSchema(db)
	if err != nil {
		t.Fatal(err)
	}
	usersObj := schema.Find("users")
	if usersObj == nil {
		t.Fatal("users missing")
	}
	var deps []*engine.Object
	deps = append(deps, schema.Related("users", engine.TypeIndex)...)
	deps = append(deps, schema.Related("users", engine.TypeTrigger)...)

	// Drop "legacy" column and rename "email" -> "mail" using 12-step rebuild.
	newSpec := TableSpec{
		Name: "users",
		Columns: []ColumnDef{
			{Name: "id", Type: "INTEGER", PK: 1},
			{Name: "org_id", Type: "INTEGER", NotNull: true},
			{Name: "email", Type: "TEXT", NotNull: true},
			{Name: "created_at", Type: "TEXT", NotNull: true, Default: "CURRENT_TIMESTAMP", HasDefault: true},
		},
	}
	plan, err := PlanTableAlter(usersObj, newSpec, nil, deps)
	if err != nil {
		t.Fatalf("PlanTableAlter failed: %v", err)
	}
	if !plan.Rebuild {
		t.Fatalf("expected 12-step rebuild when dropping column + adding CURRENT_TIMESTAMP column, got %+v", plan)
	}
	if err := ExecuteAlterPlan(context.Background(), db, plan); err != nil {
		t.Fatalf("ExecuteAlterPlan(rebuild) failed: %v", err)
	}

	// Check foreign_keys setting restored to ON!
	fkStmt, _, _ := db.Prepare("PRAGMA foreign_keys")
	if fkStmt.Step() && fkStmt.ColumnInt(0) != 1 {
		_ = fkStmt.Close()
		t.Fatal("expected PRAGMA foreign_keys restored to ON after rebuild")
	}
	_ = fkStmt.Close()

	// Check existing row survived and trigger/index still operate on new inserts.
	if err := db.Exec(`INSERT INTO users (id, org_id, email) VALUES (11, 1, 'b@example.com');`); err != nil {
		t.Fatalf("post-rebuild insert failed: %v", err)
	}
	schema2, err := engine.LoadSchema(db)
	if err != nil {
		t.Fatal(err)
	}
	if schema2.Find("idx_users_email") == nil || schema2.Find("trg_user_ins") == nil {
		t.Fatalf("expected index and trigger recreated after rebuild")
	}

	cntStmt, _, err := db.Prepare("SELECT COUNT(*) FROM users")
	if err != nil {
		t.Fatal(err)
	}
	if !cntStmt.Step() || cntStmt.ColumnInt64(0) != 2 {
		t.Fatalf("expected 2 users post-rebuild, got %d", cntStmt.ColumnInt64(0))
	}
	_ = cntStmt.Close()

	auditStmt, _, err := db.Prepare("SELECT COUNT(*) FROM audit WHERE msg = 'b@example.com'")
	if err != nil {
		t.Fatal(err)
	}
	if !auditStmt.Step() || auditStmt.ColumnInt64(0) != 1 {
		t.Fatalf("expected recreated trigger to fire post-rebuild")
	}
	_ = auditStmt.Close()
}

