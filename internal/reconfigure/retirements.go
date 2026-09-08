package reconfigure

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/devidevio/gitone/internal/config"
	"github.com/devidevio/gitone/internal/git"
	"github.com/devidevio/gitone/internal/repository"
	"github.com/devidevio/gitone/internal/worktree"
)

// named is how many unpublished commits a refusal lists before it stops. A
// repository nobody ever pushed can hold thousands, and the first ones are
// what identifies the work.
const named = 10

// retirement is one repository the project has metadata for and the effective
// configuration no longer names. Nothing is ever deleted: the metadata is
// moved to destination and every working-tree file it tracked stays where it
// is, so what a retirement changes is only who may commit those files.
//
// visibility is empty for a plain reconfiguration: the projected
// configuration no longer names the repository, so nothing states it.
type retirement struct {
	name        string
	visibility  string
	branch      string
	head        string
	index       string
	origin      string
	destination string
	refs        []string
	// commits is what the repository holds on its own branches and tags, and
	// unpublished the ones its origin does not have.
	commits     int
	unpublished []string
	// tracked is every path its index holds, owners the repositories that own
	// those paths in the projected configuration, and stranded the ones that
	// would be left relevant and unassigned.
	tracked  []string
	owners   []string
	stranded []string
}

// removed lists every repository below .gitone/repositories/ the effective
// configuration no longer names, with everything the preview and the two
// refusals need. It reads native Git only, so it runs before the project is
// validated: an unassigned path a retirement would leave behind is exactly
// what that validation would report as an unexplained PATH001.
func removed(configuration *config.Config, root string) ([]*retirement, error) {
	existing, err := repository.Existing(root)
	if err != nil {
		return nil, err
	}
	matcher, err := configuration.Matcher()
	if err != nil {
		return nil, err
	}

	var retirements []*retirement
	for _, name := range existing {
		// A directory that is not a repository name was never created by
		// GitOne, and this command does not move what it does not manage.
		if _, configured := configuration.Repositories[name]; configured || config.ValidateName(name) != nil {
			continue
		}
		current := &retirement{name: name, destination: repository.RetiredPath(name)}
		if err := current.inspect(root); err != nil {
			return nil, err
		}
		if err := current.classify(matcher, root); err != nil {
			return nil, err
		}
		retirements = append(retirements, current)
	}
	return retirements, nil
}

// inspect reads from native Git what the repository is: the branch it is on,
// its configured origin, its refs, how many commits it holds and which of
// them that origin does not have.
func (r *retirement) inspect(root string) error {
	directory := repository.Directory(root, r.name)
	gitDirectory := "--git-dir=" + directory
	var err error
	if r.branch, r.head, r.index, err = retirementSnapshot(root, directory); err != nil {
		return err
	}
	if r.origin, err = configuredRemoteURL(root, r.name, config.OriginRemote); err != nil {
		return err
	}
	if r.refs, err = refs(root, r.name); err != nil {
		return err
	}

	// Every ref plus HEAD is work the repository holds; the remote-tracking refs
	// of origin are the part that also exists there. HEAD is explicit because a
	// detached commit need not be reachable from any ref.
	revisions := []string{"--all"}
	if r.head != "" {
		revisions = append(revisions, "HEAD")
	}
	counted, err := git.Run(root, append([]string{gitDirectory, "rev-list", "--count"}, revisions...)...)
	if err != nil {
		return err
	}
	if r.commits, err = strconv.Atoi(strings.TrimSpace(counted)); err != nil {
		return err
	}
	unpublished := append([]string{gitDirectory, "rev-list"}, revisions...)
	unpublished = append(unpublished, "--not", "--remotes="+config.OriginRemote)
	listed, err := git.Run(root, unpublished...)
	if err != nil {
		return err
	}
	r.unpublished = strings.Fields(listed)

	indexed, err := git.Run(root, "--no-optional-locks", gitDirectory, "--work-tree="+root, "ls-files", "-z")
	if err != nil {
		return err
	}
	r.tracked = slices.DeleteFunc(strings.Split(indexed, "\x00"), func(entry string) bool { return entry == "" })
	return nil
}

// classify resolves every path the repository holds against the projected
// configuration: who owns it afterwards, and which paths nobody would.
//
// A path the projected ignore rules hide is not relevant and therefore not
// stranded, which is one of the three repairs. A path that no longer exists
// in the working tree is not relevant either.
func (r *retirement) classify(matcher *config.Matcher, root string) error {
	owners := map[string]bool{}
	var candidates []string
	for _, entry := range r.tracked {
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(entry))); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return err
		}
		switch owner, issue, _ := matcher.Owner(entry); issue {
		case config.OwnerValid:
			owners[owner] = true
		case config.OwnerUnassigned:
			candidates = append(candidates, entry)
		}
	}
	r.owners = slices.Sorted(maps.Keys(owners))

	hidden, err := worktree.IgnoredBy(root, candidates)
	if err != nil {
		return err
	}
	for _, entry := range candidates {
		if !hidden[entry] {
			r.stranded = append(r.stranded, entry)
		}
	}
	return nil
}

// retirable refuses every retirement that would strand work or leave the
// project unusable. Both refusals hold with and without every flag: a
// repository whose commits exist nowhere else is the one thing a move cannot
// make safe, and a project whose paths nobody owns stops every mutating
// command with PATH001.
func retirable(root string, retirements []*retirement) error {
	var issues []string
	for _, current := range retirements {
		if len(current.unpublished) != 0 {
			named, err := current.describe(root)
			if err != nil {
				return err
			}
			issues = append(issues, named)
		}
		if len(current.stranded) != 0 {
			issues = append(issues, fmt.Sprintf(
				"%s retiring repository %q would leave paths no repository owns:\n%s\n"+
					"Let another repository own each path in the same configuration, ignore it, or remove it, then run %s again.\n"+
					"No flag ever accepts this.",
				reconfigureFailed, current.name, indent(current.stranded), Command))
		}
	}
	if len(issues) == 0 {
		return nil
	}
	return errors.New(strings.Join(append(issues, unmodified), "\n"))
}

// describe names the commits that exist only here and both repairs: publish
// them, or keep the name because this is a rename rather than a removal.
// GitOne never infers a rename, which is what makes the missing declaration
// safe rather than merely inconvenient.
func (r *retirement) describe(root string) (string, error) {
	arguments := []string{"--git-dir=" + repository.Directory(root, r.name), "log", "--no-color",
		"--format=%h %s", "--max-count=" + strconv.Itoa(named), "--all"}
	if r.head != "" {
		arguments = append(arguments, "HEAD")
	}
	arguments = append(arguments, "--not", "--remotes="+config.OriginRemote)
	subjects, err := git.Run(root, arguments...)
	if err != nil {
		return "", err
	}
	lines := strings.Split(strings.TrimRight(subjects, "\n"), "\n")
	if remaining := len(r.unpublished) - len(lines); remaining > 0 {
		lines = append(lines, fmt.Sprintf("and %d more", remaining))
	}
	held := fmt.Sprintf("has %s %s does not have", commits(len(r.unpublished)), r.origin)
	publish := fmt.Sprintf("Restore %q in %s, run \"gitone push %s\" to publish them and remove it again",
		r.name, config.PublicFile, r.name)
	if r.origin == "" {
		held = fmt.Sprintf("has no configured %s and %s", config.OriginRemote, commits(len(r.unpublished)))
		publish = fmt.Sprintf("Restore %q in %s, add its %s, run \"gitone push %s\" to publish them and remove it again",
			r.name, config.PublicFile, config.OriginRemote, r.name)
	}
	return fmt.Sprintf("%s repository %q %s, and retiring it would strand them:\n%s\n%s, "+
		"or declare the rename with \"%s %s %s=<new name>\" if the repository was renamed rather than "+
		"removed: GitOne never infers a rename.",
		reconfigureFailed, r.name, held, indent(lines), publish, Command, RenameFlag, r.name), nil
}

// preview names one retirement with everything a person reviews before the
// metadata moves: where it goes, what it holds and where that is published,
// and who owns its paths once it is gone.
func (r *retirement) preview(output io.Writer) {
	fmt.Fprintf(output, "  - %s   %s   %s\n", r.name, visibility(r.visibility), or(r.origin))
	fmt.Fprintf(output, "      metadata is retired to %s/, never deleted, and no working-tree file is touched\n", r.destination)
	fmt.Fprintf(output, "      %s\n", r.published())
	fmt.Fprintf(output, "      %s\n", r.owned())
}

// published states what the repository holds and where else it exists, which
// is the one fact that decides whether retiring it can lose anything.
func (r *retirement) published() string {
	switch {
	case r.commits == 0:
		return "no commit yet"
	case r.origin == "":
		return fmt.Sprintf("%s, no configured %s", commits(r.commits), config.OriginRemote)
	}
	return fmt.Sprintf("%s, all present on %s", commits(r.commits), config.OriginRemote)
}

// owned states who may commit the paths this repository tracked once it is
// retired. Every path that nobody would own was already refused.
func (r *retirement) owned() string {
	if len(r.owners) == 0 {
		return "no path it tracks is relevant in this configuration"
	}
	quoted := make([]string, len(r.owners))
	for index, name := range r.owners {
		quoted[index] = strconv.Quote(name)
	}
	return "its paths are owned by " + strings.Join(quoted, " and ") + " in this configuration"
}

// line is what one retirement ended up doing, in the wording both the
// reconfiguration report and a recovery report use.
func (r *retirement) line() string {
	return "retired to " + r.destination
}

// retire moves the metadata of every repository the configuration no longer
// names. It is the last phase of the reconfiguration, run once the project is
// already consistent. A preparation failure keeps recovery state; a final move
// failure needs only the mv printed with the error.
//
// A directory that no longer holds the recorded HEAD, index and refs is never
// moved: somebody worked in it since the plan was recorded.
func retire(root string, retirements []retirementState) error {
	for _, entry := range retirements {
		source := repository.Directory(root, entry.Name)
		destination := filepath.Join(root, filepath.FromSlash(entry.Destination))
		sourceExists, err := directoryExists(source)
		if err != nil {
			return err
		}
		destinationExists, err := directoryExists(destination)
		if err != nil && !sourceExists {
			return err
		}
		switch {
		case sourceExists && destinationExists:
			return changedName(root, entry.Name, "both the active and retired metadata directories exist")
		case !sourceExists && !destinationExists:
			return changedName(root, entry.Name, "both the active and retired metadata directories are missing")
		case destinationExists:
			if err := validateMoved(root, destination, entry.recorded()); err != nil {
				return err
			}
			continue
		}
		if err := validateMoved(root, source, entry.recorded()); err != nil {
			return err
		}
		if err := repository.RetireTo(root, entry.Name, entry.Destination); err != nil {
			if !errors.Is(err, repository.ErrRetirementMove) {
				return fmt.Errorf("%s repository %q metadata could not be prepared for retirement to %s: %s\n"+
					"%s recovery state was kept; run gitone recover to retry or gitone abort to undo the reconfiguration",
					reconfigureFailed, entry.Name, entry.Destination, detail(err), recoveryRequired)
			}
			return fmt.Errorf("%s repository %q metadata could not be moved to %s: %w\n"+
				"The reconfiguration itself is complete and no further GitOne action is required. Finish the move with:\n"+
				"  mv %s %s",
				reconfigureFailed, entry.Name, entry.Destination, err,
				repository.Path(entry.Name), entry.Destination)
		}
	}
	return nil
}

// unretire moves every recorded retirement back to .gitone/repositories/,
// which is what gitone abort does. A retired directory whose state differs,
// and a name that has metadata again, are reported instead of being moved over.
func unretire(root string, retirements []retirementState) ([]string, error) {
	lines := make([]string, len(retirements))
	for index, entry := range retirements {
		source := repository.Directory(root, entry.Name)
		destination := filepath.Join(root, filepath.FromSlash(entry.Destination))
		sourceExists, err := directoryExists(source)
		if err != nil {
			return nil, err
		}
		destinationExists, err := directoryExists(destination)
		if err != nil && !sourceExists {
			return nil, err
		}
		switch {
		case sourceExists && destinationExists:
			return nil, changedName(root, entry.Name,
				"it has metadata again, so the retired "+entry.Destination+" was not moved back")
		case !sourceExists && !destinationExists:
			return nil, changedName(root, entry.Name, "both the active and retired metadata directories are missing")
		case sourceExists:
			if err := validateMoved(root, source, entry.recorded()); err != nil {
				return nil, err
			}
			lines[index] = "was not retired"
			continue
		}
		if err := validateMoved(root, destination, entry.recorded()); err != nil {
			return nil, err
		}
		if err := repository.Restore(root, entry.Name, entry.Destination); err != nil {
			return nil, err
		}
		lines[index] = "moved back from " + entry.Destination
	}
	return lines, nil
}

func directoryExists(directory string) (bool, error) {
	_, err := os.Stat(directory)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// retirementSnapshot is the native state that recovery must not overwrite.
func retirementSnapshot(root, directory string) (branch, head, index string, err error) {
	gitDirectory := "--git-dir=" + directory
	listedBranch, err := git.Run(root, gitDirectory, "branch", "--show-current")
	if err != nil {
		return "", "", "", err
	}
	branch = strings.TrimSpace(listedBranch)

	listedHead, headErr := git.Run(root, gitDirectory, "rev-parse", "--verify", "HEAD")
	if headErr == nil {
		head = strings.TrimSpace(listedHead)
	} else {
		symbolic, symbolicErr := git.Run(root, gitDirectory, "symbolic-ref", "-q", "HEAD")
		if symbolicErr != nil {
			return "", "", "", headErr
		}
		existing, refErr := git.Run(root, gitDirectory, "for-each-ref", "--format=%(objectname)", strings.TrimSpace(symbolic))
		if refErr != nil {
			return "", "", "", refErr
		}
		if strings.TrimSpace(existing) != "" {
			return "", "", "", headErr
		}
	}

	index, err = git.Run(root, "--no-optional-locks", gitDirectory, "--work-tree="+root, "ls-files", "--stage", "-z")
	return branch, head, index, err
}

// recorded is what one metadata directory held when the plan was written
// down, so neither a move nor its reversal can overwrite work somebody did
// since. Retirements and renames record and check the same state.
type recorded struct {
	name, branch, head, index string
	refs                      []string
}

// validateMoved refuses a metadata directory that no longer holds exactly
// what the plan recorded, which is the one case a move must never touch.
func validateMoved(root, directory string, want recorded) error {
	branch, head, index, err := retirementSnapshot(root, directory)
	if err != nil {
		return err
	}
	if branch != want.branch || head != want.head {
		return changedName(root, want.name, "HEAD differs from the recorded reconfiguration state")
	}
	if index != want.index {
		return changedName(root, want.name, "index differs from the recorded reconfiguration state")
	}
	listed, err := refsIn(root, directory)
	if err != nil {
		return err
	}
	if !slices.Equal(localRefs(listed), localRefs(want.refs)) {
		return changedName(root, want.name, "refs differ from the recorded reconfiguration state")
	}
	return nil
}

// localRefs is the ref state a move is compared against: the branches, tags
// and notes a person can move, without the remote-tracking refs. A renamed
// repository is fetched by this very command, and a fetched ref is a cache
// that is never rolled back, exactly as after a failed pull.
func localRefs(listed []string) []string {
	return slices.DeleteFunc(slices.Clone(listed), func(line string) bool {
		return strings.HasPrefix(line, "refs/remotes/")
	})
}

// visibility renders the visibility of a repository the projected
// configuration no longer names, which is nothing rather than a value.
func visibility(value string) string {
	if value == "" {
		return "unconfigured"
	}
	return value
}

func commits(count int) string {
	if count == 1 {
		return "1 commit"
	}
	return fmt.Sprintf("%d commits", count)
}

// indent renders a listed refusal as the two-space block every other refusal
// of this command prints.
func indent(lines []string) string {
	return "  " + strings.Join(lines, "\n  ")
}
