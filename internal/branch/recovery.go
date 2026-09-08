package branch

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/devidevio/gitone/internal/git"
	"github.com/devidevio/gitone/internal/lock"
	"github.com/devidevio/gitone/internal/repository"
)

const (
	stateFile = lock.StateFile

	// Version 3 records deletion intent and resumable restoration. Older
	// creation records remain readable; version 2 deletions lack this proof.
	stateVersion       = 3
	legacyStateVersion = 1

	reflogPrefix = "gitone branch: Created from HEAD "
)

// recorded is the progress of a branch creation or deletion. It shares the
// version and command envelope of the other recorded operations, so gitone
// recover and gitone abort can tell an interrupted creation from an
// interrupted commit, push or pull. Exactly one of Repositories and Deleted
// is filled, which is what says which of the two was interrupted.
type recorded struct {
	Version      int            `json:"version"`
	Command      string         `json:"command"`
	Branch       string         `json:"branch"`
	Marker       string         `json:"marker"`
	Repositories []refRecord    `json:"repositories"`
	Deleted      []deleteRecord `json:"deleted,omitempty"`
	Finalizing   bool           `json:"finalizing,omitempty"`
}

// deleteRecord is the deleted branch of one repository. Commit is the tip it
// had; Reflog and Settings are the snapshots. Started is persisted before
// deletion, Restoring before recreation with the operation marker.
type deleteRecord struct {
	Name      string    `json:"name"`
	Commit    string    `json:"commit"`
	Reflog    []byte    `json:"reflog,omitempty"`
	Settings  []setting `json:"settings,omitempty"`
	Started   bool      `json:"started,omitempty"`
	Restoring bool      `json:"restoring,omitempty"`
}

// refRecord is the new branch of one repository. Commit is the starting ref
// the branch is created at, which is the commit that repository has checked
// out, and Created records that Git reported the ref as written.
type refRecord struct {
	Name    string `json:"name"`
	Commit  string `json:"commit"`
	Created bool   `json:"created,omitempty"`
}

// apply writes the new ref of every repository in configuration order. The
// expected commits are recorded before the first ref exists and the progress
// after every repository, so an interrupted creation can be finished or
// undone afterwards.
func apply(root string, inspected []State, name string, output io.Writer) error {
	directory := lock.RecoveryPath(root)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	saved := &recorded{Version: stateVersion, Command: Command, Branch: name, Marker: rand.Text()}
	for _, current := range inspected {
		saved.Repositories = append(saved.Repositories, refRecord{Name: current.Name, Commit: current.Head})
	}
	if err := lock.WriteState(directory, saved); err != nil {
		_ = os.RemoveAll(directory)
		return err
	}

	for index, current := range inspected {
		if err := CreateRef(root, current.Name, name, current.Head, reflogPrefix+saved.Marker); err != nil {
			return interrupted(root, directory, saved, current.Name, err)
		}
		saved.Repositories[index].Created = true
		if err := lock.WriteState(directory, saved); err != nil {
			return interrupted(root, directory, saved, current.Name,
				fmt.Errorf("could not record branch creation progress: %w", err))
		}
	}
	if err := os.RemoveAll(directory); err != nil {
		return err
	}
	report(output, inspected, name)
	return nil
}

// interrupted closes a creation that failed after preflight. The refs it
// already wrote are deleted when they still point at the recorded commit,
// which returns the whole project to its starting state. Otherwise the
// recorded state stays for gitone recover and gitone abort.
func interrupted(root, directory string, saved *recorded, name string, failure error) error {
	reason := fmt.Errorf("%s repository %q could not create branch %s: %s",
		branchFailed, name, saved.Branch, detail(failure))
	if err := rollback(root, directory, saved); err != nil {
		return errors.Join(reason, err, fmt.Errorf(
			"%s the branches created before it were not removed: run gitone abort to undo the creation or gitone recover to finish it",
			recoveryRequired))
	}
	if err := os.RemoveAll(directory); err != nil {
		return err
	}
	return fmt.Errorf("%w\n%s", reason, unmodified)
}

// rollback deletes every ref this creation already wrote, newest first. A ref
// without this operation's marker at the recorded commit is never removed:
// its recovery state must be resolved instead.
func rollback(root, directory string, saved *recorded) error {
	entries := slices.Clone(saved.Repositories)
	slices.Reverse(entries)
	for _, entry := range entries {
		if !entry.Created {
			continue
		}
		existing, err := repository.Reference(root, entry.Name, "refs/heads/"+saved.Branch)
		if err != nil {
			return err
		}
		if existing != entry.Commit {
			return changed(entry, saved.Branch, directory)
		}
		owned, err := OwnsRef(root, entry.Name, saved.Branch, reflogPrefix+saved.Marker)
		if err != nil {
			return err
		}
		if !owned {
			return changed(entry, saved.Branch, directory)
		}
		if err := DeleteRef(root, entry.Name, saved.Branch, entry.Commit); err != nil {
			return err
		}
	}
	return nil
}

// Resume finishes or undoes an interrupted branch creation or deletion.
// recovering creates the missing refs or removes the remaining ones;
// otherwise every ref the operation wrote is removed again, or every ref it
// deleted is put back. A branch that changed outside GitOne since the
// interruption is never overwritten or deleted.
func Resume(root string, recovering bool, output io.Writer) error {
	directory := lock.RecoveryPath(root)
	saved, err := readState(directory)
	if err != nil {
		return err
	}
	if len(saved.Deleted) != 0 {
		return resumeDeletion(root, saved, recovering, output)
	}
	entries := slices.Clone(saved.Repositories)
	if !recovering {
		slices.Reverse(entries)
	}
	results := make([]string, len(entries))
	for index, entry := range entries {
		if results[index], err = resolve(root, saved.Branch, saved.Marker, entry, directory, recovering); err != nil {
			return err
		}
	}

	names := make([]string, len(entries))
	for index, entry := range entries {
		names[index] = entry.Name
	}
	reportResume(output, recovering, names, results)
	return nil
}

// reportResume states what the resumed creation or deletion did in every
// repository, so both report their result the same way.
func reportResume(output io.Writer, recovering bool, names, results []string) {
	verb := "aborted"
	if recovering {
		verb = "recovered"
	}
	fmt.Fprintf(output, "%s %s.\n\n", Command, verb)
	for index, name := range names {
		fmt.Fprintf(output, "%-10s %s\n", name, results[index])
	}
}

// resolve brings the recorded branch of one repository to the state the
// resumed command wants and reports what it did. Preflight guaranteed the
// branch did not exist. Abort additionally checks the operation marker in its
// reflog, so an external ref at the same commit is not mistaken for ours.
func resolve(root, branch, marker string, entry refRecord, directory string, recovering bool) (string, error) {
	existing, err := repository.Reference(root, entry.Name, "refs/heads/"+branch)
	if err != nil {
		return "", err
	}
	switch {
	case existing == "":
		// The ref was never written.
		if !recovering {
			return "unchanged, " + branch + " was not created", nil
		}
		if err := CreateRef(root, entry.Name, branch, entry.Commit, reflogPrefix+marker); err != nil {
			return "", err
		}
		return "created " + branch + " at " + short(entry.Commit), nil
	case existing == entry.Commit:
		// The ref was written, whether or not the progress reached the state.
		if recovering {
			return "already at " + short(entry.Commit), nil
		}
		owned, err := OwnsRef(root, entry.Name, branch, reflogPrefix+marker)
		if err != nil {
			return "", err
		}
		if !owned {
			return "", changed(entry, branch, directory)
		}
		if err := DeleteRef(root, entry.Name, branch, entry.Commit); err != nil {
			return "", err
		}
		return "removed " + branch, nil
	}
	return "", changed(entry, branch, directory)
}

// CreateRef writes one new branch of a repository at commit. The empty
// expected old value makes Git refuse a ref that already exists, and message
// is the reflog entry that lets an undo prove afterwards which operation
// created the ref. gitone switch creates the branches it needs with exactly
// these three helpers.
func CreateRef(root, name, branch, commit, message string) error {
	_, err := git.Run(root, "--git-dir="+repository.Directory(root, name),
		"update-ref", "--create-reflog", "-m", message, "refs/heads/"+branch, commit, "")
	return err
}

// OwnsRef reports whether the newest reflog entry of a branch is message,
// which proves the operation that wrote message also created the ref, even
// when the process stopped before recording its progress.
func OwnsRef(root, name, branch, message string) (bool, error) {
	logged, err := git.Run(root, "--git-dir="+repository.Directory(root, name),
		"reflog", "show", "--format=%gs", "--max-count=1", "refs/heads/"+branch)
	return strings.TrimSpace(logged) == message, err
}

// DeleteRef removes a branch. The expected old value makes Git refuse a
// branch that moved meanwhile.
func DeleteRef(root, name, branch, commit string) error {
	_, err := git.Run(root, "--git-dir="+repository.Directory(root, name),
		"update-ref", "-d", "refs/heads/"+branch, commit)
	return err
}

func changed(entry refRecord, branch, directory string) error {
	return fmt.Errorf("%s repository %q changed outside GitOne since the interrupted branch creation: resolve it manually\n"+
		"  1. inspect branch %s, which was being created at %s\n"+
		"  2. set the branch to the state you want to keep, or delete it\n"+
		"  3. archive %s outside the project afterwards, do not delete it",
		recoveryRequired, entry.Name, branch, short(entry.Commit), directory)
}

func readState(directory string) (*recorded, error) {
	contents, err := os.ReadFile(filepath.Join(directory, stateFile))
	if err != nil {
		return nil, err
	}
	saved := new(recorded)
	if err := json.Unmarshal(contents, saved); err != nil {
		return nil, fmt.Errorf("%s the recorded branch state is unreadable: %w", recoveryRequired, err)
	}
	if saved.Version != stateVersion && saved.Version != 2 && saved.Version != legacyStateVersion {
		return nil, fmt.Errorf("%s the recorded branch state has unsupported version %d", recoveryRequired, saved.Version)
	}
	if len(saved.Deleted) != 0 && saved.Version != stateVersion {
		return nil, fmt.Errorf("%s the recorded deletion lacks safe recovery progress; resolve it manually", recoveryRequired)
	}
	if len(saved.Repositories) != 0 && len(saved.Deleted) != 0 {
		return nil, fmt.Errorf("%s the recorded branch state mixes a creation and a deletion; resolve it manually", recoveryRequired)
	}
	return saved, nil
}
