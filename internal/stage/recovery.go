package stage

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/devidevio/gitone/internal/branch"
	"github.com/devidevio/gitone/internal/config"
	"github.com/devidevio/gitone/internal/lock"
	"github.com/devidevio/gitone/internal/pull"
	"github.com/devidevio/gitone/internal/push"
	"github.com/devidevio/gitone/internal/reconfigure"
	"github.com/devidevio/gitone/internal/repository"
	"github.com/devidevio/gitone/internal/status"
	"github.com/devidevio/gitone/internal/switching"
)

const (
	stateFile        = lock.StateFile
	originalSuffix   = ".original"
	nextSuffix       = ".next"
	stateVersion     = 1
	recoveryRequired = "REC001"

	// The saved working-tree versions keep the project-relative path, so they
	// live in a directory each instead of carrying a suffix.
	originalDirectory = "original"
	nextDirectory     = "next"

	// The recorded commands, which also select how an interrupted operation
	// is resumed.
	addCommand     = "gitone add"
	unstageCommand = "gitone unstage"
	restoreCommand = "gitone restore"
	commitCommand  = "gitone commit"
)

// state is the recorded state of an interrupted operation. The hashes identify
// the index each repository had before the operation and the index it must
// have afterwards, so recovery can tell finished from pending repositories and
// detect indexes that changed outside GitOne. Message and Group belong to a
// commit and stay empty for index-only operations. Paths belongs to a restore
// and stays empty for operations that only change indexes.
type state struct {
	Version      int               `json:"version"`
	Command      string            `json:"command"`
	Message      string            `json:"message,omitempty"`
	Group        string            `json:"group,omitempty"`
	Repositories []repositoryState `json:"repositories"`
	Paths        []pathState       `json:"paths,omitempty"`
}

// pathState is the recorded progress of one working-tree file. The hashes
// identify the file the restore started from and the index version it must
// hold afterwards, exactly like repositoryState does for an index.
type pathState struct {
	Path     string `json:"path"`
	Original string `json:"original"`
	Next     string `json:"next"`
}

// repositoryState is the recorded progress of one repository. Branch, Head and
// Committed belong to a commit: Head is the branch position the commit started
// from, empty while the branch was unborn, and Committed is the created commit
// or empty while the repository is still pending. Selected is the path
// selection of a path-limited commit together with the identity each selected
// working-tree entry had, and stays empty for every other operation.
type repositoryState struct {
	Name      string      `json:"name"`
	Original  string      `json:"original"`
	Next      string      `json:"next"`
	Branch    string      `json:"branch,omitempty"`
	Head      string      `json:"head,omitempty"`
	Committed string      `json:"committed,omitempty"`
	Selected  []pathState `json:"selected,omitempty"`
}

// selection is the recorded path selection, in the order it is passed back to
// native Git.
func (r *repositoryState) selection() []string {
	selected := make([]string, 0, len(r.Selected))
	for _, entry := range r.Selected {
		selected = append(selected, entry.Path)
	}
	return selected
}

// Recover finishes an interrupted operation: an unfinished index replacement
// is completed, the repositories a grouped commit did not reach yet are
// committed after the project has been validated again, the branches an
// interrupted creation did not reach yet are created, the fast-forwards a
// pull did not finish are completed, and the repositories a switch did not
// reach yet are checked out.
func Recover(configuration *config.Config, root string, output io.Writer) error {
	return resume(configuration, root, "gitone recover", true, output)
}

// Abort undoes an interrupted operation: the indexes it started from are
// restored, the commits and branches it already created are removed again,
// the branches a pull advanced are set back to the commits it started from,
// and the repositories a switch moved return to their starting branch.
func Abort(configuration *config.Config, root string, output io.Writer) error {
	return resume(configuration, root, "gitone abort", false, output)
}

func resume(configuration *config.Config, root, command string, recovering bool, output io.Writer) error {
	release, err := lock.AcquireRecovery(root, command)
	if err != nil {
		return err
	}
	defer release()

	directory := recoveryPath(root)
	saved, err := readState(directory)
	if errors.Is(err, os.ErrNotExist) {
		// Prepared indexes without recorded state predate the first index
		// replacement, so no repository was changed and they are discarded.
		if err := os.RemoveAll(directory); err != nil {
			return err
		}
		fmt.Fprintln(output, "No interrupted GitOne operation found.")
		return nil
	}
	if err != nil {
		return err
	}
	// A push publishes refs that must never be rolled back or repeated, so
	// recover and abort both only reconcile what really reached the remotes.
	if saved.Command == push.Command {
		if err := push.Reconcile(root, output); err != nil {
			return err
		}
		return os.RemoveAll(directory)
	}
	// A branch creation only wrote refs, so it resumes its own recorded
	// branches instead of replacing indexes.
	if saved.Command == branch.Command {
		if err := branch.Resume(root, recovering, output); err != nil {
			return err
		}
		return os.RemoveAll(directory)
	}
	// A pull changed branches, indexes and working-tree files together, so it
	// resumes its own recorded fast-forwards instead of replacing indexes.
	if saved.Command == pull.Command {
		if err := pull.Resume(root, recovering, output); err != nil {
			return err
		}
		return os.RemoveAll(directory)
	}
	// A reconfiguration created repositories and changed configured remotes,
	// so it resumes its own recorded plan instead of replacing indexes.
	if saved.Command == reconfigure.Command {
		if err := reconfigure.Resume(root, recovering, output); err != nil {
			return err
		}
		return os.RemoveAll(directory)
	}
	// A switch changed HEAD, indexes and working-tree files together, so it
	// resumes its own recorded checkouts instead of replacing indexes.
	if saved.Command == switching.Command {
		if err := switching.Resume(root, recovering, output); err != nil {
			return err
		}
		return os.RemoveAll(directory)
	}
	if saved.Version != stateVersion {
		return fmt.Errorf("%s the recorded operation state has unsupported version %d", recoveryRequired, saved.Version)
	}
	if err := continueOperation(configuration, root, directory, saved, recovering, output); err != nil {
		return err
	}
	if err := os.RemoveAll(directory); err != nil {
		return err
	}
	verb := "aborted"
	if recovering {
		verb = "recovered"
	}
	fmt.Fprintf(output, "%s %s.\n", saved.Command, verb)
	return nil
}

// continueOperation resumes the recorded operation. A commit is finished by
// committing the pending repositories or undone by restoring the recorded
// branch positions and indexes; index-only operations only have to complete
// or revert their index replacement.
func continueOperation(configuration *config.Config, root, directory string, saved *state, recovering bool, output io.Writer) error {
	if saved.Command != commitCommand {
		// Finishing an operation applies its prepared result, which the
		// project may have drifted away from meanwhile. Undoing one only puts
		// back the state the project was validated in.
		if recovering {
			if err := validatePrepared(configuration, root, directory, saved); err != nil {
				return err
			}
		}
		return finish(root, directory, saved, recovering)
	}

	for index := range saved.Repositories {
		if err := verifyRecorded(root, &saved.Repositories[index], directory); err != nil {
			return err
		}
	}
	if !recovering {
		return restore(root, directory, saved)
	}
	// A recovered commit creates commits, so the project must be safe again
	// before a hook runs or a commit is written.
	if _, _, err := status.Validate(configuration, root); err != nil {
		return err
	}
	if err := commitPending(configuration, root, directory, saved); err != nil {
		return err
	}
	printCommits(output, saved)
	return nil
}

// validatePrepared checks the result again before recovery, including a
// restore's projected configuration, so it cannot complete an unsafe result.
func validatePrepared(configuration *config.Config, root, directory string, saved *state) error {
	if len(saved.Paths) != 0 {
		_, inventory, err := status.Inspect(configuration, root)
		if err != nil {
			return err
		}
		return refusePrepared(configuration, root, directory, inventory.Tracked, saved)
	}
	matcher, err := configuration.Matcher()
	if err != nil {
		return err
	}
	indexes := map[string]string{}
	for _, name := range configuration.RepositoryNames() {
		indexes[name] = repository.IndexPath(root, name)
	}
	for _, entry := range saved.Repositories {
		indexes[entry.Name] = filepath.Join(directory, entry.Name+nextSuffix)
	}
	return refuseLinks(matcher, root, indexes)
}

// recorded pairs one file a resumed operation replaces with the two saved
// versions of it and their hashes. Indexes and working-tree files only differ
// in where their versions are kept, so both are resumed by the same code.
type recorded struct {
	label        string
	file         string
	original     string
	next         string
	originalHash string
	nextHash     string
}

// records lists every file the recorded operation replaces, indexes first.
func records(root, directory string, saved *state) []recorded {
	list := make([]recorded, 0, len(saved.Repositories)+len(saved.Paths))
	for _, entry := range saved.Repositories {
		list = append(list, recorded{
			label:        fmt.Sprintf("repository %q", entry.Name),
			file:         repository.IndexPath(root, entry.Name),
			original:     filepath.Join(directory, entry.Name+originalSuffix),
			next:         filepath.Join(directory, entry.Name+nextSuffix),
			originalHash: entry.Original,
			nextHash:     entry.Next,
		})
	}
	for _, entry := range saved.Paths {
		relative := filepath.FromSlash(entry.Path)
		list = append(list, recorded{
			label:        fmt.Sprintf("path %q", entry.Path),
			file:         filepath.Join(root, relative),
			original:     filepath.Join(directory, originalDirectory, relative),
			next:         filepath.Join(directory, nextDirectory, relative),
			originalHash: entry.Original,
			nextHash:     entry.Next,
		})
	}
	return list
}

// finish replaces every recorded index and working-tree file with the version
// the operation must end in: the prepared one when it is completed, the one it
// started from when it is undone. A file that already holds the wanted version
// is skipped, and a file that matches neither saved version changed outside
// GitOne and stops the whole operation before the first replacement.
func finish(root, directory string, saved *state, recovering bool) error {
	type replacement struct{ source, target string }
	var pending []replacement
	for _, entry := range records(root, directory, saved) {
		source, wanted, other := entry.original, entry.originalHash, entry.nextHash
		if recovering {
			source, wanted, other = entry.next, entry.nextHash, entry.originalHash
		}
		current, err := hashFile(entry.file)
		if err != nil {
			return err
		}
		if current == wanted {
			continue
		}
		prepared, err := hashFile(source)
		if err != nil {
			return err
		}
		if current != other || prepared != wanted {
			return stale(entry.label, entry.file, directory)
		}
		pending = append(pending, replacement{source: source, target: entry.file})
	}
	for _, change := range pending {
		if err := replace(change.source, change.target); err != nil {
			return err
		}
	}
	return nil
}

func stale(label, file, directory string) error {
	return fmt.Errorf("%s %s changed outside GitOne since the interrupted operation: resolve it manually\n"+
		"  1. compare %s with the saved versions in %s\n"+
		"  2. copy the version you want to keep into place\n"+
		"  3. archive %s outside the project afterwards, do not delete it",
		recoveryRequired, label, file, directory, directory)
}

// replace puts source in place of target without leaving a partial entry
// behind. A regular file keeps its mode, so a restored file stays executable;
// a symbolic link is recreated from its recorded target and never opened. A
// missing source removes the target, which restores a repository that had no
// index yet and a path that did not exist before.
//
// ponytail: no fsync, this survives a process crash but not a power loss.
func replace(source, target string) error {
	info, err := os.Lstat(source)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return replaceLink(source, target)
	}
	contents, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(target), "."+filepath.Base(target)+".gitone-*")
	if err != nil {
		return err
	}
	defer func() {
		_ = temporary.Close()
		_ = os.Remove(temporary.Name())
	}()
	if _, err := temporary.Write(contents); err != nil {
		return err
	}
	if err := temporary.Chmod(info.Mode().Perm()); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporary.Name(), target)
}

// replaceLink recreates a symbolic link beside the destination and renames it
// into place, so the link target is never opened, read or written.
func replaceLink(source, target string) error {
	stored, err := os.Readlink(source)
	if err != nil {
		return err
	}
	temporary := filepath.Join(filepath.Dir(target), "."+filepath.Base(target)+".gitone-link")
	if err := os.Remove(temporary); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Symlink(stored, temporary); err != nil {
		return err
	}
	if err := os.Rename(temporary, target); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return nil
}

// hashFile identifies a path by what it is: missing, a symbolic link with its
// stored target, or a regular file with its permissions and contents. The
// entry kind is part of the hash, so a file that became a link is a change.
func hashFile(name string) (string, error) {
	info, err := os.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return "missing", nil
	}
	if err != nil {
		return "", err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		stored, err := os.Readlink(name)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("link:%x", sha256.Sum256([]byte(stored))), nil
	}
	contents, err := os.ReadFile(name)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%04o:%x", info.Mode().Perm(), sha256.Sum256(contents)), nil
}

func readState(directory string) (*state, error) {
	contents, err := os.ReadFile(filepath.Join(directory, stateFile))
	if err != nil {
		return nil, err
	}
	saved := new(state)
	if err := json.Unmarshal(contents, saved); err != nil {
		return nil, fmt.Errorf("%s the recorded operation state is unreadable: %w", recoveryRequired, err)
	}
	return saved, nil
}

func recoveryPath(root string) string {
	return lock.RecoveryPath(root)
}
