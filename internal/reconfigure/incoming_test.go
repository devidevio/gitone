package reconfigure_test

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/devidevio/gitone/internal/assign"
	"github.com/devidevio/gitone/internal/config"
	"github.com/devidevio/gitone/internal/fetch"
	"github.com/devidevio/gitone/internal/git"
	"github.com/devidevio/gitone/internal/lock"
	"github.com/devidevio/gitone/internal/reconfigure"
	"github.com/devidevio/gitone/internal/repository"
	"github.com/devidevio/gitone/internal/stage"
	"github.com/devidevio/gitone/internal/status"
)

// incoming reconfigures from a source without a terminal, which is what the
// flags alone accept.
func incoming(t *testing.T, configuration *config.Config, root string, source reconfigure.Source,
	accepted reconfigure.Accepted, renames ...reconfigure.Rename) (string, error) {
	t.Helper()
	var output bytes.Buffer
	err := reconfigure.Reconfigure(configuration, root, source, renames, accepted, false,
		strings.NewReader(""), &output)
	return output.String(), err
}

// publish commits files onto one branch of a bare repository, which is what
// an incoming commit of a managed repository looks like.
func publish(t *testing.T, bareRepository, branchName string, files map[string]string) string {
	t.Helper()
	work := temporary(t)
	if _, err := git.Run(work, "clone", "--quiet", bareRepository, work); err != nil {
		t.Fatal(err)
	}
	for path, contents := range files {
		write(t, work, path, contents)
	}
	for _, arguments := range [][]string{{"add", "-A"}, {"commit", "--quiet", "-m", "Incoming"},
		{"push", "--quiet", "origin", "HEAD:refs/heads/" + branchName}} {
		if _, err := git.Run(work, arguments...); err != nil {
			t.Fatal(err)
		}
	}
	commit, err := git.Run(bareRepository, "--git-dir="+bareRepository,
		"for-each-ref", "--format=%(objectname)", "refs/heads/"+branchName)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(commit)
}

// hosted is a bare repository whose branch already holds the given files,
// which is what a repository an incoming configuration adds looks like.
func hosted(t *testing.T, name, branchName string, files map[string]string) string {
	t.Helper()
	full := bare(t, temporary(t), name)
	publish(t, full, branchName, files)
	return full
}

// branched creates one branch in one managed repository, commits files on it
// and returns the repository to the branch it was on.
func branched(t *testing.T, root, name, branchName string, files map[string]string) {
	t.Helper()
	managed(t, root, name, "checkout", "--quiet", "-b", branchName)
	for path, contents := range files {
		write(t, root, path, contents)
		managed(t, root, name, "add", "--", path)
	}
	managed(t, root, name, "commit", "--quiet", "-m", branchName+" in "+name)
	managed(t, root, name, "checkout", "--quiet", "main")
}

// managed runs git against one managed repository the way GitOne does.
func managed(t *testing.T, root, name string, arguments ...string) {
	t.Helper()
	arguments = append([]string{"--git-dir=" + repository.Directory(root, name), "--work-tree=" + root,
		"-c", "user.name=GitOne", "-c", "user.email=gitone@example.com"}, arguments...)
	if _, err := git.Run(root, arguments...); err != nil {
		t.Fatal(err)
	}
}

// configuration renders a project configuration with an optional third
// repository, which is what an incoming addition carries.
func configuration(private, public, extra string) string {
	rendered := fmt.Sprintf(`version: 1
default_branch: main
repositories:
  private:
    remote: %s
    visibility: private
    paths:
      - secrets/**
  public:
    remote: %s
    visibility: public
    paths:
      - .gitignore
      - .gitone.yml
      - README.md
`, private, public)
	if extra == "" {
		return rendered
	}
	return rendered + fmt.Sprintf(`  docs:
    remote: %s
    visibility: public
    paths:
      - docs/**
`, extra)
}

func TestReconfigurePullAddsARepositoryAndAdvancesInOneCommand(t *testing.T) {
	loaded, root, remotes := seed(t)
	docs := hosted(t, "docs", "main", map[string]string{"docs/guide.md": "guide\n"})
	target := publish(t, remotes["public"], "main", map[string]string{
		config.PublicFile: configuration(remotes["private"], remotes["public"], docs),
	})

	output, err := incoming(t, loaded, root, reconfigure.Source{Pull: true},
		reconfigure.Accepted{Repository: true})
	if err != nil {
		t.Fatalf("reconfigure --pull: %v\noutput: %s", err, output)
	}
	if got := head(t, root, "public"); got != target {
		t.Fatalf("public head = %s, want the incoming %s\noutput: %s", got, target, output)
	}
	if got := contents(t, root, config.PublicFile); !strings.Contains(got, "docs:") {
		t.Fatalf("%s = %q, want the incoming configuration", config.PublicFile, got)
	}
	if got := contents(t, root, "docs/guide.md"); got != "guide\n" {
		t.Fatalf("docs/guide.md = %q, want the checked-out incoming file", got)
	}
	if _, err := os.Stat(repository.Directory(root, "docs")); err != nil {
		t.Fatalf("docs metadata: %v", err)
	}
	// The project has to be usable afterwards, which is what a status of a
	// half-applied change would refuse.
	if _, _, err := status.Validate(reload(t, root), root); err != nil {
		t.Fatalf("status after the reconfiguration: %v", err)
	}
}

func TestReconfigureSwitchAppliesTheBranchConfigurationAndMovesEveryRepository(t *testing.T) {
	loaded, root, remotes := seed(t)
	docs := hosted(t, "docs", "feature", map[string]string{"docs/guide.md": "guide\n"})
	branched(t, root, "private", "feature", map[string]string{"secrets/more.txt": "more\n"})
	branched(t, root, "public", "feature", map[string]string{
		config.PublicFile: configuration(remotes["private"], remotes["public"], docs),
	})

	output, err := incoming(t, loaded, root, reconfigure.Source{Branch: "feature"},
		reconfigure.Accepted{Repository: true})
	if err != nil {
		t.Fatalf("reconfigure --switch: %v\noutput: %s", err, output)
	}
	for _, name := range []string{"private", "public", "docs"} {
		reference, err := git.Run(root, "--git-dir="+repository.Directory(root, name),
			"symbolic-ref", "--quiet", "--short", "HEAD")
		if err != nil {
			t.Fatalf("%s HEAD: %v", name, err)
		}
		if got := strings.TrimSpace(reference); got != "feature" {
			t.Fatalf("%s is on %q, want feature\noutput: %s", name, got, output)
		}
	}
	if got := contents(t, root, config.PublicFile); !strings.Contains(got, "docs:") {
		t.Fatalf("%s = %q, want the branch configuration", config.PublicFile, got)
	}
	if got := contents(t, root, "docs/guide.md"); got != "guide\n" {
		t.Fatalf("docs/guide.md = %q, want the checked-out branch file", got)
	}
}

func TestReconfigurePullNeedsAcceptConfigChangeForAnOrdinaryChange(t *testing.T) {
	loaded, root, remotes := seed(t)
	before := head(t, root, "public")
	widened := strings.Replace(configuration(remotes["private"], remotes["public"], ""),
		"      - README.md\n", "      - README.md\n      - src/**\n", 1)
	publish(t, remotes["public"], "main", map[string]string{config.PublicFile: widened})

	output, err := incoming(t, loaded, root, reconfigure.Source{Pull: true},
		reconfigure.Accepted{Repository: true})
	if err == nil {
		t.Fatalf("%s accepted an ordinary policy change\noutput: %s", reconfigure.AcceptFlag, output)
	}
	wants(t, err.Error(), "--accept-config-change")
	if got := head(t, root, "public"); got != before {
		t.Fatalf("public head = %s, want the unchanged %s", got, before)
	}
	if strings.Contains(contents(t, root, config.PublicFile), "src/**") {
		t.Fatalf("%s was replaced by a refused change", config.PublicFile)
	}

	if output, err = incoming(t, loaded, root, reconfigure.Source{Pull: true},
		reconfigure.Accepted{Config: true}); err != nil {
		t.Fatalf("reconfigure --pull with the config flag: %v\noutput: %s", err, output)
	}
	if !strings.Contains(contents(t, root, config.PublicFile), "src/**") {
		t.Fatalf("%s = %q, want the accepted change", config.PublicFile, contents(t, root, config.PublicFile))
	}
}

func TestReconfigurePullRefusesAnIncomingNameTheLocalConfigurationDefines(t *testing.T) {
	loaded, root, remotes := seed(t)
	before := head(t, root, "public")
	docs := hosted(t, "docs", "main", map[string]string{"docs/guide.md": "guide\n"})
	write(t, root, config.LocalFile, fmt.Sprintf(`repositories:
  docs:
    remote: %s
    visibility: private
    paths:
      - docs/**
`, docs))
	// The locally defined repository exists before anything incoming arrives,
	// which is exactly the state the collision protects.
	if output, err := run(t, reload(t, root), root, true); err != nil {
		t.Fatalf("plain reconfigure: %v\noutput: %s", err, output)
	}
	publish(t, remotes["public"], "main", map[string]string{
		config.PublicFile: configuration(remotes["private"], remotes["public"], docs),
	})

	loaded = reload(t, root)
	for _, accepted := range []reconfigure.Accepted{{}, {Repository: true, Config: true, Paths: true}} {
		output, err := incoming(t, loaded, root, reconfigure.Source{Pull: true}, accepted)
		if err == nil {
			t.Fatalf("a local name collision was accepted with %+v\noutput: %s", accepted, output)
		}
		wants(t, err.Error(), "adds repository \"docs\"", config.LocalFile)
		if got := head(t, root, "public"); got != before {
			t.Fatalf("public head = %s, want the unchanged %s", got, before)
		}
	}
}

func TestReconfigurePullRefusesMovingTheConfigurationToAnotherRepository(t *testing.T) {
	loaded, root, remotes := seed(t)
	before := head(t, root, "public")
	moved := strings.Replace(configuration(remotes["private"], remotes["public"], ""),
		"      - secrets/**\n", "      - secrets/**\n      - .gitone.yml\n", 1)
	moved = strings.Replace(moved, "      - .gitone.yml\n      - README.md\n", "      - README.md\n", 1)
	publish(t, remotes["private"], "main", map[string]string{config.PublicFile: moved})
	dropped := temporary(t)
	if _, err := git.Run(dropped, "clone", "--quiet", remotes["public"], dropped); err != nil {
		t.Fatal(err)
	}
	for _, arguments := range [][]string{{"rm", "--quiet", config.PublicFile},
		{"commit", "--quiet", "-m", "Move the configuration"}, {"push", "--quiet", "origin", "HEAD:refs/heads/main"}} {
		if _, err := git.Run(dropped, arguments...); err != nil {
			t.Fatal(err)
		}
	}

	output, err := incoming(t, loaded, root, reconfigure.Source{Pull: true},
		reconfigure.Accepted{Repository: true, Config: true, Paths: true})
	if err == nil {
		t.Fatalf("a moved %s was accepted\noutput: %s", config.PublicFile, output)
	}
	wants(t, err.Error(), config.PublicFile)
	if got := head(t, root, "public"); got != before {
		t.Fatalf("public head = %s, want the unchanged %s", got, before)
	}
}

func TestReconfigurePullRefusesAOneSidedOwnershipMove(t *testing.T) {
	loaded, root, remotes := seed(t)
	before := head(t, root, "public")
	// README.md moves to private, but the incoming public tree still carries
	// it: an ownership move needs both repositories' commits to agree.
	moved := strings.Replace(configuration(remotes["private"], remotes["public"], ""),
		"      - secrets/**\n", "      - secrets/**\n      - README.md\n", 1)
	moved = strings.Replace(moved, "      - .gitone.yml\n      - README.md\n", "      - .gitone.yml\n", 1)
	publish(t, remotes["public"], "main", map[string]string{config.PublicFile: moved})

	output, err := incoming(t, loaded, root, reconfigure.Source{Pull: true},
		reconfigure.Accepted{Repository: true, Config: true, Paths: true})
	if err == nil {
		t.Fatalf("a one-sided ownership move was accepted\noutput: %s", output)
	}
	wants(t, err.Error(), "public", "README.md", "owned by private")
	if got := head(t, root, "public"); got != before {
		t.Fatalf("public head = %s, want the unchanged %s", got, before)
	}
}

func TestReconfigurePullRefusesATargetAnUnrelatedNewOriginDoesNotContain(t *testing.T) {
	loaded, root, remotes := seed(t)
	before := head(t, root, "public")
	elsewhere := unrelated(t, "public")
	publish(t, remotes["public"], "main", map[string]string{
		config.PublicFile: configuration(remotes["private"], elsewhere, ""),
	})

	output, err := incoming(t, loaded, root, reconfigure.Source{Pull: true},
		reconfigure.Accepted{Repository: true, Config: true})
	if err == nil {
		t.Fatalf("an unrelated new origin was accepted\noutput: %s", output)
	}
	wants(t, err.Error(), "unrelated history")
	if got := head(t, root, "public"); got != before {
		t.Fatalf("public head = %s, want the unchanged %s", got, before)
	}
	if got := remoteURL(t, root, "public", config.OriginRemote); got != remotes["public"] {
		t.Fatalf("public origin = %q, want the restored %q", got, remotes["public"])
	}
	if strings.Contains(contents(t, root, config.PublicFile), elsewhere) {
		t.Fatalf("%s was replaced by a refused change", config.PublicFile)
	}
}

func TestReconfigurePullContactsNoNewURLBeforeTheConfirmation(t *testing.T) {
	loaded, root, remotes := seed(t)
	unreachable := filepath.Join(temporary(t), "absent.git")
	publish(t, remotes["public"], "main", map[string]string{
		config.PublicFile: configuration(remotes["private"], remotes["public"], unreachable),
	})

	output, err := incoming(t, loaded, root, reconfigure.Source{Pull: true}, reconfigure.Accepted{})
	if err == nil {
		t.Fatalf("a run without the flag was accepted\noutput: %s", output)
	}
	wants(t, err.Error(), reconfigure.AcceptFlag)
	if strings.Contains(err.Error(), "could not be fetched") {
		t.Fatalf("the new URL was contacted before the confirmation: %v", err)
	}
	if _, err := os.Stat(repository.Directory(root, "docs")); !os.IsNotExist(err) {
		t.Fatalf("docs metadata exists after a refused run: %v", err)
	}
}

func TestReconfigurePullAppliesNothingWhenALocalOverrideNeutralizesTheChange(t *testing.T) {
	_, root, remotes := seed(t)
	write(t, root, config.LocalFile, fmt.Sprintf(`repositories:
  public:
    remote: %s
`, remotes["public"]))
	elsewhere := mirror(t, remotes["public"], "public")
	target := publish(t, remotes["public"], "main", map[string]string{
		config.PublicFile: configuration(remotes["private"], elsewhere, ""),
	})

	output, err := incoming(t, reload(t, root), root, reconfigure.Source{Pull: true}, reconfigure.Accepted{})
	if err != nil {
		t.Fatalf("a neutralized change was refused: %v\noutput: %s", err, output)
	}
	if strings.Contains(output, "Accept this") {
		t.Fatalf("a neutralized change asked a question:\n%s", output)
	}
	if got := head(t, root, "public"); got != target {
		t.Fatalf("public head = %s, want the incoming %s", got, target)
	}
	if got := remoteURL(t, root, "public", config.OriginRemote); got != remotes["public"] {
		t.Fatalf("public origin = %q, want the unchanged %q", got, remotes["public"])
	}
}

func TestReconfigureSwitchRefusesAnUntrackedFileTheTargetWouldOverwrite(t *testing.T) {
	loaded, root, remotes := seed(t)
	publish(t, remotes["private"], "feature", map[string]string{"secrets/new.txt": "incoming\n"})
	managed(t, root, "private", "fetch", "--quiet", "origin", "refs/heads/feature:refs/heads/feature")
	managed(t, root, "public", "branch", "feature")
	write(t, root, "secrets/new.txt", "local\n")

	output, err := incoming(t, loaded, root, reconfigure.Source{Branch: "feature"}, reconfigure.Accepted{})
	if err == nil {
		t.Fatalf("an overwriting switch was accepted\noutput: %s", output)
	}
	wants(t, err.Error(), `repository "private"`, "cannot check out feature")
	if got := contents(t, root, "secrets/new.txt"); got != "local\n" {
		t.Fatalf("secrets/new.txt = %q, want the untouched local file", got)
	}
}

func TestReconfigurePullRefusesAMissingUpstreamBranch(t *testing.T) {
	loaded, root, remotes := seed(t)
	if _, err := git.Run(remotes["private"], "--git-dir="+remotes["private"],
		"update-ref", "-d", "refs/heads/main"); err != nil {
		t.Fatal(err)
	}
	managed(t, root, "private", "update-ref", "-d", "refs/remotes/origin/main")

	output, err := incoming(t, loaded, root, reconfigure.Source{Pull: true}, reconfigure.Accepted{})
	if err == nil {
		t.Fatalf("a missing upstream branch was accepted\noutput: %s", output)
	}
	wants(t, err.Error(), `repository "private"`, "has no origin/main to pull from")
}

func TestReconfigureSwitchRenamesAndMovesARepository(t *testing.T) {
	loaded, root, remotes := seed(t)
	renamed := strings.Replace(configuration(remotes["private"], remotes["public"], ""),
		"  private:\n", "  notes:\n", 1)
	publish(t, remotes["private"], "feature", map[string]string{
		"secrets/more.txt": "more\n",
		"notes.md":         "new\n",
	})
	publish(t, remotes["public"], "feature", map[string]string{config.PublicFile: renamed})
	for _, name := range []string{"private", "public"} {
		managed(t, root, name, "fetch", "--quiet", "origin", "refs/heads/feature:refs/heads/feature")
	}

	output, err := incoming(t, loaded, root, reconfigure.Source{Branch: "feature"},
		reconfigure.Accepted{Repository: true, Paths: true}, declaredRename(t, "private=notes"))
	if err != nil {
		t.Fatalf("reconfigure --switch with rename: %v\noutput: %s", err, output)
	}
	if exists(t, root, "private") || !exists(t, root, "notes") {
		t.Fatalf("repository metadata was not renamed\noutput: %s", output)
	}
	branch, err := git.Run(root, "--git-dir="+repository.Directory(root, "notes"), "symbolic-ref", "--short", "HEAD")
	if err != nil || strings.TrimSpace(branch) != "feature" {
		t.Fatalf("notes branch = %q (%v), want feature", strings.TrimSpace(branch), err)
	}
	if got := contents(t, root, "secrets/more.txt"); got != "more\n" {
		t.Fatalf("secrets/more.txt = %q, want the target content", got)
	}
	if got := contents(t, root, config.PublicFile); !strings.Contains(got, `- "notes.md"`) {
		t.Fatalf("%s does not assign notes.md to the renamed repository", config.PublicFile)
	}
}

func TestReconfigureSwitchValidatesARenamedTargetTree(t *testing.T) {
	loaded, root, remotes := seed(t)
	renamed := strings.Replace(configuration(remotes["private"], remotes["public"], ""),
		"  private:\n", "  notes:\n", 1)
	renamed = strings.Replace(renamed, "      - README.md\n",
		"      - README.md\n      - public-only/**\n", 1)
	publish(t, remotes["private"], "feature", map[string]string{"public-only/new.txt": "wrong repository\n"})
	publish(t, remotes["public"], "feature", map[string]string{config.PublicFile: renamed})
	for _, name := range []string{"private", "public"} {
		managed(t, root, name, "fetch", "--quiet", "origin", "refs/heads/feature:refs/heads/feature")
	}

	output, err := incoming(t, loaded, root, reconfigure.Source{Branch: "feature"},
		reconfigure.Accepted{Repository: true, Config: true}, declaredRename(t, "private=notes"))
	if err == nil {
		t.Fatalf("an unsafe renamed target tree was accepted\noutput: %s", output)
	}
	wants(t, err.Error(), `repository "notes"`, "public-only/new.txt", "owned by public")
	if !exists(t, root, "private") || exists(t, root, "notes") {
		t.Fatal("a refused renamed target moved repository metadata")
	}
}

func TestRecoverAcceptsTheConfigurationBeforeAnIncomingMove(t *testing.T) {
	_, root, remotes := seed(t)
	before := head(t, root, "public")
	start := []byte(contents(t, root, config.PublicFile))
	targetConfiguration := strings.Replace(string(start), "      - README.md\n",
		"      - README.md\n      - docs/**\n", 1)
	target := publish(t, remotes["public"], "main", map[string]string{config.PublicFile: targetConfiguration})
	if err := fetch.Update(root, "public"); err != nil {
		t.Fatal(err)
	}
	directory := lock.RecoveryPath(root)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := repository.SaveIndex(root, "public", filepath.Join(directory, "public.original")); err != nil {
		t.Fatal(err)
	}
	journal(t, root, map[string]any{
		"public_configuration": []byte(targetConfiguration), "start_configuration": start,
		"repositories": []map[string]any{{"name": "public", "branch": "main", "head": before,
			"remotes": []map[string]any{{"name": config.OriginRemote, "before": remotes["public"], "before_exists": true}}}},
		"moves": []map[string]any{{"name": "public", "from": "main", "head": before,
			"branch": "main", "target": target}},
	})

	var output bytes.Buffer
	if err := stage.Recover(reload(t, root), root, &output); err != nil {
		t.Fatalf("recover: %v\noutput: %s", err, output.String())
	}
	if got := head(t, root, "public"); got != target {
		t.Fatalf("public head = %s, want %s", got, target)
	}
}

func TestRecoverAcceptsAGeneratedConfigurationPatch(t *testing.T) {
	_, root, _ := seed(t)
	original := []byte(contents(t, root, config.PublicFile))
	patchedConfiguration, err := config.AddPaths(original, "public", []string{"docs/**"})
	if err != nil {
		t.Fatal(err)
	}
	write(t, root, config.PublicFile, string(patchedConfiguration))
	directory := lock.RecoveryPath(root)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "0.configuration.original"), original, 0o600); err != nil {
		t.Fatal(err)
	}
	file := assign.Patched{Path: config.PublicFile, Patterns: map[string][]string{"public": {"docs/**"}},
		OriginalHash: assign.Hash(original), TargetHash: assign.Hash(original), PatchedHash: assign.Hash(patchedConfiguration)}
	journal(t, root, map[string]any{
		"public_configuration": original, "start_configuration": original,
		"configuration": []assign.Patched{file},
	})

	var output bytes.Buffer
	if err := stage.Recover(reload(t, root), root, &output); err != nil {
		t.Fatalf("recover: %v\noutput: %s", err, output.String())
	}
	if got := contents(t, root, config.PublicFile); got != string(patchedConfiguration) {
		t.Fatalf("%s changed during recovery", config.PublicFile)
	}
}

func TestRecoverValidatesThePlannedMoveAgainstAChangedOrigin(t *testing.T) {
	_, root, remotes := seed(t)
	before := head(t, root, "public")
	newOrigin := mirror(t, remotes["public"], "public")
	start := []byte(contents(t, root, config.PublicFile))
	targetConfiguration := configuration(remotes["private"], newOrigin, "")
	target := publish(t, remotes["public"], "main", map[string]string{config.PublicFile: targetConfiguration})
	if err := fetch.Update(root, "public"); err != nil {
		t.Fatal(err)
	}
	directory := lock.RecoveryPath(root)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := repository.SaveIndex(root, "public", filepath.Join(directory, "public.original")); err != nil {
		t.Fatal(err)
	}
	journal(t, root, map[string]any{
		"public_configuration": []byte(targetConfiguration), "start_configuration": start,
		"repositories": []map[string]any{{"name": "public", "branch": "main", "head": before,
			"remotes": []map[string]any{{"name": config.OriginRemote, "before": remotes["public"],
				"before_exists": true, "after": newOrigin}}}},
		"moves": []map[string]any{{"name": "public", "from": "main", "head": before,
			"branch": "main", "target": target}},
	})

	var output bytes.Buffer
	err := stage.Recover(reload(t, root), root, &output)
	if err == nil {
		t.Fatalf("recover accepted a target absent from the changed origin\noutput: %s", output.String())
	}
	wants(t, err.Error(), shortCommit(target), "unrelated history")
	if got := head(t, root, "public"); got != before {
		t.Fatalf("public head = %s, want the unchanged %s", got, before)
	}
}

func shortCommit(commit string) string {
	if len(commit) <= 7 {
		return commit
	}
	return commit[:7]
}

// TestRecoverAndAbortResolveAnInterruptedIncomingMove drives both recovery
// commands from a journal recorded around the one write an incoming source
// adds to a reconfiguration: the fast-forward of an existing repository.
func TestRecoverAndAbortResolveAnInterruptedIncomingMove(t *testing.T) {
	for _, recovering := range []bool{false, true} {
		name := "abort"
		if recovering {
			name = "recover"
		}
		t.Run(name, func(t *testing.T) {
			_, root, remotes := seed(t)
			before := head(t, root, "public")
			target := publish(t, remotes["public"], "main", map[string]string{"README.md": "changed\n"})
			if err := fetch.Update(root, "public"); err != nil {
				t.Fatal(err)
			}
			directory := lock.RecoveryPath(root)
			if err := os.MkdirAll(directory, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := repository.SaveIndex(root, "public", filepath.Join(directory, "public.original")); err != nil {
				t.Fatal(err)
			}
			// The branch moved, but the process stopped before that progress
			// reached the journal.
			managed(t, root, "public", "merge", "--quiet", "--ff-only", target)
			journal(t, root, map[string]any{
				"repositories": []map[string]any{{"name": "public", "branch": "main", "head": before,
					"remotes": []map[string]any{{"name": config.OriginRemote,
						"before": remotes["public"], "before_exists": true}}}},
				"moves": []map[string]any{{"name": "public", "from": "main", "head": before,
					"branch": "main", "target": target, "started": true}},
			})

			resume, wanted, file := stage.Abort, before, "hello\n"
			if recovering {
				resume, wanted, file = stage.Recover, target, "changed\n"
			}
			var output bytes.Buffer
			if err := resume(reload(t, root), root, &output); err != nil {
				t.Fatalf("%s: %v\noutput: %s", name, err, output.String())
			}
			if got := head(t, root, "public"); got != wanted {
				t.Fatalf("public head = %s, want %s\noutput: %s", got, wanted, output.String())
			}
			if got := contents(t, root, "README.md"); got != file {
				t.Fatalf("README.md = %q, want %q", got, file)
			}
			if _, err := os.Stat(lock.RecoveryPath(root)); !os.IsNotExist(err) {
				t.Fatalf("recovery state was left behind: %v", err)
			}
			healthy(t, root)
		})
	}
}
