package widgets

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"strings"
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

func TestInspectorTimestampAndImageHelpers(t *testing.T) {
	if got := FormatTimestampHint("1700000000", data.KindInteger); !strings.Contains(got, "2023-11-14 22:13:20 UTC") {
		t.Fatalf("unexpected Unix seconds hint: %q", got)
	}
	if got := FormatTimestampHint("1700000000123", data.KindInteger); !strings.Contains(got, "2023-11-14 22:13:20.123 UTC") {
		t.Fatalf("unexpected Unix ms hint: %q", got)
	}
	if got := FormatTimestampHint("42", data.KindInteger); got != "" {
		t.Fatalf("expected empty hint for small integer, got %q", got)
	}
	if got := FormatTimestampHint("1700000000", data.KindText); got != "" {
		t.Fatalf("expected empty hint for TEXT column, got %q", got)
	}

	if got := DetectImageFormat([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0}); got != "png" {
		t.Fatalf("expected png, got %q", got)
	}
	if got := DetectImageFormat([]byte{0xFF, 0xD8, 0xFF, 0xE0}); got != "jpeg" {
		t.Fatalf("expected jpeg, got %q", got)
	}
	if got := DetectImageFormat([]byte("GIF89a...")); got != "gif" {
		t.Fatalf("expected gif, got %q", got)
	}
	if got := DetectImageFormat([]byte("plain text")); got != "" {
		t.Fatalf("expected empty format for plain text, got %q", got)
	}
}

func TestInspectorImageHexOffsetAndActions(t *testing.T) {
	th := theme.New()
	ins := NewInspector()

	var loadedOffset int64 = -1
	var exportedCalled bool
	var importedCalled bool
	ins.OnLoadOffset = func(row int64, col int, offset int64) {
		loadedOffset = offset
	}
	ins.OnExportCell = func(row int64, col int) {
		exportedCalled = true
	}
	ins.OnImportFile = func(row int64, col int) {
		importedCalled = true
	}

	// Build a real 4x4 PNG image.
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	img.Set(1, 1, color.RGBA{R: 255, A: 255})
	var pngBuf bytes.Buffer
	if err := png.Encode(&pngBuf, img); err != nil {
		t.Fatal(err)
	}

	col := data.Column{Name: "avatar", Type: "BLOB"}
	ins.SetCell(2, 1, col, "BLOB", data.KindBlob, false, true)
	ins.SetDetail(data.CellDetail{
		Row:        2,
		Col:        1,
		Kind:       data.KindBlob,
		Offset:     0,
		TotalBytes: int64(pngBuf.Len()),
		Data:       pngBuf.Bytes(),
		Truncated:  false,
	}, col, true)

	if ins.ModeName() != "image" {
		t.Fatalf("expected PNG cell detail to auto-switch to image mode, got %q", ins.ModeName())
	}
	sz, fm, ok := ins.HasDecodedImage()
	if !ok || sz != image.Pt(4, 4) || fm != "png" {
		t.Fatalf("unexpected decoded image: sz=%v fmt=%q ok=%v", sz, fm, ok)
	}

	// Now simulate incremental paging into a large 256 KB BLOB at offset 64 KB.
	ins.SetDetail(data.CellDetail{
		Row:        2,
		Col:        1,
		Kind:       data.KindBlob,
		Offset:     InspectorPageBytes,
		TotalBytes: 4 * InspectorPageBytes,
		Data:       []byte("ABCDEFGHIJKLMNOP"),
		Truncated:  true,
	}, col, true)

	var router input.Router
	var ops op.Ops
	paneSz := image.Pt(320, 480)
	frame := func() {
		ops.Reset()
		gtx := layout.Context{
			Ops:         &ops,
			Now:         time.Now(),
			Source:      router.Source(),
			Metric:      unit.Metric{PxPerDp: 1, PxPerSp: 1},
			Constraints: layout.Exact(paneSz),
		}
		ins.Layout(gtx, th)
		router.Frame(gtx.Ops)
	}
	clickAt := func(x, y float32) {
		router.Queue(
			pointer.Event{Kind: pointer.Press, Buttons: pointer.ButtonPrimary, Position: f32.Pt(x, y)},
			pointer.Event{Kind: pointer.Release, Position: f32.Pt(x, y)},
		)
		frame()
		frame()
	}

	frame()

	// Switch to Hex tab (header tabs at w=320: xImg=276..312, xHex=237..273, xJSON=198..234, xTxt=159..195).
	clickAt(255, 18)
	if ins.ModeName() != "hex" {
		t.Fatalf("expected hex mode after clicking Hex tab, got %q", ins.ModeName())
	}
	lines := ins.Lines()
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "010000  ") {
		t.Fatalf("expected hex address starting at 010000 for 64 KB offset, got %v", lines)
	}

	// Header=36px, Meta=48px -> paging bar sits at y=[84, 118), center y=101.
	clickAt(140, 101)
	if loadedOffset != InspectorPageBytes+16 {
		t.Fatalf("expected OnLoadOffset(%d), got %d", InspectorPageBytes+16, loadedOffset)
	}

	// Click "< Prev 64 KB" button (x ~ 50, y ~ 101).
	clickAt(50, 101)
	if loadedOffset != 0 {
		t.Fatalf("expected OnLoadOffset(0), got %d", loadedOffset)
	}

	// Click "Export Cell…" and "Import File…" in bottom file bar (y = 480-18 = 462).
	clickAt(50, 462)
	if !exportedCalled {
		t.Fatal("expected OnExportCell callback when clicking Export Cell…")
	}
	clickAt(140, 462)
	if !importedCalled {
		t.Fatal("expected OnImportFile callback when clicking Import File…")
	}
}
