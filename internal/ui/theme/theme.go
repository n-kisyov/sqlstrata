// Package theme defines the visual palette, typography and layout metrics of
// SQLStrata. The defaults follow Windows 11 / Fluent 2 neutral layering with
// the user's accent colour.
package theme

import (
	"image/color"
	"os"

	"gioui.org/font"
	"gioui.org/font/opentype"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/text"
	"gioui.org/unit"
	"gioui.org/widget"

	"github.com/sqlstrata/sqlstrata/internal/platform/win"
)

// Theme holds all styling tokens and the text shaper.
type Theme struct {
	Shaper *text.Shaper
	UI     font.Typeface
	Mono   font.Typeface

	Dark   bool
	UISize unit.Sp
	Code   unit.Sp
	Small  unit.Sp

	// Layered neutral surfaces (Window → Chrome → Surface → Raised).
	Window       color.NRGBA
	Chrome       color.NRGBA
	Surface      color.NRGBA
	SurfaceAlt   color.NRGBA // subtle stripe / header background
	Raised       color.NRGBA
	Hover        color.NRGBA
	Pressed      color.NRGBA
	Selected     color.NRGBA
	SelectedText color.NRGBA

	Border       color.NRGBA
	BorderSubtle color.NRGBA

	Text       color.NRGBA
	TextMuted  color.NRGBA
	TextSubtle color.NRGBA

	Accent     color.NRGBA
	AccentText color.NRGBA
	Danger     color.NRGBA

	// Cell value colours in the grid and SQL editor.
	NullText   color.NRGBA
	NumberText color.NRGBA
	StringText color.NRGBA
	BlobText   color.NRGBA
	BlobBg     color.NRGBA
	Keyword    color.NRGBA
	Comment    color.NRGBA
}

// New builds a theme matching the current Windows appearance.
func New() *Theme {
	shaper, uiFace, monoFace := loadFonts()
	th := &Theme{
		Shaper: shaper,
		UI:     uiFace,
		Mono:   monoFace,
		UISize: 13,
		Code:   12.5,
		Small:  11.5,
	}
	accent := rgb(0x0067c0)
	if a, ok := win.AccentColor(); ok {
		accent = a
	}
	th.SetDark(win.SystemDarkMode(), accent)
	return th
}

// SetDark switches between the dark and light palettes.
func (t *Theme) SetDark(dark bool, accent color.NRGBA) {
	t.Dark = dark
	t.Accent = accent
	t.AccentText = rgb(0xffffff)
	if dark {
		// Brighten a very dark accent so it stays readable on #1e1e1e.
		t.Accent = lighten(accent, 0.28)
		t.Window = rgb(0x181818)
		t.Chrome = rgb(0x1f1f1f)
		t.Surface = rgb(0x202020)
		t.SurfaceAlt = rgb(0x252525)
		t.Raised = rgb(0x2b2b2b)
		t.Hover = rgba(0xffffff, 0x10)
		t.Pressed = rgba(0xffffff, 0x18)
		t.Selected = withAlpha(t.Accent, 0x38)
		t.SelectedText = rgb(0xf5f5f5)
		t.Border = rgb(0x333333)
		t.BorderSubtle = rgb(0x2a2a2a)
		t.Text = rgb(0xebebeb)
		t.TextMuted = rgb(0xa6a6a6)
		t.TextSubtle = rgb(0x727272)
		t.Danger = rgb(0xf47067)
		t.NullText = rgb(0x6e6e6e)
		t.NumberText = rgb(0xb5cea8)
		t.StringText = rgb(0xce9178)
		t.BlobText = rgb(0x79c0ff)
		t.BlobBg = rgba(0x388bfd, 0x22)
		t.Keyword = rgb(0x569cd6)
		t.Comment = rgb(0x6a9955)
	} else {
		t.Window = rgb(0xf3f3f3)
		t.Chrome = rgb(0xededed)
		t.Surface = rgb(0xffffff)
		t.SurfaceAlt = rgb(0xf8f8f8)
		t.Raised = rgb(0xffffff)
		t.Hover = rgba(0x000000, 0x09)
		t.Pressed = rgba(0x000000, 0x12)
		t.Selected = withAlpha(t.Accent, 0x24)
		t.SelectedText = rgb(0x141414)
		t.Border = rgb(0xe0e0e0)
		t.BorderSubtle = rgb(0xeeeeee)
		t.Text = rgb(0x1b1b1b)
		t.TextMuted = rgb(0x5c5c5c)
		t.TextSubtle = rgb(0x8a8a8a)
		t.Danger = rgb(0xc42b1c)
		t.NullText = rgb(0x9a9a9a)
		t.NumberText = rgb(0x098658)
		t.StringText = rgb(0xa31515)
		t.BlobText = rgb(0x0550ae)
		t.BlobBg = rgba(0x0969da, 0x16)
		t.Keyword = rgb(0x0000ff)
		t.Comment = rgb(0x008000)
	}
}

func loadFonts() (*text.Shaper, font.Typeface, font.Typeface) {
	var faces []font.FontFace
	var ui, mono font.Typeface
	add := func(file string, weight font.Weight) font.Typeface {
		p, ok := win.FontPath(file)
		if !ok {
			return ""
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return ""
		}
		parsed, err := opentype.ParseCollection(b)
		if err != nil || len(parsed) == 0 {
			return ""
		}
		for i := range parsed {
			if weight != 0 {
				parsed[i].Font.Weight = weight
			}
		}
		faces = append(faces, parsed...)
		return parsed[0].Font.Typeface
	}
	if tf := add("SegUIVar.ttf", 0); tf != "" {
		ui = tf
	} else {
		ui = add("segoeui.ttf", font.Normal)
		add("seguisb.ttf", font.SemiBold)
		add("segoeuib.ttf", font.Bold)
	}
	if tf := add("CascadiaMono.ttf", 0); tf != "" {
		mono = tf
	} else {
		mono = add("consola.ttf", font.Normal)
		add("consolab.ttf", font.Bold)
	}
	return text.NewShaper(text.WithCollection(faces)), ui, mono
}

// Label draws a single-line truncated label.
func (t *Theme) Label(gtx layout.Context, size unit.Sp, col color.NRGBA, txt string) layout.Dimensions {
	return t.LabelFont(gtx, font.Font{Typeface: t.UI}, size, col, text.Start, txt)
}

// LabelFont draws a single-line truncated label with explicit font and alignment.
func (t *Theme) LabelFont(gtx layout.Context, f font.Font, size unit.Sp, col color.NRGBA, align text.Alignment, txt string) layout.Dimensions {
	m := op.Record(gtx.Ops)
	paint.ColorOp{Color: col}.Add(gtx.Ops)
	mat := m.Stop()
	return widget.Label{
		Alignment: align,
		MaxLines:  1,
		Truncator: "…",
	}.Layout(gtx, t.Shaper, f, size, txt, mat)
}

// Fill paints the current constraint box (0..Max) with col.
func Fill(gtx layout.Context, col color.NRGBA) {
	FillRect(gtx, clip.Rect{Max: gtx.Constraints.Max}, col)
}

// FillRect paints r with col.
func FillRect(gtx layout.Context, r clip.Rect, col color.NRGBA) {
	defer r.Push(gtx.Ops).Pop()
	paint.Fill(gtx.Ops, col)
}

// FillRRect paints a rounded rectangle.
func FillRRect(gtx layout.Context, r clip.RRect, col color.NRGBA) {
	defer r.Push(gtx.Ops).Pop()
	paint.Fill(gtx.Ops, col)
}

func rgb(v uint32) color.NRGBA {
	return color.NRGBA{R: uint8(v >> 16), G: uint8(v >> 8), B: uint8(v), A: 0xff}
}

func rgba(v uint32, a uint8) color.NRGBA {
	c := rgb(v)
	c.A = a
	return c
}

func withAlpha(c color.NRGBA, a uint8) color.NRGBA {
	c.A = a
	return c
}

func lighten(c color.NRGBA, t float32) color.NRGBA {
	mix := func(v uint8) uint8 { return uint8(float32(v) + (255-float32(v))*t) }
	return color.NRGBA{R: mix(c.R), G: mix(c.G), B: mix(c.B), A: c.A}
}
