// Package ui contains the small amount of terminal presentation shared by
// GitOne's human-facing commands. Machine-readable output never uses it.
package ui

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
	"github.com/devidevio/gitone/internal/config"
)

// Styles is GitOne's fixed terminal palette.
type Styles struct {
	Heading Style
	Success Style
	Warning Style
	Error   Style
	Muted   Style
	Branch  Style
}

// Style sanitizes the value before adding presentation and marks only the
// resulting GitOne escapes as trusted when it belongs to a guarded stream.
type Style struct {
	style lipgloss.Style
	guard *guard
}

func (s Style) Render(values ...string) string {
	safe := make([]string, len(values))
	for index := range values {
		safe[index] = Safe(values[index])
	}
	rendered := s.style.Render(safe...)
	if s.guard != nil {
		return s.guard.trust(rendered)
	}
	return rendered
}

// New returns the palette, or plain styles when color is disabled.
func New(enabled bool) Styles {
	if !enabled {
		return Styles{}
	}
	return Styles{
		Heading: Style{style: lipgloss.NewStyle().Bold(true)},
		Success: Style{style: lipgloss.NewStyle().Foreground(lipgloss.Green)},
		Warning: Style{style: lipgloss.NewStyle().Foreground(lipgloss.Yellow)},
		Error:   Style{style: lipgloss.NewStyle().Foreground(lipgloss.Red)},
		Muted:   Style{style: lipgloss.NewStyle().Foreground(lipgloss.BrightBlack)},
		Branch:  Style{style: lipgloss.NewStyle().Foreground(lipgloss.Cyan)},
	}
}

// For selects colored or plain styles for output.
func For(output io.Writer) Styles {
	styles := New(ColorEnabled(output))
	if collected, ok := output.(*buffered); ok {
		output = collected.stream
	}
	if guarded, ok := output.(*guard); ok {
		styles.Heading.guard = guarded
		styles.Success.guard = guarded
		styles.Warning.guard = guarded
		styles.Error.guard = guarded
		styles.Muted.guard = guarded
		styles.Branch.guard = guarded
	}
	return styles
}

// RenderLines colors multiline text without padding shorter lines to the
// longest one, which Lip Gloss does for a single multiline block.
func RenderLines(style Style, value string) string {
	lines := strings.Split(value, "\n")
	for i := range lines {
		lines[i] = style.Render(lines[i])
	}
	return strings.Join(lines, "\n")
}

// ColorEnabled follows the usual automatic CLI behavior: only a real terminal
// gets color, unless the user disabled it explicitly.
func ColorEnabled(output io.Writer) bool {
	_, noColor := os.LookupEnv("NO_COLOR")
	return !noColor && renderable(output)
}

// Live reports whether input and output can safely use cursor-based rendering.
func Live(input io.Reader, output io.Writer) bool {
	return renderable(input) && renderable(output)
}

// Plain reports whether a command must keep its stable, unstyled output: no
// color and no cursor-based rendering. It is the single switch every
// human-facing command asks, so spinners and setup's questions agree.
func Plain(input io.Reader, output io.Writer) bool {
	return !ColorEnabled(output) || !Live(input, output)
}

// renderable reports whether a stream is a terminal GitOne may render on.
// It is the one place that decides what counts as a usable terminal. A
// guarded or buffered stream is asked about the terminal it stands for, so
// neither wrapping nor buffering output turns colored output plain.
func renderable(stream any) bool {
	switch source := stream.(type) {
	case *buffered:
		return renderable(source.stream)
	case *guard:
		return renderable(source.writer)
	}
	return os.Getenv("TERM") != "dumb" && Terminal(stream)
}

// Terminal reports whether stream is a terminal. It exists so that every
// package asks the same question of standard input and output.
func Terminal(stream any) bool {
	file, ok := stream.(interface{ Fd() uintptr })
	return ok && term.IsTerminal(file.Fd())
}

// Spin runs one slow, silent action with live feedback when possible and a
// stable line otherwise.
func Spin(input io.Reader, output io.Writer, title string, action func() error) error {
	if Plain(input, output) {
		return action()
	}
	// The frames are GitOne's own cursor rendering, so they bypass the guard
	// that stops repository data from moving the cursor.
	return spin(Raw(output), title, action)
}

// spin draws the frames itself instead of starting a Bubble Tea program. Bubble
// Tea asks the terminal for the synchronized output and unicode modes and for
// the background color. A short action finishes before those answers arrive, so
// the shell reads them as typed input once GitOne exits. Writing frames asks the
// terminal nothing, so nothing can be left unread.
func spin(output io.Writer, title string, action func() error) error {
	done := make(chan error, 1)
	go func() { done <- action() }()

	frames := [...]string{"|", "/", "-", "\\"}
	style := For(output).Branch
	ticker := time.NewTicker(time.Second / 10)
	defer ticker.Stop()

	for frame := 0; ; frame++ {
		fmt.Fprint(output, "\r"+style.Render(frames[frame%len(frames)])+" "+title+"..."+ansi.EraseLineRight)
		select {
		case err := <-done:
			fmt.Fprint(output, "\r"+ansi.EraseLineRight)
			return err
		case <-ticker.C:
		}
	}
}

// buffered is the writer a SpinBuffered action renders to. Writes are held
// back until the spinner is cleared, while styles and the trusted-escape
// guard follow stream, the output the report is copied to.
type buffered struct {
	bytes.Buffer
	stream io.Writer
}

// SpinBuffered keeps action output away from the spinner and prints it once
// live rendering has finished. The report is styled for the real output, so a
// buffered command is colored exactly like an unbuffered one, and it reaches
// that output through the same guard.
func SpinBuffered(input io.Reader, output io.Writer, title string, action func(io.Writer) error) error {
	rendered := &buffered{stream: output}
	err := Spin(input, output, title, func() error { return action(rendered) })
	_, _ = io.Copy(output, rendered)
	return err
}

// Repositories lists the configured repositories as successful lines. Init,
// migrate and setup all report their result this way.
func Repositories(output io.Writer, configuration *config.Config) {
	style := For(output)
	for _, name := range configuration.RepositoryNames() {
		fmt.Fprintln(output, style.Success.Render(fmt.Sprintf("✓ %s (%s)", name, configuration.DefaultBranch)))
	}
}
