package reconfigure_test

import (
	"bytes"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/devidevio/gitone/internal/config"
	"github.com/devidevio/gitone/internal/git"
	"github.com/devidevio/gitone/internal/lock"
	"github.com/devidevio/gitone/internal/push"
	"github.com/devidevio/gitone/internal/reconfigure"
	"github.com/devidevio/gitone/internal/repository"
	"github.com/devidevio/gitone/internal/stage"
)

// seed is an initialized project whose two repositories share one commit with
// the bare repositories they were configured with. remotes holds those bare
// repositories by repository name.
func seed(t *testing.T) (*config.Config, string, map[string]string) {
	t.Helper()
	for _, name := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(name, "GitOne")
	}
	for _, name := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(name, "gitone@example.com")
	}

	root, origins := temporary(t), temporary(t)
	remotes := map[string]string{}
	for _, name := range []string{"private", "public"} {
		remotes[name] = bare(t, origins, name)
	}
	writeConfig(t, root, remotes["private"], remotes["public"])

	loaded, discovered, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.Init(loaded, discovered); err != nil {
		t.Fatal(err)
	}
	write(t, root, "README.md", "hello\n")
	write(t, root, "secrets/key.txt", "secret\n")

	var output bytes.Buffer
	if err := stage.Add(loaded, discovered, discovered, []string{"-A"}, &output); err != nil {
		t.Fatal(err)
	}
	if err := stage.Commit(loaded, discovered, discovered, nil, "Initial", &output); err != nil {
		t.Fatal(err)
	}
	for name := range remotes {
		if err := push.Push(loaded, discovered, name, true, false, nil, &output); err != nil {
			t.Fatalf("push %s: %v\noutput: %s", name, err, output.String())
		}
	}
	return loaded, discovered, remotes
}

func writeConfig(t *testing.T, root, private, public string) {
	t.Helper()
	write(t, root, config.PublicFile, fmt.Sprintf(`version: 1
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
`, private, public))
}

func bare(t *testing.T, directory, name string) string {
	t.Helper()
	full := filepath.Join(directory, name+".git")
	if _, err := git.Run(directory, "init", "--bare", "--quiet", "--initial-branch=main", full); err != nil {
		t.Fatal(err)
	}
	return full
}

// mirror is a second bare repository holding exactly the history of source,
// which is what a repository that moved to another host looks like.
func mirror(t *testing.T, source, name string) string {
	t.Helper()
	full := filepath.Join(temporary(t), name+".git")
	if _, err := git.Run(filepath.Dir(full), "clone", "--bare", "--quiet", source, full); err != nil {
		t.Fatal(err)
	}
	return full
}

// unrelated is a bare repository with a main branch of its own history, which
// is a different project rather than a moved one.
func unrelated(t *testing.T, name string) string {
	t.Helper()
	directory := temporary(t)
	full := bare(t, directory, name)
	work := temporary(t)
	if _, err := git.Run(work, "clone", "--quiet", full, work); err != nil {
		t.Fatal(err)
	}
	write(t, work, "OTHER.md", "other\n")
	for _, arguments := range [][]string{{"add", "OTHER.md"}, {"commit", "--quiet", "-m", "Other"},
		{"push", "--quiet", "origin", "HEAD:refs/heads/main"}} {
		if _, err := git.Run(work, arguments...); err != nil {
			t.Fatal(err)
		}
	}
	return full
}

// reload is the effective configuration after a configuration file changed.
func reload(t *testing.T, root string) *config.Config {
	t.Helper()
	loaded, _, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	return loaded
}

// run reconfigures without a terminal, which is what the flag alone accepts.
func run(t *testing.T, configuration *config.Config, root string, accepted bool,
	renames ...reconfigure.Rename) (string, error) {
	t.Helper()
	var output bytes.Buffer
	err := reconfigure.Reconfigure(configuration, root, reconfigure.Source{}, renames,
		reconfigure.Accepted{Repository: accepted}, false, strings.NewReader(""), &output)
	return output.String(), err
}

// answered reconfigures with a terminal answering the one question.
func answered(configuration *config.Config, root, answer string, renames ...reconfigure.Rename) (string, error) {
	var output bytes.Buffer
	err := reconfigure.Reconfigure(configuration, root, reconfigure.Source{}, renames,
		reconfigure.Accepted{}, true, strings.NewReader(answer), &output)
	return output.String(), err
}

func remoteURL(t *testing.T, root, name, remote string) string {
	t.Helper()
	url, err := git.Run(root, "--git-dir="+repository.Directory(root, name), "config", "remote."+remote+".url")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(url)
}

func hasRemote(t *testing.T, root, name, remote string) bool {
	t.Helper()
	remotes, err := git.Run(root, "--git-dir="+repository.Directory(root, name), "remote")
	if err != nil {
		t.Fatal(err)
	}
	for _, existing := range strings.Fields(remotes) {
		if existing == remote {
			return true
		}
	}
	return false
}

func head(t *testing.T, root, name string) string {
	t.Helper()
	commit, err := repository.Reference(root, name, "refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}
	return commit
}

func temporary(t *testing.T) string {
	t.Helper()
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return directory
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

func wants(t *testing.T, got string, expected ...string) {
	t.Helper()
	for _, want := range expected {
		if !strings.Contains(got, want) {
			t.Fatalf("got %q, want %q", got, want)
		}
	}
}

func TestReconfigureWithMatchingRemotesChangesNothing(t *testing.T) {
	configuration, root, remotes := seed(t)

	output, err := run(t, configuration, root, false)
	if err != nil {
		t.Fatalf("reconfigure: %v\noutput: %s", err, output)
	}
	wants(t, output, "Nothing to reconcile")
	if strings.Contains(output, "Accept this") {
		t.Fatalf("a matching project asked a question:\n%s", output)
	}
	if got := remoteURL(t, root, "public", config.OriginRemote); got != remotes["public"] {
		t.Fatalf("public origin = %q, want the unchanged %q", got, remotes["public"])
	}
}

func TestReconfigureAppliesAChangedOriginAfterOneConfirmation(t *testing.T) {
	_, root, remotes := seed(t)
	moved := mirror(t, remotes["public"], "public")
	writeConfig(t, root, remotes["private"], moved)

	output, err := answered(reload(t, root), root, "y\n")
	if err != nil {
		t.Fatalf("reconfigure: %v\noutput: %s", err, output)
	}
	wants(t, output, "REMOTES", "public / origin",
		"before: "+remotes["public"], "after:  "+moved,
		"this repository owns "+config.PublicFile,
		"NETWORK", "public will be fetched from "+moved,
		"Accept this repository reconfiguration? [y/N]")
	if got := remoteURL(t, root, "public", config.OriginRemote); got != moved {
		t.Fatalf("public origin = %q, want %q", got, moved)
	}
	if issues := repository.Check(root, "public", reload(t, root).Repositories["public"]); len(issues) != 0 {
		t.Fatalf("public still has issues after the reconfiguration: %v", issues)
	}
	if _, err := os.Stat(lock.RecoveryPath(root)); !os.IsNotExist(err) {
		t.Fatalf("recovery state was left behind: %v", err)
	}
}

func TestReconfigureAddsAConfiguredRemoteNativeGitDoesNotHave(t *testing.T) {
	_, root, remotes := seed(t)
	backup := mirror(t, remotes["public"], "backup")
	contents, err := os.ReadFile(filepath.Join(root, config.PublicFile))
	if err != nil {
		t.Fatal(err)
	}
	write(t, root, config.PublicFile, strings.Replace(string(contents),
		"    remote: "+remotes["public"]+"\n",
		"    remotes:\n      origin: "+remotes["public"]+"\n      backup: "+backup+"\n", 1))

	output, err := run(t, reload(t, root), root, true)
	if err != nil {
		t.Fatalf("reconfigure: %v\noutput: %s", err, output)
	}
	wants(t, output, "public / backup", "before: (absent)", "after:  "+backup)
	if got := remoteURL(t, root, "public", "backup"); got != backup {
		t.Fatalf("public backup = %q, want %q", got, backup)
	}
}

func TestReconfigureLeavesEverythingUnchangedWhenDeclinedOrNonInteractive(t *testing.T) {
	_, root, remotes := seed(t)
	moved := mirror(t, remotes["public"], "public")
	writeConfig(t, root, remotes["private"], moved)
	projected := reload(t, root)
	before := head(t, root, "public")

	declined, err := answered(projected, root, "n\n")
	if err != nil {
		t.Fatalf("reconfigure: %v\noutput: %s", err, declined)
	}
	wants(t, declined, "Reconfiguration aborted.", "No repository was created or renamed and no configured remote, branch, HEAD, index or working-tree file was changed.")

	refused, err := run(t, projected, root, false)
	if err == nil || !strings.Contains(err.Error(), reconfigure.AcceptFlag) {
		t.Fatalf("error = %v, want the required flag\noutput: %s", err, refused)
	}
	if !strings.HasPrefix(err.Error(), "RECONF001") {
		t.Fatalf("error = %v, want RECONF001", err)
	}
	if got := remoteURL(t, root, "public", config.OriginRemote); got != remotes["public"] {
		t.Fatalf("public origin = %q, want the unchanged %q", got, remotes["public"])
	}
	if got := head(t, root, "public"); got != before {
		t.Fatalf("public head = %q, want the unchanged %q", got, before)
	}
}

func TestReconfigureFlagIsHarmlessWithoutADifference(t *testing.T) {
	configuration, root, _ := seed(t)

	output, err := run(t, configuration, root, true)
	if err != nil {
		t.Fatalf("reconfigure: %v\noutput: %s", err, output)
	}
	wants(t, output, "Nothing to reconcile")
}

func TestReconfigureRefusesAPasswordInAConfiguredURLWithAndWithoutTheFlag(t *testing.T) {
	_, root, remotes := seed(t)
	writeConfig(t, root, remotes["private"], "https://user:secret@git.example.com/example/public.git")
	projected := reload(t, root)

	for _, accepted := range []bool{false, true} {
		output, err := run(t, projected, root, accepted)
		if err == nil {
			t.Fatalf("accepted = %v: reconfigure succeeded\noutput: %s", accepted, output)
		}
		wants(t, err.Error(), "RECONF001", `repository "public"`, `remote "origin"`,
			"password", config.PublicFile)
	}
	if got := remoteURL(t, root, "public", config.OriginRemote); got != remotes["public"] {
		t.Fatalf("public origin = %q, want the unchanged %q", got, remotes["public"])
	}
}

func TestReconfigureReportsARemoteTheConfigurationDoesNotName(t *testing.T) {
	configuration, root, _ := seed(t)
	if _, err := git.Run(root, "--git-dir="+repository.Directory(root, "public"),
		"remote", "add", "mirror", "https://git.example.com/example/mirror.git"); err != nil {
		t.Fatal(err)
	}

	output, err := run(t, configuration, root, false)
	if err != nil {
		t.Fatalf("reconfigure: %v\noutput: %s", err, output)
	}
	wants(t, output, "public / mirror", "left in place", "No GitOne command uses it",
		"git --git-dir="+repository.Directory(root, "public")+" remote remove mirror")
	if got := remoteURL(t, root, "public", "mirror"); got == "" {
		t.Fatal("the unconfigured remote was removed")
	}
}

func TestReconfigureRefusesAnUnrelatedHistoryAndRestoresTheURLs(t *testing.T) {
	_, root, remotes := seed(t)
	other := unrelated(t, "public")
	writeConfig(t, root, remotes["private"], other)
	before := head(t, root, "public")

	output, err := run(t, reload(t, root), root, true)
	if err == nil {
		t.Fatalf("reconfigure succeeded on an unrelated history\noutput: %s", output)
	}
	wants(t, err.Error(), "RECONF001", `repository "public"`, "unrelated history")
	if got := remoteURL(t, root, "public", config.OriginRemote); got != remotes["public"] {
		t.Fatalf("public origin = %q, want the restored %q", got, remotes["public"])
	}
	if got := head(t, root, "public"); got != before {
		t.Fatalf("public head = %q, want the unchanged %q", got, before)
	}
	if _, err := os.Stat(lock.RecoveryPath(root)); !os.IsNotExist(err) {
		t.Fatalf("recovery state was left behind: %v", err)
	}
}

func TestReconfigureRestoresARemoteThatStartedWithoutAURL(t *testing.T) {
	_, root, remotes := seed(t)
	gitDirectory := "--git-dir=" + repository.Directory(root, "public")
	if _, err := git.Run(root, gitDirectory, "config", "--unset-all", "remote.origin.url"); err != nil {
		t.Fatal(err)
	}
	writeConfig(t, root, remotes["private"], unrelated(t, "public"))

	output, err := run(t, reload(t, root), root, true)
	if err == nil {
		t.Fatalf("reconfigure succeeded on an unrelated history\noutput: %s", output)
	}
	if !hasRemote(t, root, "public", config.OriginRemote) {
		t.Fatal("rollback removed the remote that started without a URL")
	}
	if got := remoteURL(t, root, "public", config.OriginRemote); got != "" {
		t.Fatalf("public origin = %q, want no URL", got)
	}
}

func TestReconfigureRollbackKeepsFetchedTrackingRefsForANewRemote(t *testing.T) {
	_, root, remotes := seed(t)
	gitDirectory := "--git-dir=" + repository.Directory(root, "public")
	if _, err := git.Run(root, gitDirectory, "remote", "remove", config.OriginRemote); err != nil {
		t.Fatal(err)
	}
	writeConfig(t, root, remotes["private"], unrelated(t, "public"))

	output, err := run(t, reload(t, root), root, true)
	if err == nil {
		t.Fatalf("reconfigure succeeded on an unrelated history\noutput: %s", output)
	}
	if hasRemote(t, root, "public", config.OriginRemote) {
		t.Fatal("rollback left the newly configured remote in place")
	}
	tracking, err := repository.Reference(root, "public", "refs/remotes/origin/main")
	if err != nil {
		t.Fatal(err)
	}
	if tracking == "" {
		t.Fatal("rollback removed the tracking ref fetched from the rejected remote")
	}
}

func TestReconfigureAcceptsANewOriginWithoutTheCurrentBranch(t *testing.T) {
	configuration, root, remotes := seed(t)
	write(t, root, "README.md", "local work\n")
	var output bytes.Buffer
	if err := stage.Add(configuration, root, root, []string{"README.md"}, &output); err != nil {
		t.Fatal(err)
	}
	if err := stage.Commit(configuration, root, root, nil, "Local work", &output); err != nil {
		t.Fatal(err)
	}
	emptyOrigin := bare(t, temporary(t), "empty")
	writeConfig(t, root, remotes["private"], emptyOrigin)

	result, err := run(t, reload(t, root), root, true)
	if err != nil {
		t.Fatalf("reconfigure: %v\noutput: %s", err, result)
	}
	if got := remoteURL(t, root, "public", config.OriginRemote); got != emptyOrigin {
		t.Fatalf("public origin = %q, want %q", got, emptyOrigin)
	}
}

func TestReconfigureRefusesAnUnreachableRemoteAndRestoresTheURLs(t *testing.T) {
	_, root, remotes := seed(t)
	missing := filepath.Join(temporary(t), "gone.git")
	writeConfig(t, root, remotes["private"], missing)

	output, err := run(t, reload(t, root), root, true)
	if err == nil {
		t.Fatalf("reconfigure succeeded against a missing remote\noutput: %s", output)
	}
	wants(t, err.Error(), "RECONF001", "could not be fetched")
	if got := remoteURL(t, root, "public", config.OriginRemote); got != remotes["public"] {
		t.Fatalf("public origin = %q, want the restored %q", got, remotes["public"])
	}
}

// recorded is the journal a reconfiguration would write, produced by hand so
// recover and abort can be driven from a known interruption point.
func recorded(t *testing.T, root string, entries ...map[string]any) {
	t.Helper()
	journal(t, root, map[string]any{"repositories": entries})
}

// journal writes one recorded reconfiguration, with extra holding whatever
// the interruption point needs beyond the shared envelope.
func journal(t *testing.T, root string, extra map[string]any) {
	t.Helper()
	directory := lock.RecoveryPath(root)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	public, err := os.ReadFile(filepath.Join(root, config.PublicFile))
	if err != nil {
		t.Fatal(err)
	}
	state := map[string]any{
		"version": 4, "command": reconfigure.Command, "public_configuration": public,
	}
	maps.Copy(state, extra)
	local, err := os.ReadFile(filepath.Join(root, config.LocalFile))
	if err == nil {
		state["local_configuration"] = local
		state["local_exists"] = true
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := lock.WriteState(directory, state); err != nil {
		t.Fatal(err)
	}
}

func entry(t *testing.T, root, name string, remotes ...map[string]string) map[string]any {
	t.Helper()
	recordedRemotes := make([]map[string]string, 0, len(remotes))
	recordedRemotes = append(recordedRemotes, remotes...)
	return map[string]any{"name": name, "branch": "main", "head": head(t, root, name), "remotes": recordedRemotes}
}

func TestRecoverFinishesAndAbortUndoesARecordedReconfiguration(t *testing.T) {
	for _, recovering := range []bool{true, false} {
		t.Run(fmt.Sprint("recovering=", recovering), func(t *testing.T) {
			configuration, root, remotes := seed(t)
			moved := mirror(t, remotes["public"], "public")
			recorded(t, root, entry(t, root, "public",
				map[string]string{"name": config.OriginRemote, "before": remotes["public"], "after": moved}))

			var output bytes.Buffer
			resume := stage.Abort
			if recovering {
				resume = stage.Recover
			}
			if err := resume(configuration, root, &output); err != nil {
				t.Fatalf("resume: %v\noutput: %s", err, output.String())
			}
			want := remotes["public"]
			if recovering {
				want = moved
			}
			if got := remoteURL(t, root, "public", config.OriginRemote); got != want {
				t.Fatalf("public origin = %q, want %q", got, want)
			}
			if _, err := os.Stat(lock.RecoveryPath(root)); !os.IsNotExist(err) {
				t.Fatalf("recovery state was left behind: %v", err)
			}
		})
	}
}

func TestRecoverAndAbortRefuseARepositoryChangedOutsideGitOne(t *testing.T) {
	for _, recovering := range []bool{true, false} {
		t.Run(fmt.Sprint("recovering=", recovering), func(t *testing.T) {
			configuration, root, remotes := seed(t)
			moved := mirror(t, remotes["public"], "public")
			interrupted := head(t, root, "public")

			// One commit made between the interruption and the resumed
			// command moves the recorded branch.
			write(t, root, "README.md", "changed\n")
			var staged bytes.Buffer
			if err := stage.Add(configuration, root, root, []string{"README.md"}, &staged); err != nil {
				t.Fatal(err)
			}
			if _, err := git.Run(root, "--git-dir="+repository.Directory(root, "public"), "--work-tree="+root,
				"commit", "--quiet", "-m", "Outside"); err != nil {
				t.Fatal(err)
			}
			recorded(t, root, map[string]any{"name": "public", "branch": "main", "head": interrupted,
				"remotes": []map[string]string{{"name": config.OriginRemote, "before": remotes["public"], "after": moved}}})

			var output bytes.Buffer
			resume := stage.Abort
			if recovering {
				resume = stage.Recover
			}
			err := resume(configuration, root, &output)
			if err == nil {
				t.Fatalf("resume accepted a repository changed outside GitOne\noutput: %s", output.String())
			}
			wants(t, err.Error(), "REC001", `repository "public"`, "changed outside GitOne")
			if got := remoteURL(t, root, "public", config.OriginRemote); got != remotes["public"] {
				t.Fatalf("public origin = %q, want the untouched %q", got, remotes["public"])
			}
			if _, err := os.Stat(lock.RecoveryPath(root)); err != nil {
				t.Fatalf("the recorded state was removed by a refusal: %v", err)
			}
		})
	}
}

func TestReconfigureRefusesWhileRecoveryStateExists(t *testing.T) {
	configuration, root, _ := seed(t)
	recorded(t, root, entry(t, root, "public"))

	output, err := run(t, configuration, root, true)
	if err == nil || !strings.HasPrefix(err.Error(), "REC001") {
		t.Fatalf("error = %v, want REC001\noutput: %s", err, output)
	}
}

func TestReconfigureRestoresEveryURLWhenAFailureFollowsTheFirstChange(t *testing.T) {
	_, root, remotes := seed(t)
	// The first repository moves to a valid mirror and the second one to an
	// unrelated history, so the failure comes after a remote already changed.
	movedPrivate := mirror(t, remotes["private"], "private")
	writeConfig(t, root, movedPrivate, unrelated(t, "public"))

	output, err := run(t, reload(t, root), root, true)
	if err == nil {
		t.Fatalf("reconfigure succeeded on an unrelated history\noutput: %s", output)
	}
	for name, want := range remotes {
		if got := remoteURL(t, root, name, config.OriginRemote); got != want {
			t.Fatalf("%s origin = %q, want the restored %q", name, got, want)
		}
	}
	if _, err := os.Stat(lock.RecoveryPath(root)); !os.IsNotExist(err) {
		t.Fatalf("recovery state was left behind: %v", err)
	}
}
