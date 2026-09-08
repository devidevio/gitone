package git_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/devidevio/gitone/internal/git"
)

// globalConfiguration points Git at a private global configuration file that
// only the current test process sees.
func globalConfiguration(t *testing.T, contents string) {
	t.Helper()
	name := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(name, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", name)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
}

func TestRunKeepsUserConfiguration(t *testing.T) {
	globalConfiguration(t, "[user]\n\tname = Ada\n")

	output, err := git.Run(t.TempDir(), "config", "user.name")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(output) != "Ada" {
		t.Fatalf("user.name = %q, want %q", strings.TrimSpace(output), "Ada")
	}
}

func TestRunIsolatedDropsUserConfiguration(t *testing.T) {
	globalConfiguration(t, "[user]\n\tname = Ada\n")

	if output, err := git.RunIsolated(t.TempDir(), "config", "user.name"); err == nil {
		t.Fatalf("user.name = %q, want no value", strings.TrimSpace(output))
	}
}

func TestRunIgnoresInheritedRepositoryState(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("GIT_DIR", filepath.Join(directory, "missing"))
	t.Setenv("GIT_INDEX_FILE", filepath.Join(directory, "missing-index"))

	gitDirectory := filepath.Join(directory, "repository")
	if _, err := git.Run(directory, "--git-dir="+gitDirectory, "init", "--quiet"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(gitDirectory, "HEAD")); err != nil {
		t.Fatal(err)
	}
}

func TestRunReportsFailuresWithCode(t *testing.T) {
	_, err := git.Run(t.TempDir(), "rev-parse", "--git-dir")
	if err == nil || !strings.HasPrefix(err.Error(), "GIT001 ") {
		t.Fatalf("error = %v, want a GIT001 failure", err)
	}
}

// A staging failure must name the temporary index it ran against. Without it
// the reported command reads as one anybody could repeat by hand, and that
// repetition would touch the real index instead.
func TestRunIndexedReportsTheIndexItUsed(t *testing.T) {
	index := filepath.Join(t.TempDir(), "index")

	_, err := git.RunIndexed(t.TempDir(), index, "", "rev-parse", "--git-dir")
	if err == nil || !strings.HasPrefix(err.Error(), "GIT001 ") {
		t.Fatalf("error = %v, want a GIT001 failure", err)
	}
	if want := "GIT_INDEX_FILE=" + index + " git rev-parse"; !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want it to contain %q", err, want)
	}
}

// An ordinary command has no redirected index, so nothing is prefixed to it.
func TestRunReportsThePlainInvocation(t *testing.T) {
	_, err := git.Run(t.TempDir(), "rev-parse", "--git-dir")
	if err == nil || !strings.HasPrefix(err.Error(), "GIT001 git rev-parse --git-dir failed") {
		t.Fatalf("error = %v, want the bare invocation", err)
	}
}

func TestRunReportsSignalFailure(t *testing.T) {
	directory := t.TempDir()
	fake := filepath.Join(directory, "git")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nkill -TERM $$\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory)

	if _, err := git.Run(directory, "ignored"); err == nil {
		t.Fatal("signal-terminated git succeeded")
	}
}

func TestRunOfflineOverridesInteractiveEnvironment(t *testing.T) {
	directory := t.TempDir()
	fake := filepath.Join(directory, "git")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nprintf '%s\\n%s\\n' \"$GIT_TERMINAL_PROMPT\" \"$GIT_SSH_COMMAND\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GIT_TERMINAL_PROMPT", "1")
	t.Setenv("GIT_SSH_COMMAND", "interactive-ssh")

	output, err := git.RunOffline(directory, "ignored")
	if err != nil {
		t.Fatal(err)
	}
	if output != "0\nssh -oBatchMode=yes\n" {
		t.Fatalf("offline environment = %q", output)
	}
}

func TestCheckRefFormatFollowsNativeGit(t *testing.T) {
	valid := []string{"refs/heads/main", "refs/heads/feature/one", "refs/remotes/origin/HEAD"}
	for _, reference := range valid {
		switch got, err := git.CheckRefFormat(reference); {
		case err != nil:
			t.Fatalf("%q: %v", reference, err)
		case !got:
			t.Fatalf("%q is rejected, want accepted", reference)
		}
	}
	invalid := []string{"refs/heads/bad name", "refs/heads/bad..name", "refs/heads/name.lock", "refs/remotes/re:mote/HEAD"}
	for _, reference := range invalid {
		switch got, err := git.CheckRefFormat(reference); {
		case err != nil:
			t.Fatalf("%q: %v", reference, err)
		case got:
			t.Fatalf("%q is accepted, want rejected", reference)
		}
	}
}

// A git that cannot be executed must never look like a rejected name.
func TestCheckRefFormatReportsAnUnavailableGit(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	got, err := git.CheckRefFormat("refs/heads/main")
	if got || !errors.Is(err, git.ErrUnavailable) {
		t.Fatalf("valid = %v, err = %v, want false and ErrUnavailable", got, err)
	}
}
