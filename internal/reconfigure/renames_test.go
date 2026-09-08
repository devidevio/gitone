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
	"github.com/devidevio/gitone/internal/reconfigure"
	"github.com/devidevio/gitone/internal/repository"
	"github.com/devidevio/gitone/internal/stage"
	"github.com/devidevio/gitone/internal/status"
)

// renaming writes the configuration of seed with the private repository under
// another name, which is the change a rename declares. secrets names the
// paths that name owns, so a rename that is really an ownership change can be
// written too.
func renaming(t *testing.T, root, name, remote, public string, secrets ...string) {
	t.Helper()
	owned := ""
	for _, pattern := range append([]string{".gitignore", config.PublicFile, "README.md"}, secrets[1:]...) {
		owned += "      - " + pattern + "\n"
	}
	write(t, root, config.PublicFile, fmt.Sprintf(`version: 1
default_branch: main
repositories:
  %s:
    remote: %s
    visibility: private
    paths:
      - %s
  public:
    remote: %s
    visibility: public
    paths:
%s`, name, remote, secrets[0], public, owned))
}

// declaredRename is what the CLI hands the command for one --rename value.
func declaredRename(t *testing.T, value string) reconfigure.Rename {
	t.Helper()
	declared, ok := reconfigure.ParseRename(value)
	if !ok {
		t.Fatalf("--rename %s was rejected as malformed", value)
	}
	return declared
}

// marker plants a file inside the metadata directory of one repository, so a
// moved directory can be told from a re-created one.
func marker(t *testing.T, root, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repository.Directory(root, name), "marker.txt"), []byte("moved\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestReconfigureRenamesARepositoryAfterOneConfirmation(t *testing.T) {
	_, root, remotes := seed(t)
	renaming(t, root, "notes", remotes["private"], remotes["public"], "secrets/**")

	output, err := answered(reload(t, root), root, "y\n", declaredRename(t, "private=notes"))
	if err != nil {
		t.Fatalf("reconfigure: %v\noutput: %s", err, output)
	}
	wants(t, output, "REPOSITORIES", "~ private -> notes   private   remote unchanged",
		"metadata is moved to .gitone/repositories/notes",
		"Accept this repository reconfiguration? [y/N]",
		"private renamed to notes, metadata moved without fetching")
	if strings.Contains(output, "- private") || strings.Contains(output, "+ notes") {
		t.Fatalf("a rename was previewed as a removal plus an addition:\n%s", output)
	}
	if exists(t, root, "private") {
		t.Fatal("the renamed repository still has metadata under its old name")
	}
	if !exists(t, root, "notes") {
		t.Fatal("the renamed repository has no metadata under its new name")
	}
	if listed, err := repository.Retired(root); err != nil || len(listed) != 0 {
		t.Fatalf("retired directories = %v (%v), want none: a rename retires nothing", listed, err)
	}
	healthy(t, root)
	if _, err := os.Stat(lock.RecoveryPath(root)); !os.IsNotExist(err) {
		t.Fatalf("recovery state was left behind: %v", err)
	}
}

func TestRenamedRepositoryKeepsItsHistoryIndexAndReflog(t *testing.T) {
	_, root, remotes := seed(t)
	before := snapshot(t, root, "private")
	marker(t, root, "private")
	renaming(t, root, "notes", remotes["private"], remotes["public"], "secrets/**")

	output, err := run(t, reload(t, root), root, true, declaredRename(t, "private=notes"))
	if err != nil {
		t.Fatalf("reconfigure: %v\noutput: %s", err, output)
	}
	// The very directory moved: a re-created repository would carry neither
	// the planted file nor the reflog of the commit that was made here.
	if _, err := os.Stat(filepath.Join(repository.Directory(root, "notes"), "marker.txt")); err != nil {
		t.Fatalf("the metadata directory was re-created instead of moved: %v", err)
	}
	if got := snapshot(t, root, "notes"); got != before {
		t.Fatalf("renamed repository state =\n%s\nwant the unchanged\n%s", got, before)
	}
	if got := contents(t, root, "secrets/key.txt"); got != "secret\n" {
		t.Fatalf("secrets/key.txt = %q, want the untouched content", got)
	}
}

// snapshot is everything a rename must keep: the history, the index and the
// reflog of one repository.
func snapshot(t *testing.T, root, name string) string {
	t.Helper()
	gitDirectory := "--git-dir=" + repository.Directory(root, name)
	recorded := ""
	for _, arguments := range [][]string{
		{gitDirectory, "log", "--format=%H %s", "--all"},
		{gitDirectory, "--work-tree=" + root, "ls-files", "--stage"},
		{gitDirectory, "reflog", "show", "--format=%H %gs", "main"},
	} {
		output, err := git.Run(root, arguments...)
		if err != nil {
			t.Fatal(err)
		}
		recorded += output
	}
	return recorded
}

func TestRenamedRepositoryIsReportedUnderItsNewName(t *testing.T) {
	_, root, remotes := seed(t)
	renaming(t, root, "notes", remotes["private"], remotes["public"], "secrets/**")

	if output, err := run(t, reload(t, root), root, true, declaredRename(t, "private=notes")); err != nil {
		t.Fatalf("reconfigure: %v\noutput: %s", err, output)
	}
	healthy(t, root)
	result, err := status.Collect(reload(t, root), root)
	if err != nil {
		t.Fatal(err)
	}
	reported := map[string]bool{}
	for _, entry := range result.Repositories {
		reported[entry.Name] = true
	}
	if !reported["notes"] || reported["private"] {
		t.Fatalf("gitone status reports %v, want the new name and not the old one", reported)
	}
	log, err := git.Run(root, "--git-dir="+repository.Directory(root, "notes"), "log", "--oneline")
	if err != nil || !strings.Contains(log, "Initial") {
		t.Fatalf("gitone log on the renamed repository = %q (%v), want its history", log, err)
	}
}

func TestReconfigureRefusesARenameThatDoesNotMatchTheConfiguration(t *testing.T) {
	tests := map[string]struct {
		declared []string
		wants    []string
	}{
		"does not disappear": {
			declared: []string{"public=notes"},
			wants:    []string{"--rename public=notes", `still names "public"`, "does not disappear"},
		},
		"has no metadata": {
			declared: []string{"absent=notes"},
			wants:    []string{"--rename absent=notes", ".gitone/repositories/absent", "holds no metadata"},
		},
		"does not appear": {
			declared: []string{"private=missing"},
			wants:    []string{"--rename private=missing", `does not name "missing"`, "appears under it"},
		},
		"already has metadata": {
			declared: []string{"private=public"},
			wants:    []string{"--rename private=public", ".gitone/repositories/public", "already holds metadata"},
		},
		"named twice": {
			declared: []string{"private=notes", "private=other"},
			wants:    []string{`--rename names repository "private" 2 times`, "renamed at most once"},
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			_, root, remotes := seed(t)
			renaming(t, root, "notes", remotes["private"], remotes["public"], "secrets/**")
			var declared []reconfigure.Rename
			for _, value := range test.declared {
				declared = append(declared, declaredRename(t, value))
			}

			for _, accepted := range []bool{false, true} {
				output, err := run(t, reload(t, root), root, accepted, declared...)
				if err == nil {
					t.Fatalf("a mismatched rename was accepted\noutput: %s", output)
				}
				wants(t, err.Error(), append(test.wants, "RECONF001",
					"No repository was created or renamed")...)
			}
			if !exists(t, root, "private") || exists(t, root, "notes") {
				t.Fatal("a refused rename moved the metadata")
			}
		})
	}
}

func TestReconfigureRefusesARenameThatIsAnOwnershipChange(t *testing.T) {
	configuration, root, remotes := seed(t)
	write(t, root, "secrets/extra.txt", "extra\n")
	var staged bytes.Buffer
	if err := stage.Add(configuration, root, root, []string{"-A"}, &staged); err != nil {
		t.Fatal(err)
	}
	if err := stage.Commit(configuration, root, root, nil, "Extra", &staged); err != nil {
		t.Fatal(err)
	}
	// notes keeps one of the two paths its metadata holds; the other one is
	// owned by public in the same configuration, which is an ownership move
	// rather than a rename.
	renaming(t, root, "notes", remotes["private"], remotes["public"], "secrets/key.txt", "secrets/extra.txt")

	for _, accepted := range []bool{false, true} {
		output, err := run(t, reload(t, root), root, accepted, declaredRename(t, "private=notes"))
		if err == nil {
			t.Fatalf("a rename that moves ownership was accepted\noutput: %s", output)
		}
		wants(t, err.Error(), "RECONF001", `repository "private"`, "secrets/extra.txt",
			`as "notes"`, "is owned by public")
	}
	if !exists(t, root, "private") || exists(t, root, "notes") {
		t.Fatal("a refused rename moved the metadata")
	}
}

func TestReconfigureRefusesAStagedOwnershipChangeBeforeRenaming(t *testing.T) {
	configuration, root, remotes := seed(t)
	write(t, root, "secrets/extra.txt", "extra\n")
	var staged bytes.Buffer
	if err := stage.Add(configuration, root, root, []string{"secrets/extra.txt"}, &staged); err != nil {
		t.Fatal(err)
	}
	// notes owns the committed path, but the newly staged path moves to public.
	// The preserved index would make the renamed project immediately invalid.
	renaming(t, root, "notes", remotes["private"], remotes["public"], "secrets/key.txt", "secrets/extra.txt")

	output, err := run(t, reload(t, root), root, true, declaredRename(t, "private=notes"))
	if err == nil {
		t.Fatalf("a staged ownership change was accepted\noutput: %s", output)
	}
	wants(t, err.Error(), "RECONF001", `repository "private"`, "secrets/extra.txt",
		"from its index", `as "notes"`, "is owned by public")
	if !exists(t, root, "private") || exists(t, root, "notes") {
		t.Fatal("a refused staged ownership change moved the metadata")
	}
}

func TestReconfigureRefusesAnUnsafeIndexLinkBeforeRenaming(t *testing.T) {
	_, root, remotes := seed(t)
	if err := os.Symlink("../README.md", filepath.Join(root, "secrets", "readme")); err != nil {
		t.Fatal(err)
	}
	if _, err := git.Run(root, "--git-dir="+repository.Directory(root, "private"),
		"--work-tree="+root, "add", "secrets/readme"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "secrets", "readme")); err != nil {
		t.Fatal(err)
	}
	renaming(t, root, "notes", remotes["private"], remotes["public"], "secrets/**")

	output, err := run(t, reload(t, root), root, true, declaredRename(t, "private=notes"))
	if err == nil {
		t.Fatalf("an unsafe index link was accepted\noutput: %s", output)
	}
	wants(t, err.Error(), "RECONF001", `repository "private" index`, "secrets/readme",
		`as "notes"`, "symbolic link", "belongs to public")
	if !exists(t, root, "private") || exists(t, root, "notes") {
		t.Fatal("a refused unsafe index link moved the metadata")
	}
}

func TestReconfigureAppliesARenameThatAlsoChangesTheRemote(t *testing.T) {
	_, root, remotes := seed(t)
	moved := mirror(t, remotes["private"], "notes")
	renaming(t, root, "notes", moved, remotes["public"], "secrets/**")

	output, err := answered(reload(t, root), root, "y\n", declaredRename(t, "private=notes"))
	if err != nil {
		t.Fatalf("reconfigure: %v\noutput: %s", err, output)
	}
	wants(t, output, "~ private -> notes   private   remotes change, see REMOTES",
		"REMOTES", "notes / origin", "before: "+remotes["private"], "after:  "+moved,
		"NETWORK", "notes will be fetched from "+moved,
		"Accept this repository reconfiguration? [y/N]")
	if strings.Count(output, "Accept this repository reconfiguration?") != 1 {
		t.Fatalf("the rename and the URL change asked twice:\n%s", output)
	}
	if got := remoteURL(t, root, "notes", config.OriginRemote); got != moved {
		t.Fatalf("notes origin = %q, want %q", got, moved)
	}
	healthy(t, root)
}

func TestReconfigureRefusesARenameOntoAnUnrelatedHistory(t *testing.T) {
	_, root, remotes := seed(t)
	other := unrelated(t, "notes")
	renaming(t, root, "notes", other, remotes["public"], "secrets/**")

	output, err := run(t, reload(t, root), root, true, declaredRename(t, "private=notes"))
	if err == nil {
		t.Fatalf("a rename onto an unrelated history was accepted\noutput: %s", output)
	}
	wants(t, err.Error(), "RECONF001", "unrelated history", other)
	if !exists(t, root, "private") || exists(t, root, "notes") {
		t.Fatal("a refused rename left the metadata under the new name")
	}
	if got := remoteURL(t, root, "private", config.OriginRemote); got != remotes["private"] {
		t.Fatalf("private origin = %q, want the restored %q", got, remotes["private"])
	}
	if _, err := os.Stat(lock.RecoveryPath(root)); !os.IsNotExist(err) {
		t.Fatalf("recovery state was left behind: %v", err)
	}
}

func TestTheSameChangeWithoutTheFlagIsARetirementPlusAnAddition(t *testing.T) {
	configuration, root, remotes := seed(t)
	renaming(t, root, "notes", remotes["private"], remotes["public"], "secrets/**")

	// Declining shows what the same configuration change means without the
	// declaration: two repositories, not one moved directory.
	declined, err := answered(reload(t, root), root, "n\n")
	if err != nil {
		t.Fatalf("reconfigure: %v\noutput: %s", err, declined)
	}
	wants(t, declined, "+ notes   private   "+remotes["private"], "- private   unconfigured",
		"metadata is retired to .gitone/retired/")
	if strings.Contains(declined, "~ private -> notes") {
		t.Fatalf("a rename was inferred without the declaration:\n%s", declined)
	}

	// And the retirement refusal of an unpublished commit still stops it.
	writeConfig(t, root, remotes["private"], remotes["public"])
	write(t, root, "secrets/extra.txt", "more\n")
	var staged bytes.Buffer
	if err := stage.Add(configuration, root, root, []string{"-A"}, &staged); err != nil {
		t.Fatal(err)
	}
	if err := stage.Commit(configuration, root, root, nil, "Unpublished", &staged); err != nil {
		t.Fatal(err)
	}
	renaming(t, root, "notes", remotes["private"], remotes["public"], "secrets/**")

	output, err := run(t, reload(t, root), root, true)
	if err == nil {
		t.Fatalf("a removal stranded unpublished commits\noutput: %s", output)
	}
	wants(t, err.Error(), "RECONF001", `repository "private"`, "would strand them", "never infers a rename")
	if !exists(t, root, "private") {
		t.Fatal("a refused retirement moved the metadata")
	}
}

// renameEntry is the journal entry one recorded rename writes.
func renameEntry(t *testing.T, root, from, to string) map[string]any {
	t.Helper()
	entry := retirementState(t, root, from, "")
	delete(entry, "name")
	delete(entry, "destination")
	entry["from"], entry["to"] = from, to
	return entry
}

func TestRecoverFinishesAndAbortReversesARecordedRename(t *testing.T) {
	for _, recovering := range []bool{true, false} {
		t.Run(fmt.Sprint("recovering=", recovering), func(t *testing.T) {
			_, root, remotes := seed(t)
			renaming(t, root, "notes", remotes["private"], remotes["public"], "secrets/**")
			entry := renameEntry(t, root, "private", "notes")
			// Aborting reverses a move that already happened; recovering
			// finishes one that did not.
			if !recovering {
				if err := repository.Move(root, "private", "notes"); err != nil {
					t.Fatal(err)
				}
			}
			journal(t, root, map[string]any{"renames": []map[string]any{entry}})

			var output bytes.Buffer
			resume := stage.Abort
			if recovering {
				resume = stage.Recover
			}
			if err := resume(reload(t, root), root, &output); err != nil {
				t.Fatalf("resume: %v\noutput: %s", err, output.String())
			}
			if got := exists(t, root, "notes"); got != recovering {
				t.Fatalf("metadata under the new name = %v, want %v", got, recovering)
			}
			if got := exists(t, root, "private"); got == recovering {
				t.Fatalf("metadata under the old name = %v, want %v", got, !recovering)
			}
			if _, err := os.Stat(lock.RecoveryPath(root)); !os.IsNotExist(err) {
				t.Fatalf("recovery state was left behind: %v", err)
			}
		})
	}
}

func TestRecoverAndAbortRefuseARenamedRepositoryChangedOutsideGitOne(t *testing.T) {
	for _, recovering := range []bool{true, false} {
		t.Run(fmt.Sprint("recovering=", recovering), func(t *testing.T) {
			_, root, remotes := seed(t)
			renaming(t, root, "notes", remotes["private"], remotes["public"], "secrets/**")
			entry := renameEntry(t, root, "private", "notes")
			name := "private"
			if !recovering {
				if err := repository.Move(root, "private", "notes"); err != nil {
					t.Fatal(err)
				}
				name = "notes"
			}
			// One commit between the interruption and the resumed command
			// moves the recorded refs.
			if _, err := git.Run(root, "--git-dir="+repository.Directory(root, name), "--work-tree="+root,
				"commit", "--quiet", "--allow-empty", "-m", "Outside"); err != nil {
				t.Fatal(err)
			}
			journal(t, root, map[string]any{"renames": []map[string]any{entry}})

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
			if !exists(t, root, name) {
				t.Fatalf("the refused resume moved the metadata away from %q", name)
			}
			if _, err := os.Stat(lock.RecoveryPath(root)); err != nil {
				t.Fatalf("the recorded state was removed by a refusal: %v", err)
			}
		})
	}
}

func TestAbortValidatesRenamedMetadataBeforeRestoringRemotes(t *testing.T) {
	tests := map[string]func(t *testing.T, root string){
		"both names hold metadata": func(t *testing.T, root string) {
			if err := repository.Create(root, "private", "main", config.Repository{}); err != nil {
				t.Fatal(err)
			}
		},
		"index changed": func(t *testing.T, root string) {
			write(t, root, "secrets/key.txt", "changed\n")
			if _, err := git.Run(root, "--git-dir="+repository.Directory(root, "notes"),
				"--work-tree="+root, "add", "secrets/key.txt"); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			_, root, remotes := seed(t)
			moved := mirror(t, remotes["private"], "notes")
			renaming(t, root, "notes", moved, remotes["public"], "secrets/**")
			rename := renameEntry(t, root, "private", "notes")
			repositoryState := entry(t, root, "private", map[string]string{
				"name": config.OriginRemote, "before": remotes["private"], "after": moved,
			})
			repositoryState["name"] = "notes"
			if err := repository.Move(root, "private", "notes"); err != nil {
				t.Fatal(err)
			}
			if _, err := git.Run(root, "--git-dir="+repository.Directory(root, "notes"),
				"remote", "set-url", config.OriginRemote, moved); err != nil {
				t.Fatal(err)
			}
			change(t, root)
			journal(t, root, map[string]any{
				"repositories": []map[string]any{repositoryState},
				"renames":      []map[string]any{rename},
			})

			var output bytes.Buffer
			err := stage.Abort(reload(t, root), root, &output)
			if err == nil {
				t.Fatalf("abort accepted changed rename metadata\noutput: %s", output.String())
			}
			wants(t, err.Error(), "REC001", `repository "private"`, "changed outside GitOne")
			if got := remoteURL(t, root, "notes", config.OriginRemote); got != moved {
				t.Fatalf("notes origin = %q, want untouched %q", got, moved)
			}
		})
	}
}

func TestRecoverDoesNotRenameWhenTheDestinationCannotBeInspected(t *testing.T) {
	_, root, remotes := seed(t)
	renaming(t, root, "notes", remotes["private"], remotes["public"], "secrets/**")
	rename := renameEntry(t, root, "private", "notes")
	journal(t, root, map[string]any{"renames": []map[string]any{rename}})
	destination := repository.Directory(root, "notes")
	if err := os.Symlink("notes", destination); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	if err := stage.Recover(reload(t, root), root, &output); err == nil {
		t.Fatalf("recover replaced an uninspectable destination\noutput: %s", output.String())
	}
	if !exists(t, root, "private") {
		t.Fatal("recover moved the source despite the destination inspection error")
	}
	if info, err := os.Lstat(destination); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("destination = %v (%v), want the untouched symbolic link", info, err)
	}
}
