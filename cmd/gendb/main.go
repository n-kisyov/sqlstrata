// Command gendb generates large synthetic SQLite databases used for
// benchmarking and manually testing SQLStrata against huge files.
//
//	go run ./cmd/gendb -o testdata/big.db -rows 10000000
package main

import (
	"flag"
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"strings"
	"time"

	"github.com/ncruces/go-sqlite3"
)

func main() {
	out := flag.String("o", "testdata/big.db", "output database path (overwritten)")
	rows := flag.Int64("rows", 1_000_000, "number of rows in the orders table")
	customers := flag.Int64("customers", 100_000, "number of rows in the customers table")
	blobEvery := flag.Int64("blob-every", 1000, "attach a binary payload to every Nth order (0 = never)")
	blobSize := flag.Int("blob-size", 64*1024, "payload size in bytes")
	wide := flag.Int("wide-cols", 120, "number of columns in the wide table (0 = skip)")
	wideRows := flag.Int64("wide-rows", 50_000, "rows in the wide table")
	flag.Parse()

	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		log.Fatal(err)
	}
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		_ = os.Remove(*out + suffix)
	}

	db, err := sqlite3.Open(*out)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	must(db.Exec(`PRAGMA journal_mode=OFF; PRAGMA synchronous=OFF; PRAGMA cache_size=-262144;`))
	must(db.Exec(schema))

	rng := rand.New(rand.NewPCG(1, 2))
	start := time.Now()

	genCustomers(db, rng, *customers)
	genOrders(db, rng, *rows, *customers, *blobEvery, *blobSize)
	genKV(db, rng, 100_000)
	if *wide > 0 {
		genWide(db, rng, *wide, *wideRows)
	}

	log.Printf("creating indexes…")
	must(db.Exec(`CREATE INDEX orders_customer ON orders(customer_id);
	              CREATE INDEX orders_created ON orders(created_at);`))
	must(db.Exec(`ANALYZE;`))

	fi, _ := os.Stat(*out)
	log.Printf("done in %s, file size %.2f GB", time.Since(start).Round(time.Millisecond), float64(fi.Size())/(1<<30))
}

const schema = `
CREATE TABLE customers (
	id         INTEGER PRIMARY KEY,
	name       TEXT NOT NULL,
	email      TEXT UNIQUE,
	country    TEXT,
	created_at TEXT DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE orders (
	id          INTEGER PRIMARY KEY,
	customer_id INTEGER REFERENCES customers(id),
	amount      REAL NOT NULL,
	qty         INTEGER NOT NULL,
	status      TEXT CHECK (status IN ('new','paid','shipped','cancelled')),
	created_at  TEXT NOT NULL,
	note        TEXT,
	payload     BLOB
);
CREATE TABLE kv (
	k TEXT PRIMARY KEY,
	v ANY
) STRICT, WITHOUT ROWID;
CREATE VIEW big_orders AS
	SELECT o.id, c.name, o.amount, o.created_at
	FROM orders o JOIN customers c ON c.id = o.customer_id
	WHERE o.amount > 900;
CREATE TRIGGER orders_no_negative BEFORE INSERT ON orders
	WHEN NEW.amount < 0 BEGIN SELECT RAISE(ABORT, 'negative amount'); END;
`

var (
	firstNames = []string{"Ana", "Boris", "Chen", "Dimitar", "Elena", "Farah", "Georgi", "Hana", "Ivan", "Julia", "Kofi", "Lena", "Mira", "Nikolay", "Olga", "Petar", "Quinn", "Rosa", "Stefan", "Tanya"}
	lastNames  = []string{"Ivanova", "Smith", "Petrov", "Müller", "García", "Kowalski", "Rossi", "Tanaka", "Nguyen", "Dimitrov", "Silva", "Novak"}
	countries  = []string{"BG", "DE", "US", "FR", "JP", "BR", "IN", "PL", "IT", "ES", "GB", "SE"}
	statuses   = []string{"new", "paid", "shipped", "cancelled"}
	words      = strings.Fields("lorem ipsum dolor sit amet consectetur adipiscing elit sed do eiusmod tempor incididunt ut labore et dolore magna aliqua sqlite strata huge database editor")
)

func genCustomers(db *sqlite3.Conn, rng *rand.Rand, n int64) {
	stmt := prepare(db, `INSERT INTO customers(id, name, email, country) VALUES (?,?,?,?)`)
	defer stmt.Close()
	batched(db, "customers", n, func(i int64) {
		name := firstNames[rng.IntN(len(firstNames))] + " " + lastNames[rng.IntN(len(lastNames))]
		must(stmt.BindInt64(1, i+1))
		must(stmt.BindText(2, name))
		must(stmt.BindText(3, fmt.Sprintf("user%d@example.com", i+1)))
		must(stmt.BindText(4, countries[rng.IntN(len(countries))]))
		must(stmt.Exec())
	})
}

func genOrders(db *sqlite3.Conn, rng *rand.Rand, n, customers, blobEvery int64, blobSize int) {
	stmt := prepare(db, `INSERT INTO orders(id, customer_id, amount, qty, status, created_at, note, payload) VALUES (?,?,?,?,?,?,?,?)`)
	defer stmt.Close()
	payload := make([]byte, blobSize)
	base := time.Date(2015, 1, 1, 0, 0, 0, 0, time.UTC)
	var note strings.Builder
	batched(db, "orders", n, func(i int64) {
		must(stmt.BindInt64(1, i+1))
		must(stmt.BindInt64(2, 1+rng.Int64N(customers)))
		must(stmt.BindFloat(3, float64(rng.IntN(100000))/100))
		must(stmt.BindInt64(4, 1+rng.Int64N(20)))
		must(stmt.BindText(5, statuses[rng.IntN(len(statuses))]))
		must(stmt.BindText(6, base.Add(time.Duration(i)*7*time.Second).Format("2006-01-02 15:04:05")))
		if rng.IntN(4) == 0 {
			must(stmt.BindNull(7))
		} else {
			note.Reset()
			for w := 3 + rng.IntN(12); w > 0; w-- {
				note.WriteString(words[rng.IntN(len(words))])
				note.WriteByte(' ')
			}
			must(stmt.BindText(7, note.String()))
		}
		if blobEvery > 0 && i%blobEvery == 0 {
			for j := range payload {
				payload[j] = byte(rng.Uint32())
			}
			must(stmt.BindBlob(8, payload))
		} else {
			must(stmt.BindNull(8))
		}
		must(stmt.Exec())
	})
}

func genKV(db *sqlite3.Conn, rng *rand.Rand, n int64) {
	stmt := prepare(db, `INSERT INTO kv(k, v) VALUES (?,?)`)
	defer stmt.Close()
	batched(db, "kv", n, func(i int64) {
		must(stmt.BindText(1, fmt.Sprintf("key:%08d", i)))
		switch i % 3 {
		case 0:
			must(stmt.BindInt64(2, rng.Int64()))
		case 1:
			must(stmt.BindText(2, fmt.Sprintf(`{"n":%d,"ok":%t}`, i, i%2 == 0)))
		default:
			must(stmt.BindFloat(2, rng.Float64()))
		}
		must(stmt.Exec())
	})
}

func genWide(db *sqlite3.Conn, rng *rand.Rand, cols int, n int64) {
	var ddl, ins strings.Builder
	ddl.WriteString("CREATE TABLE wide (id INTEGER PRIMARY KEY")
	ins.WriteString("INSERT INTO wide VALUES (?")
	for c := range cols {
		fmt.Fprintf(&ddl, ", c%03d", c)
		ins.WriteString(",?")
	}
	ddl.WriteString(")")
	ins.WriteString(")")
	must(db.Exec(ddl.String()))
	stmt := prepare(db, ins.String())
	defer stmt.Close()
	batched(db, "wide", n, func(i int64) {
		must(stmt.BindInt64(1, i+1))
		for c := range cols {
			must(stmt.BindInt64(c+2, rng.Int64N(1_000_000)))
		}
		must(stmt.Exec())
	})
}

// batched runs fn n times inside transactions of 50k rows, logging progress.
func batched(db *sqlite3.Conn, label string, n int64, fn func(i int64)) {
	const batch = 50_000
	start := time.Now()
	last := start
	for i := int64(0); i < n; i++ {
		if i%batch == 0 {
			if i > 0 {
				must(db.Exec("COMMIT"))
			}
			must(db.Exec("BEGIN"))
			if time.Since(last) > 2*time.Second {
				last = time.Now()
				rate := float64(i) / time.Since(start).Seconds()
				log.Printf("%s: %d / %d (%.0f rows/s)", label, i, n, rate)
			}
		}
		fn(i)
	}
	must(db.Exec("COMMIT"))
	log.Printf("%s: %d rows in %s", label, n, time.Since(start).Round(time.Millisecond))
}

func prepare(db *sqlite3.Conn, sql string) *sqlite3.Stmt {
	stmt, _, err := db.Prepare(sql)
	must(err)
	return stmt
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
