// Package worktree inventories the managed working tree and resolves the
// owning repository of every relevant path. It fails closed: a path that
// cannot be assigned to exactly one repository produces a stable error
// instead of a guess.
package worktree

import (
	"errors"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/devidevio/gitone/internal/config"
	"github.com/devidevio/gitone/internal/git"
	"github.com/devidevio/gitone/internal/link"
)

// PathUnassigned, PathAmbiguous and PathUnsafe are the stable codes of the
// three ownership problems a working tree can have. Only the first two can be
// corrected by choosing different owned paths.
const (
	PathUnassigned = "PATH001"
	PathAmbiguous  = "PATH002"
	PathUnsafe     = "PATH003"
)

// Issue is a stable, machine-readable problem found while inventorying or
// resolving the working tree.
type Issue struct {
	Code   string
	Path   string
	Detail string
}

func (i *Issue) Error() string {
	if i.Path == "" {
		return i.Code + " " + i.Detail
	}
	return i.Code + " " + i.Path + ": " + i.Detail
}

// Result is the complete ownership view of one working tree.
type Result struct {
	Owners map[string]string
	Issues []*Issue
	// Tracked are the project-relative paths the managed indexes hold. They
	// stay relevant even once ignored.
	Tracked []string
	// Ignored are the project-relative paths the ignore rules hide and no
	// managed repository tracks directly. A directory entry ends with a slash
	// and stands for everything below it except IgnoreExceptions.
	Ignored []string
	// IgnoreExceptions are tracked paths below an ignored directory. They must
	// not inherit that directory's ignored state in editor integrations.
	IgnoreExceptions []string

	issued map[string]bool
}

// inventory is the one classification of the non-ignored worktree entries
// shared by ownership validation and setup suggestions.
type inventory struct {
	entries  []string
	relevant map[string]bool
	links    []string
	issues   []*Issue
}

func (r *Result) add(code, relativePath, detail string) {
	r.Issues = append(r.Issues, &Issue{Code: code, Path: relativePath, Detail: detail})
	r.issued[relativePath] = true
}

// Scan inventories the working tree below root and resolves ownership for
// every relevant path. Tracked lists the project-relative paths already
// tracked by a managed repository; they stay relevant even once ignored.
func Scan(configuration *config.Config, root string, tracked []string) (*Result, error) {
	return scan(configuration, root, tracked, nil)
}

// Prepared inventories the working tree a transaction would leave behind.
// replaced maps every project-relative path the transaction changes to the
// file holding its prepared version; a prepared version that does not exist
// stands for a removed path. Nothing else is affected, so the result is
// exactly the state the transaction would end in.
func Prepared(configuration *config.Config, root string, tracked []string, replaced map[string]string) (*Result, error) {
	return scan(configuration, root, tracked, replaced)
}

func scan(configuration *config.Config, root string, tracked []string, replaced map[string]string) (*Result, error) {
	result := &Result{Owners: map[string]string{}, Tracked: slices.Clone(tracked), issued: map[string]bool{}}

	if info, err := os.Lstat(filepath.Join(root, ".git")); err == nil && info.IsDir() {
		result.add(PathUnsafe, ".git", "the project root is a Git repository: run gitone migrate")
	} else if err == nil {
		result.add(PathUnsafe, ".git", ".git is not a directory: linked worktrees and submodules are not supported")
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	inventory, err := inspect(root, replaced)
	if err != nil {
		return nil, err
	}
	checkBareRepositories(root, inventory.entries, tracked, result)
	for _, issue := range inventory.issues {
		result.add(issue.Code, issue.Path, issue.Detail)
	}
	relevant := inventory.relevant
	links := inventory.links

	for _, entry := range tracked {
		if err := config.ValidatePath(entry); err != nil {
			result.add(PathUnsafe, entry, err.Error())
			continue
		}
		if config.IsReserved(entry) {
			result.add(PathUnsafe, entry, "reserved GitOne paths must not be tracked")
			continue
		}
		relevant[entry] = true
	}

	hidden, err := listIgnored(root)
	if err != nil {
		return nil, err
	}
	for _, name := range hidden {
		if path.Base(name) != config.ProjectIgnoreFile || relevant[name] || config.IsReserved(name) {
			continue
		}
		result.add(PathUnsafe, name, "ignored .gitignore files must not hide other paths")
	}
	result.Ignored, result.IgnoreExceptions = classifyIgnored(hidden, tracked)

	matcher, err := configuration.Matcher()
	if err != nil {
		return nil, err
	}
	checkLinks(&tree{root: root, replaced: replaced, matcher: matcher, entries: known(relevant, links)}, links, relevant, result)
	checkCaseCollisions(relevant, result)

	for _, name := range slices.Sorted(maps.Keys(relevant)) {
		if result.issued[name] {
			continue
		}
		owner, issue, reason := matcher.Owner(name)
		switch issue {
		case config.OwnerUnsafe:
			result.add(PathUnsafe, name, reason)
		case config.OwnerUnassigned:
			result.add(PathUnassigned, name, reason)
		case config.OwnerAmbiguous:
			result.add(PathAmbiguous, name, reason)
		default:
			result.Owners[name] = owner
		}
	}

	slices.SortFunc(result.Issues, func(a, b *Issue) int {
		if a.Path != b.Path {
			return strings.Compare(a.Path, b.Path)
		}
		return strings.Compare(a.Code, b.Code)
	})
	return result, nil
}

func inspect(root string, replaced map[string]string) (*inventory, error) {
	entries, err := listRelevant(root)
	if err != nil {
		return nil, err
	}
	entries = applyPrepared(root, entries, replaced)
	found := &inventory{entries: entries, relevant: map[string]bool{}}
	for _, entry := range entries {
		name := strings.TrimSuffix(entry, "/")
		if config.IsReserved(name) {
			continue
		}
		if err := config.ValidatePath(name); err != nil {
			found.issues = append(found.issues, &Issue{Code: PathUnsafe, Path: name, Detail: err.Error()})
			continue
		}
		if entry != name {
			found.issues = append(found.issues, &Issue{Code: PathUnsafe, Path: name, Detail: describeDirectory(root, name)})
			continue
		}
		info, err := os.Lstat(physical(root, replaced, name))
		switch {
		case err != nil:
			found.issues = append(found.issues, &Issue{Code: PathUnsafe, Path: name, Detail: "path cannot be inspected"})
		case info.Mode()&fs.ModeSymlink != 0:
			// A link is decided once the matcher and the other relevant paths
			// are known, because its target must have the link's owner.
			found.links = append(found.links, name)
		case !info.Mode().IsRegular():
			found.issues = append(found.issues, &Issue{Code: PathUnsafe, Path: name, Detail: "path is not a regular file"})
		default:
			found.relevant[name] = true
		}
	}
	return found, nil
}

// Relative converts a command-line path argument, interpreted relative to
// directory, into a project-relative path below root.
func Relative(root, directory, argument string) (string, error) {
	if argument == "" {
		return "", &Issue{Code: PathUnsafe, Path: argument, Detail: "path is empty"}
	}
	if filepath.IsAbs(argument) || strings.HasPrefix(argument, "/") {
		return "", &Issue{Code: PathUnsafe, Path: argument, Detail: "absolute paths are not supported"}
	}
	relative, err := filepath.Rel(root, filepath.Join(directory, filepath.FromSlash(argument)))
	if err != nil {
		return "", &Issue{Code: PathUnsafe, Path: argument, Detail: "path cannot be resolved"}
	}
	relative = filepath.ToSlash(relative)
	if relative == ".." || strings.HasPrefix(relative, "../") {
		return "", &Issue{Code: PathUnsafe, Path: argument, Detail: "path escapes the project root"}
	}
	if relative == "." {
		return relative, nil
	}
	if err := config.ValidatePath(relative); err != nil {
		return "", &Issue{Code: PathUnsafe, Path: argument, Detail: err.Error()}
	}
	if config.IsReserved(relative) {
		return "", &Issue{Code: PathUnsafe, Path: argument, Detail: "reserved GitOne paths must not be managed"}
	}
	return relative, nil
}

// tree answers the safe-link questions about the working tree. It never
// follows a link and never reads a link target.
type tree struct {
	root     string
	replaced map[string]string
	matcher  *config.Matcher
	entries  map[string]bool
}

func (t *tree) path(relativePath string) string {
	return physical(t.root, t.replaced, relativePath)
}

func (t *tree) Kind(relativePath string) (link.Kind, error) {
	info, err := os.Lstat(t.path(relativePath))
	switch {
	case errors.Is(err, os.ErrNotExist):
		// A prepared state recreates a directory by recreating the paths
		// below it, which the working tree itself does not hold yet.
		if t.preparedDirectory(relativePath) {
			return link.Directory, nil
		}
		return link.Missing, nil
	case err != nil:
		return link.Other, err
	case info.Mode()&fs.ModeSymlink != 0:
		return link.Symbolic, nil
	case info.IsDir():
		return link.Directory, nil
	case info.Mode().IsRegular():
		return link.Regular, nil
	}
	return link.Other, nil
}

// preparedDirectory reports whether a prepared state puts a path below
// relativePath, which makes it a directory once the transaction is applied.
func (t *tree) preparedDirectory(relativePath string) bool {
	for name, prepared := range t.replaced {
		if !strings.HasPrefix(name, relativePath+"/") {
			continue
		}
		if _, err := os.Lstat(prepared); err == nil {
			return true
		}
	}
	return false
}

// Relevant lists the paths this scan already found below directory. An
// ignored tree never enters that listing, so it stays uninspected here too.
func (t *tree) Relevant(directory string) ([]string, error) {
	var names []string
	for name := range t.entries {
		if strings.HasPrefix(name, directory+"/") {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names, nil
}

func (t *tree) Owner(relativePath string) (string, config.OwnerIssue, string) {
	return t.matcher.Owner(relativePath)
}

// checkLinks accepts the symbolic links GitOne may manage and reports every
// other one. An accepted link becomes an ordinary relevant path: only the
// lexical link path is managed, never a path below it.
func checkLinks(source *tree, links []string, relevant map[string]bool, result *Result) {
	for _, name := range links {
		if reason := acceptLink(source, source.path(name), name); reason != "" {
			result.add(PathUnsafe, name, reason)
			continue
		}
		relevant[name] = true
	}
}

// acceptLink reports why the symbolic link at name must not be managed, or
// the empty string when it becomes an ordinary relevant path.
func acceptLink(source link.Tree, physicalPath, name string) string {
	target, err := os.Readlink(physicalPath)
	if err != nil {
		return "symbolic link cannot be read"
	}
	if _, reason := link.Check(source, name, target); reason != "" {
		return "symbolic link to " + target + ": " + reason
	}
	return ""
}

// known is the set of paths a link target may prove its ownership with: the
// relevant paths and the links still being decided, so the outcome does not
// depend on the order the links are checked in.
func known(relevant map[string]bool, links []string) map[string]bool {
	entries := maps.Clone(relevant)
	for _, name := range links {
		entries[name] = true
	}
	return entries
}

// physical is the file a project-relative path is inspected in: the prepared
// version while a transaction is being validated, the working-tree file
// otherwise.
func physical(root string, replaced map[string]string, relativePath string) string {
	if prepared, listed := replaced[relativePath]; listed {
		return prepared
	}
	return filepath.Join(root, filepath.FromSlash(relativePath))
}

// applyPrepared rewrites the working-tree listing into the listing the
// prepared state has: a path whose prepared version is missing disappears, a
// path that only the prepared state has appears.
func applyPrepared(root string, entries []string, replaced map[string]string) []string {
	if len(replaced) == 0 {
		return entries
	}
	listed := map[string]bool{}
	for _, entry := range entries {
		listed[strings.TrimSuffix(entry, "/")] = true
	}
	for name, prepared := range replaced {
		_, err := os.Lstat(prepared)
		switch {
		case err == nil && !listed[name]:
			entries = append(entries, name)
		case err != nil && listed[name]:
			entries = slices.DeleteFunc(entries, func(entry string) bool { return entry == name })
		}
	}
	slices.Sort(entries)
	return entries
}

func checkCaseCollisions(relevant map[string]bool, result *Result) {
	components := map[string]map[string][]string{}
	for name := range relevant {
		foldedPrefix := ""
		for _, component := range strings.Split(name, "/") {
			if foldedPrefix != "" {
				foldedPrefix += "/"
			}
			foldedPrefix += config.FoldPath(component)
			if components[foldedPrefix] == nil {
				components[foldedPrefix] = map[string][]string{}
			}
			components[foldedPrefix][component] = append(components[foldedPrefix][component], name)
		}
	}

	collisions := map[string]map[string]bool{}
	for _, variants := range components {
		spellings := slices.Sorted(maps.Keys(variants))
		if len(spellings) < 2 {
			continue
		}
		for index, spelling := range spellings {
			for _, otherSpelling := range spellings[index+1:] {
				for _, name := range variants[spelling] {
					for _, other := range variants[otherSpelling] {
						addCaseCollision(collisions, name, other)
						addCaseCollision(collisions, other, name)
					}
				}
			}
		}
	}
	for _, name := range slices.Sorted(maps.Keys(collisions)) {
		others := slices.Sorted(maps.Keys(collisions[name]))
		result.add(PathUnsafe, name, "case-insensitive collision with "+strings.Join(others, ", "))
	}
}

func addCaseCollision(collisions map[string]map[string]bool, name, other string) {
	if collisions[name] == nil {
		collisions[name] = map[string]bool{}
	}
	collisions[name][other] = true
}

func checkBareRepositories(root string, entries, tracked []string, result *Result) {
	if isBareRepository(root) {
		result.add(PathUnsafe, ".", "the project root is a bare Git repository")
	}

	directories := map[string]bool{}
	for _, entry := range append(slices.Clone(entries), tracked...) {
		name := strings.TrimSuffix(entry, "/")
		if config.IsReserved(name) {
			continue
		}
		directory := path.Dir(name)
		if entry != name {
			directory = name
		}
		for directory != "." {
			directories[directory] = true
			directory = path.Dir(directory)
		}
	}
	for _, directory := range slices.Sorted(maps.Keys(directories)) {
		if isBareRepository(filepath.Join(root, filepath.FromSlash(directory))) {
			result.add(PathUnsafe, directory, "bare Git repositories are not supported")
		}
	}
}

func isBareRepository(directory string) bool {
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() {
		return false
	}
	if _, err := os.Lstat(filepath.Join(directory, "HEAD")); err != nil {
		return false
	}
	output, err := git.RunIsolated(directory, "--git-dir="+directory, "rev-parse", "--is-bare-repository")
	return err == nil && strings.TrimSpace(output) == "true"
}

func describeDirectory(root, name string) string {
	if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(name), ".git")); err == nil {
		return "nested Git repositories are not supported"
	}
	return "directory cannot be inspected"
}

// listRelevant asks Git for every untracked path that project .gitignore files
// do not hide. Ignored directory trees are never traversed, and global Git
// excludes are disabled so they cannot hide an unassigned project file.
func listRelevant(root string) ([]string, error) {
	return lsFiles(root, "--others", "--exclude-standard")
}

func lsFiles(root string, arguments ...string) ([]string, error) {
	gitDirectory, err := os.MkdirTemp("", "gitone-scan")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(gitDirectory)

	// An empty scratch index makes every working-tree file untracked, so one
	// listing covers the project regardless of how many repositories exist.
	if _, err := git.RunIsolated(root, "init", "--bare", "--quiet", "--template=", gitDirectory); err != nil {
		return nil, err
	}
	arguments = append([]string{
		"--git-dir=" + gitDirectory, "--work-tree=" + root,
		"-c", "core.excludesFile=", "-c", "core.ignorecase=false",
		"ls-files", "-z",
	}, arguments...)
	output, err := git.RunIsolated(root, arguments...)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(strings.Split(output, "\x00"), func(entry string) bool { return entry == "" }), nil
}

// listIgnored asks Git for the ignored paths of the project. Directories that
// an ignore rule hides are collapsed to a single entry and never enumerated,
// so only .gitignore files that hide content of an otherwise relevant
// directory appear here.
func listIgnored(root string) ([]string, error) {
	return Ignored(root, "")
}

// Ignored lists the working-tree paths the project ignore rules hide. Without
// a directory the listing collapses every hidden directory into one entry
// with a trailing slash, so an ignored tree is never enumerated; with one it
// lists exactly the hidden files below that directory.
func Ignored(root, directory string) ([]string, error) {
	arguments := []string{"--others", "--ignored", "--exclude-standard"}
	if directory == "" {
		return lsFiles(root, append(arguments, "--directory")...)
	}
	return lsFiles(root, append(arguments, "--", directory)...)
}

// classifyIgnored keeps ignored directories collapsed and records tracked
// descendants separately so editor integrations can exempt them without
// enumerating the ignored tree.
func classifyIgnored(entries, tracked []string) ([]string, []string) {
	indexed := make(map[string]bool, len(tracked))
	descendants := map[string][]string{}
	for _, name := range tracked {
		if indexed[name] {
			continue
		}
		indexed[name] = true
		for directory := path.Dir(name); directory != "."; directory = path.Dir(directory) {
			descendants[directory] = append(descendants[directory], name)
		}
	}

	ignored := []string{}
	exceptions := []string{}
	for _, entry := range entries {
		name := strings.TrimSuffix(entry, "/")
		if name == entry {
			if !indexed[name] {
				ignored = append(ignored, entry)
			}
			continue
		}
		ignored = append(ignored, entry)
		exceptions = append(exceptions, descendants[name]...)
	}
	slices.Sort(ignored)
	slices.Sort(exceptions)
	return ignored, exceptions
}

// IgnoredBy reports which of the given project-relative paths the .gitignore
// files below overlay hide. The overlay holds nothing but ignore files and
// the paths need not exist in it, so a projected ignore policy is evaluated
// without writing a single byte into the working tree.
func IgnoredBy(overlay string, paths []string) (map[string]bool, error) {
	hidden := map[string]bool{}
	if len(paths) == 0 {
		return hidden, nil
	}
	gitDirectory, err := os.MkdirTemp("", "gitone-ignore")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(gitDirectory)

	if _, err := git.RunIsolated(overlay, "init", "--bare", "--quiet", "--template=", gitDirectory); err != nil {
		return nil, err
	}
	// check-ignore reports "no path matched" as exit code 1, which is an
	// answer. --no-index keeps the empty scratch index out of the decision.
	output, err := git.RunIsolatedInput(overlay, strings.Join(paths, "\x00")+"\x00", 1,
		"--git-dir="+gitDirectory, "--work-tree="+overlay,
		"-c", "core.excludesFile=", "-c", "core.ignorecase=false",
		"check-ignore", "-z", "--stdin", "--no-index")
	if err != nil {
		return nil, err
	}
	for _, name := range strings.Split(output, "\x00") {
		if name != "" {
			hidden[name] = true
		}
	}
	return hidden, nil
}
