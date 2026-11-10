package engine

import (
	"fmt"
	"strings"

	"github.com/ncruces/go-sqlite3"
)

// ObjectType is the kind of a schema object.
type ObjectType string

const (
	TypeTable   ObjectType = "table"
	TypeView    ObjectType = "view"
	TypeIndex   ObjectType = "index"
	TypeTrigger ObjectType = "trigger"
)

// Column describes one column of a table or view (PRAGMA table_xinfo).
type Column struct {
	Name    string
	Type    string
	NotNull bool
	Default string // SQL text of the default, "" if none
	HasDflt bool
	PK      int // 1-based position in the primary key, 0 if not part of it
	Hidden  int // 0 normal, 1 hidden (vtab), 2 generated virtual, 3 generated stored
}

// Object is an entry of sqlite_schema with resolved details.
type Object struct {
	Type    ObjectType
	Name    string
	Table   string // owning table for indexes and triggers
	SQL     string
	Columns []Column // tables and views only

	WithoutRowid bool
	Strict       bool
	// RowidAlias is the INTEGER PRIMARY KEY column aliasing rowid, if any.
	RowidAlias string
}

// HasRowid reports whether the object can be paged by rowid.
func (o *Object) HasRowid() bool { return o.Type == TypeTable && !o.WithoutRowid }

// PKColumns returns the primary key columns in key order.
func (o *Object) PKColumns() []string {
	var pk []string
	for n := 1; ; n++ {
		found := false
		for _, c := range o.Columns {
			if c.PK == n {
				pk = append(pk, c.Name)
				found = true
			}
		}
		if !found {
			return pk
		}
	}
}

// Schema is a snapshot of the main database schema.
type Schema struct {
	Objects []*Object
}

// Find returns the object with the given name (case-insensitive), or nil.
func (s *Schema) Find(name string) *Object {
	for _, o := range s.Objects {
		if strings.EqualFold(o.Name, name) {
			return o
		}
	}
	return nil
}

// ByType returns objects of type t in schema order.
func (s *Schema) ByType(t ObjectType) []*Object {
	var out []*Object
	for _, o := range s.Objects {
		if o.Type == t {
			out = append(out, o)
		}
	}
	return out
}

// LoadSchema reads the schema of the main database. It never scans table data,
// so it is fast regardless of file size.
func LoadSchema(db *sqlite3.Conn) (*Schema, error) {
	s := &Schema{}
	stmt, _, err := db.Prepare(`SELECT type, name, tbl_name, coalesce(sql,'') FROM main.sqlite_schema
		WHERE name NOT LIKE 'sqlite_%' ORDER BY CASE type WHEN 'table' THEN 0 WHEN 'view' THEN 1 WHEN 'index' THEN 2 ELSE 3 END, name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	for stmt.Step() {
		s.Objects = append(s.Objects, &Object{
			Type:  ObjectType(stmt.ColumnText(0)),
			Name:  stmt.ColumnText(1),
			Table: stmt.ColumnText(2),
			SQL:   stmt.ColumnText(3),
		})
	}
	if err := stmt.Close(); err != nil {
		return nil, err
	}

	flags, err := tableFlags(db)
	if err != nil {
		return nil, err
	}
	for _, o := range s.Objects {
		if o.Type != TypeTable && o.Type != TypeView {
			continue
		}
		if f, ok := flags[o.Name]; ok {
			o.WithoutRowid, o.Strict = f[0], f[1]
		}
		o.Columns, err = tableColumns(db, o.Name)
		if err != nil {
			return nil, fmt.Errorf("columns of %s: %w", o.Name, err)
		}
		if o.Type == TypeTable && !o.WithoutRowid {
			pk := o.PKColumns()
			if len(pk) == 1 {
				for _, c := range o.Columns {
					if c.Name == pk[0] && strings.EqualFold(c.Type, "INTEGER") {
						o.RowidAlias = c.Name
					}
				}
			}
		}
	}
	return s, nil
}

func tableFlags(db *sqlite3.Conn) (map[string][2]bool, error) {
	out := map[string][2]bool{}
	stmt, _, err := db.Prepare(`SELECT name, wr, strict FROM pragma_table_list WHERE schema = 'main'`)
	if err != nil {
		return nil, err
	}
	for stmt.Step() {
		out[stmt.ColumnText(0)] = [2]bool{stmt.ColumnBool(1), stmt.ColumnBool(2)}
	}
	return out, stmt.Close()
}

func tableColumns(db *sqlite3.Conn, table string) ([]Column, error) {
	stmt, _, err := db.Prepare(`SELECT name, type, "notnull", dflt_value, pk, hidden FROM pragma_table_xinfo(?, 'main') ORDER BY cid`)
	if err != nil {
		return nil, err
	}
	if err := stmt.BindText(1, table); err != nil {
		stmt.Close()
		return nil, err
	}
	var cols []Column
	for stmt.Step() {
		c := Column{
			Name:    stmt.ColumnText(0),
			Type:    stmt.ColumnText(1),
			NotNull: stmt.ColumnBool(2),
			HasDflt: stmt.ColumnType(3) != sqlite3.NULL,
			PK:      stmt.ColumnInt(4),
			Hidden:  stmt.ColumnInt(5),
		}
		if c.HasDflt {
			c.Default = stmt.ColumnText(3)
		}
		if c.Hidden != 1 {
			cols = append(cols, c)
		}
	}
	return cols, stmt.Close()
}

// QuoteIdent quotes an SQL identifier.
func QuoteIdent(name string) string { return sqlite3.QuoteIdentifier(name) }
