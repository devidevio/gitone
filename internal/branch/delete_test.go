package branch_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/devidevio/gitone/internal/config"
	"github.com/devidevio/gitone/internal/git"
	"github.com/devidevio/gitone/internal/lock"
	"github.com/devidevio/gitone/internal/repository"
	"github.com/devidevio/gitone/internal/stage"
)

// released is a project where both repositories have the branch release at
// their current HEAD, which is the state a finished feature is deleted from.
func released(t *testing.T) (*config.Config, string) {
	t.Helper()
	configuration, root := committed(t)
	run(t, root, "private", "branch", "release")
	run(t, root, "public", "branch", "release")
	return configuration, root
}

// track configures branch to track origin/<branch> and points that
// remote-tracking ref at commit. An empty commit leaves the ref missing,
// which is a configured upstream the repository has never fetched.
func track(t *testing.T, root, name, branch, commit string) {
	t.Helper()
	run(t, root, name, "config", "--local", "remote.origin.url", "https://example.test/"+name+".git")
	run(t, root, name, "config", "--local", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*")
	run(t, root, name, "config", "--local", "branch."+branch+".remote", "origin")
	run(t, root, name, "config", "--local", "branch."+branch+".merge", "refs/heads/"+branch)
	if commit != "" {
		run(t, root, name, "update-ref", "refs/remotes/origin/"+branch, commit)
	}
}

// settings are the branch.<branch>.* entries of one repository. git config
// reports "nothing matched" as exit code 1, which is the empty answer here.
func settings(t *testing.T, root, name, branch string) string {
	t.Helper()
	output, _ := git.Run(root, "--git-dir="+repository.Directory(root, name),
		"config", "--local", "--get-regexp", "^branch\\."+branch+"\\.")
	return strings.TrimSpace(output)
}

func reflogPath(root, name, branch string) string {
	return filepath.Join(repository.Directory(root, name), "logs", "refs", "heads", branch)
}

func TestDeleteRemovesAMergedBranchEverywhere(t *testing.T) {
	configuration, root := released(t)
	commits := map[string]string{
		"private": reference(t, root, "private", "refs/heads/main"),
		"public":  reference(t, root, "public", "refs/heads/main"),
	}

	output, err := remove(configuration, root, "release")
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if !strings.Contains(output, "Branch release deleted:") {
		t.Fatalf("output does not report the deletion:\n%s", output)
	}
	if !strings.Contains(output, "no remote branch was deleted") {
		t.Fatalf("output does not state that no remote was involved:\n%s", output)
	}
	for name, commit := range commits {
		if got := reference(t, root, name, "refs/heads/release"); got != "" {
			t.Fatalf("%s release = %q, want none", name, got)
		}
		if got := reference(t, root, name, "refs/heads/main"); got != commit {
			t.Fatalf("%s main = %q, want %q", name, got, commit)
		}
	}
	if contents, err := os.ReadFile(filepath.Join(root, "README.md")); err != nil || string(contents) != "one\n" {
		t.Fatalf("README.md = %q, %v", contents, err)
	}
	if _, err := os.Stat(lock.RecoveryPath(root)); !os.IsNotExist(err) {
		t.Fatalf("recovery state remained: %v", err)
	}
}

func TestDeleteRemovesTheBranchSettingsWithTheBranch(t *testing.T) {
	configuration, root := released(t)
	for _, name := range []string{"private", "public"} {
		track(t, root, name, "release", reference(t, root, name, "refs/heads/release"))
	}

	if _, err := remove(configuration, root, "release"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	for _, name := range []string{"private", "public"} {
		if got := settings(t, root, name, "release"); got != "" {
			t.Fatalf("%s kept branch settings %q", name, got)
		}
	}
}

// The configured upstream decides instead of HEAD: the branch is ahead of
// every HEAD and still deletable, because origin already holds it.
func TestDeleteComparesWithTheConfiguredUpstream(t *testing.T) {
	configuration, root := released(t)
	for _, name := range []string{"private", "public"} {
		run(t, root, name, "symbolic-ref", "HEAD", "refs/heads/release")
		run(t, root, name, "commit", "--allow-empty", "-m", "published work")
		run(t, root, name, "symbolic-ref", "HEAD", "refs/heads/main")
		track(t, root, name, "release", reference(t, root, name, "refs/heads/release"))
	}

	output, err := remove(configuration, root, "release")
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if !strings.Contains(output, "merged into origin/release") {
		t.Fatalf("output does not name the upstream comparison:\n%s", output)
	}
	for _, name := range []string{"private", "public"} {
		if got := reference(t, root, name, "refs/heads/release"); got != "" {
			t.Fatalf("%s release = %q, want none", name, got)
		}
	}
}

// A branch contained in HEAD but not in its configured upstream stays: the
// upstream replaces HEAD as the comparison, exactly like native Git.
func TestDeleteRefusesAnUnpublishedBranchWithAnUpstream(t *testing.T) {
	configuration, root := committed(t)
	for _, name := range []string{"private", "public"} {
		published := reference(t, root, name, "refs/heads/main")
		run(t, root, name, "commit", "--allow-empty", "-m", "later work")
		run(t, root, name, "branch", "release")
		track(t, root, name, "release", published)
	}

	_, err := remove(configuration, root, "release")
	if err == nil || !strings.Contains(err.Error(),
		`BRANCH001 repository "private" has not merged branch release into origin/release: merge or publish it first`) {
		t.Fatalf("error = %v, want the unmerged upstream of private", err)
	}
	if !strings.Contains(err.Error(), `repository "public" has not merged branch release into origin/release`) {
		t.Fatalf("error does not report every blocking repository: %v", err)
	}
	for _, name := range []string{"private", "public"} {
		if got := reference(t, root, name, "refs/heads/release"); got == "" {
			t.Fatalf("%s release disappeared", name)
		}
	}
}

// Without an upstream the comparison falls back to HEAD, so a branch merged
// in only part of the project is refused for the whole project.
func TestDeleteRefusesABranchMergedInOnlyPartOfTheProject(t *testing.T) {
	configuration, root := released(t)
	kept := reference(t, root, "private", "refs/heads/release")
	run(t, root, "public", "symbolic-ref", "HEAD", "refs/heads/release")
	run(t, root, "public", "commit", "--allow-empty", "-m", "unmerged work")
	run(t, root, "public", "symbolic-ref", "HEAD", "refs/heads/main")

	_, err := remove(configuration, root, "release")
	if err == nil || !strings.Contains(err.Error(),
		`BRANCH001 repository "public" has not merged branch release into HEAD`) {
		t.Fatalf("error = %v, want the unmerged public repository", err)
	}
	if !strings.Contains(err.Error(), "No branch, HEAD, index or working-tree file was changed.") {
		t.Fatalf("error does not state that nothing changed: %v", err)
	}
	// A refusal in the last repository must not have deleted the first one.
	if got := reference(t, root, "private", "refs/heads/release"); got != kept {
		t.Fatalf("private release = %q, want %q", got, kept)
	}
}

func TestDeleteRefusesAnUnfetchedUpstream(t *testing.T) {
	configuration, root := released(t)
	track(t, root, "public", "release", "")

	_, err := remove(configuration, root, "release")
	if err == nil || !strings.Contains(err.Error(),
		`BRANCH001 repository "public" cannot compare branch release with its configured upstream origin/release, which does not exist locally: run gitone fetch public`) {
		t.Fatalf("error = %v, want the unfetched upstream of public", err)
	}
	if got := reference(t, root, "private", "refs/heads/release"); got == "" {
		t.Fatal("private release disappeared")
	}
}

// A branch whose upstream configuration no remote resolves is refused too:
// native Git would silently compare with HEAD instead.
func TestDeleteRefusesAnUnresolvableUpstream(t *testing.T) {
	configuration, root := released(t)
	run(t, root, "public", "config", "--local", "branch.release.remote", "gone")
	run(t, root, "public", "config", "--local", "branch.release.merge", "refs/heads/release")

	_, err := remove(configuration, root, "release")
	if err == nil || !strings.Contains(err.Error(),
		`BRANCH001 repository "public" configures an upstream for branch release that no remote resolves: repair or remove branch.release.merge first`) {
		t.Fatalf("error = %v, want the unresolvable upstream of public", err)
	}
	if got := reference(t, root, "private", "refs/heads/release"); got == "" {
		t.Fatal("private release disappeared")
	}
}

func TestDeleteRefusesTheCurrentBranch(t *testing.T) {
	configuration, root := committed(t)

	_, err := remove(configuration, root, "main")
	if err == nil || !strings.Contains(err.Error(),
		`BRANCH001 repository "private" is on branch main: switch to another branch before deleting it`) {
		t.Fatalf("error = %v, want the current branch of private", err)
	}
	if got := reference(t, root, "private", "refs/heads/main"); got == "" {
		t.Fatal("private main disappeared")
	}
}

func TestDeleteRefusesAMissingBranch(t *testing.T) {
	configuration, root := committed(t)
	run(t, root, "public", "branch", "release")

	_, err := remove(configuration, root, "release")
	if err == nil || !strings.Contains(err.Error(), `BRANCH001 repository "private" does not have branch release`) {
		t.Fatalf("error = %v, want the missing branch of private", err)
	}
	if got := reference(t, root, "public", "refs/heads/release"); got == "" {
		t.Fatal("public release was deleted although the project refused")
	}
}

func TestDeleteRefusesInvalidNames(t *testing.T) {
	configuration, root := released(t)
	for _, name := range []string{"bad..name", "bad name", "-dash", "name.lock"} {
		output, err := remove(configuration, root, name)
		if err == nil || !strings.HasPrefix(err.Error(), "BRANCH001") {
			t.Fatalf("%q: error = %v, want BRANCH001", name, err)
		}
		if output != "" {
			t.Fatalf("%q: output = %q, want empty", name, output)
		}
	}
}

// deletedEntry mirrors one recorded repository of an interrupted deletion.
type deletedEntry struct {
	Name     string              `json:"name"`
	Commit   string              `json:"commit"`
	Reflog   []byte              `json:"reflog,omitempty"`
	Settings []map[string]string `json:"settings,omitempty"`
	Started  bool                `json:"started,omitempty"`
}

// interrupt removes branch from the named repositories the way gitone branch
// -d does - ref first, then its settings - and leaves behind the recovery
// state a process stopped right afterwards would have written.
func interrupt(t *testing.T, root, branch string, done ...string) map[string]string {
	t.Helper()
	commits := map[string]string{}
	var entries []deletedEntry
	for _, name := range []string{"private", "public"} {
		commits[name] = reference(t, root, name, "refs/heads/"+branch)
		entry := deletedEntry{Name: name, Commit: commits[name], Started: slices.Contains(done, name)}
		if contents, err := os.ReadFile(reflogPath(root, name, branch)); err == nil {
			entry.Reflog = contents
		}
		for _, line := range strings.Split(settings(t, root, name, branch), "\n") {
			if key, value, found := strings.Cut(line, " "); found {
				entry.Settings = append(entry.Settings, map[string]string{"key": key, "value": value})
			}
		}
		if entry.Started {
			run(t, root, name, "update-ref", "-d", "refs/heads/"+branch, commits[name])
			if len(entry.Settings) != 0 {
				run(t, root, name, "config", "--local", "--remove-section", "branch."+branch)
			}
		}
		entries = append(entries, entry)
	}

	directory := lock.RecoveryPath(root)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	contents, err := json.Marshal(map[string]any{
		"version": 3, "command": "gitone branch", "branch": branch, "marker": recoveryMarker, "deleted": entries,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, lock.StateFile), contents, 0o600); err != nil {
		t.Fatal(err)
	}
	return commits
}

func TestRecoverFinishesAnInterruptedDeletion(t *testing.T) {
	configuration, root := released(t)
	track(t, root, "public", "release", reference(t, root, "public", "refs/heads/release"))
	interrupt(t, root, "release", "private")

	var output bytes.Buffer
	if err := stage.Recover(configuration, root, &output); err != nil {
		t.Fatalf("recover: %v\noutput: %s", err, output.String())
	}
	for _, name := range []string{"private", "public"} {
		if got := reference(t, root, name, "refs/heads/release"); got != "" {
			t.Fatalf("%s release = %q, want none", name, got)
		}
		if got := settings(t, root, name, "release"); got != "" {
			t.Fatalf("%s kept branch settings %q", name, got)
		}
	}
	if !strings.Contains(output.String(), "gitone branch recovered.") {
		t.Fatalf("output does not report the recovery:\n%s", output.String())
	}
	if _, err := os.Stat(lock.RecoveryPath(root)); !os.IsNotExist(err) {
		t.Fatalf("recovery state remained: %v", err)
	}
}

func TestAbortRestoresRefsReflogsAndBranchSettings(t *testing.T) {
	configuration, root := released(t)
	track(t, root, "private", "release", reference(t, root, "private", "refs/heads/release"))
	track(t, root, "public", "release", reference(t, root, "public", "refs/heads/release"))
	reflogs := map[string]string{
		"private": run(t, root, "private", "reflog", "show", "--format=%gs", "refs/heads/release"),
		"public":  run(t, root, "public", "reflog", "show", "--format=%gs", "refs/heads/release"),
	}
	settingsBefore := settings(t, root, "private", "release")
	commits := interrupt(t, root, "release", "private", "public")

	var output bytes.Buffer
	if err := stage.Abort(configuration, root, &output); err != nil {
		t.Fatalf("abort: %v\noutput: %s", err, output.String())
	}
	for name, commit := range commits {
		if got := reference(t, root, name, "refs/heads/release"); got != commit {
			t.Fatalf("%s release = %q, want %q", name, got, commit)
		}
		if got := run(t, root, name, "reflog", "show", "--format=%gs", "refs/heads/release"); got != reflogs[name] {
			t.Fatalf("%s reflog = %q, want %q", name, got, reflogs[name])
		}
		if got := settings(t, root, name, "release"); got != settingsBefore {
			t.Fatalf("%s settings = %q, want %q", name, got, settingsBefore)
		}
	}
	if !strings.Contains(output.String(), "gitone branch aborted.") {
		t.Fatalf("output does not report the abort:\n%s", output.String())
	}
	if _, err := os.Stat(lock.RecoveryPath(root)); !os.IsNotExist(err) {
		t.Fatalf("recovery state remained: %v", err)
	}
}

func TestAbortRefusesABranchRecreatedAfterTheDeletion(t *testing.T) {
	configuration, root := released(t)
	commits := interrupt(t, root, "release", "private")
	run(t, root, "private", "branch", "release", "main")

	var output bytes.Buffer
	err := stage.Abort(configuration, root, &output)
	if err == nil || !strings.HasPrefix(err.Error(), "REC001") {
		t.Fatalf("error = %v, want REC001", err)
	}
	if !strings.Contains(err.Error(), "usage#resolving-an-interrupted-branch-deletion") ||
		!strings.Contains(err.Error(), "every participating repository") ||
		strings.Contains(err.Error(), "remove "+lock.RecoveryPath(root)) {
		t.Fatalf("unsafe or incomplete recovery guidance: %v", err)
	}

	if got := reference(t, root, "private", "refs/heads/release"); got != commits["private"] {
		t.Fatalf("private release = %q, want the recreated %q", got, commits["private"])
	}
	if _, err := os.Stat(lock.RecoveryPath(root)); err != nil {
		t.Fatalf("recovery state was removed: %v", err)
	}
}

func TestAbortRefusesBranchSettingsChangedAfterTheDeletion(t *testing.T) {
	configuration, root := released(t)
	interrupt(t, root, "release", "private")
	run(t, root, "private", "config", "--local", "branch.release.description", "added outside GitOne")

	var output bytes.Buffer
	err := stage.Abort(configuration, root, &output)
	if err == nil || !strings.HasPrefix(err.Error(), "REC001") {
		t.Fatalf("error = %v, want REC001", err)
	}
	if !strings.Contains(err.Error(), "changed the settings of branch release") ||
		!strings.Contains(err.Error(), "usage#resolving-an-interrupted-branch-deletion") ||
		!strings.Contains(err.Error(), "every participating repository") ||
		strings.Contains(err.Error(), "remove "+lock.RecoveryPath(root)) {
		t.Fatalf("unsafe or incomplete recovery guidance: %v", err)
	}
	if _, err := os.Stat(lock.RecoveryPath(root)); err != nil {
		t.Fatalf("recovery state was removed: %v", err)
	}
}

func TestAbortLeavesTheRepositoriesTheDeletionNeverReached(t *testing.T) {
	configuration, root := released(t)
	track(t, root, "public", "release", reference(t, root, "public", "refs/heads/release"))
	commits := interrupt(t, root, "release", "private")

	var output bytes.Buffer
	if err := stage.Abort(configuration, root, &output); err != nil {
		t.Fatalf("abort: %v\noutput: %s", err, output.String())
	}
	for name, commit := range commits {
		if got := reference(t, root, name, "refs/heads/release"); got != commit {
			t.Fatalf("%s release = %q, want %q", name, got, commit)
		}
	}
	if !strings.Contains(output.String(), "unchanged, release was not deleted") {
		t.Fatalf("output does not report the untouched repository:\n%s", output.String())
	}
	if got := settings(t, root, "public", "release"); !strings.Contains(got, "branch.release.remote origin") {
		t.Fatalf("public settings = %q, want the untouched upstream", got)
	}
}

func TestAbortRefusesAMovedBranchAfterAnInterruptedDeletion(t *testing.T) {
	configuration, root := released(t)
	interrupt(t, root, "release", "private")
	run(t, root, "public", "commit", "--allow-empty", "-m", "external")
	moved := reference(t, root, "public", "refs/heads/main")
	run(t, root, "public", "update-ref", "refs/heads/release", moved)

	var output bytes.Buffer
	err := stage.Abort(configuration, root, &output)
	if err == nil || !strings.HasPrefix(err.Error(), "REC001") {
		t.Fatalf("error = %v, want REC001", err)
	}
	if got := reference(t, root, "public", "refs/heads/release"); got != moved {
		t.Fatalf("public release = %q, want the external %q", got, moved)
	}
}

// editDeletionState places the process at an exact persisted crash boundary.
func editDeletionState(t *testing.T, root string, edit func(map[string]any)) {
	t.Helper()
	path := filepath.Join(lock.RecoveryPath(root), lock.StateFile)
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]any
	if err := json.Unmarshal(contents, &saved); err != nil {
		t.Fatal(err)
	}
	edit(saved)
	contents, err = json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverProtectsRecreatedBranchBeforeDeletionProgressWasSaved(t *testing.T) {
	for _, withReflog := range []bool{false, true} {
		t.Run(fmt.Sprint(withReflog), func(t *testing.T) {
			configuration, root := released(t)
			commits := interrupt(t, root, "release", "private")
			editDeletionState(t, root, func(saved map[string]any) {
				entry := saved["deleted"].([]any)[0].(map[string]any)
				entry["started"] = true
			})
			run(t, root, "private", "config", "core.logAllRefUpdates", fmt.Sprint(withReflog))
			run(t, root, "private", "branch", "release", "main")
			before, _ := os.ReadFile(reflogPath(root, "private", "release"))
			err := stage.Recover(configuration, root, io.Discard)
			if err == nil || !strings.HasPrefix(err.Error(), "REC001") {
				t.Fatalf("recover = %v, want REC001", err)
			}
			if got := reference(t, root, "private", "refs/heads/release"); got != commits["private"] {
				t.Fatalf("recreated ref = %q", got)
			}
			after, _ := os.ReadFile(reflogPath(root, "private", "release"))
			if !bytes.Equal(before, after) {
				t.Fatal("recreated reflog changed")
			}
			if got := reference(t, root, "public", "refs/heads/release"); got != commits["public"] {
				t.Fatal("other repository changed despite refusal")
			}
		})
	}
}

func TestRecoverPreservesExternallyChangedSettingsBeforeAnyMutation(t *testing.T) {
	for _, deleted := range []bool{false, true} {
		t.Run(fmt.Sprint(deleted), func(t *testing.T) {
			configuration, root := released(t)
			track(t, root, "public", "release", reference(t, root, "public", "refs/heads/release"))
			var done []string
			if deleted {
				done = []string{"public"}
			}
			commits := interrupt(t, root, "release", done...)
			run(t, root, "public", "config", "branch.release.description", "external description")
			before := settings(t, root, "public", "release")
			err := stage.Recover(configuration, root, io.Discard)
			if err == nil || !strings.HasPrefix(err.Error(), "REC001") {
				t.Fatalf("recover = %v, want REC001", err)
			}
			if got := settings(t, root, "public", "release"); got != before {
				t.Fatalf("settings = %q, want %q", got, before)
			}
			if got := reference(t, root, "private", "refs/heads/release"); got != commits["private"] {
				t.Fatal("first repository changed before settings refusal")
			}
			want := commits["public"]
			if deleted {
				want = ""
			}
			if got := reference(t, root, "public", "refs/heads/release"); got != want {
				t.Fatal("ref changed before settings refusal")
			}
		})
	}
}

func TestAbortRetriesAfterRestoringARefOrAWholeRepository(t *testing.T) {
	for _, blocked := range []string{"public", "private"} {
		t.Run(blocked, func(t *testing.T) {
			configuration, root := released(t)
			for _, name := range []string{"private", "public"} {
				track(t, root, name, "release", reference(t, root, name, "refs/heads/release"))
				run(t, root, name, "config", "--add", "branch.release.description", "first")
				run(t, root, name, "config", "--add", "branch.release.description", "second")
			}
			beforeSettings := settings(t, root, "public", "release")
			beforeLogs := map[string][]byte{}
			for _, name := range []string{"private", "public"} {
				beforeLogs[name], _ = os.ReadFile(reflogPath(root, name, "release"))
			}
			commits := interrupt(t, root, "release", "private", "public")
			// Abort visits public first. A config lock fails either immediately
			// after its ref creation or after restoring that entire repository.
			configLock := filepath.Join(repository.Directory(root, blocked), "config.lock")
			if err := os.WriteFile(configLock, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := stage.Abort(configuration, root, io.Discard); err == nil {
				t.Fatal("abort ignored config lock")
			}
			if got := reference(t, root, "public", "refs/heads/release"); got != commits["public"] {
				t.Fatal("did not reach expected crash boundary")
			}
			if err := os.Remove(configLock); err != nil {
				t.Fatal(err)
			}
			if err := stage.Abort(configuration, root, io.Discard); err != nil {
				t.Fatalf("retry: %v", err)
			}
			for name, commit := range commits {
				if got := reference(t, root, name, "refs/heads/release"); got != commit {
					t.Fatalf("%s ref = %q", name, got)
				}
				if got := settings(t, root, name, "release"); got != beforeSettings {
					t.Fatalf("%s settings = %q", name, got)
				}
				log, err := os.ReadFile(reflogPath(root, name, "release"))
				if err != nil || !bytes.Equal(log, beforeLogs[name]) {
					t.Fatalf("%s reflog not restored: %v", name, err)
				}
			}
		})
	}
}

func TestAbortRetriesReflogFinalizationWithoutAReflog(t *testing.T) {
	configuration, root := released(t)
	for _, name := range []string{"private", "public"} {
		if err := os.Remove(reflogPath(root, name, "release")); err != nil {
			t.Fatal(err)
		}
	}
	commits := interrupt(t, root, "release", "private", "public")
	for name, commit := range commits {
		run(t, root, name, "update-ref", "--create-reflog", "-m", "gitone branch: Restored deleted branch "+recoveryMarker, "refs/heads/release", commit, "")
	}
	editDeletionState(t, root, func(saved map[string]any) {
		saved["finalizing"] = true
		for _, raw := range saved["deleted"].([]any) {
			raw.(map[string]any)["restoring"] = true
		}
	})
	// The first original (absent) reflog was restored before the crash.
	if err := os.Remove(reflogPath(root, "private", "release")); err != nil {
		t.Fatal(err)
	}
	if err := stage.Abort(configuration, root, io.Discard); err != nil {
		t.Fatalf("retry: %v", err)
	}
	for name, commit := range commits {
		if got := reference(t, root, name, "refs/heads/release"); got != commit {
			t.Fatalf("%s ref = %q", name, got)
		}
		if _, err := os.Stat(reflogPath(root, name, "release")); !os.IsNotExist(err) {
			t.Fatalf("%s reflog should be absent: %v", name, err)
		}
	}
}

func TestRecoverRefusesLegacyDeletionWithoutIntent(t *testing.T) {
	configuration, root := released(t)
	commits := interrupt(t, root, "release")
	editDeletionState(t, root, func(saved map[string]any) { saved["version"] = 2 })
	if err := stage.Recover(configuration, root, io.Discard); err == nil || !strings.HasPrefix(err.Error(), "REC001") {
		t.Fatalf("recover = %v, want REC001", err)
	}
	for name, commit := range commits {
		if got := reference(t, root, name, "refs/heads/release"); got != commit {
			t.Fatalf("%s ref changed", name)
		}
	}
}

func TestRecoverRecordsIntentBeforeDeletingAnotherRepository(t *testing.T) {
	configuration, root := released(t)
	commits := interrupt(t, root, "release", "private")
	configLock := filepath.Join(repository.Directory(root, "public"), "config.lock")
	if err := os.WriteFile(configLock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := stage.Recover(configuration, root, io.Discard); err == nil {
		t.Fatal("recover ignored config lock")
	}
	if got := reference(t, root, "public", "refs/heads/release"); got != "" {
		t.Fatal("recover did not reach ref deletion")
	}
	if err := os.Remove(configLock); err != nil {
		t.Fatal(err)
	}
	run(t, root, "public", "branch", "release", "main")
	before := run(t, root, "public", "reflog", "show", "refs/heads/release")
	if err := stage.Recover(configuration, root, io.Discard); err == nil || !strings.HasPrefix(err.Error(), "REC001") {
		t.Fatalf("recover = %v, want REC001", err)
	}
	if got := reference(t, root, "public", "refs/heads/release"); got != commits["public"] {
		t.Fatal("recreated branch was deleted")
	}
	if got := run(t, root, "public", "reflog", "show", "refs/heads/release"); got != before {
		t.Fatal("recreated reflog changed")
	}
}
