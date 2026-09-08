package worktree

import (
	"maps"
	"slices"
	"strings"

	"github.com/devidevio/gitone/internal/config"
	"github.com/devidevio/gitone/internal/link"
)

// Suggestion is one owned-path pattern a project can be offered, together
// with the relevant paths it currently covers.
type Suggestion struct {
	Pattern string
	Paths   []string
}

// Suggest inventories a project that has no configuration yet and proposes
// one ownership pattern per top-level entry: a relevant root file or accepted
// root symbolic link as its exact path, a relevant root directory as
// "directory/**". It also returns every relevant path, which is what tells a
// custom pattern whether it matches anything today.
//
// The inventory is the one Scan uses, so an ignored path never appears. No
// repository exists yet, so the link check asks every question except the ones
// about ownership; the complete validation before the write asks those. Every
// PATH003 problem it does answer is returned, because no ownership answer can
// make those paths safe.
func Suggest(root string) (suggestions []Suggestion, relevant []string, unsafe []*Issue, err error) {
	inventory, err := inspect(root, nil)
	if err != nil {
		return nil, nil, nil, err
	}
	found := inventory.relevant
	unsafe = inventory.issues
	source := &unowned{tree{root: root, entries: known(found, inventory.links)}}
	for _, name := range inventory.links {
		if reason := acceptLink(source, source.path(name), name); reason != "" {
			unsafe = append(unsafe, &Issue{Code: PathUnsafe, Path: name, Detail: reason})
			continue
		}
		found[name] = true
	}
	slices.SortFunc(unsafe, func(a, b *Issue) int { return strings.Compare(a.Path, b.Path) })

	covered := map[string][]string{}
	for name := range found {
		relevant = append(relevant, name)
		pattern := name
		if directory, _, nested := strings.Cut(name, "/"); nested {
			pattern = directory + "/**"
		} else if name == config.PublicFile || name == config.ProjectIgnoreFile {
			// Setup assigns its own project files after the repositories.
			continue
		}
		covered[pattern] = append(covered[pattern], name)
	}
	slices.Sort(relevant)
	for _, pattern := range slices.Sorted(maps.Keys(covered)) {
		paths := covered[pattern]
		slices.Sort(paths)
		suggestions = append(suggestions, Suggestion{Pattern: pattern, Paths: paths})
	}
	return suggestions, relevant, unsafe, nil
}

// unowned answers the safe-link questions of a project without a
// configuration: every path has the same imaginary owner, so link.Check
// applies every rule except the ones that need one.
type unowned struct {
	tree
}

func (*unowned) Owner(string) (string, config.OwnerIssue, string) {
	return "", config.OwnerValid, ""
}

var _ link.Tree = (*unowned)(nil)
