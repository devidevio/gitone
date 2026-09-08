package setup

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/devidevio/gitone/internal/config"
	"github.com/devidevio/gitone/internal/repository"
)

// entry is one collected repository. The exported fields are the configured
// ones and are written as they are; the unexported fields only record where
// the repository goes and are never part of a file.
type entry struct {
	name  string
	local bool
	// derived are the project files setup assigned itself. They are part of
	// Paths, but never appear in the owned-path question.
	derived []string

	Visibility string   `yaml:"visibility"`
	Remote     string   `yaml:"remote,omitempty"`
	Paths      []string `yaml:"paths"`
}

// owned are the patterns the user chose, without the assigned project files.
func (e *entry) owned() []string {
	return slices.DeleteFunc(slices.Clone(e.Paths), func(owned string) bool {
		return slices.Contains(e.derived, owned)
	})
}

// own replaces the chosen patterns and keeps the assigned project files, so a
// correction can never lose or duplicate them.
func (e *entry) own(paths []string) {
	e.Paths = append(slices.Clone(paths), e.derived...)
	slices.Sort(e.Paths)
}

// releaseProjectFiles drops only the project files setup assigned itself, so
// other derived paths such as a created AGENTS.md keep their owner.
func (e *entry) releaseProjectFiles() {
	e.Paths = slices.DeleteFunc(e.Paths, func(owned string) bool {
		return slices.Contains(projectFiles, owned) && slices.Contains(e.derived, owned)
	})
	e.derived = slices.DeleteFunc(e.derived, func(owned string) bool {
		return slices.Contains(projectFiles, owned)
	})
}

type document struct {
	Version       int               `yaml:"version"`
	DefaultBranch string            `yaml:"default_branch,omitempty"`
	Repositories  map[string]*entry `yaml:"repositories"`
}

// files is the configuration setup generates. Every repository is written
// completely to exactly one file, so the two files never override each other.
type files struct {
	defaultBranch string
	entries       []*entry
}

// document renders one configuration file, or an empty string when the local
// file is not needed. The committed file always exists, because it is what
// makes the directory a project.
func (f *files) document(local bool) string {
	repositories := map[string]*entry{}
	for _, collected := range f.entries {
		if collected.local == local {
			repositories[collected.name] = collected
		}
	}
	if local && len(repositories) == 0 {
		return ""
	}
	contents := new(bytes.Buffer)
	encoder := yaml.NewEncoder(contents)
	encoder.SetIndent(2)
	rendered := document{Version: 1, Repositories: repositories}
	if !local {
		rendered.DefaultBranch = f.defaultBranch
	}
	if err := encoder.Encode(rendered); err != nil {
		// Encoding a fixed struct of validated strings cannot fail.
		panic(err)
	}
	_ = encoder.Close()
	return contents.String()
}

// load validates the generated files with the ordinary strict loader, so
// setup can never write configuration the project itself would reject.
func (f *files) load() (*config.Config, error) {
	directory, err := os.MkdirTemp("", "gitone-setup")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(directory)

	for name, contents := range f.generated() {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(contents), 0o600); err != nil {
			return nil, err
		}
	}
	configuration, _, err := config.Load(directory)
	return configuration, err
}

// generated returns the files to write, keyed by name and in a fixed order.
func (f *files) generated() map[string]string {
	generated := map[string]string{config.PublicFile: f.document(false)}
	if local := f.document(true); local != "" {
		generated[config.LocalFile] = local
	}
	return generated
}

func (f *files) describe(output io.Writer) {
	for _, name := range []string{config.PublicFile, config.LocalFile} {
		contents, ok := f.generated()[name]
		if !ok {
			continue
		}
		fmt.Fprintf(output, "%s\n\n", name)
		for _, line := range strings.Split(strings.TrimRight(contents, "\n"), "\n") {
			fmt.Fprintf(output, "  %s\n", line)
		}
		fmt.Fprintln(output)
	}
}

// write publishes the generated configuration.
func (f *files) write(root string) error {
	generated := f.generated()
	return publish(root, generated[config.PublicFile], generated[config.LocalFile])
}

// publish writes the configuration of one setup run, whether it was generated
// by the wizard or is a template. The ignore entries exist before any
// configuration file does, so private configuration is never visible to Git,
// and an existing file is never replaced. An empty local document is not
// written at all.
func publish(root, public, local string) error {
	if err := repository.UpdateIgnore(root); err != nil {
		return err
	}
	if local != "" {
		if err := writeNew(filepath.Join(root, config.LocalFile), local, 0o600); err != nil {
			return err
		}
	}
	if err := writeNew(filepath.Join(root, config.PublicFile), public, 0o644); err != nil {
		if local != "" {
			_ = os.Remove(filepath.Join(root, config.LocalFile))
		}
		return err
	}
	return nil
}

// writeNew writes one configuration file atomically and never replaces an
// existing one.
func writeNew(name, contents string, permissions os.FileMode) error {
	temporary, err := os.CreateTemp(filepath.Dir(name), "."+filepath.Base(name)+".*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(permissions); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.WriteString(contents); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Link(temporaryName, name); errors.Is(err, os.ErrExist) {
		return fail("%s already exists: setup never overwrites configuration\n%s", name, unchanged)
	} else if err != nil {
		return err
	}
	return nil
}
