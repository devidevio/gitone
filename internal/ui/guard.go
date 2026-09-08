package ui

import (
	"bytes"
	"crypto/rand"
	"io"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

// heldLimit bounds the bytes a guard keeps back for a control sequence a
// write left unfinished. GitOne never prints a sequence anywhere near it, so
// repository data cannot make the guard buffer without end.
const heldLimit = 4096

// guard is the shared boundary between repository-controlled bytes and a
// person's terminal. Commit subjects, patch content, path names and Git error
// details all reach the terminal through it, so none of them can move the
// cursor, retitle the window, write the clipboard or hide what GitOne printed.
type guard struct {
	writer io.Writer
	parser *ansi.Parser
	held   []byte
	marker []byte
	color  bool
}

// Guard wraps a human-facing stream. GitOne's own cursor rendering writes to
// Raw(output) instead, because the guard exists to contain repository data,
// not GitOne's own presentation.
func Guard(output io.Writer) io.Writer {
	return &guard{
		writer: output,
		parser: ansi.NewParser(),
		marker: []byte("\x00" + rand.Text() + "\x00"),
		color:  ColorEnabled(output),
	}
}

// Raw is the stream a guard wraps, or output itself when it is not guarded.
// Exact bytes, such as the file content show writes, and GitOne's own live
// rendering go there.
func Raw(output io.Writer) io.Writer {
	if guarded, ok := output.(*guard); ok {
		return guarded.writer
	}
	return output
}

// Safe removes terminal controls from one complete value. It is used before
// repository text is given trusted GitOne styling.
func Safe(value string) string {
	var output bytes.Buffer
	guarded := &guard{writer: &output, parser: ansi.NewParser()}
	_, _ = guarded.Write([]byte(value))
	return output.String()
}

// trust marks the escapes GitOne's own styles generated. The per-stream random
// marker cannot occur in a value after Safe removed its NUL bytes, so repository
// data cannot grant itself trusted styling.
func (g *guard) trust(value string) string {
	if !g.color {
		return Safe(value)
	}
	marked := append(append([]byte{}, g.marker...), ansi.ESC)
	return string(bytes.ReplaceAll([]byte(value), []byte{ansi.ESC}, marked))
}

// Write forwards printable text, line breaks, tabs and explicitly trusted
// GitOne styling. It reports the whole input as written because dropping a
// control sequence is not a short write for the caller.
func (g *guard) Write(data []byte) (int, error) {
	rest := data
	if len(g.held) != 0 {
		rest = append(g.held, data...)
	}
	var kept []byte
	for len(rest) != 0 {
		if len(g.marker) != 0 && rest[0] == g.marker[0] {
			switch {
			case len(rest) < len(g.marker) && bytes.Equal(rest, g.marker[:len(rest)]):
				goto write
			case bytes.HasPrefix(rest, g.marker):
				trusted := rest[len(g.marker):]
				sequence, _, size, state := ansi.DecodeSequence(trusted, 0, g.parser)
				if len(trusted) == 0 || state != 0 && len(rest) <= heldLimit {
					goto write
				}
				if size != 0 && attribute(sequence) {
					kept = append(kept, sequence...)
				}
				if size == 0 {
					size = 1
				}
				rest = trusted[size:]
				continue
			}
		}

		first := rest[0]
		if first >= utf8.RuneSelf {
			if first >= 0x80 && first <= 0x9f {
				_, _, size, state := ansi.DecodeSequence(rest, 0, g.parser)
				if state != 0 && len(rest) <= heldLimit {
					goto write
				}
				if size == 0 {
					size = 1
				}
				rest = rest[size:]
				continue
			}
			if !utf8.FullRune(rest) {
				goto write
			}
			r, size := utf8.DecodeRune(rest)
			if r == utf8.RuneError && size == 1 {
				rest = rest[1:]
				continue
			}
			if r < 0x80 || r > 0x9f {
				kept = append(kept, rest[:size]...)
			}
			rest = rest[size:]
			continue
		}

		sequence, _, size, state := ansi.DecodeSequence(rest, 0, g.parser)
		if size == 0 || state != 0 && len(rest) <= heldLimit {
			goto write
		}
		if keepsText(sequence) {
			kept = append(kept, sequence...)
		}
		rest = rest[size:]
	}

write:
	g.held = append(g.held[:0], rest...)
	if len(kept) == 0 {
		return len(data), nil
	}
	if _, err := g.writer.Write(kept); err != nil {
		return 0, err
	}
	return len(data), nil
}

func keepsText(sequence []byte) bool {
	first := sequence[0]
	return first == '\n' || first == '\t' || first >= 0x20 && first < 0x7f
}

// attribute reports whether sequence selects a color or a text attribute.
func attribute(sequence []byte) bool {
	if !ansi.HasCsiPrefix(sequence) || sequence[len(sequence)-1] != 'm' {
		return false
	}
	parameters := sequence[1 : len(sequence)-1]
	if sequence[0] == ansi.ESC {
		parameters = sequence[2 : len(sequence)-1]
	}
	for _, parameter := range parameters {
		if (parameter < '0' || parameter > '9') && parameter != ';' && parameter != ':' {
			return false
		}
	}
	return true
}
