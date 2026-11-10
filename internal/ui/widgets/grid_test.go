package widgets

import (
	"fmt"
	"image"
	"testing"
	"time"

	"gioui.org/f32"
	"gioui.org/io/input"
	"gioui.org/io/pointer"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/unit"

	"github.com/sqlstrata/sqlstrata/internal/data"
	"github.com/sqlstrata/sqlstrata/internal/ui/theme"
)

func makeTestSource(numCols, numRows int) *data.StaticSource {
	cols := make([]data.Column, numCols)
	for c := 0; c < numCols; c++ {
		cols[c] = data.Column{
			Name: fmt.Sprintf("column_%d", c),
			Type: "TEXT",
			PK:   c == 0,
		}
	}
	src := data.NewStaticSource(cols)
	for r := 0; r < numRows; r++ {
		for c := 0; c < numCols; c++ {
			src.AppendCell([]byte(fmt.Sprintf("r%d_c%d", r, c)), data.KindText)
		}
	}
	return src
}

func TestDataGridScrollbarsAndColumnResize(t *testing.T) {
	th := theme.New()
	grid := NewDataGrid()
	src := makeTestSource(10, 500)
	grid.SetSource(src)

	var router input.Router
	var ops op.Ops
	size := image.Pt(800, 500)

	frame := func() {
		ops.Reset()
		gtx := layout.Context{
			Ops:         &ops,
			Now:         time.Now(),
			Source:      router.Source(),
			Metric:      unit.Metric{PxPerDp: 1, PxPerSp: 1},
			Constraints: layout.Exact(size),
		}
		grid.Layout(gtx, th)
		router.Frame(&ops)
	}

	// Initial frame registers all pointer hit areas.
	frame()

	gutterW := grid.gutterWidth(layout.Context{Metric: unit.Metric{PxPerDp: 1, PxPerSp: 1}}, 500)
	initialW0 := grid.ColumnWidth(0)
	if initialW0 <= 0 {
		t.Fatalf("expected positive initial column 0 width, got %v", initialW0)
	}

	// 1. Test column 0 resize by dragging its right header edge +60px to the right.
	edgeX := float32(gutterW + int(initialW0))
	headerY := float32(15)
	router.Queue(
		pointer.Event{
			Kind:      pointer.Press,
			Source:    pointer.Mouse,
			Buttons:   pointer.ButtonPrimary,
			PointerID: 1,
			Position:  f32.Pt(edgeX, headerY),
		},
	)
	frame()
	router.Queue(
		pointer.Event{
			Kind:      pointer.Move,
			Source:    pointer.Mouse,
			Buttons:   pointer.ButtonPrimary,
			PointerID: 1,
			Position:  f32.Pt(edgeX+60, headerY),
		},
		pointer.Event{
			Kind:      pointer.Release,
			Source:    pointer.Mouse,
			PointerID: 1,
			Position:  f32.Pt(edgeX+60, headerY),
		},
	)
	frame()

	newW0 := grid.ColumnWidth(0)
	if newW0 != initialW0+60 {
		t.Fatalf("column 0 resize failed: got width %v, want %v", newW0, initialW0+60)
	}

	// 2. Test vertical scrollbar dragging on the right edge (x = size.X - 6).
	vBarX := float32(size.X - 6)
	vStartY := float32(36)
	router.Queue(
		pointer.Event{
			Kind:      pointer.Press,
			Source:    pointer.Mouse,
			Buttons:   pointer.ButtonPrimary,
			PointerID: 2,
			Position:  f32.Pt(vBarX, vStartY),
		},
	)
	frame()
	router.Queue(
		pointer.Event{
			Kind:      pointer.Move,
			Source:    pointer.Mouse,
			Buttons:   pointer.ButtonPrimary,
			PointerID: 2,
			Position:  f32.Pt(vBarX, float32(size.Y/2)),
		},
		pointer.Event{
			Kind:      pointer.Release,
			Source:    pointer.Mouse,
			PointerID: 2,
			Position:  f32.Pt(vBarX, float32(size.Y/2)),
		},
	)
	frame()

	if grid.TopRow() <= 0 {
		t.Fatalf("vertical scrollbar drag did not scroll rows: TopRow() = %d", grid.TopRow())
	}

	// 3. Test horizontal scrollbar dragging at the bottom edge (y = size.Y - 6).
	hBarY := float32(size.Y - 6)
	hStartX := float32(gutterW + 10)
	router.Queue(
		pointer.Event{
			Kind:      pointer.Press,
			Source:    pointer.Mouse,
			Buttons:   pointer.ButtonPrimary,
			PointerID: 3,
			Position:  f32.Pt(hStartX, hBarY),
		},
	)
	frame()
	router.Queue(
		pointer.Event{
			Kind:      pointer.Move,
			Source:    pointer.Mouse,
			Buttons:   pointer.ButtonPrimary,
			PointerID: 3,
			Position:  f32.Pt(float32(size.X/2), hBarY),
		},
		pointer.Event{
			Kind:      pointer.Release,
			Source:    pointer.Mouse,
			PointerID: 3,
			Position:  f32.Pt(float32(size.X/2), hBarY),
		},
	)
	frame()

	if grid.ScrollX() <= 0 {
		t.Fatalf("horizontal scrollbar drag did not scroll columns: ScrollX() = %d", grid.ScrollX())
	}
}

func TestDataGridMouseWheelScroll(t *testing.T) {
	th := theme.New()
	grid := NewDataGrid()
	grid.SetSource(makeTestSource(12, 500))

	var router input.Router
	var ops op.Ops
	size := image.Pt(800, 500)

	frame := func() {
		ops.Reset()
		gtx := layout.Context{
			Ops:         &ops,
			Now:         time.Now(),
			Source:      router.Source(),
			Metric:      unit.Metric{PxPerDp: 1, PxPerSp: 1},
			Constraints: layout.Exact(size),
		}
		grid.Layout(gtx, th)
		router.Frame(&ops)
	}

	frame()

	// 1. Vertical mouse wheel over data cells scrolls rows down and back up.
	router.Queue(pointer.Event{
		Kind:     pointer.Scroll,
		Source:   pointer.Mouse,
		Position: f32.Pt(300, 200),
		Scroll:   f32.Pt(0, 240),
	})
	frame()
	if grid.TopRow() != 10 {
		t.Fatalf("body wheel down: got TopRow=%d, want 10", grid.TopRow())
	}

	router.Queue(pointer.Event{
		Kind:     pointer.Scroll,
		Source:   pointer.Mouse,
		Position: f32.Pt(300, 200),
		Scroll:   f32.Pt(0, -120),
	})
	frame()
	if grid.TopRow() != 5 {
		t.Fatalf("body wheel up: got TopRow=%d, want 5", grid.TopRow())
	}

	// 2. Vertical wheel over left row-number gutter and right vertical scrollbar.
	router.Queue(pointer.Event{
		Kind:     pointer.Scroll,
		Source:   pointer.Mouse,
		Position: f32.Pt(15, 200),
		Scroll:   f32.Pt(0, 120),
	})
	frame()
	if grid.TopRow() != 10 {
		t.Fatalf("gutter wheel down: got TopRow=%d, want 10", grid.TopRow())
	}

	router.Queue(pointer.Event{
		Kind:     pointer.Scroll,
		Source:   pointer.Mouse,
		Position: f32.Pt(float32(size.X-5), 200),
		Scroll:   f32.Pt(0, 120),
	})
	frame()
	if grid.TopRow() != 15 {
		t.Fatalf("vbar wheel down: got TopRow=%d, want 15", grid.TopRow())
	}

	// 3. Horizontal wheel / Shift+wheel over grid body scrolls columns horizontally.
	router.Queue(pointer.Event{
		Kind:     pointer.Scroll,
		Source:   pointer.Mouse,
		Position: f32.Pt(300, 200),
		Scroll:   f32.Pt(180, 0),
	})
	frame()
	if grid.ScrollX() != 180 {
		t.Fatalf("horizontal wheel right: got ScrollX=%d, want 180", grid.ScrollX())
	}

	// 4. Vertical wheel over column header and bottom horizontal scrollbar pans horizontally.
	router.Queue(pointer.Event{
		Kind:     pointer.Scroll,
		Source:   pointer.Mouse,
		Position: f32.Pt(300, 15),
		Scroll:   f32.Pt(0, 120),
	})
	frame()
	if grid.ScrollX() != 300 {
		t.Fatalf("header wheel right: got ScrollX=%d, want 300", grid.ScrollX())
	}

	router.Queue(pointer.Event{
		Kind:     pointer.Scroll,
		Source:   pointer.Mouse,
		Position: f32.Pt(300, float32(size.Y-5)),
		Scroll:   f32.Pt(0, -100),
	})
	frame()
	if grid.ScrollX() != 200 {
		t.Fatalf("hbar wheel left: got ScrollX=%d, want 200", grid.ScrollX())
	}

	// 5. Clicking a cell still works through the pass-through scroll overlays.
	router.Queue(
		pointer.Event{
			Kind:      pointer.Press,
			Source:    pointer.Mouse,
			Buttons:   pointer.ButtonPrimary,
			PointerID: 4,
			Position:  f32.Pt(100, 50),
		},
		pointer.Event{
			Kind:      pointer.Release,
			Source:    pointer.Mouse,
			PointerID: 4,
			Position:  f32.Pt(100, 50),
		},
	)
	frame()
	selR, selC := grid.SelectedCell()
	if selR < 0 || selC < 0 {
		t.Fatalf("cell click blocked by scroll overlay: selected=(%d, %d)", selR, selC)
	}

	// 6. Wide short table (no vertical overflow) pans horizontally on vertical wheel.
	grid.SetSource(makeTestSource(12, 3))
	frame()
	router.Queue(pointer.Event{
		Kind:     pointer.Scroll,
		Source:   pointer.Mouse,
		Position: f32.Pt(300, 200),
		Scroll:   f32.Pt(0, 120),
	})
	frame()
	if grid.ScrollX() != 120 {
		t.Fatalf("short wide table wheel: got ScrollX=%d, want 120", grid.ScrollX())
	}
}
