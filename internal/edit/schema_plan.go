package edit

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/ncruces/go-sqlite3"

	"github.com/sqlstrata/sqlstrata/internal/engine"
)

// ColumnDef describes a table column definition for visual schema design and
// ALTER TABLE / 12-step table rebuild planning.
type ColumnDef struct {
	Name          string
	Type          string
	NotNull       bool
	Default       string
	HasDefault    bool
	PK            int  // 0 if not part of primary key; 1..N indicates key order
	AutoIncrement bool // valid only for single INTEGER PRIMARY KEY
}

// TableSpec defines the full desired structure and options of a SQLite table.
type TableSpec struct {
	Name         string
	Columns      []ColumnDef
	WithoutRowid bool
	Strict       bool
}

// AlterPlan is an ordered sequence of SQLite statements transforming an existing
// table into a target TableSpec, either via native ALTER TABLE or the official
// SQLite 12-step rebuild workflow.
type AlterPlan struct {
	Rebuild    bool
	Table      string
	Statements []string
	Summary    string
}

// TableSpecFromObject converts an introspected engine.Object table into a TableSpec.
func TableSpecFromObject(obj *engine.Object) TableSpec {
	if obj == nil {
		return TableSpec{}
	}
	cols := make([]ColumnDef, 0, len(obj.Columns))
	for _, c := range obj.Columns {
		if c.Hidden != 0 {
			continue
		}
		cols = append(cols, ColumnDef{
			Name:       c.Name,
			Type:       c.Type,
			NotNull:    c.NotNull,
			Default:    c.Default,
			HasDefault: c.HasDflt,
			PK:         c.PK,
		})
	}
	return TableSpec{
		Name:         obj.Name,
		Columns:      cols,
		WithoutRowid: obj.WithoutRowid,
		Strict:       obj.Strict,
	}
}

// CreateTableSQL generates a CREATE TABLE statement from spec.
func CreateTableSQL(spec TableSpec) (string, error) {
	name := strings.TrimSpace(spec.Name)
	if name == "" {
		return "", errors.New("table name is required")
	}
	if len(spec.Columns) == 0 {
		return "", errors.New("at least one column is required")
	}

	seen := make(map[string]struct{}, len(spec.Columns))
	var pkCols []ColumnDef
	for i, c := range spec.Columns {
		cn := strings.TrimSpace(c.Name)
		if cn == "" {
			return "", fmt.Errorf("column %d name is required", i+1)
		}
		lower := strings.ToLower(cn)
		if _, dup := seen[lower]; dup {
			return "", fmt.Errorf("duplicate column name %q", cn)
		}
		seen[lower] = struct{}{}
		if c.PK > 0 {
			pkCols = append(pkCols, c)
		}
	}

	sort.SliceStable(pkCols, func(i, j int) bool {
		return pkCols[i].PK < pkCols[j].PK
	})

	singlePKName := ""
	if len(pkCols) == 1 {
		singlePKName = strings.TrimSpace(pkCols[0].Name)
	}

	var b strings.Builder
	b.WriteString("CREATE TABLE ")
	b.WriteString(engine.QuoteIdent(name))
	b.WriteString(" (\n")

	for i, c := range spec.Columns {
		b.WriteString("  ")
		b.WriteString(formatColumnDefSQL(c, strings.TrimSpace(c.Name) == singlePKName))
		if i+1 < len(spec.Columns) || len(pkCols) > 1 {
			b.WriteByte(',')
		}
		b.WriteByte('\n')
	}

	if len(pkCols) > 1 {
		b.WriteString("  PRIMARY KEY (")
		for i, pk := range pkCols {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(engine.QuoteIdent(strings.TrimSpace(pk.Name)))
		}
		b.WriteString(")\n")
	}

	b.WriteByte(')')
	var opts []string
	if spec.WithoutRowid {
		opts = append(opts, "WITHOUT ROWID")
	}
	if spec.Strict {
		opts = append(opts, "STRICT")
	}
	if len(opts) > 0 {
		b.WriteByte(' ')
		b.WriteString(strings.Join(opts, ", "))
	}
	b.WriteByte(';')
	return b.String(), nil
}

func formatColumnDefSQL(c ColumnDef, inlinePK bool) string {
	var parts []string
	parts = append(parts, engine.QuoteIdent(strings.TrimSpace(c.Name)))
	typ := strings.TrimSpace(c.Type)
	if typ != "" {
		parts = append(parts, typ)
	}
	if inlinePK {
		pkClause := "PRIMARY KEY"
		if c.AutoIncrement && strings.EqualFold(typ, "INTEGER") {
			pkClause += " AUTOINCREMENT"
		}
		parts = append(parts, pkClause)
	}
	if c.NotNull {
		parts = append(parts, "NOT NULL")
	}
	if c.HasDefault && strings.TrimSpace(c.Default) != "" {
		parts = append(parts, "DEFAULT "+strings.TrimSpace(c.Default))
	}
	return strings.Join(parts, " ")
}

// DropObjectSQL returns a parameterized-safe DROP statement for a schema object.
func DropObjectSQL(obj *engine.Object) (string, error) {
	if obj == nil || strings.TrimSpace(obj.Name) == "" {
		return "", errors.New("no schema object selected")
	}
	var kw string
	switch obj.Type {
	case engine.TypeTable:
		kw = "TABLE"
	case engine.TypeView:
		kw = "VIEW"
	case engine.TypeIndex:
		kw = "INDEX"
	case engine.TypeTrigger:
		kw = "TRIGGER"
	default:
		return "", fmt.Errorf("unsupported object type %q", obj.Type)
	}
	return fmt.Sprintf("DROP %s IF EXISTS %s;", kw, engine.QuoteIdent(obj.Name)), nil
}

// EmptyTableSQL returns a statement clearing all rows from table.
func EmptyTableSQL(table string) (string, error) {
	table = strings.TrimSpace(table)
	if table == "" {
		return "", errors.New("table name is required")
	}
	return fmt.Sprintf("DELETE FROM %s;", engine.QuoteIdent(table)), nil
}

// RenameTableSQL builds an ALTER TABLE ... RENAME TO statement.
func RenameTableSQL(oldName, newName string) (string, error) {
	oldName = strings.TrimSpace(oldName)
	newName = strings.TrimSpace(newName)
	if oldName == "" || newName == "" {
		return "", errors.New("old and new table names are required")
	}
	return fmt.Sprintf("ALTER TABLE %s RENAME TO %s;", engine.QuoteIdent(oldName), engine.QuoteIdent(newName)), nil
}

// PlanTableAlter determines whether transforming oldObj into newSpec can be done
// with native ALTER TABLE statements or requires SQLite's official 12-step
// table rebuild workflow (preserving existing data, indexes, views, triggers,
// and verifying foreign keys before commit).
func PlanTableAlter(oldObj *engine.Object, newSpec TableSpec, colRenames map[string]string, dependents []*engine.Object) (AlterPlan, error) {
	if oldObj == nil || oldObj.Type != engine.TypeTable {
		return AlterPlan{}, errors.New("source object must be a table")
	}
	if _, err := CreateTableSQL(newSpec); err != nil {
		return AlterPlan{}, err
	}

	oldByLower := make(map[string]engine.Column, len(oldObj.Columns))
	var oldNormalCols []engine.Column
	for _, c := range oldObj.Columns {
		if c.Hidden != 0 {
			return planRebuild(oldObj, newSpec, colRenames, dependents)
		}
		oldByLower[strings.ToLower(c.Name)] = c
		oldNormalCols = append(oldNormalCols, c)
	}

	// Map each new column back to its source old column name if one exists.
	mappedOld := make(map[string]string, len(newSpec.Columns))
	usedOld := make(map[string]bool, len(oldNormalCols))
	for _, nc := range newSpec.Columns {
		srcName := nc.Name
		if colRenames != nil {
			if mapped, ok := colRenames[nc.Name]; ok && mapped != "" {
				srcName = mapped
			}
		}
		if oc, ok := oldByLower[strings.ToLower(srcName)]; ok {
			mappedOld[nc.Name] = oc.Name
			usedOld[strings.ToLower(oc.Name)] = true
		}
	}

	if canNativeAlter(oldObj, oldNormalCols, newSpec, mappedOld, usedOld, dependents) {
		var stmts []string
		curTable := oldObj.Name
		for i, nc := range newSpec.Columns {
			if ocName, ok := mappedOld[nc.Name]; ok {
				if ocName != nc.Name {
					stmts = append(stmts, fmt.Sprintf("ALTER TABLE %s RENAME COLUMN %s TO %s;",
						engine.QuoteIdent(curTable), engine.QuoteIdent(ocName), engine.QuoteIdent(nc.Name)))
				}
				continue
			}
			_ = i
			stmts = append(stmts, fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s;",
				engine.QuoteIdent(curTable), formatColumnDefSQL(nc, false)))
		}
		if newSpec.Name != oldObj.Name {
			stmts = append(stmts, fmt.Sprintf("ALTER TABLE %s RENAME TO %s;",
				engine.QuoteIdent(curTable), engine.QuoteIdent(newSpec.Name)))
		}
		if len(stmts) == 0 {
			return AlterPlan{Rebuild: false, Table: newSpec.Name, Summary: "No structural changes"}, nil
		}
		return AlterPlan{
			Rebuild:    false,
			Table:      newSpec.Name,
			Statements: stmts,
			Summary:    fmt.Sprintf("Native ALTER TABLE (%d statement(s))", len(stmts)),
		}, nil
	}

	return planRebuild(oldObj, newSpec, colRenames, dependents)
}

func canNativeAlter(
	oldObj *engine.Object,
	oldCols []engine.Column,
	newSpec TableSpec,
	mappedOld map[string]string,
	usedOld map[string]bool,
	dependents []*engine.Object,
) bool {
	if newSpec.WithoutRowid != oldObj.WithoutRowid || newSpec.Strict != oldObj.Strict {
		return false
	}
	if newSpec.Name != oldObj.Name && len(dependents) > 0 {
		// Table rename with dependent triggers/views is safer via full verification or explicit check.
		for _, d := range dependents {
			if d.Type == engine.TypeView || d.Type == engine.TypeTrigger {
				return false
			}
		}
	}
	// Dropping or reordering existing columns requires rebuild.
	if len(usedOld) != len(oldCols) || len(newSpec.Columns) < len(oldCols) {
		return false
	}

	oldMap := make(map[string]engine.Column, len(oldCols))
	for _, c := range oldCols {
		oldMap[c.Name] = c
	}

	// First len(oldCols) entries must match oldCols in order (possibly renamed),
	// with identical types, PK flags, NOT NULL, and defaults.
	for i := 0; i < len(oldCols); i++ {
		nc := newSpec.Columns[i]
		srcName, ok := mappedOld[nc.Name]
		if !ok || srcName != oldCols[i].Name {
			return false
		}
		oc := oldCols[i]
		if nc.PK != oc.PK || nc.AutoIncrement || nc.NotNull != oc.NotNull ||
			!strings.EqualFold(strings.TrimSpace(nc.Type), strings.TrimSpace(oc.Type)) ||
			nc.HasDefault != oc.HasDflt ||
			strings.TrimSpace(nc.Default) != strings.TrimSpace(oc.Default) {
			return false
		}
	}

	// Trailing appended columns must follow SQLite ADD COLUMN rules.
	for i := len(oldCols); i < len(newSpec.Columns); i++ {
		nc := newSpec.Columns[i]
		if _, exists := mappedOld[nc.Name]; exists {
			return false
		}
		if nc.PK > 0 || nc.AutoIncrement {
			return false
		}
		if nc.NotNull && (!nc.HasDefault || !isConstantDefault(nc.Default)) {
			return false
		}
		if nc.HasDefault && !isConstantDefault(nc.Default) {
			return false
		}
	}
	return true
}

func isConstantDefault(dflt string) bool {
	u := strings.ToUpper(strings.TrimSpace(dflt))
	if u == "" || u == "CURRENT_TIME" || u == "CURRENT_DATE" || u == "CURRENT_TIMESTAMP" {
		return false
	}
	// Parenthesized expression defaults like (datetime('now')) require table rebuild.
	if strings.HasPrefix(u, "(") {
		return false
	}
	return true
}

func planRebuild(oldObj *engine.Object, newSpec TableSpec, colRenames map[string]string, dependents []*engine.Object) (AlterPlan, error) {
	tmpName := "_sqlstrata_new_" + newSpec.Name
	tmpSpec := newSpec
	tmpSpec.Name = tmpName

	createTmpSQL, err := CreateTableSQL(tmpSpec)
	if err != nil {
		return AlterPlan{}, err
	}

	oldByLower := make(map[string]string, len(oldObj.Columns))
	for _, c := range oldObj.Columns {
		if c.Hidden == 0 {
			oldByLower[strings.ToLower(c.Name)] = c.Name
		}
	}

	var dstCols, srcCols []string
	for _, nc := range newSpec.Columns {
		src := ""
		if colRenames != nil && colRenames[nc.Name] != "" {
			src = colRenames[nc.Name]
		} else {
			src = nc.Name
		}
		if exactOld, ok := oldByLower[strings.ToLower(src)]; ok {
			dstCols = append(dstCols, engine.QuoteIdent(strings.TrimSpace(nc.Name)))
			srcCols = append(srcCols, engine.QuoteIdent(exactOld))
		}
	}

	stmts := make([]string, 0, 6+len(dependents))
	stmts = append(stmts, createTmpSQL)
	if len(dstCols) > 0 {
		stmts = append(stmts, fmt.Sprintf("INSERT INTO %s (%s) SELECT %s FROM %s;",
			engine.QuoteIdent(tmpName),
			strings.Join(dstCols, ", "),
			strings.Join(srcCols, ", "),
			engine.QuoteIdent(oldObj.Name),
		))
	}
	stmts = append(stmts, fmt.Sprintf("DROP TABLE %s;", engine.QuoteIdent(oldObj.Name)))
	stmts = append(stmts, fmt.Sprintf("ALTER TABLE %s RENAME TO %s;",
		engine.QuoteIdent(tmpName), engine.QuoteIdent(newSpec.Name)))

	// Sort dependents so indexes recreate first, then views, then triggers.
	sortedDeps := append([]*engine.Object(nil), dependents...)
	rank := func(t engine.ObjectType) int {
		switch t {
		case engine.TypeIndex:
			return 0
		case engine.TypeView:
			return 1
		case engine.TypeTrigger:
			return 2
		default:
			return 3
		}
	}
	sort.SliceStable(sortedDeps, func(i, j int) bool {
		return rank(sortedDeps[i].Type) < rank(sortedDeps[j].Type)
	})
	for _, dep := range sortedDeps {
		sqlTxt := strings.TrimSpace(dep.SQL)
		if sqlTxt == "" {
			continue // Skip sqlite_autoindex entries recreated by table constraints.
		}
		if !strings.HasSuffix(sqlTxt, ";") {
			sqlTxt += ";"
		}
		stmts = append(stmts, sqlTxt)
	}

	return AlterPlan{
		Rebuild:    true,
		Table:      newSpec.Name,
		Statements: stmts,
		Summary:    fmt.Sprintf("12-Step Rebuild (%d statement(s), %d dependent object(s))", len(stmts), len(sortedDeps)),
	}, nil
}

// ExecuteAlterPlan runs plan atomically on db. When plan.Rebuild is true, it
// temporarily disables foreign_keys outside the transaction, executes the
// 12-step rebuild inside BEGIN IMMEDIATE ... COMMIT, verifies PRAGMA
// foreign_key_check before committing, and restores the prior foreign_keys mode.
func ExecuteAlterPlan(ctx context.Context, db *sqlite3.Conn, plan AlterPlan) error {
	if db == nil {
		return errors.New("database is nil")
	}
	if len(plan.Statements) == 0 {
		return nil
	}

	var prevFK bool
	if plan.Rebuild {
		fkStmt, _, err := db.Prepare("PRAGMA foreign_keys")
		if err != nil {
			return err
		}
		if fkStmt.Step() {
			prevFK = fkStmt.ColumnInt(0) != 0
		}
		_ = fkStmt.Close()
		if err := db.Exec("PRAGMA foreign_keys = OFF"); err != nil {
			return err
		}
		defer func() {
			if prevFK {
				_ = db.Exec("PRAGMA foreign_keys = ON")
			} else {
				_ = db.Exec("PRAGMA foreign_keys = OFF")
			}
		}()
	}

	if err := db.Exec("BEGIN IMMEDIATE"); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed && !db.GetAutocommit() {
			_ = db.Exec("ROLLBACK")
		}
	}()

	for _, sqlText := range plan.Statements {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := db.Exec(sqlText); err != nil {
			return fmt.Errorf("execute %q: %w", sqlText, err)
		}
	}

	if plan.Rebuild {
		chkStmt, _, err := db.Prepare("PRAGMA foreign_key_check")
		if err != nil {
			return fmt.Errorf("foreign_key_check prepare: %w", err)
		}
		var fkErr error
		if chkStmt.Step() {
			fkErr = fmt.Errorf("foreign key violation on table %q rowid %d referencing %q",
				chkStmt.ColumnText(0), chkStmt.ColumnInt64(1), chkStmt.ColumnText(2))
		}
		err = chkStmt.Err()
		_ = chkStmt.Close()
		if fkErr != nil {
			return fkErr
		}
		if err != nil {
			return err
		}
	}

	if err := db.Exec("COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}
