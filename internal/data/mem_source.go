package data

import (
	"fmt"
	"strconv"
)

// StaticSource is a compact, arena-backed GridSource used for bounded query
// results in the SQL editor.
type StaticSource struct {
	cols  []Column
	nrows int64
	buf   []byte
	cells []cell
}

// NewStaticSource creates an arena-backed result set for ncols columns.
func NewStaticSource(cols []Column) *StaticSource {
	return &StaticSource{
		cols: cols,
		buf:  make([]byte, 0, 4096),
	}
}

// AppendCell appends one cell to the result set (must be called in row-major order).
func (s *StaticSource) AppendCell(val []byte, kind Kind) {
	c := cell{
		off:  uint32(len(s.buf)),
		n:    uint32(len(val)),
		kind: kind,
	}
	s.buf = append(s.buf, val...)
	s.cells = append(s.cells, c)
	if len(s.cols) > 0 && len(s.cells)%len(s.cols) == 0 {
		s.nrows++
	}
}

func (s *StaticSource) Columns() []Column            { return s.cols }
func (s *StaticSource) RowCount() int64              { return s.nrows }
func (s *StaticSource) SetVisible(first, last int64) {}

func (s *StaticSource) Cell(row int64, col int) (string, Kind, bool, bool) {
	if row < 0 || row >= s.nrows || col < 0 || col >= len(s.cols) {
		return "", KindNull, false, false
	}
	ce := s.cells[int(row)*len(s.cols)+col]
	if ce.n == 0 {
		return "", ce.kind, false, true
	}
	return string(s.buf[ce.off : ce.off+ce.n]), ce.kind, false, true
}

// Demo100MSource is a zero-allocation procedural GridSource with 100,000,000
// rows used to demonstrate and test virtualized scrolling at scale.
type Demo100MSource struct {
	rows int64
	cols []Column
}

// NewDemo100MSource creates a 100M-row synthetic dataset.
func NewDemo100MSource(rows int64) *Demo100MSource {
	return &Demo100MSource{
		rows: rows,
		cols: []Column{
			{Name: "id", Type: "INTEGER", PK: true},
			{Name: "account", Type: "TEXT"},
			{Name: "region", Type: "TEXT"},
			{Name: "amount", Type: "REAL"},
			{Name: "status", Type: "TEXT"},
			{Name: "notes", Type: "TEXT"},
			{Name: "payload", Type: "BLOB"},
		},
	}
}

func (d *Demo100MSource) Columns() []Column            { return d.cols }
func (d *Demo100MSource) RowCount() int64              { return d.rows }
func (d *Demo100MSource) SetVisible(first, last int64) {}

var (
	demoRegions  = [...]string{"EU-Central", "US-East", "AP-Tokyo", "EU-North", "SA-East", "US-West"}
	demoStatuses = [...]string{"settled", "pending", "flagged", "archived"}
)

func (d *Demo100MSource) Cell(row int64, col int) (string, Kind, bool, bool) {
	switch col {
	case 0:
		return strconv.FormatInt(row+1, 10), KindInteger, false, true
	case 1:
		return "ACCT-" + strconv.FormatInt(100_000+(row*2654435761)%900_000, 10), KindText, false, true
	case 2:
		return demoRegions[uint64(row)%uint64(len(demoRegions))], KindText, false, true
	case 3:
		v := float64((row*48271)%1_000_000) / 100.0
		return strconv.FormatFloat(v, 'f', 2, 64), KindReal, false, true
	case 4:
		return demoStatuses[uint64(row)%uint64(len(demoStatuses))], KindText, false, true
	case 5:
		if row%7 == 0 {
			return "", KindNull, false, true
		}
		return fmt.Sprintf("Synthetic ledger entry #%d", row+1), KindText, false, true
	case 6:
		if row%5 == 0 {
			return "BLOB 64.0 KB", KindBlob, false, true
		}
		return "", KindNull, false, true
	}
	return "", KindNull, false, true
}
