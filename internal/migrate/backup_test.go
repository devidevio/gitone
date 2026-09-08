package migrate_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/devidevio/gitone/internal/migrate"
)

// migrated returns a migrated project and the ID of the backup it created.
func migrated(t *testing.T) (string, string) {
	t.Helper()
	root := project(t, standard())
	if _, err := run(t, root, "", true); err != nil {
		t.Fatal(err)
	}
	found, err := migrate.Backups(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 {
		t.Fatalf("backups = %d, want 1", len(found))
	}
	return root, found[0].ID
}

func backupPath(root, id string) string {
	return filepath.Join(root, ".gitone", "migration-backup", id)
}

// tree records every path below directory with its contents, so an
// inspection that wrote into the backup is detected.
func tree(t *testing.T, directory string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(directory, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		relative, err := filepath.Rel(directory, path)
		if err != nil {
			return err
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

func inspect(t *testing.T, root, id string, arguments ...string) (string, string, int) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code, err := migrate.BackupGit(root, id, arguments, &stdout, &stderr)
	if err != nil {
		stderr.WriteString(err.Error())
	}
	return stdout.String(), stderr.String(), code
}

func restore(t *testing.T, root, id, answer string, yes bool) (string, error) {
	t.Helper()
	output := new(bytes.Buffer)
	err := migrate.BackupRestore(root, id, yes, true, strings.NewReader(answer), output)
	return output.String(), err
}

func TestBackupsListsOnlyRealDirectoriesNewestFirst(t *testing.T) {
	root, id := migrated(t)
	parent := filepath.Join(root, ".gitone", "migration-backup")
	for _, name := range []string{"20260101T000000Z-000001", "20270101T000000Z-000002", "not-a-time", "broken"} {
		if err := os.Mkdir(filepath.Join(parent, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(id, filepath.Join(parent, "alias")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, "note.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	found, err := migrate.Backups(root)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, backup := range found {
		ids = append(ids, backup.ID)
	}
	// Timestamped IDs newest first, the malformed ones after them by name.
	want := []string{"20270101T000000Z-000002", id, "20260101T000000Z-000001", "not-a-time", "broken"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
	for _, backup := range found {
		if valid := backup.ID == id; backup.Valid != valid {
			t.Errorf("backup %s valid = %v, want %v", backup.ID, backup.Valid, valid)
		}
		if timestamped := strings.HasPrefix(backup.ID, "202"); backup.Timestamped != timestamped {
			t.Errorf("backup %s timestamped = %v, want %v", backup.ID, backup.Timestamped, timestamped)
		}
	}

	var listing bytes.Buffer
	if err := migrate.BackupList(root, &listing); err != nil {
		t.Fatal(err)
	}
	printed := listing.String()
	for _, want := range append(ids, "valid", "invalid", "unknown", "2027-01-01 00:00:00") {
		if !strings.Contains(printed, want) {
			t.Fatalf("listing is missing %q:\n%s", want, printed)
		}
	}
	if second := new(bytes.Buffer); migrate.BackupList(root, second) != nil || second.String() != printed {
		t.Fatal("the listing is not deterministic")
	}
}

func TestBackupListReportsAnEmptyResult(t *testing.T) {
	root := project(t, standard())
	var listing bytes.Buffer
	if err := migrate.BackupList(root, &listing); err != nil {
		t.Fatal(err)
	}
	if got := listing.String(); got != "No migration backups found.\n" {
		t.Fatalf("listing = %q", got)
	}
}

func TestBackupSelectionRefusesEveryAmbiguousOrUnsafeID(t *testing.T) {
	root, id := migrated(t)
	parent := filepath.Join(root, ".gitone", "migration-backup")
	if err := os.Symlink(id, filepath.Join(parent, "alias")); err != nil {
		t.Fatal(err)
	}
	// The explicit ID still selects the one real backup beside the link.
	if _, _, code := inspect(t, root, id, "log", "--oneline"); code != 0 {
		t.Fatalf("exit code = %d for the explicit ID", code)
	}
	for _, selected := range []string{"alias", "unknown", "..", ".", "../..", "../migration/state.json", id + "/objects"} {
		t.Run(selected, func(t *testing.T) {
			_, stderr, code := inspect(t, root, selected, "log")
			if code == 0 || !strings.HasPrefix(stderr, "MIG001 ") {
				t.Fatalf("exit code = %d, stderr = %q", code, stderr)
			}
		})
	}
}

func TestBackupOperationsRefuseSymlinkedPathComponents(t *testing.T) {
	for _, component := range []string{".gitone", filepath.Join(".gitone", "migration-backup")} {
		t.Run(component, func(t *testing.T) {
			root, id := migrated(t)
			linked := filepath.Join(root, component)
			real := filepath.Join(t.TempDir(), "state")
			if err := os.Rename(linked, real); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(real, linked); err != nil {
				t.Fatal(err)
			}

			if _, err := migrate.Backups(root); err == nil || !strings.HasPrefix(err.Error(), "MIG001 ") {
				t.Fatalf("listing through %s = %v", component, err)
			}
			_, stderr, code := inspect(t, root, id, "log")
			if code == 0 || !strings.HasPrefix(stderr, "MIG001 ") {
				t.Fatalf("inspection through %s: exit code = %d, stderr = %q", component, code, stderr)
			}
		})
	}
}

func TestBackupSelectionNeedsAnIDBeyondOneBackup(t *testing.T) {
	root, id := migrated(t)
	if _, _, code := inspect(t, root, "", "log", "--oneline"); code != 0 {
		t.Fatalf("the only backup was not selected implicitly: exit code = %d", code)
	}
	if err := os.Mkdir(backupPath(root, "20260101T000000Z-000001"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, stderr, code := inspect(t, root, "", "log")
	if code == 0 || !strings.Contains(stderr, "--backup") {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr)
	}
	if _, _, code := inspect(t, root, id, "log", "--oneline"); code != 0 {
		t.Fatalf("the explicit ID was refused: exit code = %d", code)
	}
	if _, err := restore(t, root, "", "", true); err == nil {
		t.Fatal("an ambiguous restore was accepted")
	}
	if exists(t, filepath.Join(root, ".git")) {
		t.Fatal("the refused restore created a root repository")
	}
}

func TestBackupGitInspectsAgainstTheProjectWorktree(t *testing.T) {
	root, id := migrated(t)
	head := root0(t, backupPath(root, id), "--git-dir="+backupPath(root, id), "rev-parse", "HEAD")
	write(t, root, "README.md", "# changed\n")
	before := tree(t, backupPath(root, id))

	for _, test := range []struct {
		arguments []string
		want      string
	}{
		{[]string{"log", "--oneline"}, head[:7]},
		{[]string{"show", "--stat", head}, "initial"},
		{[]string{"diff", "--", "README.md"}, "+# changed"},
		{[]string{"status", "--short"}, "M README.md"},
	} {
		t.Run(test.arguments[0], func(t *testing.T) {
			stdout, stderr, code := inspect(t, root, id, test.arguments...)
			if code != 0 {
				t.Fatalf("exit code = %d, stderr = %q", code, stderr)
			}
			if !strings.Contains(stdout, test.want) {
				t.Fatalf("output = %q, want %q", stdout, test.want)
			}
		})
	}

	for name, contents := range tree(t, backupPath(root, id)) {
		if before[name] != contents {
			t.Errorf("the inspection changed the backup file %s", name)
		}
	}
	if len(tree(t, backupPath(root, id))) != len(before) {
		t.Error("the inspection added or removed a backup file")
	}
}

func TestBackupGitReportsTheGitExitCode(t *testing.T) {
	root, id := migrated(t)
	stdout, stderr, code := inspect(t, root, id, "show", "0000000000000000000000000000000000000000")
	if code == 0 || code == 1 {
		t.Fatalf("exit code = %d, want the native Git code", code)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q", stdout)
	}
	if !strings.Contains(stderr, "GIT001 ") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestBackupRestoreNeedsConfirmation(t *testing.T) {
	root, id := migrated(t)
	before := snapshot(t, root)

	output, err := restore(t, root, id, "n\n", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{id, "the ordinary GitOne commands are unavailable", "Restoration aborted."} {
		if !strings.Contains(output, want) {
			t.Fatalf("output is missing %q:\n%s", want, output)
		}
	}
	if exists(t, filepath.Join(root, ".git")) {
		t.Fatal("the aborted restore created a root repository")
	}

	if err := migrate.BackupRestore(root, id, false, false, strings.NewReader(""), new(bytes.Buffer)); err == nil ||
		!strings.Contains(err.Error(), "--yes") {
		t.Fatalf("a non-interactive restore without --yes = %v", err)
	}
	if exists(t, filepath.Join(root, ".git")) {
		t.Fatal("the refused restore created a root repository")
	}
	for name, contents := range snapshot(t, root) {
		if before[name] != contents {
			t.Errorf("file %s changed: %q, want %q", name, contents, before[name])
		}
	}
}

func TestBackupRestorePublishesAndKeepsEverythingElse(t *testing.T) {
	root, id := migrated(t)
	before := snapshot(t, root)
	backupBefore := tree(t, backupPath(root, id))
	repositories := tree(t, filepath.Join(root, ".gitone", "repositories"))

	output, err := restore(t, root, id, "y\n", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "was restored from") || !strings.Contains(output, id) {
		t.Fatalf("output = %q", output)
	}
	if root0(t, root, "rev-parse", "--is-bare-repository") != "false" {
		t.Fatal("the restored repository is not the original working-tree repository")
	}
	if got := root0(t, root, "log", "--oneline", "-n", "1"); !strings.Contains(got, "initial") {
		t.Fatalf("restored history = %q", got)
	}

	for name, contents := range snapshot(t, root) {
		if before[name] != contents {
			t.Errorf("file %s changed: %q, want %q", name, contents, before[name])
		}
	}
	for name, contents := range backupBefore {
		if tree(t, backupPath(root, id))[name] != contents {
			t.Errorf("the restore changed the backup file %s", name)
		}
	}
	for name, contents := range tree(t, filepath.Join(root, ".gitone", "repositories")) {
		if repositories[name] != contents {
			t.Errorf("the restore changed the managed repository file %s", name)
		}
	}
	if temporary := leftovers(t, root); len(temporary) != 0 {
		t.Fatalf("the restore left %v behind", temporary)
	}

	// The backup stays listable and inspectable, and a second restore refuses
	// the root repository it just published.
	if _, _, code := inspect(t, root, id, "log", "--oneline"); code != 0 {
		t.Fatalf("inspection after the restore: exit code = %d", code)
	}
	if err := migrate.BackupRestore(root, id, true, true, strings.NewReader(""), new(bytes.Buffer)); err == nil ||
		!strings.Contains(err.Error(), "already exists") {
		t.Fatalf("a second restore = %v", err)
	}
}

func TestBackupRestoreRefusesEveryExistingRootPath(t *testing.T) {
	for _, test := range []struct {
		name   string
		create func(t *testing.T, target string)
	}{
		{"file", func(t *testing.T, target string) {
			if err := os.WriteFile(target, []byte("gitdir: elsewhere\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"symlink", func(t *testing.T, target string) {
			if err := os.Symlink("elsewhere", target); err != nil {
				t.Fatal(err)
			}
		}},
		{"directory", func(t *testing.T, target string) {
			if err := os.Mkdir(target, 0o700); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, id := migrated(t)
			target := filepath.Join(root, ".git")
			test.create(t, target)
			backupBefore := tree(t, backupPath(root, id))

			err := migrate.BackupRestore(root, id, true, true, strings.NewReader(""), new(bytes.Buffer))
			if err == nil || !strings.Contains(err.Error(), "already exists") {
				t.Fatalf("restore = %v", err)
			}
			if len(tree(t, backupPath(root, id))) != len(backupBefore) {
				t.Error("the refused restore changed the backup")
			}
			if temporary := leftovers(t, root); len(temporary) != 0 {
				t.Fatalf("the refused restore left %v behind", temporary)
			}
		})
	}
}

// A backup holding something os.CopyFS cannot copy fails the copy after the
// temporary directory was created, which is the failure every publication
// step has to survive.
func TestBackupRestoreLeavesNothingBehindOnFailure(t *testing.T) {
	root, id := migrated(t)
	if err := syscall.Mkfifo(filepath.Join(backupPath(root, id), "uncopyable"), 0o600); err != nil {
		t.Skipf("named pipes are unavailable: %v", err)
	}
	before := snapshot(t, root)

	err := migrate.BackupRestore(root, id, true, true, strings.NewReader(""), new(bytes.Buffer))
	if err == nil || !strings.HasPrefix(err.Error(), "MIG001 ") {
		t.Fatalf("restore = %v", err)
	}
	if exists(t, filepath.Join(root, ".git")) {
		t.Fatal("the failed restore published a partial root repository")
	}
	if temporary := leftovers(t, root); len(temporary) != 0 {
		t.Fatalf("the failed restore left %v behind", temporary)
	}
	if !exists(t, backupPath(root, id)) {
		t.Fatal("the failed restore removed the backup")
	}
	for name, contents := range snapshot(t, root) {
		if before[name] != contents {
			t.Errorf("file %s changed: %q, want %q", name, contents, before[name])
		}
	}
}

// leftovers lists the temporary restore directories in the project root.
func leftovers(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".gitone-restore-") {
			names = append(names, entry.Name())
		}
	}
	return names
}
