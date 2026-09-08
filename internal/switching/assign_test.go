package switching_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"maps"
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

// assigned runs a switch that accepts the generated path assignments but no
// policy change, which is what --accept-new-paths alone does.
func assigned(t *testing.T, configuration *config.Config, root, name string) string {
	t.Helper()
	output, err := attempted(configuration, root, name, false, true, "")
	if err != nil {
		t.Fatalf("switch %q: %v\noutput: %s", name, err, output)
	}
	return output
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

// ordered fails unless the given texts appear in the output in that order,
// which is what one ordered review of a policy change and its assignments is.
func ordered(t *testing.T, got string, expected ...string) {
	t.Helper()
	position := 0
	for _, want := range expected {
		index := strings.Index(got[position:], want)
		if index < 0 {
			t.Fatalf("got %q, want %q after position %d", got, want, position)
		}
		position += index + len(want)
	}
}

func TestSwitchAssignsOneNewTargetPathToItsOwnRepository(t *testing.T) {
	configuration, root := targeted(t, map[string]string{"CHANGELOG.md": "# changes\n"})

	output := assigned(t, configuration, root, "feature")
	onFeature(t, root)
	wants(t, output, "CHANGELOG.md -> public")
	if got := owned(t, root, "public"); !slices.Contains(got, "CHANGELOG.md") {
		t.Fatalf("public paths = %v, want the assigned CHANGELOG.md", got)
	}
	if !exists(t, root, "CHANGELOG.md") {
		t.Fatal("CHANGELOG.md is missing after the switch")
	}
}

func TestSwitchProposesOnlyAWholeNewDirectory(t *testing.T) {
	configuration, root := targeted(t, map[string]string{
		"docs/a.md": "a\n", "docs/b.md": "b\n", "docs/c.md": "c\n", "notes/single.md": "single\n"})

	output := assigned(t, configuration, root, "feature")
	wants(t, output, "docs/** -> public (3 new paths)", "notes/single.md -> public")
	if got := owned(t, root, "public"); !slices.Contains(got, "docs/**") || !slices.Contains(got, "notes/single.md") {
		t.Fatalf("public paths = %v, want the directory and the exact path", got)
	}
}

func TestSwitchRefusesNewTargetPathsWithoutTheFlag(t *testing.T) {
	configuration, root := targeted(t, map[string]string{"CHANGELOG.md": "# changes\n"})
	before := contents(t, root, config.PublicFile)

	_, err := switchTo(configuration, root, "feature")
	if err == nil || !strings.Contains(err.Error(), "--accept-new-paths") {
		t.Fatalf("error = %v, want the required flag", err)
	}
	onMain(t, root)
	if got := contents(t, root, config.PublicFile); got != before {
		t.Fatalf("%s was patched by a refused switch:\n%s", config.PublicFile, got)
	}
}

func TestSwitchShowsEveryProposalBeforeOneConfirmationAndCancelsCleanly(t *testing.T) {
	configuration, root := targeted(t, map[string]string{"docs/a.md": "a\n", "docs/b.md": "b\n"})
	before := contents(t, root, config.PublicFile)

	output, err := answered(configuration, root, "feature", false, "n\n")
	if err != nil {
		t.Fatalf("switch: %v\noutput: %s", err, output)
	}
	wants(t, output, "New paths without an owner:", "docs/** -> public (2 new paths)",
		"Assign these paths?", "Switch aborted.")
	onMain(t, root)
	if got := contents(t, root, config.PublicFile); got != before {
		t.Fatalf("%s was patched by a declined switch:\n%s", config.PublicFile, got)
	}
	if exists(t, root, "docs/a.md") {
		t.Fatal("docs/a.md was checked out by a declined switch")
	}
}

func TestSwitchDoesNotAssistWhileAnotherProblemRemains(t *testing.T) {
	configuration, root := targeted(t, map[string]string{"CHANGELOG.md": "# changes\n"})
	// private carries a path public owns on the same target branch, which no
	// assignment may repair and which stops the whole switch.
	run(t, root, "private", "checkout", "--quiet", "feature")
	run(t, root, "private", "add", "--", "README.md")
	run(t, root, "private", "commit", "-m", "foreign on feature")
	run(t, root, "private", "checkout", "--quiet", "main")
	write(t, root, "README.md", "one\n")
	before := contents(t, root, config.PublicFile)

	_, err := attempted(configuration, root, "feature", false, true, "")
	if err == nil {
		t.Fatal("the switch succeeded despite the foreign path")
	}
	wants(t, err.Error(), `SWITCH001 repository "private" README.md in feature: path is owned by public`,
		"CHANGELOG.md in feature")
	onMain(t, root)
	if got := contents(t, root, config.PublicFile); got != before {
		t.Fatalf("%s was patched by a refused switch:\n%s", config.PublicFile, got)
	}
}

func TestSwitchPreviewsAPolicyChangeBeforeTheAssignmentsAndNeedsBothFlags(t *testing.T) {
	configuration, root := targeted(t, map[string]string{
		config.PublicFile: committed(append(slices.Clone(publicPaths), "extra/**"), ""),
		"CHANGELOG.md":    "# changes\n"})

	_, err := attempted(configuration, root, "feature", false, true, "")
	if err == nil || !strings.Contains(err.Error(), "--accept-config-change") {
		t.Fatalf("error = %v, want the required policy flag", err)
	}
	onMain(t, root)

	_, err = attempted(configuration, root, "feature", true, false, "")
	if err == nil || !strings.Contains(err.Error(), "--accept-new-paths") {
		t.Fatalf("error = %v, want the required assignment flag", err)
	}
	onMain(t, root)

	output, err := attempted(configuration, root, "feature", false, false, "y\n")
	if err != nil {
		t.Fatalf("switch: %v\noutput: %s", err, output)
	}
	ordered(t, output, "Incoming policy change:", "New paths without an owner:", "CHANGELOG.md -> public",
		"Accept this policy change and assign these paths? [y/N]")
	if got := strings.Count(output, "[y/N]"); got != 1 {
		t.Fatalf("confirmation count = %d, want one\noutput: %s", got, output)
	}
	onFeature(t, root)
	if got := owned(t, root, "public"); !slices.Contains(got, "extra/**") || !slices.Contains(got, "CHANGELOG.md") {
		t.Fatalf("public paths = %v, want the target policy and the assignment", got)
	}
}

func TestAcceptNewPathsIsHarmlessWithoutProposals(t *testing.T) {
	configuration, root := feature(t)

	output := assigned(t, configuration, root, "feature")
	onFeature(t, root)
	if strings.Contains(output, "New paths without an owner:") {
		t.Fatalf("output previews an assignment that does not exist:\n%s", output)
	}
}

func TestSwitchLeavesTheAssignmentUnstagedAndReportsIt(t *testing.T) {
	configuration, root := targeted(t, map[string]string{"CHANGELOG.md": "# changes\n"})

	output := assigned(t, configuration, root, "feature")
	wants(t, output, "Assigned 1 new path pattern, unstaged:",
		`review .gitone.yml and commit it with: gitone commit .gitone.yml -m "Assign new paths"`)
	if got := porcelain(t, root, "public"); !strings.Contains(got, " M .gitone.yml") {
		t.Fatalf("public status = %q, want the unstaged configuration patch", got)
	}
}

func TestSwitchLeavesTheConfigurationAloneWhenACheckoutFails(t *testing.T) {
	configuration, root := targeted(t, map[string]string{"CHANGELOG.md": "# changes\n", "src/app.js": "app\n"})
	before := contents(t, root, config.PublicFile)
	// A read-only src/ passes every preflight, which only reads, and makes
	// just the checkout of public fail, after private already switched.
	directory := filepath.Join(root, "src")
	if err := os.Chmod(directory, 0o500); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(directory, 0o755) }()

	_, err := attempted(configuration, root, "feature", false, true, "")
	if err == nil || !strings.Contains(err.Error(), `SWITCH001 repository "public" could not be switched to feature`) {
		t.Fatalf("error = %v, want the failed public checkout", err)
	}
	onMain(t, root)
	if got := contents(t, root, config.PublicFile); got != before {
		t.Fatalf("%s was patched by a rolled back switch:\n%s", config.PublicFile, got)
	}
	if _, err := os.Stat(lock.RecoveryPath(root)); !os.IsNotExist(err) {
		t.Fatalf("recovery state remained: %v", err)
	}
}

// interruptAssignment records the state a switch interrupted between its
// checkouts and its configuration patch leaves behind. indexes are the
// pre-switch indexes and original the pre-switch configuration file.
func interruptAssignment(t *testing.T, root string, indexes map[string][]byte, original string, patterns map[string][]string) {
	t.Helper()
	directory := lock.RecoveryPath(root)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	var entries []string
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(directory, name+".original"), indexes[name], 0o600); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, fmt.Sprintf(`{"name":%q,"from":"main","head":%q,"target":%q,"switched":true}`,
			name, reference(t, root, name, "refs/heads/main"), reference(t, root, name, "refs/heads/feature")))
	}
	if err := os.WriteFile(filepath.Join(directory, "0.configuration.original"), []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	patched := []byte(original)
	for _, name := range slices.Sorted(maps.Keys(patterns)) {
		var err error
		if patched, err = config.AddPaths(patched, name, patterns[name]); err != nil {
			t.Fatal(err)
		}
	}
	hash := func(contents []byte) string { return fmt.Sprintf("%x", sha256.Sum256(contents)) }
	configured, err := json.Marshal([]map[string]any{{"path": config.PublicFile, "patterns": patterns,
		"original_hash": hash([]byte(original)), "target_hash": hash([]byte(original)), "patched_hash": hash(patched)}})
	if err != nil {
		t.Fatal(err)
	}
	contents := fmt.Sprintf(`{"version":2,"command":"gitone switch","branch":"feature","repositories":[%s],"configuration":%s}`,
		strings.Join(entries, ","), configured)
	if err := os.WriteFile(filepath.Join(directory, lock.StateFile), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

// switchedWithAssignment runs a complete assisted switch and returns the
// pre-switch indexes and configuration, so a test can record the state an
// interruption of exactly that switch would have left behind.
func switchedWithAssignment(t *testing.T) (*config.Config, string, map[string][]byte, string) {
	t.Helper()
	configuration, root := targeted(t, map[string]string{"CHANGELOG.md": "# changes\n"})
	original := contents(t, root, config.PublicFile)
	indexes := map[string][]byte{}
	for _, name := range names {
		saved, err := os.ReadFile(filepath.Join(repository.Directory(root, name), "index"))
		if err != nil {
			t.Fatal(err)
		}
		indexes[name] = saved
	}
	assigned(t, configuration, root, "feature")
	return configuration, root, indexes, original
}

func TestRecoverFinishesTheConfigurationPatchOfAnInterruptedSwitch(t *testing.T) {
	configuration, root, indexes, original := switchedWithAssignment(t)
	// The checkouts finished but the configuration patch did not.
	if err := os.WriteFile(filepath.Join(root, config.PublicFile), []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	interruptAssignment(t, root, indexes, original, map[string][]string{"public": {"CHANGELOG.md"}})

	var output bytes.Buffer
	if err := stage.Recover(configuration, root, &output); err != nil {
		t.Fatalf("recover: %v\noutput: %s", err, output.String())
	}
	onFeature(t, root)
	if got := owned(t, root, "public"); !slices.Contains(got, "CHANGELOG.md") {
		t.Fatalf("public paths = %v, want the finished assignment", got)
	}
	wants(t, output.String(), config.PublicFile)
}

func TestAbortRestoresTheConfigurationOfAnInterruptedSwitch(t *testing.T) {
	configuration, root, indexes, original := switchedWithAssignment(t)
	interruptAssignment(t, root, indexes, original, map[string][]string{"public": {"CHANGELOG.md"}})

	var output bytes.Buffer
	if err := stage.Abort(configuration, root, &output); err != nil {
		t.Fatalf("abort: %v\noutput: %s", err, output.String())
	}
	onMain(t, root)
	if got := contents(t, root, config.PublicFile); got != original {
		t.Fatalf("%s =\n%s\nwant the restored\n%s", config.PublicFile, got, original)
	}
	if exists(t, root, "CHANGELOG.md") {
		t.Fatal("CHANGELOG.md survived the abort")
	}
}

func TestSwitchRecoveryRefusesAConfigurationChangedOutsideGitOne(t *testing.T) {
	commands := map[string]func(*config.Config, string, io.Writer) error{
		"recover": stage.Recover,
		"abort":   stage.Abort,
	}
	for _, command := range slices.Sorted(maps.Keys(commands)) {
		t.Run(command, func(t *testing.T) {
			configuration, root, indexes, original := switchedWithAssignment(t)
			interruptAssignment(t, root, indexes, original, map[string][]string{"public": {"CHANGELOG.md"}})
			changed := contents(t, root, config.PublicFile) + "# changed outside GitOne\n"
			if err := os.WriteFile(filepath.Join(root, config.PublicFile), []byte(changed), 0o644); err != nil {
				t.Fatal(err)
			}

			var output bytes.Buffer
			err := commands[command](configuration, root, &output)
			if err == nil || !strings.Contains(err.Error(), "configuration file .gitone.yml changed outside GitOne") {
				t.Fatalf("%s error = %v", command, err)
			}
			if got := contents(t, root, config.PublicFile); got != changed {
				t.Fatalf("%s overwrote the external change", command)
			}
			if _, err := os.Stat(lock.RecoveryPath(root)); err != nil {
				t.Fatalf("recovery state was removed after a refusal: %v", err)
			}
		})
	}
}
