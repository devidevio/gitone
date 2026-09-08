// Package repository creates and validates the managed Git repositories.
// Every repository owns independent Git metadata and an independent index
// below .gitone/repositories/ while sharing the project working tree.
package repository

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/devidevio/gitone/internal/config"
	"github.com/devidevio/gitone/internal/git"
	"github.com/devidevio/gitone/internal/guidance"
	"github.com/devidevio/gitone/internal/link"
	"github.com/devidevio/gitone/internal/lock"
	"github.com/devidevio/gitone/internal/worktree"
	"gopkg.in/yaml.v3"
)

const (
	internalDirectory     = ".gitone"
	repositoriesDirectory = "repositories"
	retiredDirectory      = "retired"

	// RetiredFile records, beside the metadata of a retired repository, what
	// that repository was. It travels inside the directory that moves.
	RetiredFile = "retired.yml"

	// MissingIssue identifies a repository problem that other validators may
	// report too.
	MissingIssue = "missing"

	// NotInitialized is the one message every reporter prints for a
	// configured repository without metadata, so status and doctor name the
	// same next steps instead of two different ones.
	NotInitialized = "is not initialized: run gitone reconfigure to create it from the configuration"

	// worktreeValue points from .gitone/repositories/<name> back to the
	// project root. A relative value keeps the project movable.
	worktreeValue = "../../.."
)

// ErrRetirementMove identifies the final metadata rename after retired.yml and
// the destination parent were prepared successfully.
var ErrRetirementMove = errors.New("retirement metadata move failed")

// ignoreEntries are the GitOne paths every project must ignore.
var ignoreEntries = []string{internalDirectory + "/", ".gitone.local.yml"}

// Issue is one independently actionable repository problem.
type Issue struct {
	Kind       string
	Repository string
	Detail     string
}

func (i Issue) Error() string {
	return fmt.Sprintf("REPO001 repository %q %s", i.Repository, i.Detail)
}

// Directory is the native Git directory of one managed repository.
func Directory(root, name string) string {
	return filepath.Join(root, filepath.FromSlash(Path(name)))
}

// Path is the project-relative native Git directory of one managed
// repository, which is what a message a person retypes has to name.
func Path(name string) string {
	return path.Join(internalDirectory, repositoriesDirectory, name)
}

// TracksOriginBranch reports whether branch tracks the exact origin branch
// that GitOne fetches from and publishes to.
func TracksOriginBranch(root, name, branch string) (bool, error) {
	output, err := git.Run(root, "--git-dir="+Directory(root, name),
		"for-each-ref", "--format=%(upstream:remotename)%00%(upstream)", "refs/heads/"+branch)
	want := config.OriginRemote + "\x00refs/remotes/" + config.OriginRemote + "/" + branch
	return strings.TrimSpace(output) == want, err
}

// HeadExists reports whether the repository has a valid HEAD commit. A
// missing branch in an initialized repository is an ordinary unborn HEAD;
// every other failure is returned instead of being mistaken for one.
func HeadExists(root, name string) (bool, error) {
	gitDirectory := "--git-dir=" + Directory(root, name)
	_, err := git.Run(root, gitDirectory, "rev-parse", "--verify", "HEAD")
	if err == nil {
		return true, nil
	}
	branch, branchErr := git.Run(root, gitDirectory, "symbolic-ref", "-q", "HEAD")
	if branchErr != nil {
		return false, err
	}
	existing, refErr := git.Run(root, gitDirectory, "for-each-ref", "--format=%(objectname)", strings.TrimSpace(branch))
	if refErr != nil {
		return false, refErr
	}
	if strings.TrimSpace(existing) != "" {
		return false, err
	}
	return false, nil
}

// Init prepares an existing, uninitialized project. It validates the complete
// working tree first, then creates the missing repositories and the ignore
// entries. It never stages, commits or contacts a remote, and it never
// overwrites existing Git state.
func Init(configuration *config.Config, root string) error {
	release, err := lock.Acquire(root, "gitone init")
	if err != nil {
		return err
	}
	defer release()

	if err := ValidateWorktree(configuration, root, false); err != nil {
		return err
	}
	return Prepare(configuration, root)
}

// WorktreeIssues lists every working-tree problem that keeps a project from
// being initialized. withRootRepository accepts an existing root .git/, which
// migration inspects and removes itself. Setup uses the issues themselves to
// decide which ones the user can still correct.
func WorktreeIssues(configuration *config.Config, root string, withRootRepository bool) ([]*worktree.Issue, error) {
	// .gitignore becomes a managed file, so it must be owned by exactly one
	// repository even when it does not exist yet.
	result, err := worktree.Scan(configuration, root, []string{config.ProjectIgnoreFile})
	if err != nil {
		return nil, err
	}
	var issues []*worktree.Issue
	for _, issue := range result.Issues {
		// The root repository itself is the one issue migration resolves, and
		// it is checked separately there.
		if withRootRepository && issue.Path == ".git" {
			continue
		}
		issues = append(issues, issue)
	}
	return issues, nil
}

// ValidateWorktree reports the working-tree problems of WorktreeIssues as one
// error.
func ValidateWorktree(configuration *config.Config, root string, withRootRepository bool) error {
	issues, err := WorktreeIssues(configuration, root, withRootRepository)
	if err != nil {
		return err
	}
	errorsToJoin := make([]error, len(issues))
	for index := range issues {
		errorsToJoin[index] = issues[index]
	}
	return errors.Join(errorsToJoin...)
}

// Prepare creates the missing repositories and the ignore entries of a
// validated project. The caller holds the project lock.
func Prepare(configuration *config.Config, root string) error {
	if err := os.MkdirAll(filepath.Join(root, internalDirectory, repositoriesDirectory), 0o700); err != nil {
		return err
	}

	var missing []string
	for _, name := range configuration.RepositoryNames() {
		exists, issues := inspect(root, name, configuration.Repositories[name])
		err := join(issues)
		if err != nil {
			return err
		}
		if !exists {
			missing = append(missing, name)
		}
	}
	for _, name := range missing {
		repository := configuration.Repositories[name]
		gitDirectory := Directory(root, name)
		if err := create(root, gitDirectory, configuration.DefaultBranch, repository); err != nil {
			return err
		}
	}
	return UpdateIgnore(root)
}

// Ready is the readiness check every command that works on repository state
// runs before doing any work. A project that was never initialized names the
// command that initializes it; anything else keeps the REPO001 failure and
// points at doctor, because reinitializing over existing metadata is not safe.
func Ready(configuration *config.Config, root string) error {
	var problems []string
	missing := 0
	for _, name := range configuration.RepositoryNames() {
		switch exists, err := validateMetadata(root, name); {
		case err != nil:
			problems = append(problems, err.Error())
		case !exists:
			missing++
			problems = append(problems, invalid(name, NotInitialized).Error())
		}
	}
	switch {
	case len(problems) == 0:
		return nil
	case missing == len(configuration.Repositories):
		return fmt.Errorf("REPO001 no managed repository is initialized: run %s", nextCommand(root))
	}
	return errors.New(strings.Join(problems, "\n") + "\nRun gitone doctor to inspect the project.")
}

// nextCommand names the safe next step for an uninitialized project. Only a
// real root .git/ can be migrated; unsupported .git forms need diagnosis.
func nextCommand(root string) string {
	info, err := os.Lstat(filepath.Join(root, ".git"))
	switch {
	case err == nil && info.IsDir():
		return "gitone migrate to convert the existing .git/"
	case err == nil || !errors.Is(err, os.ErrNotExist):
		return "gitone doctor to inspect the existing .git"
	default:
		return "gitone init"
	}
}

// Check reports every independent problem of one configured repository. It
// applies exactly the rules init establishes.
func Check(root, name string, repository config.Repository) []Issue {
	exists, issues := inspect(root, name, repository)
	if !exists && len(issues) == 0 {
		issues = append(issues, Issue{Kind: MissingIssue, Repository: name, Detail: NotInitialized})
	}
	return issues
}

func inspect(root, name string, repository config.Repository) (bool, []Issue) {
	exists, err := validateMetadata(root, name)
	if err != nil {
		prefix := fmt.Sprintf("REPO001 repository %q ", name)
		return exists, []Issue{{Repository: name, Detail: strings.TrimPrefix(err.Error(), prefix)}}
	}
	if !exists {
		return false, nil
	}
	return true, verifyConfiguration(root, Directory(root, name), name, repository)
}

func validateMetadata(root, name string) (bool, error) {
	gitDirectory := Directory(root, name)
	switch info, err := os.Stat(gitDirectory); {
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	case err != nil:
		return false, err
	case !info.IsDir():
		return false, invalid(name, "exists and is not a directory")
	}
	if _, err := git.Run(root, "--git-dir="+gitDirectory, "--work-tree="+root, "rev-parse", "--git-dir"); err != nil {
		return false, invalid(name, "exists but is not a Git repository")
	}
	bare, err := git.Run(root, "--git-dir="+gitDirectory, "--work-tree="+root, "config", "core.bare")
	if err != nil || strings.TrimSpace(bare) != "false" {
		return false, invalid(name, "exists as a bare repository")
	}
	configured, err := git.Run(root, "--git-dir="+gitDirectory, "--work-tree="+root, "config", "core.worktree")
	if err != nil || strings.TrimSpace(configured) != worktreeValue {
		return false, invalid(name, "does not use the project working tree")
	}
	return true, nil
}

// Create initializes one missing repository on branch. gitone reconfigure
// creates a repository the configuration added on the branch the project is
// currently on, which is not always the configured default branch.
func Create(root, name, branch string, configured config.Repository) error {
	return create(root, Directory(root, name), branch, configured)
}

// create initializes one repository whose working tree is the project root.
// Its metadata, index, HEAD, refs and hooks stay inside gitDirectory, so the
// repositories cannot interfere with one another.
func create(root, gitDirectory, branch string, repository config.Repository) error {
	if err := os.MkdirAll(filepath.Dir(gitDirectory), 0o700); err != nil {
		return err
	}
	if _, err := git.Run(root, "--git-dir="+gitDirectory, "--work-tree="+root, "init", "--quiet", "--initial-branch="+branch); err != nil {
		return err
	}
	if _, err := git.Run(root, "--git-dir="+gitDirectory, "config", "core.worktree", worktreeValue); err != nil {
		return err
	}
	return configureRemotes(root, gitDirectory, repository, false)
}

func configureRemotes(root, gitDirectory string, repository config.Repository, originExists bool) error {
	for _, remoteName := range repository.RemoteNames() {
		action := "add"
		if originExists && remoteName == config.OriginRemote {
			action = "set-url"
		}
		if _, err := git.Run(root, "--git-dir="+gitDirectory, "remote", action, remoteName, repository.Remotes[remoteName]); err != nil {
			return err
		}
	}
	return nil
}

// verifyConfiguration checks the remotes that init owns in addition to the
// repository metadata already checked by inspect. The branch a repository has
// checked out is not verified here: gitone switch moves the whole project off
// the configured default branch, and that the repositories still share one
// branch is a project-wide check.
func verifyConfiguration(root, gitDirectory, name string, repository config.Repository) []Issue {
	var issues []Issue
	for _, remoteName := range repository.RemoteNames() {
		configured := repository.Remotes[remoteName]
		existing, err := git.Run(root, "--git-dir="+gitDirectory, "config", "remote."+remoteName+".url")
		if err != nil {
			issues = append(issues, Issue{Repository: name, Detail: fmt.Sprintf(
				"is missing configured remote %q: run gitone reconfigure", remoteName)})
			continue
		}
		if strings.TrimSpace(existing) != configured {
			issues = append(issues, Issue{Repository: name, Detail: fmt.Sprintf(
				"remote %q is %s, not the configured %s: run gitone reconfigure", remoteName, strings.TrimSpace(existing), configured)})
		}
	}
	return issues
}

func join(issues []Issue) error {
	errorsToJoin := make([]error, len(issues))
	for index := range issues {
		errorsToJoin[index] = issues[index]
	}
	return errors.Join(errorsToJoin...)
}

// UpdateIgnore appends the missing GitOne entries to the project .gitignore.
// Existing bytes stay untouched, so comments and entries survive and repeated
// runs change nothing.
func UpdateIgnore(root string) error {
	name := filepath.Join(root, config.ProjectIgnoreFile)
	contents, err := os.ReadFile(name)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	lines := strings.Split(string(contents), "\n")
	missing := slices.DeleteFunc(slices.Clone(ignoreEntries), func(entry string) bool {
		return slices.ContainsFunc(lines, func(line string) bool { return strings.TrimSpace(line) == entry })
	})
	if len(missing) == 0 {
		return nil
	}

	addition := strings.Join(missing, "\n") + "\n"
	if len(contents) != 0 && !strings.HasSuffix(string(contents), "\n") {
		addition = "\n" + addition
	}
	file, err := os.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	if _, err := file.WriteString(addition); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func invalid(name, detail string) error {
	return fmt.Errorf("REPO001 repository %q %s", name, detail)
}

// Attach makes an existing Git directory below .gitone/repositories/ use the
// project working tree and configured remotes. gitone clone adopts a native
// clone this way, whose origin already exists.
func Attach(root, name string, repository config.Repository) error {
	gitDirectory := Directory(root, name)
	if _, err := git.Run(root, "--git-dir="+gitDirectory, "config", "core.worktree", worktreeValue); err != nil {
		return err
	}
	return configureRemotes(root, gitDirectory, repository, true)
}

// RetiredPath is the project-relative directory one repository's metadata is
// moved to. It is computed when the move is planned, so the preview, the
// journal and the move itself all name the same directory.
func RetiredPath(name string) string {
	return path.Join(internalDirectory, retiredDirectory,
		time.Now().UTC().Format("20060102T150405Z")+"-"+name)
}

// Retire moves the complete metadata of one repository to RetiredPath(name)
// and returns the project-relative destination.
func Retire(root, name string) (string, error) {
	relative := RetiredPath(name)
	if err := RetireTo(root, name, relative); err != nil {
		return "", err
	}
	return relative, nil
}

// RetireTo moves the complete metadata of one repository to a planned
// destination below the project root, beside a retired.yml recording what the
// repository was. Nothing is ever deleted: the destination keeps the depth
// .gitone/repositories/<name> had, so the recorded core.worktree stays valid
// and the repository can still be read with plain git --git-dir=... Removing
// it is a deliberate human action.
func RetireTo(root, name, relative string) error {
	// The note is written into the directory that moves, so an interrupted
	// rename leaves it exactly where the finishing mv takes it.
	if err := writeNote(root, name); err != nil {
		return err
	}
	destination := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	if err := os.Rename(Directory(root, name), destination); err != nil {
		return fmt.Errorf("%w: %w", ErrRetirementMove, err)
	}
	return nil
}

// Move renames the metadata directory of one repository, which is the whole of
// a declared rename: the bytes never leave the disk, so the history, the
// index, the reflog and the hooks survive without anything being fetched,
// cloned or copied.
func Move(root, from, to string) error {
	return os.Rename(Directory(root, from), Directory(root, to))
}

// Restore moves a retired metadata directory back to
// .gitone/repositories/<name>, which is what gitone abort does with a
// retirement it recorded.
func Restore(root, name, relative string) error {
	if err := os.MkdirAll(filepath.Dir(Directory(root, name)), 0o700); err != nil {
		return err
	}
	return os.Rename(filepath.Join(root, filepath.FromSlash(relative)), Directory(root, name))
}

// note is what retired.yml records: enough to identify the repository, find
// its history again and see when it left the configuration.
type note struct {
	Name    string            `yaml:"name"`
	Branch  string            `yaml:"branch,omitempty"`
	Commit  string            `yaml:"commit,omitempty"`
	Retired string            `yaml:"retired"`
	Remotes map[string]string `yaml:"remotes,omitempty"`
}

// writeNote records the repository beside its metadata, read from native Git
// itself so the file describes what is actually being moved.
func writeNote(root, name string) error {
	gitDirectory := "--git-dir=" + Directory(root, name)
	branch, err := git.Run(root, gitDirectory, "branch", "--show-current")
	if err != nil {
		return err
	}
	recorded := note{Name: name, Branch: strings.TrimSpace(branch), Retired: time.Now().UTC().Format(time.RFC3339)}
	headExists, err := HeadExists(root, name)
	if err != nil {
		return err
	}
	if headExists {
		commit, err := git.Run(root, gitDirectory, "rev-parse", "--verify", "HEAD")
		if err != nil {
			return err
		}
		recorded.Commit = strings.TrimSpace(commit)
		if recorded.Branch == "" {
			recorded.Branch = "(detached)"
		}
	}
	listed, err := git.Run(root, gitDirectory, "remote")
	if err != nil {
		return err
	}
	for _, remoteName := range strings.Fields(listed) {
		url, err := git.Run(root, gitDirectory,
			"config", "--default", "", "--get", "remote."+remoteName+".url")
		if err != nil {
			return err
		}
		if recorded.Remotes == nil {
			recorded.Remotes = map[string]string{}
		}
		recorded.Remotes[remoteName] = strings.TrimSpace(url)
	}
	contents, err := yaml.Marshal(recorded)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(Directory(root, name), RetiredFile), contents, 0o600)
}

// Existing lists every repository that has metadata below
// .gitone/repositories/, whether or not the configuration still names it.
func Existing(root string) ([]string, error) {
	return directories(filepath.Join(root, internalDirectory, repositoriesDirectory))
}

// Retired lists the project-relative directories below .gitone/retired/,
// oldest first. GitOne never removes one.
func Retired(root string) ([]string, error) {
	listed, err := directories(filepath.Join(root, internalDirectory, retiredDirectory))
	if err != nil {
		return nil, err
	}
	for index, name := range listed {
		listed[index] = path.Join(internalDirectory, retiredDirectory, name)
	}
	return listed, nil
}

// directories lists the subdirectory names of one directory, sorted, and
// reports a directory that does not exist as no entries at all.
func directories(directory string) ([]string, error) {
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var listed []string
	for _, entry := range entries {
		if entry.IsDir() {
			listed = append(listed, entry.Name())
		}
	}
	return listed, nil
}

// Unborn is the report of a repository that has no commit yet, together with
// the next step a person takes. gitone clone and gitone reconfigure both
// leave such a repository behind and print the same one.
func Unborn(name, reason string) string {
	return fmt.Sprintf("Repository %q has no commit yet: %s.\n%s Include local overrides in %s.\n",
		name, reason, guidance.UnbornRepair, config.LocalFile)
}

// Reference is the commit a ref of one repository points at, or the empty
// string when the ref does not exist.
func Reference(root, name, ref string) (string, error) {
	output, err := git.Run(root, "--git-dir="+Directory(root, name),
		"for-each-ref", "--format=%(refname)%00%(objectname)", ref)
	if err != nil {
		return "", err
	}
	for line := range strings.SplitSeq(strings.TrimSpace(output), "\n") {
		name, commit, found := strings.Cut(line, "\x00")
		if found && name == ref {
			return commit, nil
		}
	}
	return "", nil
}

// gitlinkMode is the Git entry mode of a submodule. GitOne does not manage
// one, so it can never be the target of an accepted link either.
const gitlinkMode = "160000"

// TreeIssue is one entry of an incoming tree that must not reach the working
// tree, with the reason a person is shown. Issue keeps the classification, so
// a command can tell an unassigned path from every hard ownership failure
// without reading the reason text; an unsafe link is always OwnerUnsafe.
type TreeIssue struct {
	Repository string
	Path       string
	Issue      config.OwnerIssue
	Reason     string
}

// entries is one index or tree of a repository: the Git entry mode and object
// of every path it holds. It answers the safe-link questions for a source
// that has no filesystem behind it, so an index, a tree and the working tree
// all decide a link with the same policy.
type entries struct {
	root        string
	matcher     *config.Matcher
	modes       map[string]string
	directories map[string]bool
	entries     []entry
}

type entry struct {
	repository string
	source     string
	mode       string
	object     string
	path       string
}

func newEntries(matcher *config.Matcher, root string) *entries {
	return &entries{root: root, matcher: matcher, modes: map[string]string{}, directories: map[string]bool{}}
}

func (e *entries) add(repository, source, mode, object, entryPath string) {
	e.modes[entryPath] = mode
	e.entries = append(e.entries, entry{repository, source, mode, object, entryPath})
	for parent := path.Dir(entryPath); parent != "."; parent = path.Dir(parent) {
		e.directories[parent] = true
	}
}

func (e *entries) Kind(relativePath string) (link.Kind, error) {
	switch mode, listed := e.modes[relativePath]; {
	case listed && mode == link.Mode:
		return link.Symbolic, nil
	case listed && mode == gitlinkMode:
		return link.Other, nil
	case listed:
		return link.Regular, nil
	case e.directories[relativePath]:
		return link.Directory, nil
	}
	return link.Missing, nil
}

// Relevant lists the entries below directory. A tree and an index have no
// ignored paths, so every entry below it counts.
func (e *entries) Relevant(directory string) ([]string, error) {
	var names []string
	for _, current := range e.entries {
		if strings.HasPrefix(current.path, directory+"/") && !slices.Contains(names, current.path) {
			names = append(names, current.path)
		}
	}
	slices.Sort(names)
	return names, nil
}

func (e *entries) Owner(relativePath string) (string, config.OwnerIssue, string) {
	return e.matcher.Owner(relativePath)
}

// linkIssue reports why one mode 120000 entry must not be materialized. The
// stored target is read from its blob, so no filesystem link is ever
// followed.
func (e *entries) linkIssue(current entry) (string, error) {
	target, err := git.Run(e.root, "--git-dir="+Directory(e.root, current.source), "cat-file", "blob", current.object)
	if err != nil {
		return "", err
	}
	if _, reason := link.Check(e, current.path, target); reason != "" {
		return "symbolic link to " + target + ": " + reason, nil
	}
	return "", nil
}

// issues reports every entry the repository must not manage: an unsafe
// symbolic link, or a path it does not own.
func (e *entries) issues() ([]TreeIssue, error) {
	var issues []TreeIssue
	for _, current := range e.entries {
		issue, reason := e.matcher.UnownedIssue(current.repository, current.path)
		if current.mode == link.Mode {
			// An unsafe link decides before ownership does, because it is
			// refused even in a path the repository owns.
			problem, err := e.linkIssue(current)
			if err != nil {
				return nil, err
			}
			if problem != "" {
				issue, reason = config.OwnerUnsafe, problem
			}
		}
		if reason != "" {
			issues = append(issues, TreeIssue{Repository: current.repository, Path: current.path, Issue: issue, Reason: reason})
		}
	}
	return issues, nil
}

// linkIssues reports only the unsafe symbolic links, for callers that check
// ownership themselves.
func (e *entries) linkIssues() ([]TreeIssue, error) {
	var issues []TreeIssue
	for _, current := range e.entries {
		if current.mode != link.Mode {
			continue
		}
		reason, err := e.linkIssue(current)
		if err != nil {
			return nil, err
		}
		if reason != "" {
			issues = append(issues, TreeIssue{Repository: current.repository, Path: current.path, Issue: config.OwnerUnsafe, Reason: reason})
		}
	}
	return issues, nil
}

// treeEntries reads one commit tree of a repository.
func addTree(listed *entries, name, commit string) error {
	return addTreeFrom(listed, name, name, commit)
}

// addTreeFrom reads a commit from source while ownership is checked under
// name. They differ only while reconfigure validates a declared rename before
// moving its metadata directory.
func addTreeFrom(listed *entries, name, source, commit string) error {
	output, err := git.Run(listed.root, "--git-dir="+Directory(listed.root, source),
		"ls-tree", "-r", "--full-tree", "-z", commit)
	if err != nil {
		return err
	}
	for record := range strings.SplitSeq(output, "\x00") {
		if record == "" {
			continue
		}
		// An ls-tree record is "<mode> <type> <object>\t<path>".
		mode, rest, _ := strings.Cut(record, " ")
		header, entryPath, tabbed := strings.Cut(rest, "\t")
		if !tabbed {
			return fmt.Errorf("GIT001 repository %q has an unreadable tree entry in %s", name, short(commit))
		}
		_, object, _ := strings.Cut(header, " ")
		listed.add(name, source, mode, object, entryPath)
	}
	return nil
}

// TreeIssues reports every entry the given commits would put into their shared
// working tree that its repository must not materialize: an unsafe symbolic
// link, or a path it does not own.
func TreeIssues(matcher *config.Matcher, root string, commits map[string]string) ([]TreeIssue, error) {
	return TreeIssuesFrom(matcher, root, commits, nil)
}

// TreeIssuesFrom reports tree issues while selected logical repository names
// still use another repository's metadata directory. sources maps the logical
// name to that existing directory name.
func TreeIssuesFrom(matcher *config.Matcher, root string, commits, sources map[string]string) ([]TreeIssue, error) {
	listed := newEntries(matcher, root)
	for _, name := range slices.Sorted(maps.Keys(commits)) {
		source := sources[name]
		if source == "" {
			source = name
		}
		if err := addTreeFrom(listed, name, source, commits[name]); err != nil {
			return nil, fmt.Errorf("repository %q tree cannot be read: %w", name, err)
		}
	}
	return listed.issues()
}

// TreeFiles maps every path of one commit tree to the object holding it, so a
// committed file can be read and validated before a checkout materializes it.
func TreeFiles(root, name, commit string) (map[string]string, error) {
	listed := newEntries(nil, root)
	if err := addTree(listed, name, commit); err != nil {
		return nil, fmt.Errorf("repository %q tree cannot be read: %w", name, err)
	}
	files := make(map[string]string, len(listed.entries))
	for _, current := range listed.entries {
		files[current.path] = current.object
	}
	return files, nil
}

// Blob is the content of one object of a repository.
func Blob(root, name, object string) (string, error) {
	return git.Run(root, "--git-dir="+Directory(root, name), "cat-file", "blob", object)
}

// TreeLinkIssues reports only the unsafe symbolic links of one commit tree.
// Push checks every outgoing commit with it while validating ownership from
// the changed paths of that commit.
func TreeLinkIssues(matcher *config.Matcher, root, name, commit string) ([]TreeIssue, error) {
	listed := newEntries(matcher, root)
	if err := addTree(listed, name, commit); err != nil {
		return nil, err
	}
	return listed.linkIssues()
}

// IndexLinkIssues reports unsafe symbolic links across the given indexes. An
// index may be a prepared copy, so a command can refuse its complete result
// before applying it.
func IndexLinkIssues(matcher *config.Matcher, root string, indexes map[string]string) ([]TreeIssue, error) {
	listed, err := indexEntries(matcher, root, indexes)
	if err != nil {
		return nil, err
	}
	return listed.linkIssues()
}

// IndexIssues checks ownership and symbolic links across the resulting indexes,
// including prepared copies, before a restore applies them or its configuration.
func IndexIssues(matcher *config.Matcher, root string, indexes map[string]string) ([]TreeIssue, error) {
	listed, err := indexEntries(matcher, root, indexes)
	if err != nil {
		return nil, err
	}
	return listed.issues()
}

func indexEntries(matcher *config.Matcher, root string, indexes map[string]string) (*entries, error) {
	listed := newEntries(matcher, root)
	for _, name := range slices.Sorted(maps.Keys(indexes)) {
		if err := addIndex(listed, root, name, indexes[name]); err != nil {
			return nil, fmt.Errorf("repository %q index cannot be read: %w", name, err)
		}
	}
	return listed, nil
}

func addIndex(listed *entries, root, name, index string) error {
	output, err := git.RunIndexed(root, index, "", "--no-optional-locks",
		"--git-dir="+Directory(root, name), "--work-tree="+root, "ls-files", "-s", "-z")
	if err != nil {
		return err
	}
	for record := range strings.SplitSeq(output, "\x00") {
		if record == "" {
			continue
		}
		// An ls-files -s record is "<mode> <object> <stage>\t<path>".
		mode, rest, _ := strings.Cut(record, " ")
		object, rest, _ := strings.Cut(rest, " ")
		_, entryPath, tabbed := strings.Cut(rest, "\t")
		if !tabbed {
			return fmt.Errorf("GIT001 repository %q has an unreadable index entry", name)
		}
		listed.add(name, name, mode, object, entryPath)
	}
	return nil
}

// ReservedIssues reports GitOne's own paths in a managed index or history.
// Doctor and clone use the same check so a clone is never published in a
// state doctor immediately refuses.
func ReservedIssues(configuration *config.Config, root string) []error {
	pathspec := append([]string{"--"}, config.ReservedPaths...)
	var issues []error
	for _, name := range configuration.RepositoryNames() {
		gitDirectory := Directory(root, name)
		if _, err := os.Stat(gitDirectory); err != nil {
			continue
		}
		indexed, err := git.Run(root, append([]string{"--no-optional-locks",
			"--git-dir=" + gitDirectory, "--work-tree=" + root, "ls-files", "-z"}, pathspec...)...)
		switch {
		case err != nil:
			issues = append(issues, fmt.Errorf("repository %q index cannot be read: %s", name, firstLine(err)))
		case strings.Trim(indexed, "\x00") != "":
			paths := slices.DeleteFunc(strings.Split(indexed, "\x00"), func(path string) bool { return path == "" })
			issues = append(issues, fmt.Errorf("PATH003 repository %q index contains %s", name, strings.Join(paths, ", ")))
		}
		committed, err := git.Run(root, append([]string{"--git-dir=" + gitDirectory,
			"rev-list", "--all", "--full-history", "--max-count=1"}, pathspec...)...)
		switch {
		case err != nil:
			issues = append(issues, fmt.Errorf("repository %q history cannot be read: %s", name, firstLine(err)))
		case strings.TrimSpace(committed) != "":
			issues = append(issues, fmt.Errorf("PATH003 repository %q commit %s contains reserved GitOne paths", name, short(committed)))
		}
	}
	return issues
}

func firstLine(err error) string {
	line, _, _ := strings.Cut(err.Error(), "\n")
	return line
}

func short(commit string) string {
	commit = strings.TrimSpace(commit)
	if len(commit) <= 7 {
		return commit
	}
	return commit[:7]
}

// IndexPath is the index file of one managed repository. Every operation that
// saves or restores an exact index addresses it through this path.
func IndexPath(root, name string) string {
	return filepath.Join(Directory(root, name), "index")
}

// SaveIndex copies the current index of one repository to destination, so an
// interrupted operation can put back the exact index it started from.
func SaveIndex(root, name, destination string) error {
	contents, err := os.ReadFile(IndexPath(root, name))
	if err != nil {
		return err
	}
	return os.WriteFile(destination, contents, 0o600)
}

// RestoreIndex puts a saved index back without leaving a partial file behind.
//
// ponytail: no fsync, this survives a process crash but not a power loss.
func RestoreIndex(root, name, source string) error {
	contents, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	target := IndexPath(root, name)
	temporary := target + ".gitone"
	if err := os.WriteFile(temporary, contents, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, target)
}

// Matches reports whether the index and the working tree of one repository
// hold exactly commit. Untracked files are no difference; native Git refuses
// to overwrite one on its own. Excluded paths are left out of the comparison,
// which is how a caller ignores a file it deliberately changed itself.
func Matches(root, name, commit string, excluded ...string) bool {
	arguments := []string{"--git-dir=" + Directory(root, name), "--work-tree=" + root}
	// A file whose stat data changed without its content is not a difference,
	// so the index is refreshed first. Its result is the refusal below.
	_, _ = git.Run(root, append(slices.Clone(arguments), "update-index", "-q", "--refresh")...)
	arguments = append(arguments, "diff-index", "--quiet", commit, "--")
	if len(excluded) != 0 {
		arguments = append(arguments, ":/")
		for _, entryPath := range excluded {
			arguments = append(arguments, ":(exclude,literal)"+entryPath)
		}
	}
	_, err := git.Run(root, arguments...)
	return err == nil
}
