package worktree_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/devidevio/gitone/internal/config"
	"github.com/devidevio/gitone/internal/worktree"
)

const twoRepositories = `
version: 1
default_branch: main
repositories:
  private:
    visibility: private
    paths:
      - tasks/**
  public:
    visibility: public
    paths:
      - .gitignore
      - .gitone.yml
      - README.md
      - src/**
`

func TestScanAssignsEveryRelevantPath(t *testing.T) {
	root := project(t, twoRepositories, map[string]string{
		".gitignore":     "node_modules/\n",
		"README.md":      "readme",
		"src/main.go":    "package main",
		"src/a/b/c.go":   "package c",
		"tasks/0001.md":  "task",
		"node_modules/x": "ignored",
	})

	result := scan(t, root, nil)

	assertNoIssues(t, result)
	want := map[string]string{
		".gitignore":    "public",
		".gitone.yml":   "public",
		"README.md":     "public",
		"src/main.go":   "public",
		"src/a/b/c.go":  "public",
		"tasks/0001.md": "private",
	}
	if !reflect.DeepEqual(result.Owners, want) {
		t.Fatalf("owners = %#v, want %#v", result.Owners, want)
	}
}

func TestScanReportsUnassignedPath(t *testing.T) {
	root := project(t, twoRepositories, map[string]string{"random.txt": "x"})

	assertIssue(t, scan(t, root, nil), "PATH001", "random.txt")
}

func TestScanRejectsProtectedRelevantAndTrackedPaths(t *testing.T) {
	root := project(t, `
version: 1
default_branch: main
rules:
  protected_paths:
    - src/private.key
    - secrets/**
repositories:
  public:
    visibility: public
    paths:
      - .gitignore
      - .gitone.yml
      - src/**
`, map[string]string{
		".gitignore":      "secrets/\n",
		"src/private.key": "visible",
		"secrets/key":     "ignored but tracked",
	})

	result := scan(t, root, []string{"secrets/key"})

	assertIssue(t, result, "PATH003", "src/private.key")
	assertIssue(t, result, "PATH003", "secrets/key")
}

func TestScanReportsAmbiguousPath(t *testing.T) {
	root := project(t, twoRepositories, map[string]string{"src/internal/a.go": "package a"})
	overlapping := &config.Config{Version: 1, Repositories: map[string]config.Repository{
		"public":  {Visibility: "public", Paths: []string{"src/**"}},
		"private": {Visibility: "private", Paths: []string{"src/internal/**"}},
	}}

	result, err := worktree.Scan(overlapping, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertIssue(t, result, "PATH002", "src/internal/a.go")
}

func TestScanResolvesGlobPatterns(t *testing.T) {
	// One repository may claim a path through several patterns; the other one
	// overlaps it only theoretically until a concrete path matches both.
	root := project(t, `
version: 1
default_branch: main
repositories:
  private:
    visibility: private
    paths:
      - "**/*.secret"
  public:
    visibility: public
    paths:
      - "*"
      - docs/**/*.md
      - docs/**
`, map[string]string{
		".gitignore":       "",
		"README.md":        "readme",
		"docs/guide.md":    "guide",
		"docs/a/b/deep.md": "deep",
		"docs/a/notes.txt": "notes",
	})

	result := scan(t, root, nil)

	assertNoIssues(t, result)
	want := map[string]string{
		".gitignore":       "public",
		".gitone.yml":      "public",
		"README.md":        "public",
		"docs/guide.md":    "public",
		"docs/a/b/deep.md": "public",
		"docs/a/notes.txt": "public",
	}
	if !reflect.DeepEqual(result.Owners, want) {
		t.Fatalf("owners = %#v, want %#v", result.Owners, want)
	}

	ambiguous := project(t, `
version: 1
default_branch: main
repositories:
  private:
    visibility: private
    paths:
      - "**/*.secret"
  public:
    visibility: public
    paths:
      - "*"
      - .gitone.yml
      - docs/**
`, map[string]string{
		".gitignore":       "",
		"docs/keys.secret": "shh",
	})
	assertIssue(t, scan(t, ambiguous, nil), "PATH002", "docs/keys.secret")
}

func TestScanFailsClosedOnUnusablePattern(t *testing.T) {
	root := project(t, twoRepositories, map[string]string{"src/main.go": "package main"})
	broken := &config.Config{Version: 1, Repositories: map[string]config.Repository{
		"public": {Visibility: "public", Paths: []string{"src/a**.go"}},
	}}

	if _, err := worktree.Scan(broken, root, nil); err == nil {
		t.Fatal("Scan accepted an unusable pattern instead of failing closed")
	}
}

// aliasRepositories owns the motivating shape: the alias .claude/skills and
// its real target .agents/skills belong to the same repository.
const aliasRepositories = `
version: 1
default_branch: main
repositories:
  private:
    visibility: private
    paths:
      - tasks/**
  public:
    visibility: public
    paths:
      - .gitignore
      - .gitone.yml
      - .agents/**
      - .claude/skills
      - src/**
`

func TestScanAcceptsDirectoryAliasAsOneManagedPath(t *testing.T) {
	root := project(t, aliasRepositories, map[string]string{".agents/skills/plan.md": "skill"})
	symlink(t, root, "../.agents/skills", ".claude/skills")

	result := scan(t, root, nil)

	assertNoIssues(t, result)
	// Only the lexical link path is managed; nothing below the alias is.
	if result.Owners[".claude/skills"] != "public" {
		t.Fatalf("owners = %#v, want .claude/skills owned by public", result.Owners)
	}
	if _, invented := result.Owners[".claude/skills/plan.md"]; invented {
		t.Fatal("the alias was traversed to invent a managed path below it")
	}
}

func TestScanAcceptsFileAliasWithTheLinkOwner(t *testing.T) {
	root := project(t, aliasRepositories, map[string]string{".agents/skills/plan.md": "skill"})
	symlink(t, root, "../.agents/skills/plan.md", "src/plan.md")

	result := scan(t, root, nil)

	assertNoIssues(t, result)
	if result.Owners["src/plan.md"] != "public" {
		t.Fatalf("owners = %#v, want src/plan.md owned by public", result.Owners)
	}
}

func TestScanRejectsEveryUnsafeLinkClass(t *testing.T) {
	root := project(t, aliasRepositories, map[string]string{
		".agents/skills/plan.md": "skill",
		"tasks/0001.md":          "task",
		"src/vendor/x.go":        "package vendor",
	})
	links := map[string]string{
		"src/absolute":   "/etc/passwd",
		"src/escaping":   "../../outside",
		"src/dangling":   "missing.go",
		"src/empty":      "",
		"src/crossing":   "../tasks/0001.md",
		"src/control":    "../.gitone.yml",
		"src/ignore":     "../.gitignore",
		"src/chained":    "absolute",
		"src/cycle":      "cycle",
		"src/emptyalias": "vendor/empty",
	}
	// Linux refuses symlink(2) with an empty target, so that class cannot
	// reach a working tree there at all - not through a checkout either, since
	// Git creates links with the same call. Drop the one class the kernel will
	// not make rather than skipping the test: the other nine still run, and on
	// a system that allows it so does this one.
	if err := os.Symlink("", filepath.Join(t.TempDir(), "empty")); err != nil {
		delete(links, "src/empty")
	}
	for name, target := range links {
		symlink(t, root, target, name)
	}
	if err := os.MkdirAll(filepath.Join(root, "src", "vendor", "empty"), 0o755); err != nil {
		t.Fatal(err)
	}

	result := scan(t, root, nil)

	for name, target := range links {
		assertIssue(t, result, "PATH003", name)
		if !slices.ContainsFunc(result.Issues, func(issue *worktree.Issue) bool {
			return issue.Path == name && strings.Contains(issue.Detail, "symbolic link to "+target+":")
		}) {
			t.Fatalf("issue for %s does not name the stored target %q: %v", name, target, details(result))
		}
	}
}

func TestScanRejectsDirectoryAliasWithAForeignDescendant(t *testing.T) {
	root := project(t, aliasRepositories, map[string]string{
		".agents/skills/plan.md": "skill",
		"tasks/0001.md":          "task",
	})
	symlink(t, root, "../tasks", ".claude/skills")

	assertIssue(t, scan(t, root, nil), "PATH003", ".claude/skills")
}

func TestScanRejectsLinksToOrThroughProtectedDirectories(t *testing.T) {
	root := project(t, `
version: 1
default_branch: main
rules:
  protected_paths:
    - src/parent
    - src/protected
repositories:
  public:
    visibility: public
    paths:
      - .gitignore
      - .gitone.yml
      - src/**
`, map[string]string{
		"src/parent/target/a.go": "package target",
		"src/protected/a.go":     "package protected",
	})
	symlink(t, root, "protected", "src/exact")
	symlink(t, root, "parent/target", "src/through")

	result := scan(t, root, nil)

	assertIssue(t, result, "PATH003", "src/exact")
	assertIssue(t, result, "PATH003", "src/through")
}

func TestScanRejectsDirectoryAliasWithOnlyIgnoredContent(t *testing.T) {
	root := project(t, aliasRepositories, map[string]string{
		".gitignore":            "node_modules/\n",
		"node_modules/pkg/a.js": "ignored",
	})
	symlink(t, root, "../node_modules", ".claude/skills")

	assertIssue(t, scan(t, root, nil), "PATH003", ".claude/skills")
}

func TestScanRejectsSymlinkOutsideIgnoredTree(t *testing.T) {
	root := project(t, twoRepositories, map[string]string{".gitignore": "node_modules/\n"})
	symlink(t, root, "src/target", "src/link")
	symlink(t, root, "/etc/passwd", "src/escape")

	result := scan(t, root, nil)

	assertIssue(t, result, "PATH003", "src/link")
	assertIssue(t, result, "PATH003", "src/escape")
}

func TestScanIgnoresSymlinkAndNestedRepositoryInsideIgnoredTree(t *testing.T) {
	root := project(t, twoRepositories, map[string]string{
		".gitignore":            "node_modules/\n",
		"node_modules/pkg/a.js": "ignored",
	})
	symlink(t, root, "/etc", "node_modules/escape")
	initRepository(t, filepath.Join(root, "node_modules", "nested"))
	initBareRepository(t, filepath.Join(root, "node_modules", "bare.git"))

	assertNoIssues(t, scan(t, root, nil))
}

func TestScanRejectsNestedRepository(t *testing.T) {
	root := project(t, twoRepositories, nil)
	initRepository(t, filepath.Join(root, "src", "vendor"))

	assertIssue(t, scan(t, root, nil), "PATH003", "src/vendor")
}

func TestScanRejectsNestedBareRepository(t *testing.T) {
	root := project(t, twoRepositories, nil)
	initBareRepository(t, filepath.Join(root, "src", "vendor.git"))

	assertIssue(t, scan(t, root, nil), "PATH003", "src/vendor.git")
}

func TestScanRejectsRootRepository(t *testing.T) {
	root := project(t, twoRepositories, nil)
	initRepository(t, root)

	assertIssue(t, scan(t, root, nil), "PATH003", ".git")
}

func TestScanRejectsRootBareRepository(t *testing.T) {
	root := project(t, twoRepositories, nil)
	initBareRepository(t, root)

	assertIssue(t, scan(t, root, nil), "PATH003", ".")
}

func TestScanRejectsCaseCollision(t *testing.T) {
	root := project(t, twoRepositories, map[string]string{"src/main.go": "package main"})

	// A tracked path that differs only in case collides with the file on disk
	// on both case-sensitive and case-insensitive filesystems.
	result := scan(t, root, []string{"src/Main.go"})

	assertIssue(t, result, "PATH003", "src/Main.go")
	assertIssue(t, result, "PATH003", "src/main.go")
}

func TestScanRejectsCaseCollisionInDirectoryComponent(t *testing.T) {
	root := project(t, twoRepositories, map[string]string{"src/Dir/a.txt": "a"})

	result := scan(t, root, []string{"src/dir/b.txt"})

	assertIssue(t, result, "PATH003", "src/Dir/a.txt")
	assertIssue(t, result, "PATH003", "src/dir/b.txt")
}

func TestScanRejectsUnicodeCaseCollision(t *testing.T) {
	root := project(t, twoRepositories, map[string]string{"src/Σ.txt": "sigma"})

	result := scan(t, root, []string{"src/ς.txt"})

	assertIssue(t, result, "PATH003", "src/Σ.txt")
	assertIssue(t, result, "PATH003", "src/ς.txt")
}

func TestScanRejectsPathMatchingOnlyWhenCaseIsIgnored(t *testing.T) {
	root := project(t, twoRepositories, map[string]string{"Readme.md": "readme"})

	assertIssue(t, scan(t, root, nil), "PATH003", "Readme.md")
}

func TestScanIgnoresGlobalGitExcludes(t *testing.T) {
	root := project(t, twoRepositories, map[string]string{"random.txt": "x"})
	excludes := filepath.Join(t.TempDir(), "excludes")
	write(t, filepath.Dir(excludes), "excludes", "random.txt\n")
	globalConfig := filepath.Join(t.TempDir(), "config")
	write(t, filepath.Dir(globalConfig), "config", "[core]\n\texcludesFile = "+excludes+"\n")
	t.Setenv("GIT_CONFIG_GLOBAL", globalConfig)

	assertIssue(t, scan(t, root, nil), "PATH001", "random.txt")
}

func TestScanRequiresOwnershipForTrackedIgnoredPath(t *testing.T) {
	root := project(t, twoRepositories, map[string]string{
		".gitignore":     "node_modules/\n",
		"node_modules/x": "tracked but ignored",
	})

	assertIssue(t, scan(t, root, []string{"node_modules/x"}), "PATH001", "node_modules/x")
}

func TestScanRejectsHiddenIgnoreFile(t *testing.T) {
	root := project(t, twoRepositories, map[string]string{
		".gitignore":      "src/.gitignore\n",
		"src/.gitignore":  "*.key\n",
		"src/main.go":     "package main",
		"src/private.key": "secret",
	})

	assertIssue(t, scan(t, root, nil), "PATH003", "src/.gitignore")
}

func TestScanRejectsIgnoreFileHidingItsWholeDirectory(t *testing.T) {
	root := project(t, twoRepositories, map[string]string{
		".gitignore":                  "node_modules/\n",
		"src/vendor/.gitignore":       "*\n",
		"src/vendor/hidden.key":       "secret",
		"node_modules/pkg/.gitignore": "*\n",
	})

	result := scan(t, root, nil)

	// The directory keeps no visible file at all, yet its ignore file must not
	// hide content silently. An ignore file inside an ignored tree stays
	// irrelevant.
	assertIssue(t, result, "PATH003", "src/vendor/.gitignore")
	if slices.Contains(issues(result), "PATH003 node_modules/pkg/.gitignore") {
		t.Fatalf("issues = %v, want no issue inside the ignored tree", issues(result))
	}
}

func TestScanNeverOwnsReservedPaths(t *testing.T) {
	root := project(t, twoRepositories, map[string]string{
		".gitone.local.yml":             "version: 1\n",
		".gitone/repositories/keep.txt": "state",
	})
	initRepository(t, filepath.Join(root, ".gitone", "repositories", "public"))
	initBareRepository(t, filepath.Join(root, ".gitone", "repositories", "private"))

	result := scan(t, root, nil)

	assertNoIssues(t, result)
	for owned := range result.Owners {
		if config.IsReserved(owned) {
			t.Fatalf("reserved path %q was assigned to %q", owned, result.Owners[owned])
		}
	}
}

func TestScanRejectsTrackedReservedPath(t *testing.T) {
	root := project(t, twoRepositories, nil)

	assertIssue(t, scan(t, root, []string{".gitone.local.yml"}), "PATH003", ".gitone.local.yml")
}

func TestRelative(t *testing.T) {
	root := t.TempDir()
	for _, testCase := range []struct {
		directory string
		argument  string
		want      string
	}{
		{root, "src/main.go", "src/main.go"},
		{root, "./src/../src/main.go", "src/main.go"},
		{filepath.Join(root, "src"), "main.go", "src/main.go"},
		{filepath.Join(root, "src"), "../tasks/a.md", "tasks/a.md"},
		{root, ".", "."},
	} {
		got, err := worktree.Relative(root, testCase.directory, testCase.argument)
		if err != nil || got != testCase.want {
			t.Fatalf("Relative(%q, %q) = %q, %v, want %q", testCase.directory, testCase.argument, got, err, testCase.want)
		}
	}

	for _, testCase := range []struct{ directory, argument string }{
		{root, "../outside.txt"},
		{root, "src/../../outside.txt"},
		{root, "/etc/passwd"},
		{root, ""},
		{root, "src/\x01.go"},
		{root, ".gitone.local.yml"},
		{root, ".gitone/repositories/public"},
		{filepath.Join(root, "src"), "../../outside.txt"},
	} {
		got, err := worktree.Relative(root, testCase.directory, testCase.argument)
		if err == nil {
			t.Fatalf("Relative(%q, %q) = %q, want an error", testCase.directory, testCase.argument, got)
		}
		if code := err.(*worktree.Issue).Code; code != "PATH003" {
			t.Fatalf("Relative(%q, %q) code = %q, want PATH003", testCase.directory, testCase.argument, code)
		}
	}
}

func scan(t *testing.T, root string, tracked []string) *worktree.Result {
	t.Helper()
	configuration, discovered, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	result, err := worktree.Scan(configuration, discovered, tracked)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

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

func symlink(t *testing.T, root, target, name string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, full); err != nil {
		t.Fatal(err)
	}
}

func initRepository(t *testing.T, directory string) {
	t.Helper()
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("git", "init", "--quiet", "--template=", directory)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	write(t, directory, "a.txt", "content")
}

func initBareRepository(t *testing.T, directory string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(directory), 0o755); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("git", "init", "--bare", "--quiet", "--template=", directory)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v: %s", err, output)
	}
}

func assertNoIssues(t *testing.T, result *worktree.Result) {
	t.Helper()
	if len(result.Issues) != 0 {
		t.Fatalf("issues = %v, want none", issues(result))
	}
}

func assertIssue(t *testing.T, result *worktree.Result, code, relativePath string) {
	t.Helper()
	if !slices.Contains(issues(result), code+" "+relativePath) {
		t.Fatalf("issues = %v, want %s %s", issues(result), code, relativePath)
	}
	if _, owned := result.Owners[relativePath]; owned {
		t.Fatalf("%s was assigned to %q despite %s", relativePath, result.Owners[relativePath], code)
	}
}

func details(result *worktree.Result) []string {
	found := make([]string, 0, len(result.Issues))
	for _, issue := range result.Issues {
		found = append(found, issue.Error())
	}
	return found
}

func issues(result *worktree.Result) []string {
	found := make([]string, 0, len(result.Issues))
	for _, issue := range result.Issues {
		found = append(found, strings.TrimSpace(issue.Code+" "+issue.Path))
	}
	return found
}

func TestScanReportsIgnoredPathsWithoutTrackedOnes(t *testing.T) {
	root := project(t, twoRepositories, map[string]string{
		".gitignore":            "node_modules/\n*.log\n!keep.log\nsrc/build/\n",
		"README.md":             "readme",
		"src/main.go":           "package main",
		"src/app.log":           "log",
		"src/keep.log":          "kept",
		"node_modules/pkg/x.js": "ignored",
		"src/build/out.o":       "ignored",
		"src/build/keep.txt":    "tracked but ignored",
	})

	result := scan(t, root, []string{"src/build/keep.txt"})

	assertNoIssues(t, result)
	// Both directories stay collapsed, the negated keep.log is not ignored,
	// and the tracked file is reported as an exact exception.
	want := []string{"node_modules/", "src/app.log", "src/build/"}
	if !reflect.DeepEqual(result.Ignored, want) {
		t.Fatalf("ignored = %#v, want %#v", result.Ignored, want)
	}
	wantExceptions := []string{"src/build/keep.txt"}
	if !reflect.DeepEqual(result.IgnoreExceptions, wantExceptions) {
		t.Fatalf("ignore exceptions = %#v, want %#v", result.IgnoreExceptions, wantExceptions)
	}
}
