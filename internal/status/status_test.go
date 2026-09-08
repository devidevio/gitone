package status_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/devidevio/gitone/internal/config"
	"github.com/devidevio/gitone/internal/git"
	"github.com/devidevio/gitone/internal/lock"
	"github.com/devidevio/gitone/internal/repository"
	"github.com/devidevio/gitone/internal/status"
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

func project(t *testing.T, files map[string]string) (*config.Config, string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	write(t, root, ".gitone.yml", twoRepositories)
	for name, contents := range files {
		write(t, root, name, contents)
	}
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
	gitDirectory := repository.Directory(root, name)
	arguments = append([]string{"--git-dir=" + gitDirectory, "--work-tree=" + root,
		"-c", "user.name=GitOne", "-c", "user.email=gitone@example.com"}, arguments...)
	output, err := git.Run(root, arguments...)
	if err != nil {
		t.Fatal(err)
	}
	return output
}

func collect(t *testing.T, configuration *config.Config, root string) *status.Status {
	t.Helper()
	result, err := status.Collect(configuration, root)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func changeSet(result *status.Status) []string {
	records := make([]string, 0, len(result.Changes))
	for _, change := range result.Changes {
		record := change.Repository + " " + change.State + " " + change.Type + " " + change.Path
		if change.From != "" {
			record += " <- " + change.From
		}
		records = append(records, record)
	}
	return records
}

func equal(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestCollectReportsEveryChangeKind(t *testing.T) {
	configuration, root := project(t, map[string]string{
		"README.md":         "readme\n",
		"src/keep.go":       "keep\n",
		"src/gone.go":       "gone\n",
		"src/old.go":        "old\n",
		"secrets/notes.md":  "notes\n",
		"secrets/plans.md":  "plans\n",
		"src/with space.go": "spaced\n",
	})

	run(t, root, "public", "add", "README.md", ".gitignore", ".gitone.yml", "src/keep.go", "src/gone.go", "src/old.go", "src/with space.go")
	run(t, root, "public", "commit", "-qm", "initial")
	run(t, root, "private", "add", "secrets/notes.md")
	run(t, root, "private", "commit", "-qm", "initial")

	// staged rename, staged addition, unstaged modification, deletion and an
	// untracked path, in one working tree shared by both repositories.
	run(t, root, "public", "mv", "src/old.go", "src/new.go")
	write(t, root, "src/keep.go", "changed\n")
	if err := os.Remove(filepath.Join(root, "src/gone.go")); err != nil {
		t.Fatal(err)
	}
	write(t, root, "src/added.go", "added\n")
	run(t, root, "public", "add", "src/added.go")
	write(t, root, "src/fresh.go", "fresh\n")

	result := collect(t, configuration, root)
	if len(result.Issues) != 0 {
		t.Fatalf("issues = %+v", result.Issues)
	}
	equal(t, changeSet(result), []string{
		"private untracked added secrets/plans.md",
		"public staged added src/added.go",
		"public staged renamed src/new.go <- src/old.go",
		"public unstaged deleted src/gone.go",
		"public unstaged modified src/keep.go",
		"public untracked added src/fresh.go",
	})

	if len(result.Repositories) != 2 || result.Repositories[0].Name != "private" || result.Repositories[1].Name != "public" {
		t.Fatalf("repositories = %+v", result.Repositories)
	}
	if result.Repositories[1].Visibility != "public" || result.Repositories[1].Branch != "main" {
		t.Fatalf("public = %+v", result.Repositories[1])
	}
	// No push happened, so no remote-tracking ref exists and the counts are
	// unknown rather than zero.
	if result.Repositories[1].Ahead != nil || result.Repositories[1].Behind != nil {
		t.Fatalf("ahead/behind = %v/%v, want unknown", result.Repositories[1].Ahead, result.Repositories[1].Behind)
	}

	var human bytes.Buffer
	result.WriteHuman(&human)
	for _, want := range []string{
		"PRIVATE\n", "PUBLIC\n",
		"renamed src/old.go -> src/new.go",
		"untracked: src/fresh.go",
		"BRANCHES\n  private: main (no remote-tracking branch)",
	} {
		if !strings.Contains(human.String(), want) {
			t.Fatalf("human output is missing %q:\n%s", want, human.String())
		}
	}
}

func TestWriteHumanReportsVisibility(t *testing.T) {
	result := status.Status{Repositories: []status.Repository{{Name: "backend", Visibility: "private", Branch: "main"}}}

	var output bytes.Buffer
	result.WriteHuman(&output)
	if !strings.Contains(output.String(), "BACKEND\n  visibility: private\n") {
		t.Fatalf("human output = %q", output.String())
	}
}

func TestCollectIsDeterministicAndReadOnly(t *testing.T) {
	configuration, root := project(t, map[string]string{
		"README.md":        "readme\n",
		"src/a.go":         "a\n",
		"secrets/notes.md": "notes\n",
	})
	run(t, root, "public", "add", "README.md", ".gitignore", ".gitone.yml", "src/a.go")
	run(t, root, "public", "commit", "-qm", "initial")
	write(t, root, "src/a.go", "changed\n")

	before := snapshot(t, root)
	var first, second bytes.Buffer
	if err := collect(t, configuration, root).WriteJSON(&first); err != nil {
		t.Fatal(err)
	}
	if err := collect(t, configuration, root).WriteJSON(&second); err != nil {
		t.Fatal(err)
	}
	if first.String() != second.String() {
		t.Fatalf("output differs between runs:\n%s\n%s", first.String(), second.String())
	}
	if after := snapshot(t, root); after != before {
		t.Fatalf("status modified project state:\n%s\n%s", before, after)
	}

	var document struct {
		Version      int `json:"version"`
		Repositories []struct {
			Name   string `json:"name"`
			Ahead  *int   `json:"ahead"`
			Behind *int   `json:"behind"`
		} `json:"repositories"`
		Changes []struct {
			Repository string `json:"repository"`
			State      string `json:"state"`
			Path       string `json:"path"`
		} `json:"changes"`
		Issues []status.Issue `json:"issues"`
	}
	if err := json.Unmarshal(first.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	if document.Version != status.Version || len(document.Repositories) != 2 {
		t.Fatalf("document = %+v", document)
	}
	if len(document.Changes) != 2 || document.Changes[0].Path != "secrets/notes.md" ||
		document.Changes[1].Repository != "public" || document.Changes[1].State != status.Unstaged ||
		document.Changes[1].Path != "src/a.go" {
		t.Fatalf("changes = %+v", document.Changes)
	}
	if document.Issues == nil || len(document.Issues) != 0 {
		t.Fatalf("issues = %v, want an empty array", document.Issues)
	}
}

// snapshot records the mutable Git state of the project so a read-only
// command can be proven not to touch it.
func snapshot(t *testing.T, root string) string {
	t.Helper()
	var builder strings.Builder
	err := filepath.Walk(filepath.Join(root, ".gitone"), func(name string, info os.FileInfo, err error) error {
		// Git's own transient locks are not project state. Auto maintenance
		// runs detached after a command, so objects/maintenance.lock appears
		// and vanishes on a schedule of its own. This has to come before the
		// error is read: the file can go away between this walk's readdir and
		// its lstat, and then it arrives here as an error rather than as a
		// file - which is a race against a background process, not something
		// status did.
		if strings.HasSuffix(name, ".lock") {
			return nil
		}
		if err != nil || info.IsDir() {
			return err
		}
		contents, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		builder.WriteString(name + " " + info.ModTime().String() + " " + string(contents) + "\n")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return builder.String()
}

func TestCollectReportsUnassignedAmbiguousAndMisownedPaths(t *testing.T) {
	configuration, root := project(t, map[string]string{
		"README.md":        "readme\n",
		"secrets/notes.md": "notes\n",
	})
	// Both problems appear after initialization: an unassigned file and a
	// path the public repository tracks while the private one owns it.
	write(t, root, "stray.txt", "stray\n")
	run(t, root, "public", "add", "-f", "secrets/notes.md")

	result := collect(t, configuration, root)
	var codes []string
	for _, issue := range result.Issues {
		codes = append(codes, issue.Code+" "+issue.Path)
	}
	equal(t, codes, []string{"PATH003 secrets/notes.md", "PATH001 stray.txt"})
}

func TestDeletedPathOwnershipDoesNotHideUnsafePaths(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		code   string
		rename bool
	}{
		{name: "existing", code: "PATH001"},
		{name: "recreated", code: "PATH001"},
		{name: "dangling-link", code: "PATH003"},
		{name: "protected", code: "PATH003"},
		{name: "reassigned", code: "PATH003"},
		{name: "ambiguous", code: "PATH002"},
		{name: "tracked-twice", code: "PATH003"},
		// Staging the deletion together with a new file makes Git infer a
		// rename, which reaches the same exception through the rename source.
		{name: "recreated", code: "PATH001", rename: true},
		{name: "protected", code: "PATH003", rename: true},
		{name: "reassigned", code: "PATH003", rename: true},
		{name: "ambiguous", code: "PATH002", rename: true},
		{name: "tracked-twice", code: "PATH003", rename: true},
	} {
		label := scenario.name
		if scenario.rename {
			label = "renamed-" + scenario.name
		}
		t.Run(label, func(t *testing.T) {
			_, root := project(t, map[string]string{"README.md": "readme\n"})
			run(t, root, "public", "add", "README.md")
			run(t, root, "public", "commit", "-qm", "Initial")
			if scenario.name == "tracked-twice" {
				run(t, root, "private", "add", "README.md")
			}
			if scenario.name != "existing" {
				if err := os.Remove(filepath.Join(root, "README.md")); err != nil {
					t.Fatal(err)
				}
			}
			if scenario.rename {
				write(t, root, "src/readme.md", "readme\n")
				run(t, root, "public", "add", "-A", "--", "README.md", "src/readme.md")
			}
			contents := strings.Replace(twoRepositories, "      - README.md\n", "", 1)
			switch scenario.name {
			case "recreated", "dangling-link":
				run(t, root, "public", "add", "-u")
				if scenario.name == "recreated" {
					write(t, root, "README.md", "recreated\n")
				} else if err := os.Symlink("missing", filepath.Join(root, "README.md")); err != nil {
					t.Fatal(err)
				}
			case "protected":
				contents += "rules:\n  protected_paths: [README.md]\n"
			case "reassigned":
				contents = strings.Replace(contents, "      - secrets/**", "      - secrets/**\n      - README.md", 1)
			case "ambiguous":
				contents = strings.Replace(twoRepositories, "      - secrets/**", "      - secrets/**\n      - README.*", 1)
			}
			write(t, root, ".gitone.yml", contents)
			configuration, _, err := config.Load(root)
			if err != nil {
				t.Fatal(err)
			}
			result := collect(t, configuration, root)
			// Without an inferred rename the scenario would silently repeat
			// the plain deletion above instead of exercising the rename source.
			if scenario.rename {
				inferred := false
				for _, change := range result.Changes {
					inferred = inferred || change.From == "README.md"
				}
				if !inferred {
					t.Fatalf("changes = %+v, want a rename from README.md", result.Changes)
				}
			}
			for _, issue := range result.Issues {
				if issue.Path == "README.md" && issue.Code == scenario.code {
					return
				}
			}
			t.Fatalf("issues = %+v, want %s README.md", result.Issues, scenario.code)
		})
	}
}

func TestCollectReportsBranchDisagreement(t *testing.T) {
	configuration, root := project(t, map[string]string{
		"README.md":        "readme\n",
		"secrets/notes.md": "notes\n",
	})
	run(t, root, "private", "add", "secrets/notes.md")
	run(t, root, "private", "commit", "-qm", "initial")
	run(t, root, "private", "checkout", "-q", "-b", "other")

	result := collect(t, configuration, root)
	if len(result.Issues) != 1 || result.Issues[0].Code != "REPO001" ||
		!strings.Contains(result.Issues[0].Detail, "other") {
		t.Fatalf("issues = %+v", result.Issues)
	}
}

func TestCollectReportsUninitializedRepository(t *testing.T) {
	configuration, root := project(t, map[string]string{
		"README.md":        "readme\n",
		"secrets/notes.md": "notes\n",
	})
	if err := os.RemoveAll(repository.Directory(root, "private")); err != nil {
		t.Fatal(err)
	}

	result := collect(t, configuration, root)
	if len(result.Repositories) != 1 || result.Repositories[0].Name != "public" {
		t.Fatalf("repositories = %+v", result.Repositories)
	}
	if len(result.Issues) != 1 || result.Issues[0].Code != "REPO001" {
		t.Fatalf("issues = %+v", result.Issues)
	}
	// The unknown repository gets no change list, so nothing claims to know
	// the state of the paths it owns.
	for _, change := range result.Changes {
		if change.Repository == "private" {
			t.Fatalf("change for an uninitialized repository: %+v", change)
		}
	}
}

func TestCollectReportsUnreadableRepository(t *testing.T) {
	configuration, root := project(t, map[string]string{
		"README.md":        "readme\n",
		"secrets/notes.md": "notes\n",
	})
	if err := os.WriteFile(filepath.Join(repository.Directory(root, "private"), "HEAD"), []byte("broken"), 0o644); err != nil {
		t.Fatal(err)
	}

	result := collect(t, configuration, root)
	if len(result.Repositories) != 1 || result.Repositories[0].Name != "public" {
		t.Fatalf("repositories = %+v", result.Repositories)
	}
	if len(result.Issues) != 1 || result.Issues[0].Code != "REPO001" ||
		!strings.Contains(result.Issues[0].Detail, "cannot be read") {
		t.Fatalf("issues = %+v", result.Issues)
	}
	// The working tree of the readable repository is still reported.
	equal(t, changeSet(result), []string{
		"public untracked added .gitignore",
		"public untracked added .gitone.yml",
		"public untracked added README.md",
	})
}

func TestCollectRefusesToReadDuringAnActiveOperation(t *testing.T) {
	configuration, root := project(t, map[string]string{
		"README.md":        "readme\n",
		"secrets/notes.md": "notes\n",
	})
	release, err := lock.Acquire(root, "gitone commit")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	if _, err := status.Collect(configuration, root); err == nil || !strings.HasPrefix(err.Error(), "LOCK001") {
		t.Fatalf("error = %v, want LOCK001", err)
	}
}

func TestPorcelainKeepsSpacesInPathsUnambiguous(t *testing.T) {
	configuration, root := project(t, map[string]string{
		"README.md":         "readme\n",
		"src/with space.go": "spaced\n",
		"secrets/notes.md":  "notes\n",
	})

	var output bytes.Buffer
	collect(t, configuration, root).WritePorcelain(&output)
	lines := strings.Split(strings.TrimSuffix(output.String(), "\n"), "\n")
	if lines[0] != "version\t1" {
		t.Fatalf("first record = %q", lines[0])
	}

	found := false
	for _, line := range lines {
		fields := strings.Split(line, "\t")
		switch fields[0] {
		case "repository":
			if len(fields) != 6 || fields[4] != "?" || fields[5] != "?" {
				t.Fatalf("repository record = %q", line)
			}
		case "change":
			if len(fields) != 6 {
				t.Fatalf("change record = %q", line)
			}
			if fields[4] == "src/with space.go" {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("path with a space is missing:\n%s", output.String())
	}
}

func TestCollectReportsKnownAheadBehind(t *testing.T) {
	configuration, root := project(t, map[string]string{
		"README.md":        "readme\n",
		"secrets/notes.md": "notes\n",
	})
	run(t, root, "public", "add", "README.md", ".gitignore", ".gitone.yml")
	run(t, root, "public", "commit", "-qm", "initial")

	// A remote-tracking ref only exists after a push, which is what makes the
	// counts known instead of unknown.
	remote := filepath.Join(t.TempDir(), "remote.git")
	if _, err := git.Run(root, "init", "--bare", "--quiet", remote); err != nil {
		t.Fatal(err)
	}
	run(t, root, "public", "remote", "add", "backup", remote)
	run(t, root, "public", "push", "-q", "-u", "backup", "main")
	write(t, root, "README.md", "changed\n")
	run(t, root, "public", "commit", "-qam", "second")

	result := collect(t, configuration, root)
	public := result.Repositories[1]
	if public.Ahead == nil || *public.Ahead != 1 || public.Behind == nil || *public.Behind != 0 {
		t.Fatalf("ahead/behind = %v/%v, want 1/0", public.Ahead, public.Behind)
	}

	var output bytes.Buffer
	result.WritePorcelain(&output)
	if !strings.Contains(output.String(), "repository\tpublic\tpublic\tmain\t1\t0\n") {
		t.Fatalf("porcelain = %q", output.String())
	}
}

func TestCollectReportsIgnoredPaths(t *testing.T) {
	configuration, root := project(t, map[string]string{
		".gitignore":          "danger*\nsrc/build/\n",
		"README.md":           "readme\n",
		"danger\nissue\tfake": "ignored\n",
		"src/main.go":         "a\n",
		"src/build/out.o":     "ignored\n",
		"src/build/keep.txt":  "tracked but ignored\n",
	})
	run(t, root, "public", "add", "-f", "src/build/keep.txt")

	// Repository initialization adds GitOne's own directory to .gitignore, so
	// the reported listing holds it next to the project's own ignored tree.
	want := []string{".gitone/", "danger\nissue\tfake", "src/build/"}
	result := collect(t, configuration, root)
	if !reflect.DeepEqual(result.Ignored, want) {
		t.Fatalf("ignored = %#v, want %#v", result.Ignored, want)
	}

	var document bytes.Buffer
	if err := result.WriteJSON(&document); err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Ignored          []string `json:"ignored"`
		IgnoreExceptions []string `json:"ignore_exceptions"`
	}
	if err := json.Unmarshal(document.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded.Ignored, want) {
		t.Fatalf("json ignored = %#v, want %#v", decoded.Ignored, want)
	}
	wantExceptions := []string{"src/build/keep.txt"}
	if !reflect.DeepEqual(result.IgnoreExceptions, wantExceptions) {
		t.Fatalf("ignore exceptions = %#v, want %#v", result.IgnoreExceptions, wantExceptions)
	}
	if !reflect.DeepEqual(decoded.IgnoreExceptions, wantExceptions) {
		t.Fatalf("json ignore exceptions = %#v, want %#v", decoded.IgnoreExceptions, wantExceptions)
	}

	var porcelain bytes.Buffer
	result.WritePorcelain(&porcelain)
	if strings.Contains(porcelain.String(), "ignored\t") ||
		strings.Contains(porcelain.String(), "\nissue\tfake\n") {
		t.Fatalf("porcelain = %q", porcelain.String())
	}
}
