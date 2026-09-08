// Package doctor reports the health of a GitOne project. Every check only
// reads: no file, index, ref, remote ref, lock or recovery state is modified.
// The rules come from the validators the operational commands use, so a
// passing doctor means those commands would accept the project too.
package doctor

import (
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/devidevio/gitone/internal/config"
	"github.com/devidevio/gitone/internal/git"
	"github.com/devidevio/gitone/internal/lock"
	"github.com/devidevio/gitone/internal/push"
	"github.com/devidevio/gitone/internal/repository"
	"github.com/devidevio/gitone/internal/status"
	"github.com/devidevio/gitone/internal/ui"
)

// The marks one check line can carry.
const (
	passed  = "✓"
	failed  = "✗"
	skipped = "-"
)

// minimumGit is the oldest native Git that supports every command GitOne
// runs. gitone init uses git init --initial-branch, which arrived in 2.28.
var minimumGit = [2]int{2, 28}

// check is one reported line: its name, its result and the details explaining
// it. Details are printed below the line, whether the check passed or not.
type check struct {
	name    string
	mark    string
	details []string
	issues  map[string]bool
}

func (c *check) add(format string, arguments ...any) {
	c.details = append(c.details, fmt.Sprintf(format, arguments...))
}

func (c *check) fail(format string, arguments ...any) {
	c.mark = failed
	c.add(format, arguments...)
}

// Report runs every check in a fixed order and prints the result. It returns
// false when a required check failed. Checks whose prerequisites are missing
// are reported as skipped instead of silently disappearing, and independent
// checks keep running after a failure.
func Report(cwd string, output io.Writer) bool {
	root, rootErr := config.DiscoverRoot(cwd)
	configuration, _, configErr := config.Load(cwd)

	directory := cwd
	if rootErr == nil {
		directory = root
	}
	checks := []check{configurationCheck(configuration, configErr), gitCheck(directory)}
	if configErr == nil {
		repositories := repositoryChecks(configuration, root)
		checks = append(checks, repositories...)
		checks = append(checks, projectChecks(configuration, root, repositories)...)
		checks = append(checks, reservedCheck(configuration, root), remotesCheck(configuration, root))
	} else {
		checks = append(checks, check{name: "Project", mark: skipped,
			details: []string{"repositories, paths and remotes are only checked once the configuration is valid"}})
	}
	if rootErr == nil {
		checks = append(checks, lockChecks(root)...)
	}
	return write(output, checks)
}

// configurationCheck reports the merged configuration of .gitone.yml and
// .gitone.local.yml, including its path patterns.
func configurationCheck(configuration *config.Config, err error) check {
	current := check{name: "Configuration", mark: passed}
	if err != nil {
		current.fail("%v", err)
		return current
	}
	patterns := 0
	for _, name := range configuration.RepositoryNames() {
		patterns += len(configuration.Repositories[name].Paths)
	}
	current.add("%d repositories, %d ownership patterns, %d protected patterns, no duplicate patterns",
		len(configuration.Repositories), patterns, len(configuration.ProtectedPaths))
	return current
}

// gitCheck requires a native Git new enough for every command GitOne runs.
// Installing or upgrading Git is left to the user.
func gitCheck(directory string) check {
	current := check{name: "Git", mark: passed}
	output, err := git.RunIsolated(directory, "--version")
	if err != nil {
		current.fail("git is not available: %s", firstLine(err))
		return current
	}
	version := strings.TrimSpace(output)
	major, minor, ok := parseVersion(version)
	switch {
	case !ok:
		current.fail("GIT001 the version cannot be read from %q", version)
	case major < minimumGit[0] || major == minimumGit[0] && minor < minimumGit[1]:
		current.fail("GIT001 %s is too old: GitOne requires Git %d.%d or newer", version, minimumGit[0], minimumGit[1])
	default:
		current.add("%s", version)
	}
	return current
}

// repositoryChecks report one line per configured repository: its internal Git
// directory, its shared working-tree wiring, its branch and its remotes. The
// repositories are independent, so each is reported even when another failed.
func repositoryChecks(configuration *config.Config, root string) []check {
	var checks []check
	for _, name := range configuration.RepositoryNames() {
		current := check{name: "Repository " + name, mark: passed, issues: map[string]bool{}}
		for _, issue := range repository.Check(root, name, configuration.Repositories[name]) {
			current.fail("%v", issue)
			if issue.Kind != "" {
				current.issues[issueKey(issue.Kind, issue.Repository)] = true
			}
		}
		checks = append(checks, current)
	}
	return checks
}

// projectChecks turn the shared project inspection into one check per problem
// class. status.Inspect is what every mutating command validates against, so
// doctor reports exactly the state those commands would refuse. It reads the
// project without the lock, which is what lets doctor inspect during a running
// operation.
func projectChecks(configuration *config.Config, root string, repositories []check) []check {
	buckets := []struct{ name, code string }{
		{"Working tree", "PATH003"},
		{"Unassigned files", "PATH001"},
		{"Ambiguous paths", "PATH002"},
		{"Repository state", "REPO001"},
	}
	result, _, err := status.Inspect(configuration, root)
	if err != nil {
		return []check{{name: "Working tree", mark: failed, details: []string{"PATH003 the project cannot be inspected: " + err.Error()}}}
	}

	checks := make([]check, 0, len(buckets))
	for _, bucket := range buckets {
		current := check{name: bucket.name, mark: passed}
		for _, issue := range result.Issues {
			// A problem one repository check already printed is not repeated.
			if issue.Code == bucket.code && !reported(repositories, issue) {
				current.fail("%s", issue)
			}
		}
		checks = append(checks, current)
	}
	return checks
}

// reservedCheck detects .gitone.local.yml and .gitone/ in a managed index or
// in any commit. GitOne's own state must never be recorded by a repository,
// and a commit that already contains it cannot be fixed by ownership rules.
func reservedCheck(configuration *config.Config, root string) check {
	current := check{name: "Reserved paths", mark: passed}
	for _, issue := range repository.ReservedIssues(configuration, root) {
		current.fail("%v", issue)
	}
	return current
}

// remotesCheck contacts every configured remote and reads its refs. A
// repository without a remote is local-only and stays informational; a remote
// that is configured but cannot be reached fails. Nothing is fetched, pushed
// or written, and no credential prompt can block the check.
func remotesCheck(configuration *config.Config, root string) check {
	current := check{name: "Remotes", mark: passed}
	for _, name := range configuration.RepositoryNames() {
		configured := configuration.Repositories[name]
		if len(configured.Remotes) == 0 {
			current.add("repository %q has no remote and stays local", name)
			continue
		}
		gitDirectory := repository.Directory(root, name)
		if _, err := os.Stat(gitDirectory); err != nil {
			continue
		}
		for _, remoteName := range configured.RemoteNames() {
			if _, err := git.RunOffline(root, "--git-dir="+gitDirectory, "ls-remote", "--quiet", "--heads", remoteName); err != nil {
				current.fail("repository %q remote %q is unavailable: %s", name, remoteName, firstLine(err))
				continue
			}
			current.add("repository %q remote %q is reachable", name, remoteName)
		}
	}
	return current
}

// lockChecks report the project lock and the recorded state of an interrupted
// operation. Doctor only reads them: an active lock is left alone, a stale
// lock is not reclaimed and recovery state is never resolved here.
func lockChecks(root string) []check {
	locks := check{name: "Locks", mark: passed}
	recovery := check{name: "Recovery", mark: passed}
	state, err := lock.Inspect(root)
	if err != nil {
		locks.fail("LOCK001 the lock state cannot be read: %v", err)
		recovery.mark, recovery.details = skipped, []string{"the lock state cannot be read"}
		return []check{locks, recovery}
	}

	switch {
	case state.Active:
		locks.add("a GitOne operation is running: %s", describe(state))
		locks.add("doctor only inspected the project and left the lock untouched")
	case state.Unattributed:
		locks.fail("LOCK001 the lock names no process this machine can check: %s", describe(state))
		locks.add("no operation can be confirmed running, so waiting for it does not end")
		locks.add("follow Repairing an unattributable lock in the usage guide; doctor changed nothing")
	case state.Stale && state.PID <= 0:
		locks.add("a lock file recording no owner remains and the next operation reclaims it")
	case state.Stale:
		locks.add("a lock of the ended %s remains and the next operation reclaims it", describe(state))
	default:
		locks.add("no operation is running")
	}

	switch {
	case state.Interrupted == push.Command:
		recovery.fail("REC001 %s was interrupted: run gitone recover to read back what the remotes really have", state.Interrupted)
		recovery.add("a push is never rolled back and never silently repeated")
	case state.Interrupted != "":
		recovery.fail("REC001 %s was interrupted: run gitone recover to finish it or gitone abort to undo it", state.Interrupted)
		recovery.add("recorded state: %s", lock.RecoveryPath(root))
	case state.Recovery:
		recovery.fail("REC001 an interrupted operation left state in %s without a readable command", lock.RecoveryPath(root))
		recovery.add("run gitone recover to finish it or gitone abort to undo it")
	}
	return []check{locks, recovery}
}

func reported(checks []check, issue status.Issue) bool {
	if issue.Kind == "" {
		return false
	}
	key := issueKey(issue.Kind, issue.Repository)
	return slices.ContainsFunc(checks, func(current check) bool { return current.issues[key] })
}

func issueKey(kind, repository string) string {
	return kind + "\x00" + repository
}

func describe(state lock.State) string {
	who := "unknown process"
	switch {
	case state.PID > 0 && state.Command != "":
		who = fmt.Sprintf("%s, PID %d", state.Command, state.PID)
	case state.PID > 0:
		who = fmt.Sprintf("PID %d", state.PID)
	}
	// The host only matters when it is why the lock cannot be attributed.
	if state.Unattributed && state.Host != "" {
		who += " on host " + state.Host
	}
	return who
}

// write prints every check in the order it was collected and reports whether
// all required checks passed.
func write(output io.Writer, checks []check) bool {
	style := ui.For(output)
	width := 0
	for _, current := range checks {
		width = max(width, len(current.name))
	}
	fmt.Fprintf(output, "%s\n─────────────\n\n", style.Heading.Render("GitOne Doctor"))
	healthy := true
	for _, current := range checks {
		mark := style.Warning.Render(current.mark)
		if current.mark == passed {
			mark = style.Success.Render(current.mark)
		} else if current.mark == failed {
			mark = style.Error.Render(current.mark)
		}
		fmt.Fprintf(output, "%-*s  %s\n", width, current.name, mark)
		detailStyle := style.Muted
		if current.mark == failed {
			detailStyle = style.Error
		}
		for _, detail := range current.details {
			fmt.Fprintf(output, "%*s  %s\n", width, "", detailStyle.Render(detail))
		}
		healthy = healthy && current.mark != failed
	}
	if healthy {
		fmt.Fprintf(output, "\n%s\n", style.Success.Render("Everything looks good."))
		return true
	}
	fmt.Fprintf(output, "\n%s\n", style.Error.Render("Some checks failed. Nothing was changed."))
	return false
}

func parseVersion(value string) (major, minor int, ok bool) {
	numbers := strings.Fields(value)
	if len(numbers) < 3 {
		return 0, 0, false
	}
	parts := strings.Split(numbers[2], ".")
	if len(parts) < 2 {
		return 0, 0, false
	}
	major, majorErr := strconv.Atoi(parts[0])
	minor, minorErr := strconv.Atoi(parts[1])
	return major, minor, majorErr == nil && minorErr == nil
}

// firstLine keeps one reported problem on one line.
func firstLine(err error) string {
	line, _, _ := strings.Cut(err.Error(), "\n")
	return line
}
