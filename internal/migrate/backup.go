package migrate

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/devidevio/gitone/internal/atomicfs"
	"github.com/devidevio/gitone/internal/git"
	"github.com/devidevio/gitone/internal/lock"
)

// RestoreCommand is the recorded command of a running backup restoration.
const RestoreCommand = "gitone backup restore"

// GitCommands are the native Git commands a backup may be inspected with.
// They only read; every other command, including every mutating one, is
// rejected before Git runs.
var GitCommands = []string{"log", "show", "diff", "status"}

// idLayout is the UTC timestamp a backup ID starts with. createBackup writes
// it, and the listing reads it back.
const idLayout = "20060102T150405Z"

// Backup is one retained migration backup below .gitone/migration-backup/.
// Its directory basename is the only durable identifier it has.
type Backup struct {
	ID string
	// Created is the UTC time the ID encodes, and Timestamped reports whether
	// it encodes one at all.
	Created     time.Time
	Timestamped bool
	// Valid reports whether the directory passes the same Git integrity
	// check a migration runs on the backup it just wrote.
	Valid bool
}

// Backups lists the retained migration backups, newest first. Only direct,
// real directories count: a symbolic link is skipped instead of followed out
// of the project. An invalid backup stays visible so it is never silently
// hidden or picked.
func Backups(root string) ([]Backup, error) {
	parent, err := backupParent(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		return nil, fail("migration backups below %s cannot be read: %s", filepath.Join(internalDirectory, backupDirectory), detail(err))
	}
	found := make([]Backup, 0, len(entries))
	for _, entry := range entries {
		// ReadDir reports the entry itself, so a symbolic link is not a
		// directory here and is left out.
		if !entry.IsDir() {
			continue
		}
		backup := Backup{ID: entry.Name(), Valid: verifyBackup(filepath.Join(parent, entry.Name())) == nil}
		backup.Created, backup.Timestamped = backupTime(entry.Name())
		found = append(found, backup)
	}
	// Newest first, with the IDs that carry no time last and the ID itself as
	// the tiebreaker, so the same directory always lists the same way.
	slices.SortFunc(found, func(a, b Backup) int {
		if a.Timestamped != b.Timestamped {
			if a.Timestamped {
				return -1
			}
			return 1
		}
		if a.Timestamped && !a.Created.Equal(b.Created) {
			return b.Created.Compare(a.Created)
		}
		return strings.Compare(b.ID, a.ID)
	})
	return found, nil
}

// BackupList prints every retained backup with its full ID, its UTC time and
// its validity. A project without any backup is a successful empty result.
func BackupList(root string, output io.Writer) error {
	found, err := Backups(root)
	if err != nil {
		return err
	}
	if len(found) == 0 {
		fmt.Fprintln(output, "No migration backups found.")
		return nil
	}
	fmt.Fprintf(output, "Migration backups below %s\n\n", filepath.Join(internalDirectory, backupDirectory))
	writer := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "ID\tCREATED (UTC)\tSTATE")
	for _, backup := range found {
		created := "unknown"
		if backup.Timestamped {
			created = backup.Created.Format("2006-01-02 15:04:05")
		}
		state := "invalid"
		if backup.Valid {
			state = "valid"
		}
		fmt.Fprintf(writer, "%s\t%s\t%s\n", backup.ID, created, state)
	}
	return writer.Flush()
}

// BackupGit runs one allowed native Git inspection against a retained backup.
// The backup is the Git directory and the project root is the working tree,
// so status and diff compare the retained repository with the current files.
// It returns the exit code of Git.
func BackupGit(root, id string, arguments []string, stdout, stderr io.Writer) (int, error) {
	directory, err := selectBackup(root, id)
	if err != nil {
		return 1, err
	}
	if err := verifyBackup(directory); err != nil {
		return 1, err
	}
	// --no-optional-locks keeps every inspection read-only: not even an index
	// refresh may write into the backup.
	full := append([]string{"--no-pager", "--no-optional-locks",
		"--git-dir=" + directory, "--work-tree=" + root}, arguments...)
	code, err := git.Stream(root, stdout, stderr, full...)
	if err != nil {
		return code, err
	}
	if code != 0 {
		// Git already explained itself on standard error; only the stable
		// code is added.
		return code, fmt.Errorf("GIT001 git %s failed", strings.Join(arguments, " "))
	}
	return 0, nil
}

// BackupRestore copies a retained backup back to the root .git/. It changes
// no project file, no configuration and no managed repository, and it keeps
// the backup itself. yes replaces the confirmation, which is required when no
// interactive terminal can answer it.
func BackupRestore(root, id string, yes, interactive bool, input io.Reader, output io.Writer) error {
	if !yes && !interactive {
		return fail("restoring a migration backup without an interactive terminal requires --yes\n%s", unchanged)
	}
	directory, err := selectBackup(root, id)
	if err != nil {
		return err
	}
	if err := verifyBackup(directory); err != nil {
		return err
	}

	release, err := lock.Acquire(root, RestoreCommand)
	if err != nil {
		return err
	}
	defer release()

	target := filepath.Join(root, gitDirectory)
	if err := absent(target); err != nil {
		return err
	}

	selected := filepath.Base(directory)
	fmt.Fprintf(output, "Restoring the migration backup:\n\n  %s\n\nIt is copied back to:\n\n  %s\n\n"+
		"The backup itself is kept. Your files, the ignore entries, the configuration\n"+
		"and the managed repositories are not changed.\n\n"+
		"While a root .git/ exists, the ordinary GitOne commands are unavailable.\n\n",
		filepath.Join(internalDirectory, backupDirectory, selected), target)
	confirmed, err := confirm(yes, input, output, "Restoration aborted.")
	if err != nil || !confirmed {
		return err
	}
	if err := publish(root, directory, target); err != nil {
		return err
	}
	fmt.Fprintf(output, "\nThe root .git/ was restored from:\n\n  %s\n\n"+
		"The backup was kept. The ordinary GitOne commands are unavailable while the\n"+
		"root .git/ exists; gitone backup list and gitone backup git still work.\n",
		filepath.Join(internalDirectory, backupDirectory, selected))
	return nil
}

// publish copies the backup into a temporary directory inside the project
// root, so the finished copy is swapped in by a rename on one filesystem.
// Every failure removes the temporary copy and leaves the backup and the
// project unchanged.
func publish(root, backup, target string) error {
	temporary, err := os.MkdirTemp(root, ".gitone-restore-*")
	if err != nil {
		return fail("a temporary restored repository could not be created: %s\n%s", detail(err), unchanged)
	}
	if err := copyTree(backup, temporary); err != nil {
		return discard(temporary, fail("the backup could not be copied: %s\n%s", detail(err), unchanged))
	}
	if err := verify(backup, temporary); err != nil {
		return discard(temporary, err)
	}
	// The exclusive rename closes the race between the earlier existence check
	// and publication: a target that appeared meanwhile is never replaced.
	if err := atomicfs.RenameNoReplace(temporary, target); err != nil {
		return discard(temporary, fail("the restored repository could not be published as %s: %s\n%s", target, detail(err), unchanged))
	}
	return nil
}

func discard(temporary string, cause error) error {
	_ = os.RemoveAll(temporary)
	return cause
}

// absent refuses every existing root .git path, whatever it is.
func absent(target string) error {
	switch _, err := os.Lstat(target); {
	case err == nil:
		return fail("%s already exists: move or remove it first\n%s", target, unchanged)
	case !errors.Is(err, os.ErrNotExist):
		return fail("%s cannot be inspected: %s\n%s", target, detail(err), unchanged)
	}
	return nil
}

// selectBackup resolves the backup a command works on. Only a project with
// exactly one backup selects it implicitly; every other case requires the
// exact ID, because inspecting or restoring the wrong repository is not
// something a guess may cause.
func selectBackup(root, id string) (string, error) {
	relative := filepath.Join(internalDirectory, backupDirectory)
	if id == "" {
		found, err := Backups(root)
		if err != nil {
			return "", err
		}
		switch len(found) {
		case 0:
			return "", fail("no migration backup exists below %s", relative)
		case 1:
			return filepath.Join(backupParentPath(root), found[0].ID), nil
		default:
			return "", fail("%d migration backups exist below %s: select one with --backup <id> from gitone backup list", len(found), relative)
		}
	}
	// "." is a local path and its own base, but it names the backup directory
	// itself rather than a backup, so it is refused with every other ID that
	// is not a plain directory name.
	if id == "." || id != filepath.Base(id) || !filepath.IsLocal(id) {
		return "", fail("the migration backup ID %q is not a plain backup directory name", id)
	}
	parent, err := backupParent(root)
	if errors.Is(err, os.ErrNotExist) {
		return "", fail("no migration backup %q exists below %s", id, relative)
	}
	if err != nil {
		return "", err
	}
	directory := filepath.Join(parent, id)
	// Lstat, so a symbolic link is not accepted as the backup it points at.
	switch info, err := os.Lstat(directory); {
	case err == nil && info.IsDir():
		return directory, nil
	case err != nil && !errors.Is(err, os.ErrNotExist):
		return "", fail("the migration backup %q cannot be inspected: %s", id, detail(err))
	}
	return "", fail("no migration backup %q exists below %s", id, relative)
}

func backupParentPath(root string) string {
	return filepath.Join(root, internalDirectory, backupDirectory)
}

// backupParent refuses a symbolic link or non-directory in every component
// controlled by GitOne before any backup is read through it.
func backupParent(root string) (string, error) {
	parent := backupParentPath(root)
	for _, directory := range []string{filepath.Join(root, internalDirectory), parent} {
		info, err := os.Lstat(directory)
		if errors.Is(err, os.ErrNotExist) {
			return "", os.ErrNotExist
		}
		if err != nil {
			return "", fail("the migration backup path %s cannot be inspected: %s", directory, detail(err))
		}
		if !info.IsDir() {
			return "", fail("the migration backup path %s is not a real directory", directory)
		}
	}
	return parent, nil
}

// backupTime reads the UTC timestamp a backup ID starts with. An ID that was
// not created by GitOne carries none, which is reported instead of guessed.
func backupTime(id string) (time.Time, bool) {
	stamp, _, found := strings.Cut(id, "-")
	if !found {
		return time.Time{}, false
	}
	parsed, err := time.Parse(idLayout, stamp)
	if err != nil {
		return time.Time{}, false
	}
	return parsed, true
}
