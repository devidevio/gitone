package vscode_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/devidevio/gitone/internal/vscode"
)

func fakeCode(t *testing.T, body string) string {
	t.Helper()
	directory := t.TempDir()
	path := filepath.Join(directory, "code")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	return path
}

func TestInstalledUsesTheCodeExtensionList(t *testing.T) {
	fakeCode(t, `
if [ "$1" = "--list-extensions" ]; then
  printf 'another.extension\nDevidevio.GitOne\n'
  exit 0
fi
exit 1
`)

	installed, err := vscode.Installed()
	if err != nil || !installed {
		t.Fatalf("installed = %t, error = %v", installed, err)
	}
}

func TestInstallUsesMarketplaceOrLocalVSIXExplicitly(t *testing.T) {
	log := filepath.Join(t.TempDir(), "arguments")
	t.Setenv("GITONE_CODE_LOG", log)
	fakeCode(t, `
printf '%s\n' "$@" > "$GITONE_CODE_LOG"
`)

	root := t.TempDir()
	if err := vscode.Install(root, "", false); err != nil {
		t.Fatal(err)
	}
	if got := read(t, log); got != "--install-extension\n"+vscode.ExtensionID+"\n" {
		t.Fatalf("marketplace arguments = %q", got)
	}

	vsix := filepath.Join(root, "gitone.vsix")
	if err := os.WriteFile(vsix, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := vscode.Install(root, "gitone.vsix", true); err != nil {
		t.Fatal(err)
	}
	if got := read(t, log); got != "--install-extension\n"+vsix+"\n--force\n" {
		t.Fatalf("VSIX arguments = %q", got)
	}
}

func TestInstallReportsMissingCodeAndVSIX(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PATH", root)
	if vscode.Available() {
		t.Fatal("code is available despite an empty test PATH")
	}
	if err := vscode.Install(root, "", false); err == nil || !strings.HasPrefix(err.Error(), "VSCODE001 ") {
		t.Fatalf("missing code error = %v", err)
	}

	fakeCode(t, "exit 0\n")
	if err := vscode.Install(root, "missing.vsix", false); err == nil || !strings.HasPrefix(err.Error(), "VSCODE001 ") {
		t.Fatalf("missing VSIX error = %v", err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(contents)
}
