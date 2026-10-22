package restoreplan

import (
	"bufio"
	"context"
	"errors"
	"io"
)

// Framing only locates raw statement boundaries. split still validates every
// frame, including executable comments, quotes, UTF-8, nesting, and client SQL.
func scanStatements(ctx context.Context, input io.Reader, options Options, consume func(string, int64) error) error {
	r := bufio.NewReaderSize(io.LimitReader(input, options.MaxInputBytes+1), 64<<10)
	var raw []byte
	var offset, position int64
	var quote byte
	var escaped, line, block, blockStar bool
	read := func() (byte, error) {
		if position%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
		}
		c, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		position++
		if position > options.MaxInputBytes || len(raw) >= options.MaxStatementBytes*2 {
			return 0, ErrLimit
		}
		raw = append(raw, c)
		return c, nil
	}
	for {
		c, err := read()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return consume(string(raw), offset)
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, ErrLimit) {
				return err
			}
			return &safeError{ErrInput, err}
		}
		if line {
			line = c != '\n'
			continue
		}
		if block {
			if blockStar && c == '/' {
				block = false
			}
			blockStar = c == '*'
			continue
		}
		if quote != 0 {
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '\'', '"', '`':
			quote = c
		case '#':
			line = true
		case '-':
			next, _ := r.Peek(2)
			line = len(next) == 2 && next[0] == '-' && next[1] <= ' '
		case '/':
			next, _ := r.Peek(1)
			if len(next) == 1 && next[0] == '*' {
				if _, err := read(); err != nil {
					if errors.Is(err, ErrLimit) || ctx.Err() != nil {
						return err
					}
					return &safeError{ErrInput, err}
				}
				block = true
				blockStar = false
			}
		case ';':
			if err := consume(string(raw), offset); err != nil {
				return err
			}
			raw = raw[:0]
			offset = position
		}
	}
}
