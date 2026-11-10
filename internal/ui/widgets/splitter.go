// Package widgets provides custom, theme-aware Gio widgets built for SQLStrata.
package widgets

import (
	"image"

	"gioui.org/gesture"
	"gioui.org/io/pointer"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/unit"

	"github.com/sqlstrata/sqlstrata/internal/ui/theme"
)

// Splitter lays out two panes separated by a 1 px divider with a wider
// invisible drag handle.
type Splitter struct {
	Axis layout.Axis
	// Size is the width (Horizontal) or height (Vertical) of the first pane,
	// or of the second pane when SecondFixed is true.
	Size        unit.Dp
	Min         unit.Dp
	Max         unit.Dp
	SecondFixed bool
	Collapsed   bool

	drag   gesture.Drag
	origin unit.Dp
	active bool
}

// Layout draws first and second separated by the draggable divider.
func (s *Splitter) Layout(gtx layout.Context, th *theme.Theme, first, second layout.Widget) layout.Dimensions {
	total := gtx.Constraints.Max
	if s.Collapsed {
		if s.SecondFixed {
			return first(gtx)
		}
		return second(gtx)
	}

	if s.Min == 0 {
		s.Min = 140
	}
	if s.Max == 0 {
		s.Max = 640
	}
	if s.Size < s.Min {
		s.Size = s.Min
	}

	// Process drag events on the divider handle.
	for {
		ev, ok := s.drag.Update(gtx.Metric, gtx.Source, gesture.Axis(s.Axis))
		if !ok {
			break
		}
		switch ev.Kind {
		case pointer.Press:
			s.active = true
			if s.Axis == layout.Horizontal {
				s.origin = unit.Dp(ev.Position.X) / unit.Dp(gtx.Metric.PxPerDp)
			} else {
				s.origin = unit.Dp(ev.Position.Y) / unit.Dp(gtx.Metric.PxPerDp)
			}
		case pointer.Drag:
			var cur unit.Dp
			if s.Axis == layout.Horizontal {
				cur = unit.Dp(ev.Position.X) / unit.Dp(gtx.Metric.PxPerDp)
			} else {
				cur = unit.Dp(ev.Position.Y) / unit.Dp(gtx.Metric.PxPerDp)
			}
			delta := cur - s.origin
			if s.SecondFixed {
				s.Size -= delta
			} else {
				s.Size += delta
			}
			if s.Size < s.Min {
				s.Size = s.Min
			}
			if s.Size > s.Max {
				s.Size = s.Max
			}
		case pointer.Release, pointer.Cancel:
			s.active = false
		}
	}

	px := gtx.Dp(s.Size)
	divPx := max(1, gtx.Dp(1))
	hitPad := gtx.Dp(4)

	if s.Axis == layout.Horizontal {
		avail := max(0, total.X-divPx)
		if px > avail-gtx.Dp(120) {
			px = max(gtx.Dp(s.Min), avail-gtx.Dp(120))
		}
		w1, w2 := px, avail-px
		if s.SecondFixed {
			w2 = px
			w1 = avail - px
		}

		// First pane.
		clip1 := clip.Rect{Max: image.Pt(w1, total.Y)}.Push(gtx.Ops)
		c1 := gtx
		c1.Constraints = layout.Exact(image.Pt(w1, total.Y))
		first(c1)
		clip1.Pop()

		// Second pane.
		s2 := op.Offset(image.Pt(w1+divPx, 0)).Push(gtx.Ops)
		clip2 := clip.Rect{Max: image.Pt(w2, total.Y)}.Push(gtx.Ops)
		c2 := gtx
		c2.Constraints = layout.Exact(image.Pt(w2, total.Y))
		second(c2)
		clip2.Pop()
		s2.Pop()

		// 1 px visual border.
		col := th.Border
		if s.active || s.drag.Dragging() {
			col = th.Accent
		}
		theme.FillRect(gtx, clip.Rect{
			Min: image.Pt(w1, 0),
			Max: image.Pt(w1+divPx, total.Y),
		}, col)

		// Wider hit target for dragging.
		hit := clip.Rect{
			Min: image.Pt(w1-hitPad, 0),
			Max: image.Pt(w1+divPx+hitPad, total.Y),
		}.Push(gtx.Ops)
		pointer.CursorColResize.Add(gtx.Ops)
		s.drag.Add(gtx.Ops)
		hit.Pop()
	} else {
		avail := max(0, total.Y-divPx)
		if px > avail-gtx.Dp(80) {
			px = max(gtx.Dp(s.Min), avail-gtx.Dp(80))
		}
		h1, h2 := px, avail-px
		if s.SecondFixed {
			h2 = px
			h1 = avail - px
		}

		clip1 := clip.Rect{Max: image.Pt(total.X, h1)}.Push(gtx.Ops)
		c1 := gtx
		c1.Constraints = layout.Exact(image.Pt(total.X, h1))
		first(c1)
		clip1.Pop()

		s2 := op.Offset(image.Pt(0, h1+divPx)).Push(gtx.Ops)
		clip2 := clip.Rect{Max: image.Pt(total.X, h2)}.Push(gtx.Ops)
		c2 := gtx
		c2.Constraints = layout.Exact(image.Pt(total.X, h2))
		second(c2)
		clip2.Pop()
		s2.Pop()

		col := th.Border
		if s.active || s.drag.Dragging() {
			col = th.Accent
		}
		theme.FillRect(gtx, clip.Rect{
			Min: image.Pt(0, h1),
			Max: image.Pt(total.X, h1+divPx),
		}, col)

		hit := clip.Rect{
			Min: image.Pt(0, h1-hitPad),
			Max: image.Pt(total.X, h1+divPx+hitPad),
		}.Push(gtx.Ops)
		pointer.CursorRowResize.Add(gtx.Ops)
		s.drag.Add(gtx.Ops)
		hit.Pop()
	}

	return layout.Dimensions{Size: total}
}
