package engine

import (
	"testing"

	"github.com/ncruces/go-sqlite3"
)

func TestLoadIndexInfoAndRelated(t *testing.T) {
	db, err := sqlite3.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	err = db.Exec(`
		CREATE TABLE orders (
			id INTEGER PRIMARY KEY,
			customer_id INTEGER NOT NULL,
			created_at TEXT NOT NULL,
			status TEXT
		);
		CREATE INDEX orders_created ON orders(created_at DESC);
		CREATE UNIQUE INDEX orders_cust_partial ON orders(customer_id, lower(status)) WHERE status IS NOT NULL;
		CREATE TRIGGER orders_tr AFTER INSERT ON orders BEGIN SELECT 1; END;
	`)
	if err != nil {
		t.Fatal(err)
	}

	schema, err := LoadSchema(db)
	if err != nil {
		t.Fatal(err)
	}

	idx1 := schema.Find("orders_created")
	if idx1 == nil || idx1.Type != TypeIndex {
		t.Fatalf("orders_created not found: %+v", idx1)
	}
	info1, err := LoadIndexInfo(db, idx1)
	if err != nil {
		t.Fatal(err)
	}
	if info1.Unique || info1.Partial {
		t.Fatalf("unexpected flags on orders_created: %+v", info1)
	}
	if len(info1.Columns) < 2 || info1.Columns[0].Name != "created_at" || !info1.Columns[0].Desc || !info1.Columns[0].Key {
		t.Fatalf("unexpected index columns for orders_created: %+v", info1.Columns)
	}
	if info1.Columns[1].CID != -1 || info1.Columns[1].Name != "rowid" || info1.Columns[1].Key {
		t.Fatalf("expected auxiliary rowid column, got %+v", info1.Columns[1])
	}

	idx2 := schema.Find("orders_cust_partial")
	if idx2 == nil {
		t.Fatal("orders_cust_partial not found")
	}
	info2, err := LoadIndexInfo(db, idx2)
	if err != nil {
		t.Fatal(err)
	}
	if !info2.Unique || !info2.Partial {
		t.Fatalf("expected unique & partial flags on orders_cust_partial, got %+v", info2)
	}
	if len(info2.Columns) < 2 || info2.Columns[1].CID != -2 || info2.Columns[1].Name != "(expression)" {
		t.Fatalf("expected expression column at index 1, got %+v", info2.Columns)
	}

	if got := schema.Related("orders", TypeIndex); len(got) != 2 {
		t.Fatalf("expected 2 indexes for orders, got %d", len(got))
	}
	if got := schema.Related("orders", TypeTrigger); len(got) != 1 {
		t.Fatalf("expected 1 trigger for orders, got %d", len(got))
	}
}

