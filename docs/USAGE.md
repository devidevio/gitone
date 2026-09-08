# GitOne usage guide

GitOne keeps several Git repositories in one working directory. This guide
covers setup, the daily workflow and the safety rules you need in practice.

For a complete walkthrough against disposable repositories, use the
[hands-on tutorial](TUTORIAL.md). For exact flags, run
`gitone <command> --help`. Error codes, causes and fixes live in the
[error reference](https://gitone.io/errors).

- [Configuration](#configuration)
- [Interactive setup](#interactive-setup)
- [Commands](#commands)
- [The normal workflow](#the-normal-workflow)
- [Branches and remotes](#branches-and-remotes)
- [Creating or importing a project](#creating-or-importing-a-project)
- [Recovery](#recovery)
- [Security boundary](#security-boundary)

## Configuration

A GitOne project is the nearest parent directory containing `.gitone.yml`.
There is no `.git/` in the project root.

| Path | Purpose |
| --- | --- |
| `.gitone.yml` | Committed public ownership map. |
| `.gitone.local.yml` | Uncommitted private repositories, paths and remotes. |
| `.gitone/` | Managed Git repositories, the project lock and recovery state. |

`gitone init` adds `.gitone/` and `.gitone.local.yml` to `.gitignore`.
Neither path can be owned, staged or committed.

### Example

`.gitone.yml`:

```yaml
version: 1
default_branch: main

rules:
  protected_paths:
    - .env
    - secrets/**
  push:
    require_clean_worktree: true

repositories:
  website:
    visibility: public
    remote: git@github.com:example/website.git
    paths:
      - .gitignore
      - .gitone.yml
      - README.md
      - src/**
```

`.gitone.local.yml`:

```yaml
version: 1

repositories:
  notes:
    visibility: private
    push: disabled
    remote: git@internal.example.com:ops/notes.git
    paths:
      - notes/**
```

Validate both files as one effective configuration:

```bash
gitone repo validate
gitone repo list
```

### Fields

| Field | Meaning |
| --- | --- |
| `version` | Required; currently `1`. |
| `default_branch` | Required in `.gitone.yml`; initial branch for new repositories. |
| `rules.protected_paths` | Paths GitOne must never manage or publish. |
| `rules.push.require_clean_worktree` | Refuse push while the working tree has changes. |
| `repositories.<name>.visibility` | Required; `public` or `private`. |
| `repositories.<name>.paths` | Required ownership patterns. |
| `repositories.<name>.remote` | Optional shorthand for `remotes.origin`. |
| `repositories.<name>.push` | Optional; `allowed` or `disabled`. |

Repository names match `[a-z][a-z0-9_-]*`; `all` is reserved. GitOne validates
branch and remote names with native Git.

### Path grammar

| Pattern | Matches |
| --- | --- |
| `README.md` | One exact path. |
| `*.md` | Markdown files in the project root. |
| `src/**` | Everything below `src/`. |
| `docs/**/*.md` | Markdown files directly and recursively below `docs/`. |
| `**/*.md` | Markdown files anywhere, including the project root. |

`*` stays inside one path segment. `**` matches complete path segments and may
appear only once in a pattern. Git pathspecs and regular expressions are not
supported.

Every relevant file must match exactly one repository. GitOne refuses
unassigned and multiply assigned paths instead of choosing an owner. A path
covered by `protected_paths` is always refused, even if an ownership pattern
also matches it.

### Merge rules

`.gitone.local.yml` merges repositories and remotes into the public file.
Local protected paths extend the public list, and local push rules may tighten
but never weaken public rules. A local repository's `paths` replaces its
public list; it does not extend it.

## Interactive setup

Run the guided setup in an existing directory:

```bash
gitone setup
```

Setup collects missing configuration, shows a preview and asks once before it
initializes a new project or migrates an existing root repository. Existing
configuration is validated and reused, never silently rewritten.

After successful setup, GitOne can show the release VSIX download and installation
command for VS Code. See the [README installation section](https://github.com/devidevio/gitone#install)
for extension availability.

The command needs an interactive terminal. With `NO_COLOR` or a basic
terminal it uses stable line-oriented prompts instead of the interactive
browser.

### Setup modes

| Mode | Result |
| --- | --- |
| `Wizard` | Asks for repositories and owned paths, then initializes or migrates. |
| `Template` | Writes commented configuration templates and stops. |

`Wizard` is the default. Both modes can add the managed GitOne block to the
project's `AGENTS.md`; the default answer is no.

### Template mode

Template mode creates `.gitone.yml` and `.gitone.local.yml`, updates
`.gitignore` and optionally updates `AGENTS.md`. It does not initialize or
migrate repositories. Edit the files, validate them, then rerun setup:

```bash
gitone repo validate
gitone setup
```

### Choosing the owned paths

The wizard lists existing files and directories. Choose the smallest stable
paths that describe ownership: `src/**` for a complete feature tree, or an
exact path for a single project file. GitOne does not infer public or private
ownership from filenames.

The project files `.gitone.yml`, `.gitignore`, `README.md` and `LICENSE` need
an owner like every other relevant file. `.gitone.local.yml` and `.gitone/`
are reserved and never need one.

### Correcting the owned paths

If validation reports an unassigned or multiply assigned path, return to the
path selection in setup or edit the configuration directly. Setup keeps valid
answers while you correct the ownership list.

### Cancellation and retry

Cancelling before confirmation writes nothing. If setup fails after writing a
valid configuration, fix the reported problem and rerun `gitone setup`; it
reuses that configuration and retries the unfinished action.

## Commands

GitOne exposes an allowlist. Unknown commands and flags are rejected rather
than forwarded to Git.

| Command | Purpose |
| --- | --- |
| `gitone setup` | Configure and initialize or migrate interactively. |
| `gitone clone <url> [directory]` | Create a project from a repository containing `.gitone.yml`. |
| `gitone init` | Initialize managed repositories from existing configuration. |
| `gitone migrate [--yes]` | Convert a root `.git/` and retain it as a backup. |
| `gitone backup list` | List migration backups. |
| `gitone backup git ...` | Inspect a migration backup with native Git. |
| `gitone backup restore ...` | Restore a migration backup to the root. |
| `gitone repo validate` | Validate the merged configuration. |
| `gitone repo list` | List configured repositories and remotes. |
| `gitone status [--porcelain \| --json]` | Show all repository states and changes. |
| `gitone add [-A \| -u \| <path>...]` | Stage owned changes. |
| `gitone unstage [-A \| <path>...]` | Unstage without changing files. |
| `gitone restore [--source HEAD] [--staged] [--worktree] <path>...` | Discard file changes, staged changes or both. |
| `gitone commit [<path>...] <-m <message> \| -F ->` | Commit all staged or selected files. |
| `gitone diff [all \| <repository>] [--staged]` | Show native patches by repository. |
| `gitone log [all \| <repository>] [-n <count>]` | Show recent commits. |
| `gitone branch [<name> \| -d <branch>]` | List branches, create one everywhere or delete a merged one everywhere. |
| `gitone switch [-c] <branch> ...` | Switch every repository to one branch, creating it where it is missing. |
| `gitone fetch [all \| <repository>]` | Fetch `origin` remote-tracking refs. |
| `gitone pull [all \| <repository>] ...` | Fast-forward from `origin`. |
| `gitone push [all \| <repository>] [--yes]` | Push outgoing commits. |
| `gitone reconfigure ...` | Apply configured repository additions, removals, renames or remote changes. |
| `gitone show --repository <name> --source <head\|index> -- <path>` | Read exact file content for editor integrations. |
| `gitone vscode info --json` | Report the editor protocol and GitOne version. |
| `gitone vscode install ...` | Install the VS Code extension. |
| `gitone recover` | Finish an interrupted operation. |
| `gitone abort` | Undo an interrupted operation. |
| `gitone doctor` | Check project health without changing it. |
| `gitone agents` | Check the managed `AGENTS.md` block. |
| `gitone agents update` | Write the current managed `AGENTS.md` block. |

Use `gitone --help` for the full command list, `gitone <command> --help` for
exact accepted arguments and `gitone --version` for the installed version.
Commands return `0` on success and `1` on failure. `show` returns `2` when the
requested path is absent from the selected source.

### Terminal presentation

Color and progress rendering are automatic. Redirected output, `TERM=dumb`
and `NO_COLOR` stay plain. Machine-readable output never contains presentation
codes, and GitOne strips control sequences from repository-provided text in
normal human output.

## The normal workflow

Once configuration is valid and the repositories are initialized, the daily
flow stays close to Git:

```bash
gitone status
gitone add -A
gitone diff --staged
gitone commit -m "Describe the change"
gitone push
```

GitOne groups output by repository and routes each path to its configured
owner. A commit touching several repositories creates one native commit in
each affected repository with a shared `GitOne-Group` trailer.

### Selecting changes

Paths are relative to the current directory. For `add`, `unstage` and `restore`
a directory selects every managed change below it; `.` and `..` are ordinary
directories here, so `..` selects the parent subtree. GitOne does not accept
absolute paths, literal globs or Git pathspecs. Shell-expanded globs work when
every resulting argument is a managed file or non-empty managed directory.

```bash
gitone add src/app.ts docs/USAGE.md
gitone add docs
gitone unstage docs/USAGE.md
gitone commit src/app.ts -m "Update app startup"
```

A commit without paths consumes the staged state of every affected
repository. A commit with paths commits the current working-tree versions of
exactly those known files and leaves unrelated staged changes staged. It never
accepts a directory or `.`.

You can delete a tracked file and remove its ownership rule in the same
change. While the deletion is pending, GitOne routes the missing file to the
repository that tracks it, including after staging. Stage both changes with
`gitone add -u`, or name both paths in a selective commit. Existing files still
require an ownership rule; conflicting repository assignments remain errors.

### Undoing changes

Unstage without touching the working tree:

```bash
gitone unstage src/app.ts
gitone restore --staged src/app.ts
```

Discard working-tree changes and restore the index version:

```bash
gitone restore src/app.ts
```

Discard staged and unstaged changes together, so the file matches the last
commit of the repository owning it again:

```bash
gitone restore --source=HEAD --staged --worktree src/app.ts
```

`--staged` and `--worktree` are the destinations; without either, the working
tree is the destination. The source is the owning repository's own HEAD as soon
as `--staged` is given and otherwise its index, and `--source HEAD` selects
HEAD explicitly. The flags come before the paths, each at most once and in any
order.

Every form except `--staged` alone destroys uncommitted changes in the selected
files. A selected path the source does not hold, such as a newly staged file,
is removed from the destination. GitOne does not restore untracked files or
remove directories, and it refuses a restored `.gitone.yml` that would add,
remove or rename a repository or change a remote; use `gitone reconfigure` for
that.

### Machine-readable status

Use JSON for tools and porcelain for simple line processing:

```bash
gitone status --json
gitone status --porcelain
```

Both formats start with schema version `1` and report repositories, changes and
issues deterministically. Ahead and behind are `null`/`?` until a local
remote-tracking branch exists.

Exit 0 means ordinary changes. Exit 1 has two causes and stdout tells them
apart: either the document was written and its `issues` array explains why the
project is unsafe, or GitOne refused before producing any output at all - no
configuration, nothing initialized, an unreadable project - and only stderr
carries the error code. Parse stdout when it is non-empty; treat empty output
on exit 1 as the refusal it is and read stderr.

The JSON document also contains `ignored` and `ignore_exceptions`. `ignored`
lists the project-relative paths the project `.gitignore` files hide, evaluated
by native Git. An entry ending in `/` is a collapsed directory and stands for
everything below it except the exact tracked paths in `ignore_exceptions`.
Porcelain omits both fields because ignored file names may contain its tab and
newline delimiters.

The VS Code extension uses only public GitOne commands. It does not inspect
`.gitone/` or invoke Git directly.

### Editor integration contract

Editor integrations use `status --json`, `vscode info --json` and `show`.
`show` returns the exact bytes stored in `HEAD` or the index; use its exit code
to distinguish missing content from a failed command.

`vscode info --json` reports the contract as `protocol`, currently `1`. An
integration checks it before anything else and refuses to run against a
different number. Protocol version `1` remains fixed for the pre-release
contract. Every array of the status document is required, so a status without
`ignored` or `ignore_exceptions` is rejected instead of presenting an
incomplete project state as complete. The VS Code extension colors the
reported ignored paths with VS Code's own
`gitDecoration.ignoredResourceForeground`; it never evaluates ignore rules
itself.

## Branches and remotes

### Branches

List project branches, create one in every repository, or delete a finished
one from every repository:

```bash
gitone branch
gitone branch feature/auth
gitone branch -d feature/auth
```

Branch creation does not switch. Repositories must have commits and agree on
their current branch.

`-d` (or `--delete`) deletes one local branch everywhere, and only when every
repository agrees that it is safe to lose. Each repository is judged on its
own, with native Git's own rule for `git branch -d`: the branch has to be
contained in its configured upstream, or in `HEAD` where no upstream is
configured. A branch that is merged in only part of the project is refused for
the whole project, and every blocking repository is named together with the ref
it was compared with.

Only local refs decide this. GitOne does not fetch first, so a configured
upstream this repository has never fetched stops the deletion instead of being
quietly replaced by a comparison with `HEAD` - run the `gitone fetch
<repository>` the message names and try again. No remote branch is ever deleted,
and there is no force option. If the branch is not merged, keep it until you
have deliberately integrated or published the work to its comparison ref.

The branch settings go with the branch, exactly like native Git. The refs, the
reflogs and those settings are recorded before the first branch disappears, so
an interrupted deletion is finished by `gitone recover` or completely undone by
`gitone abort`. A branch that was recreated or moved outside GitOne meanwhile
is never overwritten or deleted. Once an abort starts restoring branches,
finish it with `gitone abort`; `recover` refuses to restart deletion. If a
process stopped after recording deletion intent but the branch still exists,
GitOne cannot prove whether it was recreated and refuses both commands for
[manual resolution](#resolving-an-interrupted-branch-deletion). Preserve the
recovery data until every participating repository has been reconciled.

### Switching branches

```bash
gitone switch -c feature/auth
gitone switch feature/auth
```

`-c` (or `--create`) starts a new feature: it creates `feature/auth` in every
repository, each at the commit that repository currently has checked out, and
then switches the whole project to it. It refuses when any repository already
has the name or has no commit yet, so half a project is never left to be
created by hand.

Without `-c`, a repository that already has the branch keeps its own local
ref. A repository missing it gets a new branch created from its own fetched
`origin/feature/auth`, configured to track that ref. The start point is never
guessed from another repository and never taken from `HEAD`, and GitOne does
not fetch on its own. A repository with neither the branch nor a matching
`origin` ref stops the whole switch and is named with its next step: `gitone
fetch <repository>` when it has an `origin`, otherwise
[Repairing missing branches](#repairing-missing-branches).

GitOne refuses staged or unstaged changes and never stashes, merges or
discards work. Untracked files remain, unless checkout would overwrite one.

If the target branch changes `.gitone.yml`, GitOne previews the policy change
before checkout. Non-interactive callers must pass the acceptance flag named
by `gitone switch --help` and the refusal message.

Branch creation, upstream configuration and checkout are one recoverable
operation. A failure returns the project to its starting state, or leaves
recovery state for `gitone recover` and `gitone abort`. An undo removes only
the refs and upstreams this switch created; a pre-existing branch, a
pre-existing upstream and anything changed outside GitOne are kept.

### Fetching

```bash
gitone fetch
gitone fetch website
```

Fetch updates `origin` remote-tracking refs only. It does not merge, rebase or
change working-tree files. Multi-repository fetch is sequential: refs fetched
before a later failure stay updated.

### Pulling

```bash
gitone pull
gitone pull website
```

Pull performs fast-forward updates only. Before changing anything it checks
all selected repositories, their incoming trees and any incoming policy
change. Local staged or unstaged changes are refused. For diverged history, follow
[the manual repair procedure](#resolving-diverged-history).

Pull across several repositories is transactional for branches, indexes and
working-tree files. Fetched remote-tracking refs are not rolled back.

### Incoming policy changes

An incoming `.gitone.yml` or tracked `.gitignore` is untrusted input. GitOne
validates the projected configuration and checks the incoming trees against
it before `pull` or `switch` changes local files.

Safe changes are previewed once. Structural repository changes are handled by
`gitone reconfigure`; new unassigned incoming paths can be assigned only to
the repository that introduces them. Exact non-interactive flags are shown by
the command's `--help` and refusal message.

### Assigning new incoming paths

When new incoming paths are the only ownership problem, pull or switch can
propose exact assignments to the repository whose incoming commit contains
them. The configuration change is shown before it is written and remains
unstaged for review.

### Reconfiguring the repositories

Use reconfigure after deliberately editing repository names, remotes or the
repository set:

```bash
gitone reconfigure
gitone reconfigure --pull
gitone reconfigure --switch feature/auth
```

GitOne previews additions, retirements, renames and remote changes before
applying them. Renames must be declared explicitly; they are never inferred.
See the [reconfiguration reference](RECONFIGURATION.md) for the full safety
contract and non-interactive forms.

### Adopting history into an empty repository

A repository created without `origin` has no first commit. For new local work,
create owned files, stage them and commit normally. If an existing remote
already holds its history, do **not** create an unrelated first commit: pull
cannot join unrelated histories, and adding a remote with `reconfigure` alone
does not check out an already initialized repository.

For an empty secondary repository named `notes`, use the existing retirement
and addition workflow:

1. Run `gitone status` and `gitone log notes` to confirm that `notes` has no
   commits or staged changes. Move any local files it owns to a safe directory
   outside the project before continuing, and keep them until step 5. Leave
   the other repositories on one branch.
2. Save its configuration, then remove `notes` from `.gitone.yml` and from
   `.gitone.local.yml` if it appears there. At least one repository must remain;
   never remove the repository owning `.gitone.yml`.
3. Run `gitone reconfigure` and accept the retirement of the empty repository.
   Its metadata is retained under `.gitone/retired/`; no manual deletion is needed.
4. Restore the `notes` entry with its owned paths and the existing remote URL.
   Use `.gitone.local.yml` for private configuration. Run `gitone reconfigure`
   again. It now adds the repository, validates the incoming files and checks
   out the remote branch matching the project's current branch.
5. Run `gitone status` and `gitone doctor`. Compare any saved local files with
   the checked-out versions before copying edits back. Review and commit any
   public configuration changes separately.

The remote must contain that branch and only paths assigned to `notes`.
For a project consisting of a single empty repository, start in a new
location with `gitone clone` if the remote commits `.gitone.yml`; keep the old
folder until you have compared and preserved its local files.

### Manual Git repairs

GitOne deliberately has no merge, rebase or per-repository branch creation.
The following are explicit human repair procedures, not automatic fallbacks
for ownership errors. Native Git bypasses GitOne's checks. Run the commands
from the project root, replace `website` and branch names with the reported
ones, and do not run other GitOne operations while a merge is unresolved.
Never use native Git to push around a GitOne refusal.

#### Repairing missing branches

This is only needed for a start point GitOne does not choose itself. A new
branch everywhere is `gitone switch -c feature/auth` only when no repository
already has that name. If any repository already has it, do not retry with
`-c`: repair only the missing branches. A repository whose
`origin/feature/auth` was already fetched is created and tracked by
`gitone switch feature/auth` without any manual step.

Start with `gitone branch`. For each missing repository with an `origin`,
fetch and let `gitone switch` take the branch from there:

```bash
gitone fetch website
gitone switch feature/auth
```

Only when no remote branch exists (including a repository without `origin`,
where there is nothing to fetch) choose the intended starting commit
explicitly. For a repository that should keep its current committed files on
the new branch:

```bash
git --git-dir=.gitone/repositories/website branch feature/auth HEAD
```

This requires an existing commit and only creates the missing ref; it does
not check out files. After repairing each missing repository, run
`gitone switch feature/auth`. GitOne validates the target trees before checkout.
Finish with `gitone status` and `gitone doctor`.

#### Resolving diverged history

Use this when local and remote commits have diverged but share a common
ancestor. First finish or safely save local edits, then require a clean
`gitone status` and run `gitone fetch website`. Inspect both sides:

```bash
git --git-dir=.gitone/repositories/website log --oneline --graph --left-right HEAD...origin/main
git --git-dir=.gitone/repositories/website diff --stat HEAD...origin/main
```

Review the incoming paths and any configuration changes before merging. If
this also changes repository structure or transfers ownership, stop and
coordinate that change using the [reconfiguration reference](RECONFIGURATION.md).
For an ordinary content merge on `main`:

```bash
git --git-dir=.gitone/repositories/website --work-tree=. merge --no-edit origin/main
```

If Git reports conflicts, edit the named files, stage **only those resolved
paths** in this repository and finish the native merge:

```bash
git --git-dir=.gitone/repositories/website --work-tree=. add -- src/conflicted-file.ts
git --git-dir=.gitone/repositories/website --work-tree=. commit --no-edit
```

To cancel an unresolved merge instead:

```bash
git --git-dir=.gitone/repositories/website --work-tree=. merge --abort
```

Then run `gitone status` and `gitone doctor`. Publish the resolved history
through `gitone push website`, which checks the outgoing commits and tree.
Do not use `--allow-unrelated-histories` or force-push to make a migrated
snapshot replace an existing remote. Migration starts a new history: use an
empty remote for that history, or keep working from a clone of the original.

### Push safety and partial failures

```bash
gitone push
gitone push website
gitone push --yes
```

Before the first remote changes, GitOne checks push policy, `origin`,
connectivity, fast-forward safety and ownership of outgoing history and the
resulting tree. A failed preflight pushes nothing.

Pure deletions do not require a current ownership rule: removing a published
file and its rule can be committed and pushed together. Added or changed
content in every outgoing commit is still checked, even if a later commit
deletes it. Removing a file does not remove its content from Git history.

Push across several remotes cannot be atomic. GitOne records each result; if
a later remote fails, earlier successful pushes remain published. Rerun push
to publish the rest.

## Creating or importing a project

### Cloning a project

```bash
gitone clone git@github.com:example/website.git
gitone clone git@github.com:example/website.git my-project
```

The bootstrap repository must commit `.gitone.yml`. Clone validates that
configuration, adopts the bootstrap repository with its history, initializes
the other configured repositories and checks out their default branches.

The destination must not exist. GitOne builds the project in a temporary
directory and publishes it only after validation succeeds.

`.gitone.local.yml` is never committed, so repositories that exist only there
cannot be discovered by clone. Add them locally afterwards and run
`gitone reconfigure`.

### Migrating an existing repository

From a directory with a root `.git/` and a valid `.gitone.yml`:

```bash
gitone migrate
```

Migration creates empty managed repositories and leaves your files in place.
It stages and commits nothing, and does not split the old history. The original
`.git/` is copied to a verified backup below `.gitone/migration-backup/` before
the root repository is removed.

Use `gitone migrate --yes` only after reviewing the preview. On failure,
GitOne restores the original root repository or leaves recovery instructions.

### What only lives in the backup afterwards

The managed repositories start without commits or staged files. Earlier commits,
branches, tags, reflogs and repository-local settings remain only in the
retained backup.

### Listing, inspecting and restoring a backup

```bash
gitone backup list
gitone backup git -- log --oneline
gitone backup restore
```

`backup git` supports the read-only native Git commands `log`, `show`, `diff`
and `status`. Restore copies the selected backup back to `.git/`; it never
deletes the retained backup and refuses to overwrite an existing root `.git/`.

## Recovery

### Locking, recovery and abort

Only one mutating GitOne command runs at a time. If an operation is
interrupted, subsequent writes stop and tell you to choose:

```bash
gitone recover
gitone abort
```

`recover` finishes the recorded operation. `abort` restores its recorded
starting state. Both refuse to overwrite files, indexes or refs changed by
something else after the interruption.

For interrupted push, both commands only read remote refs and report what was
published. They cannot roll back a remote update. `gitone doctor` reports lock
and recovery state without changing it.

### Repairing an unattributable lock

The lock file records the process, the host and the command that took it.
GitOne reclaims one whose process has ended on this machine and waits for one
whose process is alive. An empty file records no owner at all and is reclaimed
the same way, so it needs nothing from you. A file naming **another host**, or
one whose contents **cannot be read**, is the one case that is neither: there
is no process here to wait for, so waiting never ends. `gitone doctor` reports
that state as its own and changes nothing:

```text
Locks     ✗
          LOCK001 the lock names no process this machine can check:
          gitone commit, PID 4711 on host other-machine
          no operation can be confirmed running, so waiting for it does not end
```

Removing the file is then the repair, and it is safe only once nothing owns it.
Establish that first, in this order:

1. Make sure no GitOne command is running against this project - on this
   machine and on every other machine that reaches the same directory through a
   network, shared or synchronised filesystem. A lock naming another host
   usually means exactly that: a second machine sees this project.
2. Read what it claims with `cat .gitone/lock` and keep a copy of the line. It
   is the only record of which command was interrupted.
3. Run `gitone doctor` again and confirm it still reports the lock as
   unattributable rather than as a running operation.
4. Only then `rm .gitone/lock`. Remove nothing else, and leave
   `.gitone/recovery/` untouched.
5. Run `gitone doctor` once more. If it now reports `REC001`, the recorded
   command was interrupted: finish it with `gitone recover` or undo it with
   `gitone abort` as described above. Never delete `.gitone/recovery/` by hand.

If `doctor` reports a running operation instead, none of this applies: wait for
it. Do not move or remove lock files to bypass a running operation.

### Resolving an interrupted branch deletion

Use this only when an interrupted `gitone branch -d` refuses recovery or abort
because refs or branch settings changed, or deletion intent is ambiguous.
Do not delete `.gitone/recovery/` or edit its state to bypass the refusal.
The record covers **every participating repository**, not just the one named
in the first error.

This is a human repair procedure. Stop editors and other processes that write
Git state; do not run GitOne writes during the repair.

1. Make a private backup of the entire project outside its working directory,
   including hidden files, `.gitone/`, `.gitone.local.yml` and uncommitted
   files. Verify the backup before changing anything. It contains private data.
2. Read `.gitone/recovery/state.json`. Confirm it is a branch deletion record
   (`command: "gitone branch"` with a `deleted` array). For **each** entry,
   record its `name`, full `commit`, `settings` and saved `reflog`
   (base64-encoded), along with the top-level `branch`. These are the
   pre-deletion snapshots; progress flags do not prove who now owns a ref.
3. Inspect each recorded repository, including ones not named in the error.
   Replace `website` and `feature/auth` below with the recorded names:

   ```bash
   git --git-dir=.gitone/repositories/website show-ref --verify refs/heads/feature/auth
   git --git-dir=.gitone/repositories/website reflog show refs/heads/feature/auth
   git --git-dir=.gitone/repositories/website config --local --get-regexp '^branch\.'
   ```

   A missing ref or reflog can be the result of the interrupted deletion.
   Compare the current ref, reflog and settings with the saved snapshots.
4. Decide and reconcile the final state of **every** recorded branch. To undo,
   restore missing refs at their full recorded commits and restore their
   branch settings and reflogs. Preserve any newer ref, reflog or settings
   separately before replacing them; do not assume a ref at the same commit
   is the original. To finish deletion, first inspect and preserve any newly
   created work, then explicitly remove only the branches and settings you
   chose to delete. Do not change HEAD, indexes or working files. If any
   comparison or restoration is unclear, keep the recovery record and
   [ask for help](https://github.com/devidevio/gitone/issues) with a redacted
   description; do not upload the project backup or raw recovery data.
5. Check every recorded repository against those decisions. Only when all
   refs, settings and reflogs have the intended state, move the entire
   `.gitone/recovery/` directory into your external private backup under a
   new, unused name. **Archive it; do not delete it.** This acknowledges the
   completed manual repair and removes the pending-operation block. Do not
   move or remove lock files to bypass a running operation.
6. Run `gitone branch`, `gitone status` and `gitone doctor`. Confirm the branch
   state and that no recovery is pending. Keep the backup and archived record
   until you have verified normal work can resume.

### Error codes

Every failure starts with a stable error code. Use the
[GitOne error reference](https://gitone.io/errors) for its meaning, usual
cause and next action. The original message remains authoritative for the
exact path, repository or command involved.

Useful diagnostics:

```bash
gitone doctor
gitone repo validate
gitone status --json
gitone <command> --help
```

## Instructions for AI agents

`gitone setup` can add a managed GitOne block to the root `AGENTS.md`.
Maintain it non-interactively with:

```bash
gitone agents
gitone agents update
```

Only the marked block is managed; surrounding project instructions are
preserved. The block tells agents to use `gitone`, respect ownership and avoid
cross-repository dependencies. Read the
[canonical agent instructions](https://github.com/devidevio/gitone/blob/main/internal/agents/instructions.md)
when you need the complete block without running the command.

## Security boundary

GitOne enforces path ownership:

- every relevant path belongs to exactly one repository
- protected and reserved paths are refused
- outgoing commits and resulting remote trees are checked before push
- incoming trees and policy changes are checked before checkout
- unsafe symbolic links are refused
- unknown commands and flags are never forwarded to Git
- repository-provided text cannot emit terminal control sequences in normal
  human-readable output

GitOne does not inspect file contents. It is not a secret scanner, data
classifier or access-control boundary between files on the same machine. A
secret inside an allowed public path can still be published.

GitOne also trusts the local Git binary, Git configuration, credential
helpers, hooks and Git LFS filters. Multi-remote push and fetch are not atomic.

Symbolic links are supported only as safe internal aliases: relative, inside
the project, resolving without another link, and owned by the same repository
as their target. Absolute, escaping, dangling, chained, cyclic,
cross-repository and reserved-path links are refused.

Submodules, arbitrary Git command forwarding, history splitting during
migration, Windows and general Git-compatible symlink support are not
implemented. See [Deferred](https://github.com/devidevio/gitone/blob/main/README.md#deferred) for the current scope.
