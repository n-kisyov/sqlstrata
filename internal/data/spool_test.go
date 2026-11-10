package data

import (
	"fmt"
	"testing"
)

func TestResultSpoolPagingAndEviction(t *testing.T) {
	cols := []Column{
		{Name: "id", Type: "INTEGER"},
		{Name: "val", Type: "TEXT"},
	}
	sp, err := NewResultSpool(cols)
	if err != nil {
		t.Fatal(err)
	}
	sp.budget = 8 * 1024 // tiny budget to exercise LRU eviction across pages
	defer sp.Close()

	const total = 1500 // ~6 pages of 256 rows
	for r := range total {
		kinds := []Kind{KindInteger, KindText}
		vals := [][]byte{
			[]byte(fmt.Sprintf("%d", r)),
			[]byte(fmt.Sprintf("payload-row-%05d-padding-text", r)),
		}
		if r%10 == 0 {
			kinds[1] = KindNull
			vals[1] = nil
		}
		if err := sp.AppendRowValues(kinds, vals); err != nil {
			t.Fatal(err)
		}
	}
	if err := sp.Finish(); err != nil {
		t.Fatal(err)
	}
	if sp.RowCount() != total {
		t.Fatalf("RowCount = %d, want %d", sp.RowCount(), total)
	}

	// Access rows across distant pages forward and backward.
	checkRows := []int64{0, 255, 256, 700, 1499, 5, 1024, 300}
	for _, r := range checkRows {
		idTxt, idKind, _, ok := sp.Cell(r, 0)
		if !ok || idKind != KindInteger || idTxt != fmt.Sprintf("%d", r) {
			t.Fatalf("row %d col 0: got (%q, %v, ok=%v)", r, idTxt, idKind, ok)
		}
		vTxt, vKind, _, ok := sp.Cell(r, 1)
		if !ok {
			t.Fatalf("row %d col 1 not ok", r)
		}
		if r%10 == 0 {
			if vKind != KindNull {
				t.Fatalf("row %d col 1: expected KindNull, got %v", r, vKind)
			}
		} else if vTxt != fmt.Sprintf("payload-row-%05d-padding-text", r) {
			t.Fatalf("row %d col 1 mismatch: got %q", r, vTxt)
		}
	}
}

