// Package git executes the native git CLI. Every GitOne Git operation goes
// through this runner so hooks, credentials, Git configuration, signing and
// Git LFS keep behaving exactly as they do for a plain git invocation.
package git

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// indexVariable redirects git to another index. GitOne sets it deliberately
// and reports it in a failure, so it is named once here.
const indexVariable = "GIT_INDEX_FILE"

// stateVariables carry repository state. They are removed because they would
// silently redirect a command to a different repository, index or object
// store than the one GitOne selected.
var stateVariables = []string{
	"GIT_DIR",
	"GIT_WORK_TREE",
	indexVariable,
	"GIT_COMMON_DIR",
	"GIT_NAMESPACE",
	"GIT_OBJECT_DIRECTORY",
	"GIT_ALTERNATE_OBJECT_DIRECTORIES",
}

// Run executes git in directory and returns its standard output. Only the
// inherited repository state is dropped; user and system Git configuration
// stay in effect.
func Run(directory string, arguments ...string) (string, error) {
	return run(directory, environment(), "", arguments)
}

// RunInput executes git with input on standard input. It keeps large values
// such as a commit message out of the argument list, so a failure reports the
// command instead of the whole input.
func RunInput(directory, input string, arguments ...string) (string, error) {
	return run(directory, environment(), input, arguments)
}

// RunIndexed executes git against indexFile instead of the repository index
// and passes input on standard input. GitOne stages into a temporary index
// copy, so a real index only changes once the whole operation succeeded.
func RunIndexed(directory, indexFile, input string, arguments ...string) (string, error) {
	return run(directory, append(environment(), indexVariable+"="+indexFile), input, arguments)
}

// RunHook executes one repository hook against an explicit index. It keeps the
// real Git directory and shared working tree visible to the hook while letting
// a prepared index be checked before it can update a branch.
func RunHook(directory, gitDirectory, indexFile, name string, arguments ...string) error {
	path, err := RunIndexed(directory, indexFile, "", "--git-dir="+gitDirectory,
		"--work-tree="+directory, "rev-parse", "--git-path", "hooks/"+name)
	if err != nil {
		return err
	}
	path = strings.TrimSpace(path)
	if !filepath.IsAbs(path) {
		path = filepath.Join(directory, filepath.FromSlash(path))
	}
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) || err == nil && info.Mode().Perm()&0o111 == 0 {
		return nil
	}
	if err != nil {
		return err
	}

	command := exec.Command(path, arguments...)
	command.Dir = directory
	command.Env = append(environment(),
		"GIT_DIR="+gitDirectory,
		"GIT_WORK_TREE="+directory,
		indexVariable+"="+indexFile,
		"GIT_EDITOR=:",
	)
	command.Stdin = strings.NewReader("")
	output, err := command.CombinedOutput()
	if err != nil {
		detail := err.Error()
		if len(output) != 0 {
			detail = strings.TrimSpace(string(output))
		}
		return fmt.Errorf("GIT001 %s hook failed: %s", name, detail)
	}
	return nil
}

func environment() []string {
	return slices.DeleteFunc(os.Environ(), func(variable string) bool {
		name, _, _ := strings.Cut(variable, "=")
		return slices.Contains(stateVariables, name)
	})
}

// RunOffline executes git for an inspection that must never wait for a
// person. Its SSH command deliberately overrides the ambient one because an
// inherited command may still prompt through /dev/tty.
func RunOffline(directory string, arguments ...string) (string, error) {
	offline := slices.DeleteFunc(environment(), func(variable string) bool {
		name, _, _ := strings.Cut(variable, "=")
		return name == "GIT_TERMINAL_PROMPT" || name == "GIT_SSH_COMMAND"
	})
	offline = append(offline, "GIT_TERMINAL_PROMPT=0", "GIT_SSH_COMMAND=ssh -oBatchMode=yes")
	return run(directory, offline, "", arguments)
}

// RunIsolated executes git without any GIT_* variable and without user or
// system Git configuration. Use it for inspection whose result must not
// depend on the ambient Git setup.
func RunIsolated(directory string, arguments ...string) (string, error) {
	return run(directory, isolated(), "", arguments)
}

// RunIsolatedInput executes git like RunIsolated with input on standard
// input, and treats accepted as a result instead of a failure. check-ignore
// reports "no path matched" as exit code 1, which is an answer.
func RunIsolatedInput(directory, input string, accepted int, arguments ...string) (string, error) {
	return runAccepting(directory, isolated(), input, &accepted, arguments)
}

func isolated() []string {
	environment := slices.DeleteFunc(os.Environ(), func(variable string) bool {
		return strings.HasPrefix(variable, "GIT_")
	})
	return append(environment, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
}

// ErrUnavailable reports that git itself could not be executed. It separates
// a broken Git installation from a git that ran and rejected its input.
var ErrUnavailable = errors.New("GIT001 git could not be executed")

// CheckRefFormat asks native Git whether reference is a usable ref name, so
// every GitOne identifier follows Git's own grammar instead of a second one.
// A reference Git rejects reports false without an error; a git that could
// not run at all reports ErrUnavailable, because that is an operational
// failure and not invalid input.
func CheckRefFormat(reference string) (bool, error) {
	command := exec.Command("git", "check-ref-format", "--allow-onelevel", reference)
	command.Env = isolated()
	command.Stdin = strings.NewReader("")

	var exit *exec.ExitError
	switch err := command.Run(); {
	case err == nil:
		return true, nil
	case errors.As(err, &exit) && exit.ExitCode() == 1:
		return false, nil
	default:
		return false, fmt.Errorf("%w: git check-ref-format failed: %s", ErrUnavailable, err)
	}
}

func run(directory string, environment []string, input string, arguments []string) (string, error) {
	return runAccepting(directory, environment, input, nil, arguments)
}

func runAccepting(directory string, environment []string, input string, accepted *int, arguments []string) (string, error) {
	command := exec.Command("git", arguments...)
	command.Dir = directory
	command.Env = environment
	command.Stdin = strings.NewReader(input)
	output, err := command.Output()
	if err != nil {
		detail := err.Error()
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			if accepted != nil && exit.ExitCode() == *accepted {
				return string(output), nil
			}
			if len(exit.Stderr) != 0 {
				detail = strings.TrimSpace(string(exit.Stderr))
			}
		}
		return "", fmt.Errorf("GIT001 %s failed: %s", invocation(environment, arguments), detail)
	}
	return string(output), nil
}

// invocation renders the command as it actually ran. Staging happens against a
// temporary index copy, so the variable redirecting git there is part of the
// command: without it the reported line names the real index instead of the
// one that failed.
func invocation(environment, arguments []string) string {
	command := "git " + strings.Join(arguments, " ")
	for _, variable := range environment {
		if strings.HasPrefix(variable, indexVariable+"=") {
			return variable + " " + command
		}
	}
	return command
}

// Stream executes git and writes its output to stdout and stderr while it is
// produced, so an unbounded log, show or diff is never buffered first. It
// reports the exit code of git; a git that could not run at all is an error.
func Stream(directory string, stdout, stderr io.Writer, arguments ...string) (int, error) {
	command := exec.Command("git", arguments...)
	command.Dir = directory
	command.Env = environment()
	command.Stdin = strings.NewReader("")
	command.Stdout = stdout
	command.Stderr = stderr

	var exit *exec.ExitError
	switch err := command.Run(); {
	case err == nil:
		return 0, nil
	case errors.As(err, &exit) && exit.ExitCode() > 0:
		return exit.ExitCode(), nil
	default:
		return 1, fmt.Errorf("GIT001 git %s failed: %s", strings.Join(arguments, " "), err)
	}
}
