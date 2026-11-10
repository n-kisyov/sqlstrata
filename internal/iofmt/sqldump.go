package iofmt

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"

	"github.com/ncruces/go-sqlite3"
)

// DumpDatabaseSQL streams the entire database schema (tables, indexes, views,
// triggers) and all table rows as a transactional SQL script without buffering
// whole tables in memory. Returns the total number of table rows dumped.
func DumpDatabaseSQL(ctx context.Context, db *sqlite3.Conn, w io.Writer) (int64, error) {
	if db == nil {
		return 0, errors.New("database is nil")
	}
	bw := bufio.NewWriterSize(w, 64*1024)

	type schemaEntry struct {
		typ  string
		name string
		sql  string
	}

	var tables []schemaEntry
	var others []schemaEntry

	qStmt, _, err := db.Prepare(`
		SELECT type, name, sql
		FROM sqlite_schema
		WHERE sql IS NOT NULL
		  AND name NOT LIKE 'sqlite_%'
		ORDER BY
			CASE type
				WHEN 'table' THEN 0
				WHEN 'index' THEN 1
				WHEN 'view' THEN 2
				WHEN 'trigger' THEN 3
				ELSE 4
			END,
			rowid`)
	if err != nil {
		return 0, fmt.Errorf("read sqlite_schema: %w", err)
	}
	for qStmt.Step() {
		e := schemaEntry{
			typ:  qStmt.ColumnText(0),
			name: qStmt.ColumnText(1),
			sql:  qStmt.ColumnText(2),
		}
		if e.typ == "table" {
			tables = append(tables, e)
		} else {
			others = append(others, e)
		}
	}
	if err := qStmt.Err(); err != nil {
		_ = qStmt.Close()
		return 0, err
	}
	_ = qStmt.Close()

	if _, err := bw.WriteString("-- SQLStrata streaming SQL dump\nPRAGMA foreign_keys=OFF;\nBEGIN TRANSACTION;\n"); err != nil {
		return 0, err
	}

	var totalRows int64
	for _, tbl := range tables {
		if ctx.Err() != nil {
			return totalRows, ctx.Err()
		}
		if _, err := bw.WriteString("\n" + ensureSemicolon(tbl.sql) + "\n"); err != nil {
			return totalRows, err
		}
		rows, err := dumpTableRows(ctx, db, tbl.name, bw)
		totalRows += rows
		if err != nil {
			return totalRows, err
		}
	}

	if len(others) > 0 {
		if err := bw.WriteByte('\n'); err != nil {
			return totalRows, err
		}
		for _, obj := range others {
			if ctx.Err() != nil {
				return totalRows, ctx.Err()
			}
			if _, err := bw.WriteString(ensureSemicolon(obj.sql) + "\n"); err != nil {
				return totalRows, err
			}
		}
	}

	if _, err := bw.WriteString("COMMIT;\nPRAGMA foreign_keys=ON;\n"); err != nil {
		return totalRows, err
	}
	return totalRows, bw.Flush()
}

func dumpTableRows(ctx context.Context, db *sqlite3.Conn, table string, bw *bufio.Writer) (int64, error) {
	colStmt, _, err := db.Prepare("PRAGMA table_xinfo(" + quoteIdent(table) + ")")
	if err != nil {
		return 0, fmt.Errorf("inspect %s columns: %w", table, err)
	}
	var cols []string
	for colStmt.Step() {
		// PRAGMA table_xinfo columns: 0=cid, 1=name, 2=type, 3=notnull, 4=dflt_value, 5=pk, 6=hidden.
		// Skip hidden/generated columns (hidden != 0) because SQLite forbids
		// INSERT into VIRTUAL or STORED generated columns.
		if colStmt.ColumnInt(6) != 0 {
			continue
		}
		cols = append(cols, colStmt.ColumnText(1))
	}
	if err := colStmt.Err(); err != nil {
		_ = colStmt.Close()
		return 0, err
	}
	_ = colStmt.Close()
	if len(cols) == 0 {
		return 0, nil
	}

	var selB strings.Builder
	selB.WriteString("SELECT ")
	for i, c := range cols {
		if i > 0 {
			selB.WriteString(", ")
		}
		selB.WriteString(quoteIdent(c))
	}
	selB.WriteString(" FROM ")
	selB.WriteString(quoteIdent(table))

	var insPrefixB strings.Builder
	insPrefixB.WriteString("INSERT INTO ")
	insPrefixB.WriteString(quoteIdent(table))
	insPrefixB.WriteString(" (")
	for i, c := range cols {
		if i > 0 {
			insPrefixB.WriteString(", ")
		}
		insPrefixB.WriteString(quoteIdent(c))
	}
	insPrefixB.WriteString(") VALUES (")
	insPrefix := insPrefixB.String()

	stmt, _, err := db.Prepare(selB.String())
	if err != nil {
		return 0, fmt.Errorf("select %s: %w", table, err)
	}
	defer stmt.Close()

	var rows int64
	for stmt.Step() {
		if rows%256 == 0 && ctx.Err() != nil {
			return rows, ctx.Err()
		}
		if _, err := bw.WriteString(insPrefix); err != nil {
			return rows, err
		}
		for i := range cols {
			if i > 0 {
				if _, err := bw.WriteString(", "); err != nil {
					return rows, err
				}
			}
			if _, err := bw.WriteString(formatSQLLiteral(stmt, i)); err != nil {
				return rows, err
			}
		}
		if _, err := bw.WriteString(");\n"); err != nil {
			return rows, err
		}
		rows++
	}
	return rows, stmt.Err()
}

func formatSQLLiteral(stmt *sqlite3.Stmt, col int) string {
	switch stmt.ColumnType(col) {
	case sqlite3.NULL:
		return "NULL"
	case sqlite3.INTEGER:
		return string(stmt.ColumnRawText(col))
	case sqlite3.FLOAT:
		f := stmt.ColumnFloat(col)
		if math.IsInf(f, 1) {
			return "9e999"
		}
		if math.IsInf(f, -1) {
			return "-9e999"
		}
		if math.IsNaN(f) {
			return "NULL"
		}
		return string(stmt.ColumnRawText(col))
	case sqlite3.BLOB:
		return sqlite3.Quote(stmt.ColumnRawBlob(col))
	default:
		return sqlite3.Quote(stmt.ColumnText(col))
	}
}

func ensureSemicolon(sql string) string {
	s := strings.TrimSpace(sql)
	if s != "" && !strings.HasSuffix(s, ";") {
		s += ";"
	}
	return s
}
