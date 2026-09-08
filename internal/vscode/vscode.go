// Package vscode installs the GitOne extension through Visual Studio Code's
// own command-line interface.
package vscode

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const ExtensionID = "devidevio.gitone"

// Available reports whether the official VS Code command is on PATH.
func Available() bool {
	_, err := exec.LookPath("code")
	return err == nil
}

// Installed reports whether GitOne is already installed in VS Code.
func Installed() (bool, error) {
	output, err := run("--list-extensions")
	if err != nil {
		return false, err
	}
	for _, extension := range strings.Fields(string(output)) {
		if strings.EqualFold(extension, ExtensionID) {
			return true, nil
		}
	}
	return false, nil
}

// Install installs the Marketplace extension or a local VSIX. Relative VSIX
// paths are resolved from cwd. force is never implied.
func Install(cwd, vsix string, force bool) error {
	target := ExtensionID
	if vsix != "" {
		if !filepath.IsAbs(vsix) {
			vsix = filepath.Join(cwd, vsix)
		}
		absolute, err := filepath.Abs(vsix)
		if err != nil {
			return fail("cannot resolve VSIX path: %v", err)
		}
		info, err := os.Stat(absolute)
		if err != nil {
			return fail("cannot read VSIX %s: %v", absolute, err)
		}
		if !info.Mode().IsRegular() {
			return fail("VSIX is not a regular file: %s", absolute)
		}
		target = absolute
	}

	arguments := []string{"--install-extension", target}
	if force {
		arguments = append(arguments, "--force")
	}
	_, err := run(arguments...)
	return err
}

func run(arguments ...string) ([]byte, error) {
	path, err := exec.LookPath("code")
	if err != nil {
		return nil, fail(`Visual Studio Code CLI "code" was not found in PATH`)
	}
	output, err := exec.Command(path, arguments...).CombinedOutput()
	if err == nil {
		return output, nil
	}
	detail := strings.TrimSpace(string(output))
	if detail == "" {
		detail = err.Error()
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return nil, fail("code exited with %d: %s", exit.ExitCode(), detail)
	}
	return nil, fail("code failed: %s", detail)
}

func fail(format string, arguments ...any) error {
	return fmt.Errorf("VSCODE001 %s", fmt.Sprintf(format, arguments...))
}
