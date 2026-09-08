package doctor_test

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/devidevio/gitone/internal/config"
	"github.com/devidevio/gitone/internal/doctor"
	"github.com/devidevio/gitone/internal/git"
	"github.com/devidevio/gitone/internal/lock"
	"github.com/devidevio/gitone/internal/repository"
)

const local = `version: 1
default_branch: main
repositories:
  private:
    visibility: private
    paths: [tasks/**]
  public:
    visibility: public
    paths: [.gitignore, .gitone.yml, README.md]
`

// project writes the given files below a new project root. An initialized
// project additionally gets its managed repositories.
func project(t *testing.T, configuration string, files map[string]string, initialize bool) string {
	t.Helper()
	for _, name := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(name, "GitOne")
	}
	for _, name := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(name, "gitone@example.com")
	}

	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	write(t, root, ".gitone.yml", configuration)
	for name, contents := range files {
		write(t, root, name, contents)
	}
	if !initialize {
		return root
	}
	loaded, discovered, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.Init(loaded, discovered); err != nil {
		t.Fatal(err)
	}
	return root
}

func write(t *testing.T, root, name, contents string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

// report runs doctor and returns its output together with its result.
func report(t *testing.T, cwd string) (string, bool) {
	t.Helper()
	var output bytes.Buffer
	healthy := doctor.Report(cwd, &output)
	t.Log("\n" + output.String())
	return output.String(), healthy
}

// fingerprint hashes every path below root together with its contents, so a
// check that changed anything at all is detected.
func fingerprint(t *testing.T, root string) string {
	t.Helper()
	hash := sha256.New()
	err := filepath.WalkDir(root, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fmt.Fprintf(hash, "%s\x00%v\x00", name, entry.IsDir())
		if entry.Type().IsRegular() {
			contents, err := os.ReadFile(name)
			if err != nil {
				return err
			}
			hash.Write(contents)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%x", hash.Sum(nil))
}

func requireLines(t *testing.T, output string, wanted ...string) {
	t.Helper()
	for _, want := range wanted {
		if !strings.Contains(output, want) {
			t.Fatalf("output is missing %q:\n%s", want, output)
		}
	}
}

func TestHealthyLocalProjectPasses(t *testing.T) {
	root := project(t, local, map[string]string{"README.md": "readme\n", "tasks/plan.md": "plan\n"}, true)
	before := fingerprint(t, root)

	output, healthy := report(t, filepath.Join(root, "tasks"))
	if !healthy {
		t.Fatalf("doctor failed:\n%s", output)
	}
	requireLines(t, output,
		"GitOne Doctor",
		"Configuration       ✓",
		"Git                 ✓",
		"Repository private  ✓",
		"Repository public   ✓",
		"Working tree        ✓",
		"Unassigned files    ✓",
		"Ambiguous paths     ✓",
		"Repository state    ✓",
		"Reserved paths      ✓",
		"Remotes             ✓",
		`repository "public" has no remote and stays local`,
		"Locks               ✓",
		"Recovery            ✓",
		"Everything looks good.",
	)
	if after := fingerprint(t, root); after != before {
		t.Fatal("doctor changed the project")
	}
}

func TestInvalidConfigurationFailsAndSkipsProjectChecks(t *testing.T) {
	root := project(t, `version: 1
default_branch: main
repositories:
  public:
    visibility: public
    paths: [src/**, src/**]
`, nil, false)

	output, healthy := report(t, root)
	if healthy {
		t.Fatalf("doctor passed:\n%s", output)
	}
	requireLines(t, output, "Configuration  ✗", "CONFIG003 ", "Project        -", "Some checks failed.")
}

func TestMissingConfigurationIsReported(t *testing.T) {
	output, healthy := report(t, t.TempDir())
	if healthy {
		t.Fatalf("doctor passed:\n%s", output)
	}
	requireLines(t, output, "Configuration  ✗", "CONFIG001 ")
	if strings.Contains(output, "Locks") {
		t.Fatalf("locks were checked without a project root:\n%s", output)
	}
}

func TestUnsupportedGitVersionHasStableCode(t *testing.T) {
	directory := t.TempDir()
	fake := filepath.Join(directory, "git")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho 'git version 2.27.0'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))

	output, healthy := report(t, t.TempDir())
	if healthy {
		t.Fatalf("doctor passed:\n%s", output)
	}
	requireLines(t, output, "Git            ✗", "GIT001 git version 2.27.0 is too old")
}

// Independent problems are reported together: an uninitialized repository, an
// unassigned file, an ambiguous path and a symlink all appear in one run.
func TestIndependentFailuresAreReportedTogether(t *testing.T) {
	root := project(t, local, map[string]string{"README.md": "readme\n", "tasks/plan.md": "plan\n"}, true)
	write(t, root, "stray.txt", "stray\n")
	if err := os.Symlink("README.md", filepath.Join(root, "tasks", "link.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(repository.Directory(root, "private")); err != nil {
		t.Fatal(err)
	}

	output, healthy := report(t, root)
	if healthy {
		t.Fatalf("doctor passed:\n%s", output)
	}
	requireLines(t, output,
		"Repository private  ✗",
		`REPO001 repository "private" is not initialized: run gitone reconfigure to create it from the configuration`,
		"Repository public   ✓",
		"Working tree        ✗",
		"PATH003 tasks/link.md: symbolic link to README.md: the link target tasks/README.md does not exist",
		"Unassigned files    ✗",
		"PATH001 stray.txt: path is not assigned to any repository",
	)
}

func TestBranchDisagreementIsReported(t *testing.T) {
	configuration := strings.Replace(local, "  public:\n", "  public:\n    remote: expected\n", 1)
	root := project(t, configuration, map[string]string{"README.md": "readme\n", "tasks/plan.md": "plan\n"}, true)
	if _, err := git.Run(root, "--git-dir="+repository.Directory(root, "public"), "symbolic-ref", "HEAD", "refs/heads/other"); err != nil {
		t.Fatal(err)
	}
	if _, err := git.Run(root, "--git-dir="+repository.Directory(root, "public"), "remote", "set-url", "origin", "actual"); err != nil {
		t.Fatal(err)
	}

	output, healthy := report(t, root)
	if healthy {
		t.Fatalf("doctor passed:\n%s", output)
	}
	requireLines(t, output,
		"Repository public   ✗",
		`remote "origin" is actual, not the configured expected: run gitone reconfigure`,
		"Repository state    ✗",
		"the repositories are not on the same branch: private on main, public on other",
	)
}

func TestReservedPathsInIndexAndHistoryAreReported(t *testing.T) {
	root := project(t, local, map[string]string{"README.md": "readme\n", "tasks/plan.md": "plan\n"}, true)
	write(t, root, ".gitone.local.yml", "version: 1\n")
	// Native Git can record what GitOne itself refuses to stage.
	gitDirectory := repository.Directory(root, "public")
	for _, arguments := range [][]string{
		{"add", "--force", ".gitone.local.yml"},
		{"commit", "--quiet", "-m", "contaminated"},
	} {
		if _, err := git.Run(root, append([]string{"--git-dir=" + gitDirectory, "--work-tree=" + root}, arguments...)...); err != nil {
			t.Fatal(err)
		}
	}

	output, healthy := report(t, root)
	if healthy {
		t.Fatalf("doctor passed:\n%s", output)
	}
	requireLines(t, output,
		"Reserved paths      ✗",
		`PATH003 repository "public" index contains .gitone.local.yml`,
		"PATH003 repository \"public\" commit",
	)
}

func TestRemotesAreCheckedWithoutModifyingThem(t *testing.T) {
	origins, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	reachable := filepath.Join(origins, "public.git")
	if _, err := git.Run(origins, "init", "--bare", "--quiet", "--initial-branch=main", reachable); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(origins, "gone.git")

	root := project(t, fmt.Sprintf(`version: 1
default_branch: main
repositories:
  private:
    remote: %s
    visibility: private
    paths: [tasks/**]
  public:
    remote: %s
    visibility: public
    paths: [.gitignore, .gitone.yml, README.md]
`, missing, reachable), map[string]string{"README.md": "readme\n", "tasks/plan.md": "plan\n"}, true)
	before := fingerprint(t, reachable)

	output, healthy := report(t, root)
	if healthy {
		t.Fatalf("doctor passed:\n%s", output)
	}
	requireLines(t, output,
		"Remotes             ✗",
		`repository "private" remote "origin" is unavailable`,
		`repository "public" remote "origin" is reachable`,
	)
	if after := fingerprint(t, reachable); after != before {
		t.Fatal("doctor changed the remote")
	}
}

func TestActiveLockIsReportedWithoutChangingIt(t *testing.T) {
	root := project(t, local, map[string]string{"README.md": "readme\n", "tasks/plan.md": "plan\n"}, true)
	release, err := lock.Acquire(root, "gitone commit")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	before := fingerprint(t, root)

	output, healthy := report(t, root)
	if !healthy {
		t.Fatalf("doctor failed:\n%s", output)
	}
	requireLines(t, output, "Locks               ✓", "a GitOne operation is running: gitone commit, PID ", "left the lock untouched")
	if after := fingerprint(t, root); after != before {
		t.Fatal("doctor changed the project while a mutation held the lock")
	}
	if _, err := os.Stat(filepath.Join(root, ".gitone", lock.File)); err != nil {
		t.Fatalf("the lock was removed: %v", err)
	}
}

func TestRecoveryStateIsReported(t *testing.T) {
	tests := map[string]struct{ recorded, wanted string }{
		"commit": {`{"version":1,"command":"gitone commit"}`, "gitone commit was interrupted: run gitone recover to finish it or gitone abort to undo it"},
		"push":   {`{"version":1,"command":"gitone push"}`, "gitone push was interrupted: run gitone recover to read back what the remotes really have"},
		"broken": {"{", "without a readable command"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			root := project(t, local, map[string]string{"README.md": "readme\n", "tasks/plan.md": "plan\n"}, true)
			if err := os.MkdirAll(lock.RecoveryPath(root), 0o700); err != nil {
				t.Fatal(err)
			}
			write(t, filepath.Join(root, ".gitone", "recovery"), lock.StateFile, test.recorded)
			before := fingerprint(t, root)

			output, healthy := report(t, root)
			if healthy {
				t.Fatalf("doctor passed:\n%s", output)
			}
			requireLines(t, output, "Recovery            ✗", "REC001 ", test.wanted)
			if after := fingerprint(t, root); after != before {
				t.Fatal("doctor changed the recovery state")
			}
		})
	}
}
