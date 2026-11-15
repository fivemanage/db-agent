package restoreplan

import (
	"context"
	"sort"
	"strings"

	"vitess.io/vitess/go/vt/sqlparser"
)

type Trigger struct {
	Name      string
	Table     string
	Timing    string
	Event     string
	Collation string
	SQLMode   string
	Order     int
}

type triggerPlan struct {
	Trigger
	body  string
	order int
}

type triggerToken struct {
	text       string
	word       string
	start, end int
}

// Vitess does not parse CREATE TRIGGER. This grammar recognizes only headers,
// blocks and IF branches; every executable leaf still passes its strict parser
// and the destination-reference allowlist. Original SQL is never emitted.
func triggerTokens(input string) ([]triggerToken, error) {
	var tokens []triggerToken
	for i := 0; i < len(input); {
		if strings.ContainsRune(" \t\r\n", rune(input[i])) {
			i++
			continue
		}
		start, word := i, ""
		switch input[i] {
		case '\'', '"', '`':
			end, err := quoted(context.Background(), input, i)
			if err != nil {
				return nil, err
			}
			i = end
		default:
			for i < len(input) && (input[i] >= 'a' && input[i] <= 'z' || input[i] >= 'A' && input[i] <= 'Z' ||
				input[i] >= '0' && input[i] <= '9' || input[i] == '_' || input[i] == '$' || input[i] >= 128) {
				i++
			}
			if i == start {
				i++
			} else {
				word = strings.ToLower(input[start:i])
			}
		}
		tokens = append(tokens, triggerToken{text: input[start:i], word: word, start: start, end: i})
		if len(tokens) > 65536 {
			return nil, ErrLimit
		}
	}
	return tokens, nil
}

type triggerParser struct {
	input  string
	tokens []triggerToken
	pos    int
	parser *sqlparser.Parser
	b      *builder
	plan   *triggerPlan
	leaves int
}

func (p *triggerParser) take(word string) bool {
	if p.pos < len(p.tokens) && p.tokens[p.pos].word == word {
		p.pos++
		return true
	}
	return false
}

func (p *triggerParser) name() (sqlparser.TableName, error) {
	read := func() (string, error) {
		if p.pos >= len(p.tokens) {
			return "", ErrUnsupported
		}
		token := p.tokens[p.pos]
		p.pos++
		name := token.text
		if strings.HasPrefix(name, "`") {
			name = strings.ReplaceAll(name[1:len(name)-1], "``", "`")
		} else if token.word == "" {
			return "", ErrUnsupported
		}
		if !identifier(name) {
			return "", ErrUnsupported
		}
		return name, nil
	}
	first, err := read()
	if err != nil {
		return sqlparser.TableName{}, err
	}
	name := sqlparser.TableName{Name: sqlparser.NewIdentifierCS(first)}
	if p.pos < len(p.tokens) && p.tokens[p.pos].text == "." {
		p.pos++
		second, err := read()
		if err != nil {
			return sqlparser.TableName{}, err
		}
		name.Qualifier, name.Name = name.Name, sqlparser.NewIdentifierCS(second)
	}
	if !p.b.used || !name.Qualifier.IsEmpty() && name.Qualifier.String() != p.b.options.Source {
		return sqlparser.TableName{}, ErrUnsupported
	}
	return name, nil
}

func (b *builder) triggerStatement(parser *sqlparser.Parser, statement string) (bool, error) {
	first := strings.IndexAny(statement, " \t\r\n")
	if first < 0 || !strings.EqualFold(statement[:first], "create") && !strings.EqualFold(statement[:first], "drop") {
		return false, nil
	}
	head := strings.TrimSpace(statement[first:])
	if !triggerKeyword(head, "trigger") && !triggerKeyword(head, "definer") {
		return false, nil
	}
	tokens, err := triggerTokens(statement)
	if err != nil {
		return false, err
	}
	p := &triggerParser{input: statement, tokens: tokens, parser: parser, b: b}
	if p.take("drop") {
		if !p.take("trigger") {
			return false, nil
		}
		if !p.take("if") || !p.take("exists") {
			return true, ErrUnsupported
		}
		name, err := p.name()
		key := strings.ToLower(name.Name.String())
		if err != nil || p.pos != len(tokens) || b.triggerDrops[key] || b.triggers[key] != nil {
			return true, ErrUnsupported
		}
		if len(b.triggerDrops) >= b.options.MaxTables {
			return true, ErrLimit
		}
		b.triggerDrops[key] = true
		return true, nil
	}
	p.take("create")
	if !p.take("trigger") {
		if !p.take("definer") {
			return false, nil
		}
		for p.pos < len(tokens) && tokens[p.pos].word != "trigger" && tokens[p.pos].word != "view" {
			p.pos++
		}
		if !p.take("trigger") {
			return false, nil
		}
	}
	// Reuse the existing parser for the optional DEFINER rather than accepting
	// arbitrary text between CREATE and TRIGGER.
	prefix := statement[:tokens[p.pos-1].start]
	node, err := parser.ParseStrictDDL(prefix + " VIEW `__fm_trigger_header` AS SELECT 1")
	if err != nil {
		return true, ErrUnsupported
	}
	header, ok := node.(*sqlparser.CreateView)
	if !ok || header.IsReplace || header.Algorithm != "" || header.Security != "" {
		return true, ErrUnsupported
	}
	name, err := p.name()
	if err != nil {
		return true, err
	}
	key := strings.ToLower(name.Name.String())
	if b.triggers[key] != nil || b.viewCollation == "" || b.sqlMode == nil {
		return true, ErrUnsupported
	}
	if b.objectCount() >= b.options.MaxTables {
		return true, ErrLimit
	}
	timing, event := "", ""
	for _, value := range []string{"before", "after"} {
		if p.take(value) {
			timing = strings.ToUpper(value)
			break
		}
	}
	for _, value := range []string{"insert", "update", "delete"} {
		if p.take(value) {
			event = strings.ToUpper(value)
			break
		}
	}
	if timing == "" || event == "" || !p.take("on") {
		return true, ErrUnsupported
	}
	table, err := p.name()
	if err != nil || !p.take("for") || !p.take("each") || !p.take("row") || p.pos == len(tokens) {
		return true, ErrUnsupported
	}
	b.triggers[key] = &triggerPlan{
		Trigger: Trigger{Name: name.Name.String(), Table: table.Name.String(), Timing: timing, Event: event,
			Collation: b.viewCollation, SQLMode: *b.sqlMode},
		body: statement[tokens[p.pos].start:], order: len(b.triggers),
	}
	return true, nil
}

func (b *builder) finishTriggers(plan *Plan) error {
	for name := range b.triggerDrops {
		if b.triggers[name] == nil {
			return ErrUnsupported
		}
	}
	triggers := make([]*triggerPlan, 0, len(b.triggers))
	for _, trigger := range b.triggers {
		triggers = append(triggers, trigger)
	}
	sort.Slice(triggers, func(i, j int) bool { return triggers[i].order < triggers[j].order })
	parser, err := sqlparser.New(sqlparser.Options{})
	if err != nil {
		return ErrUnsupported
	}
	orders := make(map[string]int)
	for _, trigger := range triggers {
		table := b.tables[strings.ToLower(trigger.Table)]
		if table == nil || table.create.Table.Name.String() != trigger.Table {
			return ErrUnsupported
		}
		tokens, err := triggerTokens(trigger.body)
		if err != nil {
			return err
		}
		p := &triggerParser{input: trigger.body, tokens: tokens, parser: parser, b: b, plan: trigger}
		body, err := p.body(0)
		if err != nil || p.pos != len(tokens) {
			if err != nil {
				return err
			}
			return ErrUnsupported
		}
		name := b.destination(sqlparser.TableName{Name: sqlparser.NewIdentifierCS(trigger.Name)})
		target := b.destination(sqlparser.TableName{Name: sqlparser.NewIdentifierCS(trigger.Table)})
		create := "CREATE TRIGGER " + sqlparser.String(name) + " " + trigger.Timing + " " + trigger.Event +
			" ON " + sqlparser.String(target) + " FOR EACH ROW " + body
		if err := plan.add("SET SESSION sql_mode = "+sqlparser.String(sqlparser.NewStrLiteral(trigger.SQLMode)),
			"SET SESSION collation_connection = "+trigger.Collation, create); err != nil {
			return err
		}
		group := trigger.Table + ":" + trigger.Timing + ":" + trigger.Event
		orders[group]++
		trigger.Order = orders[group]
		plan.triggers = append(plan.triggers, trigger.Trigger)
	}
	if len(triggers) != 0 {
		return plan.add(SessionModeSQL)
	}
	return nil
}

func (p *triggerParser) body(depth int) (string, error) {
	if depth > 32 {
		return "", ErrLimit
	}
	if p.take("begin") {
		body, err := p.list(depth + 1)
		if err != nil || !p.take("end") {
			return "", triggerBodyError(err)
		}
		return "BEGIN " + body + " END", nil
	}
	if p.take("if") {
		var branches []string
		for {
			start := p.pos
			for p.pos < len(p.tokens) && p.tokens[p.pos].word != "then" {
				p.pos++
			}
			if start == p.pos || !p.take("then") {
				return "", ErrUnsupported
			}
			expr, err := p.parser.ParseExpr(p.input[p.tokens[start].start:p.tokens[p.pos-2].end])
			if err != nil {
				return "", ErrUnsupported
			}
			context := p.context()
			if err := context.expr(expr); err != nil {
				return "", err
			}
			body, err := p.list(depth + 1)
			if err != nil {
				return "", err
			}
			branches = append(branches, sqlparser.String(expr)+" THEN "+body)
			if !p.take("elseif") {
				break
			}
		}
		result := "IF " + strings.Join(branches, " ELSEIF ")
		if p.take("else") {
			body, err := p.list(depth + 1)
			if err != nil {
				return "", err
			}
			result += " ELSE " + body
		}
		if !p.take("end") || !p.take("if") {
			return "", ErrUnsupported
		}
		return result + " END IF", nil
	}
	start := p.pos
	for p.pos < len(p.tokens) && p.tokens[p.pos].text != ";" {
		p.pos++
	}
	if start == p.pos {
		return "", ErrUnsupported
	}
	p.leaves++
	if p.leaves > 1024 {
		return "", ErrLimit
	}
	return p.leaf(p.input[p.tokens[start].start:p.tokens[p.pos-1].end])
}

func triggerBodyError(err error) error {
	if err != nil {
		return err
	}
	return ErrUnsupported
}

func (p *triggerParser) list(depth int) (string, error) {
	var statements []string
	for p.pos < len(p.tokens) {
		switch p.tokens[p.pos].word {
		case "end", "else", "elseif":
			if len(statements) == 0 {
				return "", ErrUnsupported
			}
			return strings.Join(statements, " "), nil
		}
		statement, err := p.body(depth)
		if err != nil || p.pos >= len(p.tokens) || p.tokens[p.pos].text != ";" {
			return "", triggerBodyError(err)
		}
		p.pos++
		statements = append(statements, statement+";")
	}
	return "", ErrUnsupported
}

type triggerContext struct {
	viewContext
	trigger *triggerPlan
}

func (p *triggerParser) context() *triggerContext {
	view := &viewPlan{sources: make(map[string]viewSource)}
	if p.plan.Event != "DELETE" {
		view.sources["new"] = viewSource{name: p.plan.Table, alias: "NEW", aliased: true}
	}
	if p.plan.Event != "INSERT" {
		view.sources["old"] = viewSource{name: p.plan.Table, alias: "OLD", aliased: true}
	}
	return &triggerContext{viewContext{builder: p.b, view: view}, p.plan}
}

func (v *triggerContext) expr(expr sqlparser.Expr) error {
	// Pseudo-row qualifiers are case-insensitive, unlike table aliases on
	// case-sensitive servers. They must never carry a schema qualifier.
	nodes := 0
	if err := sqlparser.Walk(func(node sqlparser.SQLNode) (bool, error) {
		nodes++
		if nodes > 1024 {
			return false, ErrLimit
		}
		if ref, ok := node.(*sqlparser.ColName); ok && ref.Qualifier.Qualifier.IsEmpty() {
			switch strings.ToLower(ref.Qualifier.Name.String()) {
			case "new", "old":
				ref.Qualifier.Name = sqlparser.NewIdentifierCS(strings.ToUpper(ref.Qualifier.Name.String()))
			}
		}
		return true, nil
	}, expr); err != nil {
		return err
	}
	v.view.refs = nil
	if err := v.viewContext.expr(expr, 0); err != nil {
		return err
	}
	for _, ref := range v.view.refs {
		matches := 0
		for alias, source := range v.view.sources {
			if ref.Qualifier.IsEmpty() {
				if alias == "new" || alias == "old" {
					continue
				}
			} else if !strings.EqualFold(ref.Qualifier.Name.String(), alias) {
				continue
			}
			columns, err := v.builder.sourceColumns(source)
			if err != nil {
				return err
			}
			for _, column := range columns {
				if strings.EqualFold(column, ref.Name.String()) {
					matches++
				}
			}
		}
		if matches != 1 {
			return ErrUnsupported
		}
	}
	return nil
}

func (v *triggerContext) target(expr *sqlparser.AliasedTableExpr) (*tablePlan, error) {
	if !plainTable(expr) {
		return nil, ErrUnsupported
	}
	name := expr.Expr.(sqlparser.TableName)
	key, err := v.builder.tableName(name)
	table := v.builder.tables[key]
	if err != nil || table == nil || strings.EqualFold(name.Name.String(), v.trigger.Table) ||
		key == "new" || key == "old" {
		// MySQL forbids a trigger from changing its own invoking table.
		return nil, ErrUnsupported
	}
	v.view.sources[key] = viewSource{name: name.Name.String(), alias: name.Name.String()}
	expr.Expr = v.builder.destination(name)
	return table, nil
}

func triggerColumn(table *tablePlan, name string) bool {
	for _, column := range table.create.TableSpec.Columns {
		if strings.EqualFold(column.Name.String(), name) {
			return true
		}
	}
	return false
}

func (p *triggerParser) leaf(input string) (string, error) {
	assign := triggerKeyword(input, "set")
	if assign {
		input = "UPDATE `__fm_new` " + input
	}
	node, err := p.parser.ParseStrictDDL(input)
	if err != nil {
		return "", ErrUnsupported
	}
	context := p.context()
	switch node := node.(type) {
	case *sqlparser.Insert:
		if node.Action != sqlparser.InsertAct || node.Ignore || len(node.Partitions) != 0 || node.RowAlias != nil || len(node.OnDup) != 0 {
			return "", ErrUnsupported
		}
		table, err := context.target(node.Table)
		if err != nil {
			return "", err
		}
		// INSERT VALUES has no current target row for unqualified expressions.
		delete(context.view.sources, strings.ToLower(table.create.Table.Name.String()))
		width := len(table.create.TableSpec.Columns)
		if len(node.Columns) != 0 {
			width = len(node.Columns)
			seen := make(map[string]bool)
			for _, column := range node.Columns {
				if !triggerColumn(table, column.String()) || seen[column.Lowered()] {
					return "", ErrUnsupported
				}
				seen[column.Lowered()] = true
			}
		}
		values, ok := node.Rows.(sqlparser.Values)
		if !ok || len(values) == 0 {
			return "", ErrUnsupported
		}
		for _, row := range values {
			if len(row) != width {
				return "", ErrUnsupported
			}
			for _, expr := range row {
				if err := context.expr(expr); err != nil {
					return "", err
				}
			}
		}
	case *sqlparser.Update:
		if node.With != nil || node.Ignore || len(node.TableExprs) != 1 || len(node.OrderBy) != 0 || node.Limit != nil {
			return "", ErrUnsupported
		}
		target, ok := node.TableExprs[0].(*sqlparser.AliasedTableExpr)
		if !ok {
			return "", ErrUnsupported
		}
		var table *tablePlan
		if assign {
			if p.plan.Timing != "BEFORE" || p.plan.Event == "DELETE" {
				return "", ErrUnsupported
			}
			table = p.b.tables[strings.ToLower(p.plan.Table)]
		} else {
			table, err = context.target(target)
			if err != nil {
				return "", err
			}
		}
		for _, assignment := range node.Exprs {
			ref := assignment.Name
			if !triggerColumn(table, ref.Name.String()) {
				return "", ErrUnsupported
			}
			if assign {
				if !strings.EqualFold(ref.Qualifier.Name.String(), "NEW") || !ref.Qualifier.Qualifier.IsEmpty() {
					return "", ErrUnsupported
				}
				ref.Qualifier.Name = sqlparser.NewIdentifierCS("NEW")
			} else if !ref.Qualifier.IsEmpty() {
				if err := context.expr(ref); err != nil {
					return "", err
				}
				if ref.Qualifier.Name.String() == "NEW" || ref.Qualifier.Name.String() == "OLD" {
					return "", ErrUnsupported
				}
			}
			if err := context.expr(assignment.Expr); err != nil {
				return "", err
			}
		}
		if assign {
			if node.Where != nil {
				return "", ErrUnsupported
			}
			return "SET " + sqlparser.String(node.Exprs), nil
		}
		if node.Where != nil {
			if err := context.expr(node.Where.Expr); err != nil {
				return "", err
			}
		}
	case *sqlparser.Delete:
		if node.With != nil || node.Ignore || len(node.TableExprs) != 1 || len(node.Targets) != 0 ||
			len(node.Partitions) != 0 || len(node.OrderBy) != 0 || node.Limit != nil {
			return "", ErrUnsupported
		}
		target, ok := node.TableExprs[0].(*sqlparser.AliasedTableExpr)
		if !ok {
			return "", ErrUnsupported
		}
		if _, err := context.target(target); err != nil {
			return "", err
		}
		if node.Where != nil {
			if err := context.expr(node.Where.Expr); err != nil {
				return "", err
			}
		}
	default:
		return "", ErrUnsupported
	}
	return sqlparser.String(node), nil
}

func triggerKeyword(input, word string) bool {
	return len(input) > len(word) && strings.EqualFold(input[:len(word)], word) &&
		strings.ContainsRune(" \t\r\n=", rune(input[len(word)]))
}
