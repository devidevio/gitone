package switching

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

	"github.com/devidevio/gitone/internal/assign"
	"github.com/devidevio/gitone/internal/branch"
	"github.com/devidevio/gitone/internal/git"
	"github.com/devidevio/gitone/internal/lock"
	"github.com/devidevio/gitone/internal/repository"
)

const (
	stateFile = lock.StateFile
	// Version 1 predates the generated path assignments, version 2 the
	// branches a switch creates itself, and version 3 the upstream snapshots.
	// An older gitone must refuse a state
	// it would silently leave half undone, so every addition raises this.
	legacyStateVersion     = 1
	assignmentStateVersion = 2
	creationStateVersion   = 3
	stateVersion           = 4
	originalIndexSuffix    = ".original"

	// reflogPrefix marks every ref this switch creates, so an undo can prove
	// a branch is its own work before deleting it.
	reflogPrefix = "gitone switch: Created "
)

// state is the recorded progress of a switch. It shares the version and
// command envelope of the other recorded operations, so gitone recover and
// gitone abort can tell an interrupted switch from an interrupted commit,
// branch creation, push or pull.
type state struct {
	Version       int              `json:"version"`
	Command       string           `json:"command"`
	Branch        string           `json:"branch"`
	Marker        string           `json:"marker,omitempty"`
	Repositories  []switchRecord   `json:"repositories"`
	Configuration []assign.Patched `json:"configuration,omitempty"`
}

// switchRecord is the checkout of one repository. From is the branch it
// started on and Head the commit that branch holds, Target the commit of the
// requested branch, and Switched records that Git reported the checkout as
// done. Create records that this switch writes the branch here first, at
// Target, and Track that it then configures origin/<branch> as its upstream.
// Whether either already happened is read back from the repository instead of
// recorded: the reflog marker and the configured upstream say it exactly, also
// when the process stopped before it could record anything. The exact starting
// index is saved beside this state before the first repository moves.
type switchRecord struct {
	Name     string          `json:"name"`
	From     string          `json:"from"`
	Head     string          `json:"head"`
	Target   string          `json:"target"`
	Create   bool            `json:"create,omitempty"`
	Track    bool            `json:"track,omitempty"`
	Upstream *upstreamConfig `json:"upstream,omitempty"`
	Switched bool            `json:"switched,omitempty"`
}

// apply creates the branch where a repository lacks it, checks it out in
// every moving repository, in configuration order, and then writes the
// configuration patch the switch generated. The starting branches, commits,
// indexes and the configuration files as they are now are recorded before the
// first ref is written and the progress after every repository, so an
// interrupted switch can be finished or undone afterwards.
func apply(root string, participants []*participant, name string, added *assign.Assignment, output io.Writer) error {
	var moving []*participant
	for _, current := range participants {
		if current.state == "" {
			moving = append(moving, current)
		}
	}
	if len(moving) == 0 {
		report(output, participants, name)
		return nil
	}

	directory := lock.RecoveryPath(root)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	saved := &state{Version: stateVersion, Command: Command, Branch: name, Marker: rand.Text()}
	for _, current := range moving {
		if err := repository.SaveIndex(root, current.name, indexCopy(directory, current.name)); err != nil {
			_ = os.RemoveAll(directory)
			return err
		}
		var err error
		entry := switchRecord{Name: current.name, From: current.from,
			Head: current.head, Target: current.target, Create: current.create, Track: current.track}
		if current.track {
			entry.Upstream, err = prepareUpstream(root, directory, current.name, name)
			if err != nil {
				_ = os.RemoveAll(directory)
				return err
			}
		}
		saved.Repositories = append(saved.Repositories, entry)
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
		if current.create {
			// Nothing is checked out yet when this fails, so only the refs
			// and the configuration written before it have to go.
			if err := createBranch(root, saved.Repositories[index], name, saved.Marker); err != nil {
				current.state, current.detail = failed, detail(err)
				return restored(root, directory, saved, participants, name, fmt.Errorf(
					"%s repository %q could not create branch %s: %s", switchFailed, current.name, name, current.detail))
			}
		}
		if err := Checkout(root, current.name, name, current.target); err != nil {
			return interrupted(root, directory, saved, participants, current, saved.Repositories[index], name, err)
		}
		current.state = switched
		saved.Repositories[index].Switched = true
		if err := lock.WriteState(directory, saved); err != nil {
			return err
		}
	}
	// The patch extends the configuration the checkouts just materialized, so
	// it is written last and is part of the same recoverable transaction.
	if err := assign.Apply(root, saved.Configuration); err != nil {
		return restored(root, directory, saved, participants, name, fmt.Errorf(
			"%s the generated path assignments could not be written: %s", switchFailed, detail(err)))
	}
	if err := os.RemoveAll(directory); err != nil {
		return err
	}
	report(output, participants, name)
	added.Report(output)
	return nil
}

// interrupted closes a switch that failed after preflight. The repositories it
// already moved are returned to their starting branch when they still hold
// exactly what this switch left behind, which returns the whole project to its
// starting state. Otherwise the recorded state stays for gitone recover and
// gitone abort.
func interrupted(root, directory string, saved *state, participants []*participant, current *participant, entry switchRecord, name string, failure error) error {
	current.state, current.detail = failed, detail(failure)
	reason := fmt.Errorf("%s repository %q could not be switched to %s: %s",
		switchFailed, current.name, name, current.detail)
	// The repository this switch was working on is undone first and without
	// further proof: the project lock was held throughout and preflight
	// proved it clean on its starting branch, so whatever an incomplete
	// checkout left behind is this switch's own work.
	if err := back(root, directory, entry); err != nil {
		return errors.Join(reason, err, fmt.Errorf(
			"%s repository %q was left mid-checkout: run gitone abort to undo the switch or gitone recover to finish it",
			recoveryRequired, current.name))
	}
	return restored(root, directory, saved, participants, name, reason)
}

// restored returns every repository this switch already moved to its starting
// branch and undoes the configuration patch, which returns the whole project
// to its starting state. Otherwise the recorded state stays for gitone
// recover and gitone abort.
func restored(root, directory string, saved *state, participants []*participant, name string, reason error) error {
	if err := rollback(root, directory, saved, name); err != nil {
		for _, participant := range participants {
			if participant.state == switched {
				participant.state, participant.detail = failed, "recovery required; inspect the recorded state"
			}
		}
		return errors.Join(reason, err, fmt.Errorf(
			"%s the repositories switched before it were not restored: run gitone abort to undo the switch or gitone recover to finish it",
			recoveryRequired))
	}
	for _, participant := range participants {
		if participant.state == switched {
			participant.state, participant.detail = unchanged, "restored to "+participant.from
		}
	}
	if err := os.RemoveAll(directory); err != nil {
		return err
	}
	return fmt.Errorf("%w\n%s", reason, unmodified)
}

// rollback returns every repository this switch already moved to its starting
// branch, newest first, removes every branch and upstream it created, and
// then restores every configuration file it patched. A repository that no
// longer holds what the switch left behind is not touched: its recovery state
// must be resolved instead. A repository the switch never reached still has
// its ref checked, because a ref can exist while the checkout after it did
// not run.
func rollback(root, directory string, saved *state, name string) error {
	if err := assign.Verify(root, directory, "switch", saved.Configuration); err != nil {
		return err
	}
	entries := slices.Clone(saved.Repositories)
	slices.Reverse(entries)
	for _, entry := range entries {
		if _, err := verifyCreated(root, entry, name, saved.Marker, directory); err != nil {
			return err
		}
		if entry.Switched {
			current, files, err := position(root, entry, name, assign.Names(saved.Configuration)...)
			if err != nil {
				return err
			}
			if current != name || files != entry.Target {
				return changed(entry, name, directory)
			}
			if err := back(root, directory, entry); err != nil {
				return err
			}
		}
		if err := removeBranch(root, directory, entry, name, saved.Marker); err != nil {
			return err
		}
	}
	return assign.Restore(root, directory, saved.Configuration)
}

// Resume finishes or undoes an interrupted switch. recovering checks out the
// requested branch in the remaining repositories; otherwise every repository
// the switch moved is returned to the branch, commit and index it started on.
// A repository that changed outside GitOne since the interruption is never
// overwritten.
func Resume(root string, recovering bool, output io.Writer) error {
	directory := lock.RecoveryPath(root)
	saved, err := readState(directory)
	if err != nil {
		return err
	}
	if err := assign.Verify(root, directory, "switch", saved.Configuration); err != nil {
		return err
	}
	entries := slices.Clone(saved.Repositories)
	if !recovering {
		slices.Reverse(entries)
	}
	results := make([]string, len(entries))
	for index, entry := range entries {
		if results[index], err = resolve(root, saved, entry, directory, recovering); err != nil {
			return err
		}
	}

	// The configuration patch belongs to the same transaction: recovering
	// finishes it on the branches it now extends, aborting undoes it.
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
	return "restored to the version before the switch"
}

// resolve brings one recorded repository to the state the resumed command
// wants and reports what it did. A branch this switch creates is written
// before its checkout can be finished and removed after that checkout was
// undone, so the ref, its upstream and the checkout resume as one step.
func resolve(root string, saved *state, entry switchRecord, directory string, recovering bool) (string, error) {
	name := saved.Branch
	existing, err := verifyCreated(root, entry, name, saved.Marker, directory)
	if err != nil {
		return "", err
	}
	current, files, err := position(root, entry, name, assign.Names(saved.Configuration)...)
	if err != nil {
		return "", err
	}
	if current == "" || files == "" {
		return "", changed(entry, name, directory)
	}
	if entry.Create {
		if existing == "" {
			if current != entry.From || files != entry.Head {
				return "", changed(entry, name, directory)
			}
			if !recovering {
				if err := removeBranch(root, directory, entry, name, saved.Marker); err != nil {
					return "", err
				}
				if err := restoreIndex(root, directory, entry.Name); err != nil {
					return "", err
				}
				return "unchanged on " + entry.From + ", " + name + " was not created", nil
			}
			if err := createBranch(root, entry, name, saved.Marker); err != nil {
				return "", err
			}
		} else if recovering && entry.Track {
			if err := entry.Upstream.write(root, entry.Name, true); err != nil {
				return "", err
			}
		}
	}
	result, err := move(root, entry, directory, name, current, files, recovering)
	if err != nil {
		return "", err
	}
	if recovering || !entry.Create {
		return result, nil
	}
	if err := removeBranch(root, directory, entry, name, saved.Marker); err != nil {
		return "", err
	}
	return result + ", removed " + name, nil
}

// move brings the checkout of one recorded repository to the state the
// resumed command wants and reports what it did. current is the recorded
// branch HEAD points at and files the recorded commit the index and working
// tree hold, each empty when it is neither: that repository changed outside
// GitOne and is refused.
func move(root string, entry switchRecord, directory, name, current, files string, recovering bool) (string, error) {
	switch {
	case current == entry.From && files == entry.Head:
		// The checkout never started.
		if !recovering {
			if err := restoreIndex(root, directory, entry.Name); err != nil {
				return "", err
			}
			return "unchanged on " + entry.From, nil
		}
		if err := Checkout(root, entry.Name, name, entry.Target); err != nil {
			return "", err
		}
		return "switched " + entry.From + " → " + name, nil
	case current == name && files == entry.Target:
		// The checkout finished.
		if recovering {
			return "already on " + name, nil
		}
		if err := back(root, directory, entry); err != nil {
			return "", err
		}
		return "restored " + entry.From + " at " + short(entry.Head), nil
	case current == entry.From && files == entry.Target:
		// The files were replaced but HEAD did not move yet.
		if recovering {
			if err := moveHead(root, entry.Name, name); err != nil {
				return "", err
			}
			return "switched " + entry.From + " → " + name, nil
		}
		if err := back(root, directory, entry); err != nil {
			return "", err
		}
		return "restored " + entry.From + " at " + short(entry.Head), nil
	}
	return "", changed(entry, name, directory)
}

// position reports which recorded branch HEAD points at and which recorded
// commit the index and working tree hold. Both results are empty once either
// branch no longer holds the commit the switch recorded, because then the
// repository changed outside GitOne and nothing may be restored over it.
func position(root string, entry switchRecord, name string, patched ...string) (current, files string, err error) {
	from, err := repository.Reference(root, entry.Name, "refs/heads/"+entry.From)
	if err != nil {
		return "", "", err
	}
	target, err := repository.Reference(root, entry.Name, "refs/heads/"+name)
	if err != nil {
		return "", "", err
	}
	if from != entry.Head || (target != entry.Target && !(entry.Create && target == "")) {
		return "", "", nil
	}
	// A detached HEAD leaves current empty, which is refused like every other
	// external change.
	reference, _ := git.Run(root, "--git-dir="+repository.Directory(root, entry.Name),
		"symbolic-ref", "--quiet", "--short", "HEAD")
	switch strings.TrimSpace(reference) {
	case entry.From:
		current = entry.From
	case name:
		if target != "" {
			current = name
		}
	}
	headMatches := repository.Matches(root, entry.Name, entry.Head, patched...)
	targetMatches := target != "" && repository.Matches(root, entry.Name, entry.Target, patched...)
	switch {
	case current == entry.From && headMatches:
		files = entry.Head
	case current == name && targetMatches:
		files = entry.Target
	case targetMatches:
		files = entry.Target
	case headMatches:
		files = entry.Head
	}
	return current, files, nil
}

// createBranch writes one new branch at the start point this switch planned
// for it and, for a branch taken from origin, configures that origin branch
// as its upstream. The reflog marker proves afterwards that this switch, and
// not something else, created the ref.
func createBranch(root string, entry switchRecord, name, marker string) error {
	if err := branch.CreateRef(root, entry.Name, name, entry.Target, reflogPrefix+marker); err != nil {
		return err
	}
	if entry.Track {
		return entry.Upstream.write(root, entry.Name, true)
	}
	return nil
}

// verifyCreated refuses externally changed refs and configuration before any
// checkout, index restoration or upstream write can take place.
func verifyCreated(root string, entry switchRecord, name, marker, directory string) (string, error) {
	if !entry.Create {
		return "", nil
	}
	existing, err := repository.Reference(root, entry.Name, "refs/heads/"+name)
	if err != nil {
		return "", err
	}
	if existing != "" {
		owned, err := branch.OwnsRef(root, entry.Name, name, reflogPrefix+marker)
		if err != nil {
			return "", err
		}
		if existing != entry.Target || !owned {
			return "", keptBranch(entry, name, directory)
		}
	}
	if entry.Track {
		if err := entry.Upstream.verify(root, entry.Name); err != nil {
			return "", err
		}
	}
	return existing, nil
}

func removeBranch(root, directory string, entry switchRecord, name, marker string) error {
	if !entry.Create {
		return nil
	}
	existing, err := verifyCreated(root, entry, name, marker, directory)
	if err != nil {
		return err
	}
	if entry.Track {
		if err := entry.Upstream.write(root, entry.Name, false); err != nil {
			return err
		}
	}
	if existing == "" {
		return nil
	}
	return branch.DeleteRef(root, entry.Name, name, entry.Target)
}

// Checkout moves one repository to a branch. Native Git updates the index,
// the working-tree files and HEAD in one step, and refuses to overwrite local
// work or an untracked file. It also reports a checkout as done when it could
// not write every file, so the result is verified instead of trusted.
// gitone reconfigure --switch moves its repositories with exactly this
// checkout.
func Checkout(root, name, branch, target string) error {
	if _, err := git.Run(root, "--git-dir="+repository.Directory(root, name), "--work-tree="+root,
		"checkout", "--quiet", branch); err != nil {
		return err
	}
	if !repository.Matches(root, name, target) {
		return fmt.Errorf("the index and working tree do not hold %s afterwards", short(target))
	}
	return nil
}

// back returns one repository to the branch, files and index it started on.
// HEAD is pointed at the starting branch first, so the reset restores that
// branch's files instead of moving another branch. Only a repository that
// still holds what the switch left behind reaches this point, so this
// discards nothing but the switch's own work.
func back(root, directory string, entry switchRecord) error {
	if _, err := git.Run(root, "--git-dir="+repository.Directory(root, entry.Name), "--work-tree="+root,
		"read-tree", "--dry-run", "-u", "-m", entry.Target, entry.Head); err != nil {
		return fmt.Errorf("%s repository %q cannot be restored without overwriting external changes: %s",
			recoveryRequired, entry.Name, detail(err))
	}
	if err := moveHead(root, entry.Name, entry.From); err != nil {
		return err
	}
	if _, err := git.Run(root, "--git-dir="+repository.Directory(root, entry.Name), "--work-tree="+root,
		"reset", "--hard", "--quiet", entry.Head); err != nil {
		return err
	}
	return restoreIndex(root, directory, entry.Name)
}

// moveHead points HEAD at a branch without touching the index or the working
// tree, which completes a checkout whose files are already in place.
func moveHead(root, name, branch string) error {
	_, err := git.Run(root, "--git-dir="+repository.Directory(root, name),
		"symbolic-ref", "HEAD", "refs/heads/"+branch)
	return err
}

func changed(entry switchRecord, name, directory string) error {
	return fmt.Errorf("%s repository %q changed outside GitOne since the interrupted switch: resolve it manually\n"+
		"  1. inspect branch %s at %s, which was being left for %s at %s\n"+
		"  2. set HEAD, the index and the working tree to the state you want to keep\n"+
		"  3. archive %s outside the project afterwards, do not delete it",
		recoveryRequired, entry.Name, entry.From, short(entry.Head), name, short(entry.Target), directory)
}

func keptBranch(entry switchRecord, name, directory string) error {
	return fmt.Errorf("%s repository %q changed branch %s outside GitOne since the interrupted switch: resolve it manually\n"+
		"  1. inspect branch %s, which the switch created at %s\n"+
		"  2. keep the branch or delete it yourself\n"+
		"  3. archive %s outside the project afterwards, do not delete it",
		recoveryRequired, entry.Name, name, name, short(entry.Target), directory)
}

func restoreIndex(root, directory, name string) error {
	return repository.RestoreIndex(root, name, indexCopy(directory, name))
}

func indexCopy(directory, name string) string {
	return filepath.Join(directory, name+originalIndexSuffix)
}

func readState(directory string) (*state, error) {
	contents, err := os.ReadFile(filepath.Join(directory, stateFile))
	if err != nil {
		return nil, err
	}
	saved := new(state)
	if err := json.Unmarshal(contents, saved); err != nil {
		return nil, fmt.Errorf("%s the recorded switch state is unreadable: %w", recoveryRequired, err)
	}
	switch saved.Version {
	case legacyStateVersion:
		if len(saved.Configuration) != 0 {
			return nil, fmt.Errorf("%s the recorded switch state version %d cannot contain configuration changes", recoveryRequired, saved.Version)
		}
	case assignmentStateVersion, creationStateVersion, stateVersion:
		for _, file := range saved.Configuration {
			if file.Path == "" || file.OriginalHash == "" || file.TargetHash == "" || file.PatchedHash == "" {
				return nil, fmt.Errorf("%s the recorded switch state is missing configuration identities", recoveryRequired)
			}
		}
	default:
		return nil, fmt.Errorf("%s the recorded switch state has unsupported version %d", recoveryRequired, saved.Version)
	}
	if saved.Marker == "" && slices.ContainsFunc(saved.Repositories, func(entry switchRecord) bool { return entry.Create }) {
		return nil, fmt.Errorf("%s the recorded switch state creates branches without the marker identifying them", recoveryRequired)
	}
	for _, entry := range saved.Repositories {
		if entry.Track && (!entry.Create || entry.Upstream == nil || len(entry.Upstream.Original) == 0 || len(entry.Upstream.Target) == 0) {
			return nil, fmt.Errorf("%s the recorded switch state lacks upstream configuration snapshots; resolve it manually", recoveryRequired)
		}
	}
	return saved, nil
}
