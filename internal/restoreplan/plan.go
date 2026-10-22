// Package restoreplan preflights authenticated logical dumps without database
// access. Only a completely validated plan can enumerate destination statements.
package restoreplan

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"vitess.io/vitess/go/vt/sqlparser"
)

var (
	ErrUnsupported = errors.New("backup SQL contains an unsafe or unsupported import construct; no import plan was produced")
	ErrLimit       = errors.New("backup SQL exceeds the local preflight resource limit; no import plan was produced")
	ErrInput       = errors.New("cannot read authenticated backup SQL for preflight")
	ErrOutput      = errors.New("cannot write or consume the private restore plan")
	ErrOptions     = errors.New("restore preflight requires a supported driver, original application schema, isolated destination, and positive limits")
)

const (
	DefaultMaxInputBytes     = int64(8 << 30)
	DefaultMaxStatementBytes = 4 << 20
	DefaultMaxTables         = 4096
	DefaultMaxStatements     = 1000000
)

const SessionModeSQL = "SET SESSION sql_mode = 'NO_AUTO_VALUE_ON_ZERO,STRICT_ALL_TABLES,NO_ENGINE_SUBSTITUTION'"

var destinationPattern = regexp.MustCompile(`^fm_restore_[a-f0-9]{32}$`)
var optionNamePattern = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

type Options struct {
	Driver            string
	Source            string
	Destination       string
	MaxInputBytes     int64
	MaxStatementBytes int
	MaxTables         int
	MaxStatements     int
}

// Table is local validation metadata, not a control-plane payload.
type Table struct {
	Name string
	Rows int64
}

type View struct {
	Name      string
	Columns   []string
	Collation string
}

type Plan struct {
	statements  []plannedStatement
	tables      []Table
	views       []View
	bytes       int64
	maxBytes    int64
	destination string
	input       io.ReaderAt
	options     Options
	jsonChecks  bool
}

type plannedStatement struct {
	sql    string
	offset int64
	size   int
	digest [32]byte
}

func (p *Plan) String() string   { return "private restore plan" }
func (p *Plan) GoString() string { return p.String() }

func (p *Plan) Tables() []Table { return append([]Table(nil), p.tables...) }

func (p *Plan) HasJSONChecks() bool { return p.jsonChecks }

func (p *Plan) Destination() string {
	if p == nil {
		return ""
	}
	return p.destination
}

func (p *Plan) Views() []View {
	views := append([]View(nil), p.views...)
	for i := range views {
		views[i].Columns = append([]string(nil), views[i].Columns...)
	}
	return views
}

func (p *Plan) add(statements ...string) error {
	for _, statement := range statements {
		p.bytes += int64(len(statement))
		if p.bytes > p.maxBytes {
			return ErrLimit
		}
		p.statements = append(p.statements, plannedStatement{sql: statement})
	}
	return nil
}

// ForEach must only feed a private output or an independently authorized,
// destination-confined executor. SQL and returned callback errors may be secret.
// This package does not establish SQL privileges or authorize database writes.
func (p *Plan) ForEach(ctx context.Context, consume func(string) error) error {
	if p == nil || len(p.statements) == 0 || consume == nil {
		return ErrOutput
	}
	parser, err := sqlparser.New(sqlparser.Options{})
	if err != nil {
		return ErrOutput
	}
	for _, statement := range p.statements {
		if err := ctx.Err(); err != nil {
			return err
		}
		sql := statement.sql
		if statement.size != 0 {
			data := make([]byte, statement.size)
			if _, err := p.input.ReadAt(data, statement.offset); err != nil || sha256.Sum256(data) != statement.digest {
				return ErrInput
			}
			parts, err := split(ctx, string(data), p.options)
			if err != nil || len(parts) != 1 {
				return ErrInput
			}
			node, err := parser.ParseStrictDDL(parts[0])
			if err != nil {
				return ErrInput
			}
			insert, ok := node.(*sqlparser.Insert)
			if !ok || !plainTable(insert.Table) {
				return ErrInput
			}
			name := insert.Table.Expr.(sqlparser.TableName)
			name.Qualifier = sqlparser.NewIdentifierCS(p.destination)
			insert.Table.Expr = name
			sql = sqlparser.String(insert)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := consume(sql); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return &safeError{ErrOutput, err}
		}
	}
	return nil
}

type builder struct {
	options             Options
	database            *sqlparser.CreateDatabase
	used                bool
	tables              map[string]*tablePlan
	dropped             map[string]bool
	bytes               int64
	views               map[string]*viewPlan
	stubs               map[string]*viewStub
	viewDrops           map[string]bool
	viewCollation       string
	jsonChecks          bool
	input               io.ReaderAt
	current             plannedStatement
	autocommitBlock     bool
	autocommitCommitted bool
}

type tablePlan struct {
	create  *sqlparser.CreateTable
	data    []plannedStatement
	rows    int64
	parents map[string]bool
}

func Prepare(ctx context.Context, input io.Reader, options Options) (*Plan, error) {
	return prepare(ctx, input, nil, options)
}

// PrepareFile retains bounded, hashed references to row statements instead of
// keeping their SQL in memory. The authenticated private file must remain open
// and unchanged until the returned plan has been consumed.
func PrepareFile(ctx context.Context, input *os.File, options Options) (*Plan, error) {
	if input == nil {
		return nil, ErrOptions
	}
	if _, err := input.Seek(0, io.SeekStart); err != nil {
		return nil, ErrInput
	}
	return prepare(ctx, input, input, options)
}

func prepare(ctx context.Context, input io.Reader, backing io.ReaderAt, options Options) (*Plan, error) {
	if err := options.defaults(); err != nil || input == nil {
		return nil, ErrOptions
	}
	parser, err := sqlparser.New(sqlparser.Options{})
	if err != nil {
		return nil, ErrUnsupported
	}
	b := &builder{options: options, input: backing, tables: make(map[string]*tablePlan), dropped: make(map[string]bool),
		views: make(map[string]*viewPlan), stubs: make(map[string]*viewStub), viewDrops: make(map[string]bool)}
	ordinal := 0
	err = scanStatements(ctx, input, options, func(raw string, offset int64) error {
		statements, err := split(ctx, raw, options)
		if err != nil {
			return fmt.Errorf("SQL framing rejected: %w", err)
		}
		for _, statement := range statements {
			ordinal++
			if ordinal > options.MaxStatements {
				return ErrLimit
			}
			node, err := parser.ParseStrictDDL(statement)
			if err != nil {
				// Parser diagnostics contain raw SQL, including customer data.
				return fmt.Errorf("SQL statement %d cannot be parsed completely: %w", ordinal, ErrUnsupported)
			}
			b.current = plannedStatement{offset: offset, size: len(raw)}
			if backing != nil {
				b.current.digest = sha256.Sum256([]byte(raw))
			}
			if err := b.accept(node); err != nil {
				return fmt.Errorf("SQL statement %d (%T) rejected: %w", ordinal, node, err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	plan, err := b.finish()
	if err != nil {
		return nil, fmt.Errorf("SQL dependency validation rejected: %w", err)
	}
	return plan, nil
}

func (o *Options) defaults() error {
	if (o.Driver != "mysql" && o.Driver != "mariadb") || !identifier(o.Source) ||
		!destinationPattern.MatchString(o.Destination) || strings.EqualFold(o.Source, o.Destination) {
		return ErrOptions
	}
	switch strings.ToLower(o.Source) {
	case "mysql", "sys", "information_schema", "performance_schema":
		return ErrOptions
	}
	if o.MaxInputBytes == 0 {
		o.MaxInputBytes = DefaultMaxInputBytes
	}
	if o.MaxStatementBytes == 0 {
		o.MaxStatementBytes = DefaultMaxStatementBytes
	}
	if o.MaxTables == 0 {
		o.MaxTables = DefaultMaxTables
	}
	if o.MaxStatements == 0 {
		o.MaxStatements = DefaultMaxStatements
	}
	if o.MaxInputBytes < 1 || o.MaxInputBytes > DefaultMaxInputBytes || o.MaxStatementBytes < 1 || o.MaxStatementBytes > 64<<20 ||
		o.MaxTables < 1 || o.MaxTables > 10000 || o.MaxStatements < 1 || o.MaxStatements > 1000000 {
		return ErrOptions
	}
	return nil
}

func identifier(name string) bool {
	return name != "" && len(name) <= 64 && utf8.ValidString(name) && !strings.ContainsFunc(name, unicode.IsControl) &&
		!strings.ContainsAny(name, "\\/.")
}

func (b *builder) tableName(name sqlparser.TableName) (string, error) {
	if !b.used || !identifier(name.Name.String()) ||
		(!name.Qualifier.IsEmpty() && name.Qualifier.String() != b.options.Source) {
		return "", ErrUnsupported
	}
	key := strings.ToLower(name.Name.String())
	if known := b.tables[key]; known != nil && known.create.Table.Name.String() != name.Name.String() {
		return "", ErrUnsupported
	}
	if known := b.views[key]; known != nil && known.create.ViewName.Name.String() != name.Name.String() {
		return "", ErrUnsupported
	}
	if known := b.stubs[key]; known != nil && known.name != name.Name.String() {
		return "", ErrUnsupported
	}
	return key, nil
}

func (b *builder) destination(name sqlparser.TableName) sqlparser.TableName {
	name.Qualifier = sqlparser.NewIdentifierCS(b.options.Destination)
	return name
}

func (b *builder) accept(node sqlparser.Statement) error {
	switch node := node.(type) {
	case *sqlparser.CreateDatabase:
		if b.database != nil || b.used || !node.FullyParsed || node.DBName.String() != b.options.Source {
			return ErrUnsupported
		}
		seen := make(map[sqlparser.DatabaseOptionType]bool)
		for _, option := range node.CreateOptions {
			if seen[option.Type] {
				return ErrUnsupported
			}
			seen[option.Type] = true
			switch option.Type {
			case sqlparser.CharacterSetType, sqlparser.CollateType:
				if !optionNamePattern.MatchString(option.Value) {
					return ErrUnsupported
				}
			case sqlparser.EncryptionType:
				if b.options.Driver != "mysql" || !strings.EqualFold(option.Value, "'N'") {
					return ErrUnsupported
				}
			default:
				return ErrUnsupported
			}
		}
		if !seen[sqlparser.CharacterSetType] || !seen[sqlparser.CollateType] {
			return ErrUnsupported
		}
		node.DBName, node.IfNotExists = sqlparser.NewIdentifierCS(b.options.Destination), false
		b.database = node
		return nil
	case *sqlparser.Use:
		if b.database == nil || node.DBName.String() != b.options.Source {
			return ErrUnsupported
		}
		b.used = true
		return nil
	case *sqlparser.CreateTable:
		return b.createTable(node)
	case *sqlparser.CreateView:
		return b.createView(node)
	case *sqlparser.DropView:
		return b.dropView(node)
	case *sqlparser.Insert:
		return b.insert(node)
	case *sqlparser.Set:
		return b.bookkeeping(node)
	case *sqlparser.Commit:
		// MariaDB's per-table dump wrapper is discarded as a complete unit;
		// the executor pins autocommit on and never replays transaction state.
		if !b.autocommitBlock || b.autocommitCommitted {
			return ErrUnsupported
		}
		b.autocommitCommitted = true
		return nil
	case *sqlparser.DropTable:
		return b.dropTable(node)
	case *sqlparser.AlterTable:
		name, err := b.tableName(node.Table)
		if err != nil || b.tables[name] == nil || !node.FullyParsed || len(node.AlterOptions) != 1 || node.PartitionSpec != nil || node.PartitionOption != nil {
			return ErrUnsupported
		}
		if _, ok := node.AlterOptions[0].(*sqlparser.KeyState); !ok {
			return ErrUnsupported
		}
		return nil
	case *sqlparser.LockTables:
		for _, lock := range node.Tables {
			table, ok := lock.Table.(*sqlparser.AliasedTableExpr)
			if !ok || !plainTable(table) {
				return ErrUnsupported
			}
			name, err := b.tableName(table.Expr.(sqlparser.TableName))
			if err != nil || b.tables[name] == nil {
				return ErrUnsupported
			}
		}
		return nil
	case *sqlparser.UnlockTables:
		return nil
	default:
		return ErrUnsupported
	}
}

func (b *builder) finish() (*Plan, error) {
	if b.database == nil || !b.used || len(b.tables) == 0 || len(b.stubs) != 0 || b.autocommitBlock {
		return nil, ErrUnsupported
	}
	for name := range b.dropped {
		if b.tables[name] == nil && b.views[name] == nil {
			return nil, ErrUnsupported
		}
	}
	for name := range b.viewDrops {
		if b.views[name] == nil {
			return nil, ErrUnsupported
		}
	}
	for _, table := range b.tables {
		for _, constraint := range table.create.TableSpec.Constraints {
			fk, ok := constraint.Details.(*sqlparser.ForeignKeyDefinition)
			if !ok {
				continue
			}
			reference := fk.ReferenceDefinition
			parent := b.tables[strings.ToLower(reference.ReferencedTable.Name.String())]
			if parent == nil || parent.create.Table.Name.String() != reference.ReferencedTable.Name.String() {
				return nil, ErrUnsupported
			}
			columns := make(map[string]bool)
			for _, column := range parent.create.TableSpec.Columns {
				columns[column.Name.Lowered()] = true
			}
			for _, column := range reference.ReferencedColumns {
				if !columns[column.Lowered()] {
					return nil, ErrUnsupported
				}
			}
		}
	}
	names := make([]string, 0, len(b.tables))
	for name := range b.tables {
		names = append(names, name)
	}
	sort.Strings(names)
	var ordered []string
	visiting, visited := make(map[string]bool), make(map[string]bool)
	var visit func(string) error
	visit = func(name string) error {
		if visiting[name] || b.tables[name] == nil {
			return ErrUnsupported
		}
		if visited[name] {
			return nil
		}
		visiting[name] = true
		parents := make([]string, 0, len(b.tables[name].parents))
		for parent := range b.tables[name].parents {
			parents = append(parents, parent)
		}
		sort.Strings(parents)
		for _, parent := range parents {
			if err := visit(parent); err != nil {
				return err
			}
		}
		visiting[name], visited[name] = false, true
		ordered = append(ordered, name)
		return nil
	}
	for _, name := range names {
		if err := visit(name); err != nil {
			return nil, err
		}
	}
	plan := &Plan{maxBytes: b.options.MaxInputBytes * 2, destination: b.options.Destination, input: b.input, options: b.options, jsonChecks: b.jsonChecks}
	if err := plan.add(
		SessionModeSQL,
		"SET NAMES utf8mb4", "SET SESSION time_zone = '+00:00'",
		"SET SESSION foreign_key_checks = 1", "SET SESSION unique_checks = 1", sqlparser.String(b.database),
	); err != nil {
		return nil, err
	}
	if b.jsonChecks && b.options.Driver == "mariadb" {
		// MariaDB can inherit disabled CHECK enforcement from the server; don't
		// let a valid constraint silently stop protecting the imported data.
		if err := plan.add("SET SESSION check_constraint_checks = 1"); err != nil {
			return nil, err
		}
	}
	for _, name := range ordered {
		table := b.tables[name]
		if err := plan.add(sqlparser.String(table.create)); err != nil {
			return nil, err
		}
		plan.tables = append(plan.tables, Table{Name: table.create.Table.Name.String(), Rows: table.rows})
	}
	for _, name := range ordered {
		for _, statement := range b.tables[name].data {
			if statement.size == 0 {
				if err := plan.add(statement.sql); err != nil {
					return nil, err
				}
			} else {
				plan.statements = append(plan.statements, statement)
			}
		}
	}
	if b.input != nil {
		plan.bytes += b.bytes
	}
	if plan.bytes > plan.maxBytes {
		return nil, ErrLimit
	}
	if err := b.finishViews(plan); err != nil {
		return nil, err
	}
	return plan, nil
}

type safeError struct {
	message error
	cause   error
}

func (e *safeError) Error() string        { return e.message.Error() }
func (e *safeError) Unwrap() error        { return e.cause }
func (e *safeError) Is(target error) bool { return target == e.message }
