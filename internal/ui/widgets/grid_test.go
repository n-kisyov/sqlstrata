package widgets

import (
	"fmt"
	"image"
	"strconv"
	"testing"
	"time"

	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/unit"

	"github.com/sqlstrata/sqlstrata/internal/data"
	"github.com/sqlstrata/sqlstrata/internal/ui/theme"
)

type syntheticSource struct {
	rows      int64
	cols      []data.Column
	visFirst  int64
	visLast   int64
	cellCalls int
}

func newSyntheticSource(rows int64, ncols int) *syntheticSource {
	cols := make([]data.Column, ncols)
	for i := range cols {
		cols[i] = data.Column{
			Name: fmt.Sprintf("col_%02d", i),
			Type: "TEXT",
			PK:   i == 0,
		}
	}
	return &syntheticSource{rows: rows, cols: cols}
}

func (s *syntheticSource) Columns() []data.Column       { return s.cols }
func (s *syntheticSource) RowCount() int64              { return s.rows }
func (s *syntheticSource) SetVisible(first, last int64) { s.visFirst, s.visLast = first, last }
func (s *syntheticSource) Cell(row int64, col int) (string, data.Kind, bool, bool) {
	s.cellCalls++
	if col == 0 {
		return strconv.FormatInt(row+1, 10), data.KindInteger, false, true
	}
	if (row+int64(col))%11 == 0 {
		return "", data.KindNull, false, true
	}
	return "value_" + strconv.FormatInt(row, 10), data.KindText, false, true
}

func TestDataGrid100MRowsVirtualization(t *testing.T) {
	const totalRows = 100_000_000
	src := newSyntheticSource(totalRows, 50)
	grid := NewDataGrid()
	grid.SetSource(src)
	th := theme.New()

	var ops op.Ops
	gtx := layout.Context{
		Ops: &ops,
		Metric: unit.Metric{
			PxPerDp: 1,
			PxPerSp: 1,
		},
		Constraints: layout.Exact(image.Pt(1600, 900)),
		Now:         time.Now(),
	}

	// Warm up the font shaper caches on the first frame.
	grid.Layout(gtx, th)

	// Jump to various positions across 100M rows and measure frame layout cost.
	positions := []int64{0, 500, 12_345_678, 73_000_000, 99_999_950}
	var totalElapsed time.Duration
	for _, pos := range positions {
		ops.Reset()
		src.cellCalls = 0
		grid.ScrollToRow(pos, 35)
		start := time.Now()
		dims := grid.Layout(gtx, th)
		totalElapsed += time.Since(start)

		if dims.Size != image.Pt(1600, 900) {
			t.Fatalf("unexpected dims %v", dims.Size)
		}
		// Only visible rows × visible columns should ever be queried.
		if src.cellCalls > 45*20 {
			t.Fatalf("at row %d: queried %d cells, virtualization broken", pos, src.cellCalls)
		}
	}
	if avg := totalElapsed / time.Duration(len(positions)); avg > 16*time.Millisecond {
		t.Errorf("average frame took %s (> 16ms 60fps budget)", avg)
	}
}

func BenchmarkDataGrid100MRows(b *testing.B) {
	src := newSyntheticSource(100_000_000, 50)
	grid := NewDataGrid()
	grid.SetSource(src)
	th := theme.New()

	var ops op.Ops
	gtx := layout.Context{
		Ops: &ops,
		Metric: unit.Metric{
			PxPerDp: 1,
			PxPerSp: 1,
		},
		Constraints: layout.Exact(image.Pt(1920, 1080)),
		Now:         time.Now(),
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ops.Reset()
		grid.ScrollToRow(int64(i*997)%99_999_900, 40)
		grid.Layout(gtx, th)
	}
}
