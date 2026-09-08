package agents_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUsageGuideLinksToAgentInstructions(t *testing.T) {
	path := filepath.Join("..", "..", "docs", "USAGE.md")
	guide, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	const link = "[canonical agent instructions](https://github.com/devidevio/gitone/blob/main/internal/agents/instructions.md)"
	if !strings.Contains(string(guide), link) {
		t.Errorf("%s must link to internal/agents/instructions.md", path)
	}
}
