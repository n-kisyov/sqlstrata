package theme

import (
	"image"
	"testing"

	"gioui.org/font"
	"gioui.org/gpu/headless"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/text"
	"gioui.org/unit"
)

func TestThemeFontsAndHeadlessRender(t *testing.T) {
	th := New()
	t.Logf("UI typeface=%q, Mono typeface=%q, Dark=%v, Text=%+v, Surface=%+v",
		th.UI, th.Mono, th.Dark, th.Text, th.Surface)

	sz := image.Pt(400, 100)
	w, err := headless.NewWindow(sz.X, sz.Y)
	if err != nil {
		t.Skipf("headless window unavailable: %v", err)
	}
	defer w.Release()

	var ops op.Ops
	gtx := layout.Context{
		Ops:         &ops,
		Metric:      unit.Metric{PxPerDp: 1, PxPerSp: 1},
		Constraints: layout.Exact(sz),
	}
	Fill(gtx, th.Surface)
	dims := th.LabelFont(gtx, font.Font{Typeface: th.UI}, th.UISize, th.Text, text.Start, "Hello SQLStrata 12345")
	t.Logf("Label dims=%+v", dims)

	if err := w.Frame(&ops); err != nil {
		t.Fatal(err)
	}
	img := image.NewRGBA(image.Rectangle{Max: sz})
	if err := w.Screenshot(img); err != nil {
		t.Fatal(err)
	}
	nonBg := 0
	bg := th.Surface
	for y := 0; y < sz.Y; y++ {
		for x := 0; x < sz.X; x++ {
			c := img.RGBAAt(x, y)
			if c.R != bg.R || c.G != bg.G || c.B != bg.B {
				nonBg++
			}
		}
	}
	t.Logf("non-background pixels rendered: %d", nonBg)
	if nonBg == 0 {
		t.Fatal("ZERO text pixels rendered!")
	}
}

