package restoreplan

import (
	"strings"

	"vitess.io/vitess/go/vt/sqlparser"
)

var savedVariables = map[string]string{
	"old_character_set_client": "character_set_client", "saved_cs_client": "character_set_client",
	"old_character_set_results": "character_set_results", "old_collation_connection": "collation_connection",
	"old_time_zone": "time_zone", "old_unique_checks": "unique_checks", "old_foreign_key_checks": "foreign_key_checks",
	"old_sql_mode": "sql_mode", "old_sql_notes": "sql_notes",
	"old_note_verbosity": "note_verbosity",
	"saved_cs_results":   "character_set_results", "saved_col_connection": "collation_connection",
}

func (b *builder) bookkeeping(node *sqlparser.Set) error {
	if err := boilerplate(node); err != nil {
		return err
	}
	for _, assignment := range node.Exprs {
		if assignment.Var.Scope == sqlparser.VariableScope || assignment.Var.Name.Lowered() != "collation_connection" {
			continue
		}
		b.viewCollation = ""
		switch value := assignment.Expr.(type) {
		case *sqlparser.Literal:
			b.viewCollation = value.Val
		case *sqlparser.ColName:
			b.viewCollation = value.Name.String()
		}
	}
	return nil
}

// Native dump session bookkeeping is recognized but never executed. The plan
// starts a clean, pinned session and keeps foreign-key and uniqueness checks on.
func boilerplate(node *sqlparser.Set) error {
	for _, assignment := range node.Exprs {
		if assignment.Var == nil {
			return ErrUnsupported
		}
		name := assignment.Var.Name.Lowered()
		if assignment.Var.Scope == sqlparser.VariableScope {
			base := savedVariables[name]
			value, ok := assignment.Expr.(*sqlparser.Variable)
			if base == "" || !ok || (value.Scope != sqlparser.SessionScope && value.Scope != sqlparser.NoScope) || value.Name.Lowered() != base {
				return ErrUnsupported
			}
			continue
		}
		if assignment.Var.Scope != sqlparser.SessionScope && assignment.Var.Scope != sqlparser.NoScope {
			return ErrUnsupported
		}
		if value, ok := assignment.Expr.(*sqlparser.Variable); ok {
			if value.Scope == sqlparser.VariableScope && savedVariables[value.Name.Lowered()] == name {
				continue
			}
			return ErrUnsupported
		}
		value := ""
		switch expr := assignment.Expr.(type) {
		case *sqlparser.Literal:
			value = expr.Val
		case *sqlparser.ColName:
			if !expr.Qualifier.IsEmpty() {
				return ErrUnsupported
			}
			value = expr.Name.String()
		default:
			return ErrUnsupported
		}
		switch name {
		case "names", "character_set_client", "character_set_results":
			if !strings.EqualFold(value, "utf8mb4") {
				return ErrUnsupported
			}
		case "collation_connection":
			if !strings.HasPrefix(strings.ToLower(value), "utf8mb4_") || !optionNamePattern.MatchString(value) {
				return ErrUnsupported
			}
		case "time_zone":
			if value != "+00:00" {
				return ErrUnsupported
			}
		case "sql_mode":
			if value != "NO_AUTO_VALUE_ON_ZERO" {
				return ErrUnsupported
			}
		case "unique_checks", "foreign_key_checks", "sql_notes", "note_verbosity":
			if value != "0" && value != "1" {
				return ErrUnsupported
			}
		default:
			return ErrUnsupported
		}
	}
	return nil
}
