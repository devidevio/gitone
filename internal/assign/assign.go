// Package assign turns the newly unassigned paths an incoming or a target
// commit carries into the ownership a command may add to the configuration.
// An incoming path already belongs physically to the repository whose commit
// contains it, so the only open question is whether that deterministic
// ownership may be written down.
//
// gitone pull and gitone switch are its two consumers. Both detect, group,
// present, confirm and patch identically; the transaction that applies the
// patch beside the branch updates stays part of each command.
package assign

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/devidevio/gitone/internal/config"
	"github.com/devidevio/gitone/internal/policy"
	"github.com/devidevio/gitone/internal/repository"
	"github.com/devidevio/gitone/internal/ui"
)

// Flag is the explicit approval of the ownership a command generates for the
// new paths its incoming commits carry. It accepts nothing else, and in
// particular never accepts an incoming policy change.
const Flag = "--accept-new-paths"

const recoveryRequired = "REC001"

// proposal is one ownership pattern a command would add: the repository whose
// incoming commit carries the paths it covers, and those paths themselves. A
// pattern is either one exact path or one directory subtree that is entirely
// new, so it can never claim a path that already exists locally.
type proposal struct {
	repository string
	pattern    string
	covered    []string
}

// Assignment is the complete ownership one command generates locally: the
// proposals a person confirms, and the configuration files carrying them.
type Assignment struct {
	proposals []proposal

	// Files are the configuration files the accepted proposals patch. They
	// are what a command records in its own transaction.
	Files []Patched
}

// Patched is one configuration file and the patterns it gains per repository.
type Patched struct {
	Path         string              `json:"path"`
	Patterns     map[string][]string `json:"patterns"`
	OriginalHash string              `json:"original_hash"`
	TargetHash   string              `json:"target_hash"`
	PatchedHash  string              `json:"patched_hash"`
}

// Propose turns the newly unassigned entries of the incoming trees into the
// ownership a command can add, and returns nil when there is nothing to
// assign. command names the calling command in every refusal.
func Propose(command policy.Command, incoming *policy.Policy, root string, unassigned []repository.TreeIssue, tracked []string) (*Assignment, error) {
	return propose(command, incoming, root, unassigned, tracked, nil)
}

// ProposeFrom is Propose for a tree whose logical repository name still uses
// another repository's metadata directory during a declared rename.
func ProposeFrom(command policy.Command, incoming *policy.Policy, root string, unassigned []repository.TreeIssue,
	tracked []string, sources map[string]string) (*Assignment, error) {
	return propose(command, incoming, root, unassigned, tracked, sources)
}

func propose(command policy.Command, incoming *policy.Policy, root string, unassigned []repository.TreeIssue,
	tracked []string, sources map[string]string) (*Assignment, error) {
	if len(unassigned) == 0 {
		return nil, nil
	}
	existing := map[string]bool{}
	for _, entry := range tracked {
		for parent := path.Dir(entry); parent != "."; parent = path.Dir(parent) {
			existing[parent] = true
		}
	}

	// Every new path is grouped by the highest directory that does not exist
	// yet. A group of at least two paths from one repository becomes that
	// whole subtree; everything else stays the exact path it is.
	owners := map[string]map[string]bool{}
	grouped := map[string][]repository.TreeIssue{}
	for _, entry := range unassigned {
		subtree := newSubtree(root, entry.Path, existing)
		grouped[subtree] = append(grouped[subtree], entry)
		if subtree != "" {
			if owners[subtree] == nil {
				owners[subtree] = map[string]bool{}
			}
			owners[subtree][entry.Repository] = true
		}
	}

	result := new(Assignment)
	for _, subtree := range slices.Sorted(maps.Keys(grouped)) {
		entries := grouped[subtree]
		if subtree != "" && len(entries) > 1 && len(owners[subtree]) == 1 {
			covered := make([]string, len(entries))
			for index, entry := range entries {
				covered[index] = entry.Path
			}
			result.proposals = append(result.proposals,
				proposal{repository: entries[0].Repository, pattern: subtree + "/**", covered: covered})
			continue
		}
		for _, entry := range entries {
			result.proposals = append(result.proposals,
				proposal{repository: entry.Repository, pattern: entry.Path, covered: []string{entry.Path}})
		}
	}
	slices.SortFunc(result.proposals, func(first, second proposal) int {
		return strings.Compare(first.repository+"\x00"+first.pattern, second.repository+"\x00"+second.pattern)
	})

	if err := unclaimed(command, root, result.proposals); err != nil {
		return nil, err
	}
	if err := result.collect(root); err != nil {
		return nil, err
	}
	return result, result.revalidate(command, incoming, root, sources)
}

// newSubtree is the highest directory of the given path that does not exist
// before the command applies, or the empty string while every directory of it
// already does. Nothing can live below an absent directory, so owning such a
// subtree whole can never claim an existing local path.
func newSubtree(root, relativePath string, existing map[string]bool) string {
	parent := path.Dir(relativePath)
	if parent == "." {
		return ""
	}
	segments := strings.Split(parent, "/")
	for index := range segments {
		directory := strings.Join(segments[:index+1], "/")
		if existing[directory] {
			continue
		}
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(directory))); err != nil {
			return directory
		}
		existing[directory] = true
	}
	return ""
}

// unclaimed refuses every proposal whose pattern starts at a path that
// already exists locally. An added pattern is purely additive, so it can only
// change the ignored, relevant or owned state of a path its own concrete
// prefix already exists at. Refusing those keeps the rule an incoming policy
// change already follows: nothing that is here today becomes publishable
// without an explicit local decision.
func unclaimed(command policy.Command, root string, proposals []proposal) error {
	var claimed []string
	for _, current := range proposals {
		prefix := strings.TrimSuffix(current.pattern, "/**")
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(prefix))); err == nil {
			claimed = append(claimed, fmt.Sprintf("  %s -> %s", prefix, current.repository))
		}
	}
	if len(claimed) == 0 {
		return nil
	}
	return fmt.Errorf("%s the new incoming paths cannot be assigned: these already exist locally and would be put into a repository:\n%s\n"+
		"Protect, move, remove or explicitly reconfigure each path locally, then run %s again.\n"+
		"%s never accepts this.\n%s",
		command.Code, strings.Join(claimed, "\n"), command.Retry, Flag, command.Unmodified)
}

// collect groups the proposals by the configuration file that supplies the
// effective paths list of each repository, which is the only file an addition
// may change.
func (a *Assignment) collect(root string) error {
	files := map[string]*Patched{}
	for _, current := range a.proposals {
		name, err := config.PathsFile(root, current.repository)
		if err != nil {
			return err
		}
		if files[name] == nil {
			files[name] = &Patched{Path: name, Patterns: map[string][]string{}}
		}
		files[name].Patterns[current.repository] = append(files[name].Patterns[current.repository], current.pattern)
	}
	for _, name := range slices.Sorted(maps.Keys(files)) {
		a.Files = append(a.Files, *files[name])
	}
	return nil
}

// revalidate proves the complete projected configuration the command would
// leave behind is usable and owns every incoming tree entry. The additions are
// applied to the accepted target version of the committed file, not to the
// current one, so a reviewed incoming policy and the generated ownership are
// validated as one configuration.
func (a *Assignment) revalidate(command policy.Command, incoming *policy.Policy, root string, sources map[string]string) error {
	public, local, err := a.apply(command, incoming.Projected(), root)
	if err != nil {
		return err
	}
	projected, err := config.Projected(public, local)
	if err != nil {
		return fmt.Errorf("%s the generated path assignments leave an unusable configuration: %s\n%s",
			command.Code, detail(err), command.Unmodified)
	}
	matcher, err := projected.Matcher()
	if err != nil {
		return fmt.Errorf("%s the generated path assignments leave an unusable configuration: %s\n%s",
			command.Code, detail(err), command.Unmodified)
	}
	refused, err := repository.TreeIssuesFrom(matcher, root, incoming.Commits, sources)
	if err != nil {
		return fmt.Errorf("%s incoming trees cannot be read: %s\n%s", command.Code, detail(err), command.Unmodified)
	}
	if len(refused) == 0 {
		return nil
	}
	issues := make([]string, len(refused))
	for index, entry := range refused {
		issues[index] = fmt.Sprintf("  %s in repository %q: %s", entry.Path, entry.Repository, entry.Reason)
	}
	return fmt.Errorf("%s the generated path assignments do not make every incoming path safe:\n%s\n%s",
		command.Code, strings.Join(issues, "\n"), command.Unmodified)
}

// apply is the pair of configuration files the command would leave behind.
// target is the accepted incoming committed file, or nil while no incoming
// commit replaces it; the local file is never read from repository content.
func (a *Assignment) apply(command policy.Command, target []byte, root string) (public, local []byte, err error) {
	if public = target; public == nil {
		if public, err = os.ReadFile(filepath.Join(root, config.PublicFile)); err != nil {
			return nil, nil, err
		}
	}
	local, err = os.ReadFile(filepath.Join(root, config.LocalFile))
	if errors.Is(err, os.ErrNotExist) {
		local, err = nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	for index := range a.Files {
		file := &a.Files[index]
		var contents []byte
		if file.Path == config.LocalFile {
			contents = local
		} else {
			contents = public
		}
		file.TargetHash = Hash(contents)
		contents, err = file.add(contents)
		if err != nil {
			return nil, nil, fmt.Errorf("%s %s\n%s", command.Code, detail(err), command.Unmodified)
		}
		file.PatchedHash = Hash(contents)
		if file.Path == config.LocalFile {
			local = contents
		} else {
			public = contents
		}
	}
	return public, local, nil
}

// add appends the collected patterns of one configuration file to it.
func (p Patched) add(contents []byte) ([]byte, error) {
	var err error
	for _, name := range slices.Sorted(maps.Keys(p.Patterns)) {
		if contents, err = config.AddPaths(contents, name, p.Patterns[name]); err != nil {
			return nil, fmt.Errorf("%s cannot be patched: %s", p.Path, detail(err))
		}
	}
	return contents, nil
}

// Preview lists every proposed pattern in full, with the number of new
// entries a directory proposal covers, so one confirmation answers a complete
// review instead of a truncated one.
func (a *Assignment) Preview(output io.Writer) {
	if a == nil {
		return
	}
	style := ui.For(output)
	fmt.Fprintf(output, "%s\n\n", style.Heading.Render("New paths without an owner:"))
	for _, current := range a.proposals {
		covered := ""
		if len(current.covered) > 1 {
			covered = fmt.Sprintf(" (%d new paths)", len(current.covered))
		}
		fmt.Fprintf(output, "  %s -> %s%s\n", ui.Safe(current.pattern), current.repository, covered)
	}
	fmt.Fprintf(output, "\n%s\n\n", style.Muted.Render(
		"Each path is assigned to the repository whose incoming commit carries it, by adding these patterns to "+
			strings.Join(a.names(), " and ")+"."))
}

// Confirm asks once for the explicit approval the generated ownership needs.
func (a *Assignment) Confirm(command policy.Command, accepted, interactive bool, input io.Reader, output io.Writer) (bool, error) {
	if a == nil {
		return true, nil
	}
	return policy.Ask(command, "assigning new incoming paths", "Assign these paths?",
		Flag, accepted, interactive, input, output)
}

// Report closes a successful command with the file a person still has to
// review. The addition is left unstaged on purpose: ownership is a policy
// decision and is committed by hand.
func (a *Assignment) Report(output io.Writer) {
	if a == nil {
		return
	}
	style := ui.For(output)
	fmt.Fprintf(output, "\n%s\n\n", style.Heading.Render(
		"Assigned "+count(len(a.proposals), "new path pattern")+", unstaged:"))
	for _, file := range a.Files {
		if file.Path == config.LocalFile {
			fmt.Fprintf(output, "  review %s; it stays local and is never committed\n", file.Path)
			continue
		}
		fmt.Fprintf(output, "  review %s and commit it with: gitone commit %s -m \"Assign new paths\"\n", file.Path, file.Path)
	}
}

func (a *Assignment) names() []string {
	return Names(a.Files)
}

// Names are the configuration files an assignment patches. They are
// deliberately changed by the command itself, so they never count as a
// repository that changed outside GitOne.
func Names(files []Patched) []string {
	names := make([]string, len(files))
	for index, file := range files {
		names[index] = file.Path
	}
	return names
}

// Save keeps every configuration file the command patches exactly as it is
// now, so aborting can put the version from before it back even when no
// repository owns the file.
func Save(root, directory string, files []Patched) error {
	for index, file := range files {
		contents, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(file.Path)))
		if err != nil {
			return err
		}
		files[index].OriginalHash = Hash(contents)
		if err := os.WriteFile(backup(directory, index), contents, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// Verify refuses recovery when a configuration file no longer has one of the
// exact byte sequences the interrupted command can have produced. The saved
// original is checked too, so a damaged state file cannot bless an unrelated
// working-tree version by changing only its recorded hash. command is the
// interrupted command as it is named in the refusal, such as "pull".
func Verify(root, directory, command string, files []Patched) error {
	for index, file := range files {
		original, err := os.ReadFile(backup(directory, index))
		if err != nil {
			return err
		}
		if Hash(original) != file.OriginalHash {
			return fmt.Errorf("%s the recorded original of %s is unreadable: its contents changed", recoveryRequired, file.Path)
		}
		contents, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(file.Path)))
		if err != nil {
			return changed(file.Path, command, directory)
		}
		current := Hash(contents)
		if current != file.OriginalHash && current != file.TargetHash && current != file.PatchedHash {
			return changed(file.Path, command, directory)
		}
	}
	return nil
}

// Apply adds the recorded patterns to the configuration files as they are
// after the branches moved, which is the accepted target version of a
// committed file. Adding a pattern a file already lists is a no-op, so an
// interruption between the write and the recorded progress cannot duplicate
// it.
func Apply(root string, files []Patched) error {
	for _, file := range files {
		name := filepath.Join(root, filepath.FromSlash(file.Path))
		contents, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		patched, err := file.add(contents)
		if err != nil {
			return err
		}
		if err := replace(name, patched); err != nil {
			return err
		}
	}
	return nil
}

// Restore puts back the exact bytes every patched configuration file had
// before the command. A repository reset restores a committed file to the
// same content; an ignored local file is only restored here.
func Restore(root, directory string, files []Patched) error {
	for index, file := range files {
		contents, err := os.ReadFile(backup(directory, index))
		if err != nil {
			return err
		}
		if err := replace(filepath.Join(root, filepath.FromSlash(file.Path)), contents); err != nil {
			return err
		}
	}
	return nil
}

// Hash identifies one exact version of a configuration file.
func Hash(contents []byte) string {
	return fmt.Sprintf("%x", sha256.Sum256(contents))
}

// backup names the saved version of one patched file from before the command.
// A repository name can never start with a digit, so it never collides with a
// saved index.
func backup(directory string, index int) string {
	return filepath.Join(directory, fmt.Sprintf("%d.configuration.original", index))
}

func changed(name, command, directory string) error {
	return fmt.Errorf("%s configuration file %s changed outside GitOne since the interrupted %s: resolve it manually\n"+
		"  1. inspect the file and the saved version in %s\n"+
		"  2. keep the version you want\n"+
		"  3. archive %s outside the project afterwards, do not delete it",
		recoveryRequired, name, command, directory, directory)
}

// replace rewrites one configuration file atomically, so an interrupted write
// can never leave the project with a truncated configuration.
func replace(name string, contents []byte) error {
	temporary, err := os.CreateTemp(filepath.Dir(name), "."+filepath.Base(name)+".*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	permissions := os.FileMode(0o600)
	if info, err := os.Stat(name); err == nil {
		permissions = info.Mode().Perm()
	}
	if err := temporary.Chmod(permissions); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(contents); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryName, name)
}

func count(value int, noun string) string {
	if value == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", value, noun)
}

// detail is the first line of an error, so one reported problem stays one line.
func detail(err error) string {
	line, _, _ := strings.Cut(err.Error(), "\n")
	return line
}
