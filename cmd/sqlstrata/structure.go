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
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/text"
	"github.com/ncruces/go-sqlite3"

	"github.com/sqlstrata/sqlstrata/internal/data"
	"github.com/sqlstrata/sqlstrata/internal/engine"
	"github.com/sqlstrata/sqlstrata/internal/sqltok"
	"github.com/sqlstrata/sqlstrata/internal/ui/theme"
)

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

