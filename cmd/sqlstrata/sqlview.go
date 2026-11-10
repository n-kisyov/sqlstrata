package main

import (
	"context"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"gioui.org/font"
	"gioui.org/gesture"
	"gioui.org/io/key"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/text"
	"github.com/ncruces/go-sqlite3"

	"github.com/sqlstrata/sqlstrata/internal/data"
	"github.com/sqlstrata/sqlstrata/internal/engine"
	"github.com/sqlstrata/sqlstrata/internal/iofmt"
	"github.com/sqlstrata/sqlstrata/internal/platform/win"
	"github.com/sqlstrata/sqlstrata/internal/sqltok"
	"github.com/sqlstrata/sqlstrata/internal/ui/theme"
	"github.com/sqlstrata/sqlstrata/internal/ui/widgets"
)

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
	u.saveQueryHistory(sqlText)
	u.win.Invalidate()

	stmts := sqltok.SplitStatements(sqlText)
	go func() {
		start := time.Now()
		var resGrid widgets.GridSource
		var resSpool *data.ResultSpool
		var rowsRead int64
		var affected int64
		var stmtsRun int
		var schemaDirty bool
		const staticLimit = 2_000
		const spoolCap = 500_000

		err := db.Fg.Do(ctx, func(c *sqlite3.Conn) error {
			for _, st := range stmts {
				if isDDLStatement(st.SQL) {
					schemaDirty = true
				}
				stmt, _, err := c.Prepare(st.SQL)
				if err != nil {
					return fmt.Errorf("line %d: %w", st.StartLine, err)
				}
				if stmt == nil {
					continue
				}
				stmtsRun++
				ncols := stmt.ColumnCount()
				if ncols == 0 {
					err = stmt.Exec()
					_ = stmt.Close()
					if err != nil {
						return fmt.Errorf("line %d: %w", st.StartLine, err)
					}
					affected += int64(c.Changes())
					continue
				}

				cols := make([]data.Column, ncols)
				for i := range ncols {
					cols[i] = data.Column{
						Name: stmt.ColumnName(i),
						Type: stmt.ColumnDeclType(i),
					}
				}
				curStatic := data.NewStaticSource(cols)
				var curSpool *data.ResultSpool
				var curRows int64
				for stmt.Step() {
					if curSpool == nil && curRows < staticLimit {
						for i := range ncols {
							switch stmt.ColumnType(i) {
							case sqlite3.NULL:
								curStatic.AppendCell(nil, data.KindNull)
							case sqlite3.INTEGER:
								curStatic.AppendCell(stmt.ColumnRawText(i), data.KindInteger)
							case sqlite3.FLOAT:
								curStatic.AppendCell(stmt.ColumnRawText(i), data.KindReal)
							case sqlite3.TEXT:
								v := stmt.ColumnRawText(i)
								if len(v) > data.PreviewBytes {
									v = v[:data.PreviewBytes]
								}
								curStatic.AppendCell(v, data.KindText)
							case sqlite3.BLOB:
								msg := "BLOB " + data.FormatSize(int64(len(stmt.ColumnRawBlob(i))))
								curStatic.AppendCell([]byte(msg), data.KindBlob)
							}
						}
					} else {
						if curSpool == nil {
							sp, serr := spillStaticToSpool(cols, curStatic)
							if serr != nil {
								_ = stmt.Close()
								return serr
							}
							curSpool = sp
							curStatic = nil
						}
						if err := curSpool.AppendRowFromStmt(stmt); err != nil {
							_ = curSpool.Close()
							_ = stmt.Close()
							return err
						}
					}
					curRows++
					if curRows >= spoolCap {
						break
					}
				}
				serr := stmt.Err()
				_ = stmt.Close()
				if serr != nil {
					if curSpool != nil {
						_ = curSpool.Close()
					}
					return fmt.Errorf("line %d: %w", st.StartLine, serr)
				}
				if resSpool != nil {
					_ = resSpool.Close()
					resSpool = nil
				}
				if curSpool != nil {
					if err := curSpool.Finish(); err != nil {
						_ = curSpool.Close()
						return err
					}
					resSpool = curSpool
					resGrid = curSpool
				} else {
					resGrid = curStatic
				}
				rowsRead = curRows
			}
			return nil
		})
		cancel()

		if err != nil && resSpool != nil {
			_ = resSpool.Close()
			resSpool = nil
		}

		if err == nil && schemaDirty {
			u.refreshSchemaAsync(db)
		}

		u.postUI(func() {
			u.mu.Lock()
			u.sqlRunning = false
			u.sqlCancel = nil
			src := u.tableSrc
			if err != nil {
				u.sqlStatus = fmt.Sprintf("Error: %v", err)
				u.mu.Unlock()
				return
			}
			if resGrid != nil {
				oldSpool := u.sqlSpool
				u.sqlSpool = resSpool
				suffix := ""
				if rowsRead >= spoolCap {
					suffix = fmt.Sprintf(" (spooled, capped at %d rows)", spoolCap)
				} else if resSpool != nil {
					suffix = " (spooled to disk)"
				}
				if stmtsRun > 1 {
					u.sqlStatus = fmt.Sprintf("%d statements · %d rows in %s%s", stmtsRun, rowsRead, time.Since(start).Round(time.Microsecond), suffix)
				} else {
					u.sqlStatus = fmt.Sprintf("%d rows in %s%s", rowsRead, time.Since(start).Round(time.Microsecond), suffix)
				}
				u.mu.Unlock()
				u.sqlGrid.SetSource(resGrid)
				if oldSpool != nil && oldSpool != resSpool {
					_ = oldSpool.Close()
				}
			} else {
				u.sqlStatus = fmt.Sprintf("%d statement(s), %d row(s) affected in %s", max(1, stmtsRun), affected, time.Since(start).Round(time.Microsecond))
				u.mu.Unlock()
			}
			if (affected > 0 || schemaDirty) && src != nil {
				src.Reload()
			}
		})
	}()
}

func spillStaticToSpool(cols []data.Column, st *data.StaticSource) (*data.ResultSpool, error) {
	sp, err := data.NewResultSpool(cols)
	if err != nil {
		return nil, err
	}
	nrows := st.RowCount()
	kinds := make([]data.Kind, len(cols))
	vals := make([][]byte, len(cols))
	for r := int64(0); r < nrows; r++ {
		for c := range cols {
			txt, k, _, _ := st.Cell(r, c)
			kinds[c] = k
			if k == data.KindNull {
				vals[c] = nil
			} else {
				vals[c] = []byte(txt)
			}
		}
		if err := sp.AppendRowValues(kinds, vals); err != nil {
			_ = sp.Close()
			return nil, err
		}
	}
	return sp, nil
}

func (u *uiState) runSQLFileDialog() {
	u.mu.Lock()
	db := u.db
	running := u.sqlRunning
	u.mu.Unlock()
	if db == nil {
		u.setStatus("Open a database first to run a .sql file")
		return
	}
	if running {
		return
	}
	hwnd := u.hwnd
	go func() {
		path, ok := win.OpenFile(hwnd, "Execute SQL Script (Streamed)", []win.Filter{
			{Name: "SQL Scripts (*.sql;*.txt)", Pattern: "*.sql;*.txt"},
			{Name: "All Files (*.*)", Pattern: "*.*"},
		})
		if ok && path != "" {
			u.runSQLFile(path)
		}
	}()
}

func (u *uiState) runSQLFile(path string) {
	u.mu.Lock()
	db := u.db
	if db == nil || u.sqlRunning {
		u.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	u.sqlRunning = true
	u.sqlCancel = cancel
	u.sqlStatus = "Streaming " + filepath.Base(path) + "…"
	u.activeTab = tabSQL
	u.mu.Unlock()
	u.win.Invalidate()

	go func() {
		start := time.Now()
		var stmtsRun int64
		var schemaDirty bool
		f, err := os.Open(path)
		if err == nil {
			defer f.Close()
			err = db.Fg.Do(ctx, func(c *sqlite3.Conn) error {
				batchTx := false
				rollback := func(rerr error) error {
					if !c.GetAutocommit() {
						_ = c.Exec("ROLLBACK")
					}
					return rerr
				}
				serr := sqltok.StreamStatements(f, func(st sqltok.Statement) error {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					kw := firstKeywordUpper(st.SQL)
					if isDDLStatement(st.SQL) {
						schemaDirty = true
					}
					switch kw {
					case "BEGIN":
						if batchTx {
							if cerr := c.Exec("COMMIT"); cerr != nil {
								return cerr
							}
							batchTx = false
						}
					case "PRAGMA", "VACUUM", "ATTACH", "DETACH":
						if batchTx {
							if cerr := c.Exec("COMMIT"); cerr != nil {
								return cerr
							}
							batchTx = false
						}
					case "COMMIT", "END", "ROLLBACK":
						// Executed directly below.
					default:
						if c.GetAutocommit() {
							if berr := c.Exec("BEGIN IMMEDIATE"); berr != nil {
								return berr
							}
							batchTx = true
						}
					}

					stmt, _, perr := c.Prepare(st.SQL)
					if perr != nil {
						return fmt.Errorf("line %d: %w", st.StartLine, perr)
					}
					if stmt == nil {
						return nil
					}
					err := stmt.Exec()
					_ = stmt.Close()
					if err != nil {
						return fmt.Errorf("line %d: %w", st.StartLine, err)
					}
					if c.GetAutocommit() {
						batchTx = false
					}
					stmtsRun++
					if stmtsRun%500 == 0 {
						u.mu.Lock()
						u.sqlStatus = fmt.Sprintf("Streaming %s: %d statements…", filepath.Base(path), stmtsRun)
						u.mu.Unlock()
						u.win.Invalidate()
					}
					return nil
				})
				if serr != nil {
					return rollback(serr)
				}
				if batchTx && !c.GetAutocommit() {
					if err := c.Exec("COMMIT"); err != nil {
						return rollback(err)
					}
				}
				return nil
			})
		}
		cancel()

		if err == nil && schemaDirty {
			u.refreshSchemaAsync(db)
		}

		u.postUI(func() {
			u.mu.Lock()
			u.sqlRunning = false
			u.sqlCancel = nil
			src := u.tableSrc
			if err != nil {
				u.sqlStatus = fmt.Sprintf("Script error: %v", err)
			} else {
				u.sqlStatus = fmt.Sprintf("Executed %s: %d statement(s) in %s", filepath.Base(path), stmtsRun, time.Since(start).Round(time.Millisecond))
			}
			u.mu.Unlock()
			if err == nil && src != nil {
				src.Reload()
			}
		})
	}()
}

func firstKeywordUpper(sql string) string {
	toks := sqltok.Tokenize(sql)
	for _, tk := range toks {
		if tk.Kind == sqltok.TokenWhitespace || tk.Kind == sqltok.TokenComment {
			continue
		}
		if tk.Kind == sqltok.TokenKeyword {
			return strings.ToUpper(tk.Text(sql))
		}
		return ""
	}
	return ""
}

func isDDLStatement(sql string) bool {
	switch firstKeywordUpper(sql) {
	case "CREATE", "ALTER", "DROP", "VACUUM", "ATTACH", "DETACH", "REINDEX":
		return true
	}
	return false
}

func (u *uiState) refreshSchemaAsync(db *engine.DB) {
	if db == nil {
		return
	}
	var schema *engine.Schema
	err := db.Fg.Do(context.Background(), func(c *sqlite3.Conn) error {
		var err error
		schema, err = engine.LoadSchema(c)
		return err
	})
	if err != nil {
		return
	}
	u.postUI(func() {
		u.mu.Lock()
		sameDB := u.db == db
		if sameDB {
			u.schema = schema
			if fi, serr := os.Stat(db.Path); serr == nil {
				u.dbSize = fi.Size()
			}
		}
		u.mu.Unlock()
		if sameDB {
			u.tree.SetSchema(schema)
		}
	})
}

func (u *uiState) exportSQLResults(asJSON bool) {
	src, ok := u.sqlGrid.Source().(iofmt.GridSourceReader)
	if !ok || src == nil || src.RowCount() == 0 {
		u.setStatus("Run a query with result rows first before exporting")
		return
	}
	hwnd := u.hwnd
	ext := "csv"
	filters := []win.Filter{{Name: "CSV Files (*.csv)", Pattern: "*.csv"}, {Name: "All Files (*.*)", Pattern: "*.*"}}
	if asJSON {
		ext = "json"
		filters = []win.Filter{{Name: "JSON Files (*.json)", Pattern: "*.json"}, {Name: "All Files (*.*)", Pattern: "*.*"}}
	}
	go func() {
		path, ok := win.SaveFile(hwnd, "Export Query Results", "query_results."+ext, ext, filters)
		if !ok || path == "" {
			return
		}
		start := time.Now()
		f, err := os.Create(path)
		if err != nil {
			u.setStatus(fmt.Sprintf("Export failed: %v", err))
			return
		}
		var n int64
		if asJSON {
			n, err = iofmt.ExportGridSourceJSON(src, f)
		} else {
			n, err = iofmt.ExportGridSourceCSV(src, f)
		}
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			u.setStatus(fmt.Sprintf("Export failed: %v", err))
			return
		}
		u.setStatus(fmt.Sprintf("Exported %d row(s) to %s in %s", n, filepath.Base(path), time.Since(start).Round(time.Millisecond)))
	}()
}

func (u *uiState) exportBrowseTable(asJSON bool) {
	u.mu.Lock()
	db := u.db
	src := u.tableSrc
	tbl := u.activeTable
	u.mu.Unlock()
	if db == nil || src == nil {
		u.setStatus("Open a database table first before exporting")
		return
	}
	ext := "csv"
	filters := []win.Filter{{Name: "CSV Files (*.csv)", Pattern: "*.csv"}, {Name: "All Files (*.*)", Pattern: "*.*"}}
	if asJSON {
		ext = "json"
		filters = []win.Filter{{Name: "JSON Files (*.json)", Pattern: "*.json"}, {Name: "All Files (*.*)", Pattern: "*.*"}}
	}
	query := src.ExportSQL()
	hwnd := u.hwnd
	go func() {
		path, ok := win.SaveFile(hwnd, "Export Table Data", tbl+"."+ext, ext, filters)
		if !ok || path == "" {
			return
		}
		u.setStatus(fmt.Sprintf("Exporting %s to %s…", tbl, filepath.Base(path)))
		start := time.Now()
		f, err := os.Create(path)
		if err != nil {
			u.setStatus(fmt.Sprintf("Export failed: %v", err))
			return
		}
		var n int64
		err = db.Bg.Do(context.Background(), func(c *sqlite3.Conn) error {
			var eerr error
			if asJSON {
				n, eerr = iofmt.ExportQueryJSON(context.Background(), c, query, f)
			} else {
				n, eerr = iofmt.ExportQueryCSV(context.Background(), c, query, f)
			}
			return eerr
		})
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			u.setStatus(fmt.Sprintf("Export failed: %v", err))
			return
		}
		u.setStatus(fmt.Sprintf("Exported %d row(s) of %s to %s in %s", n, tbl, filepath.Base(path), time.Since(start).Round(time.Millisecond)))
	}()
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

func (u *uiState) layoutSQLEditorPane(gtx layout.Context) layout.Dimensions {
	theme.Fill(gtx, u.th.Surface)
	barH := gtx.Dp(34)
	w := gtx.Constraints.Max.X

	for {
		if _, ok := u.sqlEdit.Update(gtx); !ok {
			break
		}
	}
	if txt := u.sqlEdit.Text(); strings.ContainsAny(txt, "\r\t") {
		clean := strings.NewReplacer("\r\n", "\n", "\r", "\n", "\t", "    ").Replace(txt)
		if clean != txt {
			startRune, endRune := u.sqlEdit.Selection()
			u.sqlEdit.SetText(clean)
			maxR := utf8.RuneCountInString(clean)
			u.sqlEdit.SetCaret(min(maxR, startRune), min(maxR, endRune))
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
	if clicked(&u.btnRunSQLFile, gtx) {
		u.runSQLFileDialog()
	}
	if clicked(&u.btnSQLHistory, gtx) {
		u.sqlHistoryOpen = !u.sqlHistoryOpen
	}
	if clicked(&u.btnSQLExpCSV, gtx) {
		u.exportSQLResults(false)
	}
	if clicked(&u.btnSQLExpJSON, gtx) {
		u.exportSQLResults(true)
	}

	u.mu.Lock()
	schema := u.schema
	running := u.sqlRunning
	status := u.sqlStatus
	var history []string
	if u.session != nil && u.sqlHistoryOpen {
		history = append(history, u.session.QueryHistory...)
	}
	u.mu.Unlock()

	sqlText := u.sqlEdit.Text()
	selStart, selEnd := u.sqlEdit.Selection()
	byteCaret := byteOffsetForRune(sqlText, selStart)
	prefix, suggestions := sqltok.Complete(sqlText, byteCaret, schema, 7)

	if len(u.sqlSuggestClicks) < len(suggestions) {
		u.sqlSuggestClicks = make([]gesture.Click, len(suggestions))
	}
	for i, s := range suggestions {
		if clicked(&u.sqlSuggestClicks[i], gtx) {
			suffix := s[min(len(s), len(prefix)):]
			if strings.HasPrefix(strings.ToLower(s), strings.ToLower(prefix)) {
				// Replace prefix completely to match candidate casing cleanly.
				prefRunes := utf8.RuneCountInString(prefix)
				u.sqlEdit.SetCaret(max(0, selStart-prefRunes), selEnd)
				u.sqlEdit.Insert(s)
			} else {
				u.sqlEdit.Insert(suffix)
			}
			gtx.Execute(key.FocusCmd{Tag: &u.sqlEdit})
		}
	}

	if len(u.sqlHistClicks) < len(history) {
		u.sqlHistClicks = make([]gesture.Click, len(history))
	}
	for i, q := range history {
		if clicked(&u.sqlHistClicks[i], gtx) {
			u.sqlEdit.SetText(q)
			u.sqlHistoryOpen = false
			gtx.Execute(key.FocusCmd{Tag: &u.sqlEdit})
		}
	}

	hasResults := u.sqlGrid.Source() != nil && u.sqlGrid.Source().RowCount() > 0

	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			theme.FillRect(gtx, clip.Rect{Max: image.Pt(w, barH)}, u.th.SurfaceAlt)
			x := gtx.Dp(10)

			x += u.drawPillButton(gtx, x, barH, "▶ Run (Ctrl+Enter)", true, true, &u.btnRunSQL) + gtx.Dp(6)
			if running {
				x += u.drawPillButton(gtx, x, barH, "■ Cancel", false, false, &u.btnCancelSQL) + gtx.Dp(6)
			}
			x += u.drawPillButton(gtx, x, barH, "Run .sql File…", false, false, &u.btnRunSQLFile) + gtx.Dp(6)
			x += u.drawPillButton(gtx, x, barH, "History", u.sqlHistoryOpen, false, &u.btnSQLHistory) + gtx.Dp(6)
			if hasResults {
				x += u.drawPillButton(gtx, x, barH, "CSV", false, false, &u.btnSQLExpCSV) + gtx.Dp(4)
				x += u.drawPillButton(gtx, x, barH, "JSON", false, false, &u.btnSQLExpJSON) + gtx.Dp(10)
			}
			if status != "" && x < w-gtx.Dp(40) {
				off := op.Offset(image.Pt(x+gtx.Dp(4), gtx.Dp(8))).Push(gtx.Ops)
				cg := gtx
				cg.Constraints.Max.X = max(0, w-x-gtx.Dp(12))
				u.th.LabelFont(cg, font.Font{Typeface: u.th.Mono}, u.th.Small, u.th.TextMuted, text.Start, status)
				off.Pop()
			}
			theme.FillRect(gtx, clip.Rect{Min: image.Pt(0, barH-1), Max: image.Pt(w, barH)}, u.th.BorderSubtle)
			return layout.Dimensions{Size: image.Pt(w, barH)}
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			if len(suggestions) == 0 {
				return layout.Dimensions{}
			}
			sBarH := gtx.Dp(30)
			theme.FillRect(gtx, clip.Rect{Max: image.Pt(w, sBarH)}, u.th.Chrome)
			x := gtx.Dp(10)
			off := op.Offset(image.Pt(x, gtx.Dp(7))).Push(gtx.Ops)
			u.th.Label(gtx, u.th.Small, u.th.TextSubtle, "Suggest:")
			off.Pop()
			x += gtx.Dp(60)
			for i, s := range suggestions {
				if x > w-gtx.Dp(90) {
					break
				}
				x += u.drawPillButton(gtx, x, sBarH, s, i == 0, false, &u.sqlSuggestClicks[i]) + gtx.Dp(5)
			}
			theme.FillRect(gtx, clip.Rect{Min: image.Pt(0, sBarH-1), Max: image.Pt(w, sBarH)}, u.th.BorderSubtle)
			return layout.Dimensions{Size: image.Pt(w, sBarH)}
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			if !u.sqlHistoryOpen || len(history) == 0 {
				return layout.Dimensions{}
			}
			hBarH := gtx.Dp(32)
			theme.FillRect(gtx, clip.Rect{Max: image.Pt(w, hBarH)}, u.th.Chrome)
			x := gtx.Dp(10)
			off := op.Offset(image.Pt(x, gtx.Dp(8))).Push(gtx.Ops)
			u.th.Label(gtx, u.th.Small, u.th.TextSubtle, "Recent:")
			off.Pop()
			x += gtx.Dp(56)
			for i, q := range history {
				if x > w-gtx.Dp(120) {
					break
				}
				oneLine := strings.Join(strings.Fields(q), " ")
				if utf8.RuneCountInString(oneLine) > 28 {
					rs := []rune(oneLine)
					oneLine = string(rs[:28]) + "…"
				}
				x += u.drawPillButton(gtx, x, hBarH, oneLine, false, false, &u.sqlHistClicks[i]) + gtx.Dp(5)
			}
			theme.FillRect(gtx, clip.Rect{Min: image.Pt(0, hBarH-1), Max: image.Pt(w, hBarH)}, u.th.BorderSubtle)
			return layout.Dimensions{Size: image.Pt(w, hBarH)}
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

func byteOffsetForRune(s string, runeIndex int) int {
	if runeIndex <= 0 {
		return 0
	}
	i := 0
	for off := range s {
		if i == runeIndex {
			return off
		}
		i++
	}
	return len(s)
}

