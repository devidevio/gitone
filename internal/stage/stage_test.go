package stage_test

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/devidevio/gitone/internal/config"
	"github.com/devidevio/gitone/internal/git"
	"github.com/devidevio/gitone/internal/repository"
	"github.com/devidevio/gitone/internal/stage"
	"github.com/devidevio/gitone/internal/status"
)

const twoRepositories = `
version: 1
default_branch: main
repositories:
  private:
    visibility: private
    paths:
      - secrets/**
  public:
    visibility: public
    paths:
      - .gitignore
      - .gitone.yml
      - README.md
      - src/**
`

// project creates an initialized project containing files.
func project(t *testing.T, files map[string]string) (*config.Config, string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	write(t, root, ".gitone.yml", twoRepositories)
	for name, contents := range files {
		write(t, root, name, contents)
	}
	configuration, discovered, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.Init(configuration, discovered); err != nil {
		t.Fatal(err)
	}
	return configuration, discovered
}

// writes counts the files written so far, so every write can get its own
// modification time. The tests never run in parallel, so one counter is enough.
var writes int

// write replaces a file and gives it a modification time no earlier write
// used. Git compares whole seconds on this platform, so a replacement of the
// same length written in the same second would otherwise look unchanged and
// stage nothing.
func write(t *testing.T, root, name, contents string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	writes++
	stamp := time.Now().Add(-time.Duration(writes) * time.Second)
	if err := os.Chtimes(full, stamp, stamp); err != nil {
		t.Fatal(err)
	}
}

// run executes git against one managed repository the way GitOne does.
func run(t *testing.T, root, name string, arguments ...string) string {
	t.Helper()
	arguments = append([]string{"--git-dir=" + repository.Directory(root, name), "--work-tree=" + root,
		"-c", "user.name=GitOne", "-c", "user.email=gitone@example.com"}, arguments...)
	output, err := git.Run(root, arguments...)
	if err != nil {
		t.Fatal(err)
	}
	return output
}

// tracked lists the paths in the index of one repository. The listing is NUL
// separated, so paths containing spaces stay comparable.
func tracked(t *testing.T, root, name string) []string {
	t.Helper()
	var paths []string
	for _, path := range strings.Split(run(t, root, name, "ls-files", "-z"), "\x00") {
		if path != "" {
			paths = append(paths, path)
		}
	}
	return paths
}

func add(t *testing.T, configuration *config.Config, root, directory string, arguments ...string) string {
	t.Helper()
	var output bytes.Buffer
	if err := stage.Add(configuration, root, directory, arguments, &output); err != nil {
		t.Fatalf("add %v: %v", arguments, err)
	}
	return output.String()
}

func unstage(t *testing.T, configuration *config.Config, root, directory string, arguments ...string) string {
	t.Helper()
	var output bytes.Buffer
	if err := stage.Unstage(configuration, root, directory, arguments, &output); err != nil {
		t.Fatalf("unstage %v: %v", arguments, err)
	}
	return output.String()
}

func stagedPaths(t *testing.T, root, name string) []string {
	t.Helper()
	var paths []string
	for _, path := range strings.Split(run(t, root, name, "diff", "--cached", "--name-only", "-z"), "\x00") {
		if path != "" {
			paths = append(paths, path)
		}
	}
	return paths
}

func equal(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// snapshot records the contents and modification times of every file below
// directory, so a test can prove that nothing changed.
func snapshot(t *testing.T, directory string) string {
	t.Helper()
	var builder strings.Builder
	err := filepath.Walk(directory, func(name string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		contents, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		builder.WriteString(name + " " + info.ModTime().String() + " " + string(contents) + "\n")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return builder.String()
}

func TestAddStagesEveryPathIntoItsOwnRepository(t *testing.T) {
	configuration, root := project(t, map[string]string{
		"README.md":        "readme\n",
		"src/a.go":         "a\n",
		"secrets/notes.md": "notes\n",
	})

	output := add(t, configuration, root, root, "-A")
	if !strings.Contains(output, "private:\n  added        secrets/notes.md") ||
		!strings.Contains(output, "public:\n  added        .gitignore") {
		t.Fatalf("output = %q", output)
	}
	equal(t, tracked(t, root, "private"), []string{"secrets/notes.md"})
	equal(t, tracked(t, root, "public"), []string{".gitignore", ".gitone.yml", "README.md", "src/a.go"})
}

func TestUnstageAllFromUnbornRepositoriesKeepsTheWorkingTree(t *testing.T) {
	configuration, root := project(t, map[string]string{
		"README.md":        "readme\n",
		"src/a.go":         "a\n",
		"secrets/notes.md": "notes\n",
	})
	add(t, configuration, root, root, "-A")

	output := unstage(t, configuration, root, root, "-A")

	if !strings.Contains(output, "Unstaged 5 paths") {
		t.Fatalf("output = %q", output)
	}
	equal(t, tracked(t, root, "private"), nil)
	equal(t, tracked(t, root, "public"), nil)
	for name, want := range map[string]string{
		"README.md":        "readme\n",
		"src/a.go":         "a\n",
		"secrets/notes.md": "notes\n",
	} {
		contents, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil || string(contents) != want {
			t.Fatalf("%s = %q, %v", name, contents, err)
		}
	}
}

func TestUnstageSelectsPathsAcrossRepositoriesAndKeepsLaterEdits(t *testing.T) {
	configuration, root := project(t, map[string]string{
		"README.md":        "initial\n",
		"secrets/notes.md": "initial\n",
	})
	add(t, configuration, root, root, "-A")
	run(t, root, "private", "commit", "-qm", "initial")
	run(t, root, "public", "commit", "-qm", "initial")
	write(t, root, "README.md", "staged\n")
	write(t, root, "secrets/notes.md", "staged\n")
	add(t, configuration, root, root, "-A")
	write(t, root, "README.md", "working tree\n")

	unstage(t, configuration, root, root, "README.md")

	equal(t, stagedPaths(t, root, "public"), nil)
	equal(t, stagedPaths(t, root, "private"), []string{"secrets/notes.md"})
	contents, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil || string(contents) != "working tree\n" {
		t.Fatalf("README.md = %q, %v", contents, err)
	}
}

func TestUnstageRenameRestoresBothIndexPaths(t *testing.T) {
	configuration, root := project(t, map[string]string{"src/a.go": "a\n"})
	add(t, configuration, root, root, "-A")
	run(t, root, "public", "commit", "-qm", "initial")
	if err := os.Rename(filepath.Join(root, "src", "a.go"), filepath.Join(root, "src", "b.go")); err != nil {
		t.Fatal(err)
	}
	add(t, configuration, root, root, "-A")

	unstage(t, configuration, root, root, "src/b.go")

	equal(t, tracked(t, root, "public"), []string{".gitignore", ".gitone.yml", "src/a.go"})
	equal(t, stagedPaths(t, root, "public"), nil)
	if _, err := os.Stat(filepath.Join(root, "src", "b.go")); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverCompletesInterruptedUnstage(t *testing.T) {
	requireUnprivileged(t)
	configuration, root := project(t, map[string]string{
		"README.md":        "initial\n",
		"secrets/notes.md": "initial\n",
	})
	add(t, configuration, root, root, "-A")
	run(t, root, "private", "commit", "-qm", "initial")
	run(t, root, "public", "commit", "-qm", "initial")
	write(t, root, "README.md", "changed\n")
	write(t, root, "secrets/notes.md", "changed\n")
	add(t, configuration, root, root, "-A")

	blocked := repository.Directory(root, "public")
	if err := os.Chmod(blocked, 0o500); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	err := stage.Unstage(configuration, root, root, []string{"-A"}, &output)
	if chmodErr := os.Chmod(blocked, 0o700); chmodErr != nil {
		t.Fatal(chmodErr)
	}
	if err == nil || !strings.Contains(err.Error(), "REC001") {
		t.Fatalf("error = %v, want an incomplete operation", err)
	}
	equal(t, stagedPaths(t, root, "private"), nil)
	equal(t, stagedPaths(t, root, "public"), []string{"README.md"})

	output.Reset()
	if err := stage.Recover(configuration, root, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "gitone unstage recovered.") {
		t.Fatalf("output = %q", output.String())
	}
	equal(t, stagedPaths(t, root, "private"), nil)
	equal(t, stagedPaths(t, root, "public"), nil)
}

func TestAddResolvesPathArgumentsRelativeToTheCallerAndSubtree(t *testing.T) {
	configuration, root := project(t, map[string]string{
		"README.md":        "readme\n",
		"src/a.go":         "a\n",
		"src/deep/b.go":    "b\n",
		"secrets/notes.md": "notes\n",
	})

	// A shell-expanded file and directory select the file and the directory's
	// complete subtree.
	add(t, configuration, root, root, "README.md", "src")
	equal(t, tracked(t, root, "public"), []string{"README.md", "src/a.go", "src/deep/b.go"})
	equal(t, tracked(t, root, "private"), nil)

	// Explicit relative files are still resolved from a sibling directory.
	add(t, configuration, root, filepath.Join(root, "src"), "../secrets/notes.md")
	equal(t, tracked(t, root, "public"), []string{"README.md", "src/a.go", "src/deep/b.go"})
	equal(t, tracked(t, root, "private"), []string{"secrets/notes.md"})
}

func TestAddSelectsTheParentSubtree(t *testing.T) {
	configuration, root := project(t, map[string]string{
		"README.md":        "readme\n",
		"src/a.go":         "a\n",
		"secrets/notes.md": "notes\n",
	})

	// ".." names the project root from src/, so it selects the whole project.
	add(t, configuration, root, filepath.Join(root, "src"), "..")
	equal(t, tracked(t, root, "public"), []string{".gitignore", ".gitone.yml", "README.md", "src/a.go"})
	equal(t, tracked(t, root, "private"), []string{"secrets/notes.md"})
}

func TestAddSeparatesTrackedAndUntrackedScopes(t *testing.T) {
	configuration, root := project(t, map[string]string{
		"README.md":        "readme\n",
		"src/a.go":         "a\n",
		"src/gone.go":      "gone\n",
		"secrets/notes.md": "notes\n",
	})
	add(t, configuration, root, root, "-A")
	run(t, root, "public", "commit", "-qm", "initial")
	run(t, root, "private", "commit", "-qm", "initial")

	write(t, root, "src/a.go", "changed\n")
	if err := os.Remove(filepath.Join(root, "src/gone.go")); err != nil {
		t.Fatal(err)
	}
	write(t, root, "src/new.go", "new\n")

	// -u stages the changed and deleted tracked paths of the whole project
	// but never an untracked one.
	output := add(t, configuration, root, filepath.Join(root, "secrets"), "-u")
	if !strings.Contains(output, "modified     src/a.go") || !strings.Contains(output, "deleted      src/gone.go") ||
		strings.Contains(output, "src/new.go") {
		t.Fatalf("output = %q", output)
	}
	equal(t, tracked(t, root, "public"), []string{".gitignore", ".gitone.yml", "README.md", "src/a.go"})

	// -A also covers the untracked path, again project-wide.
	add(t, configuration, root, filepath.Join(root, "secrets"), "-A")
	equal(t, tracked(t, root, "public"), []string{".gitignore", ".gitone.yml", "README.md", "src/a.go", "src/new.go"})
}

func TestAddStagesNothingTwice(t *testing.T) {
	configuration, root := project(t, map[string]string{"README.md": "readme\n", "secrets/notes.md": "notes\n"})
	add(t, configuration, root, root, "-A")

	if output := add(t, configuration, root, root, "-A"); !strings.Contains(output, "Nothing to stage.") {
		t.Fatalf("output = %q", output)
	}
	// An unchanged managed path is accepted and stages nothing.
	if output := add(t, configuration, root, root, "README.md"); !strings.Contains(output, "Nothing to stage.") {
		t.Fatalf("output = %q", output)
	}
}

func TestAddRejectsUnusablePaths(t *testing.T) {
	configuration, root := project(t, map[string]string{
		"README.md":        "readme\n",
		"src/a.go":         "a\n",
		"secrets/notes.md": "notes\n",
		".gitignore":       "build/\n",
		"build/out.o":      "binary\n",
	})
	if err := os.Mkdir(filepath.Join(root, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}

	tests := map[string]struct{ argument, code string }{
		"absolute":          {filepath.Join(root, "README.md"), "PATH003"},
		"outside":           {"../escape.md", "PATH003"},
		"reserved":          {".gitone.local.yml", "PATH003"},
		"internal":          {".gitone/repositories", "PATH003"},
		"ignored":           {"build/out.o", "PATH003"},
		"unknown":           {"missing.md", "PATH003"},
		"empty":             {"empty", "PATH003"},
		"ignored directory": {"build", "PATH003"},
		"unknown directory": {"missing", "PATH003"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			var output bytes.Buffer
			err := stage.Add(configuration, root, root, []string{test.argument}, &output)
			if err == nil {
				t.Fatalf("add %q succeeded", test.argument)
			}
			if !strings.HasPrefix(err.Error(), test.code) {
				t.Fatalf("error = %v, want %s", err, test.code)
			}
			// A rejected argument stages nothing at all.
			equal(t, tracked(t, root, "public"), nil)
		})
	}
}

func TestAddRejectsUnsafeProjectState(t *testing.T) {
	configuration, root := project(t, map[string]string{
		"README.md":        "readme\n",
		"src/a.go":         "a\n",
		"secrets/notes.md": "notes\n",
	})
	// An unassigned path makes the whole project unsafe, even though the
	// requested path itself is owned.
	write(t, root, "stray.txt", "stray\n")

	before := snapshot(t, filepath.Join(root, ".gitone"))
	var output bytes.Buffer
	err := stage.Add(configuration, root, root, []string{"src/a.go"}, &output)
	if err == nil || !strings.Contains(err.Error(), "PATH001 stray.txt") {
		t.Fatalf("error = %v, want PATH001", err)
	}
	if after := snapshot(t, filepath.Join(root, ".gitone")); after != before {
		t.Fatalf("internal state changed:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestAddLeavesEveryIndexUnchangedWhenGitFails(t *testing.T) {
	requireUnprivileged(t)
	configuration, root := project(t, map[string]string{
		"README.md":        "readme\n",
		"src/a.go":         "a\n",
		"secrets/notes.md": "notes\n",
	})
	add(t, configuration, root, root, "-A")
	run(t, root, "private", "commit", "-qm", "initial")
	run(t, root, "public", "commit", "-qm", "initial")
	write(t, root, "src/a.go", "public change\n")
	write(t, root, "secrets/notes.md", "private change\n")

	// Git cannot write the new blob of the public repository, so its
	// preparation fails after the private one already succeeded.
	objects := filepath.Join(repository.Directory(root, "public"), "objects")
	if err := os.Chmod(objects, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(objects, 0o700)

	privateIndex := filepath.Join(repository.Directory(root, "private"), "index")
	publicIndex := filepath.Join(repository.Directory(root, "public"), "index")
	privateBefore := snapshot(t, privateIndex)
	publicBefore := snapshot(t, publicIndex)
	var output bytes.Buffer
	err := stage.Add(configuration, root, root, []string{"-A"}, &output)
	if err == nil || !strings.HasPrefix(err.Error(), "GIT001") {
		t.Fatalf("error = %v, want GIT001", err)
	}
	if after := snapshot(t, privateIndex); after != privateBefore {
		t.Fatalf("private index changed:\nbefore:\n%s\nafter:\n%s", privateBefore, after)
	}
	if after := snapshot(t, publicIndex); after != publicBefore {
		t.Fatalf("public index changed:\nbefore:\n%s\nafter:\n%s", publicBefore, after)
	}
	// A failed preparation is no interrupted operation.
	if _, err := os.Stat(filepath.Join(root, ".gitone", "recovery")); !os.IsNotExist(err) {
		t.Fatalf("recovery state = %v, want none", err)
	}
}

func TestAddNeverChangesTheWorkingTree(t *testing.T) {
	configuration, root := project(t, map[string]string{
		"README.md":        "readme\n",
		"src/a.go":         "a\n",
		"src/gone.go":      "gone\n",
		"secrets/notes.md": "notes\n",
	})
	add(t, configuration, root, root, "-A")
	run(t, root, "public", "commit", "-qm", "initial")
	write(t, root, "src/a.go", "changed\n")
	if err := os.Remove(filepath.Join(root, "src/gone.go")); err != nil {
		t.Fatal(err)
	}
	write(t, root, "src/new.go", "new\n")

	before := snapshot(t, filepath.Join(root, "src"))
	add(t, configuration, root, root, "-A")
	if after := snapshot(t, filepath.Join(root, "src")); after != before {
		t.Fatalf("working tree changed:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// interrupt runs an add whose final index replacement fails for the public
// repository, which leaves exactly the state a crash between two index
// replacements produces.
func interrupt(t *testing.T, configuration *config.Config, root string) {
	t.Helper()
	blocked := repository.Directory(root, "public")
	if err := os.Chmod(blocked, 0o500); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	err := stage.Add(configuration, root, root, []string{"-A"}, &output)
	if chmodErr := os.Chmod(blocked, 0o700); chmodErr != nil {
		t.Fatal(chmodErr)
	}
	if err == nil || !strings.Contains(err.Error(), "REC001") {
		t.Fatalf("error = %v, want an incomplete operation", err)
	}
}

func requireUnprivileged(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
}

func interruptedProject(t *testing.T) (*config.Config, string) {
	t.Helper()
	requireUnprivileged(t)
	configuration, root := project(t, map[string]string{
		"README.md":        "readme\n",
		"src/a.go":         "a\n",
		"secrets/notes.md": "notes\n",
	})
	interrupt(t, configuration, root)
	// The first repository was replaced, the second one was not.
	equal(t, tracked(t, root, "private"), []string{"secrets/notes.md"})
	equal(t, tracked(t, root, "public"), nil)
	return configuration, root
}

func TestInterruptedStagingIsDetected(t *testing.T) {
	configuration, root := interruptedProject(t)

	_, err := status.Collect(configuration, root)
	if err == nil || !strings.HasPrefix(err.Error(), "REC001") {
		t.Fatalf("status error = %v, want REC001", err)
	}
	// Every mutating command stops as well.
	var output bytes.Buffer
	if err := stage.Add(configuration, root, root, []string{"-A"}, &output); err == nil || !strings.HasPrefix(err.Error(), "REC001") {
		t.Fatalf("add error = %v, want REC001", err)
	}
}

func TestRecoverCompletesInterruptedStaging(t *testing.T) {
	configuration, root := interruptedProject(t)

	var output bytes.Buffer
	if err := stage.Recover(configuration, root, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "gitone add recovered.") {
		t.Fatalf("output = %q", output.String())
	}
	equal(t, tracked(t, root, "private"), []string{"secrets/notes.md"})
	equal(t, tracked(t, root, "public"), []string{".gitignore", ".gitone.yml", "README.md", "src/a.go"})

	result, err := status.Collect(configuration, root)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Issues) != 0 {
		t.Fatalf("issues = %+v", result.Issues)
	}
}

func TestAbortRestoresInterruptedStaging(t *testing.T) {
	configuration, root := interruptedProject(t)

	var output bytes.Buffer
	if err := stage.Abort(configuration, root, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "gitone add aborted.") {
		t.Fatalf("output = %q", output.String())
	}
	equal(t, tracked(t, root, "private"), nil)
	equal(t, tracked(t, root, "public"), nil)

	if _, err := status.Collect(configuration, root); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryRefusesStaleState(t *testing.T) {
	for _, name := range []string{"recover", "abort"} {
		t.Run(name, func(t *testing.T) {
			configuration, root := interruptedProject(t)
			// An external change to the already replaced index makes the saved
			// state stale.
			run(t, root, "private", "rm", "--cached", "-q", "secrets/notes.md")

			var output bytes.Buffer
			resolve := stage.Recover
			if name == "abort" {
				resolve = stage.Abort
			}
			err := resolve(configuration, root, &output)
			if err == nil || !strings.HasPrefix(err.Error(), "REC001") {
				t.Fatalf("error = %v, want REC001", err)
			}
			for _, want := range []string{`repository "private" changed outside GitOne`, "resolve it manually", "recovery"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error is missing %q: %v", want, err)
				}
			}
			// The state survives so the operation stays resolvable.
			if _, statErr := os.Stat(filepath.Join(root, ".gitone", "recovery", "state.json")); statErr != nil {
				t.Fatal(statErr)
			}
			// The untouched repository keeps its original index.
			equal(t, tracked(t, root, "public"), nil)
		})
	}
}

func TestRecoveryWithoutStateReportsNothing(t *testing.T) {
	configuration, root := project(t, map[string]string{"README.md": "readme\n", "secrets/notes.md": "notes\n"})

	for _, resolve := range []func(*config.Config, string, io.Writer) error{stage.Recover, stage.Abort} {
		var output bytes.Buffer
		if err := resolve(configuration, root, &output); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(output.String(), "No interrupted GitOne operation found.") {
			t.Fatalf("output = %q", output.String())
		}
	}
}

func TestRecoveryDiscardsPreparationWithoutRecordedState(t *testing.T) {
	configuration, root := project(t, map[string]string{"README.md": "readme\n", "secrets/notes.md": "notes\n"})
	// Preparation files without state.json predate the first index
	// replacement, so nothing was changed and they are simply discarded.
	recovery := filepath.Join(root, ".gitone", "recovery")
	if err := os.MkdirAll(recovery, 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, recovery, "public.next", "leftover")

	var output bytes.Buffer
	if err := stage.Recover(configuration, root, &output); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(recovery); !os.IsNotExist(err) {
		t.Fatalf("recovery state = %v, want none", err)
	}
}

func TestAddKeepsPathsWithSpacesAndSpecialCharactersApart(t *testing.T) {
	configuration, root := project(t, map[string]string{
		"README.md":            "readme\n",
		"src/with space.go":    "spaced\n",
		"src/*star.go":         "star\n",
		"secrets/my notes.md":  "notes\n",
		"secrets/:magic.md":    "magic\n",
		"secrets/deep/deep.md": "deep\n",
	})

	add(t, configuration, root, filepath.Join(root, "secrets"), ".")
	equal(t, tracked(t, root, "private"), []string{"secrets/:magic.md", "secrets/deep/deep.md", "secrets/my notes.md"})
	equal(t, tracked(t, root, "public"), nil)

	add(t, configuration, root, root, "-A")
	equal(t, tracked(t, root, "public"), []string{".gitignore", ".gitone.yml", "README.md", "src/*star.go", "src/with space.go"})
}

func TestAddRefusesAPreparedIndexThatWouldBreakALink(t *testing.T) {
	configuration, root := project(t, map[string]string{"src/lib/a.go": "a\n"})
	symlink(t, root, "lib", "src/alias")
	add(t, configuration, root, root, "-A")
	before := tracked(t, root, "public")
	// Staging only the deletion would leave the alias pointing at a directory
	// the index no longer holds anything below.
	if err := os.Remove(filepath.Join(root, "src", "lib", "a.go")); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	err := stage.Add(configuration, root, root, []string{"src/lib/a.go"}, &output)
	if err == nil || !strings.Contains(err.Error(),
		`PATH003 src/alias in repository "public": symbolic link to lib: the link target src/lib does not exist`) {
		t.Fatalf("error = %v, want the refused prepared index", err)
	}
	equal(t, tracked(t, root, "public"), before)
}
