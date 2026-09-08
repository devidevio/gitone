// Package log renders the recent history of the managed repositories as one
// read-only view. Every line comes from native Git, so abbreviated commit
// IDs, dates and ref decorations behave exactly as they do for a plain
// git log.
package log

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

const (
	// DefaultCount is the number of commits one repository section shows
	// without an explicit -n.
	DefaultCount = 20
	// MaxCount bounds -n, so the view stays a recent-history view instead of
	// a way to render an unbounded amount of output.
	MaxCount = 1000
)

// Log writes one history section per selected repository, in configuration
// order. target is empty or "all" for every repository, or the name of one.
// count is the maximum number of commits per repository. A repository
// without any commit is labeled instead of failing the whole command.
func Log(configuration *config.Config, root, target string, count int, output io.Writer) error {
	names := configuration.RepositoryNames()
	if target != "" && target != "all" {
		if !slices.Contains(names, target) {
			return fmt.Errorf("CLI001 unknown repository %q", target)
		}
		names = []string{target}
	}
	if count < 1 || count > MaxCount {
		return fmt.Errorf("CLI001 invalid commit count %d", count)
	}

	release, err := lock.AcquireRead(root)
	if err != nil {
		return err
	}
	defer release()

	// The whole view is built before the first byte is written, so a failing
	// repository never leaves a partial history that reads like the full log.
	var rendered strings.Builder
	style := ui.For(output)
	for _, name := range names {
		section, err := commits(root, name, count)
		if err != nil {
			return err
		}
		if section == "" {
			section = style.Muted.Render("no commits yet") + "\n"
		} else {
			section = colorCommits(style, section)
		}
		if rendered.Len() != 0 {
			rendered.WriteString("\n")
		}
		fmt.Fprintf(&rendered, "%s\n%s", style.Heading.Render(strings.ToUpper(name)), section)
	}
	_, err = io.WriteString(output, rendered.String())
	return err
}

// commits is the native Git one-line history of one repository, empty for an
// unborn branch. The pager, signatures and Git colors are disabled so the
// output is deterministic and can be sanitized before GitOne colors it.
func commits(root, name string, count int) (string, error) {
	exists, err := repository.HeadExists(root, name)
	if err != nil {
		return "", err
	}
	if !exists {
		return "", nil
	}
	arguments := []string{"--no-pager", "--no-optional-locks",
		"--git-dir=" + repository.Directory(root, name),
		"log", "--no-show-signature", "--max-count=" + fmt.Sprint(count), "--date=short",
		"--pretty=tformat:%h %ad%d %s", "--no-color"}
	output, err := git.Run(root, arguments...)
	return ui.Safe(output), err
}

// colorCommits colors the stable hash field after repository controls have
// been removed, so commit subjects can never create presentation sequences.
func colorCommits(style ui.Styles, section string) string {
	lines := strings.SplitAfter(section, "\n")
	for index, line := range lines {
		text, newline := strings.CutSuffix(line, "\n")
		if hash, rest, found := strings.Cut(text, " "); found {
			text = style.Muted.Render(hash) + " " + rest
		}
		lines[index] = text
		if newline {
			lines[index] += "\n"
		}
	}
	return strings.Join(lines, "")
}
