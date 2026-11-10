package iofmt

import (
	"context"
	"strings"
	"testing"

	"github.com/ncruces/go-sqlite3"
)

func TestDetectCSVDelimiterAndInferTypes(t *testing.T) {
	if got := DetectCSVDelimiter([]byte("\xEF\xBB\xBFid\tname\tamount\n1\t\"a,b\"\t12.5\n2\tbar\t9.0\n")); got != '\t' {
		t.Fatalf("expected tab delimiter, got %q", got)
	}
	if got := DetectCSVDelimiter([]byte("id;label;zip\n1;hello;00123\n2;world;04567\n")); got != ';' {
		t.Fatalf("expected semicolon delimiter, got %q", got)
	}

	types := InferColumnTypes([][]string{
		{"1", "12.5", "00123", "alpha", ""},
		{"-42", "7", "90210", "beta", ""},
	})
	want := []string{"INTEGER", "REAL", "TEXT", "TEXT", "TEXT"}
	if len(types) != len(want) {
		t.Fatalf("InferColumnTypes len = %d, want %d", len(types), len(want))
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("col %d type = %q, want %q", i, types[i], want[i])
		}
	}
}

func TestImportCSVStreamAndBatches(t *testing.T) {
	db, err := sqlite3.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var sb strings.Builder
	sb.WriteString("\xEF\xBB\xBFid;name;name;score;zip\n")
	for i := 1; i <= 25; i++ {
		if i == 10 {
			sb.WriteString("10;\"semi;colon\";dup;;00999\n")
			continue
		}
		sb.WriteString(strings.ReplaceAll(strings.ReplaceAll(
			"{ID};user_{ID};alt_{ID};{ID}.5;00{ID}\n",
			"{ID}", itoa(i)), "\r", ""))
	}

	opt := DefaultImportOptions("users")
	opt.BatchSize = 7
	res, err := ImportCSV(context.Background(), db, strings.NewReader(sb.String()), opt)
	if err != nil {
		t.Fatalf("ImportCSV failed: %v", err)
	}
	if res.Comma != ';' || res.Rows != 25 {
		t.Fatalf("unexpected ImportResult: %+v", res)
	}
	if len(res.Columns) != 5 || res.Columns[2] != "name_2" {
		t.Fatalf("expected deduplicated headers, got %+v", res.Columns)
	}
	if res.Types[0] != "INTEGER" || res.Types[3] != "REAL" || res.Types[4] != "TEXT" {
		t.Fatalf("unexpected inferred types: %+v", res.Types)
	}

	// Append additional rows into existing table.
	appendOpt := DefaultImportOptions("users")
	appendOpt.Append = true
	appendOpt.HasHeader = true
	res2, err := ImportCSV(context.Background(), db, strings.NewReader("id;name;name;score;zip\n26;extra;more;4.25;00026\n"), appendOpt)
	if err != nil || res2.Rows != 1 {
		t.Fatalf("ImportCSV append failed: res=%+v err=%v", res2, err)
	}

	stmt, _, err := db.Prepare(`SELECT COUNT(*), MIN(score), MAX(score) FROM "users"`)
	if err != nil {
		t.Fatal(err)
	}
	defer stmt.Close()
	if !stmt.Step() || stmt.ColumnInt64(0) != 26 {
		t.Fatalf("expected 26 rows in users, got %d (err=%v)", stmt.ColumnInt64(0), stmt.Err())
	}

	// Check row 10 NULL score and preserved leading-zero zip + quoted semicolon.
	row10, _, err := db.Prepare(`SELECT name, score, zip FROM "users" WHERE id = 10`)
	if err != nil {
		t.Fatal(err)
	}
	defer row10.Close()
	if !row10.Step() {
		t.Fatalf("row 10 missing: %v", row10.Err())
	}
	if row10.ColumnText(0) != "semi;colon" || row10.ColumnType(1) != sqlite3.NULL || row10.ColumnText(2) != "00999" {
		t.Fatalf("unexpected row 10 values: name=%q scoreType=%v zip=%q", row10.ColumnText(0), row10.ColumnType(1), row10.ColumnText(2))
	}
}

func itoa(n int) string {
	return string(func() []byte {
		if n == 0 {
			return []byte{'0'}
		}
		var b [20]byte
		i := len(b)
		for n > 0 {
			i--
			b[i] = byte('0' + n%10)
			n /= 10
		}
		return b[i:]
	}())
}

