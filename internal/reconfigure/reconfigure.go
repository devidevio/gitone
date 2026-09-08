// Package reconfigure makes the managed Git repositories match the effective
// configuration. It is the only command that creates a managed repository the
// configuration added or changes a configured remote: every other command
// reads the repositories and remotes native Git already holds, so a
// configured repository or URL native Git does not have is a dead end until
// this command reconciles it.
//
// The configuration in the working tree is the source and the repositories
// are made to match it. Nothing is applied before the complete plan was
// previewed and accepted once, no remote is contacted before its exact old
// and new URL were shown, and a failure afterwards either restores every URL
// this command changed or leaves recovery state for gitone recover and
// gitone abort.
package reconfigure

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/devidevio/gitone/internal/assign"
	"github.com/devidevio/gitone/internal/config"
	"github.com/devidevio/gitone/internal/fetch"
	"github.com/devidevio/gitone/internal/git"
	"github.com/devidevio/gitone/internal/lock"
	"github.com/devidevio/gitone/internal/policy"
	"github.com/devidevio/gitone/internal/repository"
	"github.com/devidevio/gitone/internal/status"
	"github.com/devidevio/gitone/internal/ui"
	"github.com/devidevio/gitone/internal/worktree"
)

const (
	// Command is the recorded command of an interrupted reconfiguration.
	Command = "gitone reconfigure"

	// AcceptFlag is the explicit approval of exactly the previewed repository
	// reconfiguration. It accepts no other class of failure and is harmless
	// when there is nothing to reconcile.
	AcceptFlag = "--accept-repository-change"

	reconfigureFailed = "RECONF001"
	recoveryRequired  = "REC001"

	// unmodified closes every message that stopped before the first remote
	// changed, and every one whose remote changes were restored.
	unmodified = "No repository was created or renamed and no configured remote, branch, HEAD, index or working-tree file was changed."

	// fetched is added whenever a fetch already ran. Remote-tracking refs are
	// a cache and are never rolled back, exactly as after a failed pull.
	fetched = " The remote-tracking refs this reconfiguration fetched remain updated."
)

// change is one configured remote whose URL native Git does not have. before
// is empty while native Git has no such remote at all, which is added instead
// of re-pointed.
type change struct {
	repository string
	remote     string
	before     string
	after      string
	holder     bool
}

// nativeRemote is the complete state needed to distinguish a missing remote
// from one that exists without a URL.
type nativeRemote struct {
	exists bool
	url    string
}

// foreign is a remote native Git has and the configuration does not name. It
// is reported and never applied: git remote remove also deletes that remote's
// tracking refs, and no GitOne command uses an unconfigured remote.
type foreign struct {
	repository string
	remote     string
	url        string
	command    string
}

// target is one repository the reconfiguration fetches once the remotes are
// in place, and the branch position its new origin has to contain.
type target struct {
	name   string
	branch string
	head   string
	url    string
}

// plan is everything one reconfiguration would do, computed from the
// projected configuration before the first remote is contacted.
type plan struct {
	// starting is the one branch every repository of the project is on now,
	// and branch the one it ends on. They differ only for --switch.
	starting string

	// branch is also the branch every added repository is created and checked
	// out on: every repository of a project must agree on one branch.
	branch      string
	changes     []change
	additions   []*addition
	renames     []*rename
	retirements []*retirement
	foreign     []foreign
	fetching    []target
	states      []repositoryState

	// source is where the configuration came from, moves the repositories an
	// incoming source advances or checks out, incoming the accepted
	// .gitone.yml bytes those moves materialize, added the ownership
	// generated for the new paths they carry, and public the paths a private
	// repository owns today and a public one owns afterwards.
	source   Source
	moves    []*movement
	incoming []byte
	added    *assign.Assignment
	public   []string
}

// pending reports whether anything has to be reconciled or moved at all.
func (p *plan) pending() bool {
	return p.structural() || p.moving()
}

// structural reports whether the repository set or a configured remote
// changes, which is the class --accept-repository-change answers.
func (p *plan) structural() bool {
	return len(p.changes) != 0 || len(p.additions) != 0 || len(p.renames) != 0 || len(p.retirements) != 0
}

// moving reports whether any existing repository advances or is checked out.
func (p *plan) moving() bool {
	return slices.ContainsFunc(p.moves, func(current *movement) bool { return current.target != "" })
}

// planned is the commit every moving repository ends at, which is the tree
// set the projected configuration has to validate and the position a
// re-pointed origin has to contain.
func (p *plan) planned() map[string]string {
	commits := map[string]string{}
	for _, current := range p.moves {
		if current.target != "" {
			commits[current.finalName()] = current.target
		}
	}
	return commits
}

// adopt points the plan at the accepted source: every fetch is validated
// against the commit the reconfiguration leaves checked out rather than
// against the one the repository is on now, because that is the position the
// new origin has to contain.
func (p *plan) adopt(source Source, moves []*movement, incoming []byte) {
	p.source, p.moves, p.incoming = source, moves, incoming
	planned := p.planned()
	for index, entry := range p.fetching {
		if commit := planned[entry.name]; commit != "" {
			p.fetching[index].head = commit
		}
		for _, current := range moves {
			if current.finalName() == entry.name {
				p.fetching[index].branch = current.branch
			}
		}
	}
}

// Reconfigure makes the managed repositories match the configuration source
// asks for: the working tree, the incoming commits of --pull, or the target
// branch of --switch. declared names the repositories one configured name
// replaces another for, which GitOne never infers by itself. accepted
// approves the previewed classes in advance and is required when no
// interactive terminal can answer their questions.
func Reconfigure(configuration *config.Config, root string, source Source, declared []Rename,
	accepted Accepted, interactive bool, input io.Reader, output io.Writer) error {
	release, err := lock.Acquire(root, Command)
	if err != nil {
		return err
	}
	defer release()

	projected, result, inventory := configuration, (*status.Status)(nil), (*worktree.Result)(nil)
	var reviewed *policy.Policy
	var moves []*movement
	if source.incoming() {
		// A configuration that arrives from repository content is read only
		// once the project already agrees with the file it has: repository
		// metadata, the managed working tree and one shared branch. Creating
		// what a local file already asks for is a plain reconfiguration.
		if result, inventory, err = status.Validate(configuration, root); err != nil {
			return err
		}
		if moves, reviewed, projected, err = source.sourced(configuration, root, result, inventory); err != nil {
			return err
		}
		if err := holder(configuration, projected); err != nil {
			return err
		}
	}
	// A password in a configured URL is refused before anything is planned:
	// the file holding it is committed to a repository other people clone, so
	// writing it into native Git is a credential disclosure by construction.
	if err := credentials(projected, root); err != nil {
		return err
	}
	// A repository the configuration no longer names is planned before a
	// local project is validated: the paths it holds are exactly what that
	// validation would otherwise report as unassigned without naming the
	// retirement that caused it or the repairs that resolve it.
	retirements, err := removed(projected, root)
	if err != nil {
		return err
	}
	// A declared rename takes its repository back out of that plan: the name
	// disappeared and another appeared, but the repository is the same one and
	// moving its metadata keeps everything a retirement plus an addition would
	// have to fetch again.
	renames, retirements, err := renamed(projected, root, declared, retirements)
	if err != nil {
		return err
	}
	if err := retirable(root, retirements); err != nil {
		return err
	}
	if !source.incoming() {
		// A configured repository without metadata is the one problem this
		// command resolves itself rather than refusing.
		if result, inventory, err = status.ValidateMissingRepositories(configuration, root); err != nil {
			return err
		}
	}

	// A repository the projected configuration retires does not move: its
	// metadata is put aside exactly as it is, and validating its tree against
	// a configuration that no longer names it would refuse every path it
	// holds. A renamed one moves under the name its directory gets, because
	// the rename is the first write of the plan.
	moves = slices.DeleteFunc(moves, func(current *movement) bool {
		return slices.ContainsFunc(retirements, func(entry *retirement) bool { return entry.name == current.name })
	})
	for _, entry := range renames {
		for _, current := range moves {
			if current.name == entry.from.name {
				current.to = entry.to
			}
		}
	}
	// The incoming trees are validated against the projected policy before
	// anything is previewed, and the entries a moving repository newly
	// introduces without an owner become the ownership an assignment offers.
	reviewed, treeSources := renamedTrees(reviewed, renames)
	unassigned, err := treeIssues(reviewed, root, moves, treeSources, inventory)
	if err != nil {
		return err
	}
	added, err := assign.ProposeFrom(command(source), reviewed, root, unassigned, inventory.Tracked, treeSources)
	if err != nil {
		return err
	}
	starting := current(result, projected, renames)
	branch := starting
	if source.Branch != "" {
		branch = source.Branch
	}
	computed, err := compute(projected, root, starting, branch, result, inventory, retirements, renames)
	if err != nil {
		return err
	}
	if err := renamable(projected, root, computed); err != nil {
		return err
	}
	computed.adopt(source, moves, reviewed.Projected())
	computed.added = added
	if computed.public, err = exposed(configuration, projected, inventory); err != nil {
		return err
	}

	computed.reportForeign(output)
	if !computed.pending() && !reviewed.Changed() && computed.added == nil {
		fmt.Fprintf(output, "Nothing to reconcile: %s\n", source.settled())
		return nil
	}

	// One ordered review: the configuration, the ignore rules and the local
	// paths they affect first, then the repository reconfiguration, then the
	// ownership generated for the new paths. Each class keeps its own
	// question and its own flag, and no flag accepts another one's class.
	if reviewed.Changed() {
		reviewed.Preview(output)
	}
	if computed.structural() || len(computed.public) != 0 {
		computed.preview(output)
	}
	computed.added.Preview(output)
	// Every question reads from the same buffered reader, so answering one
	// does not swallow the answer to the next.
	answers := bufio.NewReader(input)
	confirmed, err := reviewed.Confirm(accepted.Config, interactive, answers, output)
	if err != nil || !confirmed {
		return err
	}
	if computed.structural() {
		confirmed, err = policy.Ask(command(source), "a repository reconfiguration",
			"Accept this repository reconfiguration?", AcceptFlag, accepted.Repository, interactive, answers, output)
		if err != nil || !confirmed {
			return err
		}
	}
	if confirmed, err = computed.added.Confirm(command(source), accepted.Paths, interactive, answers, output); err != nil || !confirmed {
		return err
	}
	return ui.SpinBuffered(input, output, "Reconfiguring repositories", func(rendered io.Writer) error {
		return apply(projected, root, computed, inventory, rendered)
	})
}

// renamedTrees checks a declared rename under its projected owner name while
// reading the commit from the metadata directory that still has the old name.
func renamedTrees(reviewed *policy.Policy, renames []*rename) (*policy.Policy, map[string]string) {
	if reviewed == nil || len(renames) == 0 {
		return reviewed, nil
	}
	result := *reviewed
	result.Commits = maps.Clone(reviewed.Commits)
	sources := map[string]string{}
	for _, entry := range renames {
		commit, exists := result.Commits[entry.from.name]
		if !exists {
			continue
		}
		delete(result.Commits, entry.from.name)
		result.Commits[entry.to] = commit
		sources[entry.to] = entry.from.name
	}
	return &result, sources
}

// settled is what a source found nothing to do about, in its own words.
func (s Source) settled() string {
	if s.incoming() {
		return "every configured repository exists, every configured remote is the one native Git has, and no repository moves."
	}
	return "every configured repository exists and every configured remote is the one native Git has."
}

// command describes the reconfiguration to the shared confirmation, the
// shared policy review and the shared path assignment, which is the same
// review every other command runs. Reconfigure names no further command: it
// is the one that applies a structural change rather than refusing it.
func command(source Source) policy.Command {
	return policy.Command{
		Code:       reconfigureFailed,
		Retry:      source.name(),
		Repair:     "Repair or revert the incoming " + config.PublicFile + " and run " + source.name() + " again.",
		Unmodified: unmodified,
		Aborted:    "Reconfiguration aborted.",
	}
}

// credentials refuses every configured remote URL that embeds a password.
// The whole effective configuration is checked, not only the remotes that
// change: this command guarantees that every configured remote exists with
// exactly the configured URL, and it can never guarantee that for a URL
// nobody may write down.
func credentials(configuration *config.Config, root string) error {
	var issues []string
	for _, name := range configuration.RepositoryNames() {
		configured := configuration.Repositories[name]
		for _, remoteName := range configured.RemoteNames() {
			if !password(configured.Remotes[remoteName]) {
				continue
			}
			file, err := config.RemoteFile(root, name, remoteName)
			if err != nil {
				return err
			}
			issues = append(issues, fmt.Sprintf(
				"%s repository %q remote %q has a password in its configured URL: remove it from %s and use a credential helper or an SSH key",
				reconfigureFailed, name, remoteName, file))
		}
	}
	if len(issues) == 0 {
		return nil
	}
	return errors.New(strings.Join(append(issues, unmodified), "\n"))
}

// password reports whether url embeds a password. Only the scheme form can
// carry one; a scp-like git@host:path has no place to put it, and a bare
// user@host without a colon is what SSH normally looks like.
func password(url string) bool {
	_, rest, found := strings.Cut(url, "://")
	if !found {
		return false
	}
	authority, _, _ := strings.Cut(rest, "/")
	at := strings.LastIndex(authority, "@")
	return at >= 0 && strings.Contains(authority[:at], ":")
}

// compute builds the complete plan from the effective configuration: every
// configured repository the project does not have yet, and for every
// repository it does have, the URL native Git holds for each configured
// remote name versus the URL the configuration says.
func compute(configuration *config.Config, root, starting, branch string, result *status.Status,
	inventory *worktree.Result, retirements []*retirement, renames []*rename) (*plan, error) {
	matcher, err := configuration.Matcher()
	if err != nil {
		return nil, err
	}
	holder, _, _ := matcher.Owner(config.PublicFile)

	branches := map[string]string{}
	for _, reported := range result.Repositories {
		branches[reported.Name] = reported.Branch
	}
	computed := &plan{starting: starting, branch: branch, retirements: retirements, renames: renames}
	// A renamed repository is read under the name its metadata still has, and
	// is neither created nor retired: everything it holds moves with the
	// directory.
	sources := map[string]string{}
	for _, entry := range renames {
		sources[entry.to] = entry.from.name
		branches[entry.to] = entry.from.branch
	}
	if computed.additions, err = added(configuration, root, computed.branch, inventory); err != nil {
		return nil, err
	}
	computed.additions = slices.DeleteFunc(computed.additions, func(entry *addition) bool { return sources[entry.name] != "" })
	created := map[string]*addition{}
	for _, entry := range computed.additions {
		created[entry.name] = entry
		branches[entry.name] = entry.branch
	}

	for _, name := range configuration.RepositoryNames() {
		configured := configuration.Repositories[name]
		branch := branches[name]
		existing := map[string]nativeRemote{}
		head := ""
		// An added repository has no native Git state to compare against: it
		// is created with exactly the configured remotes, on this branch and
		// without a commit of its own.
		if created[name] == nil {
			metadata := name
			if from := sources[name]; from != "" {
				metadata = from
			}
			if existing, err = remotes(root, metadata); err != nil {
				return nil, err
			}
			if head, err = headOf(root, metadata, branch); err != nil {
				return nil, err
			}
		}

		saved := repositoryState{Name: name, Branch: branch, Head: head, Created: created[name] != nil}
		for _, remoteName := range configured.RemoteNames() {
			before, after := existing[remoteName], configured.Remotes[remoteName]
			entry := remoteState{Name: remoteName, Before: before.url, BeforeExists: before.exists}
			if !before.exists || before.url != after {
				entry.After = after
				// The remotes of an added repository are part of its creation,
				// not a reviewed change of an existing repository.
				if created[name] == nil {
					computed.changes = append(computed.changes, change{repository: name, remote: remoteName,
						before: before.url, after: after, holder: name == holder && remoteName == config.OriginRemote})
				}
			}
			saved.Remotes = append(saved.Remotes, entry)
		}
		for _, remoteName := range slices.Sorted(maps.Keys(existing)) {
			if configured.Remotes[remoteName] != "" {
				continue
			}
			computed.foreign = append(computed.foreign, foreign{repository: name, remote: remoteName, url: existing[remoteName].url,
				command: fmt.Sprintf("git --git-dir=%s remote remove %s", repository.Directory(root, name), remoteName)})
		}
		if url := configured.Remotes[config.OriginRemote]; url != "" {
			computed.fetching = append(computed.fetching, target{name: name, branch: branch, head: head, url: url})
		}
		computed.states = append(computed.states, saved)
	}
	for _, entry := range renames {
		entry.changed = slices.ContainsFunc(computed.changes, func(current change) bool { return current.repository == entry.to })
	}
	return computed, nil
}

// remotes is every remote native Git has for one repository, with its URL.
func remotes(root, name string) (map[string]nativeRemote, error) {
	listed, err := git.Run(root, "--git-dir="+repository.Directory(root, name), "remote")
	if err != nil {
		return nil, err
	}
	existing := map[string]nativeRemote{}
	for _, remoteName := range strings.Fields(listed) {
		url, err := configuredRemoteURL(root, name, remoteName)
		if err != nil {
			return nil, err
		}
		existing[remoteName] = nativeRemote{exists: true, url: url}
	}
	return existing, nil
}

func readRemote(root, name, remoteName string) (nativeRemote, error) {
	existing, err := remotes(root, name)
	if err != nil {
		return nativeRemote{}, err
	}
	return existing[remoteName], nil
}

// configuredRemoteURL reads an optional URL without hiding operational Git
// errors. Git returns the supplied default only when the key is absent.
func configuredRemoteURL(root, name, remoteName string) (string, error) {
	existing, err := git.Run(root, "--git-dir="+repository.Directory(root, name),
		"config", "--default", "", "--get", "remote."+remoteName+".url")
	return strings.TrimSpace(existing), err
}

// current is the branch the project is on. Every reported repository shares
// it; a project whose repositories all still have to be created starts on the
// configured default branch.
func current(result *status.Status, configuration *config.Config, renames []*rename) string {
	for _, reported := range result.Repositories {
		return reported.Branch
	}
	// A renamed repository is not reported: the configuration no longer names
	// the name its metadata has. It is still on the branch the project is on.
	for _, entry := range renames {
		if entry.from.branch != "" {
			return entry.from.branch
		}
	}
	return configuration.DefaultBranch
}

// headOf is the commit the current branch of one repository points at, and the
// empty string while the repository has no commit on it or no branch at all.
func headOf(root, name, branch string) (string, error) {
	if branch == "" || branch == status.Detached {
		return "", nil
	}
	return repository.Reference(root, name, "refs/heads/"+branch)
}

// reportForeign names every remote native Git has that the configuration does
// not, with the exact command that would drop it. It is printed whether or
// not anything is reconciled and never asks a question.
func (p *plan) reportForeign(output io.Writer) {
	if len(p.foreign) == 0 {
		return
	}
	style := ui.For(output)
	fmt.Fprintf(output, "%s\n\n", style.Heading.Render("Remotes the configuration does not name:"))
	for _, current := range p.foreign {
		fmt.Fprintf(output, "  %s / %s  %s\n", current.repository, current.remote, current.url)
		fmt.Fprintf(output, "    left in place. No GitOne command uses it. Remove it yourself with:\n")
		fmt.Fprintf(output, "      %s\n", current.command)
	}
	fmt.Fprintln(output)
}

// preview is the one ordered block printed in full before the single
// question: what is created, what changes, and which repositories are
// contacted afterwards.
func (p *plan) preview(output io.Writer) {
	style := ui.For(output)
	fmt.Fprintf(output, "%s\n\n", style.Heading.Render("Repository reconfiguration"))
	if len(p.additions) != 0 || len(p.renames) != 0 || len(p.retirements) != 0 {
		fmt.Fprintf(output, "%s\n\n", style.Heading.Render("REPOSITORIES"))
		for _, entry := range p.additions {
			entry.preview(output)
		}
		for _, entry := range p.renames {
			entry.preview(output)
		}
		for _, entry := range p.retirements {
			entry.preview(output)
		}
		fmt.Fprintln(output)
	}
	if len(p.changes) == 0 {
		previewExposed(output, p.public)
		p.network(output, style)
		return
	}
	fmt.Fprintf(output, "%s\n\n", style.Heading.Render("REMOTES"))
	for _, current := range p.changes {
		fmt.Fprintf(output, "  %s / %s\n", current.repository, current.remote)
		fmt.Fprintf(output, "    before: %s\n", or(current.before))
		fmt.Fprintf(output, "    after:  %s\n", current.after)
		if line := endpoint(current.before, current.after); line != "" {
			fmt.Fprintf(output, "    %s\n", line)
		}
		if current.holder {
			fmt.Fprintf(output, "    this repository owns %s, so new clones must use the new URL\n", config.PublicFile)
		}
	}
	fmt.Fprintln(output)
	previewExposed(output, p.public)
	p.network(output, style)
}

// network is the last preview section: every repository this reconfiguration
// contacts once the plan was accepted.
func (p *plan) network(output io.Writer, style ui.Styles) {
	fmt.Fprintf(output, "%s\n\n", style.Heading.Render("NETWORK"))
	if len(p.fetching) == 0 {
		fmt.Fprintf(output, "  no repository has a configured %s, so nothing is fetched\n", config.OriginRemote)
	}
	for _, current := range p.fetching {
		fmt.Fprintf(output, "  %s will be fetched from %s\n", current.name, current.url)
	}
	fmt.Fprintln(output)
}

// endpoint names the one difference that decides whether the new URL is
// contacted like the old one: a changed host, or a changed scheme on the same
// host. It is empty while both point at the same endpoint.
func endpoint(before, after string) string {
	beforeScheme, beforeHost := parse(before)
	afterScheme, afterHost := parse(after)
	const asked = "your credential helper and SSH configuration will be asked for it"
	switch {
	case before == "" && afterHost != "":
		return fmt.Sprintf("first use of %s; %s", afterHost, asked)
	case before == "":
		return ""
	case beforeHost != afterHost:
		return fmt.Sprintf("host changes %s -> %s; %s", or(beforeHost), or(afterHost), asked)
	case beforeScheme != afterScheme:
		return fmt.Sprintf("scheme changes %s -> %s; %s", or(beforeScheme), or(afterScheme), asked)
	}
	return ""
}

// parse is the scheme and the host of a remote URL. A scp-like git@host:path
// has no scheme, and a local path has neither.
func parse(url string) (scheme, host string) {
	authority, rest, found := "", "", false
	if scheme, rest, found = strings.Cut(url, "://"); found {
		authority, _, _ = strings.Cut(rest, "/")
	} else {
		scheme = ""
		if authority, _, found = strings.Cut(url, ":"); !found || strings.Contains(authority, "/") {
			return "", ""
		}
	}
	if at := strings.LastIndex(authority, "@"); at >= 0 {
		authority = authority[at+1:]
	}
	return scheme, authority
}

// refresh fetches every repository that has a configured origin, which is the
// first contact with a URL the preview has just been accepted for.
func refresh(root string, targets []target, record func() error) error {
	for _, current := range targets {
		if err := fetch.Update(root, current.name); err != nil {
			return fmt.Errorf("%s repository %q could not be fetched from %s: %s",
				reconfigureFailed, current.name, current.url, detail(err))
		}
		if err := record(); err != nil {
			return err
		}
	}
	return nil
}

// reachable requires every repository to still fast-forward from the remote
// it now points at. A new URL whose history does not contain the commit the
// repository is on is a different project, and re-pointing at it would strand
// the local history instead of publishing it.
func reachable(root string, targets []target) error {
	var issues []string
	for _, current := range targets {
		if current.head == "" {
			continue
		}
		upstream, err := remoteReference(root, current.name, current.branch)
		if err != nil {
			return fmt.Errorf("%s repository %q branch %s could not be read from %s: %s",
				reconfigureFailed, current.name, current.branch, current.url, detail(err))
		}
		// A branch the new remote does not have yet is the ordinary state of
		// a branch nobody pushed there; only a history that exists and does
		// not contain the local commit is an unrelated one.
		if upstream == "" || ancestor(root, current.name, current.head, upstream) {
			continue
		}
		issues = append(issues, fmt.Sprintf(
			"%s repository %q is on %s at %s, which %s/%s at %s does not contain: %s holds an unrelated history",
			reconfigureFailed, current.name, current.branch, short(current.head),
			config.OriginRemote, current.branch, short(upstream), current.url))
	}
	if len(issues) == 0 {
		return nil
	}
	return errors.New(strings.Join(issues, "\n"))
}

// remoteReference reads the branch from the accepted origin itself. A local
// tracking ref may still belong to the previous origin when the new one does
// not have this branch.
func remoteReference(root, name, branch string) (string, error) {
	output, err := git.Run(root, "--git-dir="+repository.Directory(root, name),
		"ls-remote", config.OriginRemote, "refs/heads/"+branch)
	if err != nil {
		return "", err
	}
	value, _, _ := strings.Cut(strings.TrimSpace(output), "\t")
	return value, nil
}

func ancestor(root, name, commit, other string) bool {
	_, err := git.Run(root, "--git-dir="+repository.Directory(root, name), "merge-base", "--is-ancestor", commit, other)
	return err == nil
}

// report states what the reconfiguration left behind, one line per created
// repository, one per changed remote and one per fetched repository.
func report(output io.Writer, current *plan) {
	style := ui.For(output)
	fmt.Fprintf(output, "%s\n\n", style.Heading.Render("Reconfiguration results:"))
	reportAdditions(output, current.additions, style)
	for _, entry := range current.renames {
		fmt.Fprintln(output, style.Success.Render("✓ "+entry.from.name+" "+entry.line()))
	}
	for _, entry := range current.changes {
		fmt.Fprintln(output, style.Success.Render(fmt.Sprintf("✓ %s / %s now points at %s", entry.repository, entry.remote, entry.after)))
	}
	for _, entry := range current.fetching {
		fmt.Fprintln(output, style.Muted.Render(fmt.Sprintf("  %s fetched from %s", entry.name, entry.url)))
	}
	for _, entry := range current.retirements {
		fmt.Fprintln(output, style.Success.Render("✓ "+entry.name+" "+entry.line()))
	}
	reportUnborn(output, current.additions)
}

// or renders an absent URL, host or scheme as the value a person compares
// against instead of as an empty column.
func or(value string) string {
	if value == "" {
		return "(absent)"
	}
	return value
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
