package widgets

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"slices"
	"strconv"
	"strings"
	"time"

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

// InspectorPageBytes is the incremental byte window used when paging large cells.
const InspectorPageBytes int64 = 64 * 1024

type inspectorMode uint8

const (
	modeText inspectorMode = iota
	modeJSON
	modeHex
	modeImage
)

// Inspector displays and edits the currently selected cell in Text, JSON, Hex,
// or Image mode, with incremental BLOB paging and file import/export hooks.
type Inspector struct {
	mode      inspectorMode
	txtClick  gesture.Click
	jsonClick gesture.Click
	hexClick  gesture.Click
	imgClick  gesture.Click

	prevPageClick gesture.Click
	nextPageClick gesture.Click
	applyClick    gesture.Click
	nullClick     gesture.Click
	delClick      gesture.Click
	exportClick   gesture.Click
	importClick   gesture.Click

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
	offset     int64
	totalBytes int64
	trunc      bool
	userEdited bool
	lines      []string

	hasImg    bool
	imgOp     paint.ImageOp
	imgSize   image.Point
	imgFormat string
	imgErr    string

	// Callbacks for staging edits, paging, and cell I/O from the inspector.
	OnStageCell  func(row int64, col int, orig edit.Value, hasOrig bool, val edit.Value)
	OnDeleteRow  func(row int64)
	OnLoadOffset func(row int64, col int, offset int64)
	OnLoadImage  func(row int64, col int, wantBytes int)
	OnExportCell func(row int64, col int)
	OnImportFile func(row int64, col int)
}

// NewInspector creates a cell inspector panel.
func NewInspector() *Inspector {
	return &Inspector{
		list: layout.List{Axis: layout.Vertical},
	}
}

// HasFocus reports whether the inspector editor currently holds keyboard focus.
func (ins *Inspector) HasFocus(gtx layout.Context) bool {
	return ins.hasCell && gtx.Focused(&ins.editor)
}

// ModeName returns the active inspector mode ("text", "json", "hex", or "image").
func (ins *Inspector) ModeName() string {
	switch ins.mode {
	case modeJSON:
		return "json"
	case modeHex:
		return "hex"
	case modeImage:
		return "image"
	default:
		return "text"
	}
}

// Offset returns the byte offset of the currently displayed cell window.
func (ins *Inspector) Offset() int64 {
	return ins.offset
}

// Lines returns the formatted lines currently prepared for Text/JSON/Hex display.
func (ins *Inspector) Lines() []string {
	return ins.lines
}

// RawBytes returns a copy of the currently loaded raw cell window bytes.
func (ins *Inspector) RawBytes() []byte {
	return slices.Clone(ins.raw)
}

// ColumnName returns the active inspected column name.
func (ins *Inspector) ColumnName() string {
	return ins.colName
}

// Kind returns the storage class of the active inspected cell.
func (ins *Inspector) Kind() data.Kind {
	return ins.kind
}

// HasDecodedImage reports whether the current cell decoded as a raster image.
func (ins *Inspector) HasDecodedImage() (image.Point, string, bool) {
	return ins.imgSize, ins.imgFormat, ins.hasImg
}

// Clear resets the inspector when no cell is selected.
func (ins *Inspector) Clear() {
	ins.hasCell = false
	ins.userEdited = false
	ins.offset = 0
	ins.totalBytes = 0
	ins.trunc = false
	ins.raw = nil
	ins.lines = nil
	ins.clearImage()
}

func (ins *Inspector) clearImage() {
	ins.hasImg = false
	ins.imgOp = paint.ImageOp{}
	ins.imgSize = image.Point{}
	ins.imgFormat = ""
	ins.imgErr = ""
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
	ins.offset = 0
	ins.totalBytes = int64(len(val))
	ins.trunc = trunc
	ins.userEdited = false
	ins.clearImage()
	if kind == data.KindNull {
		ins.editor.SetText("")
	} else {
		ins.editor.SetText(val)
	}
	ins.rebuildLines()
}

// SetDetail updates the inspected cell once the full (or window) value
// has been fetched asynchronously.
func (ins *Inspector) SetDetail(det data.CellDetail, col data.Column, editable bool) {
	if !ins.hasCell || ins.row != det.Row || ins.colIdx != det.Col {
		return
	}
	ins.editable = editable
	ins.colName = col.Name
	ins.colType = col.Type
	ins.kind = det.Kind
	ins.raw = slices.Clone(det.Data)
	ins.offset = det.Offset
	ins.totalBytes = det.TotalBytes
	ins.trunc = det.Truncated

	imgFmt := DetectImageFormat(ins.raw)
	if det.Kind == data.KindBlob && det.Offset == 0 && imgFmt != "" {
		if !det.Truncated {
			ins.refreshImagePreview()
			if ins.hasImg && (ins.mode == modeText || ins.mode == modeHex) {
				ins.mode = modeImage
			}
		} else if det.TotalBytes <= 8<<20 && ins.OnLoadImage != nil {
			// Image fits well within memory budget: upgrade the 64 KB probe fetch to full bytes.
			ins.OnLoadImage(det.Row, det.Col, int(det.TotalBytes))
		}
	} else if det.Kind == data.KindBlob && ins.mode == modeText {
		ins.mode = modeHex
	} else if ins.mode == modeImage {
		ins.refreshImagePreview()
	}

	if !ins.userEdited && det.Kind != data.KindNull && det.Kind != data.KindBlob && det.Offset == 0 {
		ins.editor.SetText(string(det.Data))
	}
	ins.rebuildLines()
}

// DetectImageFormat identifies PNG, JPEG, or GIF headers in raw bytes.
func DetectImageFormat(raw []byte) string {
	switch {
	case len(raw) >= 8 && bytes.Equal(raw[:8], []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}):
		return "png"
	case len(raw) >= 3 && raw[0] == 0xFF && raw[1] == 0xD8 && raw[2] == 0xFF:
		return "jpeg"
	case len(raw) >= 6 && (bytes.Equal(raw[:6], []byte("GIF87a")) || bytes.Equal(raw[:6], []byte("GIF89a"))):
		return "gif"
	default:
		return ""
	}
}

// FormatTimestampHint interprets likely Unix timestamps (10-digit seconds or
// 13-digit milliseconds between 2001 and 2100 UTC) as human-readable UTC time.
func FormatTimestampHint(raw string, kind data.Kind) string {
	if kind != data.KindInteger {
		return ""
	}
	n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return ""
	}
	switch {
	case n >= 1_000_000_000 && n <= 4_102_444_800:
		return "Unix s: " + time.Unix(n, 0).UTC().Format("2006-01-02 15:04:05 UTC")
	case n >= 1_000_000_000_000 && n <= 4_102_444_800_000:
		return "Unix ms: " + time.UnixMilli(n).UTC().Format("2006-01-02 15:04:05.000 UTC")
	default:
		return ""
	}
}

func (ins *Inspector) refreshImagePreview() {
	ins.clearImage()
	if ins.kind != data.KindBlob || len(ins.raw) == 0 {
		ins.imgErr = "Cell does not contain BLOB bytes."
		return
	}
	if ins.offset > 0 || int64(len(ins.raw)) < ins.totalBytes {
		ins.imgErr = fmt.Sprintf("Image truncated (%s of %s loaded).", data.FormatSize(int64(len(ins.raw))), data.FormatSize(ins.totalBytes))
		return
	}
	img, format, err := image.Decode(bytes.NewReader(ins.raw))
	if err != nil {
		ins.imgErr = "Not a valid PNG/JPEG/GIF image."
		return
	}
	op := paint.NewImageOp(img)
	op.Filter = paint.FilterLinear
	ins.imgOp = op
	ins.imgSize = img.Bounds().Size()
	ins.imgFormat = format
	ins.hasImg = true
}

func (ins *Inspector) rebuildLines() {
	ins.lines = ins.lines[:0]
	if !ins.hasCell {
		return
	}
	if ins.kind == data.KindNull && ins.mode != modeHex && ins.mode != modeImage {
		ins.lines = append(ins.lines, "NULL")
		return
	}
	switch ins.mode {
	case modeHex:
		raw := ins.raw
		const stride = 16
		addrFmt := "%06x  "
		if ins.offset+int64(len(raw)) > 0xffffff {
			addrFmt = "%08x  "
		}
		for off := 0; off < len(raw); off += stride {
			end := min(len(raw), off+stride)
			chunk := raw[off:end]
			var b strings.Builder
			fmt.Fprintf(&b, addrFmt, ins.offset+int64(off))
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
			norm := strings.NewReplacer("\r\n", "\n", "\t", "    ").Replace(string(ins.raw))
			ins.lines = append(ins.lines, strings.Split(norm, "\n")...)
		}
	case modeImage:
		ins.refreshImagePreview()
	default:
		norm := strings.NewReplacer("\r\n", "\n", "\t", "    ").Replace(string(ins.raw))
		ins.lines = strings.Split(norm, "\n")
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
	if clicked(&ins.imgClick, gtx) && ins.mode != modeImage {
		ins.mode = modeImage
		if ins.hasCell && ins.kind == data.KindBlob && (ins.offset > 0 || int64(len(ins.raw)) < ins.totalBytes) && ins.totalBytes <= 8<<20 && ins.OnLoadImage != nil {
			ins.OnLoadImage(ins.row, ins.colIdx, int(ins.totalBytes))
		}
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

	btnW := gtx.Dp(36)
	btnH := gtx.Dp(22)
	gap := gtx.Dp(3)
	y := (h - btnH) / 2
	xImg := w - gtx.Dp(8) - btnW
	xHex := xImg - gap - btnW
	xJSON := xHex - gap - btnW
	xTxt := xJSON - gap - btnW

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
	drawTab(xImg, "Img", ins.mode == modeImage, &ins.imgClick)

	theme.FillRect(gtx, clip.Rect{Min: image.Pt(0, h-1), Max: image.Pt(w, h)}, th.Border)
	return layout.Dimensions{Size: image.Pt(w, h)}
}

func (ins *Inspector) layoutBody(gtx layout.Context, th *theme.Theme) layout.Dimensions {
	w := gtx.Constraints.Max.X

	kindStr := map[data.Kind]string{
		data.KindNull:    "NULL",
		data.KindInteger: "INTEGER",
		data.KindReal:    "REAL",
		data.KindText:    "TEXT",
		data.KindBlob:    "BLOB",
	}[ins.kind]

	for {
		ev, ok := ins.editor.Update(gtx)
		if !ok {
			break
		}
		if _, isChange := ev.(widget.ChangeEvent); isChange {
			ins.userEdited = true
		}
	}
	origVal := ins.currentOrigValue()
	canStage := !ins.trunc && ins.offset == 0 && ins.kind != data.KindBlob
	if clicked(&ins.applyClick, gtx) && canStage && ins.OnStageCell != nil {
		ins.OnStageCell(ins.row, ins.colIdx, origVal, true, edit.TextValue(ins.editor.Text()))
	}
	if clicked(&ins.nullClick, gtx) && !ins.trunc && ins.offset == 0 && ins.OnStageCell != nil {
		ins.OnStageCell(ins.row, ins.colIdx, origVal, true, edit.NullValue())
	}
	if clicked(&ins.delClick, gtx) && ins.OnDeleteRow != nil {
		ins.OnDeleteRow(ins.row)
	}
	if clicked(&ins.exportClick, gtx) && ins.kind != data.KindNull && ins.OnExportCell != nil {
		ins.OnExportCell(ins.row, ins.colIdx)
	}
	if clicked(&ins.importClick, gtx) && ins.editable && ins.OnImportFile != nil {
		ins.OnImportFile(ins.row, ins.colIdx)
	}

	windowEnd := ins.offset + int64(len(ins.raw))
	canPrev := ins.offset > 0 && ins.OnLoadOffset != nil
	canNext := windowEnd < ins.totalBytes && ins.OnLoadOffset != nil
	hasPaging := ins.OnLoadOffset != nil && (ins.offset > 0 || (ins.trunc && windowEnd < ins.totalBytes))
	if clicked(&ins.prevPageClick, gtx) && canPrev {
		prev := max(int64(0), ins.offset-InspectorPageBytes)
		ins.OnLoadOffset(ins.row, ins.colIdx, prev)
	}
	if clicked(&ins.nextPageClick, gtx) && canNext {
		ins.OnLoadOffset(ins.row, ins.colIdx, windowEnd)
	}

	tsHint := FormatTimestampHint(string(ins.raw), ins.kind)
	metaH := gtx.Dp(48)
	if tsHint != "" {
		metaH = gtx.Dp(66)
	}
	showFileBar := (ins.kind != data.KindNull && ins.OnExportCell != nil) || (ins.editable && ins.OnImportFile != nil)

	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			off1 := op.Offset(image.Pt(gtx.Dp(12), gtx.Dp(6))).Push(gtx.Ops)
			title := fmt.Sprintf("Row %d · %s", ins.row+1, ins.colName)
			th.LabelFont(gtx, font.Font{Typeface: th.UI, Weight: font.SemiBold}, th.Small, th.Text, text.Start, title)
			off1.Pop()

			off2 := op.Offset(image.Pt(gtx.Dp(12), gtx.Dp(24))).Push(gtx.Ops)
			meta := fmt.Sprintf("%s · %s · %s", kindStr, ins.colType, data.FormatSize(ins.totalBytes))
			if ins.offset > 0 || windowEnd < ins.totalBytes {
				meta += fmt.Sprintf(" [%s..%s]", data.FormatSize(ins.offset), data.FormatSize(windowEnd))
			} else if ins.trunc {
				meta += " (truncated preview)"
			}
			cg := gtx
			cg.Constraints.Max.X = max(0, w-gtx.Dp(24))
			th.LabelFont(cg, font.Font{Typeface: th.Mono}, th.Small, th.TextSubtle, text.Start, meta)
			off2.Pop()

			if tsHint != "" {
				off3 := op.Offset(image.Pt(gtx.Dp(12), gtx.Dp(42))).Push(gtx.Ops)
				cg := gtx
				cg.Constraints.Max.X = max(0, w-gtx.Dp(24))
				th.LabelFont(cg, font.Font{Typeface: th.Mono}, th.Small, th.Accent, text.Start, tsHint)
				off3.Pop()
			}

			theme.FillRect(gtx, clip.Rect{Min: image.Pt(0, metaH-1), Max: image.Pt(w, metaH)}, th.BorderSubtle)
			return layout.Dimensions{Size: image.Pt(w, metaH)}
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			if !hasPaging {
				return layout.Dimensions{}
			}
			barH := gtx.Dp(34)
			theme.FillRect(gtx, clip.Rect{Max: image.Pt(w, barH)}, th.Chrome)
			theme.FillRect(gtx, clip.Rect{Min: image.Pt(0, barH-1), Max: image.Pt(w, barH)}, th.BorderSubtle)

			x := gtx.Dp(10)
			if canPrev {
				x += drawSmallButton(gtx, th, x, barH, "< Prev 64 KB", false, &ins.prevPageClick) + gtx.Dp(6)
			}
			if canNext {
				x += drawSmallButton(gtx, th, x, barH, "Next 64 KB >", false, &ins.nextPageClick) + gtx.Dp(8)
			}
			return layout.Dimensions{Size: image.Pt(w, barH)}
		}),
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			theme.Fill(gtx, th.Surface)
			if ins.mode == modeImage {
				if ins.hasImg {
					return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
						layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
							return layout.Inset{Top: 10, Bottom: 6, Left: 10, Right: 10}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
								return widget.Image{
									Src:      ins.imgOp,
									Fit:      widget.ScaleDown,
									Position: layout.Center,
								}.Layout(gtx)
							})
						}),
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							return layout.Inset{Bottom: 8, Left: 12, Right: 12}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
								info := fmt.Sprintf("%dx%d px · %s · %s",
									ins.imgSize.X, ins.imgSize.Y, strings.ToUpper(ins.imgFormat), data.FormatSize(ins.totalBytes))
								return th.LabelFont(gtx, font.Font{Typeface: th.Mono}, th.Small, th.TextMuted, text.Middle, info)
							})
						}),
					)
				}
				return layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					msg := ins.imgErr
					if msg == "" {
						msg = "Not a decodable image (PNG/JPEG/GIF)."
					}
					return layout.Inset{Left: 12, Right: 12}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						return th.Label(gtx, th.Small, th.TextSubtle, msg)
					})
				})
			}
			if ins.mode == modeText && ins.editable && ins.kind != data.KindBlob && !ins.trunc && ins.offset == 0 {
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
			if canStage {
				x += drawSmallButton(gtx, th, x, barH, "Stage Edit", true, &ins.applyClick) + gtx.Dp(6)
			}
			if !ins.trunc && ins.offset == 0 {
				x += drawSmallButton(gtx, th, x, barH, "Set NULL", false, &ins.nullClick) + gtx.Dp(6)
			}
			drawSmallButton(gtx, th, x, barH, "Delete Row", false, &ins.delClick)
			return layout.Dimensions{Size: image.Pt(w, barH)}
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			if !showFileBar {
				return layout.Dimensions{}
			}
			barH := gtx.Dp(36)
			theme.FillRect(gtx, clip.Rect{Max: image.Pt(w, barH)}, th.Chrome)
			theme.FillRect(gtx, clip.Rect{Max: image.Pt(w, 1)}, th.BorderSubtle)

			x := gtx.Dp(10)
			if ins.kind != data.KindNull && ins.OnExportCell != nil {
				x += drawSmallButton(gtx, th, x, barH, "Export Cell…", false, &ins.exportClick) + gtx.Dp(6)
			}
			if ins.editable && ins.OnImportFile != nil {
				drawSmallButton(gtx, th, x, barH, "Import File…", false, &ins.importClick)
			}
			return layout.Dimensions{Size: image.Pt(w, barH)}
		}),
	)
}

func (ins *Inspector) currentOrigValue() edit.Value {
	switch ins.kind {
	case data.KindNull:
		return edit.NullValue()
	case data.KindInteger:
		return edit.ExactValue(edit.KindInt, string(ins.raw), nil)
	case data.KindReal:
		return edit.ExactValue(edit.KindReal, string(ins.raw), nil)
	case data.KindBlob:
		return edit.ExactValue(edit.KindBlob, "", slices.Clone(ins.raw))
	default:
		return edit.ExactValue(edit.KindText, string(ins.raw), nil)
	}
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
	for {
		ev, ok := c.Update(gtx.Source)
		if !ok {
			return false
		}
		if ev.Kind == gesture.KindClick {
			return true
		}
	}
}
