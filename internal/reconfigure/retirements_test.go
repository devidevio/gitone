package reconfigure_test

import (
	"bytes"
	"errors"
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
)

// removing rewrites the configuration so that it no longer names private and
// public owns every path it holds, which is the retirement that keeps the
// project usable.
func removing(t *testing.T, root, public string, paths ...string) {
	t.Helper()
	block := ""
	for _, pattern := range append([]string{".gitignore", config.PublicFile, "README.md"}, paths...) {
		block += "      - " + pattern + "\n"
	}
	write(t, root, config.PublicFile, fmt.Sprintf(`version: 1
default_branch: main
repositories:
  public:
    remote: %s
    visibility: public
    paths:
%s`, public, block))
}

// retired is the one directory below .gitone/retired/ a test produced.
func retired(t *testing.T, root string) string {
	t.Helper()
	listed, err := repository.Retired(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 {
		t.Fatalf("retired directories = %v, want exactly one", listed)
	}
	return listed[0]
}

func TestReconfigureRetiresARepositoryTheConfigurationRemoved(t *testing.T) {
	_, root, remotes := seed(t)
	removing(t, root, remotes["public"], "secrets/**")
	before := contents(t, root, "secrets/key.txt")

	output, err := answered(reload(t, root), root, "y\n")
	if err != nil {
		t.Fatalf("reconfigure: %v\noutput: %s", err, output)
	}
	wants(t, output, "- private", "unconfigured", remotes["private"],
		"metadata is retired to .gitone/retired/", "never deleted",
		"1 commit, all present on origin",
		`its paths are owned by "public" in this configuration`,
		"Accept this repository reconfiguration?", "private retired to .gitone/retired/")

	if exists(t, root, "private") {
		t.Fatal("the retired repository still has metadata below .gitone/repositories/")
	}
	directory := filepath.Join(root, filepath.FromSlash(retired(t, root)))
	note, err := os.ReadFile(filepath.Join(directory, repository.RetiredFile))
	if err != nil {
		t.Fatalf("retired.yml: %v", err)
	}
	for _, want := range []string{"name: private", "branch: main", "commit: ", "retired: ", remotes["private"]} {
		if !strings.Contains(string(note), want) {
			t.Fatalf("retired.yml = %q, want %q", note, want)
		}
	}

	// The retired metadata is a complete repository, readable with plain Git.
	log, err := git.Run(root, "--git-dir="+directory, "log", "--oneline")
	if err != nil {
		t.Fatalf("the retired repository cannot be read: %v", err)
	}
	if !strings.Contains(log, "Initial") {
		t.Fatalf("retired log = %q, want the recorded history", log)
	}
	if got := contents(t, root, "secrets/key.txt"); got != before {
		t.Fatalf("secrets/key.txt = %q, want the untouched %q", got, before)
	}
	healthy(t, root)
	if _, err := os.Stat(lock.RecoveryPath(root)); !os.IsNotExist(err) {
		t.Fatalf("recovery state was left behind: %v", err)
	}
}

func TestReconfigureRefusesARetirementWithUnpublishedCommits(t *testing.T) {
	configuration, root, remotes := seed(t)
	write(t, root, "secrets/extra.txt", "more\n")
	var staged bytes.Buffer
	if err := stage.Add(configuration, root, root, []string{"-A"}, &staged); err != nil {
		t.Fatal(err)
	}
	if err := stage.Commit(configuration, root, root, nil, "Unpublished", &staged); err != nil {
		t.Fatal(err)
	}
	removing(t, root, remotes["public"], "secrets/**")

	for _, accepted := range []bool{false, true} {
		t.Run(fmt.Sprint("accepted=", accepted), func(t *testing.T) {
			output, err := run(t, reload(t, root), root, accepted)
			if err == nil {
				t.Fatalf("a retirement stranded unpublished commits\noutput: %s", output)
			}
			wants(t, err.Error(), "RECONF001", `repository "private"`, "1 commit",
				remotes["private"], "does not have", "would strand them", "Unpublished",
				`Restore "private" in .gitone.yml`, "gitone push private", "never infers a rename")
			if !exists(t, root, "private") {
				t.Fatal("a refused retirement moved the metadata")
			}
		})
	}
}

func TestReconfigureRefusesUnpublishedDetachedHeadAndStashCommits(t *testing.T) {
	tests := map[string]func(t *testing.T, root string){
		"detached HEAD": func(t *testing.T, root string) {
			gitDirectory := "--git-dir=" + repository.Directory(root, "private")
			if _, err := git.Run(root, gitDirectory, "--work-tree="+root, "checkout", "--quiet", "--detach"); err != nil {
				t.Fatal(err)
			}
			if _, err := git.Run(root, gitDirectory, "--work-tree="+root,
				"commit", "--quiet", "--allow-empty", "-m", "Detached unpublished"); err != nil {
				t.Fatal(err)
			}
		},
		"stash": func(t *testing.T, root string) {
			write(t, root, "secrets/key.txt", "stashed\n")
			if _, err := git.Run(root, "--git-dir="+repository.Directory(root, "private"), "--work-tree="+root,
				"stash", "push", "--quiet", "-m", "Unpublished stash", "--", "secrets/key.txt"); err != nil {
				t.Fatal(err)
			}
		},
	}

	for name, prepare := range tests {
		t.Run(name, func(t *testing.T) {
			_, root, remotes := seed(t)
			prepare(t, root)
			removing(t, root, remotes["public"], "secrets/**")

			for _, accepted := range []bool{false, true} {
				output, err := run(t, reload(t, root), root, accepted)
				if err == nil {
					t.Fatalf("retirement accepted unpublished %s commit\noutput: %s", name, output)
				}
				wants(t, err.Error(), "RECONF001", `repository "private"`, "would strand them")
			}
		})
	}
}

func TestRetiredNoteRecordsDetachedHead(t *testing.T) {
	_, root, remotes := seed(t)
	gitDirectory := "--git-dir=" + repository.Directory(root, "private")
	if _, err := git.Run(root, gitDirectory, "--work-tree="+root, "checkout", "--quiet", "--detach"); err != nil {
		t.Fatal(err)
	}
	head, err := git.Run(root, gitDirectory, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	removing(t, root, remotes["public"], "secrets/**")

	if output, err := run(t, reload(t, root), root, true); err != nil {
		t.Fatalf("reconfigure: %v\noutput: %s", err, output)
	}
	note, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(retired(t, root)), repository.RetiredFile))
	if err != nil {
		t.Fatal(err)
	}
	wants(t, string(note), "branch: (detached)", "commit: "+strings.TrimSpace(head))
}

func TestReconfigureRefusesARetirementWithoutAnOrigin(t *testing.T) {
	_, root, remotes := seed(t)
	if _, err := git.Run(root, "--git-dir="+repository.Directory(root, "private"),
		"remote", "remove", config.OriginRemote); err != nil {
		t.Fatal(err)
	}
	removing(t, root, remotes["public"], "secrets/**")

	for _, accepted := range []bool{false, true} {
		t.Run(fmt.Sprint("accepted=", accepted), func(t *testing.T) {
			output, err := run(t, reload(t, root), root, accepted)
			if err == nil {
				t.Fatalf("a retirement without an origin was accepted\noutput: %s", output)
			}
			wants(t, err.Error(), "RECONF001", `repository "private"`,
				"has no configured origin and 1 commit", "would strand them", "add its origin")
			if !exists(t, root, "private") {
				t.Fatal("a refused retirement moved the metadata")
			}
		})
	}
}

func TestReconfigureRefusesARetirementThatLeavesAPathUnassigned(t *testing.T) {
	_, root, remotes := seed(t)
	removing(t, root, remotes["public"])

	for _, accepted := range []bool{false, true} {
		t.Run(fmt.Sprint("accepted=", accepted), func(t *testing.T) {
			output, err := run(t, reload(t, root), root, accepted)
			if err == nil {
				t.Fatalf("a retirement left a path unassigned\noutput: %s", output)
			}
			wants(t, err.Error(), "RECONF001", `retiring repository "private"`,
				"would leave paths no repository owns", "secrets/key.txt",
				"Let another repository own each path", "ignore it, or remove it",
				"No flag ever accepts this")
			if !exists(t, root, "private") {
				t.Fatal("a refused retirement moved the metadata")
			}
		})
	}
}

// An ignored path is one of the three repairs, so the same removal applies
// once the path is no longer relevant.
func TestReconfigureRetiresWhenTheStrandedPathIsIgnored(t *testing.T) {
	_, root, remotes := seed(t)
	write(t, root, ".gitignore", ".gitone/\n.gitone.local.yml\nsecrets/\n")
	removing(t, root, remotes["public"])

	output, err := run(t, reload(t, root), root, true)
	if err != nil {
		t.Fatalf("reconfigure: %v\noutput: %s", err, output)
	}
	wants(t, output, "no path it tracks is relevant in this configuration")
	if exists(t, root, "private") {
		t.Fatal("the retirement did not move the metadata")
	}
	if _, err := os.Stat(filepath.Join(root, "secrets", "key.txt")); err != nil {
		t.Fatalf("the ignored working-tree file was touched: %v", err)
	}
}

func TestReconfigureDecliningARetirementLeavesTheRepositoryInPlace(t *testing.T) {
	_, root, remotes := seed(t)
	removing(t, root, remotes["public"], "secrets/**")

	output, err := answered(reload(t, root), root, "n\n")
	if err != nil {
		t.Fatalf("declining failed: %v\noutput: %s", err, output)
	}
	wants(t, output, "Reconfiguration aborted.")
	if !exists(t, root, "private") {
		t.Fatal("a declined retirement moved the metadata")
	}
	if listed, err := repository.Retired(root); err != nil || len(listed) != 0 {
		t.Fatalf("retired = %v, %v, want nothing", listed, err)
	}
}

func TestRetirementPreparationFailureKeepsRecoveryState(t *testing.T) {
	_, root, remotes := seed(t)
	removing(t, root, remotes["public"], "secrets/**")
	// A file where the retirement parent belongs fails before the metadata can
	// be moved, so the command must not claim a plain mv finishes the work.
	write(t, root, ".gitone/retired", "")

	output, err := run(t, reload(t, root), root, true)
	if err == nil {
		t.Fatalf("a failed retirement was reported as success\noutput: %s", output)
	}
	wants(t, err.Error(), "RECONF001", `repository "private"`,
		"could not be prepared", "REC001", "gitone recover", "gitone abort")
	if strings.Contains(err.Error(), "\n  mv ") {
		t.Fatalf("preparation failure printed an unusable move: %v", err)
	}
	if !exists(t, root, "private") {
		t.Fatal("the metadata moved although the retirement failed")
	}
	if _, err := os.Stat(lock.RecoveryPath(root)); err != nil {
		t.Fatalf("preparation failure removed recovery state: %v", err)
	}
	if err := os.Remove(filepath.Join(root, ".gitone", "retired")); err != nil {
		t.Fatal(err)
	}
	var recovered bytes.Buffer
	if err := stage.Recover(reload(t, root), root, &recovered); err != nil {
		t.Fatalf("recover: %v\noutput: %s", err, recovered.String())
	}
}

func TestRetireToMarksTheFinalMoveFailure(t *testing.T) {
	_, root, _ := seed(t)
	destination := ".gitone/retired/fixed-private"
	write(t, root, destination+"/occupied", "")

	err := repository.RetireTo(root, "private", destination)
	if !errors.Is(err, repository.ErrRetirementMove) {
		t.Fatalf("RetireTo error = %v, want ErrRetirementMove", err)
	}
}

// retirementState is the journal entry one recorded retirement writes.
func retirementState(t *testing.T, root, name, destination string) map[string]any {
	t.Helper()
	gitDirectory := "--git-dir=" + repository.Directory(root, name)
	branch, err := git.Run(root, gitDirectory, "branch", "--show-current")
	if err != nil {
		t.Fatal(err)
	}
	head, err := git.Run(root, gitDirectory, "rev-parse", "--verify", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	index, err := git.Run(root, "--no-optional-locks", gitDirectory, "--work-tree="+root,
		"ls-files", "--stage", "-z")
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{"name": name, "branch": strings.TrimSpace(branch), "head": strings.TrimSpace(head),
		"index": index, "destination": destination, "refs": recordedRefs(t, root, name)}
}

func TestRecoverFinishesAndAbortReversesARecordedRetirement(t *testing.T) {
	for _, recovering := range []bool{true, false} {
		t.Run(fmt.Sprint("recovering=", recovering), func(t *testing.T) {
			_, root, remotes := seed(t)
			removing(t, root, remotes["public"], "secrets/**")
			destination := ".gitone/retired/20200102T030405Z-private"
			entry := retirementState(t, root, "private", destination)
			// Aborting reverses a move that already happened; recovering
			// finishes one that did not.
			if !recovering {
				if err := repository.RetireTo(root, "private", destination); err != nil {
					t.Fatal(err)
				}
			}
			journal(t, root, map[string]any{"retirements": []map[string]any{entry}})

			var output bytes.Buffer
			resume := stage.Abort
			if recovering {
				resume = stage.Recover
			}
			if err := resume(reload(t, root), root, &output); err != nil {
				t.Fatalf("resume: %v\noutput: %s", err, output.String())
			}
			if got := exists(t, root, "private"); got == recovering {
				t.Fatalf("metadata below .gitone/repositories/ = %v, want %v", got, !recovering)
			}
			_, err := os.Stat(filepath.Join(root, filepath.FromSlash(destination)))
			if os.IsNotExist(err) == recovering {
				t.Fatalf("the retired directory does not match the resumed command: %v", err)
			}
			if _, err := os.Stat(lock.RecoveryPath(root)); !os.IsNotExist(err) {
				t.Fatalf("recovery state was left behind: %v", err)
			}
		})
	}
}

func TestRecoverAndAbortRefuseARetiredRepositoryChangedOutsideGitOne(t *testing.T) {
	for _, recovering := range []bool{true, false} {
		t.Run(fmt.Sprint("recovering=", recovering), func(t *testing.T) {
			_, root, remotes := seed(t)
			removing(t, root, remotes["public"], "secrets/**")
			destination := ".gitone/retired/20200102T030405Z-private"
			entry := retirementState(t, root, "private", destination)

			directory := repository.Directory(root, "private")
			if !recovering {
				if err := repository.RetireTo(root, "private", destination); err != nil {
					t.Fatal(err)
				}
				directory = filepath.Join(root, filepath.FromSlash(destination))
			}
			// One commit between the interruption and the resumed command
			// moves the recorded refs.
			if _, err := git.Run(root, "--git-dir="+directory, "--work-tree="+root,
				"commit", "--quiet", "--allow-empty", "-m", "Outside"); err != nil {
				t.Fatal(err)
			}
			journal(t, root, map[string]any{"retirements": []map[string]any{entry}})

			var output bytes.Buffer
			resume := stage.Abort
			if recovering {
				resume = stage.Recover
			}
			err := resume(reload(t, root), root, &output)
			if err == nil {
				t.Fatalf("resume moved a repository changed outside GitOne\noutput: %s", output.String())
			}
			wants(t, err.Error(), "REC001", `repository "private"`, "changed outside GitOne")
			if got := exists(t, root, "private"); got != recovering {
				t.Fatalf("metadata below .gitone/repositories/ = %v, want %v", got, recovering)
			}
			if _, err := os.Stat(lock.RecoveryPath(root)); err != nil {
				t.Fatalf("the recorded state was removed by a refusal: %v", err)
			}
		})
	}
}

func TestRecoverAndAbortRefuseAChangedRetirementIndex(t *testing.T) {
	for _, recovering := range []bool{true, false} {
		t.Run(fmt.Sprint("recovering=", recovering), func(t *testing.T) {
			_, root, remotes := seed(t)
			removing(t, root, remotes["public"], "secrets/**")
			destination := ".gitone/retired/20200102T030405Z-private"
			entry := retirementState(t, root, "private", destination)
			directory := repository.Directory(root, "private")
			if !recovering {
				if err := repository.RetireTo(root, "private", destination); err != nil {
					t.Fatal(err)
				}
				directory = filepath.Join(root, filepath.FromSlash(destination))
			}
			write(t, root, "secrets/key.txt", "changed outside GitOne\n")
			if _, err := git.Run(root, "--git-dir="+directory, "--work-tree="+root,
				"add", "secrets/key.txt"); err != nil {
				t.Fatal(err)
			}
			journal(t, root, map[string]any{"retirements": []map[string]any{entry}})

			var output bytes.Buffer
			resume := stage.Abort
			if recovering {
				resume = stage.Recover
			}
			err := resume(reload(t, root), root, &output)
			if err == nil {
				t.Fatalf("resume accepted a changed index\noutput: %s", output.String())
			}
			wants(t, err.Error(), "REC001", `repository "private"`, "index differs")
			if _, err := os.Stat(lock.RecoveryPath(root)); err != nil {
				t.Fatalf("the recorded state was removed by a refusal: %v", err)
			}
		})
	}
}

func TestRecoverAndAbortRefuseMissingRetirementMetadata(t *testing.T) {
	for _, recovering := range []bool{true, false} {
		t.Run(fmt.Sprint("recovering=", recovering), func(t *testing.T) {
			_, root, remotes := seed(t)
			removing(t, root, remotes["public"], "secrets/**")
			destination := ".gitone/retired/20200102T030405Z-private"
			entry := retirementState(t, root, "private", destination)
			directory := repository.Directory(root, "private")
			if !recovering {
				if err := repository.RetireTo(root, "private", destination); err != nil {
					t.Fatal(err)
				}
				directory = filepath.Join(root, filepath.FromSlash(destination))
			}
			if err := os.Rename(directory, filepath.Join(root, "moved-outside-gitone")); err != nil {
				t.Fatal(err)
			}
			journal(t, root, map[string]any{"retirements": []map[string]any{entry}})

			var output bytes.Buffer
			resume := stage.Abort
			if recovering {
				resume = stage.Recover
			}
			err := resume(reload(t, root), root, &output)
			if err == nil {
				t.Fatalf("resume accepted missing metadata\noutput: %s", output.String())
			}
			wants(t, err.Error(), "REC001", `repository "private"`, "directories are missing")
			if _, err := os.Stat(lock.RecoveryPath(root)); err != nil {
				t.Fatalf("the recorded state was removed by a refusal: %v", err)
			}
		})
	}
}
