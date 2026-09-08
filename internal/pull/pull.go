// Package pull brings the managed branches to the state their origin remotes
// already have. Every pull fetches first and then fast-forwards; it never
// merges, rebases, stashes or discards local work. No local branch moves
// before every selected repository passed preflight, and a failure afterwards
// either restores the branches this pull advanced or leaves recovery state
// for gitone recover and gitone abort.
package pull

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/devidevio/gitone/internal/assign"
	"github.com/devidevio/gitone/internal/config"
	"github.com/devidevio/gitone/internal/fetch"
	"github.com/devidevio/gitone/internal/git"
	"github.com/devidevio/gitone/internal/guidance"
	"github.com/devidevio/gitone/internal/lock"
	"github.com/devidevio/gitone/internal/policy"
	"github.com/devidevio/gitone/internal/repository"
	"github.com/devidevio/gitone/internal/status"
	"github.com/devidevio/gitone/internal/ui"
)

const (
	// Command is the recorded command of an interrupted pull.
	Command = "gitone pull"

	// AcceptPathsFlag is the explicit approval of the ownership a pull
	// generates for the new paths its incoming commits carry.
	AcceptPathsFlag = assign.Flag

	// ReconfigureCommand is the exact next command for an incoming policy
	// that also adds, removes or renames a repository or changes a configured
	// remote. It is spelled out rather than imported: gitone reconfigure
	// reuses this package's fast-forward.
	ReconfigureCommand = "gitone reconfigure --pull"

	pullFailed       = "PULL001"
	recoveryRequired = "REC001"

	// unmodified closes every message that stopped before a branch moved. A
	// pull fetches first, and fetched refs are never rolled back.
	unmodified = "No branch, index or working-tree file was changed. " +
		"The remote-tracking refs this pull fetched remain updated."
)

// The outcome of one repository, in the order the report explains them.
const (
	updated   = "updated"
	unchanged = "unchanged"
	skipped   = "skipped"
	failed    = "failed"
)

// participant is one selected repository and what the pull does with it. head
// is the commit its branch starts from and target the commit it advances to,
// both empty while the repository does not move.
type participant struct {
	fetch.Repository
	branch string
	dirty  bool
	head   string
	target string
	state  string
	detail string
}

// Pull fast-forwards the selected repositories to their origin branches.
// target is empty or "all" for every configured repository, or the name of
// one. accepted approves a reviewed incoming policy change and assigned
// approves the ownership generated for newly unassigned incoming paths; each
// is required when no interactive terminal can answer its question.
func Pull(configuration *config.Config, root, target string, accepted, assigned, interactive bool, input io.Reader, output io.Writer) error {
	selected, err := fetch.Select(configuration, target)
	if err != nil {
		return err
	}
	release, err := lock.Acquire(root, Command)
	if err != nil {
		return err
	}
	defer release()

	if err := fetch.Check(configuration, root, selected); err != nil {
		return err
	}
	// The complete working tree is validated even for a single repository,
	// and before the first remote is contacted.
	result, inventory, err := status.Validate(configuration, root)
	if err != nil {
		return err
	}

	participants := plan(selected, result)
	var incoming *policy.Policy
	var added *assign.Assignment
	err = ui.SpinBuffered(input, output, "Pulling repositories", func(rendered io.Writer) error {
		if err := refresh(root, participants); err != nil {
			return stopped(rendered, participants, err)
		}
		if err := preflight(root, participants); err != nil {
			return stopped(rendered, participants, err)
		}
		// The incoming policy is read, validated and compared before the
		// first branch, index or working-tree file changes, and the projected
		// policy is what the incoming trees are then validated against.
		incoming, err = review(configuration, root, inventory, participants)
		if err != nil {
			return stopped(rendered, participants, err)
		}
		unassigned, err := treeIssues(incoming, root, participants, inventory.Tracked)
		if err != nil {
			return stopped(rendered, participants, err)
		}
		// Assistance is offered only here: every fetch, upstream,
		// fast-forward, local-change, target-policy and tree check has
		// passed, so the new paths are the last blocker left.
		added, err = assign.Propose(reviewed(participants), incoming, root, unassigned, inventory.Tracked)
		if err != nil {
			return stopped(rendered, participants, err)
		}
		if incoming.Changed() || added != nil {
			// The preview and its questions come before the work a spinner
			// would cover, so the update runs in a second phase.
			return nil
		}
		return update(root, participants, nil, rendered)
	})
	if err != nil || !incoming.Changed() && added == nil {
		return err
	}

	// One ordered review: the remote policy change and the local paths it
	// affects first, then the ownership this pull generates for the new paths.
	if incoming.Changed() {
		incoming.Preview(output)
	}
	added.Preview(output)
	// Both questions read from the same buffered reader, so answering the
	// first one does not swallow the answer to the second.
	answers := bufio.NewReader(input)
	confirmed, err := incoming.Confirm(accepted, interactive, answers, output)
	if err != nil || !confirmed {
		return err
	}
	if confirmed, err = added.Confirm(reviewed(participants), assigned, interactive, answers, output); err != nil || !confirmed {
		return err
	}
	return ui.SpinBuffered(input, output, "Updating repositories", func(rendered io.Writer) error {
		return update(root, participants, added, rendered)
	})
}

func update(root string, participants []*participant, added *assign.Assignment, rendered io.Writer) error {
	if err := advance(root, participants, added, rendered); err != nil {
		return stopped(rendered, participants, err)
	}
	added.Report(rendered)
	return nil
}

// stopped closes a failed phase with the complete report, so a refusal always
// states which repositories were advanced before it and which were not.
func stopped(rendered io.Writer, participants []*participant, failure error) error {
	stop(participants)
	report(rendered, participants)
	return failure
}

// plan pairs every selected repository with the branch it has checked out and
// whether it holds local changes a fast-forward must not touch. Untracked
// files are no such change; native Git still refuses to overwrite one.
func plan(selected []fetch.Repository, result *status.Status) []*participant {
	branches := map[string]string{}
	for _, reported := range result.Repositories {
		branches[reported.Name] = reported.Branch
	}
	dirty := map[string]bool{}
	for _, change := range result.Changes {
		if change.State == status.Staged || change.State == status.Unstaged {
			dirty[change.Repository] = true
		}
	}
	participants := make([]*participant, 0, len(selected))
	for _, current := range selected {
		participants = append(participants, &participant{
			Repository: current,
			branch:     branches[current.Name],
			dirty:      dirty[current.Name],
		})
	}
	return participants
}

// refresh fetches every participating origin, so the pull decides on the
// current remote state. Failures do not stop the remaining fetches, but any
// failure stops preflight and every local branch stays unchanged.
func refresh(root string, participants []*participant) error {
	var broken []string
	for _, current := range participants {
		if current.Remote == "" {
			current.state = skipped
			continue
		}
		if err := fetch.Update(root, current.Name); err != nil {
			current.state, current.detail = failed, detail(err)
			broken = append(broken, current.Name)
		}
	}
	if len(broken) == 0 {
		return nil
	}
	return fmt.Errorf("%s %s could not be fetched.\n%s", pullFailed, strings.Join(broken, ", "), unmodified)
}

// preflight decides for every fetched repository what the pull would do and
// refuses everything that is not a safe fast-forward. It reports all problems
// at once and completes before the first branch, index or working-tree file
// changes.
func preflight(root string, participants []*participant) error {
	var issues []string
	for _, current := range participants {
		if current.state == skipped {
			continue
		}
		head, err := repository.Reference(root, current.Name, "refs/heads/"+current.branch)
		if err != nil {
			return err
		}
		target, issue, err := Plan(root, current.Name, current.branch, head, current.dirty)
		if err != nil {
			return err
		}
		if issue != "" {
			issues = append(issues, problem(current, "%s", issue))
			continue
		}
		if target != "" {
			current.head, current.target = head, target
			continue
		}
		upstream, err := repository.Reference(root, current.Name, "refs/remotes/"+config.OriginRemote+"/"+current.branch)
		if err != nil {
			return err
		}
		if head == upstream {
			current.state, current.detail = unchanged, "already up to date"
		} else {
			current.state, current.detail = unchanged, fmt.Sprintf("already up to date, %s is ahead of %s/%s", current.branch, config.OriginRemote, current.branch)
		}
	}
	if len(issues) == 0 {
		return nil
	}
	return errors.New(strings.Join(append(issues, unmodified), "\n"))
}

// Plan returns the fast-forward target for one repository, or the same
// refusal gitone pull reports when that move is unsafe.
func Plan(root, name, branch, head string, dirty bool) (string, string, error) {
	tracksOrigin, err := repository.TracksOriginBranch(root, name, branch)
	if err != nil {
		return "", "", err
	}
	upstream, err := repository.Reference(root, name, "refs/remotes/"+config.OriginRemote+"/"+branch)
	if err != nil {
		return "", "", err
	}
	switch {
	case head == "":
		return "", fmt.Sprintf("has no commit on %s yet. %s", branch, guidance.UnbornRepair), nil
	case !tracksOrigin:
		return "", fmt.Sprintf("%s does not track %s/%s", branch, config.OriginRemote, branch), nil
	case upstream == "":
		return "", fmt.Sprintf("has no %s/%s to pull from: push %s first", config.OriginRemote, branch, branch), nil
	case head == upstream || ancestor(root, name, upstream, head):
		return "", "", nil
	case !ancestor(root, name, head, upstream):
		return "", fmt.Sprintf("%s and %s/%s have diverged: %s is not a fast-forward; follow "+guidance.ResolvingDivergedHistory,
			branch, config.OriginRemote, branch, short(upstream)), nil
	case dirty:
		return "", fmt.Sprintf("has local changes on %s: commit or undo them before pulling", branch), nil
	default:
		return upstream, "", nil
	}
}

// treeIssues reports every entry of the incoming tree that the repository must
// not materialize, so an unsafe tree is refused before it reaches the working
// tree. It validates against the projected matcher, which is the policy that
// is in effect once the pull applied.
//
// An entry that is only unassigned and that a selected target newly
// introduces is returned instead of refused: that ownership is deterministic
// and is what an assisted assignment offers. Every other ownership or link
// failure stays a hard refusal, and one such failure reports the unassigned
// entries with it: assistance never repairs a project that is broken anyway.
func treeIssues(incoming *policy.Policy, root string, participants []*participant, tracked []string) ([]repository.TreeIssue, error) {
	commits := incoming.Commits
	byName := map[string]*participant{}
	for _, current := range participants {
		byName[current.Name] = current
	}
	existing := map[string]bool{}
	for _, entry := range tracked {
		existing[entry] = true
	}
	refused, err := repository.TreeIssues(incoming.Matcher, root, commits)
	if err != nil {
		return nil, fmt.Errorf("%s incoming trees cannot be read: %s\n%s", pullFailed, detail(err), unmodified)
	}

	// A pre-existing unassigned path is already tracked, and a repository
	// this pull does not move brings nothing new, so neither is assisted. A
	// path holding a * is not assisted either: as a pattern that character
	// matches inside a segment, so the generated ownership would silently
	// reach further than the one path the commit carries.
	var unassigned []repository.TreeIssue
	hard := false
	for _, entry := range refused {
		current := byName[entry.Repository]
		if entry.Issue == config.OwnerUnassigned && current != nil && current.target != "" &&
			!existing[entry.Path] && !strings.Contains(entry.Path, "*") {
			unassigned = append(unassigned, entry)
			continue
		}
		hard = true
	}
	if !hard {
		return unassigned, nil
	}

	issues := make([]string, 0, len(refused))
	for _, entry := range refused {
		if current := byName[entry.Repository]; current != nil {
			issues = append(issues, problem(current, "%s in %s: %s", entry.Path, short(commits[entry.Repository]), entry.Reason))
		} else {
			issues = append(issues, fmt.Sprintf("%s repository %q would hold %s in %s: %s",
				pullFailed, entry.Repository, entry.Path, short(commits[entry.Repository]), entry.Reason))
		}
	}
	return nil, errors.New(strings.Join(append(issues, unmodified), "\n"))
}

// report prints one line per repository, so a partial failure states exactly
// which repositories were advanced before it.
func report(output io.Writer, participants []*participant) {
	style := ui.For(output)
	fmt.Fprintf(output, "%s\n\n", style.Heading.Render("Pull results:"))
	for _, current := range participants {
		fmt.Fprintf(output, "%-10s %s\n", current.Name, current.line(style))
	}
}

func (p *participant) line(style ui.Styles) string {
	switch p.state {
	case updated:
		return style.Success.Render(fmt.Sprintf("✓ updated %s to %s", p.branch, short(p.target)))
	case unchanged:
		return style.Muted.Render(p.detail)
	case skipped:
		return style.Muted.Render("skipped, no configured " + config.OriginRemote + " remote")
	default:
		return style.Error.Render("failed: " + p.detail)
	}
}

// ancestor reports whether commit is contained in other, which decides
// already current, locally ahead, fast-forward and diverged.
func ancestor(root, name, commit, other string) bool {
	_, err := git.Run(root, "--git-dir="+repository.Directory(root, name), "merge-base", "--is-ancestor", commit, other)
	return err == nil
}

func problem(current *participant, format string, arguments ...any) string {
	detail := fmt.Sprintf(format, arguments...)
	current.state = failed
	if current.detail == "" {
		current.detail = detail
	}
	return fmt.Sprintf("%s repository %q %s", pullFailed, current.Name, detail)
}

// stop marks repositories that fetched successfully but were not pulled
// because another repository failed.
func stop(participants []*participant) {
	for _, current := range participants {
		if current.state == "" {
			current.state, current.detail = unchanged, "unchanged, pull stopped before local updates"
		}
	}
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
