package cli_test

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/devidevio/gitone/internal/cli"
)

// documentedForms is every input form the help documents as accepted. The
// tests below keep the help and the accepted CLI input in sync in both
// directions: each form must be listed in the global help and must not be
// rejected as unsupported input.
var documentedForms = [][]string{
	{"setup"},
	// A nonexistent local path is still a URL to Git, so the documented clone
	// forms reach the parser and fail on the missing repository instead of on
	// the network.
	{"clone", "/nonexistent/website.git"},
	{"clone", "/nonexistent/website.git", "site"},
	{"init"},
	{"migrate"},
	{"migrate", "--yes"},
	{"backup", "list"},
	{"backup", "git", "--", "log"},
	{"backup", "git", "--backup", "20260826T133234Z-223441957", "--", "status"},
	{"backup", "restore"},
	{"backup", "restore", "--yes"},
	{"backup", "restore", "--backup", "20260826T133234Z-223441957", "--yes"},
	{"repo", "validate"},
	{"repo", "list"},
	{"status"},
	{"status", "--porcelain"},
	{"status", "--json"},
	{"add", "-A"},
	{"add", "-u"},
	{"add", "."},
	{"add", "README.md", "src/site.css"},
	{"unstage", "-A"},
	{"unstage", "."},
	{"unstage", "README.md", "src/site.css"},
	{"restore", "README.md"},
	{"restore", "--staged", "README.md", "src/site.css"},
	{"restore", "--worktree", "README.md"},
	{"restore", "--source", "HEAD", "README.md"},
	{"restore", "--source=HEAD", "--staged", "--worktree", "README.md"},
	{"commit", "-m", "message"},
	{"commit", "-F", "-"},
	{"show", "--repository", "public", "--source", "head", "--", "README.md"},
	{"show", "--repository", "public", "--source", "index", "--", "README.md"},
	{"diff"},
	{"diff", "all"},
	{"diff", "website"},
	{"diff", "--staged"},
	{"diff", "all", "--staged"},
	{"log"},
	{"log", "all"},
	{"log", "website"},
	{"log", "-n", "5"},
	{"log", "all", "-n", "5"},
	{"branch"},
	{"branch", "feature/auth"},
	{"branch", "-d", "feature/auth"},
	{"branch", "--delete", "feature/auth"},
	{"switch", "feature/auth"},
	{"switch", "-c", "feature/auth"},
	{"switch", "--create", "feature/auth"},
	{"switch", "feature/auth", "--accept-config-change"},
	{"switch", "feature/auth", "--accept-new-paths"},
	{"fetch"},
	{"fetch", "all"},
	{"fetch", "website"},
	{"pull"},
	{"pull", "all"},
	{"pull", "website"},
	{"pull", "--accept-config-change"},
	{"pull", "all", "--accept-config-change"},
	{"pull", "--accept-new-paths"},
	{"pull", "website", "--accept-new-paths", "--accept-config-change"},
	{"push"},
	{"push", "all"},
	{"push", "website"},
	{"push", "--yes"},
	{"push", "all", "--yes"},
	{"reconfigure"},
	{"reconfigure", "--accept-repository-change"},
	{"reconfigure", "--rename", "notes=journal"},
	{"reconfigure", "--rename", "notes=journal", "--rename", "old=new"},
	{"recover"},
	{"abort"},
	{"doctor"},
	{"agents"},
	{"agents", "update"},
	{"vscode", "info", "--json"},
	{"vscode", "install"},
	{"vscode", "install", "--vsix", "extension.vsix"},
	{"vscode", "install", "--force"},
	{"--version"},
}

// commandOf reports the command a documented form calls, or an empty string
// for a global option such as --version.
func commandOf(form []string) string {
	if strings.HasPrefix(form[0], "-") {
		return ""
	}
	switch form[0] {
	case "backup", "repo", "vscode":
		return form[0] + " " + form[1]
	case "agents":
		if len(form) > 1 && form[1] == "update" {
			return "agents update"
		}
	}
	return form[0]
}

// help runs a help form in an empty directory and returns its output. Help
// must never need a project.
func help(t *testing.T, args ...string) string {
	t.Helper()
	root := t.TempDir()
	var stdout, stderr bytes.Buffer
	if exitCode := cli.Run(args, root, nil, &stdout, &stderr); exitCode != 0 {
		t.Fatalf("exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("help changed the directory: %v %v", entries, err)
	}
	return stdout.String()
}

func TestGlobalHelpListsEverySupportedCommandAndOption(t *testing.T) {
	global := help(t, "--help")
	for group := range cli.HelpGroups {
		if !listsCommand(global, group+" <command>") {
			t.Fatalf("global help is missing the %s command group:\n%s", group, global)
		}
	}
	for _, form := range documentedForms {
		command := commandOf(form)
		if command == "" {
			continue
		}
		if !listsCommand(global, command) {
			t.Fatalf("global help is missing %q:\n%s", command, global)
		}
	}
	for _, option := range []string{"--help", "--version"} {
		if !strings.Contains(global, option) {
			t.Fatalf("global help is missing %q:\n%s", option, global)
		}
	}
	if global != help(t, "--help") {
		t.Fatal("global help is not deterministic")
	}
}

// documentedForms is checked against the global help, so a command missing
// from the list would be documented with no test behind it at all. A command
// group has no call of its own; TestBareInvocationsPrintHelp covers those.
func TestDocumentedFormsCoverEveryCommand(t *testing.T) {
	covered := map[string]bool{}
	for _, form := range documentedForms {
		covered[commandOf(form)] = true
	}
	for _, command := range cli.HelpCommands {
		if _, group := cli.HelpGroups[command]; group || covered[command] {
			continue
		}
		t.Errorf("documentedForms has no form for %q", command)
	}
}

// listsCommand reports whether the help lists the command as an entry of its
// own, so a command that only appears inside prose does not count.
func listsCommand(output, command string) bool {
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == command || strings.HasPrefix(trimmed, command+" ") {
			return true
		}
	}
	return false
}

func TestCommandHelpDocumentsTheAcceptedArguments(t *testing.T) {
	usages := map[string]string{
		"setup":          "gitone setup",
		"clone":          "gitone clone <url> [directory]",
		"init":           "gitone init",
		"migrate":        "gitone migrate [--yes]",
		"backup":         "gitone backup <command>",
		"backup list":    "gitone backup list",
		"backup git":     "gitone backup git [--backup <id>] -- <log|show|diff|status> [arguments...]",
		"backup restore": "gitone backup restore [--backup <id>] [--yes]",
		"repo":           "gitone repo <command>",
		"repo validate":  "gitone repo validate",
		"repo list":      "gitone repo list",
		"status":         "gitone status [--porcelain | --json]",
		"add":            "gitone add [-A | -u | <path>...]",
		"unstage":        "gitone unstage [-A | <path>...]",
		"restore":        "gitone restore [--source HEAD] [--staged] [--worktree] <path>...",
		"commit":         "gitone commit <-m <message> | -F -> [<path>...]",
		"show":           "gitone show --repository <name> --source <head|index> -- <path>",
		"diff":           "gitone diff [all | <repository>] [--staged]",
		"log":            "gitone log [all | <repository>] [-n <count>]",
		"branch":         "gitone branch [<name> | -d <branch>]",
		"switch":         "gitone switch [-c | --create] <branch> [--accept-config-change] [--accept-new-paths]",
		"fetch":          "gitone fetch [all | <repository>]",
		"pull":           "gitone pull [all | <repository>] [--accept-config-change] [--accept-new-paths]",
		"push":           "gitone push [all | <repository>] [--yes]",
		"reconfigure": "gitone reconfigure [--pull | --switch <branch>] [--accept-repository-change] " +
			"[--accept-config-change] [--accept-new-paths] [--rename <old>=<new>]...",
		"recover":        "gitone recover",
		"abort":          "gitone abort",
		"doctor":         "gitone doctor",
		"agents":         "gitone agents",
		"agents update":  "gitone agents update",
		"vscode":         "gitone vscode <command>",
		"vscode info":    "gitone vscode info --json",
		"vscode install": "gitone vscode install [--vsix <path>] [--force]",
	}
	for _, command := range cli.HelpCommands {
		if _, ok := usages[command]; !ok {
			t.Errorf("no expected usage line for %q", command)
		}
	}
	for command, want := range usages {
		t.Run(command, func(t *testing.T) {
			args := append(strings.Split(command, " "), "--help")
			page := help(t, args...)
			if got := usageLine(t, page); got != want {
				t.Fatalf("usage = %q, want %q", got, want)
			}
		})
	}
}

func TestLongCommandHelpStaysConciseAndLinksToTheFullDocumentation(t *testing.T) {
	pages := map[string]string{
		"switch":      "https://gitone.io/docs/usage#switching-branches",
		"pull":        "https://gitone.io/docs/usage#pulling",
		"reconfigure": "https://gitone.io/docs/usage#reconfiguring-the-repositories",
	}
	for command, documentation := range pages {
		t.Run(command, func(t *testing.T) {
			page := help(t, command, "--help")
			if lines := strings.Count(page, "\n"); lines > 24 {
				t.Fatalf("help has %d lines, want at most 24:\n%s", lines, page)
			}
			if !strings.Contains(page, documentation) {
				t.Fatalf("help has no documentation link:\n%s", page)
			}
		})
	}
}

// usageLine returns the single call syntax a help page documents.
func usageLine(t *testing.T, page string) string {
	t.Helper()
	_, usage, found := strings.Cut(page, "Usage:\n  ")
	if !found {
		t.Fatalf("help has no usage:\n%s", page)
	}
	usage, _, _ = strings.Cut(usage, "\n")
	return usage
}

func TestDocumentedFormsAreAcceptedInput(t *testing.T) {
	for _, form := range documentedForms {
		t.Run(strings.Join(form, " "), func(t *testing.T) {
			if commandOf(form) == "vscode install" {
				fakeCode(t, "exit 1\n")
			}
			var stdout, stderr bytes.Buffer
			// An empty directory: a form may fail for a missing project, but
			// never as unsupported input.
			cli.Run(form, t.TempDir(), nil, &stdout, &stderr)
			if strings.HasPrefix(stderr.String(), "CLI001") {
				t.Fatalf("documented form is rejected: %s", stderr.String())
			}
		})
	}
}

func TestHelpRejectsUnsupportedForms(t *testing.T) {
	tests := [][]string{
		{"-h"},
		{"help"},
		{"help", "add"},
		{"--help", "extra"},
		{"--help", "--help"},
		{"add", "--help", "-A"},
		{"repo", "--help", "validate"},
		{"repo", "validate", "list", "--help"},
		{"unknown", "--help"},
		{"--version", "--help"},
	}
	for _, args := range tests {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if exitCode := cli.Run(args, t.TempDir(), nil, &stdout, &stderr); exitCode == 0 {
				t.Fatalf("exit code = 0, stdout = %q", stdout.String())
			}
			if !strings.HasPrefix(stderr.String(), "CLI001 unsupported command") {
				t.Fatalf("stderr = %q", stderr.String())
			}
		})
	}
}

// TestBareInvocationsPrintHelp covers the commands that have no action of
// their own: gitone itself and every command group answer with the page a
// person needs next instead of rejecting the call.
func TestBareInvocationsPrintHelp(t *testing.T) {
	if got, want := help(t), help(t, "--help"); got != want {
		t.Fatalf("gitone = %q, want the global help %q", got, want)
	}
	for _, group := range []string{"repo", "backup", "vscode"} {
		t.Run(group, func(t *testing.T) {
			if got, want := help(t, group), help(t, group, "--help"); got != want {
				t.Fatalf("gitone %s = %q, want the group help %q", group, got, want)
			}
		})
	}
}

// TestRejectedInputNamesItsHelpPage covers the other half: input that stays
// rejected names the one help page that answers it.
func TestRejectedInputNamesItsHelpPage(t *testing.T) {
	tests := []struct {
		args []string
		hint string
	}{
		{[]string{"log", "-n", "nope"}, `Run "gitone log --help" for the arguments it accepts.`},
		{[]string{"repo", "unknown"}, `Run "gitone repo --help" for the arguments it accepts.`},
		{[]string{"backup", "git", "--", "gc"}, `Run "gitone backup git --help" for the arguments it accepts.`},
		{[]string{"unknown"}, `Run "gitone --help" for the supported commands.`},
		{[]string{"-h"}, `Run "gitone --help" for the supported commands.`},
	}
	for _, test := range tests {
		t.Run(strings.Join(test.args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if exitCode := cli.Run(test.args, t.TempDir(), nil, &stdout, &stderr); exitCode != 1 {
				t.Fatalf("exit code = %d, want 1", exitCode)
			}
			got := stderr.String()
			if !strings.HasPrefix(got, "CLI001 unsupported command") || !strings.HasSuffix(got, test.hint+"\n") {
				t.Fatalf("stderr = %q, want the CLI001 prefix and %q", got, test.hint)
			}
		})
	}
}
