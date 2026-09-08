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
	"github.com/devidevio/gitone/internal/git"
	"github.com/devidevio/gitone/internal/repository"
	"github.com/devidevio/gitone/internal/ui"
	"github.com/devidevio/gitone/internal/worktree"
)

// noOrigin is why an added repository stays unborn, in the wording gitone
// clone already uses for a repository it cannot fetch.
const noOrigin = "no configured " + config.OriginRemote + " remote"

// addition is one configured repository the project does not have yet. It is
// created exactly as gitone init creates one, except that it starts on the
// branch the project is currently on rather than on the configured default
// branch: every repository of a project must agree on one branch.
type addition struct {
	name       string
	visibility string
	origin     string
	branch     string
	patterns   []string
	// claims counts, per configured pattern, the existing local paths the
	// pattern would put into this repository.
	claims map[string]int
	// commit is what the repository is checked out at, and stays empty while
	// it has no origin to take a branch from; unborn then says why.
	commit string
	unborn string
	// materialized marks a repository a recovered reconfiguration already
	// checked out before it was interrupted. Its files are on disk on
	// purpose, so they are not the existing paths a checkout must not touch.
	materialized bool
}

// added lists every configured repository without metadata, with the branch
// the project is on and the existing local paths its patterns claim.
func added(configuration *config.Config, root, branch string, inventory *worktree.Result) ([]*addition, error) {
	paths, err := existingLocalPaths(root, inventory)
	if err != nil {
		return nil, err
	}

	var additions []*addition
	for _, name := range configuration.RepositoryNames() {
		switch _, err := os.Stat(repository.Directory(root, name)); {
		case err == nil:
			continue
		case !errors.Is(err, os.ErrNotExist):
			return nil, err
		}
		configured := configuration.Repositories[name]
		current := &addition{
			name: name, visibility: configured.Visibility, origin: configured.Remotes[config.OriginRemote],
			branch: branch, patterns: slices.Clone(configured.Paths), claims: map[string]int{},
		}
		for _, path := range paths {
			for _, pattern := range current.patterns {
				if config.Owns(pattern, path) {
					current.claims[pattern]++
				}
			}
		}
		if current.origin == "" {
			current.unborn = noOrigin
		}
		additions = append(additions, current)
	}
	return additions, nil
}

// existingLocalPaths lists every concrete local path an addition can make
// publishable. The ordinary inventory contains relevant paths; ignored trees
// have to be opened explicitly because the scanner deliberately collapses
// them to one directory entry.
func existingLocalPaths(root string, inventory *worktree.Result) ([]string, error) {
	paths := maps.Clone(inventory.Owners)
	hidden, err := worktree.Ignored(root, "")
	if err != nil {
		return nil, err
	}
	for _, entry := range hidden {
		if config.IsReserved(strings.TrimSuffix(entry, "/")) {
			continue
		}
		if !strings.HasSuffix(entry, "/") {
			paths[entry] = ""
			continue
		}
		entries, err := worktree.Ignored(root, entry)
		if err != nil {
			return nil, err
		}
		for _, name := range entries {
			if !config.IsReserved(name) {
				paths[name] = ""
			}
		}
	}
	return slices.Sorted(maps.Keys(paths)), nil
}

// preview names one addition with everything a person reviews before it is
// created: its visibility, its remote, its patterns and, per pattern, how
// much of the working tree that pattern already claims. A wide pattern on a
// repository somebody else controls is the sharpest edge of the whole change,
// so the count is printed even when it is zero.
func (a *addition) preview(output io.Writer) {
	fmt.Fprintf(output, "  + %s   %s   %s\n", a.name, a.visibility, or(a.origin))
	for _, pattern := range a.patterns {
		fmt.Fprintf(output, "      paths: %s   (claims %s)\n", pattern, claimed(a.claims[pattern]))
	}
	if a.origin == "" {
		fmt.Fprintf(output, "      %s: created unborn on %s, with nothing to fetch or check out\n", noOrigin, a.branch)
		return
	}
	fmt.Fprintf(output, "      created on %s and checked out at %s/%s\n", a.branch, config.OriginRemote, a.branch)
}

// claimed renders how much of the working tree one pattern already covers.
func claimed(count int) string {
	if count == 1 {
		return "1 existing local path"
	}
	return fmt.Sprintf("%d existing local paths", count)
}

// build creates every added repository with the project working tree, its
// core.worktree and its configured remotes, exactly as gitone init does.
func build(root string, configuration *config.Config, additions []*addition, record func() error) error {
	for _, current := range additions {
		// A recovered reconfiguration created part of the plan already.
		switch _, err := os.Stat(repository.Directory(root, current.name)); {
		case err == nil:
			continue
		case !errors.Is(err, os.ErrNotExist):
			return err
		}
		if err := repository.Create(root, current.name, current.branch, configuration.Repositories[current.name]); err != nil {
			return fmt.Errorf("%s repository %q could not be created on %s: %s",
				reconfigureFailed, current.name, current.branch, detail(err))
		}
		if err := record(); err != nil {
			return err
		}
	}
	return nil
}

// born reads the commit every added repository is checked out at from the
// origin it was just fetched from. A configured origin without the branch the
// project is on is refused: the repository would stay empty on a branch that
// exists nowhere, and guessing another branch is not this command's decision.
func born(root string, additions []*addition) error {
	var issues []string
	for _, current := range additions {
		if current.origin == "" {
			continue
		}
		commit, err := repository.Reference(root, current.name, "refs/remotes/"+config.OriginRemote+"/"+current.branch)
		if err != nil {
			return err
		}
		if commit == "" {
			issues = append(issues, fmt.Sprintf(
				"%s repository %q has no %s/%s at %s: the project is on %s, so push that branch there or remove the %s until the repository is ready",
				reconfigureFailed, current.name, config.OriginRemote, current.branch, current.origin,
				current.branch, config.OriginRemote))
			continue
		}
		switch {
		case current.commit == "":
			current.commit = commit
		case !ancestor(root, current.name, current.commit, commit):
			issues = append(issues, fmt.Sprintf(
				"%s repository %q recorded target %s is no longer contained by %s/%s at %s",
				reconfigureFailed, current.name, short(current.commit), config.OriginRemote,
				current.branch, current.origin))
			continue
		}
		head, err := repository.Reference(root, current.name, "refs/heads/"+current.branch)
		if err != nil {
			return err
		}
		current.materialized = head == commit
	}
	if len(issues) == 0 {
		return nil
	}
	return errors.New(strings.Join(issues, "\n"))
}

// feasible refuses every added tree that must not reach the working tree. It
// completes before the first file is written, so nothing below is a partial
// result:
//
//   - the complete projected tree set, every existing repository at its HEAD
//     and every addition at its target commit, is validated against the
//     effective matcher, so an added repository can never carry a path it does
//     not own, an unsafe link or a reserved path;
//   - a path the tree carries that already exists in the working tree is
//     refused. An existing local path that no repository manages today and
//     that the addition would track is publishable the moment it applies,
//     which is the exposure task 0052 refuses with or without any flag; every
//     other one would be overwritten by the checkout.
func feasible(configuration *config.Config, root, branch string, moves map[string]string,
	additions []*addition, inventory *worktree.Result) error {
	matcher, err := configuration.Matcher()
	if err != nil {
		return err
	}
	// Every repository of a project is on the same branch, so one branch name
	// names the commit each of them currently holds. A repository that has no
	// commit on it yet contributes no tree.
	commits := map[string]string{}
	for _, name := range configuration.RepositoryNames() {
		commit, err := repository.Reference(root, name, "refs/heads/"+branch)
		if err != nil {
			return err
		}
		if commit != "" {
			commits[name] = commit
		}
	}
	for _, current := range additions {
		if current.commit != "" {
			commits[current.name] = current.commit
		}
	}
	// A moving repository ends at the commit the accepted source carries, not
	// at the one its branch holds now.
	for name, commit := range moves {
		commits[name] = commit
	}
	refused, err := repository.TreeIssues(matcher, root, commits)
	if err != nil {
		return err
	}
	var issues []string
	for _, entry := range refused {
		// A path a moving repository newly introduces without an owner was
		// already decided before the preview: refused there, or accepted as
		// the ownership the confirmed assignment writes after these checkouts.
		if entry.Issue == config.OwnerUnassigned && moves[entry.Repository] != "" {
			continue
		}
		issues = append(issues, fmt.Sprintf("%s repository %q must not manage %s from %s: %s",
			reconfigureFailed, entry.Repository, entry.Path, short(commits[entry.Repository]), entry.Reason))
	}

	paths, err := existingLocalPaths(root, inventory)
	if err != nil {
		return err
	}
	var exposed, overwritten []string
	for _, current := range additions {
		files := map[string]string{}
		if current.commit != "" {
			files, err = repository.TreeFiles(root, current.name, current.commit)
			if err != nil {
				return err
			}
		}
		for _, entry := range paths {
			if !slices.ContainsFunc(current.patterns, func(pattern string) bool {
				return config.Owns(pattern, entry)
			}) {
				continue
			}
			line := fmt.Sprintf("  %s  -> %s", entry, current.name)
			_, incoming := files[entry]
			if current.materialized && incoming {
				continue
			}
			if !incoming || inventory.Owners[entry] == "" {
				exposed = append(exposed, line)
				continue
			}
			overwritten = append(overwritten, line)
		}
	}
	if len(exposed) != 0 {
		issues = append(issues, fmt.Sprintf(
			"%s the configuration would put existing local ignored or unassigned paths into a repository:\n%s\n"+
				"Protect, move, remove or explicitly reconfigure each path locally, then run %s again.\n"+
				"No flag ever accepts this.", reconfigureFailed, strings.Join(exposed, "\n"), Command))
	}
	if len(overwritten) != 0 {
		issues = append(issues, fmt.Sprintf(
			"%s the added repositories would overwrite existing working-tree files:\n%s\n"+
				"Move or remove each file locally, then run %s again.",
			reconfigureFailed, strings.Join(overwritten, "\n"), Command))
	}
	if len(issues) == 0 {
		return nil
	}
	return errors.New(strings.Join(issues, "\n"))
}

// checkout puts one added repository on the project branch: the branch points
// at the fetched origin commit, tracks that branch, and the index and the
// working-tree files are written. The index starts empty, so this only adds
// the repository's own validated paths and removes nothing.
func checkout(root string, current *addition) error {
	gitDirectory := "--git-dir=" + repository.Directory(root, current.name)
	if _, err := git.Run(root, gitDirectory, "update-ref", "refs/heads/"+current.branch, current.commit); err != nil {
		return err
	}
	for _, setting := range [][2]string{
		{"branch." + current.branch + ".remote", config.OriginRemote},
		{"branch." + current.branch + ".merge", "refs/heads/" + current.branch},
	} {
		if _, err := git.Run(root, gitDirectory, "config", setting[0], setting[1]); err != nil {
			return err
		}
	}
	_, err := git.Run(root, gitDirectory, "--work-tree="+root, "reset", "--hard", "--quiet", current.branch)
	return err
}

// materialize checks out every added repository that has a commit.
func materialize(root string, additions []*addition, record func() error) error {
	for _, current := range additions {
		if current.commit == "" {
			continue
		}
		if err := checkout(root, current); err != nil {
			return fmt.Errorf("%s repository %q could not be checked out at %s: %s",
				reconfigureFailed, current.name, short(current.commit), detail(err))
		}
		if err := record(); err != nil {
			return err
		}
	}
	return nil
}

// discard removes a repository this command created, together with the
// working-tree files its checkout wrote. Nothing else can be there: the
// checkout was refused for every path that already existed.
func discard(root, name, branch string) error {
	head, err := repository.Reference(root, name, "refs/heads/"+branch)
	if err != nil {
		return err
	}
	if head != "" {
		files, err := repository.TreeFiles(root, name, head)
		if err != nil {
			return err
		}
		for _, entry := range slices.Sorted(maps.Keys(files)) {
			full := filepath.Join(root, filepath.FromSlash(entry))
			if err := os.Remove(full); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			prune(root, filepath.Dir(full))
		}
	}
	return os.RemoveAll(repository.Directory(root, name))
}

// prune removes the directories a discarded checkout left empty, up to but
// never including the project root. A directory that still holds anything
// stops it, so no path a person put there is ever removed.
func prune(root, directory string) {
	for directory != root && strings.HasPrefix(directory, root+string(filepath.Separator)) {
		if os.Remove(directory) != nil {
			return
		}
		directory = filepath.Dir(directory)
	}
}

// refs is the complete ref state of one repository, recorded so that gitone
// abort can tell the directory this command created from one somebody worked
// in afterwards.
func refs(root, name string) ([]string, error) {
	return refsIn(root, repository.Directory(root, name))
}

// refsIn reads the same ref state from a Git directory named directly, which
// is how a retired directory outside .gitone/repositories/ is compared.
func refsIn(root, gitDirectory string) ([]string, error) {
	output, err := git.Run(root, "--git-dir="+gitDirectory,
		"for-each-ref", "--format=%(refname) %(objectname)")
	if err != nil {
		return nil, err
	}
	listed := strings.Split(strings.TrimSpace(output), "\n")
	return slices.DeleteFunc(listed, func(line string) bool { return line == "" }), nil
}

// line is what one created repository ended up holding, in the wording both
// the reconfiguration report and a recovery report use.
func (a *addition) line() string {
	if a.commit == "" {
		return "created, unborn " + a.branch + ": " + a.unborn
	}
	return fmt.Sprintf("created and checked out %s at %s", a.branch, short(a.commit))
}

// reportAdditions states what every created repository holds, one line each.
func reportAdditions(output io.Writer, additions []*addition, style ui.Styles) {
	for _, current := range additions {
		if current.commit == "" {
			fmt.Fprintln(output, style.Muted.Render("  "+current.name+" "+current.line()))
			continue
		}
		fmt.Fprintln(output, style.Success.Render("✓ "+current.name+" "+current.line()))
	}
}

// reportUnborn closes the report with the next step of every repository that
// was created without a commit, exactly as gitone clone reports one.
func reportUnborn(output io.Writer, additions []*addition) {
	for _, current := range additions {
		if current.commit != "" {
			continue
		}
		fmt.Fprintf(output, "\n%s", repository.Unborn(current.name, current.unborn))
	}
}
