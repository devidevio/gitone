package setup

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"charm.land/huh/v2"
	"gopkg.in/yaml.v3"

	"github.com/devidevio/gitone/internal/config"
)

// template is the decoded active part of a template: everything the loader
// would see before a single comment is removed.
type templateFields struct {
	Version       int    `yaml:"version"`
	DefaultBranch string `yaml:"default_branch"`
	Rules         any    `yaml:"rules"`
	Repositories  map[string]struct {
		Visibility string            `yaml:"visibility"`
		Push       string            `yaml:"push"`
		Remote     string            `yaml:"remote"`
		Remotes    map[string]string `yaml:"remotes"`
		Paths      []string          `yaml:"paths"`
	} `yaml:"repositories"`
}

func decode(t *testing.T, contents string) templateFields {
	t.Helper()
	var decoded templateFields
	if err := yaml.Unmarshal([]byte(contents), &decoded); err != nil {
		t.Fatalf("the template is not valid YAML: %v", err)
	}
	return decoded
}

func TestPublicTemplateActivatesOnlyTheRequiredFields(t *testing.T) {
	decoded := decode(t, publicTemplate)
	if decoded.Version != 1 || decoded.DefaultBranch != "main" || decoded.Rules != nil {
		t.Fatalf("project fields = %+v", decoded)
	}
	if len(decoded.Repositories) != 1 {
		t.Fatalf("repositories = %v", decoded.Repositories)
	}
	repository, ok := decoded.Repositories["<repository-name>"]
	if !ok {
		t.Fatalf("the placeholder repository is missing: %v", decoded.Repositories)
	}
	if config.ValidateName("<repository-name>") == nil {
		t.Error("the placeholder name is a usable repository name")
	}
	if repository.Visibility != "public" || repository.Push != "" ||
		repository.Remote != "" || repository.Remotes != nil {
		t.Errorf("repository fields = %+v", repository)
	}
	if !slices.Equal(repository.Paths, []string{config.ProjectIgnoreFile, config.PublicFile}) {
		t.Errorf("paths = %v", repository.Paths)
	}
}

func TestLocalTemplateActivatesOnlyTheVersion(t *testing.T) {
	decoded := decode(t, localTemplate)
	if decoded.Version != 1 || len(decoded.Repositories) != 0 {
		t.Fatalf("local template = %+v", decoded)
	}
}

func TestTemplatesMatchGoldenFiles(t *testing.T) {
	for name, got := range map[string]string{
		config.PublicFile: publicTemplate,
		config.LocalFile:  localTemplate,
	} {
		want, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		if got != string(want) {
			t.Errorf("%s does not match its golden file\nwant:\n%s\ngot:\n%s", name, want, got)
		}
	}
}

// The templates replace the configuration guide while a project is edited, so
// every optional field and every alternative value has to appear in them.
func TestTemplatesDocumentEverySupportedFieldAndAlternative(t *testing.T) {
	for _, documented := range []string{
		"rules:", "protected_paths:", "secrets/**", "require_clean_worktree: true",
		"public or private", "push: disabled", "allowed (default)",
		"remote: git@", "remotes:", "origin:", "TODO",
		"README.md        # one exact file", "src/**", "\"*.md\"", "docs/**/*.md",
	} {
		if !strings.Contains(publicTemplate, documented) {
			t.Errorf("%s does not document %q", config.PublicFile, documented)
		}
	}
	for _, documented := range []string{
		"never committed", ".gitignore", "visibility: private", "push: disabled",
		"remote: git@", "paths replace", "protected_paths extend",
	} {
		if !strings.Contains(localTemplate, documented) {
			t.Errorf("%s does not document %q", config.LocalFile, documented)
		}
	}
}

func TestSetupModeDefaultsToTheWizardOnBothTerminals(t *testing.T) {
	// Pressing Enter keeps the preselected value on a normal terminal, which
	// running the field without touching it models.
	interactive := &prompt{output: io.Discard, runField: func(huh.Field) error { return nil }}
	got, ok := interactive.mode()
	if !ok || got != wizardMode {
		t.Fatalf("interactive mode = %q, %v", got, ok)
	}

	for answer, want := range map[string]string{"\n": wizardMode, "Template\n": templateMode} {
		input := strings.NewReader(answer)
		plain := &prompt{reader: bufio.NewReader(input), input: input, output: io.Discard, plain: true}
		got, ok := plain.mode()
		if !ok || got != want {
			t.Fatalf("plain mode for %q = %q, %v", answer, got, ok)
		}
	}
}
