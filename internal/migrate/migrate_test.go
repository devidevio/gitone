package migrate_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/devidevio/gitone/internal/config"
	"github.com/devidevio/gitone/internal/git"
	"github.com/devidevio/gitone/internal/migrate"
	"github.com/devidevio/gitone/internal/repository"
)

const twoRepositories = `
version: 1
default_branch: main
repositories:
  notes:
    visibility: private
    paths:
      - notes/**
  website:
    visibility: public
    remote: git@example.com:website.git
    paths:
      - .gitignore
      - .gitone.yml
      - README.md
      - src/**
`

// project creates a configured project whose root is one ordinary Git
// repository with one commit.
func project(t *testing.T, files map[string]string) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	write(t, root, ".gitone.yml", twoRepositories)
	for name, contents := range files {
		write(t, root, name, contents)
	}
	root0(t, root, "init", "--quiet", "--initial-branch=main")
	root0(t, root, "-c", "user.name=GitOne", "-c", "user.email=gitone@example.com", "add", "-A")
	root0(t, root, "-c", "user.name=GitOne", "-c", "user.email=gitone@example.com", "commit", "--quiet", "-m", "initial")
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

// root0 runs git against the root repository of the project.
func root0(t *testing.T, root string, arguments ...string) string {
	t.Helper()
	output, err := git.Run(root, arguments...)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(output)
}

func run(t *testing.T, root, answer string, yes bool) (string, error) {
	t.Helper()
	configuration, discovered, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	output := new(bytes.Buffer)
	err = migrate.Migrate(configuration, discovered, yes, true, strings.NewReader(answer), output)
	return output.String(), err
}

// snapshot records every project file outside .gitone/ and .git/.
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
			if relative == ".gitone" || relative == ".git" {
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

func backups(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, ".gitone", "migration-backup"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, filepath.Join(root, ".gitone", "migration-backup", entry.Name()))
	}
	return names
}

func recordInterruption(t *testing.T, root, backup string, repositories []string, ignore []byte) {
	t.Helper()
	relative, err := filepath.Rel(root, backup)
	if err != nil {
		t.Fatal(err)
	}
	contents, err := json.Marshal(struct {
		Version      int      `json:"version"`
		Command      string   `json:"command"`
		Backup       string   `json:"backup"`
		Repositories []string `json:"repositories"`
		Ignore       []byte   `json:"ignore"`
	}{1, "gitone migrate", filepath.ToSlash(relative), repositories, ignore})
	if err != nil {
		t.Fatal(err)
	}
	write(t, root, ".gitone/migration/state.json", string(contents))
}

func exists(t *testing.T, name string) bool {
	t.Helper()
	_, err := os.Lstat(name)
	return err == nil
}

func standard() map[string]string {
	return map[string]string{
		"README.md":     "# readme\n",
		"src/site.css":  "body {}\n",
		"notes/plan.md": "# plan\n",
		".gitignore":    ".gitone/\n.gitone.local.yml\n",
	}
}

func TestMigrateConvertsCleanProject(t *testing.T) {
	root := project(t, standard())
	before := snapshot(t, root)
	head := root0(t, root, "rev-parse", "HEAD")

	output, err := run(t, root, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if exists(t, filepath.Join(root, ".git")) {
		t.Error("the root repository was not removed")
	}
	for name, contents := range snapshot(t, root) {
		if before[name] != contents {
			t.Errorf("file %s changed: %q, want %q", name, contents, before[name])
		}
	}
	if len(snapshot(t, root)) != len(before) {
		t.Error("the working tree gained or lost files")
	}

	configuration, _, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.Ready(configuration, root); err != nil {
		t.Fatal(err)
	}
	for _, name := range configuration.RepositoryNames() {
		if issues := repository.Check(root, name, configuration.Repositories[name]); len(issues) != 0 {
			t.Errorf("repository %s: %v", name, issues)
		}
		gitDirectory := repository.Directory(root, name)
		if commits, err := git.Run(root, "--git-dir="+gitDirectory, "rev-list", "--all", "--count"); err != nil || strings.TrimSpace(commits) != "0" {
			t.Errorf("repository %s has commits: %q %v", name, commits, err)
		}
		if staged, err := git.Run(root, "--git-dir="+gitDirectory, "--work-tree="+root, "diff", "--cached", "--name-only"); err != nil || strings.TrimSpace(staged) != "" {
			t.Errorf("repository %s has staged files: %q %v", name, staged, err)
		}
	}

	// The backup stays and still holds the original history.
	saved := backups(t, root)
	if len(saved) != 1 {
		t.Fatalf("got %d backups, want 1", len(saved))
	}
	if got := root0(t, saved[0], "--git-dir="+saved[0], "rev-parse", "HEAD"); got != head {
		t.Errorf("backup HEAD is %s, want %s", got, head)
	}
	if strings.Contains(output, "WARNING") {
		t.Errorf("a clean tree was reported as dirty:\n%s", output)
	}
	if !strings.Contains(output, filepath.Join(".gitone", "migration-backup", filepath.Base(saved[0]))) {
		t.Errorf("the backup location is not reported:\n%s", output)
	}
	if exists(t, filepath.Join(root, ".gitone", "migration")) {
		t.Error("migration state was kept")
	}
}

func TestMigrateWarnsAboutDirtyTreeAndRespectsCancellation(t *testing.T) {
	root := project(t, standard())
	write(t, root, "README.md", "# changed\n")
	write(t, root, "src/new.css", "a {}\n")

	output, err := run(t, root, "n\n", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "WARNING") {
		t.Errorf("no warning for a dirty tree:\n%s", output)
	}
	if !exists(t, filepath.Join(root, ".git")) {
		t.Fatal("the cancelled migration removed the root repository")
	}
	if backups(t, root) != nil {
		t.Error("the cancelled migration wrote a backup")
	}

	if _, err := run(t, root, "y\n", false); err != nil {
		t.Fatal(err)
	}
	if exists(t, filepath.Join(root, ".git")) {
		t.Error("the confirmed migration kept the root repository")
	}
	if contents, err := os.ReadFile(filepath.Join(root, "README.md")); err != nil || string(contents) != "# changed\n" {
		t.Errorf("the working tree changed: %q %v", contents, err)
	}
}

func TestMigrateWithoutTerminalRequiresYes(t *testing.T) {
	root := project(t, standard())
	configuration, discovered, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	err = migrate.Migrate(configuration, discovered, false, false, strings.NewReader(""), new(bytes.Buffer))
	if err == nil || !strings.HasPrefix(err.Error(), "MIG001") {
		t.Fatalf("got %v, want MIG001", err)
	}
	if !exists(t, filepath.Join(root, ".git")) {
		t.Error("the refused migration removed the root repository")
	}
}

func TestMigrateRejectsUnsafeProjects(t *testing.T) {
	tests := map[string]func(t *testing.T, root string){
		"git file": func(t *testing.T, root string) {
			if err := os.RemoveAll(filepath.Join(root, ".git")); err != nil {
				t.Fatal(err)
			}
			write(t, root, ".git", "gitdir: /elsewhere\n")
		},
		"submodules": func(t *testing.T, root string) {
			write(t, root, ".gitmodules", "[submodule \"x\"]\n")
		},
		"linked worktree": func(t *testing.T, root string) {
			root0(t, root, "worktree", "add", "--quiet", "--detach", filepath.Join(t.TempDir(), "linked"))
		},
		"nested repository": func(t *testing.T, root string) {
			root0(t, filepath.Join(root, "src"), "init", "--quiet")
		},
		"unassigned path": func(t *testing.T, root string) {
			write(t, root, "stray.txt", "stray\n")
		},
		"already migrated": func(t *testing.T, root string) {
			if err := os.MkdirAll(repository.Directory(root, "notes"), 0o700); err != nil {
				t.Fatal(err)
			}
		},
		"unconfigured managed repository": func(t *testing.T, root string) {
			if err := os.MkdirAll(repository.Directory(root, "legacy"), 0o700); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, prepare := range tests {
		t.Run(name, func(t *testing.T) {
			root := project(t, standard())
			prepare(t, root)
			before := snapshot(t, root)

			if _, err := run(t, root, "", true); err == nil {
				t.Fatal("the unsafe project was migrated")
			}
			if !exists(t, filepath.Join(root, ".git")) {
				t.Error("the root repository was removed")
			}
			if backups(t, root) != nil {
				t.Error("a backup was written")
			}
			for file, contents := range snapshot(t, root) {
				if before[file] != contents {
					t.Errorf("file %s changed", file)
				}
			}
		})
	}
}

func TestMigrateRollsBackAFailedConversion(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	root := project(t, standard())
	head := root0(t, root, "rev-parse", "HEAD")

	// A read-only repositories directory makes repository creation fail after
	// the verified backup was written.
	if err := os.Mkdir(filepath.Join(root, ".gitone"), 0o700); err != nil {
		t.Fatal(err)
	}
	repositories := filepath.Join(root, ".gitone", "repositories")
	if err := os.Mkdir(repositories, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(repositories, 0o700) })

	if _, err := run(t, root, "", true); err == nil {
		t.Fatal("the failed conversion was reported as successful")
	}
	if !exists(t, filepath.Join(root, ".git")) {
		t.Fatal("the failed conversion removed the root repository")
	}
	if got := root0(t, root, "rev-parse", "HEAD"); got != head {
		t.Errorf("the root repository is at %s, want %s", got, head)
	}
	if len(backups(t, root)) != 1 {
		t.Error("the backup of the failed migration is missing")
	}
	if exists(t, filepath.Join(root, ".gitone", "migration")) {
		t.Error("migration state was kept after the rollback")
	}
}

func TestMigrateDetectsAndRestoresAnInterruptedMigration(t *testing.T) {
	root := project(t, standard())
	head := root0(t, root, "rev-parse", "HEAD")
	if _, err := run(t, root, "", true); err != nil {
		t.Fatal(err)
	}
	saved := backups(t, root)
	if len(saved) != 1 {
		t.Fatalf("got %d backups, want 1", len(saved))
	}

	// Simulate an interruption between the backup and the finished migration:
	// the state of that migration is still recorded.
	ignore, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	recordInterruption(t, root, saved[0], []string{"notes", "website"}, ignore)

	output, err := run(t, root, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "interrupted migration") {
		t.Errorf("the interruption was not reported:\n%s", output)
	}
	if got := root0(t, root, "rev-parse", "HEAD"); got != head {
		t.Errorf("the restored repository is at %s, want %s", got, head)
	}
	if len(backups(t, root)) != 1 || !exists(t, saved[0]) {
		t.Error("the backup was consumed by the restoration")
	}
	if exists(t, repository.Directory(root, "notes")) || exists(t, repository.Directory(root, "website")) {
		t.Error("the repositories of the interrupted migration were kept")
	}
	if exists(t, filepath.Join(root, ".gitone", "migration")) {
		t.Error("migration state was kept after the restoration")
	}
}

func TestRestoreNeverOverwritesAnExistingRootRepository(t *testing.T) {
	root := project(t, standard())
	if _, err := run(t, root, "", true); err != nil {
		t.Fatal(err)
	}
	saved := backups(t, root)[0]
	ignore, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	recordInterruption(t, root, saved, []string{"notes", "website"}, ignore)

	// An unrelated root repository exists again.
	root0(t, root, "init", "--quiet", "--initial-branch=other")
	unrelated := root0(t, root, "symbolic-ref", "HEAD")

	if _, err := run(t, root, "", true); err == nil || !strings.HasPrefix(err.Error(), "MIG001") {
		t.Fatalf("got %v, want MIG001", err)
	}
	if got := root0(t, root, "symbolic-ref", "HEAD"); got != unrelated {
		t.Errorf("the unrelated root repository was overwritten: %s", got)
	}
	if !exists(t, saved) {
		t.Error("the backup was removed")
	}
	if !exists(t, repository.Directory(root, "notes")) || !exists(t, repository.Directory(root, "website")) {
		t.Error("managed repositories were removed before restoration was possible")
	}
	if !exists(t, filepath.Join(root, ".gitone", "migration", "state.json")) {
		t.Error("migration state was removed before restoration was possible")
	}

	if err := os.RemoveAll(filepath.Join(root, ".git")); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, root, "", true); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreRejectsUnsafeState(t *testing.T) {
	tests := map[string]struct {
		backup       string
		repositories []string
	}{
		"backup path":     {backup: "../outside", repositories: []string{"notes", "website"}},
		"repository name": {repositories: []string{"../../../victim"}},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			root := project(t, standard())
			if _, err := run(t, root, "", true); err != nil {
				t.Fatal(err)
			}
			saved := backups(t, root)[0]
			backup := saved
			if test.backup != "" {
				backup = filepath.Join(root, test.backup)
			}
			recordInterruption(t, root, backup, test.repositories, []byte("original\n"))

			victim := filepath.Join(filepath.Dir(root), "victim")
			write(t, victim, "keep", "keep\n")
			if _, err := run(t, root, "", true); err == nil || !strings.HasPrefix(err.Error(), "MIG001") {
				t.Fatalf("got %v, want MIG001", err)
			}
			if !exists(t, filepath.Join(victim, "keep")) {
				t.Error("unsafe migration state removed a path outside the project")
			}
			if !exists(t, filepath.Join(root, ".gitone", "migration", "state.json")) {
				t.Error("unsafe migration state was removed")
			}
		})
	}
}

func TestRestoreRejectsInvalidBackup(t *testing.T) {
	root := project(t, standard())
	if _, err := run(t, root, "", true); err != nil {
		t.Fatal(err)
	}
	invalid := filepath.Join(root, ".gitone", "migration-backup", "not-a-repository")
	if err := os.Mkdir(invalid, 0o700); err != nil {
		t.Fatal(err)
	}
	recordInterruption(t, root, invalid, []string{"notes", "website"}, []byte("original\n"))

	if _, err := run(t, root, "", true); err == nil || !strings.HasPrefix(err.Error(), "MIG001") {
		t.Fatalf("got %v, want MIG001", err)
	}
	if !exists(t, repository.Directory(root, "notes")) || !exists(t, repository.Directory(root, "website")) {
		t.Error("managed repositories were removed before the backup was validated")
	}
}

func TestRestoreRestoresOriginalGitignore(t *testing.T) {
	root := project(t, map[string]string{
		"README.md":     "# readme\n",
		"src/site.css":  "body {}\n",
		"notes/plan.md": "# plan\n",
		".gitignore":    "custom\n",
	})
	original := []byte("custom\n")
	if _, err := run(t, root, "", true); err != nil {
		t.Fatal(err)
	}
	if contents, err := os.ReadFile(filepath.Join(root, ".gitignore")); err != nil || bytes.Equal(contents, original) {
		t.Fatalf("migration did not update .gitignore: %q %v", contents, err)
	}
	saved := backups(t, root)[0]
	recordInterruption(t, root, saved, []string{"notes", "website"}, original)

	if _, err := run(t, root, "", true); err != nil {
		t.Fatal(err)
	}
	if contents, err := os.ReadFile(filepath.Join(root, ".gitignore")); err != nil || !bytes.Equal(contents, original) {
		t.Fatalf("restored .gitignore = %q, want %q: %v", contents, original, err)
	}
}
