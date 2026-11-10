package main

import (
	"context"
	"image"
	"image/png"
	"os"
	"path/filepath"
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

