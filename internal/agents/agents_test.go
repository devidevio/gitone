package agents_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/devidevio/gitone/internal/agents"
	"github.com/devidevio/gitone/internal/config"
)

// block is the file GitOne writes: the canonical text inside its markers.
func block() string {
	return "<!-- gitone:agents:start -->\n" + agents.Instructions() + "<!-- gitone:agents:end -->\n"
}

func temp(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func write(t *testing.T, root, contents string, permissions os.FileMode) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, agents.File), []byte(contents), permissions); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, root string) string {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join(root, agents.File))
	if err != nil {
		t.Fatal(err)
	}
	return string(contents)
}

// configuration is a valid project whose one repository owns the paths.
func configuration(t *testing.T, paths ...string) *config.Config {
	t.Helper()
	document := "version: 1\ndefault_branch: main\nrepositories:\n  website:\n    visibility: public\n    paths:\n"
	for _, owned := range paths {
		document += "      - " + owned + "\n"
	}
	return loadDocument(t, document)
}

// loadDocument validates a complete committed configuration the ordinary way.
func loadDocument(t *testing.T, document string) *config.Config {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".gitone.yml"), []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, _, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	return loaded
}

// TestStates walks the complete file-state table: what each state is
// classified as, and what an update leaves behind.
func TestStates(t *testing.T) {
	tests := map[string]struct {
		contents string // the file before, or "" for no file at all
		state    agents.State
		want     string // the file after an update
	}{
		"missing": {
			state: agents.Missing,
			want:  block(),
		},
		"ordinary file": {
			contents: "# House rules\n\nBe nice.\n",
			state:    agents.Absent,
			want:     "# House rules\n\nBe nice.\n\n" + block(),
		},
		"ordinary file without a final newline": {
			contents: "# House rules",
			state:    agents.Absent,
			want:     "# House rules\n\n" + block(),
		},
		"ordinary file with extra final newlines": {
			contents: "# House rules\n\n\n",
			state:    agents.Absent,
			want:     "# House rules\n\n\n" + block(),
		},
		"empty file": {
			contents: "",
			state:    agents.Absent,
			want:     block(),
		},
		"exact block without markers": {
			contents: "# Rules\n\n" + agents.Instructions(),
			state:    agents.Unmarked,
			want:     "# Rules\n\n" + block(),
		},
		"current marked block": {
			contents: "# Rules\n\n" + block() + "\nAnd more.\n",
			state:    agents.Current,
			want:     "# Rules\n\n" + block() + "\nAnd more.\n",
		},
		"stale marked block": {
			contents: "# Rules\n\n<!-- gitone:agents:start -->\nThis project uses GitOne: an older block.\n<!-- gitone:agents:end -->\n\nAnd more.\n",
			state:    agents.Stale,
			want:     "# Rules\n\n" + block() + "\nAnd more.\n",
		},
		"locally modified marked block": {
			contents: "<!-- gitone:agents:start -->\nedited by hand\n<!-- gitone:agents:end -->\n",
			state:    agents.Stale,
			want:     block(),
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			root := temp(t)
			if tt.state != agents.Missing {
				write(t, root, tt.contents, 0o644)
			}
			state, err := agents.Inspect(root)
			if err != nil {
				t.Fatalf("inspect: %v", err)
			}
			if state != tt.state {
				t.Fatalf("state = %d, want %d", state, tt.state)
			}
			updated, err := agents.Update(root)
			if err != nil {
				t.Fatalf("update: %v", err)
			}
			if updated != tt.state {
				t.Errorf("update reported state %d, want %d", updated, tt.state)
			}
			if got := read(t, root); got != tt.want {
				t.Fatalf("file =\n%q\nwant\n%q", got, tt.want)
			}
			// A second update is a no-op on every state.
			if state, err := agents.Update(root); err != nil || state != agents.Current {
				t.Fatalf("second update: state %d, %v", state, err)
			}
			if got := read(t, root); got != tt.want {
				t.Fatalf("the second update changed the file:\n%q", got)
			}
		})
	}
}

// TestRefusedStates covers every file GitOne must not edit by itself.
func TestRefusedStates(t *testing.T) {
	tests := map[string]struct {
		contents string
		want     string
	}{
		"unmarked instructions that are not current": {
			contents: "This project uses GitOne: an older block nobody marked.\n",
			want:     "neither marked nor exactly the current block",
		},
		"duplicate current blocks": {
			contents: block() + "\n" + block(),
			want:     "repeated or misordered",
		},
		"duplicate unmarked blocks": {
			contents: agents.Instructions() + "\n" + agents.Instructions(),
			want:     "neither marked nor exactly the current block",
		},
		"marked and unmarked instructions": {
			contents: block() + "\n" + agents.Instructions(),
			want:     "marked and unmarked",
		},
		"missing end marker": {
			contents: "<!-- gitone:agents:start -->\n" + agents.Instructions(),
			want:     "repeated or misordered",
		},
		"missing start marker": {
			contents: agents.Instructions() + "<!-- gitone:agents:end -->\n",
			want:     "repeated or misordered",
		},
		"misordered markers": {
			contents: "<!-- gitone:agents:end -->\ntext\n<!-- gitone:agents:start -->\n",
			want:     "repeated or misordered",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			root := temp(t)
			write(t, root, tt.contents, 0o644)
			assertRefused(t, root, tt.want)
			if got := read(t, root); got != tt.contents {
				t.Fatalf("the refused file was changed:\n%q", got)
			}
		})
	}
}

func TestRefusesNonRegularFiles(t *testing.T) {
	for name, create := range map[string]func(t *testing.T, root string){
		"symbolic link": func(t *testing.T, root string) {
			target := filepath.Join(temp(t), "elsewhere.md")
			if err := os.WriteFile(target, []byte("secret\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(root, agents.File)); err != nil {
				t.Fatal(err)
			}
		},
		"directory": func(t *testing.T, root string) {
			if err := os.Mkdir(filepath.Join(root, agents.File), 0o755); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := temp(t)
			create(t, root)
			assertRefused(t, root, "is not a regular file")
			info, err := os.Lstat(filepath.Join(root, agents.File))
			if err != nil || info.Mode().IsRegular() {
				t.Fatalf("the refused path was replaced: %v, %v", info, err)
			}
		})
	}
}

func assertRefused(t *testing.T, root, want string) {
	t.Helper()
	for _, err := range []error{second(agents.Inspect(root)), second(agents.Update(root))} {
		if err == nil || !strings.HasPrefix(err.Error(), agents.Failed) || !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %v, want %s ... %s", err, agents.Failed, want)
		}
		if !strings.Contains(err.Error(), agents.Link) {
			t.Errorf("the refusal does not name the permanent explanation: %v", err)
		}
	}
}

func second(_ agents.State, err error) error { return err }

func TestUpdateKeepsPermissions(t *testing.T) {
	root := temp(t)
	write(t, root, "# Rules\n", 0o600)
	if _, err := agents.Update(root); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(root, agents.File))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode = %v, want 0600", got)
	}
	// A created file is an ordinary readable one.
	created := temp(t)
	if _, err := agents.Update(created); err != nil {
		t.Fatal(err)
	}
	if info, err = os.Stat(filepath.Join(created, agents.File)); err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Fatalf("created mode = %v, want 0644", got)
	}
}

func TestUpdateLeavesNoTemporaryFile(t *testing.T) {
	root := temp(t)
	if _, err := agents.Update(root); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != agents.File {
		t.Fatalf("directory = %v", entries)
	}
}

func TestOwnership(t *testing.T) {
	tests := map[string]struct {
		paths []string
		want  string
	}{
		"owned":      {paths: []string{".gitone.yml", "AGENTS.md"}},
		"unassigned": {paths: []string{".gitone.yml"}, want: "PATH001"},
		"case conflict": {
			paths: []string{".gitone.yml", "agents.md"},
			want:  "PATH003",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			err := agents.Ownership(configuration(t, tt.paths...))
			switch {
			case tt.want == "" && err != nil:
				t.Fatalf("error = %v, want none", err)
			case tt.want != "" && (err == nil || !strings.HasPrefix(err.Error(), tt.want)):
				t.Fatalf("error = %v, want %s", err, tt.want)
			}
		})
	}
}

func TestOwnershipRefusesAFileTwoRepositoriesClaim(t *testing.T) {
	document := `version: 1
default_branch: main
repositories:
  website:
    visibility: public
    paths:
      - .gitone.yml
      - AGENTS.md
  docs:
    visibility: public
    paths:
      - "*.md"
`
	if err := agents.Ownership(loadDocument(t, document)); err == nil || !strings.HasPrefix(err.Error(), "PATH002") {
		t.Fatalf("error = %v, want PATH002", err)
	}
}

func TestOwnershipRefusesAProtectedFile(t *testing.T) {
	document := `version: 1
default_branch: main
rules:
  protected_paths:
    - AGENTS.md
repositories:
  website:
    visibility: public
    paths:
      - .gitone.yml
      - AGENTS.md
`
	if err := agents.Ownership(loadDocument(t, document)); err == nil || !strings.HasPrefix(err.Error(), "PATH003") {
		t.Fatalf("error = %v, want PATH003", err)
	}
}

func TestCheckReportsEveryContentState(t *testing.T) {
	owning := configuration(t, ".gitone.yml", "AGENTS.md")
	tests := map[string]struct {
		contents string
		want     string
	}{
		"missing":  {want: "does not exist"},
		"ordinary": {contents: "# Rules\n", want: "does not hold the GitOne instructions"},
		"stale": {
			contents: "<!-- gitone:agents:start -->\nold\n<!-- gitone:agents:end -->\n",
			want:     "not current",
		},
		"current":  {contents: block()},
		"unmarked": {contents: agents.Instructions()},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			root := temp(t)
			if tt.contents != "" {
				write(t, root, tt.contents, 0o644)
			}
			err := agents.Check(owning, root)
			switch {
			case tt.want == "":
				if err != nil {
					t.Fatalf("error = %v, want none", err)
				}
			case err == nil || !strings.HasPrefix(err.Error(), agents.Failed) || !strings.Contains(err.Error(), tt.want):
				t.Fatalf("error = %v, want %s ... %s", err, agents.Failed, tt.want)
			}
		})
	}
}

func TestCheckReportsOwnershipBeforeContent(t *testing.T) {
	root := temp(t)
	write(t, root, block(), 0o644)
	if err := agents.Check(configuration(t, ".gitone.yml"), root); err == nil || !strings.HasPrefix(err.Error(), "PATH001") {
		t.Fatalf("error = %v, want PATH001", err)
	}
}
