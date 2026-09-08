// Package switching moves every managed repository to the same local branch.
// It creates the branch where a repository lacks it - at that repository's
// own HEAD with -c, otherwise from its own fetched origin/<branch> - and
// never guesses a start point from another repository, from HEAD or from the
// network. It never discards, stashes or carries local changes: the project
// must be clean, and every incoming tree must contain only paths its
// repository owns. No ref is written before all repositories passed
// preflight, and a failure afterwards either returns the project to its
// starting state or leaves recovery state for gitone recover and gitone
// abort.
package switching

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/devidevio/gitone/internal/assign"
	"github.com/devidevio/gitone/internal/branch"
	"github.com/devidevio/gitone/internal/config"
	"github.com/devidevio/gitone/internal/git"
	"github.com/devidevio/gitone/internal/guidance"
	"github.com/devidevio/gitone/internal/lock"
	"github.com/devidevio/gitone/internal/policy"
	"github.com/devidevio/gitone/internal/repository"
	"github.com/devidevio/gitone/internal/status"
	"github.com/devidevio/gitone/internal/ui"
)

const (
	// Command is the recorded command of an interrupted switch.
	Command = "gitone switch"

	// ReconfigureCommand is the exact next command for a target policy that
	// also adds, removes or renames a repository or changes a configured
	// remote. It is spelled out rather than imported: gitone reconfigure
	// reuses this package's checkout.
	ReconfigureCommand = "gitone reconfigure --switch"

	switchFailed     = "SWITCH001"
	recoveryRequired = "REC001"

	// CreateFlag and CreateShortFlag create the requested branch in every
	// repository, at the commit each one currently has checked out, before
	// switching the whole project to it.
	CreateFlag      = "--create"
	CreateShortFlag = "-c"

	// unmodified closes every message of a refused or rolled back switch.
	unmodified = "No branch, HEAD, index or working-tree file was changed."
)

// The outcome of one repository, in the order the report explains them.
const (
	switched  = "switched"
	unchanged = "unchanged"
	failed    = "failed"
)

// participant is one repository and what the switch does with it. from is the
// branch it starts on and head the commit that branch holds; target is the
// commit of the requested branch, which for a branch this switch creates is
// the start point it will create it at. create records that the branch has to
// be written here first and track that it then takes origin/<branch> as its
// upstream. state stays empty while the repository is still to be switched.
type participant struct {
	name     string
	from     string
	head     string
	target   string
	create   bool
	track    bool
	conflict string
	state    string
	detail   string
}

// Switch moves every configured repository to the local branch name, creating
// it where a repository lacks it. creating requests a new branch at every
// repository's current HEAD; without it a missing local branch is taken from
// that repository's own fetched origin/<branch>. accepted approves a reviewed
// target policy change and assigned approves the ownership generated for
// newly unassigned target paths; each is required when no interactive
// terminal can answer its question.
func Switch(configuration *config.Config, root, name string, creating, accepted, assigned, interactive bool, input io.Reader, output io.Writer) error {
	if creating {
		if err := validate(name); err != nil {
			return err
		}
	}
	release, err := lock.Acquire(root, Command)
	if err != nil {
		return err
	}
	defer release()

	inspected, err := branch.Inspect(configuration, root)
	if err != nil {
		return err
	}
	participants, err := plan(root, inspected, name, creating)
	if err != nil {
		return err
	}
	// The branch state is checked first, so a missing branch is reported as a
	// branch problem instead of as whatever else the project may need.
	if err := preflight(configuration, participants, name, creating); err != nil {
		return err
	}
	// The complete working tree is validated before the first repository
	// moves, exactly like every other mutating command. Only repositories
	// that disagree about their branch are tolerated, because putting them
	// back on one branch is what this command does.
	result, inventory, err := status.ValidateBranchDisagreement(configuration, root)
	if err != nil {
		return err
	}
	// The target policy is read, validated and compared before the first
	// branch, HEAD, index or working-tree file changes, and the projected
	// policy is what the target trees are then validated against.
	incoming, err := review(configuration, root, name, inventory, participants)
	if err != nil {
		return err
	}
	unassigned, err := feasible(incoming, root, participants, name, result, inventory.Tracked)
	if err != nil {
		return err
	}
	// Assistance is offered only here: every branch, working-tree,
	// target-policy, ownership and checkout check has passed, so the new
	// paths are the last blocker left.
	added, err := assign.Propose(reviewed(name), incoming, root, unassigned, inventory.Tracked)
	if err != nil {
		return err
	}

	// One ordered review: the target policy change and the local paths it
	// affects first, then the ownership this switch generates for the new
	// paths.
	if incoming.Changed() {
		incoming.Preview(output)
	}
	added.Preview(output)
	var confirmed bool
	if incoming.Changed() && added != nil && interactive && !accepted && !assigned {
		confirmed, err = policy.Ask(reviewed(name), "an incoming policy change and assigning new target paths",
			"Accept this policy change and assign these paths?", policy.AcceptFlag+" and "+assign.Flag,
			false, true, input, output)
	} else {
		confirmed, err = incoming.Confirm(accepted, interactive, input, output)
		if err != nil || !confirmed {
			return err
		}
		confirmed, err = added.Confirm(reviewed(name), assigned, interactive, input, output)
	}
	if err != nil || !confirmed {
		return err
	}
	return ui.SpinBuffered(input, output, "Switching to "+name, func(rendered io.Writer) error {
		return apply(root, participants, name, added, rendered)
	})
}

// validate applies the configured branch rule, which is native Git's own, so
// a created branch name is accepted here exactly when gitone branch accepts
// it. Only a switch that creates checks the name; an existing branch already
// has one Git wrote.
func validate(name string) error {
	err := config.ValidateBranch(name)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, git.ErrUnavailable):
		return err
	default:
		return fmt.Errorf("%s %q is not a valid branch name", switchFailed, name)
	}
}

// plan pairs every repository with the branch it starts on and the commit the
// requested branch holds there. Where the branch is missing, the start point
// comes from that repository alone: its current HEAD when the switch creates
// the branch, otherwise its own fetched origin/<branch>. Nothing is fetched
// here, and a repository that has neither leaves the target empty and is
// refused by preflight.
func plan(root string, inspected []branch.State, name string, creating bool) ([]*participant, error) {
	participants := make([]*participant, 0, len(inspected))
	for _, current := range inspected {
		target, err := repository.Reference(root, current.Name, "refs/heads/"+name)
		if err != nil {
			return nil, err
		}
		entry := &participant{name: current.Name, from: current.Current, head: current.Head, target: target}
		switch {
		case target != "":
			// The branch already exists here and is left exactly as it is.
		case creating:
			entry.target, entry.create = current.Head, true
		default:
			remote, err := repository.Reference(root, current.Name, "refs/remotes/"+config.OriginRemote+"/"+name)
			if err != nil {
				return nil, err
			}
			if remote != "" {
				entry.target, entry.create, entry.track = remote, true, true
			}
		}
		if entry.create {
			for _, existing := range current.Branches {
				if strings.HasPrefix(existing, name+"/") || strings.HasPrefix(name, existing+"/") {
					entry.conflict = existing
					break
				}
			}
		}
		participants = append(participants, entry)
	}
	return participants, nil
}

// missingBranch names the next step for a repository that has neither the
// branch nor a ref to create it from. A configured origin can still carry it,
// so fetching is the step; without one there is nothing to fetch and the
// start point is a human decision.
func missingBranch(configuration *config.Config, repositoryName, name string) string {
	if _, remote := configuration.Repositories[repositoryName].Remotes[config.OriginRemote]; remote {
		return fmt.Sprintf("has neither branch %s nor %s/%s: run gitone fetch %s and retry; if the remote has no such branch, choose the missing branch's start point using %s",
			name, config.OriginRemote, name, repositoryName, guidance.RepairingMissingBranches)
	}
	return fmt.Sprintf("has no branch %s and no %s to take it from: create only the missing branches using %s",
		name, config.OriginRemote, guidance.RepairingMissingBranches)
}

// preflight refuses every branch state in which one shared switch is not
// possible. It reports all problems at once and completes before anything
// else is inspected. A creation additionally needs the name to be free and a
// starting commit in every repository, so half the project is never left to
// be created by hand afterwards.
func preflight(configuration *config.Config, participants []*participant, name string, creating bool) error {
	var issues []string
	for _, current := range participants {
		switch {
		case current.from == "":
			// Without a starting branch there is nothing to record and
			// nothing to return to; a detached HEAD is not supported.
			issues = append(issues, problem(current, "is not on a branch: switching needs a recorded starting branch"))
		case current.conflict != "":
			issues = append(issues, problem(current, "cannot create branch %s: conflicts with existing branch %s", name, current.conflict))
		case creating && !current.create:
			issues = append(issues, problem(current, "already has branch %s: switch to it without %s", name, CreateShortFlag))
		case creating && current.head == "":
			issues = append(issues, problem(current, "has no commit on %s yet: commit before creating %s", current.from, name))
		case current.head == "":
			// Nothing can be checked out over an unborn HEAD, and there
			// would be no commit to return the repository to afterwards.
			issues = append(issues, problem(current, "has no commit on %s yet. %s", current.from, guidance.UnbornRepair))
		case current.target == "":
			issues = append(issues, problem(current, "%s", missingBranch(configuration, current.name, name)))
		case current.from == name:
			current.state, current.detail = unchanged, "already on "+name
		}
	}
	if len(issues) == 0 {
		return nil
	}
	return errors.New(strings.Join(append(issues, unmodified), "\n"))
}

// feasible refuses everything that keeps the moving repositories from being
// checked out safely: local changes the switch would have to carry, a target
// tree with paths the repository does not own, and a checkout native Git
// itself would refuse. Ownership is decided by the projected policy, which is
// the policy in effect once the switch applied. It reports all problems at
// once and completes before the first repository moves.
//
// A target entry that is only unassigned and that a moving repository newly
// introduces is returned instead of refused: that ownership is deterministic
// and is what an assisted assignment offers. Every other ownership or link
// failure stays a hard refusal, and one such failure reports the unassigned
// entries with it: assistance never repairs a project that is broken anyway.
func feasible(incoming *policy.Policy, root string, participants []*participant, name string, result *status.Status, tracked []string) ([]repository.TreeIssue, error) {
	var issues []string
	byName := map[string]*participant{}
	unsafe := map[string]bool{}
	for _, current := range participants {
		byName[current.name] = current
	}
	existing := map[string]bool{}
	for _, entry := range tracked {
		existing[entry] = true
	}
	refused, err := repository.TreeIssues(incoming.Matcher, root, incoming.Commits)
	if err != nil {
		issues = append(issues, switchFailed+" "+detail(err))
	}

	// A path that is already tracked was unassigned before this switch, and a
	// repository that does not move brings nothing new, so neither is
	// assisted. A path holding a * is not assisted either: as a pattern that
	// character matches inside a segment, so the generated ownership would
	// silently reach further than the one path the branch carries.
	var unassigned []repository.TreeIssue
	hard := false
	for _, entry := range refused {
		current := byName[entry.Repository]
		if entry.Issue == config.OwnerUnassigned && current != nil && current.state != unchanged &&
			!existing[entry.Path] && !strings.Contains(entry.Path, "*") {
			unassigned = append(unassigned, entry)
			continue
		}
		hard = true
	}
	if hard {
		for _, entry := range refused {
			unsafe[entry.Repository] = true
			issues = append(issues, problem(byName[entry.Repository], "%s in %s: %s", entry.Path, name, entry.Reason))
		}
	}
	for _, current := range participants {
		changes := local(result, current.name)
		if changes != "" {
			_, issue, err := Plan(root, current.name, current.from, name, current.target, changes)
			if err != nil {
				return nil, err
			}
			issues = append(issues, problem(current, "%s", issue))
			continue
		}
		if current.state == unchanged {
			continue
		}
		if unsafe[current.name] {
			continue
		}
		_, issue, err := Plan(root, current.name, current.from, name, current.target, "")
		if err != nil {
			return nil, err
		}
		if issue != "" {
			issues = append(issues, problem(current, "%s", issue))
		}
	}
	if len(issues) == 0 {
		return unassigned, nil
	}
	if !hard {
		// Another problem stops this switch, so the entries an assignment
		// would have offered are reported like every other one.
		for _, entry := range refused {
			issues = append(issues, problem(byName[entry.Repository], "%s in %s: %s", entry.Path, name, entry.Reason))
		}
	}
	return nil, errors.New(strings.Join(append(issues, unmodified), "\n"))
}

// Plan reports whether one repository has to move, or the same refusal
// gitone switch reports when its checkout is unsafe.
func Plan(root, repositoryName, from, branch, target, changes string) (bool, string, error) {
	switch {
	case from == "":
		return false, "is not on a branch: switching needs a recorded starting branch", nil
	case target == "":
		return false, fmt.Sprintf("has no branch %s: inspect with gitone branch and follow %s",
			branch, guidance.RepairingMissingBranches), nil
	case changes != "":
		return false, fmt.Sprintf("has %s on %s: commit or undo them before switching", changes, from), nil
	case from == branch:
		return false, "", nil
	}
	// Native Git decides whether the checkout itself would succeed, so GitOne
	// never has to reimplement its overwrite rules.
	if _, err := git.Run(root, "--git-dir="+repository.Directory(root, repositoryName), "--work-tree="+root,
		"read-tree", "--dry-run", "-u", "-m", "HEAD", target); err != nil {
		return false, fmt.Sprintf("cannot check out %s: %s", branch, detail(err)), nil
	}
	return true, "", nil
}

// local names the changes of one repository that a switch must not carry
// across branches. Untracked files are no such change; native Git still
// refuses to overwrite one.
func local(result *status.Status, name string) string {
	staged, unstaged := false, false
	for _, change := range result.Changes {
		if change.Repository != name {
			continue
		}
		switch change.State {
		case status.Staged:
			staged = true
		case status.Unstaged:
			unstaged = true
		}
	}
	switch {
	case staged && unstaged:
		return "staged and unstaged changes"
	case staged:
		return "staged changes"
	case unstaged:
		return "unstaged changes"
	}
	return ""
}

// report prints one line per repository, so a partial failure states exactly
// which repositories were switched before it.
func report(output io.Writer, participants []*participant, name string) {
	style := ui.For(output)
	fmt.Fprintf(output, "%s\n\n", style.Heading.Render("Switch results:"))
	for _, current := range participants {
		fmt.Fprintf(output, "%-10s %s\n", current.name, current.line(style, name))
	}
}

func (p *participant) line(style ui.Styles, name string) string {
	switch p.state {
	case switched:
		return style.Success.Render(fmt.Sprintf("✓ %s → %s%s", p.from, name, p.created(name)))
	case unchanged:
		return style.Muted.Render(p.detail)
	case failed:
		return style.Error.Render("failed: " + p.detail)
	default:
		return style.Muted.Render("still on " + p.from + ", switch stopped before it")
	}
}

// created states that this switch wrote the branch it moved to, and whether
// it also configured the origin branch it came from as its upstream.
func (p *participant) created(name string) string {
	switch {
	case p.track:
		return " (created from " + config.OriginRemote + "/" + name + ", tracking it)"
	case p.create:
		return " (created at " + short(p.head) + ")"
	}
	return ""
}

func problem(current *participant, format string, arguments ...any) string {
	detail := fmt.Sprintf(format, arguments...)
	current.state = failed
	if current.detail == "" {
		current.detail = detail
	}
	return fmt.Sprintf("%s repository %q %s", switchFailed, current.name, detail)
}

// detail is the first line of an error, so one reported problem stays one line.
func detail(err error) string {
	line, _, _ := strings.Cut(err.Error(), "\n")
	return line
}

func short(commit string) string {
	if len(commit) <= 7 {
		return commit
	}
	return commit[:7]
}
