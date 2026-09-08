// Package link decides whether one symbolic link is safe for GitOne to
// manage. The working tree, a managed index and an incoming Git tree ask the
// same questions about their own entries, so the decision lives here once and
// every caller only supplies the lookups of its own source.
package link

import (
	"path"
	"slices"
	"strings"

	"github.com/devidevio/gitone/internal/config"
)

// Mode is the Git entry mode of a symbolic link. An index or tree entry
// carrying it stores the link target as its blob content.
const Mode = "120000"

// Kind is what a path is, observed without ever following a symbolic link.
type Kind int

const (
	Missing Kind = iota
	Regular
	Directory
	Symbolic
	// Other is everything GitOne does not manage: a socket, a device, a FIFO
	// or a Git submodule entry.
	Other
)

// Tree is one source a link and its target are looked up in.
type Tree interface {
	// Kind reports what a project-relative path is, without following a link.
	Kind(relativePath string) (Kind, error)
	// Relevant lists the project-relative paths GitOne considers below a
	// directory. An ignored tree stays uninspected and lists nothing.
	Relevant(directory string) ([]string, error)
	// Owner returns the single repository that may manage a path, or the
	// reason no repository may.
	Owner(relativePath string) (string, config.OwnerIssue, string)
}

// Control reports whether relativePath is a path GitOne must always find as
// itself: its own configuration and internal state, the ignore files and Git
// metadata. A link may neither be nor reach one of them.
func Control(relativePath string) bool {
	if config.IsReserved(relativePath) || relativePath == config.PublicFile {
		return true
	}
	return slices.ContainsFunc(strings.Split(relativePath, "/"), func(segment string) bool {
		return segment == ".git" || segment == config.ProjectIgnoreFile
	})
}

// Resolve turns the stored target of a link into a project-relative path, or
// reports why the link can never be managed. It is purely lexical, so every
// source rejects the same targets before looking anything up.
func Resolve(linkPath, target string) (resolved, reason string) {
	switch {
	case target == "":
		return "", "the link target is empty"
	case path.IsAbs(target):
		return "", "absolute link targets are not supported"
	}
	resolved = path.Join(path.Dir(linkPath), target)
	if resolved == "." || resolved == ".." || strings.HasPrefix(resolved, "../") {
		return "", "the link target escapes the project root"
	}
	if err := config.ValidatePath(resolved); err != nil {
		return "", "the link target " + err.Error()
	}
	return resolved, ""
}

// Check reports why the link at linkPath must not be managed, or the empty
// string when it is safe. resolved is the project-relative target it names.
func Check(tree Tree, linkPath, target string) (resolved, reason string) {
	resolved, reason = Resolve(linkPath, target)
	if reason != "" {
		return "", reason
	}
	if Control(linkPath) || Control(resolved) {
		return resolved, "GitOne configuration, ignore and Git metadata paths must stay regular files"
	}
	owner, _, reason := tree.Owner(linkPath)
	if reason != "" {
		return resolved, reason
	}
	for _, ancestor := range ancestors(linkPath, resolved) {
		if _, issue, reason := tree.Owner(ancestor); issue == config.OwnerUnsafe {
			return resolved, ancestor + ": " + reason
		}
		switch kind, err := tree.Kind(ancestor); {
		case err != nil:
			return resolved, ancestor + " cannot be inspected"
		case kind == Symbolic:
			return resolved, "the link resolves through the symbolic link " + ancestor
		}
	}
	if _, issue, reason := tree.Owner(resolved); issue == config.OwnerUnsafe {
		return resolved, "the link target " + resolved + ": " + reason
	}
	kind, err := tree.Kind(resolved)
	switch {
	case err != nil:
		return resolved, "the link target " + resolved + " cannot be inspected"
	case kind == Missing:
		return resolved, "the link target " + resolved + " does not exist"
	case kind == Symbolic:
		return resolved, "the link target " + resolved + " is a symbolic link: chains and cycles are not supported"
	case kind == Regular:
		return resolved, owned(tree, owner, resolved)
	case kind == Directory:
		return resolved, directory(tree, owner, resolved)
	}
	return resolved, "the link target " + resolved + " is not a regular file or directory"
}

// owned reports why one concrete target path does not belong to the same
// repository as the link.
func owned(tree Tree, owner, target string) string {
	name, _, reason := tree.Owner(target)
	switch {
	case reason != "":
		return "the link target " + target + ": " + reason
	case name != owner:
		return "the link target " + target + " belongs to " + name + ", not " + owner
	}
	return ""
}

// directory reports why a directory target cannot prove it belongs to the
// link's repository. An empty or fully ignored directory proves nothing, so
// it is refused instead of being accepted on trust.
func directory(tree Tree, owner, target string) string {
	relevant, err := tree.Relevant(target)
	if err != nil {
		return "the link target " + target + " cannot be inspected"
	}
	if len(relevant) == 0 {
		return "the link target " + target + " has no managed content"
	}
	for _, name := range relevant {
		if reason := owned(tree, owner, name); reason != "" {
			return reason
		}
	}
	return ""
}

// ancestors are the directory components the given paths are resolved
// through, nearest the project root first.
func ancestors(paths ...string) []string {
	var list []string
	for _, name := range paths {
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			if !slices.Contains(list, parent) {
				list = append(list, parent)
			}
		}
	}
	slices.Sort(list)
	return list
}
