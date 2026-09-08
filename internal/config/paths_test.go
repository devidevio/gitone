package config_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/devidevio/gitone/internal/config"
)

const patchable = `# project policy
version: 1
default_branch: main
repositories:
  public:
    visibility: public
    paths:
      - .gitone.yml   # the policy itself
      - README.md
      # everything below src belongs here
  private:
    visibility: private
    paths:
      - secrets/**
`

func TestAddPathsKeepsEveryUnrelatedByte(t *testing.T) {
	patched, err := config.AddPaths([]byte(patchable), "public", []string{"CHANGELOG.md", "docs/**"})
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(patchable,
		"      - README.md\n",
		"      - README.md\n      - \"CHANGELOG.md\"\n      - \"docs/**\"\n", 1)
	if string(patched) != want {
		t.Fatalf("patched =\n%s\nwant\n%s", patched, want)
	}
}

func TestAddPathsQuotesExactFileNamesForYAML(t *testing.T) {
	patterns := []string{"release #1.md", "a: b.md", " report.md ", "report?.md", `say "hello".md`}
	patched, err := config.AddPaths([]byte(patchable), "public", patterns)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Projected(patched, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.Repositories["public"].Paths[2:]; !slices.Equal(got, patterns) {
		t.Fatalf("paths = %q, want %q", got, patterns)
	}
}

func TestAddPathsProducesALoadableConfiguration(t *testing.T) {
	patched, err := config.AddPaths([]byte(patchable), "public", []string{"CHANGELOG.md"})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Projected(patched, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.Repositories["public"].Paths; len(got) != 3 || got[2] != "CHANGELOG.md" {
		t.Fatalf("paths = %v", got)
	}
}

func TestAddPathsIsIdempotent(t *testing.T) {
	once, err := config.AddPaths([]byte(patchable), "public", []string{"CHANGELOG.md"})
	if err != nil {
		t.Fatal(err)
	}
	twice, err := config.AddPaths(once, "public", []string{"CHANGELOG.md"})
	if err != nil {
		t.Fatal(err)
	}
	if string(twice) != string(once) {
		t.Fatalf("second patch changed the file:\n%s", twice)
	}
}

func TestAddPathsRefusesAListItCannotExtend(t *testing.T) {
	flow := "version: 1\ndefault_branch: main\nrepositories:\n  public:\n    visibility: public\n    paths: [README.md]\n"
	if _, err := config.AddPaths([]byte(flow), "public", []string{"CHANGELOG.md"}); err == nil {
		t.Fatal("a flow sequence was patched")
	}
	if _, err := config.AddPaths([]byte(patchable), "missing", []string{"CHANGELOG.md"}); err == nil {
		t.Fatal("an absent repository was patched")
	}
}

func TestPathsFileFollowsTheOverridingList(t *testing.T) {
	root := t.TempDir()
	write := func(name, contents string) {
		if err := os.WriteFile(filepath.Join(root, name), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(config.PublicFile, patchable)
	if file, err := config.PathsFile(root, "public"); err != nil || file != config.PublicFile {
		t.Fatalf("PathsFile = %q, %v", file, err)
	}
	write(config.LocalFile, "version: 1\nrepositories:\n  public:\n    visibility: public\n    paths:\n      - README.md\n  private:\n    visibility: private\n")
	if file, err := config.PathsFile(root, "public"); err != nil || file != config.LocalFile {
		t.Fatalf("overridden PathsFile = %q, %v", file, err)
	}
	if file, err := config.PathsFile(root, "private"); err != nil || file != config.PublicFile {
		t.Fatalf("untouched PathsFile = %q, %v", file, err)
	}
}
