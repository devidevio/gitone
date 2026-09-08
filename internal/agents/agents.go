// Package agents manages one block of GitOne instructions in the project
// AGENTS.md. The canonical text is embedded from instructions.md, which is
// also the block the usage guide and the website publish, so an agent reading
// any of the three gets the same contract.
//
// Only <project-root>/AGENTS.md is managed. Nested instruction files,
// CLAUDE.md and tool-specific equivalents are never searched for or edited.
package agents

import (
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/devidevio/gitone/internal/atomicfs"
	"github.com/devidevio/gitone/internal/config"
	"github.com/devidevio/gitone/internal/worktree"
)

const (
	// Failed is the stable code of every refused instruction check or update.
	Failed = "AGENT001"

	// File is the only instruction file GitOne manages.
	File = "AGENTS.md"

	// Link is the permanent explanation of the managed block.
	Link = "https://gitone.io/#agents"

	// The markers delimit the text GitOne owns. Complete markers define that
	// text, so anything between them may be replaced.
	startMarker = "<!-- gitone:agents:start -->"
	endMarker   = "<!-- gitone:agents:end -->"

	// recognizable is the first line of the block. It identifies GitOne
	// instructions that are neither current nor marked, which are refused
	// instead of being rewritten by guesswork.
	recognizable = "This project uses GitOne:"
)

//go:embed instructions.md
var instructions string

// Instructions is the canonical block, exactly as it is written into a
// project AGENTS.md.
func Instructions() string { return instructions }

// State is what the project AGENTS.md holds.
type State int

const (
	// Missing is a project without an AGENTS.md.
	Missing State = iota
	// Absent is an ordinary AGENTS.md without any GitOne instructions.
	Absent
	// Unmarked is the exact current block, without the markers.
	Unmarked
	// Current is one marked block holding the current text.
	Current
	// Stale is one marked block holding different text.
	Stale
	// Ambiguous is every state GitOne must not edit automatically.
	Ambiguous
)

// Inspect classifies the project AGENTS.md. Ambiguous is returned with the
// AGENT001 diagnostic that names the manual remedy, so no caller has to
// classify a refusal again.
func Inspect(root string) (State, error) {
	state, _, _, err := inspect(root)
	return state, err
}

// inspect reads and classifies one regular-file snapshot. The descriptor is
// opened without following a symbolic link, so classification and update use
// the same bytes and permissions.
func inspect(root string) (State, string, os.FileInfo, error) {
	name := filepath.Join(root, File)
	switch info, err := os.Lstat(name); {
	case errors.Is(err, os.ErrNotExist):
		return Missing, "", nil, nil
	case err != nil:
		return Ambiguous, "", nil, err
	case !info.Mode().IsRegular():
		return Ambiguous, "", nil, refuseManually("is not a regular file")
	}
	file, err := atomicfs.OpenNoFollow(name)
	if err != nil {
		if info, checkErr := os.Lstat(name); checkErr == nil && !info.Mode().IsRegular() {
			return Ambiguous, "", nil, refuseManually("is not a regular file")
		}
		return Ambiguous, "", nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return Ambiguous, "", nil, err
	}
	if !info.Mode().IsRegular() {
		return Ambiguous, "", nil, refuseManually("is not a regular file")
	}
	contents, err := io.ReadAll(file)
	if err != nil {
		return Ambiguous, "", nil, err
	}
	current, err := os.Lstat(name)
	if err != nil || !current.Mode().IsRegular() || !os.SameFile(info, current) {
		return Ambiguous, "", nil, refuseManually("changed while GitOne was inspecting it")
	}
	state, err := classify(string(contents))
	return state, string(contents), info, err
}

// classify decides what a file holds without guessing. Every state the
// documented table does not name is refused rather than edited.
func classify(contents string) (State, error) {
	starts, ends := strings.Count(contents, startMarker), strings.Count(contents, endMarker)
	found := strings.Count(contents, recognizable)
	if starts == 0 && ends == 0 {
		switch exact := strings.Count(contents, instructions); {
		case found == 0:
			return Absent, nil
		case exact == 1 && found == 1:
			return Unmarked, nil
		}
		return Ambiguous, refuseManually("holds GitOne instructions that are neither marked nor exactly the current block")
	}
	start, end := strings.Index(contents, startMarker), strings.Index(contents, endMarker)
	if starts != 1 || ends != 1 || end < start {
		return Ambiguous, refuseManually("holds missing, repeated or misordered gitone:agents markers")
	}
	block := contents[start+len(startMarker) : end]
	if strings.Count(block, recognizable) != found {
		return Ambiguous, refuseManually("holds marked and unmarked GitOne instructions")
	}
	if strings.TrimPrefix(block, "\n") != instructions {
		return Stale, nil
	}
	return Current, nil
}

// Check reports why the project AGENTS.md is not the current managed block,
// ownership included. It reads and changes nothing else.
func Check(configuration *config.Config, root string) error {
	if err := Ownership(configuration); err != nil {
		return err
	}
	state, err := Inspect(root)
	if err != nil {
		return err
	}
	switch state {
	case Missing:
		return refuse("does not exist: run gitone agents update")
	case Absent:
		return refuse("does not hold the GitOne instructions: run gitone agents update")
	case Stale:
		return refuse("holds GitOne instructions that are not current: run gitone agents update")
	}
	return nil
}

// Update brings the project AGENTS.md to the current block and reports the
// state it found. Every byte outside the managed block is preserved, an
// existing file keeps its permissions, the result is published atomically and
// a symbolic link is refused instead of followed.
func Update(root string) (State, error) {
	name := filepath.Join(root, File)
	state, contents, info, err := inspect(root)
	if err != nil || state == Current {
		return state, err
	}
	if state == Missing {
		return state, publish(name, marked(), 0o644, nil, "")
	}
	original := contents
	switch state {
	case Absent:
		contents = separated(contents) + marked()
	case Unmarked:
		contents = strings.Replace(contents, instructions, marked(), 1)
	default:
		start := strings.Index(contents, startMarker)
		end := strings.Index(contents, endMarker) + len(endMarker)
		contents = contents[:start] + strings.TrimSuffix(marked(), "\n") + contents[end:]
	}
	return state, publish(name, contents, info.Mode().Perm(), info, original)
}

// Ownership reports why the configuration does not give AGENTS.md exactly one
// safe owner. It is the ordinary path rule every other command uses, so an
// unassigned, ambiguous, protected, case-conflicting or otherwise unsafe file
// fails with the same PATH001, PATH002 or PATH003 diagnostic.
func Ownership(configuration *config.Config) error {
	matcher, err := configuration.Matcher()
	if err != nil {
		return err
	}
	_, issue, reason := matcher.Owner(File)
	switch issue {
	case config.OwnerUnsafe:
		return &worktree.Issue{Code: worktree.PathUnsafe, Path: File, Detail: reason}
	case config.OwnerUnassigned:
		return &worktree.Issue{Code: worktree.PathUnassigned, Path: File, Detail: reason}
	case config.OwnerAmbiguous:
		return &worktree.Issue{Code: worktree.PathAmbiguous, Path: File, Detail: reason}
	}
	return nil
}

// marked is the canonical block inside its markers, as one whole Markdown
// section ending in a newline.
func marked() string {
	return startMarker + "\n" + instructions + endMarker + "\n"
}

// separated adds a missing Markdown separator without changing existing bytes.
func separated(contents string) string {
	if contents == "" {
		return ""
	}
	return contents + strings.Repeat("\n", max(0, 2-trailingNewlines(contents)))
}

func trailingNewlines(contents string) int {
	return len(contents) - len(strings.TrimRight(contents, "\n"))
}

// publish writes contents to name atomically, with the requested permissions.
func publish(name, contents string, permissions os.FileMode, expected os.FileInfo, original string) error {
	temporary, err := os.CreateTemp(filepath.Dir(name), "."+filepath.Base(name)+".*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = os.Remove(temporaryName)
		}
	}()
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
	if expected == nil {
		return atomicfs.RenameNoReplace(temporaryName, name)
	}
	if err := atomicfs.RenameExchange(temporaryName, name); err != nil {
		return err
	}
	if err := verifySnapshot(temporaryName, expected, original); err != nil {
		if rollbackErr := atomicfs.RenameExchange(temporaryName, name); rollbackErr != nil {
			removeTemporary = false
			return fmt.Errorf("%w\nRollback failed; the original file remains at %s: %v", err, temporaryName, rollbackErr)
		}
		return err
	}
	return nil
}

// verifySnapshot checks the file exchanged out of AGENTS.md. A mismatch is
// rolled back, so a concurrent writer is never silently overwritten.
func verifySnapshot(name string, expected os.FileInfo, contents string) error {
	file, err := atomicfs.OpenNoFollow(name)
	if err != nil {
		return refuseManually("changed while GitOne was updating it")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || !os.SameFile(expected, info) || info.Mode().Perm() != expected.Mode().Perm() {
		return refuseManually("changed while GitOne was updating it")
	}
	current, err := io.ReadAll(file)
	if err != nil || string(current) != contents {
		return refuseManually("changed while GitOne was updating it")
	}
	return nil
}

func refuse(detail string) error {
	return fmt.Errorf("%s %s %s", Failed, File, detail)
}

// refuseManually names the manual remedy and the permanent explanation,
// because GitOne cannot decide what such a file means.
func refuseManually(detail string) error {
	return fmt.Errorf("%s %s %s\nEdit %s by hand so it holds exactly one marked GitOne block, then run gitone agents again.\n%s",
		Failed, File, detail, File, Link)
}
