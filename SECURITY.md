# Security policy

GitOne decides which Git repository may publish which path. A bug that lets a
path reach a repository that does not own it is the most serious kind of bug
this project can have. Please report it privately.

## Reporting a vulnerability

Use GitHub's private vulnerability reporting:
**[Report a vulnerability](https://github.com/devidevio/gitone/security/advisories/new)**.
The report stays private between you and the maintainer until an advisory is
published.

Please do not open a public issue for a security problem, and please do not
attach real private data - a minimal `.gitone.yml` and the commands you ran are
enough.

Helpful in a report:

- the GitOne version (`gitone --version`) and your operating system
- the `.gitone.yml` and, redacted, the relevant part of `.gitone.local.yml`
- the exact commands, and what was published or written that should not have been

GitOne is maintained by one person. You will get an acknowledgement as soon as
I can manage, and an honest estimate rather than a promised deadline. If you
plan to disclose publicly, tell me your timeline and I will work to it. Credit
is yours unless you prefer otherwise.

Only the latest release is supported. Fixes go into a new release rather than
into patches for older tags.

## In scope

A vulnerability is anything that breaks the ownership boundary:

- a path published by, staged in or committed to a repository that does not own
  it
- a path matched by `rules.protected_paths` reaching an index, a commit or a
  remote
- an unassigned or ambiguous path that does not stop a mutating operation
- `.gitone/` or `.gitone.local.yml` becoming ownable, stageable or committable
- an incoming tree from `clone`, `pull` or `switch` that writes outside its
  repository, or a symbolic-link entry that is not refused
- an incoming `.gitone.yml` or tracked `.gitignore` that `pull` or `switch`
  applies without the documented preview and confirmation, or that makes an
  existing local ignored or unassigned path managed by a repository
- ownership `pull` or `switch` generates for a new incoming path without the
  documented preview and confirmation, for a path the selected targets do not
  introduce, or for a path or directory that already exists in the working tree
- an incoming `.gitone.yml` that adds, removes or renames a repository or
  changes a configured remote and is applied by `pull` or `switch`, or that
  `gitone reconfigure` applies without the documented preview and
  `--accept-repository-change`
- a `gitone reconfigure`, `gitone reconfigure --pull` or
  `gitone reconfigure --switch <branch>` that reaches a remote before the exact
  old and new URL were shown and accepted, that accepts one reviewed class with
  another class's flag, that adopts a configured URL embedding a password, that
  moves `.gitone.yml` into another repository's tree, that adds a repository
  whose name `.gitone.local.yml` defines, or that leaves the configuration and
  the repository set disagreeing after a failure or an interruption. The
  contract is [incoming repository reconfiguration](docs/RECONFIGURATION.md)
- incoming repository content that changes `.gitone.local.yml` or weakens what
  it restricts
- a path that escapes the project root
- repository content - a commit subject, a path name, a Git error - that drives
  the terminal instead of being printed as text

## Out of scope

These are documented properties of the tool, not bugs. They are described in
the [security boundary](docs/USAGE.md#security-boundary) of the usage guide:

- **GitOne never reads file contents.** There is no secret scanning and no data
  classification. A private key inside a path the public repository owns is
  published like any other owned file, unless its path is covered by
  `rules.protected_paths`.
- **GitOne is not an access-control boundary.** Every file sits in one working
  directory that your shell, editor, backup tool and every other local program
  can read. Private means "not published by Git", not "unreadable locally".
- **The local environment is trusted.** The Git binary, Git configuration,
  credential helpers, hooks and LFS filters run as they normally do. An attacker
  who already controls those, or who can edit your local `.gitone.yml` or
  `.gitone.local.yml`, controls the outcome. Repository content is not trusted
  this way: a `.gitone.yml` reaching you through `pull` or through the branch
  a `switch` checks out is reviewed first, see
  [Incoming policy changes](docs/USAGE.md#incoming-policy-changes).
- Anything that requires an attacker to already have code execution on the
  machine.

If you are unsure which side of that line something falls on, report it
privately anyway and I will tell you.
