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
	generated := make(map[string]bool)
	var generatedCols []int
	fulltextColumns := make(map[string]bool)
	for i, column := range spec.Columns {
		key := strings.ToLower(column.Name.String())
		if !identifier(column.Name.String()) || columns[key] || !columnType(column.Type) {
			return ErrUnsupported
		}
		columns[key] = true
		if column.Type.Options != nil && column.Type.Options.As != nil {
			generated[key] = true
			generatedCols = append(generatedCols, i)
		}
		switch strings.ToLower(column.Type.Type) {
		case "char", "varchar", "tinytext", "text", "mediumtext", "longtext":
			fulltextColumns[key] = true
		}
	}
	for _, column := range spec.Columns {
		if column.Type.Options == nil || column.Type.Options.As == nil {
			continue
		}
		if err := generatedExpr(column.Type.Options.As, columns, generated, 0); err != nil {
			return err
		}
	}
	for _, index := range spec.Indexes {
		if index.Info == nil || (index.Info.Type != sqlparser.IndexTypePrimary && index.Info.Type != sqlparser.IndexTypeUnique && index.Info.Type != sqlparser.IndexTypeDefault && index.Info.Type != sqlparser.IndexTypeFullText) {
			return ErrUnsupported
		}
		for _, column := range index.Columns {
			if column.Expression != nil || !columns[strings.ToLower(column.Column.String())] {
				return ErrUnsupported
			}
			if index.Info.Type == sqlparser.IndexTypeFullText && (column.Length != nil || !fulltextColumns[column.Column.Lowered()]) {
				return ErrUnsupported
			}
		}
		for _, option := range index.Options {
			if !strings.EqualFold(option.Name, "comment") && !(index.Info.Type != sqlparser.IndexTypeFullText && strings.EqualFold(option.Name, "using") && strings.EqualFold(option.String, "btree")) {
				return ErrUnsupported
			}
		}
	}
	table := &tablePlan{create: node, parents: make(map[string]bool), generatedCols: generatedCols}
	for _, constraint := range spec.Constraints {
		if check, ok := constraint.Details.(*sqlparser.CheckConstraintDefinition); ok {
			if !jsonColumnCheck(check, columns) {
				return ErrUnsupported
			}
			b.jsonChecks = true
			continue
		}
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

func jsonColumnCheck(check *sqlparser.CheckConstraintDefinition, columns map[string]bool) bool {
	function, ok := check.Expr.(*sqlparser.JSONAttributesExpr)
	if !check.Enforced || !ok || function.Type != sqlparser.ValidAttributeType || function.Path != nil {
		return false
	}
	column, ok := function.JSONDoc.(*sqlparser.ColName)
	return ok && column.Qualifier.IsEmpty() && columns[column.Name.Lowered()]
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
	if o.Reference != nil || o.Invisible != nil && *o.Invisible || o.EngineAttribute != nil || o.SecondaryEngineAttribute != nil || o.SRID != nil || o.Format != sqlparser.UnspecifiedFormat {
		return false
	}
	if o.As != nil {
		if o.Default != nil || o.OnUpdate != nil || o.Autoincrement {
			return false
		}
		if o.Storage != sqlparser.VirtualStorage && o.Storage != sqlparser.StoredStorage {
			return false
		}
	} else if o.Storage != sqlparser.VirtualStorage {
		return false
	}
	if o.KeyOpt == sqlparser.ColKeySpatialKey || o.KeyOpt == sqlparser.ColKeyFulltextKey {
		return false
	}
	return (o.Collate == "" || optionNamePattern.MatchString(o.Collate)) && defaultValue(o.Default) && onUpdateValue(o.OnUpdate)
}

func defaultValue(expr sqlparser.Expr) bool {
	if expr == nil || literal(expr) {
		return true
	}
	switch expr := expr.(type) {
	case *sqlparser.CurTimeFuncExpr:
		return expr.Name.Lowered() == "current_timestamp" && expr.Fsp >= 0 && expr.Fsp <= 6
	case *sqlparser.JSONObjectExpr:
		return len(expr.Params) == 0
	case *sqlparser.JSONArrayExpr:
		return len(expr.Params) == 0
	}
	return false
}

func onUpdateValue(expr sqlparser.Expr) bool {
	if expr == nil {
		return true
	}
	if current, ok := expr.(*sqlparser.CurTimeFuncExpr); ok {
		return current.Name.Lowered() == "current_timestamp" && current.Fsp >= 0 && current.Fsp <= 6
	}
	return false
}

func generatedExpr(expr sqlparser.Expr, columns, generated map[string]bool, depth int) error {
	if depth > 128 {
		return ErrLimit
	}
	if literal(expr) {
		return nil
	}
	var children []sqlparser.Expr
	switch expr := expr.(type) {
	case *sqlparser.ColName:
		key := expr.Name.Lowered()
		if !expr.Qualifier.IsEmpty() || !identifier(expr.Name.String()) || !columns[key] || generated[key] {
			return ErrUnsupported
		}
		return nil
	case *sqlparser.BinaryExpr:
		switch expr.Operator {
		case sqlparser.PlusOp, sqlparser.MinusOp, sqlparser.MultOp, sqlparser.DivOp, sqlparser.IntDivOp, sqlparser.ModOp:
		default:
			return ErrUnsupported
		}
		children = []sqlparser.Expr{expr.Left, expr.Right}
	case *sqlparser.ComparisonExpr:
		if expr.Modifier != sqlparser.Missing || expr.Escape != nil || expr.Operator > sqlparser.NullSafeEqualOp {
			return ErrUnsupported
		}
		children = []sqlparser.Expr{expr.Left, expr.Right}
	case *sqlparser.AndExpr:
		children = []sqlparser.Expr{expr.Left, expr.Right}
	case *sqlparser.OrExpr:
		children = []sqlparser.Expr{expr.Left, expr.Right}
	case *sqlparser.NotExpr:
		children = []sqlparser.Expr{expr.Expr}
	case *sqlparser.IsExpr:
		children = []sqlparser.Expr{expr.Left}
	case *sqlparser.UnaryExpr:
		if expr.Operator != sqlparser.UPlusOp && expr.Operator != sqlparser.UMinusOp {
			return ErrUnsupported
		}
		children = []sqlparser.Expr{expr.Expr}
	case *sqlparser.IntroducerExpr:
		if !strings.EqualFold(strings.TrimPrefix(expr.CharacterSet, "_"), "utf8mb4") || !literal(expr.Expr) {
			return ErrUnsupported
		}
		return nil
	case *sqlparser.CollateExpr:
		if !strings.HasPrefix(strings.ToLower(expr.Collation), "utf8mb4_") || !optionNamePattern.MatchString(expr.Collation) {
			return ErrUnsupported
		}
		children = []sqlparser.Expr{expr.Expr}
	case *sqlparser.FuncExpr:
		if !expr.Qualifier.IsEmpty() {
			return ErrUnsupported
		}
		switch expr.Name.Lowered() {
		case "abs", "lower", "upper", "lcase", "ucase", "trim", "ltrim", "rtrim", "char_length", "length":
			if len(expr.Exprs) != 1 {
				return ErrUnsupported
			}
		case "ifnull", "nullif":
			if len(expr.Exprs) != 2 {
				return ErrUnsupported
			}
		case "if":
			if len(expr.Exprs) != 3 {
				return ErrUnsupported
			}
		case "coalesce", "concat", "concat_ws":
			if len(expr.Exprs) < 1 || len(expr.Exprs) > 64 {
				return ErrUnsupported
			}
		default:
			return ErrUnsupported
		}
		children = expr.Exprs
	default:
		return ErrUnsupported
	}
	for _, child := range children {
		if err := generatedExpr(child, columns, generated, depth+1); err != nil {
			return err
		}
	}
	return nil
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
	generatedIndex := make(map[int]bool, len(table.generatedCols))
	for _, idx := range table.generatedCols {
		generatedIndex[idx] = true
	}
	expectedCount := len(columns)
	if len(node.Columns) != 0 {
		expectedColumns := columns
		if len(table.generatedCols) > 0 && len(node.Columns) == len(columns)-len(table.generatedCols) {
			expectedColumns = make([]*sqlparser.ColumnDefinition, 0, len(node.Columns))
			for i, col := range columns {
				if !generatedIndex[i] {
					expectedColumns = append(expectedColumns, col)
				}
			}
		}
		if len(node.Columns) != len(expectedColumns) {
			return ErrUnsupported
		}
		for i, column := range node.Columns {
			if !column.Equal(expectedColumns[i].Name) {
				return ErrUnsupported
			}
		}
		expectedCount = len(expectedColumns)
	}
	rewriteGenerated := len(node.Columns) == 0 && len(table.generatedCols) > 0
	for _, row := range rows {
		if len(row) != expectedCount {
			return ErrUnsupported
		}
		for i, expr := range row {
			if rewriteGenerated && generatedIndex[i] {
				if def, ok := expr.(*sqlparser.Default); ok && def.ColName == "" {
					continue
				}
				if !literal(expr) {
					return ErrUnsupported
				}
				continue
			}
			if !literal(expr) {
				return ErrUnsupported
			}
		}
	}
	if rewriteGenerated {
		rewriteGeneratedInsertDefaults(node, table.generatedCols)
	}
	node.Table.Expr = b.destination(node.Table.Expr.(sqlparser.TableName))
	statement := sqlparser.String(node)
	b.bytes += int64(len(statement))
	if b.bytes > b.options.MaxInputBytes*2 {
		return ErrLimit
	}
	table.rows += int64(len(rows))
	if b.input != nil {
		planned := b.current
		if rewriteGenerated {
			planned.generatedCols = table.generatedCols
		}
		table.data = append(table.data, planned)
	} else {
		table.data = append(table.data, plannedStatement{sql: statement})
	}
	return nil
}

// MariaDB dumps emit computed literals in positional INSERTs for generated
// columns, which MariaDB rejects under STRICT_ALL_TABLES unless replaced with DEFAULT.
func rewriteGeneratedInsertDefaults(insert *sqlparser.Insert, generatedCols []int) {
	if len(generatedCols) == 0 || len(insert.Columns) != 0 {
		return
	}
	rows, ok := insert.Rows.(sqlparser.Values)
	if !ok {
		return
	}
	for _, row := range rows {
		for _, idx := range generatedCols {
			if idx >= 0 && idx < len(row) {
				row[idx] = &sqlparser.Default{}
			}
		}
	}
}

// normalizeCreateTableDDL adapts two MariaDB-only column syntax forms so the
// strict MySQL DDL parser can parse them without bypassing any allowlist check:
//   - unparenthesized empty DEFAULT json_object() / json_array()
//   - inline column CHECK (...) following DEFAULT ...
func normalizeCreateTableDDL(statement string) string {
	trimmed := strings.TrimSpace(statement)
	if len(trimmed) < len("CREATE TABLE ") || !strings.EqualFold(trimmed[:len("CREATE TABLE ")], "CREATE TABLE ") {
		return statement
	}

	var out strings.Builder
	out.Grow(len(statement) + 16)
	depth := 0
	var quote byte
	inConstraintClause := false
	seenDefaultInColumn := false

	for i := 0; i < len(statement); {
		c := statement[i]
		if quote != 0 {
			out.WriteByte(c)
			if c == '\\' && quote != '`' && i+1 < len(statement) {
				i++
				out.WriteByte(statement[i])
			} else if c == quote {
				if i+1 < len(statement) && statement[i+1] == quote {
					i++
					out.WriteByte(statement[i])
				} else {
					quote = 0
				}
			}
			i++
			continue
		}
		if c == '\'' || c == '"' || c == '`' {
			quote = c
			out.WriteByte(c)
			i++
			continue
		}
		if c == '(' {
			depth++
			if depth == 1 {
				inConstraintClause = false
				seenDefaultInColumn = false
			}
			out.WriteByte(c)
			i++
			continue
		}
		if c == ')' {
			if depth > 0 {
				depth--
			}
			out.WriteByte(c)
			i++
			continue
		}
		if c == ',' && depth == 1 {
			inConstraintClause = false
			seenDefaultInColumn = false
			out.WriteByte(c)
			i++
			continue
		}

		if depth == 1 && isWordStart(c) {
			end := i + 1
			for end < len(statement) && isWordPart(statement[end]) {
				end++
			}
			word := statement[i:end]
			switch {
			case strings.EqualFold(word, "CONSTRAINT"):
				inConstraintClause = true
				out.WriteString(word)
				i = end
				continue
			case strings.EqualFold(word, "DEFAULT"):
				seenDefaultInColumn = true
				out.WriteString(word)
				i = end
				if next, replaced, ok := matchEmptyJSONDefault(statement, i); ok {
					out.WriteString(replaced)
					i = next
				}
				continue
			case strings.EqualFold(word, "CHECK") && seenDefaultInColumn && !inConstraintClause:
				j := skipSpaces(statement, end)
				if j < len(statement) && statement[j] == '(' {
					out.WriteString(", ")
					out.WriteString(word)
					seenDefaultInColumn = false
					i = end
					continue
				}
			}
			out.WriteString(word)
			i = end
			continue
		}

		out.WriteByte(c)
		i++
	}
	return out.String()
}

func matchEmptyJSONDefault(s string, pos int) (int, string, bool) {
	wsEnd := skipSpaces(s, pos)
	if wsEnd == pos || wsEnd >= len(s) {
		return 0, "", false
	}
	for _, fn := range []string{"json_object", "json_array"} {
		if wsEnd+len(fn) <= len(s) && strings.EqualFold(s[wsEnd:wsEnd+len(fn)], fn) {
			afterName := skipSpaces(s, wsEnd+len(fn))
			if afterName < len(s) && s[afterName] == '(' {
				afterOpen := skipSpaces(s, afterName+1)
				if afterOpen < len(s) && s[afterOpen] == ')' {
					afterClose := afterOpen + 1
					if afterClose < len(s) && isWordPart(s[afterClose]) {
						return 0, "", false
					}
					return afterClose, " (" + fn + "())", true
				}
			}
		}
	}
	return 0, "", false
}

func skipSpaces(s string, i int) int {
	for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
		i++
	}
	return i
}

func isWordStart(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || c == '_'
}

func isWordPart(c byte) bool {
	return isWordStart(c) || (c >= '0' && c <= '9')
}
