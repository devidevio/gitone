package migrate

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/devidevio/gitone/internal/git"
)

func validBackup(t *testing.T) string {
	t.Helper()
	parent := t.TempDir()
	backup := filepath.Join(parent, "backup")
	if _, err := git.RunIsolated(parent, "init", "--bare", backup); err != nil {
		t.Fatal(err)
	}
	return backup
}

func assertCleanPublishFailure(t *testing.T, root, backup string, err error) {
	t.Helper()
	if err == nil || !strings.HasPrefix(err.Error(), "MIG001 ") {
		t.Fatalf("publish error = %v", err)
	}
	if _, err := os.Lstat(backup); err != nil {
		t.Fatalf("backup changed: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(root, ".gitone-restore-*"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("temporary restored repositories = %v, %v", matches, err)
	}
}

func TestPublishCleansUpAfterValidationFailure(t *testing.T) {
	root := t.TempDir()
	backup := filepath.Join(t.TempDir(), "invalid-backup")
	if err := os.Mkdir(backup, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, ".git")

	err := publish(root, backup, target)
	assertCleanPublishFailure(t, root, backup, err)
	if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("target exists after validation failure: %v", err)
	}
}

func TestPublishNeverReplacesRacedTarget(t *testing.T) {
	root := t.TempDir()
	backup := validBackup(t)
	target := filepath.Join(root, ".git")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(target, "keep")
	if err := os.WriteFile(marker, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := publish(root, backup, target)
	assertCleanPublishFailure(t, root, backup, err)
	if contents, err := os.ReadFile(marker); err != nil || string(contents) != "keep" {
		t.Fatalf("raced target changed: %q, %v", contents, err)
	}
}

func TestPublishCleansUpAfterPublicationFailure(t *testing.T) {
	root := t.TempDir()
	backup := validBackup(t)
	target := filepath.Join(root, "missing", ".git")

	err := publish(root, backup, target)
	assertCleanPublishFailure(t, root, backup, err)
	if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("target exists after publication failure: %v", err)
	}
}
