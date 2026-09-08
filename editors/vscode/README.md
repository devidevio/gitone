# GitOne for Visual Studio Code

Native Source Control for [GitOne](https://github.com/devidevio/gitone)
projects: one working directory, several Git repositories, ownership decided by
path.

A GitOne project keeps public and private files side by side in one directory
and maps every file to exactly one Git repository. This extension shows that
split where you already work - one Source Control provider per project, with
staged and working-tree groups per managed repository.

## Requirements

- **The `gitone` CLI**, installed separately. The extension never bundles it,
  never invokes `git` and never reads `.gitone/` directly; all state, diffs and
  mutations go through the public CLI. Install it from the
  [releases](https://github.com/devidevio/gitone/releases) or with
  `go install github.com/devidevio/gitone/cmd/gitone@latest`.
- **A GitOne project** - a workspace folder whose root contains `.gitone.yml`.
  The extension stays dormant in every other workspace.
- **macOS or Linux**, and a
  [trusted workspace](https://code.visualstudio.com/api/extension-guides/workspace-trust).
  Because it executes the GitOne CLI, it is disabled in Restricted Mode and in
  virtual workspaces.

## Features

- one Source Control provider per GitOne project, next to - not instead of -
  the built-in Git extension
- staged and working-tree groups per managed repository, with native Git status
  letters and colors
- file decorations, gutter quick diff and HEAD/index/working-tree diffs
- native ignored-resource color for paths the project `.gitignore` files hide
- Stage, Unstage, Commit, Commit & Push, Push, Recover and Abort from the SCM
  view
- automatic refresh after a mutating `gitone` command runs in an external
  terminal
- multi-root workspaces, each GitOne project with its own provider

## Commands

All commands are available in the Command Palette under **GitOne**, and the
relevant ones in the Source Control view.

| Command                               | Purpose                                          |
| ------------------------------------- | ------------------------------------------------ |
| Refresh                               | Re-read project state from the CLI               |
| Stage Changes / Stage All Changes     | Stage a file or a whole group                    |
| Unstage Changes / Unstage All Changes | Unstage without touching working files           |
| Open File                             | Open the working-tree file behind a change       |
| Commit                                | Commit the staged changes of every repository    |
| Commit & Push                         | Commit, then push after an explicit confirmation |
| Push                                  | Push, after an explicit confirmation             |
| Recover Interrupted Operation         | Finish an interrupted GitOne operation           |
| Abort Interrupted Operation           | Undo an interrupted GitOne operation             |
| Show Output                           | Open the GitOne output channel                   |

Push and Abort always ask before they run.

## Settings

| Setting       | Default | Meaning                                                                  |
| ------------- | ------- | ------------------------------------------------------------------------ |
| `gitone.path` | empty   | Absolute path to the GitOne executable. Empty uses `gitone` from `PATH`. |

## Not in this version

Branch, pull, fetch, sync, stash, history and discard are not part of this
version. Use the CLI for them.

## Documentation

- [Usage guide](https://github.com/devidevio/gitone/blob/main/docs/USAGE.md)
- [Hands-on tutorial](https://github.com/devidevio/gitone/blob/main/docs/TUTORIAL.md)
- [Report an issue](https://github.com/devidevio/gitone/issues)

The implementation follows the official
[Source Control API](https://code.visualstudio.com/api/extension-guides/scm-provider).

## License

[MIT](https://github.com/devidevio/gitone/blob/main/LICENSE).
