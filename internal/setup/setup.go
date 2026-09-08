// Package setup is the interactive entry point of GitOne. It asks for a
// complete configuration when a project has none and then hands the project
// to the existing init and migrate behavior. It never edits configuration
// that already exists and never infers repositories from the working tree; it
// only offers the owned paths a project already has. Project setup itself
// never contacts a remote; after it succeeds, the user may explicitly install
// the VS Code extension.
package setup

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"charm.land/huh/v2"
	"github.com/devidevio/gitone/internal/agents"
	"github.com/devidevio/gitone/internal/config"
	"github.com/devidevio/gitone/internal/git"
	"github.com/devidevio/gitone/internal/migrate"
	"github.com/devidevio/gitone/internal/repository"
	"github.com/devidevio/gitone/internal/ui"
	"github.com/devidevio/gitone/internal/vscode"
	"github.com/devidevio/gitone/internal/worktree"
)

const (
	// failed is the stable code of every refused setup.
	failed = "SETUP001"

	gitDirectory = ".git"

	unchanged = "Nothing was changed."

	// wizardMode and templateMode are the two ways to set up a project that
	// has no configuration yet.
	wizardMode   = "Wizard"
	templateMode = "Template"
)

// Setup asks for everything the project needs and then initializes or
// migrates it. interactive reports whether a terminal can answer the
// questions; without one setup refuses instead of assuming answers.
func Setup(cwd string, interactive bool, input io.Reader, output io.Writer) error {
	if !interactive {
		return fail("gitone setup asks questions and needs an interactive terminal\n%s", unchanged)
	}
	root, err := filepath.Abs(cwd)
	if err != nil {
		return err
	}
	configured, repository, err := locate(root)
	if err != nil {
		return err
	}
	switch {
	case configured != "" && configured != root:
		return fail("%s is inside the GitOne project %s: nested projects are not supported\n%s", root, configured, unchanged)
	case configured == "" && repository != "" && repository != root:
		return fail("%s is inside the Git repository %s: run gitone setup in that directory instead\n%s", root, repository, unchanged)
	}
	interrupted, err := migrate.Interrupted(root)
	if err != nil {
		return err
	}
	if interrupted {
		return fail("an interrupted migration was found: resolve it with gitone migrate first\n%s", unchanged)
	}

	asked := &prompt{
		reader: bufio.NewReader(input),
		input:  input,
		output: output,
		plain:  ui.Plain(input, output),
	}
	if configured == root {
		return existing(root, asked, output)
	}
	return create(root, repository == root, asked, output)
}

// locate reports the nearest directory at or above start that holds a project
// configuration, and the nearest one that holds a Git repository. An empty
// result means there is none.
func locate(start string) (configured, repository string, err error) {
	for current := start; ; {
		if configured == "" {
			_, publicErr := os.Lstat(filepath.Join(current, config.PublicFile))
			_, localErr := os.Lstat(filepath.Join(current, config.LocalFile))
			switch {
			case publicErr == nil:
				configured = current
			case !errors.Is(publicErr, os.ErrNotExist):
				return "", "", publicErr
			case localErr == nil:
				return "", "", fail("%s exists without %s\n%s", filepath.Join(current, config.LocalFile), config.PublicFile, unchanged)
			case !errors.Is(localErr, os.ErrNotExist):
				return "", "", localErr
			}
		}
		if repository == "" {
			switch _, err := os.Lstat(filepath.Join(current, gitDirectory)); {
			case err == nil:
				repository = current
			case !errors.Is(err, os.ErrNotExist):
				return "", "", err
			}
		}
		parent := filepath.Dir(current)
		if parent == current || (configured != "" && repository != "") {
			return configured, repository, nil
		}
		current = parent
	}
}

// existing sets up a project that already has a configuration. The
// configuration is loaded and validated, but never edited or replaced.
func existing(root string, asked *prompt, output io.Writer) error {
	configuration, _, err := config.Load(root)
	if err != nil {
		return err
	}
	fmt.Fprintf(output, "GitOne setup\n\nThe existing configuration in %s is used unchanged.\n\n", root)
	describe(output, configuration)
	// An existing configuration is never edited, so the block is only offered
	// when it already gives AGENTS.md exactly one owner.
	plan, ok := offerAgents(root, agents.Ownership(configuration) == nil, asked, output)
	if !ok {
		return asked.cancelled(output)
	}
	return finish(configuration, root, nil, plan, asked, output)
}

// create asks how a project without configuration is set up and runs the
// chosen mode. The current directory becomes the project root.
func create(root string, rootRepository bool, asked *prompt, output io.Writer) error {
	fmt.Fprintf(output, "GitOne setup\n\nA new project is created in %s.\n\n", root)
	mode, ok := asked.mode()
	if !ok {
		return asked.cancelled(output)
	}
	if mode == templateMode {
		return template(root, asked, output)
	}
	return wizard(root, rootRepository, asked, output)
}

// mode asks which of the two setup modes a project without configuration
// uses. Enter takes the wizard on both question styles.
func (p *prompt) mode() (string, bool) {
	choices := []string{wizardMode, templateMode}
	return p.selectValue("Setup mode",
		"Wizard asks for the configuration and sets the project up, Template writes\ncommented files you edit yourself",
		wizardMode, choices, oneOf(choices))
}

// wizard asks for a complete configuration and sets the project up with it.
func wizard(root string, rootRepository bool, asked *prompt, output io.Writer) error {
	suggestions, relevant, unsafe, err := worktree.Suggest(root)
	if err != nil {
		return err
	}
	// Both problems are decided by the working tree alone, so they are reported
	// before the default-branch and repository questions.
	if len(unsafe) != 0 {
		return refuse(unsafe)
	}
	if err := validateInternal(nil, root); err != nil {
		return err
	}
	asked.suggestions, asked.relevant = suggestions, relevant

	if rootRepository {
		fmt.Fprintf(output, "\nThis directory is one ordinary Git repository, so setup will migrate it.\n")
	}
	fmt.Fprintf(output, "\nEvery repository is entered explicitly. Only the owned paths are offered\nfrom the files this project already has.\n\n")

	branch, ok := asked.value("Default branch for every repository", "main", config.ValidateBranch)
	if !ok {
		return asked.cancelled(output)
	}
	fmt.Fprintln(output)
	entries, ok := asked.repositories()
	if !ok {
		return asked.cancelled(output)
	}
	if !assign(entries, asked, output) {
		return asked.cancelled(output)
	}
	plan, ok := offerAgents(root, true, asked, output)
	if !ok {
		return asked.cancelled(output)
	}
	if plan.create && !ownAgents(entries, asked, output) {
		return asked.cancelled(output)
	}
	for {
		generated := &files{defaultBranch: branch, entries: entries}
		configuration, issues, local, err := check(generated, root, rootRepository)
		if err != nil {
			return err
		}
		if local != nil {
			if !reassign(local, entries, asked, output) {
				return asked.cancelled(output)
			}
			if !assign(entries, asked, output) {
				return asked.cancelled(output)
			}
			continue
		}
		if len(issues) == 0 {
			fmt.Fprintln(output)
			generated.describe(output)
			describePlan(root, output)
			return finish(configuration, root, generated, plan, asked, output)
		}
		report(issues, output)
		if paths := unassigned(issues); len(paths) != 0 {
			if !distribute(entries, paths, asked, output) {
				return asked.cancelled(output)
			}
			continue
		}
		corrected, ok := correct(entries, asked, output)
		if !ok {
			return asked.cancelled(output)
		}
		paths, ok := asked.paths(without(entries, corrected), corrected.owned())
		if !ok {
			return asked.cancelled(output)
		}
		corrected.own(paths)
		if !assign(entries, asked, output) {
			return asked.cancelled(output)
		}
	}
}

// report names the ownership problems of a complete configuration, before
// either correction flow asks about them.
func report(issues []*worktree.Issue, output io.Writer) {
	fmt.Fprintf(output, "\nThe owned paths do not cover the project:\n")
	for _, issue := range issues {
		fmt.Fprintf(output, "  %v\n", issue)
	}
}

// unassigned returns the reported path of every PATH001 issue, alphabetically
// and each path once.
func unassigned(issues []*worktree.Issue) []string {
	var paths []string
	for _, issue := range issues {
		if issue.Code == worktree.PathUnassigned {
			paths = append(paths, issue.Path)
		}
	}
	slices.Sort(paths)
	return slices.Compact(paths)
}

// distribute gives every unassigned path an owner directly. Only the exact
// reported path is added, never a directory or pattern derived from it, so an
// answer can never claim more than the reported file. With one repository
// there is nothing left to decide.
func distribute(entries []*entry, paths []string, asked *prompt, output io.Writer) bool {
	if len(entries) == 1 {
		fmt.Fprintf(output, "\nEvery unassigned path is added to %s; every other answer is kept.\n", entries[0].name)
	} else {
		fmt.Fprintf(output, "\nEach unassigned path is added to the repository you name; every other answer is kept.\n")
	}
	names := make([]string, len(entries))
	for index, collected := range entries {
		names[index] = collected.name
	}
	for _, owned := range paths {
		chosen := entries[0]
		if len(entries) > 1 {
			answer, ok := asked.selectValue("Repository owning "+owned, "", "", names, oneOf(names))
			if !ok {
				return false
			}
			chosen = entries[slices.Index(names, answer)]
		}
		chosen.own(append(chosen.owned(), owned))
	}
	return true
}

// correct asks whose owned paths to reopen for the ambiguity the direct
// assignment above cannot resolve. Every other answer is kept.
func correct(entries []*entry, asked *prompt, output io.Writer) (*entry, bool) {
	fmt.Fprintf(output, "\nOnly the owned paths are asked again; every other answer is kept.\n")
	names := make([]string, len(entries))
	for index, collected := range entries {
		names[index] = collected.name
	}
	slices.Sort(names)
	answer, ok := asked.selectValue("Correct the paths of", "", "", names, oneOf(names))
	if !ok {
		return nil, false
	}
	index := slices.IndexFunc(entries, func(collected *entry) bool { return collected.name == answer })
	return entries[index], true
}

// without returns every collected repository except one.
func without(entries []*entry, excluded *entry) []*entry {
	return slices.DeleteFunc(slices.Clone(entries), func(collected *entry) bool { return collected == excluded })
}

// oneOf checks an answer against a fixed list of choices.
func oneOf(names []string) func(string) error {
	return func(answer string) error {
		if !slices.Contains(names, answer) {
			return fmt.Errorf("answer one of %s", strings.Join(names, ", "))
		}
		return nil
	}
}

// projectFiles are the files setup generates itself. They need explicit
// ownership like every other project file, but the user cannot assign files
// that do not exist yet, so setup assigns them after the repositories.
var projectFiles = []string{config.PublicFile, config.ProjectIgnoreFile}

// assign gives both project files an owner among the collected repositories.
// An owner already entered by hand is kept; a project file without one is
// added to the owner of the other project file, or to a repository the user
// selects. It reports false when the input ended, which cancels setup.
func assign(entries []*entry, asked *prompt, output io.Writer) bool {
	var missing []string
	var owner *entry
	for _, file := range projectFiles {
		switch owners := claimants(entries, file); len(owners) {
		case 0:
			missing = append(missing, file)
		case 1:
			if owner == nil {
				owner = owners[0]
			}
		default:
			// Several repositories claim the file. Setup does not resolve
			// that; the strict validation rejects it before anything is
			// written.
			return true
		}
	}
	if len(missing) == 0 {
		return true
	}
	if owner == nil {
		if owner = pick(entries, asked, output); owner == nil {
			return false
		}
	}
	owner.derived = append(owner.derived, missing...)
	owner.Paths = append(owner.Paths, missing...)
	slices.Sort(owner.Paths)
	return true
}

// claimants returns the collected repositories that own file. A claim is
// never overruled here, not even a local one that cannot hold a project file:
// adding the file to a second repository would only hide the conflict, which
// the validation reports concretely instead.
func claimants(entries []*entry, file string) []*entry {
	var owners []*entry
	for _, collected := range entries {
		owns := func(pattern string) bool { return config.Owns(pattern, file) }
		if slices.ContainsFunc(collected.Paths, owns) {
			owners = append(owners, collected)
		}
	}
	return owners
}

// pick asks which repository owns the project files. One repository in the
// committed file is the only possible answer and is used directly; several
// are an explicit question without a default. Without any repository in the
// committed file, the selected one is moved there, because the project files
// cannot belong to a repository that only this machine knows.
func pick(entries []*entry, asked *prompt, output io.Writer) *entry {
	candidates := slices.DeleteFunc(slices.Clone(entries), func(collected *entry) bool { return collected.local })
	move := len(candidates) == 0
	if move {
		candidates = slices.Clone(entries)
	}
	fmt.Fprintf(output, "\n%s and %s belong to exactly one repository in %s.\n",
		config.PublicFile, config.ProjectIgnoreFile, config.PublicFile)
	if !move && len(candidates) == 1 {
		fmt.Fprintf(output, "They are owned by %s.\n", candidates[0].name)
		return candidates[0]
	}
	if move {
		fmt.Fprintf(output, "No repository is stored there yet, so the selected one is moved from %s to %s.\n",
			config.LocalFile, config.PublicFile)
	}
	slices.SortFunc(candidates, func(a, b *entry) int { return strings.Compare(a.name, b.name) })
	names := make([]string, len(candidates))
	for i, candidate := range candidates {
		names[i] = candidate.name
	}
	answer, ok := asked.selectValue("Owner", "", "", names, oneOf(names))
	if !ok {
		return nil
	}
	chosen := candidates[slices.Index(names, answer)]
	chosen.local = false
	return chosen
}

// describePlan reports the project files setup writes itself. Their ownership is part
// of the generated configuration printed above it.
func describePlan(root string, output io.Writer) {
	fmt.Fprintf(output, "%s is created, %s is %s with the GitOne entries.\n\n",
		config.PublicFile, config.ProjectIgnoreFile, ignoreAction(root))
}

// ignoreAction reports whether writing the GitOne entries creates or updates
// the project ignore file.
func ignoreAction(root string) string {
	if _, err := os.Lstat(filepath.Join(root, config.ProjectIgnoreFile)); err == nil {
		return "updated"
	}
	return "created"
}

// check validates a collected configuration exactly as the loader and the
// following action do. It returns the unassigned and ambiguous paths, which
// choosing different owned paths can still fix, and the repository whose
// project-file claim reopens its own two questions; every other problem is an
// error that ends setup without writing anything.
func check(generated *files, root string, rootRepository bool) (*config.Config, []*worktree.Issue, *entry, error) {
	configuration, err := generated.load()
	if err != nil {
		return nil, nil, nil, err
	}
	matcher, err := configuration.Matcher()
	if err != nil {
		return nil, nil, nil, err
	}
	var issues []*worktree.Issue
	// The configuration file does not exist yet, so the working-tree scan
	// below cannot check who owns it.
	switch owners, _ := matcher.Owners(config.PublicFile); {
	case len(owners) == 0:
		issues = append(issues, &worktree.Issue{Code: worktree.PathUnassigned, Path: config.PublicFile, Detail: "path is not assigned to any repository"})
	case len(owners) > 1:
		issues = append(issues, &worktree.Issue{Code: worktree.PathAmbiguous, Path: config.PublicFile, Detail: "path matches " + strings.Join(owners, ", ")})
	}
	// A clone has only the committed file, so it would never find an owner
	// that only this machine knows. The claim is reopened, not refused.
	if local := localOwner(generated.entries); local != nil {
		return nil, nil, local, nil
	}
	found, err := repository.WorktreeIssues(configuration, root, rootRepository)
	if err != nil {
		return nil, nil, nil, err
	}
	issues = append(issues, found...)

	if slices.ContainsFunc(issues, func(issue *worktree.Issue) bool {
		return issue.Code != worktree.PathUnassigned && issue.Code != worktree.PathAmbiguous
	}) {
		return nil, nil, nil, refuse(issues)
	}
	return configuration, issues, nil, nil
}

// localOwner returns a repository that claims a project file while being
// stored only in the local file. Its focused correction runs before any
// remaining ambiguity is handled as PATH002.
func localOwner(entries []*entry) *entry {
	for _, file := range projectFiles {
		for _, collected := range claimants(entries, file) {
			if collected.local {
				return collected
			}
		}
	}
	return nil
}

// reassign reopens the owned paths and the storage of the one repository that
// claims a project file although only this machine knows it. Its name,
// visibility, remote and every other repository stay as entered; the project
// files setup assigned itself are released, so the assignment below runs
// again on the corrected answers.
func reassign(collected *entry, entries []*entry, asked *prompt, output io.Writer) bool {
	fmt.Fprintf(output, "\n%s claims %s or %s but is stored in %s. A clone reads only %s,\nso only a repository stored there can own the project files.\n",
		collected.name, config.PublicFile, config.ProjectIgnoreFile, config.LocalFile, config.PublicFile)
	fmt.Fprintf(output, "Only the owned paths and the storage of %s are asked again; every other answer is kept.\n", collected.name)
	collected.releaseProjectFiles()
	paths, ok := asked.paths(without(entries, collected), collected.owned())
	if !ok {
		return false
	}
	collected.own(paths)
	stored, ok := asked.storage("local")
	if !ok {
		return false
	}
	collected.local = stored == "local"
	return true
}

// refuse turns the problems no answer can fix into the error that ends setup
// without writing anything.
func refuse(issues []*worktree.Issue) error {
	problems := make([]error, len(issues))
	for index := range issues {
		problems[index] = issues[index]
	}
	return fmt.Errorf("%w\n\n%s", errors.Join(problems...), unchanged)
}

// action is the project state setup has to resolve.
type action int

const (
	initialize action = iota
	migration
	nothing
)

// choose reports the action the current root state needs. Every state that
// neither init nor migrate explains is refused before anything is asked.
func choose(configuration *config.Config, root string) (action, error) {
	if err := validateInternal(configuration, root); err != nil {
		return 0, err
	}
	switch info, err := os.Lstat(filepath.Join(root, gitDirectory)); {
	case err == nil && info.IsDir():
		return migration, nil
	case err == nil:
		return 0, fail(".git is not a directory: linked worktrees and submodules are not supported\n%s", unchanged)
	case !errors.Is(err, os.ErrNotExist):
		return 0, err
	}

	var missing, broken []error
	for _, name := range configuration.RepositoryNames() {
		for _, issue := range repository.Check(root, name, configuration.Repositories[name]) {
			if issue.Kind == repository.MissingIssue {
				missing = append(missing, issue)
				continue
			}
			broken = append(broken, issue)
		}
	}
	switch {
	case len(broken) != 0:
		return 0, fmt.Errorf("%s the existing repository state is not usable:\n%w\n%s", failed, errors.Join(broken...), unchanged)
	case len(missing) == 0:
		return nothing, nil
	default:
		return initialize, nil
	}
}

// validateInternal refuses state that no GitOne operation owns. A nil
// configuration is the preflight of a project that has none: setup never
// adopts repository state it was not told about, so every managed repository
// found there is unexplained. The complete check runs again before the write,
// which closes the race with a concurrent operation.
func validateInternal(configuration *config.Config, root string) error {
	entries, err := os.ReadDir(filepath.Join(root, ".gitone"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !slices.Contains([]string{"lock", "migration-backup", "recovery", "repositories"}, entry.Name()) {
			return fail("unexplained GitOne state exists at %s\n%s", filepath.Join(root, ".gitone", entry.Name()), unchanged)
		}
	}
	repositories, err := os.ReadDir(filepath.Join(root, ".gitone", "repositories"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var names []string
	if configuration != nil {
		names = configuration.RepositoryNames()
	}
	for _, entry := range repositories {
		if !slices.Contains(names, entry.Name()) {
			return fail("unexplained repository state exists at %s\n%s", filepath.Join(root, ".gitone", "repositories", entry.Name()), unchanged)
		}
	}
	return nil
}

// finish shows the planned action, asks the one confirmation that authorizes
// both the configuration and the action, and then runs it. generated is nil
// when the configuration already existed.
func finish(configuration *config.Config, root string, generated *files, plan *agentsPlan, asked *prompt, output io.Writer) error {
	chosen, err := choose(configuration, root)
	if err != nil {
		return err
	}
	switch chosen {
	case migration:
		// The migration plan carries its own explanation, including the
		// warning about a modified working tree.
		if err := migrate.Plan(configuration, root, output); err != nil {
			return err
		}
	case initialize:
		if err := repository.ValidateWorktree(configuration, root, false); err != nil {
			return err
		}
		fmt.Fprintf(output, "Planned action: gitone init creates the managed repositories.\n\n")
	default:
		if generated == nil {
			fmt.Fprintf(output, "The project is already set up. %s\n", unchanged)
			if err := applyAgents(plan, agents.Ownership(configuration) == nil, root, output); err != nil {
				return err
			}
			return showVSCodeInstallHint(output)
		}
		fmt.Fprintf(output, "Planned action: none, the managed repositories already exist.\n\n")
	}

	confirmed, ok := asked.confirm("Continue?", false)
	if !ok || !confirmed {
		return asked.cancelled(output)
	}
	if generated != nil {
		if err := generated.write(root); err != nil {
			return err
		}
		if configuration, _, err = config.Load(root); err != nil {
			return kept(err, generated)
		}
	}

	switch chosen {
	case migration:
		err = ui.SpinBuffered(asked.input, output, "Migrating the root repository", func(rendered io.Writer) error {
			return migrate.Confirmed(configuration, root, rendered)
		})
	case initialize:
		err = ui.Spin(asked.input, output, "Initializing managed repositories", func() error {
			return repository.Init(configuration, root)
		})
		if err == nil {
			succeed(output, configuration)
		}
	default:
		succeed(output, configuration)
	}
	if err != nil {
		return kept(err, generated)
	}
	if err := applyAgents(plan, agents.Ownership(configuration) == nil, root, output); err != nil {
		return err
	}
	return showVSCodeInstallHint(output)
}

func showVSCodeInstallHint(output io.Writer) error {
	if !vscode.Available() {
		return nil
	}
	installed, err := vscode.Installed()
	if err != nil {
		fmt.Fprintf(output, "\nCould not check for the VS Code extension: %v\n", err)
		return nil
	}
	if installed {
		return nil
	}
	fmt.Fprintln(output, "\nOptional VS Code extension: download the release VSIX from https://github.com/devidevio/gitone/releases/latest")
	fmt.Fprintln(output, "Then run: gitone vscode install --vsix <downloaded-file.vsix>")
	return nil
}

// kept reports a failed action of a setup that already wrote a valid
// configuration. The configuration stays, so the action can be retried.
func kept(err error, generated *files) error {
	if generated == nil {
		return err
	}
	return fmt.Errorf("%w\n\nThe configuration was written and is kept. Fix the problem above and\nrun gitone setup again.", err)
}

func cancelled(output io.Writer) error {
	fmt.Fprintf(output, "\nSetup cancelled. %s\n", unchanged)
	return nil
}

func succeed(output io.Writer, configuration *config.Config) {
	fmt.Fprintln(output, "GitOne is set up.")
	fmt.Fprintln(output)
	ui.Repositories(output, configuration)
	fmt.Fprintf(output, "\nNothing was staged or committed.\n")
}

// describe prints a complete configuration that already exists.
func describe(output io.Writer, configuration *config.Config) {
	fmt.Fprintln(output, "Configuration")
	fmt.Fprintln(output)
	for _, name := range configuration.RepositoryNames() {
		repository := configuration.Repositories[name]
		fmt.Fprintf(output, "  %s (%s, %s, push %s)\n", name, repository.Visibility, configuration.DefaultBranch, repository.Push)
		for _, remote := range repository.RemoteNames() {
			fmt.Fprintf(output, "    remote %s: %s\n", remote, repository.Remotes[remote])
		}
		for _, owned := range repository.Paths {
			fmt.Fprintf(output, "    path %s\n", owned)
		}
	}
	fmt.Fprintln(output)
}

func fail(format string, arguments ...any) error {
	return fmt.Errorf("%s %s", failed, fmt.Sprintf(format, arguments...))
}

// prompt asks the setup questions on one terminal. A closed input cancels
// setup instead of guessing an answer.
type prompt struct {
	reader *bufio.Reader
	input  io.Reader
	output io.Writer
	plain  bool
	err    error
	// runField lets tests drive the same Huh fields without a real terminal.
	runField func(huh.Field) error

	// suggestions are the owned paths this project can offer and relevant is
	// every path it currently has. Both come from one working-tree inventory.
	suggestions []worktree.Suggestion
	relevant    []string
}

// ask prints question and returns the trimmed answer. It reports false when
// the input ended, which cancels setup.
func (p *prompt) ask(question string) (string, bool) {
	fmt.Fprint(p.output, question)
	line, err := p.reader.ReadString('\n')
	answer := strings.TrimSpace(line)
	if err != nil && (!errors.Is(err, io.EOF) || answer == "") {
		return "", false
	}
	return answer, true
}

// askUntil asks question until check accepts the answer. An empty answer
// takes fallback, so only a question with a default can be answered with
// Enter. It reports false when the input ended, which cancels setup.
func (p *prompt) askUntil(question, fallback string, check func(string) error) (string, bool) {
	for {
		answer, ok := p.ask(question)
		if !ok {
			return "", false
		}
		if answer == "" {
			answer = fallback
		}
		if err := check(answer); err != nil {
			fmt.Fprintf(p.output, "  %v\n", err)
			continue
		}
		return answer, true
	}
}

// question turns a title and its default into the text of a line-oriented
// question, so the stable questions always name what can be answered.
func question(title, fallback string) string {
	if fallback != "" {
		title += " (" + fallback + ")"
	}
	return title + ": "
}

// value asks for one text answer until check accepts it. An empty answer
// takes fallback, so only a question with a default can be answered with
// Enter. Answers are trimmed on both question styles, so nothing but the
// entered value reaches the configuration.
func (p *prompt) value(title, fallback string, check func(string) error) (string, bool) {
	validate := func(answer string) error {
		err := check(strings.TrimSpace(answer))
		if errors.Is(err, git.ErrUnavailable) {
			// Huh retries every validator error, so let the field close and
			// return this operational failure from setup itself.
			p.err = err
			return nil
		}
		return err
	}
	if p.plain {
		answer, ok := p.askUntil(question(title, fallback), fallback, validate)
		return answer, ok && p.err == nil
	}
	answer := fallback
	field := huh.NewInput().
		Title(title).
		Value(&answer).
		Validate(validate)
	if !p.run(field) || p.err != nil {
		return "", false
	}
	if answer = strings.TrimSpace(answer); answer == "" {
		answer = fallback
	}
	return answer, true
}

// confirm asks one yes/no question with the requested default.
func (p *prompt) confirm(question string, fallback bool) (bool, bool) {
	if !p.plain {
		answer := fallback
		if !p.run(huh.NewConfirm().Title(question).Value(&answer)) {
			return false, false
		}
		return answer, true
	}
	options := "[y/N] "
	if fallback {
		options = "[Y/n] "
	}
	answer, ok := p.ask(question + " " + options)
	if !ok {
		return false, false
	}
	answer = strings.ToLower(answer)
	if answer == "" {
		return fallback, true
	}
	return slices.Contains([]string{"y", "yes"}, answer), true
}

// repositories asks for one or more repositories.
func (p *prompt) repositories() ([]*entry, bool) {
	var entries []*entry
	for {
		collected, ok := p.repository(entries)
		if !ok {
			return nil, false
		}
		entries = append(entries, collected)
		fmt.Fprintln(p.output)
		answer, ok := p.confirm("Add another repository?", false)
		if !ok {
			return nil, false
		}
		if !answer {
			return entries, true
		}
		fmt.Fprintln(p.output)
	}
}

// repository asks for one complete repository. Every answer is validated on
// its own, so one invalid value is asked again instead of discarding the
// answers collected so far.
func (p *prompt) repository(collected []*entry) (*entry, bool) {
	name, ok := p.value("Repository name", "", func(answer string) error {
		if err := config.ValidateName(answer); err != nil {
			return err
		}
		if slices.ContainsFunc(collected, func(e *entry) bool { return e.name == answer }) {
			return fmt.Errorf("repository %q is already configured", answer)
		}
		return nil
	})
	if !ok {
		return nil, false
	}
	visibility, ok := p.selectValue("Visibility", "", "public", []string{"public", "private"}, config.ValidateVisibility)
	if !ok {
		return nil, false
	}
	paths, ok := p.paths(collected, nil)
	if !ok {
		return nil, false
	}
	remote, ok := p.value("Origin remote URL (optional, empty for none)", "", func(answer string) error {
		if answer == "" {
			return nil
		}
		return config.ValidateRemote("origin", answer)
	})
	if !ok {
		return nil, false
	}
	stored, ok := p.storage("shared")
	if !ok {
		return nil, false
	}
	return &entry{
		name:       name,
		local:      stored == "local",
		Visibility: visibility,
		Remote:     remote,
		Paths:      paths,
	}, true
}

// storage asks which configuration file one repository is written to.
// fallback is the answer Enter keeps, so a reopened question starts on the
// current one.
func (p *prompt) storage(fallback string) (string, bool) {
	return p.selectValue("Store in", fmt.Sprintf("shared = %s, local = %s", config.PublicFile, config.LocalFile),
		fallback, []string{"shared", "local"}, func(answer string) error {
			if answer != "shared" && answer != "local" {
				return errors.New("answer shared or local")
			}
			return nil
		})
}

// selectValue asks for one of choices. A terminal shows them as a list; the
// stable question names the choices and the default in its text.
func (p *prompt) selectValue(title, description, fallback string, choices []string, check func(string) error) (string, bool) {
	if p.plain {
		if description != "" {
			fmt.Fprintln(p.output, description)
		}
		return p.askUntil(question(title+" ["+strings.Join(choices, "/")+"]", fallback), fallback, check)
	}
	answer := fallback
	options := make([]huh.Option[string], 0, len(choices)+1)
	if fallback == "" {
		options = append(options, huh.NewOption("Select...", ""))
	}
	for _, choice := range choices {
		options = append(options, huh.NewOption(choice, choice))
	}
	field := huh.NewSelect[string]().Title(title).Description(description).Options(options...).Value(&answer).Validate(check)
	if !p.run(field) {
		return "", false
	}
	return answer, true
}

func (p *prompt) run(field huh.Field) bool {
	if p.runField != nil {
		if err := p.runField(field); err != nil {
			p.err = err
			return false
		}
		return p.err == nil
	}
	err := huh.NewForm(huh.NewGroup(field)).
		WithInput(p.input).
		// The form draws its own cursor rendering, which is GitOne's, not a
		// repository's, so it writes past the guard.
		WithOutput(ui.Raw(p.output)).
		WithTheme(huh.ThemeFunc(huh.ThemeBase16)).
		Run()
	if err != nil {
		p.err = err
		return false
	}
	return true
}

func (p *prompt) cancelled(output io.Writer) error {
	if p.err != nil && !errors.Is(p.err, huh.ErrUserAborted) {
		return p.err
	}
	return cancelled(output)
}
