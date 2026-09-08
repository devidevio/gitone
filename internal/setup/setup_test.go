package setup_test

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/devidevio/gitone/internal/agents"
	"github.com/devidevio/gitone/internal/config"
	"github.com/devidevio/gitone/internal/git"
	"github.com/devidevio/gitone/internal/setup"
)

const publicConfiguration = `version: 1
default_branch: main
repositories:
  website:
    visibility: public
    paths:
      - .gitignore
      - .gitone.yml
      - README.md
`

// website answers the setup mode, the default branch and one public
// repository owning the one real file of the test project, up to but
// excluding the question for a further repository. Owned path 1 is the
// README.md suggestion; the two project files are assigned by setup itself.
var website = []string{"", "", "website", "", "1", "", ""}

// answers continues the website answers with the remaining ones.
func answers(rest ...string) []string {
	return append(append([]string(nil), website...), rest...)
}

func temp(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
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

func read(t *testing.T, root, name string) string {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
	if err != nil {
		t.Fatal(err)
	}
	return string(contents)
}

func exists(t *testing.T, name string) bool {
	t.Helper()
	_, err := os.Lstat(name)
	return err == nil
}

// project creates a directory with one file that the answers above assign.
func project(t *testing.T) string {
	root := temp(t)
	write(t, root, "README.md", "# project\n")
	return root
}

// run answers the questions in order and returns the complete output.
func run(t *testing.T, root string, answers ...string) (string, error) {
	t.Helper()
	fakeCode(t, `
if [ "$1" = "--list-extensions" ]; then
  printf 'devidevio.gitone\n'
  exit 0
fi
exit 1
`)
	return runSetup(root, answers...)
}

func runSetup(root string, answers ...string) (string, error) {
	output := new(bytes.Buffer)
	err := setup.Setup(root, true, strings.NewReader(strings.Join(answers, "\n")+"\n"), output)
	return output.String(), err
}

func fakeCode(t *testing.T, body string) {
	t.Helper()
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "code"), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestSetupCreatesConfigurationAndInitializes(t *testing.T) {
	root := project(t)

	output, err := run(t, root, answers("", "", "y")...)
	if err != nil {
		t.Fatalf("%v\n%s", err, output)
	}
	if got := read(t, root, ".gitone.yml"); got != publicConfiguration {
		t.Fatalf("configuration =\n%s\nwant\n%s", got, publicConfiguration)
	}
	if !strings.Contains(output, publicConfiguration[:len("version: 1")]) || !strings.Contains(output, "GitOne is set up.") {
		t.Fatalf("output does not show the configuration and the result:\n%s", output)
	}
	if !exists(t, filepath.Join(root, ".gitone", "repositories", "website", "HEAD")) {
		t.Error("the repository was not created")
	}
	if got := read(t, root, ".gitignore"); !strings.Contains(got, ".gitone/") || !strings.Contains(got, ".gitone.local.yml") {
		t.Errorf("ignore entries are missing: %q", got)
	}
	if exists(t, filepath.Join(root, ".gitone.local.yml")) {
		t.Error("a local configuration was written although none was selected")
	}
	if strings.Contains(output, "Install the GitOne VS Code extension?") {
		t.Error("setup offered an extension that the fake code CLI reported as installed")
	}
}

func TestSetupShowsVSIXInstructionsWithoutContactingMarketplace(t *testing.T) {
	root := project(t)
	log := filepath.Join(t.TempDir(), "arguments")
	t.Setenv("GITONE_CODE_LOG", log)
	fakeCode(t, `
printf '%s\n' "$@" >> "$GITONE_CODE_LOG"
if [ "$1" = "--list-extensions" ]; then
  exit 0
fi
exit 23
`)

	output, err := runSetup(root, answers("", "", "y")...)
	if err != nil {
		t.Fatalf("%v\n%s", err, output)
	}
	for _, want := range []string{"https://github.com/devidevio/gitone/releases/latest", "gitone vscode install --vsix <downloaded-file.vsix>"} {
		if !strings.Contains(output, want) {
			t.Fatalf("missing %q in output:\n%s", want, output)
		}
	}
	if got := read(t, filepath.Dir(log), filepath.Base(log)); got != "--list-extensions\n" {
		t.Fatalf("unexpected VS Code invocation: %q", got)
	}
	if !exists(t, filepath.Join(root, ".gitone", "repositories", "website", "HEAD")) {
		t.Error("project setup did not finish")
	}
}

func TestSetupWritesSelectedRepositoryToTheLocalFile(t *testing.T) {
	root := project(t)
	write(t, root, "notes/todo.md", "todo\n")

	output, err := run(t, root, answers("y",
		"notes", "private", "1", "git@example.com:notes.git", "local", "",
		"", "y")...)
	if err != nil {
		t.Fatalf("%v\n%s", err, output)
	}
	if got := read(t, root, ".gitone.yml"); got != publicConfiguration {
		t.Fatalf("the committed file contains private configuration:\n%s", got)
	}
	local := read(t, root, ".gitone.local.yml")
	want := `version: 1
repositories:
  notes:
    visibility: private
    remote: git@example.com:notes.git
    paths:
      - notes/**
`
	if local != want {
		t.Fatalf("local configuration =\n%s\nwant\n%s", local, want)
	}
	info, err := os.Stat(filepath.Join(root, ".gitone.local.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("local configuration permissions = %v, want 0600", info.Mode().Perm())
	}
	if got := read(t, root, ".gitignore"); !strings.Contains(got, ".gitone.local.yml") {
		t.Errorf("the local configuration is not ignored: %q", got)
	}
	if !exists(t, filepath.Join(root, ".gitone", "repositories", "notes", "HEAD")) {
		t.Error("the local repository was not created")
	}
}

func TestSetupMigratesARootRepository(t *testing.T) {
	root := project(t)
	for _, arguments := range [][]string{
		{"init", "--quiet", "--initial-branch=main"},
		{"-c", "user.name=GitOne", "-c", "user.email=gitone@example.com", "add", "-A"},
		{"-c", "user.name=GitOne", "-c", "user.email=gitone@example.com", "commit", "--quiet", "-m", "initial"},
	} {
		if _, err := git.Run(root, arguments...); err != nil {
			t.Fatal(err)
		}
	}
	write(t, root, "README.md", "# changed\n")

	output, err := run(t, root, answers("", "", "y")...)
	if err != nil {
		t.Fatalf("%v\n%s", err, output)
	}
	if !strings.Contains(output, "WARNING") {
		t.Errorf("the dirty working tree was not reported:\n%s", output)
	}
	if strings.Count(output, "Continue?") != 1 {
		t.Errorf("migration asked more than one confirmation:\n%s", output)
	}
	if exists(t, filepath.Join(root, ".git")) {
		t.Error("the root repository was kept")
	}
	if read(t, root, "README.md") != "# changed\n" {
		t.Error("the working tree changed")
	}
	if !exists(t, filepath.Join(root, ".gitone", "repositories", "website", "HEAD")) {
		t.Error("the repository was not created")
	}
}

func TestSetupCancellationWritesNothing(t *testing.T) {
	root := project(t)

	output, err := run(t, root, answers("", "", "n")...)
	if err != nil {
		t.Fatalf("%v\n%s", err, output)
	}
	if !strings.Contains(output, "Setup cancelled.") {
		t.Errorf("the cancellation was not reported:\n%s", output)
	}
	assertUntouched(t, root)
}

func TestSetupWithoutTerminalRefuses(t *testing.T) {
	root := project(t)

	output := new(bytes.Buffer)
	err := setup.Setup(root, false, strings.NewReader(""), output)
	if err == nil || !strings.HasPrefix(err.Error(), "SETUP001") {
		t.Fatalf("error = %v, want SETUP001", err)
	}
	assertUntouched(t, root)
}

func TestSetupRefusesLocalConfigurationWithoutPublicConfiguration(t *testing.T) {
	root := project(t)
	write(t, root, ".gitone.local.yml", "existing local configuration\n")

	output, err := run(t, root, answers("", "", "y")...)
	if err == nil || !strings.HasPrefix(err.Error(), "SETUP001") {
		t.Fatalf("error = %v, want SETUP001\n%s", err, output)
	}
	if got := read(t, root, ".gitone.local.yml"); got != "existing local configuration\n" {
		t.Fatalf("the local configuration was changed: %q", got)
	}
	for _, name := range []string{".gitone.yml", ".gitignore", ".gitone"} {
		if exists(t, filepath.Join(root, name)) {
			t.Errorf("%s was written", name)
		}
	}
}

func TestSetupEndedInputCancels(t *testing.T) {
	root := project(t)

	output := new(bytes.Buffer)
	if err := setup.Setup(root, true, strings.NewReader(""), output); err != nil {
		t.Fatalf("%v\n%s", err, output)
	}
	assertUntouched(t, root)
}

func TestSetupRepromptsInvalidValues(t *testing.T) {
	root := project(t)

	output, err := run(t, root,
		"",                               // setup mode
		"",                               // default branch
		"Website", "web site", "website", // two invalid names
		"open", "public", // invalid visibility
		"9", "", "1", // an unknown and an empty selection
		"", "", "",
		"", // the AGENTS.md question
		"y")
	if err != nil {
		t.Fatalf("%v\n%s", err, output)
	}
	if got := read(t, root, ".gitone.yml"); got != publicConfiguration {
		t.Fatalf("configuration =\n%s\nwant\n%s", got, publicConfiguration)
	}
	if !strings.Contains(output, "answer numbers between 1 and 2") ||
		!strings.Contains(output, "select at least one path") ||
		!strings.Contains(output, "visibility is public or private") {
		t.Errorf("invalid values were not explained:\n%s", output)
	}
}

func TestSetupRejectsConflictingCustomPatternsWhileTheyAreEntered(t *testing.T) {
	root := project(t)

	output, err := run(t, root,
		"", // setup mode
		"", // default branch
		"website", "", "1", "", "", "y",
		// README.md is not offered a second time, so the only way to claim it
		// again is a custom pattern, which is rejected immediately.
		"docs", "", "1", "README.md", "docs/**", "y", "",
		"", "", "",
		"website", // owner of the project files
		"",        // the AGENTS.md question
		"y")
	if err != nil {
		t.Fatalf("%v\n%s", err, output)
	}
	if !strings.Contains(output, `path is already configured for repository "website"`) {
		t.Errorf("the conflicting pattern was not rejected:\n%s", output)
	}
	if strings.Count(output, "Repository name: ") != 2 {
		t.Errorf("the repositories were entered again:\n%s", output)
	}
	want := `version: 1
default_branch: main
repositories:
  docs:
    visibility: public
    paths:
      - docs/**
  website:
    visibility: public
    paths:
      - .gitignore
      - .gitone.yml
      - README.md
`
	if got := read(t, root, ".gitone.yml"); got != want {
		t.Fatalf("configuration =\n%s\nwant\n%s", got, want)
	}
}

func TestSetupSelectsTheProjectFileOwnerAmongSeveralRepositories(t *testing.T) {
	root := project(t)
	write(t, root, "docs/guide.md", "guide\n")

	output, err := run(t, root,
		"", "", "website", "", "1", "", "", "y",
		"docs", "", "1", "", "", "",
		"none", "docs", // an unknown owner is asked again
		"", // the AGENTS.md question
		"y")
	if err != nil {
		t.Fatalf("%v\n%s", err, output)
	}
	want := `version: 1
default_branch: main
repositories:
  docs:
    visibility: public
    paths:
      - .gitignore
      - .gitone.yml
      - docs/**
  website:
    visibility: public
    paths:
      - README.md
`
	if got := read(t, root, ".gitone.yml"); got != want {
		t.Fatalf("configuration =\n%s\nwant\n%s", got, want)
	}
	if !strings.Contains(output, "answer one of docs, website") {
		t.Errorf("the unknown owner was not explained:\n%s", output)
	}
	if strings.Count(output, "Repository name: ") != 2 {
		t.Errorf("the repositories were entered again:\n%s", output)
	}
}

func TestSetupMovesTheProjectFileOwnerToTheCommittedFile(t *testing.T) {
	root := project(t)

	output, err := run(t, root,
		"", "", "website", "", "1", "", "local", "",
		"website", // the only repository, moved to the committed file
		"",        // the AGENTS.md question
		"y")
	if err != nil {
		t.Fatalf("%v\n%s", err, output)
	}
	if got := read(t, root, ".gitone.yml"); got != publicConfiguration {
		t.Fatalf("configuration =\n%s\nwant\n%s", got, publicConfiguration)
	}
	if exists(t, filepath.Join(root, ".gitone.local.yml")) {
		t.Error("the moved repository was also written to the local file")
	}
	if !strings.Contains(output, "moved from .gitone.local.yml") {
		t.Errorf("the move was not reported:\n%s", output)
	}
}

func TestSetupCompletesAnEnteredProjectFileOwner(t *testing.T) {
	root := project(t)
	write(t, root, "docs/guide.md", "guide\n")

	// The user assigned .gitone.yml by hand, so the missing .gitignore joins
	// it instead of asking for an owner.
	output, err := run(t, root,
		"", "", "website", "", "1,3", ".gitone.yml", "y", "", "", "", "y",
		"docs", "", "1", "", "", "",
		"", // the AGENTS.md question
		"y")
	if err != nil {
		t.Fatalf("%v\n%s", err, output)
	}
	if !strings.Contains(read(t, root, ".gitone.yml"), "      - .gitignore\n      - .gitone.yml\n      - README.md\n") {
		t.Fatalf("the missing project file was not added to the entered owner:\n%s", read(t, root, ".gitone.yml"))
	}
	if strings.Contains(output, "Owner [") {
		t.Errorf("an owner was asked although one was entered:\n%s", output)
	}
}

func TestSetupKeepsDistinctProjectFileOwners(t *testing.T) {
	root := project(t)
	write(t, root, "docs/guide.md", "guide\n")

	output, err := run(t, root,
		"", "", "website", "", "1,3", ".gitone.yml", "y", "", "", "", "y",
		"docs", "", "1,2", ".gitignore", "y", "", "", "", "",
		"", // the AGENTS.md question
		"y")
	if err != nil {
		t.Fatalf("%v\n%s", err, output)
	}
	configuration := read(t, root, ".gitone.yml")
	if !strings.Contains(configuration, "      - .gitignore\n      - docs/**\n") ||
		!strings.Contains(configuration, "      - .gitone.yml\n      - README.md\n") {
		t.Fatalf("the entered owners were changed:\n%s", configuration)
	}
	if strings.Contains(output, "Owner [") {
		t.Errorf("an owner was asked although both files were assigned:\n%s", output)
	}
}

// localProjectFileOwner answers a second, locally stored repository that
// claims .gitone.yml, up to and including the agent-instruction question. The
// claim reaches the correction below, because a clone reads only the
// committed file and would never find that owner.
var localProjectFileOwner = []string{
	"", "", "website", "", "1", "", "", "y",
	"notes", "private", "1,2", ".gitone.yml", "y", "", "", "local", "",
	"",
}

func TestSetupCorrectsALocalProjectFileOwnerByNarrowingItsPaths(t *testing.T) {
	root := project(t)
	write(t, root, "notes/todo.md", "todo\n")

	// Only the owned paths and the storage are asked again: the claim is
	// dropped, notes stays local and website ends up owning the project files.
	output, err := run(t, root, append(slices.Clone(localProjectFileOwner), "2", "", "y")...)
	if err != nil {
		t.Fatalf("%v\n%s", err, output)
	}
	if !strings.Contains(output, "Only the owned paths and the storage of notes are asked again") {
		t.Fatalf("the correction was not announced:\n%s", output)
	}
	if got, want := read(t, root, ".gitone.yml"), `version: 1
default_branch: main
repositories:
  website:
    visibility: public
    paths:
      - .gitignore
      - .gitone.yml
      - README.md
`; got != want {
		t.Fatalf("configuration =\n%s\nwant\n%s", got, want)
	}
	if got, want := read(t, root, ".gitone.local.yml"), `version: 1
repositories:
  notes:
    visibility: private
    paths:
      - notes/**
`; got != want {
		t.Fatalf("local configuration =\n%s\nwant\n%s", got, want)
	}
}

func TestSetupCorrectsALocalProjectFileOwnerByStoringItShared(t *testing.T) {
	root := project(t)
	write(t, root, "notes/todo.md", "todo\n")

	// Keeping the paths and answering shared is the other way out: name,
	// visibility and every other answer stay as entered.
	output, err := run(t, root, append(slices.Clone(localProjectFileOwner), "", "shared", "y")...)
	if err != nil {
		t.Fatalf("%v\n%s", err, output)
	}
	if got, want := read(t, root, ".gitone.yml"), `version: 1
default_branch: main
repositories:
  notes:
    visibility: private
    paths:
      - .gitignore
      - .gitone.yml
      - notes/**
  website:
    visibility: public
    paths:
      - README.md
`; got != want {
		t.Fatalf("configuration =\n%s\nwant\n%s", got, want)
	}
	if exists(t, filepath.Join(root, ".gitone.local.yml")) {
		t.Error(".gitone.local.yml was written although no repository is stored there")
	}
}

func TestSetupKeepsTheCreatedAgentsOwnerDuringLocalProjectFileCorrection(t *testing.T) {
	root := project(t)
	write(t, root, "notes/todo.md", "todo\n")
	answers := slices.Clone(localProjectFileOwner)
	answers[len(answers)-1] = "y"

	output, err := run(t, root, append(answers, "notes", "2", "", "y")...)
	if err != nil {
		t.Fatalf("%v\n%s", err, output)
	}
	if got, want := read(t, root, ".gitone.local.yml"), `version: 1
repositories:
  notes:
    visibility: private
    paths:
      - AGENTS.md
      - notes/**
`; got != want {
		t.Fatalf("local configuration =\n%s\nwant\n%s", got, want)
	}
	if got := read(t, root, "AGENTS.md"); got != markedBlock() {
		t.Fatalf("AGENTS.md =\n%s", got)
	}
}

func TestSetupCancellationAtTheLocalProjectFileCorrectionWritesNothing(t *testing.T) {
	root := project(t)
	write(t, root, "notes/todo.md", "todo\n")

	output, err := runSetup(root, localProjectFileOwner...)
	if err != nil {
		t.Fatalf("%v\n%s", err, output)
	}
	if !strings.Contains(output, "Setup cancelled.") {
		t.Fatalf("the ended input did not cancel setup:\n%s", output)
	}
	assertUntouched(t, root)
}

func TestSetupCancellationAtTheOwnerSelectionWritesNothing(t *testing.T) {
	root := project(t)
	write(t, root, "docs/guide.md", "guide\n")

	output, err := run(t, root,
		"", "", "website", "", "1", "", "", "y",
		"docs", "", "1", "", "", "", "")
	if err != nil {
		t.Fatalf("%v\n%s", err, output)
	}
	if !strings.Contains(output, "Setup cancelled.") {
		t.Errorf("the cancellation was not reported:\n%s", output)
	}
	assertUntouched(t, root)
}

func TestSetupRefusesNestedProjects(t *testing.T) {
	tests := map[string]func(t *testing.T, root string){
		"gitone": func(t *testing.T, root string) {
			write(t, root, ".gitone.yml", publicConfiguration)
		},
		"git": func(t *testing.T, root string) {
			if _, err := git.Run(root, "init", "--quiet", "--initial-branch=main"); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, prepare := range tests {
		t.Run(name, func(t *testing.T) {
			root := project(t)
			prepare(t, root)
			nested := filepath.Join(root, "nested")
			if err := os.Mkdir(nested, 0o755); err != nil {
				t.Fatal(err)
			}

			output, err := run(t, nested, answers("", "", "y")...)
			if err == nil || !strings.HasPrefix(err.Error(), "SETUP001") {
				t.Fatalf("error = %v, want SETUP001\n%s", err, output)
			}
			if exists(t, filepath.Join(nested, ".gitone.yml")) {
				t.Error("a nested configuration was written")
			}
		})
	}
}

func TestSetupUsesExistingConfiguration(t *testing.T) {
	root := project(t)
	write(t, root, ".gitone.yml", publicConfiguration)

	output, err := run(t, root, "y")
	if err != nil {
		t.Fatalf("%v\n%s", err, output)
	}
	if got := read(t, root, ".gitone.yml"); got != publicConfiguration {
		t.Fatalf("the existing configuration was edited:\n%s", got)
	}
	if !exists(t, filepath.Join(root, ".gitone", "repositories", "website", "HEAD")) {
		t.Error("the repository was not created")
	}

	// A healthy project is a success that changes nothing and asks nothing.
	output, err = run(t, root)
	if err != nil {
		t.Fatalf("%v\n%s", err, output)
	}
	if !strings.Contains(output, "already set up") || strings.Contains(output, "Continue?") {
		t.Errorf("an initialized project was not reported as done:\n%s", output)
	}
}

func TestSetupRefusesUnusableState(t *testing.T) {
	tests := map[string]struct {
		prepare func(t *testing.T, root string)
		want    string
	}{
		"invalid configuration": {
			prepare: func(t *testing.T, root string) { write(t, root, ".gitone.yml", "version: 1\n") },
			want:    "CONFIG003",
		},
		"interrupted migration": {
			prepare: func(t *testing.T, root string) {
				write(t, root, ".gitone.yml", publicConfiguration)
				write(t, root, ".gitone/migration/state.json", "{}")
			},
			want: "SETUP001",
		},
		"broken repository": {
			prepare: func(t *testing.T, root string) {
				write(t, root, ".gitone.yml", publicConfiguration)
				write(t, root, ".gitone/repositories/website", "not a repository")
			},
			want: "SETUP001",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			root := project(t)
			tt.prepare(t, root)

			output, err := run(t, root, "y")
			if err == nil || !strings.HasPrefix(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %s\n%s", err, tt.want, output)
			}
			if exists(t, filepath.Join(root, ".gitone", "repositories", "website", "HEAD")) {
				t.Error("a refused setup created repositories")
			}
		})
	}
}

func TestSetupRefusesUnexplainedInternalState(t *testing.T) {
	for _, name := range []string{".gitone/unknown", ".gitone/repositories/orphan/HEAD"} {
		t.Run(name, func(t *testing.T) {
			root := project(t)
			write(t, root, name, "unknown\n")

			output, err := run(t, root, answers("", "", "y")...)
			if err == nil || !strings.HasPrefix(err.Error(), "SETUP001") {
				t.Fatalf("error = %v, want SETUP001\n%s", err, output)
			}
			if strings.Contains(output, "Repository name") {
				t.Fatalf("unexplained state consumed the repository questions:\n%s", output)
			}
			if !exists(t, filepath.Join(root, filepath.FromSlash(name))) {
				t.Error("the unexplained state was changed")
			}
			for _, written := range []string{".gitone.yml", ".gitignore", ".gitone/repositories/website"} {
				if exists(t, filepath.Join(root, written)) {
					t.Errorf("%s was written", written)
				}
			}
		})
	}
}

func TestSetupDoesNotReuseItsOldTemporaryFileName(t *testing.T) {
	root := project(t)
	write(t, root, ".gitignore", "*.gitone\n")
	write(t, root, ".gitone.yml.gitone", "keep\n")

	output, err := run(t, root, answers("", "", "y")...)
	if err != nil {
		t.Fatalf("%v\n%s", err, output)
	}
	if got := read(t, root, ".gitone.yml.gitone"); got != "keep\n" {
		t.Fatalf("the existing temporary-name file was changed: %q", got)
	}
}

func TestSetupKeepsWrittenConfigurationWhenTheActionFails(t *testing.T) {
	root := project(t)
	// Recovery state of an unrelated interrupted operation blocks the lock
	// that init takes, which fails only after the configuration was written.
	if err := os.MkdirAll(filepath.Join(root, ".gitone", "recovery"), 0o700); err != nil {
		t.Fatal(err)
	}

	output, err := run(t, root, answers("y", "notes", "private", "1", "notes/**", "y", "", "", "local", "", "", "y")...)
	if err == nil {
		t.Fatalf("the failing action was reported as success:\n%s", output)
	}
	if !strings.HasPrefix(err.Error(), "REC001") || !strings.Contains(err.Error(), "kept") {
		t.Fatalf("error = %v", err)
	}
	if got := read(t, root, ".gitone.yml"); got != publicConfiguration {
		t.Fatalf("the configuration was not kept:\n%s", got)
	}
	if !strings.Contains(read(t, root, ".gitignore"), ".gitone.local.yml") {
		t.Error("the kept local configuration is not ignored")
	}
}

// assertUntouched checks that a refused or cancelled setup changed nothing.
func assertUntouched(t *testing.T, root string) {
	t.Helper()
	for _, name := range []string{".gitone.yml", ".gitone.local.yml", ".gitignore", ".gitone"} {
		if exists(t, filepath.Join(root, name)) {
			t.Errorf("%s was written", name)
		}
	}
}

func symlink(t *testing.T, root, target, name string) {
	t.Helper()
	if err := os.Symlink(target, filepath.Join(root, filepath.FromSlash(name))); err != nil {
		t.Fatal(err)
	}
}

func TestSetupOffersWorktreeSuggestionsWithoutIgnoredOrProjectPaths(t *testing.T) {
	root := project(t)
	write(t, root, ".gitignore", "build/\n")
	write(t, root, "build/out.js", "ignored\n")
	write(t, root, "docs/guide.md", "guide\n")

	output, err := run(t, root, "", "", "website", "", "1,2", "", "", "", "", "y")
	if err != nil {
		t.Fatalf("%v\n%s", err, output)
	}
	want := "  1) README.md\n  2) docs/**\n  3) Custom pattern...\n"
	if !strings.Contains(output, want) {
		t.Fatalf("offered paths =\n%s\nwant\n%s", output, want)
	}
	if !strings.Contains(read(t, root, ".gitone.yml"), "      - .gitignore\n      - .gitone.yml\n      - README.md\n      - docs/**\n") {
		t.Fatalf("configuration =\n%s", read(t, root, ".gitone.yml"))
	}
}

func TestSetupOffersAnAcceptedLinkLikeAnyOtherPath(t *testing.T) {
	root := project(t)
	write(t, root, "docs/guide.md", "guide\n")
	symlink(t, root, "docs/guide.md", "guide")

	output, err := run(t, root, "", "", "website", "", "1,2,3", "", "", "", "", "y")
	if err != nil {
		t.Fatalf("%v\n%s", err, output)
	}
	if !strings.Contains(output, "  3) guide\n") {
		t.Fatalf("the accepted link was not offered:\n%s", output)
	}
	if !strings.Contains(read(t, root, ".gitone.yml"), "      - guide\n") {
		t.Fatalf("configuration =\n%s", read(t, root, ".gitone.yml"))
	}
}

// TestSetupAbortsOnAnUnsafePathBeforeAnyQuestion covers the problems the
// wizard inventory answers without knowing a single owner. They end setup
// before the first question instead of after the last one.
func TestSetupAbortsOnAnUnsafePathBeforeAnyQuestion(t *testing.T) {
	tests := map[string]struct {
		prepare func(t *testing.T, root string)
		want    string
	}{
		"unsupported link": {
			prepare: func(t *testing.T, root string) { symlink(t, root, "/etc/passwd", "escape") },
			want:    "PATH003 escape",
		},
		"link chain": {
			prepare: func(t *testing.T, root string) {
				symlink(t, root, "README.md", "readme.link")
				symlink(t, root, "readme.link", "chain")
			},
			want: "PATH003 chain",
		},
		"nested repository": {
			prepare: func(t *testing.T, root string) {
				write(t, root, "vendor/main.go", "package main\n")
				if _, err := git.Run(filepath.Join(root, "vendor"), "init", "--quiet"); err != nil {
					t.Fatal(err)
				}
			},
			want: "PATH003 vendor",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			root := project(t)
			tt.prepare(t, root)

			output, err := run(t, root, answers("", "", "y")...)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %s\n%s", err, tt.want, output)
			}
			if strings.Contains(output, "Default branch") || strings.Contains(output, "Repository name") {
				t.Errorf("an unfixable problem consumed the wizard:\n%s", output)
			}
			assertUntouched(t, root)
		})
	}
}

// TestSetupTemplateModeIgnoresAnUnsafeWorkingTree proves that the preflight
// belongs to the wizard only: template mode inventories nothing.
func TestSetupTemplateModeIgnoresAnUnsafeWorkingTree(t *testing.T) {
	root := project(t)
	symlink(t, root, "/etc/passwd", "escape")

	output, err := runSetup(root, "Template", "", "y")
	if err != nil {
		t.Fatalf("%v\n%s", err, output)
	}
	if !exists(t, filepath.Join(root, ".gitone.yml")) {
		t.Error("the templates were not written")
	}
	if exists(t, filepath.Join(root, ".gitone")) {
		t.Error("template mode created managed state")
	}
}

func TestSetupRequiresACustomPatternWithoutSuggestions(t *testing.T) {
	root := temp(t)

	output, err := run(t, root, "", "", "website", "", "1", "src/**", "y", "", "", "", "", "", "y")
	if err != nil {
		t.Fatalf("%v\n%s", err, output)
	}
	if !strings.Contains(output, "  1) Custom pattern...\n") || strings.Contains(output, "  2)") {
		t.Fatalf("an empty project was offered more than a custom pattern:\n%s", output)
	}
	if !strings.Contains(output, "src/** matches no file this project has right now.") {
		t.Errorf("the zero-match warning is missing:\n%s", output)
	}
	want := `version: 1
default_branch: main
repositories:
  website:
    visibility: public
    paths:
      - .gitignore
      - .gitone.yml
      - src/**
`
	if got := read(t, root, ".gitone.yml"); got != want {
		t.Fatalf("configuration =\n%s\nwant\n%s", got, want)
	}
}

func TestSetupLetsAZeroMatchPatternBeEdited(t *testing.T) {
	root := project(t)

	// The mistyped pattern is refused at the warning and replaced by one that
	// matches, so it never reaches the configuration.
	output, err := run(t, root, "", "", "website", "", "2", "readme.md", "n", "README.md", "", "", "", "", "", "y")
	if err != nil {
		t.Fatalf("%v\n%s", err, output)
	}
	if !strings.Contains(output, "readme.md matches no file this project has right now.") {
		t.Errorf("the zero-match warning is missing:\n%s", output)
	}
	if got := read(t, root, ".gitone.yml"); got != publicConfiguration {
		t.Fatalf("configuration =\n%s\nwant\n%s", got, publicConfiguration)
	}
}

func TestSetupAssignsUnassignedPathsDirectly(t *testing.T) {
	root := project(t)
	write(t, root, "docs/guide.md", "guide\n")
	write(t, root, "notes/todo.md", "todo\n")
	write(t, root, "src/main.go", "package main\n")

	output, err := run(t, root,
		"", "develop", "website", "", "1", "", "", "y",
		"code", "private", "3", "git@example.com:code.git", "local", "",
		"", // the AGENTS.md question
		// The project files get their owner as before, and both unassigned
		// paths are then asked once, alphabetically, each answer adding
		// exactly the reported path.
		"website", "code",
		"y")
	if err != nil {
		t.Fatalf("%v\n%s", err, output)
	}
	if !strings.Contains(output, "PATH001 docs/guide.md") || !strings.Contains(output, "PATH001 notes/todo.md") {
		t.Errorf("the unassigned paths were not reported:\n%s", output)
	}
	first := strings.Index(output, "Repository owning docs/guide.md [website/code]")
	second := strings.Index(output, "Repository owning notes/todo.md [website/code]")
	if first < 0 || second < 0 || second < first {
		t.Errorf("the paths were not asked once in alphabetical order, with the repositories in setup order:\n%s", output)
	}
	if strings.Count(output, "Repository owning ") != 2 {
		t.Errorf("a path was asked more than once:\n%s", output)
	}
	if strings.Contains(output, "Correct the paths of") {
		t.Errorf("the owned-path browser was reopened for an unassigned path:\n%s", output)
	}
	if strings.Count(output, "Repository name: ") != 2 {
		t.Errorf("the repositories were entered again:\n%s", output)
	}
	want := `version: 1
default_branch: develop
repositories:
  website:
    visibility: public
    paths:
      - .gitignore
      - .gitone.yml
      - README.md
      - docs/guide.md
`
	if got := read(t, root, ".gitone.yml"); got != want {
		t.Fatalf("configuration =\n%s\nwant\n%s", got, want)
	}
	local := `version: 1
repositories:
  code:
    visibility: private
    remote: git@example.com:code.git
    paths:
      - notes/todo.md
      - src/**
`
	if got := read(t, root, ".gitone.local.yml"); got != local {
		t.Fatalf("local configuration =\n%s\nwant\n%s", got, local)
	}
}

func TestSetupAssignsUnassignedPathsToTheOnlyRepository(t *testing.T) {
	root := project(t)
	write(t, root, "docs/guide.md", "guide\n")

	output, err := run(t, root, "", "", "website", "", "1", "", "", "", "", "y")
	if err != nil {
		t.Fatalf("%v\n%s", err, output)
	}
	if strings.Contains(output, "Repository owning ") {
		t.Errorf("the only repository was still offered as a choice:\n%s", output)
	}
	if !strings.Contains(output, "Every unassigned path is added to website") {
		t.Errorf("the direct assignment was not reported:\n%s", output)
	}
	want := `version: 1
default_branch: main
repositories:
  website:
    visibility: public
    paths:
      - .gitignore
      - .gitone.yml
      - README.md
      - docs/guide.md
`
	if got := read(t, root, ".gitone.yml"); got != want {
		t.Fatalf("configuration =\n%s\nwant\n%s", got, want)
	}
}

func TestSetupWritesNothingWhenAnAssignmentIsCancelled(t *testing.T) {
	root := project(t)
	write(t, root, "docs/guide.md", "guide\n")
	write(t, root, "src/main.go", "package main\n")

	// The input ends at the assignment question, so the revalidated
	// configuration is never confirmed.
	output, err := run(t, root,
		"", "", "website", "", "1", "", "", "y",
		"code", "", "3", "", "", "",
		"website", "")
	if err != nil {
		t.Fatalf("%v\n%s", err, output)
	}
	if !strings.Contains(output, "Setup cancelled.") {
		t.Errorf("the cancellation was not reported:\n%s", output)
	}
	assertUntouched(t, root)
}

func TestSetupCorrectsAmbiguousProjectFileOwnership(t *testing.T) {
	root := project(t)

	output, err := run(t, root,
		"", "", "website", "", "1,2", ".gitone.yml", "y", "", "", "", "y",
		// Both repositories claim .gitone.yml, which no file in the working
		// tree can show while it is entered.
		"docs", "", "1", "*.yml", "y", "", "", "", "",
		"", // the AGENTS.md question
		// The unassigned .gitignore of the same round is assigned directly;
		// the remaining ambiguity still goes through the owned-path browser.
		"website",
		"docs", "2", "docs/**", "y", "",
		"y")
	if err != nil {
		t.Fatalf("%v\n%s", err, output)
	}
	if !strings.Contains(output, "PATH002 .gitone.yml: path matches docs, website") {
		t.Errorf("the ambiguous project file was not reported:\n%s", output)
	}
	if !strings.Contains(output, "Repository owning .gitignore") || !strings.Contains(output, "Correct the paths of") {
		t.Errorf("the mixed round did not assign directly and then browse:\n%s", output)
	}
	want := `version: 1
default_branch: main
repositories:
  docs:
    visibility: public
    paths:
      - docs/**
  website:
    visibility: public
    paths:
      - .gitignore
      - .gitone.yml
      - README.md
`
	if got := read(t, root, ".gitone.yml"); got != want {
		t.Fatalf("configuration =\n%s\nwant\n%s", got, want)
	}
}

func TestSetupCorrectsAnAmbiguousLocalProjectFileOwnerFirst(t *testing.T) {
	root := project(t)

	output, err := run(t, root,
		"", "", "website", "", "1,2", ".gitone.yml", "y", "", "", "", "y",
		// The local repository also claims the absent .gitone.yml. Its focused
		// correction runs before the remaining PATH002 flow.
		"docs", "", "1", "*.yml", "y", "", "", "local", "",
		"", // the AGENTS.md question
		"2", "docs/**", "y", "", "", "y")
	if err != nil {
		t.Fatalf("%v\n%s", err, output)
	}
	if !strings.Contains(output, "Only the owned paths and the storage of docs are asked again") ||
		!strings.Contains(output, "Store in [shared/local] (local)") {
		t.Fatalf("the focused local correction was not used:\n%s", output)
	}
	if strings.Contains(output, "Correct the paths of") {
		t.Fatalf("the local claim entered the generic PATH002 correction:\n%s", output)
	}
	if got, want := read(t, root, ".gitone.local.yml"), `version: 1
repositories:
  docs:
    visibility: public
    paths:
      - docs/**
`; got != want {
		t.Fatalf("local configuration =\n%s\nwant\n%s", got, want)
	}
}

// templateContinuation is the manual next step the template mode ends with.
const templateContinuation = `Edit .gitone.yml and optionally .gitone.local.yml.
Then run:
  gitone repo validate
  gitone setup
`

func TestSetupTemplateModeWritesEditableTemplates(t *testing.T) {
	root := project(t)
	write(t, root, ".gitignore", "build/\n")

	output, err := runSetup(root, "Template", "", "y")
	if err != nil {
		t.Fatalf("%v\n%s", err, output)
	}
	for name, want := range map[string]os.FileMode{".gitone.yml": 0o644, ".gitone.local.yml": 0o600} {
		info, err := os.Stat(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s mode = %v, want %v", name, got, want)
		}
	}
	if _, _, err := config.Load(root); err == nil || !strings.Contains(err.Error(), "<repository-name>") {
		t.Errorf("the untouched template is usable: %v", err)
	}
	if got := read(t, root, ".gitignore"); !strings.Contains(got, "build/\n") ||
		!strings.Contains(got, ".gitone/") || !strings.Contains(got, ".gitone.local.yml") {
		t.Errorf("ignore file = %q", got)
	}
	if exists(t, filepath.Join(root, ".gitone")) {
		t.Error("template mode created managed state")
	}
	if strings.Contains(output, "Default branch") || strings.Contains(output, "Repository name") {
		t.Errorf("template mode asked wizard questions:\n%s", output)
	}
	if !strings.Contains(output, templateContinuation) {
		t.Errorf("the manual continuation is missing:\n%s", output)
	}
}

func TestSetupTemplateModeCancellationWritesNothing(t *testing.T) {
	root := project(t)

	output, err := runSetup(root, "Template", "", "")
	if err != nil {
		t.Fatalf("%v\n%s", err, output)
	}
	if !strings.Contains(output, "Setup cancelled.") {
		t.Errorf("the cancellation was not reported:\n%s", output)
	}
	assertUntouched(t, root)
}

func TestSetupTemplateModeLeavesARootRepositoryAlone(t *testing.T) {
	root := project(t)
	if _, err := git.Run(root, "init", "--quiet", "--initial-branch=main"); err != nil {
		t.Fatal(err)
	}

	output, err := runSetup(root, "Template", "", "y")
	if err != nil {
		t.Fatalf("%v\n%s", err, output)
	}
	if !exists(t, filepath.Join(root, ".git")) {
		t.Error("the root repository was migrated")
	}
	if exists(t, filepath.Join(root, ".gitone")) {
		t.Error("template mode created managed state")
	}
	if strings.Contains(output, "Planned action") {
		t.Errorf("template mode planned an action:\n%s", output)
	}
}

func TestSetupDoesNotAskForAModeWithAnExistingConfiguration(t *testing.T) {
	root := project(t)
	write(t, root, ".gitone.yml", publicConfiguration)

	output, err := run(t, root, "y")
	if err != nil {
		t.Fatalf("%v\n%s", err, output)
	}
	if strings.Contains(output, "Setup mode") {
		t.Errorf("an existing configuration was asked for a mode:\n%s", output)
	}
	if got := read(t, root, ".gitone.yml"); got != publicConfiguration {
		t.Errorf("the existing configuration changed:\n%s", got)
	}
}

// agentsConfiguration is the committed configuration of a project whose one
// repository also owns AGENTS.md.
const agentsConfiguration = `version: 1
default_branch: main
repositories:
  website:
    visibility: public
    paths:
      - .gitignore
      - .gitone.yml
      - AGENTS.md
      - README.md
`

// markedBlock is the managed AGENTS.md GitOne writes.
func markedBlock() string {
	return "<!-- gitone:agents:start -->\n" + agents.Instructions() + "<!-- gitone:agents:end -->\n"
}

func TestSetupAddsTheAgentInstructionsAfterTheProjectIsSetUp(t *testing.T) {
	root := project(t)

	output, err := run(t, root, answers("", "y", "y")...)
	if err != nil {
		t.Fatalf("%v\n%s", err, output)
	}
	if got := read(t, root, ".gitone.yml"); got != agentsConfiguration {
		t.Fatalf("configuration =\n%s\nwant\n%s", got, agentsConfiguration)
	}
	if got := read(t, root, "AGENTS.md"); got != markedBlock() {
		t.Fatalf("AGENTS.md =\n%s", got)
	}
	// The optional write happens only once the project itself is set up.
	if !exists(t, filepath.Join(root, ".gitone", "repositories", "website", "HEAD")) {
		t.Error("the repository was not created")
	}
	if strings.Index(output, "GitOne is set up.") > strings.Index(output, "AGENTS.md holds the current") {
		t.Errorf("the instructions were written before the core result:\n%s", output)
	}
}

func TestSetupLetsALocalRepositoryOwnTheGeneratedAgentInstructions(t *testing.T) {
	root := project(t)
	write(t, root, "notes/todo.md", "todo\n")

	output, err := run(t, root, answers("y",
		"notes", "private", "1", "", "local", "",
		"y",     // add the instructions
		"notes", // the private repository owns them
		"y")...)
	if err != nil {
		t.Fatalf("%v\n%s", err, output)
	}
	if got := read(t, root, ".gitone.yml"); got != publicConfiguration {
		t.Fatalf("the committed file claims the instructions:\n%s", got)
	}
	local := `version: 1
repositories:
  notes:
    visibility: private
    paths:
      - AGENTS.md
      - notes/**
`
	if got := read(t, root, ".gitone.local.yml"); got != local {
		t.Fatalf("local configuration =\n%s\nwant\n%s", got, local)
	}
	if got := read(t, root, "AGENTS.md"); got != markedBlock() {
		t.Fatalf("AGENTS.md =\n%s", got)
	}
}

func TestSetupNeverAssignsTheInstructionsAfterNo(t *testing.T) {
	root := project(t)

	output, err := run(t, root, answers("", "", "y")...)
	if err != nil {
		t.Fatalf("%v\n%s", err, output)
	}
	if got := read(t, root, ".gitone.yml"); got != publicConfiguration {
		t.Fatalf("the declined file was assigned:\n%s", got)
	}
	if exists(t, filepath.Join(root, "AGENTS.md")) {
		t.Error("the declined file was written")
	}
	for _, want := range []string{
		"Assign AGENTS.md to exactly one repository in .gitone.yml or\n.gitone.local.yml",
		"gitone agents update",
		"https://gitone.io/#agents",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("the later steps do not name %q:\n%s", want, output)
		}
	}
}

func TestSetupCancellationAtTheAgentQuestionWritesNothing(t *testing.T) {
	root := project(t)

	output, err := run(t, root, answers("")...)
	if err != nil {
		t.Fatalf("%v\n%s", err, output)
	}
	if !strings.Contains(output, "Setup cancelled.") {
		t.Errorf("the cancellation was not reported:\n%s", output)
	}
	assertUntouched(t, root)
}

// existingAgents prepares a configured, uninitialized project whose owner of
// AGENTS.md is already decided, with the given instruction file.
func existingAgents(t *testing.T, contents string) string {
	t.Helper()
	root := project(t)
	write(t, root, ".gitone.yml", agentsConfiguration)
	if contents != "" {
		write(t, root, "AGENTS.md", contents)
	}
	return root
}

func TestSetupUpdatesAStaleInstructionBlockByDefault(t *testing.T) {
	root := existingAgents(t, "# Rules\n\n<!-- gitone:agents:start -->\nThis project uses GitOne: an older block.\n<!-- gitone:agents:end -->\n")

	// Enter accepts the offered update, the following Enter would not accept
	// the action, so the confirmation is answered explicitly.
	output, err := run(t, root, "", "y")
	if err != nil {
		t.Fatalf("%v\n%s", err, output)
	}
	if !strings.Contains(output, "Update GitOne instructions? [Y/n]") {
		t.Fatalf("the update was not offered with default yes:\n%s", output)
	}
	if got := read(t, root, "AGENTS.md"); got != "# Rules\n\n"+markedBlock() {
		t.Fatalf("AGENTS.md =\n%s", got)
	}
}

func TestSetupSkipsTheQuestionWhenTheBlockIsCurrent(t *testing.T) {
	for name, contents := range map[string]string{
		"marked":   markedBlock(),
		"unmarked": agents.Instructions(),
	} {
		t.Run(name, func(t *testing.T) {
			root := existingAgents(t, contents)

			output, err := run(t, root, "y")
			if err != nil {
				t.Fatalf("%v\n%s", err, output)
			}
			if strings.Contains(output, "GitOne instructions?") || strings.Contains(output, "gitone agents update") {
				t.Errorf("a current file was still asked about:\n%s", output)
			}
			if got := read(t, root, "AGENTS.md"); got != contents {
				t.Fatalf("the current file was changed:\n%s", got)
			}
		})
	}
}

func TestSetupWarnsAboutInstructionsItMustNotEdit(t *testing.T) {
	unclear := "This project uses GitOne: an older block nobody marked.\n"
	root := existingAgents(t, unclear)

	output, err := run(t, root, "y")
	if err != nil {
		t.Fatalf("%v\n%s", err, output)
	}
	if !strings.Contains(output, "AGENT001") || !strings.Contains(output, "https://gitone.io/#agents") {
		t.Fatalf("the unclear file was not reported:\n%s", output)
	}
	if strings.Contains(output, "GitOne instructions?") {
		t.Errorf("an unclear file was still offered for editing:\n%s", output)
	}
	if got := read(t, root, "AGENTS.md"); got != unclear {
		t.Fatalf("the unclear file was changed:\n%s", got)
	}
}

func TestSetupDoesNotOfferInstructionsAnExistingConfigurationCannotOwn(t *testing.T) {
	root := project(t)
	write(t, root, ".gitone.yml", publicConfiguration)

	output, err := run(t, root, "y")
	if err != nil {
		t.Fatalf("%v\n%s", err, output)
	}
	if strings.Contains(output, "GitOne instructions?") {
		t.Errorf("an unowned file was offered:\n%s", output)
	}
	if exists(t, filepath.Join(root, "AGENTS.md")) {
		t.Error("an unowned file was written")
	}
	if got := read(t, root, ".gitone.yml"); got != publicConfiguration {
		t.Fatalf("the existing configuration was edited:\n%s", got)
	}
	if !strings.Contains(output, "Assign AGENTS.md to exactly one repository") {
		t.Errorf("the missing assignment was not reported:\n%s", output)
	}
}

func TestSetupOffersAddingInstructionsToAnOwnedPath(t *testing.T) {
	root := existingAgents(t, "")

	output, err := run(t, root, "y", "y")
	if err != nil {
		t.Fatalf("%v\n%s", err, output)
	}
	if !strings.Contains(output, "Add the GitOne instructions for AI agents to AGENTS.md? [y/N]") {
		t.Fatalf("the addition was not offered with default no:\n%s", output)
	}
	if got := read(t, root, "AGENTS.md"); got != markedBlock() {
		t.Fatalf("AGENTS.md =\n%s", got)
	}
}

func TestSetupKeepsTheProjectWhenTheInstructionWriteFails(t *testing.T) {
	root := existingAgents(t, "")
	// The project is set up first, so the second run has nothing left to do
	// but the optional instruction write.
	if output, err := run(t, root, "n", "y"); err != nil {
		t.Fatalf("%v\n%s", err, output)
	}
	if err := os.Chmod(root, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o755) })

	output, err := run(t, root, "y")
	if err == nil || !strings.Contains(err.Error(), "only the optional AGENTS.md update failed") {
		t.Fatalf("error = %v\n%s", err, output)
	}
	if !strings.Contains(output, "already set up") {
		t.Errorf("the completed result was not reported:\n%s", output)
	}
	if !exists(t, filepath.Join(root, ".gitone", "repositories", "website", "HEAD")) {
		t.Error("the completed project setup was undone")
	}
}

func TestSetupTemplateModeAddsTheInstructionsToThePlaceholderRepository(t *testing.T) {
	root := temp(t)

	output, err := runSetup(root, "Template", "y", "y")
	if err != nil {
		t.Fatalf("%v\n%s", err, output)
	}
	configuration := read(t, root, ".gitone.yml")
	if !strings.Contains(configuration, "      - AGENTS.md\n") {
		t.Fatalf("the template does not own the instructions:\n%s", configuration)
	}
	if !strings.Contains(configuration, ".gitone.local.yml instead") {
		t.Errorf("the privacy alternative is not explained:\n%s", configuration)
	}
	if got := read(t, root, "AGENTS.md"); got != markedBlock() {
		t.Fatalf("AGENTS.md =\n%s", got)
	}
	if exists(t, filepath.Join(root, ".gitone")) {
		t.Error("template mode created managed state")
	}
}

func TestSetupTemplateModeLeavesTheInstructionsOutAfterNo(t *testing.T) {
	root := temp(t)

	output, err := runSetup(root, "Template", "", "y")
	if err != nil {
		t.Fatalf("%v\n%s", err, output)
	}
	if strings.Contains(read(t, root, ".gitone.yml"), "AGENTS.md") {
		t.Error("the declined file was assigned")
	}
	if exists(t, filepath.Join(root, "AGENTS.md")) {
		t.Error("the declined file was written")
	}
	if !strings.Contains(output, "gitone agents update") || !strings.Contains(output, "https://gitone.io/#agents") {
		t.Errorf("the later steps are missing:\n%s", output)
	}
}

func TestSetupTemplateModeLeavesAnUnownedAgentsFileAlone(t *testing.T) {
	root := temp(t)
	write(t, root, "AGENTS.md", "# Existing rules\n")

	output, err := runSetup(root, "Template", "y")
	if err != nil {
		t.Fatalf("%v\n%s", err, output)
	}
	if strings.Contains(output, "GitOne instructions?") {
		t.Errorf("an unowned file was offered:\n%s", output)
	}
	if got := read(t, root, "AGENTS.md"); got != "# Existing rules\n" {
		t.Fatalf("AGENTS.md changed:\n%s", got)
	}
	if strings.Contains(read(t, root, ".gitone.yml"), "AGENTS.md") {
		t.Error("the existing file was assigned to the public placeholder")
	}
	if !strings.Contains(output, "Assign AGENTS.md to exactly one repository") {
		t.Errorf("the later ownership step is missing:\n%s", output)
	}
}
