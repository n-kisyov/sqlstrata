package main

import (
	"context"
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

