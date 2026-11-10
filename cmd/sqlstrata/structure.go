package main

import (
	"context"
	"fmt"
	"image"
	"io"
	"strconv"
	"strings"

	"gioui.org/font"
	"gioui.org/gesture"
	"gioui.org/io/clipboard"
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
	"github.com/sqlstrata/sqlstrata/internal/sqltok"
	"github.com/sqlstrata/sqlstrata/internal/ui/theme"
)

var designerColTypes = []string{"INTEGER", "TEXT", "REAL", "BLOB", "NUMERIC"}

type designerColRow struct {
	nameEd    widget.Editor
	dfltEd    widget.Editor
	typeIdx   int
	pk        bool
	notNull   bool
	typeClick gesture.Click
	pkClick   gesture.Click
	nnClick   gesture.Click
	delClick  gesture.Click
}

func newDesignerColRow(name string, typeIdx int, pk, notNull bool, dflt string) *designerColRow {
	r := &designerColRow{
		typeIdx: typeIdx,
		pk:      pk,
		notNull: notNull,
	}
	r.nameEd.SingleLine = true
	r.dfltEd.SingleLine = true
	r.nameEd.SetText(name)
	r.dfltEd.SetText(dflt)
	return r
}

func (u *uiState) structureHasFocus(gtx layout.Context) bool {
	if u.addColOpen && (gtx.Focused(&u.addColName) || gtx.Focused(&u.addColDflt)) {
		return true
	}
	if u.newTableOpen {
		if gtx.Focused(&u.ntTableName) {
			return true
		}
		for _, r := range u.ntCols {
			if gtx.Focused(&r.nameEd) || gtx.Focused(&r.dfltEd) {
				return true
			}
		}
	}
	return false
}

// formatDDLForDisplay normalizes line endings and expands indentation tabs in
// SQL whitespace/comments to spaces so Gio's monospace shaper never renders
// missing-glyph rectangles for \t.
func formatDDLForDisplay(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "-- (Internal or auto-generated SQLite schema object)"
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")

	toks := sqltok.Tokenize(s)
	var b strings.Builder
	b.Grow(len(s) + 16)
	for _, tk := range toks {
		part := tk.Text(s)
		if tk.Kind == sqltok.TokenWhitespace || tk.Kind == sqltok.TokenComment {
			part = strings.ReplaceAll(part, "\t", "    ")
		}
		b.WriteString(part)
	}
	out := strings.TrimSpace(b.String())
	if out != "" && !strings.HasSuffix(out, ";") {
		out += ";"
	}
	return out
}

func (u *uiState) loadStructure(obj *engine.Object) {
	u.mu.Lock()
	db := u.db
	schema := u.schema
	u.structObj = obj
	u.structIndex = nil
	u.confirmEmptyTarget = ""
	u.confirmDropName = ""
	u.confirmDropType = ""
	u.addColOpen = false
	u.newTableOpen = false
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

	u.ddlEdit.SetText(formatDDLForDisplay(obj.SQL))

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
					u.postUI(func() {
						u.mu.Lock()
						ok := u.structObj == obj
						u.mu.Unlock()
						if ok {
							u.structGrid.SetSource(buildIndexStructSource(obj, info))
						}
					})
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

func (u *uiState) openNewTableDesigner() {
	u.mu.Lock()
	db := u.db
	schema := u.schema
	u.mu.Unlock()
	if db == nil {
		u.setStatus("Open a database first to create a table")
		return
	}
	if db.ReadOnly {
		u.setStatus("Cannot create table: database opened read-only")
		return
	}
	u.newTableOpen = true
	u.addColOpen = false
	name := "new_table"
	if schema != nil && schema.Find(name) != nil {
		for n := 2; n < 1000; n++ {
			cand := fmt.Sprintf("new_table_%d", n)
			if schema.Find(cand) == nil {
				name = cand
				break
			}
		}
	}
	u.ntTableName.SetText(name)
	u.ntWithoutRowid = false
	u.ntStrict = false
	u.ntCols = []*designerColRow{
		newDesignerColRow("id", 0, true, true, ""),
		newDesignerColRow("name", 1, false, true, ""),
	}
	u.activeTab = tabStructure
	u.win.Invalidate()
}

func (u *uiState) buildDesignerTableSpec() edit.TableSpec {
	spec := edit.TableSpec{
		Name:         strings.TrimSpace(u.ntTableName.Text()),
		WithoutRowid: u.ntWithoutRowid,
		Strict:       u.ntStrict,
		Columns:      make([]edit.ColumnDef, 0, len(u.ntCols)),
	}
	pkOrd := 0
	for _, r := range u.ntCols {
		if r.pk {
			pkOrd++
		}
		dflt := strings.TrimSpace(r.dfltEd.Text())
		spec.Columns = append(spec.Columns, edit.ColumnDef{
			Name:       strings.TrimSpace(r.nameEd.Text()),
			Type:       designerColTypes[r.typeIdx%len(designerColTypes)],
			NotNull:    r.notNull,
			Default:    dflt,
			HasDefault: dflt != "",
			PK:         boolInt(r.pk, pkOrd),
		})
	}
	return spec
}

func boolInt(cond bool, val int) int {
	if !cond {
		return 0
	}
	return val
}

func (u *uiState) createTableFromDesigner(spec edit.TableSpec) {
	u.mu.Lock()
	db := u.db
	u.mu.Unlock()
	if db == nil || db.ReadOnly {
		return
	}
	ddl, err := edit.CreateTableSQL(spec)
	if err != nil {
		u.setStatus(fmt.Sprintf("Invalid table design: %v", err))
		return
	}
	go func() {
		var newSchema *engine.Schema
		err := db.Fg.Do(context.Background(), func(c *sqlite3.Conn) error {
			if err := c.Exec(ddl); err != nil {
				return err
			}
			var lerr error
			newSchema, lerr = engine.LoadSchema(c)
			return lerr
		})
		if err != nil {
			u.setStatus(fmt.Sprintf("Create table failed: %v", err))
			return
		}
		u.postUI(func() {
			u.mu.Lock()
			if u.db == db {
				u.schema = newSchema
			}
			u.newTableOpen = false
			u.mu.Unlock()
			u.tree.SetSchema(newSchema)
			if obj := newSchema.Find(spec.Name); obj != nil {
				u.tree.SelectObject(obj)
				u.selectObject(obj)
			}
			u.setStatus(fmt.Sprintf("Created table %q (%d column(s))", spec.Name, len(spec.Columns)))
		})
		u.refreshToolsInfo()
	}()
}

func (u *uiState) openAddColumnBar() {
	u.mu.Lock()
	db := u.db
	obj := u.structObj
	u.mu.Unlock()
	if db == nil || db.ReadOnly || obj == nil || obj.Type != engine.TypeTable {
		return
	}
	u.addColOpen = true
	u.newTableOpen = false
	u.addColName.SetText("new_col")
	u.addColDflt.SetText("")
	u.addColTypeIdx = 1 // TEXT
	u.addColNotNull = false
	u.win.Invalidate()
}

func (u *uiState) applyAddColumn(obj *engine.Object, col edit.ColumnDef, rel []*engine.Object) {
	u.mu.Lock()
	db := u.db
	u.mu.Unlock()
	if db == nil || db.ReadOnly || obj == nil {
		return
	}
	spec := edit.TableSpecFromObject(obj)
	spec.Columns = append(spec.Columns, col)
	plan, err := edit.PlanTableAlter(obj, spec, nil, rel)
	if err != nil {
		u.setStatus(fmt.Sprintf("Plan add column failed: %v", err))
		return
	}
	go func() {
		var newSchema *engine.Schema
		err := db.Fg.Do(context.Background(), func(c *sqlite3.Conn) error {
			if err := edit.ExecuteAlterPlan(context.Background(), c, plan); err != nil {
				return err
			}
			var lerr error
			newSchema, lerr = engine.LoadSchema(c)
			return lerr
		})
		if err != nil {
			u.setStatus(fmt.Sprintf("Add column failed: %v", err))
			return
		}
		u.postUI(func() {
			u.mu.Lock()
			if u.db == db {
				u.schema = newSchema
			}
			u.addColOpen = false
			curSrc := u.tableSrc
			activeTbl := u.activeTable
			u.mu.Unlock()
			u.tree.SetSchema(newSchema)
			if updated := newSchema.Find(obj.Name); updated != nil {
				if activeTbl == updated.Name && curSrc != nil {
					u.selectObject(updated)
				}
				u.loadStructure(updated)
				u.activeTab = tabStructure
			}
			u.setStatus(fmt.Sprintf("Added column %q to %q (%s)", col.Name, obj.Name, plan.Summary))
		})
		u.refreshToolsInfo()
	}()
}

func (u *uiState) emptyTableObject(obj *engine.Object) {
	u.mu.Lock()
	db := u.db
	u.confirmEmptyTarget = ""
	u.mu.Unlock()
	if db == nil || db.ReadOnly || obj == nil || obj.Type != engine.TypeTable {
		return
	}
	stmtSQL, err := edit.EmptyTableSQL(obj.Name)
	if err != nil {
		u.setStatus(err.Error())
		return
	}
	go func() {
		var deleted int64
		err := db.Fg.Do(context.Background(), func(c *sqlite3.Conn) error {
			if err := c.Exec("BEGIN IMMEDIATE"); err != nil {
				return err
			}
			if err := c.Exec(stmtSQL); err != nil {
				_ = c.Exec("ROLLBACK")
				return err
			}
			deleted = c.Changes()
			return c.Exec("COMMIT")
		})
		if err != nil {
			u.setStatus(fmt.Sprintf("Empty table failed: %v", err))
			return
		}
		u.postUI(func() {
			u.mu.Lock()
			src := u.tableSrc
			active := u.activeTable
			u.mu.Unlock()
			if src != nil && active == obj.Name {
				src.Changes().Clear()
				src.Reload()
			}
			u.setStatus(fmt.Sprintf("Emptied %q (%d row(s) deleted)", obj.Name, deleted))
		})
		u.refreshToolsInfo()
	}()
}

func (u *uiState) dropSchemaObject(obj *engine.Object) {
	u.mu.Lock()
	db := u.db
	u.confirmDropName = ""
	u.confirmDropType = ""
	u.mu.Unlock()
	if db == nil || db.ReadOnly || obj == nil {
		return
	}
	stmtSQL, err := edit.DropObjectSQL(obj)
	if err != nil {
		u.setStatus(err.Error())
		return
	}
	droppedType := obj.Type
	droppedName := obj.Name
	go func() {
		var newSchema *engine.Schema
		err := db.Fg.Do(context.Background(), func(c *sqlite3.Conn) error {
			if err := c.Exec(stmtSQL); err != nil {
				return err
			}
			var lerr error
			newSchema, lerr = engine.LoadSchema(c)
			return lerr
		})
		if err != nil {
			u.setStatus(fmt.Sprintf("Drop %s failed: %v", droppedType, err))
			return
		}
		u.postUI(func() {
			u.mu.Lock()
			if u.db == db {
				u.schema = newSchema
			}
			var closeSrc *data.TableSource
			if u.tableSrc != nil && u.activeTable == droppedName {
				closeSrc = u.tableSrc
				u.tableSrc = nil
				u.activeTable = ""
			}
			if u.structObj == obj {
				u.structObj = nil
				u.structIndex = nil
				u.structRel = nil
			}
			u.mu.Unlock()

			if closeSrc != nil {
				closeSrc.Close()
				u.browseGrid.SetSource(nil)
				u.inspector.Clear()
			}
			u.tree.SetSchema(newSchema)
			if tables := newSchema.ByType(engine.TypeTable); len(tables) > 0 {
				u.tree.SelectObject(tables[0])
				u.loadStructure(tables[0])
			} else if len(newSchema.Objects) > 0 {
				u.tree.SelectObject(newSchema.Objects[0])
				u.loadStructure(newSchema.Objects[0])
			} else {
				u.structGrid.SetSource(nil)
				u.ddlEdit.SetText("")
			}
			u.setStatus(fmt.Sprintf("Dropped %s %q", droppedType, droppedName))
		})
		u.refreshToolsInfo()
	}()
}

func (u *uiState) layoutStructureView(gtx layout.Context) layout.Dimensions {
	theme.Fill(gtx, u.th.Surface)
	if u.newTableOpen {
		return u.layoutNewTableDesigner(gtx)
	}

	u.mu.Lock()
	db := u.db
	obj := u.structObj
	idxInfo := u.structIndex
	rel := u.structRel
	schema := u.schema
	u.mu.Unlock()

	if db == nil {
		return layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return u.th.Label(gtx, u.th.UISize, u.th.TextSubtle, "Open a database to inspect or design tables, views, indexes, and triggers.")
		})
	}
	readOnly := db.ReadOnly

	if obj == nil {
		if clicked(&u.btnNewTable, gtx) && !readOnly {
			u.openNewTableDesigner()
		}
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				barH := gtx.Dp(40)
				w := gtx.Constraints.Max.X
				theme.FillRect(gtx, clip.Rect{Max: image.Pt(w, barH)}, u.th.SurfaceAlt)
				if !readOnly {
					u.drawPillButton(gtx, gtx.Dp(12), barH, "+ New Table", false, true, &u.btnNewTable)
				}
				theme.FillRect(gtx, clip.Rect{Min: image.Pt(0, barH-1), Max: image.Pt(w, barH)}, u.th.Border)
				return layout.Dimensions{Size: image.Pt(w, barH)}
			}),
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				return layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return u.th.Label(gtx, u.th.UISize, u.th.TextSubtle, "Select any schema object from the sidebar, or click + New Table to create one.")
				})
			}),
		)
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
	if clicked(&u.btnNewTable, gtx) && !readOnly {
		u.openNewTableDesigner()
	}
	if clicked(&u.btnAddColToggle, gtx) && !readOnly && obj.Type == engine.TypeTable {
		if u.addColOpen {
			u.addColOpen = false
		} else {
			u.openAddColumnBar()
		}
	}
	if clicked(&u.btnEmptyTable, gtx) && !readOnly && obj.Type == engine.TypeTable {
		if u.confirmEmptyTarget == obj.Name {
			u.emptyTableObject(obj)
		} else {
			u.confirmEmptyTarget = obj.Name
			u.confirmDropName = ""
		}
	}
	if clicked(&u.btnDropObject, gtx) && !readOnly {
		if u.confirmDropName == obj.Name && u.confirmDropType == obj.Type {
			u.dropSchemaObject(obj)
		} else {
			u.confirmDropName = obj.Name
			u.confirmDropType = obj.Type
			u.confirmEmptyTarget = ""
		}
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
			x += u.drawPillButton(gtx, x, barH, "Copy DDL", false, false, &u.btnCopyDDL) + gtx.Dp(6)
			if !readOnly {
				x += u.drawPillButton(gtx, x, barH, "+ New Table", false, false, &u.btnNewTable) + gtx.Dp(6)
				if obj.Type == engine.TypeTable {
					x += u.drawPillButton(gtx, x, barH, "+ Add Col", u.addColOpen, false, &u.btnAddColToggle) + gtx.Dp(6)
					emptyLbl := "Empty Table"
					if u.confirmEmptyTarget == obj.Name {
						emptyLbl = "Confirm Empty?"
					}
					x += u.drawDangerPillButton(gtx, x, barH, emptyLbl, u.confirmEmptyTarget == obj.Name, &u.btnEmptyTable) + gtx.Dp(6)
				}
				dropLbl := "Drop"
				confirmDrop := u.confirmDropName == obj.Name && u.confirmDropType == obj.Type
				if confirmDrop {
					dropLbl = "Confirm Drop?"
				}
				x += u.drawDangerPillButton(gtx, x, barH, dropLbl, confirmDrop, &u.btnDropObject) + gtx.Dp(10)
			}

			if x < w-gtx.Dp(40) {
				off := op.Offset(image.Pt(x, gtx.Dp(11))).Push(gtx.Ops)
				cg := gtx
				cg.Constraints.Max.X = max(0, w-x-gtx.Dp(12))
				u.th.LabelFont(cg, font.Font{Typeface: u.th.UI, Weight: font.SemiBold}, u.th.Small, u.th.Text, text.Start, title)
				off.Pop()
			}

			theme.FillRect(gtx, clip.Rect{Min: image.Pt(0, barH-1), Max: image.Pt(w, barH)}, u.th.Border)
			return layout.Dimensions{Size: image.Pt(w, barH)}
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			if !u.addColOpen || readOnly || obj.Type != engine.TypeTable {
				return layout.Dimensions{}
			}
			return u.layoutAddColumnBar(gtx, obj, rel)
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

func (u *uiState) layoutAddColumnBar(gtx layout.Context, obj *engine.Object, rel []*engine.Object) layout.Dimensions {
	for {
		if _, ok := u.addColName.Update(gtx); !ok {
			break
		}
	}
	for {
		if _, ok := u.addColDflt.Update(gtx); !ok {
			break
		}
	}
	if clicked(&u.btnAddColType, gtx) {
		u.addColTypeIdx = (u.addColTypeIdx + 1) % len(designerColTypes)
	}
	if clicked(&u.btnAddColNN, gtx) {
		u.addColNotNull = !u.addColNotNull
	}
	if clicked(&u.btnAddColCanc, gtx) {
		u.addColOpen = false
	}

	dflt := strings.TrimSpace(u.addColDflt.Text())
	newCol := edit.ColumnDef{
		Name:       strings.TrimSpace(u.addColName.Text()),
		Type:       designerColTypes[u.addColTypeIdx%len(designerColTypes)],
		NotNull:    u.addColNotNull,
		Default:    dflt,
		HasDefault: dflt != "",
	}
	spec := edit.TableSpecFromObject(obj)
	spec.Columns = append(spec.Columns, newCol)
	plan, planErr := edit.PlanTableAlter(obj, spec, nil, rel)

	if clicked(&u.btnAddColSave, gtx) && planErr == nil {
		u.applyAddColumn(obj, newCol, rel)
	}

	barH := gtx.Dp(42)
	w := gtx.Constraints.Max.X
	theme.FillRect(gtx, clip.Rect{Max: image.Pt(w, barH)}, u.th.Chrome)
	theme.FillRect(gtx, clip.Rect{Min: image.Pt(0, barH-1), Max: image.Pt(w, barH)}, u.th.Border)

	x := gtx.Dp(12)
	nameW := min(gtx.Dp(150), max(gtx.Dp(90), w/6))
	u.drawInputBox(gtx, x, gtx.Dp(7), nameW, &u.addColName, "Column name…")
	x += nameW + gtx.Dp(6)

	typeLbl := designerColTypes[u.addColTypeIdx%len(designerColTypes)]
	x += u.drawPillButton(gtx, x, barH, typeLbl, false, false, &u.btnAddColType) + gtx.Dp(6)
	x += u.drawPillButton(gtx, x, barH, "NOT NULL", u.addColNotNull, false, &u.btnAddColNN) + gtx.Dp(6)

	dfltW := min(gtx.Dp(130), max(gtx.Dp(80), w/7))
	u.drawInputBox(gtx, x, gtx.Dp(7), dfltW, &u.addColDflt, "Default (optional)…")
	x += dfltW + gtx.Dp(8)

	x += u.drawPillButton(gtx, x, barH, "Apply Column", false, planErr == nil, &u.btnAddColSave) + gtx.Dp(6)
	x += u.drawPillButton(gtx, x, barH, "Cancel", false, false, &u.btnAddColCanc) + gtx.Dp(10)

	hint := ""
	hintCol := u.th.TextMuted
	if planErr != nil {
		hint = planErr.Error()
		hintCol = u.th.Danger
	} else {
		hint = plan.Summary
	}
	if x < w-gtx.Dp(40) && hint != "" {
		off := op.Offset(image.Pt(x, gtx.Dp(12))).Push(gtx.Ops)
		cg := gtx
		cg.Constraints.Max.X = max(0, w-x-gtx.Dp(12))
		u.th.LabelFont(cg, font.Font{Typeface: u.th.Mono}, u.th.Small, hintCol, text.Start, hint)
		off.Pop()
	}
	return layout.Dimensions{Size: image.Pt(w, barH)}
}

func (u *uiState) layoutNewTableDesigner(gtx layout.Context) layout.Dimensions {
	for {
		if _, ok := u.ntTableName.Update(gtx); !ok {
			break
		}
	}
	if clicked(&u.btnNTWithoutRowid, gtx) {
		u.ntWithoutRowid = !u.ntWithoutRowid
	}
	if clicked(&u.btnNTStrict, gtx) {
		u.ntStrict = !u.ntStrict
	}
	if clicked(&u.btnNTAddField, gtx) {
		u.ntCols = append(u.ntCols, newDesignerColRow(fmt.Sprintf("col_%d", len(u.ntCols)+1), 1, false, false, ""))
	}
	if clicked(&u.btnNTCancel, gtx) {
		u.newTableOpen = false
	}

	removeIdx := -1
	for i, r := range u.ntCols {
		for {
			if _, ok := r.nameEd.Update(gtx); !ok {
				break
			}
		}
		for {
			if _, ok := r.dfltEd.Update(gtx); !ok {
				break
			}
		}
		if clicked(&r.typeClick, gtx) {
			r.typeIdx = (r.typeIdx + 1) % len(designerColTypes)
		}
		if clicked(&r.pkClick, gtx) {
			r.pk = !r.pk
		}
		if clicked(&r.nnClick, gtx) {
			r.notNull = !r.notNull
		}
		if len(u.ntCols) > 1 && clicked(&r.delClick, gtx) {
			removeIdx = i
		}
	}
	if removeIdx >= 0 {
		u.ntCols = append(u.ntCols[:removeIdx], u.ntCols[removeIdx+1:]...)
	}

	spec := u.buildDesignerTableSpec()
	previewSQL, previewErr := edit.CreateTableSQL(spec)
	if clicked(&u.btnNTCreate, gtx) {
		u.createTableFromDesigner(spec)
	}

	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			barH := gtx.Dp(44)
			w := gtx.Constraints.Max.X
			theme.FillRect(gtx, clip.Rect{Max: image.Pt(w, barH)}, u.th.SurfaceAlt)

			x := gtx.Dp(12)
			off := op.Offset(image.Pt(x, gtx.Dp(13))).Push(gtx.Ops)
			u.th.LabelFont(gtx, font.Font{Typeface: u.th.UI, Weight: font.SemiBold}, u.th.Small, u.th.Text, text.Start, "New Table:")
			off.Pop()
			x += gtx.Dp(80)

			tblW := min(gtx.Dp(200), max(gtx.Dp(110), w/5))
			u.drawInputBox(gtx, x, gtx.Dp(8), tblW, &u.ntTableName, "table_name")
			x += tblW + gtx.Dp(8)

			x += u.drawPillButton(gtx, x, barH, "WITHOUT ROWID", u.ntWithoutRowid, false, &u.btnNTWithoutRowid) + gtx.Dp(6)
			x += u.drawPillButton(gtx, x, barH, "STRICT", u.ntStrict, false, &u.btnNTStrict) + gtx.Dp(10)
			x += u.drawPillButton(gtx, x, barH, "+ Add Field", false, false, &u.btnNTAddField) + gtx.Dp(6)
			x += u.drawPillButton(gtx, x, barH, "Create Table", false, previewErr == nil, &u.btnNTCreate) + gtx.Dp(6)
			u.drawPillButton(gtx, x, barH, "Cancel", false, false, &u.btnNTCancel)

			theme.FillRect(gtx, clip.Rect{Min: image.Pt(0, barH-1), Max: image.Pt(w, barH)}, u.th.Border)
			return layout.Dimensions{Size: image.Pt(w, barH)}
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			hdrH := gtx.Dp(28)
			w := gtx.Constraints.Max.X
			theme.FillRect(gtx, clip.Rect{Max: image.Pt(w, hdrH)}, u.th.Chrome)
			o := op.Offset(image.Pt(gtx.Dp(14), gtx.Dp(6))).Push(gtx.Ops)
			u.th.LabelFont(gtx, font.Font{Typeface: u.th.UI, Weight: font.SemiBold}, u.th.Small, u.th.TextMuted, text.Start, "Columns")
			o.Pop()
			theme.FillRect(gtx, clip.Rect{Min: image.Pt(0, hdrH-1), Max: image.Pt(w, hdrH)}, u.th.BorderSubtle)
			return layout.Dimensions{Size: image.Pt(w, hdrH)}
		}),
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			return u.ntColList.Layout(gtx, len(u.ntCols), func(gtx layout.Context, i int) layout.Dimensions {
				r := u.ntCols[i]
				rowH := gtx.Dp(40)
				w := gtx.Constraints.Max.X
				if i%2 == 1 {
					theme.FillRect(gtx, clip.Rect{Max: image.Pt(w, rowH)}, u.th.SurfaceAlt)
				}
				x := gtx.Dp(12)
				nameW := min(gtx.Dp(200), max(gtx.Dp(100), w/4))
				u.drawInputBox(gtx, x, gtx.Dp(6), nameW, &r.nameEd, "column_name")
				x += nameW + gtx.Dp(8)

				tLbl := designerColTypes[r.typeIdx%len(designerColTypes)]
				x += u.drawPillButton(gtx, x, rowH, tLbl, false, false, &r.typeClick) + gtx.Dp(6)
				x += u.drawPillButton(gtx, x, rowH, "PK", r.pk, false, &r.pkClick) + gtx.Dp(6)
				x += u.drawPillButton(gtx, x, rowH, "NOT NULL", r.notNull, false, &r.nnClick) + gtx.Dp(8)

				dfltW := min(gtx.Dp(180), max(gtx.Dp(90), w/5))
				u.drawInputBox(gtx, x, gtx.Dp(6), dfltW, &r.dfltEd, "Default expr…")
				x += dfltW + gtx.Dp(8)

				if len(u.ntCols) > 1 {
					u.drawDangerPillButton(gtx, x, rowH, "Remove", false, &r.delClick)
				}
				theme.FillRect(gtx, clip.Rect{Min: image.Pt(0, rowH-1), Max: image.Pt(w, rowH)}, u.th.BorderSubtle)
				return layout.Dimensions{Size: image.Pt(w, rowH)}
			})
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			boxH := gtx.Dp(160)
			w := gtx.Constraints.Max.X
			theme.FillRect(gtx, clip.Rect{Max: image.Pt(w, boxH)}, u.th.Chrome)
			theme.FillRect(gtx, clip.Rect{Max: image.Pt(w, 1)}, u.th.Border)

			hdr := "DDL Preview:"
			body := previewSQL
			bodyCol := u.th.Text
			if previewErr != nil {
				hdr = "Validation Error:"
				body = previewErr.Error()
				bodyCol = u.th.Danger
			}
			o1 := op.Offset(image.Pt(gtx.Dp(14), gtx.Dp(8))).Push(gtx.Ops)
			u.th.LabelFont(gtx, font.Font{Typeface: u.th.UI, Weight: font.SemiBold}, u.th.Small, u.th.TextMuted, text.Start, hdr)
			o1.Pop()

			o2 := op.Offset(image.Pt(gtx.Dp(14), gtx.Dp(28))).Push(gtx.Ops)
			cg := gtx
			cg.Constraints.Max.X = max(0, w-gtx.Dp(28))
			u.th.LabelFont(cg, font.Font{Typeface: u.th.Mono}, u.th.Small, bodyCol, text.Start, body)
			o2.Pop()
			return layout.Dimensions{Size: image.Pt(w, boxH)}
		}),
	)
}

func (u *uiState) drawDangerPillButton(gtx layout.Context, x, barH int, label string, confirming bool, clk *gesture.Click) int {
	bw := gtx.Dp(unit.Dp(len(label)*6 + 22))
	bh := gtx.Dp(26)
	y := (barH - bh) / 2
	r := image.Rect(x, y, x+bw, y+bh)
	rr := clip.RRect{
		Rect: r,
		NE:   gtx.Dp(5), NW: gtx.Dp(5), SE: gtx.Dp(5), SW: gtx.Dp(5),
	}
	bg := u.th.Surface
	fg := u.th.Danger
	if confirming {
		bg = u.th.Danger
		fg = u.th.AccentText
	} else if clk.Hovered() {
		bg = u.th.Hover
	}
	theme.FillRRect(gtx, rr, bg)
	if !confirming {
		b := clip.Stroke{Path: rr.Path(gtx.Ops), Width: float32(max(1, gtx.Dp(1)))}.Op()
		paint.FillShape(gtx.Ops, u.th.Border, b)
	}
	o := op.Offset(image.Pt(x, y+gtx.Dp(4))).Push(gtx.Ops)
	cg := gtx
	cg.Constraints.Min.X = bw
	cg.Constraints.Max.X = bw
	u.th.LabelFont(cg, font.Font{Typeface: u.th.UI, Weight: font.Medium}, u.th.Small, fg, text.Middle, label)
	o.Pop()

	hit := clip.Rect(r).Push(gtx.Ops)
	pointer.CursorPointer.Add(gtx.Ops)
	clk.Add(gtx.Ops)
	hit.Pop()
	return bw
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
