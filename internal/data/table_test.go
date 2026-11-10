package data

import (
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/ncruces/go-sqlite3"

	"github.com/sqlstrata/sqlstrata/internal/edit"
	"github.com/sqlstrata/sqlstrata/internal/engine"
)

// makeDB creates a table with gaps in its rowids so that offset-based and
// keyset-based positioning disagree if implemented wrongly.
func makeDB(t *testing.T, n int) (path string, ids []int64) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "t.db")
	db, err := sqlite3.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Exec(`CREATE TABLE t(id INTEGER PRIMARY KEY, name TEXT, v REAL, b BLOB);
		CREATE TABLE w(k TEXT PRIMARY KEY, v) WITHOUT ROWID;
		CREATE VIEW vw AS SELECT id, name FROM t WHERE id % 2 = 0;
		BEGIN;`); err != nil {
		t.Fatal(err)
	}
	stmt, _, err := db.Prepare(`INSERT INTO t VALUES (?, ?, ?, ?)`)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewPCG(1, 1))
	id := int64(0)
	for i := range n {
		id += 1 + rng.Int64N(5)
		ids = append(ids, id)
		stmt.BindInt64(1, id)
		stmt.BindText(2, "row"+strconv.FormatInt(id, 10))
		if i%7 == 0 {
			stmt.BindNull(3)
		} else {
			stmt.BindFloat(3, float64(i)/4)
		}
		if i%100 == 0 {
			stmt.BindZeroBlob(4, 4096)
		} else {
			stmt.BindNull(4)
		}
		if err := stmt.Exec(); err != nil {
			t.Fatal(err)
		}
	}
	stmt.Close()
	if err := db.Exec(`INSERT INTO w SELECT 'k' || printf('%06d', id), id FROM t; COMMIT;`); err != nil {
		t.Fatal(err)
	}
	return path, ids
}

func openSource(t *testing.T, path, table string) (*TableSource, chan struct{}) {
	t.Helper()
	db, err := engine.Open(path, engine.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	var schema *engine.Schema
	if err := db.Fg.Do(t.Context(), func(c *sqlite3.Conn) error {
		schema, err = engine.LoadSchema(c)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	obj := schema.Find(table)
	if obj == nil {
		t.Fatalf("table %s not found", table)
	}
	ch := make(chan struct{}, 1)
	src := NewTableSource(db, obj, SourceOptions{CacheBudget: 64 << 10}, func() {
		select {
		case ch <- struct{}{}:
		default:
		}
	})
	t.Cleanup(src.Close)
	return src, ch
}

// loadCell waits until the given cell is loaded.
func loadCell(t *testing.T, src *TableSource, ch chan struct{}, row int64, col int) (string, Kind) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	src.SetVisible(row, row+20)
	for {
		if txt, kind, _, ok := src.Cell(row, col); ok {
			return txt, kind
		}
		if err := src.Err(); err != nil {
			t.Fatal(err)
		}
		select {
		case <-ch:
		case <-deadline:
			t.Fatalf("timeout loading row %d", row)
		}
	}
}

func waitExact(t *testing.T, src *TableSource, ch chan struct{}) int64 {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		if n, st := src.Count(); st == CountExact {
			return n
		}
		select {
		case <-ch:
		case <-deadline:
			t.Fatal("timeout waiting for count")
		}
	}
}

func TestTableSourceRandomAccess(t *testing.T) {
	const n = 20_000
	path, ids := makeDB(t, n)
	for _, phase := range []string{"before-anchors", "with-anchors"} {
		t.Run(phase, func(t *testing.T) {
			src, ch := openSource(t, path, "t")
			if phase == "with-anchors" {
				if got := waitExact(t, src, ch); got != n {
					t.Fatalf("count = %d, want %d", got, n)
				}
			}
			rng := rand.New(rand.NewPCG(3, 3))
			rows := []int64{0, 1, PageRows - 1, PageRows, n - 1, n / 2}
			for range 200 {
				rows = append(rows, rng.Int64N(n))
			}
			// Sequential scroll down and up exercises both keyset directions.
			for r := int64(5000); r < 6000; r += 37 {
				rows = append(rows, r)
			}
			for r := int64(9000); r > 8000; r -= 41 {
				rows = append(rows, r)
			}
			for _, r := range rows {
				got, kind := loadCell(t, src, ch, r, 0)
				if want := strconv.FormatInt(ids[r], 10); got != want || kind != KindInteger {
					t.Fatalf("row %d: id = %q (%v), want %s", r, got, kind, want)
				}
				name, _ := loadCell(t, src, ch, r, 1)
				if name != "row"+strconv.FormatInt(ids[r], 10) {
					t.Fatalf("row %d: name = %q", r, name)
				}
			}
			if b := src.CacheBytes(); b > 64<<10+200<<10 {
				t.Errorf("cache grew to %d bytes despite budget", b)
			}
		})
	}
}

func TestTableSourceKinds(t *testing.T) {
	path, _ := makeDB(t, 500)
	src, ch := openSource(t, path, "t")
	if _, k := loadCell(t, src, ch, 0, 2); k != KindNull {
		t.Errorf("v[0] kind = %v, want null", k)
	}
	if txt, k := loadCell(t, src, ch, 0, 3); k != KindBlob || txt != "BLOB 4.0 KB" {
		t.Errorf("b[0] = %q (%v)", txt, k)
	}
	if _, k := loadCell(t, src, ch, 1, 2); k != KindReal {
		t.Errorf("v[1] kind = %v, want real", k)
	}
}

func TestTableSourceWithoutRowidAndView(t *testing.T) {
	path, ids := makeDB(t, 3000)
	src, ch := openSource(t, path, "w")
	if n := waitExact(t, src, ch); n != 3000 {
		t.Fatalf("w count = %d", n)
	}
	got, _ := loadCell(t, src, ch, 2999, 0)
	if want := fmt.Sprintf("k%06d", ids[2999]); got != want {
		t.Errorf("w last key = %q, want %q", got, want)
	}

	vs, vch := openSource(t, path, "vw")
	even := 0
	for _, id := range ids {
		if id%2 == 0 {
			even++
		}
	}
	if n := waitExact(t, vs, vch); n != int64(even) {
		t.Fatalf("view count = %d, want %d", n, even)
	}
}

func TestFormatSize(t *testing.T) {
	for in, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1024: "1.0 KB", 65536: "64.0 KB", 5 << 30: "5.0 GB", 150 << 20: "150 MB"} {
		if got := FormatSize(in); got != want {
			t.Errorf("FormatSize(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestBigDBOpenAndPageLatency(t *testing.T) {
	path := filepath.Join("..", "..", "testdata", "big.db")
	if _, err := filepath.Abs(path); err != nil {
		t.Skip(err)
	}
	t0 := time.Now()
	db, err := engine.Open(path, engine.DefaultOptions())
	if err != nil {
		t.Skipf("big.db not available: %v", err)
	}
	defer db.Close()

	var schema *engine.Schema
	if err := db.Fg.Do(t.Context(), func(c *sqlite3.Conn) error {
		var err error
		schema, err = engine.LoadSchema(c)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	openDur := time.Since(t0)

	orders := schema.Find("orders")
	if orders == nil {
		t.Fatal("orders table missing in big.db")
	}
	ch := make(chan struct{}, 1)
	t1 := time.Now()
	src := NewTableSource(db, orders, SourceOptions{CacheBudget: 32 << 20}, func() {
		select {
		case ch <- struct{}{}:
		default:
		}
	})
	defer src.Close()

	got, kind := loadCell(t, src, ch, 0, 0)
	firstPageDur := time.Since(t1)
	if got != "1" || kind != KindInteger {
		t.Fatalf("first cell = %q (%v)", got, kind)
	}
	t.Logf("big.db (20M rows): open+schema=%s, first-page=%s, cache=%s",
		openDur.Round(time.Microsecond), firstPageDur.Round(time.Microsecond), FormatSize(int64(src.CacheBytes())))
	if openDur > time.Second {
		t.Errorf("open+schema took %s (> 1s target)", openDur)
	}
	if firstPageDur > 100*time.Millisecond {
		t.Errorf("first page took %s (> 100ms target)", firstPageDur)
	}
}

func TestTableSourceSortFilterAndStagedEdit(t *testing.T) {
	path, ids := makeDB(t, 1000)
	src, ch := openSource(t, path, "t")
	_ = waitExact(t, src, ch)

	// 1. Sort descending by id (col 0, which is RowidAlias).
	src.SetSort(0, true)
	got, _ := loadCell(t, src, ch, 0, 0)
	if want := strconv.FormatInt(ids[len(ids)-1], 10); got != want {
		t.Fatalf("desc sort row 0 id = %q, want %s", got, want)
	}

	// 2. Reset sort and apply a filter.
	src.SetSort(-1, false)
	targetID := ids[42]
	src.SetFilter(fmt.Sprintf("id = %d", targetID))
	if n := waitExact(t, src, ch); n != 1 {
		t.Fatalf("filtered count = %d, want 1", n)
	}
	got, _ = loadCell(t, src, ch, 0, 1)
	if want := "row" + strconv.FormatInt(targetID, 10); got != want {
		t.Fatalf("filtered name = %q, want %q", got, want)
	}

	// 3. Stage an edit on the filtered row and verify overlay + CellStatus.
	if !src.StageCellEdit(0, 1, edit.TextValue("updated_name")) {
		t.Fatal("StageCellEdit failed")
	}
	if edited, del, ins := src.CellStatus(0, 1); !edited || del || ins {
		t.Fatalf("unexpected CellStatus: edited=%v del=%v ins=%v", edited, del, ins)
	}
	got, _ = loadCell(t, src, ch, 0, 1)
	if got != "updated_name" {
		t.Fatalf("staged cell = %q, want updated_name", got)
	}
}
