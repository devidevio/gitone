// Package branch inspects, creates and deletes the local branches of the
// managed repositories. A creation adds one branch to every repository at
// once, at the commit each one currently has checked out; a deletion removes
// one merged branch from every repository at once. Both act only after every
// repository passed preflight, and both write nothing but refs and branch
// settings: HEAD, the indexes and the working-tree files stay exactly as they
// were.
package branch

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/devidevio/gitone/internal/config"
	"github.com/devidevio/gitone/internal/git"
	"github.com/devidevio/gitone/internal/lock"
	"github.com/devidevio/gitone/internal/repository"
	"github.com/devidevio/gitone/internal/status"
	"github.com/devidevio/gitone/internal/ui"
)

const (
	// Command is the recorded command of an interrupted branch creation.
	Command = "gitone branch"

	branchFailed     = "BRANCH001"
	recoveryRequired = "REC001"

	// detached labels a repository whose HEAD is not on a branch.
	detached = "(detached)"

	// unmodified closes every message of a refused or rolled back creation.
	unmodified = "No branch, HEAD, index or working-tree file was changed."
)

// State is the branch state of one managed repository. Current is the branch
// HEAD points at, empty for a detached HEAD, and Head the commit that branch
// holds, empty while it is unborn.
type State struct {
	Name     string
	Current  string
	Head     string
	Branches []string
}

// Branch lists the local branches of every managed repository, creates name
// in all of them at once, or deletes it from all of them. An empty name
// lists.
func Branch(configuration *config.Config, root, name string, deleting bool, input io.Reader, output io.Writer) error {
	switch {
	case deleting:
		return remove(configuration, root, name, input, output)
	case name == "":
		return list(configuration, root, output)
	}
	return create(configuration, root, name, input, output)
}

// list reports every local branch and the current branch of every
// repository. It only reads, so inconsistent state is shown instead of
// refused: that is exactly what the list exists for.
func list(configuration *config.Config, root string, output io.Writer) error {
	release, err := lock.AcquireRead(root)
	if err != nil {
		return err
	}
	defer release()

	inspected, err := Inspect(configuration, root)
	if err != nil {
		return err
	}
	writeList(output, inspected)
	return nil
}

// create adds one branch to every repository at its current HEAD. No ref is
// written before every repository passed preflight, and a failure afterwards
// either removes the refs this creation made or leaves recovery state.
func create(configuration *config.Config, root, name string, input io.Reader, output io.Writer) error {
	if err := validate(name); err != nil {
		return err
	}
	release, err := lock.Acquire(root, Command)
	if err != nil {
		return err
	}
	defer release()

	inspected, err := Inspect(configuration, root)
	if err != nil {
		return err
	}
	if err := preflight(inspected, name); err != nil {
		return err
	}
	// The complete working tree is validated before the first ref is
	// written, exactly like every other mutating command. Branch preflight
	// runs first, so a branch problem is reported as one.
	if _, _, err := status.Validate(configuration, root); err != nil {
		return err
	}
	return ui.SpinBuffered(input, output, "Creating branch "+name, func(rendered io.Writer) error {
		return apply(root, inspected, name, rendered)
	})
}

// validate applies the configured branch rule, which is native Git's own, so
// a branch name is accepted here exactly when the configuration accepts it.
func validate(name string) error {
	err := config.ValidateBranch(name)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, git.ErrUnavailable):
		return err
	default:
		return fmt.Errorf("%s %q is not a valid branch name", branchFailed, name)
	}
}

// Inspect reads the branches, the current branch and its commit of every
// configured repository, in configuration order. gitone switch reads the
// same state, so it is described in exactly one place.
func Inspect(configuration *config.Config, root string) ([]State, error) {
	inspected := make([]State, 0, len(configuration.Repositories))
	for _, name := range configuration.RepositoryNames() {
		current := State{Name: name}
		// A detached HEAD makes symbolic-ref exit non-zero and is the only
		// expected failure here; unusable metadata is already refused by the
		// readiness check every branch command runs before this.
		if reference, err := git.Run(root, "--git-dir="+repository.Directory(root, name),
			"symbolic-ref", "--quiet", "--short", "HEAD"); err == nil {
			current.Current = strings.TrimSpace(reference)
		}
		listed, err := git.Run(root, "--git-dir="+repository.Directory(root, name),
			"for-each-ref", "--format=%(refname:short)", "refs/heads/")
		if err != nil {
			return nil, err
		}
		// A ref name can contain no whitespace, so the sorted output of
		// for-each-ref splits into exactly the branch names.
		current.Branches = strings.Fields(listed)
		if current.Current != "" {
			if current.Head, err = repository.Reference(root, name, "refs/heads/"+current.Current); err != nil {
				return nil, err
			}
		}
		inspected = append(inspected, current)
	}
	return inspected, nil
}

// preflight refuses every project state in which one shared branch cannot be
// created. It reports all problems at once and completes before the first
// ref is written.
func preflight(inspected []State, name string) error {
	var issues []string
	for _, current := range inspected {
		switch {
		case current.Current == "":
			issues = append(issues, problem(current.Name, "is not on a branch"))
		case current.Head == "":
			issues = append(issues, problem(current.Name, "has no commit on %s yet: commit before creating a branch", current.Current))
		}
		if slices.Contains(current.Branches, name) {
			issues = append(issues, problem(current.Name, "already has branch %s", name))
		}
	}
	if len(shared(inspected)) > 1 {
		issues = append(issues, fmt.Sprintf("%s the repositories are not on the same branch: %s",
			branchFailed, strings.Join(describe(inspected), ", ")))
	}
	if len(issues) == 0 {
		return nil
	}
	return errors.New(strings.Join(append(issues, unmodified), "\n"))
}

// shared are the distinct current branches of the repositories that are on
// one. More than one of them means the project has no single branch to
// create the new one beside.
func shared(inspected []State) []string {
	current := map[string]bool{}
	for _, one := range inspected {
		if one.Current != "" {
			current[one.Current] = true
		}
	}
	return slices.Sorted(maps.Keys(current))
}

// describe names the current branch of every repository, in configuration
// order, for a message that has to show the disagreement itself.
func describe(inspected []State) []string {
	described := make([]string, 0, len(inspected))
	for _, current := range inspected {
		described = append(described, current.Name+" on "+branchName(current))
	}
	return described
}

// writeList prints every branch with the repositories holding it, the
// current branch of every repository, and one note per inconsistency.
func writeList(output io.Writer, inspected []State) {
	style := ui.For(output)
	fmt.Fprintf(output, "%s\n\n", style.Heading.Render("Branches"))
	writer := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "BRANCH\tREPOSITORIES")
	for _, name := range names(inspected) {
		fmt.Fprintf(writer, "%s\t%s\n", name, join(holders(inspected, name)))
	}
	_ = writer.Flush()

	fmt.Fprintf(output, "\n%s\n", style.Heading.Render("Current branch"))
	for _, current := range inspected {
		fmt.Fprintf(output, "  %s: %s\n", current.Name, style.Branch.Render(branchName(current)))
	}
	if notes := inconsistencies(inspected); len(notes) != 0 {
		fmt.Fprintln(output)
		for _, note := range notes {
			fmt.Fprintln(output, style.Warning.Render(note))
		}
	}
}

// names are every local branch of the project plus every current branch that
// is still unborn, sorted, so the list is deterministic and a repository
// waiting for its first commit is visible.
func names(inspected []State) []string {
	branches := map[string]bool{}
	for _, current := range inspected {
		for _, name := range current.Branches {
			branches[name] = true
		}
		if current.Current != "" {
			branches[current.Current] = true
		}
	}
	return slices.Sorted(maps.Keys(branches))
}

// holders are the repositories whose refs contain a branch, in configuration
// order.
func holders(inspected []State, name string) []string {
	var holding []string
	for _, current := range inspected {
		if slices.Contains(current.Branches, name) {
			holding = append(holding, current.Name)
		}
	}
	return holding
}

// missing are the repositories a branch does not exist in, in configuration
// order.
func missing(inspected []State, name string) []string {
	var lacking []string
	for _, current := range inspected {
		if !slices.Contains(current.Branches, name) {
			lacking = append(lacking, current.Name)
		}
	}
	return lacking
}

// inconsistencies states every branch that only part of the project has and
// every disagreement about the current branch, so the read-only list never
// leaves them to be spotted in the table.
func inconsistencies(inspected []State) []string {
	var notes []string
	for _, name := range names(inspected) {
		switch lacking := missing(inspected, name); {
		case len(lacking) == len(inspected):
			// Only a current branch nobody has committed on yet reaches this.
			notes = append(notes, fmt.Sprintf("%s does not exist in any repository yet.", name))
		case len(lacking) != 0:
			notes = append(notes, fmt.Sprintf("%s is missing from %s.", name, strings.Join(lacking, ", ")))
		}
	}
	if len(shared(inspected)) > 1 {
		notes = append(notes, "The repositories are not on the same branch.")
	}
	for _, current := range inspected {
		if current.Current == "" {
			notes = append(notes, fmt.Sprintf("%s is not on a branch.", current.Name))
		}
	}
	return notes
}

// report states the created branch of every repository and that nothing else
// moved, because a Git branch command that leaves HEAD alone is unusual.
func report(output io.Writer, inspected []State, name string) {
	style := ui.For(output)
	fmt.Fprintf(output, "%s\n\n", style.Heading.Render("Branch "+name+" created:"))
	for _, current := range inspected {
		fmt.Fprintf(output, "%-10s %s\n", current.Name,
			style.Success.Render(fmt.Sprintf("✓ %s at %s", name, short(current.Head))))
	}
	fmt.Fprintf(output, "\n%s\n", style.Muted.Render(
		"No repository switched branches. Every HEAD, index and working-tree file is unchanged."))
}

func problem(name, format string, arguments ...any) string {
	return fmt.Sprintf("%s repository %q %s", branchFailed, name, fmt.Sprintf(format, arguments...))
}

func branchName(current State) string {
	if current.Current == "" {
		return detached
	}
	return current.Current
}

func join(names []string) string {
	if len(names) == 0 {
		return "-"
	}
	return strings.Join(names, ", ")
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
