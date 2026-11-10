package widgets

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"strings"

	"gioui.org/font"
	"gioui.org/gesture"
	"gioui.org/io/pointer"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/text"
	"gioui.org/unit"
	"gioui.org/widget"

	"github.com/sqlstrata/sqlstrata/internal/data"
	"github.com/sqlstrata/sqlstrata/internal/edit"
	"github.com/sqlstrata/sqlstrata/internal/ui/theme"
)

type inspectorMode uint8

const (
	modeText inspectorMode = iota
	modeJSON
	modeHex
)

// Inspector displays and edits the currently selected cell in Text, JSON or Hex mode.
type Inspector struct {
	mode      inspectorMode
	txtClick  gesture.Click
	jsonClick gesture.Click
	hexClick  gesture.Click

	applyClick gesture.Click
	nullClick  gesture.Click
	delClick   gesture.Click

	editor widget.Editor
	list   layout.List

	hasCell    bool
	editable   bool
	row        int64
	colIdx     int
	colName    string
	colType    string
	kind       data.Kind
	raw        []byte
	totalBytes int64
	trunc      bool
	lines      []string

	// Callbacks for staging edits from the inspector.
	OnStageCell func(row int64, col int, val edit.Value)
	OnDeleteRow func(row int64)
}

// NewInspector creates a cell inspector panel.
func NewInspector() *Inspector {
	return &Inspector{
		list: layout.List{Axis: layout.Vertical},
	}
}

// Clear resets the inspector when no cell is selected.
func (ins *Inspector) Clear() {
	ins.hasCell = false
	ins.lines = nil
}

// SetCell updates the inspected cell with its immediate preview value.
func (ins *Inspector) SetCell(row int64, colIdx int, col data.Column, val string, kind data.Kind, trunc bool, editable bool) {
	ins.hasCell = true
	ins.editable = editable
	ins.row = row
	ins.colIdx = colIdx
	ins.colName = col.Name
	ins.colType = col.Type
	ins.kind = kind
	ins.raw = []byte(val)
	ins.totalBytes = int64(len(val))
	ins.trunc = trunc
	if kind == data.KindNull {
		ins.editor.SetText("")
	} else {
		ins.editor.SetText(val)
	}
	ins.rebuildLines()
}

// SetDetail updates the inspected cell once the full (or 64 KB window) value
// has been fetched asynchronously.
func (ins *Inspector) SetDetail(det data.CellDetail, col data.Column, editable bool) {
	if !ins.hasCell || ins.row != det.Row || ins.colIdx != det.Col {
		return
	}
	ins.editable = editable
	ins.colName = col.Name
	ins.colType = col.Type
	ins.kind = det.Kind
	ins.raw = det.Data
	ins.totalBytes = det.TotalBytes
	ins.trunc = det.Truncated
	if det.Kind == data.KindBlob && ins.mode == modeText {
		ins.mode = modeHex
	}
	if det.Kind != data.KindNull && det.Kind != data.KindBlob {
		ins.editor.SetText(string(det.Data))
	}
	ins.rebuildLines()
}

func (ins *Inspector) rebuildLines() {
	ins.lines = ins.lines[:0]
	if !ins.hasCell {
		return
	}
	if ins.kind == data.KindNull && ins.mode != modeHex {
		ins.lines = append(ins.lines, "NULL")
		return
	}
	switch ins.mode {
	case modeHex:
		raw := ins.raw
		const stride = 16
		for off := 0; off < len(raw); off += stride {
			end := min(len(raw), off+stride)
			chunk := raw[off:end]
			var b strings.Builder
			fmt.Fprintf(&b, "%06x  ", off)
			for i := 0; i < stride; i++ {
				if i < len(chunk) {
					fmt.Fprintf(&b, "%02x ", chunk[i])
				} else {
					b.WriteString("   ")
				}
				if i == 7 {
					b.WriteByte(' ')
				}
			}
			b.WriteString(" |")
			for _, c := range chunk {
				if c >= 0x20 && c < 0x7f {
					b.WriteByte(c)
				} else {
					b.WriteByte('.')
				}
			}
			b.WriteByte('|')
			ins.lines = append(ins.lines, b.String())
		}
		if len(ins.lines) == 0 {
			ins.lines = append(ins.lines, strings.TrimSpace(hex.Dump(nil)))
		}
	case modeJSON:
		var out bytes.Buffer
		if json.Valid(ins.raw) && json.Indent(&out, ins.raw, "", "  ") == nil {
			ins.lines = strings.Split(out.String(), "\n")
		} else {
			ins.lines = append(ins.lines, "(Not valid JSON)", "")
			ins.lines = append(ins.lines, strings.Split(strings.ReplaceAll(string(ins.raw), "\r\n", "\n"), "\n")...)
		}
	default:
		ins.lines = strings.Split(strings.ReplaceAll(string(ins.raw), "\r\n", "\n"), "\n")
	}
}

// Layout renders the cell inspector panel.
func (ins *Inspector) Layout(gtx layout.Context, th *theme.Theme) layout.Dimensions {
	theme.Fill(gtx, th.Chrome)

	if clicked(&ins.txtClick, gtx) && ins.mode != modeText {
		ins.mode = modeText
		ins.rebuildLines()
	}
	if clicked(&ins.jsonClick, gtx) && ins.mode != modeJSON {
		ins.mode = modeJSON
		ins.rebuildLines()
	}
	if clicked(&ins.hexClick, gtx) && ins.mode != modeHex {
		ins.mode = modeHex
		ins.rebuildLines()
	}

	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return ins.layoutHeader(gtx, th)
		}),
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			if !ins.hasCell {
				return layout.Inset{Top: 16, Left: 12, Right: 12}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return th.Label(gtx, th.Small, th.TextSubtle, "Click any cell to inspect or edit its value.")
				})
			}
			return ins.layoutBody(gtx, th)
		}),
	)
}

func (ins *Inspector) layoutHeader(gtx layout.Context, th *theme.Theme) layout.Dimensions {
	h := gtx.Dp(36)
	w := gtx.Constraints.Max.X

	off := op.Offset(image.Pt(gtx.Dp(12), gtx.Dp(9))).Push(gtx.Ops)
	th.LabelFont(gtx, font.Font{Typeface: th.UI, Weight: font.SemiBold}, th.UISize, th.Text, text.Start, "Inspector")
	off.Pop()

	btnW := gtx.Dp(42)
	btnH := gtx.Dp(22)
	y := (h - btnH) / 2
	xHex := w - gtx.Dp(10) - btnW
	xJSON := xHex - gtx.Dp(4) - btnW
	xTxt := xJSON - gtx.Dp(4) - btnW

	drawTab := func(x int, label string, active bool, clk *gesture.Click) {
		r := image.Rect(x, y, x+btnW, y+btnH)
		rr := clip.RRect{
			Rect: r,
			NE:   gtx.Dp(4), NW: gtx.Dp(4), SE: gtx.Dp(4), SW: gtx.Dp(4),
		}
		bg := th.Surface
		fg := th.TextMuted
		if active {
			bg = th.Selected
			fg = th.SelectedText
		} else if clk.Hovered() {
			bg = th.Hover
		}
		theme.FillRRect(gtx, rr, bg)
		off := op.Offset(image.Pt(x, y+gtx.Dp(3))).Push(gtx.Ops)
		cg := gtx
		cg.Constraints.Min.X = btnW
		cg.Constraints.Max.X = btnW
		th.LabelFont(cg, font.Font{Typeface: th.UI}, th.Small, fg, text.Middle, label)
		off.Pop()

		hit := clip.Rect(r).Push(gtx.Ops)
		pointer.CursorPointer.Add(gtx.Ops)
		clk.Add(gtx.Ops)
		hit.Pop()
	}
	drawTab(xTxt, "Text", ins.mode == modeText, &ins.txtClick)
	drawTab(xJSON, "JSON", ins.mode == modeJSON, &ins.jsonClick)
	drawTab(xHex, "Hex", ins.mode == modeHex, &ins.hexClick)

	theme.FillRect(gtx, clip.Rect{Min: image.Pt(0, h-1), Max: image.Pt(w, h)}, th.Border)
	return layout.Dimensions{Size: image.Pt(w, h)}
}

func (ins *Inspector) layoutBody(gtx layout.Context, th *theme.Theme) layout.Dimensions {
	metaH := gtx.Dp(48)
	w := gtx.Constraints.Max.X

	kindStr := map[data.Kind]string{
		data.KindNull:    "NULL",
		data.KindInteger: "INTEGER",
		data.KindReal:    "REAL",
		data.KindText:    "TEXT",
		data.KindBlob:    "BLOB",
	}[ins.kind]

	for {
		if _, ok := ins.editor.Update(gtx); !ok {
			break
		}
	}
	if clicked(&ins.applyClick, gtx) && ins.OnStageCell != nil {
		ins.OnStageCell(ins.row, ins.colIdx, edit.TextValue(ins.editor.Text()))
	}
	if clicked(&ins.nullClick, gtx) && ins.OnStageCell != nil {
		ins.OnStageCell(ins.row, ins.colIdx, edit.NullValue())
	}
	if clicked(&ins.delClick, gtx) && ins.OnDeleteRow != nil {
		ins.OnDeleteRow(ins.row)
	}

	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			off1 := op.Offset(image.Pt(gtx.Dp(12), gtx.Dp(6))).Push(gtx.Ops)
			title := fmt.Sprintf("Row %d · %s", ins.row+1, ins.colName)
			th.LabelFont(gtx, font.Font{Typeface: th.UI, Weight: font.SemiBold}, th.Small, th.Text, text.Start, title)
			off1.Pop()

			off2 := op.Offset(image.Pt(gtx.Dp(12), gtx.Dp(24))).Push(gtx.Ops)
			meta := fmt.Sprintf("%s · %s · %s", kindStr, ins.colType, data.FormatSize(ins.totalBytes))
			if ins.trunc {
				meta += " (first 64 KB)"
			}
			th.LabelFont(gtx, font.Font{Typeface: th.Mono}, th.Small, th.TextSubtle, text.Start, meta)
			off2.Pop()

			theme.FillRect(gtx, clip.Rect{Min: image.Pt(0, metaH-1), Max: image.Pt(w, metaH)}, th.BorderSubtle)
			return layout.Dimensions{Size: image.Pt(w, metaH)}
		}),
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			theme.Fill(gtx, th.Surface)
			if ins.mode == modeText && ins.editable && ins.kind != data.KindBlob {
				return layout.Inset{Top: 8, Bottom: 8, Left: 12, Right: 12}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					if ins.editor.Len() == 0 && ins.kind == data.KindNull && !gtx.Focused(&ins.editor) {
						th.LabelFont(gtx, font.Font{Typeface: th.Mono, Style: font.Italic}, th.Code, th.NullText, text.Start, "NULL (type to edit…)")
					}
					m := op.Record(gtx.Ops)
					paint.ColorOp{Color: th.Text}.Add(gtx.Ops)
					textMat := m.Stop()
					ms := op.Record(gtx.Ops)
					paint.ColorOp{Color: th.Selected}.Add(gtx.Ops)
					selMat := ms.Stop()
					return ins.editor.Layout(gtx, th.Shaper, font.Font{Typeface: th.Mono}, th.Code, textMat, selMat)
				})
			}
			lineH := gtx.Dp(unit.Dp(18))
			return ins.list.Layout(gtx, len(ins.lines), func(gtx layout.Context, i int) layout.Dimensions {
				off := op.Offset(image.Pt(gtx.Dp(12), gtx.Dp(2))).Push(gtx.Ops)
				cg := gtx
				cg.Constraints.Max.X = max(0, w-gtx.Dp(24))
				col := th.Text
				if ins.kind == data.KindNull && ins.mode != modeHex {
					col = th.NullText
				}
				th.LabelFont(cg, font.Font{Typeface: th.Mono}, th.Small, col, text.Start, ins.lines[i])
				off.Pop()
				return layout.Dimensions{Size: image.Pt(w, lineH)}
			})
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			if !ins.editable {
				return layout.Dimensions{}
			}
			barH := gtx.Dp(38)
			theme.FillRect(gtx, clip.Rect{Max: image.Pt(w, barH)}, th.Chrome)
			theme.FillRect(gtx, clip.Rect{Max: image.Pt(w, 1)}, th.Border)

			x := gtx.Dp(10)
			if ins.kind != data.KindBlob {
				x += drawSmallButton(gtx, th, x, barH, "Stage Edit", true, &ins.applyClick) + gtx.Dp(6)
				x += drawSmallButton(gtx, th, x, barH, "Set NULL", false, &ins.nullClick) + gtx.Dp(6)
			}
			drawSmallButton(gtx, th, x, barH, "Delete Row", false, &ins.delClick)
			return layout.Dimensions{Size: image.Pt(w, barH)}
		}),
	)
}

func drawSmallButton(gtx layout.Context, th *theme.Theme, x, barH int, label string, primary bool, clk *gesture.Click) int {
	btnW := gtx.Dp(unit.Dp(len(label)*6 + 20))
	btnH := gtx.Dp(24)
	y := (barH - btnH) / 2
	r := image.Rect(x, y, x+btnW, y+btnH)
	rr := clip.RRect{
		Rect: r,
		NE:   gtx.Dp(4), NW: gtx.Dp(4), SE: gtx.Dp(4), SW: gtx.Dp(4),
	}
	bg := th.Surface
	fg := th.Text
	if primary {
		bg = th.Accent
		fg = th.AccentText
	} else if clk.Hovered() {
		bg = th.Hover
	}
	theme.FillRRect(gtx, rr, bg)
	if !primary {
		border := clip.Stroke{Path: rr.Path(gtx.Ops), Width: float32(max(1, gtx.Dp(1)))}.Op()
		paint.FillShape(gtx.Ops, th.Border, border)
	}
	off := op.Offset(image.Pt(x, y+gtx.Dp(3))).Push(gtx.Ops)
	cg := gtx
	cg.Constraints.Min.X = btnW
	cg.Constraints.Max.X = btnW
	th.LabelFont(cg, font.Font{Typeface: th.UI}, th.Small, fg, text.Middle, label)
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
