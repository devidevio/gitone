// Package push publishes the managed repositories to their origin remotes.
// Every participating repository completes ownership, history, destination and
// connectivity preflight before the first remote is modified. The push itself
// is best effort and never claimed to be atomic: what a partial failure left
// behind is read back from the remotes instead of being guessed.
package push

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
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
	// Command is the recorded command of an interrupted push.
	Command = "gitone push"

	repositoryInvalid = "REPO001"
	preflightFailed   = "PUSH001"
	recoveryRequired  = "REC001"

	// remoteName is the only remote GitOne publishes to. Arbitrary remotes,
	// refspecs, deletions and force pushes are unsupported.
	remoteName = config.OriginRemote

	// unmodified closes every message that stopped before a remote changed.
	unmodified = "No repositories were modified."
)

// participant is one repository of a push.
type participant struct {
	name   string
	branch string
	// head is the local commit the branch points at, and remote is the value
	// the remote branch has, empty while it does not exist yet.
	head     string
	remote   string
	commits  int
	upstream bool
}

// Push publishes the selected repositories. target is empty or "all" for every
// configured repository, or the name of one repository. yes replaces the
// confirmation, which is required when no interactive terminal can answer it.
func Push(configuration *config.Config, root, target string, yes, interactive bool, input io.Reader, output io.Writer) error {
	if !yes && !interactive {
		return fmt.Errorf("%s a push without an interactive terminal requires --yes\n%s", preflightFailed, unmodified)
	}
	release, err := lock.Acquire(root, Command)
	if err != nil {
		return err
	}
	defer release()

	result, _, err := status.Validate(configuration, root)
	if err != nil {
		return err
	}
	selected, skipped, err := selection(configuration, result, target)
	if err != nil {
		return err
	}
	if configuration.Push.RequireCleanWorktree && len(result.Changes) != 0 {
		return fmt.Errorf("%s push requires a clean working tree\n%s", preflightFailed, unmodified)
	}
	participants, err := outgoing(root, selected)
	if err != nil {
		return err
	}
	if len(skipped) != 0 {
		fmt.Fprintf(output, "Skipped push-disabled repositories: %s.\n", strings.Join(skipped, ", "))
	}
	if len(participants) == 0 {
		fmt.Fprintln(output, "Nothing to push.")
		return nil
	}
	if err := preflight(configuration, root, participants); err != nil {
		return err
	}

	summary(output, participants)
	confirmed, err := confirm(yes, input, output)
	if err != nil || !confirmed {
		return err
	}
	return ui.SpinBuffered(input, output, "Pushing repositories", func(rendered io.Writer) error {
		return publish(root, participants, rendered)
	})
}

// selection resolves the command input into the repositories the push may
// consider.
func selection(configuration *config.Config, result *status.Status, target string) ([]*participant, []string, error) {
	named := target != "" && target != "all"
	if named && !slices.ContainsFunc(result.Repositories, func(reported status.Repository) bool { return reported.Name == target }) {
		return nil, nil, fmt.Errorf("%s unknown repository %q", repositoryInvalid, target)
	}
	var selected []*participant
	var skipped []string
	for _, reported := range result.Repositories {
		if named && reported.Name != target {
			continue
		}
		if configuration.Repositories[reported.Name].Push == config.PushDisabled {
			if named {
				return nil, nil, fmt.Errorf("%s repository %q has push disabled", preflightFailed, reported.Name)
			}
			skipped = append(skipped, reported.Name)
			continue
		}
		if reported.Branch == status.Detached {
			return nil, nil, fmt.Errorf("%s repository %q has no branch checked out: HEAD is detached", repositoryInvalid, reported.Name)
		}
		selected = append(selected, &participant{name: reported.Name, branch: reported.Branch})
	}
	return selected, skipped, nil
}

// outgoing keeps the repositories that have commits to publish. The local
// remote-tracking ref decides that, so an unchanged repository is skipped
// without contacting its remote.
func outgoing(root string, selected []*participant) ([]*participant, error) {
	var participants []*participant
	for _, candidate := range selected {
		head, err := reference(root, candidate.name, "refs/heads/"+candidate.branch)
		if err != nil {
			return nil, err
		}
		if head == "" {
			continue
		}
		tracked, err := reference(root, candidate.name, "refs/remotes/"+remoteName+"/"+candidate.branch)
		if err != nil {
			return nil, err
		}
		count, err := countCommits(root, candidate.name, head, tracked)
		if err != nil {
			return nil, err
		}
		if count == 0 {
			continue
		}
		upstream, err := repository.TracksOriginBranch(root, candidate.name, candidate.branch)
		if err != nil {
			return nil, err
		}
		candidate.head, candidate.upstream = head, upstream
		participants = append(participants, candidate)
	}
	return participants, nil
}

// preflight completes every check before the first remote is modified: the
// destination each participant publishes to, whether it can be reached, and
// whether its outgoing history contains only paths the repository owns.
func preflight(configuration *config.Config, root string, participants []*participant) error {
	matcher, err := configuration.Matcher()
	if err != nil {
		return err
	}
	if err := remotes(configuration, root, participants); err != nil {
		return err
	}
	if err := reachable(root, participants); err != nil {
		return err
	}
	if err := destinations(root, participants); err != nil {
		return err
	}
	return histories(matcher, root, participants)
}

// remotes requires a configured origin that the repository really uses. A
// separate push URL is refused because preflight would then inspect a
// different destination than the push writes to.
func remotes(configuration *config.Config, root string, participants []*participant) error {
	var issues []string
	for _, current := range participants {
		configured := configuration.Repositories[current.name].Remotes[remoteName]
		url, err := option(root, current.name, "remote."+remoteName+".url")
		if err != nil {
			return err
		}
		switch {
		case configured == "":
			issues = append(issues, invalid(current,
				"has no configured %s remote, so the whole push is refused: add its %s to publish it, "+
					"set \"push: disabled\" to keep it local, or push another repository by name",
				remoteName, remoteName))
			continue
		case url == "":
			issues = append(issues, invalid(current, "is missing the %s remote: run gitone init", remoteName))
			continue
		case url != configured:
			issues = append(issues, invalid(current, "%s is %s, not the configured %s", remoteName, url, configured))
			continue
		}
		fetchURLs, err := remoteURLs(root, current.name, false)
		if err != nil {
			return err
		}
		pushURLs, err := remoteURLs(root, current.name, true)
		if err != nil {
			return err
		}
		switch {
		case len(fetchURLs) != 1:
			issues = append(issues, invalid(current, "%s has %d fetch destinations; exactly one is required", remoteName, len(fetchURLs)))
		case len(pushURLs) != 1:
			issues = append(issues, invalid(current, "%s has %d push destinations; exactly one is required", remoteName, len(pushURLs)))
		case pushURLs[0] != fetchURLs[0]:
			issues = append(issues, invalid(current, "%s pushes to %s instead of %s", remoteName, pushURLs[0], fetchURLs[0]))
		}
	}
	return join(issues)
}

// reachable contacts every participating remote and records the ref the push
// would move. It reads refs only, so no hook and no remote state is touched.
func reachable(root string, participants []*participant) error {
	var issues []string
	for _, current := range participants {
		value, err := remoteReference(root, current.name, current.branch)
		if err != nil {
			issues = append(issues, failed(current, "is unreachable: %s", detail(err)))
			continue
		}
		current.remote = value
	}
	return join(issues)
}

// destinations refuses everything a plain non-force push could not publish:
// an unknown remote position and a branch that would not fast-forward.
func destinations(root string, participants []*participant) error {
	var issues []string
	for _, current := range participants {
		if current.remote == "" {
			continue
		}
		if !known(root, current.name, current.remote) {
			issues = append(issues, failed(current, "%s/%s is at %s, which this repository does not have: fetch it first",
				remoteName, current.branch, short(current.remote)))
			continue
		}
		if !ancestor(root, current.name, current.remote, current.head) {
			issues = append(issues, failed(current, "%s/%s is at %s and would not fast-forward: integrate it using "+guidance.ResolvingDivergedHistory,
				remoteName, current.branch, short(current.remote)))
		}
	}
	return join(issues)
}

// histories validates the outgoing history of every participant against
// current ownership: every path a commit adds or changes and every
// path of the published tree must belong to that repository. A branch the
// remote does not have yet is validated from its root commit on.
// Deletions publish no new content and do not need a current ownership rule.
//
// ponytail: one git process per outgoing commit, batch it into a single log
// walk if the first push of a long history becomes too slow.
func histories(matcher *config.Matcher, root string, participants []*participant) error {
	var issues []string
	for _, current := range participants {
		commits, err := commitList(root, current.name, current.head, current.remote)
		if err != nil {
			return err
		}
		current.commits = len(commits)
		checked := map[string]bool{}
		for _, commit := range commits {
			paths, err := changedPaths(root, current.name, commit)
			if err != nil {
				return err
			}
			issues = append(issues, unowned(matcher, current, paths, checked, "commit "+short(commit))...)
			// The published tree is not enough: a commit that carries an
			// unsafe link enters the remote history even when a later commit
			// repairs it, so every outgoing tree is inspected.
			refused, err := repository.TreeLinkIssues(matcher, root, current.name, commit)
			if err != nil {
				return err
			}
			for _, entry := range refused {
				if checked[entry.Path+"\x00"+entry.Reason] {
					continue
				}
				checked[entry.Path+"\x00"+entry.Reason] = true
				issues = append(issues, failed(current, "%s in commit %s: %s", entry.Path, short(commit), entry.Reason))
			}
		}
		paths, err := treePaths(root, current.name, current.head)
		if err != nil {
			return err
		}
		issues = append(issues, unowned(matcher, current, paths, checked, "the published tree")...)
	}
	return join(issues)
}

// unowned reports the paths of one location that the repository does not own.
// checked keeps a path that several commits touch from being reported twice.
func unowned(matcher *config.Matcher, current *participant, paths []string, checked map[string]bool, where string) []string {
	var issues []string
	for _, path := range paths {
		if checked[path] {
			continue
		}
		checked[path] = true
		if problem := matcher.Unowned(current.name, path); problem != "" {
			issues = append(issues, failed(current, "%s in %s: %s", path, where, problem))
		}
	}
	return issues
}

// summary prints the one deterministic overview a confirmation answers. It is
// only reached once the complete preflight passed.
func summary(output io.Writer, participants []*participant) {
	style := ui.For(output)
	fmt.Fprintf(output, "%s\n\n", style.Heading.Render("Checking repositories..."))
	for _, current := range participants {
		fmt.Fprintf(output, "%-10s %s\n", strings.ToUpper(current.name), style.Success.Render("✓ reachable"))
	}
	fmt.Fprintf(output, "\n%s\n\n", style.Heading.Render("Validating paths..."))
	for _, line := range []string{"✓ no unassigned files", "✓ no ambiguous paths", "✓ every published path belongs to its repository"} {
		fmt.Fprintln(output, style.Success.Render(line))
	}
	fmt.Fprintf(output, "\n%s\n", style.Heading.Render("Ready to push:"))
	for _, current := range participants {
		fmt.Fprintf(output, "\n%s\n%s\n  branch: %s\n%s\n",
			style.Heading.Render(strings.ToUpper(current.name)), style.Muted.Render("  remote: "+remoteName),
			style.Branch.Render(current.branch), style.Muted.Render(fmt.Sprintf("  commits: %d", current.commits)))
	}
	fmt.Fprintln(output)
}

// confirm asks for the explicit approval a push requires. The default answer
// is no, and an invocation nobody can answer needs --yes instead.
func confirm(yes bool, input io.Reader, output io.Writer) (bool, error) {
	if yes {
		return true, nil
	}
	fmt.Fprint(output, "Continue? [y/N] ")
	answer, err := bufio.NewReader(input).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	if slices.Contains([]string{"y", "yes"}, strings.ToLower(strings.TrimSpace(answer))) {
		return true, nil
	}
	fmt.Fprintf(output, "\nPush aborted.\n\n%s\n", unmodified)
	return false, nil
}

// reference is the commit a local ref points at, or the empty string when it
// does not exist.
func reference(root, name, ref string) (string, error) {
	output, err := git.Run(root, "--git-dir="+repository.Directory(root, name), "for-each-ref", "--format=%(objectname)", ref)
	return strings.TrimSpace(output), err
}

// remoteURLs asks Git for every effective fetch or push destination, including
// URL rewrites such as pushInsteadOf.
func remoteURLs(root, name string, push bool) ([]string, error) {
	arguments := []string{"--git-dir=" + repository.Directory(root, name), "remote", "get-url"}
	if push {
		arguments = append(arguments, "--push")
	}
	output, err := git.Run(root, append(arguments, "--all", remoteName)...)
	if err != nil {
		return nil, err
	}
	return strings.Split(strings.TrimSpace(output), "\n"), nil
}

// remoteReference is the commit the remote branch points at, or the empty
// string when the remote does not have that branch yet.
func remoteReference(root, name, branch string) (string, error) {
	output, err := git.Run(root, "--git-dir="+repository.Directory(root, name),
		"ls-remote", remoteName, "refs/heads/"+branch)
	if err != nil {
		return "", err
	}
	value, _, _ := strings.Cut(strings.TrimSpace(output), "\t")
	return value, nil
}

// option reads one Git configuration value, which is empty when it is unset.
func option(root, name, key string) (string, error) {
	output, err := git.Run(root, "--git-dir="+repository.Directory(root, name), "config", "--default", "", "--get", key)
	return strings.TrimSpace(output), err
}

func countCommits(root, name, head, base string) (int, error) {
	output, err := git.Run(root, append(revisions(root, name, head, base), "--count")...)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(output))
}

// commitList are the outgoing commits, newest first. Without a known remote
// base the complete reachable history is returned.
func commitList(root, name, head, base string) ([]string, error) {
	output, err := git.Run(root, revisions(root, name, head, base)...)
	if err != nil {
		return nil, err
	}
	return strings.Fields(output), nil
}

func revisions(root, name, head, base string) []string {
	arguments := []string{"--git-dir=" + repository.Directory(root, name), "rev-list", head}
	if base != "" {
		arguments = append(arguments, "--not", base)
	}
	return arguments
}

// changedPaths are the paths one commit adds or changes, including every side
// of a merge and the full content of a root commit. The lowercase d of
// --diff-filter=d excludes deletions, and a rename arrives here as a deletion
// plus an addition: the destination stays checked, the deleted source drops
// out. Content an outgoing commit adds is still checked; content the remote
// already has is not checked again.
func changedPaths(root, name, commit string) ([]string, error) {
	output, err := git.Run(root, "--git-dir="+repository.Directory(root, name),
		"diff-tree", "-r", "-m", "--root", "--diff-filter=d", "--no-commit-id", "--name-only", "-z", commit)
	if err != nil {
		return nil, err
	}
	return split(output), nil
}

// treePaths are the paths the pushed commit publishes.
func treePaths(root, name, commit string) ([]string, error) {
	output, err := git.Run(root, "--git-dir="+repository.Directory(root, name),
		"ls-tree", "-r", "--full-tree", "--name-only", "-z", commit)
	if err != nil {
		return nil, err
	}
	return split(output), nil
}

// known reports whether the repository has the object a remote ref names.
func known(root, name, commit string) bool {
	_, err := git.Run(root, "--git-dir="+repository.Directory(root, name), "cat-file", "-e", commit+"^{commit}")
	return err == nil
}

// ancestor reports whether publishing head only fast-forwards the remote.
func ancestor(root, name, remote, head string) bool {
	_, err := git.Run(root, "--git-dir="+repository.Directory(root, name), "merge-base", "--is-ancestor", remote, head)
	return err == nil
}

func invalid(current *participant, format string, arguments ...any) string {
	return fmt.Sprintf("%s repository %q %s", repositoryInvalid, current.name, fmt.Sprintf(format, arguments...))
}

func failed(current *participant, format string, arguments ...any) string {
	return fmt.Sprintf("%s repository %q: %s", preflightFailed, current.name, fmt.Sprintf(format, arguments...))
}

// join turns the collected preflight problems into one error. The closing line
// is part of it because a failed preflight never modified a remote.
func join(issues []string) error {
	if len(issues) == 0 {
		return nil
	}
	return errors.New(strings.Join(append(issues, unmodified), "\n"))
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

func split(output string) []string {
	return slices.DeleteFunc(strings.Split(output, "\x00"), func(entry string) bool { return entry == "" })
}
