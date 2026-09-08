package clone

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPublishNeverReplacesAnExistingDestination(t *testing.T) {
	parent := t.TempDir()
	project := filepath.Join(parent, "project")
	destination := filepath.Join(parent, "destination")
	if err := os.Mkdir(project, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(destination, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := publish(project, destination); err == nil {
		t.Fatal("publish replaced an existing destination")
	}
	if _, err := os.Stat(project); err != nil {
		t.Fatalf("project was moved: %v", err)
	}
}
