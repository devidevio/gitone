// Package status combines every managed repository into one project status.
// It only reads: no index, ref, file or remote-tracking data is modified.
package status

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/devidevio/gitone/internal/config"
	"github.com/devidevio/gitone/internal/git"
	"github.com/devidevio/gitone/internal/lock"
	"github.com/devidevio/gitone/internal/repository"
	"github.com/devidevio/gitone/internal/worktree"
)

// Version is the schema version of the machine-readable status output. It
// changes whenever an existing field changes meaning or disappears.
const Version = 1

// The change states of a path, in the order they are reported.
const (
	Staged    = "staged"
	Unstaged  = "unstaged"
	Untracked = "untracked"
)

var stateOrder = []string{Staged, Unstaged, Untracked}

// Detached is the branch name Git reports for a detached HEAD.
const Detached = "(detached)"

// BranchIssue marks the issue that the repositories are not on the same
// branch. gitone switch validates the project with it tolerated, because
// moving every repository to one branch is exactly how it is resolved.
const BranchIssue = "branch"

// changeTypes maps the porcelain v2 status characters to stable names.
var changeTypes = map[byte]string{
	'M': "modified",
	'T': "typechanged",
	'A': "added",
	'D': "deleted",
	'R': "renamed",
	'C': "copied",
	'U': "unmerged",
}

// Repository is the reported state of one managed repository.
type Repository struct {
	Name       string `json:"name"`
	Visibility string `json:"visibility"`
	Branch     string `json:"branch"`
	// Ahead and Behind are null while no local remote-tracking ref exists,
	// which is the case until the first successful push.
	Ahead  *int `json:"ahead"`
	Behind *int `json:"behind"`
}

// Change is one changed path together with the repository owning it.
type Change struct {
	Repository string `json:"repository"`
	State      string `json:"state"`
	Type       string `json:"type"`
	Path       string `json:"path"`
	// From is the previous path of a rename or copy.
	From string `json:"from,omitempty"`
}

// Issue is a detected problem that makes the project state unsafe.
type Issue struct {
	Code       string `json:"code"`
	Path       string `json:"path"`
	Detail     string `json:"detail"`
	Kind       string `json:"-"`
	Repository string `json:"-"`
}

// Status is the complete project status.
type Status struct {
	Version      int          `json:"version"`
	Repositories []Repository `json:"repositories"`
	Changes      []Change     `json:"changes"`
	Issues       []Issue      `json:"issues"`
	// Ignored are the project-relative paths the .gitignore files hide. A
	// directory entry ends with a slash and stands for everything below it
	// except IgnoreExceptions.
	Ignored []string `json:"ignored"`
	// IgnoreExceptions are exact tracked paths below an ignored directory.
	IgnoreExceptions []string `json:"ignore_exceptions"`
}

// Collect reads the project status under a shared project lock.
func Collect(configuration *config.Config, root string) (*Status, error) {
	release, err := lock.AcquireRead(root)
	if err != nil {
		return nil, err
	}
	defer release()

	result, _, err := Inspect(configuration, root)
	return result, err
}

// Validate reports the complete project state and refuses to continue while
// any issue makes it unsafe. Every mutating command runs it, even when it only
// touches a single path or repository.
func Validate(configuration *config.Config, root string) (*Status, *worktree.Result, error) {
	return validate(configuration, root)
}

// ValidateBranchDisagreement validates like Validate but permits repositories
// on different branches, which only gitone switch can safely resolve.
func ValidateBranchDisagreement(configuration *config.Config, root string) (*Status, *worktree.Result, error) {
	return validate(configuration, root, BranchIssue)
}

// ValidateMissingRepositories validates like Validate but permits configured
// repositories without metadata, which only gitone reconfigure can create.
func ValidateMissingRepositories(configuration *config.Config, root string) (*Status, *worktree.Result, error) {
	return validate(configuration, root, repository.MissingIssue)
}

// validate reports every issue that makes the project unsafe. tolerated names
// the issue kinds the calling command resolves itself and therefore accepts.
func validate(configuration *config.Config, root string, tolerated ...string) (*Status, *worktree.Result, error) {
	result, inventory, err := Inspect(configuration, root)
	if err != nil {
		return nil, nil, err
	}
	var issues []error
	for _, issue := range result.Issues {
		if issue.Kind != "" && slices.Contains(tolerated, issue.Kind) {
			continue
		}
		issues = append(issues, errors.New(issue.String()))
	}
	if len(issues) != 0 {
		return nil, nil, errors.Join(issues...)
	}
	return result, inventory, nil
}

// Inspect reads the state of every configured repository and the ownership of
// every relevant path, and returns the working-tree inventory alongside the
// status. It reports all detected issues instead of stopping at the first one.
// An error means the status itself could not be determined. It takes no lock:
// a mutating command calls it while holding the project lock.
func Inspect(configuration *config.Config, root string) (*Status, *worktree.Result, error) {
	result := &Status{
		Version:          Version,
		Repositories:     []Repository{},
		Changes:          []Change{},
		Issues:           []Issue{},
		Ignored:          []string{},
		IgnoreExceptions: []string{},
	}
	matcher, err := configuration.Matcher()
	if err != nil {
		return nil, nil, err
	}
	// indexed maps every path in a repository index to that repository, and
	// reported holds the repositories whose state could actually be read.
	indexed := map[string]string{}
	indexes := map[string]string{}
	reported := map[string]bool{}
	for _, name := range configuration.RepositoryNames() {
		gitDirectory := repository.Directory(root, name)
		if _, err := os.Stat(gitDirectory); errors.Is(err, os.ErrNotExist) {
			result.Issues = append(result.Issues, Issue{
				Code:       "REPO001",
				Kind:       repository.MissingIssue,
				Repository: name,
				Detail:     fmt.Sprintf("repository %q %s", name, repository.NotInitialized),
			})
			continue
		} else if err != nil {
			return nil, nil, err
		}

		// A repository that cannot be read is reported like any other issue so
		// the remaining repositories still produce a complete status.
		state, changes, err := read(root, gitDirectory, name)
		if err != nil {
			result.issue("REPO001", "", fmt.Sprintf("repository %q cannot be read: %v", name, err))
			continue
		}
		tracked, err := trackedPaths(root, gitDirectory)
		if err != nil {
			result.issue("REPO001", "", fmt.Sprintf("repository %q cannot be read: %v", name, err))
			continue
		}
		indexes[name] = repository.IndexPath(root, name)
		// Every staged path participates in ownership validation. A deletion or
		// rename source no longer exists in the current index, so add it here.
		for _, change := range changes {
			if change.State == Staged {
				tracked = append(tracked, change.Path)
				if change.From != "" {
					tracked = append(tracked, change.From)
				}
			}
		}
		for _, path := range tracked {
			// An index lists an unmerged path once per stage, so only a path
			// claimed by a second repository is a conflict.
			if owner, exists := indexed[path]; exists {
				if owner != name {
					result.issue("PATH003", path, fmt.Sprintf("path is tracked by %s and %s", owner, name))
				}
				continue
			}
			indexed[path] = name
		}

		state.Visibility = configuration.Repositories[name].Visibility
		result.Repositories = append(result.Repositories, state)
		result.Changes = append(result.Changes, changes...)
		reported[name] = true
	}
	// Managed indexes share one project tree. Checking them together catches
	// directory links whose descendants are split across repositories.
	refused, err := repository.IndexLinkIssues(matcher, root, indexes)
	if err != nil {
		result.issue("REPO001", "", err.Error())
	} else {
		for _, entry := range refused {
			result.issue("PATH003", entry.Path, entry.Reason)
		}
	}
	result.checkBranches()

	inventory, err := worktree.Scan(configuration, root, slices.Sorted(maps.Keys(indexed)))
	if err != nil {
		return nil, nil, err
	}
	// Removing a rule together with a file must not prevent recording its
	// deletion. Git still identifies its repository until the deletion commits.
	// Staging a deletion together with a new file makes Git infer a rename, so
	// the source of a rename is a pending deletion too.
	deleted := map[string]string{}
	for _, change := range result.Changes {
		if change.Type == changeTypes['D'] {
			deleted[change.Path] = change.Repository
		} else if change.Type == changeTypes['R'] {
			deleted[change.From] = change.Repository
		}
	}
	inventory.Issues = slices.DeleteFunc(inventory.Issues, func(issue *worktree.Issue) bool {
		owner := deleted[issue.Path]
		if issue.Code != worktree.PathUnassigned || owner == "" || indexed[issue.Path] != owner {
			return false
		}
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(issue.Path))); !errors.Is(err, os.ErrNotExist) {
			return false
		}
		inventory.Owners[issue.Path] = owner
		return true
	})
	for _, issue := range inventory.Issues {
		result.issue(issue.Code, issue.Path, issue.Detail)
	}
	result.Ignored = inventory.Ignored
	result.IgnoreExceptions = inventory.IgnoreExceptions

	for _, path := range slices.Sorted(maps.Keys(inventory.Owners)) {
		owner := inventory.Owners[path]
		switch tracking, exists := indexed[path]; {
		case !exists:
			// A repository whose state is unknown gets no change list; its
			// REPO001 issue already explains why.
			if reported[owner] {
				result.Changes = append(result.Changes, Change{Repository: owner, State: Untracked, Type: "added", Path: path})
			}
		case tracking != owner:
			result.issue("PATH003", path, fmt.Sprintf("path is tracked by %s but owned by %s", tracking, owner))
		}
	}

	result.sort()
	return result, inventory, nil
}

func (s *Status) issue(code, path, detail string) {
	s.Issues = append(s.Issues, Issue{Code: code, Path: path, Detail: detail})
}

// checkBranches rejects repositories that do not share one branch, because a
// grouped commit or push could otherwise span different lines of history.
// Which branch they share does not matter: gitone switch moves the whole
// project to any existing one, so the configured default branch is only where
// a repository starts.
func (s *Status) checkBranches() {
	branches := map[string]bool{}
	described := make([]string, 0, len(s.Repositories))
	for _, reported := range s.Repositories {
		if reported.Branch == Detached {
			s.Issues = append(s.Issues, Issue{
				Code:       "REPO001",
				Repository: reported.Name,
				Detail:     fmt.Sprintf("repository %q is not on a branch", reported.Name),
			})
		}
		branches[reported.Branch] = true
		described = append(described, reported.Name+" on "+reported.Branch)
	}
	if len(branches) > 1 {
		s.Issues = append(s.Issues, Issue{
			Code: "REPO001",
			Kind: BranchIssue,
			Detail: "the repositories are not on the same branch: " + strings.Join(described, ", ") +
				": run gitone switch <branch>",
		})
	}
}

func (s *Status) sort() {
	slices.SortFunc(s.Changes, func(a, b Change) int {
		if a.Repository != b.Repository {
			return strings.Compare(a.Repository, b.Repository)
		}
		if a.State != b.State {
			return slices.Index(stateOrder, a.State) - slices.Index(stateOrder, b.State)
		}
		return strings.Compare(a.Path, b.Path)
	})
	slices.SortFunc(s.Issues, func(a, b Issue) int {
		if a.Path != b.Path {
			return strings.Compare(a.Path, b.Path)
		}
		if a.Code != b.Code {
			return strings.Compare(a.Code, b.Code)
		}
		return strings.Compare(a.Detail, b.Detail)
	})
}

// trackedPaths lists the project-relative paths in the index of one
// repository, including paths whose file was deleted from the working tree.
func trackedPaths(root, gitDirectory string) ([]string, error) {
	output, err := git.Run(root, "--no-optional-locks", "--git-dir="+gitDirectory, "--work-tree="+root, "ls-files", "-z")
	if err != nil {
		return nil, err
	}
	return split(output), nil
}

// read collects branch, ahead/behind and the tracked changes of one
// repository. Untracked paths are excluded because every repository shares
// one working tree; they are derived from path ownership instead.
func read(root, gitDirectory, name string) (Repository, []Change, error) {
	output, err := git.Run(root, "--no-optional-locks", "--git-dir="+gitDirectory, "--work-tree="+root,
		"status", "--porcelain=v2", "--branch", "--untracked-files=no", "-z")
	if err != nil {
		return Repository{}, nil, err
	}

	state := Repository{Name: name}
	var changes []Change
	records := split(output)
	for index := 0; index < len(records); index++ {
		record := records[index]
		switch {
		case strings.HasPrefix(record, "# branch.head "):
			state.Branch = strings.TrimPrefix(record, "# branch.head ")
		case strings.HasPrefix(record, "# branch.ab "):
			state.Ahead, state.Behind = parseAheadBehind(strings.TrimPrefix(record, "# branch.ab "))
		case strings.HasPrefix(record, "1 "), strings.HasPrefix(record, "2 "), strings.HasPrefix(record, "u "):
			from := ""
			if strings.HasPrefix(record, "2 ") && index+1 < len(records) {
				index++
				from = records[index]
			}
			changes = append(changes, parseEntry(record, name, from)...)
		}
	}
	return state, changes, nil
}

// entryFields is the number of space separated fields of each porcelain v2
// entry type. The path is always the last field.
var entryFields = map[byte]int{'1': 9, '2': 10, 'u': 11}

// parseEntry turns one porcelain v2 entry into its staged and unstaged
// changes. The staged change of a rename or copy carries the previous path.
func parseEntry(record, name, from string) []Change {
	count, known := entryFields[record[0]]
	fields := strings.SplitN(record, " ", count)
	if !known || len(fields) != count || len(fields[1]) != 2 {
		return nil
	}
	path := fields[count-1]

	var changes []Change
	for position, state := range []string{Staged, Unstaged} {
		changeType, known := changeTypes[fields[1][position]]
		if !known {
			continue
		}
		change := Change{Repository: name, State: state, Type: changeType, Path: path}
		if state == Staged && (changeType == "renamed" || changeType == "copied") {
			change.From = from
		}
		changes = append(changes, change)
	}
	return changes
}

func parseAheadBehind(value string) (*int, *int) {
	fields := strings.Fields(value)
	if len(fields) != 2 {
		return nil, nil
	}
	ahead, aheadErr := strconv.Atoi(strings.TrimPrefix(fields[0], "+"))
	behind, behindErr := strconv.Atoi(strings.TrimPrefix(fields[1], "-"))
	if aheadErr != nil || behindErr != nil {
		return nil, nil
	}
	return &ahead, &behind
}

func split(output string) []string {
	return slices.DeleteFunc(strings.Split(output, "\x00"), func(record string) bool { return record == "" })
}
