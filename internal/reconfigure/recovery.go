package reconfigure

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/devidevio/gitone/internal/assign"
	"github.com/devidevio/gitone/internal/config"
	"github.com/devidevio/gitone/internal/git"
	"github.com/devidevio/gitone/internal/lock"
	"github.com/devidevio/gitone/internal/repository"
	"github.com/devidevio/gitone/internal/worktree"
)

const stateVersion = 4

// state is the recorded plan of a reconfiguration, including the exact public
// and local configuration sources it was accepted against. It shares the
// version and command envelope of the other recoverable operations, so gitone
// recover and gitone abort can identify it.
type state struct {
	Version int    `json:"version"`
	Command string `json:"command"`

	// PublicConfiguration is the .gitone.yml the reconfiguration leaves
	// behind, which for an incoming source is the accepted incoming version
	// rather than the one on disk. StartConfiguration is what was on disk
	// when the plan was recorded, and is empty while the two are the same.
	PublicConfiguration []byte            `json:"public_configuration"`
	StartConfiguration  []byte            `json:"start_configuration,omitempty"`
	LocalConfiguration  []byte            `json:"local_configuration"`
	LocalExists         bool              `json:"local_exists,omitempty"`
	Repositories        []repositoryState `json:"repositories"`
	Renames             []renameState     `json:"renames,omitempty"`
	Retirements         []retirementState `json:"retirements,omitempty"`
	Moves               []moveState       `json:"moves,omitempty"`
	Configuration       []assign.Patched  `json:"configuration,omitempty"`
}

// moveState is one repository the accepted source moves. From is the branch it
// starts on and Head the commit that branch holds; Branch is the branch it
// ends on and Target the commit that branch holds there. Started is recorded
// before the native move and Moved after it, so recovery can resolve an
// interruption between the move and its completion marker. The exact starting
// index is saved beside this state before the first repository moves.
type moveState struct {
	Name       string `json:"name"`
	SourceName string `json:"source_name,omitempty"`
	From       string `json:"from"`
	Head       string `json:"head"`
	Branch     string `json:"branch"`
	Target     string `json:"target"`
	Started    bool   `json:"started,omitempty"`
	Moved      bool   `json:"moved,omitempty"`
}

func (m moveState) sourceName() string {
	if m.SourceName != "" {
		return m.SourceName
	}
	return m.Name
}

// renameState is one declared rename: the name the metadata directory has
// today, the name it gets, and what that directory held when the plan was
// recorded, so neither gitone recover nor gitone abort moves later work.
type renameState struct {
	From   string   `json:"from"`
	To     string   `json:"to"`
	Branch string   `json:"branch"`
	Head   string   `json:"head,omitempty"`
	Index  string   `json:"index,omitempty"`
	Refs   []string `json:"refs,omitempty"`
}

func (r renameState) recorded() recorded {
	return recorded{name: r.From, branch: r.Branch, head: r.Head, index: r.Index, refs: r.Refs}
}

// retirementState is one repository the reconfiguration moves out of
// .gitone/repositories/. Destination is the planned directory, recorded
// before the first write so the preview, the move, gitone recover and gitone
// abort all name the same one. Branch, Head, Index and Refs are what the
// directory held then, so neither recovery moves later work.
type retirementState struct {
	Name        string   `json:"name"`
	Branch      string   `json:"branch"`
	Head        string   `json:"head,omitempty"`
	Index       string   `json:"index,omitempty"`
	Destination string   `json:"destination"`
	Refs        []string `json:"refs,omitempty"`
}

func (r retirementState) recorded() recorded {
	return recorded{name: r.Name, branch: r.Branch, head: r.Head, index: r.Index, refs: r.Refs}
}

// repositoryState is what one repository held before the first remote
// changed: the branch it is on, the commit that branch points at, and every
// configured remote name with the URL native Git had.
//
// Created marks a repository this reconfiguration creates, which has no such
// starting state. Target is the fetched commit it will check out. Refs is what
// the command last left in that directory, rewritten after every step, so
// recovery can distinguish its own work from later changes.
type repositoryState struct {
	Name    string        `json:"name"`
	Branch  string        `json:"branch"`
	Head    string        `json:"head"`
	Target  string        `json:"target,omitempty"`
	Remotes []remoteState `json:"remotes"`
	Created bool          `json:"created,omitempty"`
	Refs    []string      `json:"refs,omitempty"`
}

// remoteState is one configured remote. Before is the URL native Git had,
// empty while it had no such remote, and After the URL the reconfiguration
// sets it to, empty while the remote does not change.
type remoteState struct {
	Name         string `json:"name"`
	Before       string `json:"before,omitempty"`
	BeforeExists bool   `json:"before_exists,omitempty"`
	After        string `json:"after,omitempty"`
}

func (r remoteState) before() nativeRemote {
	return nativeRemote{exists: r.BeforeExists || r.Before != "", url: r.Before}
}

func (r remoteState) after() nativeRemote {
	return nativeRemote{exists: true, url: r.After}
}

// apply performs the accepted plan in the design's phase order: the journal
// first, then the created repositories and the remote changes, then one fetch
// per repository that has an origin, then every refusal that needs the
// fetched trees, and only then the checkouts.
//
// Progress is recorded for a created repository, whose refs are the only
// thing gitone abort cannot derive from the project itself. A changed remote
// needs no progress: it holds either the URL it started from or the one this
// command set, and anything else was changed outside GitOne.
func apply(configuration *config.Config, root string, current *plan, inventory *worktree.Result, output io.Writer) error {
	directory := lock.RecoveryPath(root)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	public, local, localExists, err := configurationFiles(root)
	if err != nil {
		_ = os.RemoveAll(directory)
		return err
	}
	saved := &state{Version: stateVersion, Command: Command, PublicConfiguration: public,
		LocalConfiguration: local, LocalExists: localExists, Repositories: current.states,
		Renames: renameStates(current.renames), Retirements: retirementStates(current.retirements),
		Moves: moveStates(current.moves)}
	// An incoming source ends with the .gitone.yml its commits carry, so the
	// projected file is what recovery finishes the plan against and the one on
	// disk is what an abort puts back.
	if current.incoming != nil {
		saved.PublicConfiguration, saved.StartConfiguration = current.incoming, public
	}
	for _, entry := range saved.Moves {
		if err := repository.SaveIndex(root, entry.sourceName(), indexCopy(directory, entry.Name)); err != nil {
			_ = os.RemoveAll(directory)
			return err
		}
	}
	if current.added != nil {
		saved.Configuration = current.added.Files
		if err := assign.Save(root, directory, saved.Configuration); err != nil {
			_ = os.RemoveAll(directory)
			return err
		}
	}
	if err := lock.WriteState(directory, saved); err != nil {
		_ = os.RemoveAll(directory)
		return err
	}
	record := func() error { return progress(root, directory, saved) }

	// A rename is the first write of the plan: every later step reads the
	// renamed repository under its new name, and the move itself fetches,
	// clones, checks out and copies nothing.
	if err := moveRenames(root, saved.Renames); err != nil {
		return interrupted(root, directory, saved, err, unmodified)
	}
	if err := build(root, configuration, current.additions, record); err != nil {
		return interrupted(root, directory, saved, err, unmodified)
	}
	if err := repoint(root, saved); err != nil {
		return interrupted(root, directory, saved, err, unmodified)
	}
	if err := refresh(root, current.fetching, record); err != nil {
		return interrupted(root, directory, saved, err, unmodified+fetched)
	}
	if err := born(root, current.additions); err != nil {
		return interrupted(root, directory, saved, err, unmodified+fetched)
	}
	recordTargets(saved, current.additions)
	if err := record(); err != nil {
		return interrupted(root, directory, saved, err, unmodified+fetched)
	}
	if err := reachable(root, current.fetching); err != nil {
		return interrupted(root, directory, saved, err, unmodified+fetched)
	}
	if err := feasible(configuration, root, current.branch, plannedCommits(saved.Moves),
		current.additions, inventory); err != nil {
		return interrupted(root, directory, saved, err, unmodified+fetched)
	}
	if err := materialize(root, current.additions, record); err != nil {
		return interrupted(root, directory, saved, err, unmodified+fetched)
	}
	// The repositories the accepted source moves are advanced or checked out
	// with exactly the fast-forward of gitone pull and the checkout of gitone
	// switch, inside this transaction, so .gitone.yml and the repository set
	// change together or not at all.
	if err := advance(root, saved.Moves, record); err != nil {
		return interrupted(root, directory, saved, err, unmodified+fetched)
	}
	// The ignore entries are written after the checkouts, so a committed
	// .gitignore cannot overwrite them again.
	if err := repository.UpdateIgnore(root); err != nil {
		return err
	}
	// The generated ownership extends the configuration those moves just
	// materialized, so it is written last and is part of the same transaction.
	if err := assign.Apply(root, saved.Configuration); err != nil {
		return interrupted(root, directory, saved, fmt.Errorf(
			"%s the generated path assignments could not be written: %s", reconfigureFailed, detail(err)), unmodified+fetched)
	}
	// Retirement is the last phase, run once the project is already consistent.
	// A preparation failure keeps recovery state. A final rename failure leaves
	// none because the one mv in its message finishes the work.
	if err := retire(root, saved.Retirements); err != nil {
		if errors.Is(err, repository.ErrRetirementMove) {
			return errors.Join(err, os.RemoveAll(directory))
		}
		return err
	}
	if err := os.RemoveAll(directory); err != nil {
		return err
	}
	report(output, current)
	current.added.Report(output)
	return nil
}

// renameStates records every declared rename with the native state gitone
// recover and gitone abort must not overwrite.
func renameStates(renames []*rename) []renameState {
	states := make([]renameState, len(renames))
	for index, current := range renames {
		states[index] = renameState{From: current.from.name, To: current.to, Branch: current.from.branch,
			Head: current.from.head, Index: current.from.index, Refs: current.from.refs}
	}
	return states
}

// retirementStates records every planned retirement with the directory it
// moves to and the native state recovery must not overwrite.
func retirementStates(retirements []*retirement) []retirementState {
	states := make([]retirementState, len(retirements))
	for index, current := range retirements {
		states[index] = retirementState{Name: current.name, Branch: current.branch,
			Head: current.head, Index: current.index, Destination: current.destination, Refs: current.refs}
	}
	return states
}

// progress records the current refs of every created repository, so an abort
// after this point can tell exactly which directories are still this
// command's own work.
func progress(root, directory string, saved *state) error {
	for index, entry := range saved.Repositories {
		if !entry.Created {
			continue
		}
		switch _, err := os.Stat(repository.Directory(root, entry.Name)); {
		case errors.Is(err, os.ErrNotExist):
			continue
		case err != nil:
			return err
		}
		listed, err := refs(root, entry.Name)
		if err != nil {
			return err
		}
		saved.Repositories[index].Refs = listed
	}
	return lock.WriteState(directory, saved)
}

// repoint adds every missing configured remote and re-points every differing
// one, in configuration order.
func repoint(root string, saved *state) error {
	for _, entry := range saved.Repositories {
		for _, remote := range entry.Remotes {
			if remote.After == "" {
				continue
			}
			if err := set(root, entry.Name, remote.Name, remote.after()); err != nil {
				return fmt.Errorf("%s repository %q remote %q could not be set to %s: %s",
					reconfigureFailed, entry.Name, remote.Name, remote.After, detail(err))
			}
		}
	}
	return nil
}

// interrupted closes a reconfiguration that failed after the journal was
// written. Every remote it already changed is set back when the project still
// holds exactly what this command left behind, which returns it to its
// starting state. Otherwise the recorded state stays for gitone recover and
// gitone abort.
func interrupted(root, directory string, saved *state, reason error, closing string) error {
	if err := rollback(root, saved); err != nil {
		return errors.Join(reason, err, fmt.Errorf(
			"%s the remotes changed before it were not restored: run gitone abort to undo the reconfiguration or gitone recover to finish it",
			recoveryRequired))
	}
	if err := os.RemoveAll(directory); err != nil {
		return err
	}
	return fmt.Errorf("%w\n%s", reason, closing)
}

// rollback restores every remote this reconfiguration changed, in reverse
// order. A repository that no longer holds what the command left behind is
// not touched: its recovery state must be resolved instead.
func rollback(root string, saved *state) error {
	directory := lock.RecoveryPath(root)
	// The moves are undone first, in reverse order of the plan: every later
	// step of this rollback compares a repository against the branch and
	// commit it started on.
	if err := assign.Verify(root, directory, "reconfiguration", saved.Configuration); err != nil {
		return err
	}
	if err := assign.Restore(root, directory, saved.Configuration); err != nil {
		return err
	}
	if err := restore(root, directory, saved.Moves, assign.Names(saved.Configuration)); err != nil {
		return err
	}
	// A rename that did not happen was never followed by a remote change, and
	// its repository cannot be read under the new name at all.
	pending, err := unmoved(root, saved.Renames)
	if err != nil {
		return err
	}
	entries := slices.Clone(saved.Repositories)
	slices.Reverse(entries)
	for _, entry := range entries {
		if pending[entry.Name] {
			continue
		}
		if _, err := resolve(root, entry, "", false); err != nil {
			return err
		}
	}
	_, err = unmove(root, saved.Renames)
	return err
}

// Resume finishes or undoes an interrupted reconfiguration. recovering
// applies the remote changes that are still missing, creates and checks out
// the repositories the plan adds, and fetches from them; otherwise every
// changed remote is set back to the URL it had and every directory this
// command created is removed again. A repository that changed outside GitOne
// since the interruption is never overwritten.
func Resume(root string, recovering bool, output io.Writer) error {
	saved, err := readState(lock.RecoveryPath(root))
	if err != nil {
		return err
	}
	directory := lock.RecoveryPath(root)
	if err := assign.Verify(root, directory, "reconfiguration", saved.Configuration); err != nil {
		return err
	}
	configuration, err := recordedConfiguration(root, saved)
	if err != nil {
		return err
	}
	// A recorded rename is the first write of the plan, so recovering performs
	// the move that is still missing before anything reads the repository
	// under its new name. Aborting reads it there first and moves it back
	// afterwards, and skips a rename that never happened: nothing was applied
	// to that repository under a name it does not have.
	if recovering {
		if err := moveRenames(root, saved.Renames); err != nil {
			return err
		}
	}
	pendingRenames, err := unmoved(root, saved.Renames)
	if err != nil {
		return err
	}
	// Aborting returns the moved repositories before anything else is
	// compared, so every later step sees the branch and commit each one
	// started on. Recovering performs the moves last, exactly as the
	// interrupted run would have.
	if !recovering {
		if err := assign.Restore(root, directory, saved.Configuration); err != nil {
			return err
		}
		if err := restore(root, directory, saved.Moves, assign.Names(saved.Configuration)); err != nil {
			return err
		}
	}
	tolerated := advanced(saved)
	entries := slices.Clone(saved.Repositories)
	if !recovering {
		slices.Reverse(entries)
	}
	results := make([][]string, len(entries))
	for index, entry := range entries {
		if pendingRenames[entry.Name] {
			continue
		}
		if results[index], err = resolve(root, entry, tolerated[entry.Name], recovering); err != nil {
			return err
		}
	}

	// Recovering finishes the plan, which is complete only once the added
	// repositories exist, the new remotes were contacted and still contain
	// what the repositories are on, and every addition is checked out.
	if recovering {
		additions, err := pending(configuration, root, saved)
		if err != nil {
			return err
		}
		record := func() error { return progress(root, directory, saved) }
		if err := build(root, configuration, additions, record); err != nil {
			return err
		}
		targets := planned(saved)
		if err := refresh(root, targets, record); err != nil {
			return err
		}
		if err := reachable(root, targets); err != nil {
			return fmt.Errorf("%w\nRun gitone abort to undo the reconfiguration instead.", err)
		}
		if len(additions) != 0 {
			if err := born(root, additions); err != nil {
				return fmt.Errorf("%w\nRun gitone abort to undo the reconfiguration instead.", err)
			}
			recordTargets(saved, additions)
			if err := record(); err != nil {
				return err
			}
			inventory, err := worktree.Scan(configuration, root, nil)
			if err != nil {
				return err
			}
			if err := feasible(configuration, root, additions[0].branch, plannedCommits(saved.Moves),
				additions, inventory); err != nil {
				return fmt.Errorf("%w\nRun gitone abort to undo the reconfiguration instead.", err)
			}
			if err := materialize(root, additions, record); err != nil {
				return fmt.Errorf("%w\nRun gitone abort to undo the reconfiguration instead.", err)
			}
		}
		if err := advance(root, saved.Moves, record); err != nil {
			return fmt.Errorf("%w\nRun gitone abort to undo the reconfiguration instead.", err)
		}
		if err := repository.UpdateIgnore(root); err != nil {
			return err
		}
		if err := assign.Apply(root, saved.Configuration); err != nil {
			return err
		}
		if err := retire(root, saved.Retirements); err != nil {
			if errors.Is(err, repository.ErrRetirementMove) {
				return errors.Join(err, os.RemoveAll(lock.RecoveryPath(root)))
			}
			return err
		}
		for index, entry := range entries {
			for _, current := range additions {
				if current.name == entry.Name {
					results[index] = append(results[index], current.line())
				}
			}
		}
	}
	retirements := make([]string, len(saved.Retirements))
	for index, entry := range saved.Retirements {
		retirements[index] = "retired to " + entry.Destination
	}
	renames := make([]string, len(saved.Renames))
	for index, entry := range saved.Renames {
		renames[index] = "renamed to " + entry.To
	}
	if !recovering {
		if retirements, err = unretire(root, saved.Retirements); err != nil {
			return err
		}
		if renames, err = unmove(root, saved.Renames); err != nil {
			return err
		}
	}

	verb := "aborted"
	if recovering {
		verb = "recovered"
	}
	fmt.Fprintf(output, "%s %s.\n\n", Command, verb)
	for index, entry := range entries {
		if len(results[index]) == 0 {
			fmt.Fprintf(output, "%-10s %s\n", entry.Name, "unchanged")
			continue
		}
		for _, line := range results[index] {
			fmt.Fprintf(output, "%-10s %s\n", entry.Name, line)
		}
	}
	for index, entry := range saved.Renames {
		fmt.Fprintf(output, "%-10s %s\n", entry.From, renames[index])
	}
	for index, entry := range saved.Retirements {
		fmt.Fprintf(output, "%-10s %s\n", entry.Name, retirements[index])
	}
	for _, entry := range saved.Moves {
		if recovering {
			fmt.Fprintf(output, "%-10s %s\n", entry.Name, entry.line())
			continue
		}
		fmt.Fprintf(output, "%-10s restored %s at %s\n", entry.Name, entry.From, short(entry.Head))
	}
	for _, file := range saved.Configuration {
		fmt.Fprintf(output, "%-10s %s\n", file.Path, assignmentResult(recovering))
	}
	return nil
}

// assignmentResult states what happened to a configuration file the
// interrupted reconfiguration patched with generated ownership.
func assignmentResult(recovering bool) string {
	if recovering {
		return "assigned the new paths, unstaged"
	}
	return "restored to the version before the reconfiguration"
}

// resolve brings every recorded remote of one repository to the URL the
// resumed command wants and reports what it did. A remote holding neither the
// recorded old nor the recorded new URL, and a branch that moved since the
// interruption, are refused instead of being overwritten.
func resolve(root string, entry repositoryState, tolerated string, recovering bool) ([]string, error) {
	if entry.Created {
		return resolveCreated(root, entry, recovering)
	}
	head, err := repository.Reference(root, entry.Name, "refs/heads/"+entry.Branch)
	if err != nil {
		return nil, err
	}
	// A recovering run has not undone the fast-forwards this reconfiguration
	// already performed, so a branch at its recorded target is its own work
	// rather than a change from outside GitOne.
	if entry.Branch != "" && head != entry.Head && head != tolerated {
		return nil, changed(root, entry, fmt.Sprintf("branch %s is at %s, not at the recorded %s",
			entry.Branch, or(short(head)), or(short(entry.Head))))
	}

	var results []string
	for _, remote := range entry.Remotes {
		if remote.After == "" {
			continue
		}
		wanted := remote.before()
		if recovering {
			wanted = remote.after()
		}
		actual, err := readRemote(root, entry.Name, remote.Name)
		if err != nil {
			return nil, err
		}
		switch {
		case actual == wanted:
			results = append(results, fmt.Sprintf("%s already points at %s", remote.Name, remoteDescription(wanted)))
			continue
		case actual != remote.before() && actual != remote.after():
			return nil, changed(root, entry, fmt.Sprintf("remote %q is %s, neither the recorded %s nor the planned %s",
				remote.Name, remoteDescription(actual), remoteDescription(remote.before()), remote.After))
		}
		if err := set(root, entry.Name, remote.Name, wanted); err != nil {
			return nil, err
		}
		results = append(results, fmt.Sprintf("%s set to %s", remote.Name, remoteDescription(wanted)))
	}
	return results, nil
}

// resolveCreated resolves one repository the interrupted reconfiguration
// created. Recovering leaves it to the finishing steps; aborting removes it
// again, together with the working-tree files its checkout wrote.
//
// A directory whose refs are not the recorded ones, or whose files were
// changed since the checkout, is retired instead of removed: somebody worked
// in it between the interruption and the abort, and nothing below .gitone/
// ever disappears without a deliberate human action.
func resolveCreated(root string, entry repositoryState, recovering bool) ([]string, error) {
	switch _, err := os.Stat(repository.Directory(root, entry.Name)); {
	case errors.Is(err, os.ErrNotExist):
		if recovering {
			return nil, nil
		}
		return []string{"was not created"}, nil
	case err != nil:
		return nil, err
	}

	listed, err := refs(root, entry.Name)
	if err != nil {
		return nil, err
	}
	head, err := repository.Reference(root, entry.Name, "refs/heads/"+entry.Branch)
	if err != nil {
		return nil, err
	}
	if recovering {
		if !slices.Equal(listed, entry.Refs) {
			return nil, changed(root, entry, "refs differ from the recorded reconfiguration state")
		}
		for _, remote := range entry.Remotes {
			if remote.After == "" {
				continue
			}
			actual, err := readRemote(root, entry.Name, remote.Name)
			if err != nil {
				return nil, err
			}
			if actual != remote.after() {
				return nil, changed(root, entry, fmt.Sprintf("remote %q is %s, not the recorded %s",
					remote.Name, remoteDescription(actual), remote.After))
			}
		}
		if head == "" {
			indexed, err := git.Run(root, "--git-dir="+repository.Directory(root, entry.Name), "ls-files", "-z")
			if err != nil {
				return nil, err
			}
			if strings.Trim(indexed, "\x00") != "" {
				return nil, changed(root, entry, "index differs from the recorded empty index")
			}
		} else if !repository.Matches(root, entry.Name, head) {
			return nil, changed(root, entry, "index or tracked working-tree files differ from the recorded checkout")
		}
		return nil, nil
	}
	if !slices.Equal(listed, entry.Refs) || (head != "" && !repository.Matches(root, entry.Name, head)) {
		destination, err := repository.Retire(root, entry.Name)
		if err != nil {
			return nil, err
		}
		return []string{"changed since the reconfiguration created it: retired to " + destination}, nil
	}
	if err := discard(root, entry.Name, entry.Branch); err != nil {
		return nil, err
	}
	return []string{"created by the reconfiguration: removed again"}, nil
}

// pending is every addition of a recorded plan, as the finishing steps need
// it: the configured origin and the branch the interrupted run recorded.
func pending(configuration *config.Config, root string, saved *state) ([]*addition, error) {
	var additions []*addition
	for _, entry := range saved.Repositories {
		configured, exists := configuration.Repositories[entry.Name]
		switch {
		case !entry.Created:
			continue
		case !exists:
			return nil, fmt.Errorf("%s repository %q is no longer configured: remove %s by hand instead",
				recoveryRequired, entry.Name, lock.RecoveryPath(root))
		}
		current := &addition{name: entry.Name, visibility: configured.Visibility,
			origin: configured.Remotes[config.OriginRemote], branch: entry.Branch,
			patterns: slices.Clone(configured.Paths), commit: entry.Target}
		if current.origin == "" {
			current.unborn = noOrigin
		}
		additions = append(additions, current)
	}
	return additions, nil
}

// planned is the repositories a recovered reconfiguration still has to fetch:
// every recorded repository that has a configured origin.
func planned(saved *state) []target {
	moves := map[string]moveState{}
	for _, entry := range saved.Moves {
		moves[entry.Name] = entry
	}
	var targets []target
	for _, entry := range saved.Repositories {
		for _, remote := range entry.Remotes {
			if remote.Name != config.OriginRemote {
				continue
			}
			url := remote.After
			if url == "" {
				url = remote.Before
			}
			if url != "" {
				head, branch := entry.Head, entry.Branch
				if move, exists := moves[entry.Name]; exists {
					head, branch = move.Target, move.Branch
				} else if entry.Created && entry.Target != "" {
					head = entry.Target
				}
				targets = append(targets, target{name: entry.Name, branch: branch, head: head, url: url})
			}
		}
	}
	return targets
}

// recordTargets freezes the fetched commit each addition will check out, so a
// later recovery cannot silently follow a branch that advanced meanwhile.
func recordTargets(saved *state, additions []*addition) {
	byName := map[string]string{}
	for _, current := range additions {
		byName[current.name] = current.commit
	}
	for index, entry := range saved.Repositories {
		if entry.Created {
			saved.Repositories[index].Target = byName[entry.Name]
		}
	}
}

// configurationFiles reads the exact trust sources a plain reconfiguration
// used. A missing local file differs from an existing empty one.
func configurationFiles(root string) (public, local []byte, localExists bool, err error) {
	public, err = os.ReadFile(filepath.Join(root, config.PublicFile))
	if err != nil {
		return nil, nil, false, err
	}
	local, err = os.ReadFile(filepath.Join(root, config.LocalFile))
	switch {
	case err == nil:
		return public, local, true, nil
	case errors.Is(err, os.ErrNotExist):
		return public, nil, false, nil
	default:
		return nil, nil, false, err
	}
}

// recordedConfiguration refuses a configuration edited after the
// interruption and rebuilds the exact effective configuration that was
// previewed and accepted.
func recordedConfiguration(root string, saved *state) (*config.Config, error) {
	public, local, localExists, err := configurationFiles(root)
	if err != nil {
		return nil, fmt.Errorf("%s the recorded configuration cannot be verified: %w", recoveryRequired, err)
	}
	changedFile := ""
	switch {
	case !patched(saved.Configuration, config.PublicFile) &&
		!bytes.Equal(public, saved.PublicConfiguration) && !bytes.Equal(public, saved.StartConfiguration):
		changedFile = config.PublicFile
	case !patched(saved.Configuration, config.LocalFile) &&
		(localExists != saved.LocalExists || !bytes.Equal(local, saved.LocalConfiguration)):
		changedFile = config.LocalFile
	}
	if changedFile != "" {
		return nil, fmt.Errorf("%s %s changed outside GitOne since the interrupted reconfiguration: restore its recorded contents or remove %s after resolving the operation manually",
			recoveryRequired, changedFile, lock.RecoveryPath(root))
	}
	if !saved.LocalExists {
		return config.Projected(saved.PublicConfiguration, nil)
	}
	local = saved.LocalConfiguration
	if local == nil {
		local = []byte{}
	}
	return config.Projected(saved.PublicConfiguration, local)
}

func patched(files []assign.Patched, name string) bool {
	return slices.ContainsFunc(files, func(file assign.Patched) bool { return file.Path == name })
}

// set points one remote at the requested state. Removing only its configuration
// preserves fetched tracking refs as the cache they are.
func set(root, name, remoteName string, wanted nativeRemote) error {
	gitDirectory := "--git-dir=" + repository.Directory(root, name)
	actual, err := readRemote(root, name, remoteName)
	if err != nil {
		return err
	}
	switch {
	case !wanted.exists:
		_, err = git.Run(root, gitDirectory, "config", "--remove-section", "remote."+remoteName)
	case !actual.exists:
		_, err = git.Run(root, gitDirectory, "remote", "add", remoteName, wanted.url)
	case wanted.url == "":
		_, err = git.Run(root, gitDirectory, "config", "--unset-all", "remote."+remoteName+".url")
	default:
		_, err = git.Run(root, gitDirectory, "remote", "set-url", remoteName, wanted.url)
	}
	return err
}

func remoteDescription(remote nativeRemote) string {
	if !remote.exists {
		return "(absent)"
	}
	if remote.url == "" {
		return "(no URL)"
	}
	return remote.url
}

func changed(root string, entry repositoryState, detail string) error {
	return changedName(root, entry.Name, detail)
}

func changedName(root, name, detail string) error {
	return fmt.Errorf("%s repository %q changed outside GitOne since the interrupted reconfiguration: %s\n"+
		"Set it to the state you want to keep and archive %s outside the project afterwards; do not delete it.",
		recoveryRequired, name, detail, lock.RecoveryPath(root))
}

func readState(directory string) (*state, error) {
	contents, err := os.ReadFile(filepath.Join(directory, lock.StateFile))
	if err != nil {
		return nil, err
	}
	saved := new(state)
	if err := json.Unmarshal(contents, saved); err != nil {
		return nil, fmt.Errorf("%s the recorded reconfiguration state is unreadable: %w", recoveryRequired, err)
	}
	if saved.Version != stateVersion {
		return nil, fmt.Errorf("%s the recorded reconfiguration state has unsupported version %d", recoveryRequired, saved.Version)
	}
	return saved, nil
}
