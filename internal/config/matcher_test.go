package config_test

import (
	"strings"
	"testing"

	"github.com/devidevio/gitone/internal/config"
)

// matcherConfig builds a loaded-equivalent configuration without touching the
// filesystem, so the grammar can be exercised path by path.
func matcherConfig(t *testing.T, protected []string, repositories map[string][]string) *config.Matcher {
	t.Helper()
	configuration := &config.Config{
		Version:        1,
		DefaultBranch:  "main",
		ProtectedPaths: protected,
		Repositories:   map[string]config.Repository{},
	}
	for name, paths := range repositories {
		configuration.Repositories[name] = config.Repository{Visibility: "public", Push: config.PushAllowed, Paths: paths}
	}
	matcher, err := configuration.Matcher()
	if err != nil {
		t.Fatalf("Matcher() = %v", err)
	}
	return matcher
}

func TestMatcherGrammar(t *testing.T) {
	tests := []struct {
		pattern string
		match   []string
		reject  []string
	}{
		{
			pattern: "README.md",
			match:   []string{"README.md"},
			reject:  []string{"readme.md", "docs/README.md", "README.mdx"},
		},
		{
			pattern: "src/**",
			match:   []string{"src/site.css", "src/lib/parser.ts", "src/.env.example"},
			reject:  []string{"src", "srcx/a.go", "a/src/b.go"},
		},
		{
			pattern: "docs/**/*.md",
			match:   []string{"docs/README.md", "docs/guide/intro.md", "docs/a/b/c.md", "docs/.hidden.md"},
			reject:  []string{"docs/guide/intro.txt", "README.md", "docs"},
		},
		{
			pattern: "**/*.md",
			match:   []string{"README.md", "docs/README.md", "a/b/c.md", ".hidden.md"},
			reject:  []string{"README.txt", "docs"},
		},
		{
			pattern: "*",
			match:   []string{"README.md", ".env.example"},
			reject:  []string{"src/site.css"},
		},
		{
			pattern: "src/*.test.*.go",
			match:   []string{"src/a.test.unit.go"},
			reject:  []string{"src/a.test.go", "src/x/a.test.unit.go"},
		},
		{
			pattern: "**",
			match:   []string{"README.md", "a/b/c.md"},
			reject:  []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.pattern, func(t *testing.T) {
			matcher := matcherConfig(t, nil, map[string][]string{"public": {tt.pattern}})
			for _, name := range tt.match {
				if owners, _ := matcher.Owners(name); len(owners) != 1 {
					t.Errorf("Owners(%q) = %v, want [public]", name, owners)
				}
			}
			for _, name := range tt.reject {
				if owners, _ := matcher.Owners(name); len(owners) != 0 {
					t.Errorf("Owners(%q) = %v, want none", name, owners)
				}
			}
		})
	}
}

func TestMatcherOverlappingPatterns(t *testing.T) {
	// Several patterns of one repository may match; the repository is still
	// reported once and no priority decision happens.
	same := matcherConfig(t, nil, map[string][]string{"public": {"docs/**", "**/*.md", "docs/guide/intro.md"}})
	owners, fold := same.Owners("docs/guide/intro.md")
	if len(owners) != 1 || owners[0] != "public" || len(fold) != 1 {
		t.Fatalf("Owners = %v, %v, want [public] once", owners, fold)
	}

	// Different repositories may overlap theoretically. Only a concrete path
	// matching both is ambiguous, and only that path.
	across := matcherConfig(t, nil, map[string][]string{"public": {"src/**"}, "private": {"**/*.secret"}})
	if reason := across.Unowned("public", "src/main.go"); reason != "" {
		t.Fatalf("Unowned(src/main.go) = %q, want owned", reason)
	}
	reason := across.Unowned("public", "src/keys.secret")
	if !strings.Contains(reason, "private") || !strings.Contains(reason, "public") {
		t.Fatalf("Unowned(src/keys.secret) = %q, want both repositories named", reason)
	}
}

func TestMatcherProtectedPathsUseTheSameGrammar(t *testing.T) {
	matcher := matcherConfig(t, []string{"**/*.pem", "secrets/**"}, map[string][]string{"public": {"src/**"}})
	for _, name := range []string{"key.pem", "src/key.pem", "secrets/deep/token.txt"} {
		if !matcher.Protected(name) {
			t.Errorf("Protected(%q) = false, want true", name)
		}
		if reason := matcher.Unowned("public", name); reason != config.ProtectedReason {
			t.Errorf("Unowned(%q) = %q, want the protected reason", name, reason)
		}
	}
	// The deny is case-insensitive, ownership is not.
	if !matcher.Protected("KEY.PEM") {
		t.Error("Protected(KEY.PEM) = false, want true")
	}
	if matcher.Protected("src/main.go") {
		t.Error("Protected(src/main.go) = true, want false")
	}
}

func TestMatcherCaseSensitivity(t *testing.T) {
	matcher := matcherConfig(t, nil, map[string][]string{"public": {"src/**"}})
	owners, fold := matcher.Owners("SRC/main.go")
	if len(owners) != 0 || len(fold) != 1 {
		t.Fatalf("Owners(SRC/main.go) = %v, %v, want no exact owner and one folded owner", owners, fold)
	}
	if reason := matcher.Unowned("public", "SRC/main.go"); !strings.Contains(reason, "case is ignored") {
		t.Fatalf("Unowned(SRC/main.go) = %q, want a case warning", reason)
	}
}

func TestOwns(t *testing.T) {
	tests := []struct {
		pattern string
		path    string
		want    bool
	}{
		{"*.yml", ".gitone.yml", true},
		{".gitone.yml", ".gitone.yml", true},
		{"src/**", ".gitone.yml", false},
		{"src/*", "src/a/b.go", false},
		{"src/[ab].go", "src/a.go", false},
		{"src/[ab].go", "src/[ab].go", true},
		{"report?.md", "report1.md", false},
		{"report?.md", "report?.md", true},
		{"notes/{draft}.md", "notes/{draft}.md", true},
	}
	for _, tt := range tests {
		if got := config.Owns(tt.pattern, tt.path); got != tt.want {
			t.Errorf("Owns(%q, %q) = %v, want %v", tt.pattern, tt.path, got, tt.want)
		}
	}
}
