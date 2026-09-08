package stage_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/devidevio/gitone/internal/config"
	"github.com/devidevio/gitone/internal/lock"
	"github.com/devidevio/gitone/internal/stage"
	"github.com/devidevio/gitone/internal/status"
)

// The four restores the CLI resolves its flags into: the index into the
// working tree, HEAD into the working tree, HEAD into the index, and HEAD into
// both.
var (
	fromIndex  = stage.RestoreTarget{Worktree: true}
	fromHead   = stage.RestoreTarget{Worktree: true, Head: true}
	intoIndex  = stage.RestoreTarget{Index: true, Head: true}
	completely = stage.RestoreTarget{Index: true, Worktree: true, Head: true}
)

func restore(t *testing.T, configuration *config.Config, root, directory string, target stage.RestoreTarget, arguments ...string) string {
	t.Helper()
	var output bytes.Buffer
	if err := stage.Restore(configuration, root, directory, arguments, target, &output); err != nil {
		t.Fatalf("restore %v: %v", arguments, err)
	}
	return output.String()
}

func restoreError(t *testing.T, configuration *config.Config, root string, target stage.RestoreTarget, arguments ...string) error {
	t.Helper()
	var output bytes.Buffer
	err := stage.Restore(configuration, root, root, arguments, target, &output)
	if err == nil {
		t.Fatalf("restore %v succeeded, want an error: %s", arguments, output.String())
	}
	return err
}

func contents(t *testing.T, root, name string) string {
	t.Helper()
	value, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
	if err != nil {
		t.Fatal(err)
	}
	return string(value)
}

// changedProject stages every path and then changes the working tree only, so
// each repository has one path a restore has to undo.
func changedProject(t *testing.T) (*config.Config, string) {
	t.Helper()
	configuration, root := project(t, map[string]string{
		"README.md":        "readme\n",
		"src/a.go":         "a\n",
		"secrets/notes.md": "notes\n",
	})
	add(t, configuration, root, root, "-A")
	write(t, root, "src/a.go", "changed\n")
	write(t, root, "secrets/notes.md", "changed\n")
	return configuration, root
}

func TestRestoreReplacesWorkingTreeFilesFromTheIndex(t *testing.T) {
	configuration, root := changedProject(t)
	write(t, root, "README.md", "changed\n")
	write(t, root, "src/empty.txt", "")
	add(t, configuration, root, root, "src/empty.txt")
	if err := os.Remove(filepath.Join(root, "src", "a.go")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "src", "empty.txt")); err != nil {
		t.Fatal(err)
	}

	output := restore(t, configuration, root, root, fromIndex, ".")

	if !strings.Contains(output, "Restored 4 paths") ||
		!strings.Contains(output, "private:\n  modified     secrets/notes.md") ||
		!strings.Contains(output, "public:\n  modified     README.md") {
		t.Fatalf("output = %q", output)
	}
	for name, want := range map[string]string{
		"README.md":        "readme\n",
		"src/a.go":         "a\n",
		"src/empty.txt":    "",
		"secrets/notes.md": "notes\n",
	} {
		if got := contents(t, root, name); got != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
	// The index is not a restore target and keeps every staged path.
	equal(t, tracked(t, root, "private"), []string{"secrets/notes.md"})
	equal(t, tracked(t, root, "public"), []string{".gitignore", ".gitone.yml", "README.md", "src/a.go", "src/empty.txt"})
}

func TestRestorePreservesTheFileMode(t *testing.T) {
	configuration, root := project(t, map[string]string{"src/tool.sh": "#!/bin/sh\n", "secrets/notes.md": "notes\n"})
	executable := filepath.Join(root, "src", "tool.sh")
	if err := os.Chmod(executable, 0o755); err != nil {
		t.Fatal(err)
	}
	add(t, configuration, root, root, "-A")
	if err := os.Chmod(executable, 0o644); err != nil {
		t.Fatal(err)
	}

	restore(t, configuration, root, root, fromIndex, "src/tool.sh")

	info, err := os.Stat(executable)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("mode = %v, want an executable file", info.Mode())
	}
}

func TestRestoreDoesNotTouchANeighboringTemporaryName(t *testing.T) {
	configuration, root := project(t, map[string]string{
		"src/a":            "index\n",
		"src/a.gitone":     "neighbor\n",
		"secrets/notes.md": "notes\n",
	})
	add(t, configuration, root, root, "-A")
	write(t, root, "src/a", "changed\n")

	restore(t, configuration, root, root, fromIndex, "src/a")

	if got := contents(t, root, "src/a"); got != "index\n" {
		t.Fatalf("src/a = %q, want the index version", got)
	}
	if got := contents(t, root, "src/a.gitone"); got != "neighbor\n" {
		t.Fatalf("src/a.gitone = %q, want the untouched neighboring file", got)
	}
}

func TestRestoreStagedUnstagesThroughTheSharedImplementation(t *testing.T) {
	configuration, root := project(t, map[string]string{"README.md": "readme\n", "secrets/notes.md": "notes\n"})
	add(t, configuration, root, root, "-A")

	output := restore(t, configuration, root, root, intoIndex, "README.md")

	if !strings.Contains(output, "Unstaged 1 path") {
		t.Fatalf("output = %q", output)
	}
	equal(t, tracked(t, root, "public"), []string{".gitignore", ".gitone.yml"})
	if got := contents(t, root, "README.md"); got != "readme\n" {
		t.Fatalf("README.md = %q, want the working-tree version", got)
	}
}

func TestRestoreDirectoryOnlySelectsItsSubtree(t *testing.T) {
	configuration, root := changedProject(t)

	restore(t, configuration, root, root, fromIndex, "secrets")

	if got := contents(t, root, "secrets/notes.md"); got != "notes\n" {
		t.Fatalf("secrets/notes.md = %q, want the index version", got)
	}
	if got := contents(t, root, "src/a.go"); got != "changed\n" {
		t.Fatalf("src/a.go = %q, want the untouched working-tree version", got)
	}
}

func TestRestoreRefusesUnsafeInputBeforeChangingAnything(t *testing.T) {
	configuration, root := changedProject(t)
	if err := os.Mkdir(filepath.Join(root, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, root)

	for _, argument := range []string{"missing.md", "empty", "src/*.go", "/etc/hosts", "../outside.md"} {
		var output bytes.Buffer
		err := stage.Restore(configuration, root, root, []string{argument, "src/a.go"}, fromIndex, &output)
		if err == nil || !strings.HasPrefix(err.Error(), "PATH003") {
			t.Fatalf("restore %q: error = %v, want PATH003", argument, err)
		}
	}
	if after := snapshot(t, root); after != before {
		t.Fatalf("project changed:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestRestoreWithoutSelectedChangesReportsNothing(t *testing.T) {
	configuration, root := project(t, map[string]string{"README.md": "readme\n", "secrets/notes.md": "notes\n"})
	add(t, configuration, root, root, "-A")

	if output := restore(t, configuration, root, root, fromIndex, "README.md"); !strings.Contains(output, "Nothing to restore.") {
		t.Fatalf("output = %q", output)
	}
}

// interruptRestore runs a restore whose second file replacement fails, which
// leaves exactly the state a crash between two replacements produces.
func interruptRestore(t *testing.T) (*config.Config, string) {
	t.Helper()
	requireUnprivileged(t)
	configuration, root := changedProject(t)

	blocked := filepath.Join(root, "src")
	if err := os.Chmod(blocked, 0o500); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	err := stage.Restore(configuration, root, root, []string{"."}, fromIndex, &output)
	if chmodErr := os.Chmod(blocked, 0o700); chmodErr != nil {
		t.Fatal(chmodErr)
	}
	if err == nil || !strings.Contains(err.Error(), "REC001") {
		t.Fatalf("error = %v, want an incomplete restore", err)
	}
	// The first repository was restored, the second one was not.
	if got := contents(t, root, "secrets/notes.md"); got != "notes\n" {
		t.Fatalf("secrets/notes.md = %q, want the index version", got)
	}
	if got := contents(t, root, "src/a.go"); got != "changed\n" {
		t.Fatalf("src/a.go = %q, want the unchanged working-tree version", got)
	}
	// The interruption is visible to every other command.
	if _, statusErr := status.Collect(configuration, root); statusErr == nil || !strings.HasPrefix(statusErr.Error(), "REC001") {
		t.Fatalf("status error = %v, want REC001", statusErr)
	}
	return configuration, root
}

func TestRecoverCompletesInterruptedRestore(t *testing.T) {
	configuration, root := interruptRestore(t)

	var output bytes.Buffer
	if err := stage.Recover(configuration, root, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "gitone restore recovered.") {
		t.Fatalf("output = %q", output.String())
	}
	for name, want := range map[string]string{"secrets/notes.md": "notes\n", "src/a.go": "a\n"} {
		if got := contents(t, root, name); got != want {
			t.Fatalf("%s = %q, want the index version %q", name, got, want)
		}
	}
}

func TestAbortUndoesInterruptedRestore(t *testing.T) {
	configuration, root := interruptRestore(t)

	var output bytes.Buffer
	if err := stage.Abort(configuration, root, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "gitone restore aborted.") {
		t.Fatalf("output = %q", output.String())
	}
	for _, name := range []string{"secrets/notes.md", "src/a.go"} {
		if got := contents(t, root, name); got != "changed\n" {
			t.Fatalf("%s = %q, want the working-tree version the restore started from", name, got)
		}
	}
}

func TestInterruptedRestoreRefusesExternallyChangedPaths(t *testing.T) {
	for _, external := range []string{"contents", "mode"} {
		for _, name := range []string{"recover", "abort"} {
			t.Run(external+"/"+name, func(t *testing.T) {
				configuration, root := interruptRestore(t)
				file := filepath.Join(root, "secrets", "notes.md")
				if external == "contents" {
					write(t, root, "secrets/notes.md", "external\n")
				} else if err := os.Chmod(file, 0o600); err != nil {
					t.Fatal(err)
				}

				resolve := stage.Recover
				if name == "abort" {
					resolve = stage.Abort
				}
				var output bytes.Buffer
				err := resolve(configuration, root, &output)
				if err == nil || !strings.HasPrefix(err.Error(), "REC001") {
					t.Fatalf("error = %v, want REC001", err)
				}
				if !strings.Contains(err.Error(), `path "secrets/notes.md" changed outside GitOne`) {
					t.Fatalf("error = %v", err)
				}
				if external == "contents" {
					if got := contents(t, root, "secrets/notes.md"); got != "external\n" {
						t.Fatalf("secrets/notes.md = %q, want the external version", got)
					}
				} else if info, statErr := os.Stat(file); statErr != nil {
					t.Fatal(statErr)
				} else if info.Mode().Perm() != 0o600 {
					t.Fatalf("mode = %v, want the external mode", info.Mode().Perm())
				}
			})
		}
	}
}

// symlink creates a symbolic link with the exact stored target, which is what
// GitOne has to preserve through a transaction.
func symlink(t *testing.T, root, target, name string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(full); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Symlink(target, full); err != nil {
		t.Fatal(err)
	}
}

func storedTarget(t *testing.T, root, name string) string {
	t.Helper()
	target, err := os.Readlink(filepath.Join(root, filepath.FromSlash(name)))
	if err != nil {
		t.Fatalf("%s is not a symbolic link: %v", name, err)
	}
	return target
}

// aliasProject stages the alias src/alias -> lib beside its real target and
// then retargets it in the working tree only, so a restore has to put the
// recorded link back as a link.
func aliasProject(t *testing.T) (*config.Config, string) {
	t.Helper()
	configuration, root := project(t, map[string]string{
		"src/lib/a.go":     "a\n",
		"src/other/b.go":   "b\n",
		"secrets/notes.md": "notes\n",
	})
	symlink(t, root, "lib", "src/alias")
	add(t, configuration, root, root, "-A")
	symlink(t, root, "other", "src/alias")
	return configuration, root
}

func TestRestorePutsASymbolicLinkBackAsALink(t *testing.T) {
	configuration, root := aliasProject(t)

	restore(t, configuration, root, root, fromIndex, "src/alias")

	if got := storedTarget(t, root, "src/alias"); got != "lib" {
		t.Fatalf("src/alias -> %q, want the recorded target lib", got)
	}
}

func TestRestoreRefusesASelectionThatWouldBreakALink(t *testing.T) {
	configuration, root := project(t, map[string]string{"src/lib/a.go": "a\n"})
	symlink(t, root, "lib", "src/alias")
	add(t, configuration, root, root, "-A")
	// The alias now points at a directory the working tree no longer has, so
	// only restoring both paths together leaves a valid link behind.
	if err := os.RemoveAll(filepath.Join(root, "src", "lib")); err != nil {
		t.Fatal(err)
	}
	write(t, root, "src/keep.go", "keep\n")
	symlink(t, root, "keep.go", "src/alias")

	var output bytes.Buffer
	err := stage.Restore(configuration, root, root, []string{"src/alias"}, fromIndex, &output)
	if err == nil || !strings.Contains(err.Error(), "PATH003 src/alias: symbolic link to lib: the link target src/lib does not exist") {
		t.Fatalf("error = %v, want the refused partial selection", err)
	}
	if got := storedTarget(t, root, "src/alias"); got != "keep.go" {
		t.Fatalf("src/alias -> %q, want the untouched working-tree link", got)
	}
	if _, err := os.Stat(lock.RecoveryPath(root)); !os.IsNotExist(err) {
		t.Fatalf("recovery state exists after a refused restore: %v", err)
	}
}

// interruptLinkRestore runs a restore whose alias replacement fails, which
// leaves the recorded link half applied.
func interruptLinkRestore(t *testing.T) (*config.Config, string) {
	t.Helper()
	requireUnprivileged(t)
	configuration, root := aliasProject(t)
	write(t, root, "secrets/notes.md", "changed\n")

	blocked := filepath.Join(root, "src")
	if err := os.Chmod(blocked, 0o500); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	err := stage.Restore(configuration, root, root, []string{"."}, fromIndex, &output)
	if chmodErr := os.Chmod(blocked, 0o700); chmodErr != nil {
		t.Fatal(chmodErr)
	}
	if err == nil || !strings.Contains(err.Error(), "REC001") {
		t.Fatalf("error = %v, want an incomplete restore", err)
	}
	if got := storedTarget(t, root, "src/alias"); got != "other" {
		t.Fatalf("src/alias -> %q, want the unreplaced working-tree link", got)
	}
	return configuration, root
}

func TestRecoverAndAbortPreserveTheExactLinkTarget(t *testing.T) {
	for name, want := range map[string]string{"recover": "lib", "abort": "other"} {
		t.Run(name, func(t *testing.T) {
			configuration, root := interruptLinkRestore(t)

			resolve := stage.Recover
			if name == "abort" {
				resolve = stage.Abort
			}
			var output bytes.Buffer
			if err := resolve(configuration, root, &output); err != nil {
				t.Fatal(err)
			}
			if got := storedTarget(t, root, "src/alias"); got != want {
				t.Fatalf("src/alias -> %q, want %q", got, want)
			}
		})
	}
}

func TestInterruptedRestoreRefusesAnExternallyRetargetedLink(t *testing.T) {
	configuration, root := interruptLinkRestore(t)
	write(t, root, "src/third/c.go", "c\n")
	symlink(t, root, "third", "src/alias")

	var output bytes.Buffer
	err := stage.Recover(configuration, root, &output)
	if err == nil || !strings.Contains(err.Error(), `path "src/alias" changed outside GitOne`) {
		t.Fatalf("error = %v, want REC001 for the retargeted link", err)
	}
	if got := storedTarget(t, root, "src/alias"); got != "third" {
		t.Fatalf("src/alias -> %q, want the external link", got)
	}
}

func remove(t *testing.T, root, name string) {
	t.Helper()
	if err := os.Remove(filepath.Join(root, filepath.FromSlash(name))); err != nil {
		t.Fatal(err)
	}
}

// headProject is a committed project whose public repository holds every
// difference a restore from HEAD has to undo: a staged-only change, a staged
// change that was edited again, a staged deletion, a tracked file deleted
// without staging it, and a file only the index knows. The private repository
// and an untracked file are what has to stay untouched.
func headProject(t *testing.T) (*config.Config, string) {
	t.Helper()
	configuration, root := committed(t, map[string]string{
		"README.md":        "readme\n",
		"src/a.go":         "a\n",
		"src/gone.go":      "gone\n",
		"src/kept.go":      "kept\n",
		"secrets/notes.md": "notes\n",
	})
	write(t, root, "README.md", "staged\n")
	write(t, root, "src/a.go", "staged\n")
	write(t, root, "src/new.go", "new\n")
	remove(t, root, "src/gone.go")
	add(t, configuration, root, root, "-A")

	write(t, root, "src/a.go", "changed again\n")
	remove(t, root, "src/kept.go")
	write(t, root, "src/untracked.txt", "untracked\n")
	write(t, root, "secrets/notes.md", "changed\n")
	return configuration, root
}

func TestCompleteRestoreMakesIndexAndWorkingTreeMatchHead(t *testing.T) {
	configuration, root := headProject(t)

	output := restore(t, configuration, root, root, completely,
		"README.md", "src/a.go", "src/gone.go", "src/kept.go", "src/new.go")

	if !strings.Contains(output, "Restored 5 paths") {
		t.Fatalf("output = %q", output)
	}
	for name, want := range map[string]string{
		"README.md":   "readme\n",
		"src/a.go":    "a\n",
		"src/gone.go": "gone\n",
		"src/kept.go": "kept\n",
	} {
		if got := contents(t, root, name); got != want {
			t.Fatalf("%s = %q, want the HEAD version %q", name, got, want)
		}
	}
	// A path HEAD does not hold is removed instead of checked out.
	if _, err := os.Stat(filepath.Join(root, "src", "new.go")); !os.IsNotExist(err) {
		t.Fatalf("src/new.go still exists: %v", err)
	}
	equal(t, stagedPaths(t, root, "public"), nil)
	// Nothing outside the selection moved, in either repository.
	if got := contents(t, root, "src/untracked.txt"); got != "untracked\n" {
		t.Fatalf("src/untracked.txt = %q, want the untouched untracked file", got)
	}
	if got := contents(t, root, "secrets/notes.md"); got != "changed\n" {
		t.Fatalf("secrets/notes.md = %q, want the untouched working-tree version", got)
	}
	equal(t, stagedPaths(t, root, "private"), nil)
}

func TestRestoreFromHeadLeavesTheIndexAlone(t *testing.T) {
	configuration, root := headProject(t)
	before := stagedPaths(t, root, "public")

	restore(t, configuration, root, root, fromHead, "README.md", "src/a.go")

	for name, want := range map[string]string{"README.md": "readme\n", "src/a.go": "a\n"} {
		if got := contents(t, root, name); got != want {
			t.Fatalf("%s = %q, want the HEAD version %q", name, got, want)
		}
	}
	equal(t, stagedPaths(t, root, "public"), before)
}

func TestRestoreStagedFromHeadLeavesTheWorkingTreeAlone(t *testing.T) {
	configuration, root := headProject(t)

	restore(t, configuration, root, root, intoIndex, "README.md", "src/new.go")

	equal(t, stagedPaths(t, root, "public"), []string{"src/a.go", "src/gone.go"})
	for name, want := range map[string]string{"README.md": "staged\n", "src/new.go": "new\n"} {
		if got := contents(t, root, name); got != want {
			t.Fatalf("%s = %q, want the untouched working-tree version %q", name, got, want)
		}
	}
}

func TestRestoreFromHeadRestoresTheCommittedFileMode(t *testing.T) {
	identity(t)
	configuration, root := project(t, map[string]string{"src/tool.sh": "#!/bin/sh\n", "secrets/notes.md": "notes\n"})
	executable := filepath.Join(root, "src", "tool.sh")
	if err := os.Chmod(executable, 0o755); err != nil {
		t.Fatal(err)
	}
	add(t, configuration, root, root, "-A")
	commit(t, configuration, root, "Initial")
	if err := os.Chmod(executable, 0o644); err != nil {
		t.Fatal(err)
	}
	write(t, root, "src/tool.sh", "changed\n")

	restore(t, configuration, root, root, fromHead, "src/tool.sh")

	if got := contents(t, root, "src/tool.sh"); got != "#!/bin/sh\n" {
		t.Fatalf("src/tool.sh = %q, want the committed version", got)
	}
	info, err := os.Stat(executable)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("mode = %v, want an executable file", info.Mode())
	}
}

func TestCompleteRestorePutsASymbolicLinkBackAsALink(t *testing.T) {
	identity(t)
	configuration, root := project(t, map[string]string{
		"src/lib/a.go":     "a\n",
		"src/other/b.go":   "b\n",
		"secrets/notes.md": "notes\n",
	})
	symlink(t, root, "lib", "src/alias")
	add(t, configuration, root, root, "-A")
	commit(t, configuration, root, "Initial")
	symlink(t, root, "other", "src/alias")
	add(t, configuration, root, root, "src/alias")

	restore(t, configuration, root, root, completely, "src/alias")

	if got := storedTarget(t, root, "src/alias"); got != "lib" {
		t.Fatalf("src/alias -> %q, want the committed target lib", got)
	}
	equal(t, stagedPaths(t, root, "public"), nil)
}

func TestRestoreFromHeadRefusesARepositoryWithoutACommit(t *testing.T) {
	configuration, root := changedProject(t)
	before := snapshot(t, root)

	err := restoreError(t, configuration, root, completely, "src/a.go")
	if !strings.HasPrefix(err.Error(), "REPO001") || !strings.Contains(err.Error(), "has no commit yet") {
		t.Fatalf("error = %v, want REPO001 for the unborn repository", err)
	}
	if after := snapshot(t, root); after != before {
		t.Fatalf("project changed:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestRestoreRefusesAStructuralConfigurationChange(t *testing.T) {
	_, root := committed(t, map[string]string{"README.md": "readme\n", "secrets/notes.md": "notes\n"})
	// The working tree configures a remote the committed configuration does
	// not have, so restoring the committed one would remove it again.
	changed := strings.Replace(twoRepositories, "  public:\n    visibility: public\n",
		"  public:\n    visibility: public\n    remote: https://example.com/site.git\n", 1)
	write(t, root, ".gitone.yml", changed)
	reloaded, _, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}

	failure := restoreError(t, reloaded, root, fromHead, ".gitone.yml")
	if !strings.HasPrefix(failure.Error(), "PATH003") ||
		!strings.Contains(failure.Error(), "repositories.public.remotes.origin") ||
		!strings.Contains(failure.Error(), "gitone reconfigure") {
		t.Fatalf("error = %v, want the refused structural change", failure)
	}
	if got := contents(t, root, ".gitone.yml"); got != changed {
		t.Fatalf(".gitone.yml = %q, want the untouched working-tree version", got)
	}
	if _, err := os.Stat(lock.RecoveryPath(root)); !os.IsNotExist(err) {
		t.Fatalf("recovery state exists after a refused restore: %v", err)
	}
}

// interruptCompleteRestore runs a complete restore whose last file replacement
// fails, which leaves the indexes and one file replaced and one file not.
func interruptCompleteRestore(t *testing.T) (*config.Config, string) {
	t.Helper()
	requireUnprivileged(t)
	configuration, root := committed(t, map[string]string{
		"README.md":        "readme\n",
		"src/a.go":         "a\n",
		"secrets/notes.md": "notes\n",
	})
	write(t, root, "src/a.go", "changed\n")
	write(t, root, "secrets/notes.md", "changed\n")
	add(t, configuration, root, root, "-A")

	blocked := filepath.Join(root, "src")
	if err := os.Chmod(blocked, 0o500); err != nil {
		t.Fatal(err)
	}
	err := restoreError(t, configuration, root, completely, ".")
	if chmodErr := os.Chmod(blocked, 0o700); chmodErr != nil {
		t.Fatal(chmodErr)
	}
	if !strings.Contains(err.Error(), "REC001") {
		t.Fatalf("error = %v, want an incomplete restore", err)
	}
	// Both indexes and the first file were replaced, the second file was not.
	equal(t, stagedPaths(t, root, "public"), nil)
	if got := contents(t, root, "secrets/notes.md"); got != "notes\n" {
		t.Fatalf("secrets/notes.md = %q, want the HEAD version", got)
	}
	if got := contents(t, root, "src/a.go"); got != "changed\n" {
		t.Fatalf("src/a.go = %q, want the unchanged working-tree version", got)
	}
	return configuration, root
}

func TestRecoverCompletesInterruptedCompleteRestore(t *testing.T) {
	configuration, root := interruptCompleteRestore(t)

	var output bytes.Buffer
	if err := stage.Recover(configuration, root, &output); err != nil {
		t.Fatal(err)
	}
	if got := contents(t, root, "src/a.go"); got != "a\n" {
		t.Fatalf("src/a.go = %q, want the HEAD version", got)
	}
	equal(t, stagedPaths(t, root, "public"), nil)
}

func TestAbortUndoesInterruptedCompleteRestore(t *testing.T) {
	configuration, root := interruptCompleteRestore(t)

	var output bytes.Buffer
	if err := stage.Abort(configuration, root, &output); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"src/a.go": "changed\n", "secrets/notes.md": "changed\n"} {
		if got := contents(t, root, name); got != want {
			t.Fatalf("%s = %q, want the working-tree version the restore started from", name, got)
		}
	}
	// The index changes the restore already applied are undone with them.
	equal(t, stagedPaths(t, root, "public"), []string{"src/a.go"})
	equal(t, stagedPaths(t, root, "private"), []string{"secrets/notes.md"})
}

func TestRestoreRefusesProjectedIndexOwnershipChange(t *testing.T) {
	for name, target := range map[string]stage.RestoreTarget{
		"index to worktree": fromIndex,
		"HEAD to worktree":  fromHead,
		"HEAD to both":      completely,
	} {
		t.Run(name, func(t *testing.T) {
			_, root := committed(t, map[string]string{"README.md": "readme\n"})
			changed := strings.Replace(twoRepositories, "      - secrets/**", "      - private/**", 1)
			changed = strings.Replace(changed, "      - src/**", "      - src/**\n      - secrets/**", 1)
			write(t, root, ".gitone.yml", changed)
			write(t, root, "secrets/new.txt", "private data\n")
			current, _, err := config.Load(root)
			if err != nil {
				t.Fatal(err)
			}
			add(t, current, root, root, "secrets/new.txt")
			if _, _, err := status.Validate(current, root); err != nil {
				t.Fatal(err)
			}
			before := snapshot(t, root)

			err = restoreError(t, current, root, target, ".gitone.yml")

			if !strings.Contains(err.Error(), "PATH003") || !strings.Contains(err.Error(), "secrets/new.txt") {
				t.Fatalf("error = %v, want refused index ownership", err)
			}
			if after := snapshot(t, root); after != before {
				t.Fatalf("project changed:\nbefore:\n%s\nafter:\n%s", before, after)
			}
		})
	}
}

func TestCompleteRestoreRefusesBrokenIndexLink(t *testing.T) {
	identity(t)
	configuration, root := project(t, map[string]string{"src/lib.go": "a\n", "secrets/notes.md": "notes\n"})
	symlink(t, root, "lib.go", "src/alias")
	add(t, configuration, root, root, "-A")
	commit(t, configuration, root, "Initial")
	if err := os.Remove(filepath.Join(root, "src/alias")); err != nil {
		t.Fatal(err)
	}
	write(t, root, "src/alias", "regular\n")
	if err := os.Remove(filepath.Join(root, "src/lib.go")); err != nil {
		t.Fatal(err)
	}
	add(t, configuration, root, root, "-A")
	// The working tree has a valid target, but the prepared index would not.
	write(t, root, "src/lib.go", "a\n")
	before := snapshot(t, root)

	err := restoreError(t, configuration, root, completely, "src/alias")

	if !strings.Contains(err.Error(), "PATH003") || !strings.Contains(err.Error(), "the link target src/lib.go does not exist") {
		t.Fatalf("error = %v, want refused index link", err)
	}
	if after := snapshot(t, root); after != before {
		t.Fatalf("project changed:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	// Selecting the target as well produces a valid index and working tree.
	restore(t, configuration, root, root, completely, "src/alias", "src/lib.go")
	if _, _, err := status.Validate(configuration, root); err != nil {
		t.Fatal(err)
	}
}
