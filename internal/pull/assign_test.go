package pull_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/devidevio/gitone/internal/config"
	"github.com/devidevio/gitone/internal/git"
	"github.com/devidevio/gitone/internal/lock"
	"github.com/devidevio/gitone/internal/pull"
	"github.com/devidevio/gitone/internal/repository"
	"github.com/devidevio/gitone/internal/stage"
)

// assign runs a pull that accepts the generated path assignments but no
// policy change, which is what --accept-new-paths alone does.
func assign(t *testing.T, loaded *config.Config, root, target string) string {
	t.Helper()
	var output bytes.Buffer
	if err := pull.Pull(loaded, root, target, false, true, false, nil, &output); err != nil {
		t.Fatalf("pull %q: %v\noutput: %s", target, err, output.String())
	}
	return output.String()
}

// answer runs an interactive pull with a prepared answer for every question.
func answer(t *testing.T, loaded *config.Config, root, answers string) (error, string) {
	t.Helper()
	var output bytes.Buffer
	err := pull.Pull(loaded, root, "", false, false, true, strings.NewReader(answers), &output)
	return err, output.String()
}

// owned is the paths list the effective configuration assigns to a repository.
func owned(t *testing.T, root, name string) []string {
	t.Helper()
	loaded, _, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	return loaded.Repositories[name].Paths
}

// external clones a bare repository so one external commit can carry several
// files, the way a real remote change usually does.
func external(t *testing.T, bare string) string {
	t.Helper()
	clone := temporary(t)
	if _, err := git.Run(clone, "clone", "--quiet", bare, clone); err != nil {
		t.Fatal(err)
	}
	return clone
}

// publishExternal commits every written file of a clone and returns the commit
// its main branch now has.
func publishExternal(t *testing.T, clone, bare, message string) string {
	t.Helper()
	// -f because an ignore rule of the project must not stop a remote from
	// carrying the very path a test needs it to carry.
	for _, arguments := range [][]string{{"add", "-A", "-f"}, {"commit", "--quiet", "-m", message},
		{"push", "--quiet", "origin", "HEAD:refs/heads/main"}} {
		if _, err := git.Run(clone, arguments...); err != nil {
			t.Fatal(err)
		}
	}
	output, err := git.Run(bare, "--git-dir="+bare, "for-each-ref", "--format=%(objectname)", "refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(output)
}

// porcelain is the native status of one managed repository, which decides
// whether a patched configuration file was left unstaged.
func porcelain(t *testing.T, root, name string) string {
	t.Helper()
	output, err := git.Run(root, "--no-optional-locks", "--git-dir="+repository.Directory(root, name),
		"--work-tree="+root, "status", "--porcelain")
	if err != nil {
		t.Fatal(err)
	}
	return output
}

func TestPullAssignsOneNewPathToItsOwnRepository(t *testing.T) {
	loaded, root, remotes := seed(t, nil)
	target := advance(t, remotes["public"], "CHANGELOG.md", "# changes\n")

	message := fails(t, loaded, root, "")
	if !strings.Contains(message, pull.AcceptPathsFlag) {
		t.Fatalf("message = %q, want the accept flag", message)
	}
	if got := head(t, root, "public"); got == target {
		t.Fatal("public advanced without a confirmation")
	}

	output := assign(t, loaded, root, "")
	if !strings.Contains(output, "CHANGELOG.md -> public") {
		t.Fatalf("output = %q, want the exact proposal", output)
	}
	if got := head(t, root, "public"); got != target {
		t.Fatalf("public head = %s, want %s", got, target)
	}
	if got := owned(t, root, "public"); !slices.Contains(got, "CHANGELOG.md") {
		t.Fatalf("public paths = %v, want CHANGELOG.md", got)
	}
	if got := contents(t, root, "CHANGELOG.md"); got != "# changes\n" {
		t.Fatalf("CHANGELOG.md = %q", got)
	}
}

func TestPullAssignsExactPathsWithLiteralAndYAMLSpecialCharacters(t *testing.T) {
	loaded, root, remotes := seed(t, nil)
	clone := external(t, remotes["public"])
	paths := []string{"release #1.md", "a: b.md", "report?.md", "literal[1].md", "{draft}.md"}
	for _, name := range paths {
		writeExternal(t, clone, name, name+"\n")
	}
	publishExternal(t, clone, remotes["public"], "External literal paths")

	assign(t, loaded, root, "")
	got := owned(t, root, "public")
	for _, name := range paths {
		if !slices.Contains(got, name) {
			t.Fatalf("public paths = %q, want exact %q", got, name)
		}
		if contents(t, root, name) != name+"\n" {
			t.Fatalf("%s was not pulled", name)
		}
	}
}

func TestPullNeverOffersAnotherOwnerOrAnIgnoreAction(t *testing.T) {
	loaded, root, remotes := seed(t, nil)
	advance(t, remotes["public"], "CHANGELOG.md", "# changes\n")

	_, output := answer(t, loaded, root, "n\n")
	for _, unwanted := range []string{"private", "gnore", "hoose", "Which repository"} {
		if strings.Contains(output, unwanted) {
			t.Fatalf("output = %q, want no %q offer", output, unwanted)
		}
	}
	if strings.Count(output, "Assign these paths?") != 1 {
		t.Fatalf("output = %q, want exactly one question", output)
	}
}

func TestPullProposesOnlyAWholeNewDirectory(t *testing.T) {
	loaded, root, remotes := seed(t, nil)
	// docs/ and notes/ do not exist yet, so their subtrees are safe to own;
	// src/ already does, and its incoming file is owned anyway.
	clone := external(t, remotes["public"])
	for name, contents := range map[string]string{
		"docs/guide/one.md": "one\n", "docs/guide/two.md": "two\n", "docs/only.md": "three\n",
		"notes/single.md": "alone\n", "src/app.go": "package main\n",
	} {
		writeExternal(t, clone, name, contents)
	}
	publishExternal(t, clone, remotes["public"], "External docs")

	output := assign(t, loaded, root, "")
	for _, want := range []string{"docs/** -> public (3 new paths)", "notes/single.md -> public"} {
		if !strings.Contains(output, want) {
			t.Fatalf("output = %q, want %q", output, want)
		}
	}
	got := owned(t, root, "public")
	if !slices.Contains(got, "docs/**") || !slices.Contains(got, "notes/single.md") {
		t.Fatalf("public paths = %v", got)
	}
	if slices.Contains(got, "notes/**") {
		t.Fatalf("public paths = %v, want no subtree for a single new path", got)
	}
}

func TestPullNeverWidensIntoAnExistingDirectory(t *testing.T) {
	loaded, root, remotes := seed(t, nil)
	// build/ exists locally and is ignored, so its subtree is not new: the
	// incoming paths stay exact and the local file is never claimed.
	publish(t, loaded, root, config.ProjectIgnoreFile, "build/\n")
	write(t, root, "build/local.txt", "local\n")
	clone := external(t, remotes["public"])
	writeExternal(t, clone, "build/a.js", "a\n")
	writeExternal(t, clone, "build/b.js", "b\n")
	publishExternal(t, clone, remotes["public"], "External build")

	output := assign(t, loaded, root, "")
	for _, want := range []string{"build/a.js -> public", "build/b.js -> public"} {
		if !strings.Contains(output, want) {
			t.Fatalf("output = %q, want %q", output, want)
		}
	}
	if got := owned(t, root, "public"); slices.Contains(got, "build/**") {
		t.Fatalf("public paths = %v, want no widened directory", got)
	}
	if got := contents(t, root, "build/local.txt"); got != "local\n" {
		t.Fatalf("build/local.txt = %q, want the untouched local file", got)
	}
}

func TestPullRefusesToAssignAPathThatExistsLocally(t *testing.T) {
	loaded, root, remotes := seed(t, nil)
	// The local file is ignored today, so nothing reports it. Owning the
	// incoming path of the same name would make it publishable.
	publish(t, loaded, root, config.ProjectIgnoreFile, "CHANGELOG.md\n")
	write(t, root, "CHANGELOG.md", "local notes\n")
	before := head(t, root, "public")
	clone := external(t, remotes["public"])
	writeExternal(t, clone, "CHANGELOG.md", "# changes\n")
	publishExternal(t, clone, remotes["public"], "External changelog")

	var output bytes.Buffer
	err := pull.Pull(loaded, root, "", true, true, false, nil, &output)
	if err == nil {
		t.Fatalf("an existing local path was assigned, output: %s", output.String())
	}
	if !strings.Contains(err.Error(), "already exist locally") || !strings.Contains(err.Error(), "CHANGELOG.md") {
		t.Fatalf("message = %q, want the local-collision refusal", err.Error())
	}
	if got := head(t, root, "public"); got != before {
		t.Fatalf("public head = %s, want the unchanged %s", got, before)
	}
	if got := contents(t, root, "CHANGELOG.md"); got != "local notes\n" {
		t.Fatalf("CHANGELOG.md = %q, want the untouched local file", got)
	}
}

func TestPullShowsEveryProposalBeforeOneConfirmationAndCancelsCleanly(t *testing.T) {
	loaded, root, remotes := seed(t, nil)
	before := head(t, root, "public")
	configuration := contents(t, root, config.PublicFile)
	clone := external(t, remotes["public"])
	writeExternal(t, clone, "docs/a.md", "a\n")
	writeExternal(t, clone, "docs/b.md", "b\n")
	writeExternal(t, clone, "CHANGELOG.md", "c\n")
	publishExternal(t, clone, remotes["public"], "External batch")

	err, output := answer(t, loaded, root, "n\n")
	if err != nil {
		t.Fatalf("declined pull failed: %v\noutput: %s", err, output)
	}
	for _, want := range []string{"CHANGELOG.md -> public", "docs/** -> public (2 new paths)", "Pull aborted."} {
		if !strings.Contains(output, want) {
			t.Fatalf("output = %q, want %q", output, want)
		}
	}
	if got := head(t, root, "public"); got != before {
		t.Fatalf("public head = %s, want the unchanged %s", got, before)
	}
	if got := contents(t, root, config.PublicFile); got != configuration {
		t.Fatalf("%s changed on cancellation", config.PublicFile)
	}
	if contents(t, root, "CHANGELOG.md") != "" {
		t.Fatal("a declined pull created a working-tree file")
	}
	if recovered(t, root) {
		t.Fatal("a declined pull left recovery state")
	}
}

func TestPullDoesNotAssistWhileAnotherProblemRemains(t *testing.T) {
	loaded, root, remotes := seed(t, nil)
	clone := external(t, remotes["public"])
	writeExternal(t, clone, "CHANGELOG.md", "c\n")
	writeExternal(t, clone, "secrets/leak.txt", "owned by private\n")
	publishExternal(t, clone, remotes["public"], "External mixed")

	var output bytes.Buffer
	err := pull.Pull(loaded, root, "", true, true, false, nil, &output)
	if err == nil {
		t.Fatalf("%s accepted a foreign path, output: %s", pull.AcceptPathsFlag, output.String())
	}
	for _, want := range []string{"secrets/leak.txt", "CHANGELOG.md"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("message = %q, want %q reported", err.Error(), want)
		}
	}
	if got := owned(t, root, "public"); slices.Contains(got, "CHANGELOG.md") {
		t.Fatalf("public paths = %v, want no assignment", got)
	}
}

func TestPullAssistsOnlyUnassignedOwnership(t *testing.T) {
	for name, incoming := range map[string]string{
		"reserved":  ".gitone/state.json",
		"protected": ".gitone.local.yml",
	} {
		t.Run(name, func(t *testing.T) {
			loaded, root, remotes := seed(t, nil)
			before := head(t, root, "public")
			clone := external(t, remotes["public"])
			writeExternal(t, clone, incoming, "x\n")
			publishExternal(t, clone, remotes["public"], "External "+incoming)

			var output bytes.Buffer
			err := pull.Pull(loaded, root, "", true, true, false, nil, &output)
			if err == nil {
				t.Fatalf("%s was accepted, output: %s", incoming, output.String())
			}
			if !strings.Contains(err.Error(), incoming) {
				t.Fatalf("message = %q, want %q", err.Error(), incoming)
			}
			if got := head(t, root, "public"); got != before {
				t.Fatalf("public head = %s, want the unchanged %s", got, before)
			}
		})
	}
}

func TestPullNeverAssignsAPathThatWouldBecomeAGlob(t *testing.T) {
	loaded, root, remotes := seed(t, nil)
	before := head(t, root, "public")
	// A * is a legal path character but matches inside a segment as a
	// pattern, so assigning it would claim more than the one incoming path.
	clone := external(t, remotes["public"])
	writeExternal(t, clone, "docs/a*b.md", "glob\n")
	publishExternal(t, clone, remotes["public"], "External glob")

	var output bytes.Buffer
	err := pull.Pull(loaded, root, "", true, true, false, nil, &output)
	if err == nil {
		t.Fatalf("a glob path was assigned, output: %s", output.String())
	}
	if !strings.Contains(err.Error(), "docs/a*b.md") || !strings.Contains(err.Error(), "not assigned to any repository") {
		t.Fatalf("message = %q, want the ordinary unassigned refusal", err.Error())
	}
	if got := head(t, root, "public"); got != before {
		t.Fatalf("public head = %s, want the unchanged %s", got, before)
	}
}

func TestPullValidatesPolicyAndAssignmentsAsOneConfiguration(t *testing.T) {
	loaded, root, remotes := seed(t, nil)
	// The incoming commit changes the policy and brings one path neither the
	// old nor the new policy assigns.
	extended := append(append([]string{}, publicPaths...), "tools/**")
	clone := external(t, remotes["public"])
	writeExternal(t, clone, config.PublicFile, committed(remotes, extended, ""))
	writeExternal(t, clone, "tools/build.sh", "#!/bin/sh\n")
	writeExternal(t, clone, "CHANGELOG.md", "c\n")
	target := publishExternal(t, clone, remotes["public"], "External policy and paths")

	var output bytes.Buffer
	if err := pull.Pull(loaded, root, "", true, false, false, nil, &output); err == nil {
		t.Fatalf("%s alone accepted the assignments", pull.AcceptFlag)
	}
	if err := pull.Pull(loaded, root, "", false, true, false, nil, &output); err == nil {
		t.Fatalf("%s alone accepted the policy change", pull.AcceptPathsFlag)
	}

	output.Reset()
	if err := pull.Pull(loaded, root, "", true, true, false, nil, &output); err != nil {
		t.Fatalf("pull: %v\noutput: %s", err, output.String())
	}
	// One ordered review: the remote policy change and the paths it affects
	// come before the locally generated assignments.
	policy := strings.Index(output.String(), "Incoming policy change:")
	assignments := strings.Index(output.String(), "New paths without an owner:")
	if policy < 0 || assignments < 0 || policy > assignments {
		t.Fatalf("output = %q, want the policy preview before the assignments", output.String())
	}
	if got := head(t, root, "public"); got != target {
		t.Fatalf("public head = %s, want %s", got, target)
	}
	// tools/** is owned by the accepted incoming policy, so only the still
	// unassigned CHANGELOG.md is added, and it is added to that new version.
	got := owned(t, root, "public")
	if !slices.Contains(got, "CHANGELOG.md") || !slices.Contains(got, "tools/**") {
		t.Fatalf("public paths = %v", got)
	}
	if strings.Contains(output.String(), "tools/build.sh -> public") {
		t.Fatalf("output = %q, want no proposal for an already owned path", output.String())
	}
}

func TestPullPatchesOnlyTheAffectedSequence(t *testing.T) {
	loaded, root, remotes := seed(t, nil)
	before := contents(t, root, config.PublicFile)
	advance(t, remotes["public"], "CHANGELOG.md", "# changes\n")

	assign(t, loaded, root, "")
	want := strings.Replace(before, "      - src/**\n", "      - src/**\n      - \"CHANGELOG.md\"\n", 1)
	if after := contents(t, root, config.PublicFile); after != want {
		t.Fatalf("%s =\n%s\nwant\n%s", config.PublicFile, after, want)
	}
}

func TestPullPatchesTheOverridingLocalFile(t *testing.T) {
	_, root, remotes := seed(t, nil)
	// The local file takes over the paths list of the public repository, so
	// it supplies that list and is the only file an addition may touch.
	write(t, root, config.LocalFile, `version: 1
repositories:
  public:
    visibility: public
    paths:
      - .gitignore
      - .gitone.yml
      - README.md
      - src/**
`)
	local, _, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	published := contents(t, root, config.PublicFile)
	advance(t, remotes["public"], "CHANGELOG.md", "# changes\n")

	output := assign(t, local, root, "")
	if !strings.Contains(output, config.LocalFile) {
		t.Fatalf("output = %q, want %s named", output, config.LocalFile)
	}
	if got := contents(t, root, config.PublicFile); got != published {
		t.Fatalf("%s was patched: %s", config.PublicFile, got)
	}
	if got := contents(t, root, config.LocalFile); !strings.Contains(got, `- "CHANGELOG.md"`) {
		t.Fatalf("%s = %s", config.LocalFile, got)
	}
	if got := owned(t, root, "public"); !slices.Contains(got, "CHANGELOG.md") {
		t.Fatalf("public paths = %v", got)
	}
}

func TestPullLeavesTheAssignmentUnstagedAndReportsIt(t *testing.T) {
	loaded, root, remotes := seed(t, nil)
	advance(t, remotes["public"], "CHANGELOG.md", "# changes\n")

	output := assign(t, loaded, root, "")
	for _, want := range []string{"review " + config.PublicFile, "gitone commit " + config.PublicFile} {
		if !strings.Contains(output, want) {
			t.Fatalf("output = %q, want %q", output, want)
		}
	}
	if got := porcelain(t, root, "public"); !strings.Contains(got, " M "+config.PublicFile) {
		t.Fatalf("status = %q, want %s unstaged", got, config.PublicFile)
	}
	if recovered(t, root) {
		t.Fatal("a completed pull left recovery state")
	}
}

func TestAcceptNewPathsIsHarmlessAndAcceptsNothingElse(t *testing.T) {
	loaded, root, remotes := seed(t, nil)
	target := advance(t, remotes["public"], "src/app.go", "package main\n")

	// Nothing to assign: the flag changes nothing and the pull is ordinary.
	output := assign(t, loaded, root, "")
	if strings.Contains(output, "New paths without an owner") {
		t.Fatalf("output = %q, want no proposal", output)
	}
	if got := head(t, root, "public"); got != target {
		t.Fatalf("public head = %s, want %s", got, target)
	}
	// Repeating it stays a no-op, exactly like an ordinary repeated pull.
	assign(t, loaded, root, "")
	if got := contents(t, root, config.PublicFile); strings.Count(got, "- src/**") != 1 {
		t.Fatalf("%s = %s, want an unchanged paths list", config.PublicFile, got)
	}

	// It never accepts a policy change.
	extended := append(append([]string{}, publicPaths...), "docs/**")
	advance(t, remotes["public"], config.PublicFile, committed(remotes, extended, ""))
	var second bytes.Buffer
	if err := pull.Pull(loaded, root, "", false, true, false, nil, &second); err == nil {
		t.Fatalf("%s accepted a policy change, output: %s", pull.AcceptPathsFlag, second.String())
	}
}

func TestPullLeavesTheConfigurationAloneWhenAFastForwardFails(t *testing.T) {
	loaded, root, remotes := seed(t, nil)
	before := head(t, root, "public")
	configuration := contents(t, root, config.PublicFile)
	clone := external(t, remotes["public"])
	writeExternal(t, clone, "docs/a.md", "a\n")
	writeExternal(t, clone, "docs/b.md", "b\n")
	writeExternal(t, clone, "src/blocked.go", "package main\n")
	publishExternal(t, clone, remotes["public"], "External blocked")
	// An untracked owned file native Git refuses to overwrite makes the
	// fast-forward itself fail, after the configuration was already recorded.
	write(t, root, "src/blocked.go", "package other\n")

	var output bytes.Buffer
	err := pull.Pull(loaded, root, "", false, true, false, nil, &output)
	if err == nil {
		t.Fatalf("the blocked fast-forward succeeded, output: %s", output.String())
	}
	if got := contents(t, root, config.PublicFile); got != configuration {
		t.Fatalf("%s was patched by a failed pull:\n%s", config.PublicFile, got)
	}
	if got := head(t, root, "public"); got != before {
		t.Fatalf("public head = %s, want the unchanged %s", got, before)
	}
	if recovered(t, root) {
		t.Fatal("a rolled back pull left recovery state")
	}
}

// interrupt records the state a pull interrupted between its fast-forward and
// its configuration patch leaves behind.
func interrupt(t *testing.T, root string, originals map[string][]byte, entry map[string]any,
	file string, patterns map[string][]string, original string) {
	t.Helper()
	directory := lock.RecoveryPath(root)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	name := entry["name"].(string)
	if err := os.WriteFile(filepath.Join(directory, name+".original"), originals[name], 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "0.configuration.original"), []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	target := []byte(original)
	patched := target
	for _, repositoryName := range slices.Sorted(maps.Keys(patterns)) {
		var err error
		patched, err = config.AddPaths(patched, repositoryName, patterns[repositoryName])
		if err != nil {
			t.Fatal(err)
		}
	}
	hash := func(contents []byte) string { return fmt.Sprintf("%x", sha256.Sum256(contents)) }
	saved := map[string]any{"version": 2, "command": pull.Command,
		"repositories": []map[string]any{entry},
		"configuration": []map[string]any{{"path": file, "patterns": patterns,
			"original_hash": hash([]byte(original)), "target_hash": hash(target), "patched_hash": hash(patched)}},
	}
	data, err := json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, lock.StateFile), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverFinishesTheConfigurationPatchOfAnInterruptedPull(t *testing.T) {
	loaded, root, remotes := seed(t, nil)
	original := contents(t, root, config.PublicFile)
	target := advance(t, remotes["public"], "CHANGELOG.md", "# changes\n")
	before := head(t, root, "public")
	originals := map[string][]byte{"public": index(t, root, "public")}
	// The pull replaced the files but was interrupted before it could move
	// the branch and write the configuration patch.
	assign(t, loaded, root, "")
	write(t, root, config.PublicFile, original)
	if _, err := git.Run(root, "--git-dir="+repository.Directory(root, "public"),
		"update-ref", "refs/heads/main", before, target); err != nil {
		t.Fatal(err)
	}
	interrupt(t, root, originals, map[string]any{"name": "public", "branch": "main", "head": before, "target": target},
		config.PublicFile, map[string][]string{"public": {"CHANGELOG.md"}}, original)

	var output bytes.Buffer
	if err := stage.Recover(loaded, root, &output); err != nil {
		t.Fatalf("recover: %v\noutput: %s", err, output.String())
	}
	if got := head(t, root, "public"); got != target {
		t.Fatalf("public main = %s, want the finished %s", got, target)
	}
	if got := owned(t, root, "public"); !slices.Contains(got, "CHANGELOG.md") {
		t.Fatalf("public paths = %v, want the finished assignment", got)
	}
	if !strings.Contains(output.String(), config.PublicFile) {
		t.Fatalf("output = %q, want the configuration file named", output.String())
	}
}

func TestAbortRestoresTheConfigurationOfAnInterruptedPull(t *testing.T) {
	loaded, root, remotes := seed(t, nil)
	original := contents(t, root, config.PublicFile)
	target := advance(t, remotes["public"], "CHANGELOG.md", "# changes\n")
	before := head(t, root, "public")
	originals := map[string][]byte{"public": index(t, root, "public")}
	// The pull finished completely; only its recorded state is still there.
	assign(t, loaded, root, "")
	interrupt(t, root, originals, map[string]any{"name": "public", "branch": "main", "head": before, "target": target, "updated": true},
		config.PublicFile, map[string][]string{"public": {"CHANGELOG.md"}}, original)

	var output bytes.Buffer
	if err := stage.Abort(loaded, root, &output); err != nil {
		t.Fatalf("abort: %v\noutput: %s", err, output.String())
	}
	if got := head(t, root, "public"); got != before {
		t.Fatalf("public main = %s, want the restored %s", got, before)
	}
	if got := contents(t, root, config.PublicFile); got != original {
		t.Fatalf("%s =\n%s\nwant the restored\n%s", config.PublicFile, got, original)
	}
}

func TestRecoveryRefusesAConfigurationChangedOutsideGitOne(t *testing.T) {
	commands := map[string]func(*config.Config, string, *bytes.Buffer) error{
		"recover": func(loaded *config.Config, root string, output *bytes.Buffer) error {
			return stage.Recover(loaded, root, output)
		},
		"abort": func(loaded *config.Config, root string, output *bytes.Buffer) error {
			return stage.Abort(loaded, root, output)
		},
	}
	for _, command := range slices.Sorted(maps.Keys(commands)) {
		t.Run(command, func(t *testing.T) {
			loaded, root, remotes := seed(t, nil)
			original := contents(t, root, config.PublicFile)
			target := advance(t, remotes["public"], "CHANGELOG.md", "# changes\n")
			before := head(t, root, "public")
			originals := map[string][]byte{"public": index(t, root, "public")}
			assign(t, loaded, root, "")
			interrupt(t, root, originals,
				map[string]any{"name": "public", "branch": "main", "head": before, "target": target, "updated": true},
				config.PublicFile, map[string][]string{"public": {"CHANGELOG.md"}}, original)

			changed := contents(t, root, config.PublicFile) + "# changed outside GitOne\n"
			write(t, root, config.PublicFile, changed)
			var output bytes.Buffer
			err := commands[command](loaded, root, &output)
			if err == nil || !strings.Contains(err.Error(), "configuration file .gitone.yml changed outside GitOne") {
				t.Fatalf("%s error = %v", command, err)
			}
			if got := head(t, root, "public"); got != target {
				t.Fatalf("public head = %s, want unchanged %s", got, target)
			}
			if got := contents(t, root, config.PublicFile); got != changed {
				t.Fatalf("%s overwrote the external change", command)
			}
			if !recovered(t, root) {
				t.Fatal("recovery state was removed after a refusal")
			}
		})
	}
}

func writeExternal(t *testing.T, clone, name, contents string) {
	t.Helper()
	full := filepath.Join(clone, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}
