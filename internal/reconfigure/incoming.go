package reconfigure

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/devidevio/gitone/internal/config"
	"github.com/devidevio/gitone/internal/fetch"
	"github.com/devidevio/gitone/internal/git"
	"github.com/devidevio/gitone/internal/policy"
	"github.com/devidevio/gitone/internal/pull"
	"github.com/devidevio/gitone/internal/repository"
	"github.com/devidevio/gitone/internal/status"
	"github.com/devidevio/gitone/internal/switching"
	"github.com/devidevio/gitone/internal/ui"
	"github.com/devidevio/gitone/internal/worktree"
)

const (
	// PullFlag takes the configuration from the fast-forward target of the
	// repository whose tree carries .gitone.yml. The project stays on its
	// branch.
	PullFlag = "--pull"

	// SwitchFlag takes the configuration from a target branch and moves every
	// repository to that branch in the same transaction.
	SwitchFlag = "--switch"
)

// Source is where a reconfiguration reads .gitone.yml from. The three forms
// differ in nothing else: the plan, the preview, the confirmations, the
// validation, the transaction and the recovery are identical, so a person who
// has read one form has read them all.
type Source struct {
	// Pull adopts the incoming configuration of the fetched origin.
	Pull bool

	// Branch adopts the configuration that branch carries, and is also the
	// branch every repository ends on.
	Branch string
}

// Accepted is the reviewed classes one run approves in advance, which is what
// a run without an interactive terminal needs. Each flag accepts exactly its
// own class: no flag ever accepts another one's.
type Accepted struct {
	// Repository approves the previewed repository reconfiguration.
	Repository bool

	// Config approves the ordinary incoming policy change of the same source.
	Config bool

	// Paths approves the ownership generated for newly unassigned paths.
	Paths bool
}

// incoming reports whether the configuration comes from repository content
// rather than from the working tree, which is the only case that fetches, that
// moves a repository and that reviews an ordinary policy change.
func (s Source) incoming() bool { return s.Pull || s.Branch != "" }

// name is the command as its refusals, its retry hints and its recovery
// journal spell it.
func (s Source) name() string {
	switch {
	case s.Pull:
		return Command + " " + PullFlag
	case s.Branch != "":
		return Command + " " + SwitchFlag + " " + s.Branch
	}
	return Command
}

// movement is one existing repository the reconfiguration moves. from is the
// branch it starts on and head the commit that branch holds; branch is the
// branch it ends on and target the commit it ends at. target stays empty
// while the repository does not move at all.
type movement struct {
	name   string
	to     string
	from   string
	branch string
	head   string
	target string
}

func (m *movement) finalName() string {
	if m.to != "" {
		return m.to
	}
	return m.name
}

// where names the commit an incoming policy came from, in the wording pull and
// switch already use for their own reviews.
func (m *movement) where(source Source) string {
	if source.Pull {
		return config.OriginRemote + "/" + m.branch + " at " + short(m.target)
	}
	return m.branch + " at " + short(m.target)
}

// sourced reads the configuration the source carries together with the commit
// every existing repository would end at, and returns the projected effective
// configuration the whole plan is then computed from.
//
// It is the only phase that contacts a remote before the confirmation, and it
// contacts exclusively the URLs native Git already holds: a new or re-pointed
// URL is first used once the preview naming it was accepted.
func (s Source) sourced(configuration *config.Config, root string, result *status.Status,
	inventory *worktree.Result) ([]*movement, *policy.Policy, *config.Config, error) {
	movements, err := s.movements(configuration, root, result)
	if err != nil {
		return nil, nil, nil, err
	}
	repositories := make([]policy.Repository, 0, len(movements))
	for _, current := range movements {
		entry := policy.Repository{Name: current.name}
		if current.target != "" {
			entry.Head, entry.Target, entry.Source = current.head, current.target, current.where(s)
		}
		repositories = append(repositories, entry)
	}
	// The shared review is the same one pull and switch run, with the same
	// refusals: an unreadable, invalid or no longer singly owned .gitone.yml,
	// an existing local ignored or unassigned path the projected policy would
	// manage, and the ignore diff. Only the structural change it refuses for
	// them is what this command exists to apply.
	reviewed, err := policy.Review(command(s), configuration, root, inventory, repositories)
	if err != nil {
		return nil, nil, nil, err
	}
	projected := configuration
	if contents := reviewed.Projected(); contents != nil {
		if err := collision(root, contents); err != nil {
			return nil, nil, nil, err
		}
		if projected, err = config.Effective(contents, root); err != nil {
			return nil, nil, nil, err
		}
	}
	return movements, reviewed, projected, nil
}

// movements is the commit every existing repository would end at: the fetched
// fast-forward target for --pull, and the commit the target branch holds for
// --switch. It applies exactly the preflight of gitone pull and gitone switch
// and reports every problem at once, before the first local write.
func (s Source) movements(configuration *config.Config, root string, result *status.Status) ([]*movement, error) {
	dirty := map[string]bool{}
	for _, change := range result.Changes {
		if change.State == status.Staged || change.State == status.Unstaged {
			dirty[change.Repository] = true
		}
	}
	movements := make([]*movement, 0, len(result.Repositories))
	for _, reported := range result.Repositories {
		head, err := repository.Reference(root, reported.Name, "refs/heads/"+reported.Branch)
		if err != nil {
			return nil, err
		}
		movements = append(movements, &movement{name: reported.Name, from: reported.Branch,
			branch: reported.Branch, head: head})
	}
	if s.Pull {
		if err := s.fetched(configuration, root, movements); err != nil {
			return nil, err
		}
	}

	var issues []string
	for _, current := range movements {
		issue, err := s.destination(root, current, dirty[current.name])
		if err != nil {
			return nil, err
		}
		if issue != "" {
			issues = append(issues, fmt.Sprintf("%s repository %q %s", reconfigureFailed, current.name, issue))
		}
	}
	if len(issues) == 0 {
		return movements, nil
	}
	return nil, errors.New(strings.Join(append(issues, unmodified+closing(s.Pull)), "\n"))
}

// fetched updates every repository from the origin native Git already points
// at, which is the one network access this command performs before its
// preview is accepted.
func (s Source) fetched(configuration *config.Config, root string, movements []*movement) error {
	selected, err := fetch.Select(configuration, "")
	if err != nil {
		return err
	}
	if err := fetch.Check(configuration, root, selected); err != nil {
		return err
	}
	moving := map[string]bool{}
	for _, current := range movements {
		moving[current.name] = true
	}
	var broken []string
	for _, current := range selected {
		if current.Remote == "" || !moving[current.Name] {
			continue
		}
		if err := fetch.Update(root, current.Name); err != nil {
			broken = append(broken, fmt.Sprintf("%s repository %q could not be fetched from %s: %s",
				reconfigureFailed, current.Name, current.Remote, detail(err)))
		}
	}
	if len(broken) == 0 {
		return nil
	}
	return errors.New(strings.Join(append(broken, unmodified+closing(true)), "\n"))
}

// destination decides what one repository does and names the one thing that
// keeps it from doing it. An empty issue and an empty target together mean the
// repository is already where the source wants it.
func (s Source) destination(root string, current *movement, dirty bool) (string, error) {
	if current.from == "" || current.from == status.Detached {
		if s.Pull {
			return "is not on a branch: pulling needs a branch to advance", nil
		}
		return "is not on a branch: switching needs a recorded starting branch", nil
	}
	if s.Pull {
		target, issue, err := pull.Plan(root, current.name, current.branch, current.head, dirty)
		current.target = target
		return issue, err
	}
	target, err := repository.Reference(root, current.name, "refs/heads/"+s.Branch)
	if err != nil {
		return "", err
	}
	changes := ""
	if dirty {
		changes = "local changes"
	}
	moving, issue, err := switching.Plan(root, current.name, current.from, s.Branch, target, changes)
	current.branch = s.Branch
	if moving {
		current.target = target
	}
	return issue, err
}

// collision refuses an incoming repository whose name .gitone.local.yml
// already defines. Accepting it would silently attach a remote-controlled
// remote and visibility to a repository defined privately, and the merged
// configuration names both the same way, so only the file each name comes
// from can tell them apart.
func collision(root string, contents []byte) error {
	local, err := os.ReadFile(filepath.Join(root, config.LocalFile))
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err != nil:
		return err
	}
	defined, err := config.DefinedNames(config.LocalFile, local)
	if err != nil {
		return err
	}
	if len(defined) == 0 {
		return nil
	}
	current, err := os.ReadFile(filepath.Join(root, config.PublicFile))
	if err != nil {
		return err
	}
	existing, err := config.DefinedNames(config.PublicFile, current)
	if err != nil {
		return err
	}
	arriving, err := config.DefinedNames(config.PublicFile, contents)
	if err != nil {
		return err
	}

	var issues []string
	for _, name := range arriving {
		if slices.Contains(existing, name) || !slices.Contains(defined, name) {
			continue
		}
		issues = append(issues, fmt.Sprintf(
			"%s the incoming %s adds repository %q, which %s already defines: rename one of them, because accepting it would attach a remote-controlled remote and visibility to a repository defined locally",
			reconfigureFailed, config.PublicFile, name, config.LocalFile))
	}
	if len(issues) == 0 {
		return nil
	}
	return errors.New(strings.Join(append(issues, unmodified), "\n"))
}

// holder refuses a configuration that moves .gitone.yml into another
// repository's tree. That file is the anchor of the whole review and of
// gitone clone, so which repository owns it is never changed as a side effect
// of anything else.
func holder(configuration, projected *config.Config) error {
	if configuration == projected {
		return nil
	}
	current, err := configuration.Matcher()
	if err != nil {
		return err
	}
	after, err := projected.Matcher()
	if err != nil {
		return err
	}
	before, _, _ := current.Owner(config.PublicFile)
	owner, _, _ := after.Owner(config.PublicFile)
	if before == owner {
		return nil
	}
	return fmt.Errorf("%s the incoming configuration moves %s from repository %q to repository %q: %s anchors this review and gitone clone, so no flag ever accepts moving it\n%s",
		reconfigureFailed, config.PublicFile, before, owner, config.PublicFile, unmodified)
}

// exposed is every path a private repository owns today and a public one owns
// in the projected configuration. It is the change most worth reading twice,
// so it gets its own preview section instead of one line among the field
// differences.
func exposed(configuration, projected *config.Config, inventory *worktree.Result) ([]string, error) {
	if configuration == projected {
		return nil, nil
	}
	after, err := projected.Matcher()
	if err != nil {
		return nil, err
	}
	var moved []string
	for _, path := range slices.Sorted(maps.Keys(inventory.Owners)) {
		before := inventory.Owners[path]
		if configuration.Repositories[before].Visibility != config.Private {
			continue
		}
		owner, issue, _ := after.Owner(path)
		if issue != config.OwnerValid || projected.Repositories[owner].Visibility != config.Public {
			continue
		}
		moved = append(moved, fmt.Sprintf("  %s   %s -> %s", ui.Safe(path), before, owner))
	}
	return moved, nil
}

// previewExposed prints the private-to-public ownership moves as their own
// section, between the repository plan and the network it will contact.
func previewExposed(output io.Writer, moved []string) {
	if len(moved) == 0 {
		return
	}
	style := ui.For(output)
	fmt.Fprintf(output, "%s\n\n", style.Heading.Render("OWNERSHIP MOVED TO PUBLIC"))
	for _, line := range moved {
		fmt.Fprintln(output, line)
	}
	fmt.Fprintln(output)
}

// treeIssues validates the trees the accepted source would materialize
// against the projected policy and refuses every entry a repository must not
// carry: an unsafe symbolic link, a reserved or protected path, and a path
// another repository owns - which is what a one-sided ownership move looks
// like, named with the repository still carrying it and the one that owns it.
//
// An entry that is only unassigned and that a moving repository newly
// introduces is returned instead of refused: that ownership is deterministic
// and is what an assisted assignment offers. One hard failure reports the
// unassigned entries with it, because assistance never repairs a project that
// is broken anyway.
func treeIssues(reviewed *policy.Policy, root string, movements []*movement,
	sources map[string]string, inventory *worktree.Result) ([]repository.TreeIssue, error) {
	if reviewed == nil {
		return nil, nil
	}
	refused, err := repository.TreeIssuesFrom(reviewed.Matcher, root, reviewed.Commits, sources)
	if err != nil {
		return nil, fmt.Errorf("%s the incoming trees cannot be read: %s\n%s", reconfigureFailed, detail(err), unmodified)
	}
	moving := map[string]bool{}
	for _, current := range movements {
		moving[current.finalName()] = current.target != ""
	}
	existing := map[string]bool{}
	for _, entry := range inventory.Tracked {
		existing[entry] = true
	}

	// A path that is already tracked was unassigned before this command, and
	// a repository that does not move brings nothing new, so neither is
	// assisted. A path holding a * is not assisted either: as a pattern that
	// character matches inside a segment, so the generated ownership would
	// silently reach further than the one path the commit carries.
	var unassigned []repository.TreeIssue
	hard := false
	for _, entry := range refused {
		if entry.Issue == config.OwnerUnassigned && moving[entry.Repository] &&
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
		issues = append(issues, fmt.Sprintf("%s repository %q must not manage %s from %s: %s",
			reconfigureFailed, entry.Repository, entry.Path, short(reviewed.Commits[entry.Repository]), entry.Reason))
	}
	return nil, errors.New(strings.Join(append(issues, unmodified), "\n"))
}

// closing names the fetched refs a refusal leaves behind, which only a --pull
// updates before it stops.
func closing(fetched bool) string {
	if fetched {
		return " The remote-tracking refs this reconfiguration fetched remain updated."
	}
	return ""
}

// moveStates records every repository the accepted source moves, with the
// position gitone recover and gitone abort restore it to.
func moveStates(moves []*movement) []moveState {
	var states []moveState
	for _, current := range moves {
		if current.target == "" {
			continue
		}
		sourceName := ""
		if current.name != current.finalName() {
			sourceName = current.name
		}
		states = append(states, moveState{Name: current.finalName(), SourceName: sourceName, From: current.from,
			Head: current.head, Branch: current.branch, Target: current.target})
	}
	return states
}

// plannedCommits is the commit every recorded move ends at, which is what the
// projected configuration validates instead of the commit each repository is
// on now.
func plannedCommits(moves []moveState) map[string]string {
	commits := map[string]string{}
	for _, entry := range moves {
		commits[entry.Name] = entry.Target
	}
	return commits
}

// advance moves every repository to the commit the accepted source carries:
// the fast-forward of gitone pull, or the checkout of gitone switch. A
// repository whose index and working tree already hold the target only has
// its ref moved, which is exactly the state an interruption between the files
// and the ref leaves behind.
func advance(root string, moves []moveState, record func() error) error {
	for index, entry := range moves {
		if entry.Moved && repository.Matches(root, entry.Name, entry.Target) {
			continue
		}
		if !entry.Started {
			moves[index].Started = true
			if err := record(); err != nil {
				return err
			}
			entry.Started = true
		}
		var err error
		switch {
		case repository.Matches(root, entry.Name, entry.Target):
			err = settle(root, entry)
		case entry.From == entry.Branch:
			err = pull.Forward(root, entry.Name, entry.Target)
		default:
			err = switching.Checkout(root, entry.Name, entry.Branch, entry.Target)
		}
		if err != nil {
			return fmt.Errorf("%s repository %q could not be moved to %s at %s: %s",
				reconfigureFailed, entry.Name, entry.Branch, short(entry.Target), detail(err))
		}
		moves[index].Moved = true
		if err := record(); err != nil {
			return err
		}
	}
	return nil
}

// settle finishes a move whose index and working tree already hold the target
// commit by moving only the ref. A fast-forward passes the expected old value,
// so a branch that moved meanwhile is refused by Git itself.
func settle(root string, entry moveState) error {
	gitDirectory := "--git-dir=" + repository.Directory(root, entry.Name)
	if entry.From != entry.Branch {
		_, err := git.Run(root, gitDirectory, "symbolic-ref", "HEAD", "refs/heads/"+entry.Branch)
		return err
	}
	head, err := repository.Reference(root, entry.Name, "refs/heads/"+entry.Branch)
	if err != nil || head == entry.Target {
		return err
	}
	_, err = git.Run(root, gitDirectory, "update-ref", "refs/heads/"+entry.Branch, entry.Target, entry.Head)
	return err
}

// restore returns every repository this reconfiguration moved to the branch,
// commit and index it started on, newest first. A repository that no longer
// holds what the reconfiguration left behind is refused instead of
// overwritten: its recovery state is resolved by hand.
func restore(root, directory string, moves []moveState, patched []string) error {
	entries := slices.Clone(moves)
	slices.Reverse(entries)
	for _, entry := range entries {
		if !entry.Started && !entry.Moved {
			continue
		}
		name := moveRepositoryName(root, entry)
		if moveAtStart(root, name, entry) {
			continue
		}
		if !repository.Matches(root, name, entry.Target, patched...) {
			return changedName(root, entry.Name, fmt.Sprintf(
				"its index or tracked working-tree files hold neither %s nor the recorded %s",
				short(entry.Target), short(entry.Head)))
		}
		if err := back(root, directory, name, entry); err != nil {
			return err
		}
	}
	return nil
}

func moveAtStart(root, name string, entry moveState) bool {
	branch, err := git.Run(root, "--git-dir="+repository.Directory(root, name),
		"symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil || strings.TrimSpace(branch) != entry.From {
		return false
	}
	head, err := repository.Reference(root, name, "HEAD")
	if err != nil || head != entry.Head {
		return false
	}
	// The first comparison refreshes a racily-clean index left by Git; the
	// second reads that refreshed state.
	return repository.Matches(root, name, entry.Head) || repository.Matches(root, name, entry.Head)
}

func moveRepositoryName(root string, entry moveState) string {
	if entry.sourceName() == entry.Name {
		return entry.Name
	}
	if _, err := os.Stat(repository.Directory(root, entry.Name)); err == nil {
		return entry.Name
	}
	return entry.sourceName()
}

// back returns one repository to the branch, files and index it started on.
func back(root, directory, name string, entry moveState) error {
	gitDirectory := "--git-dir=" + repository.Directory(root, name)
	if _, err := git.Run(root, gitDirectory, "--work-tree="+root,
		"read-tree", "--dry-run", "-u", "-m", entry.Target, entry.Head); err != nil {
		return fmt.Errorf("%s repository %q cannot be restored without overwriting external changes: %s",
			recoveryRequired, entry.Name, detail(err))
	}
	if entry.From != entry.Branch {
		if _, err := git.Run(root, gitDirectory, "symbolic-ref", "HEAD", "refs/heads/"+entry.From); err != nil {
			return err
		}
	}
	if _, err := git.Run(root, gitDirectory, "--work-tree="+root, "reset", "--hard", "--quiet", entry.Head); err != nil {
		return err
	}
	return repository.RestoreIndex(root, name, indexCopy(directory, entry.Name))
}

// indexCopy names the exact index one repository had before it moved. A
// repository name never starts with a digit, so it never collides with the
// saved configuration files an assignment patches.
func indexCopy(directory, name string) string {
	return filepath.Join(directory, name+".original")
}

// advanced is the commit a recorded fast-forward leaves the compared branch
// at, so a repository this reconfiguration already advanced is not mistaken
// for one that changed outside GitOne. A checkout leaves the starting branch
// exactly where it was and needs no such tolerance.
func advanced(saved *state) map[string]string {
	tolerated := map[string]string{}
	for _, entry := range saved.Moves {
		if entry.From == entry.Branch {
			tolerated[entry.Name] = entry.Target
		}
	}
	return tolerated
}

// line is what one move ended up doing, in the wording both the
// reconfiguration report and a recovery report use.
func (m moveState) line() string {
	if m.From == m.Branch {
		return fmt.Sprintf("advanced %s to %s", m.Branch, short(m.Target))
	}
	return fmt.Sprintf("switched %s → %s at %s", m.From, m.Branch, short(m.Target))
}
