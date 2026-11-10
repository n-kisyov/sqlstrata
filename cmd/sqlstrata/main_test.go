package main

import (
	"context"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gioui.org/app"
	"gioui.org/gpu/headless"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/unit"

	"github.com/ncruces/go-sqlite3"

	"github.com/sqlstrata/sqlstrata/internal/data"
	"github.com/sqlstrata/sqlstrata/internal/edit"
	"github.com/sqlstrata/sqlstrata/internal/engine"
)

func TestRenderOpenedDB(t *testing.T) {
	dbPath := filepath.Join("..", "..", "testdata", "small.db")
	fi, err := os.Stat(dbPath)
	if err != nil {
		t.Skipf("small.db not found: %v", err)
	}

	sz := image.Pt(1360, 840)
	hw, err := headless.NewWindow(sz.X, sz.Y)
	if err != nil {
		t.Fatal(err)
	}
	defer hw.Release()

	w := new(app.Window)
	ui := newUI(w)

	db, err := engine.Open(dbPath, engine.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var schema *engine.Schema
	if err := db.Fg.Do(context.Background(), func(c *sqlite3.Conn) error {
		var err error
		schema, err = engine.LoadSchema(c)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	t.Logf("Loaded schema objects: %d (tables=%d, views=%d)",
		len(schema.Objects), len(schema.ByType(engine.TypeTable)), len(schema.ByType(engine.TypeView)))

	ui.db = db
	ui.dbSize = fi.Size()
	ui.schema = schema
	ui.tree.SetSchema(schema)
	tables := schema.ByType(engine.TypeTable)
	if len(tables) > 0 {
		ui.tree.Select(tables[0].Name)
		ch := make(chan struct{}, 1)
		src := data.NewTableSource(db, tables[0], data.SourceOptions{CacheBudget: 32 << 20}, func() {
			select {
			case ch <- struct{}{}:
			default:
			}
		})
		defer src.Close()
		ui.tableSrc = src
		ui.activeTable = tables[0].Name
		ui.browseGrid.SetSource(src)
	}

	// Render frame 1 (requests first page), wait 150ms for async fetch, render frame 2.
	var ops op.Ops
	for i := range 3 {
		ops.Reset()
		gtx := layout.Context{
			Ops:         &ops,
			Metric:      unit.Metric{PxPerDp: 1, PxPerSp: 1},
			Constraints: layout.Exact(sz),
			Now:         time.Now(),
		}
		ui.layout(gtx)
		if err := hw.Frame(&ops); err != nil {
			t.Fatal(err)
		}
		if i < 2 {
			time.Sleep(100 * time.Millisecond)
		}
	}

	img := image.NewRGBA(image.Rectangle{Max: sz})
	if err := hw.Screenshot(img); err != nil {
		t.Fatal(err)
	}
	outPath := `C:\Users\nikolay\.gemini\antigravity\brain\0691d6af-496c-4b4c-b68d-59dbcff66066\scratch\render_check.png`
	_ = os.MkdirAll(filepath.Dir(outPath), 0o755)
	f, err := os.Create(outPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
}

func TestSelectIndexShowsStructure(t *testing.T) {
	dbPath := filepath.Join("..", "..", "testdata", "small.db")
	if _, err := os.Stat(dbPath); err != nil {
		t.Skipf("small.db not found: %v", err)
	}

	sz := image.Pt(1360, 840)
	hw, err := headless.NewWindow(sz.X, sz.Y)
	if err != nil {
		t.Fatal(err)
	}
	defer hw.Release()

	ui := newUI(new(app.Window))
	db, err := engine.Open(dbPath, engine.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var schema *engine.Schema
	if err := db.Fg.Do(context.Background(), func(c *sqlite3.Conn) error {
		var err error
		schema, err = engine.LoadSchema(c)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	ui.db = db
	ui.schema = schema
	ui.tree.SetSchema(schema)

	idx := schema.Find("orders_created")
	if idx == nil || idx.Type != engine.TypeIndex {
		t.Fatalf("orders_created index not found: %+v", idx)
	}
	ui.tree.SelectObject(idx)
	ui.selectObject(idx)

	// Wait briefly for LoadIndexInfo on db.Fg.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ui.mu.Lock()
		done := ui.structIndex != nil
		ui.mu.Unlock()
		if done {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	ui.drainUI()

	if ui.activeTab != tabStructure {
		t.Fatalf("expected activeTab == tabStructure, got %v", ui.activeTab)
	}
	src := ui.structGrid.Source()
	if src == nil || src.RowCount() < 1 {
		t.Fatalf("expected index column rows in structGrid, got %+v", src)
	}
	colName, _, _, ok := src.Cell(0, 1)
	if !ok || colName != "created_at" {
		t.Fatalf("expected index column 0 to be 'created_at', got %q (ok=%v)", colName, ok)
	}

	var ops op.Ops
	gtx := layout.Context{
		Ops:         &ops,
		Metric:      unit.Metric{PxPerDp: 1, PxPerSp: 1},
		Constraints: layout.Exact(sz),
		Now:         time.Now(),
	}
	ui.layout(gtx)
	if err := hw.Frame(&ops); err != nil {
		t.Fatal(err)
	}

	// Verify DDL display strips/expands raw tab characters so Gio never renders tofu boxes.
	viewObj := schema.Find("big_orders")
	if viewObj == nil || viewObj.Type != engine.TypeView {
		t.Fatalf("big_orders view not found: %+v", viewObj)
	}
	ui.loadStructure(viewObj)
	ddl := ui.ddlEdit.Text()
	if strings.ContainsRune(ddl, '\t') {
		t.Fatalf("DDL still contains raw tab characters:\n%s", ddl)
	}
	if !strings.Contains(ddl, "    SELECT") {
		t.Fatalf("expected 4-space indentation in DDL display, got:\n%s", ddl)
	}
}

func TestExecuteSQLAndRunSQLFile(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	db, err := engine.Open(dbPath, engine.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	ui := newUI(new(app.Window))
	defer func() {
		ui.mu.Lock()
		sp := ui.sqlSpool
		ui.sqlSpool = nil
		ui.mu.Unlock()
		if sp != nil {
			_ = sp.Close()
		}
	}()
	ui.db = db

	waitSQL := func() {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			ui.drainUI()
			ui.mu.Lock()
			running := ui.sqlRunning
			ui.mu.Unlock()
			if !running {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("timed out waiting for SQL execution")
	}

	// 1. Multi-row query exceeding staticLimit (2000) should spill to ResultSpool.
	ui.sqlEdit.SetText(`WITH RECURSIVE cnt(x) AS (
		SELECT 1
		UNION ALL
		SELECT x + 1 FROM cnt WHERE x < 2105
	)
	SELECT x, 'row_' || x AS name FROM cnt;`)
	ui.runSQL()
	waitSQL()

	ui.mu.Lock()
	sp := ui.sqlSpool
	status := ui.sqlStatus
	ui.mu.Unlock()
	if sp == nil || sp.RowCount() != 2105 {
		t.Fatalf("expected 2105-row ResultSpool, got sp=%v status=%q", sp, status)
	}
	if got := ui.sqlGrid.Source(); got == nil || got.RowCount() != 2105 {
		t.Fatalf("expected sqlGrid to hold 2105 spooled rows, got %v", got)
	}

	// 2. runSQLFile with PRAGMA + explicit BEGIN/COMMIT + trailing statement.
	scriptPath := filepath.Join(dir, "seed.sql")
	script := `PRAGMA foreign_keys = ON;
CREATE TABLE script_items (id INTEGER PRIMARY KEY, name TEXT);
BEGIN TRANSACTION;
INSERT INTO script_items (id, name) VALUES (1, 'first');
INSERT INTO script_items (id, name) VALUES (2, 'second');
COMMIT;
INSERT INTO script_items (id, name) VALUES (3, 'third');
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	ui.runSQLFile(scriptPath)
	waitSQL()

	// Wait briefly for refreshSchemaAsync triggered by DDL.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		ui.drainUI()
		ui.mu.Lock()
		sc := ui.schema
		ui.mu.Unlock()
		if sc != nil && sc.Find("script_items") != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	ui.mu.Lock()
	st := ui.sqlStatus
	sc := ui.schema
	ui.mu.Unlock()
	if strings.HasPrefix(st, "Error") {
		t.Fatalf("runSQLFile failed: %s", st)
	}
	if sc == nil || sc.Find("script_items") == nil {
		t.Fatalf("expected schema to reload with script_items, status=%q", st)
	}

	var cnt int64
	if err := db.Fg.Do(context.Background(), func(c *sqlite3.Conn) error {
		s, _, err := c.Prepare("SELECT COUNT(*) FROM script_items")
		if err != nil {
			return err
		}
		defer s.Close()
		if s.Step() {
			cnt = s.ColumnInt64(0)
		}
		return s.Err()
	}); err != nil {
		t.Fatal(err)
	}
	if cnt != 3 {
		t.Fatalf("expected 3 rows inserted by runSQLFile, got %d", cnt)
	}
}

func TestTableDesignerAndToolsWorkflow(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "designer.db")
	db, err := engine.Open(dbPath, engine.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	sz := image.Pt(1360, 840)
	hw, err := headless.NewWindow(sz.X, sz.Y)
	if err != nil {
		t.Fatal(err)
	}
	defer hw.Release()

	ui := newUI(new(app.Window))
	defer func() {
		ui.mu.Lock()
		src := ui.tableSrc
		ui.tableSrc = nil
		ui.mu.Unlock()
		if src != nil {
			src.Close()
		}
	}()
	ui.db = db

	waitCond := func(desc string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			ui.drainUI()
			if cond() {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		ui.mu.Lock()
		st := ui.statusMsg
		ui.mu.Unlock()
		t.Fatalf("timed out waiting for %s (status=%q)", desc, st)
	}

	// 1. Open New Table Designer & render headless frame.
	ui.openNewTableDesigner()
	ui.ntTableName.SetText("projects")
	var ops op.Ops
	gtx := layout.Context{
		Ops:         &ops,
		Metric:      unit.Metric{PxPerDp: 1, PxPerSp: 1},
		Constraints: layout.Exact(sz),
		Now:         time.Now(),
	}
	ui.layout(gtx)
	if err := hw.Frame(&ops); err != nil {
		t.Fatal(err)
	}

	spec := ui.buildDesignerTableSpec()
	ui.createTableFromDesigner(spec)
	waitCond("projects table creation", func() bool {
		ui.mu.Lock()
		defer ui.mu.Unlock()
		return ui.schema != nil && ui.schema.Find("projects") != nil && !ui.newTableOpen
	})

	// Seed a row, then add column triggering 12-step rebuild ((0.0) expression default).
	if err := db.Fg.Do(context.Background(), func(c *sqlite3.Conn) error {
		return c.Exec(`INSERT INTO projects (id, name) VALUES (1, 'Atlas');`)
	}); err != nil {
		t.Fatal(err)
	}

	ui.mu.Lock()
	projObj := ui.schema.Find("projects")
	ui.mu.Unlock()
	ui.applyAddColumn(projObj, edit.ColumnDef{
		Name:       "budget",
		Type:       "REAL",
		NotNull:    true,
		Default:    "(0.0)",
		HasDefault: true,
	}, nil)
	waitCond("projects budget column via 12-step rebuild", func() bool {
		ui.mu.Lock()
		defer ui.mu.Unlock()
		o := ui.schema.Find("projects")
		return o != nil && len(o.Columns) == 3
	})

	// 2. Import CSV & Dump SQL via UI helpers.
	csvPath := filepath.Join(dir, "metrics.csv")
	if err := os.WriteFile(csvPath, []byte("day,hits\n2026-01-01,100\n2026-01-02,250\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ui.importCSVFile(csvPath)
	waitCond("imported metrics CSV table", func() bool {
		ui.mu.Lock()
		defer ui.mu.Unlock()
		return ui.schema != nil && ui.schema.Find("metrics") != nil
	})

	dumpPath := filepath.Join(dir, "backup.sql")
	ui.exportSQLDump(dumpPath)
	rawDump, err := os.ReadFile(dumpPath)
	if err != nil {
		t.Fatalf("read exported dump failed: %v", err)
	}
	if !strings.Contains(string(rawDump), `CREATE TABLE "projects"`) || !strings.Contains(string(rawDump), "'Atlas'") {
		t.Fatalf("unexpected SQL dump contents:\n%s", string(rawDump))
	}

	// 3. Empty & Drop projects table.
	ui.mu.Lock()
	projObj2 := ui.schema.Find("projects")
	ui.mu.Unlock()
	ui.emptyTableObject(projObj2)
	waitCond("empty projects table", func() bool {
		ui.mu.Lock()
		defer ui.mu.Unlock()
		return strings.Contains(ui.statusMsg, "Emptied")
	})
	ui.dropSchemaObject(projObj2)
	waitCond("drop projects table", func() bool {
		ui.mu.Lock()
		defer ui.mu.Unlock()
		return ui.schema != nil && ui.schema.Find("projects") == nil
	})
}

func TestGenerateScreenshots(t *testing.T) {
	if os.Getenv("SQLSTRATA_SCREENSHOTS") != "1" {
		t.Skip("set SQLSTRATA_SCREENSHOTS=1 to regenerate docs/screenshots")
	}

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "showcase.db")
	db, err := engine.Open(dbPath, engine.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if err := db.Fg.Do(context.Background(), func(c *sqlite3.Conn) error {
		ddl := `
		PRAGMA foreign_keys = ON;
		CREATE TABLE customers (
			id INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			email TEXT NOT NULL UNIQUE,
			country TEXT NOT NULL,
			created_at INTEGER NOT NULL
		);
		CREATE TABLE orders (
			id INTEGER PRIMARY KEY,
			customer_id INTEGER NOT NULL REFERENCES customers(id),
			product TEXT NOT NULL,
			amount REAL NOT NULL,
			status TEXT NOT NULL DEFAULT 'paid',
			notes TEXT
		);
		CREATE TABLE kv (
			k TEXT PRIMARY KEY,
			v TEXT NOT NULL
		) WITHOUT ROWID, STRICT;
		CREATE INDEX orders_created ON orders(customer_id, status);
		CREATE VIEW high_value_orders AS
			SELECT o.id, c.name AS customer, o.product, o.amount, o.status
			FROM orders o JOIN customers c ON c.id = o.customer_id
			WHERE o.amount >= 250.0;
		CREATE TABLE audit_log (msg TEXT);
		CREATE TRIGGER orders_no_negative BEFORE INSERT ON orders WHEN NEW.amount < 0 BEGIN
			SELECT RAISE(ABORT, 'amount must be >= 0');
		END;
		`
		if err := c.Exec(ddl); err != nil {
			return err
		}
		names := []struct{ n, e, co string }{
			{"Elena Vance", "elena@acme.io", "DE"},
			{"Marcus Chen", "m.chen@northstar.dev", "US"},
			{"Sofia Petrov", "sofia@bgcloud.bg", "BG"},
			{"Kenji Sato", "kenji@tokyo-labs.jp", "JP"},
			{"Lukas Weber", "lukas@alpine.ch", "CH"},
			{"Priya Nair", "priya@bengal.in", "IN"},
			{"Mateo Silva", "mateo@andes.cl", "CL"},
			{"Hannah Abbott", "hannah@thames.uk", "GB"},
			{"Jonas Lind", "jonas@nordic.se", "SE"},
			{"Claire Moreau", "claire@seine.fr", "FR"},
			{"Diego Ruiz", "diego@iberia.es", "ES"},
			{"Nora Kim", "nora@han.kr", "KR"},
			{"Ivan Kovač", "ivan@adria.hr", "HR"},
			{"Mia Jensen", "mia@fjord.dk", "DK"},
			{"Omar Hassan", "omar@nile.eg", "EG"},
		}
		if err := c.Exec("BEGIN"); err != nil {
			return err
		}
		for i, x := range names {
			s, _, err := c.Prepare("INSERT INTO customers (id, name, email, country, created_at) VALUES (?, ?, ?, ?, ?)")
			if err != nil {
				return err
			}
			_ = s.BindInt(1, i+1)
			_ = s.BindText(2, x.n)
			_ = s.BindText(3, x.e)
			_ = s.BindText(4, x.co)
			_ = s.BindInt64(5, 1762800000+int64(i)*86400)
			err = s.Exec()
			_ = s.Close()
			if err != nil {
				return err
			}
		}
		products := []string{"Pro License", "Enterprise Seat", "Team Bundle", "Cloud Sync", "Support Add-on"}
		statuses := []string{"paid", "paid", "pending", "shipped", "paid"}
		for i := 1; i <= 24; i++ {
			cid := ((i - 1) % len(names)) + 1
			prod := products[(i-1)%len(products)]
			st := statuses[(i-1)%len(statuses)]
			amt := 120.0 + float64((i*73)%680)
			note := fmt.Sprintf("Priority #%d (%s)", i, st)
			s, _, err := c.Prepare("INSERT INTO orders (id, customer_id, product, amount, status, notes) VALUES (?, ?, ?, ?, ?, ?)")
			if err != nil {
				return err
			}
			_ = s.BindInt(1, i)
			_ = s.BindInt(2, cid)
			_ = s.BindText(3, prod)
			_ = s.BindFloat(4, amt)
			_ = s.BindText(5, st)
			_ = s.BindText(6, note)
			err = s.Exec()
			_ = s.Close()
			if err != nil {
				return err
			}
		}
		if err := c.Exec(`INSERT INTO kv (k, v) VALUES ('schema_ver', '3'), ('wal_mode', 'enabled');`); err != nil {
			return err
		}
		return c.Exec("COMMIT")
	}); err != nil {
		t.Fatal(err)
	}

	sz := image.Pt(1400, 780)
	hw, err := headless.NewWindow(sz.X, sz.Y)
	if err != nil {
		t.Fatal(err)
	}
	defer hw.Release()

	ui := newUI(new(app.Window))
	defer func() {
		ui.mu.Lock()
		src := ui.tableSrc
		sp := ui.sqlSpool
		ui.tableSrc = nil
		ui.sqlSpool = nil
		ui.mu.Unlock()
		if src != nil {
			src.Close()
		}
		if sp != nil {
			_ = sp.Close()
		}
	}()

	var schema *engine.Schema
	if err := db.Fg.Do(context.Background(), func(c *sqlite3.Conn) error {
		var err error
		schema, err = engine.LoadSchema(c)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	ui.db = db
	if fi, serr := os.Stat(dbPath); serr == nil {
		ui.dbSize = fi.Size()
	}
	ui.schema = schema
	ui.leftSplit.Size = 215
	ui.rightSplit.Size = 295
	ui.statusMsg = "Opened showcase.db in 2ms"
	ui.tree.SetSchema(schema)

	outDir := filepath.Join("..", "..", "docs", "screenshots")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}

	renderFrame := func() {
		var ops op.Ops
		gtx := layout.Context{
			Ops:         &ops,
			Metric:      unit.Metric{PxPerDp: 1, PxPerSp: 1},
			Constraints: layout.Exact(sz),
			Now:         time.Now(),
		}
		ui.layout(gtx)
		if err := hw.Frame(&ops); err != nil {
			t.Fatal(err)
		}
	}
	saveShot := func(name string) {
		renderFrame()
		img := image.NewRGBA(image.Rectangle{Max: sz})
		if err := hw.Screenshot(img); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(outDir, name)
		f, err := os.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if err := png.Encode(f, img); err != nil {
			t.Fatal(err)
		}
	}

	// 1. Browse Data + Cell Inspector screenshot.
	custObj := schema.Find("customers")
	ui.tree.SelectObject(custObj)
	ui.selectObject(custObj)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		renderFrame()
		if ui.tableSrc != nil {
			if _, _, _, ok := ui.tableSrc.Cell(0, 0); ok {
				break
			}
		}
		time.Sleep(15 * time.Millisecond)
	}
	ui.rightSplit.Collapsed = false
	ui.browseGrid.OnSelectCell(2, 4) // created_at (Unix timestamp hint in Inspector)
	renderFrame()
	saveShot("browse_inspector.png")

	// 2. Execute SQL screenshot (wide view).
	ui.activeTab = tabSQL
	ui.rightSplit.Collapsed = true
	ui.sqlEdit.SetText("SELECT o.id, c.name AS customer, c.country, o.product, o.amount, o.status\nFROM orders o\nJOIN customers c ON c.id = o.customer_id\nWHERE o.amount >= 250\nORDER BY o.amount DESC;")
	ui.runSQL()
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		renderFrame()
		ui.mu.Lock()
		running := ui.sqlRunning
		ui.mu.Unlock()
		if !running {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	renderFrame()
	saveShot("sql_runner.png")

	// 3. Structure / Schema & DDL screenshot (wide view).
	ordersObj := schema.Find("orders")
	ui.tree.SelectObject(ordersObj)
	ui.loadStructure(ordersObj)
	ui.activeTab = tabStructure
	ui.rightSplit.Collapsed = true
	renderFrame()
	saveShot("structure_view.png")
}




