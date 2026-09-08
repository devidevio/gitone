// Package migrate converts a project that still has one root Git repository
// into a GitOne project. History is never imported: the complete root
// repository is copied into a verified backup that stays untouched, and the
// managed repositories start empty. Every failure keeps or restores the
// original repository.
package migrate

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/devidevio/gitone/internal/config"
	"github.com/devidevio/gitone/internal/git"
	"github.com/devidevio/gitone/internal/lock"
	"github.com/devidevio/gitone/internal/repository"
	"github.com/devidevio/gitone/internal/ui"
)

const (
	// Command is the recorded command of an interrupted migration.
	Command = "gitone migrate"

	// failed is the stable code of every migration and restoration failure.
	failed = "MIG001"

	internalDirectory = ".gitone"
	// backupDirectory holds one timestamped copy of a root repository per
	// migration. It is never moved or removed by GitOne.
	backupDirectory = "migration-backup"
	// stateDirectory holds the recorded state of a running migration, which
	// only exists while the migration is unfinished.
	stateDirectory = "migration"
	stateVersion   = 1

	gitDirectory = ".git"

	unchanged = "The project was not changed."
)

// state is the minimum record needed to detect an interrupted migration and
// undo it: the backup it may be restored from and the repositories it created.
type state struct {
	Version      int      `json:"version"`
	Command      string   `json:"command"`
	Backup       string   `json:"backup"`
	Repositories []string `json:"repositories"`
	Ignore       []byte   `json:"ignore"`
}

// Migrate converts the root repository of the project into managed
// repositories, or offers to restore an interrupted migration. yes replaces
// the confirmation, which is required when no interactive terminal can answer
// it.
func Migrate(configuration *config.Config, root string, yes, interactive bool, input io.Reader, output io.Writer) error {
	if !yes && !interactive {
		return fmt.Errorf("%s a migration without an interactive terminal requires --yes\n%s", failed, unchanged)
	}
	release, err := lock.Acquire(root, Command)
	if err != nil {
		return err
	}
	defer release()

	saved, err := readState(root)
	if err != nil {
		return err
	}
	if saved != nil {
		if err := validateState(configuration, saved); err != nil {
			return err
		}
		return restore(root, saved, yes, input, output)
	}
	return start(configuration, root, yes, true, input, output)
}

// Interrupted reports whether an unfinished migration must be resolved
// before anything else may change the project.
func Interrupted(root string) (bool, error) {
	switch _, err := os.Lstat(filepath.Join(root, internalDirectory, stateDirectory)); {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	default:
		return false, err
	}
}

// Plan checks that the project can be migrated and prints what a migration
// would do, including the warning about a modified working tree. It changes
// nothing. It exists for setup, which shows this plan inside its own summary
// and asks the single confirmation itself.
func Plan(configuration *config.Config, root string, output io.Writer) error {
	if err := preflight(configuration, root); err != nil {
		return err
	}
	modified, err := dirty(root)
	if err != nil {
		return err
	}
	summary(output, configuration, root, modified)
	return nil
}

// Confirmed migrates a project whose plan the caller already showed and whose
// confirmation the caller already collected, so nothing is printed or asked
// twice. An interrupted migration is left to Migrate, which can restore it.
func Confirmed(configuration *config.Config, root string, output io.Writer) error {
	release, err := lock.Acquire(root, Command)
	if err != nil {
		return err
	}
	defer release()

	saved, err := readState(root)
	if err != nil {
		return err
	}
	if saved != nil {
		return fail("an interrupted migration was found: resolve it with gitone migrate first")
	}
	return start(configuration, root, true, false, nil, output)
}

// start runs a complete migration: preflight, confirmation, verified backup,
// managed repositories, and only then the removal of the root repository.
// explain prints the plan; a caller that already showed it turns it off.
func start(configuration *config.Config, root string, yes, explain bool, input io.Reader, output io.Writer) error {
	if err := preflight(configuration, root); err != nil {
		return err
	}
	if explain {
		modified, err := dirty(root)
		if err != nil {
			return err
		}
		summary(output, configuration, root, modified)
	}
	confirmed, err := confirm(yes, input, output, "Migration aborted.")
	if err != nil || !confirmed {
		return err
	}

	var backup string
	if err := ui.Spin(input, output, "Migrating the root repository", func() (err error) {
		backup, err = perform(configuration, root)
		return err
	}); err != nil {
		return err
	}
	success(output, configuration, backup)
	return nil
}

// perform makes the changes of a confirmed migration: the verified backup,
// the recorded state, the managed repositories, and only then the removal of
// the root repository. It asks and prints nothing, so a spinner may cover it.
func perform(configuration *config.Config, root string) (string, error) {
	ignore, err := readIgnore(root)
	if err != nil {
		return "", err
	}
	backup, err := createBackup(root)
	if err != nil {
		return "", err
	}
	names := configuration.RepositoryNames()
	if err := writeState(root, &state{Version: stateVersion, Command: Command, Backup: backup, Repositories: names, Ignore: ignore}); err != nil {
		return "", err
	}
	removingRoot, err := convert(configuration, root)
	if err != nil {
		return "", rollback(root, backup, names, ignore, removingRoot, err)
	}
	if err := os.RemoveAll(filepath.Join(root, internalDirectory, stateDirectory)); err != nil {
		return "", err
	}
	return backup, nil
}

// convert creates and validates every managed repository and removes the root
// repository only afterwards.
func convert(configuration *config.Config, root string) (bool, error) {
	if err := repository.Prepare(configuration, root); err != nil {
		return false, err
	}
	for _, name := range configuration.RepositoryNames() {
		if issues := repository.Check(root, name, configuration.Repositories[name]); len(issues) != 0 {
			return false, issues[0]
		}
	}
	return true, os.RemoveAll(filepath.Join(root, gitDirectory))
}

// preflight refuses every project state whose migration would be unsafe or
// ambiguous, before anything is changed.
func preflight(configuration *config.Config, root string) error {
	directory := filepath.Join(root, gitDirectory)
	info, err := os.Lstat(directory)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return fail("the project root has no .git directory: run gitone init")
	case err != nil:
		return err
	case !info.IsDir():
		return fail(".git is not a directory: linked worktrees and submodules are not supported")
	}
	for _, unsupported := range []struct{ path, reason string }{
		{filepath.Join(directory, "worktrees"), "the root repository has linked worktrees"},
		{filepath.Join(directory, "modules"), "the root repository has submodules"},
		{filepath.Join(root, ".gitmodules"), "the root repository has submodules"},
	} {
		switch _, err := os.Lstat(unsupported.path); {
		case err == nil:
			return fail("%s", unsupported.reason)
		case !errors.Is(err, os.ErrNotExist):
			return err
		}
	}

	toplevel, err := git.Run(root, "rev-parse", "--show-toplevel")
	if err != nil {
		return fail("the root repository cannot be inspected: %s", detail(err))
	}
	resolved, err := filepath.EvalSymlinks(strings.TrimSpace(toplevel))
	if err != nil {
		return err
	}
	if resolved != root {
		return fail("the working tree of the root repository is %s, not the project root", resolved)
	}
	bare, err := git.Run(root, "rev-parse", "--is-bare-repository")
	if err != nil || strings.TrimSpace(bare) != "false" {
		return fail("the root repository is bare")
	}

	managed := filepath.Join(root, internalDirectory, "repositories")
	info, err = os.Lstat(managed)
	if err == nil && !info.IsDir() {
		return fail("managed repository state at %s is not a directory", managed)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	entries, err := os.ReadDir(managed)
	if err == nil && len(entries) != 0 {
		return fail("managed repository state already exists at %s: this project is already migrated", managed)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return repository.ValidateWorktree(configuration, root, true)
}

// createBackup copies the complete root repository to a unique timestamped
// path and verifies the copy. A failure leaves the project untouched.
func createBackup(root string) (string, error) {
	parent := filepath.Join(root, internalDirectory, backupDirectory)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return "", err
	}
	directory, err := os.MkdirTemp(parent, time.Now().UTC().Format(idLayout)+"-*")
	if err != nil {
		return "", err
	}
	source := filepath.Join(root, gitDirectory)
	if err := copyTree(source, directory); err != nil {
		_ = os.RemoveAll(directory)
		return "", fail("the backup could not be written: %s\n%s", detail(err), unchanged)
	}
	if err := verify(source, directory); err != nil {
		_ = os.RemoveAll(directory)
		return "", err
	}
	relative, err := filepath.Rel(root, directory)
	if err != nil {
		return "", err
	}
	return relative, nil
}

// verify checks that the backup is a usable Git repository holding exactly the
// refs of the original.
func verify(source, backup string) error {
	if err := verifyBackup(backup); err != nil {
		return err
	}
	original, err := refs(source)
	if err != nil {
		return err
	}
	copied, err := refs(backup)
	if err != nil {
		return err
	}
	if original != copied {
		return fail("the backup does not contain the same refs as the root repository\n%s", unchanged)
	}
	return nil
}

func verifyBackup(backup string) error {
	if _, err := git.RunIsolated(backup, "--git-dir="+backup, "rev-parse", "--git-dir"); err != nil {
		return fail("the backup is not a Git repository: %s\n%s", detail(err), unchanged)
	}
	if _, err := git.RunIsolated(backup, "--git-dir="+backup, "fsck", "--connectivity-only", "--no-dangling", "--no-progress"); err != nil {
		return fail("the backup is not internally consistent: %s\n%s", detail(err), unchanged)
	}
	return nil
}

func refs(directory string) (string, error) {
	output, err := git.RunIsolated(directory, "--git-dir="+directory, "for-each-ref", "--format=%(objectname) %(refname)")
	if err != nil {
		return "", fail("the refs of %s cannot be read: %s\n%s", directory, detail(err), unchanged)
	}
	return output, nil
}

// restore offers to undo an interrupted migration. It copies the backup back
// and never overwrites an existing root repository.
func restore(root string, saved *state, yes bool, input io.Reader, output io.Writer) error {
	backup := filepath.Join(root, saved.Backup)
	info, err := os.Lstat(backup)
	if err != nil {
		return fail("the interrupted migration recorded the backup %s, which cannot be read: %s", saved.Backup, detail(err))
	}
	if !info.IsDir() {
		return fail("the interrupted migration recorded backup %s, which is not a directory", saved.Backup)
	}
	if err := verifyBackup(backup); err != nil {
		return err
	}
	target := filepath.Join(root, gitDirectory)
	if _, err := os.Lstat(target); err == nil {
		return fail("the interrupted migration cannot be restored while %s exists: move or remove it first", target)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	fmt.Fprintf(output, "An interrupted migration was found.\n\nBackup: %s\n\n"+
		"Restoring copies the backup back to %s and removes the repositories the\n"+
		"migration created. The backup itself is kept.\n\n",
		saved.Backup, filepath.Join(root, gitDirectory))
	confirmed, err := confirm(yes, input, output, "Restoration aborted.")
	if err != nil || !confirmed {
		if err == nil {
			fmt.Fprintf(output, "The interrupted migration must be resolved before GitOne continues.\n")
		}
		return err
	}
	if err := restoreInterrupted(root, backup, saved.Repositories, saved.Ignore); err != nil {
		return err
	}
	if err := os.RemoveAll(filepath.Join(root, internalDirectory, stateDirectory)); err != nil {
		return err
	}
	fmt.Fprintf(output, "\nThe root repository was restored from %s.\nThe backup was kept.\n", saved.Backup)
	return nil
}

// rollback undoes a failed migration and reports the original failure. The
// recorded state survives a failed rollback so the restoration can be retried.
func rollback(root, backup string, names []string, ignore []byte, removingRoot bool, cause error) error {
	if err := rollbackChanges(root, filepath.Join(root, backup), names, ignore, removingRoot); err != nil {
		return fmt.Errorf("%s migration failed: %s\nthe root repository could not be restored automatically: %s\n"+
			"the complete backup is %s: copy it to %s and remove %s afterwards",
			failed, detail(cause), detail(err), backup, filepath.Join(root, gitDirectory),
			filepath.Join(root, internalDirectory, stateDirectory))
	}
	if err := os.RemoveAll(filepath.Join(root, internalDirectory, stateDirectory)); err != nil {
		return err
	}
	return fmt.Errorf("%s migration failed and was rolled back: %s\nThe root repository is unchanged and the backup remains at %s.",
		failed, detail(cause), backup)
}

func restoreInterrupted(root, backup string, names []string, ignore []byte) error {
	target := filepath.Join(root, gitDirectory)
	if _, err := os.Lstat(target); err == nil {
		return fail("the interrupted migration cannot be restored while %s exists: move or remove it first", target)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, name := range names {
		if err := os.RemoveAll(repository.Directory(root, name)); err != nil {
			return err
		}
	}
	if err := restoreIgnore(root, ignore); err != nil {
		return err
	}
	if err := copyTreeToNew(backup, target); err != nil {
		_ = os.RemoveAll(target)
		return err
	}
	return nil
}

func rollbackChanges(root, backup string, names []string, ignore []byte, removingRoot bool) error {
	if removingRoot {
		target := filepath.Join(root, gitDirectory)
		if err := os.RemoveAll(target); err != nil {
			return err
		}
		if err := copyTreeToNew(backup, target); err != nil {
			_ = os.RemoveAll(target)
			return err
		}
	}
	for _, name := range names {
		if err := os.RemoveAll(repository.Directory(root, name)); err != nil {
			return err
		}
	}
	return restoreIgnore(root, ignore)
}

// copyTree copies a whole Git directory. Symlinked Git metadata is refused
// instead of being flattened into a copy that only looks complete.
func copyTree(source, target string) error {
	if err := os.MkdirAll(target, 0o700); err != nil {
		return err
	}
	return os.CopyFS(target, os.DirFS(source))
}

func copyTreeToNew(source, target string) error {
	if err := os.Mkdir(target, 0o700); err != nil {
		return err
	}
	return os.CopyFS(target, os.DirFS(source))
}

func readIgnore(root string) ([]byte, error) {
	contents, err := os.ReadFile(filepath.Join(root, config.ProjectIgnoreFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return contents, err
}

func restoreIgnore(root string, contents []byte) error {
	name := filepath.Join(root, config.ProjectIgnoreFile)
	if contents == nil {
		if err := os.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	return os.WriteFile(name, contents, 0o644)
}

// dirty reports whether the root repository has staged, unstaged or untracked
// changes. The index is left alone so a refused migration changes nothing.
func dirty(root string) (bool, error) {
	output, err := git.Run(root, "--no-optional-locks", "status", "--porcelain")
	if err != nil {
		return false, fail("the root repository cannot be inspected: %s", detail(err))
	}
	return strings.TrimSpace(output) != "", nil
}

func summary(output io.Writer, configuration *config.Config, root string, modified bool) {
	style := ui.For(output)
	fmt.Fprintf(output, "Migrating the root repository into GitOne.\n\nRoot repository: %s\n",
		filepath.Join(root, gitDirectory))
	fmt.Fprintf(output, "Managed repositories: %s\n\n", strings.Join(configuration.RepositoryNames(), ", "))
	fmt.Fprintf(output, "The existing history is NOT imported. The managed repositories start without\n"+
		"commits. Every commit, branch, tag, stash, hook, remote and Git setting of the\n"+
		"root repository remains available only in the backup below %s/.\n"+
		"Your files themselves are not touched.\n\n",
		filepath.Join(internalDirectory, backupDirectory))
	if modified {
		fmt.Fprintf(output, "%s\n",
			ui.RenderLines(style.Warning, "WARNING: the working tree has staged, unstaged or untracked changes. Their\n"+
				"Git state, including the index, is kept only in the backup. The files stay as\n"+
				"they are and start as untracked files of the managed repositories."))
		fmt.Fprintln(output)
	}
}

func success(output io.Writer, configuration *config.Config, backup string) {
	fmt.Fprintln(output, "Migration complete.")
	fmt.Fprintln(output)
	ui.Repositories(output, configuration)
	// The full backup ID is always spelled out, even while it is the only
	// one, so both commands can be copied straight out of the output.
	id := filepath.Base(backup)
	fmt.Fprintf(output, "\nThe original root Git repository was preserved at:\n\n  %s\n\n", backup)
	fmt.Fprintf(output, "Inspect it with:\n  gitone backup git --backup %s -- log --all\n\n"+
		"Restore it with:\n  gitone backup restore --backup %s\n\n", id, id)
	fmt.Fprintln(output, "Nothing was staged or committed.")
}

// confirm asks for the explicit approval every migration and restoration
// requires. The default answer is no.
func confirm(yes bool, input io.Reader, output io.Writer, aborted string) (bool, error) {
	if yes {
		return true, nil
	}
	fmt.Fprint(output, "Continue? [y/N] ")
	answer, err := bufio.NewReader(input).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	if slices.Contains([]string{"y", "yes"}, strings.ToLower(strings.TrimSpace(answer))) {
		return true, nil
	}
	fmt.Fprintf(output, "\n%s\n\n%s\n", aborted, unchanged)
	return false, nil
}

func writeState(root string, saved *state) error {
	directory := filepath.Join(root, internalDirectory, stateDirectory)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	return lock.WriteState(directory, saved)
}

// readState returns the recorded state of an interrupted migration, or nil
// when no migration is unfinished.
func readState(root string) (*state, error) {
	directory := filepath.Join(root, internalDirectory, stateDirectory)
	contents, err := os.ReadFile(filepath.Join(directory, lock.StateFile))
	if errors.Is(err, os.ErrNotExist) {
		// A prepared directory without recorded state predates the backup, so
		// nothing was changed and it is discarded.
		if _, err := os.Stat(directory); err == nil {
			return nil, os.RemoveAll(directory)
		}
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	saved := new(state)
	if err := json.Unmarshal(contents, saved); err != nil {
		return nil, fmt.Errorf("%s the recorded migration state is unreadable: %w", failed, err)
	}
	if saved.Version != stateVersion {
		return nil, fmt.Errorf("%s the recorded migration state has unsupported version %d", failed, saved.Version)
	}
	return saved, nil
}

func validateState(configuration *config.Config, saved *state) error {
	if saved.Command != Command {
		return fail("the recorded migration state has unexpected command %q", saved.Command)
	}
	if !slices.Equal(saved.Repositories, configuration.RepositoryNames()) {
		return fail("the recorded migration state does not match the configured repositories")
	}
	clean := filepath.Clean(saved.Backup)
	if clean != saved.Backup || !filepath.IsLocal(clean) ||
		filepath.Dir(clean) != filepath.Join(internalDirectory, backupDirectory) {
		return fail("the recorded migration backup path %q is unsafe", saved.Backup)
	}
	return nil
}

func fail(format string, arguments ...any) error {
	return fmt.Errorf("%s %s", failed, fmt.Sprintf(format, arguments...))
}

// detail is the first line of an error, so one reported problem stays one line.
func detail(err error) string {
	line, _, _ := strings.Cut(err.Error(), "\n")
	return line
}
