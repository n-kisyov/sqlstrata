package main

import (
	"context"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gioui.org/font"
	"gioui.org/gesture"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/text"
	"github.com/ncruces/go-sqlite3"

	"github.com/sqlstrata/sqlstrata/internal/data"
	"github.com/sqlstrata/sqlstrata/internal/engine"
	"github.com/sqlstrata/sqlstrata/internal/iofmt"
	"github.com/sqlstrata/sqlstrata/internal/platform/win"
	"github.com/sqlstrata/sqlstrata/internal/ui/theme"
)

func (u *uiState) newDatabaseDialog() {
	hwnd := u.hwnd
	go func() {
		path, ok := win.SaveFile(hwnd, "Create New SQLite Database", "database.db", "db", []win.Filter{
			{Name: "SQLite Databases (*.db;*.sqlite;*.sqlite3)", Pattern: "*.db;*.sqlite;*.sqlite3"},
			{Name: "All Files (*.*)", Pattern: "*.*"},
		})
		if ok && path != "" {
			cleanNew := filepath.Clean(path)
			u.mu.Lock()
			var openPath string
			if u.db != nil {
				openPath = filepath.Clean(u.db.Path)
			}
			u.mu.Unlock()
			if openPath != "" && strings.EqualFold(cleanNew, openPath) {
				u.setStatus("Cannot overwrite the currently open database file")
				return
			}
			// SaveFile confirmed overwrite if file existed; ensure a fresh empty DB.
			for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
				_ = os.Remove(path + suffix)
			}
			u.openDatabase(path, false)
		}
	}()
}

func (u *uiState) refreshToolsInfo() {
	u.mu.Lock()
	db := u.db
	dbSize := u.dbSize
	u.mu.Unlock()
	if db == nil {
		u.toolsGrid.SetSource(nil)
		return
	}
	go func() {
		src := data.NewStaticSource([]data.Column{
			{Name: "property", Type: "TEXT"},
			{Name: "value", Type: "TEXT"},
		})
		addKV := func(k, v string) {
			src.AppendCell([]byte(k), data.KindText)
			src.AppendCell([]byte(v), data.KindText)
		}
		addKV("File Path", db.Path)
		addKV("File Size", fmt.Sprintf("%s (%d bytes)", data.FormatSize(dbSize), dbSize))
		if db.ReadOnly {
			addKV("Access Mode", "Read-Only")
		} else {
			addKV("Access Mode", "Read-Write")
		}

		pragmas := []struct {
			label string
			sql   string
		}{
			{"SQLite Version", "SELECT sqlite_version()"},
			{"Journal Mode", "PRAGMA journal_mode"},
			{"Page Size", "PRAGMA page_size"},
			{"Page Count", "PRAGMA page_count"},
			{"Freelist Count", "PRAGMA freelist_count"},
			{"Encoding", "PRAGMA encoding"},
			{"Auto Vacuum", "PRAGMA auto_vacuum"},
			{"User Version", "PRAGMA user_version"},
			{"Schema Version", "PRAGMA schema_version"},
			{"Application ID", "PRAGMA application_id"},
		}
		_ = db.Fg.Do(context.Background(), func(c *sqlite3.Conn) error {
			for _, p := range pragmas {
				stmt, _, err := c.Prepare(p.sql)
				if err != nil {
					continue
				}
				if stmt.Step() {
					addKV(p.label, stmt.ColumnText(0))
				}
				_ = stmt.Close()
			}
			return nil
		})
		u.postUI(func() {
			u.mu.Lock()
			same := u.db == db
			u.mu.Unlock()
			if same {
				u.toolsGrid.SetSource(src)
			}
		})
	}()
}

func (u *uiState) runMaintenancePragma(label, pragmaSQL string) {
	u.mu.Lock()
	db := u.db
	u.mu.Unlock()
	if db == nil {
		u.setStatus("Open a database first")
		return
	}
	u.setToolsStatus("Running " + label + "…")
	go func() {
		start := time.Now()
		var res *data.StaticSource
		err := db.Bg.Do(context.Background(), func(c *sqlite3.Conn) error {
			stmt, _, err := c.Prepare(pragmaSQL)
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
				cols[i] = data.Column{Name: stmt.ColumnName(i), Type: stmt.ColumnDeclType(i)}
			}
			res = data.NewStaticSource(cols)
			for stmt.Step() {
				for i := range ncols {
					if stmt.ColumnType(i) == sqlite3.NULL {
						res.AppendCell(nil, data.KindNull)
					} else {
						res.AppendCell(stmt.ColumnRawText(i), data.KindText)
					}
				}
				if res.RowCount() >= 5000 {
					break
				}
			}
			return stmt.Err()
		})
		if err != nil {
			u.setToolsStatus(fmt.Sprintf("%s failed: %v", label, err))
			return
		}
		elapsed := time.Since(start).Round(time.Millisecond)
		if res != nil {
			if res.RowCount() == 0 {
				res = data.NewStaticSource([]data.Column{{Name: "result", Type: "TEXT"}})
				res.AppendCell([]byte(label+": ok (0 issues)"), data.KindText)
			}
			u.postUI(func() {
				u.toolsGrid.SetSource(res)
			})
			u.setToolsStatus(fmt.Sprintf("%s completed in %s (%d row(s))", label, elapsed, res.RowCount()))
		} else {
			u.setToolsStatus(fmt.Sprintf("%s completed in %s", label, elapsed))
			u.refreshToolsInfo()
		}
	}()
}

func (u *uiState) vacuumIntoDialog() {
	u.mu.Lock()
	db := u.db
	u.mu.Unlock()
	if db == nil {
		u.setStatus("Open a database first")
		return
	}
	hwnd := u.hwnd
	base := strings.TrimSuffix(filepath.Base(db.Path), filepath.Ext(db.Path))
	go func() {
		outPath, ok := win.SaveFile(hwnd, "VACUUM INTO Compact Copy", base+"_compact.db", "db", []win.Filter{
			{Name: "SQLite Databases (*.db;*.sqlite;*.sqlite3)", Pattern: "*.db;*.sqlite;*.sqlite3"},
			{Name: "All Files (*.*)", Pattern: "*.*"},
		})
		if !ok || outPath == "" {
			return
		}
		if strings.EqualFold(filepath.Clean(outPath), filepath.Clean(db.Path)) {
			u.setToolsStatus("VACUUM INTO target cannot be the open database itself")
			return
		}
		_ = os.Remove(outPath)
		u.setToolsStatus("Running VACUUM INTO " + filepath.Base(outPath) + "…")
		start := time.Now()
		err := db.Fg.Do(context.Background(), func(c *sqlite3.Conn) error {
			stmt, _, err := c.Prepare("VACUUM INTO ?")
			if err != nil {
				return err
			}
			defer stmt.Close()
			if err := stmt.BindText(1, outPath); err != nil {
				return err
			}
			return stmt.Exec()
		})
		if err != nil {
			u.setToolsStatus(fmt.Sprintf("VACUUM INTO failed: %v", err))
			return
		}
		var szTxt string
		if fi, serr := os.Stat(outPath); serr == nil {
			szTxt = " (" + data.FormatSize(fi.Size()) + ")"
		}
		u.setToolsStatus(fmt.Sprintf("Created compact copy %s%s in %s", filepath.Base(outPath), szTxt, time.Since(start).Round(time.Millisecond)))
	}()
}

func (u *uiState) importCSVDialog() {
	u.mu.Lock()
	db := u.db
	u.mu.Unlock()
	if db == nil {
		u.setStatus("Open a database first to import CSV/TSV")
		return
	}
	if db.ReadOnly {
		u.setToolsStatus("Cannot import CSV: database opened read-only")
		return
	}
	hwnd := u.hwnd
	go func() {
		path, ok := win.OpenFile(hwnd, "Import CSV / TSV into New Table", []win.Filter{
			{Name: "CSV / TSV Files (*.csv;*.tsv;*.txt)", Pattern: "*.csv;*.tsv;*.txt"},
			{Name: "All Files (*.*)", Pattern: "*.*"},
		})
		if ok && path != "" {
			u.importCSVFile(path)
		}
	}()
}

func (u *uiState) importCSVFile(csvPath string) {
	u.mu.Lock()
	db := u.db
	schema := u.schema
	u.mu.Unlock()
	if db == nil {
		return
	}
	if db.ReadOnly {
		u.setToolsStatus("Cannot import CSV: database opened read-only")
		return
	}

	tableName := uniqueCSVTableName(schema, csvPath)
	u.setToolsStatus("Importing " + filepath.Base(csvPath) + " into " + tableName + "…")
	start := time.Now()

	f, err := os.Open(csvPath)
	if err != nil {
		u.setToolsStatus(fmt.Sprintf("CSV open failed: %v", err))
		return
	}
	defer f.Close()

	opt := iofmt.DefaultImportOptions(tableName)
	var res iofmt.ImportResult
	err = db.Fg.Do(context.Background(), func(c *sqlite3.Conn) error {
		var ierr error
		res, ierr = iofmt.ImportCSV(context.Background(), c, f, opt)
		return ierr
	})
	if err != nil {
		u.setToolsStatus(fmt.Sprintf("CSV import failed: %v", err))
		return
	}

	u.refreshSchemaAsync(db)
	u.refreshToolsInfo()
	u.setToolsStatus(fmt.Sprintf("Imported %d rows into %q (%d cols, delim %q) in %s",
		res.Rows, res.Table, len(res.Columns), res.Comma, time.Since(start).Round(time.Millisecond)))
}

func uniqueCSVTableName(schema *engine.Schema, csvPath string) string {
	base := strings.TrimSuffix(filepath.Base(csvPath), filepath.Ext(csvPath))
	base = strings.TrimSpace(base)
	if base == "" {
		base = "imported_csv"
	}
	if schema == nil || schema.Find(base) == nil {
		return base
	}
	for n := 2; n < 1000; n++ {
		cand := fmt.Sprintf("%s_%d", base, n)
		if schema.Find(cand) == nil {
			return cand
		}
	}
	return fmt.Sprintf("%s_%d", base, time.Now().Unix())
}

func (u *uiState) exportSQLDialog() {
	u.mu.Lock()
	db := u.db
	var dbPath string
	if db != nil {
		dbPath = db.Path
	}
	u.mu.Unlock()
	if db == nil {
		u.setStatus("Open a database first to export SQL dump")
		return
	}
	stem := strings.TrimSuffix(filepath.Base(dbPath), filepath.Ext(dbPath))
	if stem == "" {
		stem = "database"
	}
	defaultName := stem + "_dump.sql"
	hwnd := u.hwnd
	go func() {
		outPath, ok := win.SaveFile(hwnd, "Export Database as SQL Dump", defaultName, "sql", []win.Filter{
			{Name: "SQL Script (*.sql)", Pattern: "*.sql"},
			{Name: "All Files (*.*)", Pattern: "*.*"},
		})
		if ok && outPath != "" {
			u.exportSQLDump(outPath)
		}
	}()
}

func (u *uiState) exportSQLDump(outPath string) {
	u.mu.Lock()
	db := u.db
	u.mu.Unlock()
	if db == nil {
		return
	}

	u.setToolsStatus("Exporting SQL dump " + filepath.Base(outPath) + "…")
	start := time.Now()
	f, err := os.Create(outPath)
	if err != nil {
		u.setToolsStatus(fmt.Sprintf("SQL dump create failed: %v", err))
		return
	}

	var rows int64
	err = db.Bg.Do(context.Background(), func(c *sqlite3.Conn) error {
		var derr error
		rows, derr = iofmt.DumpDatabaseSQL(context.Background(), c, f)
		return derr
	})
	cerr := f.Close()
	if err != nil {
		_ = os.Remove(outPath)
		u.setToolsStatus(fmt.Sprintf("SQL dump failed: %v", err))
		return
	}
	if cerr != nil {
		_ = os.Remove(outPath)
		u.setToolsStatus(fmt.Sprintf("SQL dump close failed: %v", cerr))
		return
	}
	var szTxt string
	if fi, serr := os.Stat(outPath); serr == nil {
		szTxt = " (" + data.FormatSize(fi.Size()) + ")"
	}
	u.setToolsStatus(fmt.Sprintf("Exported %d rows to %s%s in %s",
		rows, filepath.Base(outPath), szTxt, time.Since(start).Round(time.Millisecond)))
}

func (u *uiState) setToolsStatus(msg string) {
	u.mu.Lock()
	u.toolsStatus = msg
	u.statusMsg = msg
	u.mu.Unlock()
	u.win.Invalidate()
}

func (u *uiState) layoutToolsView(gtx layout.Context) layout.Dimensions {
	theme.Fill(gtx, u.th.Surface)
	u.mu.Lock()
	db := u.db
	status := u.toolsStatus
	u.mu.Unlock()

	if db == nil {
		return layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return u.th.Label(gtx, u.th.UISize, u.th.TextSubtle, "Open a database to inspect PRAGMAs, run integrity checks, or run VACUUM INTO.")
		})
	}

	if clicked(&u.btnToolsRefresh, gtx) {
		u.setToolsStatus("Refreshed database metadata")
		u.refreshToolsInfo()
	}
	if clicked(&u.btnToolsQuickChk, gtx) {
		u.runMaintenancePragma("PRAGMA quick_check", "PRAGMA quick_check")
	}
	if clicked(&u.btnToolsIntegChk, gtx) {
		u.runMaintenancePragma("PRAGMA integrity_check", "PRAGMA integrity_check")
	}
	if clicked(&u.btnToolsFKChk, gtx) {
		u.runMaintenancePragma("PRAGMA foreign_key_check", "PRAGMA foreign_key_check")
	}
	if clicked(&u.btnToolsOptimize, gtx) {
		u.runMaintenancePragma("PRAGMA optimize", "PRAGMA optimize")
	}
	if clicked(&u.btnToolsVacuumInto, gtx) {
		u.vacuumIntoDialog()
	}
	if clicked(&u.btnToolsImportCSV, gtx) {
		u.importCSVDialog()
	}
	if clicked(&u.btnToolsExportSQL, gtx) {
		u.exportSQLDialog()
	}

	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			barH := gtx.Dp(40)
			w := gtx.Constraints.Max.X
			theme.FillRect(gtx, clip.Rect{Max: image.Pt(w, barH)}, u.th.SurfaceAlt)

			x := gtx.Dp(10)
			x += u.drawPillButton(gtx, x, barH, "Refresh Info", false, false, &u.btnToolsRefresh) + gtx.Dp(6)
			x += u.drawPillButton(gtx, x, barH, "Quick Check", false, false, &u.btnToolsQuickChk) + gtx.Dp(6)
			x += u.drawPillButton(gtx, x, barH, "Integrity Check", false, false, &u.btnToolsIntegChk) + gtx.Dp(6)
			x += u.drawPillButton(gtx, x, barH, "FK Check", false, false, &u.btnToolsFKChk) + gtx.Dp(6)
			x += u.drawPillButton(gtx, x, barH, "Optimize", false, false, &u.btnToolsOptimize) + gtx.Dp(6)
			x += u.drawPillButton(gtx, x, barH, "VACUUM INTO…", false, true, &u.btnToolsVacuumInto) + gtx.Dp(6)
			x += u.drawPillButton(gtx, x, barH, "Import CSV…", false, false, &u.btnToolsImportCSV) + gtx.Dp(6)
			x += u.drawPillButton(gtx, x, barH, "Export SQL…", false, false, &u.btnToolsExportSQL) + gtx.Dp(12)

			if status != "" && x < w-gtx.Dp(40) {
				off := op.Offset(image.Pt(x, gtx.Dp(11))).Push(gtx.Ops)
				cg := gtx
				cg.Constraints.Max.X = max(0, w-x-gtx.Dp(12))
				u.th.LabelFont(cg, font.Font{Typeface: u.th.Mono}, u.th.Small, u.th.TextMuted, text.Start, status)
				off.Pop()
			}
			theme.FillRect(gtx, clip.Rect{Min: image.Pt(0, barH-1), Max: image.Pt(w, barH)}, u.th.Border)
			return layout.Dimensions{Size: image.Pt(w, barH)}
		}),
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			return u.toolsGrid.Layout(gtx, u.th)
		}),
	)
}

func (u *uiState) layoutWelcomeView(gtx layout.Context) layout.Dimensions {
	theme.Fill(gtx, u.th.Surface)

	if clicked(&u.btnWelOpen, gtx) {
		u.openDialog(false)
	}
	if clicked(&u.btnWelOpenRO, gtx) {
		u.openDialog(true)
	}
	if clicked(&u.btnWelNew, gtx) {
		u.newDatabaseDialog()
	}
	if clicked(&u.btnWelDemo, gtx) {
		u.loadDemo100M()
	}

	u.mu.Lock()
	var recent []string
	if u.session != nil {
		recent = append(recent, u.session.RecentFiles...)
	}
	u.mu.Unlock()

	if len(u.recentClicks) < len(recent) {
		u.recentClicks = make([]gesture.Click, len(recent))
	}
	for i, p := range recent {
		if clicked(&u.recentClicks[i], gtx) {
			u.openDatabase(p, false)
		}
	}

	size := gtx.Constraints.Max
	cardW := min(gtx.Dp(620), max(gtx.Dp(320), size.X-gtx.Dp(60)))
	topY := max(gtx.Dp(32), (size.Y-gtx.Dp(420))/2)
	x0 := (size.X - cardW) / 2

	off := op.Offset(image.Pt(x0, topY)).Push(gtx.Ops)
	defer off.Pop()

	oTitle := op.Offset(image.Pt(0, 0)).Push(gtx.Ops)
	u.th.LabelFont(gtx, font.Font{Typeface: u.th.UI, Weight: font.SemiBold}, 20, u.th.Text, text.Start, "SQLStrata")
	oTitle.Pop()

	oSub := op.Offset(image.Pt(0, gtx.Dp(30))).Push(gtx.Ops)
	u.th.Label(gtx, u.th.UISize, u.th.TextMuted, "Native, memory-bounded SQLite GUI editor for huge databases.")
	oSub.Pop()

	y := gtx.Dp(64)
	ax := 0
	ax += u.drawPillButtonAt(gtx, ax, y, gtx.Dp(32), "Open DB… (Ctrl+O)", true, true, &u.btnWelOpen) + gtx.Dp(8)
	ax += u.drawPillButtonAt(gtx, ax, y, gtx.Dp(32), "Open Read-Only…", false, false, &u.btnWelOpenRO) + gtx.Dp(8)
	ax += u.drawPillButtonAt(gtx, ax, y, gtx.Dp(32), "New DB… (Ctrl+N)", false, false, &u.btnWelNew) + gtx.Dp(8)
	u.drawPillButtonAt(gtx, ax, y, gtx.Dp(32), "100M Row Demo", false, false, &u.btnWelDemo)

	y += gtx.Dp(54)
	if len(recent) > 0 {
		oHdr := op.Offset(image.Pt(0, y)).Push(gtx.Ops)
		u.th.LabelFont(gtx, font.Font{Typeface: u.th.UI, Weight: font.SemiBold}, u.th.Small, u.th.TextSubtle, text.Start, "RECENT DATABASES")
		oHdr.Pop()
		y += gtx.Dp(24)
		rowH := gtx.Dp(30)
		maxShow := min(len(recent), 8)
		for i := range maxShow {
			p := recent[i]
			clk := &u.recentClicks[i]
			r := image.Rect(0, y, cardW, y+rowH-2)
			if clk.Hovered() {
				theme.FillRect(gtx, clip.Rect(r), u.th.Hover)
			} else {
				theme.FillRect(gtx, clip.Rect(r), u.th.Chrome)
			}
			theme.FillRect(gtx, clip.Rect{Min: image.Pt(0, y+rowH-3), Max: image.Pt(cardW, y+rowH-2)}, u.th.BorderSubtle)

			oName := op.Offset(image.Pt(gtx.Dp(10), y+gtx.Dp(6))).Push(gtx.Ops)
			u.th.LabelFont(gtx, font.Font{Typeface: u.th.UI, Weight: font.SemiBold}, u.th.Small, u.th.Text, text.Start, strconv.Itoa(i+1)+". "+filepath.Base(p))
			oName.Pop()

			oPath := op.Offset(image.Pt(gtx.Dp(160), y+gtx.Dp(7))).Push(gtx.Ops)
			cg := gtx
			cg.Constraints.Max.X = max(0, cardW-gtx.Dp(172))
			u.th.LabelFont(cg, font.Font{Typeface: u.th.Mono}, u.th.Small, u.th.TextSubtle, text.Start, p)
			oPath.Pop()

			hit := clip.Rect(r).Push(gtx.Ops)
			clk.Add(gtx.Ops)
			hit.Pop()
			y += rowH
		}
	}

	return layout.Dimensions{Size: size}
}

