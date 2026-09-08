# Contributing to GitOne

Issues, edge cases and honest criticism are all useful - especially the edge
cases, because this is a tool whose whole value is not being surprising.

Found a security problem? Do not open an issue. See [SECURITY.md](SECURITY.md).

By taking part you agree to the [code of conduct](CODE_OF_CONDUCT.md): argue
with the design as hard as you like, and leave the person out of it.

## The one rule

**GitOne refuses rather than guesses.** When ownership of a path cannot be
determined with certainty, the operation stops. There is no priority system, no
most-specific-match, no default repository and no "probably public". A change
that makes GitOne guess is not a change that gets merged, however convenient it
feels.

Everything else - output format, command surface, internal structure - is open
for discussion. Open an issue before a large change, so we agree on the shape
before you spend an evening on it.

## Layout

| Path | What it holds |
| --- | --- |
| `cmd/gitone/` | the binary entry point |
| `internal/` | one package per concern - `config`, `repository`, `worktree`, `stage`, `push`, `pull`, `clone`, `migrate`, `lock`, `ui` and one per command |
| `editors/vscode/` | the VS Code extension, TypeScript, talks only to the public CLI |
| `docs/` | the usage guide and the hands-on tutorial |

The extension never invokes `git` and never reads `.gitone/`. If it needs
something new, the CLI grows a documented command first.

## Building and checking

Requirements: Go 1.26 or newer, Node.js 24 or newer, Git 2.28 or newer.

```bash
go build -o gitone ./cmd/gitone
gofmt -l .        # must print nothing
go vet ./...
go test ./...
```

```bash
cd editors/vscode && npm ci && npm run lint && npm test
```

These are exactly what CI runs on every pull request. `npm run format` applies
the formatter; the Go side is `gofmt -w`.

Style is not a matter of taste here: Go is formatted by `gofmt`, TypeScript by
Prettier - four spaces, single quotes - and linted by oxlint. Run the formatter
rather than arguing with it.

## Tests

Go code uses the standard `testing` package, and the tests run against real Git
repositories in temporary directories rather than against mocks. A change to
path ownership, staging, push preflight or recovery needs a test that fails
without it. A test that only proves the happy path is not enough for anything
touching the ownership boundary - show that the unsafe case is refused.

## Commits and pull requests

Commit subjects follow [Conventional Commits](https://www.conventionalcommits.org):
`feat`, `fix`, `docs`, `refactor`, `style`, `build`, `test`, `chore`, with an
optional scope such as `cli`, `config` or `vscode`.

```
fix(cli): separate trusted terminal styling from repository data
```

Write the body for whoever runs `git blame` on this line in two years: what was
wrong, and why this is the fix. One concern per pull request.

## What is deliberately missing

Several Git operations are absent on purpose rather than by oversight - see
[Deferred](README.md#deferred). Before adding one, say in an issue how it stays
safe across several repositories at once. That design question, not the
implementation, is the hard part.
