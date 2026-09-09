# GitOne

One working directory, several Git repositories.

[gitone.io](https://gitone.io) · [Usage guide](docs/USAGE.md) · [Tutorial](docs/TUTORIAL.md)

A project often mixes files that must be published with files that must not:
the open source library next to the private deployment notes, the public
website next to the client contract. With plain Git you either split the
project into separate checkouts and lose one coherent working directory, or
you keep everything in one repository and rely on nobody ever committing the
wrong file.

GitOne keeps **one working directory** and maps every file to exactly one Git
repository by path. `src/**` belongs to the public repository, `notes/**`
belongs to the private one. Staging, committing and pushing route each change
to the repository that owns it. A file that belongs to no repository, or to
two, stops the operation instead of being guessed.

![A real GitOne session: refuse an unassigned path, then commit public and private changes to their owning repositories](docs/assets/demo.gif)

```
project/                     one directory you work in
├── .gitone.yml              public ownership map (committed)
├── .gitone.local.yml        private additions (never committed)
├── .gitone/repositories/    one native Git directory per repository
│   ├── website/
│   └── notes/
├── README.md                → website
├── src/site.css             → website
└── notes/plan.md            → notes
```

There is no `.git/` in the project root. Each managed repository has its own
Git metadata, index, HEAD, refs and hooks below `.gitone/repositories/`, and
all of them share the one working tree. Every operation is a native `git`
invocation, so hooks, credential helpers, signing and Git LFS keep working.

## Status

v0.1. Linux and macOS. See [what is deliberately not supported](#deferred).

## Install

Requirements: Git 2.28 or newer. Building from source additionally needs Go
1.26 or newer.

The quickest way installs GitOne to `~/.local/bin` without `sudo` and verifies
the downloaded release checksum:

```bash
curl -fsSL https://gitone.io/install.sh | sh
```

With Go:

```bash
go install github.com/devidevio/gitone/cmd/gitone@latest
```

Or download a prebuilt binary for Linux or macOS, amd64 or arm64, from the
[releases](https://github.com/devidevio/gitone/releases). Every release
publishes `checksums.txt`; verify the download before installing it:

```bash
sha256sum --check --ignore-missing checksums.txt
install -m 0755 gitone_<tag>_linux_amd64 /usr/local/bin/gitone
```

On macOS use `shasum -a 256 -c --ignore-missing checksums.txt`.

From a checkout:

```bash
go build -o gitone ./cmd/gitone
install -m 0755 gitone /usr/local/bin/gitone
```

Confirm which build you installed:

```bash
gitone --version
```

Optionally add the shorter `git1` command next to the installed binary:

```bash
ln -s "$(command -v gitone)" "$(dirname "$(command -v gitone)")/git1"
git1 --version
```

This creates an executable symlink that works in every shell. `gitone setup`
does not manage it because setup is project-specific, while the symlink is a
one-time machine-wide installation choice.

Every release also publishes the VS Code extension as
`gitone_<tag>_vscode.vsix`. Install a downloaded VSIX explicitly through
GitOne:

```bash
gitone vscode install --vsix ./gitone_<tag>_vscode.vsix
```

Setup prints the release VSIX instructions instead of attempting a Marketplace
installation. After the extension is published in the Marketplace, this
shorter command installs it through VS Code's own CLI:

```bash
gitone vscode install
```

Verify the installation inside a project:

```bash
gitone doctor
```

## Quick start

The guided way, which asks for the configuration and then initializes or
migrates the project:

```bash
cd my-project
gitone setup
```

Joining a project that already exists, from the repository that commits
`.gitone.yml`:

```bash
gitone clone git@github.com:example/website.git
cd website
gitone status
```

The same by hand, for an existing project with a configured remote:

```bash
cd my-project
$EDITOR .gitone.yml     # see the configuration guide
gitone repo validate
gitone init
gitone status
gitone add -A
gitone commit -m "Initial import"
gitone push
```

[`gitone setup`](docs/USAGE.md#interactive-setup) is interactive only. In a
project without configuration its first question is the
[mode](docs/USAGE.md#setup-modes): `Wizard`, preselected, asks for every
repository explicitly and offers the owned paths your project already has,
guesses nothing else from your files, never edits existing configuration and
asks one confirmation before it changes anything. On a normal terminal the
owned paths are chosen in a
[browser](docs/USAGE.md#choosing-the-owned-paths) that walks the project one
directory at a time; a plain terminal keeps the numbered list.

[`Template`](docs/USAGE.md#template-mode) is the two-step alternative: it
writes `.gitone.yml` and `.gitone.local.yml` as commented templates that
document every supported setting, and stops without initializing or migrating
anything. Edit them, then run `gitone repo validate` and `gitone setup` again.

`gitone init` expects an existing directory that already has a valid
`.gitone.yml` and **no** root `.git/`. It creates the repository metadata and
the ignore entries, and never stages, commits or contacts a remote. A
directory that already is a Git repository is converted by
[`gitone migrate`](docs/USAGE.md#migrating-an-existing-repository), which keeps
a verified backup of the old `.git/` and imports no history. That backup stays
reachable through
[`gitone backup`](docs/USAGE.md#listing-inspecting-and-restoring-a-backup):
`list` shows every retained backup, `git` reads one with `log`, `show`, `diff`
or `status`, and `restore` copies it back to the root `.git/`.

[`gitone clone`](docs/USAGE.md#cloning-a-project) needs a destination that does
not exist yet. It adopts the bootstrap repository with its complete history,
then fetches and checks out every other repository the committed configuration
declares. Repositories that exist only in `.gitone.local.yml` cannot be
discovered from a clone.

Both modes finish by offering GitOne's
[instructions for AI agents](docs/USAGE.md#instructions-for-ai-agents), with
default `no`. Answering yes writes one marked block into the project
`AGENTS.md` and assigns that file to a repository you choose - a private one
in `.gitone.local.yml` is a valid answer. Later, `gitone agents` checks the
block without changing anything and `gitone agents update` writes it, both
without a terminal, so CI can run them:

```bash
gitone agents         # exit 0 only when the current block is there
gitone agents update  # create, append, mark or replace it
```

Only `<project-root>/AGENTS.md` is managed, everything outside the markers is
preserved, and a file GitOne cannot classify is reported as `AGENT001` instead
of being overwritten.

For a complete disposable workflow from an empty directory through a confirmed
push to local test remotes, follow [the normal workflow](docs/USAGE.md#the-normal-workflow).

## Documentation

- [Hands-on tutorial](docs/TUTORIAL.md) - install GitOne and exercise every
  command against disposable local projects
- [Usage guide](docs/USAGE.md) - practical setup, configuration, daily
  commands, branches, remotes, recovery and the security boundary

## Security boundary

GitOne enforces **path ownership**. It decides which repository may publish a
path, and it refuses to push a commit or a tree containing a path the
repository does not own.

GitOne does **not** read your file contents. It is not a secret scanner, it
does not classify data, and it is not an access-control boundary between the
public and private files on your machine - everything is in one directory that
your shell, editor and every other local program can read. GitOne assumes the
local Git binary, Git configuration, credential helpers and hooks are
trustworthy.

## Deferred

Not implemented, and rejected with `CLI001` rather than forwarded to Git:
`reset`, `tag` and arbitrary Git pass-through. `branch` lists, creates and
deletes merged branches; it never renames, copies or force-deletes, and never
touches a remote branch. `switch` moves to a branch or creates one with `-c`,
always from your own HEAD or your own fetched `origin/<branch>`. `restore`
reads the index or `HEAD`, never another revision.

Symbolic links are supported only as a deliberate alias inside one repository,
such as `.claude/skills -> ../.agents/skills`: a relative target that resolves
inside the project, exists, is reached without passing through another link,
and belongs to the same repository as the link. Everything else - absolute,
escaping, dangling, cross-repository, chained, cyclic, or reaching GitOne's
own configuration, ignore or Git metadata paths - is `PATH003`.

Also out of scope: submodules, general Git-compatible symlink support,
importing the history of an existing root `.git/`, Windows, package-manager
distribution, standalone GUI/TUI and other IDE integrations.

## Contributing

Issues, edge cases and honest criticism are welcome - see
[CONTRIBUTING.md](CONTRIBUTING.md) for the layout, how to build and check, and
the one rule that decides what gets merged: **GitOne refuses rather than
guesses.** A change that makes it guess where a file belongs does not go in,
however convenient it feels.

Found a security problem? Do not open an issue - [SECURITY.md](SECURITY.md)
explains the private route.

## Sponsors

GitOne is MIT licensed and built by one developer. Sponsoring pays for the part
nothing else funds - what each tier does is on
[gitone.io/sponsors](https://gitone.io/sponsors).

<!-- gitone:sponsors:start -->

No sponsors yet. [Be the first.](https://gitone.io/sponsors)

<!-- gitone:sponsors:end -->

## License

[MIT](LICENSE).
