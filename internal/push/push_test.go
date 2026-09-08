package push_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/devidevio/gitone/internal/cli"
	"github.com/devidevio/gitone/internal/config"
	"github.com/devidevio/gitone/internal/git"
	"github.com/devidevio/gitone/internal/lock"
	"github.com/devidevio/gitone/internal/push"
	"github.com/devidevio/gitone/internal/repository"
	"github.com/devidevio/gitone/internal/stage"
)

// project creates an initialized project whose repositories push to local bare
// repositories, and returns their directories by repository name. An empty
// remote leaves that repository without a configured origin.
func project(t *testing.T, files map[string]string, missingRemote ...string) (*config.Config, string, map[string]string) {
	t.Helper()
	for _, name := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(name, "GitOne")
	}
	for _, name := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(name, "gitone@example.com")
	}

	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	origins, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
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
		if !strings.Contains(strings.Join(missingRemote, " "), name) {
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

	for name, contents := range files {
		write(t, root, name, contents)
	}
	loaded, discovered, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.Init(loaded, discovered); err != nil {
		t.Fatal(err)
	}
	return loaded, discovered, remotes
}

// committed creates a project whose files are staged and committed.
func committed(t *testing.T, files map[string]string, paths ...string) (*config.Config, string, map[string]string) {
	t.Helper()
	loaded, root, remotes := project(t, files)
	commit(t, loaded, root, "Add files", paths...)
	return loaded, root, remotes
}

func commit(t *testing.T, loaded *config.Config, root, message string, paths ...string) {
	t.Helper()
	var output bytes.Buffer
	if err := stage.Add(loaded, root, root, paths, &output); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := stage.Commit(loaded, root, root, nil, message, &output); err != nil {
		t.Fatalf("commit: %v", err)
	}
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
	arguments = append([]string{"--git-dir=" + repository.Directory(root, name), "--work-tree=" + root}, arguments...)
	output, err := git.Run(root, arguments...)
	if err != nil {
		t.Fatal(err)
	}
	return output
}

// head is the commit refs/heads/main points at, empty while it does not exist.
func head(t *testing.T, root, name string) string {
	t.Helper()
	return strings.TrimSpace(run(t, root, name, "for-each-ref", "--format=%(objectname)", "refs/heads/main"))
}

// remoteHead is the commit the bare repository holds, empty while it has no
// branch main.
func remoteHead(t *testing.T, directory string) string {
	t.Helper()
	output, err := git.Run(directory, "--git-dir="+directory, "for-each-ref", "--format=%(objectname)", "refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(output)
}

// hook installs a repository-local Git hook.
func hook(t *testing.T, root, name, kind, script string) {
	t.Helper()
	directory := filepath.Join(repository.Directory(root, name), "hooks")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, kind), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
}

func pushProject(t *testing.T, loaded *config.Config, root, target string) string {
	t.Helper()
	var output bytes.Buffer
	if err := push.Push(loaded, root, target, true, false, strings.NewReader(""), &output); err != nil {
		t.Fatalf("push: %v\n%s", err, output.String())
	}
	return output.String()
}

func pushFailure(t *testing.T, loaded *config.Config, root, target string) (error, string) {
	t.Helper()
	var output bytes.Buffer
	err := push.Push(loaded, root, target, true, false, strings.NewReader(""), &output)
	if err == nil {
		t.Fatalf("push succeeded, want an error: %s", output.String())
	}
	return err, output.String()
}

func TestPushPublishesEveryRepositoryAndSetsUpstream(t *testing.T) {
	loaded, root, remotes := committed(t, map[string]string{
		"README.md":        "readme\n",
		"secrets/notes.md": "notes\n",
	}, "-A")
	run(t, root, "private", "config", "branch.main.remote", "elsewhere")
	run(t, root, "private", "config", "branch.main.merge", "refs/heads/main")
	run(t, root, "public", "config", "branch.main.remote", "origin")
	run(t, root, "public", "config", "branch.main.merge", "refs/heads/elsewhere")

	output := pushProject(t, loaded, root, "")

	for _, name := range []string{"private", "public"} {
		if got, want := remoteHead(t, remotes[name]), head(t, root, name); got != want {
			t.Fatalf("%s remote = %q, want %q", name, got, want)
		}
		if got := strings.TrimSpace(run(t, root, name, "config", "--get", "branch.main.remote")); got != "origin" {
			t.Fatalf("%s upstream = %q, want origin", name, got)
		}
		if got := strings.TrimSpace(run(t, root, name, "config", "--get", "branch.main.merge")); got != "refs/heads/main" {
			t.Fatalf("%s upstream branch = %q", name, got)
		}
		if got := strings.TrimSpace(run(t, root, name, "for-each-ref", "--format=%(objectname)", "refs/remotes/origin/main")); got != head(t, root, name) {
			t.Fatalf("%s remote-tracking ref = %q", name, got)
		}
	}
	if !strings.Contains(output, "commits: 1") || !strings.Contains(output, "pushed ") {
		t.Fatalf("output = %q", output)
	}

	// The second push only fast-forwards the same branch.
	write(t, root, "README.md", "more\n")
	commit(t, loaded, root, "Update readme", "-A")
	pushProject(t, loaded, root, "")
	if got, want := remoteHead(t, remotes["public"]), head(t, root, "public"); got != want {
		t.Fatalf("public remote = %q, want %q", got, want)
	}
	if got := strings.TrimSpace(run(t, root, "public", "for-each-ref", "--format=%(refname)", "refs/heads/", "refs/tags/")); got != "refs/heads/main" {
		t.Fatalf("public refs = %q", got)
	}

	var again bytes.Buffer
	if err := push.Push(loaded, root, "", true, false, strings.NewReader(""), &again); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(again.String(), "Nothing to push.") {
		t.Fatalf("third push = %q", again.String())
	}
}

func TestPushHonorsDisabledRepositoryPolicy(t *testing.T) {
	loaded, root, remotes := committed(t, map[string]string{
		"README.md":        "readme\n",
		"secrets/notes.md": "notes\n",
	}, "-A")
	private := loaded.Repositories["private"]
	private.Push = config.PushDisabled
	loaded.Repositories["private"] = private

	output := pushProject(t, loaded, root, "all")
	if !strings.Contains(output, "Skipped push-disabled repositories: private") {
		t.Fatalf("output = %q", output)
	}
	if got := remoteHead(t, remotes["private"]); got != "" {
		t.Fatalf("private remote = %q, want no branch", got)
	}
	if got, want := remoteHead(t, remotes["public"]), head(t, root, "public"); got != want {
		t.Fatalf("public remote = %q, want %q", got, want)
	}

	err, _ := pushFailure(t, loaded, root, "private")
	if !strings.HasPrefix(err.Error(), "PUSH001") || !strings.Contains(err.Error(), "has push disabled") {
		t.Fatalf("error = %v", err)
	}
}

func TestPushCanRequireCleanWorktree(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*testing.T, *config.Config, string)
	}{
		{name: "unstaged", change: func(t *testing.T, _ *config.Config, root string) {
			write(t, root, "README.md", "changed\n")
		}},
		{name: "staged", change: func(t *testing.T, loaded *config.Config, root string) {
			write(t, root, "README.md", "changed\n")
			var output bytes.Buffer
			if err := stage.Add(loaded, root, root, []string{"README.md"}, &output); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "untracked", change: func(t *testing.T, _ *config.Config, root string) {
			write(t, root, "src/new.go", "package src\n")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			loaded, root, remotes := committed(t, map[string]string{"README.md": "readme\n"}, "-A")
			loaded.Push.RequireCleanWorktree = true
			test.change(t, loaded, root)

			err, _ := pushFailure(t, loaded, root, "public")
			if !strings.HasPrefix(err.Error(), "PUSH001") || !strings.Contains(err.Error(), "requires a clean working tree") {
				t.Fatalf("error = %v", err)
			}
			if got := remoteHead(t, remotes["public"]); got != "" {
				t.Fatalf("public remote = %q, want no branch", got)
			}
		})
	}
}

func TestPushRequiresCleanWorktreeEvenWithoutOutgoingCommits(t *testing.T) {
	loaded, root, _ := committed(t, map[string]string{"README.md": "readme\n"}, "-A")
	pushProject(t, loaded, root, "public")
	loaded.Push.RequireCleanWorktree = true
	write(t, root, "src/new.go", "package src\n")

	err, _ := pushFailure(t, loaded, root, "public")
	if !strings.HasPrefix(err.Error(), "PUSH001") || !strings.Contains(err.Error(), "requires a clean working tree") {
		t.Fatalf("error = %v", err)
	}
}

func TestPushRejectsNonFastForwardWithoutForcing(t *testing.T) {
	loaded, root, remotes := committed(t, map[string]string{"README.md": "readme\n"}, "-A")
	pushProject(t, loaded, root, "public")
	published := remoteHead(t, remotes["public"])

	// Rewriting the published commit locally must not reach the remote.
	run(t, root, "public", "-c", "user.name=GitOne", "-c", "user.email=gitone@example.com", "commit", "--quiet", "--amend", "-m", "Rewritten")

	err, _ := pushFailure(t, loaded, root, "public")
	if !strings.HasPrefix(err.Error(), "PUSH001") || !strings.Contains(err.Error(), "would not fast-forward") {
		t.Fatalf("error = %v", err)
	}
	if got := remoteHead(t, remotes["public"]); got != published {
		t.Fatalf("public remote = %q, want the unchanged %q", got, published)
	}
}

func TestPushRefusesUnreachableParticipantBeforeModifyingAnyRemote(t *testing.T) {
	loaded, root, remotes := committed(t, map[string]string{
		"README.md":        "readme\n",
		"secrets/notes.md": "notes\n",
	}, "-A")
	if err := os.RemoveAll(remotes["private"]); err != nil {
		t.Fatal(err)
	}

	err, _ := pushFailure(t, loaded, root, "")
	if !strings.HasPrefix(err.Error(), "PUSH001") || !strings.Contains(err.Error(), "is unreachable") {
		t.Fatalf("error = %v", err)
	}
	if !strings.HasSuffix(err.Error(), "No repositories were modified.") {
		t.Fatalf("error = %v", err)
	}
	if got := remoteHead(t, remotes["public"]); got != "" {
		t.Fatalf("public remote = %q, want no branch", got)
	}
}

func TestPushRequiresConfiguredOrigin(t *testing.T) {
	loaded, root, remotes := project(t, map[string]string{
		"README.md":        "readme\n",
		"secrets/notes.md": "notes\n",
	}, "private")
	commit(t, loaded, root, "Add files", "-A")

	err, _ := pushFailure(t, loaded, root, "")
	if !strings.HasPrefix(err.Error(), "REPO001") || !strings.Contains(err.Error(), "has no configured origin remote") {
		t.Fatalf("error = %v", err)
	}
	if got := remoteHead(t, remotes["public"]); got != "" {
		t.Fatalf("public remote = %q, want no branch", got)
	}
}

func TestPushRejectsEveryUnverifiedDestination(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*testing.T, string, string, string)
		want      string
	}{
		{
			name: "multiple fetch URLs",
			configure: func(t *testing.T, root, configured, hidden string) {
				run(t, root, "public", "config", "--add", "remote.origin.url", hidden)
				run(t, root, "public", "config", "--add", "remote.origin.url", configured)
			},
			want: "3 fetch destinations",
		},
		{
			name: "multiple push URLs",
			configure: func(t *testing.T, root, configured, hidden string) {
				run(t, root, "public", "config", "--add", "remote.origin.pushurl", configured)
				run(t, root, "public", "config", "--add", "remote.origin.pushurl", hidden)
			},
			want: "2 push destinations",
		},
		{
			name: "pushInsteadOf",
			configure: func(t *testing.T, root, configured, hidden string) {
				run(t, root, "public", "config", "url."+hidden+".pushInsteadOf", configured)
			},
			want: "pushes to",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			loaded, root, remotes := committed(t, map[string]string{"README.md": "readme\n"}, "-A")
			hidden := filepath.Join(t.TempDir(), "hidden.git")
			if _, err := git.Run(root, "init", "--bare", "--quiet", "--initial-branch=main", hidden); err != nil {
				t.Fatal(err)
			}
			test.configure(t, root, remotes["public"], hidden)

			err, _ := pushFailure(t, loaded, root, "public")
			if !strings.HasPrefix(err.Error(), "REPO001") || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v", err)
			}
			for _, directory := range []string{remotes["public"], hidden} {
				if got := remoteHead(t, directory); got != "" {
					t.Fatalf("%s remote = %q, want no branch", directory, got)
				}
			}
		})
	}
}

func TestPushRejectsContaminatedHistoryEvenWhenTheTreeIsClean(t *testing.T) {
	loaded, root, remotes := committed(t, map[string]string{"README.md": "readme\n"}, "README.md")
	write(t, root, "secrets/notes.md", "notes\n")

	// A private path enters the public history natively and is removed again,
	// so only the outgoing commits still contain it.
	identity := []string{"-c", "user.name=GitOne", "-c", "user.email=gitone@example.com"}
	run(t, root, "public", "add", "--", "secrets/notes.md")
	run(t, root, "public", append(identity, "commit", "--quiet", "-m", "Leak")...)
	run(t, root, "public", "rm", "--quiet", "--cached", "--", "secrets/notes.md")
	run(t, root, "public", append(identity, "commit", "--quiet", "-m", "Remove leak")...)

	err, output := pushFailure(t, loaded, root, "public")
	if !strings.HasPrefix(err.Error(), "PUSH001") || !strings.Contains(err.Error(), "secrets/notes.md") ||
		!strings.Contains(err.Error(), "owned by private") {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(output, "Ready to push") {
		t.Fatalf("output = %q", output)
	}
	if got := remoteHead(t, remotes["public"]); got != "" {
		t.Fatalf("public remote = %q, want no branch", got)
	}
}

func TestPushAfterDeletingFileAndOwnershipRule(t *testing.T) {
	for _, mode := range []string{"delete", "rename"} {
		t.Run(mode, func(t *testing.T) {
			loaded, root, remotes := committed(t, map[string]string{"README.md": "readme\n"}, "-A")
			pushProject(t, loaded, root, "public")
			run(t, root, "public", "config", "diff.renames", "true")

			if err := os.Remove(filepath.Join(root, "README.md")); err != nil {
				t.Fatal(err)
			}
			if mode == "rename" {
				write(t, root, "src/readme.md", "readme\n")
			}
			contents, err := os.ReadFile(filepath.Join(root, ".gitone.yml"))
			if err != nil {
				t.Fatal(err)
			}
			write(t, root, ".gitone.yml", strings.Replace(string(contents), "      - README.md\n", "", 1))

			for _, arguments := range [][]string{
				{"add", "."},
				{"commit", "-m", "Remove readme and ownership rule"},
				{"push", "public", "--yes"},
			} {
				var stdout, stderr bytes.Buffer
				if code := cli.Run(arguments, root, strings.NewReader(""), &stdout, &stderr); code != 0 {
					t.Fatalf("%v exited %d: %s\n%s", arguments, code, stdout.String(), stderr.String())
				}
			}
			if got := remoteHead(t, remotes["public"]); got != head(t, root, "public") {
				t.Fatalf("remote HEAD = %s, want local HEAD", got)
			}
		})
	}
}

func TestPushRejectsUnassignedContentDeletedBeforePush(t *testing.T) {
	loaded, root, remotes := committed(t, map[string]string{"README.md": "readme\n"}, "-A")
	pushProject(t, loaded, root, "public")
	previous := remoteHead(t, remotes["public"])

	write(t, root, "unassigned.txt", "unassigned content\n")
	run(t, root, "public", "add", "unassigned.txt")
	run(t, root, "public", "commit", "-qm", "Add unassigned content")
	run(t, root, "public", "rm", "unassigned.txt")
	run(t, root, "public", "commit", "-qm", "Remove unassigned content")

	err, _ := pushFailure(t, loaded, root, "public")
	if !strings.Contains(err.Error(), "unassigned.txt") || !strings.Contains(err.Error(), "not assigned") {
		t.Fatalf("error = %v", err)
	}
	if got := remoteHead(t, remotes["public"]); got != previous {
		t.Fatalf("rejected push changed remote HEAD to %s", got)
	}
}

func TestPushRejectsAnUnsafeLinkAnEarlierCommitCarried(t *testing.T) {
	loaded, root, remotes := committed(t, map[string]string{"README.md": "readme\n"}, "README.md")

	// The escaping link enters the outgoing history natively and the head
	// commit repairs it, so only the earlier commit still carries it.
	identity := []string{"-c", "user.name=GitOne", "-c", "user.email=gitone@example.com"}
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(root, "src", "alias")); err != nil {
		t.Fatal(err)
	}
	run(t, root, "public", "add", "--", "src/alias")
	run(t, root, "public", append(identity, "commit", "--quiet", "-m", "Add alias")...)
	run(t, root, "public", "rm", "--quiet", "--", "src/alias")
	run(t, root, "public", append(identity, "commit", "--quiet", "-m", "Remove alias")...)

	err, _ := pushFailure(t, loaded, root, "public")
	if !strings.HasPrefix(err.Error(), "PUSH001") ||
		!strings.Contains(err.Error(), "src/alias in commit") ||
		!strings.Contains(err.Error(), "absolute link targets are not supported") {
		t.Fatalf("error = %v, want the refused link of an earlier commit", err)
	}
	if got := remoteHead(t, remotes["public"]); got != "" {
		t.Fatalf("public remote = %q, want no branch", got)
	}
}

func TestPushRejectsProtectedPathRemovedFromCurrentTree(t *testing.T) {
	loaded, root, remotes := committed(t, map[string]string{"README.md": "readme\n"}, "README.md")
	write(t, root, "src/private.key", "secret\n")
	identity := []string{"-c", "user.name=GitOne", "-c", "user.email=gitone@example.com"}
	run(t, root, "public", "add", "--", "src/private.key")
	run(t, root, "public", append(identity, "commit", "--quiet", "-m", "Add secret")...)
	run(t, root, "public", "rm", "--quiet", "--", "src/private.key")
	run(t, root, "public", append(identity, "commit", "--quiet", "-m", "Remove secret")...)
	loaded.ProtectedPaths = []string{"src/private.key"}

	err, _ := pushFailure(t, loaded, root, "public")
	if !strings.HasPrefix(err.Error(), "PUSH001") || !strings.Contains(err.Error(), "path is protected") {
		t.Fatalf("error = %v", err)
	}
	if got := remoteHead(t, remotes["public"]); got != "" {
		t.Fatalf("public remote = %q, want no branch", got)
	}
}

func TestPushValidatesTheWholeProjectForASelectiveTarget(t *testing.T) {
	loaded, root, remotes := committed(t, map[string]string{"README.md": "readme\n"}, "-A")
	write(t, root, "unassigned.txt", "orphan\n")

	err, _ := pushFailure(t, loaded, root, "public")
	if !strings.Contains(err.Error(), "PATH001") {
		t.Fatalf("error = %v", err)
	}
	if got := remoteHead(t, remotes["public"]); got != "" {
		t.Fatalf("public remote = %q, want no branch", got)
	}
}

func TestPushValidatesRepositoryMetadataForTheWholeProject(t *testing.T) {
	_, root, remotes := committed(t, map[string]string{"README.md": "readme\n"}, "-A")
	run(t, root, "private", "config", "core.worktree", "../wrong")

	var stdout, stderr bytes.Buffer
	if exitCode := cli.Run([]string{"push", "public", "--yes"}, root, strings.NewReader(""), &stdout, &stderr); exitCode == 0 {
		t.Fatalf("exit code = 0, stdout = %q", stdout.String())
	}
	if got := stderr.String(); !strings.HasPrefix(got, "REPO001") || !strings.Contains(got, "does not use the project working tree") {
		t.Fatalf("stderr = %q", got)
	}
	if got := remoteHead(t, remotes["public"]); got != "" {
		t.Fatalf("public remote = %q, want no branch", got)
	}
}

func TestPushNeverContactsUnchangedOrUnselectedRepositories(t *testing.T) {
	loaded, root, remotes := committed(t, map[string]string{
		"README.md":        "readme\n",
		"secrets/notes.md": "notes\n",
	}, "README.md")
	// An unreachable private origin proves the repository is not contacted:
	// it has nothing outgoing and is not selected.
	if err := os.RemoveAll(remotes["private"]); err != nil {
		t.Fatal(err)
	}

	pushProject(t, loaded, root, "all")
	if got, want := remoteHead(t, remotes["public"]), head(t, root, "public"); got != want {
		t.Fatalf("public remote = %q, want %q", got, want)
	}

	commit(t, loaded, root, "Add notes", "-A")
	pushProject(t, loaded, root, "public")
	if got, want := remoteHead(t, remotes["public"]), head(t, root, "public"); got != want {
		t.Fatalf("public remote = %q, want %q", got, want)
	}
}

func TestPushWithoutConfirmationModifiesNoRemote(t *testing.T) {
	loaded, root, remotes := committed(t, map[string]string{"README.md": "readme\n"}, "-A")

	var declined bytes.Buffer
	if err := push.Push(loaded, root, "public", false, true, strings.NewReader("n\n"), &declined); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(declined.String(), "Continue? [y/N]") || !strings.Contains(declined.String(), "Push aborted.") {
		t.Fatalf("output = %q", declined.String())
	}
	if got := remoteHead(t, remotes["public"]); got != "" {
		t.Fatalf("public remote = %q, want no branch", got)
	}

	// An empty answer keeps the default no.
	var empty bytes.Buffer
	if err := push.Push(loaded, root, "public", false, true, strings.NewReader("\n"), &empty); err != nil {
		t.Fatal(err)
	}
	if got := remoteHead(t, remotes["public"]); got != "" {
		t.Fatalf("public remote = %q, want no branch", got)
	}

	run(t, root, "public", "config", "remote.origin.url", filepath.Join(root, "unreachable.git"))
	var automated bytes.Buffer
	err := push.Push(loaded, root, "public", false, false, strings.NewReader("y\n"), &automated)
	if err == nil || !strings.HasPrefix(err.Error(), "PUSH001") || !strings.Contains(err.Error(), "requires --yes") {
		t.Fatalf("error = %v", err)
	}
	if automated.Len() != 0 {
		t.Fatalf("output = %q, want immediate failure", automated.String())
	}
	if got := remoteHead(t, remotes["public"]); got != "" {
		t.Fatalf("public remote = %q, want no branch", got)
	}
	run(t, root, "public", "config", "remote.origin.url", remotes["public"])

	var accepted bytes.Buffer
	if err := push.Push(loaded, root, "public", false, true, strings.NewReader("y\n"), &accepted); err != nil {
		t.Fatal(err)
	}
	if got, want := remoteHead(t, remotes["public"]), head(t, root, "public"); got != want {
		t.Fatalf("public remote = %q, want %q", got, want)
	}
}

func TestPartialFailureReportsConfirmedRefsAndLeavesTheRestOutgoing(t *testing.T) {
	loaded, root, remotes := committed(t, map[string]string{
		"README.md":        "readme\n",
		"secrets/notes.md": "notes\n",
	}, "-A")
	// The repositories are pushed in name order, so private succeeds before
	// the public pre-push hook fails.
	hook(t, root, "public", "pre-push", "#!/bin/sh\nexit 1\n")

	err, output := pushFailure(t, loaded, root, "")
	if !strings.HasPrefix(err.Error(), "PUSH001") || !strings.Contains(err.Error(), `repository "public" was not pushed`) {
		t.Fatalf("error = %v", err)
	}
	if !strings.Contains(output, "private    origin/main published") || !strings.Contains(output, "public     origin/main unchanged") {
		t.Fatalf("output = %q", output)
	}
	if got, want := remoteHead(t, remotes["private"]), head(t, root, "private"); got != want {
		t.Fatalf("private remote = %q, want %q", got, want)
	}
	if got := remoteHead(t, remotes["public"]); got != "" {
		t.Fatalf("public remote = %q, want no branch", got)
	}
	if _, err := os.Stat(lock.RecoveryPath(root)); !os.IsNotExist(err) {
		t.Fatalf("recovery state = %v, want none", err)
	}

	if err := os.Remove(filepath.Join(repository.Directory(root, "public"), "hooks", "pre-push")); err != nil {
		t.Fatal(err)
	}
	rerun := pushProject(t, loaded, root, "")
	if strings.Contains(rerun, "PRIVATE") || !strings.Contains(rerun, "PUBLIC") {
		t.Fatalf("rerun = %q", rerun)
	}
	if got, want := remoteHead(t, remotes["public"]), head(t, root, "public"); got != want {
		t.Fatalf("public remote = %q, want %q", got, want)
	}
}

func TestRecoverReconcilesAnInterruptedPushWithoutModifyingRemotes(t *testing.T) {
	loaded, root, remotes := committed(t, map[string]string{
		"README.md":        "readme\n",
		"secrets/notes.md": "notes\n",
	}, "-A")
	pushProject(t, loaded, root, "private")

	// A crashed push leaves the recorded expectation behind: private was sent,
	// public was not reached.
	directory := lock.RecoveryPath(root)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	state := map[string]any{
		"version": 1,
		"command": "gitone push",
		"repositories": []map[string]any{
			{"name": "private", "branch": "main", "new": head(t, root, "private"), "pushed": true},
			{"name": "public", "branch": "main", "new": head(t, root, "public")},
		},
	}
	contents, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "state.json"), contents, 0o600); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	if err := stage.Recover(loaded, root, &output); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if !strings.Contains(output.String(), "private    origin/main published") ||
		!strings.Contains(output.String(), "public     origin/main unchanged, origin has no branch main") {
		t.Fatalf("output = %q", output.String())
	}
	if !strings.Contains(output.String(), "No remote was modified.") {
		t.Fatalf("output = %q", output.String())
	}
	if got := remoteHead(t, remotes["public"]); got != "" {
		t.Fatalf("public remote = %q, want the untouched empty branch", got)
	}
	if _, err := os.Stat(directory); !os.IsNotExist(err) {
		t.Fatalf("recovery state = %v, want none", err)
	}

	// The reconciliation only reported, so a normal rerun still has work.
	pushProject(t, loaded, root, "")
	if got, want := remoteHead(t, remotes["public"]), head(t, root, "public"); got != want {
		t.Fatalf("public remote = %q, want %q", got, want)
	}
}
