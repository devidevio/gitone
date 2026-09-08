package stage

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/devidevio/gitone/internal/config"
	"github.com/devidevio/gitone/internal/git"
	"github.com/devidevio/gitone/internal/lock"
	"github.com/devidevio/gitone/internal/repository"
	"github.com/devidevio/gitone/internal/status"
	"github.com/devidevio/gitone/internal/worktree"
)

// RestoreTarget is the resolved source and destination selection of one
// restore. Index and Worktree name the destinations, Head the source: the
// owning repository's own HEAD instead of its index. The CLI resolves the
// native defaults, so every field here states what the restore really does.
type RestoreTarget struct {
	Index    bool
	Worktree bool
	Head     bool
}

// Restore makes the selected index entries, working-tree files or both match
// the version the repository owning them has in its index or in its own HEAD,
// which discards the changes made to them. A restore with the index as its
// only destination is the Git spelling of gitone unstage and runs exactly that
// command, so both spellings share one index operation and one transaction.
func Restore(configuration *config.Config, root, directory string, arguments []string, target RestoreTarget, output io.Writer) error {
	if !target.Worktree {
		return Unstage(configuration, root, directory, arguments, output)
	}

	release, err := lock.Acquire(root, restoreCommand)
	if err != nil {
		return err
	}
	defer release()

	result, inventory, err := status.Validate(configuration, root)
	if err != nil {
		return err
	}
	selected, err := selection(result, inventory, root, directory, arguments, func(change status.Change) bool {
		// The index already holds every unstaged change. HEAD additionally
		// differs in everything only the index carries, such as a staged-only
		// edit, a staged deletion or a newly staged file.
		return change.State == status.Unstaged || target.Head && change.State == status.Staged
	})
	if err != nil {
		return err
	}
	selected = unique(selected)
	if len(selected) == 0 {
		fmt.Fprintln(output, "Nothing to restore.")
		return nil
	}
	if target.Head {
		if err := requireHead(root, selected); err != nil {
			return err
		}
	}
	if err := restoreWorktree(configuration, root, inventory.Tracked, target, selected); err != nil {
		return err
	}
	report(output, "Restored", false, selected)
	return nil
}

// unique keeps one change per path, so a path that is staged and changed in
// the working tree is restored and reported once, and orders the result the
// way the report reads it.
func unique(selected []status.Change) []status.Change {
	seen := map[string]bool{}
	kept := make([]status.Change, 0, len(selected))
	for _, change := range selected {
		key := change.Repository + "\x00" + change.Path
		if seen[key] {
			continue
		}
		seen[key] = true
		kept = append(kept, change)
	}
	slices.SortFunc(kept, func(a, b status.Change) int {
		if a.Repository != b.Repository {
			return strings.Compare(a.Repository, b.Repository)
		}
		return strings.Compare(a.Path, b.Path)
	})
	return kept
}

// requireHead refuses a restore from HEAD in a repository that has none. Its
// selected paths exist in no commit, so restoring them from HEAD would delete
// them instead of putting a recorded version back.
func requireHead(root string, selected []status.Change) error {
	for _, name := range repositories(selected) {
		exists, err := repository.HeadExists(root, name)
		if err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("REPO001 repository %q has no commit yet: restore it from its index with "+
				"\"gitone restore <path>...\", or commit first", name)
		}
	}
	return nil
}

// restoreWorktree checks the source version of every selected path out beside
// the working tree, prepares the index of every repository the restore also
// resets, and swaps all of them in afterwards. No index and no selected file
// changes until every Git operation succeeded.
func restoreWorktree(configuration *config.Config, root string, tracked []string, target RestoreTarget, selected []status.Change) error {
	directory := recoveryPath(root)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	saved, err := prepareRestore(root, directory, target, selected)
	if err == nil {
		err = refusePrepared(configuration, root, directory, tracked, saved)
	}
	if err != nil {
		// No index and no working-tree file was touched, so the recorded state
		// is dropped with it and the project stays exactly where it started.
		_ = os.RemoveAll(directory)
		return err
	}
	if err := finish(root, directory, saved, true); err != nil {
		return fmt.Errorf("%w\n%s the restore is incomplete: run gitone recover or gitone abort", err, recoveryRequired)
	}
	return os.RemoveAll(directory)
}

// prepareRestore writes the source version and a copy of the current version
// of every selected path into the recovery directory, together with the
// prepared index of every repository whose index the restore resets, and
// records all of them. Writing the state file is the point from which an
// interrupted restore becomes recoverable; the real indexes and the working
// tree itself are untouched until then.
func prepareRestore(root, directory string, target RestoreTarget, selected []status.Change) (*state, error) {
	saved := &state{Version: stateVersion, Command: restoreCommand}
	for _, name := range repositories(selected) {
		owned := paths(selected, name)
		source := repository.IndexPath(root, name)
		if target.Head {
			// HEAD becomes readable as an index by resetting a copy of the
			// real one, which is the same copy the restore installs when the
			// index is a destination too.
			entry, err := prepareIndex(root, directory, name, unstageInto, owned)
			if err != nil {
				return nil, err
			}
			source = filepath.Join(directory, name+nextSuffix)
			if target.Index {
				saved.Repositories = append(saved.Repositories, entry)
			}
		}
		if err := checkoutInto(root, name, source, filepath.Join(directory, nextDirectory), owned); err != nil {
			return nil, err
		}
		for _, relative := range owned {
			file := filepath.Join(root, filepath.FromSlash(relative))
			if err := replace(file, filepath.Join(directory, originalDirectory, filepath.FromSlash(relative))); err != nil {
				return nil, err
			}
			current, err := hashFile(file)
			if err != nil {
				return nil, err
			}
			prepared, err := hashFile(filepath.Join(directory, nextDirectory, filepath.FromSlash(relative)))
			if err != nil {
				return nil, err
			}
			saved.Paths = append(saved.Paths, pathState{Path: relative, Original: current, Next: prepared})
		}
	}
	return saved, lock.WriteState(directory, saved)
}

// checkoutInto writes the source version of the selected paths below prefix,
// with the file mode the source records for each of them. index is the index
// to read: the repository's own one for an index restore, the prepared HEAD
// copy for a HEAD restore. Only explicit owned paths are passed, so Git never
// sees a pathspec that could reach a path of another repository. A path the
// source does not hold is left out; its missing prepared version is what
// removes it from the working tree.
func checkoutInto(root, name, index, prefix string, selected []string) error {
	held, err := heldPaths(root, name, index, selected)
	if err != nil || len(held) == 0 {
		return err
	}
	if err := os.MkdirAll(prefix, 0o700); err != nil {
		return err
	}
	_, err = git.RunIndexed(root, index, strings.Join(held, "\x00"),
		"--git-dir="+repository.Directory(root, name), "--work-tree="+root,
		"checkout-index", "--force", "--prefix="+prefix+"/", "-z", "--stdin")
	return err
}

// heldPaths are the selected paths the given index holds. Everything else is
// absent from the restore source, which a checkout would fail on instead of
// removing it.
func heldPaths(root, name, index string, selected []string) ([]string, error) {
	output, err := git.RunIndexed(root, index, "", "--no-optional-locks",
		"--git-dir="+repository.Directory(root, name), "--work-tree="+root, "ls-files", "-z")
	if err != nil {
		return nil, err
	}
	entries := map[string]bool{}
	for _, entry := range strings.Split(output, "\x00") {
		entries[entry] = true
	}
	return slices.DeleteFunc(slices.Clone(selected), func(path string) bool { return !entries[path] }), nil
}

// replaced maps every path a recorded restore changes to the prepared version
// it would leave in the working tree.
func replaced(directory string, saved *state) map[string]string {
	prepared := make(map[string]string, len(saved.Paths))
	for _, entry := range saved.Paths {
		prepared[entry.Path] = filepath.Join(directory, nextDirectory, filepath.FromSlash(entry.Path))
	}
	return prepared
}

// refusePrepared checks the resulting indexes and working tree against the
// configuration the restore itself would put in effect. Both destinations
// must preserve ownership and safe links independently.
func refusePrepared(configuration *config.Config, root, directory string, tracked []string, saved *state) error {
	if len(saved.Paths) == 0 {
		return nil
	}
	replacements := replaced(directory, saved)
	projected, err := projectedConfiguration(configuration, root, replacements)
	if err != nil {
		return err
	}
	matcher, err := projected.Matcher()
	if err != nil {
		return err
	}
	indexes := map[string]string{}
	for _, name := range projected.RepositoryNames() {
		indexes[name] = repository.IndexPath(root, name)
	}
	for _, entry := range saved.Repositories {
		indexes[entry.Name] = filepath.Join(directory, entry.Name+nextSuffix)
	}
	refused, err := repository.IndexIssues(matcher, root, indexes)
	if err != nil {
		return err
	}
	var issues []error
	for _, issue := range refused {
		issues = append(issues, fmt.Errorf("%s %s in repository %q: %s", pathUnsafe, issue.Path, issue.Repository, issue.Reason))
	}
	if len(issues) != 0 {
		return errors.Join(append(issues, errors.New("Select the link and its target together; leave ownership-changing policy files out of the selection."))...)
	}
	prepared, err := worktree.Prepared(projected, root, tracked, replacements)
	if err != nil {
		return err
	}
	for _, issue := range prepared.Issues {
		issues = append(issues, issue)
	}
	if len(issues) == 0 {
		return nil
	}
	return errors.Join(append(issues, errors.New("Select the link and its target together."))...)
}

// projectedConfiguration is the configuration the restore would leave behind.
// It is the current one while the restore does not replace .gitone.yml. A
// replacement that removes the file, that is unusable, or that adds, removes
// or renames a repository or changes a configured remote is refused: only
// gitone reconfigure reconciles the repositories with such a change.
func projectedConfiguration(configuration *config.Config, root string, replacements map[string]string) (*config.Config, error) {
	prepared, replacing := replacements[config.PublicFile]
	if !replacing {
		return configuration, nil
	}
	refuse := func(format string, arguments ...any) error {
		return fmt.Errorf("%s %s: %s", pathUnsafe, config.PublicFile, fmt.Sprintf(format, arguments...))
	}
	contents, err := os.ReadFile(prepared)
	if errors.Is(err, os.ErrNotExist) {
		return nil, refuse("the restore source does not contain it, and a project without it has no configuration.\n"+
			"Leave %s out of the selection.", config.PublicFile)
	}
	if err != nil {
		return nil, err
	}
	projected, err := config.Effective(contents, root)
	if err != nil {
		return nil, refuse("the version being restored is unusable: %v", err)
	}
	structural := config.Structural(config.Differences(configuration, projected))
	if len(structural) == 0 {
		return projected, nil
	}
	lines := make([]string, 0, len(structural))
	for _, change := range structural {
		lines = append(lines, fmt.Sprintf("  %s: %s -> %s", change.Field, change.Before, change.After))
	}
	return nil, refuse("the version being restored adds, removes or renames a repository or changes a configured remote:\n%s\n"+
		"Review and apply it with: gitone reconfigure", strings.Join(lines, "\n"))
}
