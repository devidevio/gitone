package diff

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/devidevio/gitone/internal/config"
	"github.com/devidevio/gitone/internal/git"
	"github.com/devidevio/gitone/internal/lock"
	"github.com/devidevio/gitone/internal/repository"
	"github.com/devidevio/gitone/internal/ui"
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

func project(t *testing.T) (*config.Config, string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	write(t, root, ".gitone.yml", twoRepositories)
	configuration, discovered, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.Init(configuration, discovered); err != nil {
		t.Fatal(err)
	}
	return configuration, discovered
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

// run executes git against one managed repository the way GitOne does.
func run(t *testing.T, root, name string, arguments ...string) {
	t.Helper()
	arguments = append([]string{"--git-dir=" + repository.Directory(root, name), "--work-tree=" + root,
		"-c", "user.name=GitOne", "-c", "user.email=gitone@example.com"}, arguments...)
	if _, err := git.Run(root, arguments...); err != nil {
		t.Fatal(err)
	}
}

func render(t *testing.T, configuration *config.Config, root, target string, staged bool) string {
	t.Helper()
	var output bytes.Buffer
	if err := Diff(configuration, root, target, staged, &output); err != nil {
		t.Fatal(err)
	}
	return output.String()
}

// committed is a project where both repositories have one committed file.
func committed(t *testing.T) (*config.Config, string) {
	t.Helper()
	configuration, root := project(t)
	write(t, root, "README.md", "one\n")
	write(t, root, "secrets/key.txt", "secret\n")
	run(t, root, "public", "add", "--", "README.md")
	run(t, root, "public", "commit", "-m", "public")
	run(t, root, "private", "add", "--", "secrets/key.txt")
	run(t, root, "private", "commit", "-m", "private")
	return configuration, root
}

func TestDiffShowsUnstagedChangesOfEveryRepositoryInOrder(t *testing.T) {
	configuration, root := committed(t)
	write(t, root, "README.md", "two\n")
	write(t, root, "secrets/key.txt", "changed\n")

	got := render(t, configuration, root, "", false)
	if !strings.HasPrefix(got, "PRIVATE\n") {
		t.Fatalf("private is not the first section:\n%s", got)
	}
	if strings.Index(got, "PRIVATE") > strings.Index(got, "PUBLIC") {
		t.Fatalf("repositories are not in configuration order:\n%s", got)
	}
	for _, want := range []string{"a/secrets/key.txt", "+changed", "a/README.md", "+two"} {
		if !strings.Contains(got, want) {
			t.Fatalf("diff is missing %q:\n%s", want, got)
		}
	}
	if got != render(t, configuration, root, "", false) {
		t.Fatal("diff is not deterministic")
	}
}

func TestDiffSelectsOneRepository(t *testing.T) {
	configuration, root := committed(t)
	write(t, root, "README.md", "two\n")
	write(t, root, "secrets/key.txt", "changed\n")

	got := render(t, configuration, root, "public", false)
	if !strings.Contains(got, "README.md") || strings.Contains(got, "secrets/key.txt") {
		t.Fatalf("public diff = %q", got)
	}
	if all := render(t, configuration, root, "all", false); all != render(t, configuration, root, "", false) {
		t.Fatalf("all differs from the default target:\n%s", all)
	}
}

func TestDiffStagedShowsIndexAgainstHead(t *testing.T) {
	configuration, root := committed(t)
	write(t, root, "README.md", "two\n")
	run(t, root, "public", "add", "--", "README.md")

	staged := render(t, configuration, root, "", true)
	if !strings.Contains(staged, "+two") || !strings.Contains(staged, "-one") {
		t.Fatalf("staged diff = %q", staged)
	}
	if unstaged := render(t, configuration, root, "", false); unstaged != "" {
		t.Fatalf("unstaged diff = %q, want empty", unstaged)
	}
}

func TestDiffStagedReportsAdditionsOnAnUnbornBranch(t *testing.T) {
	configuration, root := project(t)
	write(t, root, "README.md", "one\n")
	run(t, root, "public", "add", "--", "README.md")

	got := render(t, configuration, root, "", true)
	for _, want := range []string{"PUBLIC\n", "new file mode", "+one"} {
		if !strings.Contains(got, want) {
			t.Fatalf("unborn staged diff is missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "PRIVATE") {
		t.Fatalf("a repository without changes was reported:\n%s", got)
	}
}

func TestDiffWithoutChangesPrintsNothing(t *testing.T) {
	configuration, root := committed(t)
	if got := render(t, configuration, root, "", false); got != "" {
		t.Fatalf("diff = %q, want empty", got)
	}
	if got := render(t, configuration, root, "", true); got != "" {
		t.Fatalf("staged diff = %q, want empty", got)
	}
}

func TestDiffRejectsAnUnknownRepository(t *testing.T) {
	configuration, root := committed(t)
	var output bytes.Buffer
	err := Diff(configuration, root, "missing", false, &output)
	if err == nil || !strings.HasPrefix(err.Error(), "CLI001") {
		t.Fatalf("error = %v, want CLI001", err)
	}
	if output.Len() != 0 {
		t.Fatalf("output = %q, want empty", output.String())
	}
}

func TestDiffRefusesWhileRecoveryStateExists(t *testing.T) {
	configuration, root := committed(t)
	write(t, root, "README.md", "two\n")
	if err := os.MkdirAll(lock.RecoveryPath(root), 0o700); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	err := Diff(configuration, root, "", false, &output)
	if err == nil || !strings.HasPrefix(err.Error(), "REC001") {
		t.Fatalf("error = %v, want REC001", err)
	}
	if output.Len() != 0 {
		t.Fatalf("output = %q, want empty", output.String())
	}
}

// A patch is read through a pipe, so nothing may be colored even when Git is
// configured to always color its own output.
func TestDiffOutputHasNoColorCodes(t *testing.T) {
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "color.ui")
	t.Setenv("GIT_CONFIG_VALUE_0", "always")
	configuration, root := committed(t)
	write(t, root, "README.md", "two\n")

	if got := render(t, configuration, root, "", false); strings.Contains(got, "\x1b[") {
		t.Fatalf("diff contains ANSI sequences: %q", got)
	}
}

func TestDiffDoesNotRunTextConversionFilters(t *testing.T) {
	configuration, root := committed(t)
	write(t, root, ".gitattributes", "README.md diff=fail\n")
	run(t, root, "public", "config", "diff.fail.textconv", "false")
	write(t, root, "README.md", "two\n")

	if got := render(t, configuration, root, "public", false); !strings.Contains(got, "+two") {
		t.Fatalf("diff = %q", got)
	}
}

// A terminal gets semantic patch colors after the repository text is safe.
func TestPatchColorsForATerminal(t *testing.T) {
	_, root := committed(t)
	write(t, root, "README.md", "two\n")

	patch, err := patch(root, "public", false)
	if err != nil {
		t.Fatal(err)
	}
	colored := colorPatch(ui.New(true), patch)
	if !strings.Contains(colored, "\x1b[") {
		t.Fatalf("colored patch has no ANSI sequences: %q", colored)
	}
}
