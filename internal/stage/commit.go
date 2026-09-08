package stage

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
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
	"github.com/devidevio/gitone/internal/worktree"
)

const (
	repositoryInvalid = "REPO001"
	gitFailure        = "GIT001"

	// groupTrailer ties the commits of one grouped operation together.
	groupTrailer = "GitOne-Group"

	// candidateSuffix names the prepared index of a path-limited commit. It
	// holds exactly the tree that commit would create, so the result can be
	// validated before the first commit exists.
	candidateSuffix = ".candidate"
)

// Commit creates one native commit in every repository that has staged
// changes. Explicit paths turn it into a native path-limited commit instead:
// only the repositories owning them take part, each commits the current
// working-tree version of its own selected paths, and every unselected staged
// change stays staged. The repositories are committed in name order under the
// project lock, and the state recorded beforehand lets a partial failure be
// finished by Recover or undone by Abort. Nothing is staged and no remote is
// contacted.
func Commit(configuration *config.Config, root, directory string, arguments []string, message string, output io.Writer) error {
	release, err := lock.Acquire(root, commitCommand)
	if err != nil {
		return err
	}
	defer release()

	result, inventory, err := status.Validate(configuration, root)
	if err != nil {
		return err
	}

	recovery := recoveryPath(root)
	if err := os.MkdirAll(recovery, 0o700); err != nil {
		return err
	}
	names, identities, err := participants(configuration, root, directory, recovery, result, inventory, arguments)
	if err != nil || len(names) == 0 {
		// Nothing was committed yet, so no state has to survive.
		_ = os.RemoveAll(recovery)
		if err != nil {
			return err
		}
		fmt.Fprintln(output, "Nothing to commit.")
		return nil
	}
	saved, err := record(root, recovery, names, identities, message)
	if err != nil {
		_ = os.RemoveAll(recovery)
		return err
	}
	if err := commitPending(configuration, root, recovery, saved); err != nil {
		return fmt.Errorf("%w\n%s the commit is incomplete: run gitone recover or gitone abort", err, recoveryRequired)
	}
	if err := os.RemoveAll(recovery); err != nil {
		return err
	}
	printCommits(output, saved)
	return nil
}

// participants lists the repositories that receive a commit and, for a
// path-limited commit, the paths each of them commits. Without paths every
// repository with staged changes takes part; with paths only a repository
// whose selection really changes the tree its HEAD holds.
func participants(configuration *config.Config, root, directory, recovery string, result *status.Status,
	inventory *worktree.Result, arguments []string) ([]string, map[string][]pathState, error) {
	if len(arguments) == 0 {
		staged := filter(result.Changes, func(change status.Change) bool { return change.State == status.Staged })
		names := repositories(staged)
		if len(names) == 0 {
			return nil, nil, nil
		}
		matcher, err := configuration.Matcher()
		if err != nil {
			return nil, nil, err
		}
		if err := refuseUnownedIndexes(matcher, root, indexPaths(configuration, root),
			"Stage pending deletions with gitone add -u before committing."); err != nil {
			return nil, nil, err
		}
		return names, nil, nil
	}
	selection, err := selectedPaths(inventory, root, directory, arguments)
	if err != nil {
		return nil, nil, err
	}
	identities, err := selectedIdentities(root, selection)
	if err != nil {
		return nil, nil, err
	}
	names, err := prepareSelection(configuration, root, recovery, selection)
	return names, identities, err
}

// selectedIdentities captures the selected working-tree entries before their
// candidate indexes are built. A later change therefore cannot become the
// recorded, trusted version without having been validated.
func selectedIdentities(root string, selection map[string][]string) (map[string][]pathState, error) {
	identities := map[string][]pathState{}
	for name, paths := range selection {
		for _, selected := range paths {
			identity, err := hashFile(filepath.Join(root, filepath.FromSlash(selected)))
			if err != nil {
				return nil, err
			}
			identities[name] = append(identities[name], pathState{Path: selected, Original: identity})
		}
	}
	return identities, nil
}

// selectedPaths resolves the explicit path arguments of a path-limited commit
// into the owned paths of each repository. Every argument must name one
// managed file that Git already knows, so a new file becomes selectable only
// once it has been staged; directories, globs and Git pathspecs are not
// resolved.
func selectedPaths(inventory *worktree.Result, root, directory string, arguments []string) (map[string][]string, error) {
	selection := map[string][]string{}
	for _, argument := range arguments {
		relative, err := worktree.Relative(root, directory, argument)
		if err != nil {
			return nil, err
		}
		owner, managed := inventory.Owners[relative]
		if !managed {
			return nil, fmt.Errorf("%s %s: path is not a managed file", pathUnsafe, argument)
		}
		if !slices.Contains(inventory.Tracked, relative) {
			return nil, fmt.Errorf("%s %s: path is not known to Git yet: stage it with gitone add first", pathUnsafe, argument)
		}
		if !slices.Contains(selection[owner], relative) {
			selection[owner] = append(selection[owner], relative)
		}
	}
	for name := range selection {
		slices.Sort(selection[name])
	}
	return selection, nil
}

// prepareSelection writes the index every selected commit would produce
// beside the real ones and validates the results together. A repository whose
// selection changes nothing is left out, so it receives no commit at all.
func prepareSelection(configuration *config.Config, root, recovery string, selection map[string][]string) ([]string, error) {
	matcher, err := configuration.Matcher()
	if err != nil {
		return nil, err
	}
	indexes := indexPaths(configuration, root)
	var names []string
	for _, name := range slices.Sorted(maps.Keys(selection)) {
		prepared := filepath.Join(recovery, name+candidateSuffix)
		changed, err := candidate(root, name, prepared, selection[name])
		if err != nil {
			return nil, err
		}
		if !changed {
			continue
		}
		indexes[name] = prepared
		names = append(names, name)
	}
	if err := refuseUnownedIndexes(matcher, root, indexes,
		"Select pending deletions together with the configuration, or stage them with gitone add -u."); err != nil {
		return nil, err
	}
	// The prepared trees are one project result, so a directory link also
	// sees descendants held by repositories this commit does not change.
	if err := refuseLinks(matcher, root, indexes); err != nil {
		return nil, err
	}
	return names, nil
}

// candidate writes the index a native path-limited commit of selected would
// commit: the tree of HEAD with the current working-tree version of every
// selected path, which is exactly what "git commit <path>..." creates. It
// reports whether that differs from HEAD, which is what makes a repository
// participate at all.
func candidate(root, name, index string, selected []string) (bool, error) {
	gitDirectory := "--git-dir=" + repository.Directory(root, name)
	existing, err := repository.HeadExists(root, name)
	if err != nil {
		return false, err
	}
	source := "--empty"
	if existing {
		source = "HEAD"
	}
	if _, err := git.RunIndexed(root, index, "", gitDirectory, "--work-tree="+root, "read-tree", source); err != nil {
		return false, err
	}
	// update-index is the plumbing a partial commit uses itself: it takes the
	// working-tree version of each path and removes the ones that are gone.
	if _, err := git.RunIndexed(root, index, strings.Join(selected, "\x00"),
		"--literal-pathspecs", gitDirectory, "--work-tree="+root,
		"update-index", "--add", "--remove", "-z", "--stdin"); err != nil {
		return false, err
	}
	prepared, err := git.RunIndexed(root, index, "", gitDirectory, "--work-tree="+root, "write-tree")
	if err != nil {
		return false, err
	}
	current, err := previousTree(root, gitDirectory, existing)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(prepared) != current, nil
}

// previousTree is the tree a commit starts from: the one HEAD points at, or
// the empty tree while the branch is still unborn.
func previousTree(root, gitDirectory string, existing bool) (string, error) {
	if !existing {
		// hash-object reports the empty tree of the repository's own hash
		// algorithm instead of a hard-coded SHA-1 identifier.
		output, err := git.Run(root, gitDirectory, "hash-object", "-t", "tree", "--stdin")
		return strings.TrimSpace(output), err
	}
	output, err := git.Run(root, gitDirectory, "rev-parse", "HEAD^{tree}")
	return strings.TrimSpace(output), err
}

// refuseUnowned reports every path a prepared commit tree holds that its
// repository must not commit: a protected, reserved or foreign path.
// indexPaths maps every repository to its real index file.
func indexPaths(configuration *config.Config, root string) map[string]string {
	indexes := map[string]string{}
	for _, name := range configuration.RepositoryNames() {
		indexes[name] = repository.IndexPath(root, name)
	}
	return indexes
}

// refuseUnownedIndexes validates the given index of every repository. A
// missing file can keep its former owner for staging, but a commit must
// remove it before recording the configuration without its rule; hint tells
// the user how to include the deletion.
func refuseUnownedIndexes(matcher *config.Matcher, root string, indexes map[string]string, hint string) error {
	for _, name := range slices.Sorted(maps.Keys(indexes)) {
		if err := refuseUnowned(matcher, root, name, indexes[name]); err != nil {
			return fmt.Errorf("%w\n%s", err, hint)
		}
	}
	return nil
}

func refuseUnowned(matcher *config.Matcher, root, name, index string) error {
	output, err := git.RunIndexed(root, index, "", "--no-optional-locks",
		"--git-dir="+repository.Directory(root, name), "--work-tree="+root, "ls-files", "-z")
	if err != nil {
		return err
	}
	var issues []error
	for entryPath := range strings.SplitSeq(output, "\x00") {
		if entryPath == "" {
			continue
		}
		if reason := matcher.Unowned(name, entryPath); reason != "" {
			issues = append(issues, fmt.Errorf("%s %s in repository %q: %s", pathUnsafe, entryPath, name, reason))
		}
	}
	return errors.Join(issues...)
}

// record saves the index, branch and HEAD every participating repository
// starts from, together with the message, the group and the path selection
// each repository commits. Writing the state file is the point from which an
// interrupted commit becomes recoverable.
func record(root, directory string, names []string, identities map[string][]pathState, message string) (*state, error) {
	saved := &state{Version: stateVersion, Command: commitCommand, Message: message}
	// A single repository produces an ordinary native commit; only a grouped
	// operation needs an identifier to reconstruct it later.
	if len(names) > 1 {
		group, err := identifier()
		if err != nil {
			return nil, err
		}
		saved.Group = group
	}
	for _, name := range names {
		branch, err := branchOf(root, name)
		if err != nil {
			return nil, err
		}
		head, err := headOf(root, name, branch)
		if err != nil {
			return nil, err
		}
		if err := replace(repository.IndexPath(root, name), filepath.Join(directory, name+originalSuffix)); err != nil {
			return nil, err
		}
		current, err := indexIdentity(root, name)
		if err != nil {
			return nil, err
		}
		entry := repositoryState{Name: name, Branch: branch, Original: current, Head: head, Selected: identities[name]}
		saved.Repositories = append(saved.Repositories, entry)
	}
	return saved, lock.WriteState(directory, saved)
}

// commitPending commits every repository that carries no recorded commit yet.
// Each result is recorded before the next repository is touched, so a failure
// in the middle leaves exactly the finished work behind.
//
// ponytail: a crash between the native commit and the state write leaves a
// commit that is not recorded; recovery then refuses with manual steps instead
// of matching the existing commit back to the operation.
func commitPending(configuration *config.Config, root, directory string, saved *state) error {
	for index := range saved.Repositories {
		entry := &saved.Repositories[index]
		if entry.Committed != "" {
			if err := verifyGroup(root, entry.Name, saved.Group); err != nil {
				return err
			}
			continue
		}
		if err := verifyRecorded(root, entry, directory); err != nil {
			return err
		}
		id, err := createCommit(configuration, root, directory, saved, entry, message(saved))
		if err != nil {
			return err
		}
		entry.Committed = id
		// A pre-commit hook may have changed the index, so what the commit
		// left behind is recorded instead of assumed.
		if entry.Next, err = indexIdentity(root, entry.Name); err != nil {
			return err
		}
		if err := lock.WriteState(directory, saved); err != nil {
			return err
		}
		if err := verifyGroup(root, entry.Name, saved.Group); err != nil {
			return err
		}
	}
	return nil
}

// createCommit runs native git commit against one repository, so its
// pre-commit, commit-msg and post-commit hooks, signing configuration and Git
// LFS filters behave exactly as they do for a plain git invocation. With a
// selection it becomes native path-limited commit: the working-tree version
// of those paths is committed and every other staged change stays staged.
func createCommit(configuration *config.Config, root, directory string, saved *state,
	entry *repositoryState, message string) (string, error) {
	gitDirectory := repository.Directory(root, entry.Name)
	if len(entry.Selected) != 0 {
		return createSelectiveCommit(configuration, root, directory, saved, entry, message)
	}
	// -F - is -m with the message on standard input: Git applies the same
	// whitespace cleanup and a failure reports the command, not the message.
	arguments := []string{"--literal-pathspecs", "--git-dir=" + gitDirectory, "--work-tree=" + root, "commit", "--quiet", "-F", "-"}
	if _, err := git.RunInput(root, message, arguments...); err != nil {
		return "", err
	}
	head, err := git.Run(root, "--git-dir="+gitDirectory, "rev-parse", "HEAD")
	return strings.TrimSpace(head), err
}

// createSelectiveCommit runs the native hooks against the prepared candidate,
// validates their exact result, then commits that same index with hooks
// disabled for the final ref update. The real index is never substituted or
// changed, so unselected staged entries stay staged.
func createSelectiveCommit(configuration *config.Config, root, directory string, saved *state,
	entry *repositoryState, message string) (string, error) {
	index := filepath.Join(directory, entry.Name+candidateSuffix)
	prepared, err := candidate(root, entry.Name, index, entry.selection())
	if err != nil {
		return "", err
	}
	if !prepared {
		return "", fmt.Errorf("%s repository %q has no selected change to commit", gitFailure, entry.Name)
	}
	message, err = runCommitHooks(root, directory, entry.Name, index, message)
	if err != nil {
		return "", err
	}
	if err := validateHookResult(configuration, root, directory, saved, entry); err != nil {
		return "", err
	}

	disabledHooks := filepath.Join(directory, "disabled-hooks")
	if err := os.MkdirAll(disabledHooks, 0o700); err != nil {
		return "", err
	}
	gitDirectory := repository.Directory(root, entry.Name)
	arguments := []string{"-c", "core.hooksPath=" + disabledHooks, "--git-dir=" + gitDirectory,
		"--work-tree=" + root, "commit", "--quiet", "--no-verify", "-F", "-"}
	if _, err := git.RunIndexed(root, index, message, arguments...); err != nil {
		return "", err
	}
	if err := updateSelectedIndex(root, entry.Name, entry.selection()); err != nil {
		return "", err
	}
	head, err := git.Run(root, "--git-dir="+gitDirectory, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	_ = git.RunHook(root, gitDirectory, index, "post-commit")
	return strings.TrimSpace(head), nil
}

// updateSelectedIndex gives the real index the same post-commit state as a
// native path commit: selected entries now match HEAD, while every unselected
// staged entry stays untouched.
func updateSelectedIndex(root, name string, selected []string) error {
	_, err := git.RunInput(root, strings.Join(selected, "\x00"),
		"--literal-pathspecs",
		"--git-dir="+repository.Directory(root, name),
		"--work-tree="+root,
		"reset", "--quiet", "HEAD", "--pathspec-from-file=-", "--pathspec-file-nul")
	return err
}

func runCommitHooks(root, directory, name, index, message string) (string, error) {
	gitDirectory := repository.Directory(root, name)
	if err := git.RunHook(root, gitDirectory, index, "pre-commit"); err != nil {
		return "", err
	}
	messageFile := filepath.Join(directory, name+".message")
	if err := os.WriteFile(messageFile, []byte(message), 0o600); err != nil {
		return "", err
	}
	if err := git.RunHook(root, gitDirectory, index, "prepare-commit-msg", messageFile, "message"); err != nil {
		return "", err
	}
	if err := git.RunHook(root, gitDirectory, index, "commit-msg", messageFile); err != nil {
		return "", err
	}
	contents, err := os.ReadFile(messageFile)
	return string(contents), err
}

// validateHookResult rejects a hook that widened the requested selection, then
// applies the ownership and safe-link checks to every projected commit tree.
func validateHookResult(configuration *config.Config, root, directory string, saved *state,
	entry *repositoryState) error {
	index := filepath.Join(directory, entry.Name+candidateSuffix)
	if err := refuseUnselected(root, entry.Name, index, entry.selection()); err != nil {
		return err
	}
	matcher, err := configuration.Matcher()
	if err != nil {
		return err
	}
	indexes := map[string]string{}
	for _, name := range configuration.RepositoryNames() {
		indexes[name] = repository.IndexPath(root, name)
	}
	for _, projected := range saved.Repositories {
		candidateIndex := filepath.Join(directory, projected.Name+candidateSuffix)
		if err := refuseUnowned(matcher, root, projected.Name, candidateIndex); err != nil {
			return err
		}
		indexes[projected.Name] = candidateIndex
	}
	return refuseLinks(matcher, root, indexes)
}

func refuseUnselected(root, name, index string, selected []string) error {
	gitDirectory := "--git-dir=" + repository.Directory(root, name)
	existing, err := repository.HeadExists(root, name)
	if err != nil {
		return err
	}
	previous, err := previousTree(root, gitDirectory, existing)
	if err != nil {
		return err
	}
	current, err := git.RunIndexed(root, index, "", gitDirectory, "--work-tree="+root, "write-tree")
	if err != nil {
		return err
	}
	output, err := git.Run(root, gitDirectory, "diff-tree", "--no-commit-id", "--name-only", "-r", "-z",
		previous, strings.TrimSpace(current))
	if err != nil {
		return err
	}
	for changed := range strings.SplitSeq(output, "\x00") {
		if changed != "" && !slices.Contains(selected, changed) {
			return fmt.Errorf("%s %s in repository %q: a commit hook changed an unselected path", pathUnsafe, changed, name)
		}
	}
	return nil
}

// message is the commit message including the group trailer. Git parses the
// trailer out of the last paragraph, so the group becomes a real trailer.
func message(saved *state) string {
	if saved.Group == "" {
		return saved.Message
	}
	return strings.TrimRight(saved.Message, "\n") + "\n\n" + groupTrailer + ": " + saved.Group + "\n"
}

// verifyGroup checks that the created commit really carries the group. A
// commit-msg hook may rewrite the message, and a grouped operation whose
// commits cannot be found again is not a group.
func verifyGroup(root, name, group string) error {
	if group == "" {
		return nil
	}
	output, err := git.Run(root, "--git-dir="+repository.Directory(root, name),
		"log", "-1", "--format=%(trailers:key="+groupTrailer+",valueonly)")
	if err != nil {
		return err
	}
	if strings.TrimSpace(output) != group {
		return fmt.Errorf("%s repository %q did not keep the %s trailer: a commit-msg hook removed or changed it", gitFailure, name, groupTrailer)
	}
	return nil
}

// restore moves every committed repository back to the branch position and
// index the commit started from. The whole operation is verified before the
// first repository changes, and no working-tree file is ever written back.
func restore(root, directory string, saved *state) error {
	for index := range saved.Repositories {
		entry := &saved.Repositories[index]
		if entry.Committed == "" {
			continue
		}
		if err := resetBranch(root, entry); err != nil {
			return err
		}
		if err := replace(filepath.Join(directory, entry.Name+originalSuffix), repository.IndexPath(root, entry.Name)); err != nil {
			return err
		}
		entry.Committed, entry.Next = "", ""
		if err := lock.WriteState(directory, saved); err != nil {
			return err
		}
	}
	return nil
}

// resetBranch moves the branch back, or removes it again when the commit was
// the first one on an unborn branch. The recorded commit is passed as the
// expected old value, so Git itself refuses a ref that moved meanwhile.
func resetBranch(root string, entry *repositoryState) error {
	arguments := []string{"--git-dir=" + repository.Directory(root, entry.Name), "update-ref"}
	if entry.Head == "" {
		arguments = append(arguments, "-d", "refs/heads/"+entry.Branch, entry.Committed)
	} else {
		arguments = append(arguments, "refs/heads/"+entry.Branch, entry.Head, entry.Committed)
	}
	_, err := git.Run(root, arguments...)
	return err
}

// verifyRecorded reports whether a repository still holds the branch, HEAD,
// index and selected working-tree files the operation recorded for it. It
// guards every commit, recovery and abort against a repository or a selected
// file that changed outside GitOne.
func verifyRecorded(root string, entry *repositoryState, directory string) error {
	branch, err := branchOf(root, entry.Name)
	if err != nil {
		return err
	}
	head, err := headOf(root, entry.Name, branch)
	if err != nil {
		return err
	}
	current, err := indexIdentity(root, entry.Name)
	if err != nil {
		return err
	}
	wantedHead, wantedIndex := entry.Head, entry.Original
	if entry.Committed != "" {
		wantedHead, wantedIndex = entry.Committed, entry.Next
	}
	if branch != entry.Branch || head != wantedHead || current != wantedIndex {
		return staleCommit(entry, directory)
	}
	// A path-limited commit commits the working tree, so the recorded
	// selection may only be resumed while those files are still the ones the
	// commit was validated for.
	for _, selected := range entry.Selected {
		identity, err := hashFile(filepath.Join(root, filepath.FromSlash(selected.Path)))
		if err != nil {
			return err
		}
		if identity != selected.Original {
			return staleSelection(entry, selected.Path, directory)
		}
	}
	return nil
}

func staleCommit(entry *repositoryState, directory string) error {
	position := "the branch did not exist yet"
	if entry.Head != "" {
		position = "the branch pointed at " + entry.Head
	}
	return fmt.Errorf("%s repository %q changed outside GitOne since the interrupted commit: resolve it manually\n"+
		"  1. inspect it with git --git-dir=.gitone/repositories/%s log --oneline\n"+
		"  2. when the commit started, %s on branch %s\n"+
		"  3. restore that position and copy %s back into .gitone/repositories/%s/index\n"+
		"  4. archive %s outside the project afterwards, do not delete it",
		recoveryRequired, entry.Name, entry.Name, position, entry.Branch,
		filepath.Join(directory, entry.Name+originalSuffix), entry.Name, directory)
}

func staleSelection(entry *repositoryState, selected, directory string) error {
	return fmt.Errorf("%s the selected path %q changed outside GitOne since the interrupted commit: resolve it manually\n"+
		"  1. the recorded selection of repository %q is listed in %s\n"+
		"  2. put the version you wanted to commit back in place, then run gitone recover or gitone abort again\n"+
		"  3. or finish repository %q yourself and archive %s outside the project afterwards, do not delete it",
		recoveryRequired, selected, entry.Name, filepath.Join(directory, stateFile), entry.Name, directory)
}

// branchOf is the branch HEAD points at, also while it is still unborn. A
// detached HEAD is rejected because a grouped commit must stay on one named
// branch per repository.
func branchOf(root, name string) (string, error) {
	output, err := git.Run(root, "--git-dir="+repository.Directory(root, name), "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return "", fmt.Errorf("%s repository %q has no branch checked out: HEAD is detached", repositoryInvalid, name)
	}
	return strings.TrimSpace(output), nil
}

// headOf is the commit a branch points at, or the empty string while the
// branch is unborn.
func headOf(root, name, branch string) (string, error) {
	output, err := git.Run(root, "--git-dir="+repository.Directory(root, name),
		"for-each-ref", "--format=%(objectname)", "refs/heads/"+branch)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(output), nil
}

// indexIdentity identifies what one index contains. Stat information is left
// out, because a failed native commit refreshes it without changing a single
// index entry.
func indexIdentity(root, name string) (string, error) {
	output, err := git.Run(root, "--no-optional-locks", "--git-dir="+repository.Directory(root, name),
		"--work-tree="+root, "ls-files", "--stage", "-z")
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(output))), nil
}

// identifier is the unpredictable shared group value.
func identifier() (string, error) {
	value := make([]byte, 4)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func printCommits(output io.Writer, saved *state) {
	for _, entry := range saved.Repositories {
		fmt.Fprintf(output, "%s:\n    commit %s\n\n", entry.Name, entry.Committed)
	}
	if saved.Group != "" {
		fmt.Fprintf(output, "%s: %s\n", groupTrailer, saved.Group)
	}
}
