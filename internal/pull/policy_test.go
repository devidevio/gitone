package pull_test

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/devidevio/gitone/internal/config"
	"github.com/devidevio/gitone/internal/git"
	"github.com/devidevio/gitone/internal/pull"
	"github.com/devidevio/gitone/internal/push"
)

// publicPaths are the paths the seeded project assigns to the public
// repository, in the order its committed configuration lists them.
var publicPaths = []string{".gitignore", ".gitone.yml", "README.md", "src/**"}

// committed renders the committed configuration of the seeded project with a
// replaced public path list. Every configured remote stays exactly as it is,
// so only the reviewed policy differs.
func committed(remotes map[string]string, paths []string, header string) string {
	owned := make([]string, len(paths))
	for index, configured := range paths {
		owned[index] = "      - " + configured
	}
	return fmt.Sprintf(`%sversion: 1
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
%s
`, header, remotes["private"], remotes["public"], strings.Join(owned, "\n"))
}

// publish commits one local file and pushes the public repository, so the
// local project and its remote agree before an incoming change is made.
func publish(t *testing.T, loaded *config.Config, root, name, contents string) {
	t.Helper()
	commit(t, loaded, root, name, contents)
	var output bytes.Buffer
	if err := push.Push(loaded, root, "public", true, false, nil, &output); err != nil {
		t.Fatalf("push: %v\noutput: %s", err, output.String())
	}
}

// changeRemote commits one native Git change in a bare repository through a
// separate clone, the way an external contributor would.
func changeRemote(t *testing.T, bare, message string, change ...string) {
	t.Helper()
	clone := temporary(t)
	if _, err := git.Run(clone, "clone", "--quiet", bare, clone); err != nil {
		t.Fatal(err)
	}
	for _, arguments := range [][]string{change, {"commit", "--quiet", "-m", message},
		{"push", "--quiet", "origin", "HEAD:refs/heads/main"}} {
		if _, err := git.Run(clone, arguments...); err != nil {
			t.Fatal(err)
		}
	}
}

func removeRemote(t *testing.T, bare, name string) {
	t.Helper()
	changeRemote(t, bare, "External remove "+name, "rm", "--quiet", name)
}

// answered runs a pull that answers the policy question from input.
func answered(t *testing.T, loaded *config.Config, root, answer string) (error, string) {
	t.Helper()
	var output bytes.Buffer
	err := pull.Pull(loaded, root, "", false, false, true, strings.NewReader(answer), &output)
	return err, output.String()
}

func TestPullAcceptsAFormattingOnlyConfigurationChange(t *testing.T) {
	loaded, root, remotes := seed(t, nil)
	reordered := []string{"src/**", "README.md", ".gitone.yml", ".gitignore"}
	target := advance(t, remotes["public"], config.PublicFile, committed(remotes, reordered, "# reordered and commented\n"))

	run(t, loaded, root, "")
	if got := head(t, root, "public"); got != target {
		t.Fatalf("public head = %s, want %s", got, target)
	}
}

func TestPullRefusesAConfigurationChangeWithoutTheFlag(t *testing.T) {
	loaded, root, remotes := seed(t, nil)
	extended := append(append([]string{}, publicPaths...), "docs/**")
	advance(t, remotes["public"], config.PublicFile, committed(remotes, extended, ""))

	message := fails(t, loaded, root, "")
	if !strings.Contains(message, pull.AcceptFlag) {
		t.Fatalf("message = %q, want the accept flag", message)
	}
	if got := contents(t, root, config.PublicFile); strings.Contains(got, "docs/**") {
		t.Fatalf("%s was replaced: %s", config.PublicFile, got)
	}
}

func TestPullAppliesAnAcceptedConfigurationChange(t *testing.T) {
	loaded, root, remotes := seed(t, nil)
	extended := append(append([]string{}, publicPaths...), "docs/**")
	target := advance(t, remotes["public"], config.PublicFile, committed(remotes, extended, ""))

	output := accept(t, loaded, root, "", true)
	for _, want := range []string{"repositories.public.paths", "docs/**"} {
		if !strings.Contains(output, want) {
			t.Fatalf("output = %q, want %q", output, want)
		}
	}
	if got := head(t, root, "public"); got != target {
		t.Fatalf("public head = %s, want %s", got, target)
	}
	if got := contents(t, root, config.PublicFile); !strings.Contains(got, "docs/**") {
		t.Fatalf("%s was not updated: %s", config.PublicFile, got)
	}
}

func TestPullPreviewsAChangedIgnoreFileAndAsksOnce(t *testing.T) {
	loaded, root, remotes := seed(t, nil)
	publish(t, loaded, root, config.ProjectIgnoreFile, ".gitone/\n.gitone.local.yml\nbuild/\n")
	advance(t, remotes["public"], config.ProjectIgnoreFile, ".gitone/\n.gitone.local.yml\n")

	err, output := answered(t, loaded, root, "\n")
	if err != nil {
		t.Fatalf("declined pull failed: %v\noutput: %s", err, output)
	}
	for _, want := range []string{"IGNORE RULES", "-build/", "Accept this policy change? [y/N]", "Pull aborted."} {
		if !strings.Contains(output, want) {
			t.Fatalf("output = %q, want %q", output, want)
		}
	}
	if got := contents(t, root, config.ProjectIgnoreFile); !strings.Contains(got, "build/") {
		t.Fatalf("%s was replaced: %s", config.ProjectIgnoreFile, got)
	}

	err, output = answered(t, loaded, root, "y\n")
	if err != nil {
		t.Fatalf("accepted pull failed: %v\noutput: %s", err, output)
	}
	if got := contents(t, root, config.ProjectIgnoreFile); strings.Contains(got, "build/") {
		t.Fatalf("%s was not updated: %s", config.ProjectIgnoreFile, got)
	}
}

func TestPullDetectsAnIgnoreFileRenamedAway(t *testing.T) {
	loaded, root, remotes := seed(t, nil)
	publish(t, loaded, root, config.ProjectIgnoreFile, ".gitone/\n.gitone.local.yml\nsrc/generated/\n")
	write(t, root, "src/generated/private.txt", "private\n")
	advance(t, remotes["public"], "src/keep.txt", "keep\n")
	changeRemote(t, remotes["public"], "External rename ignore", "mv", config.ProjectIgnoreFile, "src/old-ignore")
	before := head(t, root, "public")

	message := fails(t, loaded, root, "")
	if !strings.Contains(message, "src/generated/private.txt") {
		t.Fatalf("message = %q, want the exposed path", message)
	}
	if got := head(t, root, "public"); got != before {
		t.Fatalf("public head = %s, want the unchanged %s", got, before)
	}
}

func TestPullRefusesAPolicyThatExposesAnExistingLocalPath(t *testing.T) {
	loaded, root, remotes := seed(t, nil)
	publish(t, loaded, root, config.ProjectIgnoreFile, ".gitone/\n.gitone.local.yml\nsrc/generated/\n")
	write(t, root, "src/generated/big.js", "generated\n")
	before := head(t, root, "public")
	advance(t, remotes["public"], config.ProjectIgnoreFile, ".gitone/\n.gitone.local.yml\n")

	message := fails(t, loaded, root, "")
	if !strings.Contains(message, "src/generated/big.js") {
		t.Fatalf("message = %q, want the exposed path", message)
	}
	if got := head(t, root, "public"); got != before {
		t.Fatalf("public head = %s, want the unchanged %s", got, before)
	}

	var output bytes.Buffer
	if err := pull.Pull(loaded, root, "", true, false, false, nil, &output); err == nil {
		t.Fatalf("%s accepted an exposing policy, output: %s", pull.AcceptFlag, output.String())
	}
	if got := head(t, root, "public"); got != before {
		t.Fatalf("public head = %s after the accepted pull, want the unchanged %s", got, before)
	}
}

func TestPullRefusesAConfigurationThatLeavesALocalPathUnassigned(t *testing.T) {
	loaded, root, remotes := seed(t, nil)
	write(t, root, "src/local.go", "package local\n")
	kept := []string{".gitignore", ".gitone.yml", "README.md"}
	advance(t, remotes["public"], config.PublicFile, committed(remotes, kept, ""))
	before := head(t, root, "public")

	var output bytes.Buffer
	err := pull.Pull(loaded, root, "", true, false, false, nil, &output)
	if err == nil || !strings.Contains(err.Error(), "src/local.go") {
		t.Fatalf("error = %v, want the invalid local path", err)
	}
	if got := head(t, root, "public"); got != before {
		t.Fatalf("public head = %s, want the unchanged %s", got, before)
	}
}

func TestPullKeepsAnIgnoreFileInsideARevealedDirectory(t *testing.T) {
	loaded, root, remotes := seed(t, nil)
	publish(t, loaded, root, config.ProjectIgnoreFile, ".gitone/\n.gitone.local.yml\nsrc/generated/\n")
	write(t, root, "src/generated/.gitignore", "*\n")
	write(t, root, "src/generated/private.txt", "private\n")
	target := advance(t, remotes["public"], config.ProjectIgnoreFile, ".gitone/\n.gitone.local.yml\n")

	accept(t, loaded, root, "", true)
	if got := head(t, root, "public"); got != target {
		t.Fatalf("public head = %s, want %s", got, target)
	}
}

func TestPullRefusesAnInvalidIncomingConfiguration(t *testing.T) {
	loaded, root, remotes := seed(t, nil)
	target := advance(t, remotes["public"], config.PublicFile, "version: 2\n")

	message := fails(t, loaded, root, "")
	for _, want := range []string{"CONFIG003", target[:7], config.OriginRemote + "/main", "Repair or revert"} {
		if !strings.Contains(message, want) {
			t.Fatalf("message = %q, want %q", message, want)
		}
	}
}

func TestPullRefusesAMissingIncomingConfiguration(t *testing.T) {
	loaded, root, remotes := seed(t, nil)
	removeRemote(t, remotes["public"], config.PublicFile)

	message := fails(t, loaded, root, "")
	if !strings.Contains(message, "carries "+config.PublicFile+" any more") {
		t.Fatalf("message = %q, want the missing configuration", message)
	}
}

func TestPullRefusesAConfigurationThatNoLongerOwnsItself(t *testing.T) {
	loaded, root, remotes := seed(t, nil)
	kept := []string{".gitignore", "README.md", "src/**"}
	handed := strings.Replace(committed(remotes, kept, ""),
		"      - secrets/**", "      - secrets/**\n      - "+config.PublicFile, 1)
	before := head(t, root, "public")
	advance(t, remotes["public"], config.PublicFile, handed)

	message := fails(t, loaded, root, "")
	if !strings.Contains(message, "assigns "+config.PublicFile+" to repository \"private\"") {
		t.Fatalf("message = %q, want the ownership refusal", message)
	}
	if got := head(t, root, "public"); got != before {
		t.Fatalf("public head = %s, want the unchanged %s", got, before)
	}
}

// TestPullKeepsLocalRestrictionsAndValidatesProjectedTrees covers the two
// promises an accepted policy change must keep: the ignored local file is
// merged into the incoming one, and the projected trees are validated against
// the projected policy rather than the accepted one being trusted.
func TestPullKeepsLocalRestrictionsAndValidatesProjectedTrees(t *testing.T) {
	_, root, remotes := seed(t, nil)
	write(t, root, config.LocalFile, "version: 1\nrules:\n  protected_paths:\n    - docs/private.md\n")
	local, _, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	extended := append(append([]string{}, publicPaths...), "docs/**")
	advance(t, remotes["public"], config.PublicFile, committed(remotes, extended, ""))
	before := head(t, root, "public")
	advance(t, remotes["public"], "docs/private.md", "protected locally\n")

	var output bytes.Buffer
	err = pull.Pull(local, root, "", true, false, false, nil, &output)
	if err == nil {
		t.Fatalf("the protected incoming path was accepted, output: %s", output.String())
	}
	if !strings.Contains(err.Error(), "docs/private.md") {
		t.Fatalf("message = %q, want the locally protected path", err.Error())
	}
	if got := head(t, root, "public"); got != before {
		t.Fatalf("public head = %s, want the unchanged %s", got, before)
	}
}

func TestPullRefusesARepositoryOrRemoteChange(t *testing.T) {
	for name, incoming := range map[string]func(map[string]string) string{
		"repository": func(remotes map[string]string) string {
			return committed(remotes, publicPaths, "") + `  extra:
    visibility: private
    paths:
      - extra/**
`
		},
		"remote": func(remotes map[string]string) string {
			return strings.Replace(committed(remotes, publicPaths, ""), remotes["private"], remotes["private"]+"-elsewhere", 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			loaded, root, remotes := seed(t, nil)
			before := head(t, root, "public")
			advance(t, remotes["public"], config.PublicFile, incoming(remotes))

			message := fails(t, loaded, root, "")
			if !strings.Contains(message, "incoming policy adds, removes or renames a repository or changes a configured remote") {
				t.Fatalf("message = %q, want the structural refusal", message)
			}
			// The refusal is a dead end only until it names the command that
			// reviews and applies exactly this change.
			if !strings.Contains(message, "Review and apply it with: "+pull.ReconfigureCommand) {
				t.Fatalf("message = %q, want the next command", message)
			}
			var output bytes.Buffer
			if err := pull.Pull(loaded, root, "", true, false, false, nil, &output); err == nil {
				t.Fatalf("%s accepted a topology change, output: %s", pull.AcceptFlag, output.String())
			}
			if got := head(t, root, "public"); got != before {
				t.Fatalf("public head = %s, want the unchanged %s", got, before)
			}
		})
	}
}

func TestAcceptConfigChangeIsHarmlessAndAcceptsNothingElse(t *testing.T) {
	loaded, root, remotes := seed(t, nil)
	target := advance(t, remotes["public"], "src/app.go", "package main\n")

	accept(t, loaded, root, "", true)
	if got := head(t, root, "public"); got != target {
		t.Fatalf("public head = %s, want %s", got, target)
	}

	advance(t, remotes["public"], "elsewhere/notes.md", "unowned\n")
	var output bytes.Buffer
	err := pull.Pull(loaded, root, "", true, false, false, nil, &output)
	if err == nil {
		t.Fatalf("%s accepted an unowned incoming path, output: %s", pull.AcceptFlag, output.String())
	}
	if !strings.Contains(err.Error(), pull.AcceptPathsFlag) {
		t.Fatalf("message = %q, want the separate path flag", err.Error())
	}
	if !strings.Contains(output.String(), "elsewhere/notes.md") {
		t.Fatalf("output = %q, want the unowned path", output.String())
	}
}

func TestPullWithoutAPolicyChangeAsksNothing(t *testing.T) {
	loaded, root, remotes := seed(t, nil)
	advance(t, remotes["public"], "src/app.go", "package main\n")

	output := run(t, loaded, root, "")
	if strings.Contains(output, "Accept this policy change?") || strings.Contains(output, "Incoming policy change") {
		t.Fatalf("output = %q, want no policy question", output)
	}
}
