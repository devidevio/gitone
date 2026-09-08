package config

import (
	"maps"
	"slices"
	"strconv"
	"strings"
)

// Change is one semantic difference between two effective configurations,
// named by its configuration field and shown as the values a person compares.
// Structural marks the differences that reconfigure the repository set or a
// remote; GitOne never applies those from incoming repository content.
type Change struct {
	Field      string
	Before     string
	After      string
	Structural bool
}

// Differences lists every semantic difference between two effective
// configurations, in a stable field order. Formatting, comments and the order
// of list entries are not differences: a list is compared as the set it is.
func Differences(before, after *Config) []Change {
	var changes []Change
	add := func(field, was, is string, structural bool) {
		if was != is {
			changes = append(changes, Change{Field: field, Before: was, After: is, Structural: structural})
		}
	}
	add("version", strconv.Itoa(before.Version), strconv.Itoa(after.Version), false)
	add("default_branch", before.DefaultBranch, after.DefaultBranch, false)
	add("rules.protected_paths", list(before.ProtectedPaths), list(after.ProtectedPaths), false)
	add("rules.push.require_clean_worktree",
		strconv.FormatBool(before.Push.RequireCleanWorktree),
		strconv.FormatBool(after.Push.RequireCleanWorktree), false)

	for _, name := range union(before.Repositories, after.Repositories) {
		field := "repositories." + name
		was, existed := before.Repositories[name]
		is, exists := after.Repositories[name]
		if existed != exists {
			// An added or removed repository is one structural difference:
			// its fields are not a change a person can review separately.
			add(field, present(existed), present(exists), true)
			continue
		}
		add(field+".visibility", was.Visibility, is.Visibility, false)
		add(field+".push", was.Push, is.Push, false)
		add(field+".paths", list(was.Paths), list(is.Paths), false)
		for _, remote := range union(was.Remotes, is.Remotes) {
			add(field+".remotes."+remote, value(was.Remotes[remote]), value(is.Remotes[remote]), true)
		}
	}
	return changes
}

// Structural reports whether any change reconfigures the repository set or a
// remote.
func Structural(changes []Change) []Change {
	return slices.DeleteFunc(slices.Clone(changes), func(change Change) bool { return !change.Structural })
}

func union[V any](before, after map[string]V) []string {
	names := slices.Sorted(maps.Keys(before))
	for _, name := range slices.Sorted(maps.Keys(after)) {
		names = appendUnique(names, name)
	}
	slices.Sort(names)
	return names
}

// list renders a configured list as the set it is, so reordering it is not a
// semantic change.
func list(values []string) string {
	if len(values) == 0 {
		return "(none)"
	}
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	return strings.Join(slices.Compact(sorted), ", ")
}

func value(configured string) string {
	if configured == "" {
		return "(none)"
	}
	return configured
}

func present(exists bool) string {
	if exists {
		return "configured"
	}
	return "(absent)"
}
