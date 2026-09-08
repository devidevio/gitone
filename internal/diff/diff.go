// Package diff renders the changes of the managed repositories as one
// read-only view. Every patch comes from native Git, so renames, binary
// files, submodules and diff configuration behave exactly as they do for a
// plain git diff.
package diff

import (
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/devidevio/gitone/internal/config"
	"github.com/devidevio/gitone/internal/git"
	"github.com/devidevio/gitone/internal/lock"
	"github.com/devidevio/gitone/internal/repository"
	"github.com/devidevio/gitone/internal/ui"
)

// Diff writes one patch section per selected repository, in configuration
// order. target is empty or "all" for every repository, or the name of one.
// staged compares the index against HEAD instead of the working tree against
// the index. Repositories without matching changes are omitted.
func Diff(configuration *config.Config, root, target string, staged bool, output io.Writer) error {
	names := configuration.RepositoryNames()
	if target != "" && target != "all" {
		if !slices.Contains(names, target) {
			return fmt.Errorf("CLI001 unknown repository %q", target)
		}
		names = []string{target}
	}

	release, err := lock.AcquireRead(root)
	if err != nil {
		return err
	}
	defer release()

	// The whole view is built before the first byte is written, so a failing
	// repository never leaves a partial patch that reads like the full diff.
	var rendered strings.Builder
	style := ui.For(output)
	for _, name := range names {
		section, err := patch(root, name, staged)
		if err != nil {
			return err
		}
		if section == "" {
			continue
		}
		section = colorPatch(style, section)
		if rendered.Len() != 0 {
			rendered.WriteString("\n")
		}
		fmt.Fprintf(&rendered, "%s\n%s", style.Heading.Render(strings.ToUpper(name)), section)
	}
	_, err = io.WriteString(output, rendered.String())
	return err
}

// patch is the native Git patch of one repository. The pager and any external
// diff program and Git colors are disabled so the output is deterministic,
// never waits for a terminal and can be sanitized before GitOne colors it.
func patch(root, name string, staged bool) (string, error) {
	arguments := []string{"--no-pager", "--no-optional-locks",
		"--git-dir=" + repository.Directory(root, name), "--work-tree=" + root,
		"diff", "--no-ext-diff", "--no-textconv", "--no-color"}
	if staged {
		// --cached needs no base commit on an unborn branch: Git compares the
		// index against the empty tree itself.
		arguments = append(arguments, "--cached")
	}
	output, err := git.Run(root, arguments...)
	return ui.Safe(output), err
}

// colorPatch applies Git's usual semantic colors after repository controls
// have been removed, so only GitOne's presentation reaches the terminal.
func colorPatch(style ui.Styles, patch string) string {
	lines := strings.SplitAfter(patch, "\n")
	for index, line := range lines {
		text, newline := strings.CutSuffix(line, "\n")
		switch {
		case strings.HasPrefix(text, "diff --git "), strings.HasPrefix(text, "index "),
			strings.HasPrefix(text, "--- "), strings.HasPrefix(text, "+++ "):
			text = style.Heading.Render(text)
		case strings.HasPrefix(text, "@@"):
			text = style.Branch.Render(text)
		case strings.HasPrefix(text, "+"):
			text = style.Success.Render(text)
		case strings.HasPrefix(text, "-"):
			text = style.Error.Render(text)
		}
		lines[index] = text
		if newline {
			lines[index] += "\n"
		}
	}
	return strings.Join(lines, "")
}
