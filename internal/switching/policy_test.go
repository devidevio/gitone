package switching_test

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/devidevio/gitone/internal/config"
	"github.com/devidevio/gitone/internal/switching"
)

// publicPaths are the paths the seeded project assigns to the public
// repository, in the order its configuration lists them.
var publicPaths = []string{".gitignore", ".gitone.yml", "README.md", "src/**"}

// committed renders the project configuration with a replaced public path
// list, so only the reviewed policy differs between two branches.
func committed(paths []string, header string) string {
	owned := make([]string, len(paths))
	for index, configured := range paths {
		owned[index] = "      - " + configured
	}
	return fmt.Sprintf(`%sversion: 1
default_branch: main
repositories:
  private:
    visibility: private
    paths:
      - secrets/**
  public:
    visibility: public
    paths:
%s
`, header, strings.Join(owned, "\n"))
}

// applied writes the given files, stages them in one repository and commits
// them. An empty content removes the path instead.
func applied(t *testing.T, root, name, message string, files map[string]string) {
	t.Helper()
	for _, relativePath := range slices.Sorted(maps.Keys(files)) {
		if files[relativePath] == "" {
			run(t, root, name, "rm", "--quiet", "--", relativePath)
			continue
		}
		write(t, root, relativePath, files[relativePath])
		run(t, root, name, "add", "--", relativePath)
	}
	run(t, root, name, "commit", "-m", message)
}

// targeted is a project whose repositories both have a feature branch, the
// public one carrying files as its target policy and content. Every
// repository is back on main afterwards, so only the branch differs.
func targeted(t *testing.T, files map[string]string) (*config.Config, string) {
	t.Helper()
	configuration, root := project(t)
	branch(t, root, "private", "feature", "secrets/extra.txt", "extra\n")
	run(t, root, "public", "checkout", "--quiet", "-b", "feature")
	applied(t, root, "public", "feature policy", files)
	run(t, root, "public", "checkout", "--quiet", "main")
	return configuration, root
}

func contents(t *testing.T, root, relativePath string) string {
	t.Helper()
	read, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relativePath)))
	if err != nil {
		t.Fatal(err)
	}
	return string(read)
}

// onMain fails unless every repository is still on its starting branch, which
// is what every refused switch has to leave behind.
func onMain(t *testing.T, root string) {
	t.Helper()
	for _, name := range names {
		if got := current(t, root, name); got != "main" {
			t.Fatalf("repository %q is on %s, want main", name, got)
		}
	}
}

func onFeature(t *testing.T, root string) {
	t.Helper()
	for _, name := range names {
		if got := current(t, root, name); got != "feature" {
			t.Fatalf("repository %q is on %s, want feature", name, got)
		}
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

func TestSwitchAsksNothingForAFormattingOnlyConfigurationChange(t *testing.T) {
	reordered := []string{"src/**", "README.md", ".gitone.yml", ".gitignore"}
	configuration, root := targeted(t, map[string]string{
		config.PublicFile: committed(reordered, "# reordered and commented\n"),
	})

	output, err := switchTo(configuration, root, "feature")
	if err != nil {
		t.Fatalf("switch: %v\noutput: %s", err, output)
	}
	if strings.Contains(output, "Incoming policy change") {
		t.Fatalf("output previewed a policy change: %s", output)
	}
	onFeature(t, root)
}

// The target branch already owns the path it adds. The source branch's older
// matcher does not, so validating with it would refuse exactly the tree the
// accepted policy assigns.
func TestSwitchChecksTargetPathsAgainstTheTargetPolicy(t *testing.T) {
	extended := append(slices.Clone(publicPaths), "docs/**")
	configuration, root := targeted(t, map[string]string{
		config.PublicFile: committed(extended, ""),
		"docs/guide.md":   "guide\n",
	})

	output, err := switchTo(configuration, root, "feature")
	if err == nil {
		t.Fatalf("switch was accepted without %s: %s", switching.AcceptFlag, output)
	}
	wants(t, err.Error(), switching.AcceptFlag)
	onMain(t, root)
	if exists(t, root, "docs/guide.md") {
		t.Fatal("docs/guide.md was checked out by a refused switch")
	}

	output, err = answered(configuration, root, "feature", true, "")
	if err != nil {
		t.Fatalf("accepted switch: %v\noutput: %s", err, output)
	}
	wants(t, output, "repositories.public.paths", "docs/**")
	onFeature(t, root)
	if !exists(t, root, "docs/guide.md") {
		t.Fatal("docs/guide.md was not checked out")
	}
	wants(t, contents(t, root, config.PublicFile), "docs/**")
}

func TestSwitchPreviewsAndConfirmsAChangedIgnoreFile(t *testing.T) {
	configuration, root := project(t)
	ignored := contents(t, root, config.ProjectIgnoreFile)
	branch(t, root, "private", "feature", "secrets/extra.txt", "extra\n")
	run(t, root, "public", "checkout", "--quiet", "-b", "feature")
	applied(t, root, "public", "feature ignores", map[string]string{
		config.ProjectIgnoreFile: ignored + "dist/\n",
	})
	run(t, root, "public", "checkout", "--quiet", "main")

	output, err := answered(configuration, root, "feature", false, "n\n")
	if err != nil {
		t.Fatalf("declined switch: %v\noutput: %s", err, output)
	}
	wants(t, output, "IGNORE RULES", "+dist/", "Accept this policy change? [y/N]", "Switch aborted.")
	onMain(t, root)
	if got := contents(t, root, config.ProjectIgnoreFile); got != ignored {
		t.Fatalf("%s = %q, want it unchanged", config.ProjectIgnoreFile, got)
	}

	if output, err = answered(configuration, root, "feature", false, "y\n"); err != nil {
		t.Fatalf("accepted switch: %v\noutput: %s", err, output)
	}
	onFeature(t, root)
	wants(t, contents(t, root, config.ProjectIgnoreFile), "dist/")
}

// An existing local path that no repository manages today must never become
// publishable, because the ordinary add, commit and push workflow would then
// publish it.
func TestSwitchRefusesATargetPolicyThatExposesAnExistingLocalPath(t *testing.T) {
	configuration, root := project(t)
	ignored := contents(t, root, config.ProjectIgnoreFile)
	applied(t, root, "public", "ignore generated sources", map[string]string{
		config.ProjectIgnoreFile: ignored + "src/generated/\n",
	})
	write(t, root, "src/generated/big.js", "generated\n")
	branch(t, root, "private", "feature", "secrets/extra.txt", "extra\n")
	run(t, root, "public", "checkout", "--quiet", "-b", "feature")
	applied(t, root, "public", "stop ignoring generated sources", map[string]string{
		config.ProjectIgnoreFile: ignored,
	})
	run(t, root, "public", "checkout", "--quiet", "main")

	for _, accepted := range []bool{false, true} {
		output, err := answered(configuration, root, "feature", accepted, "")
		if err == nil {
			t.Fatalf("exposing switch was accepted with accepted=%t: %s", accepted, output)
		}
		wants(t, err.Error(), "src/generated/big.js", "ignored -> public", switching.AcceptFlag)
		onMain(t, root)
	}
	if got := contents(t, root, config.ProjectIgnoreFile); !strings.Contains(got, "src/generated/") {
		t.Fatalf("%s = %q, want the ignore rule unchanged", config.ProjectIgnoreFile, got)
	}
}

func TestSwitchRefusesAnUnusableTargetConfiguration(t *testing.T) {
	handedOver := strings.Replace(committed(
		slices.DeleteFunc(slices.Clone(publicPaths), func(configured string) bool { return configured == config.PublicFile }), ""),
		"      - secrets/**", "      - secrets/**\n      - "+config.PublicFile, 1)
	for _, testCase := range []struct {
		name   string
		file   string
		wanted string
	}{
		{"invalid", "version: 2\n", "is invalid"},
		{"missing", "", "carries " + config.PublicFile + " any more"},
		{"handed over", handedOver, "assigns " + config.PublicFile + " to repository \"private\""},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			configuration, root := targeted(t, map[string]string{config.PublicFile: testCase.file})
			commit := reference(t, root, "public", "refs/heads/feature")

			for _, accepted := range []bool{false, true} {
				output, err := answered(configuration, root, "feature", accepted, "")
				if err == nil {
					t.Fatalf("switch was accepted with accepted=%t: %s", accepted, output)
				}
				wants(t, err.Error(), "SWITCH001", testCase.wanted,
					"feature at "+commit[:7], "run gitone switch feature again")
				onMain(t, root)
			}
		})
	}
}

func TestSwitchRefusesTargetRepositoryReconfiguration(t *testing.T) {
	configuration, root := targeted(t, map[string]string{
		config.PublicFile: committed(publicPaths, "") + `  extra:
    visibility: public
    paths:
      - extra/**
`,
	})
	commit := reference(t, root, "public", "refs/heads/feature")

	for _, accepted := range []bool{false, true} {
		output, err := answered(configuration, root, "feature", accepted, "")
		if err == nil {
			t.Fatalf("reconfiguring switch was accepted with accepted=%t: %s", accepted, output)
		}
		wants(t, err.Error(), "adds, removes or renames a repository", "repositories.extra", "feature at "+commit[:7],
			"Review and apply it with: "+switching.ReconfigureCommand+" feature")
		onMain(t, root)
	}
}

func TestSwitchRefusesTargetConfigurationInMultipleRepositories(t *testing.T) {
	configuration, root := project(t)
	branch(t, root, "private", "feature", config.PublicFile, twoRepositories)
	branch(t, root, "public", "feature", "src/app.js", "app\n")
	privateCommit := reference(t, root, "private", "refs/heads/feature")
	publicCommit := reference(t, root, "public", "refs/heads/feature")

	output, err := answered(configuration, root, "feature", true, "")
	if err == nil {
		t.Fatalf("configuration in multiple repositories was accepted: %s", output)
	}
	wants(t, err.Error(), config.PublicFile+" is carried by more than one repository",
		"private (feature at "+privateCommit[:7]+")", "public (feature at "+publicCommit[:7]+")")
	onMain(t, root)
}

// The local file is never read from repository content, so an accepted target
// policy still carries every restriction it adds.
func TestSwitchKeepsLocalConfigurationRestrictions(t *testing.T) {
	extended := append(slices.Clone(publicPaths), "docs/**")
	configuration, root := targeted(t, map[string]string{
		config.PublicFile: committed(extended, ""),
		"docs/guide.md":   "guide\n",
	})
	write(t, root, config.LocalFile, "version: 1\nrules:\n  protected_paths:\n    - docs/**\n")
	configuration, _, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}

	output, err := answered(configuration, root, "feature", true, "")
	if err == nil {
		t.Fatalf("switch ignored the local restriction: %s", output)
	}
	wants(t, err.Error(), "docs/guide.md", config.ProtectedReason)
	onMain(t, root)
}
