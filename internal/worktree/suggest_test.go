package worktree_test

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/devidevio/gitone/internal/worktree"
)

// suggested prepares a project without configuration, exactly as setup sees
// it, and returns the offered patterns, every relevant path and the problems
// no ownership answer can fix.
func suggested(t *testing.T, files map[string]string, links map[string]string) ([]worktree.Suggestion, []string, []*worktree.Issue) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for name, contents := range files {
		write(t, root, name, contents)
	}
	for name, target := range links {
		symlink(t, root, target, name)
	}
	suggestions, relevant, unsafe, err := worktree.Suggest(root)
	if err != nil {
		t.Fatal(err)
	}
	return suggestions, relevant, unsafe
}

func patterns(suggestions []worktree.Suggestion) []string {
	offered := make([]string, len(suggestions))
	for index, suggestion := range suggestions {
		offered[index] = suggestion.Pattern
	}
	return offered
}

func TestSuggestCollapsesDirectoriesAndSkipsIgnoredAndProjectPaths(t *testing.T) {
	suggestions, relevant, unsafe := suggested(t, map[string]string{
		"README.md":                "readme",
		".hidden":                  "hidden",
		".gitignore":               "build/\n",
		".gitone/lock":             "state",
		".github/workflows/ci.yml": "ci",
		"docs/guide.md":            "guide",
		"docs/nested/deep.md":      "deep",
		"build/out.js":             "ignored",
	}, nil)

	if len(unsafe) != 0 {
		t.Fatalf("unsafe = %v, want none", unsafePaths(unsafe))
	}
	want := []string{".github/**", ".hidden", "README.md", "docs/**"}
	if got := patterns(suggestions); !reflect.DeepEqual(got, want) {
		t.Fatalf("patterns = %v, want %v", got, want)
	}
	wantPaths := []string{".github/workflows/ci.yml", ".gitignore", ".hidden", "README.md", "docs/guide.md", "docs/nested/deep.md"}
	if !reflect.DeepEqual(relevant, wantPaths) {
		t.Fatalf("relevant = %v, want %v", relevant, wantPaths)
	}
	for _, suggestion := range suggestions {
		if suggestion.Pattern != "docs/**" {
			continue
		}
		if want := []string{"docs/guide.md", "docs/nested/deep.md"}; !reflect.DeepEqual(suggestion.Paths, want) {
			t.Fatalf("docs/** covers %v, want %v", suggestion.Paths, want)
		}
	}
}

func TestSuggestOffersAcceptedLinksOnly(t *testing.T) {
	suggestions, _, unsafe := suggested(t, map[string]string{
		"README.md":     "readme",
		"docs/guide.md": "guide",
	}, map[string]string{
		"readme.link": "README.md",
		"shared":      "docs",
		"chain":       "readme.link",
		"escape":      "/etc/passwd",
		"outside":     "../secret",
	})

	want := []string{"README.md", "docs/**", "readme.link", "shared"}
	if got := patterns(suggestions); !reflect.DeepEqual(got, want) {
		t.Fatalf("patterns = %v, want %v", got, want)
	}
	// A rejected link is owner-independent, so setup can refuse it before it
	// asks anything.
	if want := []string{"chain", "escape", "outside"}; !reflect.DeepEqual(unsafePaths(unsafe), want) {
		t.Fatalf("unsafe = %v, want %v", unsafePaths(unsafe), want)
	}
	for _, issue := range unsafe {
		if issue.Code != worktree.PathUnsafe {
			t.Fatalf("%v is not a %s issue", issue, worktree.PathUnsafe)
		}
	}
}

// unsafePaths returns the reported path of every returned problem.
func unsafePaths(issues []*worktree.Issue) []string {
	paths := make([]string, len(issues))
	for index, issue := range issues {
		paths[index] = issue.Path
	}
	return paths
}

func TestSuggestReportsNestedRepository(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	write(t, root, "README.md", "readme")
	initRepository(t, filepath.Join(root, "vendor"))

	_, relevant, unsafe, err := worktree.Suggest(root)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"vendor"}; !reflect.DeepEqual(unsafePaths(unsafe), want) {
		t.Fatalf("unsafe = %v, want %v", unsafePaths(unsafe), want)
	}
	if want := []string{"README.md"}; !reflect.DeepEqual(relevant, want) {
		t.Fatalf("relevant = %v, want %v", relevant, want)
	}
}

func TestSuggestOffersNothingWithoutRelevantFiles(t *testing.T) {
	suggestions, relevant, unsafe := suggested(t, map[string]string{
		".gitignore":   "build/\n",
		"build/out.js": "ignored",
	}, nil)

	if len(suggestions) != 0 {
		t.Fatalf("patterns = %v, want none", patterns(suggestions))
	}
	if len(unsafe) != 0 {
		t.Fatalf("unsafe = %v, want none", unsafePaths(unsafe))
	}
	if want := []string{".gitignore"}; !reflect.DeepEqual(relevant, want) {
		t.Fatalf("relevant = %v, want %v", relevant, want)
	}
}
