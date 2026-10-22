package restoreplan

import (
	"context"
	"strings"
	"unicode/utf8"
)

// Executable comments are expanded regardless of server version. Conditional
// syntax must pass the same allowlist, never become a hidden future operation.
func split(ctx context.Context, input string, options Options) ([]string, error) {
	if !utf8.ValidString(input) || strings.IndexByte(input, 0) >= 0 {
		return nil, ErrUnsupported
	}
	var current strings.Builder
	var statements []string
	depth := 0
	nextCheck := 0
	for i := 0; i < len(input); {
		if i >= nextCheck {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			nextCheck = i + 4096
		}
		if current.Len() > options.MaxStatementBytes {
			return nil, ErrLimit
		}
		c := input[i]
		if c == '\'' || c == '"' || c == '`' {
			end, err := quoted(ctx, input, i)
			if err != nil {
				return nil, err
			}
			if current.Len()+end-i > options.MaxStatementBytes {
				return nil, ErrLimit
			}
			current.WriteString(input[i:end])
			i = end
			continue
		}
		if c == '#' || c == '-' && i+2 < len(input) && input[i+1] == '-' && input[i+2] <= ' ' {
			end := strings.IndexByte(input[i:], '\n')
			if end < 0 {
				i = len(input)
			} else {
				i += end + 1
			}
			current.WriteByte(' ')
			continue
		}
		if c == '/' && i+1 < len(input) && input[i+1] == '*' {
			end := strings.Index(input[i+2:], "*/")
			if end < 0 {
				return nil, ErrUnsupported
			}
			body := input[i+2 : i+2+end]
			if strings.Contains(body, "/*") || strings.HasPrefix(body, "+") {
				return nil, ErrUnsupported
			}
			if strings.HasPrefix(body, "!") || strings.HasPrefix(body, "M!") {
				sandbox := strings.TrimSpace(body)
				if sandbox == "M!999999\\- enable sandbox mode" || sandbox == "!999999\\- enable sandbox mode" ||
					sandbox == "M!999999\\- enable the sandbox mode" || sandbox == "!999999\\- enable the sandbox mode" {
					if options.Driver != "mariadb" {
						return nil, ErrUnsupported
					}
				} else {
					body = strings.TrimPrefix(strings.TrimPrefix(body, "M"), "!")
					digits := 0
					for digits < len(body) && body[digits] >= '0' && body[digits] <= '9' {
						digits++
					}
					if digits != 0 && digits != 5 && digits != 6 {
						return nil, ErrUnsupported
					}
					body = body[digits:]
					if current.Len()+len(body) > options.MaxStatementBytes {
						return nil, ErrLimit
					}
					if err := executable(ctx, body, &depth); err != nil {
						return nil, err
					}
					current.WriteByte(' ')
					current.WriteString(body)
				}
			} else {
				current.WriteByte(' ')
			}
			current.WriteByte(' ')
			i += end + 4
			continue
		}
		switch c {
		case '\\':
			return nil, ErrUnsupported
		case '(':
			depth++
			if depth > 128 {
				return nil, ErrLimit
			}
		case ')':
			depth--
			if depth < 0 {
				return nil, ErrUnsupported
			}
		case ';':
			if depth != 0 {
				return nil, ErrUnsupported
			}
			statement := strings.TrimSpace(current.String())
			if statement != "" {
				if len(statements) >= options.MaxStatements {
					return nil, ErrLimit
				}
				statements = append(statements, statement)
			}
			current.Reset()
			i++
			continue
		}
		current.WriteByte(c)
		i++
	}
	if depth != 0 || strings.TrimSpace(current.String()) != "" {
		return nil, ErrUnsupported
	}
	return statements, nil
}

// View bodies can contain expressions and quoted literals inside executable
// comments. Validate their boundaries exactly as SQL, never as a raw substring
// to bypass framing limits or introduce a second statement/client command.
func executable(ctx context.Context, body string, depth *int) error {
	for i := 0; i < len(body); i++ {
		if i%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		switch body[i] {
		case '\'', '"', '`':
			end, err := quoted(ctx, body, i)
			if err != nil {
				return err
			}
			i = end - 1
		case ';', '#', '\\':
			return ErrUnsupported
		case '-':
			if i+1 < len(body) && body[i+1] == '-' {
				return ErrUnsupported
			}
		case '(':
			*depth++
			if *depth > 128 {
				return ErrLimit
			}
		case ')':
			*depth--
			if *depth < 0 {
				return ErrUnsupported
			}
		}
	}
	return nil
}

func quoted(ctx context.Context, input string, start int) (int, error) {
	quote := input[start]
	nextCheck := start
	for i := start + 1; i < len(input); i++ {
		if i >= nextCheck {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
			nextCheck = i + 4096
		}
		if input[i] == '\\' {
			if quote == '`' || i+1 == len(input) {
				return 0, ErrUnsupported
			}
			i++
			continue
		}
		if input[i] == quote {
			if i+1 < len(input) && input[i+1] == quote {
				i++
				continue
			}
			return i + 1, nil
		}
	}
	return 0, ErrUnsupported
}
