package pull

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"

	"github.com/devidevio/gitone/internal/assign"
	"github.com/devidevio/gitone/internal/git"
	"github.com/devidevio/gitone/internal/lock"
	"github.com/devidevio/gitone/internal/repository"
)

const (
	stateFile           = lock.StateFile
	legacyStateVersion  = 1
	stateVersion        = 2
	originalIndexSuffix = ".original"
)

// state is the recorded progress of a pull. It shares the version and command
// envelope of the other recorded operations, so gitone recover and gitone
// abort can tell an interrupted pull from an interrupted commit or push.
type state struct {
	Version       int              `json:"version"`
	Command       string           `json:"command"`
	Repositories  []branchState    `json:"repositories"`
	Configuration []assign.Patched `json:"configuration,omitempty"`
}

// branchState is the fast-forward of one repository. Head is the commit its
// branch started from and Target the commit it is advanced to; Updated
// records that Git reported the fast-forward as done. The exact starting
// index is saved beside this state before the first branch moves.
type branchState struct {
	Name    string `json:"name"`
	Branch  string `json:"branch"`
	Head    string `json:"head"`
	Target  string `json:"target"`
	Updated bool   `json:"updated,omitempty"`
}

// advance fast-forwards every planned repository in configuration order and
// then writes the configuration patch the pull generated. The expected branch
// positions and the configuration files as they are now are recorded before
// the first one moves and the progress after every repository, so an
// interrupted pull can be finished or undone afterwards.
func advance(root string, participants []*participant, added *assign.Assignment, output io.Writer) error {
	var moving []*participant
	for _, current := range participants {
		if current.target != "" {
			moving = append(moving, current)
		}
	}
	if len(moving) == 0 {
		report(output, participants)
		return nil
	}

	directory := lock.RecoveryPath(root)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	saved := &state{Version: stateVersion, Command: Command}
	for _, current := range moving {
		if err := repository.SaveIndex(root, current.Name, filepath.Join(directory, current.Name+originalIndexSuffix)); err != nil {
			_ = os.RemoveAll(directory)
			return err
		}
		saved.Repositories = append(saved.Repositories,
			branchState{Name: current.Name, Branch: current.branch, Head: current.head, Target: current.target})
	}
	if added != nil {
		saved.Configuration = added.Files
		if err := assign.Save(root, directory, saved.Configuration); err != nil {
			_ = os.RemoveAll(directory)
			return err
		}
	}
	if err := lock.WriteState(directory, saved); err != nil {
		_ = os.RemoveAll(directory)
		return err
	}

	for index, current := range moving {
		if err := Forward(root, current.Name, current.target); err != nil {
			current.state, current.detail = failed, detail(err)
			return interrupted(root, directory, saved, participants, fmt.Errorf(
				"%s repository %q could not be advanced to %s: %s",
				pullFailed, current.Name, short(current.target), current.detail))
		}
		current.state = updated
		saved.Repositories[index].Updated = true
		if err := lock.WriteState(directory, saved); err != nil {
			return err
		}
	}
	// The patch extends the configuration the fast-forwards just checked out,
	// so it is written last and is part of the same recoverable transaction.
	if err := assign.Apply(root, saved.Configuration); err != nil {
		return interrupted(root, directory, saved, participants, fmt.Errorf(
			"%s the generated path assignments could not be written: %s", pullFailed, detail(err)))
	}
	if err := os.RemoveAll(directory); err != nil {
		return err
	}
	report(output, participants)
	return nil
}

// interrupted closes a pull that failed after preflight. The repositories it
// already advanced and every configuration file it patched are restored when
// they still hold exactly what this pull left behind, which returns the whole
// project to its starting state. Otherwise the recorded state stays for
// gitone recover and gitone abort.
func interrupted(root, directory string, saved *state, participants []*participant, reason error) error {
	if err := rollback(root, directory, saved); err != nil {
		for _, participant := range participants {
			if participant.state == updated {
				participant.state, participant.detail = failed, "recovery required; inspect the recorded state"
			}
		}
		return errors.Join(reason, err, fmt.Errorf(
			"%s the repositories advanced before it were not restored: run gitone abort to undo the pull or gitone recover to finish it",
			recoveryRequired))
	}
	for _, participant := range participants {
		if participant.state == updated {
			participant.state, participant.detail = unchanged, "restored to "+short(participant.head)
		}
	}
	if err := os.RemoveAll(directory); err != nil {
		return err
	}
	return fmt.Errorf("%w\n%s", reason, unmodified)
}

// rollback restores every repository this pull already advanced, newest
// first, and then every configuration file it patched. A repository that no
// longer holds what the pull left behind is not touched: its recovery state
// must be resolved instead.
func rollback(root, directory string, saved *state) error {
	if err := assign.Verify(root, directory, "pull", saved.Configuration); err != nil {
		return err
	}
	entries := slices.Clone(saved.Repositories)
	slices.Reverse(entries)
	for _, entry := range entries {
		if !entry.Updated {
			continue
		}
		branch, files, err := position(root, entry, assign.Names(saved.Configuration)...)
		if err != nil {
			return err
		}
		if branch != entry.Target || files != entry.Target {
			return changed(entry, directory)
		}
		if err := backward(root, directory, entry); err != nil {
			return err
		}
	}
	return assign.Restore(root, directory, saved.Configuration)
}

// Resume finishes or undoes an interrupted pull. recovering completes the
// remaining fast-forwards; otherwise every repository the pull advanced is
// restored to the commit it started from. A repository that changed outside
// GitOne since the interruption is never overwritten.
func Resume(root string, recovering bool, output io.Writer) error {
	directory := lock.RecoveryPath(root)
	saved, err := readState(directory)
	if err != nil {
		return err
	}
	if err := assign.Verify(root, directory, "pull", saved.Configuration); err != nil {
		return err
	}
	entries := slices.Clone(saved.Repositories)
	if !recovering {
		slices.Reverse(entries)
	}
	results := make([]string, len(entries))
	for index, entry := range entries {
		branch, files, err := position(root, entry, assign.Names(saved.Configuration)...)
		if err != nil {
			return err
		}
		results[index], err = resolve(root, entry, directory, branch, files, recovering)
		if err != nil {
			return err
		}
	}

	// The configuration patch belongs to the same transaction: recovering
	// finishes it on the repositories it now extends, aborting undoes it.
	configuration := assign.Restore
	if recovering {
		configuration = func(root, _ string, files []assign.Patched) error { return assign.Apply(root, files) }
	}
	if err := configuration(root, directory, saved.Configuration); err != nil {
		return err
	}

	verb := "aborted"
	if recovering {
		verb = "recovered"
	}
	fmt.Fprintf(output, "%s %s.\n\n", Command, verb)
	for index, entry := range entries {
		fmt.Fprintf(output, "%-10s %s\n", entry.Name, results[index])
	}
	for _, file := range saved.Configuration {
		fmt.Fprintf(output, "%-10s %s\n", file.Path, configurationResult(recovering))
	}
	return nil
}

func configurationResult(recovering bool) string {
	if recovering {
		return "assigned the new paths, unstaged"
	}
	return "restored to the version before the pull"
}

// resolve brings one recorded repository to the state the resumed command
// wants and reports what it did. branch and files are the recorded commits
// the branch ref and the working state currently hold, each empty when it is
// neither: that repository changed outside GitOne and is refused.
func resolve(root string, entry branchState, directory, branch, files string, recovering bool) (string, error) {
	switch {
	case branch == entry.Head && files == entry.Head:
		// The fast-forward never started.
		if !recovering {
			if err := restoreIndex(root, directory, entry.Name); err != nil {
				return "", err
			}
			return "unchanged at " + short(entry.Head), nil
		}
		if err := Forward(root, entry.Name, entry.Target); err != nil {
			return "", err
		}
		return "advanced " + entry.Branch + " to " + short(entry.Target), nil
	case branch == entry.Target && files == entry.Target:
		// The fast-forward finished.
		if recovering {
			return "already at " + short(entry.Target), nil
		}
		if err := backward(root, directory, entry); err != nil {
			return "", err
		}
		return "restored " + entry.Branch + " at " + short(entry.Head), nil
	case branch == entry.Head && files == entry.Target:
		// The files were replaced but the branch did not move yet.
		if recovering {
			if err := moveBranch(root, entry); err != nil {
				return "", err
			}
			return "advanced " + entry.Branch + " to " + short(entry.Target), nil
		}
		if err := backward(root, directory, entry); err != nil {
			return "", err
		}
		return "restored " + entry.Branch + " at " + short(entry.Head), nil
	}
	return "", changed(entry, directory)
}

// position reports which recorded commit the branch of one repository points
// at and which one its index and working tree hold. Each result is empty when
// it matches neither recorded commit.
func position(root string, entry branchState, patched ...string) (branch, files string, err error) {
	head, err := repository.Reference(root, entry.Name, "refs/heads/"+entry.Branch)
	if err != nil {
		return "", "", err
	}
	if head == entry.Head || head == entry.Target {
		branch = head
	}
	headMatches := repository.Matches(root, entry.Name, entry.Head, patched...)
	targetMatches := repository.Matches(root, entry.Name, entry.Target, patched...)
	switch {
	case branch == entry.Head && headMatches:
		files = entry.Head
	case branch == entry.Target && targetMatches:
		files = entry.Target
	case targetMatches:
		files = entry.Target
	case headMatches:
		files = entry.Head
	}
	return branch, files, nil
}

// Forward fast-forwards one repository. Native Git updates the index, the
// working-tree files and the branch in one step, and refuses anything that is
// not a fast-forward or that would overwrite local work. It never creates a
// merge commit. gitone reconfigure --pull applies its accepted incoming
// commits with exactly this fast-forward.
func Forward(root, name, target string) error {
	_, err := git.Run(root, "--git-dir="+repository.Directory(root, name), "--work-tree="+root,
		"merge", "--quiet", "--ff-only", target)
	return err
}

// backward restores one repository to the commit and exact index it started
// from. Only a repository that still holds what the pull left behind reaches
// this point, so this discards nothing but the pull's own work.
func backward(root, directory string, entry branchState) error {
	_, err := git.Run(root, "--git-dir="+repository.Directory(root, entry.Name), "--work-tree="+root,
		"reset", "--hard", "--quiet", entry.Head)
	if err != nil {
		return err
	}
	return restoreIndex(root, directory, entry.Name)
}

// moveBranch completes a fast-forward whose files are already in place. The
// expected old value is passed, so a branch that moved meanwhile is refused
// by Git itself.
func moveBranch(root string, entry branchState) error {
	_, err := git.Run(root, "--git-dir="+repository.Directory(root, entry.Name),
		"update-ref", "refs/heads/"+entry.Branch, entry.Target, entry.Head)
	return err
}

func changed(entry branchState, directory string) error {
	return fmt.Errorf("%s repository %q changed outside GitOne since the interrupted pull: resolve it manually\n"+
		"  1. inspect branch %s, which started at %s and was being advanced to %s\n"+
		"  2. set the branch, index and working tree to the state you want to keep\n"+
		"  3. archive %s outside the project afterwards, do not delete it",
		recoveryRequired, entry.Name, entry.Branch, short(entry.Head), short(entry.Target), directory)
}

func restoreIndex(root, directory, name string) error {
	return repository.RestoreIndex(root, name, filepath.Join(directory, name+originalIndexSuffix))
}

func readState(directory string) (*state, error) {
	contents, err := os.ReadFile(filepath.Join(directory, stateFile))
	if err != nil {
		return nil, err
	}
	saved := new(state)
	if err := json.Unmarshal(contents, saved); err != nil {
		return nil, fmt.Errorf("%s the recorded pull state is unreadable: %w", recoveryRequired, err)
	}
	switch saved.Version {
	case legacyStateVersion:
		if len(saved.Configuration) != 0 {
			return nil, fmt.Errorf("%s the recorded pull state version %d cannot contain configuration changes", recoveryRequired, saved.Version)
		}
	case stateVersion:
		for _, file := range saved.Configuration {
			if file.Path == "" || file.OriginalHash == "" || file.TargetHash == "" || file.PatchedHash == "" {
				return nil, fmt.Errorf("%s the recorded pull state is missing configuration identities", recoveryRequired)
			}
		}
	default:
		return nil, fmt.Errorf("%s the recorded pull state has unsupported version %d", recoveryRequired, saved.Version)
	}
	return saved, nil
}
