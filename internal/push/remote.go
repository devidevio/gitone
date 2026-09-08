package push

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/devidevio/gitone/internal/git"
	"github.com/devidevio/gitone/internal/lock"
	"github.com/devidevio/gitone/internal/repository"
)

const (
	stateFile    = lock.StateFile
	stateVersion = 1
)

// state is the recorded progress of a push. It shares the version and command
// envelope of the other recorded operations, so gitone recover and gitone
// abort can tell an interrupted push from an interrupted commit.
type state struct {
	Version      int        `json:"version"`
	Command      string     `json:"command"`
	Repositories []refState `json:"repositories"`
}

// refState is the ref one repository publishes. Old is the value the remote
// branch had during preflight, empty while it did not exist, New is the local
// commit the push sends, and Pushed records that Git reported success.
type refState struct {
	Name   string `json:"name"`
	Branch string `json:"branch"`
	Old    string `json:"old,omitempty"`
	New    string `json:"new"`
	Pushed bool   `json:"pushed,omitempty"`
}

// publish sends every participant in name order. The expected refs are
// recorded before the first remote changes and the progress after every
// repository, so an interrupted push can be reconciled afterwards.
func publish(root string, participants []*participant, output io.Writer) error {
	directory := lock.RecoveryPath(root)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	saved := &state{Version: stateVersion, Command: Command}
	for _, current := range participants {
		saved.Repositories = append(saved.Repositories,
			refState{Name: current.name, Branch: current.branch, Old: current.remote, New: current.head})
	}
	if err := lock.WriteState(directory, saved); err != nil {
		_ = os.RemoveAll(directory)
		return err
	}

	for index, current := range participants {
		if err := send(root, current); err != nil {
			// A push is not atomic, so what really happened is read back from
			// the remotes instead of being derived from the failure.
			if reportErr := report(root, saved, output); reportErr != nil {
				return errors.Join(err, fmt.Errorf("%s the remotes could not be inspected: run gitone recover: %w", recoveryRequired, reportErr))
			}
			if err := os.RemoveAll(directory); err != nil {
				return err
			}
			return fmt.Errorf("%s repository %q was not pushed: %s", preflightFailed, current.name, detail(err))
		}
		saved.Repositories[index].Pushed = true
		if err := lock.WriteState(directory, saved); err != nil {
			return err
		}
	}
	if err := os.RemoveAll(directory); err != nil {
		return err
	}
	printPushed(output, saved)
	return nil
}

// send runs the real native push, which is the only step that invokes pre-push
// hooks and authentication. The branch is published under its own name on
// origin, without force, without tags and without any other ref.
func send(root string, current *participant) error {
	arguments := []string{"--git-dir=" + repository.Directory(root, current.name), "--work-tree=" + root, "push", "--quiet", "--no-follow-tags"}
	if !current.upstream {
		arguments = append(arguments, "--set-upstream")
	}
	reference := "refs/heads/" + current.branch
	_, err := git.Run(root, append(arguments, remoteName, reference+":"+reference)...)
	return err
}

// Reconcile reports what an interrupted push really published. It reads the
// remote refs and never modifies a remote, repeats a push or rolls one back.
func Reconcile(root string, output io.Writer) error {
	saved, err := readState(lock.RecoveryPath(root))
	if err != nil {
		return err
	}
	if err := report(root, saved, output); err != nil {
		return err
	}
	fmt.Fprintf(output, "\nNo remote was modified. Rerun gitone push to publish the remaining commits.\n")
	return nil
}

// report reads every recorded ref back from its remote and prints what is
// confirmed there. It states results only, never intentions.
func report(root string, saved *state, output io.Writer) error {
	results := make([]string, len(saved.Repositories))
	for index, entry := range saved.Repositories {
		actual, err := remoteReference(root, entry.Name, entry.Branch)
		if err != nil {
			return err
		}
		switch {
		case actual == entry.New:
			results[index] = "published " + short(entry.New)
		case actual == entry.Old && entry.Pushed:
			results[index] = "unchanged although the push reported success"
		case actual == entry.Old && entry.Old == "":
			results[index] = "unchanged, " + remoteName + " has no branch " + entry.Branch
		case actual == entry.Old:
			results[index] = "unchanged at " + short(entry.Old)
		default:
			results[index] = "at " + short(actual) + ", which this push did not send"
		}
	}
	fmt.Fprintf(output, "\nPush results:\n\n")
	for index, entry := range saved.Repositories {
		fmt.Fprintf(output, "%-10s %s/%s %s\n", entry.Name, remoteName, entry.Branch, results[index])
	}
	return nil
}

func printPushed(output io.Writer, saved *state) {
	for _, entry := range saved.Repositories {
		fmt.Fprintf(output, "%s:\n    pushed %s to %s/%s\n\n", entry.Name, short(entry.New), remoteName, entry.Branch)
	}
}

func readState(directory string) (*state, error) {
	contents, err := os.ReadFile(filepath.Join(directory, stateFile))
	if err != nil {
		return nil, err
	}
	saved := new(state)
	if err := json.Unmarshal(contents, saved); err != nil {
		return nil, fmt.Errorf("%s the recorded push state is unreadable: %w", recoveryRequired, err)
	}
	if saved.Version != stateVersion {
		return nil, fmt.Errorf("%s the recorded push state has unsupported version %d", recoveryRequired, saved.Version)
	}
	return saved, nil
}
