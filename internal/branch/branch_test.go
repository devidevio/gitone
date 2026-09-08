package branch_test

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/devidevio/gitone/internal/branch"
	"github.com/devidevio/gitone/internal/config"
	"github.com/devidevio/gitone/internal/git"
	"github.com/devidevio/gitone/internal/lock"
	"github.com/devidevio/gitone/internal/repository"
	"github.com/devidevio/gitone/internal/stage"
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

const recoveryMarker = "test-recovery-marker"

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

// committed is a project where both repositories have one committed file,
// which is the state every branch creation starts from.
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

func list(t *testing.T, configuration *config.Config, root string) string {
	t.Helper()
	var output bytes.Buffer
	if err := branch.Branch(configuration, root, "", false, nil, &output); err != nil {
		t.Fatal(err)
	}
	return output.String()
}

func create(configuration *config.Config, root, name string) (string, error) {
	var output bytes.Buffer
	err := branch.Branch(configuration, root, name, false, nil, &output)
	return output.String(), err
}

func remove(configuration *config.Config, root, name string) (string, error) {
	var output bytes.Buffer
	err := branch.Branch(configuration, root, name, true, nil, &output)
	return output.String(), err
}

func reference(t *testing.T, root, name, ref string) string {
	t.Helper()
	commit, err := repository.Reference(root, name, ref)
	if err != nil {
		t.Fatal(err)
	}
	return commit
}

func TestListShowsEveryBranchAndTheCurrentBranchPerRepository(t *testing.T) {
	configuration, root := committed(t)
	run(t, root, "public", "branch", "release")
	run(t, root, "private", "branch", "release")

	got := list(t, configuration, root)
	for _, want := range []string{"BRANCH", "REPOSITORIES", "main     private, public",
		"release  private, public", "Current branch", "  private: main", "  public: main"} {
		if !strings.Contains(got, want) {
			t.Fatalf("list is missing %q:\n%s", want, got)
		}
	}
	if strings.Index(got, "main") > strings.Index(got, "release") {
		t.Fatalf("branches are not sorted:\n%s", got)
	}
	if got != list(t, configuration, root) {
		t.Fatal("list is not deterministic")
	}
}

func TestListNamesBranchesMissingFromPartOfTheProject(t *testing.T) {
	configuration, root := committed(t)
	run(t, root, "public", "branch", "release")

	got := list(t, configuration, root)
	if !strings.Contains(got, "release  public\n") {
		t.Fatalf("release is not listed for public only:\n%s", got)
	}
	if !strings.Contains(got, "release is missing from private.") {
		t.Fatalf("the missing branch is not stated:\n%s", got)
	}
}

func TestListReportsAProjectWithoutCommits(t *testing.T) {
	configuration, root := project(t)

	got := list(t, configuration, root)
	if !strings.Contains(got, "main    -\n") {
		t.Fatalf("the unborn branch has repositories:\n%s", got)
	}
	for _, want := range []string{"main does not exist in any repository yet.", "  private: main", "  public: main"} {
		if !strings.Contains(got, want) {
			t.Fatalf("list is missing %q:\n%s", want, got)
		}
	}
}

func TestListReportsADetachedHead(t *testing.T) {
	configuration, root := committed(t)
	run(t, root, "private", "update-ref", "--no-deref", "HEAD", strings.TrimSpace(run(t, root, "private", "rev-parse", "HEAD")))

	got := list(t, configuration, root)
	for _, want := range []string{"  private: (detached)", "  public: main", "private is not on a branch."} {
		if !strings.Contains(got, want) {
			t.Fatalf("list is missing %q:\n%s", want, got)
		}
	}
}

func TestListReportsDisagreeingCurrentBranches(t *testing.T) {
	configuration, root := committed(t)
	run(t, root, "public", "branch", "release")
	run(t, root, "public", "symbolic-ref", "HEAD", "refs/heads/release")

	got := list(t, configuration, root)
	for _, want := range []string{"  private: main", "  public: release",
		"The repositories are not on the same branch."} {
		if !strings.Contains(got, want) {
			t.Fatalf("list is missing %q:\n%s", want, got)
		}
	}
}

func TestListRefusesWhileRecoveryStateExists(t *testing.T) {
	configuration, root := committed(t)
	if err := os.MkdirAll(lock.RecoveryPath(root), 0o700); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	err := branch.Branch(configuration, root, "", false, nil, &output)
	if err == nil || !strings.HasPrefix(err.Error(), "REC001") {
		t.Fatalf("error = %v, want REC001", err)
	}
	if output.Len() != 0 {
		t.Fatalf("output = %q, want empty", output.String())
	}
}

func TestCreateAddsTheBranchAtEveryCurrentHead(t *testing.T) {
	configuration, root := committed(t)
	heads := map[string]string{}
	for _, name := range []string{"private", "public"} {
		heads[name] = reference(t, root, name, "refs/heads/main")
	}

	output, err := create(configuration, root, "feature/auth")
	if err != nil {
		t.Fatalf("create: %v\noutput: %s", err, output)
	}
	for name, head := range heads {
		if got := reference(t, root, name, "refs/heads/feature/auth"); got != head {
			t.Fatalf("%s feature/auth = %q, want %q", name, got, head)
		}
		if !strings.Contains(output, name) {
			t.Fatalf("report is missing %s:\n%s", name, output)
		}
	}
	if _, err := os.Stat(lock.RecoveryPath(root)); !os.IsNotExist(err) {
		t.Fatalf("recovery state remained: %v", err)
	}
}

func TestCreateLeavesHeadIndexAndWorkingTreeUnchanged(t *testing.T) {
	configuration, root := committed(t)
	write(t, root, "README.md", "changed\n")
	run(t, root, "public", "add", "--", "README.md")

	before := map[string]string{}
	indexes := map[string][]byte{}
	for _, name := range []string{"private", "public"} {
		before[name] = run(t, root, name, "--no-optional-locks", "status", "--porcelain=v2", "--branch")
		contents, err := os.ReadFile(filepath.Join(repository.Directory(root, name), "index"))
		if err != nil {
			t.Fatal(err)
		}
		indexes[name] = contents
	}
	if _, err := create(configuration, root, "feature"); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"private", "public"} {
		if got := run(t, root, name, "--no-optional-locks", "status", "--porcelain=v2", "--branch"); got != before[name] {
			t.Fatalf("%s status changed:\n%s\n%s", name, before[name], got)
		}
		contents, err := os.ReadFile(filepath.Join(repository.Directory(root, name), "index"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(contents, indexes[name]) {
			t.Fatalf("%s index changed", name)
		}
	}
	if contents, err := os.ReadFile(filepath.Join(root, "README.md")); err != nil || string(contents) != "changed\n" {
		t.Fatalf("README.md = %q, %v", contents, err)
	}
}

func TestCreateRefusesInvalidNames(t *testing.T) {
	configuration, root := committed(t)
	for _, name := range []string{"bad..name", "bad name", "HEAD", "-dash", "feature/", "name.lock", "back\\slash", "star*"} {
		output, err := create(configuration, root, name)
		if err == nil || !strings.HasPrefix(err.Error(), "BRANCH001") {
			t.Fatalf("%q: error = %v, want BRANCH001", name, err)
		}
		if output != "" {
			t.Fatalf("%q: output = %q, want empty", name, output)
		}
	}
	for _, name := range []string{"private", "public"} {
		if got := len(strings.Fields(run(t, root, name, "for-each-ref", "--format=%(refname:short)", "refs/heads/"))); got != 1 {
			t.Fatalf("%s has %d branches, want 1", name, got)
		}
	}
}

func TestCreateRefusesAnExistingBranchInOneRepository(t *testing.T) {
	configuration, root := committed(t)
	run(t, root, "public", "branch", "release")

	_, err := create(configuration, root, "release")
	if err == nil || !strings.Contains(err.Error(), `BRANCH001 repository "public" already has branch release`) {
		t.Fatalf("error = %v, want the existing branch of public", err)
	}
	if !strings.Contains(err.Error(), "No branch, HEAD, index or working-tree file was changed.") {
		t.Fatalf("error does not state that nothing changed: %v", err)
	}
	if got := reference(t, root, "private", "refs/heads/release"); got != "" {
		t.Fatalf("private release = %q, want none", got)
	}
}

func TestCreateRefusesAnUnbornRepository(t *testing.T) {
	configuration, root := project(t)
	write(t, root, "README.md", "one\n")
	run(t, root, "public", "add", "--", "README.md")
	run(t, root, "public", "commit", "-m", "public subject")

	_, err := create(configuration, root, "release")
	if err == nil || !strings.Contains(err.Error(), `BRANCH001 repository "private" has no commit on main yet`) {
		t.Fatalf("error = %v, want the unborn private repository", err)
	}
	if got := reference(t, root, "public", "refs/heads/release"); got != "" {
		t.Fatalf("public release = %q, want none", got)
	}
}

func TestCreateRefusesDisagreeingCurrentBranches(t *testing.T) {
	configuration, root := committed(t)
	run(t, root, "public", "branch", "release")
	run(t, root, "public", "symbolic-ref", "HEAD", "refs/heads/release")

	_, err := create(configuration, root, "feature")
	if err == nil || !strings.Contains(err.Error(), "BRANCH001 the repositories are not on the same branch: private on main, public on release") {
		t.Fatalf("error = %v, want the branch disagreement", err)
	}
	if got := reference(t, root, "private", "refs/heads/feature"); got != "" {
		t.Fatalf("private feature = %q, want none", got)
	}
}

func TestCreateRollsBackAPartialFailure(t *testing.T) {
	configuration, root := committed(t)
	// A directory ref makes native Git refuse the shorter name in public
	// only, after private already received it.
	run(t, root, "public", "branch", "feature/auth")

	_, err := create(configuration, root, "feature")
	if err == nil || !strings.Contains(err.Error(), `BRANCH001 repository "public" could not create branch feature`) {
		t.Fatalf("error = %v, want the failed public repository", err)
	}
	if !strings.Contains(err.Error(), "No branch, HEAD, index or working-tree file was changed.") {
		t.Fatalf("error does not state the rollback: %v", err)
	}
	if got := reference(t, root, "private", "refs/heads/feature"); got != "" {
		t.Fatalf("private feature = %q, want it rolled back", got)
	}
	if got := reference(t, root, "private", "refs/heads/main"); got == "" {
		t.Fatal("private main disappeared")
	}
	if _, err := os.Stat(lock.RecoveryPath(root)); !os.IsNotExist(err) {
		t.Fatalf("recovery state remained: %v", err)
	}
}

// record writes the recovery state of an interrupted branch creation. created
// names the repositories whose ref was already written.
func record(t *testing.T, root, name string, commits map[string]string, created map[string]bool) {
	t.Helper()
	directory := lock.RecoveryPath(root)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	var entries []string
	for _, repositoryName := range []string{"private", "public"} {
		entries = append(entries, fmt.Sprintf(`{"name":%q,"commit":%q,"created":%t}`,
			repositoryName, commits[repositoryName], created[repositoryName]))
	}
	contents := fmt.Sprintf(`{"version":1,"command":"gitone branch","branch":%q,"marker":%q,"repositories":[%s]}`,
		name, recoveryMarker, strings.Join(entries, ","))
	if err := os.WriteFile(filepath.Join(directory, lock.StateFile), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

// interrupted is a project whose branch creation reached private only.
func interrupted(t *testing.T) (*config.Config, string, map[string]string) {
	t.Helper()
	configuration, root := committed(t)
	commits := map[string]string{
		"private": reference(t, root, "private", "refs/heads/main"),
		"public":  reference(t, root, "public", "refs/heads/main"),
	}
	run(t, root, "private", "update-ref", "--create-reflog", "-m",
		"gitone branch: Created from HEAD "+recoveryMarker, "refs/heads/release", commits["private"], "")
	record(t, root, "release", commits, map[string]bool{"private": true})
	return configuration, root, commits
}

func TestRecoverFinishesAnInterruptedCreation(t *testing.T) {
	configuration, root, commits := interrupted(t)

	var output bytes.Buffer
	if err := stage.Recover(configuration, root, &output); err != nil {
		t.Fatalf("recover: %v\noutput: %s", err, output.String())
	}
	for name, commit := range commits {
		if got := reference(t, root, name, "refs/heads/release"); got != commit {
			t.Fatalf("%s release = %q, want %q", name, got, commit)
		}
	}
	if !strings.Contains(output.String(), "gitone branch recovered.") {
		t.Fatalf("output does not report the recovery:\n%s", output.String())
	}
	if _, err := os.Stat(lock.RecoveryPath(root)); !os.IsNotExist(err) {
		t.Fatalf("recovery state remained: %v", err)
	}
}

func TestAbortRemovesOnlyTheCreatedBranches(t *testing.T) {
	configuration, root, commits := interrupted(t)

	var output bytes.Buffer
	if err := stage.Abort(configuration, root, &output); err != nil {
		t.Fatalf("abort: %v\noutput: %s", err, output.String())
	}
	for name, commit := range commits {
		if got := reference(t, root, name, "refs/heads/release"); got != "" {
			t.Fatalf("%s release = %q, want none", name, got)
		}
		if got := reference(t, root, name, "refs/heads/main"); got != commit {
			t.Fatalf("%s main = %q, want %q", name, got, commit)
		}
	}
	if !strings.Contains(output.String(), "gitone branch aborted.") {
		t.Fatalf("output does not report the abort:\n%s", output.String())
	}
	if _, err := os.Stat(lock.RecoveryPath(root)); !os.IsNotExist(err) {
		t.Fatalf("recovery state remained: %v", err)
	}
}

func TestAbortRefusesAnExternallyChangedBranch(t *testing.T) {
	configuration, root, commits := interrupted(t)
	run(t, root, "private", "commit", "--allow-empty", "-m", "external")
	moved := reference(t, root, "private", "refs/heads/main")
	run(t, root, "private", "update-ref", "refs/heads/release", moved, commits["private"])

	var output bytes.Buffer
	err := stage.Abort(configuration, root, &output)
	if err == nil || !strings.HasPrefix(err.Error(), "REC001") {
		t.Fatalf("error = %v, want REC001", err)
	}
	if got := reference(t, root, "private", "refs/heads/release"); got != moved {
		t.Fatalf("private release = %q, want the external %q", got, moved)
	}
	if _, err := os.Stat(lock.RecoveryPath(root)); err != nil {
		t.Fatalf("recovery state was removed: %v", err)
	}
}

func TestAbortDistinguishesACrashWindowFromAnExternalBranchAtTheSameCommit(t *testing.T) {
	configuration, root := committed(t)
	commits := map[string]string{
		"private": reference(t, root, "private", "refs/heads/main"),
		"public":  reference(t, root, "public", "refs/heads/main"),
	}
	// GitOne wrote private but stopped before recording Created. Public was
	// created externally at the same commit and must not be mistaken for it.
	run(t, root, "private", "update-ref", "--create-reflog", "-m",
		"gitone branch: Created from HEAD "+recoveryMarker, "refs/heads/release", commits["private"], "")
	run(t, root, "public", "branch", "release")
	record(t, root, "release", commits, nil)

	var output bytes.Buffer
	err := stage.Abort(configuration, root, &output)
	if err == nil || !strings.HasPrefix(err.Error(), "REC001") {
		t.Fatalf("error = %v, want REC001", err)
	}
	for _, name := range []string{"private", "public"} {
		if got := reference(t, root, name, "refs/heads/release"); got != commits[name] {
			t.Fatalf("%s release = %q, want %q", name, got, commits[name])
		}
	}

	run(t, root, "public", "branch", "-D", "release")
	output.Reset()
	if err := stage.Abort(configuration, root, &output); err != nil {
		t.Fatalf("abort after resolving external branch: %v", err)
	}
	if got := reference(t, root, "private", "refs/heads/release"); got != "" {
		t.Fatalf("private release = %q, want none", got)
	}
}
