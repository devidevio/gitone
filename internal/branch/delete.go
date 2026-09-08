// Deletion is the mirror image of creation: the same lock, the same complete
// preflight before the first ref changes, and the same recorded state. Which
// branches may go is native Git's own non-force rule, decided separately in
// every repository and only from local refs.

package branch

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/devidevio/gitone/internal/config"
	"github.com/devidevio/gitone/internal/git"
	"github.com/devidevio/gitone/internal/guidance"
	"github.com/devidevio/gitone/internal/lock"
	"github.com/devidevio/gitone/internal/repository"
	"github.com/devidevio/gitone/internal/status"
	"github.com/devidevio/gitone/internal/ui"
)

const (
	// DeleteShortFlag and DeleteFlag select the deletion of one local branch.
	DeleteShortFlag = "-d"
	DeleteFlag      = "--delete"

	// restoreMessage is the reflog entry of a ref an undo puts back. The
	// recorded reflog replaces it whenever the deleted branch had one.
	restoreMessage = "gitone branch: Restored deleted branch "
)

// deletion is one repository's approved part of a branch deletion. Commit is
// the tip the branch has, and Against the ref its mergedness was decided
// with, which the report names because that ref differs per repository.
type deletion struct {
	Name    string
	Commit  string
	Against string
}

// setting is one branch.<name>.* entry of a repository. A deletion removes
// them together with the branch, exactly like native Git, and records them so
// an undo can put them back.
type setting struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// remove deletes one branch from every repository. Every repository is
// checked before the first ref disappears, so a branch that is unsafe to
// delete anywhere is deleted nowhere.
func remove(configuration *config.Config, root, name string, input io.Reader, output io.Writer) error {
	if err := validate(name); err != nil {
		return err
	}
	release, err := lock.Acquire(root, Command)
	if err != nil {
		return err
	}
	defer release()

	inspected, err := Inspect(configuration, root)
	if err != nil {
		return err
	}
	targets, err := removable(root, inspected, name)
	if err != nil {
		return err
	}
	// The complete working tree is validated before the first ref changes,
	// exactly like every other mutating command. Branch preflight runs first,
	// so a branch problem is reported as one.
	if _, _, err := status.Validate(configuration, root); err != nil {
		return err
	}
	return ui.SpinBuffered(input, output, "Deleting branch "+name, func(rendered io.Writer) error {
		return applyDeletion(root, targets, name, rendered)
	})
}

// removable is the deletion every repository accepts, or the complete list of
// reasons it is refused. It reports all blocking repositories at once and
// completes before anything is written.
func removable(root string, inspected []State, name string) ([]deletion, error) {
	var issues []string
	targets := make([]deletion, 0, len(inspected))
	for _, current := range inspected {
		if !slices.Contains(current.Branches, name) {
			issues = append(issues, problem(current.Name, "does not have branch %s", name))
			continue
		}
		if current.Current == name {
			issues = append(issues, problem(current.Name,
				"is on branch %s: switch to another branch before deleting it", name))
			continue
		}
		commit, err := repository.Reference(root, current.Name, "refs/heads/"+name)
		if err != nil {
			return nil, err
		}
		against, refusal, err := mergedness(root, current.Name, name)
		if err != nil {
			return nil, err
		}
		if refusal != "" {
			issues = append(issues, problem(current.Name, "%s", refusal))
			continue
		}
		targets = append(targets, deletion{Name: current.Name, Commit: commit, Against: against})
	}
	if len(issues) == 0 {
		return targets, nil
	}
	return nil, errors.New(strings.Join(append(issues, unmodified), "\n"))
}

// mergedness applies native Git's own non-force deletion rule to one
// repository: the branch has to be contained in its configured upstream, or
// in HEAD when it configures none. It names the ref it compared with and the
// reason the branch has to stay, if there is one. Only local refs are read,
// so an upstream this repository has never fetched is refused instead of
// being replaced by a guessed comparison.
func mergedness(root, name, branch string) (against, refusal string, err error) {
	gitDirectory := "--git-dir=" + repository.Directory(root, name)
	configured, err := git.Run(root, gitDirectory, "for-each-ref",
		"--format=%(upstream)%00%(upstream:short)", "refs/heads/"+branch)
	if err != nil {
		return "", "", err
	}
	full, label, _ := strings.Cut(strings.TrimSpace(configured), "\x00")
	if full == "" {
		// Git reports no upstream both for a branch that configures none and
		// for one whose configuration no remote can resolve. The second is a
		// broken setup, not a reason to fall back to HEAD.
		merge := "branch." + branch + ".merge"
		current, err := readSettings(root, name, branch)
		if err != nil {
			return "", "", err
		}
		if slices.ContainsFunc(current, func(entry setting) bool { return entry.Key == merge }) {
			return "", fmt.Sprintf("configures an upstream for branch %s that no remote resolves: repair or remove %s first",
				branch, merge), nil
		}
		full, label = "HEAD", "HEAD"
		if _, err := git.Run(root, gitDirectory, "rev-parse", "--verify", "HEAD"); err != nil {
			return label, fmt.Sprintf("has no commit on HEAD to compare branch %s with", branch), nil
		}
	} else {
		upstream, err := repository.Reference(root, name, full)
		if err != nil {
			return "", "", err
		}
		if upstream == "" {
			return label, fmt.Sprintf("cannot compare branch %s with its configured upstream %s, which does not exist locally: run gitone fetch %s",
				branch, label, name), nil
		}
	}
	if _, err := git.Run(root, gitDirectory, "merge-base", "--is-ancestor", "refs/heads/"+branch, full); err != nil {
		return label, fmt.Sprintf("has not merged branch %s into %s: merge or publish it first", branch, label), nil
	}
	return label, "", nil
}

// applyDeletion removes the branch from every approved repository in
// configuration order. The tip, the reflog and the branch settings of every
// repository are recorded before the first ref disappears, so an interrupted
// deletion can be finished or completely undone afterwards.
func applyDeletion(root string, targets []deletion, name string, output io.Writer) error {
	directory := lock.RecoveryPath(root)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	saved := &recorded{Version: stateVersion, Command: Command, Branch: name, Marker: rand.Text()}
	for _, target := range targets {
		entry := deleteRecord{Name: target.Name, Commit: target.Commit}
		var err error
		if entry.Reflog, err = readReflog(root, target.Name, name); err != nil {
			_ = os.RemoveAll(directory)
			return err
		}
		if entry.Settings, err = readSettings(root, target.Name, name); err != nil {
			_ = os.RemoveAll(directory)
			return err
		}
		saved.Deleted = append(saved.Deleted, entry)
	}
	if err := lock.WriteState(directory, saved); err != nil {
		_ = os.RemoveAll(directory)
		return err
	}

	for index, target := range targets {
		if _, err := resolveDeletion(root, saved, index, true); err != nil {
			return interruptedDeletion(root, directory, saved, target.Name, err)
		}
	}
	if err := os.RemoveAll(directory); err != nil {
		return err
	}
	reportDeletion(output, targets, name)
	return nil
}

// interruptedDeletion closes a deletion that failed after preflight. Every
// branch it already removed is put back, which returns the whole project to
// its starting state. Otherwise the recorded state stays for gitone recover
// and gitone abort.
func interruptedDeletion(root, directory string, saved *recorded, name string, failure error) error {
	reason := fmt.Errorf("%s repository %q could not delete branch %s: %s",
		branchFailed, name, saved.Branch, detail(failure))
	if err := resumeDeletion(root, saved, false, io.Discard); err != nil {
		return errors.Join(reason, err, fmt.Errorf(
			"%s the branches deleted before it were not restored: run gitone abort to finish restoring the branches",
			recoveryRequired))
	}
	if err := os.RemoveAll(directory); err != nil {
		return err
	}
	return fmt.Errorf("%w\n%s", reason, unmodified)
}

// Restoration keeps its unique reflog marker until every ref and setting is
// restored. Only then are original reflogs put back; that last phase never
// creates or deletes refs, so it is safe to repeat without the marker.
func resumeDeletion(root string, saved *recorded, recovering bool, output io.Writer) error {
	directory := lock.RecoveryPath(root)
	if recovering && (saved.Finalizing || slices.ContainsFunc(saved.Deleted, func(entry deleteRecord) bool { return entry.Restoring })) {
		return fmt.Errorf("%s branch restoration has started: run gitone abort to finish it", recoveryRequired)
	}
	for _, entry := range saved.Deleted {
		if err := verifyDeletion(root, saved, entry); err != nil {
			return err
		}
	}
	if saved.Marker == "" {
		saved.Marker = rand.Text()
		if err := lock.WriteState(directory, saved); err != nil {
			return err
		}
	}
	names := make([]string, len(saved.Deleted))
	results := make([]string, len(saved.Deleted))
	for position := range saved.Deleted {
		index := position
		if !recovering {
			index = len(saved.Deleted) - 1 - position
		}
		names[position] = saved.Deleted[index].Name
		if saved.Finalizing {
			results[position] = "restored " + saved.Branch
			continue
		}
		result, err := resolveDeletion(root, saved, index, recovering)
		if err != nil {
			return err
		}
		results[position] = result
	}
	if !recovering {
		saved.Finalizing = true
		if err := lock.WriteState(directory, saved); err != nil {
			return err
		}
		for _, entry := range saved.Deleted {
			if err := verifyDeletion(root, saved, entry); err != nil {
				return err
			}
			if entry.Restoring {
				if err := writeReflog(root, entry.Name, saved.Branch, entry.Reflog); err != nil {
					return err
				}
			}
		}
	}
	reportResume(output, recovering, names, results)
	return nil
}

// A saved deletion intent makes an existing ref ambiguous: Git may have
// deleted it before the process stopped and another writer recreated it.
// Never infer branch identity from the commit alone.
func verifyDeletion(root string, saved *recorded, entry deleteRecord) error {
	directory := lock.RecoveryPath(root)
	existing, err := repository.Reference(root, entry.Name, "refs/heads/"+saved.Branch)
	if err != nil {
		return err
	}
	if existing != "" {
		if existing != entry.Commit {
			return recreated(entry, saved.Branch, directory)
		}
		logged, err := readReflog(root, entry.Name, saved.Branch)
		if err != nil {
			return err
		}
		if entry.Restoring {
			owned, err := OwnsRef(root, entry.Name, saved.Branch, restoreMessage+saved.Marker)
			if err != nil {
				return err
			}
			if !owned && !(saved.Finalizing && bytes.Equal(logged, entry.Reflog)) {
				return recreated(entry, saved.Branch, directory)
			}
		} else if entry.Started || !bytes.Equal(logged, entry.Reflog) {
			return recreated(entry, saved.Branch, directory)
		}
	} else if saved.Finalizing || !entry.Started && !entry.Restoring {
		return recreated(entry, saved.Branch, directory)
	}
	current, err := readSettings(root, entry.Name, saved.Branch)
	if err != nil {
		return err
	}
	// Empty settings are also expected after deletion, but not on a branch
	// the operation has never reached or after restoration is complete.
	if !slices.Equal(current, entry.Settings) &&
		!(len(current) == 0 && !saved.Finalizing && (entry.Started || entry.Restoring || existing == "")) {
		return settingsChanged(entry.Name, saved.Branch, directory)
	}
	return nil
}

func resolveDeletion(root string, saved *recorded, index int, recovering bool) (string, error) {
	entry := &saved.Deleted[index]
	if err := verifyDeletion(root, saved, *entry); err != nil {
		return "", err
	}
	existing, err := repository.Reference(root, entry.Name, "refs/heads/"+saved.Branch)
	if err != nil {
		return "", err
	}
	directory := lock.RecoveryPath(root)
	if recovering {
		// Persist intent before the destructive step, including during recover.
		entry.Started = true
		if err := lock.WriteState(directory, saved); err != nil {
			return "", err
		}
		if existing != "" {
			if err := DeleteRef(root, entry.Name, saved.Branch, entry.Commit); err != nil {
				return "", err
			}
		}
		if err := writeSettings(root, entry.Name, saved.Branch, entry.Settings, true); err != nil {
			return "", err
		}
		return "removed " + saved.Branch, nil
	}
	if existing == "" {
		entry.Restoring = true
		if err := lock.WriteState(directory, saved); err != nil {
			return "", err
		}
		if err := CreateRef(root, entry.Name, saved.Branch, entry.Commit, restoreMessage+saved.Marker); err != nil {
			return "", err
		}
	}
	if err := writeSettings(root, entry.Name, saved.Branch, entry.Settings, false); err != nil {
		return "", err
	}
	if entry.Restoring {
		return "restored " + saved.Branch + " at " + short(entry.Commit), nil
	}
	return "unchanged, " + saved.Branch + " was not deleted", nil
}

// reflogPath is the log Git keeps for one branch. Deleting the ref removes
// it, so a deletion snapshots it to be able to put it back.
func reflogPath(root, name, branch string) string {
	return filepath.Join(repository.Directory(root, name), "logs", "refs", "heads", filepath.FromSlash(branch))
}

func readReflog(root, name, branch string) ([]byte, error) {
	contents, err := os.ReadFile(reflogPath(root, name, branch))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return contents, err
}

// writeReflog restores the snapshot. A repository that keeps no reflogs took
// an empty one, so the entry the restored ref just wrote is removed again.
func writeReflog(root, name, branch string, contents []byte) error {
	path := reflogPath(root, name, branch)
	if len(contents) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".gitone-reflog-*")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	if _, err := temporary.Write(contents); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Chmod(0o644); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporary.Name(), path)
}

// readSettings are the branch.<branch>.* entries of one repository, in the
// order Git stores them, so restoring them reproduces multi-valued keys too.
func readSettings(root, name, branch string) ([]setting, error) {
	// git config reports "nothing matched" as exit code 1, which is an answer
	// rather than a failure.
	output, err := git.RunIsolatedInput(root, "", 1, "--git-dir="+repository.Directory(root, name),
		"config", "--local", "--null", "--get-regexp", "^branch\\."+regexp.QuoteMeta(branch)+"\\.")
	if err != nil {
		return nil, err
	}
	var settings []setting
	for entry := range strings.SplitSeq(strings.TrimSuffix(output, "\x00"), "\x00") {
		if key, value, found := strings.Cut(entry, "\n"); found {
			settings = append(settings, setting{Key: key, Value: value})
		}
	}
	return settings, nil
}

// writeSettings edits a copy while holding Git's config lock, then publishes
// all keys together. A crash cannot leave half of a multi-key restoration.
func writeSettings(root, name, branch string, snapshot []setting, deleting bool) error {
	path := filepath.Join(repository.Directory(root, name), "config")
	locked, err := os.OpenFile(path+".lock", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer locked.Close()
	defer os.Remove(path + ".lock")
	current, err := readSettings(root, name, branch)
	if err != nil {
		return err
	}
	if len(current) != 0 && !slices.Equal(current, snapshot) {
		return settingsChanged(name, branch, lock.RecoveryPath(root))
	}
	if deleting && len(current) == 0 || !deleting && slices.Equal(current, snapshot) {
		return nil
	}
	original, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(lock.RecoveryPath(root), "branch-config-*")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	if _, err := temporary.Write(original); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if deleting {
		_, err = git.Run(root, "config", "--file", temporary.Name(), "--remove-section", "branch."+branch)
		if err != nil {
			return err
		}
	} else {
		for _, entry := range snapshot {
			if _, err := git.Run(root, "config", "--file", temporary.Name(), "--add", entry.Key, entry.Value); err != nil {
				return err
			}
		}
	}
	contents, err := os.ReadFile(temporary.Name())
	if err != nil {
		return err
	}
	if err := locked.Chmod(info.Mode().Perm()); err != nil {
		return err
	}
	if _, err := locked.Write(contents); err != nil {
		return err
	}
	if err := locked.Close(); err != nil {
		return err
	}
	return os.Rename(path+".lock", path)
}

func settingsChanged(name, branch, directory string) error {
	return fmt.Errorf("%s repository %q changed the settings of branch %s outside GitOne: resolve it manually\n"+
		"  1. back up and reconcile every participating repository using %s\n"+
		"  2. archive %s outside the project afterwards, do not delete it",
		recoveryRequired, name, branch, guidance.InterruptedBranchDeletion, directory)
}

// reportDeletion states the removed branch of every repository, the ref it
// was compared with, and that no remote was involved, because a Git branch
// deletion that never contacts one is worth saying out loud.
func reportDeletion(output io.Writer, targets []deletion, name string) {
	style := ui.For(output)
	fmt.Fprintf(output, "%s\n\n", style.Heading.Render("Branch "+name+" deleted:"))
	for _, target := range targets {
		fmt.Fprintf(output, "%-10s %s\n", target.Name,
			style.Success.Render(fmt.Sprintf("✓ %s was %s, merged into %s", name, short(target.Commit), target.Against)))
	}
	fmt.Fprintf(output, "\n%s\n", style.Muted.Render(
		"Only local refs were read: nothing was fetched, and no remote branch was deleted."))
}

func recreated(entry deleteRecord, branch, directory string) error {
	return fmt.Errorf("%s repository %q cannot safely resume the interrupted branch deletion: resolve it manually\n"+
		"  1. inspect branch %s, which was being deleted at %s\n"+
		"  2. back up and reconcile every participating repository using %s\n"+
		"  3. archive %s outside the project afterwards, do not delete it",
		recoveryRequired, entry.Name, branch, short(entry.Commit), guidance.InterruptedBranchDeletion, directory)
}
