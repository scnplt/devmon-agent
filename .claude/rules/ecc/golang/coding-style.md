---
paths:
  - "**/*.go"
  - "**/go.mod"
  - "**/go.sum"
---
# Go Coding Style

> This file extends [common/coding-style.md](../common/coding-style.md) with Go specific content.

## Formatting

- **gofmt** and **goimports** are mandatory — no style debates

## Design Principles

- Accept interfaces, return structs
- Keep interfaces small (1-3 methods)
- Define interfaces where they are consumed, not where they are implemented
- Inject dependencies through constructor functions

## Naming

Standard Go naming: `MixedCaps`, exported identifiers capitalised, no underscores, no
package-name stutter (`httpapi.Server`, not `httpapi.HTTPAPIServer`).

## Error Handling

Always wrap errors with context:

```go
if err != nil {
    return fmt.Errorf("failed to create user: %w", err)
}
```

## Testing

Table-driven tests with `go test`, always `-race`. Coverage floor and the full gate list
live in `CLAUDE.md`.

## Security

`gosec ./...` must be clean. Every blocking call takes a `context.Context` with a deadline
the caller controls.

## Reference

See skill: `golang-patterns` for comprehensive Go idioms and patterns.
