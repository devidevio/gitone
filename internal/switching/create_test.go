package switching_test

import (
	"bytes"
	"encoding/json"
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

// switchReflogPrefix is the reflog message gitone switch writes in front of
// its operation marker. It is spelled out here on purpose: an interruption
// recorded with a different prefix must be refused instead of undone, so this
// test file has to carry the exact text the package writes.
const switchReflogPrefix = "gitone switch: Created "

// remoteProject is a project whose named repositories have a local bare
// origin. Every other repository stays without a remote, so one project can
// show both refusals for a branch it cannot take from anywhere.
func remoteProject(t *testing.T, remoted ...string) (*config.Config, string, map[string]string) {
	t.Helper()
	origins, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	contents, remotes := twoRepositories, map[string]string{}
	for _, name := range remoted {
		directory := filepath.Join(origins, name+".git")
		if _, err := git.Run(origins, "init", "--bare", "--quiet", "--initial-branch=main", directory); err != nil {
			t.Fatal(err)
		}
		remotes[name] = directory
		contents = strings.Replace(contents, "  "+name+":\n", "  "+name+":\n    remote: "+directory+"\n", 1)
	}
	configuration, root := configured(t, contents)
	return configuration, root, remotes
}

// published gives one repository a branch on its origin and no local branch of
// its own, which is the state a fetch leaves behind. Pushing updates the
// remote-tracking ref, so deleting the local branch afterwards leaves exactly
// origin/<branch>.
func published(t *testing.T, root, name, branchName, path, contents string) {
	t.Helper()
	run(t, root, name, "checkout", "--quiet", "-b", branchName)
	write(t, root, path, contents)
	run(t, root, name, "add", "--", path)
	run(t, root, name, "commit", "-m", branchName+" in "+name)
	run(t, root, name, "push", "--quiet", "origin", branchName)
	run(t, root, name, "checkout", "--quiet", "main")
	run(t, root, name, "branch", "--quiet", "-D", branchName)
}

// tracks reports the upstream configured for one local branch.
func tracks(t *testing.T, root, name, branchName string) string {
	t.Helper()
	tracked, err := repository.TracksOriginBranch(root, name, branchName)
	if err != nil {
		t.Fatal(err)
	}
	if !tracked {
		return ""
	}
	return config.OriginRemote + "/" + branchName
}

func TestSwitchCreatesTheBranchInEveryRepositoryAtItsOwnHead(t *testing.T) {
	configuration, root := project(t)
	// The repositories hold different commits, so a shared start point would
	// have to pick one of them and would be visible here.
	run(t, root, "public", "commit", "--allow-empty", "-m", "one more public commit")
	starting := map[string]string{}
	for _, name := range names {
		starting[name] = reference(t, root, name, "refs/heads/main")
	}

	output, err := createTo(configuration, root, "release")
	if err != nil {
		t.Fatalf("switch -c: %v\noutput: %s", err, output)
	}
	for _, name := range names {
		if got := current(t, root, name); got != "release" {
			t.Fatalf("%s is on %q, want release", name, got)
		}
		if got := reference(t, root, name, "refs/heads/release"); got != starting[name] {
			t.Fatalf("%s release = %q, want its own previous HEAD %q", name, got, starting[name])
		}
		if got := reference(t, root, name, "refs/heads/main"); got != starting[name] {
			t.Fatalf("%s main = %q, want it left at %q", name, got, starting[name])
		}
	}
	if !strings.Contains(output, "main → release (created at") {
		t.Fatalf("output does not report the created branch:\n%s", output)
	}
	if _, err := os.Stat(lock.RecoveryPath(root)); !os.IsNotExist(err) {
		t.Fatalf("recovery state remained: %v", err)
	}
}

func TestSwitchCreateRefusesANameOneRepositoryAlreadyHas(t *testing.T) {
	configuration, root := feature(t)

	output, err := createTo(configuration, root, "feature")
	if err == nil {
		t.Fatalf("switch -c was accepted for an existing branch: %s", output)
	}
	for _, name := range names {
		if !strings.Contains(err.Error(), fmt.Sprintf("SWITCH001 repository %q already has branch feature", name)) {
			t.Fatalf("error does not name %s: %v", name, err)
		}
		if got := current(t, root, name); got != "main" {
			t.Fatalf("%s is on %q, want main", name, got)
		}
	}
	if !strings.Contains(err.Error(), "No branch, HEAD, index or working-tree file was changed.") {
		t.Fatalf("error does not state that nothing changed: %v", err)
	}
}

func TestSwitchCreateRefusesARepositoryWithoutACommit(t *testing.T) {
	configuration, root := configured(t, twoRepositories)
	// public keeps its commit, private stays unborn.
	run(t, root, "private", "update-ref", "-d", "refs/heads/main")

	_, err := createTo(configuration, root, "release")
	if err == nil || !strings.Contains(err.Error(),
		`SWITCH001 repository "private" has no commit on main yet: commit before creating release`) {
		t.Fatalf("error = %v, want the unborn private repository", err)
	}
	if got := reference(t, root, "public", "refs/heads/release"); got != "" {
		t.Fatalf("public release = %q, want no ref at all", got)
	}
}

func TestSwitchCreateRefusesAnInvalidBranchName(t *testing.T) {
	configuration, root := project(t)

	_, err := createTo(configuration, root, "feature..broken")
	if err == nil || !strings.Contains(err.Error(), `SWITCH001 "feature..broken" is not a valid branch name`) {
		t.Fatalf("error = %v, want the rejected branch name", err)
	}
}

func TestSwitchOpensAFetchedRemoteBranchAndTracksIt(t *testing.T) {
	configuration, root, _ := remoteProject(t, names...)
	published(t, root, "public", "feature", "src/app.js", "app\n")
	published(t, root, "private", "feature", "secrets/extra.txt", "extra\n")

	output, err := switchTo(configuration, root, "feature")
	if err != nil {
		t.Fatalf("switch: %v\noutput: %s", err, output)
	}
	for _, name := range names {
		if got := current(t, root, name); got != "feature" {
			t.Fatalf("%s is on %q, want feature", name, got)
		}
		remote := reference(t, root, name, "refs/remotes/origin/feature")
		if got := reference(t, root, name, "refs/heads/feature"); got != remote {
			t.Fatalf("%s feature = %q, want origin/feature %q", name, got, remote)
		}
		if got := tracks(t, root, name, "feature"); got != "origin/feature" {
			t.Fatalf("%s feature tracks %q, want origin/feature", name, got)
		}
	}
	for _, path := range []string{"src/app.js", "secrets/extra.txt"} {
		if !exists(t, root, path) {
			t.Fatalf("%s is missing from the working tree", path)
		}
	}
	if !strings.Contains(output, "created from origin/feature, tracking it") {
		t.Fatalf("output does not report the created tracking branch:\n%s", output)
	}
}

func TestSwitchKeepsTheExistingLocalBranchWhereOnlyOneRepositoryLacksIt(t *testing.T) {
	configuration, root, _ := remoteProject(t, "public")
	branch(t, root, "private", "feature", "secrets/extra.txt", "extra\n")
	published(t, root, "public", "feature", "src/app.js", "app\n")
	// private already has the branch locally and no remote at all, so only
	// public may get a new ref out of this switch.
	existing := reference(t, root, "private", "refs/heads/feature")

	output, err := switchTo(configuration, root, "feature")
	if err != nil {
		t.Fatalf("switch: %v\noutput: %s", err, output)
	}
	if got := reference(t, root, "private", "refs/heads/feature"); got != existing {
		t.Fatalf("private feature = %q, want the pre-existing %q", got, existing)
	}
	if got := tracks(t, root, "private", "feature"); got != "" {
		t.Fatalf("private feature tracks %q, want the pre-existing branch left unconfigured", got)
	}
	if got := tracks(t, root, "public", "feature"); got != "origin/feature" {
		t.Fatalf("public feature tracks %q, want origin/feature", got)
	}
	for _, name := range names {
		if got := current(t, root, name); got != "feature" {
			t.Fatalf("%s is on %q, want feature", name, got)
		}
	}
}

func TestSwitchRefusesAMissingBranchAndNamesTheNextStep(t *testing.T) {
	configuration, root, _ := remoteProject(t, "public")

	_, err := switchTo(configuration, root, "feature")
	if err == nil {
		t.Fatal("switch was accepted without any source for feature")
	}
	// public can still fetch the branch, private has nothing to fetch from.
	if !strings.Contains(err.Error(),
		`SWITCH001 repository "public" has neither branch feature nor origin/feature: run gitone fetch public`) {
		t.Fatalf("error does not name the fetch for public: %v", err)
	}
	if !strings.Contains(err.Error(),
		`SWITCH001 repository "private" has no branch feature and no origin to take it from`) {
		t.Fatalf("error does not name the missing origin of private: %v", err)
	}
	if !strings.Contains(err.Error(), "usage#repairing-missing-branches") {
		t.Fatalf("error does not link the repair guide: %v", err)
	}
	for _, name := range names {
		if got := current(t, root, name); got != "main" {
			t.Fatalf("%s is on %q, want main", name, got)
		}
	}
}

func TestSwitchDoesNotFetchTheMissingBranchItself(t *testing.T) {
	configuration, root, _ := remoteProject(t, names...)
	published(t, root, "public", "feature", "src/app.js", "app\n")
	published(t, root, "private", "feature", "secrets/extra.txt", "extra\n")
	// Both origins carry feature, but private never saw it locally.
	run(t, root, "private", "update-ref", "-d", "refs/remotes/origin/feature")

	_, err := switchTo(configuration, root, "feature")
	if err == nil || !strings.Contains(err.Error(),
		`SWITCH001 repository "private" has neither branch feature nor origin/feature: run gitone fetch private`) {
		t.Fatalf("error = %v, want the unfetched private branch", err)
	}
	if got := reference(t, root, "public", "refs/heads/feature"); got != "" {
		t.Fatalf("public feature = %q, want no ref written by a refused switch", got)
	}
}

func TestSwitchCreateRefusesLocalChangesBeforeWritingARef(t *testing.T) {
	configuration, root := project(t)
	write(t, root, "README.md", "changed\n")

	_, err := createTo(configuration, root, "release")
	if err == nil || !strings.Contains(err.Error(), "unstaged changes on main") {
		t.Fatalf("error = %v, want the refused local changes", err)
	}
	for _, name := range names {
		if got := reference(t, root, name, "refs/heads/release"); got != "" {
			t.Fatalf("%s release = %q, want no ref at all", name, got)
		}
	}
}

func TestSwitchCreateRollsBackAPartialFailure(t *testing.T) {
	configuration, root := project(t)
	// A native writer holding the second ref lock makes creation fail after
	// the first repository has switched, independently of name preflight.
	write(t, root, filepath.Join(repository.Path("public"), "refs/heads/release.lock"), "")

	_, err := createTo(configuration, root, "release")
	if err == nil {
		t.Fatal("switch -c was accepted although public cannot hold the new branch")
	}
	if !strings.Contains(err.Error(), `SWITCH001 repository "public" could not create branch release`) {
		t.Fatalf("error = %v, want the failed public creation", err)
	}
	for _, name := range names {
		if got := reference(t, root, name, "refs/heads/release"); got != "" {
			t.Fatalf("%s release = %q, want the created ref removed again", name, got)
		}
	}
	if got := current(t, root, "private"); got != "main" {
		t.Fatalf("private is on %q, want it rolled back to main", got)
	}
	if _, err := os.Stat(lock.RecoveryPath(root)); !os.IsNotExist(err) {
		t.Fatalf("recovery state remained: %v", err)
	}
}

// interruptedCreation is a project whose "gitone switch feature" created the
// tracking branch of both repositories and checked out private only. The
// recorded state is written by hand, exactly as an interruption between the
// two steps would leave it.
func interruptedCreation(t *testing.T, settings ...string) (*config.Config, string, string) {
	t.Helper()
	configuration, root, _ := remoteProject(t, names...)
	published(t, root, "public", "feature", "src/app.js", "app\n")
	published(t, root, "private", "feature", "secrets/extra.txt", "extra\n")

	directory := lock.RecoveryPath(root)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := "TESTMARKER"
	var entries []string
	for _, name := range names {
		if err := repository.SaveIndex(root, name, filepath.Join(directory, name+".original")); err != nil {
			t.Fatal(err)
		}
		for index := 0; index < len(settings); index += 2 {
			run(t, root, name, "config", settings[index], settings[index+1])
		}
		configPath := filepath.Join(repository.Directory(root, name), "config")
		original, err := os.ReadFile(configPath)
		if err != nil {
			t.Fatal(err)
		}
		target := reference(t, root, name, "refs/remotes/origin/feature")
		run(t, root, name, "update-ref", "--create-reflog", "-m", switchReflogPrefix+marker,
			"refs/heads/feature", target, "")
		run(t, root, name, "branch", "--quiet", "--set-upstream-to=origin/feature", "feature")
		tracked, err := os.ReadFile(configPath)
		if err != nil {
			t.Fatal(err)
		}
		upstream, err := json.Marshal(map[string][]byte{"original": original, "target": tracked})
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, fmt.Sprintf(
			`{"name":%q,"from":"main","head":%q,"target":%q,"create":true,"track":true,"switched":%t,"upstream":%s}`,
			name, reference(t, root, name, "refs/heads/main"), target, name == "private", upstream))
	}
	run(t, root, "private", "checkout", "--quiet", "feature")
	contents := fmt.Sprintf(
		`{"version":4,"command":"gitone switch","branch":"feature","marker":%q,"repositories":[%s]}`,
		marker, strings.Join(entries, ","))
	if err := os.WriteFile(filepath.Join(directory, lock.StateFile), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return configuration, root, marker
}

func TestRecoverFinishesAnInterruptedCreatingSwitch(t *testing.T) {
	configuration, root, _ := interruptedCreation(t)

	var output bytes.Buffer
	if err := stage.Recover(configuration, root, &output); err != nil {
		t.Fatalf("recover: %v\noutput: %s", err, output.String())
	}
	for _, name := range names {
		if got := current(t, root, name); got != "feature" {
			t.Fatalf("%s is on %q, want feature", name, got)
		}
		if got := tracks(t, root, name, "feature"); got != "origin/feature" {
			t.Fatalf("%s feature tracks %q, want origin/feature", name, got)
		}
	}
	if !exists(t, root, "src/app.js") {
		t.Fatal("src/app.js is missing after the recovery")
	}
	if _, err := os.Stat(lock.RecoveryPath(root)); !os.IsNotExist(err) {
		t.Fatalf("recovery state remained: %v", err)
	}
}

func TestAbortRemovesTheBranchesAndUpstreamsAnInterruptedSwitchCreated(t *testing.T) {
	configuration, root, _ := interruptedCreation(t)

	var output bytes.Buffer
	if err := stage.Abort(configuration, root, &output); err != nil {
		t.Fatalf("abort: %v\noutput: %s", err, output.String())
	}
	for _, name := range names {
		if got := current(t, root, name); got != "main" {
			t.Fatalf("%s is on %q, want main", name, got)
		}
		if got := reference(t, root, name, "refs/heads/feature"); got != "" {
			t.Fatalf("%s feature = %q, want the created branch removed", name, got)
		}
		if got := tracks(t, root, name, "feature"); got != "" {
			t.Fatalf("%s feature still tracks %q after the abort", name, got)
		}
		if got := reference(t, root, name, "refs/remotes/origin/feature"); got == "" {
			t.Fatalf("%s lost origin/feature, which the switch never wrote", name)
		}
	}
	if exists(t, root, "secrets/extra.txt") {
		t.Fatal("secrets/extra.txt survived the abort")
	}
	if !strings.Contains(output.String(), "removed feature") {
		t.Fatalf("output does not report the removed branches:\n%s", output.String())
	}
	if _, err := os.Stat(lock.RecoveryPath(root)); !os.IsNotExist(err) {
		t.Fatalf("recovery state remained: %v", err)
	}
}

func TestAbortKeepsACreatedBranchThatChangedOutsideGitOne(t *testing.T) {
	configuration, root, _ := interruptedCreation(t)
	// public was created but never checked out; someone moved it afterwards.
	run(t, root, "public", "update-ref", "refs/heads/feature", reference(t, root, "public", "refs/heads/main"))
	moved := reference(t, root, "public", "refs/heads/feature")

	var output bytes.Buffer
	err := stage.Abort(configuration, root, &output)
	if err == nil || !strings.HasPrefix(err.Error(), "REC001") {
		t.Fatalf("error = %v, want REC001", err)
	}
	if got := reference(t, root, "public", "refs/heads/feature"); got != moved {
		t.Fatalf("public feature = %q, want the external %q", got, moved)
	}
	if _, err := os.Stat(lock.RecoveryPath(root)); err != nil {
		t.Fatalf("recovery state was removed: %v", err)
	}
}

func TestAbortKeepsABranchItDidNotCreateItself(t *testing.T) {
	configuration, root, _ := interruptedCreation(t)
	// The ref still holds the recorded commit, but its reflog shows it was
	// written by something other than the interrupted switch.
	target := reference(t, root, "public", "refs/heads/feature")
	run(t, root, "public", "update-ref", "-d", "refs/heads/feature", target)
	run(t, root, "public", "update-ref", "--create-reflog", "-m", "written by hand",
		"refs/heads/feature", target, "")

	var output bytes.Buffer
	err := stage.Abort(configuration, root, &output)
	if err == nil || !strings.HasPrefix(err.Error(), "REC001") {
		t.Fatalf("error = %v, want REC001", err)
	}
	if got := reference(t, root, "public", "refs/heads/feature"); got != target {
		t.Fatalf("public feature = %q, want the foreign ref kept at %q", got, target)
	}
}

func TestSwitchRefusesAnUnbornRepositoryBeforePlanningACheckout(t *testing.T) {
	configuration, root, _ := remoteProject(t, names...)
	published(t, root, "private", "feature", "secrets/extra.txt", "extra\n")
	branch(t, root, "public", "feature", "src/app.js", "app\n")
	// private keeps origin/feature but loses the only commit it ever had, so
	// nothing could be checked out over its HEAD and nothing restored after.
	run(t, root, "private", "update-ref", "-d", "refs/heads/main")

	_, err := switchTo(configuration, root, "feature")
	if err == nil || !strings.Contains(err.Error(),
		`SWITCH001 repository "private" has no commit on main yet.`) {
		t.Fatalf("error = %v, want the unborn private repository", err)
	}
	if got := reference(t, root, "private", "refs/heads/feature"); got != "" {
		t.Fatalf("private feature = %q, want no ref written by a refused switch", got)
	}
	if got := current(t, root, "public"); got != "main" {
		t.Fatalf("public is on %q, want main", got)
	}
}

func TestSwitchRefusesBranchPrefixConflictsBeforeMutation(t *testing.T) {
	for _, creating := range []bool{true, false} {
		for _, existing := range []string{"release", "release/child/nested"} {
			t.Run(fmt.Sprintf("create=%t/existing=%s", creating, existing), func(t *testing.T) {
				configuration, root, _ := remoteProject(t, names...)
				if !creating {
					published(t, root, "public", "release/child", "src/app.js", "app\n")
					published(t, root, "private", "release/child", "secrets/extra.txt", "extra\n")
				}
				run(t, root, "public", "update-ref", "refs/heads/"+existing,
					reference(t, root, "public", "refs/heads/main"), "")
				before := run(t, root, "private", "reflog", "show", "HEAD")
				_, err := invoke(configuration, root, "release/child", creating, false, false, "")
				if err == nil || !strings.Contains(err.Error(), "conflicts with existing branch "+existing) {
					t.Fatalf("error = %v, want the prefix conflict", err)
				}
				if after := run(t, root, "private", "reflog", "show", "HEAD"); after != before {
					t.Fatal("first repository moved before the conflict was refused")
				}
				for _, name := range names {
					if current(t, root, name) != "main" || reference(t, root, name, "refs/heads/release/child") != "" {
						t.Fatalf("%s changed during preflight", name)
					}
				}
			})
		}
	}
}

func TestResumeKeepsExternalUpstreamChanges(t *testing.T) {
	for _, recovering := range []bool{false, true} {
		t.Run(fmt.Sprintf("recover=%t", recovering), func(t *testing.T) {
			configuration, root, _ := interruptedCreation(t)
			run(t, root, "public", "config", "branch.feature.remote", ".")
			run(t, root, "public", "config", "branch.feature.merge", "refs/heads/main")
			var output bytes.Buffer
			resume := stage.Abort
			if recovering {
				resume = stage.Recover
			}
			err := resume(configuration, root, &output)
			if err == nil || !strings.Contains(err.Error(), "REC001") {
				t.Fatalf("error = %v, want refusal of external configuration", err)
			}
			if got := strings.TrimSpace(run(t, root, "public", "config", "--get", "branch.feature.remote")); got != "." {
				t.Fatalf("external remote overwritten: %q", got)
			}
			if got := strings.TrimSpace(run(t, root, "public", "config", "--get", "branch.feature.merge")); got != "refs/heads/main" {
				t.Fatalf("external merge overwritten: %q", got)
			}
			if _, err := os.Stat(lock.RecoveryPath(root)); err != nil {
				t.Fatalf("recovery state lost: %v", err)
			}
		})
	}
}

func TestAbortRestoresUpstreamSettingsThatPredatedTheBranch(t *testing.T) {
	configuration, root, _ := interruptedCreation(t,
		"branch.feature.remote", ".", "branch.feature.merge", "refs/heads/main")
	var output bytes.Buffer
	if err := stage.Abort(configuration, root, &output); err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if got := strings.TrimSpace(run(t, root, name, "config", "--get", "branch.feature.remote")); got != "." {
			t.Fatalf("%s original remote lost: %q", name, got)
		}
		if got := strings.TrimSpace(run(t, root, name, "config", "--get", "branch.feature.merge")); got != "refs/heads/main" {
			t.Fatalf("%s original merge lost: %q", name, got)
		}
	}
}

func TestResumePreservesExternalWorkWithoutCreatedRef(t *testing.T) {
	for _, recovering := range []bool{false, true} {
		t.Run(fmt.Sprintf("recover=%t", recovering), func(t *testing.T) {
			configuration, root, _ := interruptedCreation(t)
			run(t, root, "public", "update-ref", "-d", "refs/heads/feature")
			write(t, root, "README.md", "staged work\n")
			run(t, root, "public", "add", "--", "README.md")
			write(t, root, "README.md", "later unstaged work\n")
			var output bytes.Buffer
			resume := stage.Abort
			if recovering {
				resume = stage.Recover
			}
			err := resume(configuration, root, &output)
			if err == nil || !strings.Contains(err.Error(), "REC001") {
				t.Fatalf("error = %v, want refusal of external work", err)
			}
			if got := run(t, root, "public", "show", ":README.md"); got != "staged work\n" {
				t.Fatalf("external index overwritten: %q", got)
			}
			if got := contents(t, root, "README.md"); got != "later unstaged work\n" {
				t.Fatalf("external worktree overwritten: %q", got)
			}
			if got := reference(t, root, "public", "refs/heads/feature"); got != "" {
				t.Fatalf("branch created despite external changes: %s", got)
			}
			if _, err := os.Stat(lock.RecoveryPath(root)); err != nil {
				t.Fatalf("recovery state lost: %v", err)
			}
		})
	}
}

func TestTrackingFailureRestoresPreexistingConfiguration(t *testing.T) {
	configuration, root, _ := remoteProject(t, names...)
	published(t, root, "public", "feature", "src/app.js", "app\n")
	published(t, root, "private", "feature", "secrets/extra.txt", "extra\n")
	original := map[string]string{}
	for _, name := range names {
		run(t, root, name, "config", "branch.feature.remote", ".")
		run(t, root, name, "config", "branch.feature.merge", "refs/heads/main")
		original[name] = contents(t, root, filepath.Join(repository.Path(name), "config"))
	}
	write(t, root, filepath.Join(repository.Path("public"), "refs/heads/feature.lock"), "")
	_, err := switchTo(configuration, root, "feature")
	if err == nil || !strings.Contains(err.Error(), `repository "public" could not create branch feature`) {
		t.Fatalf("error = %v, want the second ref creation to fail", err)
	}
	for _, name := range names {
		if got := contents(t, root, filepath.Join(repository.Path(name), "config")); got != original[name] {
			t.Fatalf("%s preexisting configuration was not restored", name)
		}
		if current(t, root, name) != "main" || reference(t, root, name, "refs/heads/feature") != "" {
			t.Fatalf("%s did not roll back", name)
		}
	}
	if _, err := os.Stat(lock.RecoveryPath(root)); !os.IsNotExist(err) {
		t.Fatalf("recovery state remained: %v", err)
	}
}

func TestResumeBeforeRefCreation(t *testing.T) {
	for _, recovering := range []bool{false, true} {
		t.Run(fmt.Sprintf("recover=%t", recovering), func(t *testing.T) {
			configuration, root, _ := interruptedCreation(t)
			run(t, root, "public", "update-ref", "-d", "refs/heads/feature")
			var output bytes.Buffer
			resume := stage.Abort
			want := "main"
			if recovering {
				resume, want = stage.Recover, "feature"
			}
			if err := resume(configuration, root, &output); err != nil {
				t.Fatal(err)
			}
			for _, name := range names {
				if got := current(t, root, name); got != want {
					t.Fatalf("%s on %s, want %s", name, got, want)
				}
			}
		})
	}
}

func TestResumeRefusesTrackingStateWithoutConfigurationSnapshots(t *testing.T) {
	configuration, root, _ := interruptedCreation(t)
	path := filepath.Join(lock.RecoveryPath(root), lock.StateFile)
	var saved map[string]any
	if err := json.Unmarshal([]byte(contents(t, root, filepath.Join(".gitone", "recovery", lock.StateFile))), &saved); err != nil {
		t.Fatal(err)
	}
	saved["version"] = 3
	for _, entry := range saved["repositories"].([]any) {
		delete(entry.(map[string]any), "upstream")
	}
	data, err := json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := stage.Abort(configuration, root, &output); err == nil || !strings.Contains(err.Error(), "lacks upstream configuration snapshots") {
		t.Fatalf("error = %v, want refusal without original configuration", err)
	}
	if got := current(t, root, "private"); got != "feature" {
		t.Fatalf("private moved to %s before refusing unsafe state", got)
	}
}

func TestPartialBranchWithoutRemoteSourceLinksRepairInsteadOfCreate(t *testing.T) {
	configuration, root, _ := remoteProject(t, names...)
	run(t, root, "public", "branch", "feature", "HEAD")

	_, err := switchTo(configuration, root, "feature")
	if err == nil {
		t.Fatal("switch was accepted without a private branch source")
	}
	if !strings.Contains(err.Error(), "gitone fetch private and retry") ||
		!strings.Contains(err.Error(), "usage#repairing-missing-branches") ||
		strings.Contains(err.Error(), "gitone switch -c") {
		t.Fatalf("unusable partial-branch guidance: %v", err)
	}
	for _, name := range names {
		if got := current(t, root, name); got != "main" {
			t.Fatalf("%s moved to %s after refusal", name, got)
		}
	}
	// Incoming reconfiguration uses Plan and must not suggest global creation either.
	_, refusal, err := switching.Plan(root, "private", "main", "feature", "", "")
	if err != nil || !strings.Contains(refusal, "usage#repairing-missing-branches") ||
		strings.Contains(refusal, "gitone switch -c") {
		t.Fatalf("plan guidance = %q, %v", refusal, err)
	}
}
