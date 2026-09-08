<!-- One concern per pull request. What was wrong, and why this is the fix -
written for whoever runs git blame on this line in two years. -->

## What this changes

## Why

## Checks

- [ ] `gofmt -l .` prints nothing, `go vet ./...` and `go test ./...` pass
- [ ] Touching the extension: `npm run lint` and `npm test` pass in `editors/vscode/`
- [ ] Touching ownership, staging, push preflight or recovery: a test that
      fails without this change, and that shows the unsafe case is *refused*
- [ ] Behaviour changed: `docs/USAGE.md` says so
- [ ] Commit subject follows Conventional Commits (`fix(cli): …`)
