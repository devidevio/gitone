// Package clone creates a GitOne project from a bootstrap repository. The
// repository that owns the committed .gitone.yml is adopted with its complete
// native history; every other repository the committed configuration declares
// is initialized and, when it has an origin, fetched and checked out. The
// whole project is built inside a command-owned temporary sibling of the
// destination and published only after it validates, so a failure leaves no
// destination behind and changes no existing path.
package clone

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/devidevio/gitone/internal/config"
	"github.com/devidevio/gitone/internal/fetch"
	"github.com/devidevio/gitone/internal/git"
	"github.com/devidevio/gitone/internal/repository"
	"github.com/devidevio/gitone/internal/status"
	"github.com/devidevio/gitone/internal/ui"
)

const (
	// failed is the stable code of every clone failure.
	failed = "CLONE001"

	// unchanged closes every failure. Everything a clone creates lives in one
	// temporary directory that is removed with it.
	unchanged = "No destination was created and no existing path was changed."

	// temporaryPattern names the command-owned directory the project is built
	// in, next to the destination so publishing it is a rename.
	temporaryPattern = ".gitone-clone-*"
)

// result is one configured repository and what the clone did with it.
type result struct {
	name   string
	branch string
	commit string
	// unborn explains why a repository has no commit and stays empty for a
	// checked-out one.
	unborn string
}

// Clone creates the project of the bootstrap repository at url. directory is
// the destination, empty for the repository name of the URL.
func Clone(url, directory, cwd string, input io.Reader, output io.Writer) error {
	destination, err := resolveDestination(cwd, url, directory)
	if err != nil {
		return err
	}
	parent := filepath.Dir(destination)
	temporary, err := os.MkdirTemp(parent, temporaryPattern)
	if err != nil {
		return fail("no temporary directory could be created in %s: %s\n%s", parent, detail(err), unchanged)
	}
	project := filepath.Join(temporary, "project")

	err = ui.SpinBuffered(input, output, "Cloning "+url, func(rendered io.Writer) error {
		results, err := build(url, project, destination)
		if err != nil {
			return err
		}
		if err := publish(project, destination); err != nil {
			return fail("the project could not be created at %s: %s\n%s", destination, detail(err), unchanged)
		}
		report(rendered, destination, results)
		return nil
	})
	if err != nil {
		_ = os.RemoveAll(temporary)
		return err
	}
	_ = os.RemoveAll(temporary)
	return nil
}

// build creates the complete project below project. Nothing outside that
// directory is read or written, so its caller can discard a failed attempt.
func build(url, project, destination string) ([]*result, error) {
	configuration, bootstrap, err := adopt(url, project, destination)
	if err != nil {
		return nil, err
	}
	// Prepare creates the repositories the bootstrap repository is not,
	// exactly as an initialized project has them.
	if err := repository.Prepare(configuration, project); err != nil {
		return nil, err
	}

	results, err := collect(configuration, project, destination, bootstrap)
	if err != nil {
		return nil, err
	}
	if err := validateTrees(configuration, project, results); err != nil {
		return nil, err
	}
	for _, current := range results {
		if current.commit == "" {
			continue
		}
		if err := checkout(project, current); err != nil {
			return nil, fail("repository %q could not be checked out at %s: %s\n%s",
				current.name, short(current.commit), detail(err), unchanged)
		}
	}
	// The ignore entries are written after the checkouts, so a committed
	// .gitignore cannot overwrite them again.
	if err := repository.UpdateIgnore(project); err != nil {
		return nil, err
	}
	if err := verify(configuration, project, results); err != nil {
		return nil, err
	}
	return results, nil
}

// adopt clones the bootstrap repository, reads the configuration it commits
// and turns its native Git directory into the managed repository owning
// .gitone.yml. Its complete history is kept.
func adopt(url, project, destination string) (*config.Config, string, error) {
	cloned := filepath.Join(project, ".git")
	if _, err := git.Run(filepath.Dir(project), "clone", "--quiet", "--no-checkout",
		"--origin", config.OriginRemote, cloneSource(url, destination), project); err != nil {
		return nil, "", fail("%s could not be cloned: %s\n%s", url, detail(err), unchanged)
	}

	// The configuration is read from the commit, not from a checkout, so an
	// unusable bootstrap repository is refused before any file is written.
	contents, err := git.Run(project, "--git-dir="+cloned, "show", "HEAD:"+config.PublicFile)
	if err != nil {
		return nil, "", fail("%s has no committed %s on its default branch: %s\n%s",
			url, config.PublicFile, detail(err), unchanged)
	}
	if err := os.WriteFile(filepath.Join(project, config.PublicFile), []byte(contents), 0o644); err != nil {
		return nil, "", err
	}
	configuration, root, err := config.Load(project)
	if err != nil {
		return nil, "", err
	}

	name, err := owner(configuration)
	if err != nil {
		return nil, "", err
	}
	bootstrap := configuration.Repositories[name]
	switch origin := bootstrap.Remotes[config.OriginRemote]; {
	case origin == "":
		return nil, "", fail("%s of %s assigns itself to repository %q, which has no configured %s\n"+
			"add the %s of %q to the committed configuration\n%s",
			config.PublicFile, url, name, config.OriginRemote, config.OriginRemote, name, unchanged)
	case origin != url:
		return nil, "", fail("%s of %s assigns itself to repository %q, whose configured %s is %s\n"+
			"clone %s instead, or correct the committed configuration\n%s",
			config.PublicFile, url, name, config.OriginRemote, origin, origin, unchanged)
	}
	head, err := git.Run(project, "--git-dir="+cloned, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return nil, "", fail("the default branch of %s cannot be read: %s\n%s", url, detail(err), unchanged)
	}
	if strings.TrimSpace(head) != configuration.DefaultBranch {
		return nil, "", fail("the default branch of %s is %q, not the configured %q\n%s",
			url, strings.TrimSpace(head), configuration.DefaultBranch, unchanged)
	}

	adopted := repository.Directory(root, name)
	if err := os.MkdirAll(filepath.Dir(adopted), 0o700); err != nil {
		return nil, "", err
	}
	if err := os.Rename(cloned, adopted); err != nil {
		return nil, "", err
	}
	if err := repository.Attach(root, name, bootstrap); err != nil {
		return nil, "", err
	}
	return configuration, name, nil
}

// owner is the repository the committed configuration assigns .gitone.yml to.
// A bootstrap repository is only unambiguous when exactly one repository owns
// that file.
func owner(configuration *config.Config) (string, error) {
	matcher, err := configuration.Matcher()
	if err != nil {
		return "", err
	}
	owners, _ := matcher.Owners(config.PublicFile)
	if len(owners) != 1 {
		return "", fail("the committed %s is owned by %s: exactly one repository must own it\n%s",
			config.PublicFile, describe(owners), unchanged)
	}
	return owners[0], nil
}

// collect fetches every repository with an origin and records the commit its
// default branch is checked out at. The bootstrap repository already received
// its refs from the clone. A repository without an origin stays unborn and is
// reported; a configured origin without its default branch is inconsistent.
func collect(configuration *config.Config, project, destination, bootstrap string) ([]*result, error) {
	var results []*result
	for _, name := range configuration.RepositoryNames() {
		configured := configuration.Repositories[name]
		current := &result{name: name, branch: configuration.DefaultBranch}
		results = append(results, current)

		if configured.Remotes[config.OriginRemote] == "" {
			current.unborn = "no configured " + config.OriginRemote + " remote"
			continue
		}
		if name != bootstrap {
			if err := fetch.UpdateFrom(project, name, fetchSource(configured.Remotes[config.OriginRemote], destination)); err != nil {
				return nil, fail("repository %q could not be fetched from %s: %s\n%s",
					name, configured.Remotes[config.OriginRemote], detail(err), unchanged)
			}
		}
		commit, err := repository.Reference(project, name, "refs/remotes/"+config.OriginRemote+"/"+current.branch)
		if err != nil {
			return nil, err
		}
		if commit == "" {
			return nil, fail("repository %q has no %s/%s to check out\n"+
				"push its configured default branch, or remove its %s until the repository is ready\n%s",
				current.name, config.OriginRemote, current.branch, config.OriginRemote, unchanged)
		}
		current.commit = commit
	}
	return results, nil
}

// validateTrees refuses every incoming tree that contains a path its
// repository must not manage. It completes before the first file is written,
// so an unsafe tree never reaches the working tree.
func validateTrees(configuration *config.Config, project string, results []*result) error {
	matcher, err := configuration.Matcher()
	if err != nil {
		return err
	}
	commits := map[string]string{}
	for _, current := range results {
		if current.commit != "" {
			commits[current.name] = current.commit
		}
	}
	refused, err := repository.TreeIssues(matcher, project, commits)
	if err != nil {
		return err
	}
	var issues []error
	for _, entry := range refused {
		issues = append(issues, fmt.Errorf("%s repository %q must not manage %s from %s: %s",
			failed, entry.Repository, entry.Path, short(commits[entry.Repository]), entry.Reason))
	}
	if len(issues) == 0 {
		return nil
	}
	return errors.Join(append(issues, errors.New(unchanged))...)
}

// checkout puts one repository on its default branch: the branch points at
// the fetched origin commit, tracks that branch, and the index and the
// working-tree files are written. The index starts empty, so the reset only
// adds this repository's own validated paths.
func checkout(project string, current *result) error {
	gitDirectory := "--git-dir=" + repository.Directory(project, current.name)
	if _, err := git.Run(project, gitDirectory, "update-ref", "refs/heads/"+current.branch, current.commit); err != nil {
		return err
	}
	for _, setting := range [][2]string{
		{"branch." + current.branch + ".remote", config.OriginRemote},
		{"branch." + current.branch + ".merge", "refs/heads/" + current.branch},
	} {
		if _, err := git.Run(project, gitDirectory, "config", setting[0], setting[1]); err != nil {
			return err
		}
	}
	_, err := git.Run(project, gitDirectory, "--work-tree="+project, "reset", "--hard", "--quiet", current.branch)
	return err
}

// verify runs the repository, path and configuration checks doctor reports,
// so a published project is one the other commands accept.
func verify(configuration *config.Config, project string, results []*result) error {
	var issues []error
	for _, name := range configuration.RepositoryNames() {
		for _, issue := range repository.Check(project, name, configuration.Repositories[name]) {
			issues = append(issues, issue)
		}
	}
	issues = append(issues, repository.ReservedIssues(configuration, project)...)
	for _, current := range results {
		if current.commit == "" {
			continue
		}
		tracks, err := repository.TracksOriginBranch(project, current.name, current.branch)
		if err != nil {
			return err
		}
		if !tracks {
			issues = append(issues, fmt.Errorf("%s repository %q does not track %s/%s",
				failed, current.name, config.OriginRemote, current.branch))
		}
	}
	if err := errors.Join(issues...); err != nil {
		return fmt.Errorf("%w\n%s", err, unchanged)
	}
	if _, _, err := status.Validate(configuration, project); err != nil {
		return fmt.Errorf("%w\n%s", err, unchanged)
	}
	return nil
}

// resolveDestination is the directory the project is created in. It must not
// exist yet, and its parent must, because the project is built next to it.
func resolveDestination(cwd, url, directory string) (string, error) {
	if directory == "" {
		directory = defaultDirectory(url)
		if directory == "" {
			return "", fail("no directory name can be derived from %s: name one explicitly\n%s", url, unchanged)
		}
	}
	destination := directory
	if !filepath.IsAbs(destination) {
		destination = filepath.Join(cwd, destination)
	}
	destination = filepath.Clean(destination)

	switch _, err := os.Lstat(destination); {
	case err == nil:
		return "", fail("%s already exists: gitone clone only creates a new directory\n%s", destination, unchanged)
	case !errors.Is(err, os.ErrNotExist):
		return "", fail("%s cannot be inspected: %s\n%s", destination, detail(err), unchanged)
	}
	parent := filepath.Dir(destination)
	info, err := os.Stat(parent)
	if err != nil {
		return "", fail("%s cannot be created because %s cannot be inspected: %s\n%s",
			destination, parent, detail(err), unchanged)
	}
	if !info.IsDir() {
		return "", fail("%s cannot be created because %s is not a directory\n%s", destination, parent, unchanged)
	}
	return destination, nil
}

// defaultDirectory is the repository name of a clone URL, the same name
// native git clone would use. It is empty when the URL has no usable name.
func defaultDirectory(url string) string {
	name := strings.TrimSuffix(strings.TrimRight(url, "/"), ".git")
	if index := strings.LastIndexAny(name, "/:"); index >= 0 {
		name = name[index+1:]
	}
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `\`) {
		return ""
	}
	return name
}

// cloneSource resolves a relative local origin from the final project root,
// where Git will resolve that same configured origin after publication.
func cloneSource(url, destination string) string {
	if filepath.IsAbs(url) || strings.Contains(url, "://") {
		return url
	}
	host := url
	if index := strings.IndexAny(host, `/\\`); index >= 0 {
		host = host[:index]
	}
	if strings.Contains(host, ":") {
		return url
	}
	return filepath.Clean(filepath.Join(destination, url))
}

// fetchSource is what one repository is fetched from while the project still
// lives in the temporary directory: the resolved path of a relative local
// origin, which Git would otherwise resolve from there, and the origin remote
// itself for every URL that resolves the same wherever it is used.
func fetchSource(url, destination string) string {
	if resolved := cloneSource(url, destination); resolved != url {
		return resolved
	}
	return config.OriginRemote
}

// report prints one line per repository and the next steps of every
// repository that stays unborn.
func report(output io.Writer, destination string, results []*result) {
	style := ui.For(output)
	fmt.Fprintf(output, "%s\n\n", style.Heading.Render("Cloned into "+destination+":"))
	for _, current := range results {
		fmt.Fprintf(output, "%-10s %s\n", current.name, current.line(style))
	}
	unborn := 0
	for _, current := range results {
		if current.unborn == "" {
			continue
		}
		if unborn == 0 {
			fmt.Fprintln(output)
		}
		unborn++
		fmt.Fprint(output, repository.Unborn(current.name, current.unborn))
	}
	fmt.Fprintf(output, "\nRepositories that exist only in %s cannot be discovered from a clone and\n"+
		"are therefore never cloned automatically.\n", config.LocalFile)
	fmt.Fprintf(output, "\nRun \"gitone status\" in %s to continue.\n", destination)
}

func (r *result) line(style ui.Styles) string {
	if r.unborn != "" {
		return style.Muted.Render("initialized, unborn " + r.branch + ": " + r.unborn)
	}
	return style.Success.Render(fmt.Sprintf("✓ checked out %s at %s", r.branch, short(r.commit)))
}

func describe(owners []string) string {
	if len(owners) == 0 {
		return "no repository"
	}
	return strings.Join(owners, ", ")
}

func fail(format string, arguments ...any) error {
	return fmt.Errorf("%s %s", failed, fmt.Sprintf(format, arguments...))
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
