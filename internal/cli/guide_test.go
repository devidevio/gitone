package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/devidevio/gitone/internal/agents"
	"github.com/devidevio/gitone/internal/cli"
)

// Keep the usage guide and agent instructions in sync with every supported
// leaf command exposed by CLI help.
func TestUsageGuideAndAgentInstructionsListEveryCommand(t *testing.T) {
	path := filepath.Join("..", "..", "docs", "USAGE.md")
	guide, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	for _, command := range cli.HelpCommands {
		// A group has no syntax of its own; the guide lists its subcommands.
		if _, group := cli.HelpGroups[command]; group {
			continue
		}
		if !strings.Contains(agents.Instructions(), "`"+command+"`") {
			t.Errorf("agent instructions omit %q", command)
		}
		if row := "| `gitone " + command; !strings.Contains(string(guide), row) {
			t.Errorf("%s has no command table row for %q", path, "gitone "+command)
		}
	}
}
