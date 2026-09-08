# Changelog

All notable changes to the GitOne VS Code extension are documented here. The
format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and
this extension's version matches the GitOne release it ships with.

## [0.1.0] - 2026-09-08

First release.

### Added

- One Source Control provider per GitOne project, alongside the built-in Git
  extension rather than replacing it.
- Staged and working-tree resource groups per managed repository, with native
  Git status letters and colors.
- File decorations, gutter quick diff, and HEAD/index/working-tree diffs.
- Native ignored-resource coloring for paths the project `.gitignore` files
  hide, reported by the GitOne CLI.
- Stage, Unstage, Commit, Commit & Push, Push, Recover and Abort, with an
  explicit confirmation before Push and Abort.
- Automatic refresh after a mutating `gitone` command runs in an external
  terminal.
- Multi-root workspace support, one independent provider per GitOne project.
- The `gitone.path` setting for an absolute path to the GitOne executable.

### Notes

- The extension talks only to the public `gitone` CLI. It never invokes `git`
  and never reads `.gitone/` directly.
- Requires the GitOne CLI, installed separately, and a trusted workspace.
  Restricted Mode and virtual workspaces are unsupported.
- Branch, pull, fetch, sync, stash, history and discard are not part of this
  version.

[0.1.0]: https://github.com/devidevio/gitone/releases/tag/v0.1.0
