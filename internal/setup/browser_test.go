package setup

import (
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// sample is one project inventory with a nested directory, a file directly in
// the root and an accepted directory symbolic link, which the inventory
// reports as one path without descendants.
var sample = []string{
	"README.md",
	"docs/guide.md",
	"docs/nested/deep.md",
	"docs/shared",
	"src/main.go",
}

func keyMsg(name string) tea.KeyPressMsg {
	switch name {
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "left":
		return tea.KeyPressMsg{Code: tea.KeyLeft}
	case "right":
		return tea.KeyPressMsg{Code: tea.KeyRight}
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "space":
		return tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}
	case "ctrl+a":
		return tea.KeyPressMsg{Code: 'a', Mod: tea.ModCtrl}
	default:
		return tea.KeyPressMsg{Code: []rune(name)[0], Text: name}
	}
}

func press(b *browser, keys ...string) {
	for _, name := range keys {
		b.Update(keyMsg(name))
	}
}

// lines renders the visible rows without presentation, which is what the
// tests compare.
func lines(b *browser) []string {
	listed := make([]string, len(b.rows))
	for position := range b.rows {
		marked, label := b.entry(b.rows[position])
		listed[position] = strings.TrimSpace(marked.box + " " + label + "  " + marked.note)
	}
	return listed
}

func at(t *testing.T, b *browser, label string) {
	t.Helper()
	position := slices.IndexFunc(lines(b), func(line string) bool { return strings.Contains(line, label) })
	if position < 0 {
		t.Fatalf("row %q is not visible in %v", label, lines(b))
	}
	b.cursor = position
}

func equal(t *testing.T, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestBrowserBuildsHierarchyDirectoriesFirst(t *testing.T) {
	b := newBrowser(sample, nil, nil)

	equal(t, lines(b), []string{"[ ] docs/**", "[ ] src/**", "[ ] README.md", customOption})
	if b.breadcrumb() != "/" {
		t.Fatalf("breadcrumb = %q, want /", b.breadcrumb())
	}
	press(b, "right")
	equal(t, lines(b), []string{
		"[ ] Select this directory: docs/**",
		"[ ] docs/nested/**",
		"[ ] docs/guide.md",
		"[ ] docs/shared",
		customOption,
	})
	if b.breadcrumb() != "/docs/" {
		t.Fatalf("breadcrumb = %q, want /docs/", b.breadcrumb())
	}
}

func TestBrowserSkipsTheProjectFilesSetupAssigns(t *testing.T) {
	b := newBrowser([]string{".gitignore", ".gitone.yml", "README.md", "docs/guide.md"}, nil, nil)

	equal(t, lines(b), []string{"[ ] docs/**", "[ ] README.md", customOption})
}

func TestBrowserNavigatesAndRestoresTheCursor(t *testing.T) {
	b := newBrowser(sample, nil, nil)

	press(b, "left")
	if b.current != b.root {
		t.Fatal("left moved above the project root")
	}
	press(b, "right", "down", "down")
	if got := lines(b)[b.cursor]; got != "[ ] docs/guide.md" {
		t.Fatalf("cursor on %q, want docs/guide.md", got)
	}
	press(b, "left")
	if got := lines(b)[b.cursor]; got != "[ ] docs/**" {
		t.Fatalf("returning put the cursor on %q, want docs/**", got)
	}
	press(b, "right")
	if got := lines(b)[b.cursor]; got != "[ ] docs/guide.md" {
		t.Fatalf("reentering put the cursor on %q, want docs/guide.md", got)
	}
}

func TestBrowserRestoresTheUnfilteredCursorAfterFilteredNavigation(t *testing.T) {
	b := newBrowser(sample, nil, nil)

	press(b, "/", "s", "r", "esc", "right", "left")
	if got := lines(b)[b.cursor]; got != "[ ] src/**" {
		t.Fatalf("returning put the cursor on %q, want src/**", got)
	}
}

func TestBrowserKeepsTheCursorVisibleWithinItsHeight(t *testing.T) {
	b := newBrowser(sample, nil, nil)
	b.focused = true
	b.WithWidth(80)
	b.WithHeight(4)

	press(b, "down", "down")
	view := b.View()
	if !strings.Contains(view, "README.md") {
		t.Fatalf("cursor row is outside the view:\n%s", view)
	}
	if got := lipgloss.Height(view); got != 4 {
		t.Fatalf("view height = %d, want 4", got)
	}
}

func TestBrowserNeverTraversesAnAcceptedLink(t *testing.T) {
	b := newBrowser(sample, nil, nil)

	press(b, "right")
	at(t, b, "docs/shared")
	press(b, "right")
	if b.breadcrumb() != "/docs/" {
		t.Fatalf("the link was entered: breadcrumb = %q", b.breadcrumb())
	}
	press(b, "space")
	equal(t, b.answer(), []string{"docs/shared"})
}

func TestBrowserKeepsWholeDirectorySelectionCanonical(t *testing.T) {
	b := newBrowser(sample, nil, nil)

	press(b, "right")
	at(t, b, "docs/guide.md")
	press(b, "space")
	press(b, "left")
	equal(t, lines(b), []string{"[~] docs/**  1 selected", "[ ] src/**", "[ ] README.md", customOption})

	at(t, b, "docs/**")
	press(b, "space")
	equal(t, b.answer(), []string{"docs/**"})

	press(b, "right")
	equal(t, lines(b), []string{
		"[x] Select this directory: docs/**",
		"[x] docs/nested/**  covered by docs/**",
		"[x] docs/guide.md  covered by docs/**",
		"[x] docs/shared  covered by docs/**",
		customOption,
	})

	at(t, b, "docs/guide.md")
	press(b, "space")
	equal(t, b.answer(), []string{"docs/**"})

	at(t, b, "Select this directory")
	press(b, "space")
	equal(t, b.answer(), nil)
}

func TestBrowserReleasesFullySelectedDescendants(t *testing.T) {
	b := newBrowser(sample, nil, []string{"docs/nested/**", "docs/guide.md", "docs/shared"})

	if got := lines(b)[0]; got != "[x] docs/**  3 selected" {
		t.Fatalf("directory row = %q", got)
	}
	press(b, "space")
	equal(t, b.answer(), nil)
}

func TestBrowserReleasesACoveringAncestorFromInsideADescendant(t *testing.T) {
	b := newBrowser(sample, nil, []string{"docs/**"})

	press(b, "right", "down", "right")
	if b.breadcrumb() != "/docs/nested/" {
		t.Fatalf("breadcrumb = %q, want /docs/nested/", b.breadcrumb())
	}
	at(t, b, "Select this directory")
	press(b, "space")
	equal(t, b.answer(), nil)
}

func TestBrowserShowsCustomPatternCoverage(t *testing.T) {
	b := newBrowser(sample, nil, []string{"docs/**/*.md"})

	equal(t, lines(b), []string{
		"[~] docs/**  2 selected",
		"[ ] src/**",
		"[ ] README.md",
		"Custom patterns",
		"[x] docs/**/*.md",
		customOption,
	})
	press(b, "right")
	equal(t, lines(b), []string{
		"[~] Select this directory: docs/**  2 selected",
		"[~] docs/nested/**  1 selected",
		"[x] docs/guide.md  covered by a custom pattern",
		"[ ] docs/shared",
		"Custom patterns",
		"[x] docs/**/*.md",
		customOption,
	})
	at(t, b, "docs/guide.md")
	press(b, "space")
	equal(t, b.answer(), []string{"docs/**/*.md"})

	press(b, "left")
	at(t, b, "docs/**/*.md")
	press(b, "space")
	equal(t, b.answer(), nil)
}

func TestBrowserKeepsCustomPatternsWhenADirectoryCoversThem(t *testing.T) {
	b := newBrowser(sample, nil, []string{"docs/**/*.md"})

	at(t, b, "docs/**")
	press(b, "space")
	equal(t, b.answer(), []string{"docs/**", "docs/**/*.md"})
}

// foreign is the repository that already owns the nested documentation.
var foreign = []*entry{{name: "backend", Paths: []string{"docs/nested/**"}}}

func TestBrowserShowsForeignOwnershipAndRefusesOverlap(t *testing.T) {
	b := newBrowser(sample, foreign, nil)

	equal(t, lines(b), []string{"[!] docs/**  1 unavailable", "[ ] src/**", "[ ] README.md", customOption})

	at(t, b, "docs/**")
	press(b, "space")
	equal(t, b.answer(), nil)
	if b.err == nil {
		t.Fatal("selecting a conflicting directory was accepted")
	}

	press(b, "right", "down", "right")
	equal(t, lines(b), []string{
		"[!] Select this directory: docs/nested/**  1 unavailable",
		"[!] docs/nested/deep.md  owned by backend",
		customOption,
	})
	at(t, b, "deep.md")
	press(b, "space")
	equal(t, b.answer(), nil)

	press(b, "left")
	at(t, b, "docs/guide.md")
	press(b, "space")
	equal(t, b.answer(), []string{"docs/guide.md"})
}

func TestBrowserCombinesSelectedAndUnavailableCounts(t *testing.T) {
	b := newBrowser(sample, foreign, []string{"docs/guide.md"})

	equal(t, lines(b), []string{"[~] docs/**  1 selected, 1 unavailable", "[ ] src/**", "[ ] README.md", customOption})
}

func TestBrowserKeepsAnExistingConflictRemovableAndBlocksSubmission(t *testing.T) {
	b := newBrowser(sample, foreign, []string{"docs/**"})

	if got := lines(b)[0]; got != "[x] docs/**  conflicts with backend" {
		t.Fatalf("first row = %q", got)
	}
	if err := b.validate(); err == nil {
		t.Fatal("a conflicting answer was accepted")
	}
	press(b, "right", "down", "right")
	if got := lines(b)[1]; got != "[!] docs/nested/deep.md  owned by backend" {
		t.Fatalf("conflicting leaf = %q", got)
	}
	press(b, "left", "left")
	at(t, b, "docs/**")
	press(b, "space")
	if b.validate() == nil {
		t.Fatal("an empty answer was accepted")
	}
	at(t, b, "src/**")
	press(b, "space")
	if err := b.validate(); err != nil {
		t.Fatalf("resolved answer refused: %v", err)
	}
}

func TestBrowserFiltersOnlyTheCurrentDirectory(t *testing.T) {
	b := newBrowser(sample, nil, nil)

	press(b, "right", "/", "g")
	equal(t, lines(b), []string{"[ ] docs/guide.md"})

	press(b, "esc", "ctrl+a")
	equal(t, b.answer(), []string{"docs/guide.md"})

	press(b, "left")
	if b.filtered() {
		t.Fatalf("changing directory kept the filter %q", b.filter.Value())
	}
	equal(t, lines(b), []string{"[~] docs/**  1 selected", "[ ] src/**", "[ ] README.md", customOption})
}

func TestBrowserSelectsAllWithinTheAvailableScope(t *testing.T) {
	whole := newBrowser(sample, nil, nil)
	press(whole, "right", "ctrl+a")
	equal(t, whole.answer(), []string{"docs/**"})

	root := newBrowser(sample, foreign, nil)
	press(root, "ctrl+a")
	equal(t, root.answer(), []string{"README.md", "src/**"})
}

func TestBrowserFilteredSelectAllIgnoresUnavailableRows(t *testing.T) {
	b := newBrowser(sample, foreign, []string{"src/**"})

	press(b, "/", "s", "esc", "ctrl+a")
	equal(t, b.answer(), nil)
}

func TestBrowserRestoresTheCurrentAnswer(t *testing.T) {
	b := newBrowser(sample, nil, []string{"README.md", "src/**"})

	equal(t, lines(b), []string{"[ ] docs/**", "[x] src/**", "[x] README.md", customOption})
	equal(t, b.answer(), []string{"README.md", "src/**"})
}

func TestBrowserAsksForACustomPattern(t *testing.T) {
	b := newBrowser(sample, nil, nil)

	at(t, b, customOption)
	press(b, "space")
	if !b.custom {
		t.Fatal("the custom-pattern input was not requested")
	}
	b.set([]string{"docs/**/*.md"})
	if b.custom {
		t.Fatal("the custom request survived the input")
	}
	equal(t, b.answer(), []string{"docs/**/*.md"})
}
