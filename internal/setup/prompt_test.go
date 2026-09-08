package setup

import (
	"bufio"
	"io"
	"testing"
	"time"

	"github.com/charmbracelet/x/xpty"
)

// A pasted answer is stored once. A terminal delivers a paste as one bracketed
// sequence rather than as key presses, and a Huh group that hands such a
// message to the focused field twice stores the value twice, which silently
// wrote a doubled remote URL into the generated configuration.
func TestPastedAnswerIsStoredOnce(t *testing.T) {
	const pasted = "https://example.com/project.git"
	t.Setenv("TERM", "xterm-256color")

	terminal, err := xpty.NewPty(120, 24)
	if err != nil {
		t.Fatal(err)
	}
	defer terminal.Close()
	unix, ok := terminal.(*xpty.UnixPty)
	if !ok {
		t.Skip("the paste sequence needs a Unix terminal")
	}
	// The form renders while it waits, so the terminal must keep draining.
	go io.Copy(io.Discard, unix.Master())

	asked := &prompt{reader: bufio.NewReader(unix.Slave()), input: unix.Slave(), output: unix.Slave()}
	answered := make(chan string, 1)
	go func() {
		answer, _ := asked.value("Origin remote URL", "", func(string) error { return nil })
		answered <- answer
	}()

	// The field has to be running before the paste and has to see the whole
	// paste before the answer is submitted.
	time.Sleep(500 * time.Millisecond)
	io.WriteString(unix.Master(), "\x1b[200~"+pasted+"\x1b[201~")
	time.Sleep(300 * time.Millisecond)
	io.WriteString(unix.Master(), "\r")

	select {
	case answer := <-answered:
		if answer != pasted {
			t.Fatalf("answer = %q, want %q", answer, pasted)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the question never returned")
	}
}
