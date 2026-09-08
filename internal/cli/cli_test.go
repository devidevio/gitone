package cli_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/devidevio/gitone/internal/agents"
	"github.com/devidevio/gitone/internal/cli"
	"github.com/devidevio/gitone/internal/git"
)

func fakeCode(t *testing.T, body string) {
	t.Helper()
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "code"), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestRepoListReportsRetiredRepositories(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".gitone.yml"), []byte(`version: 1
default_branch: main
repositories:
  alpha:
    visibility: private
    paths: [src/**]
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".gitone", "retired", "20200102T030405Z-scratch"), 0o700); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if exitCode := cli.Run([]string{"repo", "list"}, root, nil, &stdout, &stderr); exitCode != 0 {
		t.Fatalf("exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	got := stdout.String()
	for _, want := range []string{"Retired repositories", "scratch",
		".gitone/retired/20200102T030405Z-scratch", "git --git-dir=<directory> log", "never removes"} {
		if !strings.Contains(got, want) {
			t.Fatalf("repo list output = %q, want %q", got, want)
		}
	}
}

func TestRepoValidateAndList(t *testing.T) {
	root := t.TempDir()
	configuration := `version: 1
default_branch: main
repositories:
  zebra:
    visibility: private
    paths: [tasks/**]
  alpha:
    remote: git@example.com:alpha.git
    visibility: public
    paths: [src/**]
`
	if err := os.WriteFile(filepath.Join(root, ".gitone.yml"), []byte(configuration), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if exitCode := cli.Run([]string{"repo", "validate"}, root, nil, &stdout, &stderr); exitCode != 0 {
		t.Fatalf("exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	if got := stdout.String(); !strings.Contains(got, "2 repositories") || !strings.Contains(got, "Configuration is valid.") {
		t.Fatalf("validate output = %q", got)
	}

	stdout.Reset()
	stderr.Reset()
	if exitCode := cli.Run([]string{"repo", "list"}, root, nil, &stdout, &stderr); exitCode != 0 {
		t.Fatalf("exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	got := stdout.String()
	if strings.Index(got, "alpha") > strings.Index(got, "zebra") {
		t.Fatalf("repositories are not sorted: %q", got)
	}
	// The REMOTE column names the configured remote, or - without one.
	rows := map[string][]string{}
	for _, line := range strings.Split(got, "\n") {
		if fields := strings.Fields(line); len(fields) == 5 {
			rows[fields[0]] = fields
		}
	}
	for repository, remote := range map[string]string{"alpha": "origin", "zebra": "-"} {
		if row := rows[repository]; len(row) != 5 || row[4] != remote {
			t.Fatalf("%s row = %v, want the remote %q:\n%s", repository, row, remote, got)
		}
	}
}

func TestRunRejectsUnsupportedInput(t *testing.T) {
	tests := [][]string{
		{"repo validate"},
		{"repo", "list", "--json"},
		{"repo", "unknown"},
		{"push", "--force"},
		{"push", "-f"},
		{"push", "origin", "main"},
		{"push", "public", "--dry-run"},
		{"push", "--delete", "main"},
		{"diff", "--cached"},
		{"diff", "-p"},
		{"diff", "--stat"},
		{"diff", ""},
		{"diff", "HEAD", "--", "README.md"},
		{"diff", "public", "private"},
		{"diff", "--staged", "--staged"},
		{"fetch", "--all"},
		{"fetch", "--prune"},
		{"fetch", ""},
		{"fetch", "-p"},
		{"fetch", "origin", "main"},
		{"fetch", "public", "private"},
		{"pull", "--rebase"},
		{"pull", "--ff-only"},
		{"pull", ""},
		{"pull", "-r"},
		{"pull", "origin", "main"},
		{"pull", "public", "private"},
		{"pull", "--yes"},
		{"pull", "--accept-config-change", "--accept-config-change"},
		{"clone"},
		{"clone", ""},
		{"clone", "--depth", "1", "url"},
		{"clone", "url", "--branch", "main"},
		{"clone", "url", "directory", "extra"},
		{"doctor", "--json"},
		{"doctor", "public"},
		{"migrate", "-y"},
		{"migrate", "all"},
		{"migrate", "--yes", "--yes"},
		{"setup", "--yes"},
		{"setup", "--force"},
		{"setup", "website"},
		{"vscode", "install", "--vsix"},
		{"vscode", "install", "--vsix", "--force"},
		{"vscode", "install", "--force", "--force"},
		{"vscode", "install", "--unknown"},
		{"agents", "--json"},
		{"agents", "check"},
		{"agents", "update", "--yes"},
		{"agents", "update", "--json"},
		{"agents", "update", "extra"},
		{"agent"},
		{"backup", "unknown"},
		{"backup", "list", "--json"},
		{"backup", "list", "--backup", "20260826T133234Z-1"},
		{"backup", "git"},
		{"backup", "git", "log"},
		{"backup", "git", "--"},
		{"backup", "git", "--", "reset", "--hard"},
		{"backup", "git", "--", "branch", "-d", "main"},
		{"backup", "git", "--", "gc"},
		{"backup", "git", "--", "config", "user.name", "x"},
		{"backup", "git", "--no-pager", "--", "log"},
		{"backup", "git", "--git-dir=/other", "--", "log"},
		{"backup", "git", "--backup", "--", "log"},
		{"backup", "git", "--backup"},
		{"backup", "git", "--backup", "a", "--backup", "b", "--", "log"},
		{"backup", "restore", "--force"},
		{"backup", "restore", "--yes", "--yes"},
		{"backup", "restore", "20260826T133234Z-1"},
		{"backup", "restore", "--backup"},
		{"reconfigure", "website"},
		{"reconfigure", "all"},
		{"reconfigure", "--yes"},
		{"reconfigure", "--accept-repository-change", "--accept-repository-change"},
		{"reconfigure", "--pull", "--pull"},
		{"reconfigure", "--switch"},
		{"reconfigure", "--switch", "--accept-repository-change"},
		{"reconfigure", "--pull", "--switch", "feature"},
		{"reconfigure", "--switch", "feature", "--pull"},
		{"reconfigure", "--switch", "feature", "--switch", "other"},
		{"reconfigure", "--pull", "website"},
		{"reconfigure", "--rename"},
		{"reconfigure", "--rename", "notes"},
		{"reconfigure", "--rename", "notes="},
		{"reconfigure", "--rename", "=journal"},
		{"reconfigure", "--rename", "notes=journal=extra"},
		{"reconfigure", "--rename", "Notes=journal"},
		{"reconfigure", "--rename=notes=journal"},
	}
	for _, args := range tests {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if exitCode := cli.Run(args, t.TempDir(), nil, &stdout, &stderr); exitCode == 0 {
				t.Fatalf("exit code = 0, stderr = %q", stderr.String())
			}
			if !strings.HasPrefix(stderr.String(), "CLI001 unsupported command") {
				t.Fatalf("stderr = %q", stderr.String())
			}
			if args[0] == "reconfigure" && !strings.Contains(stderr.String(),
				"accepted form: gitone reconfigure [--pull | --switch <branch>] [--accept-repository-change]") {
				t.Fatalf("stderr = %q, want the accepted reconfigure form", stderr.String())
			}
		})
	}
}

// gitone setup is interactive only, so a piped standard input is refused
// before any configuration is read or written.
// TestReconfigureAcceptsEverySource proves that each accepted reconfigure
// form reaches the command instead of being rejected as CLI001. Without a
// project the command then fails on the missing configuration, which is
// exactly how far the parser is responsible.
func TestReconfigureAcceptsEverySource(t *testing.T) {
	for _, args := range [][]string{
		{"reconfigure"},
		{"reconfigure", "--pull"},
		{"reconfigure", "--switch", "feature/auth"},
		{"reconfigure", "--pull", "--accept-repository-change", "--accept-config-change", "--accept-new-paths"},
		{"reconfigure", "--switch", "feature/auth", "--accept-config-change", "--rename", "notes=journal"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if exitCode := cli.Run(args, t.TempDir(), nil, &stdout, &stderr); exitCode == 0 {
				t.Fatalf("exit code = 0 without a project, stderr = %q", stderr.String())
			}
			if !strings.HasPrefix(stderr.String(), "CONFIG001 ") {
				t.Fatalf("stderr = %q, want the accepted form to reach the command", stderr.String())
			}
		})
	}
}

func TestSetupRequiresATerminal(t *testing.T) {
	root := t.TempDir()
	var stdout, stderr bytes.Buffer
	if exitCode := cli.Run([]string{"setup"}, root, strings.NewReader("y\n"), &stdout, &stderr); exitCode == 0 {
		t.Fatalf("exit code = 0, stdout = %q", stdout.String())
	}
	if !strings.HasPrefix(stderr.String(), "SETUP001 ") {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("the refused setup changed the project: %v %v", entries, err)
	}
}

func TestRunPrintsStableConfigurationError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if exitCode := cli.Run([]string{"repo", "validate"}, t.TempDir(), nil, &stdout, &stderr); exitCode == 0 {
		t.Fatal("exit code = 0")
	}
	if !strings.HasPrefix(stderr.String(), "CONFIG001 ") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestInit(t *testing.T) {
	root := t.TempDir()
	configuration := `version: 1
default_branch: main
repositories:
  public:
    visibility: public
    paths: [.gitignore, .gitone.yml, README.md]
`
	if err := os.WriteFile(filepath.Join(root, ".gitone.yml"), []byte(configuration), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("readme"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if exitCode := cli.Run([]string{"init"}, root, nil, &stdout, &stderr); exitCode != 0 {
		t.Fatalf("exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	if got := stdout.String(); !strings.Contains(got, "GitOne initialized") || !strings.Contains(got, "public (main)") {
		t.Fatalf("init output = %q", got)
	}
	if _, err := os.Stat(filepath.Join(root, ".gitone", "repositories", "public", "HEAD")); err != nil {
		t.Fatal(err)
	}
}

func TestInitReportsUnassignedPath(t *testing.T) {
	root := t.TempDir()
	configuration := `version: 1
default_branch: main
repositories:
  public:
    visibility: public
    paths: [.gitignore, .gitone.yml]
`
	if err := os.WriteFile(filepath.Join(root, ".gitone.yml"), []byte(configuration), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("readme"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if exitCode := cli.Run([]string{"init"}, root, nil, &stdout, &stderr); exitCode != 1 {
		t.Fatalf("exit code = %d, want 1", exitCode)
	}
	if got := stderr.String(); !strings.Contains(got, "PATH001 README.md") {
		t.Fatalf("stderr = %q, want PATH001 for README.md", got)
	}
}

const statusProject = `version: 1
default_branch: main
repositories:
  private:
    visibility: private
    paths: [tasks/**]
  public:
    visibility: public
    paths: [.gitignore, .gitone.yml, README.md]
`

func statusRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for name, contents := range map[string]string{
		".gitone.yml":   statusProject,
		"README.md":     "readme\n",
		"tasks/plan.md": "plan\n",
	} {
		full := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var stdout, stderr bytes.Buffer
	if exitCode := cli.Run([]string{"init"}, root, nil, &stdout, &stderr); exitCode != 0 {
		t.Fatalf("init exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	return root
}

func TestStatusReportsWholeProjectFromDescendant(t *testing.T) {
	root := statusRoot(t)

	for _, args := range [][]string{{"status"}, {"status", "--porcelain"}, {"status", "--json"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			// Called from a subdirectory, status still covers the project.
			if exitCode := cli.Run(args, filepath.Join(root, "tasks"), nil, &stdout, &stderr); exitCode != 0 {
				t.Fatalf("exit code = %d, stderr = %q", exitCode, stderr.String())
			}
			got := stdout.String()
			for _, want := range []string{"private", "public", "README.md", "tasks/plan.md"} {
				if !strings.Contains(got, want) {
					t.Fatalf("output is missing %q:\n%s", want, got)
				}
			}
		})
	}
}

func TestStatusHumanOutputSeparatesSections(t *testing.T) {
	root := statusRoot(t)

	var stdout, stderr bytes.Buffer
	if exitCode := cli.Run([]string{"status"}, root, nil, &stdout, &stderr); exitCode != 0 {
		t.Fatalf("exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	got := stdout.String()
	if !strings.Contains(got, "PRIVATE\n") || !strings.Contains(got, "PUBLIC\n") || !strings.Contains(got, "BRANCHES\n") {
		t.Fatalf("output = %q", got)
	}
	if strings.Index(got, "PRIVATE") > strings.Index(got, "PUBLIC") {
		t.Fatalf("repositories are not sorted:\n%s", got)
	}
	if !strings.Contains(got, "  public: main (no remote-tracking branch)") {
		t.Fatalf("branch section = %q", got)
	}
}

func TestStatusExitsNonZeroForUnsafeState(t *testing.T) {
	root := statusRoot(t)
	if err := os.WriteFile(filepath.Join(root, "stray.txt"), []byte("stray\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if exitCode := cli.Run([]string{"status", "--porcelain"}, root, nil, &stdout, &stderr); exitCode != 1 {
		t.Fatalf("exit code = %d, want 1", exitCode)
	}
	if !strings.Contains(stdout.String(), "issue\tPATH001\tstray.txt\t") {
		t.Fatalf("output = %q", stdout.String())
	}
}

func TestStatusRejectsUnknownFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if exitCode := cli.Run([]string{"status", "--short"}, t.TempDir(), nil, &stdout, &stderr); exitCode == 0 {
		t.Fatal("exit code = 0")
	}
	if !strings.HasPrefix(stderr.String(), "CLI001 unsupported command") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestAddStagesThroughTheCLI(t *testing.T) {
	root := statusRoot(t)

	var stdout, stderr bytes.Buffer
	// Called from a subdirectory, "." covers that subtree only.
	if exitCode := cli.Run([]string{"add", "."}, filepath.Join(root, "tasks"), nil, &stdout, &stderr); exitCode != 0 {
		t.Fatalf("exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	if got := stdout.String(); !strings.Contains(got, "private:") || !strings.Contains(got, "tasks/plan.md") || strings.Contains(got, "README.md") {
		t.Fatalf("add output = %q", got)
	}

	stdout.Reset()
	if exitCode := cli.Run([]string{"status", "--porcelain"}, root, nil, &stdout, &stderr); exitCode != 0 {
		t.Fatalf("exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "change\tprivate\tstaged\tadded\ttasks/plan.md") {
		t.Fatalf("status output = %q", stdout.String())
	}

	for _, command := range []string{"recover", "abort"} {
		stdout.Reset()
		if exitCode := cli.Run([]string{command}, root, nil, &stdout, &stderr); exitCode != 0 {
			t.Fatalf("%s exit code = %d, stderr = %q", command, exitCode, stderr.String())
		}
		if !strings.Contains(stdout.String(), "No interrupted GitOne operation found.") {
			t.Fatalf("%s output = %q", command, stdout.String())
		}
	}
}

func TestUnstageThroughTheCLI(t *testing.T) {
	root := statusRoot(t)
	var stdout, stderr bytes.Buffer
	if exitCode := cli.Run([]string{"add", "-A"}, root, nil, &stdout, &stderr); exitCode != 0 {
		t.Fatalf("add exit code = %d, stderr = %q", exitCode, stderr.String())
	}

	stdout.Reset()
	if exitCode := cli.Run([]string{"unstage", "tasks"}, root, nil, &stdout, &stderr); exitCode != 0 {
		t.Fatalf("unstage exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	if got := stdout.String(); !strings.Contains(got, "tasks/plan.md") || strings.Contains(got, "README.md") {
		t.Fatalf("unstage output = %q", got)
	}

	stdout.Reset()
	if exitCode := cli.Run([]string{"status", "--porcelain"}, root, nil, &stdout, &stderr); exitCode != 0 {
		t.Fatalf("status exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	if got := stdout.String(); strings.Contains(got, "change\tprivate\tstaged") || !strings.Contains(got, "change\tpublic\tstaged") {
		t.Fatalf("status output = %q", got)
	}
}

func TestAddRejectsUnsupportedInput(t *testing.T) {
	root := statusRoot(t)
	tests := [][]string{
		{"add"},
		{"add", "-p"},
		{"add", "--all"},
		{"add", "-A", "-u"},
		{"add", "-A", "."},
		{"add", "-f", "README.md"},
		{"unstage"},
		{"unstage", "-u"},
		{"unstage", "-A", "."},
		{"restore"},
		{"restore", "--staged"},
		{"restore", "-A"},
		{"restore", "-p", "README.md"},
		{"restore", "--source", "index", "README.md"},
		{"restore", "--source", "HEAD"},
		{"restore", "--source=HEAD~1", "README.md"},
		{"restore", "README.md", "--staged"},
		{"restore", "--staged", "--staged", "README.md"},
		{"restore", "--worktree", "--worktree", "README.md"},
		{"recover", "--force"},
		{"abort", "now"},
	}
	for _, args := range tests {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if exitCode := cli.Run(args, root, nil, &stdout, &stderr); exitCode == 0 {
				t.Fatalf("exit code = 0, stdout = %q", stdout.String())
			}
			if !strings.HasPrefix(stderr.String(), "CLI001 unsupported command") {
				t.Fatalf("stderr = %q", stderr.String())
			}
		})
	}
}

func TestAddReportsUnsafePathArgument(t *testing.T) {
	root := statusRoot(t)

	var stdout, stderr bytes.Buffer
	if exitCode := cli.Run([]string{"add", filepath.Join(root, "README.md")}, root, nil, &stdout, &stderr); exitCode != 1 {
		t.Fatalf("exit code = %d, want 1", exitCode)
	}
	if !strings.HasPrefix(stderr.String(), "PATH003 ") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestCommitThroughTheCLI(t *testing.T) {
	root := statusRoot(t)
	for _, name := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(name, "GitOne")
	}
	for _, name := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(name, "gitone@example.com")
	}

	var stdout, stderr bytes.Buffer
	if exitCode := cli.Run([]string{"add", "-A"}, root, nil, &stdout, &stderr); exitCode != 0 {
		t.Fatalf("add exit code = %d, stderr = %q", exitCode, stderr.String())
	}

	stdout.Reset()
	if exitCode := cli.Run([]string{"commit", "-m", "Implement authentication"}, root, nil, &stdout, &stderr); exitCode != 0 {
		t.Fatalf("commit exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	if got := stdout.String(); !strings.Contains(got, "private:\n    commit ") || !strings.Contains(got, "GitOne-Group: ") {
		t.Fatalf("commit output = %q", got)
	}

	stdout.Reset()
	if exitCode := cli.Run([]string{"commit", "-m", "Again"}, root, nil, &stdout, &stderr); exitCode != 0 {
		t.Fatalf("commit exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Nothing to commit.") {
		t.Fatalf("commit output = %q", stdout.String())
	}
}

func TestSelectiveCommitThroughTheCLI(t *testing.T) {
	for _, order := range [][]string{
		{"commit", "README.md", "-m", "Selected"},
		{"commit", "-m", "Selected", "README.md"},
	} {
		t.Run(strings.Join(order, " "), func(t *testing.T) {
			root := statusRoot(t)
			for _, name := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
				t.Setenv(name, "GitOne")
			}
			for _, name := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
				t.Setenv(name, "gitone@example.com")
			}
			var stdout, stderr bytes.Buffer
			if exitCode := cli.Run([]string{"add", "-A"}, root, nil, &stdout, &stderr); exitCode != 0 {
				t.Fatalf("add exit code = %d, stderr = %q", exitCode, stderr.String())
			}

			stdout.Reset()
			// Only README.md is committed, so the private repository keeps its
			// staged change and gets no commit and no group trailer.
			if exitCode := cli.Run(order, root, nil, &stdout, &stderr); exitCode != 0 {
				t.Fatalf("commit exit code = %d, stderr = %q", exitCode, stderr.String())
			}
			got := stdout.String()
			if !strings.Contains(got, "public:\n    commit ") || strings.Contains(got, "private:") || strings.Contains(got, "GitOne-Group") {
				t.Fatalf("commit output = %q", got)
			}
			stdout.Reset()
			if exitCode := cli.Run([]string{"status", "--porcelain"}, root, nil, &stdout, &stderr); exitCode != 0 {
				t.Fatalf("status exit code = %d, stderr = %q", exitCode, stderr.String())
			}
			if !strings.Contains(stdout.String(), "tasks/plan.md") || strings.Contains(stdout.String(), "README.md") {
				t.Fatalf("status = %q, want only the unselected staged change", stdout.String())
			}
		})
	}
}

func TestCommitReadsTheCompleteMessageFromStdin(t *testing.T) {
	root := statusRoot(t)
	for _, name := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(name, "GitOne")
	}
	for _, name := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(name, "gitone@example.com")
	}
	var stdout, stderr bytes.Buffer
	if exitCode := cli.Run([]string{"add", "README.md"}, root, nil, &stdout, &stderr); exitCode != 0 {
		t.Fatalf("add exit code = %d, stderr = %q", exitCode, stderr.String())
	}

	message := "Subject\n\nBody from standard input\n"
	stdout.Reset()
	if exitCode := cli.Run([]string{"commit", "-F", "-"}, root, strings.NewReader(message), &stdout, &stderr); exitCode != 0 {
		t.Fatalf("commit exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	output, err := git.Run(root, "--git-dir="+filepath.Join(root, ".gitone", "repositories", "public"), "log", "-1", "--format=%B")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(output) != strings.TrimSpace(message) {
		t.Fatalf("message = %q, want %q", output, message)
	}
}

func TestShowReadsHeadAndIndexBytesAndDistinguishesMissingContent(t *testing.T) {
	root := statusRoot(t)
	for _, name := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(name, "GitOne")
	}
	for _, name := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(name, "gitone@example.com")
	}
	var stdout, stderr bytes.Buffer
	if exitCode := cli.Run([]string{"add", "-A"}, root, nil, &stdout, &stderr); exitCode != 0 {
		t.Fatalf("add exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	stdout.Reset()
	if exitCode := cli.Run([]string{"commit", "-m", "initial"}, root, nil, &stdout, &stderr); exitCode != 0 {
		t.Fatalf("commit exit code = %d, stderr = %q", exitCode, stderr.String())
	}

	want := []byte{'n', 'e', 'w', 0, 'b', 'y', 't', 'e', 's', '\n'}
	if err := os.WriteFile(filepath.Join(root, "README.md"), want, 0o644); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if exitCode := cli.Run([]string{"add", "README.md"}, root, nil, &stdout, &stderr); exitCode != 0 {
		t.Fatalf("add exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	// An unrelated unassigned path holds a project issue open across the reads
	// below, which must succeed anyway. Its own refusal is asserted last, so a
	// configuration that stopped producing the issue fails this test.
	if err := os.WriteFile(filepath.Join(root, "unassigned.txt"), []byte("issue\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	stdout.Reset()
	if exitCode := cli.Run([]string{"show", "--repository", "public", "--source", "head", "--", "README.md"}, root, nil, &stdout, &stderr); exitCode != 0 {
		t.Fatalf("show head exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	if stdout.String() != "readme\n" {
		t.Fatalf("HEAD contents = %q", stdout.Bytes())
	}
	stdout.Reset()
	if exitCode := cli.Run([]string{"show", "--repository", "public", "--source", "index", "--", "README.md"}, root, nil, &stdout, &stderr); exitCode != 0 {
		t.Fatalf("show index exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	if !bytes.Equal(stdout.Bytes(), want) {
		t.Fatalf("index contents = %q, want %q", stdout.Bytes(), want)
	}

	if err := os.WriteFile(filepath.Join(root, "tasks", "new.md"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if exitCode := cli.Run([]string{"show", "--repository", "private", "--source", "head", "--", "tasks/new.md"}, root, nil, &stdout, &stderr); exitCode != 2 {
		t.Fatalf("missing exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.HasPrefix(stderr.String(), "SHOW001 ") {
		t.Fatalf("stderr = %q", stderr.String())
	}

	stderr.Reset()
	if exitCode := cli.Run([]string{"show", "--repository", "private", "--source", "index", "--", "README.md"}, root, nil, &stdout, &stderr); exitCode != 1 {
		t.Fatalf("wrong owner exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.HasPrefix(stderr.String(), "PATH003 ") {
		t.Fatalf("stderr = %q", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if exitCode := cli.Run([]string{"show", "--repository", "public", "--source", "index", "--", "unassigned.txt"}, root, nil, &stdout, &stderr); exitCode != 1 {
		t.Fatalf("unassigned exit code = %d, stdout = %q, stderr = %q", exitCode, stdout.String(), stderr.String())
	}
	if !strings.HasPrefix(stderr.String(), "PATH001 ") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestShowRefusesAPathTrackedByASecondRepository(t *testing.T) {
	root := statusRoot(t)
	var stdout, stderr bytes.Buffer
	if exitCode := cli.Run([]string{"add", "-A"}, root, nil, &stdout, &stderr); exitCode != 0 {
		t.Fatalf("add exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	// The configuration still assigns README.md to public, so the second index
	// alone takes its one clear owner away. Serving public's bytes here would
	// answer for a path the project can no longer attribute.
	private := filepath.Join(root, ".gitone", "repositories", "private")
	if _, err := git.Run(root, "--literal-pathspecs", "--git-dir="+private, "--work-tree="+root,
		"update-index", "--add", "--", "README.md"); err != nil {
		t.Fatal(err)
	}

	stdout.Reset()
	stderr.Reset()
	if exitCode := cli.Run([]string{"show", "--repository", "public", "--source", "index", "--", "README.md"}, root, nil, &stdout, &stderr); exitCode != 1 {
		t.Fatalf("exit code = %d, stdout = %q, stderr = %q", exitCode, stdout.String(), stderr.String())
	}
	if !strings.HasPrefix(stderr.String(), "PATH003 ") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunRejectsUnsupportedInputInAProject(t *testing.T) {
	root := statusRoot(t)
	tests := [][]string{
		{"commit"},
		{"commit", "-m"},
		{"commit", "message"},
		{"commit", "-am", "message"},
		{"commit", "--amend", "-m", "message"},
		{"commit", "-m", "one", "-m", "two"},
		{"commit", "-m", "message", "--no-verify"},
		{"commit", "-F", "message.txt"},
		{"commit", "README.md"},
		{"commit", "-m", "one", "README.md", "-m", "two"},
		{"commit", "-F", "-", "README.md", "-m", "message"},
		{"commit", "-m", "message", "--", "README.md"},
		{"commit", "README.md", "-p", "-m", "message"},
		{"log", "-n"},
		{"log", "-n", "0"},
		{"log", "-n", "-1"},
		{"log", "-n", "1001"},
		{"log", "-n", "two"},
		{"log", "-n", "5", "-n", "6"},
		{"log", "-5"},
		{"log", "--graph"},
		{"log", "--oneline"},
		{"log", "public", "src/site.css"},
		{"branch", "-d"},
		{"branch", "-d", "-d", "old"},
		{"branch", "-d", "old", "new"},
		{"branch", "-D", "old"},
		{"branch", "--delete"},
		{"branch", "-m", "old", "new"},
		{"branch", "-f", "feature"},
		{"branch", "--list"},
		{"branch", "--all"},
		{"branch", "feature", "main"},
		{"switch"},
		{"switch", ""},
		{"switch", "-c"},
		{"switch", "-c", "-c", "feature"},
		{"switch", "-C", "feature"},
		{"switch", "-c", "feature", "main"},
		{"switch", "--detach"},
		{"switch", "--force", "feature"},
		{"switch", "feature", "main"},
		{"switch", "--", "README.md"},
		{"switch", "--accept-config-change"},
		{"switch", "feature", "--accept-config-change", "--accept-config-change"},
		{"switch", "--accept-new-paths"},
		{"switch", "feature", "--accept-new-paths", "--accept-new-paths"},
		{"show", "--repository", "public", "--source", "worktree", "--", "README.md"},
		{"show", "--repository", "public", "--source", "head", "README.md"},
		{"vscode", "info"},
	}
	for _, args := range tests {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if exitCode := cli.Run(args, root, nil, &stdout, &stderr); exitCode == 0 {
				t.Fatalf("exit code = 0, stdout = %q", stdout.String())
			}
			if !strings.HasPrefix(stderr.String(), "CLI001 unsupported command") {
				t.Fatalf("stderr = %q", stderr.String())
			}
		})
	}
}

func TestDoctorThroughTheCLI(t *testing.T) {
	root := statusRoot(t)

	var stdout, stderr bytes.Buffer
	if exitCode := cli.Run([]string{"doctor"}, filepath.Join(root, "tasks"), nil, &stdout, &stderr); exitCode != 0 {
		t.Fatalf("exit code = %d, stdout = %q, stderr = %q", exitCode, stdout.String(), stderr.String())
	}
	if got := stdout.String(); !strings.Contains(got, "GitOne Doctor") || !strings.Contains(got, "Everything looks good.") {
		t.Fatalf("doctor output = %q", got)
	}

	if err := os.WriteFile(filepath.Join(root, "stray.txt"), []byte("stray\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if exitCode := cli.Run([]string{"doctor"}, root, nil, &stdout, &stderr); exitCode != 1 {
		t.Fatalf("exit code = %d, want 1", exitCode)
	}
	if !strings.Contains(stdout.String(), "PATH001 stray.txt") {
		t.Fatalf("doctor output = %q", stdout.String())
	}
}

// rootRepositoryProject is a configured project that is still one ordinary
// Git repository, ready to be migrated.
func rootRepositoryProject(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	configuration := `version: 1
default_branch: main
repositories:
  whole:
    visibility: private
    paths: [.gitignore, .gitone.yml, README.md]
`
	for name, contents := range map[string]string{".gitone.yml": configuration, "README.md": "# readme\n"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, arguments := range [][]string{
		{"init", "--quiet", "--initial-branch=main"},
		{"-c", "user.name=GitOne", "-c", "user.email=gitone@example.com", "add", "-A"},
		{"-c", "user.name=GitOne", "-c", "user.email=gitone@example.com", "commit", "--quiet", "-m", "initial"},
	} {
		if _, err := git.Run(root, arguments...); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestMigrateThroughTheCLI(t *testing.T) {
	root := rootRepositoryProject(t)

	var stdout, stderr bytes.Buffer
	if exitCode := cli.Run([]string{"migrate", "--yes"}, root, nil, &stdout, &stderr); exitCode != 0 {
		t.Fatalf("exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Migration complete.") {
		t.Fatalf("migrate output = %q", stdout.String())
	}
	if _, err := os.Lstat(filepath.Join(root, ".git")); !os.IsNotExist(err) {
		t.Fatalf("the root repository still exists: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	if exitCode := cli.Run([]string{"status"}, root, nil, &stdout, &stderr); exitCode != 0 {
		t.Fatalf("status exit code = %d, stderr = %q", exitCode, stderr.String())
	}
}

// A successful migration names the backup it kept and prints both recovery
// commands with the full ID, even while it is the only backup.
func TestMigrateReportsTheBackupRecoveryCommands(t *testing.T) {
	_, id, output := migratedThroughTheCLI(t)
	for _, want := range []string{
		"The original root Git repository was preserved at:",
		filepath.Join(".gitone", "migration-backup", id),
		"gitone backup git --backup " + id + " -- log --all",
		"gitone backup restore --backup " + id,
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("migration output is missing %q:\n%s", want, output)
		}
	}
}

// migratedThroughTheCLI migrates a project through the CLI and returns the
// project root, the full ID of the backup the migration kept and what the
// migration printed.
func migratedThroughTheCLI(t *testing.T) (string, string, string) {
	t.Helper()
	root := rootRepositoryProject(t)
	var stdout, stderr bytes.Buffer
	if exitCode := cli.Run([]string{"migrate", "--yes"}, root, nil, &stdout, &stderr); exitCode != 0 {
		t.Fatalf("exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	entries, err := os.ReadDir(filepath.Join(root, ".gitone", "migration-backup"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("backups = %v, %v", entries, err)
	}
	return root, entries[0].Name(), stdout.String()
}

// The backup group works on a project the ordinary commands refuse, before
// and after the root repository is back.
func TestBackupRecoveryThroughTheCLI(t *testing.T) {
	root, id, _ := migratedThroughTheCLI(t)

	var stdout, stderr bytes.Buffer
	if exitCode := cli.Run([]string{"backup", "list"}, root, nil, &stdout, &stderr); exitCode != 0 {
		t.Fatalf("list exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	if got := stdout.String(); !strings.Contains(got, id) || !strings.Contains(got, "valid") {
		t.Fatalf("list output = %q", got)
	}

	// Native Git output reaches standard output unchanged, without a GitOne
	// heading around it.
	stdout.Reset()
	stderr.Reset()
	if exitCode := cli.Run([]string{"backup", "git", "--", "log", "--oneline"}, root, nil, &stdout, &stderr); exitCode != 0 {
		t.Fatalf("git exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	if got := stdout.String(); !strings.HasSuffix(got, "initial\n") || strings.Contains(got, "WHOLE") {
		t.Fatalf("git output = %q", got)
	}

	// A failing inspection keeps the exit code of Git itself.
	stdout.Reset()
	stderr.Reset()
	exitCode := cli.Run([]string{"backup", "git", "--backup", id, "--", "show", strings.Repeat("0", 40)}, root, nil, &stdout, &stderr)
	if exitCode <= 1 {
		t.Fatalf("exit code = %d, want the native Git code", exitCode)
	}
	if !strings.Contains(stderr.String(), "GIT001 ") {
		t.Fatalf("stderr = %q", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if exitCode := cli.Run([]string{"backup", "restore", "--backup", id, "--yes"}, root, nil, &stdout, &stderr); exitCode != 0 {
		t.Fatalf("restore exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	if info, err := os.Lstat(filepath.Join(root, ".git")); err != nil || !info.IsDir() {
		t.Fatalf("the root repository was not restored: %v %v", info, err)
	}

	// The recovery state keeps the backup group usable and still refuses to
	// overwrite the root repository it just published.
	for _, args := range [][]string{{"backup", "list"}, {"backup", "git", "--", "log", "--oneline"}} {
		stdout.Reset()
		stderr.Reset()
		if exitCode := cli.Run(args, root, nil, &stdout, &stderr); exitCode != 0 {
			t.Fatalf("%v exit code = %d, stderr = %q", args, exitCode, stderr.String())
		}
	}
	stdout.Reset()
	stderr.Reset()
	if exitCode := cli.Run([]string{"backup", "restore", "--backup", id, "--yes"}, root, nil, &stdout, &stderr); exitCode != 1 {
		t.Fatalf("second restore exit code = %d, want 1", exitCode)
	}
	if !strings.HasPrefix(stderr.String(), "MIG001 ") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestVersionReportsABuildVersionWithoutAProject(t *testing.T) {
	var stdout, stderr bytes.Buffer
	// An empty directory: --version must not need a configuration.
	if exitCode := cli.Run([]string{"--version"}, t.TempDir(), nil, &stdout, &stderr); exitCode != 0 {
		t.Fatalf("exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	got := strings.TrimSuffix(stdout.String(), "\n")
	name, reported, found := strings.Cut(got, " ")
	if !found || name != "gitone" || reported == "" {
		t.Fatalf("version output = %q", stdout.String())
	}
}

func TestVSCodeInfoReportsTheProtocolWithoutAProject(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if exitCode := cli.Run([]string{"vscode", "info", "--json"}, t.TempDir(), nil, &stdout, &stderr); exitCode != 0 {
		t.Fatalf("exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	var info struct {
		Protocol      int    `json:"protocol"`
		GitOneVersion string `json:"gitone_version"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if info.Protocol != 1 || info.GitOneVersion == "" {
		t.Fatalf("info = %+v", info)
	}
}

func TestVSCodeInstallDoesNotRequireAProject(t *testing.T) {
	log := filepath.Join(t.TempDir(), "arguments")
	t.Setenv("GITONE_CODE_LOG", log)
	fakeCode(t, `printf '%s\n' "$@" > "$GITONE_CODE_LOG"`)

	root := t.TempDir()
	vsix := filepath.Join(root, "gitone.vsix")
	if err := os.WriteFile(vsix, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if exitCode := cli.Run([]string{"vscode", "install", "--force", "--vsix", "gitone.vsix"}, root, nil, &stdout, &stderr); exitCode != 0 {
		t.Fatalf("exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	arguments, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(arguments); got != "--install-extension\n"+vsix+"\n--force\n" {
		t.Fatalf("arguments = %q", got)
	}
	if stdout.String() != "GitOne VS Code extension installed.\n" {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

// readinessProject is a valid configuration whose repositories are not
// initialized yet.
const readinessProject = `version: 1
default_branch: main
repositories:
  site:
    visibility: public
    paths: [.gitignore, .gitone.yml, README.md]
  notes:
    visibility: private
    paths: [notes/**]
`

// readinessCommands are the commands the readiness check gates, in the
// shortest accepted form of each. status and diff appear twice because their
// output variants take their own path through the CLI.
var readinessCommands = [][]string{
	{"status"},
	{"status", "--json"},
	{"add", "-A"},
	{"unstage", "-A"},
	{"restore", "README.md"},
	{"commit", "-m", "message"},
	{"show", "--repository", "site", "--source", "head", "--", "README.md"},
	{"diff"},
	{"diff", "--staged"},
	{"log"},
	{"branch"},
	{"branch", "-d", "release"},
	{"switch", "main"},
	{"fetch"},
	{"pull"},
	{"push", "--yes"},
}

// readinessCommands is checked against every gated command, so a command
// added to the readiness check cannot stay untested.
func TestReadinessCommandsCoverEveryGatedCommand(t *testing.T) {
	covered := map[string]bool{}
	for _, args := range readinessCommands {
		covered[args[0]] = true
	}
	for _, command := range cli.RepositoryStateCommands {
		if !covered[command] {
			t.Errorf("readinessCommands has no form for %q", command)
		}
	}
}

func readinessRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".gitone.yml"), []byte(readinessProject), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

// A command that needs repositories reports the missing configuration before
// anything else and names the command that creates one.
func TestReadinessWithoutConfigurationSuggestsSetup(t *testing.T) {
	for _, args := range readinessCommands {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if exitCode := cli.Run(args, t.TempDir(), nil, &stdout, &stderr); exitCode == 0 {
				t.Fatalf("exit code = 0, stdout = %q", stdout.String())
			}
			got := stderr.String()
			if !strings.HasPrefix(got, "CONFIG001 ") || !strings.Contains(got, "gitone setup") {
				t.Fatalf("stderr = %q", got)
			}
		})
	}
}

// A configured but uninitialized project is told to initialize, and the
// refused command changes nothing.
func TestReadinessWithoutRepositoriesSuggestsInit(t *testing.T) {
	for _, args := range readinessCommands {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			root := readinessRoot(t)
			var stdout, stderr bytes.Buffer
			if exitCode := cli.Run(args, root, nil, &stdout, &stderr); exitCode == 0 {
				t.Fatalf("exit code = 0, stdout = %q", stdout.String())
			}
			got := stderr.String()
			if !strings.HasPrefix(got, "REPO001 ") || !strings.Contains(got, "gitone init") {
				t.Fatalf("stderr = %q", got)
			}
			if strings.Contains(got, "gitone migrate") {
				t.Fatalf("stderr suggests migrate without a root .git/: %q", got)
			}
			if _, err := os.Stat(filepath.Join(root, ".gitone")); !os.IsNotExist(err) {
				t.Fatalf("the refused command created .gitone: %v", err)
			}
		})
	}
}

// The same project with a root .git/ needs migration, not initialization.
func TestReadinessWithRootRepositorySuggestsMigrate(t *testing.T) {
	root := readinessRoot(t)
	if _, err := git.Run(root, "init", "--quiet", root); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if exitCode := cli.Run([]string{"status"}, root, nil, &stdout, &stderr); exitCode == 0 {
		t.Fatalf("exit code = 0, stdout = %q", stdout.String())
	}
	got := stderr.String()
	if !strings.HasPrefix(got, "REPO001 ") || !strings.Contains(got, "gitone migrate") {
		t.Fatalf("stderr = %q", got)
	}
	if strings.Contains(got, "gitone init") {
		t.Fatalf("stderr suggests init next to a root .git/: %q", got)
	}
}

// Unsupported root .git forms need diagnosis instead of migration or
// initialization, neither of which can handle them safely.
func TestReadinessWithUnsupportedRootGitSuggestsDoctor(t *testing.T) {
	tests := map[string]func(t *testing.T, root string){
		"file": func(t *testing.T, root string) {
			if err := os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: /elsewhere\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"symlink": func(t *testing.T, root string) {
			target := t.TempDir()
			if err := os.Symlink(target, filepath.Join(root, ".git")); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, create := range tests {
		t.Run(name, func(t *testing.T) {
			root := readinessRoot(t)
			create(t, root)

			var stdout, stderr bytes.Buffer
			if exitCode := cli.Run([]string{"status"}, root, nil, &stdout, &stderr); exitCode == 0 {
				t.Fatalf("exit code = 0, stdout = %q", stdout.String())
			}
			got := stderr.String()
			if !strings.HasPrefix(got, "REPO001 ") || !strings.Contains(got, "gitone doctor") {
				t.Fatalf("stderr = %q", got)
			}
			if strings.Contains(got, "gitone init") || strings.Contains(got, "gitone migrate") {
				t.Fatalf("stderr suggests an unsupported operation: %q", got)
			}

			stdout.Reset()
			stderr.Reset()
			if exitCode := cli.Run([]string{"doctor"}, root, nil, &stdout, &stderr); exitCode != 1 {
				t.Fatalf("doctor exit code = %d, stderr = %q", exitCode, stderr.String())
			}
			if got := stdout.String(); !strings.Contains(got, ".git is not a directory") {
				t.Fatalf("doctor output = %q", got)
			}
		})
	}
}

// Partially present metadata is never reinitialized: it keeps the REPO001
// failure and points at doctor.
func TestReadinessWithPartialMetadataSuggestsDoctor(t *testing.T) {
	tests := map[string]func(t *testing.T, root string){
		"missing repository": func(t *testing.T, root string) {
			if err := os.RemoveAll(filepath.Join(root, ".gitone", "repositories", "notes")); err != nil {
				t.Fatal(err)
			}
		},
		"invalid repository": func(t *testing.T, root string) {
			directory := filepath.Join(root, ".gitone", "repositories", "notes")
			if err := os.RemoveAll(directory); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(directory, 0o700); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, damage := range tests {
		t.Run(name, func(t *testing.T) {
			root := readinessRoot(t)
			var stdout, stderr bytes.Buffer
			if exitCode := cli.Run([]string{"init"}, root, nil, &stdout, &stderr); exitCode != 0 {
				t.Fatalf("init exit code = %d, stderr = %q", exitCode, stderr.String())
			}
			damage(t, root)

			stdout.Reset()
			stderr.Reset()
			if exitCode := cli.Run([]string{"status"}, root, nil, &stdout, &stderr); exitCode == 0 {
				t.Fatalf("exit code = 0, stdout = %q", stdout.String())
			}
			got := stderr.String()
			if !strings.HasPrefix(got, "REPO001 ") || !strings.Contains(got, "gitone doctor") {
				t.Fatalf("stderr = %q", got)
			}
			if strings.Contains(got, "gitone init") || strings.Contains(got, "gitone migrate") {
				t.Fatalf("stderr suggests reinitialization: %q", got)
			}
			if strings.ContainsRune(got, 0x1b) {
				t.Fatalf("guidance is not plain on a redirected stream: %q", got)
			}
		})
	}
}

// The commands that exist to be usable before initialization keep working on
// an uninitialized and even unconfigured project.
func TestReadinessKeepsPreInitializationCommands(t *testing.T) {
	fakeCode(t, "exit 0\n")
	tests := []struct {
		args      []string
		configure bool
	}{
		{args: []string{"--help"}},
		{args: []string{"status", "--help"}},
		{args: []string{"--version"}},
		{args: []string{"vscode", "info", "--json"}},
		{args: []string{"vscode", "install"}},
		{args: []string{"repo", "validate"}, configure: true},
		{args: []string{"repo", "list"}, configure: true},
		{args: []string{"init"}, configure: true},
		{args: []string{"backup", "list"}, configure: true},
	}
	for _, test := range tests {
		t.Run(strings.Join(test.args, " "), func(t *testing.T) {
			root := t.TempDir()
			if test.configure {
				root = readinessRoot(t)
			}
			var stdout, stderr bytes.Buffer
			if exitCode := cli.Run(test.args, root, nil, &stdout, &stderr); exitCode != 0 {
				t.Fatalf("exit code = %d, stderr = %q", exitCode, stderr.String())
			}
		})
	}
}

// doctor inspects an uninitialized project itself instead of being stopped by
// the readiness check, and it names the command that creates the repositories
// the configuration declares.
func TestReadinessKeepsDoctorReporting(t *testing.T) {
	root := readinessRoot(t)
	var stdout, stderr bytes.Buffer
	if exitCode := cli.Run([]string{"doctor"}, root, nil, &stdout, &stderr); exitCode != 1 {
		t.Fatalf("exit code = %d, want 1", exitCode)
	}
	if got := stdout.String(); !strings.Contains(got, "gitone reconfigure") {
		t.Fatalf("doctor output = %q", got)
	}
}

// payload is what a hostile repository puts into a commit subject, a patch or
// a file name: an OSC that retitles the window, a CSI that clears the screen
// and a carriage return that overwrites the line already printed.
const payload = "\x1b]0;owned\x07\x1b[2Jharmless\rmalicious"

func TestRepositoryDataCannotDriveTheTerminal(t *testing.T) {
	root := statusRoot(t)
	for _, name := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(name, "GitOne")
	}
	for _, name := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(name, "gitone@example.com")
	}
	write := func(name, contents string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var stdout, stderr bytes.Buffer
	run := func(args ...string) string {
		t.Helper()
		stdout.Reset()
		stderr.Reset()
		if exitCode := cli.Run(args, root, nil, &stdout, &stderr); exitCode != 0 {
			t.Fatalf("%v exit code = %d, stderr = %q", args, exitCode, stderr.String())
		}
		return stdout.String()
	}

	// The payload reaches output as a commit subject, as patch content and as
	// the file name in a human error.
	write("README.md", "readme "+payload+"\n")
	run("add", "-A")
	run("commit", "-m", "subject "+payload)
	write("README.md", "changed "+payload+"\n")

	if got := run("diff"); strings.ContainsAny(got, "\x1b\x07\r") || !strings.Contains(got, "changed harmlessmalicious") {
		t.Fatalf("patch = %q, want the readable change without control sequences", got)
	}
	run("add", "README.md")
	if got := run("diff", "--staged"); strings.ContainsAny(got, "\x1b\x07\r") {
		t.Fatalf("staged patch = %q, want no terminal control sequence", got)
	}
	if got := run("log"); strings.ContainsAny(got, "\x1b\x07\r") || !strings.Contains(got, "subject harmlessmalicious") {
		t.Fatalf("log = %q, want the readable subject without control sequences", got)
	}

	write("unassigned"+payload+".md", "x\n")
	for _, args := range [][]string{{"status"}, {"status", "--porcelain"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			stdout.Reset()
			stderr.Reset()
			cli.Run(args, root, nil, &stdout, &stderr)
			for stream, got := range map[string]string{"stdout": stdout.String(), "stderr": stderr.String()} {
				if strings.ContainsAny(got, "\x1b\x07\r") {
					t.Fatalf("%s = %q, want no terminal control sequence", stream, got)
				}
			}
		})
	}
}

// agentsProject is a valid configuration whose one repository owns AGENTS.md.
const agentsProject = `version: 1
default_branch: main
repositories:
  website:
    visibility: public
    paths: [.gitignore, .gitone.yml, AGENTS.md]
`

func TestAgentsChecksAndUpdatesWithoutInitializedRepositories(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".gitone.yml"), []byte(agentsProject), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if exitCode := cli.Run([]string{"agents"}, root, nil, &stdout, &stderr); exitCode != 1 {
		t.Fatalf("exit code = %d, want 1", exitCode)
	}
	if !strings.HasPrefix(stderr.String(), "AGENT001 ") {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if _, err := os.Lstat(filepath.Join(root, "AGENTS.md")); !os.IsNotExist(err) {
		t.Fatalf("the read-only check wrote the file: %v", err)
	}

	// The update needs no terminal, no confirmation and no repository.
	stdout.Reset()
	stderr.Reset()
	if exitCode := cli.Run([]string{"agents", "update"}, root, nil, &stdout, &stderr); exitCode != 0 {
		t.Fatalf("exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "was created") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if got := read(t, filepath.Join(root, "AGENTS.md")); !strings.Contains(got, "<!-- gitone:agents:start -->") ||
		!strings.Contains(got, agents.Instructions()) {
		t.Fatalf("AGENTS.md = %q", got)
	}

	stdout.Reset()
	stderr.Reset()
	if exitCode := cli.Run([]string{"agents"}, root, nil, &stdout, &stderr); exitCode != 0 {
		t.Fatalf("exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "holds the current GitOne instructions") {
		t.Fatalf("stdout = %q", stdout.String())
	}

	// A second update reports the unchanged file instead of rewriting it.
	stdout.Reset()
	if exitCode := cli.Run([]string{"agents", "update"}, root, nil, &stdout, &stderr); exitCode != 0 {
		t.Fatalf("exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Nothing was changed") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestAgentsRefusesUnownedAndUnsafeFiles(t *testing.T) {
	tests := map[string]struct {
		configuration string
		contents      string
		want          string
	}{
		"unassigned": {
			configuration: "version: 1\ndefault_branch: main\nrepositories:\n  website:\n    visibility: public\n    paths: [.gitone.yml]\n",
			want:          "PATH001",
		},
		"unrecognizable instructions": {
			configuration: agentsProject,
			contents:      "This project uses GitOne: something older.\n",
			want:          "AGENT001",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, ".gitone.yml"), []byte(tt.configuration), 0o600); err != nil {
				t.Fatal(err)
			}
			if tt.contents != "" {
				if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte(tt.contents), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			for _, arguments := range [][]string{{"agents"}, {"agents", "update"}} {
				var stdout, stderr bytes.Buffer
				if exitCode := cli.Run(arguments, root, nil, &stdout, &stderr); exitCode != 1 {
					t.Fatalf("%v exit code = %d, want 1", arguments, exitCode)
				}
				if !strings.HasPrefix(stderr.String(), tt.want+" ") {
					t.Fatalf("%v stderr = %q, want %s", arguments, stderr.String(), tt.want)
				}
			}
			if tt.contents != "" {
				if got := read(t, filepath.Join(root, "AGENTS.md")); got != tt.contents {
					t.Fatalf("the refused file was changed: %q", got)
				}
			}
		})
	}
}

func TestAgentsNeedsAConfiguration(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if exitCode := cli.Run([]string{"agents"}, t.TempDir(), nil, &stdout, &stderr); exitCode != 1 {
		t.Fatalf("exit code = %d, want 1", exitCode)
	}
	if !strings.HasPrefix(stderr.String(), "CONFIG001 ") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func read(t *testing.T, name string) string {
	t.Helper()
	contents, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(contents)
}
