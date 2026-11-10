package iofmt

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"testing"

	"github.com/ncruces/go-sqlite3"

	"github.com/sqlstrata/sqlstrata/internal/data"
)

func TestExportQueryCSVAndJSON(t *testing.T) {
	db, err := sqlite3.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if err := db.Exec(`
		CREATE TABLE items (id INTEGER PRIMARY KEY, name TEXT, price REAL, data BLOB);
		INSERT INTO items VALUES (1, 'alpha, "quoted"', 12.5, X'0102');
		INSERT INTO items VALUES (2, NULL, NULL, NULL);
	`); err != nil {
		t.Fatal(err)
	}

	var csvBuf bytes.Buffer
	n, err := ExportQueryCSV(context.Background(), db, "SELECT id, name, price, data FROM items ORDER BY id", &csvBuf)
	if err != nil || n != 2 {
		t.Fatalf("ExportQueryCSV failed: n=%d err=%v", n, err)
	}
	recs, err := csv.NewReader(&csvBuf).ReadAll()
	if err != nil || len(recs) != 3 {
		t.Fatalf("csv parse failed: len=%d err=%v", len(recs), err)
	}
	if recs[1][1] != `alpha, "quoted"` || recs[1][3] != "AQI=" {
		t.Fatalf("unexpected csv row 1: %+v", recs[1])
	}
	if recs[2][1] != "" || recs[2][2] != "" {
		t.Fatalf("expected empty strings for NULLs, got %+v", recs[2])
	}

	var jsonBuf bytes.Buffer
	n, err = ExportQueryJSON(context.Background(), db, "SELECT id, name, price, data FROM items ORDER BY id", &jsonBuf)
	if err != nil || n != 2 {
		t.Fatalf("ExportQueryJSON failed: n=%d err=%v", n, err)
	}
	var parsed []map[string]any
	if err := json.Unmarshal(jsonBuf.Bytes(), &parsed); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, jsonBuf.String())
	}
	if len(parsed) != 2 || parsed[0]["id"] != float64(1) || parsed[0]["name"] != `alpha, "quoted"` || parsed[1]["name"] != nil {
		t.Fatalf("unexpected json output: %+v", parsed)
	}
}

func TestExportStaticCSVAndJSON(t *testing.T) {
	src := data.NewStaticSource([]data.Column{
		{Name: "k", Type: "TEXT"},
		{Name: "v", Type: "INTEGER"},
	})
	src.AppendCell([]byte("one"), data.KindText)
	src.AppendCell([]byte("42"), data.KindInteger)
	src.AppendCell(nil, data.KindNull)
	src.AppendCell([]byte("99"), data.KindInteger)

	var csvBuf bytes.Buffer
	n, err := ExportStaticCSV(src, &csvBuf)
	if err != nil || n != 2 {
		t.Fatalf("ExportStaticCSV failed: n=%d err=%v", n, err)
	}
	recs, err := csv.NewReader(&csvBuf).ReadAll()
	if err != nil || len(recs) != 3 || recs[1][0] != "one" || recs[2][0] != "" {
		t.Fatalf("unexpected static csv: %+v err=%v", recs, err)
	}

	var jsonBuf bytes.Buffer
	n, err = ExportStaticJSON(src, &jsonBuf)
	if err != nil || n != 2 {
		t.Fatalf("ExportStaticJSON failed: n=%d err=%v", n, err)
	}
	var parsed []map[string]any
	if err := json.Unmarshal(jsonBuf.Bytes(), &parsed); err != nil {
		t.Fatalf("invalid static json: %v\n%s", err, jsonBuf.String())
	}
	if parsed[0]["v"] != float64(42) || parsed[1]["k"] != nil {
		t.Fatalf("unexpected static json: %+v", parsed)
	}
}

func TestExportGridSourceSpoolCSVAndJSON(t *testing.T) {
	sp, err := data.NewResultSpool([]data.Column{
		{Name: "id", Type: "INTEGER"},
		{Name: "label", Type: "TEXT"},
		{Name: "val", Type: "REAL"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()

	if err := sp.AppendRowValues(
		[]data.Kind{data.KindInteger, data.KindText, data.KindReal},
		[][]byte{[]byte("7"), []byte(`spooled, "row"`), []byte("3.5")},
	); err != nil {
		t.Fatal(err)
	}
	if err := sp.AppendRowValues(
		[]data.Kind{data.KindInteger, data.KindNull, data.KindNull},
		[][]byte{[]byte("8"), nil, nil},
	); err != nil {
		t.Fatal(err)
	}
	if err := sp.Finish(); err != nil {
		t.Fatal(err)
	}

	var csvBuf bytes.Buffer
	n, err := ExportGridSourceCSV(sp, &csvBuf)
	if err != nil || n != 2 {
		t.Fatalf("ExportGridSourceCSV(ResultSpool) failed: n=%d err=%v", n, err)
	}
	recs, err := csv.NewReader(&csvBuf).ReadAll()
	if err != nil || len(recs) != 3 || recs[1][1] != `spooled, "row"` || recs[2][1] != "" {
		t.Fatalf("unexpected spool csv: %+v err=%v", recs, err)
	}

	var jsonBuf bytes.Buffer
	n, err = ExportGridSourceJSON(sp, &jsonBuf)
	if err != nil || n != 2 {
		t.Fatalf("ExportGridSourceJSON(ResultSpool) failed: n=%d err=%v", n, err)
	}
	var parsed []map[string]any
	if err := json.Unmarshal(jsonBuf.Bytes(), &parsed); err != nil {
		t.Fatalf("invalid spool json: %v\n%s", err, jsonBuf.String())
	}
	if len(parsed) != 2 || parsed[0]["id"] != float64(7) || parsed[0]["val"] != float64(3.5) || parsed[1]["label"] != nil {
		t.Fatalf("unexpected spool json: %+v", parsed)
	}
}


