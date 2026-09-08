package stage_test

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/devidevio/gitone/internal/config"
	"github.com/devidevio/gitone/internal/lock"
	"github.com/devidevio/gitone/internal/stage"
	"github.com/devidevio/gitone/internal/status"
)

// identity gives the native commits an author and committer, so the tests do
// not depend on the Git configuration of the machine running them.
func identity(t *testing.T) {
	t.Helper()
	for _, name := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(name, "GitOne")
	}
	for _, name := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(name, "gitone@example.com")
	}
}

// staged creates a project whose files are staged into their repositories.
func staged(t *testing.T, files map[string]string, arguments ...string) (*config.Config, string) {
	t.Helper()
	identity(t)
	configuration, root := project(t, files)
	add(t, configuration, root, root, arguments...)
	return configuration, root
}

func commit(t *testing.T, configuration *config.Config, root, message string, paths ...string) string {
	t.Helper()
	var output bytes.Buffer
	if err := stage.Commit(configuration, root, root, paths, message, &output); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return output.String()
}

func commitError(t *testing.T, configuration *config.Config, root, message string, paths ...string) error {
	t.Helper()
	var output bytes.Buffer
	err := stage.Commit(configuration, root, root, paths, message, &output)
	if err == nil {
		t.Fatalf("commit succeeded, want an error: %s", output.String())
	}
	return err
}

// body is the complete message of the last commit of one repository.
func body(t *testing.T, root, name string) string {
	t.Helper()
	return run(t, root, name, "log", "-1", "--format=%B")
}

// group is the GitOne-Group trailer Git itself parses out of the last commit.
func group(t *testing.T, root, name string) string {
	t.Helper()
	return strings.TrimSpace(run(t, root, name, "log", "-1", "--format=%(trailers:key=GitOne-Group,valueonly)"))
}

// commits counts the commits of one repository, including none at all.
func commits(t *testing.T, root, name string) int {
	t.Helper()
	output := run(t, root, name, "for-each-ref", "--format=%(objectname)", "refs/heads/main")
	if strings.TrimSpace(output) == "" {
		return 0
	}
	return len(strings.Fields(run(t, root, name, "log", "--format=%H")))
}

// hook installs a repository-local Git hook.
func hook(t *testing.T, root, name, kind, script string) {
	t.Helper()
	directory := filepath.Join(root, ".gitone", "repositories", name, "hooks")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, kind), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestCommitOfOneRepositoryCarriesNoGroup(t *testing.T) {
	configuration, root := staged(t, map[string]string{
		"README.md":        "readme\n",
		"secrets/notes.md": "notes\n",
	}, "secrets/notes.md")

	output := commit(t, configuration, root, "Add notes")

	if commits(t, root, "private") != 1 {
		t.Fatalf("private commits = %d, want 1", commits(t, root, "private"))
	}
	if commits(t, root, "public") != 0 {
		t.Fatalf("public commits = %d, want 0", commits(t, root, "public"))
	}
	if strings.TrimSpace(body(t, root, "private")) != "Add notes" {
		t.Fatalf("message = %q", body(t, root, "private"))
	}
	if got := group(t, root, "private"); got != "" {
		t.Fatalf("group = %q, want none", got)
	}
	if strings.Contains(output, "GitOne-Group") || !strings.Contains(output, "private:\n    commit ") {
		t.Fatalf("output = %q", output)
	}
	equal(t, tracked(t, root, "private"), []string{"secrets/notes.md"})
}

func TestCommitDeletionWithoutOwnershipRule(t *testing.T) {
	for _, mode := range []string{"all", "update", "explicit", "selective", "staged-selective"} {
		t.Run(mode, func(t *testing.T) {
			configuration, root := staged(t, map[string]string{"README.md": "readme\n"}, "-A")
			commit(t, configuration, root, "Initial")

			if err := os.Remove(filepath.Join(root, "README.md")); err != nil {
				t.Fatal(err)
			}
			contents := strings.Replace(twoRepositories, "      - README.md\n", "", 1)
			write(t, root, ".gitone.yml", contents)
			configuration, _, err := config.Load(root)
			if err != nil {
				t.Fatal(err)
			}
			if _, inventory, err := status.Validate(configuration, root); err != nil {
				t.Fatal(err)
			} else if inventory.Owners["README.md"] != "public" {
				t.Fatalf("deleted file owner = %q", inventory.Owners["README.md"])
			}

			switch mode {
			case "all":
				add(t, configuration, root, root, "-A")
			case "update":
				add(t, configuration, root, root, "-u")
			case "explicit", "staged-selective":
				add(t, configuration, root, root, "README.md", ".gitone.yml")
			}
			if mode != "selective" {
				if err := stage.Unstage(configuration, root, root, []string{"README.md"}, io.Discard); err != nil {
					t.Fatal(err)
				}
				add(t, configuration, root, root, "README.md")
			}
			if mode == "selective" || mode == "staged-selective" {
				commit(t, configuration, root, "Remove readme", "README.md", ".gitone.yml")
			} else {
				commit(t, configuration, root, "Remove readme")
			}

			if got := run(t, root, "public", "ls-tree", "--name-only", "HEAD", "README.md"); got != "" {
				t.Fatalf("deleted file remains committed: %s", got)
			}
			if got := run(t, root, "public", "show", "HEAD:.gitone.yml"); got != contents {
				t.Fatalf("committed configuration = %q, want %q", got, contents)
			}
			result, _, err := status.Validate(configuration, root)
			if err != nil || len(result.Changes) != 0 {
				t.Fatalf("final status = %+v, %v", result, err)
			}
		})
	}
}

func TestCommitRequiresStagingUnassignedDeletion(t *testing.T) {
	configuration, root := staged(t, map[string]string{"README.md": "readme\n"}, "-A")
	commit(t, configuration, root, "Initial")
	if err := os.Remove(filepath.Join(root, "README.md")); err != nil {
		t.Fatal(err)
	}
	write(t, root, ".gitone.yml", strings.Replace(twoRepositories, "      - README.md\n", "", 1))
	configuration, _, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	add(t, configuration, root, root, ".gitone.yml")

	err = commitError(t, configuration, root, "Remove rule only")
	if !strings.Contains(err.Error(), "README.md") || !strings.Contains(err.Error(), "gitone add -u") {
		t.Fatalf("error = %v", err)
	}
	if commits(t, root, "public") != 1 {
		t.Fatal("refused commit changed history")
	}
	if _, err := os.Stat(filepath.Join(root, ".gitone", "recovery")); !os.IsNotExist(err) {
		t.Fatalf("refused commit left recovery state: %v", err)
	}
}

func TestCommitCannotLeaveUnassignedDeletionInAnotherRepository(t *testing.T) {
	for _, mode := range []string{"staged", "selective"} {
		t.Run(mode, func(t *testing.T) {
			configuration, root := staged(t, map[string]string{"secrets/notes.md": "notes\n"}, "-A")
			commit(t, configuration, root, "Initial")
			if err := os.Remove(filepath.Join(root, "secrets/notes.md")); err != nil {
				t.Fatal(err)
			}
			write(t, root, ".gitone.yml", strings.Replace(twoRepositories, "secrets/**", "secrets/other.md", 1))
			configuration, _, err := config.Load(root)
			if err != nil {
				t.Fatal(err)
			}
			add(t, configuration, root, root, ".gitone.yml")
			var paths []string
			hint := "gitone add -u"
			if mode == "selective" {
				paths = []string{".gitone.yml"}
				hint = "Select pending deletions together with the configuration"
			}
			err = commitError(t, configuration, root, "Remove rule only", paths...)
			if !strings.Contains(err.Error(), "secrets/notes.md") || !strings.Contains(err.Error(), hint) {
				t.Fatalf("error = %v", err)
			}
			if commits(t, root, "public") != 1 || commits(t, root, "private") != 1 {
				t.Fatal("refused commit changed history")
			}
			commit(t, configuration, root, "Remove notes and rule", ".gitone.yml", "secrets/notes.md")
		})
	}
}

func TestCommitOfSeveralRepositoriesSharesOneVerifiedGroup(t *testing.T) {
	configuration, root := staged(t, map[string]string{
		"README.md":        "readme\n",
		"src/a.go":         "a\n",
		"secrets/notes.md": "notes\n",
	}, "-A")

	output := commit(t, configuration, root, "Implement authentication")

	shared := group(t, root, "private")
	if !regexp.MustCompile(`^[0-9a-f]{8}$`).MatchString(shared) {
		t.Fatalf("group = %q, want eight hex characters", shared)
	}
	if other := group(t, root, "public"); other != shared {
		t.Fatalf("public group = %q, want %q", other, shared)
	}
	for _, name := range []string{"private", "public"} {
		if commits(t, root, name) != 1 {
			t.Fatalf("%s commits = %d, want 1", name, commits(t, root, name))
		}
		if !strings.HasPrefix(body(t, root, name), "Implement authentication\n") {
			t.Fatalf("%s message = %q", name, body(t, root, name))
		}
	}
	if !strings.Contains(output, "GitOne-Group: "+shared) {
		t.Fatalf("output = %q", output)
	}
}

func TestCommitLeavesUnstagedChangesAndUntouchedRepositoriesAlone(t *testing.T) {
	configuration, root := staged(t, map[string]string{
		"README.md":        "readme\n",
		"secrets/notes.md": "notes\n",
	}, "README.md")
	// The staged content must be committed, not the later working-tree state.
	write(t, root, "README.md", "changed\n")
	write(t, root, "src/a.go", "a\n")

	commit(t, configuration, root, "Add readme")

	if committed := run(t, root, "public", "show", "HEAD:README.md"); committed != "readme\n" {
		t.Fatalf("committed README.md = %q, want the staged content", committed)
	}
	if contents, err := os.ReadFile(filepath.Join(root, "README.md")); err != nil || string(contents) != "changed\n" {
		t.Fatalf("working tree README.md = %q, %v", contents, err)
	}
	if commits(t, root, "private") != 0 {
		t.Fatalf("private commits = %d, want 0", commits(t, root, "private"))
	}
	equal(t, tracked(t, root, "public"), []string{"README.md"})
}

func TestCommitWithNothingStagedCommitsNothing(t *testing.T) {
	identity(t)
	configuration, root := project(t, map[string]string{"README.md": "readme\n"})

	var output bytes.Buffer
	if err := stage.Commit(configuration, root, root, nil, "Nothing", &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Nothing to commit.") {
		t.Fatalf("output = %q", output.String())
	}
	if commits(t, root, "public") != 0 {
		t.Fatalf("public commits = %d, want 0", commits(t, root, "public"))
	}
}

func TestHooksSeeTheOwnIndexAndTheSharedWorkTree(t *testing.T) {
	configuration, root := staged(t, map[string]string{
		"README.md":        "readme\n",
		"src/a.go":         "a\n",
		"secrets/notes.md": "notes\n",
	}, "-A")
	for _, name := range []string{"private", "public"} {
		hook(t, root, name, "pre-commit", "#!/bin/sh\ndirectory=$(git rev-parse --absolute-git-dir)\npwd > \"$directory/hook.pwd\"\ngit diff --cached --name-only > \"$directory/hook.staged\"\n")
	}

	commit(t, configuration, root, "Implement authentication")

	for name, want := range map[string]string{
		"private": "secrets/notes.md",
		"public":  ".gitignore\n.gitone.yml\nREADME.md\nsrc/a.go",
	} {
		directory := filepath.Join(root, ".gitone", "repositories", name)
		seen, err := os.ReadFile(filepath.Join(directory, "hook.staged"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(string(seen)) != want {
			t.Fatalf("%s hook saw %q, want %q", name, seen, want)
		}
		where, err := os.ReadFile(filepath.Join(directory, "hook.pwd"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(string(where)) != root {
			t.Fatalf("%s hook ran in %q, want %q", name, where, root)
		}
	}
}

func TestCommitVerifiesTheGroupAgainstARewritingHook(t *testing.T) {
	configuration, root := staged(t, map[string]string{
		"README.md":        "readme\n",
		"secrets/notes.md": "notes\n",
	}, "-A")
	// private is committed first and drops the trailer from its message.
	hook(t, root, "private", "commit-msg", "#!/bin/sh\nhead -1 \"$1\" > \"$1.stripped\"\nmv \"$1.stripped\" \"$1\"\n")

	err := commitError(t, configuration, root, "Implement authentication")
	if !strings.Contains(err.Error(), "did not keep the GitOne-Group trailer") {
		t.Fatalf("error = %v", err)
	}
	if commits(t, root, "public") != 0 {
		t.Fatalf("public commits = %d, want 0", commits(t, root, "public"))
	}

	var output bytes.Buffer
	err = stage.Recover(configuration, root, &output)
	if err == nil || !strings.Contains(err.Error(), "did not keep the GitOne-Group trailer") {
		t.Fatalf("recover error = %v", err)
	}
	if commits(t, root, "public") != 0 {
		t.Fatalf("public commits after recover = %d, want 0", commits(t, root, "public"))
	}
}

func TestCommitRejectsStagedPathsOwnedByAnotherRepository(t *testing.T) {
	for _, operation := range []string{"delete", "rename"} {
		t.Run(operation, func(t *testing.T) {
			identity(t)
			configuration, root := project(t, map[string]string{"secrets/notes.md": "notes\n"})
			run(t, root, "public", "add", "secrets/notes.md")
			run(t, root, "public", "commit", "-qm", "contaminate index")

			if operation == "delete" {
				if err := os.Remove(filepath.Join(root, "secrets", "notes.md")); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(filepath.Join(root, "secrets", "notes.md"), filepath.Join(root, "src", "notes.md")); err != nil {
					t.Fatal(err)
				}
			}
			run(t, root, "public", "add", "-A")

			err := commitError(t, configuration, root, "Unsafe change")
			if !strings.Contains(err.Error(), "PATH003") {
				t.Fatalf("error = %v, want PATH003", err)
			}
		})
	}
}

// interruptedCommit stages both repositories and lets the commit fail in the
// second one, which leaves the state a later-repository failure produces.
func interruptedCommit(t *testing.T) (*config.Config, string) {
	t.Helper()
	configuration, root := staged(t, map[string]string{
		"README.md":        "readme\n",
		"src/a.go":         "a\n",
		"secrets/notes.md": "notes\n",
	}, "-A")
	hook(t, root, "public", "pre-commit", "#!/bin/sh\nexit 1\n")

	err := commitError(t, configuration, root, "Implement authentication")
	if !strings.Contains(err.Error(), recoveryRequired) {
		t.Fatalf("error = %v, want an incomplete commit", err)
	}
	if commits(t, root, "private") != 1 || commits(t, root, "public") != 0 {
		t.Fatalf("commits = private %d, public %d, want 1 and 0", commits(t, root, "private"), commits(t, root, "public"))
	}
	return configuration, root
}

const recoveryRequired = "REC001"

func TestFailingHookKeepsRecoveryStateAndLosesNothing(t *testing.T) {
	_, root := interruptedCommit(t)

	if _, err := os.Stat(filepath.Join(root, ".gitone", "recovery", "state.json")); err != nil {
		t.Fatal(err)
	}
	// Neither the working tree nor the staged content of the failed
	// repository was touched.
	equal(t, tracked(t, root, "public"), []string{".gitignore", ".gitone.yml", "README.md", "src/a.go"})
	for name, want := range map[string]string{"README.md": "readme\n", "src/a.go": "a\n", "secrets/notes.md": "notes\n"} {
		contents, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil || string(contents) != want {
			t.Fatalf("%s = %q, %v", name, contents, err)
		}
	}
}

func TestRecoverFinishesAnInterruptedCommit(t *testing.T) {
	configuration, root := interruptedCommit(t)
	if err := os.Remove(filepath.Join(root, ".gitone", "repositories", "public", "hooks", "pre-commit")); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(root, ".gitone", "recovery", "state.json")
	originalState, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	openState, err := os.Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer openState.Close()

	var output bytes.Buffer
	if err := stage.Recover(configuration, root, &output); err != nil {
		t.Fatal(err)
	}
	openedState, err := io.ReadAll(openState)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(openedState, originalState) {
		t.Fatal("state.json was overwritten instead of atomically replaced")
	}
	if !strings.Contains(output.String(), "gitone commit recovered.") {
		t.Fatalf("output = %q", output.String())
	}
	if commits(t, root, "public") != 1 {
		t.Fatalf("public commits = %d, want 1", commits(t, root, "public"))
	}
	if shared := group(t, root, "public"); shared == "" || shared != group(t, root, "private") {
		t.Fatalf("group = %q, want the recorded one", shared)
	}
	if _, err := os.Stat(filepath.Join(root, ".gitone", "recovery")); !os.IsNotExist(err) {
		t.Fatalf("recovery state = %v, want none", err)
	}
}

func TestAbortUndoesAnInterruptedCommit(t *testing.T) {
	configuration, root := interruptedCommit(t)

	var output bytes.Buffer
	if err := stage.Abort(configuration, root, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "gitone commit aborted.") {
		t.Fatalf("output = %q", output.String())
	}
	for _, name := range []string{"private", "public"} {
		if commits(t, root, name) != 0 {
			t.Fatalf("%s commits = %d, want 0", name, commits(t, root, name))
		}
	}
	// The staged content the commit started from survives the abort.
	equal(t, tracked(t, root, "private"), []string{"secrets/notes.md"})
	equal(t, tracked(t, root, "public"), []string{".gitignore", ".gitone.yml", "README.md", "src/a.go"})
}

func TestInterruptedCommitRefusesExternalChanges(t *testing.T) {
	for _, name := range []string{"recover", "abort"} {
		t.Run(name, func(t *testing.T) {
			configuration, root := interruptedCommit(t)
			// An external commit on the already committed repository makes the
			// recorded state stale.
			write(t, root, "secrets/more.md", "more\n")
			run(t, root, "private", "add", "secrets/more.md")
			run(t, root, "private", "commit", "-qm", "external")

			resolve := stage.Recover
			if name == "abort" {
				resolve = stage.Abort
			}
			var output bytes.Buffer
			err := resolve(configuration, root, &output)
			if err == nil || !strings.HasPrefix(err.Error(), recoveryRequired) {
				t.Fatalf("error = %v, want REC001", err)
			}
			for _, want := range []string{`repository "private" changed outside GitOne`, "resolve it manually", "restore that position"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error is missing %q: %v", want, err)
				}
			}
			// The state survives, and the untouched repository keeps its own.
			if _, statErr := os.Stat(filepath.Join(root, ".gitone", "recovery", "state.json")); statErr != nil {
				t.Fatal(statErr)
			}
			if commits(t, root, "public") != 0 {
				t.Fatalf("public commits = %d, want 0", commits(t, root, "public"))
			}
		})
	}
}

func TestCommitRefusesAnUnsafeProject(t *testing.T) {
	tests := map[string]struct {
		prepare func(t *testing.T, root string)
		code    string
	}{
		"contaminated index": {
			prepare: func(t *testing.T, root string) { run(t, root, "public", "add", "secrets/notes.md") },
			code:    "PATH003",
		},
		"mismatched branch": {
			prepare: func(t *testing.T, root string) {
				run(t, root, "public", "symbolic-ref", "HEAD", "refs/heads/other")
			},
			code: "REPO001",
		},
		"unsafe path": {
			prepare: func(t *testing.T, root string) {
				if err := os.Symlink("README.md", filepath.Join(root, "src", "link.go")); err != nil {
					t.Fatal(err)
				}
			},
			code: "PATH003",
		},
		"recovery state": {
			prepare: func(t *testing.T, root string) {
				if err := os.MkdirAll(filepath.Join(root, ".gitone", "recovery"), 0o700); err != nil {
					t.Fatal(err)
				}
			},
			code: recoveryRequired,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			configuration, root := staged(t, map[string]string{
				"README.md":        "readme\n",
				"src/a.go":         "a\n",
				"secrets/notes.md": "notes\n",
			}, "-A")
			test.prepare(t, root)

			err := commitError(t, configuration, root, "Implement authentication")
			if !strings.Contains(err.Error(), test.code) {
				t.Fatalf("error = %v, want %s", err, test.code)
			}
			for _, name := range []string{"private", "public"} {
				if commits(t, root, name) != 0 {
					t.Fatalf("%s commits = %d, want 0", name, commits(t, root, name))
				}
			}
		})
	}
}

func TestCommitNeverContactsARemote(t *testing.T) {
	configuration, root := staged(t, map[string]string{
		"README.md":        "readme\n",
		"secrets/notes.md": "notes\n",
	}, "-A")
	// An unreachable remote turns any attempted network access into a failure.
	missing := filepath.Join(root, "..", "missing.git")
	for _, name := range []string{"private", "public"} {
		run(t, root, name, "remote", "add", "origin", missing)
	}

	commit(t, configuration, root, "Implement authentication")

	for _, name := range []string{"private", "public"} {
		if refs := strings.TrimSpace(run(t, root, name, "for-each-ref", "refs/remotes")); refs != "" {
			t.Fatalf("%s remote refs = %q, want none", name, refs)
		}
	}

	// A failing commit must not reach a remote either.
	write(t, root, "src/a.go", "a\n")
	add(t, configuration, root, root, "-A")
	hook(t, root, "public", "pre-commit", "#!/bin/sh\nexit 1\n")
	commitError(t, configuration, root, "Add source")
	for _, name := range []string{"private", "public"} {
		if refs := strings.TrimSpace(run(t, root, name, "for-each-ref", "refs/remotes")); refs != "" {
			t.Fatalf("%s remote refs = %q, want none", name, refs)
		}
	}
}

// committed creates a project whose files are all staged and committed, which
// is where a path-limited commit starts: every selectable path is known to
// Git and every repository has a HEAD.
func committed(t *testing.T, files map[string]string) (*config.Config, string) {
	t.Helper()
	configuration, root := staged(t, files, "-A")
	commit(t, configuration, root, "Initial")
	return configuration, root
}

// stagedNames lists the paths one repository has staged against its HEAD.
func stagedNames(t *testing.T, root, name string) []string {
	t.Helper()
	return strings.Fields(run(t, root, name, "diff", "--cached", "--name-only"))
}

func TestSelectiveCommitCommitsTheWorkingTreeVersionAndLeavesTheRestStaged(t *testing.T) {
	configuration, root := committed(t, map[string]string{
		"README.md":        "readme\n",
		"src/a.go":         "a\n",
		"secrets/notes.md": "notes\n",
	})
	// README.md was staged in one version and changed again afterwards, and
	// src/a.go is a staged change of the same repository that stays out.
	write(t, root, "README.md", "staged\n")
	add(t, configuration, root, root, "README.md")
	write(t, root, "README.md", "working\n")
	write(t, root, "src/a.go", "staged a\n")
	add(t, configuration, root, root, "src/a.go")
	write(t, root, "secrets/notes.md", "unselected notes\n")
	add(t, configuration, root, root, "secrets/notes.md")

	output := commit(t, configuration, root, "Only the readme", "README.md")

	if got := run(t, root, "public", "show", "HEAD:README.md"); got != "working\n" {
		t.Fatalf("committed README.md = %q, want the working-tree version", got)
	}
	if got := run(t, root, "public", "show", "HEAD:src/a.go"); got != "a\n" {
		t.Fatalf("committed src/a.go = %q, want the version HEAD already had", got)
	}
	equal(t, stagedNames(t, root, "public"), []string{"src/a.go"})
	if commits(t, root, "private") != 1 {
		t.Fatalf("private commits = %d, want no second commit", commits(t, root, "private"))
	}
	equal(t, stagedNames(t, root, "private"), []string{"secrets/notes.md"})
	if strings.Contains(output, "GitOne-Group") {
		t.Fatalf("output = %q, want no trailer for one repository", output)
	}
}

func TestSelectiveCommitAcrossRepositoriesSharesOneVerifiedGroup(t *testing.T) {
	configuration, root := committed(t, map[string]string{
		"README.md":        "readme\n",
		"src/a.go":         "a\n",
		"secrets/notes.md": "notes\n",
	})
	// Nothing is staged: a path-limited commit takes the working tree itself.
	write(t, root, "README.md", "public change\n")
	write(t, root, "secrets/notes.md", "private change\n")
	write(t, root, "src/a.go", "unselected\n")

	output := commit(t, configuration, root, "Change both", "README.md", "secrets/notes.md")

	shared := group(t, root, "public")
	if !regexp.MustCompile(`^[0-9a-f]{8}$`).MatchString(shared) {
		t.Fatalf("group = %q, want eight hex characters", shared)
	}
	if other := group(t, root, "private"); other != shared {
		t.Fatalf("private group = %q, want %q", other, shared)
	}
	if !strings.Contains(output, "GitOne-Group: "+shared) {
		t.Fatalf("output = %q", output)
	}
	for name, want := range map[string]string{"public": "public change\n", "private": "private change\n"} {
		path := "README.md"
		if name == "private" {
			path = "secrets/notes.md"
		}
		if got := run(t, root, name, "show", "HEAD:"+path); got != want {
			t.Fatalf("committed %s = %q, want %q", path, got, want)
		}
		if commits(t, root, name) != 2 {
			t.Fatalf("%s commits = %d, want 2", name, commits(t, root, name))
		}
	}
	if got := run(t, root, "public", "show", "HEAD:src/a.go"); got != "a\n" {
		t.Fatalf("committed src/a.go = %q, want the unselected version", got)
	}
}

func TestSelectiveCommitHandlesAdditionsDeletionsAndLiteralNames(t *testing.T) {
	const literal = "src/we -ird:*.go"
	configuration, root := committed(t, map[string]string{
		"README.md":        "readme\n",
		"src/a.go":         "a\n",
		"secrets/notes.md": "notes\n",
	})
	// A new file becomes selectable once it is staged; the literal name must
	// stay a file name instead of turning into a flag or a pathspec.
	write(t, root, "src/new.go", "new\n")
	write(t, root, literal, "weird\n")
	write(t, root, "src/-dash.go", "dash\n")
	add(t, configuration, root, root, "src/new.go", literal, "./src/-dash.go")
	if err := os.Remove(filepath.Join(root, "README.md")); err != nil {
		t.Fatal(err)
	}

	commit(t, configuration, root, "Selected changes", "src/new.go", literal, "./src/-dash.go", "README.md")

	equal(t, tracked(t, root, "public"), []string{".gitignore", ".gitone.yml", "src/-dash.go", "src/a.go", "src/new.go", literal})
	for path, want := range map[string]string{"src/new.go": "new\n", literal: "weird\n", "src/-dash.go": "dash\n"} {
		if got := run(t, root, "public", "show", "HEAD:"+path); got != want {
			t.Fatalf("committed %s = %q, want %q", path, got, want)
		}
	}
}

func TestSelectiveCommitRefusesAPathGitDoesNotKnow(t *testing.T) {
	configuration, root := committed(t, map[string]string{
		"README.md":        "readme\n",
		"secrets/notes.md": "notes\n",
	})
	write(t, root, "src/fresh.go", "fresh\n")

	err := commitError(t, configuration, root, "Fresh", "src/fresh.go")
	if !strings.Contains(err.Error(), "PATH003 src/fresh.go: path is not known to Git yet") {
		t.Fatalf("error = %v, want the untracked refusal", err)
	}
	if commits(t, root, "public") != 1 {
		t.Fatalf("public commits = %d, want no second commit", commits(t, root, "public"))
	}
	if _, err := os.Stat(lock.RecoveryPath(root)); !os.IsNotExist(err) {
		t.Fatalf("recovery state exists after a refused commit: %v", err)
	}
}

func TestSelectiveCommitRefusesAnUnmanagedOrUnsupportedArgument(t *testing.T) {
	configuration, root := committed(t, map[string]string{
		"README.md":        "readme\n",
		"src/a.go":         "a\n",
		"secrets/notes.md": "notes\n",
	})
	write(t, root, "README.md", "changed\n")

	for name, argument := range map[string]string{
		"directory":  "src",
		"subtree":    ".",
		"absolute":   filepath.Join(root, "README.md"),
		"outside":    "../README.md",
		"reserved":   ".gitone/repositories",
		"unassigned": "unassigned.md",
	} {
		t.Run(name, func(t *testing.T) {
			err := commitError(t, configuration, root, "Refused", argument)
			if !strings.HasPrefix(err.Error(), "PATH003 ") {
				t.Fatalf("error = %v, want PATH003", err)
			}
			if commits(t, root, "public") != 1 {
				t.Fatalf("public commits = %d, want no second commit", commits(t, root, "public"))
			}
		})
	}
}

func TestSelectiveCommitValidatesTheResultingTreeBeforeCommitting(t *testing.T) {
	t.Run("foreign path in the resulting tree", func(t *testing.T) {
		configuration, root := committed(t, map[string]string{
			"README.md":        "readme\n",
			"secrets/notes.md": "notes\n",
		})
		// A path another repository owns can only reach a commit tree through
		// the index, which the project validation refuses before a selection
		// is even resolved.
		run(t, root, "public", "add", "secrets/notes.md")
		write(t, root, "README.md", "changed\n")

		err := commitError(t, configuration, root, "Change the readme", "README.md")
		if !strings.Contains(err.Error(), "PATH003 secrets/notes.md: path is tracked by private and public") {
			t.Fatalf("error = %v, want the refused foreign path", err)
		}
		if commits(t, root, "public") != 1 {
			t.Fatalf("public commits = %d, want no further commit", commits(t, root, "public"))
		}
	})

	t.Run("link without its target", func(t *testing.T) {
		configuration, root := committed(t, map[string]string{
			"README.md":       "readme\n",
			"src/lib/a.go":    "a\n",
			"secrets/keep.md": "keep\n",
		})
		symlink(t, root, "lib", "src/alias")
		add(t, configuration, root, root, "src/alias")
		commit(t, configuration, root, "Add the alias")
		// The link now points at a file that only the working tree and the
		// index have, so committing it alone would leave a broken link.
		write(t, root, "src/keep.go", "keep\n")
		symlink(t, root, "keep.go", "src/alias")
		add(t, configuration, root, root, "src/keep.go")

		err := commitError(t, configuration, root, "Retarget the alias", "src/alias")
		if !strings.Contains(err.Error(), "PATH003 src/alias in repository \"public\": symbolic link to keep.go: the link target src/keep.go does not exist") {
			t.Fatalf("error = %v, want the refused partial selection", err)
		}
		if got := storedTarget(t, root, "src/alias"); got != "keep.go" {
			t.Fatalf("src/alias -> %q, want the untouched working-tree link", got)
		}
		if _, err := os.Stat(lock.RecoveryPath(root)); !os.IsNotExist(err) {
			t.Fatalf("recovery state exists after a refused commit: %v", err)
		}
	})

	t.Run("hook cannot widen the selection", func(t *testing.T) {
		configuration, root := committed(t, map[string]string{
			"README.md":        "readme\n",
			"src/a.go":         "a\n",
			"secrets/notes.md": "notes\n",
		})
		write(t, root, "README.md", "selected\n")
		write(t, root, "src/a.go", "unselected\n")
		add(t, configuration, root, root, "src/a.go")
		hook(t, root, "public", "pre-commit", "#!/bin/sh\ngit add src/a.go\n")

		err := commitError(t, configuration, root, "Only the readme", "README.md")
		if !strings.Contains(err.Error(), "PATH003 src/a.go") ||
			!strings.Contains(err.Error(), "hook changed an unselected path") {
			t.Fatalf("error = %v, want the widened-selection refusal", err)
		}
		if commits(t, root, "public") != 1 {
			t.Fatalf("public commits = %d, want no further commit", commits(t, root, "public"))
		}
		if got := run(t, root, "public", "show", "HEAD:README.md"); got != "readme\n" {
			t.Fatalf("committed README.md = %q, want the original", got)
		}
		equal(t, stagedNames(t, root, "public"), []string{"src/a.go"})
	})
}

// interruptedSelectiveCommit selects one path in each repository and lets the
// commit fail in the second one, which leaves the state a later-repository
// failure produces.
func interruptedSelectiveCommit(t *testing.T) (*config.Config, string) {
	t.Helper()
	configuration, root := committed(t, map[string]string{
		"README.md":        "readme\n",
		"src/a.go":         "a\n",
		"secrets/notes.md": "notes\n",
	})
	write(t, root, "README.md", "selected\n")
	write(t, root, "secrets/notes.md", "selected notes\n")
	// A staged change of the failing repository that the selection leaves out.
	write(t, root, "src/a.go", "staged only\n")
	add(t, configuration, root, root, "src/a.go")
	hook(t, root, "public", "pre-commit", "#!/bin/sh\nexit 1\n")

	err := commitError(t, configuration, root, "Selected change", "README.md", "secrets/notes.md")
	if !strings.Contains(err.Error(), recoveryRequired) {
		t.Fatalf("error = %v, want an incomplete commit", err)
	}
	if commits(t, root, "private") != 2 || commits(t, root, "public") != 1 {
		t.Fatalf("commits = private %d, public %d, want 2 and 1", commits(t, root, "private"), commits(t, root, "public"))
	}
	return configuration, root
}

func TestRecoverFinishesAnInterruptedSelectiveCommit(t *testing.T) {
	configuration, root := interruptedSelectiveCommit(t)
	if err := os.Remove(filepath.Join(root, ".gitone", "repositories", "public", "hooks", "pre-commit")); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	if err := stage.Recover(configuration, root, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "gitone commit recovered.") {
		t.Fatalf("output = %q", output.String())
	}
	if got := run(t, root, "public", "show", "HEAD:README.md"); got != "selected\n" {
		t.Fatalf("committed README.md = %q, want the recorded selection", got)
	}
	if got := run(t, root, "public", "show", "HEAD:src/a.go"); got != "a\n" {
		t.Fatalf("committed src/a.go = %q, want the unselected version", got)
	}
	equal(t, stagedNames(t, root, "public"), []string{"src/a.go"})
	if shared := group(t, root, "public"); shared == "" || shared != group(t, root, "private") {
		t.Fatalf("group = %q, want the recorded one", shared)
	}
}

func TestAbortUndoesAnInterruptedSelectiveCommit(t *testing.T) {
	configuration, root := interruptedSelectiveCommit(t)

	var output bytes.Buffer
	if err := stage.Abort(configuration, root, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "gitone commit aborted.") {
		t.Fatalf("output = %q", output.String())
	}
	for _, name := range []string{"private", "public"} {
		if commits(t, root, name) != 1 {
			t.Fatalf("%s commits = %d, want the starting commit only", name, commits(t, root, name))
		}
	}
	// The staged change the commit started from and every working-tree file
	// survive the abort untouched.
	equal(t, stagedNames(t, root, "public"), []string{"src/a.go"})
	for name, want := range map[string]string{"README.md": "selected\n", "secrets/notes.md": "selected notes\n", "src/a.go": "staged only\n"} {
		contents, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil || string(contents) != want {
			t.Fatalf("%s = %q, %v", name, contents, err)
		}
	}
}

func TestInterruptedSelectiveCommitRefusesAChangedSelection(t *testing.T) {
	for _, name := range []string{"recover", "abort"} {
		t.Run(name, func(t *testing.T) {
			configuration, root := interruptedSelectiveCommit(t)
			write(t, root, "README.md", "changed outside GitOne\n")

			resolve := stage.Recover
			if name == "abort" {
				resolve = stage.Abort
			}
			var output bytes.Buffer
			err := resolve(configuration, root, &output)
			if err == nil || !strings.HasPrefix(err.Error(), recoveryRequired) {
				t.Fatalf("error = %v, want REC001", err)
			}
			for _, want := range []string{`the selected path "README.md" changed outside GitOne`, "resolve it manually", "put the version you wanted to commit back in place"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error is missing %q: %v", want, err)
				}
			}
			if _, statErr := os.Stat(filepath.Join(root, ".gitone", "recovery", "state.json")); statErr != nil {
				t.Fatal(statErr)
			}
			if commits(t, root, "public") != 1 {
				t.Fatalf("public commits = %d, want no commit", commits(t, root, "public"))
			}
		})
	}
}

func TestSelectiveCommitOnAnUnbornBranch(t *testing.T) {
	configuration, root := staged(t, map[string]string{
		"README.md":        "readme\n",
		"secrets/notes.md": "notes\n",
	}, "-A")

	commit(t, configuration, root, "First", "README.md")

	if commits(t, root, "public") != 1 || commits(t, root, "private") != 0 {
		t.Fatalf("commits = public %d, private %d, want 1 and 0", commits(t, root, "public"), commits(t, root, "private"))
	}
	equal(t, strings.Fields(run(t, root, "public", "ls-tree", "--name-only", "HEAD")), []string{"README.md"})
	equal(t, stagedNames(t, root, "public"), []string{".gitignore", ".gitone.yml"})
}
