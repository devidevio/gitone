# GitOne hands-on tutorial

Install GitOne and run every supported command against disposable projects.
Use one terminal session on macOS or Linux. For details beyond this
walkthrough, see the [usage guide](USAGE.md).

## 1. Check the tools

```bash
git --version
code --version
```

GitOne requires Git 2.28 or newer, and VS Code 1.127 or newer for the
extension. Building from source instead of using a release additionally needs
Go 1.26 or newer and Node.js 24 or newer.

Create one disposable workspace for everything below:

```bash
export GITONE_TUTORIAL_ROOT="$(mktemp -d)"
export GIT_AUTHOR_NAME=GitOne
export GIT_AUTHOR_EMAIL=gitone@example.invalid
export GIT_COMMITTER_NAME="$GIT_AUTHOR_NAME"
export GIT_COMMITTER_EMAIL="$GIT_AUTHOR_EMAIL"

echo "$GITONE_TUTORIAL_ROOT"
```

## 2. Install GitOne

Install a release as described in the [README](../README.md#install), or build
the source checkout you are standing in:

```bash
export GITONE_SOURCE="$(pwd)"
go test ./...
go build -o gitone ./cmd/gitone
sudo install -m 0755 ./gitone /usr/local/bin/gitone
hash -r
```

Either way, confirm what you installed:

```bash
command -v gitone
gitone --version
gitone --help
```

Optional short command:

```bash
sudo ln -s "$(command -v gitone)" "$(dirname "$(command -v gitone)")/git1"
git1 --version
```

## 3. Install the VS Code extension

Install the `gitone_<tag>_vscode.vsix` published with the release you
installed:

```bash
gitone vscode install --vsix ./gitone_<tag>_vscode.vsix
```

Or build it from the same source checkout, where `--force` replaces an
already installed development build of the same version:

```bash
cd "$GITONE_SOURCE/editors/vscode"
npm install
npm test
npm run package -- --out gitone-<version>.vsix
gitone vscode install --vsix ./gitone-<version>.vsix --force
```

Either way, confirm the result:

```bash
code --list-extensions --show-versions | grep '^devidevio\.gitone@'
```

It should print `devidevio.gitone@<version>`.

## 4. Create a local-only project

```bash
mkdir -p "$GITONE_TUTORIAL_ROOT/local-project/src"
mkdir -p "$GITONE_TUTORIAL_ROOT/local-project/notes"
cd "$GITONE_TUTORIAL_ROOT/local-project"

printf '# Local GitOne test\n' > README.md
printf 'application\n' > src/app.txt
printf 'private notes\n' > notes/plan.txt

gitone setup
```

Use these answers in the setup wizard:

1. Setup mode: `Wizard`, which Enter selects
2. Default branch: `main`
3. Repository `app`, visibility `public`, paths `README.md` and `src/**`
4. Leave its remote empty and store it in `shared`
5. Add repository `notes`, visibility `private`, path `notes/**`
6. Leave its remote empty and store it in `local`
7. Stop adding repositories, choose `app` as owner if asked, then confirm

Inspect the result:

```bash
gitone repo validate
gitone repo list
gitone doctor
gitone status
gitone status --porcelain
gitone status --json
```

## 5. Stage, inspect and commit

```bash
gitone add README.md src/app.txt notes/plan.txt
gitone diff --staged
gitone show --repository app --source index -- README.md

gitone unstage README.md
gitone add -A
gitone commit -m "Initial local project"

gitone log all -n 3
gitone show --repository app --source head -- README.md
gitone status
```

The commit spans both repositories and therefore receives one shared
`GitOne-Group` trailer.

Exercise updates and undo operations:

```bash
printf '\ntemporary README change\n' >> README.md
printf '\napplication update\n' >> src/app.txt
printf 'new file\n' > src/new.txt
printf '\nprivate update\n' >> notes/plan.txt

gitone diff
gitone diff app
gitone add -u
gitone diff all --staged

gitone restore --staged src/app.txt
gitone unstage -A
gitone restore README.md

printf '\nsecond attempt\n' >> src/app.txt
gitone add src/app.txt
printf '\nthird attempt\n' >> src/app.txt
gitone restore --source=HEAD --staged --worktree src/app.txt

gitone add -A
printf 'Update local files\n' | gitone commit -F -
gitone status
```

`add -u` does not stage the new `src/new.txt`. Neither `unstage` nor
`restore --staged` changes working files. `restore README.md` deliberately
discards only that unstaged test change. The last restore discards the staged
and the unstaged change of `src/app.txt` in one step, so index and file match
the last commit of the repository owning it again.

A symbolic link inside one repository is an owned path of its own. GitOne
stores the link and never looks through it:

```bash
ln -s app.txt src/alias.txt
gitone status
gitone add src/alias.txt
gitone commit -m "Alias the application file"
gitone show --repository app --source head -- src/alias.txt
```

`show` prints the stored target `app.txt`, not the file behind it. A link that
leaves its repository or the project is refused instead:

```bash
ln -s ../notes/plan.txt src/plan.txt
gitone add src/plan.txt

rm src/plan.txt
ln -s /etc/hosts src/hosts
gitone add -A
rm src/hosts
```

Both must report `PATH003`, naming the link, its stored target and the reason:
the first target belongs to `notes`, the second is absolute.

## 6. Create and switch branches

```bash
gitone branch
gitone switch -c feature/tutorial

printf '\nfeature change\n' >> src/app.txt
gitone add src/app.txt
gitone commit -m "Change feature branch"
gitone log app -n 3

gitone switch main
gitone branch
gitone status
```

Both managed repositories move together. `switch -c` creates the branch in
every repository at the commit it currently has checked out and moves the
project onto it in one step; `gitone branch feature/tutorial` would only write
the refs and leave every repository where it is. The feature commit exists
only in `app` and is no longer visible after switching back to `main`.

Later, on another machine, `gitone fetch` followed by
`gitone switch feature/tutorial` opens the same branch: a repository that does
not have it locally yet gets it from its own `origin/feature/tutorial` and
tracks it.

Clean a finished branch up with `-d`:

```bash
gitone branch throwaway
gitone branch -d throwaway

gitone branch -d feature/tutorial
```

`throwaway` holds nothing that `main` does not, so it is deleted in every
repository at once. The second command is refused: `feature/tutorial` still
holds the feature commit in `app`, and one repository that would lose work is
enough to stop the whole deletion. There is no force option - merge or publish
the branch first.

Local-only repositories are skipped by fetch and pull; push is rejected
because commits exist but no remotes are configured:

```bash
gitone fetch
gitone pull
gitone push --yes
gitone recover
gitone abort
```

The push error is expected. `recover` and `abort` report that no interrupted
operation exists.

## 7. Create a project with disposable remotes

```bash
mkdir -p "$GITONE_TUTORIAL_ROOT/remotes"
mkdir -p "$GITONE_TUTORIAL_ROOT/remote-project/public"
mkdir -p "$GITONE_TUTORIAL_ROOT/remote-project/internal"

git init --bare -b main "$GITONE_TUTORIAL_ROOT/remotes/public.git"
git init --bare -b main "$GITONE_TUTORIAL_ROOT/remotes/internal.git"

cd "$GITONE_TUTORIAL_ROOT/remote-project"
```

Create the committed ownership map. The remotes are written as absolute
paths: a real project uses URLs, and a relative path would be resolved from
each managed repository below `.gitone/` rather than from the project, which
the clone in section 8 could not follow.

```bash
cat > .gitone.yml <<YAML
version: 1
default_branch: main

repositories:
  public:
    visibility: public
    remote: $GITONE_TUTORIAL_ROOT/remotes/public.git
    paths:
      - .gitignore
      - .gitone.yml
      - public/**

  internal:
    visibility: private
    remote: $GITONE_TUTORIAL_ROOT/remotes/internal.git
    paths:
      - internal/**
YAML

printf '# Public project\n' > public/README.md
printf '# Internal notes\n' > internal/notes.md

gitone repo validate
gitone init
gitone repo list
gitone doctor
gitone status
```

Create the first remote history:

```bash
gitone add -A
gitone commit -m "Initialize remote project"
gitone push
```

Press Enter at `Continue? [y/N]` to verify that the default aborts without
changing a remote. Then publish explicitly:

```bash
gitone push all --yes
gitone status
```

## 8. Clone and synchronize

```bash
cd "$GITONE_TUTORIAL_ROOT"
gitone clone "$GITONE_TUTORIAL_ROOT/remotes/public.git" cloned-project

cd "$GITONE_TUTORIAL_ROOT/cloned-project"
gitone repo list
gitone log all -n 2
gitone status
gitone doctor
```

The public repository bootstraps the project. Its committed `.gitone.yml`
then tells GitOne to clone `internal` into the same working directory.

Create a grouped update in the original project and publish one repository at
a time:

```bash
cd "$GITONE_TUTORIAL_ROOT/remote-project"
printf '\npublic update\n' >> public/README.md
printf '\ninternal update\n' >> internal/notes.md
gitone add -u
gitone commit -m "Update public and internal data"
gitone push public --yes
```

Fetch and fast-forward only `public` in the clone:

```bash
cd "$GITONE_TUTORIAL_ROOT/cloned-project"
gitone fetch public
gitone status
gitone pull public
gitone show --repository public --source head -- public/README.md
```

Publish and receive the remaining update:

```bash
cd "$GITONE_TUTORIAL_ROOT/remote-project"
gitone push internal --yes

cd "$GITONE_TUTORIAL_ROOT/cloned-project"
gitone fetch all
gitone pull all
gitone status
gitone doctor
```

## 9. Migrate an ordinary Git repository

```bash
mkdir -p "$GITONE_TUTORIAL_ROOT/migration/src"
mkdir -p "$GITONE_TUTORIAL_ROOT/migration/notes"
cd "$GITONE_TUTORIAL_ROOT/migration"

git init -b main
printf 'legacy application\n' > src/app.txt
printf 'legacy notes\n' > notes/plan.txt

cat > .gitone.yml <<'YAML'
version: 1
default_branch: main

repositories:
  app:
    visibility: public
    paths:
      - .gitignore
      - .gitone.yml
      - src/**

  notes:
    visibility: private
    paths:
      - notes/**
YAML

git add -A
git commit -m "Create legacy project"
gitone migrate
```

Press Enter to test the safe default. The root `.git/` must remain. Then
migrate and inspect the retained history:

```bash
gitone migrate --yes
test ! -e .git && echo "Root .git removed"

gitone backup list
gitone backup git -- log --oneline --all

gitone status
gitone doctor
```

The managed repositories intentionally start empty. The old history remains
only in the verified migration backup, which `gitone backup` lists, reads and -
with `gitone backup restore` - copies back to the root `.git/` whenever you
want the old repository again.

## 10. Restore the migration backup

The backup is not a one-way exit. Restoring it is an ordinary command, it
changes nothing but the root `.git/`, and it can be undone again.

```bash
cd "$GITONE_TUTORIAL_ROOT/migration"
gitone backup restore
```

Press Enter to test the safe default: the answer is `No` and nothing changes.
Then restore explicitly:

```bash
gitone backup restore --yes
git log --oneline --all
```

The old history is back at the project root, with every commit the legacy
repository had. Pass `--backup <id>` from `gitone backup list` as soon as a
project has more than one backup.

While that root `.git/` exists the ordinary commands step aside, and the
backup commands do not:

```bash
gitone status
gitone add -A
gitone backup list
gitone backup git -- log --oneline --all
```

The first two report `PATH003 .git: the project root is a Git repository` and
exit `1`. The backup is still listed: restoring copies it, it never consumes
it, so you can restore the same backup again later.

Go back to the GitOne project by removing the root repository you just
restored. Nothing else has to be undone - the managed repositories, the
configuration and your files were never touched:

```bash
cd "$GITONE_TUTORIAL_ROOT/migration"
rm -rf .git
gitone status
gitone doctor
```

## 11. See it in VS Code

```bash
cd "$GITONE_TUTORIAL_ROOT/cloned-project"
gitone vscode info --json
code .
```

In the opened trusted workspace:

- Open Source Control and run **GitOne: Refresh**.
- Append a line to `public/README.md` and confirm that it appears automatically.
- Open its diff, stage it, unstage it and stage it again.
- Enter a message and run **GitOne: Commit & Push**.
- Run **GitOne: Show Output**, **Recover Interrupted Operation** and
  **Abort Interrupted Operation**.

Finish in the terminal:

```bash
gitone status
gitone doctor
```

## 12. Verify the safety boundary

An unassigned file must block status and every mutation:

```bash
cd "$GITONE_TUTORIAL_ROOT/cloned-project"
printf 'unassigned\n' > unassigned.test

gitone status
gitone add -A
gitone push --yes

rm unassigned.test
gitone status
```

The first three GitOne commands must report `PATH001`. Unsupported commands
must report `CLI001` without changing the project:

```bash
gitone reset --hard
gitone tag tutorial
gitone status
```

## Completed

- [ ] Binary built and installed
- [ ] VS Code extension built and installed
- [ ] Local setup and every local file operation exercised
- [ ] Safe symbolic link committed, unsafe ones refused
- [ ] Branches created and switched synchronously
- [ ] Push, clone, fetch and pull exercised against two remotes
- [ ] Ordinary Git repository migrated with a verified backup
- [ ] Migration backup restored and the restored root repository removed again
- [ ] VS Code Source Control workflow exercised
- [ ] Path ownership and unsupported-command safety verified

The fixtures remain below `$GITONE_TUTORIAL_ROOT` for inspection. Delete that
single disposable directory when finished.
