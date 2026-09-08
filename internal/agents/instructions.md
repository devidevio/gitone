This project uses GitOne: one working directory, several Git repositories,
ownership decided by path in `.gitone.yml`.

- Use `gitone`, never `git`, for status, staging, committing and pushing.
  There is no `.git/` in the project root.
- `gitone status --json` is the machine-readable project state (schema
  `version: 1`). Exit code 0 means ordinary changes. Exit code 1 means one of
  two things: the document was written and its `issues` array explains why the
  project is unsafe, or GitOne refused before producing any output and only
  stderr carries the error code. Empty stdout on exit 1 is always the refusal,
  so read stderr instead of parsing.
- Commands: `setup`, `clone`, `init`, `migrate`, `backup list`, `backup git`,
  `backup restore`, `repo validate`, `repo list`, `status`, `add`, `unstage`,
  `restore`, `commit`, `show`, `diff`, `log`, `branch`, `switch`, `fetch`, `pull`,
  `push`, `reconfigure`, `recover`, `abort`, `doctor`, `agents`, `agents update`,
  `vscode info`, `vscode install`. `gitone <command> --help` prints the accepted
  syntax and flags. Unknown commands and flags fail with `CLI001`.
- Never edit `.gitone/` by hand or stage or commit `.gitone.local.yml`.
  Edit `.gitone.local.yml` when private repository configuration needs to
  change; it must stay local. After changing repository names, remotes or the
  repository set, run `gitone reconfigure` to preview and apply the change.
  Incoming structural changes use `gitone reconfigure --pull` or
  `gitone reconfigure --switch <branch>`; declare renames with `--rename old=new`.
- Start a branch everywhere with `gitone switch -c <branch>`; open an existing
  one with `gitone switch <branch>`, which creates a missing local branch from
  that repository's own fetched `origin/<branch>` and tracks it. Run
  `gitone fetch` first when a repository has not seen the branch yet; GitOne
  never fetches implicitly and never guesses a start point.
- `gitone branch -d <branch>` deletes one finished branch from every
  repository at once. Each repository has to consider it merged by native
  Git's own rule - contained in its configured upstream, or in `HEAD` where
  none is configured - decided from local refs only. There is no force option
  and no remote deletion; ask a human instead of deleting a refused branch
  with plain Git.
- Do not bypass a refusal with plain Git automatically. Manual branch repair
  and divergence resolution are human decisions; the usage guide documents
  them at https://gitone.io/docs/usage#manual-git-repairs. Never move a path
  across repository boundaries to make an error go away.
- A new file must be added to the `paths` of exactly one repository in
  `.gitone.yml` (public) or `.gitone.local.yml` (private) before it can be
  staged. `PATH001` means unassigned, `PATH002` means assigned twice.
- Repository boundaries are build boundaries. Before one file references
  another - an import, an include, a build input - check whether both paths
  have the same owner, and check the references you added again before you
  hand the work back. A reference across repositories works in your working
  directory and breaks in any checkout that has only one of them, such as CI.
  GitOne never reads file contents, so it cannot catch this: report it and ask
  a human rather than moving the path.
- `commit` with explicit paths commits the current working-tree version of
  exactly those managed files, like native `git commit <path>`, and leaves
  every other staged change staged. A file Git does not know yet has to be
  staged once before it can be selected.
- `restore <path>...` discards the unstaged changes of those files.
  `--staged` unstages them instead, `--worktree` names the working tree
  explicitly, and `--staged --worktree` makes index and file match that
  repository's own HEAD again, which also removes a selected file HEAD does not
  have. `--source HEAD` is the only accepted source; it is the default as soon
  as `--staged` is given. Every form except `--staged` alone destroys
  uncommitted work, so ask a human before running one.
- A path covered by `rules.protected_paths` must never be staged or committed;
  ownership does not override that hard deny.
- Valid patterns are exact paths such as `README.md`, `*` inside one path
  segment and at most one `**` segment, as in `src/**`, `docs/**/*.md` or
  `**/*.md`. Duplicate patterns are an error; overlapping ones are not, but a
  concrete path matching two repositories is `PATH002`.
- Non-interactive pushes require `gitone push --yes`; without a terminal a
  push fails with `PUSH001` instead of asking. Ask a human before pushing.
  `push: disabled` repositories are never published, and a configured
  `require_clean_worktree` rule must also pass.
- `LOCK001` has two forms and the message says which. When it names a running
  operation, wait and retry. When it says the lock cannot be attributed to this
  machine, no operation can be confirmed running and waiting never ends: stop
  retrying, run `gitone doctor`, and ask a human to follow
  https://gitone.io/docs/usage#repairing-an-unattributable-lock. Never remove a
  lock file yourself. On `REC001`, `recover` finishes an interrupted
  add, unstage, restore, commit, branch, switch, pull or reconfigure and `abort`
  undoes it. For an interrupted push both
  commands only report what reached the remotes. Never delete
  `.gitone/recovery/` by hand. If branch deletion recovery refuses changed or
  ambiguous state, preserve it and ask a human to follow
  https://gitone.io/docs/usage#resolving-an-interrupted-branch-deletion.
- `gitone doctor` is read-only and safe to run at any time, including while
  another operation holds the lock.
