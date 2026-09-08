package repository_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/devidevio/gitone/internal/config"
	"github.com/devidevio/gitone/internal/git"
	"github.com/devidevio/gitone/internal/repository"
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
    remote: git@example.com:public.git
    paths:
      - .gitignore
      - .gitone.yml
      - README.md
      - src/**
`

func project(t *testing.T, configuration string, files map[string]string) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	write(t, root, ".gitone.yml", configuration)
	for name, contents := range files {
		write(t, root, name, contents)
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

func initialize(t *testing.T, root string) error {
	t.Helper()
	configuration, discovered, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	return repository.Init(configuration, discovered)
}

func mustInitialize(t *testing.T, root string) {
	t.Helper()
	if err := initialize(t, root); err != nil {
		t.Fatal(err)
	}
}

func gitDirectory(root, name string) string {
	return filepath.Join(root, ".gitone", "repositories", name)
}

// run executes git against one managed repository the way GitOne does.
func run(t *testing.T, root, name string, arguments ...string) string {
	t.Helper()
	output, err := git.Run(root, append([]string{"--git-dir=" + gitDirectory(root, name)}, arguments...)...)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(output)
}

// snapshot records the contents of every project file outside .gitone/.
func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if relative == ".gitone" {
				return filepath.SkipDir
			}
			return nil
		}
		contents, err := os.ReadFile(path)
		files[filepath.ToSlash(relative)] = string(contents)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestInitCreatesEveryRepository(t *testing.T) {
	root := project(t, twoRepositories, map[string]string{
		"README.md":       "readme",
		"src/main.go":     "package main",
		"secrets/key.txt": "secret",
	})

	mustInitialize(t, root)

	for _, name := range []string{"private", "public"} {
		if run(t, root, name, "symbolic-ref", "HEAD") != "refs/heads/main" {
			t.Fatalf("%s HEAD = %q, want refs/heads/main", name, run(t, root, name, "symbolic-ref", "HEAD"))
		}
		if run(t, root, name, "rev-parse", "--show-toplevel") != root {
			t.Fatalf("%s working tree = %q, want %q", name, run(t, root, name, "rev-parse", "--show-toplevel"), root)
		}
		// Each repository keeps its own index inside its own Git directory.
		if run(t, root, name, "rev-parse", "--git-path", "index") != filepath.Join(gitDirectory(root, name), "index") {
			t.Fatalf("%s index = %q, want its own Git directory", name, run(t, root, name, "rev-parse", "--git-path", "index"))
		}
		if _, err := os.Stat(filepath.Join(gitDirectory(root, name), "hooks")); err != nil {
			t.Fatalf("%s hooks: %v", name, err)
		}
	}
	if got := run(t, root, "public", "config", "remote.origin.url"); got != "git@example.com:public.git" {
		t.Fatalf("public origin = %q", got)
	}
	if _, err := git.Run(root, "--git-dir="+gitDirectory(root, "private"), "config", "remote.origin.url"); err == nil {
		t.Fatal("private has an origin, want none")
	}
}

func TestInitStagesAndCommitsNothing(t *testing.T) {
	root := project(t, twoRepositories, map[string]string{"README.md": "readme"})

	mustInitialize(t, root)

	for _, name := range []string{"private", "public"} {
		if _, err := os.Stat(filepath.Join(gitDirectory(root, name), "index")); !os.IsNotExist(err) {
			t.Fatalf("%s has an index: %v", name, err)
		}
		if run(t, root, name, "rev-list", "--all", "--count") != "0" {
			t.Fatalf("%s has commits", name)
		}
	}
}

func TestHeadExistsDistinguishesUnbornCommittedAndBrokenRepositories(t *testing.T) {
	root := project(t, twoRepositories, map[string]string{"README.md": "readme"})
	mustInitialize(t, root)

	hasHead, err := repository.HeadExists(root, "public")
	if err != nil || hasHead {
		t.Fatalf("unborn HEAD = %v, %v", hasHead, err)
	}
	if _, err := git.Run(root, "--git-dir="+gitDirectory(root, "public"), "--work-tree="+root, "add", "README.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := git.Run(root, "--git-dir="+gitDirectory(root, "public"), "--work-tree="+root,
		"-c", "user.name=GitOne", "-c", "user.email=gitone@example.com", "commit", "-qm", "initial"); err != nil {
		t.Fatal(err)
	}
	hasHead, err = repository.HeadExists(root, "public")
	if err != nil || !hasHead {
		t.Fatalf("committed HEAD = %v, %v", hasHead, err)
	}
	write(t, gitDirectory(root, "public"), "refs/heads/main", "not-an-object\n")
	if _, err := repository.HeadExists(root, "public"); err == nil {
		t.Fatal("broken HEAD was treated as unborn")
	}
}

func TestInitLeavesProjectFilesUnchanged(t *testing.T) {
	files := map[string]string{
		"README.md":       "readme",
		"src/main.go":     "package main",
		"secrets/key.txt": "secret",
		".gitignore":      "# keep me\nnode_modules/\n",
	}
	root := project(t, twoRepositories, files)
	before := snapshot(t, root)

	mustInitialize(t, root)

	after := snapshot(t, root)
	for name, contents := range before {
		if name == ".gitignore" {
			continue
		}
		if after[name] != contents {
			t.Fatalf("%s changed", name)
		}
	}
	want := "# keep me\nnode_modules/\n.gitone/\n.gitone.local.yml\n"
	if after[".gitignore"] != want {
		t.Fatalf(".gitignore = %q, want %q", after[".gitignore"], want)
	}
}

func TestInitCreatesIgnoreFileAndStaysIdempotent(t *testing.T) {
	root := project(t, twoRepositories, map[string]string{"README.md": "readme"})

	mustInitialize(t, root)
	first := snapshot(t, root)
	if first[".gitignore"] != ".gitone/\n.gitone.local.yml\n" {
		t.Fatalf(".gitignore = %q", first[".gitignore"])
	}

	mustInitialize(t, root)
	if second := snapshot(t, root); second[".gitignore"] != first[".gitignore"] {
		t.Fatalf(".gitignore = %q, want it unchanged", second[".gitignore"])
	}
}

func TestInitAppendsMissingIgnoreEntryWithoutTrailingNewline(t *testing.T) {
	root := project(t, twoRepositories, map[string]string{
		"README.md":  "readme",
		".gitignore": ".gitone.local.yml",
	})

	mustInitialize(t, root)

	want := ".gitone.local.yml\n.gitone/\n"
	if got := snapshot(t, root)[".gitignore"]; got != want {
		t.Fatalf(".gitignore = %q, want %q", got, want)
	}
}

func TestInitIsNonDestructiveOnRerun(t *testing.T) {
	root := project(t, twoRepositories, map[string]string{"README.md": "readme"})
	mustInitialize(t, root)
	write(t, root, ".gitone/repositories/public/marker", "keep")

	mustInitialize(t, root)

	if contents, err := os.ReadFile(filepath.Join(gitDirectory(root, "public"), "marker")); err != nil || string(contents) != "keep" {
		t.Fatalf("marker = %q, %v", contents, err)
	}
}

func TestInitValidatesEveryRepositoryBeforeCreatingAny(t *testing.T) {
	root := project(t, twoRepositories, map[string]string{"README.md": "readme"})
	foreign := gitDirectory(root, "public")
	if err := os.MkdirAll(foreign, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("git", "init", "--bare", "--quiet", foreign).Run(); err != nil {
		t.Fatal(err)
	}

	if err := initialize(t, root); err == nil || !strings.HasPrefix(err.Error(), "REPO001 ") {
		t.Fatalf("error = %v, want REPO001", err)
	}
	if _, err := os.Stat(gitDirectory(root, "private")); !os.IsNotExist(err) {
		t.Fatalf("private repository was created: %v", err)
	}
}

func TestInitRejectsMissingRemoteWithoutRepairingIt(t *testing.T) {
	root := project(t, twoRepositories, map[string]string{"README.md": "readme"})
	mustInitialize(t, root)
	if _, err := git.Run(root, "--git-dir="+gitDirectory(root, "public"), "remote", "remove", "origin"); err != nil {
		t.Fatal(err)
	}

	if err := initialize(t, root); err == nil || !strings.HasPrefix(err.Error(), "REPO001 ") {
		t.Fatalf("error = %v, want REPO001", err)
	}
	if _, err := git.Run(root, "--git-dir="+gitDirectory(root, "public"), "config", "remote.origin.url"); err == nil {
		t.Fatal("origin was recreated")
	}
}

// A repository that switched branches keeps the branch it is on. The
// configured default branch is only where a repository starts, so init stays
// idempotent after gitone switch moved the project.
func TestInitKeepsTheBranchARepositorySwitchedTo(t *testing.T) {
	root := project(t, twoRepositories, map[string]string{"README.md": "readme"})
	mustInitialize(t, root)
	if _, err := git.Run(root, "--git-dir="+gitDirectory(root, "public"), "symbolic-ref", "HEAD", "refs/heads/other"); err != nil {
		t.Fatal(err)
	}

	if err := initialize(t, root); err != nil {
		t.Fatalf("init: %v", err)
	}
	if got := run(t, root, "public", "symbolic-ref", "HEAD"); got != "refs/heads/other" {
		t.Fatalf("HEAD = %q, want it unchanged", got)
	}
}

func TestInitRejectsExistingRootRepository(t *testing.T) {
	root := project(t, twoRepositories, map[string]string{"README.md": "readme"})
	if err := exec.Command("git", "init", "--quiet", root).Run(); err != nil {
		t.Fatal(err)
	}

	err := initialize(t, root)
	if err == nil || !strings.Contains(err.Error(), "PATH003 .git") {
		t.Fatalf("error = %v, want a PATH003 failure for .git", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".gitone")); !os.IsNotExist(err) {
		t.Fatalf(".gitone was created: %v", err)
	}
}

func TestInitRejectsNestedRepository(t *testing.T) {
	root := project(t, twoRepositories, map[string]string{"README.md": "readme", "src/main.go": "package main"})
	if err := exec.Command("git", "init", "--quiet", filepath.Join(root, "src")).Run(); err != nil {
		t.Fatal(err)
	}

	err := initialize(t, root)
	if err == nil || !strings.Contains(err.Error(), "nested Git repositories") {
		t.Fatalf("error = %v, want a nested repository failure", err)
	}
}

func TestInitRejectsUnassignedIgnoreFile(t *testing.T) {
	unassigned := `
version: 1
default_branch: main
repositories:
  public:
    visibility: public
    paths:
      - .gitone.yml
      - README.md
`
	root := project(t, unassigned, map[string]string{"README.md": "readme"})

	err := initialize(t, root)
	if err == nil || !strings.Contains(err.Error(), "PATH001 .gitignore") {
		t.Fatalf("error = %v, want PATH001 for .gitignore", err)
	}
}

func TestInitRejectsForeignGitDirectory(t *testing.T) {
	root := project(t, twoRepositories, map[string]string{"README.md": "readme"})
	foreign := gitDirectory(root, "public")
	if err := os.MkdirAll(foreign, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("git", "init", "--bare", "--quiet", foreign).Run(); err != nil {
		t.Fatal(err)
	}

	err := initialize(t, root)
	if err == nil || !strings.HasPrefix(err.Error(), "REPO001 ") {
		t.Fatalf("error = %v, want REPO001", err)
	}
	if bare := readConfig(t, foreign, "core.bare"); bare != "true" {
		t.Fatalf("core.bare = %q, want the existing repository untouched", bare)
	}
}

func TestInitRejectsNonRepositoryDirectory(t *testing.T) {
	root := project(t, twoRepositories, map[string]string{"README.md": "readme"})
	if err := os.MkdirAll(gitDirectory(root, "private"), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := initialize(t, root); err == nil || !strings.HasPrefix(err.Error(), "REPO001 ") {
		t.Fatalf("error = %v, want REPO001", err)
	}
}

func TestInitRejectsChangedRemote(t *testing.T) {
	root := project(t, twoRepositories, map[string]string{"README.md": "readme"})
	mustInitialize(t, root)
	if _, err := git.Run(root, "--git-dir="+gitDirectory(root, "public"), "remote", "set-url", "origin", "git@example.com:other.git"); err != nil {
		t.Fatal(err)
	}

	if err := initialize(t, root); err == nil || !strings.HasPrefix(err.Error(), "REPO001 ") {
		t.Fatalf("error = %v, want REPO001", err)
	}
	if got := run(t, root, "public", "config", "remote.origin.url"); got != "git@example.com:other.git" {
		t.Fatalf("origin = %q, want it untouched", got)
	}
}

func TestInitFailsWhileAnotherOperationHoldsTheLock(t *testing.T) {
	root := project(t, twoRepositories, map[string]string{"README.md": "readme"})
	mustInitialize(t, root)
	write(t, root, ".gitone/lock", `{"pid":`+strconv.Itoa(os.Getpid())+`,"host":"`+host(t)+`","command":"gitone commit"}`)

	err := initialize(t, root)
	if err == nil || !strings.HasPrefix(err.Error(), "LOCK001 ") {
		t.Fatalf("error = %v, want LOCK001", err)
	}
}

// TestRepositoryLocalConfigurationAndHooks proves that repository-local Git
// configuration and native hooks are reachable through the command runner.
func TestRepositoryLocalConfigurationAndHooks(t *testing.T) {
	root := project(t, twoRepositories, map[string]string{"README.md": "readme"})
	mustInitialize(t, root)

	if _, err := git.Run(root, "--git-dir="+gitDirectory(root, "public"), "config", "user.name", "Ada"); err != nil {
		t.Fatal(err)
	}
	if got := run(t, root, "public", "config", "user.name"); got != "Ada" {
		t.Fatalf("user.name = %q, want Ada", got)
	}

	hook := filepath.Join(gitDirectory(root, "public"), "hooks", "pre-commit")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := run(t, root, "public", "rev-parse", "--git-path", "hooks/pre-commit"); got != hook {
		t.Fatalf("hooks path = %q, want %q", got, hook)
	}
	if _, err := git.Run(root, "--git-dir="+gitDirectory(root, "public"), "hook", "run", "pre-commit"); err == nil {
		t.Fatal("pre-commit hook did not run through the command runner")
	}
}

func TestInitKeepsInternalStatePrivate(t *testing.T) {
	root := project(t, twoRepositories, map[string]string{"README.md": "readme"})

	mustInitialize(t, root)

	info, err := os.Stat(filepath.Join(root, ".gitone"))
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o700 {
		t.Fatalf(".gitone mode = %o, want 700", mode)
	}
}

func readConfig(t *testing.T, gitDirectory, key string) string {
	t.Helper()
	output, err := git.Run(filepath.Dir(gitDirectory), "--git-dir="+gitDirectory, "config", key)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(output)
}

func host(t *testing.T) string {
	t.Helper()
	name, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	return name
}

// aliasProject commits src/alias with the given stored target into the public
// repository and returns the matcher, the project root and the commit.
func aliasProject(t *testing.T, target string) (*config.Matcher, string, string) {
	t.Helper()
	root := project(t, twoRepositories, map[string]string{
		"src/lib/a.go":    "package lib",
		"secrets/key.txt": "secret",
	})
	mustInitialize(t, root)
	if err := os.Symlink(target, filepath.Join(root, "src", "alias")); err != nil {
		t.Fatal(err)
	}
	base := []string{"--git-dir=" + gitDirectory(root, "public"), "--work-tree=" + root,
		"-c", "user.name=GitOne", "-c", "user.email=gitone@example.com"}
	for _, arguments := range [][]string{
		{"add", "--", "src/lib/a.go", "src/alias"}, {"commit", "-qm", "alias"},
	} {
		if _, err := git.Run(root, append(append([]string{}, base...), arguments...)...); err != nil {
			t.Fatal(err)
		}
	}
	configuration, discovered, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	matcher, err := configuration.Matcher()
	if err != nil {
		t.Fatal(err)
	}
	return matcher, discovered, run(t, root, "public", "rev-parse", "HEAD")
}

// linkIssues is what the tree and the index of the public repository report,
// which must be the same decision from the same policy.
func linkIssues(t *testing.T, matcher *config.Matcher, root, commit string) (tree, index []repository.TreeIssue) {
	t.Helper()
	tree, err := repository.TreeIssues(matcher, root, map[string]string{"public": commit})
	if err != nil {
		t.Fatal(err)
	}
	index, err = repository.IndexLinkIssues(matcher, root, map[string]string{
		"public": repository.IndexPath(root, "public"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return tree, index
}

func TestTreeAndIndexRejectDirectoryAliasAcrossRepositories(t *testing.T) {
	root := project(t, `
version: 1
default_branch: main
repositories:
  private:
    visibility: private
    paths:
      - shared/private/**
  public:
    visibility: public
    paths:
      - .gitignore
      - .gitone.yml
      - links/**
      - shared/public/**
`, map[string]string{
		"shared/private/secret.txt": "private",
		"shared/public/readme.txt":  "public",
	})
	mustInitialize(t, root)
	if err := os.MkdirAll(filepath.Join(root, "links"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../shared", filepath.Join(root, "links", "alias")); err != nil {
		t.Fatal(err)
	}
	for name, paths := range map[string][]string{
		"private": {"shared/private/secret.txt"},
		"public":  {"links/alias", "shared/public/readme.txt"},
	} {
		base := []string{"--git-dir=" + gitDirectory(root, name), "--work-tree=" + root,
			"-c", "user.name=GitOne", "-c", "user.email=gitone@example.com"}
		if _, err := git.Run(root, append(append([]string{}, base...), append([]string{"add", "--"}, paths...)...)...); err != nil {
			t.Fatal(err)
		}
		if _, err := git.Run(root, append(append([]string{}, base...), "commit", "-qm", "content")...); err != nil {
			t.Fatal(err)
		}
	}
	configuration, discovered, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	matcher, err := configuration.Matcher()
	if err != nil {
		t.Fatal(err)
	}
	tree, err := repository.TreeIssues(matcher, discovered, map[string]string{
		"private": run(t, root, "private", "rev-parse", "HEAD"),
		"public":  run(t, root, "public", "rev-parse", "HEAD"),
	})
	if err != nil {
		t.Fatal(err)
	}
	index, err := repository.IndexLinkIssues(matcher, discovered, map[string]string{
		"private": repository.IndexPath(root, "private"),
		"public":  repository.IndexPath(root, "public"),
	})
	if err != nil {
		t.Fatal(err)
	}
	for label, issues := range map[string][]repository.TreeIssue{"tree": tree, "index": index} {
		if len(issues) != 1 || issues[0].Repository != "public" || issues[0].Path != "links/alias" ||
			!strings.Contains(issues[0].Reason, "shared/private/secret.txt belongs to private, not public") {
			t.Fatalf("%s = %v, want the cross-repository directory alias refused", label, issues)
		}
	}
}

func TestTreeAndIndexAcceptADirectoryAlias(t *testing.T) {
	matcher, root, commit := aliasProject(t, "lib")

	tree, index := linkIssues(t, matcher, root, commit)

	if len(tree) != 0 || len(index) != 0 {
		t.Fatalf("tree = %v, index = %v, want no issues", tree, index)
	}
}

func TestTreeAndIndexRefuseUnsafeLinks(t *testing.T) {
	for target, reason := range map[string]string{
		"/etc/passwd":    "absolute link targets are not supported",
		"../../outside":  "the link target escapes the project root",
		"missing":        "the link target src/missing does not exist",
		"../secrets":     "the link target secrets does not exist",
		"../.gitone.yml": "GitOne configuration, ignore and Git metadata paths must stay regular files",
		"alias":          "the link target src/alias is a symbolic link: chains and cycles are not supported",
	} {
		t.Run(target, func(t *testing.T) {
			matcher, root, commit := aliasProject(t, target)

			tree, index := linkIssues(t, matcher, root, commit)

			for label, issues := range map[string][]repository.TreeIssue{"tree": tree, "index": index} {
				if len(issues) != 1 || issues[0].Path != "src/alias" ||
					!strings.Contains(issues[0].Reason, "symbolic link to "+target+": "+reason) {
					t.Fatalf("%s = %v, want src/alias refused with %q", label, issues, reason)
				}
			}
		})
	}
}
