package iofmt

import (
	"bufio"
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"unicode"

	"github.com/ncruces/go-sqlite3"
)

// ImportOptions configures streaming CSV/TSV import into SQLite.
type ImportOptions struct {
	Table         string
	Comma         rune // 0 auto-detects from {',', '\t', ';', '|'}
	HasHeader     bool // first record provides column names
	Append        bool // insert into existing table instead of creating one
	KeepEmptyText bool // keep "" as empty TEXT instead of SQL NULL on TEXT columns
	BatchSize     int  // rows per transaction (default 5000)
}

// DefaultImportOptions returns sensible defaults for importing a CSV/TSV into table.
func DefaultImportOptions(table string) ImportOptions {
	return ImportOptions{
		Table:     table,
		HasHeader: true,
		BatchSize: 5000,
	}
}

// ImportResult reports the schema and row count produced by ImportCSV.
type ImportResult struct {
	Table   string
	Rows    int64
	Columns []string
	Types   []string
	Comma   rune
}

// DetectCSVDelimiter examines a sample of CSV/TSV text and selects the delimiter
// among {',', '\t', ';', '|'} with the most consistent multi-column split.
func DetectCSVDelimiter(sample []byte) rune {
	sample = bytes.TrimPrefix(sample, []byte{0xEF, 0xBB, 0xBF})
	if len(sample) == 0 {
		return ','
	}

	candidates := []rune{',', '\t', ';', '|'}
	bestDelim := ','
	bestScore := -1

	lines := splitSampleLines(string(sample), 25)
	if len(lines) == 0 {
		return ','
	}

	for _, delim := range candidates {
		counts := make([]int, 0, len(lines))
		for _, line := range lines {
			counts = append(counts, countCSVFields(line, delim))
		}
		if len(counts) == 0 || counts[0] <= 1 {
			continue
		}
		first := counts[0]
		consistent := true
		for _, c := range counts[1:] {
			if c != first {
				consistent = false
				break
			}
		}
		score := first * 10
		if consistent {
			score += 1000
		}
		// Tie-breaker prefers ',' then '\t' then ';' then '|' by candidate order.
		if score > bestScore {
			bestScore = score
			bestDelim = delim
		}
	}
	return bestDelim
}

func splitSampleLines(s string, maxLines int) []string {
	var out []string
	inQuote := false
	start := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			if inQuote && i+1 < len(s) && s[i+1] == '"' {
				i++
				continue
			}
			inQuote = !inQuote
		case '\n':
			if !inQuote {
				line := strings.TrimRight(s[start:i], "\r")
				if strings.TrimSpace(line) != "" {
					out = append(out, line)
					if len(out) >= maxLines {
						return out
					}
				}
				start = i + 1
			}
		}
	}
	if start < len(s) && len(out) < maxLines {
		// Only include trailing line if sample had only 1 line, otherwise it may be truncated mid-Peek.
		rem := strings.TrimRight(s[start:], "\r")
		if len(out) == 0 && strings.TrimSpace(rem) != "" {
			out = append(out, rem)
		}
	}
	return out
}

func countCSVFields(line string, delim rune) int {
	n := 1
	inQuote := false
	for _, r := range line {
		if r == '"' {
			inQuote = !inQuote
			continue
		}
		if !inQuote && r == delim {
			n++
		}
	}
	return n
}

// InferColumnTypes inspects sampled CSV rows and infers SQLite affinities
// ("INTEGER", "REAL", or "TEXT") per column.
func InferColumnTypes(rows [][]string) []string {
	ncols := 0
	for _, r := range rows {
		if len(r) > ncols {
			ncols = len(r)
		}
	}
	types := make([]string, ncols)
	for c := range ncols {
		allEmpty := true
		allInt := true
		allReal := true
		for _, r := range rows {
			if c >= len(r) {
				continue
			}
			s := strings.TrimSpace(r[c])
			if s == "" {
				continue
			}
			allEmpty = false
			if allInt && !looksLikeInteger(s) {
				allInt = false
			}
			if allReal && !looksLikeReal(s) {
				allReal = false
			}
			if !allInt && !allReal {
				break
			}
		}
		switch {
		case allEmpty:
			types[c] = "TEXT"
		case allInt:
			types[c] = "INTEGER"
		case allReal:
			types[c] = "REAL"
		default:
			types[c] = "TEXT"
		}
	}
	return types
}

func looksLikeInteger(s string) bool {
	if s == "" {
		return false
	}
	if s[0] == '+' {
		return false
	}
	digits := s
	if digits[0] == '-' {
		digits = digits[1:]
	}
	if len(digits) == 0 {
		return false
	}
	// Preserve identifiers / ZIP codes like "007" or "00123" as TEXT.
	if len(digits) > 1 && digits[0] == '0' {
		return false
	}
	if strings.ContainsRune(s, '_') {
		return false
	}
	_, err := strconv.ParseInt(s, 10, 64)
	return err == nil
}

func looksLikeReal(s string) bool {
	if s == "" || s[0] == '+' || strings.ContainsRune(s, '_') {
		return false
	}
	digits := s
	if digits[0] == '-' {
		digits = digits[1:]
	}
	if len(digits) == 0 {
		return false
	}
	if len(digits) > 1 && digits[0] == '0' && digits[1] != '.' && digits[1] != 'e' && digits[1] != 'E' {
		return false
	}
	f, err := strconv.ParseFloat(s, 64)
	return err == nil && !math.IsNaN(f) && !math.IsInf(f, 0)
}

// ImportCSV streams delimiter-separated rows from r into db inside opt.Table.
// When opt.Append is false, the destination table is created using sampled column types.
func ImportCSV(ctx context.Context, db *sqlite3.Conn, r io.Reader, opt ImportOptions) (ImportResult, error) {
	if db == nil {
		return ImportResult{}, errors.New("database is nil")
	}
	table := strings.TrimSpace(opt.Table)
	if table == "" {
		return ImportResult{}, errors.New("target table name is required")
	}
	batchSize := opt.BatchSize
	if batchSize <= 0 {
		batchSize = 5000
	}

	br := bufio.NewReaderSize(r, 64*1024)
	// Strip UTF-8 BOM if present.
	if bom, err := br.Peek(3); err == nil && len(bom) == 3 && bom[0] == 0xEF && bom[1] == 0xBB && bom[2] == 0xBF {
		_, _ = br.Discard(3)
	}

	comma := opt.Comma
	if comma == 0 {
		sample, _ := br.Peek(32 * 1024)
		comma = DetectCSVDelimiter(sample)
	}

	cr := csv.NewReader(br)
	cr.Comma = comma
	cr.FieldsPerRecord = -1
	cr.LazyQuotes = true

	var rawHeader []string
	var err error
	if opt.HasHeader {
		rawHeader, err = cr.Read()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return ImportResult{}, errors.New("CSV input is empty")
			}
			return ImportResult{}, fmt.Errorf("read header: %w", err)
		}
	}

	const sampleLimit = 256
	sample := make([][]string, 0, sampleLimit)
	maxCols := len(rawHeader)
	for len(sample) < sampleLimit {
		rec, rerr := cr.Read()
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			return ImportResult{}, fmt.Errorf("read CSV row %d: %w", len(sample)+1, rerr)
		}
		row := make([]string, len(rec))
		copy(row, rec)
		if len(row) > maxCols {
			maxCols = len(row)
		}
		sample = append(sample, row)
	}

	if maxCols == 0 {
		return ImportResult{}, errors.New("CSV input has no columns")
	}

	cols := buildHeaderNames(rawHeader, maxCols, opt.HasHeader)
	var colTypes []string
	if opt.Append {
		existingCols, existingTypes, err := loadExistingColumns(db, table)
		if err != nil {
			return ImportResult{}, err
		}
		if len(existingCols) == 0 {
			return ImportResult{}, fmt.Errorf("target table %q does not exist", table)
		}
		if len(cols) > len(existingCols) {
			cols = cols[:len(existingCols)]
		}
		// Match target table column order when headerless; if header provided, keep target names up to ncols.
		cols = existingCols[:len(cols)]
		colTypes = existingTypes[:len(cols)]
	} else {
		colTypes = InferColumnTypes(sample)
		if len(colTypes) < len(cols) {
			for len(colTypes) < len(cols) {
				colTypes = append(colTypes, "TEXT")
			}
		}
	}

	res := ImportResult{
		Table:   table,
		Columns: cols,
		Types:   colTypes,
		Comma:   comma,
	}

	if err := db.Exec("BEGIN IMMEDIATE"); err != nil {
		return res, err
	}
	committed := false
	defer func() {
		if !committed && !db.GetAutocommit() {
			_ = db.Exec("ROLLBACK")
		}
	}()

	if !opt.Append {
		if err := db.Exec(buildCreateImportTableSQL(table, cols, colTypes)); err != nil {
			return res, fmt.Errorf("create table: %w", err)
		}
	}

	insSQL := buildInsertSQL(table, cols)
	stmt, _, err := db.Prepare(insSQL)
	if err != nil {
		return res, fmt.Errorf("prepare insert: %w", err)
	}
	defer stmt.Close()

	inBatch := 0
	insertRow := func(rec []string) error {
		if res.Rows%256 == 0 && ctx.Err() != nil {
			return ctx.Err()
		}
		if err := stmt.Reset(); err != nil {
			return err
		}
		if err := stmt.ClearBindings(); err != nil {
			return err
		}
		for i := range cols {
			val := ""
			if i < len(rec) {
				val = rec[i]
			}
			if err := bindImportedValue(stmt, i+1, val, colTypes[i], opt.KeepEmptyText); err != nil {
				return err
			}
		}
		if err := stmt.Exec(); err != nil {
			return err
		}
		res.Rows++
		inBatch++
		if inBatch >= batchSize {
			if err := db.Exec("COMMIT"); err != nil {
				return err
			}
			if err := db.Exec("BEGIN IMMEDIATE"); err != nil {
				return err
			}
			inBatch = 0
		}
		return nil
	}

	for _, rec := range sample {
		if err := insertRow(rec); err != nil {
			return res, fmt.Errorf("insert row %d: %w", res.Rows+1, err)
		}
	}
	cr.ReuseRecord = true
	for {
		rec, rerr := cr.Read()
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			return res, fmt.Errorf("read row %d: %w", res.Rows+1, rerr)
		}
		if err := insertRow(rec); err != nil {
			return res, fmt.Errorf("insert row %d: %w", res.Rows+1, err)
		}
	}

	if err := db.Exec("COMMIT"); err != nil {
		return res, err
	}
	committed = true
	return res, nil
}

func bindImportedValue(stmt *sqlite3.Stmt, param int, val, colType string, keepEmptyText bool) error {
	trimmed := strings.TrimSpace(val)
	if val == "" {
		if colType == "TEXT" && keepEmptyText {
			return stmt.BindText(param, "")
		}
		return stmt.BindNull(param)
	}
	switch colType {
	case "INTEGER":
		if n, err := strconv.ParseInt(trimmed, 10, 64); err == nil && looksLikeInteger(trimmed) {
			return stmt.BindInt64(param, n)
		}
		return stmt.BindText(param, val)
	case "REAL":
		if f, err := strconv.ParseFloat(trimmed, 64); err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) {
			return stmt.BindFloat(param, f)
		}
		return stmt.BindText(param, val)
	default:
		return stmt.BindText(param, val)
	}
}

func buildHeaderNames(raw []string, ncols int, hasHeader bool) []string {
	out := make([]string, ncols)
	seen := make(map[string]int, ncols)
	for i := range ncols {
		name := ""
		if hasHeader && i < len(raw) {
			name = sanitizeColName(raw[i])
		}
		if name == "" {
			name = fmt.Sprintf("col_%d", i+1)
		}
		lower := strings.ToLower(name)
		if cnt := seen[lower]; cnt > 0 {
			seen[lower] = cnt + 1
			name = fmt.Sprintf("%s_%d", name, cnt+1)
			seen[strings.ToLower(name)] = 1
		} else {
			seen[lower] = 1
		}
		out[i] = name
	}
	return out
}

func sanitizeColName(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range s {
		if r == 0 || unicode.IsControl(r) {
			continue
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

func loadExistingColumns(db *sqlite3.Conn, table string) (names, types []string, err error) {
	stmt, _, err := db.Prepare("PRAGMA table_xinfo(" + quoteIdent(table) + ")")
	if err != nil {
		return nil, nil, err
	}
	defer stmt.Close()
	for stmt.Step() {
		// PRAGMA table_xinfo columns: 0=cid, 1=name, 2=type, 3=notnull, 4=dflt_value, 5=pk, 6=hidden
		hidden := stmt.ColumnInt(6)
		if hidden != 0 {
			continue
		}
		names = append(names, stmt.ColumnText(1))
		types = append(types, normalizeAffinity(stmt.ColumnText(2)))
	}
	return names, types, stmt.Err()
}

func normalizeAffinity(decl string) string {
	u := strings.ToUpper(decl)
	switch {
	case strings.Contains(u, "INT"):
		return "INTEGER"
	case strings.Contains(u, "REAL") || strings.Contains(u, "FLOA") || strings.Contains(u, "DOUB"):
		return "REAL"
	default:
		return "TEXT"
	}
}

func buildCreateImportTableSQL(table string, cols, types []string) string {
	var b strings.Builder
	b.WriteString("CREATE TABLE IF NOT EXISTS ")
	b.WriteString(quoteIdent(table))
	b.WriteString(" (\n")
	for i, c := range cols {
		b.WriteString("  ")
		b.WriteString(quoteIdent(c))
		b.WriteByte(' ')
		b.WriteString(types[i])
		if i+1 < len(cols) {
			b.WriteByte(',')
		}
		b.WriteByte('\n')
	}
	b.WriteString(");")
	return b.String()
}

func buildInsertSQL(table string, cols []string) string {
	var b strings.Builder
	b.WriteString("INSERT INTO ")
	b.WriteString(quoteIdent(table))
	b.WriteString(" (")
	for i, c := range cols {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(quoteIdent(c))
	}
	b.WriteString(") VALUES (")
	for i := range cols {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteByte('?')
		b.WriteString(strconv.Itoa(i + 1))
	}
	b.WriteString(")")
	return b.String()
}

func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
