package cli

import (
	"os"
	"testing"
)

func TestInteractiveRejectsNonTerminalCharacterDevice(t *testing.T) {
	file, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	if interactive(file) {
		t.Fatal("os.DevNull is not an interactive terminal")
	}
}
