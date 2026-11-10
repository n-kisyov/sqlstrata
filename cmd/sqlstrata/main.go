// Command sqlstrata is a native, memory-bounded SQLite GUI editor for Windows 11.
package main

import (
	"context"
	"flag"
	"fmt"
	"image"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"gioui.org/app"
	"gioui.org/font"
	"gioui.org/gesture"
	"gioui.org/io/clipboard"
	"gioui.org/io/key"
	"gioui.org/io/pointer"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/text"
	"gioui.org/unit"
	"gioui.org/widget"

	"github.com/ncruces/go-sqlite3"

	"github.com/sqlstrata/sqlstrata/internal/data"
	"github.com/sqlstrata/sqlstrata/internal/edit"
	"github.com/sqlstrata/sqlstrata/internal/engine"
	"github.com/sqlstrata/sqlstrata/internal/platform/win"
	"github.com/sqlstrata/sqlstrata/internal/ui/theme"
	"github.com/sqlstrata/sqlstrata/internal/ui/widgets"
)

func main() {
	flag.Parse()
	initialPath := flag.Arg(0)

	go func() {
		w := new(app.Window)
		w.Option(
			app.Title("SQLStrata"),
			app.Size(unit.Dp(1360), unit.Dp(840)),
			app.MinSize(unit.Dp(720), unit.Dp(480)),
		)
		ui := newUI(w)
		if initialPath != "" {
			ui.openDatabase(initialPath)
		}
		if err := ui.run(); err != nil {
			log.Fatal(err)
		}
		os.Exit(0)
	}()
	app.Main()
}

type viewTab uint8

const (
	tabBrowse viewTab = iota
	tabSQL
	tabStructure
	tabReview
)

type uiState struct {
	win  *app.Window
	hwnd uintptr
	th   *theme.Theme

	leftSplit   widgets.Splitter
	rightSplit  widgets.Splitter
	sqlSplit    widgets.Splitter
	structSplit widgets.Splitter

	tree       *widgets.TreeView
	browseGrid *widgets.DataGrid
	sqlGrid    *widgets.DataGrid
	structGrid *widgets.DataGrid
	inspector  *widgets.Inspector

	activeTab  viewTab
	sqlEdit    widget.Editor
	ddlEdit    widget.Editor
	filterEdit widget.Editor
	gotoEdit   widget.Editor
	reviewList layout.List

	// Top bar buttons.
	btnOpen         gesture.Click
	btnDemo         gesture.Click
	btnTabBrowse    gesture.Click
	btnTabSQL       gesture.Click
	btnTabStructure gesture.Click
	btnTabReview    gesture.Click
	btnRunSQL       gesture.Click
	btnCancelSQL    gesture.Click
	btnSidebar      gesture.Click
	btnInspector    gesture.Click
	btnTheme        gesture.Click

	// Structure view buttons.
	btnBrowseTarget gesture.Click
	btnQueryStruct  gesture.Click
	btnCopyDDL      gesture.Click
	structRelClicks []gesture.Click

	// Browse sub-toolbar buttons.
	btnApplyFilter gesture.Click
	btnClearFilter gesture.Click
	btnAddRow      gesture.Click
	btnUndo        gesture.Click
	btnRedo        gesture.Click
	btnCommit      gesture.Click
	btnDiscard     gesture.Click

	// Command palette overlay.
	paletteOpen   bool
	paletteEdit   widget.Editor
	paletteList   layout.List
	paletteClicks []gesture.Click
	paletteBg     gesture.Click

	mu          sync.Mutex
	db          *engine.DB
	dbSize      int64
	schema      *engine.Schema
	tableSrc    *data.TableSource
	activeTable string
	structObj   *engine.Object
	structIndex *engine.IndexInfo
	structRel   []*engine.Object
	statusMsg   string
	sqlRunning  bool
	sqlCancel   context.CancelFunc
	sqlStatus   string
}

func newUI(w *app.Window) *uiState {
	th := theme.New()
	u := &uiState{
		win:         w,
		th:          th,
		leftSplit:   widgets.Splitter{Axis: layout.Horizontal, Size: 250, Min: 170, Max: 520},
		rightSplit:  widgets.Splitter{Axis: layout.Horizontal, Size: 320, Min: 220, Max: 560, SecondFixed: true},
		sqlSplit:    widgets.Splitter{Axis: layout.Vertical, Size: 180, Min: 90, Max: 520},
		structSplit: widgets.Splitter{Axis: layout.Vertical, Size: 230, Min: 100, Max: 520},
		tree:        widgets.NewTreeView(),
		browseGrid:  widgets.NewDataGrid(),
		sqlGrid:     widgets.NewDataGrid(),
		structGrid:  widgets.NewDataGrid(),
		inspector:   widgets.NewInspector(),
		reviewList:  layout.List{Axis: layout.Vertical},
		paletteList: layout.List{Axis: layout.Vertical},
		statusMsg:   "Ready · Open a SQLite file (Ctrl+O) or try the 100M-row Demo",
	}
	u.ddlEdit.ReadOnly = true
	u.filterEdit.SingleLine = true
	u.filterEdit.Submit = true
	u.gotoEdit.SingleLine = true
	u.gotoEdit.Submit = true
	u.paletteEdit.SingleLine = true
	u.paletteEdit.Submit = true

	u.sqlEdit.SetText("SELECT id, customer_id, amount, status, created_at\nFROM orders\nWHERE amount > 950\nORDER BY id DESC\nLIMIT 500;")

	u.tree.OnSelect = func(obj *engine.Object) {
		u.selectObject(obj)
	}
	u.browseGrid.OnSelectCell = func(row int64, col int) {
		u.inspectFrom(u.browseGrid, row, col)
	}
	u.sqlGrid.OnSelectCell = func(row int64, col int) {
		u.inspectFrom(u.sqlGrid, row, col)
	}
	u.inspector.OnStageCell = func(row int64, col int, val edit.Value) {
		u.mu.Lock()
		src := u.tableSrc
		u.mu.Unlock()
		if src != nil && src.StageCellEdit(row, col, val) {
			u.inspectFrom(u.browseGrid, row, col)
			u.win.Invalidate()
		}
	}
	u.inspector.OnDeleteRow = func(row int64) {
		u.mu.Lock()
		src := u.tableSrc
		u.mu.Unlock()
		if src != nil && src.Editable() {
			if rowid, ok := src.RowKey(row); ok {
				src.Changes().ToggleDelete(rowid)
				u.win.Invalidate()
			}
		}
	}
	return u
}

func (u *uiState) inspectFrom(g *widgets.DataGrid, row int64, col int) {
	src := g.Source()
	if src == nil {
		return
	}
	cols := src.Columns()
	if col < 0 || col >= len(cols) {
		return
	}
	val, kind, trunc, ok := src.Cell(row, col)
	if !ok {
		return
	}
	ts, isTable := src.(*data.TableSource)
	editable := isTable && ts.Editable()
	u.inspector.SetCell(row, col, cols[col], val, kind, trunc, editable)

	// Stream the full value (up to 64 KB) when inspecting a TableSource cell
	// that is either a BLOB or truncated text.
	if isTable && (kind == data.KindBlob || trunc) {
		colMeta := cols[col]
		ts.FetchCellDetail(row, col, 64*1024, func(det data.CellDetail, err error) {
			if err == nil {
				u.inspector.SetDetail(det, colMeta, editable)
				u.win.Invalidate()
			}
		})
	}
}

func (u *uiState) applyWindowStyle() {
	if u.hwnd != 0 {
		win.StyleWindow(u.hwnd, u.th.Dark, u.th.Chrome, u.th.Text, u.th.Border)
	}
}

func (u *uiState) openDialog() {
	hwnd := u.hwnd
	go func() {
		path, ok := win.OpenFile(hwnd, "Open SQLite Database", []win.Filter{
			{Name: "SQLite Databases (*.db;*.sqlite;*.sqlite3)", Pattern: "*.db;*.sqlite;*.sqlite3;*.db3"},
			{Name: "All Files (*.*)", Pattern: "*.*"},
		})
		if ok && path != "" {
			u.openDatabase(path)
		}
	}()
}

func (u *uiState) openDatabase(path string) {
	go func() {
		start := time.Now()
		fi, err := os.Stat(path)
		if err != nil {
			u.setStatus(fmt.Sprintf("Open failed: %v", err))
			return
		}
		db, err := engine.Open(path, engine.DefaultOptions())
		if err != nil {
			u.setStatus(fmt.Sprintf("Open failed: %v", err))
			return
		}
		var schema *engine.Schema
		err = db.Fg.Do(context.Background(), func(c *sqlite3.Conn) error {
			var err error
			schema, err = engine.LoadSchema(c)
			return err
		})
		if err != nil {
			db.Close()
			u.setStatus(fmt.Sprintf("Schema error: %v", err))
			return
		}

		u.mu.Lock()
		oldDB := u.db
		oldSrc := u.tableSrc
		u.db = db
		u.dbSize = fi.Size()
		u.schema = schema
		u.tableSrc = nil
		u.activeTable = ""
		u.statusMsg = fmt.Sprintf("Opened %s in %s", filepath.Base(path), time.Since(start).Round(time.Millisecond))
		u.mu.Unlock()

		if oldSrc != nil {
			oldSrc.Close()
		}
		if oldDB != nil {
			oldDB.Close()
		}

		u.tree.SetSchema(schema)
		u.win.Option(app.Title("SQLStrata — " + filepath.Base(path)))

		if tables := schema.ByType(engine.TypeTable); len(tables) > 0 {
			u.tree.SelectObject(tables[0])
			u.selectObject(tables[0])
		} else if len(schema.Objects) > 0 {
			u.tree.SelectObject(schema.Objects[0])
			u.selectObject(schema.Objects[0])
		}
		u.win.Invalidate()
	}()
}

func (u *uiState) selectObject(obj *engine.Object) {
	if obj == nil {
		return
	}
	u.mu.Lock()
	db := u.db
	oldSrc := u.tableSrc
	u.mu.Unlock()
	if db == nil {
		return
	}

	u.loadStructure(obj)

	if obj.Type == engine.TypeIndex || obj.Type == engine.TypeTrigger {
		u.mu.Lock()
		u.activeTab = tabStructure
		if obj.Table != "" {
			u.statusMsg = fmt.Sprintf("%s %s on %s", strings.ToUpper(string(obj.Type)), obj.Name, obj.Table)
		} else {
			u.statusMsg = fmt.Sprintf("%s %s", strings.ToUpper(string(obj.Type)), obj.Name)
		}
		u.mu.Unlock()
		u.win.Invalidate()
		return
	}

	if oldSrc != nil {
		oldSrc.Close()
	}
	src := data.NewTableSource(db, obj, data.SourceOptions{CacheBudget: 32 << 20}, func() {
		u.win.Invalidate()
	})
	u.mu.Lock()
	u.tableSrc = src
	u.activeTable = obj.Name
	u.activeTab = tabBrowse
	u.mu.Unlock()

	u.filterEdit.SetText("")
	u.gotoEdit.SetText("")
	u.browseGrid.SetSource(src)
	u.inspector.Clear()
	u.win.Invalidate()
}

func (u *uiState) loadStructure(obj *engine.Object) {
	u.mu.Lock()
	db := u.db
	schema := u.schema
	u.structObj = obj
	u.structIndex = nil
	tableName := obj.Name
	if obj.Table != "" {
		tableName = obj.Table
	}
	var rel []*engine.Object
	if schema != nil && tableName != "" {
		rel = append(rel, schema.Related(tableName, engine.TypeIndex)...)
		rel = append(rel, schema.Related(tableName, engine.TypeTrigger)...)
	}
	u.structRel = rel
	u.mu.Unlock()

	ddl := strings.TrimSpace(obj.SQL)
	if ddl != "" && !strings.HasSuffix(ddl, ";") {
		ddl += ";"
	}
	if ddl == "" {
		ddl = "-- (Internal or auto-generated SQLite schema object)"
	}
	u.ddlEdit.SetText(ddl)

	switch obj.Type {
	case engine.TypeTable, engine.TypeView:
		u.structGrid.SetSource(buildTableStructSource(obj))
	case engine.TypeTrigger:
		u.structGrid.SetSource(buildTriggerStructSource(obj))
	case engine.TypeIndex:
		u.structGrid.SetSource(buildIndexStructSource(obj, nil))
		if db != nil {
			db.Fg.Go(context.Background(), func(c *sqlite3.Conn) error {
				info, err := engine.LoadIndexInfo(c, obj)
				if err != nil {
					return err
				}
				u.mu.Lock()
				stillSelected := u.structObj == obj
				if stillSelected {
					u.structIndex = info
				}
				u.mu.Unlock()
				if stillSelected {
					u.structGrid.SetSource(buildIndexStructSource(obj, info))
					u.win.Invalidate()
				}
				return nil
			}, nil)
		}
	}
}

func buildTableStructSource(obj *engine.Object) *data.StaticSource {
	src := data.NewStaticSource([]data.Column{
		{Name: "cid", Type: "INTEGER", PK: true},
		{Name: "column", Type: "TEXT"},
		{Name: "type", Type: "TEXT"},
		{Name: "nullable", Type: "TEXT"},
		{Name: "default", Type: "TEXT"},
		{Name: "pk", Type: "INTEGER"},
		{Name: "flags", Type: "TEXT"},
	})
	for i, c := range obj.Columns {
		src.AppendCell([]byte(strconv.Itoa(i)), data.KindInteger)
		src.AppendCell([]byte(c.Name), data.KindText)
		if c.Type != "" {
			src.AppendCell([]byte(c.Type), data.KindText)
		} else {
			src.AppendCell(nil, data.KindNull)
		}
		if c.NotNull {
			src.AppendCell([]byte("NOT NULL"), data.KindText)
		} else {
			src.AppendCell([]byte("NULL"), data.KindText)
		}
		if c.HasDflt {
			src.AppendCell([]byte(c.Default), data.KindText)
		} else {
			src.AppendCell(nil, data.KindNull)
		}
		if c.PK > 0 {
			src.AppendCell([]byte(strconv.Itoa(c.PK)), data.KindInteger)
		} else {
			src.AppendCell(nil, data.KindNull)
		}
		var flags []string
		if obj.RowidAlias != "" && c.Name == obj.RowidAlias {
			flags = append(flags, "ROWID ALIAS")
		}
		switch c.Hidden {
		case 2:
			flags = append(flags, "GENERATED VIRTUAL")
		case 3:
			flags = append(flags, "GENERATED STORED")
		}
		if len(flags) > 0 {
			src.AppendCell([]byte(strings.Join(flags, ", ")), data.KindText)
		} else {
			src.AppendCell(nil, data.KindNull)
		}
	}
	return src
}

func buildIndexStructSource(obj *engine.Object, info *engine.IndexInfo) *data.StaticSource {
	src := data.NewStaticSource([]data.Column{
		{Name: "seqno", Type: "INTEGER", PK: true},
		{Name: "column", Type: "TEXT"},
		{Name: "order", Type: "TEXT"},
		{Name: "collation", Type: "TEXT"},
		{Name: "role", Type: "TEXT"},
	})
	if info == nil {
		return src
	}
	for _, c := range info.Columns {
		src.AppendCell([]byte(strconv.Itoa(c.Seqno)), data.KindInteger)
		src.AppendCell([]byte(c.Name), data.KindText)
		if c.Desc {
			src.AppendCell([]byte("DESC"), data.KindText)
		} else {
			src.AppendCell([]byte("ASC"), data.KindText)
		}
		if c.Coll != "" {
			src.AppendCell([]byte(c.Coll), data.KindText)
		} else {
			src.AppendCell(nil, data.KindNull)
		}
		if c.Key {
			src.AppendCell([]byte("Index Key"), data.KindText)
		} else {
			src.AppendCell([]byte("Auxiliary (locator)"), data.KindText)
		}
	}
	return src
}

func buildTriggerStructSource(obj *engine.Object) *data.StaticSource {
	src := data.NewStaticSource([]data.Column{
		{Name: "property", Type: "TEXT"},
		{Name: "value", Type: "TEXT"},
	})
	src.AppendCell([]byte("Object Type"), data.KindText)
	src.AppendCell([]byte("Trigger"), data.KindText)
	src.AppendCell([]byte("Name"), data.KindText)
	src.AppendCell([]byte(obj.Name), data.KindText)
	src.AppendCell([]byte("Target Table"), data.KindText)
	src.AppendCell([]byte(obj.Table), data.KindText)
	return src
}

func (u *uiState) loadDemo100M() {
	u.mu.Lock()
	if u.tableSrc != nil {
		u.tableSrc.Close()
		u.tableSrc = nil
	}
	u.activeTable = "synthetic_100M"
	u.statusMsg = "100,000,000-row virtualized demo active — drag the scrollbar anywhere"
	u.mu.Unlock()

	u.browseGrid.SetSource(data.NewDemo100MSource(100_000_000))
	u.inspector.Clear()
	u.activeTab = tabBrowse
	u.win.Invalidate()
}

func (u *uiState) commitChanges() {
	u.mu.Lock()
	db := u.db
	src := u.tableSrc
	u.mu.Unlock()
	if db == nil || src == nil || !src.Editable() || !src.Changes().HasChanges() {
		return
	}
	n := src.Changes().Count()
	go func() {
		start := time.Now()
		err := db.Fg.Do(context.Background(), func(c *sqlite3.Conn) error {
			return src.Changes().Apply(c)
		})
		if err != nil {
			u.setStatus(fmt.Sprintf("Commit failed: %v", err))
			return
		}
		src.Reload()
		u.mu.Lock()
		u.activeTab = tabBrowse
		u.statusMsg = fmt.Sprintf("Committed %d change(s) in %s", n, time.Since(start).Round(time.Microsecond))
		u.mu.Unlock()
		u.win.Invalidate()
	}()
}

func (u *uiState) discardChanges() {
	u.mu.Lock()
	src := u.tableSrc
	if src != nil && src.Editable() {
		src.Changes().Clear()
	}
	if u.activeTab == tabReview {
		u.activeTab = tabBrowse
	}
	u.statusMsg = "Discarded staged changes"
	u.mu.Unlock()
	u.win.Invalidate()
}

func (u *uiState) runSQL() {
	u.mu.Lock()
	db := u.db
	if db == nil {
		u.sqlStatus = "Open a database first to run SQL."
		u.mu.Unlock()
		u.win.Invalidate()
		return
	}
	if u.sqlRunning {
		u.mu.Unlock()
		return
	}
	sqlText := strings.TrimSpace(u.sqlEdit.Text())
	if sqlText == "" {
		u.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	u.sqlRunning = true
	u.sqlCancel = cancel
	u.sqlStatus = "Running query…"
	u.mu.Unlock()
	u.win.Invalidate()

	go func() {
		start := time.Now()
		var res *data.StaticSource
		var rowsRead int64
		const rowCap = 10_000

		err := db.Fg.Do(ctx, func(c *sqlite3.Conn) error {
			stmt, _, err := c.Prepare(sqlText)
			if err != nil {
				return err
			}
			defer stmt.Close()

			ncols := stmt.ColumnCount()
			if ncols == 0 {
				return stmt.Exec()
			}
			cols := make([]data.Column, ncols)
			for i := range ncols {
				cols[i] = data.Column{
					Name: stmt.ColumnName(i),
					Type: stmt.ColumnDeclType(i),
				}
			}
			res = data.NewStaticSource(cols)
			for stmt.Step() {
				for i := range ncols {
					switch stmt.ColumnType(i) {
					case sqlite3.NULL:
						res.AppendCell(nil, data.KindNull)
					case sqlite3.INTEGER:
						res.AppendCell(stmt.ColumnRawText(i), data.KindInteger)
					case sqlite3.FLOAT:
						res.AppendCell(stmt.ColumnRawText(i), data.KindReal)
					case sqlite3.TEXT:
						v := stmt.ColumnRawText(i)
						if len(v) > data.PreviewBytes {
							v = v[:data.PreviewBytes]
						}
						res.AppendCell(v, data.KindText)
					case sqlite3.BLOB:
						msg := "BLOB " + data.FormatSize(int64(len(stmt.ColumnRawBlob(i))))
						res.AppendCell([]byte(msg), data.KindBlob)
					}
				}
				rowsRead++
				if rowsRead >= rowCap {
					break
				}
			}
			return stmt.Err()
		})
		cancel()

		u.mu.Lock()
		u.sqlRunning = false
		u.sqlCancel = nil
		if err != nil {
			u.sqlStatus = fmt.Sprintf("Error: %v", err)
		} else if res != nil {
			suffix := ""
			if rowsRead >= rowCap {
				suffix = fmt.Sprintf(" (capped at %d rows)", rowCap)
			}
			u.sqlStatus = fmt.Sprintf("%d rows in %s%s", rowsRead, time.Since(start).Round(time.Microsecond), suffix)
			u.sqlGrid.SetSource(res)
		} else {
			u.sqlStatus = fmt.Sprintf("Statement executed in %s", time.Since(start).Round(time.Microsecond))
		}
		u.mu.Unlock()
		u.win.Invalidate()
	}()
}

func (u *uiState) setStatus(msg string) {
	u.mu.Lock()
	u.statusMsg = msg
	u.mu.Unlock()
	u.win.Invalidate()
}

func (u *uiState) run() error {
	var ops op.Ops
	for {
		switch e := u.win.Event().(type) {
		case app.DestroyEvent:
			u.mu.Lock()
			if u.tableSrc != nil {
				u.tableSrc.Close()
			}
			if u.db != nil {
				u.db.Close()
			}
			u.mu.Unlock()
			return e.Err
		case app.Win32ViewEvent:
			u.hwnd = e.HWND
			u.applyWindowStyle()
		case app.FrameEvent:
			gtx := app.NewContext(&ops, e)
			u.layout(gtx)
			e.Frame(gtx.Ops)
		}
	}
}

func (u *uiState) layout(gtx layout.Context) layout.Dimensions {
	theme.Fill(gtx, u.th.Window)

	// Global keyboard shortcuts.
	for {
		ev, ok := gtx.Event(
			key.Filter{Name: "O", Required: key.ModCtrl},
			key.Filter{Name: "B", Required: key.ModCtrl},
			key.Filter{Name: "I", Required: key.ModCtrl},
			key.Filter{Name: "S", Required: key.ModCtrl},
			key.Filter{Name: "Z", Required: key.ModCtrl},
			key.Filter{Name: "Y", Required: key.ModCtrl},
			key.Filter{Name: "P", Required: key.ModCtrl, Optional: key.ModShift},
			key.Filter{Name: key.NameReturn, Required: key.ModCtrl},
			key.Filter{Name: key.NameEnter, Required: key.ModCtrl},
			key.Filter{Name: key.NameF5},
			key.Filter{Name: key.NameEscape},
		)
		if !ok {
			break
		}
		ke, ok := ev.(key.Event)
		if !ok || ke.State != key.Press {
			continue
		}
		switch ke.Name {
		case "O":
			u.openDialog()
		case "B":
			u.leftSplit.Collapsed = !u.leftSplit.Collapsed
		case "I":
			u.rightSplit.Collapsed = !u.rightSplit.Collapsed
		case "S":
			u.mu.Lock()
			src := u.tableSrc
			u.mu.Unlock()
			if src != nil && src.Editable() && src.Changes().HasChanges() {
				u.activeTab = tabReview
			}
		case "Z":
			u.mu.Lock()
			src := u.tableSrc
			u.mu.Unlock()
			if src != nil && src.Editable() {
				src.Changes().Undo()
			}
		case "Y":
			u.mu.Lock()
			src := u.tableSrc
			u.mu.Unlock()
			if src != nil && src.Editable() {
				src.Changes().Redo()
			}
		case "P":
			u.paletteOpen = !u.paletteOpen
			if u.paletteOpen {
				u.paletteEdit.SetText("")
				gtx.Execute(key.FocusCmd{Tag: &u.paletteEdit})
			}
		case key.NameEscape:
			if u.paletteOpen {
				u.paletteOpen = false
			}
		case key.NameReturn, key.NameEnter, key.NameF5:
			if u.activeTab == tabSQL {
				u.runSQL()
			}
		}
	}

	dims := layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return u.layoutToolbar(gtx)
		}),
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			return u.leftSplit.Layout(gtx, u.th,
				func(gtx layout.Context) layout.Dimensions {
					return u.tree.Layout(gtx, u.th)
				},
				func(gtx layout.Context) layout.Dimensions {
					return u.rightSplit.Layout(gtx, u.th,
						func(gtx layout.Context) layout.Dimensions {
							switch u.activeTab {
							case tabSQL:
								return u.layoutSQLView(gtx)
							case tabStructure:
								return u.layoutStructureView(gtx)
							case tabReview:
								return u.layoutReviewView(gtx)
							default:
								return u.layoutBrowseView(gtx)
							}
						},
						func(gtx layout.Context) layout.Dimensions {
							return u.inspector.Layout(gtx, u.th)
						},
					)
				},
			)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return u.layoutStatusBar(gtx)
		}),
	)

	if u.paletteOpen {
		u.layoutCommandPalette(gtx)
	}
	return dims
}

func (u *uiState) layoutToolbar(gtx layout.Context) layout.Dimensions {
	h := gtx.Dp(40)
	w := gtx.Constraints.Max.X
	theme.FillRect(gtx, clip.Rect{Max: image.Pt(w, h)}, u.th.Chrome)

	if clicked(&u.btnOpen, gtx) {
		u.openDialog()
	}
	if clicked(&u.btnDemo, gtx) {
		u.loadDemo100M()
	}
	if clicked(&u.btnTabBrowse, gtx) {
		u.activeTab = tabBrowse
	}
	if clicked(&u.btnTabSQL, gtx) {
		u.activeTab = tabSQL
	}
	if clicked(&u.btnTabStructure, gtx) {
		u.activeTab = tabStructure
	}
	if clicked(&u.btnTabReview, gtx) {
		u.activeTab = tabReview
	}
	if clicked(&u.btnSidebar, gtx) {
		u.leftSplit.Collapsed = !u.leftSplit.Collapsed
	}
	if clicked(&u.btnInspector, gtx) {
		u.rightSplit.Collapsed = !u.rightSplit.Collapsed
	}
	if clicked(&u.btnTheme, gtx) {
		accent := u.th.Accent
		if a, ok := win.AccentColor(); ok {
			accent = a
		}
		u.th.SetDark(!u.th.Dark, accent)
		u.applyWindowStyle()
	}

	x := gtx.Dp(10)
	x += u.drawPillButton(gtx, x, h, "Open DB…", false, false, &u.btnOpen) + gtx.Dp(6)
	x += u.drawPillButton(gtx, x, h, "100M Demo", u.activeTable == "synthetic_100M", false, &u.btnDemo) + gtx.Dp(14)

	theme.FillRect(gtx, clip.Rect{Min: image.Pt(x, gtx.Dp(10)), Max: image.Pt(x+1, h-gtx.Dp(10))}, u.th.Border)
	x += gtx.Dp(14)

	x += u.drawPillButton(gtx, x, h, "Browse Data", u.activeTab == tabBrowse, true, &u.btnTabBrowse) + gtx.Dp(6)
	x += u.drawPillButton(gtx, x, h, "Execute SQL", u.activeTab == tabSQL, true, &u.btnTabSQL) + gtx.Dp(6)
	x += u.drawPillButton(gtx, x, h, "Structure", u.activeTab == tabStructure, true, &u.btnTabStructure) + gtx.Dp(6)

	u.mu.Lock()
	src := u.tableSrc
	u.mu.Unlock()
	if src != nil && src.Editable() {
		if n := src.Changes().Count(); n > 0 {
			lbl := fmt.Sprintf("Review Changes (%d)", n)
			x += u.drawPillButton(gtx, x, h, lbl, u.activeTab == tabReview, true, &u.btnTabReview) + gtx.Dp(6)
		}
	}

	// Right-aligned controls.
	rx := w - gtx.Dp(10)
	themeLabel := "Dark"
	if !u.th.Dark {
		themeLabel = "Light"
	}
	rx -= gtx.Dp(68)
	u.drawPillButton(gtx, rx, h, themeLabel, false, false, &u.btnTheme)

	rx -= gtx.Dp(96)
	u.drawPillButton(gtx, rx, h, "Inspector", !u.rightSplit.Collapsed, false, &u.btnInspector)

	rx -= gtx.Dp(86)
	u.drawPillButton(gtx, rx, h, "Sidebar", !u.leftSplit.Collapsed, false, &u.btnSidebar)

	theme.FillRect(gtx, clip.Rect{Min: image.Pt(0, h-1), Max: image.Pt(w, h)}, u.th.Border)
	return layout.Dimensions{Size: image.Pt(w, h)}
}

func (u *uiState) layoutBrowseView(gtx layout.Context) layout.Dimensions {
	u.mu.Lock()
	src := u.tableSrc
	u.mu.Unlock()
	if src == nil {
		return u.browseGrid.Layout(gtx, u.th)
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return u.layoutBrowseBar(gtx, src)
		}),
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			return u.browseGrid.Layout(gtx, u.th)
		}),
	)
}

func (u *uiState) layoutBrowseBar(gtx layout.Context, src *data.TableSource) layout.Dimensions {
	h := gtx.Dp(36)
	w := gtx.Constraints.Max.X
	theme.FillRect(gtx, clip.Rect{Max: image.Pt(w, h)}, u.th.SurfaceAlt)

	for {
		ev, ok := u.filterEdit.Update(gtx)
		if !ok {
			break
		}
		if _, ok := ev.(widget.SubmitEvent); ok {
			src.SetFilter(u.filterEdit.Text())
		}
	}
	for {
		ev, ok := u.gotoEdit.Update(gtx)
		if !ok {
			break
		}
		if _, ok := ev.(widget.SubmitEvent); ok {
			if n, err := strconv.ParseInt(strings.TrimSpace(u.gotoEdit.Text()), 10, 64); err == nil && n >= 1 {
				u.browseGrid.ScrollToRow(n-1, 1)
			}
		}
	}
	if clicked(&u.btnApplyFilter, gtx) {
		src.SetFilter(u.filterEdit.Text())
	}
	if clicked(&u.btnClearFilter, gtx) {
		u.filterEdit.SetText("")
		src.SetFilter("")
	}
	if clicked(&u.btnAddRow, gtx) && src.Editable() {
		src.Changes().InsertRow()
		total := src.RowCount()
		if total > 0 {
			u.browseGrid.ScrollToRow(total-1, 20)
		}
	}
	if clicked(&u.btnUndo, gtx) && src.Editable() {
		src.Changes().Undo()
	}
	if clicked(&u.btnRedo, gtx) && src.Editable() {
		src.Changes().Redo()
	}

	x := gtx.Dp(8)
	// Filter input box.
	fltW := min(gtx.Dp(280), max(gtx.Dp(120), w/3))
	u.drawInputBox(gtx, x, h, fltW, &u.filterEdit, "Filter rows (search or WHERE expr)…")
	x += fltW + gtx.Dp(6)
	x += u.drawPillButton(gtx, x, h, "Filter", src.Filter() != "", false, &u.btnApplyFilter) + gtx.Dp(4)
	if src.Filter() != "" {
		x += u.drawPillButton(gtx, x, h, "Clear", false, false, &u.btnClearFilter) + gtx.Dp(8)
	}

	// Go to row input box.
	gotoW := gtx.Dp(105)
	u.drawInputBox(gtx, x, h, gotoW, &u.gotoEdit, "Go to row #…")
	x += gotoW + gtx.Dp(10)

	if src.Editable() {
		x += u.drawPillButton(gtx, x, h, "+ Row", false, false, &u.btnAddRow) + gtx.Dp(6)
		if src.Changes().HasChanges() {
			x += u.drawPillButton(gtx, x, h, "Undo", false, false, &u.btnUndo) + gtx.Dp(4)
			x += u.drawPillButton(gtx, x, h, "Redo", false, false, &u.btnRedo) + gtx.Dp(6)
		}
	}

	theme.FillRect(gtx, clip.Rect{Min: image.Pt(0, h-1), Max: image.Pt(w, h)}, u.th.Border)
	return layout.Dimensions{Size: image.Pt(w, h)}
}

func (u *uiState) drawInputBox(gtx layout.Context, x, barH, boxW int, ed *widget.Editor, hint string) {
	padY := gtx.Dp(5)
	r := image.Rect(x, padY, x+boxW, barH-padY)
	box := clip.RRect{
		Rect: r,
		NE:   gtx.Dp(4), NW: gtx.Dp(4), SE: gtx.Dp(4), SW: gtx.Dp(4),
	}
	theme.FillRRect(gtx, box, u.th.Surface)
	borderCol := u.th.Border
	if gtx.Focused(ed) {
		borderCol = u.th.Accent
	}
	border := clip.Stroke{Path: box.Path(gtx.Ops), Width: float32(max(1, gtx.Dp(1)))}.Op()
	paint.FillShape(gtx.Ops, borderCol, border)

	inner := op.Offset(image.Pt(x+gtx.Dp(8), padY+gtx.Dp(4))).Push(gtx.Ops)
	cg := gtx
	cg.Constraints = layout.Exact(image.Pt(max(0, boxW-gtx.Dp(16)), r.Dy()-gtx.Dp(6)))
	if ed.Len() == 0 && !gtx.Focused(ed) {
		u.th.Label(cg, u.th.Small, u.th.TextSubtle, hint)
	}
	m := op.Record(gtx.Ops)
	paint.ColorOp{Color: u.th.Text}.Add(gtx.Ops)
	textMat := m.Stop()
	ms := op.Record(gtx.Ops)
	paint.ColorOp{Color: u.th.Selected}.Add(gtx.Ops)
	selMat := ms.Stop()
	ed.Layout(cg, u.th.Shaper, font.Font{Typeface: u.th.UI}, u.th.Small, textMat, selMat)
	inner.Pop()
}

func (u *uiState) layoutReviewView(gtx layout.Context) layout.Dimensions {
	theme.Fill(gtx, u.th.Surface)
	u.mu.Lock()
	src := u.tableSrc
	u.mu.Unlock()
	if src == nil || !src.Editable() || !src.Changes().HasChanges() {
		return layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return u.th.Label(gtx, u.th.UISize, u.th.TextSubtle, "No staged changes to review.")
		})
	}

	if clicked(&u.btnCommit, gtx) {
		u.commitChanges()
	}
	if clicked(&u.btnDiscard, gtx) {
		u.discardChanges()
	}

	barH := gtx.Dp(40)
	w := gtx.Constraints.Max.X
	sqlPreview := src.Changes().PreviewSQL()
	lines := strings.Split(sqlPreview, "\n")

	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			theme.FillRect(gtx, clip.Rect{Max: image.Pt(w, barH)}, u.th.SurfaceAlt)
			x := gtx.Dp(12)
			x += u.drawPillButton(gtx, x, barH, "✓ Commit Transaction", true, true, &u.btnCommit) + gtx.Dp(8)
			x += u.drawPillButton(gtx, x, barH, "✕ Discard All", false, false, &u.btnDiscard) + gtx.Dp(14)

			off := op.Offset(image.Pt(x, gtx.Dp(11))).Push(gtx.Ops)
			u.th.Label(gtx, u.th.Small, u.th.TextMuted, fmt.Sprintf("%d staged modification(s) on %s — optimistic concurrency check enabled", src.Changes().Count(), src.Object().Name))
			off.Pop()

			theme.FillRect(gtx, clip.Rect{Min: image.Pt(0, barH-1), Max: image.Pt(w, barH)}, u.th.Border)
			return layout.Dimensions{Size: image.Pt(w, barH)}
		}),
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			lineH := gtx.Dp(20)
			return u.reviewList.Layout(gtx, len(lines), func(gtx layout.Context, i int) layout.Dimensions {
				off := op.Offset(image.Pt(gtx.Dp(16), gtx.Dp(2))).Push(gtx.Ops)
				cg := gtx
				cg.Constraints.Max.X = max(0, w-gtx.Dp(32))
				u.th.LabelFont(cg, font.Font{Typeface: u.th.Mono}, u.th.Code, u.th.Text, text.Start, lines[i])
				off.Pop()
				return layout.Dimensions{Size: image.Pt(w, lineH)}
			})
		}),
	)
}

func (u *uiState) layoutSQLView(gtx layout.Context) layout.Dimensions {
	return u.sqlSplit.Layout(gtx, u.th,
		func(gtx layout.Context) layout.Dimensions {
			return u.layoutSQLEditorPane(gtx)
		},
		func(gtx layout.Context) layout.Dimensions {
			return u.sqlGrid.Layout(gtx, u.th)
		},
	)
}

func (u *uiState) layoutStructureView(gtx layout.Context) layout.Dimensions {
	theme.Fill(gtx, u.th.Surface)
	u.mu.Lock()
	obj := u.structObj
	idxInfo := u.structIndex
	rel := u.structRel
	schema := u.schema
	u.mu.Unlock()

	if obj == nil {
		return layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return u.th.Label(gtx, u.th.UISize, u.th.TextSubtle, "Select any table, view, index, or trigger from the sidebar to inspect its structure and DDL.")
		})
	}

	targetTable := obj.Name
	if obj.Table != "" {
		targetTable = obj.Table
	}
	var targetObj *engine.Object
	if schema != nil && targetTable != "" {
		targetObj = schema.Find(targetTable)
	}

	if clicked(&u.btnBrowseTarget, gtx) && targetObj != nil {
		u.tree.SelectObject(targetObj)
		u.selectObject(targetObj)
	}
	if clicked(&u.btnQueryStruct, gtx) {
		q := fmt.Sprintf("SELECT * FROM %s LIMIT 500;", engine.QuoteIdent(targetTable))
		if obj.Type == engine.TypeIndex && idxInfo != nil {
			var orderCols []string
			for _, c := range idxInfo.Columns {
				if !c.Key || c.CID < 0 {
					continue
				}
				dir := ""
				if c.Desc {
					dir = " DESC"
				}
				orderCols = append(orderCols, engine.QuoteIdent(c.Name)+dir)
			}
			if len(orderCols) > 0 {
				q = fmt.Sprintf("SELECT * FROM %s\nORDER BY %s\nLIMIT 500;", engine.QuoteIdent(targetTable), strings.Join(orderCols, ", "))
			}
		}
		u.sqlEdit.SetText(q)
		u.activeTab = tabSQL
	}
	if clicked(&u.btnCopyDDL, gtx) {
		ddl := u.ddlEdit.Text()
		gtx.Execute(clipboard.WriteCmd{
			Type: "application/text",
			Data: io.NopCloser(strings.NewReader(ddl)),
		})
		u.setStatus("Copied DDL for " + obj.Name + " to clipboard")
	}

	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			barH := gtx.Dp(40)
			w := gtx.Constraints.Max.X
			theme.FillRect(gtx, clip.Rect{Max: image.Pt(w, barH)}, u.th.SurfaceAlt)

			var badgeParts []string
			badgeParts = append(badgeParts, strings.ToUpper(string(obj.Type))+" · "+obj.Name)
			if (obj.Type == engine.TypeIndex || obj.Type == engine.TypeTrigger) && obj.Table != "" {
				badgeParts = append(badgeParts, "on "+obj.Table)
			}
			if obj.Type == engine.TypeIndex && idxInfo != nil {
				if idxInfo.Unique {
					badgeParts = append(badgeParts, "UNIQUE")
				}
				if idxInfo.Partial {
					badgeParts = append(badgeParts, "PARTIAL")
				}
			}
			if obj.WithoutRowid {
				badgeParts = append(badgeParts, "WITHOUT ROWID")
			}
			if obj.Strict {
				badgeParts = append(badgeParts, "STRICT")
			}
			title := strings.Join(badgeParts, "  ·  ")

			x := gtx.Dp(12)
			if targetObj != nil && (targetObj.Type == engine.TypeTable || targetObj.Type == engine.TypeView) {
				lbl := "Browse " + targetObj.Name
				x += u.drawPillButton(gtx, x, barH, lbl, false, true, &u.btnBrowseTarget) + gtx.Dp(6)
			}
			if targetTable != "" {
				x += u.drawPillButton(gtx, x, barH, "Select in SQL", false, false, &u.btnQueryStruct) + gtx.Dp(6)
			}
			x += u.drawPillButton(gtx, x, barH, "Copy DDL", false, false, &u.btnCopyDDL) + gtx.Dp(12)

			off := op.Offset(image.Pt(x, gtx.Dp(11))).Push(gtx.Ops)
			cg := gtx
			cg.Constraints.Max.X = max(0, w-x-gtx.Dp(12))
			u.th.LabelFont(cg, font.Font{Typeface: u.th.UI, Weight: font.SemiBold}, u.th.Small, u.th.Text, text.Start, title)
			off.Pop()

			theme.FillRect(gtx, clip.Rect{Min: image.Pt(0, barH-1), Max: image.Pt(w, barH)}, u.th.Border)
			return layout.Dimensions{Size: image.Pt(w, barH)}
		}),
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			return u.structSplit.Layout(gtx, u.th,
				func(gtx layout.Context) layout.Dimensions {
					return u.structGrid.Layout(gtx, u.th)
				},
				func(gtx layout.Context) layout.Dimensions {
					return u.layoutStructureDDLPane(gtx, obj, rel)
				},
			)
		}),
	)
}

func (u *uiState) layoutStructureDDLPane(gtx layout.Context, obj *engine.Object, rel []*engine.Object) layout.Dimensions {
	theme.Fill(gtx, u.th.Surface)
	for {
		if _, ok := u.ddlEdit.Update(gtx); !ok {
			break
		}
	}
	if len(u.structRelClicks) < len(rel) {
		u.structRelClicks = make([]gesture.Click, len(rel))
	}
	for i, r := range rel {
		if clicked(&u.structRelClicks[i], gtx) {
			u.tree.SelectObject(r)
			u.selectObject(r)
		}
	}

	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			if len(rel) == 0 {
				return layout.Dimensions{}
			}
			barH := gtx.Dp(34)
			w := gtx.Constraints.Max.X
			theme.FillRect(gtx, clip.Rect{Max: image.Pt(w, barH)}, u.th.Chrome)
			x := gtx.Dp(12)
			off := op.Offset(image.Pt(x, gtx.Dp(9))).Push(gtx.Ops)
			u.th.Label(gtx, u.th.Small, u.th.TextSubtle, "Indexes & Triggers:")
			off.Pop()
			x += gtx.Dp(124)
			for i, r := range rel {
				if x > w-gtx.Dp(90) {
					break
				}
				prefix := "idx"
				if r.Type == engine.TypeTrigger {
					prefix = "trg"
				}
				active := obj != nil && r.Type == obj.Type && r.Name == obj.Name
				x += u.drawPillButton(gtx, x, barH, prefix+": "+r.Name, active, false, &u.structRelClicks[i]) + gtx.Dp(6)
			}
			theme.FillRect(gtx, clip.Rect{Min: image.Pt(0, barH-1), Max: image.Pt(w, barH)}, u.th.BorderSubtle)
			return layout.Dimensions{Size: image.Pt(w, barH)}
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			h := gtx.Dp(26)
			w := gtx.Constraints.Max.X
			theme.FillRect(gtx, clip.Rect{Max: image.Pt(w, h)}, u.th.SurfaceAlt)
			off := op.Offset(image.Pt(gtx.Dp(12), gtx.Dp(5))).Push(gtx.Ops)
			u.th.LabelFont(gtx, font.Font{Typeface: u.th.UI, Weight: font.SemiBold}, u.th.Small, u.th.TextMuted, text.Start, "DDL Definition (select text or click Copy DDL)")
			off.Pop()
			theme.FillRect(gtx, clip.Rect{Min: image.Pt(0, h-1), Max: image.Pt(w, h)}, u.th.BorderSubtle)
			return layout.Dimensions{Size: image.Pt(w, h)}
		}),
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: 10, Bottom: 10, Left: 14, Right: 14}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				m := op.Record(gtx.Ops)
				paint.ColorOp{Color: u.th.Text}.Add(gtx.Ops)
				textMat := m.Stop()
				ms := op.Record(gtx.Ops)
				paint.ColorOp{Color: u.th.Selected}.Add(gtx.Ops)
				selMat := ms.Stop()
				return u.ddlEdit.Layout(gtx, u.th.Shaper, font.Font{Typeface: u.th.Mono}, u.th.Code, textMat, selMat)
			})
		}),
	)
}

func (u *uiState) layoutSQLEditorPane(gtx layout.Context) layout.Dimensions {
	theme.Fill(gtx, u.th.Surface)
	barH := gtx.Dp(34)
	w := gtx.Constraints.Max.X

	for {
		if _, ok := u.sqlEdit.Update(gtx); !ok {
			break
		}
	}
	if clicked(&u.btnRunSQL, gtx) {
		u.runSQL()
	}
	if clicked(&u.btnCancelSQL, gtx) {
		u.mu.Lock()
		if u.sqlCancel != nil {
			u.sqlCancel()
		}
		u.mu.Unlock()
	}

	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			theme.FillRect(gtx, clip.Rect{Max: image.Pt(w, barH)}, u.th.SurfaceAlt)
			x := gtx.Dp(10)
			u.mu.Lock()
			running := u.sqlRunning
			status := u.sqlStatus
			u.mu.Unlock()

			x += u.drawPillButton(gtx, x, barH, "▶ Run (Ctrl+Enter)", true, true, &u.btnRunSQL) + gtx.Dp(6)
			if running {
				x += u.drawPillButton(gtx, x, barH, "■ Cancel", false, false, &u.btnCancelSQL) + gtx.Dp(10)
			}
			if status != "" {
				off := op.Offset(image.Pt(x+gtx.Dp(6), gtx.Dp(8))).Push(gtx.Ops)
				u.th.LabelFont(gtx, font.Font{Typeface: u.th.Mono}, u.th.Small, u.th.TextMuted, text.Start, status)
				off.Pop()
			}
			theme.FillRect(gtx, clip.Rect{Min: image.Pt(0, barH-1), Max: image.Pt(w, barH)}, u.th.BorderSubtle)
			return layout.Dimensions{Size: image.Pt(w, barH)}
		}),
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: 8, Bottom: 8, Left: 12, Right: 12}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				m := op.Record(gtx.Ops)
				paint.ColorOp{Color: u.th.Text}.Add(gtx.Ops)
				textMat := m.Stop()

				ms := op.Record(gtx.Ops)
				paint.ColorOp{Color: u.th.Selected}.Add(gtx.Ops)
				selMat := ms.Stop()

				return u.sqlEdit.Layout(gtx, u.th.Shaper, font.Font{Typeface: u.th.Mono}, u.th.Code, textMat, selMat)
			})
		}),
	)
}

type paletteItem struct {
	label string
	hint  string
	run   func()
}

func (u *uiState) layoutCommandPalette(gtx layout.Context) {
	size := gtx.Constraints.Max
	// Dim backdrop.
	theme.FillRect(gtx, clip.Rect{Max: size}, u.th.Hover)

	u.mu.Lock()
	schema := u.schema
	u.mu.Unlock()

	items := []paletteItem{
		{"Open Database…", "Ctrl+O", u.openDialog},
		{"Load 100,000,000-Row Virtualized Demo", "", u.loadDemo100M},
		{"Switch to Browse Data Tab", "", func() { u.activeTab = tabBrowse }},
		{"Switch to Execute SQL Tab", "", func() { u.activeTab = tabSQL }},
		{"Switch to Structure / DDL Tab", "", func() { u.activeTab = tabStructure }},
		{"Review & Commit Staged Changes", "Ctrl+S", func() { u.activeTab = tabReview }},
		{"Toggle Left Schema Sidebar", "Ctrl+B", func() { u.leftSplit.Collapsed = !u.leftSplit.Collapsed }},
		{"Toggle Right Cell Inspector", "Ctrl+I", func() { u.rightSplit.Collapsed = !u.rightSplit.Collapsed }},
		{"Toggle Dark / Light Theme", "", func() {
			accent := u.th.Accent
			if a, ok := win.AccentColor(); ok {
				accent = a
			}
			u.th.SetDark(!u.th.Dark, accent)
			u.applyWindowStyle()
		}},
	}
	if schema != nil {
		for _, o := range schema.Objects {
			obj := o
			verb := "Inspect"
			if obj.Type == engine.TypeTable || obj.Type == engine.TypeView {
				verb = "Browse"
			}
			items = append(items, paletteItem{
				label: verb + " " + string(obj.Type) + ": " + obj.Name,
				hint:  string(obj.Type),
				run: func() {
					u.tree.SelectObject(obj)
					u.selectObject(obj)
				},
			})
		}
	}

	q := strings.ToLower(strings.TrimSpace(u.paletteEdit.Text()))
	var filtered []paletteItem
	for _, it := range items {
		if q == "" || strings.Contains(strings.ToLower(it.label), q) {
			filtered = append(filtered, it)
		}
	}
	if len(u.paletteClicks) < len(filtered) {
		u.paletteClicks = make([]gesture.Click, len(filtered))
	}

	for {
		ev, ok := u.paletteEdit.Update(gtx)
		if !ok {
			break
		}
		if _, ok := ev.(widget.SubmitEvent); ok && len(filtered) > 0 {
			u.paletteOpen = false
			filtered[0].run()
			return
		}
	}

	boxW := min(gtx.Dp(520), size.X-gtx.Dp(40))
	boxH := min(gtx.Dp(340), size.Y-gtx.Dp(80))
	bx := (size.X - boxW) / 2
	by := gtx.Dp(56)
	boxRect := image.Rect(bx, by, bx+boxW, by+boxH)

	rr := clip.RRect{
		Rect: boxRect,
		NE:   gtx.Dp(8), NW: gtx.Dp(8), SE: gtx.Dp(8), SW: gtx.Dp(8),
	}
	theme.FillRRect(gtx, rr, u.th.Raised)
	border := clip.Stroke{Path: rr.Path(gtx.Ops), Width: float32(max(1, gtx.Dp(1)))}.Op()
	paint.FillShape(gtx.Ops, u.th.Border, border)

	off := op.Offset(boxRect.Min).Push(gtx.Ops)
	pClip := clip.Rect{Max: image.Pt(boxW, boxH)}.Push(gtx.Ops)
	cg := gtx
	cg.Constraints = layout.Exact(image.Pt(boxW, boxH))

	u.drawInputBox(cg, gtx.Dp(10), gtx.Dp(42), boxW-gtx.Dp(20), &u.paletteEdit, "Type a command or table name…")

	listOff := op.Offset(image.Pt(0, gtx.Dp(44))).Push(gtx.Ops)
	lcg := cg
	lcg.Constraints = layout.Exact(image.Pt(boxW, boxH-gtx.Dp(46)))
	rowH := gtx.Dp(30)
	u.paletteList.Layout(lcg, len(filtered), func(gtx layout.Context, i int) layout.Dimensions {
		clk := &u.paletteClicks[i]
		if clicked(clk, gtx) {
			u.paletteOpen = false
			filtered[i].run()
		}
		if clk.Hovered() || i == 0 {
			theme.FillRect(gtx, clip.Rect{Min: image.Pt(6, 1), Max: image.Pt(boxW-6, rowH-1)}, u.th.Hover)
		}
		o1 := op.Offset(image.Pt(gtx.Dp(16), gtx.Dp(6))).Push(gtx.Ops)
		u.th.Label(gtx, u.th.UISize, u.th.Text, filtered[i].label)
		o1.Pop()
		if filtered[i].hint != "" {
			o2 := op.Offset(image.Pt(0, gtx.Dp(7))).Push(gtx.Ops)
			rcg := gtx
			rcg.Constraints.Min.X = boxW - gtx.Dp(16)
			rcg.Constraints.Max.X = boxW - gtx.Dp(16)
			u.th.LabelFont(rcg, font.Font{Typeface: u.th.Mono}, u.th.Small, u.th.TextSubtle, text.End, filtered[i].hint)
			o2.Pop()
		}
		hit := clip.Rect{Max: image.Pt(boxW, rowH)}.Push(gtx.Ops)
		pointer.CursorPointer.Add(gtx.Ops)
		clk.Add(gtx.Ops)
		hit.Pop()
		return layout.Dimensions{Size: image.Pt(boxW, rowH)}
	})
	listOff.Pop()
	pClip.Pop()
	off.Pop()
}

func (u *uiState) layoutStatusBar(gtx layout.Context) layout.Dimensions {
	h := gtx.Dp(26)
	w := gtx.Constraints.Max.X
	theme.FillRect(gtx, clip.Rect{Max: image.Pt(w, h)}, u.th.Chrome)
	theme.FillRect(gtx, clip.Rect{Max: image.Pt(w, 1)}, u.th.Border)

	u.mu.Lock()
	db := u.db
	dbSize := u.dbSize
	src := u.tableSrc
	tbl := u.activeTable
	msg := u.statusMsg
	u.mu.Unlock()

	var leftParts []string
	if db != nil {
		leftParts = append(leftParts, fmt.Sprintf("%s (%s)", filepath.Base(db.Path), data.FormatSize(dbSize)))
	}
	if src != nil {
		cnt, st := src.Count()
		prefix := ""
		if st == data.CountEstimate {
			prefix = "≈"
		}
		leftParts = append(leftParts, fmt.Sprintf("%s: %s%d rows", tbl, prefix, cnt))
		leftParts = append(leftParts, fmt.Sprintf("cache %s", data.FormatSize(int64(src.CacheBytes()))))
		if src.Editable() {
			if n := src.Changes().Count(); n > 0 {
				leftParts = append(leftParts, fmt.Sprintf("%d staged change(s) (Ctrl+S to review)", n))
			}
		}
	} else if tbl != "" {
		leftParts = append(leftParts, fmt.Sprintf("%s: 100,000,000 rows", tbl))
	}
	if msg != "" {
		leftParts = append(leftParts, msg)
	}

	off := op.Offset(image.Pt(gtx.Dp(12), gtx.Dp(5))).Push(gtx.Ops)
	cg := gtx
	cg.Constraints.Max.X = max(0, w-gtx.Dp(180))
	u.th.Label(cg, u.th.Small, u.th.TextMuted, strings.Join(leftParts, "  ·  "))
	off.Pop()

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	memTxt := fmt.Sprintf("Go heap %.1f MB", float64(ms.HeapInuse)/(1<<20))
	offR := op.Offset(image.Pt(0, gtx.Dp(5))).Push(gtx.Ops)
	cgR := gtx
	cgR.Constraints.Min.X = w - gtx.Dp(12)
	cgR.Constraints.Max.X = w - gtx.Dp(12)
	u.th.LabelFont(cgR, font.Font{Typeface: u.th.Mono}, u.th.Small, u.th.TextSubtle, text.End, memTxt)
	offR.Pop()

	return layout.Dimensions{Size: image.Pt(w, h)}
}

func (u *uiState) drawPillButton(gtx layout.Context, x, barH int, label string, active, accent bool, clk *gesture.Click) int {
	btnW := gtx.Dp(unit.Dp(len(label)*7 + 22))
	btnH := gtx.Dp(26)
	y := (barH - btnH) / 2
	r := image.Rect(x, y, x+btnW, y+btnH)
	rr := clip.RRect{
		Rect: r,
		NE:   gtx.Dp(5), NW: gtx.Dp(5), SE: gtx.Dp(5), SW: gtx.Dp(5),
	}
	bg := u.th.Surface
	fg := u.th.Text
	if active {
		if accent {
			bg = u.th.Accent
			fg = u.th.AccentText
		} else {
			bg = u.th.Selected
			fg = u.th.SelectedText
		}
	} else if clk.Hovered() {
		bg = u.th.Hover
	}
	theme.FillRRect(gtx, rr, bg)
	if !active {
		border := clip.Stroke{Path: rr.Path(gtx.Ops), Width: float32(max(1, gtx.Dp(1)))}.Op()
		paint.FillShape(gtx.Ops, u.th.Border, border)
	}

	off := op.Offset(image.Pt(x, y+gtx.Dp(4))).Push(gtx.Ops)
	cg := gtx
	cg.Constraints.Min.X = btnW
	cg.Constraints.Max.X = btnW
	fnt := font.Font{Typeface: u.th.UI}
	if active {
		fnt.Weight = font.SemiBold
	}
	u.th.LabelFont(cg, fnt, u.th.Small, fg, text.Middle, label)
	off.Pop()

	hit := clip.Rect(r).Push(gtx.Ops)
	pointer.CursorPointer.Add(gtx.Ops)
	clk.Add(gtx.Ops)
	hit.Pop()
	return btnW
}

func clicked(c *gesture.Click, gtx layout.Context) bool {
	fired := false
	for {
		ev, ok := c.Update(gtx.Source)
		if !ok {
			break
		}
		if ev.Kind == gesture.KindClick {
			fired = true
		}
	}
	return fired
}
