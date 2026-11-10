// Package data implements memory-bounded, virtualized access to table rows
// for the data grid: keyset paging, a sparse anchor index for random jumps,
// asynchronous row counting and an LRU page cache with a byte budget.
package data

import (
	"strconv"
	"unsafe"
)

// Kind is the storage class of a cell as reported by SQLite's typeof().
type Kind uint8

const (
	KindLoading Kind = iota // page not fetched yet
	KindNull
	KindInteger
	KindReal
	KindText
	KindBlob
)

// Column is the grid-facing description of a column.
type Column struct {
	Name string
	Type string
	PK   bool
}

const (
	// PageRows is the number of rows fetched per query.
	PageRows = 256
	// AnchorStride is the distance (in rows) between anchors.
	AnchorStride = 1024
	// PreviewBytes is how much of a text value the grid fetches.
	PreviewBytes = 256
)

type cell struct {
	off, n uint32
	kind   Kind
	trunc  bool // text was cut to PreviewBytes
}

// page holds PageRows rows of display data in a single byte arena, which keeps
// the number of heap objects (and GC work) independent of the cell count.
type page struct {
	index   int64
	nrows   int
	ncols   int
	buf     []byte
	cells   []cell
	keys    []int64 // rowid of each row (rowid tables only)
	lastUse uint64
}

func (p *page) size() int {
	return cap(p.buf) + cap(p.cells)*int(unsafe.Sizeof(cell{})) + cap(p.keys)*8 + 128
}

func (p *page) text(r, c int) (string, Kind, bool) {
	ce := p.cells[r*p.ncols+c]
	if ce.n == 0 {
		return "", ce.kind, ce.trunc
	}
	// The arena is never mutated after the page is built, so handing out
	// zero-copy strings is safe.
	return unsafe.String(&p.buf[ce.off], int(ce.n)), ce.kind, ce.trunc
}

// FormatSize renders a byte count compactly (e.g. "64 KB").
func FormatSize(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	v := float64(n) / float64(div)
	prec := 1
	if v >= 100 {
		prec = 0
	}
	return strconv.FormatFloat(v, 'f', prec, 64) + " " + string("KMGTPE"[exp]) + "B"
}
