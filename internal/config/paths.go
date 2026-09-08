package config

import (
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// PathsFile is the project-relative configuration file that supplies the
// effective paths list of one repository. The local file wins whenever it
// defines the list at all, because merging replaces the committed list; every
// other repository keeps its committed one.
func PathsFile(root, name string) (string, error) {
	local, err := decodeOptionalFile(filepath.Join(root, LocalFile))
	if err != nil {
		return "", err
	}
	if local != nil {
		if overriding := local.Repositories[name]; overriding != nil && overriding.Paths != nil {
			return LocalFile, nil
		}
	}
	return PublicFile, nil
}

// AddPaths appends patterns to the paths sequence of one repository and
// returns the patched file. Only whole lines are inserted, at the end of that
// one sequence, so comments, ordering, formatting and every unrelated byte
// stay exactly as they were.
func AddPaths(contents []byte, name string, patterns []string) ([]byte, error) {
	if len(patterns) == 0 {
		return contents, nil
	}
	sequence, err := pathsSequence(contents, name)
	if err != nil {
		return nil, err
	}
	// Adding a pattern the list already holds is a no-op, so replaying an
	// interrupted addition can never duplicate one.
	listed := make(map[string]bool, len(sequence.Content))
	for _, item := range sequence.Content {
		listed[item.Value] = true
	}
	patterns = slices.DeleteFunc(slices.Clone(patterns), func(pattern string) bool { return listed[pattern] })
	if len(patterns) == 0 {
		return contents, nil
	}
	last := sequence.Content[len(sequence.Content)-1]
	lines := strings.Split(string(contents), "\n")
	if last.Line < 1 || last.Line > len(lines) {
		return nil, invalid("repository %q has an unreadable paths list", name)
	}
	// An item scalar starts two columns behind its dash, so the dash column
	// of the added lines is the one the existing entries already use.
	indent := strings.Repeat(" ", max(last.Column-3, 0))
	ending := ""
	if strings.HasSuffix(lines[last.Line-1], "\r") {
		ending = "\r"
	}
	added := make([]string, len(patterns))
	for index, pattern := range patterns {
		added[index] = indent + "- " + strconv.Quote(pattern) + ending
	}
	patched := append(append(append([]string{}, lines[:last.Line]...), added...), lines[last.Line:]...)
	return []byte(strings.Join(patched, "\n")), nil
}

// pathsSequence is the block sequence holding the owned paths of one
// repository. A flow or empty sequence has no line to append to, so it is
// refused instead of being reformatted.
func pathsSequence(contents []byte, name string) (*yaml.Node, error) {
	var document yaml.Node
	if err := yaml.Unmarshal(contents, &document); err != nil {
		return nil, configError("CONFIG002", "invalid YAML", err)
	}
	if document.Kind != yaml.DocumentNode || len(document.Content) == 0 {
		return nil, invalid("the file holds no configuration")
	}
	sequence := field(field(field(document.Content[0], "repositories"), name), "paths")
	if sequence == nil || sequence.Kind != yaml.SequenceNode || sequence.Style == yaml.FlowStyle || len(sequence.Content) == 0 {
		return nil, invalid("repository %q has no block paths list to extend: add the paths by hand", name)
	}
	return sequence, nil
}

func field(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for index := 0; index+1 < len(node.Content); index += 2 {
		if node.Content[index].Value == key {
			return node.Content[index+1]
		}
	}
	return nil
}

// RemoteFile is the project-relative configuration file that supplies the
// effective URL of one repository remote. The local file wins whenever it
// defines that remote at all; every other remote keeps the committed URL.
func RemoteFile(root, name, remote string) (string, error) {
	local, err := decodeOptionalFile(filepath.Join(root, LocalFile))
	if err != nil {
		return "", err
	}
	if local != nil {
		if err := normalizeRemotes(local); err != nil {
			return "", err
		}
		if overriding := local.Repositories[name]; overriding != nil && overriding.Remotes[remote] != nil {
			return LocalFile, nil
		}
	}
	return PublicFile, nil
}
