// Package iofmt provides streaming CSV and JSON export writers that operate on
// SQLite connections or GridSources without buffering full tables in memory.
package iofmt

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"io"
	"strconv"

	"github.com/ncruces/go-sqlite3"

	"github.com/sqlstrata/sqlstrata/internal/data"
)

// ExportQueryCSV streams the rows returned by executing sqlQuery on db to w as
// RFC-4180 CSV (including header row). It interrupts cleanly when ctx cancels.
func ExportQueryCSV(ctx context.Context, db *sqlite3.Conn, sqlQuery string, w io.Writer) (int64, error) {
	stmt, _, err := db.Prepare(sqlQuery)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	ncols := stmt.ColumnCount()
	cw := csv.NewWriter(bufio.NewWriterSize(w, 64*1024))
	hdr := make([]string, ncols)
	for i := range ncols {
		hdr[i] = stmt.ColumnName(i)
	}
	if err := cw.Write(hdr); err != nil {
		return 0, err
	}

	rowBuf := make([]string, ncols)
	var rows int64
	for stmt.Step() {
		if rows%256 == 0 && ctx.Err() != nil {
			return rows, ctx.Err()
		}
		for i := range ncols {
			switch stmt.ColumnType(i) {
			case sqlite3.NULL:
				rowBuf[i] = ""
			case sqlite3.BLOB:
				rowBuf[i] = base64.StdEncoding.EncodeToString(stmt.ColumnRawBlob(i))
			default:
				rowBuf[i] = string(stmt.ColumnRawText(i))
			}
		}
		if err := cw.Write(rowBuf); err != nil {
			return rows, err
		}
		rows++
	}
	if err := stmt.Err(); err != nil {
		return rows, err
	}
	cw.Flush()
	return rows, cw.Error()
}

// ExportQueryJSON streams the rows returned by executing sqlQuery on db to w as
// a JSON array of objects, keeping memory usage flat across millions of rows.
func ExportQueryJSON(ctx context.Context, db *sqlite3.Conn, sqlQuery string, w io.Writer) (int64, error) {
	stmt, _, err := db.Prepare(sqlQuery)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	ncols := stmt.ColumnCount()
	colKeys := make([][]byte, ncols)
	for i := range ncols {
		b, err := json.Marshal(stmt.ColumnName(i))
		if err != nil {
			return 0, err
		}
		colKeys[i] = b
	}

	bw := bufio.NewWriterSize(w, 64*1024)
	if _, err := bw.WriteString("[\n"); err != nil {
		return 0, err
	}
	var rows int64
	for stmt.Step() {
		if rows%256 == 0 && ctx.Err() != nil {
			return rows, ctx.Err()
		}
		if rows > 0 {
			if _, err := bw.WriteString(",\n"); err != nil {
				return rows, err
			}
		}
		if err := bw.WriteByte('{'); err != nil {
			return rows, err
		}
		for i := range ncols {
			if i > 0 {
				if err := bw.WriteByte(','); err != nil {
					return rows, err
				}
			}
			if _, err := bw.Write(colKeys[i]); err != nil {
				return rows, err
			}
			if err := bw.WriteByte(':'); err != nil {
				return rows, err
			}
			switch stmt.ColumnType(i) {
			case sqlite3.NULL:
				_, err = bw.WriteString("null")
			case sqlite3.INTEGER:
				raw := string(stmt.ColumnRawText(i))
				if _, perr := strconv.ParseInt(raw, 10, 64); perr == nil {
					_, err = bw.WriteString(raw)
				} else {
					var enc []byte
					enc, err = json.Marshal(raw)
					if err == nil {
						_, err = bw.Write(enc)
					}
				}
			case sqlite3.FLOAT:
				f := stmt.ColumnFloat(i)
				var enc []byte
				enc, err = json.Marshal(f)
				if err == nil {
					_, err = bw.Write(enc)
				}
			case sqlite3.BLOB:
				var enc []byte
				enc, err = json.Marshal(base64.StdEncoding.EncodeToString(stmt.ColumnRawBlob(i)))
				if err == nil {
					_, err = bw.Write(enc)
				}
			default:
				var enc []byte
				enc, err = json.Marshal(string(stmt.ColumnRawText(i)))
				if err == nil {
					_, err = bw.Write(enc)
				}
			}
			if err != nil {
				return rows, err
			}
		}
		if err := bw.WriteByte('}'); err != nil {
			return rows, err
		}
		rows++
	}
	if err := stmt.Err(); err != nil {
		return rows, err
	}
	if rows > 0 {
		if err := bw.WriteByte('\n'); err != nil {
			return rows, err
		}
	}
	if _, err := bw.WriteString("]\n"); err != nil {
		return rows, err
	}
	return rows, bw.Flush()
}

// ExportStaticCSV exports a loaded StaticSource (e.g. SQL editor results) as CSV.
func ExportStaticCSV(src *data.StaticSource, w io.Writer) (int64, error) {
	if src == nil {
		return 0, nil
	}
	cols := src.Columns()
	nrows := src.RowCount()
	cw := csv.NewWriter(bufio.NewWriterSize(w, 64*1024))
	hdr := make([]string, len(cols))
	for i, c := range cols {
		hdr[i] = c.Name
	}
	if err := cw.Write(hdr); err != nil {
		return 0, err
	}
	row := make([]string, len(cols))
	for r := int64(0); r < nrows; r++ {
		for c := range cols {
			txt, kind, _, _ := src.Cell(r, c)
			if kind == data.KindNull {
				row[c] = ""
			} else {
				row[c] = txt
			}
		}
		if err := cw.Write(row); err != nil {
			return r, err
		}
	}
	cw.Flush()
	return nrows, cw.Error()
}

// ExportStaticJSON exports a loaded StaticSource (e.g. SQL editor results) as JSON.
func ExportStaticJSON(src *data.StaticSource, w io.Writer) (int64, error) {
	if src == nil {
		_, err := io.WriteString(w, "[]\n")
		return 0, err
	}
	cols := src.Columns()
	nrows := src.RowCount()
	colKeys := make([][]byte, len(cols))
	for i, c := range cols {
		b, err := json.Marshal(c.Name)
		if err != nil {
			return 0, err
		}
		colKeys[i] = b
	}

	bw := bufio.NewWriterSize(w, 64*1024)
	if _, err := bw.WriteString("[\n"); err != nil {
		return 0, err
	}
	for r := int64(0); r < nrows; r++ {
		if r > 0 {
			if _, err := bw.WriteString(",\n"); err != nil {
				return r, err
			}
		}
		if err := bw.WriteByte('{'); err != nil {
			return r, err
		}
		for c := range cols {
			if c > 0 {
				if err := bw.WriteByte(','); err != nil {
					return r, err
				}
			}
			if _, err := bw.Write(colKeys[c]); err != nil {
				return r, err
			}
			if err := bw.WriteByte(':'); err != nil {
				return r, err
			}
			txt, kind, _, _ := src.Cell(r, c)
			var err error
			switch kind {
			case data.KindNull:
				_, err = bw.WriteString("null")
			case data.KindInteger:
				if _, perr := strconv.ParseInt(txt, 10, 64); perr == nil {
					_, err = bw.WriteString(txt)
				} else {
					var enc []byte
					enc, err = json.Marshal(txt)
					if err == nil {
						_, err = bw.Write(enc)
					}
				}
			case data.KindReal:
				if f, perr := strconv.ParseFloat(txt, 64); perr == nil {
					var enc []byte
					enc, err = json.Marshal(f)
					if err == nil {
						_, err = bw.Write(enc)
					}
				} else {
					var enc []byte
					enc, err = json.Marshal(txt)
					if err == nil {
						_, err = bw.Write(enc)
					}
				}
			default:
				var enc []byte
				enc, err = json.Marshal(txt)
				if err == nil {
					_, err = bw.Write(enc)
				}
			}
			if err != nil {
				return r, err
			}
		}
		if err := bw.WriteByte('}'); err != nil {
			return r, err
		}
	}
	if nrows > 0 {
		if err := bw.WriteByte('\n'); err != nil {
			return nrows, err
		}
	}
	if _, err := bw.WriteString("]\n"); err != nil {
		return nrows, err
	}
	return nrows, bw.Flush()
}

