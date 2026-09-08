// Package policy reviews the ownership and ignore policy that incoming
// repository content would put in effect, before a command applies it. The
// effective local configuration is the trust anchor: a .gitone.yml carried by
// a fetched or checked-out commit is untrusted policy input until its
// semantic change has been validated and explicitly accepted.
//
// gitone pull and gitone switch are its two consumers. Both project the
// commit every repository would hold, compare the resulting effective
// configuration with the current one, diff every changed tracked .gitignore,
// and refuse the policy classes no confirmation may accept. Nothing here
// reads the content of a working-tree file.
package policy

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/devidevio/gitone/internal/config"
	"github.com/devidevio/gitone/internal/git"
	"github.com/devidevio/gitone/internal/repository"
	"github.com/devidevio/gitone/internal/ui"
	"github.com/devidevio/gitone/internal/worktree"
)

// AcceptFlag is the explicit approval of a reviewed incoming policy change.
// It accepts nothing else: every other failure of the reviewing command stays
// a failure.
const AcceptFlag = "--accept-config-change"

// The states an existing local path can be in, on either side of a projected
// policy. Every other state is the name of the repository owning the path.
const (
	ignoredState    = "ignored"
	unassignedState = "unassigned"
	relevantState   = "relevant"
)

// Repository is one repository and the commit the reviewed command would
// leave it at. Head is the commit it starts from and Target the commit it
// moves to, both empty while the repository does not move. Source names where
// an incoming policy comes from, such as "origin/main at 4352cdf".
type Repository struct {
	Name   string
	Head   string
	Target string
	Source string
}

// Command is what the reviewing command calls itself in the messages this
// package writes. Pull and switch differ in nothing else.
type Command struct {
	// Code is the failure code every refusal starts with.
	Code string

	// Retry is the command to run again after repairing something locally.
	Retry string

	// Repair closes a refusal caused by incoming repository content, which is
	// the content that has to change.
	Repair string

	// Reconfigure is the gitone reconfigure command that applies an incoming
	// repository or remote reconfiguration, which this command refuses and
	// names instead. It is empty for reconfigure itself, which is the command
	// that applies such a change and therefore never refuses it.
	Reconfigure string

	// Unmodified closes every refusal, naming what was left unchanged.
	Unmodified string

	// Aborted is printed when the confirmation is declined.
	Aborted string

	// Failed records which repository refused, so the command's own report
	// names the same repository its message does.
	Failed func(name, summary string)
}

// Policy is the projected ownership and ignore policy of one command,
// together with everything a person reviews before it is applied. It is built
// before the first branch, index or working-tree file changes.
type Policy struct {
	// Matcher is the path matcher the command would leave behind. It equals
	// the current matcher while no incoming commit replaces .gitone.yml.
	Matcher *config.Matcher

	// Commits is the commit every configured repository would hold, which is
	// the tree set the projected policy has to validate.
	Commits map[string]string

	// holder is the repository whose incoming commits carry .gitone.yml, and
	// the zero value while no repository moves.
	holder    Repository
	command   Command
	projected []byte
	changes   []config.Change
	patches   []string
	moves     []move
}

// Projected is the accepted .gitone.yml the reviewed command would leave
// behind, and nil while no incoming commit replaces the file. It is the base
// a locally generated configuration patch has to extend, so an addition is
// never written on top of the pre-command version.
func (p *Policy) Projected() []byte {
	if p == nil {
		return nil
	}
	return p.projected
}

// move is one existing local path whose ignored, relevant or owned state the
// projected policy would change.
type move struct {
	path   string
	before string
	after  string
}

// Changed reports whether the projected policy differs semantically from the
// current one, which is the only case that needs a review and a confirmation.
func (p *Policy) Changed() bool {
	return p != nil && (len(p.changes) != 0 || len(p.patches) != 0)
}

// Review builds the projected policy and refuses every incoming policy GitOne
// must not apply: an unusable .gitone.yml, a repository or remote
// reconfiguration, and a policy that would newly manage an existing local
// path. It reads no file content of the working tree.
func Review(command Command, configuration *config.Config, root string, inventory *worktree.Result, repositories []Repository) (*Policy, error) {
	current, err := configuration.Matcher()
	if err != nil {
		return nil, err
	}
	commits, err := projectedCommits(command, configuration, root, repositories)
	if err != nil {
		return nil, err
	}
	result := &Policy{Matcher: current, Commits: commits, command: command}
	if !slices.ContainsFunc(repositories, func(current Repository) bool { return current.Target != "" }) {
		return result, nil
	}

	trees := map[string]map[string]string{}
	var holders []string
	for _, name := range slices.Sorted(maps.Keys(commits)) {
		files, err := repository.TreeFiles(root, name, commits[name])
		if err != nil {
			return nil, fmt.Errorf("%s %s\n%s", command.Code, detail(err), command.Unmodified)
		}
		trees[name] = files
		if _, held := files[config.PublicFile]; held {
			holders = append(holders, name)
		}
	}

	byName := map[string]Repository{}
	for _, current := range repositories {
		byName[current.Name] = current
	}
	holder, err := command.incomingConfig(current, byName, holders)
	if err != nil {
		return nil, err
	}

	projected, matcher := configuration, current
	if moved := byName[holder]; moved.Target != "" {
		projected, matcher, result.projected, err = command.accept(root, moved, trees[holder][config.PublicFile])
		if err != nil {
			return nil, err
		}
	}

	result.Matcher, result.holder = matcher, byName[holder]
	result.changes = config.Differences(configuration, projected)
	// A structural change is refused by every command that cannot reconcile
	// the repositories with it, and names the one command that can. Only
	// gitone reconfigure itself leaves Reconfigure empty: it applies the
	// change and reviews it in its own preview, with its own flag, so the
	// structural fields are not part of the ordinary policy class here.
	if structural := config.Structural(result.changes); len(structural) != 0 {
		if command.Reconfigure != "" {
			return nil, command.refuse(holder, "incoming repository reconfiguration needs "+command.Reconfigure,
				"%s the incoming policy adds, removes or renames a repository or changes a configured remote (repository %q, %s):\n%s\n"+
					"Review and apply it with: %s\n%s",
				command.Code, holder, result.holder.Source, fields(structural), command.Reconfigure, command.Unmodified)
		}
		result.changes = slices.DeleteFunc(result.changes, func(current config.Change) bool { return current.Structural })
	}
	result.patches, err = command.ignorePatches(root, repositories)
	if err != nil {
		return nil, err
	}
	if !result.Changed() {
		return result, nil
	}
	if result.moves, err = command.placements(root, inventory, trees, matcher); err != nil {
		return nil, err
	}
	if err := command.invalidPlacements(result.moves); err != nil {
		return result, err
	}
	return result, command.exposure(projected, result.moves)
}

// projectedCommits is the commit every configured repository would hold once
// the command applied. A repository without a commit is absent.
func projectedCommits(command Command, configuration *config.Config, root string, repositories []Repository) (map[string]string, error) {
	commits := map[string]string{}
	for _, name := range configuration.RepositoryNames() {
		commit, err := repository.Reference(root, name, "HEAD")
		if err != nil {
			return nil, fmt.Errorf("%s repository %q cannot be read: %s\n%s", command.Code, name, detail(err), command.Unmodified)
		}
		if commit != "" {
			commits[name] = commit
		}
	}
	for _, current := range repositories {
		if current.Target != "" {
			commits[current.Name] = current.Target
		}
	}
	return commits, nil
}

// incomingConfig names the single repository whose projected tree holds
// .gitone.yml, or nothing when the command cannot replace the file at all. A
// configuration that disappeared from a moving repository, or that two
// repositories would carry, is refused: GitOne would no longer know which
// file is policy.
func (c Command) incomingConfig(matcher *config.Matcher, byName map[string]Repository, holders []string) (string, error) {
	if len(holders) == 1 {
		return holders[0], nil
	}
	if len(holders) == 0 {
		// A repository that stays where it is cannot replace the working-tree
		// file, so an uncommitted configuration is not an incoming change.
		owner, _, _ := matcher.Owner(config.PublicFile)
		if byName[owner].Target == "" {
			return "", nil
		}
		return "", c.refuse(owner, config.PublicFile+" is missing from the incoming commits",
			"%s no incoming commit of repository %q (%s) carries %s any more.\n%s\n%s",
			c.Code, owner, byName[owner].Source, config.PublicFile, c.Repair, c.Unmodified)
	}
	descriptions := make([]string, len(holders))
	for index, holder := range holders {
		descriptions[index] = strings.TrimSpace(holder + " " + within(byName[holder]))
	}
	return "", c.refuse(holders[0], config.PublicFile+" is carried by more than one repository",
		"%s %s is carried by more than one repository afterwards: %s.\n%s\n%s",
		c.Code, config.PublicFile, strings.Join(descriptions, ", "), c.Repair, c.Unmodified)
}

// accept validates the incoming configuration against the unchanged local
// file. The local file is never read from repository content, so an incoming
// policy can extend the committed one but never weaken the local protections.
func (c Command) accept(root string, holder Repository, object string) (*config.Config, *config.Matcher, []byte, error) {
	invalid := func(err error) error {
		return c.refuse(holder.Name, "the incoming "+config.PublicFile+" is invalid",
			"%s the incoming %s of repository %q (%s) is invalid: %s\n%s\n%s",
			c.Code, config.PublicFile, holder.Name, holder.Source, detail(err), c.Repair, c.Unmodified)
	}
	contents, err := repository.Blob(root, holder.Name, object)
	if err != nil {
		return nil, nil, nil, c.refuse(holder.Name, "the incoming "+config.PublicFile+" cannot be read",
			"%s the incoming %s of repository %q (%s) cannot be read: %s\n%s\n%s",
			c.Code, config.PublicFile, holder.Name, holder.Source, detail(err), c.Repair, c.Unmodified)
	}
	projected, err := config.Effective([]byte(contents), root)
	if err != nil {
		return nil, nil, nil, invalid(err)
	}
	matcher, err := projected.Matcher()
	if err != nil {
		return nil, nil, nil, invalid(err)
	}
	switch owner, issue, reason := matcher.Owner(config.PublicFile); {
	case issue != config.OwnerValid:
		return nil, nil, nil, c.refuse(holder.Name, config.PublicFile+" is no longer owned by exactly one repository",
			"%s the incoming %s of repository %q (%s) no longer owns itself: %s\n%s\n%s",
			c.Code, config.PublicFile, holder.Name, holder.Source, reason, c.Repair, c.Unmodified)
	case owner != holder.Name:
		return nil, nil, nil, c.refuse(holder.Name, config.PublicFile+" would be owned by another repository",
			"%s the incoming %s of repository %q (%s) assigns %s to repository %q instead.\n%s\n%s",
			c.Code, config.PublicFile, holder.Name, holder.Source, config.PublicFile, owner, c.Repair, c.Unmodified)
	}
	return projected, matcher, []byte(contents), nil
}

// ignorePatches is the complete unified diff of every tracked .gitignore the
// incoming commits change, so an ignore rule can never be replaced unseen.
func (c Command) ignorePatches(root string, repositories []Repository) ([]string, error) {
	var patches []string
	for _, current := range repositories {
		if current.Target == "" {
			continue
		}
		gitDirectory := "--git-dir=" + repository.Directory(root, current.Name)
		changed, err := git.Run(root, gitDirectory, "diff", "--no-renames", "--name-only", "-z", current.Head, current.Target)
		if err != nil {
			return nil, fmt.Errorf("%s repository %q cannot be compared: %s\n%s", c.Code, current.Name, detail(err), c.Unmodified)
		}
		names := slices.DeleteFunc(strings.Split(changed, "\x00"), func(name string) bool {
			return name == "" || path.Base(name) != config.ProjectIgnoreFile
		})
		if len(names) == 0 {
			continue
		}
		arguments := append([]string{"--no-pager", "--no-optional-locks", gitDirectory, "--work-tree=" + root,
			"diff", "--no-renames", "--no-ext-diff", "--no-textconv", "--no-color", current.Head, current.Target, "--"}, names...)
		patch, err := git.Run(root, arguments...)
		if err != nil {
			return nil, fmt.Errorf("%s repository %q cannot be compared: %s\n%s", c.Code, current.Name, detail(err), c.Unmodified)
		}
		patches = append(patches, ui.Safe(patch))
	}
	return patches, nil
}

// placements compares the current ignored, relevant and owned state of every
// existing local path with the state the projected policy gives it. No file
// content is read: only paths, ownership patterns and ignore rules decide.
func (c Command) placements(root string, inventory *worktree.Result, trees map[string]map[string]string, matcher *config.Matcher) ([]move, error) {
	overlay, err := c.projectedIgnores(root, inventory, trees)
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(overlay)

	states := map[string]string{}
	maps.Copy(states, inventory.Owners)
	hiddenNow, err := worktree.Ignored(root, "")
	if err != nil {
		return nil, err
	}
	for _, entry := range hiddenNow {
		if !config.IsReserved(strings.TrimSuffix(entry, "/")) {
			states[entry] = ignoredState
		}
	}

	hidden, err := worktree.IgnoredBy(overlay, slices.Sorted(maps.Keys(states)))
	if err != nil {
		return nil, err
	}
	// A directory the current rules hide is one entry and its content is
	// never enumerated. Only a directory the projected rules stop hiding has
	// to be opened, and only then.
	var revealed []string
	for _, entry := range slices.Sorted(maps.Keys(states)) {
		if !strings.HasSuffix(entry, "/") || hidden[entry] {
			continue
		}
		names, err := worktree.Ignored(root, entry)
		if err != nil {
			return nil, err
		}
		if len(names) == 0 {
			continue
		}
		delete(states, entry)
		for _, name := range names {
			states[name] = ignoredState
			revealed = append(revealed, name)
			if path.Base(name) != config.ProjectIgnoreFile {
				continue
			}
			contents, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
			if err != nil {
				return nil, err
			}
			if err := place(overlay, name, contents); err != nil {
				return nil, err
			}
		}
	}
	if len(revealed) != 0 {
		more, err := worktree.IgnoredBy(overlay, revealed)
		if err != nil {
			return nil, err
		}
		maps.Copy(hidden, more)
	}

	// A path the projected trees track stays relevant even once an ignore
	// rule covers it, exactly as the working-tree scan treats a tracked path.
	for _, files := range trees {
		for entry := range files {
			delete(hidden, entry)
		}
	}

	var moves []move
	for _, entry := range slices.Sorted(maps.Keys(states)) {
		if after := placement(entry, hidden, matcher); after != states[entry] {
			moves = append(moves, move{path: entry, before: states[entry], after: after})
		}
	}
	return moves, nil
}

func placement(entry string, hidden map[string]bool, matcher *config.Matcher) string {
	if hidden[entry] {
		return ignoredState
	}
	if strings.HasSuffix(entry, "/") {
		// An emptied ignored directory owns nothing; its files decide.
		return relevantState
	}
	switch owner, issue, reason := matcher.Owner(entry); issue {
	case config.OwnerValid:
		return owner
	case config.OwnerUnassigned:
		return unassignedState
	default:
		return "unsafe: " + reason
	}
}

// projectedIgnores writes every .gitignore the command would leave behind
// into a temporary directory. It holds nothing but ignore files, so the
// projected rules are evaluated without preparing or touching a single
// managed file.
func (c Command) projectedIgnores(root string, inventory *worktree.Result, trees map[string]map[string]string) (string, error) {
	overlay, err := os.MkdirTemp("", "gitone-policy")
	if err != nil {
		return "", err
	}
	written := map[string]bool{}
	for _, name := range slices.Sorted(maps.Keys(trees)) {
		for _, entry := range slices.Sorted(maps.Keys(trees[name])) {
			if path.Base(entry) != config.ProjectIgnoreFile {
				continue
			}
			contents, err := repository.Blob(root, name, trees[name][entry])
			if err != nil {
				return overlay, fmt.Errorf("%s repository %q cannot be read: %s\n%s", c.Code, name, detail(err), c.Unmodified)
			}
			if err := place(overlay, entry, []byte(contents)); err != nil {
				return overlay, err
			}
			written[entry] = true
		}
	}
	// An untracked ignore file is not part of any incoming commit and stays
	// exactly as it is; a tracked one absent from the trees above is removed.
	tracked := map[string]bool{}
	for _, entry := range inventory.Tracked {
		tracked[entry] = true
	}
	for _, entry := range slices.Sorted(maps.Keys(inventory.Owners)) {
		if path.Base(entry) != config.ProjectIgnoreFile || written[entry] || tracked[entry] {
			continue
		}
		contents, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(entry)))
		if err != nil {
			return overlay, err
		}
		if err := place(overlay, entry, contents); err != nil {
			return overlay, err
		}
	}
	return overlay, nil
}

func place(overlay, relativePath string, contents []byte) error {
	full := filepath.Join(overlay, filepath.FromSlash(relativePath))
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		return err
	}
	return os.WriteFile(full, contents, 0o600)
}

// invalidPlacements refuses a policy that would leave an existing local path
// unassigned, ambiguous or otherwise unsafe.
func (c Command) invalidPlacements(moves []move) error {
	var invalid []string
	for _, current := range moves {
		if current.after != unassignedState && !strings.HasPrefix(current.after, "unsafe: ") {
			continue
		}
		invalid = append(invalid, current.line())
	}
	if len(invalid) == 0 {
		return nil
	}
	return fmt.Errorf("%s the incoming policy would leave existing local paths invalid:\n%s\n"+
		"Protect, move, remove or explicitly reconfigure each path locally, then run %s again.\n%s",
		c.Code, strings.Join(invalid, "\n"), c.Retry, c.Unmodified)
}

// exposure refuses the one policy class no confirmation can accept: an
// existing local path that no repository manages today and that the projected
// policy would put into one. Such a path is publishable by the ordinary add,
// commit and push workflow the moment the policy applies.
func (c Command) exposure(projected *config.Config, moves []move) error {
	var exposed []string
	for _, current := range moves {
		if current.before != ignoredState && current.before != unassignedState {
			continue
		}
		if _, managed := projected.Repositories[current.after]; managed {
			exposed = append(exposed, current.line())
		}
	}
	if len(exposed) == 0 {
		return nil
	}
	return fmt.Errorf("%s the incoming policy would put existing local paths into a repository:\n%s\n"+
		"Protect, move, remove or explicitly reconfigure each path locally, then run %s again.\n"+
		"%s never accepts this.\n%s",
		c.Code, strings.Join(exposed, "\n"), c.Retry, AcceptFlag, c.Unmodified)
}

// Preview is the one ordered review a confirmation answers: the changed
// configuration fields, the complete diff of every changed ignore file and
// every existing local path the change would move.
func (p *Policy) Preview(output io.Writer) {
	style := ui.For(output)
	fmt.Fprintf(output, "%s\n", style.Heading.Render("Incoming policy change:"))
	if len(p.changes) != 0 {
		fmt.Fprintf(output, "\n%s\n\n%s\n", style.Heading.Render(strings.TrimSpace("CONFIGURATION "+within(p.holder))), fields(p.changes))
	}
	for _, patch := range p.patches {
		fmt.Fprintf(output, "\n%s\n\n%s", style.Heading.Render("IGNORE RULES"), patch)
	}
	if len(p.moves) != 0 {
		fmt.Fprintf(output, "\n%s\n\n", style.Heading.Render("AFFECTED PATHS"))
		for _, current := range p.moves {
			fmt.Fprintf(output, "%s\n", current.line())
		}
	}
	if len(p.moves) == 0 {
		fmt.Fprintf(output, "\n%s\n", style.Muted.Render("No existing local path changes its ignored, relevant or owned state."))
	}
	fmt.Fprintln(output)
}

func (m move) line() string {
	return fmt.Sprintf("  %s  %s -> %s", m.path, m.before, m.after)
}

func fields(changes []config.Change) string {
	lines := make([]string, 0, len(changes)*3)
	for _, current := range changes {
		lines = append(lines, "  "+current.Field, "    before: "+current.Before, "    after:  "+current.After)
	}
	return strings.Join(lines, "\n")
}

// Confirm asks once for the explicit approval a policy change requires. The
// default answer is no, and an invocation nobody can answer needs the flag.
func (p *Policy) Confirm(accepted, interactive bool, input io.Reader, output io.Writer) (bool, error) {
	if !p.Changed() {
		return true, nil
	}
	return Ask(p.command, "an incoming policy change", "Accept this policy change?", AcceptFlag, accepted, interactive, input, output)
}

// Ask is the one explicit approval every reviewed change of a command needs:
// asked once, defaulting to no, and replaced by exactly one flag when no
// interactive terminal can answer it. subject names the change in the refusal
// that flag closes. Reading a single line at a time keeps several questions of
// one command answerable from the same input.
func Ask(command Command, subject, question, flag string, accepted, interactive bool, input io.Reader, output io.Writer) (bool, error) {
	if accepted {
		return true, nil
	}
	if !interactive {
		return false, fmt.Errorf("%s %s without an interactive terminal requires %s\n%s",
			command.Code, subject, flag, command.Unmodified)
	}
	fmt.Fprintf(output, "%s [y/N] ", question)
	answer, err := bufio.NewReader(input).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	if slices.Contains([]string{"y", "yes"}, strings.ToLower(strings.TrimSpace(answer))) {
		return true, nil
	}
	fmt.Fprintf(output, "\n%s\n\n%s\n", command.Aborted, command.Unmodified)
	return false, nil
}

// refuse records why one repository stopped the command and returns the
// complete failure, so the report names the same repository the message does.
func (c Command) refuse(name, summary, format string, arguments ...any) error {
	if name != "" && c.Failed != nil {
		c.Failed(name, summary)
	}
	return fmt.Errorf(format, arguments...)
}

// within names where a changed configuration comes from, and nothing when the
// repository that owns it is not moving.
func within(holder Repository) string {
	if holder.Source == "" {
		return ""
	}
	return "(" + holder.Source + ")"
}

// detail is the first line of an error, so one reported problem stays one line.
func detail(err error) string {
	line, _, _ := strings.Cut(err.Error(), "\n")
	return line
}
