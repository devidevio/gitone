# Incoming repository reconfiguration

This is the accepted design for the one thing `gitone pull` and
`gitone switch` still refuse outright: an incoming `.gitone.yml` that adds,
removes or renames a repository, or that changes a configured remote.

**This design is implemented.** Every task in
[Implementation tasks](#implementation-tasks) has landed, so this file
describes shipped behavior. `gitone pull` and `gitone switch` keep their
structural refusal exactly as
[Incoming policy changes](USAGE.md#incoming-policy-changes) documents it, and
that refusal now names the `gitone reconfigure` command that reviews and
applies the change. Nothing here weakened them.

Related contracts: [Configuration](USAGE.md#configuration),
[Cloning a project](USAGE.md#cloning-a-project), [Pulling](USAGE.md#pulling),
[Switching branches](USAGE.md#switching-branches),
[Locking, recovery and abort](USAGE.md#locking-recovery-and-abort),
[Security boundary](USAGE.md#security-boundary).

## 1. The problem

A GitOne project has two halves that must agree:

- **the configuration** - the effective `.gitone.yml` plus `.gitone.local.yml`,
  which says which repositories exist, what each owns and where each pushes;
- **the metadata** - the native Git repositories below `.gitone/repositories/`,
  their configured remotes, refs, indexes and the one shared working tree.

Every existing command changes at most one of them. `init`, `setup` and
`migrate` create metadata for a configuration that has no metadata yet. `pull`
and `switch` replace the configuration file with a version another commit
carries, but they never touch metadata. That gap is the whole problem: an
incoming commit can change the half GitOne does not reconcile.

Applying such a commit today would produce, immediately and silently:

| Incoming change | Result without reconciliation |
| --- | --- |
| repository added | `REPO001 ... is not initialized`, and every mutating command stops |
| repository removed | its paths become `PATH001`, and every mutating command stops |
| repository renamed | both of the above at once, with the old history unreferenced |
| `origin` URL changed | `doctor` reports `remote "origin" is <old>, not the configured <new>`; `fetch`, `pull` and `push` keep using the old URL |
| additional remote changed | the same disagreement, silently, because only `origin` is used by commands |

There is also a second, quieter gap that has nothing to do with remotes: a
person who edits `.gitone.yml` by hand today hits the same wall. `gitone init`
creates a repository the edit added, but nothing fetches it, nothing checks it
out, and nothing repairs a changed URL. The current refusal message tells the
user to "apply such a change to your local `.gitone.yml` instead", which walks
them straight into that wall.

Both gaps are the same missing operation: **make the metadata match the
effective configuration**. This design adds that operation once and uses it for
locally edited and for incoming configuration alike.

## 2. The decision

**One new command, `gitone reconfigure`, is the only command that creates,
retires or renames a managed repository or changes a configured remote.**

`pull` and `switch` keep their hard refusal. Their message changes from a dead
end into the exact command to run.

Why not extend `pull` and `switch`:

- `pull` is fast-forward-only and touches no metadata. Adding repository
  creation, retirement and first contact with a new host to the command people
  run several times a day makes the most routine command the most dangerous
  one.
- `switch` performs no network access at all. A structural change can require
  fetching a brand-new host, which changes the command's contract for every
  user who is not reconfiguring anything.
- `--accept-config-change` is documented to accept exactly one reviewed class
  and nothing else. Tasks 0052 and 0053 both state this. Widening it is the one
  thing this design must not do.
- The review is shaped differently. A path-pattern change is a field diff. A
  structural change is a per-repository plan with URLs, hosts, credentials and
  network steps, and it deserves its own preview.

Everything `reconfigure` needs beyond that already exists and is reused
unchanged: `internal/policy` for the projected policy, the ignore diff and the
existing-path exposure check; `internal/assign` for new incoming paths;
`internal/repository` for creation, tree validation and index handling;
`internal/lock` for the lock and the recovery journal; `pull`'s fast-forward
and `switch`'s checkout for moving branches.

## 3. Command surface

```bash
gitone reconfigure                    # apply the local configuration to the repositories
gitone reconfigure --pull             # adopt the incoming .gitone.yml from origin, then apply
gitone reconfigure --switch <branch>  # adopt the target branch's .gitone.yml, then apply
```

The three forms differ **only in where the configuration comes from**. The
plan, the preview, the confirmation, the validation, the transaction and the
recovery are identical.

| Form | Configuration source | Branch the project ends on |
| --- | --- | --- |
| plain | the `.gitone.yml` and `.gitone.local.yml` already in the working tree | unchanged |
| `--pull` | the `.gitone.yml` in the fast-forward target of the repository whose tree carries it | unchanged |
| `--switch <branch>` | the `.gitone.yml` in `<branch>` of the repository whose tree carries it | `<branch>` |

`--pull` and `--switch` are named after the command whose refusal sends the
user here, so the refusal can print the exact next command:

```
PULL001 the incoming policy adds, removes or renames a repository or changes a
configured remote (repository "public", origin/main at 4352cdf):

  repositories.docs      (absent) -> configured
  repositories.public.remotes.origin
                         git@github.com:example/public.git ->
                         git@git.example.com:example/public.git

Review and apply it with: gitone reconfigure --pull
No branch, HEAD, index, working-tree file or configuration was changed.
```

Flags:

| Flag | Meaning |
| --- | --- |
| `--accept-repository-change` | the one confirmation of the structural plan. Non-interactive equivalent of answering `y`. Accepts nothing else. |
| `--accept-config-change` | unchanged meaning from tasks 0052 and 0053. Required in addition when the same change also alters paths, protected paths, push policy, visibility, the default branch or a tracked `.gitignore`. |
| `--accept-new-paths` | unchanged meaning from tasks 0054 and 0055, when the incoming trees also carry paths nobody owns yet. |
| `--rename <old>=<new>` | declares that the disappeared name `<old>` and the appeared name `<new>` are the same repository. Repeatable. See [4.4](#44-repository-rename). |

There is no `--yes` and no flag that accepts every class at once. Each flag
keeps the exact meaning it has elsewhere, so a person who has read one command
has read them all.

`gitone reconfigure <repository>` is `CLI001`. See [8. Partial
selection](#8-partial-selection).

## 4. What a structural change is, and what each one does

Structural changes are computed from the **effective** configuration, the same
way `config.Differences` already marks them today: an added or removed
repository name, and any change below `remotes`. A change that
`.gitone.local.yml` overrides away is therefore not a change at all, and
nothing happens.

### 4.1 Remote URL change

**Preflight.** The repository exists, its metadata is valid, and the new URL is
not identical to the old one. An incoming URL that embeds a password
(`scheme://user:password@host/...`) is **refused outright**, with or without any
flag: such a URL is committed to a repository other people clone, so accepting
it is a credential disclosure by construction. A `user@host` form without a
password is fine and is what SSH normally looks like.

**Review.** The preview names the repository, the remote, and the old and new
URL verbatim, plus one line whenever the host or the scheme changes:

```
REMOTES

  public / origin
    before: git@github.com:example/public.git
    after:  git@git.example.com:example/public.git
    host changes github.com -> git.example.com; your credential helper and SSH
    configuration will be asked for the new host
```

**Application.** `git remote set-url`. Nothing else. The old URL is recorded in
the journal first.

**After application.** The repository is fetched from the new URL. The commit
the command plans to leave checked out must be an ancestor of the new
`origin/<branch>`: the current commit for plain `reconfigure`, or the target
commit for `--pull` and `--switch`. A new URL with an unrelated history is
refused, and the repair is to retire the repository and add it under a new name
rather than to silently strand the old history.

**Note for `origin` of the `.gitone.yml` holder.** `gitone clone` requires the
holder's configured `origin` to be exactly the URL that was cloned. Changing it
means new clones must use the new URL and clones from the old URL fail with
`CLONE001`. The preview says so in one line.

**Dropping a remote is not applied.** A remote removed from the configuration
stays configured in native Git and is reported once as left in place, because
`git remote remove` also deletes that remote's tracking refs. This is a
deliberate, stated asymmetry: GitOne guarantees that every **configured** remote
exists with exactly the configured URL, not that no other remote exists. No
GitOne command uses a remote that is not configured, so nothing is published
through it; removing it by hand is a one-line `git --git-dir=... remote remove`
that the report prints.

### 4.2 Repository addition

**Preflight.** The name is valid and has no existing
`.gitone/repositories/<name>` entry. An incoming addition must also have no
repository of that name in `.gitone.local.yml`; a repository defined only in
the local file is allowed for plain `reconfigure` (see
[5](#5-authority-of-gitonelocalyml)). Its `paths` do not collide with another
repository's ownership of a concrete existing path, do not cover a reserved or
protected path, and the projected configuration validates as a whole.

**Review.** The preview names the repository, its visibility, its remote and its
patterns, and - because a wide pattern on a remote-controlled repository is the
sharpest edge in this whole design - the number of **existing local paths** each
pattern would claim:

```
REPOSITORIES

  + docs   public   git@github.com:example/docs.git
           paths: docs/**   (claims 12 existing local paths)
```

**Application.**

1. create `.gitone/repositories/<name>` with the project working tree, exactly
   as `init` does, and with `--initial-branch` set to **the branch the project
   is currently on**, not to `default_branch`. The project may be on a feature
   branch, and every repository must agree on one branch.
2. add its configured remotes.
3. with an `origin`: fetch, then require `origin/<branch>` for that same branch.
   A remote without that branch is refused, naming the branch and the URL.
4. without an `origin`: the repository stays unborn and is reported with its
   next step, exactly as `clone` already reports such a repository. Its paths
   are owned but untracked, which is the normal state after `init`.
5. check out the target commit. A checkout that would overwrite an existing
   working-tree file is refused before anything is written, exactly as `switch`
   refuses one.

**Existing local files under the new repository's patterns.** A file that is
here today and that the incoming tree does **not** carry stays where it is and
becomes a managed untracked file of the new repository. That is a new exposure,
so the existing task 0052 refusal applies unchanged and stops the whole
command: a projected policy that puts an existing local **ignored or
unassigned** path into a repository is refused, with or without any flag. Every
affected path is named.

### 4.3 Repository retirement (removal)

**Nothing is ever deleted.** A repository that the projected configuration no
longer names is *retired*: its complete metadata directory is moved to

```
.gitone/retired/<UTC timestamp>-<name>/
```

beside a `retired.yml` recording the name, the configured remotes, the branch,
the commit and the time. The directory keeps the same depth below the project
root, so the recorded `core.worktree` stays valid and the retired repository can
still be inspected with plain `git --git-dir=...`. `.gitone/retired/` is inside
the always-ignored `.gitone/`, is never owned, staged or committed, and is
never cleaned up automatically. Removing it is a deliberate human action.

**Refused, with or without any flag:**

- the repository has **commits that are not on its `origin`**, or has no
  `origin` at all and has any commit. Retiring it would strand work that exists
  nowhere else. The message names the commits and the two repairs: push them,
  or declare a rename with `--rename` if that is what this is.
- any **relevant** path the repository owns today would be left unassigned. The
  message lists every such path and the three repairs: let another repository
  own it in the same configuration, ignore it, or remove it. Otherwise the
  command would succeed and leave a project where every mutating command stops
  with `PATH001`.

**Working-tree files are never touched.** The files the retired repository
tracked stay on disk byte for byte. What changes is only who may commit them.

**Application order.** Retirement is the **last** step, after the project is
already consistent, so a failure there cannot leave a half-configured project.
See [7](#7-transaction-recovery-and-abort).

### 4.4 Repository rename

**GitOne never infers a rename.** A name that disappears and a name that appears
in the same change are a retirement plus an addition unless the person says
otherwise:

```bash
gitone reconfigure --pull --rename notes=journal
```

Without `--rename`, the retirement rule of [4.3](#43-repository-retirement-removal)
applies and unpublished work stops the command. That refusal is what makes the
missing flag safe rather than merely inconvenient.

With `--rename old=new`:

- `old` must disappear and `new` must appear in the same projected
  configuration, and neither may be named by another `--rename`;
- `.gitone/repositories/<old>` is **moved** to `.gitone/repositories/<new>`. The
  rename itself performs no fetch, clone, checkout or history copy. Local
  commits, stash-free index, reflog and hooks all survive because the bytes
  never move off disk;
- the remotes of `new` are then reconciled like any other remote change, so a
  rename that also changes the URL is one reviewed operation, not two, and the
  URL change performs its normal fetch and ancestry check;
- the committed tree and index of `new` must validate against the projected
  matcher. A rename that is really an ownership change is caught here, not
  after the fact.

The preview shows it as one line, never as a removal plus an addition:

```
REPOSITORIES

  ~ notes -> journal   private   remote unchanged
```

### 4.5 Ownership moving between repositories

This is not a structural change by itself, but a structural change is where it
usually appears, and it has a rule that surprises people, so it is stated here.

A GitOne repository's tree may only contain paths it owns. So moving `docs/**`
from `private` to `public` requires **both** repositories' incoming commits to
agree: `private` must have removed the files and `public` must have added them.
GitOne validates the complete projected tree set against the projected matcher
before applying anything, so a one-sided move is refused, naming the path, the
repository still carrying it and the repository that now owns it.

The practical consequence: an ownership move is something the **producer** of
the configuration does in one coordinated push. A plain `gitone reconfigure`
against a hand-edited local file therefore cannot move ownership - the local
trees have not changed - and refuses with exactly that message.

A move that takes a path from a `private` repository into a `public` one gets
its own preview section, because it is the change most worth reading twice:

```
OWNERSHIP MOVED TO PUBLIC

  docs/internal/pricing.md   private -> public
```

`rules.protected_paths` still wins over any ownership, so a protected path can
never be moved into a repository at all.

### 4.6 Visibility and push policy

Unchanged from today: both are ordinary reviewed fields, not structural, and
they are accepted with `--accept-config-change`. `.gitone.local.yml` can still
only tighten them - a local `push: disabled` and a local
`require_clean_worktree: true` survive any incoming change, and an incoming
`allowed` never lifts them. `visibility` is reported and, per
[4.5](#45-ownership-moving-between-repositories), drives the private-to-public
preview section; it grants and denies nothing by itself.

### 4.7 Local-only repositories

A repository that exists only in `.gitone.local.yml` is invisible to every
incoming configuration and is **never** created, retired, renamed or
re-pointed by `--pull` or `--switch`. A plain `gitone reconfigure` does
reconcile it, because that is the person's own file.

## 5. Authority of `.gitone.local.yml`

The local file stays the protection layer and the trust anchor. Three rules,
two of which already hold and one of which is new:

1. **Never read from repository content.** Unchanged. No incoming commit can
   write, weaken or bypass it.
2. **Overrides win, so an overridden incoming change is no change.** Unchanged
   merge semantics: a locally defined `remotes.origin` beats the incoming one,
   locally defined `paths` replace the public list, a local `push: disabled`
   and a local `require_clean_worktree: true` survive. The preview prints such
   an entry as `overridden by .gitone.local.yml, unchanged` rather than hiding
   it, so nobody has to reason about why the diff did nothing.
3. **A name collision is refused.** *(new)* An incoming `.gitone.yml` that adds
   a repository whose name `.gitone.local.yml` already defines is refused with
   or without any flag. Accepting it would silently attach a remote-controlled
   remote and visibility to a private repository defined locally. The message
   names the repository and both files, and the repair is to rename one of
   them.

An incoming configuration that *removes* a repository the local file also
defines is not a retirement: the repository still exists, sourced from the local
file alone. Depending on what the local file defines, this may be no effective
change at all, and then nothing happens.

## 6. Phase order, network ordering and the preview

The order exists to satisfy one rule: **a changed or new remote is never
contacted before its exact old and new values have been shown and accepted.**

| # | Phase | Network | Local writes |
| --- | --- | --- | --- |
| 1 | acquire the project lock; refuse on `REC001` | no | no |
| 2 | validate the current project: metadata, working tree, one shared branch | no | no |
| 3 | read the source configuration - working tree, fetched `origin`, or target branch | **only currently configured URLs**, and only for `--pull` | no |
| 4 | merge the unchanged `.gitone.local.yml`, validate the projected configuration, compute the structural plan and the ordinary policy review of task 0052 | no | no |
| 5 | apply every hard refusal of this design and of tasks 0052-0055 | no | no |
| 6 | print the complete preview and ask **once**, default `No` | no | no |
| 7 | reconcile metadata: create added repositories, move renamed ones, add and re-point remotes | no | **yes, journalled** |
| 8 | fetch every repository that has an `origin` | **yes, new URLs now allowed** | refs only |
| 9 | validate each planned target commit against its new `origin/<branch>` and the complete projected tree set against the projected matcher, including added repositories, links, reserved and protected paths, and the checkout overwrite check | no | no |
| 10 | apply: fast-forward or check out every repository to its target commit; update the ignore entries | no | **yes, journalled** |
| 11 | retire removed repositories | no | **yes, journalled** |
| 12 | release the lock and report | no | no |

Phase 3 is the only network access before the confirmation, and it uses
exclusively the URLs already stored in native Git - the ones the user has been
fetching from all along. A brand-new host is first contacted in phase 8, after
phase 6 accepted it.

The preview is one ordered block, printed in full before the single question:

```
Incoming repository reconfiguration (origin/main at 4352cdf)

REPOSITORIES

  + docs     public    git@github.com:example/docs.git
             paths: docs/**   (claims 12 existing local paths)
  ~ notes -> journal   private   remote unchanged
  - scratch  private   git@internal.example.com:ops/scratch.git
             metadata is retired to .gitone/retired/, never deleted
             12 commits, all present on origin
             its paths are owned by "journal" in this configuration

REMOTES

  public / origin
    before: git@github.com:example/public.git
    after:  git@git.example.com:example/public.git
    host changes github.com -> git.example.com; your credential helper and SSH
    configuration will be asked for the new host
    this repository owns .gitone.yml, so new clones must use the new URL

OWNERSHIP MOVED TO PUBLIC

  docs/internal/pricing.md   private -> public

NETWORK

  docs will be fetched from git@github.com:example/docs.git
  public will be fetched from git@git.example.com:example/public.git

No existing local path changes its ignored, relevant or owned state.

Accept this repository reconfiguration? [y/N]
```

When the same change also carries an ordinary policy change or new unassigned
paths, those previews are printed too, in the documented order - configuration,
ignore rules, affected paths, then repository reconfiguration, then new path
assignments - and each keeps its own question and its own flag.

## 7. Transaction, recovery and abort

One lock, one journal, the same `.gitone/recovery/state.json` envelope every
other operation writes, with `command: "reconfigure"` and its own payload.

Recorded **before the first write of phase 7**:

- for every configured repository: name, branch, HEAD commit, a copy of its
  index, and every configured remote name with its current URL;
- the bytes of `.gitone.yml` as they are now;
- the complete ordered plan: creations, renames, remote changes, target
  commits, retirements.

Progress is recorded after every step, so `recover` and `abort` always know
exactly which prefix of the plan happened.

| Failure in | Result |
| --- | --- |
| phases 1-6 | nothing was written. The command fails with `RECONF001` and the message ends with the usual unchanged-project line. |
| phase 7 | rolled back completely: a created repository directory is removed, a moved directory is moved back, a changed URL is set back. All of it is metadata this command produced seconds ago under the lock. |
| phase 8 | rolled back like phase 7. Fetched remote-tracking refs stay, exactly as they do after a failed `pull`; they are a cache and the next run fetches again. |
| phase 9 | rolled back like phase 7. This is the common case for a bad incoming configuration and it must cost the user nothing. |
| phase 10 | the repositories already moved are returned to their recorded commits and indexes and `.gitone.yml` is restored, exactly as `pull` and `switch` do today. If that restoration is not safe, the journal stays behind. |
| phase 11 | **the project is already consistent.** The command still fails with `RECONF001`, names the metadata directory that could not be moved and the one `mv` that finishes it, and says explicitly that no further GitOne action is required. |

`gitone recover` finishes the recorded plan from the recorded point: the
remaining fetches, checkouts and retirements. `gitone abort` undoes it: branches
and indexes back to the recorded commits, `.gitone.yml` back to the recorded
bytes, renamed directories moved back, changed URLs set back.

`abort` **never deletes a repository that has content it did not record.** A
repository this command created is removed only when its refs are exactly the
ones the journal recorded; if anything else is there - somebody ran `git` by
hand between the interruption and the abort - it is retired to
`.gitone/retired/` instead. The rule is the same one that governs the whole
design: nothing disappears without a separate, explicit human action.

Both commands refuse when a repository, an index or `.gitone.yml` changed
outside GitOne since the interruption, and print the concrete manual steps
instead of overwriting work they did not make. This is the existing behavior of
`recover` and `abort` and it is not changed.

A crash between two journal writes is indistinguishable from a crash during
one, because every write is the atomic rename `lock.Save` already performs.

## 8. Partial selection

**`gitone reconfigure` has no repository target.** A structural change redefines
the set every path is validated against, so reconfiguring half a project has no
meaning - and the half-states it would produce are exactly the ones this design
exists to prevent. `gitone reconfigure <name>` is `CLI001` with the accepted
form.

A `gitone pull <name>` whose single target carries a structural change is
refused like any other, and the refusal names the untargeted
`gitone reconfigure --pull`.

## 9. Non-interactive use and machine-readable failures

- Every refusal starts with `RECONF001`, one code for one command, matching the
  existing table.
- Without a terminal and without `--accept-repository-change`, the command fails
  with `RECONF001` rather than asking, exactly as `push` fails with `PUSH001`.
- Every refusal names: what would have changed, which repository, where the
  configuration came from (working tree, remote ref plus commit, or branch plus
  commit), the exact cause, and one concrete repair.
- Every refusal ends with the unchanged-project line the other commands print.
- `gitone doctor` gains the next command in the two messages that are today a
  dead end: `is not initialized` and
  `remote "origin" is <actual>, not the configured <configured>` both name
  `gitone reconfigure`.
- `gitone repo list` reports retired repositories, so `.gitone/retired/` is
  discoverable without knowing the directory exists.
- `gitone status --json` is unchanged. A project whose metadata does not match
  its configuration already surfaces as a `REPO001` entry in `issues`; only its
  guidance text improves.

## 10. Compatibility

**The schema does not change.** A structural change produces an ordinary
`version: 1` `.gitone.yml`. Bumping the version would make older GitOne fail
every command with `CONFIG003`, including read-only ones, for a change it could
otherwise ignore.

**How an older GitOne fails.** A GitOne from the 0052/0053 era that pulls or
switches into a structural change hits the existing hard refusal - "incoming
repository reconfiguration is not supported yet" - names the repository and the
fields, and leaves the project untouched. That is the correct failure: closed,
loud and reversible by upgrading. No older version can half-apply a structural
change, because no older version applies one at all.

**Clone is unaffected in principle** - it starts in an empty destination and the
committed configuration is the bootstrap source of truth - with one consequence
worth stating: after the `origin` of the `.gitone.yml` holder changes, new
clones must use the new URL. Cloning from the old URL fails `CLONE001` on the
existing rule that the holder's configured `origin` must equal the cloned URL.
The preview says so before the change is accepted.

**A repository added by an incoming configuration is cloned by everybody
else's next `gitone clone`** automatically, because clone already initializes
and fetches every repository of the committed configuration. Retired
repositories simply stop appearing in new clones.

## 11. Threat model

| Threat | Mitigation |
| --- | --- |
| A compromised remote adds a repository with `paths: **` pointing at an attacker host, to exfiltrate the whole project. | Nothing is contacted before the preview. The preview names the URL verbatim, the host change, and the count of existing local paths each pattern claims. The question defaults to `No`. Publishing still needs a separate `gitone push`, which has its own confirmation and its own `--yes`. |
| A compromised remote re-points an existing `origin` at an attacker host to capture the next push. | The URL change is shown before/after with the host change called out. After the change the planned target commit must still fast-forward onto the new `origin/<branch>`, so an unrelated repository is refused rather than adopted. |
| An incoming URL carries credentials, which are then committed to a repository other people clone. | An incoming URL that embeds a password is refused outright, with or without any flag. |
| An incoming change quietly attaches a remote to a private repository defined in `.gitone.local.yml`. | A name collision between an incoming repository and a locally defined one is refused. Local definitions always win for the fields they set, and the preview prints overridden entries instead of hiding them. |
| Private content becomes publishable through an ownership move. | An ownership move needs both repositories' trees to agree, so it cannot happen one-sidedly. A private-to-public move gets its own preview section. A protected path is never owned at all. An existing **ignored or unassigned** local path that would become managed is refused, with or without any flag - unchanged from task 0052. |
| A "rename" that is really a removal strands unpushed work. | Retiring a repository with commits that are not on its `origin` is refused. A real rename is declared with `--rename` and moves the directory, so the rename itself fetches nothing and loses nothing. |
| A partial reconciliation leaves a project that cannot be used or repaired. | One lock, one journal, a fixed phase order in which every failure before phase 10 rolls back completely, retirement last, and deterministic `recover`/`abort`. |
| Interrupting the command destroys metadata. | Nothing is ever deleted. Retirement moves. `abort` removes only a directory this command created whose refs are exactly the recorded ones, and retires it otherwise. |
| Repository content drives the terminal through a repository name, a URL or a Git error in the preview. | Unchanged: everything printed goes through the existing `internal/ui` sanitizing, and repository names are already validated against `[a-z][a-z0-9_-]*`. |

Out of scope for this design, unchanged from the [security
boundary](USAGE.md#security-boundary): GitOne never reads file contents, is not
an access-control boundary, and trusts the local Git binary, configuration,
credential helpers and hooks.

## 12. What stays refused

Even with every flag:

- an incoming `.gitone.yml` that is missing, invalid, reserved, ambiguously
  owned or no longer owned by exactly one repository;
- a change that moves `.gitone.yml` to a different repository's tree, which is
  the anchor of the whole review and of `clone`;
- a projected policy that puts an existing local ignored or unassigned path
  into a repository;
- a remote URL that embeds a password;
- an incoming repository name collision with `.gitone.local.yml`;
- a retirement that would strand unpublished commits or leave a relevant path
  unassigned;
- an ownership move whose repositories' trees disagree;
- a projected tree containing an unsafe symbolic link, a nested repository, a
  reserved path or a protected path;
- a checkout that would overwrite an existing working-tree file;
- a repository that is not on the project's branch, has staged or unstaged
  changes, or cannot fast-forward.

Never in scope: merge, rebase, stash, force, arbitrary native Git forwarding,
automatic deletion of anything below `.gitone/`, and any `--yes` that accepts
several review classes at once.

## Implementation tasks

Cut so that each one leaves a project that works, and so that the local
primitive exists before anything incoming can use it.

All of them have landed.

| Task | Delivers | Status |
| --- | --- | --- |
| 0057 | `gitone reconfigure` with the lock, the journal, the preview frame and remote URL reconciliation. Closes the old `doctor` dead end. | done |
| 0058 | repository addition: creation, remotes, fetch, branch, checkout, unborn repositories. | done |
| 0059 | repository retirement to `.gitone/retired/`, the unpublished-commit and unassigned-path refusals, `repo list` reporting. | done |
| 0060 | `--rename`, moving metadata in place. | done |
| 0061 | `--pull` and `--switch <branch>`, and the `pull`/`switch` refusals that point at them. | done |
