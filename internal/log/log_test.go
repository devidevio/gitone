package log

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
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

func render(t *testing.T, configuration *config.Config, root, target string, count int) string {
	t.Helper()
	var output bytes.Buffer
	if err := Log(configuration, root, target, count, &output); err != nil {
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
	run(t, root, "public", "commit", "-m", "public subject")
	run(t, root, "private", "add", "--", "secrets/key.txt")
	run(t, root, "private", "commit", "-m", "private subject")
	return configuration, root
}

func TestLogShowsEveryRepositoryInOrderWithNativeCommitDetails(t *testing.T) {
	configuration, root := committed(t)

	got := render(t, configuration, root, "", DefaultCount)
	if !strings.HasPrefix(got, "PRIVATE\n") || strings.Index(got, "PRIVATE") > strings.Index(got, "PUBLIC") {
		t.Fatalf("repositories are not in configuration order:\n%s", got)
	}
	head := strings.TrimSpace(run(t, root, "public", "rev-parse", "--short", "HEAD"))
	for _, want := range []string{head, "(HEAD -> main)", "public subject", "private subject"} {
		if !strings.Contains(got, want) {
			t.Fatalf("log is missing %q:\n%s", want, got)
		}
	}
	if got != render(t, configuration, root, "", DefaultCount) {
		t.Fatal("log is not deterministic")
	}
}

func TestLogLimitsTheCommitsPerRepository(t *testing.T) {
	configuration, root := committed(t)
	for i := range 25 {
		write(t, root, "README.md", strconv.Itoa(i))
		run(t, root, "public", "add", "--", "README.md")
		run(t, root, "public", "commit", "-m", "commit "+strconv.Itoa(i))
	}

	if got := commitLines(render(t, configuration, root, "public", DefaultCount)); got != DefaultCount {
		t.Fatalf("default log has %d commits, want %d", got, DefaultCount)
	}
	if got := commitLines(render(t, configuration, root, "public", 3)); got != 3 {
		t.Fatalf("limited log has %d commits, want 3", got)
	}
}

// commitLines counts the commit lines of a one-repository section.
func commitLines(section string) int {
	return len(strings.Split(strings.TrimSuffix(section, "\n"), "\n")) - 1
}

func TestLogSelectsOneRepository(t *testing.T) {
	configuration, root := committed(t)

	got := render(t, configuration, root, "public", DefaultCount)
	if !strings.Contains(got, "public subject") || strings.Contains(got, "private subject") {
		t.Fatalf("public log = %q", got)
	}
	if all := render(t, configuration, root, "all", DefaultCount); all != render(t, configuration, root, "", DefaultCount) {
		t.Fatalf("all differs from the default target:\n%s", all)
	}
}

func TestLogLabelsAnUnbornRepositoryWithoutFailing(t *testing.T) {
	configuration, root := project(t)
	write(t, root, "README.md", "one\n")
	run(t, root, "public", "add", "--", "README.md")
	run(t, root, "public", "commit", "-m", "public subject")

	got := render(t, configuration, root, "", DefaultCount)
	if !strings.Contains(got, "PRIVATE\nno commits yet\n") {
		t.Fatalf("unborn repository is not labeled:\n%s", got)
	}
	if !strings.Contains(got, "public subject") {
		t.Fatalf("the other repository is missing:\n%s", got)
	}
}

func TestLogRejectsAnUnknownRepositoryAndAnInvalidCount(t *testing.T) {
	configuration, root := committed(t)
	tests := []struct {
		target string
		count  int
	}{
		{target: "missing", count: DefaultCount},
		{count: 0},
		{count: -1},
		{count: MaxCount + 1},
	}
	for _, test := range tests {
		var output bytes.Buffer
		err := Log(configuration, root, test.target, test.count, &output)
		if err == nil || !strings.HasPrefix(err.Error(), "CLI001") {
			t.Fatalf("Log(%q, %d) error = %v, want CLI001", test.target, test.count, err)
		}
		if output.Len() != 0 {
			t.Fatalf("output = %q, want empty", output.String())
		}
	}
}

func TestLogRefusesWhileRecoveryStateExists(t *testing.T) {
	configuration, root := committed(t)
	if err := os.MkdirAll(lock.RecoveryPath(root), 0o700); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	err := Log(configuration, root, "", DefaultCount, &output)
	if err == nil || !strings.HasPrefix(err.Error(), "REC001") {
		t.Fatalf("error = %v, want REC001", err)
	}
	if output.Len() != 0 {
		t.Fatalf("output = %q, want empty", output.String())
	}
}

func TestLogChangesNothing(t *testing.T) {
	configuration, root := committed(t)
	write(t, root, "README.md", "two\n")
	run(t, root, "public", "add", "--", "README.md")
	before := run(t, root, "public", "status", "--porcelain", "--branch")

	render(t, configuration, root, "", DefaultCount)

	if after := run(t, root, "public", "status", "--porcelain", "--branch"); after != before {
		t.Fatalf("status after log = %q, want %q", after, before)
	}
	contents, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil || string(contents) != "two\n" {
		t.Fatalf("README.md = %q, %v", contents, err)
	}
}

// The history is read through a pipe, so nothing may be colored even when Git
// is configured to always color its own output.
func TestLogOutputHasNoColorCodes(t *testing.T) {
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "color.ui")
	t.Setenv("GIT_CONFIG_VALUE_0", "always")
	configuration, root := committed(t)

	if got := render(t, configuration, root, "", DefaultCount); strings.Contains(got, "\x1b[") {
		t.Fatalf("log contains ANSI sequences: %q", got)
	}
}

// A terminal colors commit hashes after the repository text is safe.
func TestCommitsColorForATerminal(t *testing.T) {
	_, root := committed(t)

	commits, err := commits(root, "public", DefaultCount)
	if err != nil {
		t.Fatal(err)
	}
	colored := colorCommits(ui.New(true), commits)
	if !strings.Contains(colored, "\x1b[") {
		t.Fatalf("colored history has no ANSI sequences: %q", colored)
	}
}

func TestCommitsDisablesSignatureOutput(t *testing.T) {
	directory := t.TempDir()
	fake := filepath.Join(directory, "git")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\ncase \"$*\" in\n  *\"rev-parse --verify HEAD\"*|*\"--no-show-signature\"*) exit 0 ;;\n  *) exit 1 ;;\nesac\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))

	if _, err := commits(t.TempDir(), "public", DefaultCount); err != nil {
		t.Fatal(err)
	}
}
