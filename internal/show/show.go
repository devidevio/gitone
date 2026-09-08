// Package show reads one owned path from a managed repository without exposing
// GitOne's internal Git directory layout to editor integrations.
package show

import (
	"errors"
	"fmt"
	"strings"

	"github.com/devidevio/gitone/internal/config"
	"github.com/devidevio/gitone/internal/git"
	"github.com/devidevio/gitone/internal/lock"
	"github.com/devidevio/gitone/internal/repository"
	"github.com/devidevio/gitone/internal/status"
	"github.com/devidevio/gitone/internal/worktree"
)

const MissingCode = "SHOW001"

// Sources are the contents show can read. The CLI rejects every other value
// as unsupported syntax before Content runs, so this list is the only place
// the accepted sources are decided.
var Sources = []string{"head", "index"}

// MissingError reports that a valid owned path has no entry in the selected
// source. Editor integrations use it as an empty side of a diff.
type MissingError struct {
	Repository string
	Source     string
	Path       string
}

func (e *MissingError) Error() string {
	return fmt.Sprintf("%s repository %q source %s has no path %q", MissingCode, e.Repository, e.Source, e.Path)
}

// Content returns the exact blob bytes stored for path in HEAD or the index.
// The project is inspected under a shared lock without being refused as a
// whole: only an issue on the requested path stops the read, so unrelated
// project issues do not block read-only editor diffs.
func Content(configuration *config.Config, root, directory, name, source, path string) (string, error) {
	release, err := lock.AcquireRead(root)
	if err != nil {
		return "", err
	}
	defer release()

	result, inventory, err := status.Inspect(configuration, root)
	if err != nil {
		return "", err
	}
	if _, exists := configuration.Repositories[name]; !exists {
		return "", fmt.Errorf("REPO001 repository %q is not configured", name)
	}
	relative, err := worktree.Relative(root, directory, path)
	if err != nil {
		return "", err
	}
	// Recorded ownership only reflects the configured rules. An issue naming
	// the path reports every other way it can lack one clear owner, such as a
	// second repository index tracking it, and refuses the read with its own
	// stable code instead of the guess the rules alone would produce.
	for _, issue := range result.Issues {
		if issue.Path == relative {
			return "", errors.New(issue.String())
		}
	}
	if owner := inventory.Owners[relative]; owner != name {
		if owner == "" {
			return "", fmt.Errorf("PATH003 %s: path is not managed by GitOne", path)
		}
		return "", fmt.Errorf("PATH003 %s: path is owned by %s, not %s", path, owner, name)
	}

	gitDirectory := repository.Directory(root, name)
	var object string
	switch source {
	case "head":
		hasHead, err := repository.HeadExists(root, name)
		if err != nil {
			return "", err
		}
		if !hasHead {
			return "", missing(name, source, relative)
		}
		entry, err := git.Run(root, "--literal-pathspecs", "--git-dir="+gitDirectory, "ls-tree", "-z", "HEAD", "--", relative)
		if err != nil {
			return "", err
		}
		object = treeObject(entry)
	case "index":
		entry, err := git.Run(root, "--literal-pathspecs", "--git-dir="+gitDirectory, "--work-tree="+root,
			"ls-files", "--stage", "-z", "--", relative)
		if err != nil {
			return "", err
		}
		object = indexObject(entry)
	}
	if object == "" {
		return "", missing(name, source, relative)
	}
	return git.Run(root, "--git-dir="+gitDirectory, "cat-file", "blob", object)
}

func treeObject(entry string) string {
	header, _, found := strings.Cut(strings.TrimSuffix(entry, "\x00"), "\t")
	fields := strings.Fields(header)
	if !found || len(fields) != 3 || fields[1] != "blob" {
		return ""
	}
	return fields[2]
}

func indexObject(entries string) string {
	for _, entry := range strings.Split(entries, "\x00") {
		header, _, found := strings.Cut(entry, "\t")
		fields := strings.Fields(header)
		if found && len(fields) == 3 && fields[2] == "0" {
			return fields[1]
		}
	}
	return ""
}

func missing(repository, source, path string) error {
	return &MissingError{Repository: repository, Source: source, Path: path}
}
