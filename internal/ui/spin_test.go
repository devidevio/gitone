package ui

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/charmbracelet/x/xpty"
)

type terminalBuffer struct {
	bytes.Buffer
	fd uintptr
}

func (w *terminalBuffer) Fd() uintptr { return w.fd }

// The spinner must never ask the terminal for its capabilities. A short action
// exits before the terminal answers, and the shell then reads the answer as
// typed input.
func TestSpinnerAsksTheTerminalNothing(t *testing.T) {
	var output bytes.Buffer
	if err := spin(&output, "Working", func() error { return nil }); err != nil {
		t.Fatalf("spin: %v", err)
	}
	for _, query := range []string{"$p", "\x1b]11", "\x1b[c"} {
		if strings.Contains(output.String(), query) {
			t.Fatalf("spinner asked the terminal %q: %q", query, output.String())
		}
	}
	if !strings.Contains(output.String(), "Working...") {
		t.Fatalf("spinner hid its progress: %q", output.String())
	}
}

// A report a spinner holds back is styled for the terminal it is copied to,
// not for the buffer that collects it, and it reaches that terminal through
// the same guard as unbuffered output.
func TestBufferedReportKeepsTheColorsOfItsStream(t *testing.T) {
	terminal, err := xpty.NewPty(80, 24)
	if err != nil {
		t.Fatal(err)
	}
	defer terminal.Close()

	// Setenv registers restoration; ColorEnabled treats even an empty value as disabled.
	t.Setenv("NO_COLOR", "")
	if err := os.Unsetenv("NO_COLOR"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TERM", "xterm-256color")

	written := terminalBuffer{fd: terminal.Fd()}
	stream := Guard(&written)
	err = SpinBuffered(strings.NewReader(""), stream, "Working", func(rendered io.Writer) error {
		fmt.Fprintln(rendered, For(rendered).Success.Render("✓ updated main"))
		fmt.Fprint(rendered, "\x1b]0;owned\x07")
		if written.Len() != 0 {
			t.Fatalf("report reached the terminal before the spinner cleared: %q", written.String())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("SpinBuffered: %v", err)
	}

	got := written.String()
	if !strings.Contains(got, "\x1b[") {
		t.Fatalf("buffered report = %q, want the palette applied", got)
	}
	if !strings.Contains(got, "✓ updated main") {
		t.Fatalf("buffered report = %q, want the text kept", got)
	}
	if strings.Contains(got, "\x1b]") {
		t.Fatalf("buffered report = %q, want repository controls removed", got)
	}
}

// The same report stays plain when the output is redirected, TERM is dumb or
// NO_COLOR is set, because it asks the one switch every command asks.
func TestBufferedReportIsPlainForARedirectedStream(t *testing.T) {
	var written bytes.Buffer
	err := SpinBuffered(strings.NewReader(""), &written, "Working", func(rendered io.Writer) error {
		fmt.Fprintln(rendered, For(rendered).Success.Render("✓ updated main"))
		return nil
	})
	if err != nil {
		t.Fatalf("SpinBuffered: %v", err)
	}
	if got := written.String(); got != "✓ updated main\n" {
		t.Fatalf("redirected report = %q, want it plain", got)
	}
}
