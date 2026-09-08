package setup

import (
	"bufio"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"charm.land/huh/v2"
	"github.com/devidevio/gitone/internal/config"
	"github.com/devidevio/gitone/internal/git"
	"github.com/devidevio/gitone/internal/worktree"
)

func TestValueReportsAnUnavailableGit(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	for _, plain := range []bool{true, false} {
		name := "interactive"
		if plain {
			name = "plain"
		}
		t.Run(name, func(t *testing.T) {
			input := strings.NewReader("main\n")
			asked := &prompt{reader: bufio.NewReader(input), input: input, output: io.Discard, plain: plain}
			if !plain {
				asked.runField = func(field huh.Field) error { return field.RunAccessible(io.Discard, input) }
			}

			_, ok := asked.value("Default branch", "main", config.ValidateBranch)
			if ok || !errors.Is(asked.err, git.ErrUnavailable) {
				t.Fatalf("accepted = %v, error = %v, want ErrUnavailable", ok, asked.err)
			}
		})
	}
}

func TestInteractiveAndPlainPathSelectionsMatch(t *testing.T) {
	suggestions := []worktree.Suggestion{
		{Pattern: "README.md", Paths: []string{"README.md"}},
		{Pattern: "src/**", Paths: []string{"src/main.go"}},
	}
	relevant := []string{"README.md", "docs/guide.md", "src/main.go"}
	current := []string{"README.md"}

	// The browser answers with keys; only the custom-pattern input, which
	// runs between two browser rounds, still reads lines.
	browsing := [][]string{
		{"down", "space", "down", "down", "space"},
		{"enter"},
	}
	inputs := []string{"docs/**\n", "\n"}
	interactive := &prompt{
		plain:       false,
		output:      io.Discard,
		suggestions: suggestions,
		relevant:    relevant,
		runField: func(field huh.Field) error {
			if browsed, ok := field.(*browser); ok {
				press(browsed, browsing[0]...)
				browsing = browsing[1:]
				return nil
			}
			answer := inputs[0]
			inputs = inputs[1:]
			return field.RunAccessible(io.Discard, strings.NewReader(answer))
		},
	}
	got, ok := interactive.paths(nil, current)
	if !ok {
		t.Fatal("interactive selection was cancelled")
	}

	input := strings.NewReader("1,2,3\ndocs/**\n\n")
	plain := &prompt{
		reader:      bufio.NewReader(input),
		input:       input,
		output:      io.Discard,
		plain:       true,
		suggestions: suggestions,
		relevant:    relevant,
	}
	want, ok := plain.paths(nil, current)
	if !ok {
		t.Fatal("plain selection was cancelled")
	}
	if !slices.Equal(got, want) {
		t.Fatalf("interactive paths = %v, plain paths = %v", got, want)
	}
	if !slices.Equal(want, []string{"README.md", "docs/**", "src/**"}) {
		t.Fatalf("paths = %v", want)
	}
}

// pair is the two collected repositories the assignment test distributes to,
// in setup order.
func pair() []*entry {
	return []*entry{
		{name: "website", Paths: []string{"README.md"}},
		{name: "code", Paths: []string{"src/**"}},
	}
}

func TestInteractiveAndPlainAssignmentsMatch(t *testing.T) {
	unassigned := []string{"docs/guide.md", "notes/todo.md"}

	inputs := []string{"3\n", "2\n"}
	interactive := pair()
	asked := &prompt{
		output: io.Discard,
		runField: func(field huh.Field) error {
			answer := inputs[0]
			inputs = inputs[1:]
			return field.RunAccessible(io.Discard, strings.NewReader(answer))
		},
	}
	if !distribute(interactive, unassigned, asked, io.Discard) {
		t.Fatal("interactive assignment was cancelled")
	}

	input := strings.NewReader("code\nwebsite\n")
	plain := pair()
	if !distribute(plain, unassigned, &prompt{
		reader: bufio.NewReader(input),
		input:  input,
		output: io.Discard,
		plain:  true,
	}, io.Discard) {
		t.Fatal("plain assignment was cancelled")
	}

	for index := range plain {
		if !slices.Equal(interactive[index].Paths, plain[index].Paths) {
			t.Fatalf("%s: interactive paths = %v, plain paths = %v", plain[index].name, interactive[index].Paths, plain[index].Paths)
		}
	}
	if !slices.Equal(plain[0].Paths, []string{"README.md", "notes/todo.md"}) || !slices.Equal(plain[1].Paths, []string{"docs/guide.md", "src/**"}) {
		t.Fatalf("website = %v, code = %v", plain[0].Paths, plain[1].Paths)
	}
}
