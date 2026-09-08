package switching_test

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/devidevio/gitone/internal/config"
	"github.com/devidevio/gitone/internal/git"
	"github.com/devidevio/gitone/internal/lock"
	"github.com/devidevio/gitone/internal/repository"
	"github.com/devidevio/gitone/internal/stage"
	"github.com/devidevio/gitone/internal/switching"
)

// twoRepositories is the seeded project configuration, rendered from the same
// paths a reviewed target policy is compared against.
var twoRepositories = committed(publicPaths, "\n")

// names are the managed repositories in configuration order, which is also
// the order a switch works in.
var names = []string{"private", "public"}

func project(t *testing.T) (*config.Config, string) {
	t.Helper()
	return configured(t, twoRepositories)
}

// configured creates an initialized project from one rendered configuration
// and gives every repository one commit of the files it owns.
func configured(t *testing.T, contents string) (*config.Config, string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	write(t, root, ".gitone.yml", contents)
	configuration, discovered, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.Init(configuration, discovered); err != nil {
		t.Fatal(err)
	}
	write(t, root, "README.md", "one\n")
	write(t, root, "src/site.css", "body{}\n")
	write(t, root, "secrets/key.txt", "secret\n")
	run(t, discovered, "public", "add", "--", "README.md", "src/site.css", ".gitignore", ".gitone.yml")
	run(t, discovered, "public", "commit", "-m", "public subject")
	run(t, discovered, "private", "add", "--", "secrets/key.txt")
	run(t, discovered, "private", "commit", "-m", "private subject")
	return configuration, discovered
}

// feature is a project whose repositories are on main and both have a feature
// branch adding one owned file. That is the state every switch starts from.
func feature(t *testing.T) (*config.Config, string) {
	t.Helper()
	configuration, root := project(t)
	branch(t, root, "public", "feature", "src/app.js", "app\n")
	branch(t, root, "private", "feature", "secrets/extra.txt", "extra\n")
	return configuration, root
}

// branch adds one branch holding one additional file and returns the
// repository to main, so the new file is not in the working tree afterwards.
func branch(t *testing.T, root, name, branchName, path, contents string) {
	t.Helper()
	run(t, root, name, "checkout", "--quiet", "-b", branchName)
	write(t, root, path, contents)
	run(t, root, name, "add", "--", path)
	run(t, root, name, "commit", "-m", branchName+" in "+name)
	run(t, root, name, "checkout", "--quiet", "main")
}

func write(t *testing.T, root, name, contents string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

// run executes git against one managed repository the way GitOne does.
func run(t *testing.T, root, name string, arguments ...string) string {
	t.Helper()
	arguments = append([]string{"--git-dir=" + repository.Directory(root, name), "--work-tree=" + root,
		"-c", "user.name=GitOne", "-c", "user.email=gitone@example.com"}, arguments...)
	output, err := git.Run(root, arguments...)
	if err != nil {
		t.Fatal(err)
	}
	return output
}

func switchTo(configuration *config.Config, root, name string) (string, error) {
	return answered(configuration, root, name, false, "")
}

// answered runs a switch that approves a target policy change with the flag,
// with an answer, or with neither. No test answers a real terminal question.
func answered(configuration *config.Config, root, name string, accepted bool, answer string) (string, error) {
	return attempted(configuration, root, name, accepted, false, answer)
}

// attempted runs a switch with any combination of the two acceptance flags.
func attempted(configuration *config.Config, root, name string, accepted, assigned bool, answer string) (string, error) {
	return invoke(configuration, root, name, false, accepted, assigned, answer)
}

// createTo runs a switch that creates the branch in every repository first.
func createTo(configuration *config.Config, root, name string) (string, error) {
	return invoke(configuration, root, name, true, false, false, "")
}

// invoke runs a switch with any combination of the create and acceptance
// flags.
func invoke(configuration *config.Config, root, name string, creating, accepted, assigned bool, answer string) (string, error) {
	var output bytes.Buffer
	err := switching.Switch(configuration, root, name, creating, accepted, assigned, answer != "", strings.NewReader(answer), &output)
	return output.String(), err
}

func reference(t *testing.T, root, name, ref string) string {
	t.Helper()
	commit, err := repository.Reference(root, name, ref)
	if err != nil {
		t.Fatal(err)
	}
	return commit
}

// current is the branch one repository has checked out.
func current(t *testing.T, root, name string) string {
	t.Helper()
	return strings.TrimSpace(run(t, root, name, "symbolic-ref", "--short", "HEAD"))
}

func exists(t *testing.T, root, path string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(root, filepath.FromSlash(path)))
	return err == nil
}

func TestSwitchMovesEveryRepositoryAndItsOwnedFiles(t *testing.T) {
	configuration, root := feature(t)

	output, err := switchTo(configuration, root, "feature")
	if err != nil {
		t.Fatalf("switch: %v\noutput: %s", err, output)
	}
	for _, name := range names {
		if got := current(t, root, name); got != "feature" {
			t.Fatalf("%s is on %q, want feature", name, got)
		}
		if !strings.Contains(output, name) {
			t.Fatalf("output does not name %s:\n%s", name, output)
		}
	}
	if !strings.Contains(output, "main → feature") {
		t.Fatalf("output does not report the previous and the new branch:\n%s", output)
	}
	for _, path := range []string{"src/app.js", "secrets/extra.txt", "README.md", "secrets/key.txt"} {
		if !exists(t, root, path) {
			t.Fatalf("%s is missing from the working tree", path)
		}
	}
	if _, err := os.Stat(lock.RecoveryPath(root)); !os.IsNotExist(err) {
		t.Fatalf("recovery state remained: %v", err)
	}
}

func TestSwitchBackRemovesTheFilesOfTheBranchItLeaves(t *testing.T) {
	configuration, root := feature(t)
	if output, err := switchTo(configuration, root, "feature"); err != nil {
		t.Fatalf("switch: %v\noutput: %s", err, output)
	}

	if output, err := switchTo(configuration, root, "main"); err != nil {
		t.Fatalf("switch back: %v\noutput: %s", err, output)
	}
	for _, path := range []string{"src/app.js", "secrets/extra.txt"} {
		if exists(t, root, path) {
			t.Fatalf("%s survived the switch back to main", path)
		}
	}
}

func TestSwitchToTheCurrentBranchChangesNothing(t *testing.T) {
	configuration, root := feature(t)
	if output, err := switchTo(configuration, root, "feature"); err != nil {
		t.Fatalf("switch: %v\noutput: %s", err, output)
	}

	output, err := switchTo(configuration, root, "feature")
	if err != nil {
		t.Fatalf("second switch: %v\noutput: %s", err, output)
	}
	if strings.Count(output, "already on feature") != len(names) {
		t.Fatalf("output does not report every repository as unchanged:\n%s", output)
	}
	if _, err := os.Stat(lock.RecoveryPath(root)); !os.IsNotExist(err) {
		t.Fatalf("recovery state remained: %v", err)
	}
}

// A project whose repositories drifted apart is exactly what a switch puts
// back on one branch, so it is the one project issue switch tolerates.
func TestSwitchRepairsRepositoriesOnDifferentBranches(t *testing.T) {
	configuration, root := feature(t)
	run(t, root, "private", "checkout", "--quiet", "feature")

	output, err := switchTo(configuration, root, "main")
	if err != nil {
		t.Fatalf("switch: %v\noutput: %s", err, output)
	}
	for _, name := range names {
		if got := current(t, root, name); got != "main" {
			t.Fatalf("%s is on %q, want main", name, got)
		}
	}
	if exists(t, root, "secrets/extra.txt") {
		t.Fatal("secrets/extra.txt survived the switch back to main")
	}
}

func TestSwitchRefusesABranchMissingFromOneRepository(t *testing.T) {
	configuration, root := project(t)
	branch(t, root, "public", "feature", "src/app.js", "app\n")

	_, err := switchTo(configuration, root, "feature")
	if err == nil || !strings.Contains(err.Error(), `SWITCH001 repository "private" has no branch feature`) {
		t.Fatalf("error = %v, want the missing branch of private", err)
	}
	if !strings.Contains(err.Error(), "No branch, HEAD, index or working-tree file was changed.") {
		t.Fatalf("error does not state the unchanged project: %v", err)
	}
	if got := current(t, root, "public"); got != "main" {
		t.Fatalf("public is on %q, want main", got)
	}
	if !strings.Contains(err.Error(), "https://gitone.io/docs/usage#repairing-missing-branches") ||
		strings.Contains(err.Error(), "create it with gitone branch feature") {
		t.Fatalf("missing-branch repair guidance = %v", err)
	}
}

func TestSwitchSucceedsAfterRepairingMissingBranch(t *testing.T) {
	configuration, root := project(t)
	branch(t, root, "public", "feature", "src/app.js", "app\n")

	// The documented human repair creates only the missing ref; GitOne
	// still validates and performs the eventual checkout.
	run(t, root, "private", "branch", "feature", "HEAD")
	if _, err := switchTo(configuration, root, "feature"); err != nil {
		t.Fatalf("switch after documented repair: %v", err)
	}
}

func TestSwitchDoesNotTreatABranchPrefixAsTheRequestedBranch(t *testing.T) {
	configuration, root := project(t)
	for _, name := range names {
		run(t, root, name, "branch", "feature/one")
	}

	_, err := switchTo(configuration, root, "feature")
	if err == nil || !strings.Contains(err.Error(), `has no branch feature`) {
		t.Fatalf("error = %v, want an exact missing-branch failure", err)
	}
	for _, name := range names {
		if got := current(t, root, name); got != "main" {
			t.Fatalf("%s is on %q, want main", name, got)
		}
	}
}

func TestSwitchRefusesADetachedHead(t *testing.T) {
	configuration, root := feature(t)
	run(t, root, "private", "checkout", "--quiet", "--detach", "main")

	_, err := switchTo(configuration, root, "feature")
	if err == nil || !strings.Contains(err.Error(), `SWITCH001 repository "private" is not on a branch`) {
		t.Fatalf("error = %v, want the detached private repository", err)
	}
	if got := current(t, root, "public"); got != "main" {
		t.Fatalf("public is on %q, want main", got)
	}
}

func TestSwitchRefusesLocalChanges(t *testing.T) {
	configuration, root := feature(t)
	write(t, root, "README.md", "changed\n")
	write(t, root, "secrets/key.txt", "changed\n")
	run(t, root, "private", "add", "--", "secrets/key.txt")

	_, err := switchTo(configuration, root, "feature")
	if err == nil {
		t.Fatal("switch succeeded with local changes")
	}
	for _, want := range []string{
		`SWITCH001 repository "private" has staged changes on main`,
		`SWITCH001 repository "public" has unstaged changes on main`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %v, want %q", err, want)
		}
	}
	for _, name := range names {
		if got := current(t, root, name); got != "main" {
			t.Fatalf("%s is on %q, want main", name, got)
		}
	}
}

func TestSwitchRefusesLocalChangesOnARepositoryAlreadyOnTheTargetBranch(t *testing.T) {
	configuration, root := feature(t)
	run(t, root, "private", "checkout", "--quiet", "feature")
	write(t, root, "secrets/key.txt", "changed\n")

	_, err := switchTo(configuration, root, "feature")
	if err == nil || !strings.Contains(err.Error(), `repository "private" has unstaged changes on feature`) {
		t.Fatalf("error = %v, want the dirty target repository", err)
	}
	if got := current(t, root, "public"); got != "main" {
		t.Fatalf("public is on %q, want main", got)
	}
}

func TestSwitchRefusesAForeignPathInTheTargetTree(t *testing.T) {
	configuration, root := feature(t)
	// private commits a path public owns on its own branch, which no checkout
	// of private may ever write into the shared working tree.
	run(t, root, "private", "checkout", "--quiet", "-b", "rogue")
	run(t, root, "private", "add", "--", "README.md")
	run(t, root, "private", "commit", "-m", "rogue private")
	run(t, root, "private", "checkout", "--quiet", "main")
	write(t, root, "README.md", "one\n")
	run(t, root, "public", "branch", "rogue")

	_, err := switchTo(configuration, root, "rogue")
	if err == nil || !strings.Contains(err.Error(), `SWITCH001 repository "private" README.md in rogue: path is owned by public`) {
		t.Fatalf("error = %v, want the foreign path of private", err)
	}
	for _, name := range names {
		if got := current(t, root, name); got != "main" {
			t.Fatalf("%s is on %q, want main", name, got)
		}
	}
}

func TestSwitchRefusesAnUntrackedFileTheCheckoutWouldOverwrite(t *testing.T) {
	configuration, root := feature(t)
	write(t, root, "src/app.js", "written by hand\n")

	_, err := switchTo(configuration, root, "feature")
	if err == nil || !strings.Contains(err.Error(), `SWITCH001 repository "public" cannot check out feature`) {
		t.Fatalf("error = %v, want the refused public checkout", err)
	}
	if contents, readErr := os.ReadFile(filepath.Join(root, "src", "app.js")); readErr != nil ||
		string(contents) != "written by hand\n" {
		t.Fatalf("src/app.js = %q, %v, want it untouched", contents, readErr)
	}
	for _, name := range names {
		if got := current(t, root, name); got != "main" {
			t.Fatalf("%s is on %q, want main", name, got)
		}
	}
}

func TestSwitchRollsBackAPartialFailure(t *testing.T) {
	configuration, root := feature(t)
	// A read-only src/ passes every preflight, which only reads, and makes
	// just the checkout of public fail, after private already switched.
	directory := filepath.Join(root, "src")
	if err := os.Chmod(directory, 0o500); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(directory, 0o755) }()

	_, err := switchTo(configuration, root, "feature")
	if err == nil || !strings.Contains(err.Error(), `SWITCH001 repository "public" could not be switched to feature`) {
		t.Fatalf("error = %v, want the failed public checkout", err)
	}
	if !strings.Contains(err.Error(), "No branch, HEAD, index or working-tree file was changed.") {
		t.Fatalf("error does not state the rollback: %v", err)
	}
	for _, name := range names {
		if got := current(t, root, name); got != "main" {
			t.Fatalf("%s is on %q, want it rolled back to main", name, got)
		}
	}
	if exists(t, root, "secrets/extra.txt") {
		t.Fatal("secrets/extra.txt survived the rollback")
	}
	if _, err := os.Stat(lock.RecoveryPath(root)); !os.IsNotExist(err) {
		t.Fatalf("recovery state remained: %v", err)
	}
}

// interrupted is a project whose switch to feature reached private only.
func interrupted(t *testing.T) (*config.Config, string) {
	t.Helper()
	configuration, root := feature(t)
	recordInterrupted(t, root)
	return configuration, root
}

func recordInterrupted(t *testing.T, root string) {
	t.Helper()
	directory := lock.RecoveryPath(root)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	var entries []string
	for _, name := range names {
		if err := repository.SaveIndex(root, name, filepath.Join(directory, name+".original")); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, fmt.Sprintf(`{"name":%q,"from":"main","head":%q,"target":%q,"switched":%t}`,
			name, reference(t, root, name, "refs/heads/main"),
			reference(t, root, name, "refs/heads/feature"), name == "private"))
	}
	run(t, root, "private", "checkout", "--quiet", "feature")
	contents := fmt.Sprintf(`{"version":1,"command":"gitone switch","branch":"feature","repositories":[%s]}`,
		strings.Join(entries, ","))
	if err := os.WriteFile(filepath.Join(directory, lock.StateFile), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverFinishesAnInterruptedSwitch(t *testing.T) {
	configuration, root := interrupted(t)

	var output bytes.Buffer
	if err := stage.Recover(configuration, root, &output); err != nil {
		t.Fatalf("recover: %v\noutput: %s", err, output.String())
	}
	for _, name := range names {
		if got := current(t, root, name); got != "feature" {
			t.Fatalf("%s is on %q, want feature", name, got)
		}
	}
	if !exists(t, root, "src/app.js") {
		t.Fatal("src/app.js is missing after the recovery")
	}
	if !strings.Contains(output.String(), "gitone switch recovered.") {
		t.Fatalf("output does not report the recovery:\n%s", output.String())
	}
	if _, err := os.Stat(lock.RecoveryPath(root)); !os.IsNotExist(err) {
		t.Fatalf("recovery state remained: %v", err)
	}
}

func TestAbortReturnsTheSwitchedRepositoriesToTheirStartingBranch(t *testing.T) {
	configuration, root := interrupted(t)

	var output bytes.Buffer
	if err := stage.Abort(configuration, root, &output); err != nil {
		t.Fatalf("abort: %v\noutput: %s", err, output.String())
	}
	for _, name := range names {
		if got := current(t, root, name); got != "main" {
			t.Fatalf("%s is on %q, want main", name, got)
		}
	}
	if exists(t, root, "secrets/extra.txt") {
		t.Fatal("secrets/extra.txt survived the abort")
	}
	if !strings.Contains(output.String(), "gitone switch aborted.") {
		t.Fatalf("output does not report the abort:\n%s", output.String())
	}
	if _, err := os.Stat(lock.RecoveryPath(root)); !os.IsNotExist(err) {
		t.Fatalf("recovery state remained: %v", err)
	}
}

func TestAbortRefusesAnExternallyChangedRepository(t *testing.T) {
	configuration, root := interrupted(t)
	run(t, root, "private", "commit", "--allow-empty", "-m", "external")
	moved := reference(t, root, "private", "refs/heads/feature")

	var output bytes.Buffer
	err := stage.Abort(configuration, root, &output)
	if err == nil || !strings.HasPrefix(err.Error(), "REC001") {
		t.Fatalf("error = %v, want REC001", err)
	}
	if got := reference(t, root, "private", "refs/heads/feature"); got != moved {
		t.Fatalf("private feature = %q, want the external %q", got, moved)
	}
	if _, err := os.Stat(lock.RecoveryPath(root)); err != nil {
		t.Fatalf("recovery state was removed: %v", err)
	}
}

func TestAbortRefusesAnUntrackedFileTheStartingBranchWouldOverwrite(t *testing.T) {
	configuration, root := feature(t)
	run(t, root, "private", "checkout", "--quiet", "feature")
	run(t, root, "private", "rm", "--quiet", "secrets/key.txt")
	run(t, root, "private", "commit", "-m", "remove key on feature")
	run(t, root, "private", "checkout", "--quiet", "main")
	recordInterrupted(t, root)
	write(t, root, "secrets/key.txt", "external\n")

	var output bytes.Buffer
	err := stage.Abort(configuration, root, &output)
	if err == nil || !strings.HasPrefix(err.Error(), "REC001") {
		t.Fatalf("error = %v, want REC001", err)
	}
	contents, readErr := os.ReadFile(filepath.Join(root, "secrets", "key.txt"))
	if readErr != nil || string(contents) != "external\n" {
		t.Fatalf("secrets/key.txt = %q, %v, want the external file untouched", contents, readErr)
	}
	if got := current(t, root, "private"); got != "feature" {
		t.Fatalf("private is on %q, want feature", got)
	}
	if _, err := os.Stat(lock.RecoveryPath(root)); err != nil {
		t.Fatalf("recovery state was removed: %v", err)
	}
}

func TestSwitchMaterializesASafeAliasFromTheTargetTree(t *testing.T) {
	configuration, root := feature(t)
	// src/alias -> site.css stays inside the project and has the link's owner,
	// so the branch carrying it can be checked out as a link.
	run(t, root, "public", "checkout", "--quiet", "-b", "aliased")
	if err := os.Symlink("site.css", filepath.Join(root, "src", "alias")); err != nil {
		t.Fatal(err)
	}
	run(t, root, "public", "add", "--", "src/alias")
	run(t, root, "public", "commit", "-m", "aliased public")
	run(t, root, "public", "checkout", "--quiet", "main")
	run(t, root, "private", "branch", "aliased")

	if _, err := switchTo(configuration, root, "aliased"); err != nil {
		t.Fatalf("switch: %v", err)
	}
	target, err := os.Readlink(filepath.Join(root, "src", "alias"))
	if err != nil || target != "site.css" {
		t.Fatalf("src/alias = %q, %v, want the link target site.css", target, err)
	}
}

func TestSwitchRefusesASymbolicLinkInTheTargetTree(t *testing.T) {
	configuration, root := feature(t)
	// src/link is owned by public, so only the entry mode can refuse it.
	run(t, root, "public", "checkout", "--quiet", "-b", "linked")
	if err := os.Symlink("/etc/passwd", filepath.Join(root, "src", "link")); err != nil {
		t.Fatal(err)
	}
	run(t, root, "public", "add", "--", "src/link")
	run(t, root, "public", "commit", "-m", "linked public")
	run(t, root, "public", "checkout", "--quiet", "main")
	run(t, root, "private", "branch", "linked")

	_, err := switchTo(configuration, root, "linked")
	if err == nil || !strings.Contains(err.Error(),
		"src/link in linked: symbolic link to /etc/passwd: absolute link targets are not supported") {
		t.Fatalf("error = %v, want the refused symbolic link", err)
	}
	for _, name := range names {
		if got := current(t, root, name); got != "main" {
			t.Fatalf("%s is on %q, want main", name, got)
		}
	}
	if _, err := os.Lstat(filepath.Join(root, "src", "link")); err == nil {
		t.Fatal("src/link was left in the working tree")
	}
}
