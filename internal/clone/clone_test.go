package clone_test

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/devidevio/gitone/internal/clone"
	"github.com/devidevio/gitone/internal/config"
	"github.com/devidevio/gitone/internal/fetch"
	"github.com/devidevio/gitone/internal/git"
	"github.com/devidevio/gitone/internal/repository"
	"github.com/devidevio/gitone/internal/status"
)

// Every test here seeds a repository, and a commit needs an identity. A CI
// runner has no global one, so the seed fails as a Git error a long way from
// the assertion that cares. Set it once for the package rather than at each
// call site, the way internal/pull does per test.
func TestMain(m *testing.M) {
	identity := map[string]string{
		"GIT_AUTHOR_NAME":     "GitOne",
		"GIT_COMMITTER_NAME":  "GitOne",
		"GIT_AUTHOR_EMAIL":    "gitone@example.com",
		"GIT_COMMITTER_EMAIL": "gitone@example.com",
	}
	for name, value := range identity {
		if err := os.Setenv(name, value); err != nil {
			panic(err)
		}
	}
	os.Exit(m.Run())
}

// configuration is the committed .gitone.yml of the bootstrap repository.
// %s is replaced by the "remote:" line of each repository, which is empty for
// a repository without a configured origin.
const configuration = `version: 1
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
`

func TestCloneAdoptsBootstrapHistoryAndChecksOutEveryRepository(t *testing.T) {
	remotes := origins(t)
	seed(t, remotes["public"], bootstrap(remotes["public"], remotes["private"]))
	seed(t, remotes["public"], map[string]string{"src/site.css": "body {}\n"})
	seed(t, remotes["private"], map[string]string{"secrets/token": "s3cret\n"})

	destination := filepath.Join(temporary(t), "website")
	output := run(t, remotes["public"], destination)

	for path, contents := range map[string]string{
		"README.md":     "# Website\n",
		"src/site.css":  "body {}\n",
		"secrets/token": "s3cret\n",
	} {
		if read(t, destination, path) != contents {
			t.Fatalf("%s holds %q", path, read(t, destination, path))
		}
	}
	// The bootstrap repository keeps its complete native history, so both of
	// its commits are reachable from the checked-out branch.
	if commits := log(t, destination, "public"); len(commits) != 2 {
		t.Fatalf("public has %d commits, want 2: %v", len(commits), commits)
	}
	for _, name := range []string{"public", "private"} {
		tracks, err := repository.TracksOriginBranch(destination, name, "main")
		if err != nil || !tracks {
			t.Fatalf("%s does not track origin/main: %v", name, err)
		}
	}
	if !strings.Contains(output, "checked out main") {
		t.Fatalf("output does not report the checkouts:\n%s", output)
	}
	healthy(t, destination)
}

func TestCloneReportsRepositoryWithoutOriginAsUnborn(t *testing.T) {
	remotes := origins(t)
	committed := bootstrap(remotes["public"], "")
	seed(t, remotes["public"], committed)

	destination := filepath.Join(temporary(t), "website")
	output := run(t, remotes["public"], destination)

	if _, err := os.Stat(filepath.Join(destination, "secrets")); !os.IsNotExist(err) {
		t.Fatalf("the unborn repository wrote files: %v", err)
	}
	if !strings.Contains(output, `Repository "private" has no commit yet`) {
		t.Fatalf("output does not report the unborn repository:\n%s", output)
	}
	if !strings.Contains(output, config.LocalFile) {
		t.Fatalf("output does not mention the local override limitation:\n%s", output)
	}
	healthy(t, destination)
}

func TestCloneResolvesRelativeRemotesOfEveryRepository(t *testing.T) {
	parent := temporary(t)
	remotes := filepath.Join(parent, "remotes")
	if err := os.Mkdir(remotes, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"public", "private", "upstream"} {
		if _, err := git.Run(remotes, "init", "--bare", "--quiet", "--initial-branch=main", name+".git"); err != nil {
			t.Fatal(err)
		}
	}
	origin := "../remotes/public.git"
	upstream := "../remotes/upstream.git"
	// private is a secondary repository with a relative origin. It is fetched
	// while the project still lives in the temporary directory, so only a
	// resolution from the destination can reach it.
	seed(t, filepath.Join(remotes, "public.git"), map[string]string{
		".gitignore": ".gitone/\n.gitone.local.yml\n",
		"README.md":  "# Website\n",
		config.PublicFile: fmt.Sprintf(`version: 1
default_branch: main
repositories:
  private:
    remotes:
      origin: ../remotes/private.git
    visibility: private
    paths:
      - secrets/**
  public:
    remotes:
      origin: %s
      upstream: %s
    visibility: public
    paths:
      - .gitignore
      - .gitone.yml
      - README.md
`, origin, upstream),
	})
	seed(t, filepath.Join(remotes, "private.git"), map[string]string{"secrets/token": "s3cret\n"})

	destination := filepath.Join(parent, "project")
	caller := filepath.Join(parent, "caller")
	if err := os.Mkdir(caller, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := clone.Clone(origin, destination, caller, nil, new(bytes.Buffer)); err != nil {
		t.Fatal(err)
	}
	if read(t, destination, "secrets/token") != "s3cret\n" {
		t.Fatalf("secrets/token holds %q", read(t, destination, "secrets/token"))
	}
	for _, name := range []string{"public", "private"} {
		tracks, err := repository.TracksOriginBranch(destination, name, "main")
		if err != nil || !tracks {
			t.Fatalf("%s does not track origin/main: %v", name, err)
		}
	}
	// The committed URLs stay exactly as they are written, so the published
	// project keeps resolving them itself.
	for name, want := range map[string]string{"origin": origin, "upstream": upstream} {
		got, err := git.Run(destination, "--git-dir="+repository.Directory(destination, "public"),
			"config", "--get", "remote."+name+".url")
		if err != nil || strings.TrimSpace(got) != want {
			t.Fatalf("remote %s = %q, want %q: %v", name, strings.TrimSpace(got), want, err)
		}
	}
	healthy(t, destination)

	// The same unchanged configuration reaches every remote from the clone.
	loaded, root, err := config.Load(destination)
	if err != nil {
		t.Fatal(err)
	}
	if err := fetch.Fetch(loaded, root, "all", nil, new(bytes.Buffer)); err != nil {
		t.Fatal(err)
	}
}

func TestCloneRefusesOriginWithoutDefaultBranch(t *testing.T) {
	remotes := origins(t)
	seed(t, remotes["public"], bootstrap(remotes["public"], remotes["private"]))
	destination := filepath.Join(temporary(t), "website")

	err := clone.Clone(remotes["public"], destination, filepath.Dir(destination), nil, new(bytes.Buffer))
	if err == nil || !strings.Contains(err.Error(), "has no origin/main to check out") {
		t.Fatalf("error is %v, want missing default branch", err)
	}
	refused(t, destination)
}

func TestCloneRefusesReservedPathInHistory(t *testing.T) {
	remotes := origins(t)
	seed(t, remotes["public"], bootstrap(remotes["public"], ""))

	work := temporary(t)
	if _, err := git.Run(work, "clone", "--quiet", remotes["public"], work); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, config.LocalFile), []byte("version: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, arguments := range [][]string{{"add", "--force", config.LocalFile}, {"commit", "--quiet", "-m", "Add reserved path"}} {
		if _, err := git.Run(work, arguments...); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(filepath.Join(work, config.LocalFile)); err != nil {
		t.Fatal(err)
	}
	for _, arguments := range [][]string{{"add", "-A"}, {"commit", "--quiet", "-m", "Remove reserved path"},
		{"push", "--quiet", "origin", "HEAD:refs/heads/main"}} {
		if _, err := git.Run(work, arguments...); err != nil {
			t.Fatal(err)
		}
	}

	destination := filepath.Join(temporary(t), "website")
	err := clone.Clone(remotes["public"], destination, filepath.Dir(destination), nil, new(bytes.Buffer))
	if err == nil || !strings.Contains(err.Error(), "contains reserved GitOne paths") {
		t.Fatalf("error is %v, want reserved history", err)
	}
	refused(t, destination)
}

func TestCloneRefusesUnexpectedBootstrapConfiguration(t *testing.T) {
	remotes := origins(t)
	other := origins(t)
	for _, unexpected := range []struct {
		name      string
		committed map[string]string
		message   string
	}{
		{
			name:      "different origin",
			committed: bootstrap(other["public"], remotes["private"]),
			message:   "whose configured origin is",
		},
		{
			name: "unowned configuration",
			committed: map[string]string{
				".gitignore": ".gitone/\n.gitone.local.yml\n",
				"README.md":  "# Website\n",
				config.PublicFile: fmt.Sprintf(`version: 1
default_branch: main
repositories:
  public:
    remote: %s
    visibility: public
    paths:
      - .gitignore
      - README.md
`, remotes["public"]),
			},
			message: "exactly one repository must own it",
		},
	} {
		t.Run(unexpected.name, func(t *testing.T) {
			bare := origins(t)["public"]
			seed(t, bare, unexpected.committed)
			destination := filepath.Join(temporary(t), "website")

			err := clone.Clone(bare, destination, filepath.Dir(destination), nil, new(bytes.Buffer))
			if err == nil || !strings.Contains(err.Error(), unexpected.message) {
				t.Fatalf("error is %v, want %q", err, unexpected.message)
			}
			refused(t, destination)
		})
	}
}

func TestCloneRefusesRemoteTreeWithForeignPaths(t *testing.T) {
	remotes := origins(t)
	seed(t, remotes["public"], bootstrap(remotes["public"], remotes["private"]))
	// README.md belongs to the public repository, so the private tree must
	// never be written into the shared working tree.
	seed(t, remotes["private"], map[string]string{"README.md": "stolen\n"})

	destination := filepath.Join(temporary(t), "website")
	err := clone.Clone(remotes["public"], destination, filepath.Dir(destination), nil, new(bytes.Buffer))
	if err == nil || !strings.Contains(err.Error(), `repository "private" must not manage README.md`) {
		t.Fatalf("error is %v, want a refused foreign path", err)
	}
	refused(t, destination)
}

func TestCloneRefusesExistingDestinationAndUnreachableBootstrap(t *testing.T) {
	remotes := origins(t)
	seed(t, remotes["public"], bootstrap(remotes["public"], remotes["private"]))

	existing := temporary(t)
	err := clone.Clone(remotes["public"], existing, filepath.Dir(existing), nil, new(bytes.Buffer))
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("error is %v, want a refused existing destination", err)
	}
	if entries, readErr := os.ReadDir(existing); readErr != nil || len(entries) != 0 {
		t.Fatalf("the existing destination was changed: %v %v", entries, readErr)
	}

	destination := filepath.Join(temporary(t), "website")
	err = clone.Clone(filepath.Join(temporary(t), "missing.git"), destination, filepath.Dir(destination), nil, new(bytes.Buffer))
	if err == nil || !strings.Contains(err.Error(), "could not be cloned") {
		t.Fatalf("error is %v, want a refused bootstrap URL", err)
	}
	refused(t, destination)
}

// run clones and fails the test when the clone does not succeed.
func run(t *testing.T, url, destination string) string {
	t.Helper()
	output := new(bytes.Buffer)
	if err := clone.Clone(url, destination, filepath.Dir(destination), nil, output); err != nil {
		t.Fatal(err)
	}
	return output.String()
}

// refused checks that a failed clone left neither the destination nor a
// temporary directory of its own behind.
func refused(t *testing.T, destination string) {
	t.Helper()
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatalf("the destination exists after a failure: %v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(destination))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".gitone-clone-") {
			t.Fatalf("a temporary clone directory remains: %s", entry.Name())
		}
	}
}

// healthy checks that the cloned project passes the readiness and project
// checks every other command runs.
func healthy(t *testing.T, destination string) {
	t.Helper()
	loaded, root, err := config.Load(destination)
	if err != nil {
		t.Fatal(err)
	}
	if root != destination {
		t.Fatalf("root is %s, want %s", root, destination)
	}
	if err := repository.Ready(loaded, root); err != nil {
		t.Fatal(err)
	}
	if _, _, err := status.Validate(loaded, root); err != nil {
		t.Fatal(err)
	}
	if issues := repository.ReservedIssues(loaded, root); len(issues) != 0 {
		t.Fatal(issues)
	}
}

// bootstrap is the committed content of a bootstrap repository. An empty
// remote leaves that repository without a configured origin.
func bootstrap(public, private string) map[string]string {
	remote := func(url string) string {
		if url == "" {
			return ""
		}
		return "remote: " + url
	}
	return map[string]string{
		".gitignore":      ".gitone/\n.gitone.local.yml\n",
		"README.md":       "# Website\n",
		config.PublicFile: fmt.Sprintf(configuration, remote(private), remote(public)),
	}
}

// origins creates the bare repositories a project is cloned from.
func origins(t *testing.T) map[string]string {
	t.Helper()
	for _, name := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(name, "GitOne")
	}
	for _, name := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(name, "gitone@example.com")
	}
	parent := temporary(t)
	remotes := map[string]string{}
	for _, name := range []string{"private", "public"} {
		directory := filepath.Join(parent, name+".git")
		if _, err := git.Run(parent, "init", "--bare", "--quiet", "--initial-branch=main", directory); err != nil {
			t.Fatal(err)
		}
		remotes[name] = directory
	}
	return remotes
}

// seed adds one commit with the given files to a bare repository, the way a
// contributor with a plain Git clone would.
func seed(t *testing.T, bare string, files map[string]string) {
	t.Helper()
	work := temporary(t)
	if _, err := git.Run(work, "clone", "--quiet", bare, work); err != nil {
		t.Fatal(err)
	}
	for name, contents := range files {
		full := filepath.Join(work, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, arguments := range [][]string{{"add", "-A"}, {"commit", "--quiet", "-m", "Seed"},
		{"push", "--quiet", "origin", "HEAD:refs/heads/main"}} {
		if _, err := git.Run(work, arguments...); err != nil {
			t.Fatal(err)
		}
	}
}

func log(t *testing.T, root, name string) []string {
	t.Helper()
	output, err := git.Run(root, "--git-dir="+repository.Directory(root, name), "log", "--format=%H")
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(output)
}

func read(t *testing.T, root, path string) string {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
	if err != nil {
		t.Fatal(err)
	}
	return string(contents)
}

func temporary(t *testing.T) string {
	t.Helper()
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return directory
}

// seedLink adds one commit whose tree carries a symbolic link, which GitOne
// never lets native Git materialize.
func seedLink(t *testing.T, bare, name, target string) {
	t.Helper()
	work := temporary(t)
	if _, err := git.Run(work, "clone", "--quiet", bare, work); err != nil {
		t.Fatal(err)
	}
	full := filepath.Join(work, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, full); err != nil {
		t.Fatal(err)
	}
	for _, arguments := range [][]string{{"add", name}, {"commit", "--quiet", "-m", "Seed link"},
		{"push", "--quiet", "origin", "HEAD:refs/heads/main"}} {
		if _, err := git.Run(work, arguments...); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCloneMaterializesASafeAlias(t *testing.T) {
	remotes := origins(t)
	seed(t, remotes["public"], bootstrap(remotes["public"], remotes["private"]))
	seed(t, remotes["public"], map[string]string{"src/site.css": "body {}\n"})
	seed(t, remotes["private"], map[string]string{"secrets/token": "s3cret\n"})
	// src/alias -> site.css stays inside the project and belongs to the same
	// repository, so it is checked out as a link.
	seedLink(t, remotes["public"], "src/alias", "site.css")

	destination := filepath.Join(temporary(t), "website")
	run(t, remotes["public"], destination)
	target, err := os.Readlink(filepath.Join(destination, "src", "alias"))
	if err != nil || target != "site.css" {
		t.Fatalf("src/alias = %q, %v, want the link target site.css", target, err)
	}
	healthy(t, destination)
}

func TestCloneRefusesRemoteTreeWithASymbolicLink(t *testing.T) {
	remotes := origins(t)
	seed(t, remotes["public"], bootstrap(remotes["public"], remotes["private"]))
	// secrets/link is owned by private, so only the entry mode can refuse it.
	seedLink(t, remotes["private"], "secrets/link", "/etc/passwd")

	destination := filepath.Join(temporary(t), "website")
	err := clone.Clone(remotes["public"], destination, filepath.Dir(destination), nil, new(bytes.Buffer))
	// The preflight message proves the link was refused before checkout, not
	// found in the working tree afterwards.
	if err == nil || !strings.Contains(err.Error(),
		`repository "private" must not manage secrets/link`) ||
		!strings.Contains(err.Error(), "absolute link targets are not supported") {
		t.Fatalf("error is %v, want a refused symbolic link", err)
	}
	refused(t, destination)
}
