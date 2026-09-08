package pull_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/devidevio/gitone/internal/config"
	"github.com/devidevio/gitone/internal/git"
	"github.com/devidevio/gitone/internal/lock"
	"github.com/devidevio/gitone/internal/pull"
	"github.com/devidevio/gitone/internal/push"
	"github.com/devidevio/gitone/internal/repository"
	"github.com/devidevio/gitone/internal/stage"
)

// project creates an initialized project whose repositories pull from local
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

// seed is a project whose repositories share one commit with their remotes,
// which is the state every pull starts from.
func seed(t *testing.T, override map[string]string) (*config.Config, string, map[string]string) {
	t.Helper()
	loaded, root, remotes := project(t, override)
	write(t, root, "README.md", "hello\n")
	write(t, root, "secrets/key.txt", "secret\n")

	var output bytes.Buffer
	if err := stage.Add(loaded, root, root, []string{"-A"}, &output); err != nil {
		t.Fatal(err)
	}
	if err := stage.Commit(loaded, root, root, nil, "Initial", &output); err != nil {
		t.Fatal(err)
	}
	for name := range remotes {
		// A repository configured without a remote has nothing to push.
		if remote, overridden := override[name]; overridden && remote == "" {
			continue
		}
		if err := push.Push(loaded, root, name, true, false, nil, &output); err != nil {
			t.Fatalf("push %s: %v\noutput: %s", name, err, output.String())
		}
	}
	return loaded, root, remotes
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
	output, err := git.Run(bare, "--git-dir="+bare, "for-each-ref", "--format=%(objectname)", "refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(output)
}

func advanceEmpty(t *testing.T, bare string) string {
	t.Helper()
	clone := temporary(t)
	if _, err := git.Run(clone, "clone", "--quiet", bare, clone); err != nil {
		t.Fatal(err)
	}
	for _, arguments := range [][]string{{"commit", "--quiet", "--allow-empty", "-m", "External empty commit"},
		{"push", "--quiet", "origin", "HEAD:refs/heads/main"}} {
		if _, err := git.Run(clone, arguments...); err != nil {
			t.Fatal(err)
		}
	}
	output, err := git.Run(bare, "--git-dir="+bare, "for-each-ref", "--format=%(objectname)", "refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(output)
}

// head is the commit the local branch of one managed repository points at.
func head(t *testing.T, root, name string) string {
	t.Helper()
	output, err := git.Run(root, "--git-dir="+repository.Directory(root, name),
		"for-each-ref", "--format=%(objectname)", "refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(output)
}

// commit adds one local commit to every repository with staged changes.
func commit(t *testing.T, loaded *config.Config, root, path, contents string) {
	t.Helper()
	write(t, root, path, contents)
	var output bytes.Buffer
	if err := stage.Add(loaded, root, root, []string{"-A"}, &output); err != nil {
		t.Fatal(err)
	}
	if err := stage.Commit(loaded, root, root, nil, "Local "+path, &output); err != nil {
		t.Fatal(err)
	}
}

func run(t *testing.T, loaded *config.Config, root, target string) string {
	t.Helper()
	return accept(t, loaded, root, target, false)
}

// accept runs a pull that must succeed. accepted stands for the
// --accept-config-change flag; no test answers a question interactively.
func accept(t *testing.T, loaded *config.Config, root, target string, accepted bool) string {
	t.Helper()
	var output bytes.Buffer
	if err := pull.Pull(loaded, root, target, accepted, false, false, nil, &output); err != nil {
		t.Fatalf("pull %q: %v\noutput: %s", target, err, output.String())
	}
	return output.String()
}

// failure runs a pull that must fail and returns its error and report.
func failure(t *testing.T, loaded *config.Config, root, target string) (string, string) {
	t.Helper()
	var output bytes.Buffer
	err := pull.Pull(loaded, root, target, false, false, false, nil, &output)
	if err == nil {
		t.Fatalf("pull %q succeeded, output = %q", target, output.String())
	}
	return err.Error(), output.String()
}

func fails(t *testing.T, loaded *config.Config, root, target string) string {
	t.Helper()
	message, _ := failure(t, loaded, root, target)
	return message
}

func contents(t *testing.T, root, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
	if err != nil {
		return ""
	}
	return string(data)
}

// merges counts the merge commits one repository holds, which a pull must
// never create.
func merges(t *testing.T, root, name string) string {
	t.Helper()
	output, err := git.Run(root, "--git-dir="+repository.Directory(root, name), "rev-list", "--count", "--merges", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(output)
}

func recovered(t *testing.T, root string) bool {
	t.Helper()
	_, err := os.Stat(lock.RecoveryPath(root))
	return err == nil
}

func index(t *testing.T, root, name string) []byte {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join(repository.Directory(root, name), "index"))
	if err != nil {
		t.Fatal(err)
	}
	return contents
}

func TestPullFastForwardsEveryRepository(t *testing.T) {
	loaded, root, remotes := seed(t, nil)
	targets := map[string]string{
		"private": advance(t, remotes["private"], "secrets/token.txt", "token\n"),
		"public":  advance(t, remotes["public"], "src/site.css", "body {}\n"),
	}

	output := run(t, loaded, root, "")
	for name, target := range targets {
		if got := head(t, root, name); got != target {
			t.Fatalf("%s main = %q, want %q", name, got, target)
		}
		if got := merges(t, root, name); got != "0" {
			t.Fatalf("%s merge commits = %q, want 0", name, got)
		}
	}
	if got := contents(t, root, "src/site.css"); got != "body {}\n" {
		t.Fatalf("src/site.css = %q, want the pulled content", got)
	}
	if got := contents(t, root, "secrets/token.txt"); got != "token\n" {
		t.Fatalf("secrets/token.txt = %q, want the pulled content", got)
	}
	if strings.Count(output, "updated") != 2 {
		t.Fatalf("output = %q, want both repositories updated", output)
	}
	if recovered(t, root) {
		t.Fatal("recovery state remained after a successful pull")
	}

	// A repeated pull changes nothing and says so.
	if again := run(t, loaded, root, "all"); !strings.Contains(again, "already up to date") || strings.Contains(again, "✓ updated") {
		t.Fatalf("second pull = %q, want every repository up to date", again)
	}
}

func TestPullLeavesCurrentAndAheadRepositoriesUnchanged(t *testing.T) {
	loaded, root, _ := seed(t, nil)
	commit(t, loaded, root, "src/local.css", "body {}\n")
	before := map[string]string{"private": head(t, root, "private"), "public": head(t, root, "public")}

	output := run(t, loaded, root, "")
	for name, want := range before {
		if got := head(t, root, name); got != want {
			t.Fatalf("%s main = %q, want the unchanged %q", name, got, want)
		}
	}
	if !strings.Contains(output, "public") || !strings.Contains(output, "is ahead of origin/main") {
		t.Fatalf("output = %q, want public reported as ahead", output)
	}
	if strings.Count(output, "already up to date") != 2 {
		t.Fatalf("output = %q, want both repositories reported as up to date", output)
	}
}

func TestPullRejectsDivergedBranchBeforeAnyRepositoryMoves(t *testing.T) {
	loaded, root, remotes := seed(t, nil)
	commit(t, loaded, root, "src/local.css", "body {}\n")
	advance(t, remotes["public"], "src/remote.css", "body {}\n")
	advance(t, remotes["private"], "secrets/token.txt", "token\n")
	before := head(t, root, "private")

	message := fails(t, loaded, root, "")
	if !strings.Contains(message, "PULL001") || !strings.Contains(message, "diverged") {
		t.Fatalf("error = %q, want PULL001 about the diverged branch", message)
	}
	if got := head(t, root, "private"); got != before {
		t.Fatalf("private main = %q, want the unchanged %q", got, before)
	}
	if got := contents(t, root, "secrets/token.txt"); got != "" {
		t.Fatalf("secrets/token.txt = %q, want no file", got)
	}
	if !strings.Contains(message, "https://gitone.io/docs/usage#resolving-diverged-history") {
		t.Fatalf("missing divergence repair link: %s", message)
	}
}

func TestPullAndPushSucceedAfterRepairingDivergedHistory(t *testing.T) {
	loaded, root, remotes := seed(t, nil)
	commit(t, loaded, root, "src/local.css", "body {}\n")
	advance(t, remotes["public"], "src/remote.css", "body {}\n")
	message := fails(t, loaded, root, "public")
	if !strings.Contains(message, "diverged") {
		t.Fatalf("expected divergence before repair: %s", message)
	}

	// A human completes the native merge, then returns to GitOne for
	// validation and publication. GitOne never gains a merge fallback.
	if _, err := git.Run(root, "--git-dir="+repository.Directory(root, "public"),
		"--work-tree=.", "merge", "--no-edit", "origin/main"); err != nil {
		t.Fatal(err)
	}
	run(t, loaded, root, "")
	var output bytes.Buffer
	if err := push.Push(loaded, root, "public", true, false, nil, &output); err != nil {
		t.Fatalf("publish documented merge repair: %v", err)
	}
}

func TestPullRejectsLocalChangesInARepositoryThatWouldMove(t *testing.T) {
	loaded, root, remotes := seed(t, nil)
	advance(t, remotes["public"], "src/site.css", "body {}\n")
	advance(t, remotes["private"], "secrets/token.txt", "token\n")
	write(t, root, "README.md", "local\n")
	before := head(t, root, "private")

	message := fails(t, loaded, root, "")
	if !strings.Contains(message, "PULL001") || !strings.Contains(message, "local changes") {
		t.Fatalf("error = %q, want PULL001 about the local changes", message)
	}
	if got := contents(t, root, "README.md"); got != "local\n" {
		t.Fatalf("README.md = %q, want the local content", got)
	}
	if got := head(t, root, "private"); got != before {
		t.Fatalf("private main = %q, want the unchanged %q", got, before)
	}
	if got := contents(t, root, "secrets/token.txt"); got != "" {
		t.Fatalf("secrets/token.txt = %q, want no file", got)
	}
}

func TestPullRejectsIncomingTreeWithUnownedPaths(t *testing.T) {
	loaded, root, remotes := seed(t, nil)
	advance(t, remotes["public"], "secrets/leak.txt", "leaked\n")
	before := head(t, root, "public")

	message := fails(t, loaded, root, "public")
	if !strings.Contains(message, "PULL001") || !strings.Contains(message, "secrets/leak.txt") {
		t.Fatalf("error = %q, want PULL001 naming the unowned path", message)
	}
	if got := head(t, root, "public"); got != before {
		t.Fatalf("public main = %q, want the unchanged %q", got, before)
	}
	if got := contents(t, root, "secrets/leak.txt"); got != "" {
		t.Fatalf("secrets/leak.txt = %q, want no file", got)
	}
}

func TestPullRequiresAnOriginBranch(t *testing.T) {
	loaded, root, _ := project(t, nil)
	commit(t, loaded, root, "README.md", "hello\n")
	for _, name := range []string{"private", "public"} {
		for key, value := range map[string]string{"branch.main.remote": "origin", "branch.main.merge": "refs/heads/main"} {
			if _, err := git.Run(root, "--git-dir="+repository.Directory(root, name), "config", key, value); err != nil {
				t.Fatal(err)
			}
		}
	}

	message := fails(t, loaded, root, "")
	if !strings.Contains(message, "PULL001") || !strings.Contains(message, "origin/main") {
		t.Fatalf("error = %q, want PULL001 about the missing origin branch", message)
	}
}

func TestPullRequiresOriginUpstreamAndReportsEveryRepository(t *testing.T) {
	loaded, root, remotes := seed(t, nil)
	advance(t, remotes["public"], "src/site.css", "body {}\n")
	if _, err := git.Run(root, "--git-dir="+repository.Directory(root, "public"),
		"config", "branch.main.remote", "."); err != nil {
		t.Fatal(err)
	}

	message, output := failure(t, loaded, root, "")
	if !strings.Contains(message, "PULL001") || !strings.Contains(message, "does not track origin/main") {
		t.Fatalf("error = %q, want PULL001 about the configured upstream", message)
	}
	if !strings.Contains(output, "private") || !strings.Contains(output, "already up to date") ||
		!strings.Contains(output, "public") || !strings.Contains(output, "failed") {
		t.Fatalf("output = %q, want one result for every repository", output)
	}
}

func TestPullFetchesEveryRepositoryBeforeReportingFailure(t *testing.T) {
	loaded, root, remotes := seed(t, nil)
	advance(t, remotes["public"], "src/site.css", "body {}\n")
	if err := os.RemoveAll(remotes["private"]); err != nil {
		t.Fatal(err)
	}

	message, output := failure(t, loaded, root, "")
	if !strings.Contains(message, "PULL001") || !strings.Contains(message, "private") {
		t.Fatalf("error = %q, want PULL001 naming the failed fetch", message)
	}
	if !strings.Contains(output, "private") || !strings.Contains(output, "failed") ||
		!strings.Contains(output, "public") || !strings.Contains(output, "unchanged") {
		t.Fatalf("output = %q, want one result for every repository", output)
	}
}

func TestPullSkipsLocalOnlyRepositoriesAndRejectsThemAsTarget(t *testing.T) {
	loaded, root, remotes := seed(t, map[string]string{"private": ""})
	target := advance(t, remotes["public"], "src/site.css", "body {}\n")

	output := run(t, loaded, root, "")
	if !strings.Contains(output, "private") || !strings.Contains(output, "skipped") {
		t.Fatalf("output = %q, want private reported as skipped", output)
	}
	if got := head(t, root, "public"); got != target {
		t.Fatalf("public main = %q, want %q", got, target)
	}

	message := fails(t, loaded, root, "private")
	if !strings.HasPrefix(message, "REPO001 ") || !strings.Contains(message, "origin") {
		t.Fatalf("error = %q, want REPO001 about the missing origin", message)
	}
}

func TestPullRejectsUnknownRepository(t *testing.T) {
	loaded, root, _ := seed(t, nil)
	if message := fails(t, loaded, root, "website"); !strings.HasPrefix(message, "REPO001 ") {
		t.Fatalf("error = %q, want REPO001", message)
	}
}

func TestPullStopsOnUnsafeWorkingTreeBeforeAnyNetworkAccess(t *testing.T) {
	loaded, root, remotes := seed(t, nil)
	advance(t, remotes["public"], "src/site.css", "body {}\n")
	write(t, root, "unassigned.txt", "orphan\n")

	if message := fails(t, loaded, root, ""); !strings.Contains(message, "PATH001") {
		t.Fatalf("error = %q, want PATH001", message)
	}
}

func TestPullIsRejectedWhileAnotherOperationRuns(t *testing.T) {
	loaded, root, _ := seed(t, nil)
	release, err := lock.Acquire(root, "gitone commit")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	if message := fails(t, loaded, root, ""); !strings.HasPrefix(message, "LOCK001 ") {
		t.Fatalf("error = %q, want LOCK001", message)
	}
}

// TestPullRestoresAdvancedRepositoriesWhenALaterOneFails uses an untracked
// file that the incoming public tree would overwrite: it is no local change,
// so preflight passes and native Git refuses the fast-forward after the
// private repository was already advanced.
func TestPullRestoresAdvancedRepositoriesWhenALaterOneFails(t *testing.T) {
	loaded, root, remotes := seed(t, nil)
	advance(t, remotes["private"], "secrets/token.txt", "token\n")
	advance(t, remotes["public"], "src/site.css", "body {}\n")
	write(t, root, "src/site.css", "untracked\n")
	before := head(t, root, "private")

	message := fails(t, loaded, root, "")
	if !strings.Contains(message, "PULL001") || !strings.Contains(message, "public") {
		t.Fatalf("error = %q, want PULL001 naming public", message)
	}
	if got := head(t, root, "private"); got != before {
		t.Fatalf("private main = %q, want the restored %q", got, before)
	}
	if got := contents(t, root, "secrets/token.txt"); got != "" {
		t.Fatalf("secrets/token.txt = %q, want the restored working tree without it", got)
	}
	if got := contents(t, root, "src/site.css"); got != "untracked\n" {
		t.Fatalf("src/site.css = %q, want the untouched untracked file", got)
	}
	if recovered(t, root) {
		t.Fatal("recovery state remained although every repository was restored")
	}
}

// record writes the state an interrupted pull leaves behind, which is how the
// recovery of a pull is exercised without killing a process.
func record(t *testing.T, root string, originals map[string][]byte, entries ...map[string]any) {
	t.Helper()
	directory := lock.RecoveryPath(root)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry["name"].(string)
		if err := os.WriteFile(filepath.Join(directory, name+".original"), originals[name], 0o600); err != nil {
			t.Fatal(err)
		}
	}
	saved := map[string]any{"version": 1, "command": pull.Command, "repositories": entries}
	data, err := json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, lock.StateFile), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverFinishesAnInterruptedPull(t *testing.T) {
	loaded, root, remotes := seed(t, nil)
	target := advance(t, remotes["public"], "src/site.css", "body {}\n")
	before := head(t, root, "public")
	originals := map[string][]byte{"public": index(t, root, "public")}
	// The fetch of the interrupted pull already happened.
	if err := fetchOrigin(root, "public"); err != nil {
		t.Fatal(err)
	}
	record(t, root, originals, map[string]any{"name": "public", "branch": "main", "head": before, "target": target})

	var output bytes.Buffer
	if err := stage.Recover(loaded, root, &output); err != nil {
		t.Fatalf("recover: %v\noutput: %s", err, output.String())
	}
	if got := head(t, root, "public"); got != target {
		t.Fatalf("public main = %q, want the finished %q", got, target)
	}
	if got := contents(t, root, "src/site.css"); got != "body {}\n" {
		t.Fatalf("src/site.css = %q, want the pulled content", got)
	}
	if recovered(t, root) {
		t.Fatal("recovery state remained after gitone recover")
	}
}

func TestAbortUndoesAFinishedFastForward(t *testing.T) {
	loaded, root, remotes := seed(t, nil)
	target := advance(t, remotes["public"], "src/site.css", "body {}\n")
	before := head(t, root, "public")
	if _, err := git.Run(root, "--git-dir="+repository.Directory(root, "public"),
		"update-index", "--assume-unchanged", "README.md"); err != nil {
		t.Fatal(err)
	}
	originals := map[string][]byte{"public": index(t, root, "public")}
	run(t, loaded, root, "public")
	record(t, root, originals, map[string]any{"name": "public", "branch": "main", "head": before, "target": target, "updated": true})

	var output bytes.Buffer
	if err := stage.Abort(loaded, root, &output); err != nil {
		t.Fatalf("abort: %v\noutput: %s", err, output.String())
	}
	if got := head(t, root, "public"); got != before {
		t.Fatalf("public main = %q, want the restored %q", got, before)
	}
	if got := contents(t, root, "src/site.css"); got != "" {
		t.Fatalf("src/site.css = %q, want the file removed again", got)
	}
	if got := index(t, root, "public"); !bytes.Equal(got, originals["public"]) {
		t.Fatal("public index was not restored exactly")
	}
	if recovered(t, root) {
		t.Fatal("recovery state remained after gitone abort")
	}
}

func TestAbortHandlesAFastForwardWithAnUnchangedTree(t *testing.T) {
	loaded, root, remotes := seed(t, nil)
	target := advanceEmpty(t, remotes["public"])
	before := head(t, root, "public")
	originals := map[string][]byte{"public": index(t, root, "public")}
	run(t, loaded, root, "public")
	record(t, root, originals, map[string]any{"name": "public", "branch": "main", "head": before, "target": target, "updated": true})

	var output bytes.Buffer
	if err := stage.Abort(loaded, root, &output); err != nil {
		t.Fatalf("abort: %v\noutput: %s", err, output.String())
	}
	if got := head(t, root, "public"); got != before {
		t.Fatalf("public main = %q, want the restored %q", got, before)
	}
}

// TestRecoverCompletesAFastForwardWhoseBranchDidNotMove covers the one state
// an interruption can leave between the working tree and the branch.
func TestRecoverCompletesAFastForwardWhoseBranchDidNotMove(t *testing.T) {
	loaded, root, remotes := seed(t, nil)
	target := advance(t, remotes["public"], "src/site.css", "body {}\n")
	before := head(t, root, "public")
	originals := map[string][]byte{"public": index(t, root, "public")}
	run(t, loaded, root, "public")
	if _, err := git.Run(root, "--git-dir="+repository.Directory(root, "public"),
		"update-ref", "refs/heads/main", before, target); err != nil {
		t.Fatal(err)
	}
	record(t, root, originals, map[string]any{"name": "public", "branch": "main", "head": before, "target": target})

	var output bytes.Buffer
	if err := stage.Recover(loaded, root, &output); err != nil {
		t.Fatalf("recover: %v\noutput: %s", err, output.String())
	}
	if got := head(t, root, "public"); got != target {
		t.Fatalf("public main = %q, want the finished %q", got, target)
	}
}

func TestRecoveryRefusesARepositoryThatChangedOutsideGitOne(t *testing.T) {
	loaded, root, remotes := seed(t, nil)
	target := advance(t, remotes["public"], "src/site.css", "body {}\n")
	before := head(t, root, "public")
	originals := map[string][]byte{"public": index(t, root, "public")}
	run(t, loaded, root, "public")
	commit(t, loaded, root, "src/own.css", "body {}\n")
	record(t, root, originals, map[string]any{"name": "public", "branch": "main", "head": before, "target": target, "updated": true})

	var output bytes.Buffer
	err := stage.Abort(loaded, root, &output)
	if err == nil {
		t.Fatalf("abort succeeded, output = %q", output.String())
	}
	if !strings.Contains(err.Error(), "REC001") {
		t.Fatalf("error = %q, want REC001", err)
	}
	if !recovered(t, root) {
		t.Fatal("recovery state was removed although the repository was not restored")
	}
}

func fetchOrigin(root, name string) error {
	_, err := git.Run(root, "--git-dir="+repository.Directory(root, name), "fetch", "--quiet", "origin",
		"+refs/heads/*:refs/remotes/origin/*")
	return err
}

// advanceLink adds one commit whose tree carries a symbolic link, which
// GitOne never lets native Git materialize.
func advanceLink(t *testing.T, bare, name, target string) {
	t.Helper()
	clone := temporary(t)
	if _, err := git.Run(clone, "clone", "--quiet", bare, clone); err != nil {
		t.Fatal(err)
	}
	full := filepath.Join(clone, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, full); err != nil {
		t.Fatal(err)
	}
	for _, arguments := range [][]string{{"add", name}, {"commit", "--quiet", "-m", "External link"},
		{"push", "--quiet", "origin", "HEAD:refs/heads/main"}} {
		if _, err := git.Run(clone, arguments...); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPullRejectsIncomingSymbolicLink(t *testing.T) {
	loaded, root, remotes := seed(t, nil)
	// src/link is owned by public, so only the entry mode can refuse it.
	advanceLink(t, remotes["public"], "src/link", "/etc/passwd")
	before := head(t, root, "public")

	message := fails(t, loaded, root, "public")
	if !strings.Contains(message, "PULL001") || !strings.Contains(message, "src/link") ||
		!strings.Contains(message, "absolute link targets are not supported") {
		t.Fatalf("error = %q, want PULL001 refusing the symbolic link", message)
	}
	if got := head(t, root, "public"); got != before {
		t.Fatalf("public main = %q, want the unchanged %q", got, before)
	}
	if _, err := os.Lstat(filepath.Join(root, "src", "link")); err == nil {
		t.Fatal("src/link was created in the working tree")
	}
}
