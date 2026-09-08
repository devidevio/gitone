package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/devidevio/gitone/internal/config"
	"github.com/devidevio/gitone/internal/git"
)

func TestLoadPublicConfigurationFromDescendant(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".gitone.yml"), `
version: 1
default_branch: main
repositories:
  public:
    remote: git@example.com:public.git
    visibility: public
    paths:
      - .gitone.yml
      - src/**
`)
	descendant := filepath.Join(root, "src", "nested")
	if err := os.MkdirAll(descendant, 0o755); err != nil {
		t.Fatal(err)
	}

	got, gotRoot, err := config.Load(descendant)
	if err != nil {
		t.Fatal(err)
	}
	if gotRoot != root {
		t.Fatalf("root = %q, want %q", gotRoot, root)
	}
	want := config.Repository{
		Visibility: "public",
		Push:       config.PushAllowed,
		Paths:      []string{".gitone.yml", "src/**"},
		Remotes:    map[string]string{"origin": "git@example.com:public.git"},
	}
	if !reflect.DeepEqual(got.Repositories["public"], want) {
		t.Fatalf("repository = %#v, want %#v", got.Repositories["public"], want)
	}
	if got.DefaultBranch != "main" {
		t.Fatalf("default branch = %q, want main", got.DefaultBranch)
	}
}

func TestLoadPolicies(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".gitone.yml"), `
version: 1
default_branch: main
rules:
  protected_paths:
    - .env
    - secrets/**
  push:
    require_clean_worktree: true
repositories:
  public:
    visibility: public
    push: disabled
    paths:
      - .gitone.yml
      - src/**
`)

	got, _, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if got.DefaultBranch != "main" || !got.Push.RequireCleanWorktree {
		t.Fatalf("rules = %#v", got)
	}
	public := got.Repositories["public"]
	if public.Push != config.PushDisabled {
		t.Fatalf("repository = %#v", public)
	}
	matcher, err := got.Matcher()
	if err != nil {
		t.Fatal(err)
	}
	for _, protected := range []string{".env", ".ENV", "secrets/key.txt"} {
		if !matcher.Protected(protected) || !strings.Contains(matcher.Unowned("public", protected), "protected") {
			t.Fatalf("%q is not protected", protected)
		}
	}
}

func TestLoadMergesPolicies(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".gitone.yml"), `
version: 1
default_branch: main
rules:
  protected_paths: [.env]
  push:
    require_clean_worktree: true
repositories:
  public:
    visibility: public
    push: disabled
    paths: [.gitone.yml]
`)
	writeFile(t, filepath.Join(root, ".gitone.local.yml"), `
rules:
  protected_paths: [secrets/**]
  push:
    require_clean_worktree: false
repositories:
  public:
    push: allowed
`)

	got, _, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.ProtectedPaths, []string{".env", "secrets/**"}) {
		t.Fatalf("protected paths = %v", got.ProtectedPaths)
	}
	if !got.Push.RequireCleanWorktree || got.Repositories["public"].Push != config.PushDisabled {
		t.Fatalf("the local file weakened the committed push rules = %#v, repository = %#v", got.Push, got.Repositories["public"])
	}
}

func TestLoadLetsTheLocalFileTightenPushRules(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".gitone.yml"), `
version: 1
default_branch: main
repositories:
  public:
    visibility: public
    paths: [.gitone.yml]
`)
	writeFile(t, filepath.Join(root, ".gitone.local.yml"), `
rules:
  push:
    require_clean_worktree: true
repositories:
  public:
    push: disabled
`)

	got, _, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Push.RequireCleanWorktree || got.Repositories["public"].Push != config.PushDisabled {
		t.Fatalf("push rules = %#v, repository = %#v", got.Push, got.Repositories["public"])
	}
}

func TestLoadMergesLocalConfiguration(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".gitone.yml"), `
version: 1
default_branch: main
repositories:
  public:
    visibility: public
    paths: [src/**]
    remotes:
      origin: git@example.com:public.git
      upstream: git@example.com:upstream.git
`)
	writeFile(t, filepath.Join(root, ".gitone.local.yml"), `
repositories:
  public:
    visibility: private
    paths: [app/**]
    remotes:
      origin: null
      upstream: git@example.com:local-upstream.git
  private:
    visibility: private
    paths: [tasks/**]
`)

	got, _, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if names := got.RepositoryNames(); !reflect.DeepEqual(names, []string{"private", "public"}) {
		t.Fatalf("names = %v", names)
	}
	public := got.Repositories["public"]
	if !reflect.DeepEqual(public.Paths, []string{"app/**"}) {
		t.Fatalf("paths = %v", public.Paths)
	}
	if public.Visibility != "private" || got.DefaultBranch != "main" {
		t.Fatalf("merged scalars = visibility %q, branch %q", public.Visibility, got.DefaultBranch)
	}
	wantRemotes := map[string]string{
		"origin":   "git@example.com:public.git",
		"upstream": "git@example.com:local-upstream.git",
	}
	if !reflect.DeepEqual(public.Remotes, wantRemotes) {
		t.Fatalf("remotes = %v, want %v", public.Remotes, wantRemotes)
	}
}

func TestLoadFailsClosed(t *testing.T) {
	tests := []struct {
		name string
		body string
		code string
	}{
		{name: "malformed", body: "version: [", code: "CONFIG002"},
		{name: "unknown field", body: "version: 1\nunknown: true\nrepositories: {}\n", code: "CONFIG002"},
		{name: "repository default branch", body: `version: 1
default_branch: main
repositories:
  public:
    visibility: public
    default_branch: main
    paths: [.gitone.yml]
`, code: "CONFIG002"},
		{name: "duplicate key", body: "version: 1\nversion: 1\nrepositories: {}\n", code: "CONFIG002"},
		{name: "unsupported version", body: "version: 3\nrepositories: {}\n", code: "CONFIG003"},
		{name: "empty file", body: "", code: "CONFIG003"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, filepath.Join(root, ".gitone.yml"), tt.body)
			_, _, err := config.Load(root)
			assertCode(t, err, tt.code)
		})
	}

	_, _, err := config.Load(t.TempDir())
	assertCode(t, err, "CONFIG001")
}

func TestValidateRejectsInvalidRepositoryConfiguration(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "conflicting remote shorthand",
			body: validConfig(`remote: git@example.com:one.git
    remotes:
      origin: git@example.com:two.git`),
			want: "conflicting origin",
		},
		{
			name: "duplicate path across repositories",
			body: validConfig("paths: [src/**]") + `
  private:
    visibility: private
    paths: [src/**]
`,
			want: "duplicates",
		},
		{
			name: "null origin",
			body: validConfig(`paths: [src/**]
    remotes:
      origin: null`),
			want: "invalid remote",
		},
		{
			name: "duplicate path within repository",
			body: validConfig("paths: [src/**, src/**]"),
			want: "duplicates",
		},
		{
			name: "case insensitive duplicate",
			body: validConfig("paths: [SRC/**, src/**]"),
			want: "case-insensitively",
		},
		{
			name: "unicode case insensitive duplicate",
			body: validConfig("paths: [Σ/**, ς/**]"),
			want: "case-insensitively",
		},
		{
			name: "invalid project default branch",
			body: `version: 1
default_branch: ""
repositories:
  public:
    visibility: public
    paths: [.gitone.yml]
`,
			want: "default_branch",
		},
		{
			name: "reserved local file",
			body: validConfig("paths: [.gitone.local.yml]"),
			want: "reserved",
		},
		{
			name: "reserved state directory",
			body: validConfig("paths: [.gitone/**]"),
			want: "reserved",
		},
		{
			name: "reserved repository name",
			body: `version: 1
default_branch: main
repositories:
  all:
    visibility: public
    paths: [src/**]
`,
			want: "invalid",
		},
		{
			name: "configuration file is protected",
			body: `version: 1
default_branch: main
rules:
  protected_paths: [.gitone.yml]
repositories:
  public:
    visibility: public
    paths: [.gitone.yml]
`,
			want: "must be owned",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, filepath.Join(root, ".gitone.yml"), tt.body)
			_, _, err := config.Load(root)
			assertCode(t, err, "CONFIG003")
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %q does not contain %q", err, tt.want)
			}
		})
	}
}

func TestValidateRejectsIdentifiersNativeGitRejects(t *testing.T) {
	branches := []string{"bad branch", "-dash", "bad..name", "name.lock", "HEAD"}
	for _, branch := range branches {
		t.Run("branch "+branch, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, filepath.Join(root, ".gitone.yml"), `version: 1
default_branch: `+strconv.Quote(branch)+`
repositories:
  public:
    visibility: public
    paths: [.gitone.yml]
`)
			_, _, err := config.Load(root)
			assertCode(t, err, "CONFIG003")
			if !strings.Contains(err.Error(), "default_branch") {
				t.Fatalf("error %q does not name default_branch", err)
			}
		})
	}

	remotes := []string{"bad name", "-dash", "re:mote", "remote.lock"}
	for _, remote := range remotes {
		t.Run("remote "+remote, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, filepath.Join(root, ".gitone.yml"), `version: 1
default_branch: main
repositories:
  public:
    visibility: public
    paths: [.gitone.yml]
    remotes:
      `+strconv.Quote(remote)+`: git@example.com:public.git
`)
			_, _, err := config.Load(root)
			assertCode(t, err, "CONFIG003")
			if !strings.Contains(err.Error(), "invalid remote") {
				t.Fatalf("error %q does not report an invalid remote", err)
			}
		})
	}
}

// A remote named only in the ignored local file is merged before validation,
// so it is refused exactly like a committed one.
func TestValidateRejectsALocalRemoteName(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".gitone.yml"), `version: 1
default_branch: main
repositories:
  public:
    visibility: public
    paths: [.gitone.yml]
`)
	writeFile(t, filepath.Join(root, ".gitone.local.yml"), `version: 1
repositories:
  public:
    remotes:
      "bad name": git@example.com:public.git
`)
	_, _, err := config.Load(root)
	assertCode(t, err, "CONFIG003")
}

func TestValidatePathGrammar(t *testing.T) {
	invalid := []string{
		"/absolute",
		"../outside",
		"./relative",
		"src/../outside",
		"src/**/**/file.go",
		"src/a**",
		"src/**b/file.go",
		"src/***",
		"src/",
		"src//file.go",
		"src\\file.go",
		"control\x1f",
	}
	for _, pattern := range invalid {
		t.Run(pattern, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, filepath.Join(root, ".gitone.yml"), validConfig("paths: ["+quoteYAML(pattern)+"]"))
			_, _, err := config.Load(root)
			assertCode(t, err, "CONFIG003")
		})
	}

	valid := []string{"src/*", "src/**/file.go", "*.md", "**/*.md", "**", "docs/**/*.md", "a*b*c.go",
		"src?", "src/[ab].go", "src/{a,b}.go"}
	for _, pattern := range valid {
		t.Run(pattern, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, filepath.Join(root, ".gitone.yml"), validConfig("paths: ["+quoteYAML(pattern)+", .gitone.yml]"))
			if _, _, err := config.Load(root); err != nil {
				t.Fatalf("Load() = %v, want the pattern accepted", err)
			}
		})
	}
}

func validConfig(repositoryFields string) string {
	return `version: 1
default_branch: main
repositories:
  public:
    visibility: public
    ` + repositoryFields + "\n"
}

func quoteYAML(value string) string {
	return strconv.Quote(value)
}

func writeFile(t *testing.T, name, contents string) {
	t.Helper()
	if err := os.WriteFile(name, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertCode(t *testing.T, err error, want string) {
	t.Helper()
	var configErr *config.Error
	if !errors.As(err, &configErr) {
		t.Fatalf("error = %v, want *config.Error", err)
	}
	if configErr.Code != want {
		t.Fatalf("code = %q, want %q", configErr.Code, want)
	}
}

// An unavailable git is an operational failure of GitOne, not a rejected
// configured name.
func TestValidateReportsAnUnavailableGit(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".gitone.yml"), `version: 1
default_branch: main
repositories:
  public:
    visibility: public
    paths: [.gitone.yml]
`)
	t.Setenv("PATH", t.TempDir())

	_, _, err := config.Load(root)
	if !errors.Is(err, git.ErrUnavailable) {
		t.Fatalf("error = %v, want ErrUnavailable", err)
	}
}
