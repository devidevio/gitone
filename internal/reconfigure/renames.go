package reconfigure

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/devidevio/gitone/internal/config"
	"github.com/devidevio/gitone/internal/repository"
)

// RenameFlag declares that a name the configuration drops and a name it adds
// are the same repository. GitOne never infers that: a configuration never
// says whether a disappeared and an appeared name are one repository, and
// guessing it wrong either strands local history or adopts a stranger's
// history under a familiar name. Without the declaration the change is a
// retirement plus an addition, and the retirement refusal stops it before
// anything unpublished is lost.
const RenameFlag = "--rename"

// Rename is one declared rename, split at the "=" by the CLI.
type Rename struct{ From, To string }

// ParseRename splits one --rename value. Only its syntax is checked here: a
// value that is not "<old>=<new>" of two valid repository names is CLI001 and
// the command never starts. Which name disappears and which appears is
// decided against the configuration.
func ParseRename(value string) (Rename, bool) {
	from, to, _ := strings.Cut(value, "=")
	if config.ValidateName(from) != nil || config.ValidateName(to) != nil {
		return Rename{}, false
	}
	return Rename{From: from, To: to}, true
}

// rename is one repository whose metadata moves from the name the
// configuration dropped to the name it added. from is the retirement the
// declaration takes back: it already holds everything native Git has under
// the old name, and the rename keeps all of it by moving the directory
// instead of fetching anything.
type rename struct {
	from       *retirement
	to         string
	visibility string
	// changed marks a rename that also changes a configured remote, which the
	// REMOTES section then reviews like any other remote change.
	changed bool
}

// renamed takes every declared rename out of the planned retirements and
// refuses every declaration that does not describe one repository under two
// names. A refused declaration changes nothing: it is reported before the
// project is validated and long before the first write.
func renamed(configuration *config.Config, root string, declared []Rename,
	retirements []*retirement) ([]*rename, []*retirement, error) {
	counted := map[string]int{}
	for _, entry := range declared {
		counted[entry.From]++
		counted[entry.To]++
	}
	var issues []string
	for _, name := range slices.Sorted(maps.Keys(counted)) {
		if counted[name] > 1 {
			issues = append(issues, fmt.Sprintf("%s %s names repository %q %d times: a repository is renamed at most once",
				reconfigureFailed, RenameFlag, name, counted[name]))
		}
	}

	var renames []*rename
	taken := map[string]bool{}
	for _, entry := range declared {
		existing, err := directoryExists(repository.Directory(root, entry.To))
		if err != nil {
			return nil, nil, err
		}
		index := slices.IndexFunc(retirements, func(current *retirement) bool { return current.name == entry.From })
		if issue := mismatch(configuration, entry, index, existing); issue != "" {
			issues = append(issues, issue)
			continue
		}
		taken[entry.From] = true
		renames = append(renames, &rename{from: retirements[index], to: entry.To,
			visibility: configuration.Repositories[entry.To].Visibility})
	}
	if len(issues) != 0 {
		return nil, nil, errors.New(strings.Join(append(issues, unmodified), "\n"))
	}
	return renames, slices.DeleteFunc(retirements, func(current *retirement) bool { return taken[current.name] }), nil
}

// mismatch names the one thing a declaration got wrong, or nothing at all. A
// name disappears when the configuration no longer names it and its metadata
// is still there, and appears when the configuration names it and no
// metadata does.
func mismatch(configuration *config.Config, entry Rename, index int, existing bool) string {
	declaration := fmt.Sprintf("%s %s %s=%s", reconfigureFailed, RenameFlag, entry.From, entry.To)
	_, dropped := configuration.Repositories[entry.From]
	switch _, added := configuration.Repositories[entry.To]; {
	case dropped:
		return fmt.Sprintf("%s: the configuration still names %q, so it does not disappear", declaration, entry.From)
	case index < 0:
		return fmt.Sprintf("%s: %s holds no metadata, so there is nothing to rename", declaration, repository.Path(entry.From))
	case !added:
		return fmt.Sprintf("%s: the configuration does not name %q, so no repository appears under it", declaration, entry.To)
	case existing:
		return fmt.Sprintf("%s: %s already holds metadata, so %q does not appear", declaration, repository.Path(entry.To), entry.To)
	}
	return ""
}

// renamable refuses every declared rename that must not move: one whose
// repository is not on the project branch, and one whose committed tree or
// index the projected configuration does not let the new name own. A rename
// that is really an ownership change is refused before the directory moves,
// not after it.
func renamable(configuration *config.Config, root string, computed *plan) error {
	if len(computed.renames) == 0 {
		return nil
	}
	matcher, err := configuration.Matcher()
	if err != nil {
		return err
	}
	metadataNames := map[string]string{}
	destinations := map[string]string{}
	for _, current := range computed.renames {
		metadataNames[current.to] = current.from.name
		destinations[current.from.name] = current.to
	}
	indexes := map[string]string{}
	for _, current := range computed.states {
		if current.Created {
			continue
		}
		metadataName := current.Name
		if renamed := metadataNames[current.Name]; renamed != "" {
			metadataName = renamed
		}
		indexes[metadataName] = repository.IndexPath(root, metadataName)
	}
	indexLinkIssues, err := repository.IndexLinkIssues(matcher, root, indexes)
	if err != nil {
		return err
	}
	unsafeIndexPaths := map[string]bool{}
	var issues []string
	for _, entry := range indexLinkIssues {
		to := destinations[entry.Repository]
		if to == "" {
			continue
		}
		unsafeIndexPaths[entry.Repository+"\x00"+entry.Path] = true
		issues = append(issues, fmt.Sprintf("%s repository %q index must not manage %s as %q: %s",
			reconfigureFailed, entry.Repository, entry.Path, to, entry.Reason))
	}
	for _, current := range computed.renames {
		if current.from.branch != computed.starting {
			issues = append(issues, fmt.Sprintf(
				"%s repository %q is on %s, not on the project branch %s: every repository of a project is on one branch, so put it on %s before renaming it",
				reconfigureFailed, current.from.name, or(current.from.branch), computed.starting, computed.starting))
			continue
		}
		paths := map[string]string{}
		if current.from.head != "" {
			files, err := repository.TreeFiles(root, current.from.name, current.from.head)
			if err != nil {
				return err
			}
			for entry := range files {
				paths[entry] = "from " + short(current.from.head)
			}
		}
		for _, entry := range current.from.tracked {
			if paths[entry] == "" {
				paths[entry] = "from its index"
			}
		}
		for _, entry := range slices.Sorted(maps.Keys(paths)) {
			if unsafeIndexPaths[current.from.name+"\x00"+entry] {
				continue
			}
			if _, reason := matcher.UnownedIssue(current.to, entry); reason != "" {
				issues = append(issues, fmt.Sprintf("%s repository %q must not manage %s %s as %q: %s",
					reconfigureFailed, current.from.name, entry, paths[entry], current.to, reason))
			}
		}
	}
	if len(issues) == 0 {
		return nil
	}
	return errors.New(strings.Join(append(issues, unmodified), "\n"))
}

// preview names one rename as the single line it is. A rename is never shown
// as a removal plus an addition: nothing is retired, created, fetched or
// checked out, and the repository keeps everything it holds.
func (r *rename) preview(output io.Writer) {
	fmt.Fprintf(output, "  ~ %s -> %s   %s   %s\n", r.from.name, r.to, visibility(r.visibility), r.remote())
	fmt.Fprintf(output, "      metadata is moved to %s, so its history, index and reflog are kept and nothing is fetched for the rename\n",
		repository.Path(r.to))
}

// remote states whether the rename also changes a configured remote, which
// the REMOTES section reviews with both URLs like any other remote change.
func (r *rename) remote() string {
	if r.changed {
		return "remotes change, see REMOTES"
	}
	return "remote unchanged"
}

// line is what one rename ended up doing, in the wording both the
// reconfiguration report and a recovery report use.
func (r *rename) line() string {
	return "renamed to " + r.to + ", metadata moved without fetching"
}

// moveRenames performs every declared rename. The moves are the first writes
// of the plan, so a failure here leaves nothing else to undo.
func moveRenames(root string, renames []renameState) error {
	for _, entry := range renames {
		if _, err := relocate(root, entry.From, entry.To, entry.recorded()); err != nil {
			return err
		}
	}
	return nil
}

// unmove moves every recorded rename back to the name it had, which is what
// gitone abort does, and reports what it found.
func unmove(root string, renames []renameState) ([]string, error) {
	lines := make([]string, len(renames))
	for index, entry := range renames {
		moved, err := relocate(root, entry.To, entry.From, entry.recorded())
		if err != nil {
			return nil, err
		}
		lines[index] = "was not renamed"
		if moved {
			lines[index] = "moved back to " + entry.From
		}
	}
	return lines, nil
}

// unmoved validates every recorded rename location and returns the repositories
// whose metadata is still under its old name. Callers may only change remotes
// after this proves that each rename has exactly one unchanged directory.
func unmoved(root string, renames []renameState) (map[string]bool, error) {
	pending := map[string]bool{}
	for _, entry := range renames {
		sourceExists, err := directoryExists(repository.Directory(root, entry.From))
		if err != nil {
			return nil, err
		}
		destinationExists, err := directoryExists(repository.Directory(root, entry.To))
		if err != nil {
			return nil, err
		}
		switch {
		case sourceExists && destinationExists:
			return nil, changedName(root, entry.From, fmt.Sprintf("both %s and %s hold metadata",
				repository.Path(entry.From), repository.Path(entry.To)))
		case !sourceExists && !destinationExists:
			return nil, changedName(root, entry.From, fmt.Sprintf("neither %s nor %s holds metadata",
				repository.Path(entry.From), repository.Path(entry.To)))
		case sourceExists:
			if err := validateMoved(root, repository.Directory(root, entry.From), entry.recorded()); err != nil {
				return nil, err
			}
			pending[entry.To] = true
		case destinationExists:
			if err := validateMoved(root, repository.Directory(root, entry.To), entry.recorded()); err != nil {
				return nil, err
			}
		}
	}
	return pending, nil
}

// relocate moves one metadata directory to the other name of a recorded
// rename and reports whether it performed the move. A move that already
// happened is accepted, and a directory somebody worked in since the plan was
// recorded, a name that holds metadata on both sides and one that holds it on
// neither are refused instead of overwritten.
func relocate(root, from, to string, want recorded) (bool, error) {
	sourceExists, err := directoryExists(repository.Directory(root, from))
	if err != nil {
		return false, err
	}
	destinationExists, err := directoryExists(repository.Directory(root, to))
	if err != nil {
		return false, err
	}
	switch {
	case sourceExists && destinationExists:
		return false, changedName(root, want.name, fmt.Sprintf("both %s and %s hold metadata",
			repository.Path(from), repository.Path(to)))
	case !sourceExists && !destinationExists:
		return false, changedName(root, want.name, fmt.Sprintf("neither %s nor %s holds metadata",
			repository.Path(from), repository.Path(to)))
	case destinationExists:
		return false, validateMoved(root, repository.Directory(root, to), want)
	}
	if err := validateMoved(root, repository.Directory(root, from), want); err != nil {
		return false, err
	}
	return true, repository.Move(root, from, to)
}
