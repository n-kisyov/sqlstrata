// Command sqlstrata is a native, memory-bounded SQLite GUI editor for Windows 11.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"gioui.org/app"
	"gioui.org/font"
	"gioui.org/gesture"
	"gioui.org/io/event"
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

	sqlapp "github.com/sqlstrata/sqlstrata/internal/app"
	"github.com/sqlstrata/sqlstrata/internal/data"
	"github.com/sqlstrata/sqlstrata/internal/edit"
	"github.com/sqlstrata/sqlstrata/internal/engine"
	"github.com/sqlstrata/sqlstrata/internal/platform/win"
	"github.com/sqlstrata/sqlstrata/internal/ui/theme"
	"github.com/sqlstrata/sqlstrata/internal/ui/widgets"
)

func main() {
	ro := flag.Bool("ro", false, "open initial database read-only")
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
		ui.initSession()
		if initialPath != "" {
			ui.openDatabase(initialPath, *ro)
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
	tabTools
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
	toolsGrid  *widgets.DataGrid
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
	btnTabTools     gesture.Click
	btnTabReview    gesture.Click
	btnSidebar      gesture.Click
	btnInspector    gesture.Click
	btnTheme        gesture.Click

	// SQL view buttons & state.
	btnRunSQL        gesture.Click
	btnCancelSQL     gesture.Click
	btnRunSQLFile    gesture.Click
	btnSQLHistory    gesture.Click
	btnSQLExpCSV     gesture.Click
	btnSQLExpJSON    gesture.Click
	sqlHistoryOpen   bool
	sqlSuggestClicks []gesture.Click
	sqlHistClicks    []gesture.Click

	// Structure view buttons & schema designer state.
	btnBrowseTarget    gesture.Click
	btnQueryStruct     gesture.Click
	btnCopyDDL         gesture.Click
	btnNewTable        gesture.Click
	btnAddColToggle    gesture.Click
	btnEmptyTable      gesture.Click
	btnDropObject      gesture.Click
	confirmEmptyTarget string
	confirmDropType    engine.ObjectType
	confirmDropName    string
	structRelClicks    []gesture.Click

	addColOpen    bool
	addColName    widget.Editor
	addColDflt    widget.Editor
	addColTypeIdx int
	addColNotNull bool
	btnAddColType gesture.Click
	btnAddColNN   gesture.Click
	btnAddColSave gesture.Click
	btnAddColCanc gesture.Click

	newTableOpen      bool
	ntTableName       widget.Editor
	ntWithoutRowid    bool
	ntStrict          bool
	ntCols            []*designerColRow
	ntColList         layout.List
	btnNTWithoutRowid gesture.Click
	btnNTStrict       gesture.Click
	btnNTAddField     gesture.Click
	btnNTCreate       gesture.Click
	btnNTCancel       gesture.Click

	// DB tools buttons.
	btnToolsRefresh    gesture.Click
	btnToolsQuickChk   gesture.Click
	btnToolsIntegChk   gesture.Click
	btnToolsFKChk      gesture.Click
	btnToolsOptimize   gesture.Click
	btnToolsVacuumInto gesture.Click
	btnToolsImportCSV  gesture.Click
	btnToolsExportSQL  gesture.Click

	// Welcome screen buttons.
	btnWelOpen   gesture.Click
	btnWelOpenRO gesture.Click
	btnWelNew    gesture.Click
	btnWelDemo   gesture.Click
	recentClicks []gesture.Click

	// Browse sub-toolbar buttons.
	btnApplyFilter gesture.Click
	btnClearFilter gesture.Click
	btnAddRow      gesture.Click
	btnUndo        gesture.Click
	btnRedo        gesture.Click
	btnBrowseCSV   gesture.Click
	btnBrowseJSON  gesture.Click
	btnCommit      gesture.Click
	btnDiscard     gesture.Click

	// Command palette overlay.
	paletteOpen   bool
	paletteEdit   widget.Editor
	paletteList   layout.List
	paletteClicks []gesture.Click

	mu          sync.Mutex
	uiPending   []func()
	sessionPath string
	session     *sqlapp.Session
	db          *engine.DB
	dbSize      int64
	schema      *engine.Schema
	tableSrc    *data.TableSource
	sqlSpool    *data.ResultSpool
	inspectGrid *widgets.DataGrid
	activeTable string
	structObj   *engine.Object
	structIndex *engine.IndexInfo
	structRel   []*engine.Object
	toolsStatus string
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
		sqlSplit:    widgets.Splitter{Axis: layout.Vertical, Size: 200, Min: 100, Max: 520},
		structSplit: widgets.Splitter{Axis: layout.Vertical, Size: 230, Min: 100, Max: 520},
		tree:        widgets.NewTreeView(),
		browseGrid:  widgets.NewDataGrid(),
		sqlGrid:     widgets.NewDataGrid(),
		structGrid:  widgets.NewDataGrid(),
		toolsGrid:   widgets.NewDataGrid(),
		inspector:   widgets.NewInspector(),
		reviewList:  layout.List{Axis: layout.Vertical},
		paletteList: layout.List{Axis: layout.Vertical},
		ntColList:   layout.List{Axis: layout.Vertical},
		session:     &sqlapp.Session{},
		statusMsg:   "Ready · Open a SQLite file (Ctrl+O) or try the 100M-row Demo",
	}
	u.ddlEdit.ReadOnly = true
	u.addColName.SingleLine = true
	u.addColDflt.SingleLine = true
	u.ntTableName.SingleLine = true
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
	onCopy := func(val string) {
		if len(val) > 48 {
			val = val[:48] + "…"
		}
		u.setStatus("Copied to clipboard: " + val)
	}
	u.browseGrid.OnCopy = onCopy
	u.sqlGrid.OnCopy = onCopy
	u.structGrid.OnCopy = onCopy
	u.toolsGrid.OnCopy = onCopy
	u.browseGrid.OnTruncatedEdit = func(row int64, col int) {
		u.rightSplit.Collapsed = false
		u.inspectFrom(u.browseGrid, row, col)
		u.setStatus("Cell value > 256 B — edit safely in the Cell Inspector")
	}

	u.inspector.OnStageCell = func(row int64, col int, orig edit.Value, hasOrig bool, val edit.Value) {
		u.mu.Lock()
		src := u.tableSrc
		u.mu.Unlock()
		if src != nil {
			if src.StageCellEditWithOrig(row, col, orig, hasOrig, val) {
				u.inspectFrom(u.browseGrid, row, col)
				u.win.Invalidate()
			} else {
				u.setStatus("Cannot stage cell: full original value not loaded or exceeds preview limit")
			}
		}
	}
	u.inspector.OnDeleteRow = func(row int64) {
		u.mu.Lock()
		src := u.tableSrc
		u.mu.Unlock()
		if src != nil && src.Editable() {
			if key, ok := src.RowKey(row); ok {
				src.Changes().ToggleDeleteKey(key)
				u.win.Invalidate()
			}
		}
	}
	u.inspector.OnLoadOffset = func(row int64, col int, offset int64) {
		u.mu.Lock()
		src := u.tableSrc
		ig := u.inspectGrid
		u.mu.Unlock()
		if src == nil || (ig != nil && ig != u.browseGrid) {
			return
		}
		cols := src.Columns()
		if col < 0 || col >= len(cols) {
			return
		}
		colMeta := cols[col]
		editable := src.Editable()
		src.FetchCellDetailAt(row, col, offset, int(widgets.InspectorPageBytes), func(det data.CellDetail, err error) {
			if err == nil {
				u.postUI(func() {
					u.inspector.SetDetail(det, colMeta, editable)
				})
			} else {
				u.setStatus(fmt.Sprintf("Load cell offset failed: %v", err))
			}
		})
	}
	u.inspector.OnLoadImage = func(row int64, col int, wantBytes int) {
		u.mu.Lock()
		src := u.tableSrc
		ig := u.inspectGrid
		u.mu.Unlock()
		if src == nil || (ig != nil && ig != u.browseGrid) {
			return
		}
		cols := src.Columns()
		if col < 0 || col >= len(cols) {
			return
		}
		if wantBytes <= 0 || wantBytes > 8<<20 {
			wantBytes = 8 << 20
		}
		colMeta := cols[col]
		editable := src.Editable()
		src.FetchCellDetailAt(row, col, 0, wantBytes, func(det data.CellDetail, err error) {
			if err == nil {
				u.postUI(func() {
					u.inspector.SetDetail(det, colMeta, editable)
				})
			}
		})
	}
	u.inspector.OnExportCell = func(row int64, col int) {
		u.exportInspectedCell(row, col)
	}
	u.inspector.OnImportFile = func(row int64, col int) {
		u.importFileIntoCell(row, col)
	}
	return u
}

func (u *uiState) postUI(fn func()) {
	u.mu.Lock()
	u.uiPending = append(u.uiPending, fn)
	u.mu.Unlock()
	if u.win != nil {
		u.win.Invalidate()
	}
}

func (u *uiState) drainUI() {
	u.mu.Lock()
	pending := u.uiPending
	u.uiPending = nil
	u.mu.Unlock()
	for _, fn := range pending {
		fn()
	}
}

func (u *uiState) initSession() {
	p, err := sqlapp.DefaultSessionPath()
	if err != nil {
		return
	}
	s, _ := sqlapp.LoadSession(p)
	u.mu.Lock()
	u.sessionPath = p
	u.session = s
	u.mu.Unlock()

	if s.ThemeSet && s.DarkTheme != u.th.Dark {
		accent := u.th.Accent
		if a, ok := win.AccentColor(); ok {
			accent = a
		}
		u.th.SetDark(s.DarkTheme, accent)
		u.applyWindowStyle()
	}
}

func (u *uiState) persistSessionLocked() {
	if u.sessionPath == "" || u.session == nil {
		return
	}
	cp := *u.session
	cp.RecentFiles = append([]string(nil), u.session.RecentFiles...)
	cp.QueryHistory = append([]string(nil), u.session.QueryHistory...)
	path := u.sessionPath
	go func() {
		_ = cp.Save(path)
	}()
}

func (u *uiState) saveQueryHistory(sql string) {
	u.mu.Lock()
	if u.session != nil {
		u.session.AddQuery(sql)
		u.persistSessionLocked()
	}
	u.mu.Unlock()
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
	u.mu.Lock()
	u.inspectGrid = g
	u.mu.Unlock()
	ts, isTable := src.(*data.TableSource)
	editable := isTable && ts.Editable()
	u.inspector.SetCell(row, col, cols[col], val, kind, trunc, editable)

	// Stream the full value when inspecting a TableSource cell that is either a
	// BLOB (64 KB window) or truncated text (up to 256 KB).
	if isTable && (kind == data.KindBlob || trunc) {
		colMeta := cols[col]
		limit := 256 * 1024
		if kind == data.KindBlob {
			limit = 64 * 1024
		}
		ts.FetchCellDetail(row, col, limit, func(det data.CellDetail, err error) {
			if err == nil {
				u.postUI(func() {
					u.inspector.SetDetail(det, colMeta, editable)
				})
			}
		})
	}
}

func (u *uiState) exportInspectedCell(row int64, col int) {
	u.mu.Lock()
	ig := u.inspectGrid
	ts := u.tableSrc
	u.mu.Unlock()
	if ig == nil {
		ig = u.browseGrid
	}
	var tableSrc *data.TableSource
	if ig == u.browseGrid && ts != nil {
		tableSrc = ts
	}
	colName := u.inspector.ColumnName()
	if colName == "" {
		colName = fmt.Sprintf("col_%d", col+1)
	}
	rawPreview := u.inspector.RawBytes()
	kind := u.inspector.Kind()

	ext := "txt"
	if imgFmt := widgets.DetectImageFormat(rawPreview); imgFmt != "" {
		if imgFmt == "jpeg" {
			ext = "jpg"
		} else {
			ext = imgFmt
		}
	} else if kind == data.KindBlob {
		ext = "bin"
	} else if len(rawPreview) > 0 && json.Valid(rawPreview) {
		ext = "json"
	}
	defaultName := fmt.Sprintf("%s_row%d.%s", colName, row+1, ext)
	hwnd := u.hwnd
	go func() {
		outPath, ok := win.SaveFile(hwnd, "Export Cell Value", defaultName, ext, []win.Filter{
			{Name: "Matching File (*." + ext + ")", Pattern: "*." + ext},
			{Name: "All Files (*.*)", Pattern: "*.*"},
		})
		if !ok || outPath == "" {
			return
		}
		f, err := os.Create(outPath)
		if err != nil {
			u.setStatus(fmt.Sprintf("Export cell failed: %v", err))
			return
		}
		var n int64
		if tableSrc != nil {
			n, err = tableSrc.ExportCellTo(row, col, f)
		} else {
			var nw int
			nw, err = f.Write(rawPreview)
			n = int64(nw)
		}
		cerr := f.Close()
		if err != nil {
			_ = os.Remove(outPath)
			u.setStatus(fmt.Sprintf("Export cell failed: %v", err))
			return
		}
		if cerr != nil {
			_ = os.Remove(outPath)
			u.setStatus(fmt.Sprintf("Export cell close failed: %v", cerr))
			return
		}
		u.setStatus(fmt.Sprintf("Exported %s to %s", data.FormatSize(n), filepath.Base(outPath)))
	}()
}

func (u *uiState) importFileIntoCell(row int64, col int) {
	u.mu.Lock()
	ts := u.tableSrc
	ig := u.inspectGrid
	u.mu.Unlock()
	if ts == nil || !ts.Editable() || (ig != nil && ig != u.browseGrid) {
		u.setStatus("Switch to an editable table row to import a file into this cell")
		return
	}
	const maxImportCellBytes = 32 << 20
	hwnd := u.hwnd
	go func() {
		path, ok := win.OpenFile(hwnd, "Import File into BLOB Cell (<= 32 MB)", []win.Filter{
			{Name: "All Files (*.*)", Pattern: "*.*"},
			{Name: "Images (*.png;*.jpg;*.jpeg;*.gif)", Pattern: "*.png;*.jpg;*.jpeg;*.gif"},
		})
		if !ok || path == "" {
			return
		}
		fi, err := os.Stat(path)
		if err != nil {
			u.setStatus(fmt.Sprintf("Import cell failed: %v", err))
			return
		}
		if fi.Size() > maxImportCellBytes {
			u.setStatus(fmt.Sprintf("File %s (%s) exceeds 32 MB cell import limit", filepath.Base(path), data.FormatSize(fi.Size())))
			return
		}
		payload, err := os.ReadFile(path)
		if err != nil {
			u.setStatus(fmt.Sprintf("Read file failed: %v", err))
			return
		}

		// Load full original value for optimistic conflict checking & undo/redo.
		type origRes struct {
			det data.CellDetail
			err error
		}
		ch := make(chan origRes, 1)
		ts.FetchCellDetail(row, col, maxImportCellBytes+1, func(det data.CellDetail, ferr error) {
			ch <- origRes{det, ferr}
		})
		res := <-ch
		if res.err != nil {
			u.setStatus(fmt.Sprintf("Load existing cell failed: %v", res.err))
			return
		}
		if res.det.Truncated {
			u.setStatus("Existing cell > 32 MB; set NULL via SQL before replacing")
			return
		}
		var origVal edit.Value
		switch res.det.Kind {
		case data.KindNull:
			origVal = edit.NullValue()
		case data.KindInteger:
			origVal = edit.ExactValue(edit.KindInt, string(res.det.Data), nil)
		case data.KindReal:
			origVal = edit.ExactValue(edit.KindReal, string(res.det.Data), nil)
		case data.KindBlob:
			origVal = edit.ExactValue(edit.KindBlob, "", res.det.Data)
		default:
			origVal = edit.ExactValue(edit.KindText, string(res.det.Data), nil)
		}

		u.postUI(func() {
			if ts.StageCellEditWithOrig(row, col, origVal, true, edit.ExactValue(edit.KindBlob, "", payload)) {
				u.inspectFrom(u.browseGrid, row, col)
				u.setStatus(fmt.Sprintf("Staged %s BLOB from %s", data.FormatSize(int64(len(payload))), filepath.Base(path)))
			} else {
				u.setStatus("Failed to stage imported file")
			}
		})
	}()
}

func (u *uiState) applyWindowStyle() {
	if u.hwnd != 0 {
		win.StyleWindow(u.hwnd, u.th.Dark, u.th.Chrome, u.th.Text, u.th.Border)
	}
}

func (u *uiState) openDialog(readOnly bool) {
	hwnd := u.hwnd
	title := "Open SQLite Database"
	if readOnly {
		title = "Open SQLite Database (Read-Only)"
	}
	go func() {
		path, ok := win.OpenFile(hwnd, title, []win.Filter{
			{Name: "SQLite Databases (*.db;*.sqlite;*.sqlite3)", Pattern: "*.db;*.sqlite;*.sqlite3;*.db3"},
			{Name: "All Files (*.*)", Pattern: "*.*"},
		})
		if ok && path != "" {
			u.openDatabase(path, readOnly)
		}
	}()
}

func (u *uiState) openDatabase(path string, readOnly bool) {
	go func() {
		start := time.Now()
		opt := engine.DefaultOptions()
		opt.ReadOnly = readOnly
		db, err := engine.Open(path, opt)
		if err != nil {
			u.setStatus(fmt.Sprintf("Open failed: %v", err))
			return
		}
		var dbSize int64
		if fi, serr := os.Stat(path); serr == nil {
			dbSize = fi.Size()
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
		oldSpool := u.sqlSpool
		u.db = db
		u.dbSize = dbSize
		u.schema = schema
		u.tableSrc = nil
		u.sqlSpool = nil
		u.activeTable = ""
		u.structObj = nil
		u.structIndex = nil
		u.structRel = nil
		u.newTableOpen = false
		u.addColOpen = false
		u.confirmEmptyTarget = ""
		u.confirmDropName = ""
		u.confirmDropType = ""
		u.toolsStatus = ""
		u.sqlStatus = ""
		modeTag := ""
		if readOnly {
			modeTag = " [Read-Only]"
		}
		u.statusMsg = fmt.Sprintf("Opened %s%s in %s", filepath.Base(path), modeTag, time.Since(start).Round(time.Millisecond))
		if u.session != nil {
			u.session.AddRecent(path)
			u.persistSessionLocked()
		}
		u.mu.Unlock()

		if oldSrc != nil {
			oldSrc.Close()
		}
		if oldSpool != nil {
			_ = oldSpool.Close()
		}
		if oldDB != nil {
			oldDB.Close()
		}

		u.win.Option(app.Title("SQLStrata — " + filepath.Base(path) + modeTag))
		u.postUI(func() {
			u.tree.SetSchema(schema)
			u.browseGrid.SetSource(nil)
			u.sqlGrid.SetSource(nil)
			u.structGrid.SetSource(nil)
			u.inspector.Clear()

			if tables := schema.ByType(engine.TypeTable); len(tables) > 0 {
				u.tree.SelectObject(tables[0])
				u.selectObject(tables[0])
			} else if len(schema.Objects) > 0 {
				u.tree.SelectObject(schema.Objects[0])
				u.selectObject(schema.Objects[0])
			} else {
				u.activeTab = tabSQL
			}
		})
		u.refreshToolsInfo()
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

func (u *uiState) loadDemo100M() {
	u.mu.Lock()
	oldSrc := u.tableSrc
	u.tableSrc = nil
	u.structObj = nil
	u.structIndex = nil
	u.structRel = nil
	u.activeTable = "synthetic_100M"
	u.statusMsg = "100,000,000-row virtualized demo active — drag the scrollbar anywhere"
	u.mu.Unlock()
	if oldSrc != nil {
		oldSrc.Close()
	}

	u.browseGrid.SetSource(data.NewDemo100MSource(100_000_000))
	u.structGrid.SetSource(nil)
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
		u.postUI(func() {
			u.activeTab = tabBrowse
			u.setStatus(fmt.Sprintf("Committed %d change(s) in %s", n, time.Since(start).Round(time.Microsecond)))
		})
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
			if u.sqlSpool != nil {
				_ = u.sqlSpool.Close()
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

func (u *uiState) toggleTheme() {
	accent := u.th.Accent
	if a, ok := win.AccentColor(); ok {
		accent = a
	}
	next := !u.th.Dark
	u.th.SetDark(next, accent)
	u.applyWindowStyle()

	u.mu.Lock()
	if u.session != nil {
		u.session.ThemeSet = true
		u.session.DarkTheme = next
		u.persistSessionLocked()
	}
	u.mu.Unlock()
}

func (u *uiState) layout(gtx layout.Context) layout.Dimensions {
	u.drainUI()
	theme.Fill(gtx, u.th.Window)

	editorFocused := gtx.Focused(&u.sqlEdit) || gtx.Focused(&u.ddlEdit) ||
		gtx.Focused(&u.filterEdit) || gtx.Focused(&u.gotoEdit) ||
		gtx.Focused(&u.paletteEdit) || u.browseGrid.Editing() ||
		u.inspector.HasFocus(gtx) || u.structureHasFocus(gtx)

	filters := []event.Filter{
		key.Filter{Name: "O", Required: key.ModCtrl, Optional: key.ModShift},
		key.Filter{Name: "N", Required: key.ModCtrl},
		key.Filter{Name: "B", Required: key.ModCtrl},
		key.Filter{Name: "I", Required: key.ModCtrl},
		key.Filter{Name: "S", Required: key.ModCtrl},
		key.Filter{Name: "P", Required: key.ModCtrl, Optional: key.ModShift},
		key.Filter{Name: key.NameReturn, Required: key.ModCtrl},
		key.Filter{Name: key.NameEnter, Required: key.ModCtrl},
		key.Filter{Name: key.NameF5},
	}
	if !editorFocused && (u.activeTab == tabBrowse || u.activeTab == tabReview) {
		filters = append(filters,
			key.Filter{Name: "Z", Required: key.ModCtrl},
			key.Filter{Name: "Y", Required: key.ModCtrl},
		)
	}
	if u.paletteOpen || u.newTableOpen || u.addColOpen || u.confirmEmptyTarget != "" || u.confirmDropName != "" {
		filters = append(filters, key.Filter{Name: key.NameEscape})
	}

	// Global keyboard shortcuts.
	for {
		ev, ok := gtx.Event(filters...)
		if !ok {
			break
		}
		ke, ok := ev.(key.Event)
		if !ok || ke.State != key.Press {
			continue
		}
		switch ke.Name {
		case "O":
			u.openDialog(ke.Modifiers.Contain(key.ModShift))
		case "N":
			u.newDatabaseDialog()
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
			} else {
				u.addColOpen = false
				u.newTableOpen = false
				u.confirmEmptyTarget = ""
				u.confirmDropName = ""
				u.confirmDropType = ""
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
							case tabTools:
								return u.layoutToolsView(gtx)
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
		u.openDialog(false)
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
	if clicked(&u.btnTabTools, gtx) {
		u.activeTab = tabTools
		u.refreshToolsInfo()
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
		u.toggleTheme()
	}

	x := gtx.Dp(10)
	x += u.drawPillButton(gtx, x, h, "Open DB…", false, false, &u.btnOpen) + gtx.Dp(6)
	x += u.drawPillButton(gtx, x, h, "100M Demo", u.activeTable == "synthetic_100M", false, &u.btnDemo) + gtx.Dp(12)

	theme.FillRect(gtx, clip.Rect{Min: image.Pt(x, gtx.Dp(10)), Max: image.Pt(x+1, h-gtx.Dp(10))}, u.th.Border)
	x += gtx.Dp(12)

	x += u.drawPillButton(gtx, x, h, "Browse Data", u.activeTab == tabBrowse, true, &u.btnTabBrowse) + gtx.Dp(6)
	x += u.drawPillButton(gtx, x, h, "Execute SQL", u.activeTab == tabSQL, true, &u.btnTabSQL) + gtx.Dp(6)
	x += u.drawPillButton(gtx, x, h, "Structure", u.activeTab == tabStructure, true, &u.btnTabStructure) + gtx.Dp(6)
	x += u.drawPillButton(gtx, x, h, "DB Tools", u.activeTab == tabTools, true, &u.btnTabTools) + gtx.Dp(6)

	u.mu.Lock()
	src := u.tableSrc
	u.mu.Unlock()
	if src != nil && src.Editable() {
		if n := src.Changes().Count(); n > 0 {
			lbl := fmt.Sprintf("Review (%d)", n)
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

	rx -= gtx.Dp(94)
	u.drawPillButton(gtx, rx, h, "Inspector", !u.rightSplit.Collapsed, false, &u.btnInspector)

	rx -= gtx.Dp(82)
	u.drawPillButton(gtx, rx, h, "Sidebar", !u.leftSplit.Collapsed, false, &u.btnSidebar)

	theme.FillRect(gtx, clip.Rect{Min: image.Pt(0, h-1), Max: image.Pt(w, h)}, u.th.Border)
	return layout.Dimensions{Size: image.Pt(w, h)}
}

func (u *uiState) layoutBrowseView(gtx layout.Context) layout.Dimensions {
	u.mu.Lock()
	src := u.tableSrc
	db := u.db
	tbl := u.activeTable
	u.mu.Unlock()
	if db == nil && tbl == "" {
		return u.layoutWelcomeView(gtx)
	}
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
	if clicked(&u.btnBrowseCSV, gtx) {
		u.exportBrowseTable(false)
	}
	if clicked(&u.btnBrowseJSON, gtx) {
		u.exportBrowseTable(true)
	}

	x := gtx.Dp(8)
	// Filter input box.
	fltW := min(gtx.Dp(260), max(gtx.Dp(110), w/3))
	u.drawInputBox(gtx, x, h, fltW, &u.filterEdit, "Filter rows (search or WHERE expr)…")
	x += fltW + gtx.Dp(6)
	x += u.drawPillButton(gtx, x, h, "Filter", src.Filter() != "", false, &u.btnApplyFilter) + gtx.Dp(4)
	if src.Filter() != "" {
		x += u.drawPillButton(gtx, x, h, "Clear", false, false, &u.btnClearFilter) + gtx.Dp(8)
	}

	// Go to row input box.
	gotoW := gtx.Dp(96)
	u.drawInputBox(gtx, x, h, gotoW, &u.gotoEdit, "Row #…")
	x += gotoW + gtx.Dp(8)

	if src.Editable() {
		x += u.drawPillButton(gtx, x, h, "+ Row", false, false, &u.btnAddRow) + gtx.Dp(6)
		if src.Changes().HasChanges() {
			x += u.drawPillButton(gtx, x, h, "Undo", false, false, &u.btnUndo) + gtx.Dp(4)
			x += u.drawPillButton(gtx, x, h, "Redo", false, false, &u.btnRedo) + gtx.Dp(6)
		}
	}

	x += u.drawPillButton(gtx, x, h, "CSV", false, false, &u.btnBrowseCSV) + gtx.Dp(4)
	x += u.drawPillButton(gtx, x, h, "JSON", false, false, &u.btnBrowseJSON) + gtx.Dp(8)

	if err := src.Err(); err != nil && x < w-gtx.Dp(40) {
		off := op.Offset(image.Pt(x, gtx.Dp(10))).Push(gtx.Ops)
		cg := gtx
		cg.Constraints.Max.X = max(0, w-x-gtx.Dp(10))
		u.th.LabelFont(cg, font.Font{Typeface: u.th.Mono}, u.th.Small, u.th.Danger, text.Start, err.Error())
		off.Pop()
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

type paletteItem struct {
	label string
	hint  string
	run   func()
}

func (u *uiState) layoutCommandPalette(gtx layout.Context) {
	size := gtx.Constraints.Max
	theme.FillRect(gtx, clip.Rect{Max: size}, u.th.Hover)

	u.mu.Lock()
	schema := u.schema
	var recent []string
	if u.session != nil {
		recent = append(recent, u.session.RecentFiles...)
	}
	u.mu.Unlock()

	items := []paletteItem{
		{"Open Database…", "Ctrl+O", func() { u.openDialog(false) }},
		{"Open Database (Read-Only)…", "Ctrl+Shift+O", func() { u.openDialog(true) }},
		{"Create New Database…", "Ctrl+N", u.newDatabaseDialog},
		{"Execute .sql Script File (Streamed)…", "", u.runSQLFileDialog},
		{"Export Current Table as CSV…", "", func() { u.exportBrowseTable(false) }},
		{"Export Current Table as JSON…", "", func() { u.exportBrowseTable(true) }},
		{"VACUUM INTO Compact Copy…", "", u.vacuumIntoDialog},
		{"Import CSV / TSV into Table…", "", u.importCSVDialog},
		{"Export Database Dump (.sql)…", "", u.exportSQLDialog},
		{"Create New Table (Designer)…", "", u.openNewTableDesigner},
		{"Load 100,000,000-Row Virtualized Demo", "", u.loadDemo100M},
		{"Switch to Browse Data Tab", "", func() { u.activeTab = tabBrowse }},
		{"Switch to Execute SQL Tab", "", func() { u.activeTab = tabSQL }},
		{"Switch to Structure / DDL Tab", "", func() { u.activeTab = tabStructure }},
		{"Switch to DB Tools Tab", "", func() { u.activeTab = tabTools; u.refreshToolsInfo() }},
		{"Review & Commit Staged Changes", "Ctrl+S", func() { u.activeTab = tabReview }},
		{"Toggle Left Schema Sidebar", "Ctrl+B", func() { u.leftSplit.Collapsed = !u.leftSplit.Collapsed }},
		{"Toggle Right Cell Inspector", "Ctrl+I", func() { u.rightSplit.Collapsed = !u.rightSplit.Collapsed }},
		{"Toggle Dark / Light Theme", "", u.toggleTheme},
	}
	for _, p := range recent {
		path := p
		items = append(items, paletteItem{
			label: "Open Recent: " + filepath.Base(path),
			hint:  filepath.Dir(path),
			run:   func() { u.openDatabase(path, false) },
		})
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
		if q == "" || strings.Contains(strings.ToLower(it.label), q) || strings.Contains(strings.ToLower(it.hint), q) {
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

	boxW := min(gtx.Dp(540), size.X-gtx.Dp(40))
	boxH := min(gtx.Dp(360), size.Y-gtx.Dp(80))
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

	u.drawInputBox(cg, gtx.Dp(10), gtx.Dp(42), boxW-gtx.Dp(20), &u.paletteEdit, "Type a command, object, or recent file…")

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
		name := fmt.Sprintf("%s (%s)", filepath.Base(db.Path), data.FormatSize(dbSize))
		if db.ReadOnly {
			name += " RO"
		}
		leftParts = append(leftParts, name)
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
				leftParts = append(leftParts, fmt.Sprintf("%d staged change(s) (Ctrl+S)", n))
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
	btnH := gtx.Dp(26)
	y := (barH - btnH) / 2
	return u.drawPillButtonAt(gtx, x, y, btnH, label, active, accent, clk)
}

func (u *uiState) drawPillButtonAt(gtx layout.Context, x, y, btnH int, label string, active, accent bool, clk *gesture.Click) int {
	btnW := gtx.Dp(unit.Dp(utf8.RuneCountInString(label)*7 + 20))
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

	textOffY := max(2, (btnH-gtx.Dp(18))/2)
	off := op.Offset(image.Pt(x, y+textOffY)).Push(gtx.Ops)
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
