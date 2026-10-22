package restoreplan

import (
	"sort"
	"strings"

	"vitess.io/vitess/go/vt/sqlparser"
)

type viewPlan struct {
	create    *sqlparser.CreateView
	columns   []string
	sources   map[string]viewSource
	refs      []*sqlparser.ColName
	collation string
}

type viewSource struct {
	name    string
	alias   string
	aliased bool
}

type viewStub struct {
	name         string
	columns      []string
	table        bool
	tableDropped bool
	viewDropped  bool
}

func (b *builder) objectCount() int { return len(b.tables) + len(b.views) + len(b.stubs) }

func (b *builder) dropTable(node *sqlparser.DropTable) error {
	if node.Temp || !node.IfExists || len(node.FromTables) != 1 {
		return ErrUnsupported
	}
	name, err := b.tableName(node.FromTables[0])
	if err != nil || b.tables[name] != nil || b.views[name] != nil {
		return ErrUnsupported
	}
	if stub := b.stubs[name]; stub != nil {
		if stub.tableDropped || stub.viewDropped {
			return ErrUnsupported
		}
		stub.tableDropped = true
		return nil
	}
	if b.dropped[name] {
		return ErrUnsupported
	}
	b.dropped[name] = true
	return nil
}

func (b *builder) dropView(node *sqlparser.DropView) error {
	if !node.IfExists || len(node.FromTables) != 1 {
		return ErrUnsupported
	}
	name, err := b.tableName(node.FromTables[0])
	if err != nil || b.tables[name] != nil || b.views[name] != nil {
		return ErrUnsupported
	}
	if stub := b.stubs[name]; stub != nil {
		if stub.viewDropped || stub.table && !stub.tableDropped {
			return ErrUnsupported
		}
		stub.viewDropped = true
		return nil
	}
	if !b.dropped[name] || b.viewDrops[name] {
		return ErrUnsupported
	}
	b.viewDrops[name] = true
	return nil
}

// Native dumps use inert stand-ins to break view dependency ordering. They are
// never executed and must be followed by the matching drop/final definition.
func (b *builder) tableStub(name string, node *sqlparser.CreateTable) error {
	spec := node.TableSpec
	if !b.dropped[name] || len(spec.Columns) == 0 || len(spec.Indexes) != 0 || len(spec.Constraints) != 0 || spec.PartitionOption != nil {
		return ErrUnsupported
	}
	stub := &viewStub{name: node.Table.Name.String(), table: true}
	seen := make(map[string]bool)
	for _, column := range spec.Columns {
		if !identifier(column.Name.String()) || seen[column.Name.Lowered()] || !columnType(column.Type) {
			return ErrUnsupported
		}
		o := column.Type.Options
		if o != nil && (o.Default != nil || o.OnUpdate != nil || o.Autoincrement || o.Comment != nil || o.KeyOpt != sqlparser.ColKeyNone) {
			return ErrUnsupported
		}
		seen[column.Name.Lowered()] = true
		stub.columns = append(stub.columns, column.Name.String())
	}
	b.stubs[name] = stub
	return nil
}

func simpleSelect(node *sqlparser.Select) bool {
	return node.With == nil && node.Cache == nil && !node.HighPriority && !node.StraightJoinHint && !node.SQLSmallResult &&
		!node.SQLBigResult && !node.SQLBufferResult && !node.SQLCalcFoundRows && node.GroupBy == nil && node.Having == nil &&
		len(node.Windows) == 0 && len(node.OrderBy) == 0 && node.Limit == nil && node.Lock == sqlparser.NoLock && node.Into == nil
}

func placeholderColumns(node *sqlparser.CreateView) ([]string, bool) {
	query, ok := node.Select.(*sqlparser.Select)
	if !ok || !simpleSelect(query) || query.Distinct || query.Where != nil || node.Definer != nil || node.Security != "" || node.Algorithm != "" || node.CheckOption != "" || len(node.Columns) != 0 {
		return nil, false
	}
	if len(query.From) != 0 {
		if len(query.From) != 1 {
			return nil, false
		}
		from, ok := query.From[0].(*sqlparser.AliasedTableExpr)
		if !ok || !plainTable(from) {
			return nil, false
		}
		table := from.Expr.(sqlparser.TableName)
		if table.Name.String() != "dual" || !table.Qualifier.IsEmpty() {
			return nil, false
		}
	}
	var columns []string
	seen := make(map[string]bool)
	for _, projection := range query.SelectExprs {
		expr, ok := projection.(*sqlparser.AliasedExpr)
		if !ok || !literal(expr.Expr) || !identifier(expr.As.String()) || seen[expr.As.Lowered()] {
			return nil, false
		}
		seen[expr.As.Lowered()] = true
		columns = append(columns, expr.As.String())
	}
	return columns, len(columns) != 0
}

func (b *builder) createView(node *sqlparser.CreateView) error {
	name, err := b.tableName(node.ViewName)
	if err != nil || node.IsReplace || b.tables[name] != nil || b.views[name] != nil {
		return ErrUnsupported
	}
	if columns, placeholder := placeholderColumns(node); placeholder {
		if !b.dropped[name] || !b.viewDrops[name] || b.stubs[name] != nil {
			return ErrUnsupported
		}
		if b.objectCount() >= b.options.MaxTables {
			return ErrLimit
		}
		b.stubs[name] = &viewStub{name: node.ViewName.Name.String(), columns: columns}
		return nil
	}
	if stub := b.stubs[name]; stub != nil && !stub.viewDropped {
		return ErrUnsupported
	}
	if b.stubs[name] == nil && b.objectCount() >= b.options.MaxTables {
		return ErrLimit
	}
	if node.CheckOption != "" || node.Security != "" && !strings.EqualFold(node.Security, "definer") ||
		(node.Algorithm != "" && !strings.EqualFold(node.Algorithm, "undefined") && !strings.EqualFold(node.Algorithm, "merge")) || b.viewCollation == "" {
		return ErrUnsupported
	}
	query, ok := node.Select.(*sqlparser.Select)
	if !ok || !simpleSelect(query) || len(query.From) != 1 {
		return ErrUnsupported
	}
	view := &viewPlan{create: node, sources: make(map[string]viewSource), collation: b.viewCollation}
	context := viewContext{builder: b, view: view}
	if err := context.from(query.From[0], 0); err != nil {
		return err
	}
	if len(node.Columns) != 0 && len(node.Columns) != len(query.SelectExprs) {
		return ErrUnsupported
	}
	seen := make(map[string]bool)
	for i, projection := range query.SelectExprs {
		expr, ok := projection.(*sqlparser.AliasedExpr)
		if !ok {
			return ErrUnsupported
		}
		column := expr.As.String()
		if len(node.Columns) != 0 {
			column = node.Columns[i].String()
		} else if column == "" {
			if ref, ok := expr.Expr.(*sqlparser.ColName); ok {
				column = ref.Name.String()
			}
		}
		if !identifier(column) || seen[strings.ToLower(column)] {
			return ErrUnsupported
		}
		seen[strings.ToLower(column)] = true
		view.columns = append(view.columns, column)
		if err := context.expr(expr.Expr, 0); err != nil {
			return err
		}
	}
	if len(view.columns) == 0 {
		return ErrUnsupported
	}
	if query.Where != nil {
		if err := context.expr(query.Where.Expr, 0); err != nil {
			return err
		}
	}
	if stub := b.stubs[name]; stub != nil {
		if len(stub.columns) != len(view.columns) {
			return ErrUnsupported
		}
		for i, column := range stub.columns {
			if !strings.EqualFold(column, view.columns[i]) {
				return ErrUnsupported
			}
		}
		delete(b.stubs, name)
	}
	// Omitting DEFINER binds the view to the independently restricted import
	// principal. INVOKER is rejected, never used to bypass definer privileges.
	node.Definer, node.Security = nil, "definer"
	node.ViewName = b.destination(node.ViewName)
	b.views[name] = view
	return nil
}

type viewContext struct {
	builder *builder
	view    *viewPlan
}

func (v *viewContext) from(expr sqlparser.TableExpr, depth int) error {
	if depth > 32 {
		return ErrLimit
	}
	switch expr := expr.(type) {
	case *sqlparser.ParenTableExpr:
		if len(expr.Exprs) != 1 {
			return ErrUnsupported
		}
		return v.from(expr.Exprs[0], depth+1)
	case *sqlparser.AliasedTableExpr:
		if len(v.view.sources) >= 8 {
			return ErrLimit
		}
		table, ok := expr.Expr.(sqlparser.TableName)
		if !ok || len(expr.Hints) != 0 || len(expr.Columns) != 0 || len(expr.Partitions) != 0 {
			return ErrUnsupported
		}
		if _, err := v.builder.tableName(table); err != nil {
			return err
		}
		alias := expr.As.String()
		if alias == "" {
			alias = table.Name.String()
		}
		key := strings.ToLower(alias)
		if !identifier(alias) || v.view.sources[key].name != "" {
			return ErrUnsupported
		}
		v.view.sources[key] = viewSource{name: table.Name.String(), alias: alias, aliased: !expr.As.IsEmpty()}
		expr.Expr = v.builder.destination(table)
		return nil
	case *sqlparser.JoinTableExpr:
		if (expr.Join != sqlparser.NormalJoinType && expr.Join != sqlparser.LeftJoinType && expr.Join != sqlparser.RightJoinType) ||
			expr.Condition == nil || expr.Condition.On == nil || len(expr.Condition.Using) != 0 {
			return ErrUnsupported
		}
		if err := v.from(expr.LeftExpr, depth+1); err != nil {
			return err
		}
		if err := v.from(expr.RightExpr, depth+1); err != nil {
			return err
		}
		return v.expr(expr.Condition.On, 0)
	default:
		return ErrUnsupported
	}
}

func (v *viewContext) expr(expr sqlparser.Expr, depth int) error {
	if depth > 128 {
		return ErrLimit
	}
	if literal(expr) {
		return nil
	}
	var children []sqlparser.Expr
	switch expr := expr.(type) {
	case *sqlparser.ColName:
		if !identifier(expr.Name.String()) {
			return ErrUnsupported
		}
		if !expr.Qualifier.IsEmpty() {
			source := v.view.sources[strings.ToLower(expr.Qualifier.Name.String())]
			if source.name == "" || source.alias != expr.Qualifier.Name.String() {
				return ErrUnsupported
			}
			if !expr.Qualifier.Qualifier.IsEmpty() {
				if source.aliased || expr.Qualifier.Qualifier.String() != v.builder.options.Source {
					return ErrUnsupported
				}
				expr.Qualifier.Qualifier = sqlparser.NewIdentifierCS(v.builder.options.Destination)
			}
		}
		v.view.refs = append(v.view.refs, expr)
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
	default:
		return ErrUnsupported
	}
	for _, child := range children {
		if err := v.expr(child, depth+1); err != nil {
			return err
		}
	}
	return nil
}

func (b *builder) sourceColumns(source viewSource) ([]string, error) {
	name := strings.ToLower(source.name)
	if table := b.tables[name]; table != nil && table.create.Table.Name.String() == source.name {
		var columns []string
		for _, column := range table.create.TableSpec.Columns {
			columns = append(columns, column.Name.String())
		}
		return columns, nil
	}
	if view := b.views[name]; view != nil && view.create.ViewName.Name.String() == source.name {
		return view.columns, nil
	}
	return nil, ErrUnsupported
}

func (b *builder) finishViews(plan *Plan) error {
	names := make([]string, 0, len(b.views))
	for name := range b.views {
		names = append(names, name)
	}
	sort.Strings(names)
	visited, visiting := make(map[string]bool), make(map[string]bool)
	var visit func(string) error
	visit = func(name string) error {
		if visiting[name] {
			return ErrUnsupported
		}
		if visited[name] {
			return nil
		}
		visiting[name] = true
		view := b.views[name]
		aliases := make([]string, 0, len(view.sources))
		for alias := range view.sources {
			aliases = append(aliases, alias)
		}
		sort.Strings(aliases)
		columns := make(map[string]map[string]bool)
		for _, alias := range aliases {
			source := view.sources[alias]
			if b.views[strings.ToLower(source.name)] != nil {
				if err := visit(strings.ToLower(source.name)); err != nil {
					return err
				}
			}
			names, err := b.sourceColumns(source)
			if err != nil {
				return err
			}
			columns[alias] = make(map[string]bool)
			for _, column := range names {
				columns[alias][strings.ToLower(column)] = true
			}
		}
		for _, ref := range view.refs {
			if !ref.Qualifier.IsEmpty() {
				if !columns[strings.ToLower(ref.Qualifier.Name.String())][ref.Name.Lowered()] {
					return ErrUnsupported
				}
				continue
			}
			matches := 0
			for _, source := range columns {
				if source[ref.Name.Lowered()] {
					matches++
				}
			}
			if matches != 1 {
				return ErrUnsupported
			}
		}
		if err := plan.add("SET SESSION collation_connection = "+view.collation, sqlparser.String(view.create)); err != nil {
			return err
		}
		plan.views = append(plan.views, View{Name: view.create.ViewName.Name.String(), Columns: append([]string(nil), view.columns...), Collation: view.collation})
		visiting[name], visited[name] = false, true
		return nil
	}
	for _, name := range names {
		if err := visit(name); err != nil {
			return err
		}
	}
	return nil
}
