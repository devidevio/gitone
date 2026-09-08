package fetch_test

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/devidevio/gitone/internal/config"
	"github.com/devidevio/gitone/internal/fetch"
	"github.com/devidevio/gitone/internal/git"
	"github.com/devidevio/gitone/internal/lock"
	"github.com/devidevio/gitone/internal/push"
	"github.com/devidevio/gitone/internal/repository"
	"github.com/devidevio/gitone/internal/stage"
	"github.com/devidevio/gitone/internal/status"
)

// project creates an initialized project whose repositories fetch from local
// bare repositories. An override replaces the configured remote of one
// repository; the empty string leaves it without any configured remote.
func project(t *testing.T, override map[string]string) (*config.Config, string, map[string]string) {
	t.Helper()
	for _, name := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(name, "GitOne")
	}
	for _, name := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(name, "gitone@example.com")
	}

	root := temporary(t)
	origins := temporary(t)
	remotes := map[string]string{}
	for _, name := range []string{"private", "public"} {
		directory := filepath.Join(origins, name+".git")
		if _, err := git.Run(origins, "init", "--bare", "--quiet", "--initial-branch=main", directory); err != nil {
			t.Fatal(err)
		}
		remotes[name] = directory
	}

	configured := map[string]string{}
	for name, directory := range remotes {
		if replacement, exists := override[name]; exists {
			directory = replacement
		}
		if directory != "" {
			configured[name] = "remote: " + directory
		}
	}
	write(t, root, ".gitone.yml", fmt.Sprintf(`version: 1
default_branch: main
repositories:
  private:
    %s
    visibility: private
    paths:
      - secrets/**
  public:
    %s
    visibility: public
    paths:
      - .gitignore
      - .gitone.yml
      - README.md
      - src/**
`, configured["private"], configured["public"]))

	loaded, discovered, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.Init(loaded, discovered); err != nil {
		t.Fatal(err)
	}
	return loaded, discovered, remotes
}

func temporary(t *testing.T) string {
	t.Helper()
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return directory
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

// advance adds one commit to a bare repository the way an external
// contributor would, through a separate clone of it.
func advance(t *testing.T, bare, name, contents string) string {
	t.Helper()
	clone := temporary(t)
	if _, err := git.Run(clone, "clone", "--quiet", bare, clone); err != nil {
		t.Fatal(err)
	}
	write(t, clone, name, contents)
	for _, arguments := range [][]string{{"add", name}, {"commit", "--quiet", "-m", "External " + name},
		{"push", "--quiet", "origin", "HEAD:refs/heads/main"}} {
		if _, err := git.Run(clone, arguments...); err != nil {
			t.Fatal(err)
		}
	}
	return remoteHead(t, bare)
}

// remoteHead is the commit a bare repository holds on main.
func remoteHead(t *testing.T, bare string) string {
	t.Helper()
	output, err := git.Run(bare, "--git-dir="+bare, "for-each-ref", "--format=%(objectname)", "refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(output)
}

// tracking is the commit refs/remotes/origin/main points at in one managed
// repository, empty while that ref does not exist.
func tracking(t *testing.T, root, name string) string {
	t.Helper()
	output, err := git.Run(root, "--git-dir="+repository.Directory(root, name),
		"for-each-ref", "--format=%(objectname)", "refs/remotes/origin/main")
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(output)
}

func run(t *testing.T, loaded *config.Config, root, target string) string {
	t.Helper()
	var output bytes.Buffer
	if err := fetch.Fetch(loaded, root, target, nil, &output); err != nil {
		t.Fatalf("fetch %q: %v\noutput: %s", target, err, output.String())
	}
	return output.String()
}

// fails runs a fetch that must fail and returns its error message.
func fails(t *testing.T, loaded *config.Config, root, target string) string {
	t.Helper()
	var output bytes.Buffer
	err := fetch.Fetch(loaded, root, target, nil, &output)
	if err == nil {
		t.Fatalf("fetch %q succeeded, output = %q", target, output.String())
	}
	return err.Error()
}

func TestFetchUpdatesEveryConfiguredRepository(t *testing.T) {
	loaded, root, remotes := project(t, nil)
	heads := map[string]string{
		"private": advance(t, remotes["private"], "secrets/key.txt", "secret\n"),
		"public":  advance(t, remotes["public"], "README.md", "hello\n"),
	}

	output := run(t, loaded, root, "")
	for name, head := range heads {
		if got := tracking(t, root, name); got != head {
			t.Fatalf("%s origin/main = %q, want %q", name, got, head)
		}
	}
	if strings.Count(output, "updated") != 2 {
		t.Fatalf("output = %q, want both repositories updated", output)
	}

	// A repeated fetch changes nothing and says so.
	if again := run(t, loaded, root, "all"); !strings.Contains(again, "already up to date") || strings.Contains(again, "updated") {
		t.Fatalf("second fetch = %q, want every repository up to date", again)
	}
}

func TestFetchRepositoryUpdatesOnlyThatRepository(t *testing.T) {
	loaded, root, remotes := project(t, nil)
	advance(t, remotes["private"], "secrets/key.txt", "secret\n")
	head := advance(t, remotes["public"], "README.md", "hello\n")

	run(t, loaded, root, "public")
	if got := tracking(t, root, "public"); got != head {
		t.Fatalf("public origin/main = %q, want %q", got, head)
	}
	if got := tracking(t, root, "private"); got != "" {
		t.Fatalf("private origin/main = %q, want no ref", got)
	}
}

func TestFetchRejectsUnknownRepositoryBeforeFetching(t *testing.T) {
	loaded, root, remotes := project(t, nil)
	advance(t, remotes["public"], "README.md", "hello\n")

	if message := fails(t, loaded, root, "website"); !strings.HasPrefix(message, "REPO001 ") {
		t.Fatalf("error = %q, want REPO001", message)
	}
	if got := tracking(t, root, "public"); got != "" {
		t.Fatalf("public origin/main = %q, want no ref", got)
	}
}

func TestFetchRejectsChangedOriginBeforeFetching(t *testing.T) {
	loaded, root, remotes := project(t, nil)
	advance(t, remotes["private"], "secrets/key.txt", "secret\n")
	if _, err := git.Run(root, "--git-dir="+repository.Directory(root, "public"),
		"remote", "set-url", "origin", filepath.Join(temporary(t), "other.git")); err != nil {
		t.Fatal(err)
	}

	if message := fails(t, loaded, root, ""); !strings.HasPrefix(message, "REPO001 ") || !strings.Contains(message, "origin") {
		t.Fatalf("error = %q, want REPO001 about origin", message)
	}
	if got := tracking(t, root, "private"); got != "" {
		t.Fatalf("private origin/main = %q, want no ref", got)
	}
}

func TestFetchSkipsLocalOnlyRepositoriesAndRejectsThemAsTarget(t *testing.T) {
	loaded, root, remotes := project(t, map[string]string{"private": ""})
	head := advance(t, remotes["public"], "README.md", "hello\n")

	output := run(t, loaded, root, "")
	if !strings.Contains(output, "private") || !strings.Contains(output, "skipped") {
		t.Fatalf("output = %q, want private reported as skipped", output)
	}
	if got := tracking(t, root, "public"); got != head {
		t.Fatalf("public origin/main = %q, want %q", got, head)
	}

	message := fails(t, loaded, root, "private")
	if !strings.HasPrefix(message, "REPO001 ") || !strings.Contains(message, "origin") {
		t.Fatalf("error = %q, want REPO001 about the missing origin", message)
	}
}

func TestFetchAllowsDirtyWorkingTree(t *testing.T) {
	loaded, root, remotes := project(t, nil)
	head := advance(t, remotes["public"], "README.md", "hello\n")
	write(t, root, "README.md", "local\n")
	write(t, root, "src/site.css", "body {}\n")
	var staged bytes.Buffer
	if err := stage.Add(loaded, root, root, []string{"src/site.css"}, &staged); err != nil {
		t.Fatal(err)
	}

	run(t, loaded, root, "")
	if got := tracking(t, root, "public"); got != head {
		t.Fatalf("public origin/main = %q, want %q", got, head)
	}
	if contents, err := os.ReadFile(filepath.Join(root, "README.md")); err != nil || string(contents) != "local\n" {
		t.Fatalf("README.md = %q, %v, want the local content", contents, err)
	}
}

func TestFetchStopsOnUnsafeWorkingTreeBeforeAnyNetworkAccess(t *testing.T) {
	loaded, root, remotes := project(t, nil)
	advance(t, remotes["public"], "README.md", "hello\n")
	write(t, root, "unassigned.txt", "orphan\n")

	if message := fails(t, loaded, root, ""); !strings.Contains(message, "PATH001") {
		t.Fatalf("error = %q, want PATH001", message)
	}
	if got := tracking(t, root, "public"); got != "" {
		t.Fatalf("public origin/main = %q, want no ref", got)
	}
}

func TestFetchLeavesLocalStateUnchanged(t *testing.T) {
	loaded, root, remotes := project(t, nil)
	write(t, root, "README.md", "hello\n")
	var output bytes.Buffer
	if err := stage.Add(loaded, root, root, []string{"README.md", ".gitone.yml"}, &output); err != nil {
		t.Fatal(err)
	}
	if err := stage.Commit(loaded, root, root, nil, "Add files", &output); err != nil {
		t.Fatal(err)
	}
	advance(t, remotes["public"], "CONTRIBUTING.md", "contribute\n")

	before := localState(t, root, "public")
	run(t, loaded, root, "")
	if after := localState(t, root, "public"); after != before {
		t.Fatalf("local state changed:\n%s\nwant:\n%s", after, before)
	}
}

func TestFetchIgnoresUnsafeFetchConfiguration(t *testing.T) {
	loaded, root, remotes := project(t, nil)
	first := advance(t, remotes["public"], "README.md", "first\n")
	run(t, loaded, root, "public")
	directory := repository.Directory(root, "public")
	if _, err := git.Run(root, "--git-dir="+directory, "update-ref", "refs/remotes/origin/stale", first); err != nil {
		t.Fatal(err)
	}
	second := advance(t, remotes["public"], "README.md", "second\n")
	for _, arguments := range [][]string{
		{"config", "remote.origin.fetch", "+refs/heads/*:refs/heads/*"},
		{"config", "remote.origin.tagOpt", "--tags"},
		{"config", "fetch.prune", "true"},
	} {
		if _, err := git.Run(root, append([]string{"--git-dir=" + directory}, arguments...)...); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := git.Run(remotes["public"], "--git-dir="+remotes["public"], "tag", "v1", second); err != nil {
		t.Fatal(err)
	}

	run(t, loaded, root, "public")
	if got := tracking(t, root, "public"); got != second {
		t.Fatalf("public origin/main = %q, want %q", got, second)
	}
	refs, err := git.Run(root, "--git-dir="+directory, "for-each-ref", "--format=%(refname)",
		"refs/heads/", "refs/tags/", "refs/remotes/origin/stale")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(refs); got != "refs/remotes/origin/stale" {
		t.Fatalf("protected refs = %q, want only the unpruned stale origin ref", got)
	}
}

// localState is everything a fetch must never touch: HEAD, the current
// branch, the index bytes and the working-tree files.
func localState(t *testing.T, root, name string) string {
	t.Helper()
	output, err := git.Run(root, "--git-dir="+repository.Directory(root, name), "--work-tree="+root,
		"for-each-ref", "--format=%(refname) %(objectname)", "refs/heads/")
	if err != nil {
		t.Fatal(err)
	}
	head, err := os.ReadFile(filepath.Join(repository.Directory(root, name), "HEAD"))
	if err != nil {
		t.Fatal(err)
	}
	index, err := os.ReadFile(filepath.Join(repository.Directory(root, name), "index"))
	if err != nil {
		t.Fatal(err)
	}
	state := fmt.Sprintf("refs %s\nHEAD %s\nindex %x\n", output, head, index)
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		contents, err := os.ReadFile(filepath.Join(root, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		state += fmt.Sprintf("%s %x\n", entry.Name(), contents)
	}
	return state
}

func TestFetchReportsEarlierSuccessesWhenARepositoryFails(t *testing.T) {
	missing := filepath.Join(temporary(t), "gone.git")
	loaded, root, remotes := project(t, map[string]string{"public": missing})
	head := advance(t, remotes["private"], "secrets/key.txt", "secret\n")

	var output bytes.Buffer
	err := fetch.Fetch(loaded, root, "", nil, &output)
	if err == nil {
		t.Fatalf("fetch succeeded, output = %q", output.String())
	}
	message := err.Error()
	if !strings.HasPrefix(message, "FETCH001 ") || !strings.Contains(message, "public") {
		t.Fatalf("error = %q, want FETCH001 naming public", message)
	}
	if !strings.Contains(message, "remain updated") {
		t.Fatalf("error = %q, want the kept remote-tracking refs stated", message)
	}
	if !strings.Contains(output.String(), "updated") || !strings.Contains(output.String(), "failed") {
		t.Fatalf("report = %q, want private updated and public failed", output.String())
	}
	// The refs the successful repository received are never rolled back.
	if got := tracking(t, root, "private"); got != head {
		t.Fatalf("private origin/main = %q, want %q", got, head)
	}
}

func TestFetchIsRejectedWhileAnotherOperationRuns(t *testing.T) {
	loaded, root, _ := project(t, nil)
	release, err := lock.Acquire(root, "gitone commit")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	if message := fails(t, loaded, root, ""); !strings.HasPrefix(message, "LOCK001 ") {
		t.Fatalf("error = %q, want LOCK001", message)
	}
}

func TestFetchUpdatesAheadBehindReporting(t *testing.T) {
	loaded, root, remotes := project(t, nil)
	write(t, root, "README.md", "hello\n")
	var output bytes.Buffer
	if err := stage.Add(loaded, root, root, []string{"README.md", ".gitone.yml"}, &output); err != nil {
		t.Fatal(err)
	}
	if err := stage.Commit(loaded, root, root, nil, "Add files", &output); err != nil {
		t.Fatal(err)
	}
	if err := push.Push(loaded, root, "public", true, false, nil, &output); err != nil {
		t.Fatal(err)
	}
	advance(t, remotes["public"], "CONTRIBUTING.md", "contribute\n")

	run(t, loaded, root, "public")
	result, err := status.Collect(loaded, root)
	if err != nil {
		t.Fatal(err)
	}
	for _, reported := range result.Repositories {
		if reported.Name != "public" {
			continue
		}
		if reported.Behind == nil || *reported.Behind != 1 {
			t.Fatalf("public behind = %v, want 1", reported.Behind)
		}
		return
	}
	t.Fatal("public was not reported")
}
