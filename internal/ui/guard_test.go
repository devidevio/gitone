package ui

import (
	"bytes"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// colored is a guard on a stream GitOne colors, which is the only state in
// which any escape sequence survives at all.
func colored(writer *bytes.Buffer) *guard {
	return &guard{writer: writer, parser: ansi.NewParser(), marker: []byte("\x00trusted\x00"), color: true}
}

func TestGuardDropsRepositoryTerminalSequences(t *testing.T) {
	payloads := map[string]string{
		"window title":    "subject\x1b]0;owned\x07",
		"hyperlink":       "subject\x1b]8;;file:///etc/passwd\x1b\\link\x1b]8;;\x1b\\",
		"clipboard":       "subject\x1b]52;c;ZXZpbA==\x07",
		"cursor":          "subject\x1b[2J\x1b[H",
		"carriage return": "harmless\rmalicious",
		"backspace":       "harmless\b\b\b\b\b\b\b\bmalicious",
		"bell":            "subject\x07",
		"escape alone":    "subject\x1bc",
		"device control":  "subject\x1bPqrogue\x1b\\",
		"c1 introducer":   "subject\x9b2J",
		"encoded c1":      "subject\u009b2J",
		"key modes":       "subject\x1b[>4;2m",
		"concealed text":  "subject\x1b[8mhidden\x1b[0m",
	}
	for name, payload := range payloads {
		t.Run(name, func(t *testing.T) {
			var written bytes.Buffer
			guarded := colored(&written)
			if _, err := guarded.Write([]byte(payload)); err != nil {
				t.Fatal(err)
			}
			got := written.String()
			if strings.ContainsAny(got, "\x1b\x07\x08\r\u009b") {
				t.Fatalf("guarded output = %q, want no control sequence", got)
			}
			if !strings.Contains(got, "subject") && !strings.Contains(got, "harmless") {
				t.Fatalf("guarded output = %q, want the readable text kept", got)
			}
		})
	}
}

func TestGuardKeepsPrintableTextAndUnicode(t *testing.T) {
	const want = "größe\tüber ✓ 日本語 (100%)\nsecond line\n"
	var written bytes.Buffer
	if _, err := colored(&written).Write([]byte(want)); err != nil {
		t.Fatal(err)
	}
	if got := written.String(); got != want {
		t.Fatalf("guarded output = %q, want %q", got, want)
	}
}

func TestGuardKeepsTrustedColorsOnAColoredStream(t *testing.T) {
	const want = "\x1b[31mred\x1b[0m and \x1b[1;38;5;204mbright\x1b[m\n"
	var written bytes.Buffer
	guarded := colored(&written)
	if _, err := guarded.Write([]byte(guarded.trust(want))); err != nil {
		t.Fatal(err)
	}
	if got := written.String(); got != want {
		t.Fatalf("guarded output = %q, want the colors kept", got)
	}
}

func TestStyleTrustsOnlyItsOwnColors(t *testing.T) {
	var written bytes.Buffer
	guarded := colored(&written)
	style := New(true).Error
	style.guard = guarded
	rendered := style.Render("visible\x1b[8mhidden\x1b[0m")
	if _, err := guarded.Write([]byte(rendered)); err != nil {
		t.Fatal(err)
	}
	got := written.String()
	if !strings.Contains(got, "\x1b[") || strings.Contains(got, "\x1b[8m") ||
		!strings.Contains(got, "visiblehidden") {
		t.Fatalf("styled output = %q, want trusted color around safe text", got)
	}
}

func TestGuardDropsColorsOnAPlainStream(t *testing.T) {
	var written bytes.Buffer
	// A buffer is not a terminal, so Guard makes exactly the plain stream a
	// redirected, dumb or NO_COLOR run gets.
	if _, err := Guard(&written).Write([]byte("\x1b[31mred\x1b[0m\n")); err != nil {
		t.Fatal(err)
	}
	if got := written.String(); got != "red\n" {
		t.Fatalf("plain output = %q, want no presentation codes", got)
	}
}

// A large patch reaches the guard through io.Copy, which splits it on buffer
// boundaries rather than on sequence boundaries.
func TestGuardDecidesSequencesSplitAcrossWrites(t *testing.T) {
	for split := 1; ; split++ {
		var written bytes.Buffer
		guarded := colored(&written)
		payload := "a" + guarded.trust("\x1b[31m") + "red\x1b]0;owned\x07b"
		if split >= len(payload) {
			break
		}
		for _, part := range []string{payload[:split], payload[split:]} {
			if _, err := guarded.Write([]byte(part)); err != nil {
				t.Fatal(err)
			}
		}
		if got := written.String(); got != "a\x1b[31mredb" {
			t.Fatalf("split at %d = %q", split, got)
		}
	}
}

func TestGuardKeepsUnicodeSplitAcrossWrites(t *testing.T) {
	const want = "größe über ✓ 日本語"
	for split := 1; split < len(want); split++ {
		var written bytes.Buffer
		guarded := colored(&written)
		for _, part := range []string{want[:split], want[split:]} {
			if _, err := guarded.Write([]byte(part)); err != nil {
				t.Fatal(err)
			}
		}
		if got := written.String(); got != want {
			t.Fatalf("split at %d = %q, want %q", split, got, want)
		}
	}
}

func TestGuardReportsEveryWrittenByteAsWritten(t *testing.T) {
	var written bytes.Buffer
	payload := []byte("\x1b]0;owned\x07")
	count, err := Guard(&written).Write(payload)
	if err != nil || count != len(payload) {
		t.Fatalf("write = %d, %v, want %d bytes and no error", count, err, len(payload))
	}
}

func TestRawIsTheGuardedStream(t *testing.T) {
	var written bytes.Buffer
	if Raw(Guard(&written)) != (&written) {
		t.Fatal("Raw does not return the wrapped stream")
	}
	if Raw(&written) != (&written) {
		t.Fatal("Raw changed an unguarded stream")
	}
}
