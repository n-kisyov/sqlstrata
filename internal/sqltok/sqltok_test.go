package sqltok

import (
	"slices"
	"strings"
	"testing"

	"github.com/sqlstrata/sqlstrata/internal/engine"
)

func TestTokenize(t *testing.T) {
	src := `SELECT "col""1", [col 2], 'it''s;fine', X'DEADBEEF', 12.5e-2 -- comment;
FROM orders /* block; * comment */ WHERE id >= 42;`
	toks := Tokenize(src)

	var reconstructed strings.Builder
	var kinds []Kind
	for _, tk := range toks {
		reconstructed.WriteString(tk.Text(src))
		if tk.Kind != TokenWhitespace {
			kinds = append(kinds, tk.Kind)
		}
	}
	if reconstructed.String() != src {
		t.Fatalf("tokens do not cover exact source text:\ngot:  %q\nwant: %q", reconstructed.String(), src)
	}

	// Verify key token kinds are classified accurately.
	wantFirst := []Kind{
		TokenKeyword, // SELECT
		TokenIdent,   // "col""1"
		TokenPunct,   // ,
		TokenIdent,   // [col 2]
		TokenPunct,   // ,
		TokenString,  // 'it''s;fine'
		TokenPunct,   // ,
		TokenString,  // X'DEADBEEF'
		TokenPunct,   // ,
		TokenNumber,  // 12.5e-2
		TokenComment, // -- comment;
		TokenKeyword, // FROM
		TokenIdent,   // orders
		TokenComment, // /* block; * comment */
		TokenKeyword, // WHERE
		TokenIdent,   // id
	}
	if len(kinds) < len(wantFirst) {
		t.Fatalf("too few non-whitespace tokens: %v", kinds)
	}
	if !slices.Equal(kinds[:len(wantFirst)], wantFirst) {
		t.Fatalf("token kinds mismatch:\ngot:  %v\nwant: %v", kinds[:len(wantFirst)], wantFirst)
	}
}

func TestSplitAndStreamStatements(t *testing.T) {
	script := `
-- Leading comment
SELECT 'a;b''c;d' AS s;

CREATE TRIGGER tr_audit AFTER UPDATE ON orders
BEGIN
	UPDATE log SET note = CASE WHEN NEW.amount > 0 THEN 'ok;1' ELSE 'bad;2' END;
	INSERT INTO audit(val) VALUES (NEW.id);
END;

/* Trailing comment with ; inside */
DELETE FROM orders WHERE id = 99;
`
	stmts := SplitStatements(script)
	if len(stmts) != 3 {
		t.Fatalf("expected 3 statements, got %d: %+v", len(stmts), stmts)
	}
	if !strings.HasPrefix(stmts[0].SQL, "-- Leading comment\nSELECT") {
		t.Fatalf("unexpected stmt 0: %q", stmts[0].SQL)
	}
	if !strings.Contains(stmts[1].SQL, "CREATE TRIGGER tr_audit") || !strings.HasSuffix(stmts[1].SQL, "END") {
		t.Fatalf("trigger not kept intact: %q", stmts[1].SQL)
	}
	if !strings.HasSuffix(stmts[2].SQL, "WHERE id = 99") {
		t.Fatalf("unexpected stmt 2: %q", stmts[2].SQL)
	}
	if stmts[2].StartLine <= stmts[1].StartLine || stmts[1].StartLine <= stmts[0].StartLine {
		t.Fatalf("expected increasing StartLine across statements: %d, %d, %d",
			stmts[0].StartLine, stmts[1].StartLine, stmts[2].StartLine)
	}
}

func TestCompleteSchemaAndKeywords(t *testing.T) {
	schema := &engine.Schema{
		Objects: []*engine.Object{
			{
				Type: engine.TypeTable,
				Name: "orders",
				Columns: []engine.Column{
					{Name: "order_id"},
					{Name: "origin_country"},
				},
			},
			{Type: engine.TypeTable, Name: "order_items"},
		},
	}

	prefix, items := Complete("SELECT * FROM ord", len("SELECT * FROM ord"), schema, 8)
	if prefix != "ord" {
		t.Fatalf("got prefix %q, want 'ord'", prefix)
	}
	wantSchema := []string{"orders", "order_id", "order_items"}
	for _, w := range wantSchema {
		if !slices.Contains(items, w) {
			t.Fatalf("expected completion %q in %v", w, items)
		}
	}
	// Keyword "ORDER" should appear after schema matches.
	if !slices.Contains(items, "ORDER") {
		t.Fatalf("expected keyword ORDER in %v", items)
	}
	if slices.Index(items, "orders") > slices.Index(items, "ORDER") {
		t.Fatalf("expected schema object 'orders' ranked before keyword 'ORDER': %v", items)
	}
}

