// Package fetch updates the remote-tracking refs of the managed repositories.
// It contacts only the configured origin remote of each repository through
// native Git, so credentials, URL rewrites and transport configuration behave
// exactly as they do for a plain git fetch. Only refs below
// refs/remotes/origin/ are updated: branches, HEADs, indexes and working-tree
// files stay untouched.
package fetch

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/devidevio/gitone/internal/config"
	"github.com/devidevio/gitone/internal/git"
	"github.com/devidevio/gitone/internal/lock"
	"github.com/devidevio/gitone/internal/repository"
	"github.com/devidevio/gitone/internal/status"
	"github.com/devidevio/gitone/internal/ui"
)

const (
	// Command is the recorded command of a running fetch.
	Command = "gitone fetch"

	repositoryInvalid = "REPO001"
	fetchFailed       = "FETCH001"

	// stopped closes a failed fetch. A fetch is not atomic across
	// repositories, and successfully fetched refs are never rolled back.
	stopped = "The remote-tracking refs of the repositories reported as updated remain updated. " +
		"No branch, HEAD, index or working-tree file was changed."
)

// The outcome of one repository, in the order the report explains them.
const (
	updated   = "updated"
	unchanged = "unchanged"
	skipped   = "skipped"
	failed    = "failed"
)

// Repository is one repository a fetch or a pull selected. Remote is its
// configured origin, empty for a local-only repository.
type Repository struct {
	Name   string
	Remote string
}

// result is one selected repository and what the fetch did with it.
type result struct {
	Repository
	state  string
	detail string
}

// Fetch updates the remote-tracking refs of the selected repositories. target
// is empty or "all" for every configured repository, or the name of one.
func Fetch(configuration *config.Config, root, target string, input io.Reader, output io.Writer) error {
	repositories, err := Select(configuration, target)
	if err != nil {
		return err
	}
	selected := make([]*result, 0, len(repositories))
	for _, current := range repositories {
		selected = append(selected, &result{Repository: current})
	}
	release, err := lock.Acquire(root, Command)
	if err != nil {
		return err
	}
	defer release()

	if err := Check(configuration, root, repositories); err != nil {
		return err
	}
	// The complete working tree is validated even for a single repository,
	// and before the first remote is contacted. Ordinary staged and unstaged
	// changes are not issues and stay untouched by the fetch.
	if _, _, err := status.Validate(configuration, root); err != nil {
		return err
	}
	return ui.SpinBuffered(input, output, "Fetching repositories", func(rendered io.Writer) error {
		return update(root, selected, rendered)
	})
}

// Check validates every selected repository before the first remote is
// contacted, including that its native Git remotes still match the config.
func Check(configuration *config.Config, root string, selected []Repository) error {
	var issues []error
	for _, current := range selected {
		for _, issue := range repository.Check(root, current.Name, configuration.Repositories[current.Name]) {
			issues = append(issues, issue)
		}
	}
	return errors.Join(issues...)
}

// Select resolves the command input into the repositories a fetch or a pull
// may consider, in configuration order. A named repository that is unknown or
// has no configured origin fails here, before any remote is contacted; during
// an all-repository run a local-only repository is only reported as skipped.
func Select(configuration *config.Config, target string) ([]Repository, error) {
	names := configuration.RepositoryNames()
	if target != "" && target != "all" {
		if !slices.Contains(names, target) {
			return nil, fmt.Errorf("%s unknown repository %q", repositoryInvalid, target)
		}
		if configuration.Repositories[target].Remotes[config.OriginRemote] == "" {
			return nil, fmt.Errorf("%s repository %q has no configured %s remote", repositoryInvalid, target, config.OriginRemote)
		}
		names = []string{target}
	}
	selected := make([]Repository, 0, len(names))
	for _, name := range names {
		selected = append(selected, Repository{Name: name, Remote: configuration.Repositories[name].Remotes[config.OriginRemote]})
	}
	return selected, nil
}

// update fetches every selected repository in stable configuration order and
// reports what each one did. One failure never stops the remaining
// repositories and never undoes the refs an earlier one already received.
func update(root string, selected []*result, output io.Writer) error {
	var broken []string
	for _, current := range selected {
		if current.Remote == "" {
			current.state = skipped
			continue
		}
		if err := current.fetch(root); err != nil {
			current.state, current.detail = failed, detail(err)
			broken = append(broken, current.Name)
		}
	}
	report(output, selected)
	if len(broken) == 0 {
		return nil
	}
	return fmt.Errorf("%s %s could not be fetched.\n%s", fetchFailed, strings.Join(broken, ", "), stopped)
}

// Update runs the native fetch of one repository from its configured origin.
func Update(root, name string) error {
	return UpdateFrom(root, name, config.OriginRemote)
}

// UpdateFrom runs the native fetch of one repository from source, the origin
// remote name or the URL a caller has already resolved. Explicit options and
// refspec keep mutable Git configuration from adding tags, pruning refs,
// fetching submodules or updating refs outside origin.
func UpdateFrom(root, name, source string) error {
	_, err := git.Run(root, "--git-dir="+repository.Directory(root, name), "fetch", "--quiet",
		"--no-tags", "--no-prune", "--no-recurse-submodules", "--refmap=", source,
		"+refs/heads/*:refs/remotes/"+config.OriginRemote+"/*")
	return err
}

// fetch updates one repository and records whether it moved a remote-tracking
// ref. The refs are compared instead of parsing Git's output, so "unchanged"
// states what the repository really holds.
func (r *result) fetch(root string) error {
	before, err := trackingRefs(root, r.Name)
	if err != nil {
		return err
	}
	if err := Update(root, r.Name); err != nil {
		return err
	}
	after, err := trackingRefs(root, r.Name)
	if err != nil {
		return err
	}
	r.state = unchanged
	if after != before {
		r.state = updated
	}
	return nil
}

// trackingRefs is the complete remote-tracking state of one repository.
func trackingRefs(root, name string) (string, error) {
	return git.Run(root, "--no-optional-locks", "--git-dir="+repository.Directory(root, name),
		"for-each-ref", "--format=%(refname) %(objectname)", "refs/remotes/"+config.OriginRemote+"/")
}

// report prints one line per repository, so a partial failure states exactly
// which repositories were updated before it.
func report(output io.Writer, selected []*result) {
	style := ui.For(output)
	fmt.Fprintf(output, "%s\n\n", style.Heading.Render("Fetch results:"))
	for _, current := range selected {
		fmt.Fprintf(output, "%-10s %s\n", current.Name, current.line(style))
	}
}

func (r *result) line(style ui.Styles) string {
	switch r.state {
	case updated:
		return style.Success.Render("✓ updated " + config.OriginRemote + " refs")
	case unchanged:
		return style.Muted.Render("already up to date")
	case skipped:
		return style.Muted.Render("skipped, no configured " + config.OriginRemote + " remote")
	default:
		return style.Error.Render("failed: " + r.detail)
	}
}

// detail is the first line of an error, so one reported problem stays one line.
func detail(err error) string {
	line, _, _ := strings.Cut(err.Error(), "\n")
	return line
}
