package restoreplan

import (
	"strings"

	"vitess.io/vitess/go/vt/sqlparser"
)

func (b *builder) createTable(node *sqlparser.CreateTable) error {
	name, err := b.tableName(node.Table)
	if err != nil || !node.FullyParsed || node.Temp || node.IfNotExists || node.OptLike != nil || node.TableSpec == nil || b.tables[name] != nil || b.views[name] != nil || b.stubs[name] != nil {
		return ErrUnsupported
	}
	if b.objectCount() >= b.options.MaxTables {
		return ErrLimit
	}
	spec := node.TableSpec
	if len(spec.Options) == 0 {
		return b.tableStub(name, node)
	}
	if len(spec.Columns) == 0 || spec.PartitionOption != nil || !tableOptions(spec.Options) {
		return ErrUnsupported
	}
	columns := make(map[string]bool)
	for _, column := range spec.Columns {
		key := strings.ToLower(column.Name.String())
		if !identifier(column.Name.String()) || columns[key] || !columnType(column.Type) {
			return ErrUnsupported
		}
		columns[key] = true
	}
	for _, index := range spec.Indexes {
		if index.Info == nil || (index.Info.Type != sqlparser.IndexTypePrimary && index.Info.Type != sqlparser.IndexTypeUnique && index.Info.Type != sqlparser.IndexTypeDefault) {
			return ErrUnsupported
		}
		for _, column := range index.Columns {
			if column.Expression != nil || !columns[strings.ToLower(column.Column.String())] {
				return ErrUnsupported
			}
		}
		for _, option := range index.Options {
			if !strings.EqualFold(option.Name, "comment") && !(strings.EqualFold(option.Name, "using") && strings.EqualFold(option.String, "btree")) {
				return ErrUnsupported
			}
		}
	}
	table := &tablePlan{create: node, parents: make(map[string]bool)}
	for _, constraint := range spec.Constraints {
		fk, ok := constraint.Details.(*sqlparser.ForeignKeyDefinition)
		if !ok || fk.ReferenceDefinition == nil || len(fk.Source) != len(fk.ReferenceDefinition.ReferencedColumns) || len(fk.Source) == 0 {
			return ErrUnsupported
		}
		for _, column := range fk.Source {
			if !columns[strings.ToLower(column.String())] {
				return ErrUnsupported
			}
		}
		reference := fk.ReferenceDefinition
		parent, err := b.tableName(reference.ReferencedTable)
		if err != nil || reference.Match != sqlparser.DefaultMatch || reference.OnDelete == sqlparser.SetDefault || reference.OnUpdate == sqlparser.SetDefault {
			return ErrUnsupported
		}
		table.parents[parent] = true
		reference.ReferencedTable = b.destination(reference.ReferencedTable)
	}
	node.Table = b.destination(node.Table)
	b.tables[name] = table
	return nil
}

func tableOptions(options sqlparser.TableOptions) bool {
	engine := false
	seen := make(map[string]bool)
	for _, option := range options {
		name := strings.ToLower(option.Name)
		if seen[name] || len(option.Tables) != 0 {
			return false
		}
		seen[name] = true
		switch name {
		case "engine":
			if !strings.EqualFold(option.String, "innodb") {
				return false
			}
			engine = true
		case "charset", "collate":
			if !optionNamePattern.MatchString(option.String) {
				return false
			}
		case "auto_increment":
			if option.Value == nil || option.Value.Type != sqlparser.IntVal {
				return false
			}
		case "comment":
			if option.Value == nil || option.Value.Type != sqlparser.StrVal {
				return false
			}
		case "row_format":
			switch strings.ToLower(option.String) {
			case "dynamic", "compact", "redundant", "compressed":
			default:
				return false
			}
		default:
			return false
		}
	}
	return engine
}

func columnType(column *sqlparser.ColumnType) bool {
	if column == nil {
		return false
	}
	switch strings.ToLower(column.Type) {
	case "tinyint", "smallint", "mediumint", "int", "integer", "bigint", "decimal", "numeric", "float", "double", "real", "bit", "bool", "boolean",
		"date", "datetime", "timestamp", "time", "year", "char", "varchar", "binary", "varbinary",
		"tinyblob", "blob", "mediumblob", "longblob", "tinytext", "text", "mediumtext", "longtext", "enum", "set", "json":
	default:
		return false
	}
	if column.Charset.Name != "" && !optionNamePattern.MatchString(column.Charset.Name) {
		return false
	}
	o := column.Options
	if o == nil {
		return true
	}
	if o.As != nil || o.Reference != nil || o.Invisible != nil && *o.Invisible || o.EngineAttribute != nil || o.SecondaryEngineAttribute != nil || o.SRID != nil || o.Format != sqlparser.UnspecifiedFormat || o.Storage != sqlparser.VirtualStorage {
		return false
	}
	if o.KeyOpt == sqlparser.ColKeySpatialKey || o.KeyOpt == sqlparser.ColKeyFulltextKey {
		return false
	}
	return (o.Collate == "" || optionNamePattern.MatchString(o.Collate)) && defaultValue(o.Default) && defaultValue(o.OnUpdate)
}

func defaultValue(expr sqlparser.Expr) bool {
	if expr == nil || literal(expr) {
		return true
	}
	if current, ok := expr.(*sqlparser.CurTimeFuncExpr); ok {
		return current.Name.Lowered() == "current_timestamp" && current.Fsp >= 0 && current.Fsp <= 6
	}
	return false
}

func literal(expr sqlparser.Expr) bool {
	switch expr := expr.(type) {
	case *sqlparser.Literal:
		switch expr.Type {
		case sqlparser.StrVal, sqlparser.IntVal, sqlparser.DecimalVal, sqlparser.FloatVal, sqlparser.HexNum, sqlparser.HexVal, sqlparser.BitNum:
			return true
		}
	case *sqlparser.NullVal, sqlparser.BoolVal:
		return true
	case *sqlparser.UnaryExpr:
		value, ok := expr.Expr.(*sqlparser.Literal)
		return ok && (expr.Operator == sqlparser.UPlusOp || expr.Operator == sqlparser.UMinusOp) &&
			(value.Type == sqlparser.IntVal || value.Type == sqlparser.DecimalVal || value.Type == sqlparser.FloatVal)
	}
	return false
}

func plainTable(table *sqlparser.AliasedTableExpr) bool {
	if table == nil || !table.As.IsEmpty() || len(table.Hints) != 0 || len(table.Columns) != 0 || len(table.Partitions) != 0 {
		return false
	}
	_, ok := table.Expr.(sqlparser.TableName)
	return ok
}

func (b *builder) insert(node *sqlparser.Insert) error {
	if node.Action != sqlparser.InsertAct || bool(node.Ignore) || node.RowAlias != nil || len(node.OnDup) != 0 || len(node.Partitions) != 0 || !plainTable(node.Table) {
		return ErrUnsupported
	}
	name, err := b.tableName(node.Table.Expr.(sqlparser.TableName))
	table := b.tables[name]
	if err != nil || table == nil {
		return ErrUnsupported
	}
	rows, ok := node.Rows.(sqlparser.Values)
	if !ok || len(rows) == 0 {
		return ErrUnsupported
	}
	columns := table.create.TableSpec.Columns
	if len(node.Columns) != 0 {
		if len(node.Columns) != len(columns) {
			return ErrUnsupported
		}
		for i, column := range node.Columns {
			if !column.Equal(columns[i].Name) {
				return ErrUnsupported
			}
		}
	}
	for _, row := range rows {
		if len(row) != len(columns) {
			return ErrUnsupported
		}
		for _, expr := range row {
			if !literal(expr) {
				return ErrUnsupported
			}
		}
	}
	node.Table.Expr = b.destination(node.Table.Expr.(sqlparser.TableName))
	statement := sqlparser.String(node)
	b.bytes += int64(len(statement))
	if b.bytes > b.options.MaxInputBytes*2 {
		return ErrLimit
	}
	table.rows += int64(len(rows))
	table.data = append(table.data, statement)
	return nil
}
