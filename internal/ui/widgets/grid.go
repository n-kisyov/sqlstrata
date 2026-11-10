package widgets

import (
	"image"
	"image/color"
	"math"
	"strconv"
	"strings"

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

	"github.com/sqlstrata/sqlstrata/internal/data"
	"github.com/sqlstrata/sqlstrata/internal/edit"
	"github.com/sqlstrata/sqlstrata/internal/ui/theme"
)

// GridSource supplies virtualized table data to a DataGrid.
type GridSource interface {
	Columns() []data.Column
	RowCount() int64
	SetVisible(first, last int64)
	Cell(row int64, col int) (text string, kind data.Kind, trunc bool, ok bool)
}

// SortableGridSource is an optional interface implemented by GridSources that
// support sorting by column.
type SortableGridSource interface {
	Sort() (col int, desc bool)
	SetSort(col int, desc bool)
}

// EditableGridSource is an optional interface implemented by GridSources that
// support staged edits.
type EditableGridSource interface {
	Editable() bool
	CellStatus(row int64, col int) (edited, rowDeleted, rowInserted bool)
	StageCellEdit(row int64, col int, newVal edit.Value) bool
}

// DataGrid is a 2D-virtualized table view capable of scrolling hundreds of
// millions of rows and hundreds of columns at 60+ fps with flat memory usage.
type DataGrid struct {
	src        GridSource
	colWidths  []unit.Dp
	colResizes []gesture.Drag
	colClicks  []gesture.Click
	colOrigins []unit.Dp

	// Scroll state: topRow is the index of the first visible row, subY is the
	// pixel offset within topRow [0, rowH). Keeping topRow in int64 avoids
	// pixel-coordinate overflow on 100M+ row tables.
	topRow  int64
	subY    int
	scrollX int

	vScroll   gesture.Scroll
	hScroll   gesture.Scroll
	vBarDrag  gesture.Drag
	bodyClick gesture.Click
	keyTag    struct{}

	selRow int64
	selCol int

	editing    bool
	editRow    int64
	editCol    int
	cellEditor widget.Editor

	// OnSelectCell is called when the active cell changes.
	OnSelectCell func(row int64, col int)
}

// NewDataGrid creates a ready-to-use DataGrid.
func NewDataGrid() *DataGrid {
	g := &DataGrid{
		selRow: -1,
		selCol: -1,
	}
	g.cellEditor.SingleLine = true
	g.cellEditor.Submit = true
	return g
}

// SetSource attaches a new data source and resets the scroll/selection state.
func (g *DataGrid) SetSource(src GridSource) {
	g.src = src
	g.topRow = 0
	g.subY = 0
	g.scrollX = 0
	g.selRow = -1
	g.selCol = -1
	g.editing = false
	if src == nil {
		g.colWidths = nil
		g.colResizes = nil
		g.colClicks = nil
		g.colOrigins = nil
		return
	}
	cols := src.Columns()
	g.colWidths = make([]unit.Dp, len(cols))
	g.colResizes = make([]gesture.Drag, len(cols))
	g.colClicks = make([]gesture.Click, len(cols))
	g.colOrigins = make([]unit.Dp, len(cols))
	for i, c := range cols {
		w := unit.Dp(max(130, min(280, len(c.Name)*10+64)))
		up := strings.ToUpper(c.Type)
		if c.PK && strings.Contains(up, "INT") {
			w = 96
		} else if strings.Contains(up, "TEXT") || strings.Contains(c.Name, "at") || strings.Contains(c.Name, "email") || strings.Contains(c.Name, "name") {
			w = max(w, 175)
		}
		g.colWidths[i] = w
	}
}

// Source returns the current GridSource.
func (g *DataGrid) Source() GridSource { return g.src }

// SelectedCell returns the selected (row, col), or (-1, -1) if none.
func (g *DataGrid) SelectedCell() (int64, int) { return g.selRow, g.selCol }

// ScrollToRow scrolls so that row is visible.
func (g *DataGrid) ScrollToRow(row int64, visRows int64) {
	if g.src == nil {
		return
	}
	total := g.src.RowCount()
	if total <= 0 {
		return
	}
	row = max(0, min(total-1, row))
	if row < g.topRow {
		g.topRow = row
		g.subY = 0
	} else if visRows > 1 && row >= g.topRow+visRows-1 {
		g.topRow = max(0, row-visRows+2)
		g.subY = 0
	}
}

// Layout renders the virtualized grid.
func (g *DataGrid) Layout(gtx layout.Context, th *theme.Theme) layout.Dimensions {
	size := gtx.Constraints.Max
	theme.Fill(gtx, th.Surface)

	if g.src == nil {
		return layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return th.Label(gtx, th.UISize, th.TextSubtle, "Select a table or view from the left sidebar, or open a database (Ctrl+O).")
		})
	}

	cols := g.src.Columns()
	if len(g.colWidths) != len(cols) {
		g.SetSource(g.src)
	}
	totalRows := g.src.RowCount()

	headerH := gtx.Dp(30)
	rowH := max(18, gtx.Dp(24))
	vBarW := gtx.Dp(12)
	gutterW := g.gutterWidth(gtx, totalRows)

	bodyW := max(0, size.X-gutterW-vBarW)
	bodyH := max(0, size.Y-headerH)
	visRows := int64(bodyH/rowH) + 2

	// 1. Handle column resize drags and header sort clicks.
	for i := range cols {
		for {
			ev, ok := g.colResizes[i].Update(gtx.Metric, gtx.Source, gesture.Horizontal)
			if !ok {
				break
			}
			switch ev.Kind {
			case pointer.Press:
				g.colOrigins[i] = g.colWidths[i] - unit.Dp(ev.Position.X)/unit.Dp(gtx.Metric.PxPerDp)
			case pointer.Drag:
				w := g.colOrigins[i] + unit.Dp(ev.Position.X)/unit.Dp(gtx.Metric.PxPerDp)
				g.colWidths[i] = max(56, min(800, w))
			}
		}
		for {
			ev, ok := g.colClicks[i].Update(gtx.Source)
			if !ok {
				break
			}
			if ev.Kind == gesture.KindClick {
				if srt, ok := g.src.(SortableGridSource); ok {
					curCol, curDesc := srt.Sort()
					switch {
					case curCol != i:
						srt.SetSort(i, false)
					case !curDesc:
						srt.SetSort(i, true)
					default:
						srt.SetSort(-1, false)
					}
				}
			}
		}
	}

	totalColsW := 0
	for _, w := range g.colWidths {
		totalColsW += gtx.Dp(w)
	}
	maxScrollX := max(0, totalColsW-bodyW)

	// 2. Handle mouse wheel scrolling.
	if dy := g.vScroll.Update(gtx.Metric, gtx.Source, gtx.Now, gesture.Vertical,
		pointer.ScrollRange{Min: -1e6, Max: 1e6},
		pointer.ScrollRange{Min: -1e6, Max: 1e6},
	); dy != 0 {
		g.scrollByPixels(dy, rowH, bodyH, totalRows)
	}
	if dx := g.hScroll.Update(gtx.Metric, gtx.Source, gtx.Now, gesture.Horizontal,
		pointer.ScrollRange{Min: -maxScrollX, Max: maxScrollX},
		pointer.ScrollRange{Min: -1e6, Max: 1e6},
	); dx != 0 {
		g.scrollX = max(0, min(maxScrollX, g.scrollX+dx))
	}

	// 3. Handle vertical scrollbar thumb/track dragging.
	maxTopRow := max(int64(0), totalRows-max(1, int64(bodyH/rowH)))
	for {
		ev, ok := g.vBarDrag.Update(gtx.Metric, gtx.Source, gesture.Vertical)
		if !ok {
			break
		}
		if (ev.Kind == pointer.Press || ev.Kind == pointer.Drag) && bodyH > 0 && maxTopRow > 0 {
			thumbH := g.thumbHeight(bodyH, rowH, totalRows)
			track := float64(max(1, bodyH-thumbH))
			ratio := (float64(ev.Position.Y) - float64(thumbH)/2) / track
			ratio = math.Max(0, math.Min(1, ratio))
			g.topRow = int64(math.Round(ratio * float64(maxTopRow)))
			g.subY = 0
		}
	}

	g.clampScroll(rowH, bodyH, totalRows)
	g.scrollX = max(0, min(maxScrollX, g.scrollX))

	// 4. Handle keyboard navigation and inline editor events.
	if g.editing {
		for {
			ev, ok := g.cellEditor.Update(gtx)
			if !ok {
				break
			}
			if _, ok := ev.(widget.SubmitEvent); ok {
				g.commitInlineEdit(gtx)
			}
		}
		for {
			ev, ok := gtx.Event(key.Filter{Name: key.NameEscape})
			if !ok {
				break
			}
			if ke, ok := ev.(key.Event); ok && ke.State == key.Press {
				g.cancelInlineEdit(gtx)
			}
		}
	}
	g.handleKeys(gtx, totalRows, len(cols), int64(max(1, bodyH/rowH)))

	// 5. Handle clicks inside the grid body.
	for {
		ev, ok := g.bodyClick.Update(gtx.Source)
		if !ok {
			break
		}
		if ev.Kind == gesture.KindClick {
			y := int(ev.Position.Y) - headerH
			x := int(ev.Position.X) - gutterW
			if y >= 0 && x >= 0 && x < bodyW {
				clickedRow := g.topRow + int64((y+g.subY)/rowH)
				if clickedRow >= 0 && clickedRow < totalRows {
					clickedCol := g.colAtX(gtx, x+g.scrollX)
					if clickedCol >= 0 {
						if g.editing && (clickedRow != g.editRow || clickedCol != g.editCol) {
							g.commitInlineEdit(gtx)
						}
						g.selectCell(clickedRow, clickedCol)
						if ev.NumClicks >= 2 {
							g.startInlineEdit(gtx, clickedRow, clickedCol)
						} else if !g.editing {
							gtx.Execute(key.FocusCmd{Tag: &g.keyTag})
						}
					}
				}
			}
		}
	}

	// Inform the data source which rows are visible so it can page & prefetch.
	lastVis := min(max(0, totalRows-1), g.topRow+visRows)
	g.src.SetVisible(g.topRow, lastVis)

	// Determine visible column span [firstCol, lastCol] to virtualize columns.
	firstCol, firstColX, lastCol := g.visibleCols(gtx, bodyW)

	// 6. Draw visible rows in the body area.
	bodyRect := image.Rect(gutterW, headerH, gutterW+bodyW, size.Y)
	bodyClip := clip.Rect(bodyRect).Push(gtx.Ops)
	for r := g.topRow; r <= lastVis && r < totalRows; r++ {
		y := headerH + int(r-g.topRow)*rowH - g.subY
		if y >= size.Y {
			break
		}
		if y+rowH <= headerH {
			continue
		}
		// Subtle zebra striping + selected row tint.
		if r == g.selRow {
			theme.FillRect(gtx, clip.Rect{
				Min: image.Pt(gutterW, y),
				Max: image.Pt(gutterW+bodyW, y+rowH),
			}, withAlpha(th.Selected, 0x1a))
		} else if r&1 == 1 {
			theme.FillRect(gtx, clip.Rect{
				Min: image.Pt(gutterW, y),
				Max: image.Pt(gutterW+bodyW, y+rowH),
			}, th.SurfaceAlt)
		}

		x := gutterW + firstColX - g.scrollX
		for c := firstCol; c <= lastCol && c < len(cols); c++ {
			cw := gtx.Dp(g.colWidths[c])
			g.drawCell(gtx, th, r, c, image.Rect(x, y, x+cw, y+rowH))
			x += cw
		}

		// Horizontal row separator.
		theme.FillRect(gtx, clip.Rect{
			Min: image.Pt(gutterW, y+rowH-1),
			Max: image.Pt(gutterW+bodyW, y+rowH),
		}, th.BorderSubtle)
	}
	bodyClip.Pop()

	// 7. Draw frozen left row-number gutter.
	gutterClip := clip.Rect(image.Rect(0, headerH, gutterW, size.Y)).Push(gtx.Ops)
	theme.FillRect(gtx, clip.Rect{Min: image.Pt(0, headerH), Max: image.Pt(gutterW, size.Y)}, th.Chrome)
	edSrc, _ := g.src.(EditableGridSource)
	for r := g.topRow; r <= lastVis && r < totalRows; r++ {
		y := headerH + int(r-g.topRow)*rowH - g.subY
		if y >= size.Y {
			break
		}
		var rowDel, rowIns bool
		if edSrc != nil {
			_, rowDel, rowIns = edSrc.CellStatus(r, 0)
		}
		if rowDel {
			theme.FillRect(gtx, clip.Rect{Min: image.Pt(0, y), Max: image.Pt(gutterW, y+rowH)}, withAlpha(th.Danger, 0x35))
		} else if r == g.selRow {
			theme.FillRect(gtx, clip.Rect{Min: image.Pt(0, y), Max: image.Pt(gutterW, y+rowH)}, th.Selected)
		}
		off := op.Offset(image.Pt(0, y+gtx.Dp(4))).Push(gtx.Ops)
		cg := gtx
		cg.Constraints.Min.X = gutterW - gtx.Dp(8)
		cg.Constraints.Max.X = gutterW - gtx.Dp(8)
		numCol := th.TextSubtle
		if rowDel {
			numCol = th.Danger
		} else if rowIns {
			numCol = th.Accent
		} else if r == g.selRow {
			numCol = th.SelectedText
		}
		lbl := strconv.FormatInt(r+1, 10)
		if rowIns {
			lbl = "+" + lbl
		}
		th.LabelFont(cg, font.Font{Typeface: th.Mono}, th.Small, numCol, text.End, lbl)
		off.Pop()

		theme.FillRect(gtx, clip.Rect{
			Min: image.Pt(0, y+rowH-1),
			Max: image.Pt(gutterW, y+rowH),
		}, th.BorderSubtle)
	}
	// Right border of gutter.
	theme.FillRect(gtx, clip.Rect{Min: image.Pt(gutterW-1, 0), Max: image.Pt(gutterW, size.Y)}, th.Border)
	gutterClip.Pop()

	// 8. Draw frozen column header.
	theme.FillRect(gtx, clip.Rect{Max: image.Pt(size.X, headerH)}, th.Chrome)
	// Top-left corner cell ("#").
	offCorner := op.Offset(image.Pt(0, gtx.Dp(7))).Push(gtx.Ops)
	cgCorner := gtx
	cgCorner.Constraints.Min.X = gutterW - gtx.Dp(8)
	cgCorner.Constraints.Max.X = gutterW - gtx.Dp(8)
	th.LabelFont(cgCorner, font.Font{Typeface: th.Mono}, th.Small, th.TextSubtle, text.End, "#")
	offCorner.Pop()
	theme.FillRect(gtx, clip.Rect{Min: image.Pt(gutterW-1, 0), Max: image.Pt(gutterW, headerH)}, th.Border)

	hdrClip := clip.Rect(image.Rect(gutterW, 0, gutterW+bodyW, headerH)).Push(gtx.Ops)
	hx := gutterW + firstColX - g.scrollX
	for c := firstCol; c <= lastCol && c < len(cols); c++ {
		cw := gtx.Dp(g.colWidths[c])
		g.drawHeaderCell(gtx, th, c, cols[c], image.Rect(hx, 0, hx+cw, headerH))
		hx += cw
	}
	hdrClip.Pop()
	theme.FillRect(gtx, clip.Rect{Min: image.Pt(0, headerH-1), Max: image.Pt(size.X, headerH)}, th.Border)

	// 9. Draw vertical scrollbar.
	g.drawVScrollbar(gtx, th, image.Rect(size.X-vBarW, headerH, size.X, size.Y), rowH, totalRows)

	// 10. Register input handlers over the whole grid.
	whole := clip.Rect{Max: size}.Push(gtx.Ops)
	event.Op(gtx.Ops, &g.keyTag)
	g.vScroll.Add(gtx.Ops)
	g.hScroll.Add(gtx.Ops)
	g.bodyClick.Add(gtx.Ops)
	whole.Pop()

	return layout.Dimensions{Size: size}
}

func (g *DataGrid) drawHeaderCell(gtx layout.Context, th *theme.Theme, idx int, col data.Column, r image.Rectangle) {
	cellClip := clip.Rect(r).Push(gtx.Ops)
	if idx == g.selCol || g.colClicks[idx].Hovered() {
		theme.FillRect(gtx, clip.Rect(r), th.Hover)
	}
	padX := gtx.Dp(8)
	y := gtx.Dp(6)

	label := col.Name
	if col.PK {
		label = "PK · " + label
	}
	if srt, ok := g.src.(SortableGridSource); ok {
		if sc, desc := srt.Sort(); sc == idx {
			if desc {
				label += " ▼"
			} else {
				label += " ▲"
			}
		}
	}
	off := op.Offset(image.Pt(r.Min.X+padX, y)).Push(gtx.Ops)
	cg := gtx
	cg.Constraints.Max.X = max(0, r.Dx()-2*padX)
	th.LabelFont(cg, font.Font{Typeface: th.UI, Weight: font.SemiBold}, th.UISize, th.Text, text.Start, label)
	off.Pop()

	// Column right divider.
	theme.FillRect(gtx, clip.Rect{
		Min: image.Pt(r.Max.X-1, r.Min.Y+4),
		Max: image.Pt(r.Max.X, r.Max.Y-4),
	}, th.Border)
	cellClip.Pop()

	// Sort click region (excludes the rightmost 6dp resize zone).
	sortHit := clip.Rect{
		Min: r.Min,
		Max: image.Pt(max(r.Min.X, r.Max.X-gtx.Dp(6)), r.Max.Y),
	}.Push(gtx.Ops)
	pointer.CursorPointer.Add(gtx.Ops)
	g.colClicks[idx].Add(gtx.Ops)
	sortHit.Pop()

	// Column resize handle on the right edge.
	hit := clip.Rect{
		Min: image.Pt(r.Max.X-gtx.Dp(4), r.Min.Y),
		Max: image.Pt(r.Max.X+gtx.Dp(4), r.Max.Y),
	}.Push(gtx.Ops)
	pointer.CursorColResize.Add(gtx.Ops)
	g.colResizes[idx].Add(gtx.Ops)
	hit.Pop()
}

func (g *DataGrid) drawCell(gtx layout.Context, th *theme.Theme, row int64, col int, r image.Rectangle) {
	var edited, rowDel bool
	if edSrc, ok := g.src.(EditableGridSource); ok {
		edited, rowDel, _ = edSrc.CellStatus(row, col)
	}
	isSel := row == g.selRow && col == g.selCol
	if rowDel {
		theme.FillRect(gtx, clip.Rect(r), withAlpha(th.Danger, 0x1e))
	} else if isSel {
		theme.FillRect(gtx, clip.Rect(r), th.Selected)
	} else if edited {
		theme.FillRect(gtx, clip.Rect(r), withAlpha(th.Accent, 0x22))
	}

	txt, kind, _, ok := g.src.Cell(row, col)
	padX := gtx.Dp(8)
	y := r.Min.Y + gtx.Dp(4)
	w := max(0, r.Dx()-2*padX)

	cellClip := clip.Rect(r).Push(gtx.Ops)
	if g.editing && row == g.editRow && col == g.editCol {
		theme.FillRect(gtx, clip.Rect(r), th.Surface)
		off := op.Offset(image.Pt(r.Min.X+padX, y)).Push(gtx.Ops)
		cg := gtx
		cg.Constraints = layout.Exact(image.Pt(w, r.Dy()-gtx.Dp(6)))
		m := op.Record(gtx.Ops)
		paint.ColorOp{Color: th.Text}.Add(gtx.Ops)
		textMat := m.Stop()
		ms := op.Record(gtx.Ops)
		paint.ColorOp{Color: th.Selected}.Add(gtx.Ops)
		selMat := ms.Stop()
		g.cellEditor.Layout(cg, th.Shaper, font.Font{Typeface: th.Mono}, th.Code, textMat, selMat)
		off.Pop()
	} else if !ok {
		// Subtle loading skeleton bar.
		barW := min(w, gtx.Dp(48))
		if barW > 0 {
			theme.FillRect(gtx, clip.Rect{
				Min: image.Pt(r.Min.X+padX, r.Min.Y+r.Dy()/2-2),
				Max: image.Pt(r.Min.X+padX+barW, r.Min.Y+r.Dy()/2+2),
			}, th.BorderSubtle)
		}
	} else {
		fnt := font.Font{Typeface: th.Mono}
		align := text.Start
		fg := th.Text
		switch kind {
		case data.KindNull:
			txt = "NULL"
			fnt.Style = font.Italic
			fg = th.NullText
		case data.KindInteger, data.KindReal:
			align = text.End
			fg = th.NumberText
		case data.KindBlob:
			fg = th.BlobText
		case data.KindText:
			if strings.ContainsAny(txt, "\r\n\t") {
				txt = strings.NewReplacer("\r\n", " ↵ ", "\n", " ↵ ", "\r", " ↵ ", "\t", " → ").Replace(txt)
			}
		}
		if rowDel {
			fg = th.Danger
		} else if isSel {
			fg = th.SelectedText
		}

		off := op.Offset(image.Pt(r.Min.X+padX, y)).Push(gtx.Ops)
		cg := gtx
		cg.Constraints.Min.X = w
		cg.Constraints.Max.X = w
		th.LabelFont(cg, fnt, th.Code, fg, align, txt)
		off.Pop()
	}

	if edited && !rowDel {
		// Top-right accent marker indicating a staged edit on this cell.
		theme.FillRect(gtx, clip.Rect{
			Min: image.Pt(r.Max.X-gtx.Dp(5), r.Min.Y),
			Max: image.Pt(r.Max.X, r.Min.Y+gtx.Dp(5)),
		}, th.Accent)
	}

	// Vertical cell separator.
	theme.FillRect(gtx, clip.Rect{
		Min: image.Pt(r.Max.X-1, r.Min.Y),
		Max: image.Pt(r.Max.X, r.Max.Y),
	}, th.BorderSubtle)

	if isSel {
		b := gtx.Dp(1)
		if g.editing {
			b = gtx.Dp(2)
		}
		theme.FillRect(gtx, clip.Rect{Min: r.Min, Max: image.Pt(r.Max.X, r.Min.Y+b)}, th.Accent)
		theme.FillRect(gtx, clip.Rect{Min: image.Pt(r.Min.X, r.Max.Y-b), Max: r.Max}, th.Accent)
		theme.FillRect(gtx, clip.Rect{Min: r.Min, Max: image.Pt(r.Min.X+b, r.Max.Y)}, th.Accent)
		theme.FillRect(gtx, clip.Rect{Min: image.Pt(r.Max.X-b, r.Min.Y), Max: r.Max}, th.Accent)
	}
	cellClip.Pop()
}

func (g *DataGrid) drawVScrollbar(gtx layout.Context, th *theme.Theme, r image.Rectangle, rowH int, totalRows int64) {
	theme.FillRect(gtx, clip.Rect(r), th.Chrome)
	theme.FillRect(gtx, clip.Rect{Min: r.Min, Max: image.Pt(r.Min.X+1, r.Max.Y)}, th.Border)

	bodyH := r.Dy()
	visRows := int64(max(1, bodyH/rowH))
	if totalRows <= visRows || bodyH <= 0 {
		return
	}
	thumbH := g.thumbHeight(bodyH, rowH, totalRows)
	maxTop := max(int64(1), totalRows-visRows)
	ratio := float64(g.topRow) / float64(maxTop)
	ratio = math.Max(0, math.Min(1, ratio))
	thumbY := r.Min.Y + int(math.Round(ratio*float64(bodyH-thumbH)))

	thumbCol := th.TextSubtle
	if g.vBarDrag.Dragging() || g.vBarDrag.Pressed() {
		thumbCol = th.Accent
	}
	thumb := clip.RRect{
		Rect: image.Rect(r.Min.X+3, thumbY+2, r.Max.X-3, thumbY+thumbH-2),
		NE:   gtx.Dp(3), NW: gtx.Dp(3), SE: gtx.Dp(3), SW: gtx.Dp(3),
	}
	theme.FillRRect(gtx, thumb, thumbCol)

	hit := clip.Rect(r).Push(gtx.Ops)
	pointer.CursorPointer.Add(gtx.Ops)
	g.vBarDrag.Add(gtx.Ops)
	hit.Pop()
}

func (g *DataGrid) thumbHeight(bodyH, rowH int, totalRows int64) int {
	if totalRows <= 0 {
		return bodyH
	}
	vis := float64(bodyH) / float64(rowH)
	h := int(float64(bodyH) * (vis / float64(totalRows)))
	return max(24, min(bodyH, h))
}

func (g *DataGrid) scrollByPixels(dy, rowH, bodyH int, totalRows int64) {
	if rowH <= 0 || totalRows <= 0 {
		return
	}
	sub := g.subY + dy
	dRows := int64(sub / rowH)
	rem := sub % rowH
	if rem < 0 {
		dRows--
		rem += rowH
	}
	g.topRow += dRows
	g.subY = rem
	g.clampScroll(rowH, bodyH, totalRows)
}

func (g *DataGrid) clampScroll(rowH, bodyH int, totalRows int64) {
	vis := int64(max(1, bodyH/rowH))
	maxTop := max(int64(0), totalRows-vis)
	if g.topRow < 0 {
		g.topRow = 0
		g.subY = 0
	}
	if g.topRow >= maxTop {
		g.topRow = maxTop
		g.subY = 0
	}
}

func (g *DataGrid) gutterWidth(gtx layout.Context, totalRows int64) int {
	digits := len(strconv.FormatInt(max(1, totalRows), 10))
	return gtx.Dp(unit.Dp(max(44, digits*8+20)))
}

func (g *DataGrid) visibleCols(gtx layout.Context, bodyW int) (first, firstX, last int) {
	x := 0
	first = len(g.colWidths)
	for i, w := range g.colWidths {
		cw := gtx.Dp(w)
		if x+cw > g.scrollX && first == len(g.colWidths) {
			first = i
			firstX = x
		}
		if x < g.scrollX+bodyW {
			last = i
		}
		x += cw
	}
	if first == len(g.colWidths) {
		first = 0
	}
	return first, firstX, last
}

func (g *DataGrid) colAtX(gtx layout.Context, x int) int {
	cur := 0
	for i, w := range g.colWidths {
		cw := gtx.Dp(w)
		if x >= cur && x < cur+cw {
			return i
		}
		cur += cw
	}
	return -1
}

func (g *DataGrid) selectCell(row int64, col int) {
	if row == g.selRow && col == g.selCol {
		return
	}
	g.selRow = row
	g.selCol = col
	if g.OnSelectCell != nil {
		g.OnSelectCell(row, col)
	}
}

func (g *DataGrid) startInlineEdit(gtx layout.Context, row int64, col int) {
	edSrc, ok := g.src.(EditableGridSource)
	if !ok || !edSrc.Editable() {
		return
	}
	txt, kind, _, loaded := g.src.Cell(row, col)
	if !loaded || kind == data.KindBlob {
		return
	}
	if kind == data.KindNull {
		txt = ""
	}
	g.editing = true
	g.editRow = row
	g.editCol = col
	g.cellEditor.SetText(txt)
	g.cellEditor.SetCaret(g.cellEditor.Len(), 0)
	gtx.Execute(key.FocusCmd{Tag: &g.cellEditor})
}

func (g *DataGrid) commitInlineEdit(gtx layout.Context) {
	if !g.editing {
		return
	}
	g.editing = false
	if edSrc, ok := g.src.(EditableGridSource); ok && edSrc.Editable() {
		edSrc.StageCellEdit(g.editRow, g.editCol, edit.TextValue(g.cellEditor.Text()))
		if g.OnSelectCell != nil {
			g.OnSelectCell(g.editRow, g.editCol)
		}
	}
	gtx.Execute(key.FocusCmd{Tag: &g.keyTag})
}

func (g *DataGrid) cancelInlineEdit(gtx layout.Context) {
	if !g.editing {
		return
	}
	g.editing = false
	gtx.Execute(key.FocusCmd{Tag: &g.keyTag})
}

func (g *DataGrid) handleKeys(gtx layout.Context, totalRows int64, ncols int, pageRows int64) {
	for {
		ev, ok := gtx.Event(
			key.Filter{Focus: &g.keyTag, Name: key.NameUpArrow},
			key.Filter{Focus: &g.keyTag, Name: key.NameDownArrow},
			key.Filter{Focus: &g.keyTag, Name: key.NameLeftArrow},
			key.Filter{Focus: &g.keyTag, Name: key.NameRightArrow},
			key.Filter{Focus: &g.keyTag, Name: key.NamePageUp},
			key.Filter{Focus: &g.keyTag, Name: key.NamePageDown},
			key.Filter{Focus: &g.keyTag, Name: key.NameHome, Optional: key.ModCtrl},
			key.Filter{Focus: &g.keyTag, Name: key.NameEnd, Optional: key.ModCtrl},
			key.Filter{Focus: &g.keyTag, Name: key.NameF2},
			key.Filter{Focus: &g.keyTag, Name: key.NameReturn},
			key.Filter{Focus: &g.keyTag, Name: key.NameEnter},
		)
		if !ok {
			break
		}
		ke, ok := ev.(key.Event)
		if !ok || ke.State != key.Press || totalRows <= 0 || ncols <= 0 {
			continue
		}
		if (ke.Name == key.NameF2 || ke.Name == key.NameReturn || ke.Name == key.NameEnter) && g.selRow >= 0 && g.selCol >= 0 {
			g.startInlineEdit(gtx, g.selRow, g.selCol)
			continue
		}
		r, c := g.selRow, g.selCol
		if r < 0 {
			r = g.topRow
		}
		if c < 0 {
			c = 0
		}
		switch ke.Name {
		case key.NameUpArrow:
			r = max(0, r-1)
		case key.NameDownArrow:
			r = min(totalRows-1, r+1)
		case key.NameLeftArrow:
			c = max(0, c-1)
		case key.NameRightArrow:
			c = min(ncols-1, c+1)
		case key.NamePageUp:
			r = max(0, r-pageRows)
		case key.NamePageDown:
			r = min(totalRows-1, r+pageRows)
		case key.NameHome:
			if ke.Modifiers.Contain(key.ModCtrl) {
				r = 0
			} else {
				c = 0
			}
		case key.NameEnd:
			if ke.Modifiers.Contain(key.ModCtrl) {
				r = totalRows - 1
			} else {
				c = ncols - 1
			}
		}
		g.selectCell(r, c)
		g.ScrollToRow(r, pageRows)
	}
}

func withAlpha(c color.NRGBA, a uint8) color.NRGBA {
	c.A = a
	return c
}
