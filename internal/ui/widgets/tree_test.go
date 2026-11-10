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

	"github.com/sqlstrata/sqlstrata/internal/engine"
	"github.com/sqlstrata/sqlstrata/internal/ui/theme"
)

func TestTreeViewMouseWheelScroll(t *testing.T) {
	th := theme.New()
	tv := NewTreeView()
	s := &engine.Schema{}
	for i := range 60 {
		s.Objects = append(s.Objects, &engine.Object{
			Type: engine.TypeTable,
			Name: fmt.Sprintf("table_%02d", i),
		})
	}
	tv.SetSchema(s)

	var router input.Router
	var ops op.Ops
	size := image.Pt(250, 300)

	frame := func() {
		ops.Reset()
		gtx := layout.Context{
			Ops:         &ops,
			Now:         time.Now(),
			Source:      router.Source(),
			Metric:      unit.Metric{PxPerDp: 1, PxPerSp: 1},
			Constraints: layout.Exact(size),
		}
		tv.Layout(gtx, th)
		router.Frame(&ops)
	}

	frame()
	if tv.list.Position.First != 0 {
		t.Fatalf("initial First=%d, want 0", tv.list.Position.First)
	}

	router.Queue(pointer.Event{
		Kind:     pointer.Scroll,
		Source:   pointer.Mouse,
		Position: f32.Pt(100, 150),
		Scroll:   f32.Pt(0, 120),
	})
	frame()

	if tv.list.Position.First == 0 && tv.list.Position.Offset == 0 {
		t.Fatalf("TreeView did not scroll on mouse wheel")
	}
}

func TestTreeViewClickIndexAndTrigger(t *testing.T) {
	th := theme.New()
	tv := NewTreeView()
	s := &engine.Schema{
		Objects: []*engine.Object{
			{Type: engine.TypeTable, Name: "orders"},
			{Type: engine.TypeIndex, Name: "orders_created", Table: "orders", SQL: "CREATE INDEX orders_created ON orders(created_at)"},
			{Type: engine.TypeTrigger, Name: "orders_tr", Table: "orders", SQL: "CREATE TRIGGER orders_tr BEFORE INSERT ON orders BEGIN SELECT 1; END"},
		},
	}
	tv.SetSchema(s)

	var got []*engine.Object
	tv.OnSelect = func(o *engine.Object) {
		got = append(got, o)
	}

	var router input.Router
	var ops op.Ops
	size := image.Pt(250, 300)
	frame := func() {
		ops.Reset()
		gtx := layout.Context{
			Ops:         &ops,
			Now:         time.Now(),
			Source:      router.Source(),
			Metric:      unit.Metric{PxPerDp: 1, PxPerSp: 1},
			Constraints: layout.Exact(size),
		}
		tv.Layout(gtx, th)
		router.Frame(&ops)
	}

	frame()
	// Nodes: filter=36px, then rows of 26px:
	// 0: cat:table (36..62), 1: orders (62..88), 2: cat:index (88..114),
	// 3: orders_created (114..140), 4: cat:trigger (140..166), 5: orders_tr (166..192).
	clickAt := func(y float32, id pointer.ID) {
		router.Queue(
			pointer.Event{
				Kind:      pointer.Press,
				Source:    pointer.Mouse,
				Buttons:   pointer.ButtonPrimary,
				PointerID: id,
				Position:  f32.Pt(80, y),
			},
			pointer.Event{
				Kind:      pointer.Release,
				Source:    pointer.Mouse,
				PointerID: id,
				Position:  f32.Pt(80, y),
			},
		)
		frame()
	}

	clickAt(127, 1) // orders_created
	clickAt(179, 2) // orders_tr

	if len(got) != 2 {
		t.Fatalf("expected 2 OnSelect calls, got %d", len(got))
	}
	if got[0].Name != "orders_created" || got[0].Type != engine.TypeIndex {
		t.Fatalf("first selection mismatch: %+v", got[0])
	}
	if got[1].Name != "orders_tr" || got[1].Type != engine.TypeTrigger {
		t.Fatalf("second selection mismatch: %+v", got[1])
	}
}

