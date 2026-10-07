---
name: code-style
description: Apply db-agent Go conventions when writing or reviewing Go code, including errors, context, comments, secrets, formatting, and tests.
---

# Go code style

- Use `gofmt` and `go vet`. Follow the naming and import conventions of the surrounding Go package. Avoid package stutter such as `config.ConfigSettings`.
- Use a single blank line between logical steps, such as validation, setup, work, and the final return. In tests, separate setup, execution, and assertions. Keep closely related statements together, especially an assignment and its error check or resource acquisition and its cleanup registration. Do not add blank lines between every statement or just inside braces; `gofmt` preserves logical spacing but does not add it for you.
- Keep functions small and explicit. Return values and errors rather than hiding failures in global state. Add interfaces where a consumer or a test needs a boundary, not in anticipation of one.
- Wrap lower-level errors with `%w` and enough operation context to diagnose them. Use `errors.Is` or `errors.As` when checking wrapped errors; do not compare error text.
- Pass `context.Context` through cancelable network and long-running work. Close response bodies and files, and clean up partial downloads or temporary output on errors.
- Handle secrets as local sensitive data: never log or echo enrollment tokens, agent credentials, database passwords, encryption keys, or release signing private keys. Keep sensitive request bodies out of errors.
- Add comments only for non-obvious invariants, security decisions, protocol requirements, or OS-specific behavior. Do not restate what a function name or the following line already says. Keep required `//go:build` lines and useful exported API documentation.
- Keep tests close to the package in `_test.go` files. Test observable behavior and meaningful failure paths; use `t.TempDir()` and `httptest` for isolated file and HTTP cases. Do not write tests that merely mirror a branch or restate implementation details.
- Format changed Go files with `gofmt`, then run relevant tests and `go vet ./...`. Compile affected platforms when changing platform-specific code.
