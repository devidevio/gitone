package ui_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/devidevio/gitone/internal/ui"
)

func TestPaletteCanBeDisabled(t *testing.T) {
	if got := ui.New(false).Success.Render("ok"); got != "ok" {
		t.Fatalf("plain success = %q", got)
	}
	if got := ui.New(true).Error.Render("failed"); !strings.Contains(got, "\x1b[") {
		t.Fatalf("colored error = %q", got)
	}
}

func TestRedirectedOutputIsPlain(t *testing.T) {
	if ui.ColorEnabled(new(bytes.Buffer)) {
		t.Fatal("a buffer supports color")
	}
}

func TestRenderLinesDoesNotPadMultilineErrors(t *testing.T) {
	got := ui.RenderLines(ui.New(true).Error, "long error\nshort")
	if strings.Contains(got, "short ") {
		t.Fatalf("rendered error contains padding: %q", got)
	}
}
