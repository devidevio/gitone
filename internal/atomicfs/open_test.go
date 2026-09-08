package atomicfs_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/devidevio/gitone/internal/atomicfs"
)

func TestOpenNoFollowRefusesSymbolicLink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	link := filepath.Join(root, "link")
	if err := os.WriteFile(target, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if file, err := atomicfs.OpenNoFollow(link); err == nil {
		_ = file.Close()
		t.Fatal("symbolic link was opened")
	}
}
