// Package sqltok provides a zero-dependency SQLite lexer, a trigger-aware
// statement splitter (supporting streaming io.Readers for huge .sql dumps),
// and a schema-aware completion provider.
package sqltok

import (
	"bufio"
	"io"
	"slices"
	"strings"
	"unicode"

	"github.com/sqlstrata/sqlstrata/internal/engine"
)

// Kind classifies a lexical token.
type Kind uint8

const (
	TokenOther Kind = iota
	TokenWhitespace
	TokenComment
	TokenKeyword
	TokenIdent
	TokenString
	TokenNumber
	TokenPunct
)

// Token is a slice of source text [Start, End) with its lexical Kind.
type Token struct {
	Kind  Kind
	Start int
	End   int
}

// Text returns the token's substring from src.
func (t Token) Text(src string) string { return src[t.Start:t.End] }

// Tokenize lexes src into a slice of Tokens covering [0, len(src)).
func Tokenize(src string) []Token {
	var out []Token
	i, n := 0, len(src)
	for i < n {
		start := i
		ch := src[i]

		// 1. Whitespace.
		if ch == ' ' || ch == '\t' || ch == '\r' || ch == '\n' {
			for i < n && (src[i] == ' ' || src[i] == '\t' || src[i] == '\r' || src[i] == '\n') {
				i++
			}
			out = append(out, Token{Kind: TokenWhitespace, Start: start, End: i})
			continue
		}

		// 2. Line comment: -- ...
		if ch == '-' && i+1 < n && src[i+1] == '-' {
			i += 2
			for i < n && src[i] != '\n' {
				i++
			}
			out = append(out, Token{Kind: TokenComment, Start: start, End: i})
			continue
		}

		// 3. Block comment: /* ... */
		if ch == '/' && i+1 < n && src[i+1] == '*' {
			i += 2
			for i < n {
				if src[i] == '*' && i+1 < n && src[i+1] == '/' {
					i += 2
					break
				}
				i++
			}
			out = append(out, Token{Kind: TokenComment, Start: start, End: i})
			continue
		}

		// 4. String literal ('...') or quoted identifier ("..." or `...` or [...]).
		if ch == '\'' || ch == '"' || ch == '`' {
			quote := ch
			i++
			for i < n {
				if src[i] == quote {
					i++
					if i < n && src[i] == quote {
						i++ // escaped doubled quote
						continue
					}
					break
				}
				i++
			}
			kind := TokenString
			if quote != '\'' {
				kind = TokenIdent
			}
			out = append(out, Token{Kind: kind, Start: start, End: i})
			continue
		}
		if ch == '[' {
			i++
			for i < n && src[i] != ']' {
				i++
			}
			if i < n {
				i++
			}
			out = append(out, Token{Kind: TokenIdent, Start: start, End: i})
			continue
		}

		// 5. Numeric literal (including hex 0x... and floats).
		if isDigit(ch) || (ch == '.' && i+1 < n && isDigit(src[i+1])) {
			i++
			for i < n && (isAlnum(src[i]) || src[i] == '.' || ((src[i] == '+' || src[i] == '-') && i > start && (src[i-1] == 'e' || src[i-1] == 'E'))) {
				i++
			}
			out = append(out, Token{Kind: TokenNumber, Start: start, End: i})
			continue
		}

		// 6. Identifier or keyword (or blob literal X'...').
		if (ch == 'x' || ch == 'X') && i+1 < n && src[i+1] == '\'' {
			i += 2
			for i < n && src[i] != '\'' {
				i++
			}
			if i < n {
				i++
			}
			out = append(out, Token{Kind: TokenString, Start: start, End: i})
			continue
		}
		if isIdentStart(ch) {
			i++
			for i < n && isIdentPart(src[i]) {
				i++
			}
			word := src[start:i]
			kind := TokenIdent
			if IsKeyword(word) {
				kind = TokenKeyword
			}
			out = append(out, Token{Kind: kind, Start: start, End: i})
			continue
		}

		// 7. Punctuation / operators.
		i++
		out = append(out, Token{Kind: TokenPunct, Start: start, End: i})
	}
	return out
}

// Statement represents a single executable SQL statement extracted from a script.
type Statement struct {
	SQL       string
	StartLine int // 1-based line number where the statement begins
}

// SplitStatements splits a multi-statement SQL script into individual statements,
// respecting string literals, comments, and CREATE TRIGGER ... BEGIN ... END blocks.
func SplitStatements(script string) []Statement {
	var out []Statement
	_ = StreamStatements(strings.NewReader(script), func(st Statement) error {
		out = append(out, st)
		return nil
	})
	return out
}

// StreamStatements reads an arbitrarily large SQL stream from r and calls fn
// for each complete statement without loading the whole script into memory.
func StreamStatements(r io.Reader, fn func(Statement) error) error {
	br := bufio.NewReaderSize(r, 64*1024)
	var (
		buf        strings.Builder
		line       = 1
		stmtLine   = 1
		hasTokens  = false
		inTrigger  = false
		beginDepth = 0
		prevWord   string
		wordBuf    strings.Builder
	)

	flushWord := func() {
		if wordBuf.Len() == 0 {
			return
		}
		up := strings.ToUpper(wordBuf.String())
		wordBuf.Reset()
		hasTokens = true
		switch up {
		case "TRIGGER":
			if prevWord == "CREATE" || prevWord == "TEMP" || prevWord == "TEMPORARY" || prevWord == "EXISTS" {
				inTrigger = true
			}
		case "BEGIN":
			if inTrigger {
				beginDepth++
			}
		case "END":
			if inTrigger && beginDepth > 0 {
				beginDepth--
			}
		case "CASE":
			if inTrigger && beginDepth > 0 {
				beginDepth++
			}
		}
		prevWord = up
	}

	emit := func() error {
		s := strings.TrimSpace(buf.String())
		buf.Reset()
		hasTokens = false
		inTrigger = false
		beginDepth = 0
		prevWord = ""
		if s == "" {
			return nil
		}
		return fn(Statement{SQL: s, StartLine: stmtLine})
	}

	for {
		ch, err := br.ReadByte()
		if err == io.EOF {
			flushWord()
			return emit()
		}
		if err != nil {
			return err
		}

		if !hasTokens && wordBuf.Len() == 0 && (ch == ' ' || ch == '\t' || ch == '\r' || ch == '\n') {
			if ch == '\n' {
				line++
			}
			stmtLine = line
			continue
		}

		// Line comment --
		if ch == '-' {
			nxt, perr := br.Peek(1)
			if perr == nil && nxt[0] == '-' {
				flushWord()
				_, _ = br.ReadByte()
				buf.WriteString("--")
				for {
					c, cerr := br.ReadByte()
					if cerr != nil {
						break
					}
					buf.WriteByte(c)
					if c == '\n' {
						line++
						break
					}
				}
				continue
			}
		}

		// Block comment /* ... */
		if ch == '/' {
			nxt, perr := br.Peek(1)
			if perr == nil && nxt[0] == '*' {
				flushWord()
				_, _ = br.ReadByte()
				buf.WriteString("/*")
				for {
					c, cerr := br.ReadByte()
					if cerr != nil {
						break
					}
					buf.WriteByte(c)
					if c == '\n' {
						line++
					}
					if c == '*' {
						p, perr := br.Peek(1)
						if perr == nil && p[0] == '/' {
							_, _ = br.ReadByte()
							buf.WriteByte('/')
							break
						}
					}
				}
				continue
			}
		}

		// Quoted string / identifier.
		if ch == '\'' || ch == '"' || ch == '`' || ch == '[' {
			flushWord()
			hasTokens = true
			closeCh := ch
			if ch == '[' {
				closeCh = ']'
			}
			buf.WriteByte(ch)
			for {
				c, cerr := br.ReadByte()
				if cerr != nil {
					break
				}
				buf.WriteByte(c)
				if c == '\n' {
					line++
				}
				if c == closeCh {
					if closeCh != ']' {
						p, perr := br.Peek(1)
						if perr == nil && p[0] == closeCh {
							_, _ = br.ReadByte()
							buf.WriteByte(closeCh)
							continue
						}
					}
					break
				}
			}
			continue
		}

		if isIdentPart(ch) {
			if !hasTokens && wordBuf.Len() == 0 {
				stmtLine = line
			}
			wordBuf.WriteByte(ch)
			buf.WriteByte(ch)
			continue
		}

		flushWord()
		if ch == '\n' {
			line++
		}
		if ch == ';' && beginDepth == 0 {
			if err := emit(); err != nil {
				return err
			}
			stmtLine = line
			continue
		}
		if ch != ' ' && ch != '\t' && ch != '\r' && ch != '\n' {
			if !hasTokens {
				stmtLine = line
			}
			hasTokens = true
		}
		buf.WriteByte(ch)
	}
}

// Complete returns up to limit autocomplete suggestions for the identifier
// prefix immediately preceding cursorOffset in src.
func Complete(src string, cursorOffset int, schema *engine.Schema, limit int) (prefix string, items []string) {
	if cursorOffset < 0 || cursorOffset > len(src) {
		cursorOffset = len(src)
	}
	start := cursorOffset
	for start > 0 && isIdentPart(src[start-1]) {
		start--
	}
	prefix = src[start:cursorOffset]
	if len(prefix) == 0 {
		return "", nil
	}
	upPrefix := strings.ToUpper(prefix)
	lowPrefix := strings.ToLower(prefix)
	seen := map[string]bool{}

	add := func(cand string) {
		if len(items) >= limit || seen[strings.ToLower(cand)] {
			return
		}
		if strings.EqualFold(cand, prefix) {
			return
		}
		seen[strings.ToLower(cand)] = true
		items = append(items, cand)
	}

	// 1. Schema tables, views, and columns first (highest relevance).
	if schema != nil {
		for _, o := range schema.Objects {
			if strings.HasPrefix(strings.ToLower(o.Name), lowPrefix) {
				add(o.Name)
			}
			for _, c := range o.Columns {
				if strings.HasPrefix(strings.ToLower(c.Name), lowPrefix) {
					add(c.Name)
				}
			}
		}
	}

	// 2. SQLite keywords and built-in functions.
	for _, kw := range sortedKeywords {
		if strings.HasPrefix(kw, upPrefix) {
			add(kw)
		}
	}
	return prefix, items
}

// IsKeyword reports whether w is a SQLite keyword (case-insensitive).
func IsKeyword(w string) bool {
	_, ok := slices.BinarySearch(sortedKeywords, strings.ToUpper(w))
	return ok
}

var sortedKeywords = func() []string {
	kw := []string{
		"ABORT", "ADD", "AFTER", "ALL", "ALTER", "ALWAYS", "ANALYZE", "AND", "ANY", "AS", "ASC",
		"ATTACH", "AUTOINCREMENT", "BEFORE", "BEGIN", "BETWEEN", "BLOB", "BY", "CASCADE", "CASE",
		"CAST", "CHECK", "COALESCE", "COLLATE", "COLUMN", "COMMIT", "CONFLICT", "CONSTRAINT",
		"COUNT", "CREATE", "CROSS", "CURRENT_DATE", "CURRENT_TIME", "CURRENT_TIMESTAMP", "DATABASE",
		"DEFAULT", "DEFERRABLE", "DEFERRED", "DELETE", "DESC", "DETACH", "DISTINCT", "DO", "DROP",
		"EACH", "ELSE", "END", "ESCAPE", "EXCEPT", "EXCLUDE", "EXCLUSIVE", "EXISTS", "EXPLAIN",
		"FAIL", "FILTER", "FIRST", "FOLLOWING", "FOR", "FOREIGN", "FROM", "FULL", "GENERATED",
		"GLOB", "GROUP", "GROUPS", "HAVING", "IF", "IGNORE", "IMMEDIATE", "IN", "INDEX", "INDEXED",
		"INITIALLY", "INNER", "INSERT", "INSTEAD", "INT", "INTEGER", "INTERSECT", "INTO", "IS",
		"ISNULL", "JOIN", "JSON_EXTRACT", "KEY", "LAST", "LEFT", "LENGTH", "LIKE", "LIMIT",
		"LOWER", "MATCH", "MATERIALIZED", "MAX", "MIN", "NATURAL", "NO", "NOT", "NOTHING",
		"NOTNULL", "NULL", "NULLIF", "NULLS", "NUMERIC", "OFFSET", "ON", "OR", "ORDER", "OTHERS",
		"OUTER", "OVER", "PARTITION", "PLAN", "PRAGMA", "PRECEDING", "PRIMARY", "QUERY", "RAISE",
		"RANGE", "REAL", "RECURSIVE", "REFERENCES", "REGEXP", "REINDEX", "RELEASE", "RENAME",
		"REPLACE", "RESTRICT", "RETURNING", "RIGHT", "ROLLBACK", "ROW", "ROWID", "ROWS",
		"SAVEPOINT", "SELECT", "SET", "STRICT", "SUBSTR", "SUM", "TABLE", "TEMP", "TEMPORARY",
		"TEXT", "THEN", "TIES", "TOTAL", "TRANSACTION", "TRIGGER", "TYPEOF", "UNBOUNDED", "UNION",
		"UNIQUE", "UPDATE", "UPPER", "USING", "VACUUM", "VALUES", "VIEW", "VIRTUAL", "WHEN",
		"WHERE", "WINDOW", "WITH", "WITHOUT",
	}
	slices.Sort(kw)
	return slices.Compact(kw)
}()

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

func isIdentStart(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || b == '_' || b >= 0x80
}

func isIdentPart(b byte) bool {
	return isIdentStart(b) || isDigit(b) || b == '$'
}

func isAlnum(b byte) bool {
	return isDigit(b) || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || b == '_' || unicode.IsLetter(rune(b))
}

