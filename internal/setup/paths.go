package setup

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"charm.land/huh/v2"
	"github.com/devidevio/gitone/internal/config"
)

// customOption is the checkbox entry that continues into text input, so a
// pattern the working tree cannot suggest is still one selection away.
const customOption = "Custom pattern..."

// patternHelp names the three shapes of the path grammar once, for both
// question styles.
const patternHelp = "Exact path such as README.md, a whole tree such as src/** or a glob such as docs/**/*.md."

// paths collects the ownership patterns of one repository. others are the
// repositories already collected, whose ownership a selection must never
// overlap; current are the patterns this repository has now, which stay
// selected when a correction reopens the question.
func (p *prompt) paths(others []*entry, current []string) ([]string, bool) {
	if !p.plain {
		return p.browse(others, current)
	}
	options := slices.Clone(current)
	for _, suggestion := range p.suggestions {
		if slices.Contains(options, suggestion.Pattern) {
			continue
		}
		// A suggestion an earlier repository already covers is not offered,
		// because selecting it could only make the path ambiguous.
		if conflict(others, suggestion.Paths, suggestion.Pattern) == "" {
			options = append(options, suggestion.Pattern)
		}
	}
	slices.Sort(options)
	options = append(options, customOption)

	selected, ok := p.selectPatterns(options, current)
	if !ok {
		return nil, false
	}
	asked := slices.Contains(selected, customOption)
	chosen := slices.DeleteFunc(selected, func(option string) bool { return option == customOption })
	if !asked {
		slices.Sort(chosen)
		return chosen, true
	}
	return p.custom(others, chosen)
}

// browse asks a normal terminal for the owned paths with the navigable path
// browser. The custom-pattern input runs outside the form, because a field
// cannot open another one, and the browser is reopened with the same position
// afterwards.
func (p *prompt) browse(others []*entry, current []string) ([]string, bool) {
	field := newBrowser(p.relevant, others, current)
	for {
		if !p.run(field) {
			return nil, false
		}
		if !field.custom {
			return field.answer(), true
		}
		chosen, ok := p.custom(others, field.answer())
		if !ok {
			return nil, false
		}
		field.set(chosen)
	}
}

// selectPatterns shows the offered patterns as a stable numbered list. It
// answers the plain-terminal question only; a normal terminal browses the
// hierarchy instead.
func (p *prompt) selectPatterns(options, selected []string) ([]string, bool) {
	fmt.Fprintf(p.output, "Owned paths, offered from the files this project has.\n")
	for index, option := range options {
		fmt.Fprintf(p.output, "  %d) %s\n", index+1, option)
	}
	fallback := numbers(options, selected)
	answer, ok := p.askUntil(question("Select comma-separated numbers", fallback), fallback, func(answer string) error {
		_, err := selection(options, answer)
		return err
	})
	if !ok {
		return nil, false
	}
	chosen, _ := selection(options, answer)
	return chosen, true
}

func require(chosen []string) error {
	if len(chosen) == 0 {
		return errors.New("select at least one path or " + customOption)
	}
	return nil
}

// selection turns a comma-separated answer into the selected options.
func selection(options []string, answer string) ([]string, error) {
	var chosen []string
	for _, field := range strings.Split(answer, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		number, err := strconv.Atoi(field)
		if err != nil || number < 1 || number > len(options) {
			return nil, fmt.Errorf("answer numbers between 1 and %d, separated by commas", len(options))
		}
		if !slices.Contains(chosen, options[number-1]) {
			chosen = append(chosen, options[number-1])
		}
	}
	return chosen, require(chosen)
}

// numbers renders the currently selected options as the answer that keeps
// them, so a reopened question can be confirmed with Enter.
func numbers(options, selected []string) string {
	var listed []string
	for index, option := range options {
		if slices.Contains(selected, option) {
			listed = append(listed, strconv.Itoa(index+1))
		}
	}
	return strings.Join(listed, ",")
}

// custom asks for patterns of the user's own until an empty answer ends the
// list. Every pattern is checked against the repositories already collected
// immediately, so a conflict is corrected here instead of after the complete
// configuration. A pattern that matches nothing today is a warning, not an
// error: the file it describes may still be created.
func (p *prompt) custom(others []*entry, chosen []string) ([]string, bool) {
	if p.plain {
		fmt.Fprintf(p.output, "Custom patterns, one per line, empty line ends the list.\n%s\n", patternHelp)
	}
	for {
		check := func(answer string) error {
			switch {
			case answer == "" && len(chosen) == 0:
				return errors.New("at least one path is required")
			case answer == "":
				return nil
			}
			if err := config.ValidatePattern(answer); err != nil {
				return err
			}
			if slices.ContainsFunc(chosen, func(listed string) bool {
				return config.FoldPath(listed) == config.FoldPath(answer)
			}) {
				return errors.New("path is already listed")
			}
			if name := used(others, answer); name != "" {
				return fmt.Errorf("path is already configured for repository %q", name)
			}
			if name := conflict(others, p.relevant, answer); name != "" {
				return fmt.Errorf("path overlaps repository %q", name)
			}
			return nil
		}

		var answer string
		var ok bool
		if p.plain {
			answer, ok = p.askUntil("  pattern: ", "", check)
		} else {
			ok = p.run(huh.NewInput().
				Title("Custom pattern").
				Description(patternHelp + " Leave empty when done.").
				Value(&answer).
				Validate(func(entered string) error { return check(strings.TrimSpace(entered)) }))
			answer = strings.TrimSpace(answer)
		}
		if !ok {
			return nil, false
		}
		if answer == "" {
			slices.Sort(chosen)
			return chosen, true
		}
		if !slices.ContainsFunc(p.relevant, func(name string) bool { return config.Owns(answer, name) }) {
			fmt.Fprintf(p.output, "  %s matches no file this project has right now.\n", answer)
			keep, ok := p.confirm("Keep it for files created later?", false)
			if !ok {
				return nil, false
			}
			if !keep {
				continue
			}
		}
		chosen = append(chosen, answer)
	}
}

// used reports the repository that already configured pattern, including a
// spelling that differs only in case, which the loader rejects as a duplicate.
func used(others []*entry, pattern string) string {
	for _, other := range others {
		if slices.ContainsFunc(other.Paths, func(owned string) bool {
			return config.FoldPath(owned) == config.FoldPath(pattern)
		}) {
			return other.name
		}
	}
	return ""
}

// conflict reports the repository whose ownership pattern would overlap on a
// path the project actually has. Patterns that overlap only in theory stay
// allowed, exactly as the loader allows them.
func conflict(others []*entry, paths []string, pattern string) string {
	for _, other := range others {
		for _, name := range paths {
			if !config.Owns(pattern, name) {
				continue
			}
			if slices.ContainsFunc(other.Paths, func(owned string) bool { return config.Owns(owned, name) }) {
				return other.name
			}
		}
	}
	return ""
}
