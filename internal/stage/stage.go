// Package stage changes the native index, and for a restore the working-tree
// file, of the repository that owns each path. Every affected index and every
// restored file is prepared as a copy first, so the real ones are replaced
// only after all Git operations succeeded and an interrupted replacement stays
// recoverable.
package stage

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/devidevio/gitone/internal/config"
	"github.com/devidevio/gitone/internal/git"
	"github.com/devidevio/gitone/internal/lock"
	"github.com/devidevio/gitone/internal/repository"
	"github.com/devidevio/gitone/internal/status"
	"github.com/devidevio/gitone/internal/ui"
	"github.com/devidevio/gitone/internal/worktree"
)

const pathUnsafe = "PATH003"

// indexChange pairs a recorded command with the index operation it stands
// for. Both are decided here, so a call site cannot record one command while
// running the other operation.
type indexChange struct {
	command   string
	operation func(root, name, index string, selected []string) error
}

var (
	staging   = indexChange{command: addCommand, operation: stageInto}
	unstaging = indexChange{command: unstageCommand, operation: unstageInto}
)

// Add stages the changes selected by arguments, which are the accepted add
// inputs: -A, -u, or explicit relative paths resolved against directory.
func Add(configuration *config.Config, root, directory string, arguments []string, output io.Writer) error {
	release, err := lock.Acquire(root, staging.command)
	if err != nil {
		return err
	}
	defer release()

	result, inventory, err := status.Validate(configuration, root)
	if err != nil {
		return err
	}

	selected, err := selection(result, inventory, root, directory, arguments, func(change status.Change) bool {
		return change.State == status.Unstaged || change.State == status.Untracked
	})
	if err != nil {
		return err
	}
	if len(selected) == 0 {
		fmt.Fprintln(output, "Nothing to stage.")
		return nil
	}
	if err := apply(configuration, root, staging, selected); err != nil {
		return err
	}
	report(output, "Staged", true, selected)
	return nil
}

// Unstage restores selected staged paths from HEAD without changing the
// working tree. An unborn repository has no HEAD, so its selected index
// entries are removed instead.
func Unstage(configuration *config.Config, root, directory string, arguments []string, output io.Writer) error {
	release, err := lock.Acquire(root, unstaging.command)
	if err != nil {
		return err
	}
	defer release()

	result, inventory, err := status.Validate(configuration, root)
	if err != nil {
		return err
	}
	selected, err := selection(result, inventory, root, directory, arguments, func(change status.Change) bool {
		return change.State == status.Staged
	})
	if err != nil {
		return err
	}
	if len(selected) == 0 {
		fmt.Fprintln(output, "Nothing to unstage.")
		return nil
	}
	if err := apply(configuration, root, unstaging, selected); err != nil {
		return err
	}
	report(output, "Unstaged", false, selected)
	return nil
}

// selection resolves the command input into selectable changes. -A covers the
// whole project, -u covers its tracked paths only, and a path argument covers
// itself or, for a directory, its subtree.
func selection(result *status.Status, inventory *worktree.Result, root, directory string, arguments []string, selectable func(status.Change) bool) ([]status.Change, error) {
	if len(arguments) == 1 && (arguments[0] == "-A" || arguments[0] == "-u") {
		untracked := arguments[0] == "-A"
		return filter(result.Changes, func(change status.Change) bool {
			return selectable(change) && (untracked || change.State != status.Untracked)
		}), nil
	}

	selectors := make([]string, 0, len(arguments))
	for _, argument := range arguments {
		relative, err := worktree.Relative(root, directory, argument)
		if err != nil {
			return nil, err
		}
		if !managed(inventory, relative) {
			if argument == "." {
				return nil, fmt.Errorf("%s %s: path is not managed by GitOne", pathUnsafe, argument)
			}
			return nil, fmt.Errorf("%s %s: path is not a managed file or directory", pathUnsafe, argument)
		}
		selectors = append(selectors, relative)
	}
	return filter(result.Changes, func(change status.Change) bool {
		return selectable(change) && slices.ContainsFunc(selectors, func(selector string) bool {
			return covers(selector, change.Path) || change.From != "" && covers(selector, change.From)
		})
	}), nil
}

// covers reports whether a path argument selects relativePath. "." is the
// whole subtree the argument was resolved from.
func covers(selector, relativePath string) bool {
	return selector == "." || relativePath == selector || strings.HasPrefix(relativePath, selector+"/")
}

// managed reports whether relative names a managed file or a directory that
// holds one. The map lookup answers the file case without scanning.
func managed(inventory *worktree.Result, relative string) bool {
	if relative == "." {
		return true
	}
	if _, file := inventory.Owners[relative]; file {
		return true
	}
	for owned := range inventory.Owners {
		if covers(relative, owned) {
			return true
		}
	}
	return false
}

func filter(changes []status.Change, keep func(status.Change) bool) []status.Change {
	var selected []status.Change
	for _, change := range changes {
		if keep(change) {
			selected = append(selected, change)
		}
	}
	return selected
}

// apply prepares the new index of every affected repository beside the real
// one and swaps them in afterwards. Nothing outside .gitone/recovery/ changes
// until every Git operation succeeded.
func apply(configuration *config.Config, root string, change indexChange, selected []status.Change) error {
	directory := recoveryPath(root)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	saved, err := prepare(configuration, root, directory, change, selected)
	if err != nil {
		// No state was recorded, so no real index was touched.
		_ = os.RemoveAll(directory)
		return err
	}
	if err := finish(root, directory, saved, true); err != nil {
		return fmt.Errorf("%w\n%s the index operation is incomplete: run gitone recover or gitone abort", err, recoveryRequired)
	}
	return os.RemoveAll(directory)
}

// prepare copies every affected index, applies the operation to the copy and
// records the resulting state. Writing the state file is the point from which an
// interrupted operation becomes recoverable.
func prepare(configuration *config.Config, root, directory string, change indexChange, selected []status.Change) (*state, error) {
	matcher, err := configuration.Matcher()
	if err != nil {
		return nil, err
	}
	indexes := map[string]string{}
	for _, name := range configuration.RepositoryNames() {
		indexes[name] = repository.IndexPath(root, name)
	}
	saved := &state{Version: stateVersion, Command: change.command}
	for _, name := range repositories(selected) {
		entry, err := prepareIndex(root, directory, name, change.operation, paths(selected, name))
		if err != nil {
			return nil, err
		}
		indexes[name] = filepath.Join(directory, name+nextSuffix)
		saved.Repositories = append(saved.Repositories, entry)
	}
	// The prepared indexes are one project result, so a directory link also
	// sees descendants held by repositories this operation did not change.
	if err := refuseLinks(matcher, root, indexes); err != nil {
		return nil, err
	}
	return saved, lock.WriteState(directory, saved)
}

// prepareIndex copies the index of one repository beside the real one, applies
// operation to the copy and records the state both are in. The real index is
// never touched, so the caller decides afterwards whether the prepared copy
// becomes the new index or only a source to read from.
func prepareIndex(root, directory, name string, operation func(root, name, index string, selected []string) error, selected []string) (repositoryState, error) {
	index := repository.IndexPath(root, name)
	original := filepath.Join(directory, name+originalSuffix)
	next := filepath.Join(directory, name+nextSuffix)
	if err := replace(index, original); err != nil {
		return repositoryState{}, err
	}
	if err := replace(index, next); err != nil {
		return repositoryState{}, err
	}
	if err := operation(root, name, next, selected); err != nil {
		return repositoryState{}, err
	}
	current, err := hashFile(original)
	if err != nil {
		return repositoryState{}, err
	}
	prepared, err := hashFile(next)
	if err != nil {
		return repositoryState{}, err
	}
	return repositoryState{Name: name, Original: current, Next: prepared}, nil
}

// stageInto runs native git add against a temporary index. Only explicit owned
// paths are passed, so Git never sees a pathspec that could reach a path of
// another repository.
func stageInto(root, name, index string, selected []string) error {
	_, err := git.RunIndexed(root, index, strings.Join(selected, "\x00"),
		"--literal-pathspecs", "--git-dir="+repository.Directory(root, name), "--work-tree="+root,
		"add", "--pathspec-from-file=-", "--pathspec-file-nul")
	return err
}

// unstageInto restores selected entries in a temporary index. reset reads the
// entries from HEAD; without a first commit, update-index removes them.
func unstageInto(root, name, index string, selected []string) error {
	input := strings.Join(selected, "\x00")
	gitDirectory := "--git-dir=" + repository.Directory(root, name)
	hasHead, err := repository.HeadExists(root, name)
	if err != nil {
		return err
	}
	if hasHead {
		_, err = git.RunIndexed(root, index, input,
			"--literal-pathspecs", gitDirectory, "--work-tree="+root,
			"reset", "--quiet", "HEAD", "--pathspec-from-file=-", "--pathspec-file-nul")
		return err
	}
	_, err = git.RunIndexed(root, index, input,
		"--literal-pathspecs", gitDirectory, "--work-tree="+root,
		"update-index", "--force-remove", "-z", "--stdin")
	return err
}

// refuseLinks reports the unsafe symbolic links a prepared index holds. The
// link and its target have to be selected together, so a partial selection is
// named as the reason instead of being applied.
func refuseLinks(matcher *config.Matcher, root string, indexes map[string]string) error {
	refused, err := repository.IndexLinkIssues(matcher, root, indexes)
	if err != nil {
		return err
	}
	var issues []error
	for _, entry := range refused {
		issues = append(issues, fmt.Errorf("%s %s in repository %q: %s", pathUnsafe, entry.Path, entry.Repository, entry.Reason))
	}
	if len(issues) == 0 {
		return nil
	}
	return errors.Join(append(issues, errors.New("Select the link and its target together."))...)
}

func repositories(selected []status.Change) []string {
	names := map[string]bool{}
	for _, change := range selected {
		names[change.Repository] = true
	}
	return slices.Sorted(maps.Keys(names))
}

func paths(selected []status.Change, name string) []string {
	owned := map[string]bool{}
	for _, change := range selected {
		if change.Repository == name {
			owned[change.Path] = true
			if change.From != "" {
				owned[change.From] = true
			}
		}
	}
	return slices.Sorted(maps.Keys(owned))
}

// report lists the changed paths, grouped by repository. The changes arrive
// sorted by repository and path. verb names what happened to them and added
// picks the color: green where content appeared, red where it was taken away.
func report(output io.Writer, verb string, added bool, selected []status.Change) {
	style := ui.For(output)
	changeStyle := style.Error
	if added {
		changeStyle = style.Success
	}
	noun := "paths"
	if len(selected) == 1 {
		noun = "path"
	}
	fmt.Fprintln(output, changeStyle.Render(fmt.Sprintf("%s %d %s", verb, len(selected), noun)))
	current := ""
	for _, change := range selected {
		if change.Repository != current {
			current = change.Repository
			fmt.Fprintf(output, "\n%s\n", style.Heading.Render(current+":"))
		}
		fmt.Fprintln(output, changeStyle.Render(fmt.Sprintf("  %-12s %s", change.Type, change.Path)))
	}
}
