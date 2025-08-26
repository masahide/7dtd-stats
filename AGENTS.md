# Repository Guidelines

This repository contains a small Go library for tagged time-series file storage (`pkg/tsfile`) and design documents under `docs/`. Use these guidelines to keep changes consistent and easy to review.

## Project Structure & Module Organization

```
docs/            # Architecture and format specs (spec.md, tsfile.md)
pkg/tsfile/      # Go package (library code and tests)
README.md        # Project overview
```

- Package: `tsfile` provides append-only, hourly-partitioned NDJSON.gz storage with tag hashing.
- Tests live next to code as `*_test.go`.

## Build, Test, and Development Commands

- Build: `go build ./...` — compiles all packages.
- Test (all): `go test ./...`
- Test (package): `go test ./pkg/tsfile`
- Test with coverage: `go test -cover ./...`
- Vet: `go vet ./...` — static checks.
- Format: `go fmt ./...`

Go toolchain 1.25+ is declared in `go.mod` (use ≥1.22 if 1.25 is unavailable locally).

## Coding Style & Naming Conventions

- Formatting: standard `gofmt` (tabs, 8-space tab width). Run `go fmt` before pushing.
- Imports: group standard/library-local logically; prefer stable order.
- Naming: exported identifiers use CamelCase; packages are short, lowercase (`tsfile`).
- Errors: return wrapped errors with context where helpful; avoid panics in library code.
- Files: production code in `.go`; tests in `*_test.go` with focused scopes.

## Testing Guidelines

- Framework: Go’s `testing` package; prefer table-driven tests where reasonable.
- Naming: `TestXxx(t *testing.T)`; helpers use `t.Helper()`.
- Coverage: aim to cover core paths (append/rotation/scan). Use `go test -cover` locally.
- Determinism: avoid time/race flakiness; use fixed `time.Time` values in tests.

## Commit & Pull Request Guidelines

- Commits: concise, imperative subject (“Add scan range filter”), include rationale in body when non-trivial.
- Conventional Commits are welcome (feat:, fix:, refactor:, docs:), but not required.
- PRs must include: summary of change, motivation/linked issue, test coverage notes, and any docs updates (`docs/` when behavior changes).
- Keep diffs focused; avoid unrelated formatting churn. Ensure `go fmt`, `go vet`, and `go test ./...` pass.

## Notes & Tips

- Refer to `docs/tsfile.md` for on-disk layout and public API, and `docs/spec.md` for the broader system context this library can support.
- File outputs are created on-demand; be mindful of filesystem permissions in callers.

## エージェント応答ルール

- 既定の言語: すべてのユーザー向け回答は日本語で行います。
- コード・識別子: コード、識別子、API 名、ファイルパスは原則として英語を維持します（必要に応じて日本語で補足）。
- 引用: エラーメッセージやログなどの引用は原文を尊重し、必要に応じて日本語で説明を付けます。
- 簡潔さ: 余分な装飾を避け、簡潔で実用的な説明を心がけます。
- 例外: ユーザーが英語での回答を明示的に希望する場合は英語で対応します。
