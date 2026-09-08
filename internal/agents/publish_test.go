package agents

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPublishRollsBackAChangedSnapshot(t *testing.T) {
	for name, change := range map[string]func(t *testing.T, target string){
		"same file changed": func(t *testing.T, target string) {
			if err := os.WriteFile(target, []byte("changed"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"file replaced": func(t *testing.T, target string) {
			if err := os.Rename(target, target+".old"); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(target, []byte("changed"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			target := filepath.Join(root, File)
			if err := os.WriteFile(target, []byte("original"), 0o600); err != nil {
				t.Fatal(err)
			}
			expected, err := os.Lstat(target)
			if err != nil {
				t.Fatal(err)
			}
			change(t, target)

			err = publish(target, "updated", expected.Mode().Perm(), expected, "original")
			if err == nil || !strings.HasPrefix(err.Error(), Failed) {
				t.Fatalf("error = %v, want %s", err, Failed)
			}
			contents, readErr := os.ReadFile(target)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if got := string(contents); got != "changed" {
				t.Fatalf("file = %q, want the concurrent change", got)
			}
		})
	}
}
