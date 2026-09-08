package reconfigure_test

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
	"github.com/devidevio/gitone/internal/status"
)

// carrying is a bare repository holding one commit on branch with the given
// files, which is what a repository somebody else already published looks
// like.
func carrying(t *testing.T, name, branch string, files map[string]string) string {
	t.Helper()
	full := filepath.Join(temporary(t), name+".git")
	if _, err := git.Run(filepath.Dir(full), "init", "--bare", "--quiet", "--initial-branch="+branch, full); err != nil {
		t.Fatal(err)
	}
	work := temporary(t)
	if _, err := git.Run(work, "clone", "--quiet", full, work); err != nil {
		t.Fatal(err)
	}
	for path, contents := range files {
		write(t, work, path, contents)
	}
	for _, arguments := range [][]string{{"add", "-A"}, {"commit", "--quiet", "-m", "Seed"},
		{"push", "--quiet", "origin", "HEAD:refs/heads/" + branch}} {
		if _, err := git.Run(work, arguments...); err != nil {
			t.Fatal(err)
		}
	}
	return full
}

// declare appends one repository to a configuration file, which is the edit a
// person makes before running gitone reconfigure.
func declare(t *testing.T, root, file, name, remote string, paths ...string) {
	t.Helper()
	block := "  " + name + ":\n"
	if remote != "" {
		block += "    remote: " + remote + "\n"
	}
	block += "    visibility: public\n    paths:\n"
	for _, pattern := range paths {
		block += "      - " + pattern + "\n"
	}

	full := filepath.Join(root, file)
	contents, err := os.ReadFile(full)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(contents) == 0 {
		contents = []byte("repositories:\n")
	}
	write(t, root, file, string(contents)+block)
}

// onBranch moves every existing repository of the project to branch, which is
// the state gitone switch leaves behind.
func onBranch(t *testing.T, root, branch string, names ...string) {
	t.Helper()
	for _, name := range names {
		gitDirectory := "--git-dir=" + repository.Directory(root, name)
		if _, err := git.Run(root, gitDirectory, "update-ref", "refs/heads/"+branch, head(t, root, name)); err != nil {
			t.Fatal(err)
		}
		if _, err := git.Run(root, gitDirectory, "symbolic-ref", "HEAD", "refs/heads/"+branch); err != nil {
			t.Fatal(err)
		}
	}
}

func exists(t *testing.T, root, name string) bool {
	t.Helper()
	_, err := os.Stat(repository.Directory(root, name))
	return err == nil
}

func reference(t *testing.T, root, name, ref string) string {
	t.Helper()
	commit, err := repository.Reference(root, name, ref)
	if err != nil {
		t.Fatal(err)
	}
	return commit
}

func contents(t *testing.T, root, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// healthy is what gitone status and gitone doctor report about the project:
// no repository issue and no working-tree issue at all.
func healthy(t *testing.T, root string) {
	t.Helper()
	configuration := reload(t, root)
	if _, _, err := status.Validate(configuration, root); err != nil {
		t.Fatalf("the reconfigured project does not validate: %v", err)
	}
	for _, name := range configuration.RepositoryNames() {
		if issues := repository.Check(root, name, configuration.Repositories[name]); len(issues) != 0 {
			t.Fatalf("repository %q still has issues: %v", name, issues)
		}
	}
}

func TestReconfigureCreatesFetchesAndChecksOutAnAddedRepository(t *testing.T) {
	_, root, _ := seed(t)
	docs := carrying(t, "docs", "main", map[string]string{"docs/guide.md": "guide\n"})
	declare(t, root, config.PublicFile, "docs", docs, "docs/**")

	output, err := answered(reload(t, root), root, "y\n")
	if err != nil {
		t.Fatalf("reconfigure: %v\noutput: %s", err, output)
	}
	wants(t, output, "REPOSITORIES", "+ docs   public   "+docs,
		"paths: docs/**   (claims 0 existing local paths)",
		"NETWORK", "docs will be fetched from "+docs,
		"Accept this repository reconfiguration? [y/N]",
		"created and checked out main at")
	if !exists(t, root, "docs") {
		t.Fatal("the added repository was not created")
	}
	if got := head(t, root, "docs"); got == "" || got != reference(t, root, "docs", "refs/remotes/origin/main") {
		t.Fatalf("docs main = %q, want the fetched origin/main", got)
	}
	if got := contents(t, root, "docs/guide.md"); got != "guide\n" {
		t.Fatalf("docs/guide.md = %q, want the checked out content", got)
	}
	tracks, err := repository.TracksOriginBranch(root, "docs", "main")
	if err != nil || !tracks {
		t.Fatalf("docs does not track origin/main: %v", err)
	}
	healthy(t, root)
	if _, err := os.Stat(lock.RecoveryPath(root)); !os.IsNotExist(err) {
		t.Fatalf("recovery state was left behind: %v", err)
	}
}

func TestReconfigureRefusesAnAdditionThatWouldClaimAnExistingLocalPath(t *testing.T) {
	_, root, _ := seed(t)
	docs := carrying(t, "docs", "main", map[string]string{"docs/guide.md": "guide\n"})
	write(t, root, "docs/local.md", "local\n")
	declare(t, root, config.PublicFile, "docs", docs, "docs/**")

	output, err := run(t, reload(t, root), root, true)
	if err == nil {
		t.Fatalf("reconfigure exposed an existing local path\noutput: %s", output)
	}
	wants(t, output, "claims 1 existing local path")
	wants(t, err.Error(), "RECONF001",
		"would put existing local ignored or unassigned paths into a repository", "docs/local.md")
	if got := contents(t, root, "docs/local.md"); got != "local\n" {
		t.Fatalf("docs/local.md = %q, want the untouched local file", got)
	}
	if exists(t, root, "docs") {
		t.Fatal("the refused repository was left behind")
	}
}

func TestReconfigureCreatesAnAdditionOnTheBranchTheProjectIsOn(t *testing.T) {
	_, root, _ := seed(t)
	onBranch(t, root, "feature", "private", "public")
	docs := carrying(t, "docs", "feature", map[string]string{"docs/guide.md": "guide\n"})
	declare(t, root, config.PublicFile, "docs", docs, "docs/**")

	output, err := run(t, reload(t, root), root, true)
	if err != nil {
		t.Fatalf("reconfigure: %v\noutput: %s", err, output)
	}
	branch, err := git.Run(root, "--git-dir="+repository.Directory(root, "docs"), "symbolic-ref", "--short", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(branch) != "feature" {
		t.Fatalf("docs is on %q, want the project branch feature", strings.TrimSpace(branch))
	}
	if reference(t, root, "docs", "refs/heads/feature") == "" {
		t.Fatal("docs was not checked out on feature")
	}
	healthy(t, root)
}

func TestReconfigureLeavesAnAdditionWithoutAnOriginUnborn(t *testing.T) {
	_, root, _ := seed(t)
	declare(t, root, config.PublicFile, "docs", "", "docs/**")

	output, err := run(t, reload(t, root), root, true)
	if err != nil {
		t.Fatalf("reconfigure: %v\noutput: %s", err, output)
	}
	wants(t, output, "no configured origin remote", "created, unborn main",
		`Repository "docs" has no commit yet: no configured origin remote.`,
		`https://gitone.io/docs/usage#adopting-history-into-an-empty-repository`)
	if !exists(t, root, "docs") {
		t.Fatal("the unborn repository was not created")
	}
	if got := reference(t, root, "docs", "refs/heads/main"); got != "" {
		t.Fatalf("docs main = %q, want an unborn branch", got)
	}
	healthy(t, root)
}

func TestReconfigureRefusesAnOriginWithoutTheProjectBranch(t *testing.T) {
	_, root, _ := seed(t)
	elsewhere := carrying(t, "docs", "other", map[string]string{"docs/guide.md": "guide\n"})
	declare(t, root, config.PublicFile, "docs", elsewhere, "docs/**")

	output, err := run(t, reload(t, root), root, true)
	if err == nil {
		t.Fatalf("reconfigure accepted an origin without the project branch\noutput: %s", output)
	}
	wants(t, err.Error(), "RECONF001", `repository "docs"`, "origin/main", elsewhere, "the project is on main")
	if exists(t, root, "docs") {
		t.Fatal("the refused repository was left behind")
	}
	if _, err := os.Stat(lock.RecoveryPath(root)); !os.IsNotExist(err) {
		t.Fatalf("recovery state was left behind: %v", err)
	}
}

func TestReconfigureRefusesAnAdditionThatWouldOverwriteAWorkingTreeFile(t *testing.T) {
	_, root, _ := seed(t)
	docs := carrying(t, "docs", "main", map[string]string{"docs/guide.md": "incoming\n"})
	write(t, root, "docs/guide.md", "mine\n")
	declare(t, root, config.PublicFile, "docs", docs, "docs/**")

	output, err := run(t, reload(t, root), root, true)
	if err == nil {
		t.Fatalf("reconfigure overwrote an existing working-tree file\noutput: %s", output)
	}
	wants(t, err.Error(), "RECONF001", "would overwrite existing working-tree files", "docs/guide.md")
	if got := contents(t, root, "docs/guide.md"); got != "mine\n" {
		t.Fatalf("docs/guide.md = %q, want the untouched local file", got)
	}
	if exists(t, root, "docs") {
		t.Fatal("the refused repository was left behind")
	}
}

// An existing local path that no repository manages today and that the
// addition would track becomes publishable the moment the change applies.
// That is the exposure task 0052 refuses, with and without every flag.
func TestReconfigureRefusesAnAdditionThatWouldManageAnIgnoredLocalPath(t *testing.T) {
	for _, accepted := range []bool{true, false} {
		t.Run(fmt.Sprint("accepted=", accepted), func(t *testing.T) {
			_, root, _ := seed(t)
			docs := carrying(t, "docs", "main", map[string]string{"docs/private/notes.md": "incoming\n"})
			write(t, root, config.ProjectIgnoreFile, ".gitone/\n.gitone.local.yml\ndocs/private/\n")
			write(t, root, "docs/private/notes.md", "mine\n")
			declare(t, root, config.PublicFile, "docs", docs, "docs/**")

			var output string
			var err error
			if accepted {
				output, err = run(t, reload(t, root), root, true)
			} else {
				output, err = answered(reload(t, root), root, "y\n")
			}
			if err == nil {
				t.Fatalf("reconfigure exposed an ignored local path\noutput: %s", output)
			}
			wants(t, err.Error(), "RECONF001",
				"would put existing local ignored or unassigned paths into a repository",
				"docs/private/notes.md", "No flag ever accepts this.")
			if got := contents(t, root, "docs/private/notes.md"); got != "mine\n" {
				t.Fatalf("docs/private/notes.md = %q, want the untouched local file", got)
			}
			if exists(t, root, "docs") {
				t.Fatal("the refused repository was left behind")
			}
		})
	}
}

func TestReconfigureCreatesARepositoryDefinedOnlyInTheLocalFile(t *testing.T) {
	_, root, _ := seed(t)
	notes := carrying(t, "notes", "main", map[string]string{"notes/todo.md": "todo\n"})
	declare(t, root, config.LocalFile, "notes", notes, "notes/**")

	output, err := run(t, reload(t, root), root, true)
	if err != nil {
		t.Fatalf("reconfigure: %v\noutput: %s", err, output)
	}
	if !exists(t, root, "notes") {
		t.Fatal("the locally defined repository was not created")
	}
	if got := contents(t, root, "notes/todo.md"); got != "todo\n" {
		t.Fatalf("notes/todo.md = %q, want the checked out content", got)
	}
	healthy(t, root)
}

func TestReconfigureDecliningAnAdditionLeavesNothingBehind(t *testing.T) {
	_, root, _ := seed(t)
	docs := carrying(t, "docs", "main", map[string]string{"docs/guide.md": "guide\n"})
	declare(t, root, config.PublicFile, "docs", docs, "docs/**")

	output, err := answered(reload(t, root), root, "n\n")
	if err != nil {
		t.Fatalf("declining failed: %v\noutput: %s", err, output)
	}
	wants(t, output, "Reconfiguration aborted.")
	if exists(t, root, "docs") {
		t.Fatal("a declined addition created the repository")
	}
	if _, err := os.Stat(filepath.Join(root, "docs")); !os.IsNotExist(err) {
		t.Fatalf("a declined addition wrote working-tree files: %v", err)
	}
	if _, err := os.Stat(lock.RecoveryPath(root)); !os.IsNotExist(err) {
		t.Fatalf("recovery state was left behind: %v", err)
	}
}

func TestReconfigureRemovesEveryDirectoryItCreatedWhenALaterAdditionFails(t *testing.T) {
	_, root, _ := seed(t)
	good := carrying(t, "docs", "main", map[string]string{"docs/guide.md": "guide\n"})
	bad := carrying(t, "notes", "other", map[string]string{"notes/todo.md": "todo\n"})
	declare(t, root, config.PublicFile, "docs", good, "docs/**")
	declare(t, root, config.PublicFile, "notes", bad, "notes/**")

	output, err := run(t, reload(t, root), root, true)
	if err == nil {
		t.Fatalf("reconfigure accepted an origin without the project branch\noutput: %s", output)
	}
	for _, name := range []string{"docs", "notes"} {
		if exists(t, root, name) {
			t.Fatalf("repository %q was left behind after the failure", name)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "docs")); !os.IsNotExist(err) {
		t.Fatalf("the rolled back addition left working-tree files: %v", err)
	}
	if _, err := os.Stat(lock.RecoveryPath(root)); !os.IsNotExist(err) {
		t.Fatalf("recovery state was left behind: %v", err)
	}
}

// created is the journal entry of a repository the interrupted reconfiguration
// was adding.
func created(t *testing.T, root, name, origin string, refs []string) map[string]any {
	t.Helper()
	return map[string]any{"name": name, "branch": "main", "head": "", "created": true, "refs": refs,
		"remotes": []map[string]string{{"name": config.OriginRemote, "after": origin}}}
}

func recordedRefs(t *testing.T, root, name string) []string {
	t.Helper()
	output, err := git.Run(root, "--git-dir="+repository.Directory(root, name),
		"for-each-ref", "--format=%(refname) %(objectname)")
	if err != nil {
		t.Fatal(err)
	}
	var listed []string
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if line != "" {
			listed = append(listed, line)
		}
	}
	return listed
}

func TestRecoverFinishesAnInterruptedAddition(t *testing.T) {
	_, root, _ := seed(t)
	docs := carrying(t, "docs", "main", map[string]string{"docs/guide.md": "guide\n"})
	declare(t, root, config.PublicFile, "docs", docs, "docs/**")
	// The interruption happened before the directory was created, which is
	// the earliest point the journal already exists.
	recorded(t, root, created(t, root, "docs", docs, nil))

	var output bytes.Buffer
	if err := stage.Recover(reload(t, root), root, &output); err != nil {
		t.Fatalf("recover: %v\noutput: %s", err, output.String())
	}
	if !exists(t, root, "docs") {
		t.Fatal("recover did not create the recorded repository")
	}
	if got := contents(t, root, "docs/guide.md"); got != "guide\n" {
		t.Fatalf("docs/guide.md = %q, want the checked out content", got)
	}
	healthy(t, root)
	if _, err := os.Stat(lock.RecoveryPath(root)); !os.IsNotExist(err) {
		t.Fatalf("recovery state was left behind: %v", err)
	}
}

func TestRecoverRefusesAConfigurationChangedAfterTheInterruption(t *testing.T) {
	_, root, _ := seed(t)
	docs := carrying(t, "docs", "main", map[string]string{"docs/guide.md": "guide\n"})
	declare(t, root, config.PublicFile, "docs", docs, "docs/**")
	recorded(t, root, created(t, root, "docs", docs, nil))

	configured, err := os.ReadFile(filepath.Join(root, config.PublicFile))
	if err != nil {
		t.Fatal(err)
	}
	changed := filepath.Join(temporary(t), "changed.git")
	write(t, root, config.PublicFile, strings.Replace(string(configured), docs, changed, 1))

	var output bytes.Buffer
	err = stage.Recover(reload(t, root), root, &output)
	if err == nil {
		t.Fatalf("recover accepted a changed configuration\noutput: %s", output.String())
	}
	wants(t, err.Error(), "REC001", config.PublicFile, "changed outside GitOne")
	if exists(t, root, "docs") {
		t.Fatal("recover created a repository from the changed configuration")
	}
}

func TestRecoverRefusesChangedAdditionFilesAndIndex(t *testing.T) {
	for _, change := range []string{"working tree", "index"} {
		t.Run(change, func(t *testing.T) {
			_, root, _ := seed(t)
			docs := carrying(t, "docs", "main", map[string]string{"docs/guide.md": "guide\n"})
			declare(t, root, config.PublicFile, "docs", docs, "docs/**")
			if output, err := run(t, reload(t, root), root, true); err != nil {
				t.Fatalf("reconfigure: %v\noutput: %s", err, output)
			}
			recorded(t, root, created(t, root, "docs", docs, recordedRefs(t, root, "docs")))

			write(t, root, "docs/guide.md", "mine\n")
			if change == "index" {
				if _, err := git.Run(root, "--git-dir="+repository.Directory(root, "docs"),
					"--work-tree="+root, "add", "--", "docs/guide.md"); err != nil {
					t.Fatal(err)
				}
				write(t, root, "docs/guide.md", "guide\n")
			}

			var output bytes.Buffer
			err := stage.Recover(reload(t, root), root, &output)
			if err == nil {
				t.Fatalf("recover overwrote the changed %s\noutput: %s", change, output.String())
			}
			wants(t, err.Error(), "REC001", "index or tracked working-tree files differ")
			want := "mine\n"
			if change == "index" {
				want = "guide\n"
			}
			if got := contents(t, root, "docs/guide.md"); got != want {
				t.Fatalf("docs/guide.md = %q, want %q", got, want)
			}
		})
	}
}

func TestRecoverRefusesChangedAdditionRefs(t *testing.T) {
	_, root, _ := seed(t)
	docs := carrying(t, "docs", "main", map[string]string{"docs/guide.md": "guide\n"})
	declare(t, root, config.PublicFile, "docs", docs, "docs/**")
	if output, err := run(t, reload(t, root), root, true); err != nil {
		t.Fatalf("reconfigure: %v\noutput: %s", err, output)
	}
	recorded(t, root, created(t, root, "docs", docs, recordedRefs(t, root, "docs")))
	if _, err := git.Run(root, "--git-dir="+repository.Directory(root, "docs"),
		"update-ref", "refs/heads/side", head(t, root, "docs")); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	err := stage.Recover(reload(t, root), root, &output)
	if err == nil {
		t.Fatalf("recover accepted changed refs\noutput: %s", output.String())
	}
	wants(t, err.Error(), "REC001", "refs differ from the recorded reconfiguration state")
	if reference(t, root, "docs", "refs/heads/side") == "" {
		t.Fatal("recover removed the externally created ref")
	}
}

func TestRecoverRefusesANewUntrackedAdditionPath(t *testing.T) {
	_, root, _ := seed(t)
	docs := carrying(t, "docs", "main", map[string]string{"docs/guide.md": "guide\n"})
	declare(t, root, config.PublicFile, "docs", docs, "docs/**")
	if output, err := run(t, reload(t, root), root, true); err != nil {
		t.Fatalf("reconfigure: %v\noutput: %s", err, output)
	}
	recorded(t, root, created(t, root, "docs", docs, recordedRefs(t, root, "docs")))
	write(t, root, "docs/local.md", "mine\n")

	var output bytes.Buffer
	err := stage.Recover(reload(t, root), root, &output)
	if err == nil {
		t.Fatalf("recover exposed a new untracked path\noutput: %s", output.String())
	}
	wants(t, err.Error(), "RECONF001",
		"would put existing local ignored or unassigned paths into a repository", "docs/local.md")
	if got := contents(t, root, "docs/local.md"); got != "mine\n" {
		t.Fatalf("docs/local.md = %q, want the untouched local file", got)
	}
}

func TestReconfigureRecordsEverySuccessfulAdditionFetch(t *testing.T) {
	_, root, _ := seed(t)
	docs := carrying(t, "docs", "main", map[string]string{"docs/guide.md": "guide\n"})
	missing := filepath.Join(temporary(t), "missing.git")
	declare(t, root, config.PublicFile, "docs", docs, "docs/**")
	declare(t, root, config.PublicFile, "notes", missing, "notes/**")

	output, err := run(t, reload(t, root), root, true)
	if err == nil {
		t.Fatalf("reconfigure fetched every addition\noutput: %s", output)
	}
	wants(t, err.Error(), "RECONF001", `repository "notes" could not be fetched`)
	for _, name := range []string{"docs", "notes"} {
		if exists(t, root, name) {
			t.Fatalf("repository %q was left behind", name)
		}
	}
	if _, err := os.Stat(filepath.Join(root, ".gitone", "retired")); !os.IsNotExist(err) {
		t.Fatalf("a fetched addition was retired instead of rolled back: %v", err)
	}
}

func TestRecoverFinishesTheProjectIgnoreUpdate(t *testing.T) {
	_, root, _ := seed(t)
	docs := carrying(t, "docs", "main", map[string]string{"docs/guide.md": "guide\n"})
	declare(t, root, config.PublicFile, "docs", docs, "docs/**")
	recorded(t, root, created(t, root, "docs", docs, nil))

	ignore := filepath.Join(root, config.ProjectIgnoreFile)
	if err := os.Remove(ignore); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(ignore, 0o755); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := stage.Recover(reload(t, root), root, &output); err == nil {
		t.Fatalf("recover skipped the failed ignore update\noutput: %s", output.String())
	}
	if _, err := os.Stat(lock.RecoveryPath(root)); err != nil {
		t.Fatalf("recover removed the journal after the failed ignore update: %v", err)
	}

	if err := os.Remove(ignore); err != nil {
		t.Fatal(err)
	}
	write(t, root, config.ProjectIgnoreFile, "")
	output.Reset()
	if err := stage.Recover(reload(t, root), root, &output); err != nil {
		t.Fatalf("recover after repairing .gitignore: %v\noutput: %s", err, output.String())
	}
	wants(t, contents(t, root, config.ProjectIgnoreFile), ".gitone/", config.LocalFile)
}

func TestAbortRemovesACreatedRepositoryAndRetiresAChangedOne(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(fmt.Sprint("changed=", changed), func(t *testing.T) {
			_, root, _ := seed(t)
			docs := carrying(t, "docs", "main", map[string]string{"docs/guide.md": "guide\n"})
			declare(t, root, config.PublicFile, "docs", docs, "docs/**")
			if output, err := run(t, reload(t, root), root, true); err != nil {
				t.Fatalf("reconfigure: %v\noutput: %s", err, output)
			}
			refs := recordedRefs(t, root, "docs")
			if changed {
				// Somebody worked in the directory between the interruption
				// and the abort, so it is retired instead of removed.
				if _, err := git.Run(root, "--git-dir="+repository.Directory(root, "docs"),
					"update-ref", "refs/heads/side", head(t, root, "docs")); err != nil {
					t.Fatal(err)
				}
			}
			recorded(t, root, created(t, root, "docs", docs, refs))

			var output bytes.Buffer
			if err := stage.Abort(reload(t, root), root, &output); err != nil {
				t.Fatalf("abort: %v\noutput: %s", err, output.String())
			}
			if exists(t, root, "docs") {
				t.Fatal("abort left the created repository in place")
			}
			retired, err := os.ReadDir(filepath.Join(root, ".gitone", "retired"))
			switch {
			case changed && (err != nil || len(retired) != 1):
				t.Fatalf("abort did not retire the changed repository: %v", err)
			case !changed && !os.IsNotExist(err):
				t.Fatalf("abort retired a repository it could remove: %v", err)
			}
			_, err = os.Stat(filepath.Join(root, "docs", "guide.md"))
			if changed == os.IsNotExist(err) {
				t.Fatalf("docs/guide.md presence = %v, want kept only for a retired repository", err == nil)
			}
		})
	}
}

func TestAdoptRemoteHistoryByRetiringAndReaddingEmptyRepository(t *testing.T) {
	_, root, _ := seed(t)
	declare(t, root, config.LocalFile, "docs", "", "docs/**")
	if output, err := run(t, reload(t, root), root, true); err != nil {
		t.Fatalf("create empty repository: %v\n%s", err, output)
	}

	write(t, root, config.LocalFile, "version: 1\nrepositories:\n")
	if output, err := run(t, reload(t, root), root, true); err != nil {
		t.Fatalf("retire empty repository: %v\n%s", err, output)
	}
	remote := carrying(t, "docs", "main", map[string]string{"docs/guide.md": "existing history\n"})
	declare(t, root, config.LocalFile, "docs", remote, "docs/**")
	if output, err := run(t, reload(t, root), root, true); err != nil {
		t.Fatalf("adopt existing history: %v\n%s", err, output)
	}
	if got := contents(t, root, "docs/guide.md"); got != "existing history\n" {
		t.Fatalf("remote content = %q", got)
	}
	healthy(t, root)
}
