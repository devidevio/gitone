package status

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/devidevio/gitone/internal/ui"
)

// unknown marks an ahead/behind count that no local remote-tracking ref
// provides yet, which is the case before the first successful push.
const unknown = "?"

// WriteHuman prints the status for people: one section per repository, one
// section for detected issues and one summarizing branch section.
func (s *Status) WriteHuman(output io.Writer) {
	style := ui.For(output)
	for _, reported := range s.Repositories {
		fmt.Fprintln(output, style.Heading.Render(strings.ToUpper(reported.Name)))
		fmt.Fprintln(output, style.Muted.Render(fmt.Sprintf("  visibility: %s", reported.Visibility)))
		changes := s.repositoryChanges(reported.Name)
		if len(changes) == 0 {
			fmt.Fprintln(output, style.Success.Render("  clean"))
		}
		for _, change := range changes {
			line := fmt.Sprintf("  %-10s %s", change.State+":", describe(change))
			if change.State == Staged {
				line = style.Success.Render(line)
			} else {
				line = style.Error.Render(line)
			}
			fmt.Fprintln(output, line)
		}
		fmt.Fprintln(output)
	}

	if len(s.Issues) != 0 {
		fmt.Fprintln(output, style.Heading.Render("ISSUES"))
		for _, issue := range s.Issues {
			fmt.Fprintln(output, style.Error.Render("  "+issue.String()))
		}
		fmt.Fprintln(output)
	}

	fmt.Fprintln(output, style.Heading.Render("BRANCHES"))
	for _, reported := range s.Repositories {
		fmt.Fprintf(output, "  %s: %s%s\n", reported.Name, style.Branch.Render(reported.Branch), style.Muted.Render(tracking(reported)))
	}
}

// WritePorcelain prints one tab separated record per line. Managed paths
// cannot contain tabs, so spaces in path names stay unambiguous.
func (s *Status) WritePorcelain(output io.Writer) {
	fmt.Fprintf(output, "version\t%d\n", s.Version)
	for _, reported := range s.Repositories {
		fmt.Fprintf(output, "repository\t%s\t%s\t%s\t%s\t%s\n",
			reported.Name, reported.Visibility, reported.Branch, count(reported.Ahead), count(reported.Behind))
	}
	for _, change := range s.Changes {
		fmt.Fprintf(output, "change\t%s\t%s\t%s\t%s\t%s\n",
			change.Repository, change.State, change.Type, change.Path, change.From)
	}
	for _, issue := range s.Issues {
		fmt.Fprintf(output, "issue\t%s\t%s\t%s\n", issue.Code, issue.Path, issue.Detail)
	}
}

// WriteJSON prints the versioned status document.
func (s *Status) WriteJSON(output io.Writer) error {
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	return encoder.Encode(s)
}

func (s *Status) repositoryChanges(name string) []Change {
	var changes []Change
	for _, change := range s.Changes {
		if change.Repository == name {
			changes = append(changes, change)
		}
	}
	return changes
}

// String renders one issue as its stable code followed by the location.
func (i Issue) String() string {
	if i.Path == "" {
		return i.Code + " " + i.Detail
	}
	return i.Code + " " + i.Path + ": " + i.Detail
}

func describe(change Change) string {
	if change.State == Untracked {
		return change.Path
	}
	if change.From != "" {
		return change.Type + " " + change.From + " -> " + change.Path
	}
	return change.Type + " " + change.Path
}

func tracking(reported Repository) string {
	if reported.Ahead == nil || reported.Behind == nil {
		return " (no remote-tracking branch)"
	}
	result := ""
	if *reported.Ahead != 0 {
		result += " ↑" + strconv.Itoa(*reported.Ahead)
	}
	if *reported.Behind != 0 {
		result += " ↓" + strconv.Itoa(*reported.Behind)
	}
	return result
}

func count(value *int) string {
	if value == nil {
		return unknown
	}
	return strconv.Itoa(*value)
}
