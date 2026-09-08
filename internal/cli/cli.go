package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/devidevio/gitone/internal/agents"
	"github.com/devidevio/gitone/internal/assign"
	"github.com/devidevio/gitone/internal/branch"
	"github.com/devidevio/gitone/internal/clone"
	"github.com/devidevio/gitone/internal/config"
	"github.com/devidevio/gitone/internal/diff"
	"github.com/devidevio/gitone/internal/doctor"
	"github.com/devidevio/gitone/internal/fetch"
	"github.com/devidevio/gitone/internal/log"
	"github.com/devidevio/gitone/internal/migrate"
	"github.com/devidevio/gitone/internal/policy"
	"github.com/devidevio/gitone/internal/pull"
	"github.com/devidevio/gitone/internal/push"
	"github.com/devidevio/gitone/internal/reconfigure"
	"github.com/devidevio/gitone/internal/repository"
	"github.com/devidevio/gitone/internal/setup"
	"github.com/devidevio/gitone/internal/show"
	"github.com/devidevio/gitone/internal/stage"
	"github.com/devidevio/gitone/internal/status"
	"github.com/devidevio/gitone/internal/switching"
	"github.com/devidevio/gitone/internal/ui"
	"github.com/devidevio/gitone/internal/vscode"
)

func Run(args []string, cwd string, stdin io.Reader, stdout, stderr io.Writer) int {
	// Everything a person reads leaves through the shared guard, so a commit
	// subject, a patch, a path name or a Git error detail cannot drive the
	// terminal from inside a repository. It wraps both streams once, here,
	// instead of every command having to remember it.
	stdout, stderr = ui.Guard(stdout), ui.Guard(stderr)

	// Help is answered before the project is discovered, so it works from any
	// directory and reads or changes nothing.
	if page, ok := helpPage(args); ok {
		writeHelp(stdout, page)
		return 0
	}

	command := strings.Join(args, " ")
	target, yes, validPush := "", false, false
	showRepository, showSource, showPath, validShow := parseShow(args)
	vsix, force, validVSCodeInstall := parseVSCodeInstall(args)
	if len(args) != 0 && args[0] == "push" {
		target, yes, validPush = parsePush(args[1:])
	}
	diffTarget, staged, validDiff := "", false, false
	if len(args) != 0 && args[0] == "diff" {
		diffTarget, staged, validDiff = parseDiff(args[1:])
	}
	cloneURL, cloneDirectory, validClone := "", "", false
	if len(args) != 0 && args[0] == "clone" {
		cloneURL, cloneDirectory, validClone = parseClone(args[1:])
	}
	fetchTarget, validFetch := "", false
	if len(args) != 0 && args[0] == "fetch" {
		fetchTarget, validFetch = parseFetch(args[1:])
	}
	logTarget, logCount, validLog := "", 0, false
	if len(args) != 0 && args[0] == "log" {
		logTarget, logCount, validLog = parseLog(args[1:])
	}
	branchName, branchDelete, validBranch := "", false, false
	if len(args) != 0 && args[0] == "branch" {
		branchName, branchDelete, validBranch = parseBranch(args[1:])
	}
	pullTarget, pullAccept, pullAssign, validPull := "", false, false, false
	if len(args) != 0 && args[0] == "pull" {
		pullTarget, pullAccept, pullAssign, validPull = parsePull(args[1:])
	}
	switchBranch, switchCreate, switchAccept, switchAssign, validSwitch := "", false, false, false, false
	if len(args) != 0 && args[0] == "switch" {
		switchBranch, switchCreate, switchAccept, switchAssign, validSwitch = parseSwitch(args[1:])
	}
	reconfigureSource, reconfigureAccept := reconfigure.Source{}, reconfigure.Accepted{}
	reconfigureRenames, validReconfigure := []reconfigure.Rename(nil), false
	if len(args) != 0 && args[0] == "reconfigure" {
		reconfigureSource, reconfigureAccept, reconfigureRenames, validReconfigure = parseReconfigure(args[1:])
	}
	restorePaths, restoreTarget, validRestore := []string(nil), stage.RestoreTarget{}, false
	if len(args) != 0 && args[0] == "restore" {
		restorePaths, restoreTarget, validRestore = parseRestore(args[1:])
	}
	commitMessage, commitStandardInput, commitPaths, validCommit := "", false, []string(nil), false
	if len(args) != 0 && args[0] == "commit" {
		commitMessage, commitStandardInput, commitPaths, validCommit = parseCommit(args[1:])
	}
	backupCommand, backupID, backupArguments, backupYes := "", "", []string(nil), false
	if len(args) != 0 && args[0] == "backup" {
		backupCommand, backupID, backupArguments, backupYes = parseBackup(args[1:])
	}
	supported := validPush || validDiff || validFetch || validPull || validClone || validLog || validBranch || validSwitch || validRestore || validCommit || validReconfigure || backupCommand != "" ||
		len(args) == 1 && (args[0] == "--version" || args[0] == "init" || args[0] == "recover" || args[0] == "abort" || args[0] == "doctor" || args[0] == "setup") ||
		len(args) != 0 && args[0] == "migrate" && (len(args) == 1 || len(args) == 2 && args[1] == "--yes") ||
		len(args) == 2 && args[0] == "repo" && (args[1] == "validate" || args[1] == "list") ||
		len(args) != 0 && args[0] == "agents" && (len(args) == 1 || len(args) == 2 && args[1] == "update") ||
		len(args) == 3 && args[0] == "vscode" && args[1] == "info" && args[2] == "--json" ||
		validVSCodeInstall ||
		len(args) != 0 && args[0] == "status" && (len(args) == 1 ||
			len(args) == 2 && (args[1] == "--porcelain" || args[1] == "--json")) ||
		len(args) != 0 && args[0] == "add" && supportedAdd(args[1:]) ||
		len(args) != 0 && args[0] == "unstage" && supportedUnstage(args[1:]) ||
		validShow
	if !supported {
		message := fmt.Sprintf("CLI001 unsupported command: gitone %s", command)
		if len(args) != 0 && args[0] == "reconfigure" {
			message += fmt.Sprintf("; accepted form: gitone reconfigure [%s | %s <branch>] [%s] [%s] [%s] [%s <old>=<new>]...",
				reconfigure.PullFlag, reconfigure.SwitchFlag, reconfigure.AcceptFlag,
				policy.AcceptFlag, assign.Flag, reconfigure.RenameFlag)
		}
		printError(stderr, message)
		fmt.Fprintln(stderr, helpHint(args))
		return 1
	}

	if args[0] == "--version" {
		fmt.Fprintf(stdout, "gitone %s\n", releaseVersion())
		return 0
	}
	if args[0] == "vscode" {
		if args[1] == "install" {
			if err := ui.Spin(stdin, stdout, "Installing the GitOne VS Code extension", func() error {
				return vscode.Install(cwd, vsix, force)
			}); err != nil {
				printError(stderr, err)
				return 1
			}
			fmt.Fprintln(stdout, ui.For(stdout).Success.Render("GitOne VS Code extension installed."))
			return 0
		}
		if err := json.NewEncoder(stdout).Encode(struct {
			Protocol      int    `json:"protocol"`
			GitOneVersion string `json:"gitone_version"`
		}{Protocol: 1, GitOneVersion: releaseVersion()}); err != nil {
			printError(stderr, err)
			return 1
		}
		return 0
	}

	// doctor reports an unusable configuration as a failed check instead of
	// stopping on it, so it inspects the project itself.
	if args[0] == "doctor" {
		if !doctor.Report(cwd, stdout) {
			return 1
		}
		return 0
	}

	// setup creates the configuration a project may still be missing, so it
	// runs before the configuration is loaded.
	if args[0] == "setup" {
		if err := setup.Setup(cwd, interactive(stdin), stdin, stdout); err != nil {
			printError(stderr, err)
			return 1
		}
		return 0
	}

	// clone creates the project itself, so it runs outside any existing one
	// and before the configuration is loaded.
	if args[0] == "clone" {
		if err := clone.Clone(cloneURL, cloneDirectory, cwd, stdin, stdout); err != nil {
			printError(stderr, err)
			return 1
		}
		return 0
	}

	configuration, root, err := config.Load(cwd)
	if err != nil {
		printError(stderr, err)
		return 1
	}
	// Migration-backup recovery is exactly what an unusable project needs, so
	// it runs before the readiness check instead of behind it.
	if args[0] == "backup" {
		switch backupCommand {
		case "list":
			if err := migrate.BackupList(root, stdout); err != nil {
				printError(stderr, err)
				return 1
			}
		case "git":
			code, err := migrate.BackupGit(root, backupID, backupArguments, stdout, stderr)
			if err != nil {
				printError(stderr, err)
			}
			return code
		default:
			if err := migrate.BackupRestore(root, backupID, backupYes, interactive(stdin), stdin, stdout); err != nil {
				printError(stderr, err)
				return 1
			}
		}
		return 0
	}

	// The shared readiness check runs once, at the boundary, before any
	// command touches repository state.
	if slices.Contains(repositoryStateCommands, args[0]) {
		if err := repository.Ready(configuration, root); err != nil {
			printError(stderr, err)
			return 1
		}
	}

	switch args[0] {
	case "init":
		if err := ui.Spin(stdin, stdout, "Initializing managed repositories", func() error {
			return repository.Init(configuration, root)
		}); err != nil {
			printError(stderr, err)
			return 1
		}
		printInitialization(stdout, configuration)
	case "migrate":
		// migrate.Migrate shows its own progress, because the plan and the
		// confirmation come before the work a spinner may cover.
		if err := migrate.Migrate(configuration, root, len(args) == 2, interactive(stdin), stdin, stdout); err != nil {
			printError(stderr, err)
			return 1
		}
	case "agents":
		if len(args) == 1 {
			if err := agents.Check(configuration, root); err != nil {
				printError(stderr, err)
				return 1
			}
			fmt.Fprintf(stdout, "%s holds the current GitOne instructions.\n", agents.File)
			break
		}
		if err := agents.Ownership(configuration); err != nil {
			printError(stderr, err)
			return 1
		}
		state, err := agents.Update(root)
		if err != nil {
			printError(stderr, err)
			return 1
		}
		fmt.Fprintln(stdout, agentsResult(state))
	case "repo":
		if args[1] == "validate" {
			printValidation(stdout, configuration)
		} else if err := printRepositories(stdout, configuration, root); err != nil {
			printError(stderr, err)
			return 1
		}
	case "add":
		if err := stage.Add(configuration, root, cwd, args[1:], stdout); err != nil {
			printError(stderr, err)
			return 1
		}
	case "unstage":
		if err := stage.Unstage(configuration, root, cwd, args[1:], stdout); err != nil {
			printError(stderr, err)
			return 1
		}
	case "restore":
		if err := stage.Restore(configuration, root, cwd, restorePaths, restoreTarget, stdout); err != nil {
			printError(stderr, err)
			return 1
		}
	case "commit":
		message := commitMessage
		if commitStandardInput {
			if stdin == nil {
				stdin = strings.NewReader("")
			}
			contents, readErr := io.ReadAll(stdin)
			if readErr != nil {
				printError(stderr, readErr)
				return 1
			}
			message = string(contents)
		}
		if err := stage.Commit(configuration, root, cwd, commitPaths, message, stdout); err != nil {
			printError(stderr, err)
			return 1
		}
	case "show":
		contents, err := show.Content(configuration, root, cwd, showRepository, showSource, showPath)
		if err != nil {
			printError(stderr, err)
			var missing *show.MissingError
			if errors.As(err, &missing) {
				return 2
			}
			return 1
		}
		// show returns the exact blob bytes an editor diffs against, so it
		// writes past the guard instead of having its content rewritten.
		if _, err := io.WriteString(ui.Raw(stdout), contents); err != nil {
			printError(stderr, err)
			return 1
		}
	case "diff":
		if err := diff.Diff(configuration, root, diffTarget, staged, stdout); err != nil {
			printError(stderr, err)
			return 1
		}
	case "log":
		if err := log.Log(configuration, root, logTarget, logCount, stdout); err != nil {
			printError(stderr, err)
			return 1
		}
	case "branch":
		if err := branch.Branch(configuration, root, branchName, branchDelete, stdin, stdout); err != nil {
			printError(stderr, err)
			return 1
		}
	case "fetch":
		if err := fetch.Fetch(configuration, root, fetchTarget, stdin, stdout); err != nil {
			printError(stderr, err)
			return 1
		}
	case "pull":
		if err := pull.Pull(configuration, root, pullTarget, pullAccept, pullAssign, interactive(stdin), stdin, stdout); err != nil {
			printError(stderr, err)
			return 1
		}
	case "switch":
		if err := switching.Switch(configuration, root, switchBranch, switchCreate,
			switchAccept, switchAssign, interactive(stdin), stdin, stdout); err != nil {
			printError(stderr, err)
			return 1
		}
	case "reconfigure":
		if err := reconfigure.Reconfigure(configuration, root, reconfigureSource, reconfigureRenames,
			reconfigureAccept, interactive(stdin), stdin, stdout); err != nil {
			printError(stderr, err)
			return 1
		}
	case "push":
		if err := push.Push(configuration, root, target, yes, interactive(stdin), stdin, stdout); err != nil {
			printError(stderr, err)
			return 1
		}
	case "recover":
		if err := stage.Recover(configuration, root, stdout); err != nil {
			printError(stderr, err)
			return 1
		}
	case "abort":
		if err := stage.Abort(configuration, root, stdout); err != nil {
			printError(stderr, err)
			return 1
		}
	default:
		return printStatus(command, configuration, root, stdout, stderr)
	}
	return 0
}

// agentsResult names what an update did, so a person can tell an unchanged
// file from a created, extended, marked or replaced one.
func agentsResult(state agents.State) string {
	switch state {
	case agents.Missing:
		return agents.File + " was created with the GitOne instructions."
	case agents.Absent:
		return "The GitOne instructions were added to " + agents.File + "."
	case agents.Unmarked:
		return "The GitOne instructions in " + agents.File + " are now marked."
	case agents.Stale:
		return "The GitOne instructions in " + agents.File + " were updated."
	default:
		return agents.File + " already holds the current GitOne instructions. Nothing was changed."
	}
}

// repositoryStateCommands read or change managed repository state and are
// therefore refused before doing any work while the project is not usable.
// Every other command either works before initialization, performs it or
// repairs it: help, --version, clone, setup, init, migrate, backup, repo,
// reconfigure, recover, abort, agents, doctor and vscode. reconfigure creates
// a repository the configuration added and recover and abort finish or undo
// an interrupted one, so the readiness check would refuse exactly the
// projects they exist to repair.
var repositoryStateCommands = []string{
	"status", "add", "unstage", "restore", "commit", "show", "diff", "log", "branch", "switch", "fetch", "pull", "push",
}

// parseClone accepts the clone input: one bootstrap URL and an optional
// destination directory. Every Git clone option, including --branch, --depth
// and sparse or partial clone, is rejected instead of being forwarded.
func parseClone(arguments []string) (url, directory string, ok bool) {
	if len(arguments) == 0 || len(arguments) > 2 {
		return "", "", false
	}
	for _, argument := range arguments {
		if argument == "" || strings.HasPrefix(argument, "-") {
			return "", "", false
		}
	}
	if len(arguments) == 2 {
		directory = arguments[1]
	}
	return arguments[0], directory, true
}

func parseVSCodeInstall(arguments []string) (vsix string, force, ok bool) {
	if len(arguments) < 2 || arguments[0] != "vscode" || arguments[1] != "install" {
		return "", false, false
	}
	for i := 2; i < len(arguments); i++ {
		switch arguments[i] {
		case "--force":
			if force {
				return "", false, false
			}
			force = true
		case "--vsix":
			if vsix != "" || i+1 == len(arguments) || arguments[i+1] == "" || strings.HasPrefix(arguments[i+1], "-") {
				return "", false, false
			}
			i++
			vsix = arguments[i]
		default:
			return "", false, false
		}
	}
	return vsix, force, true
}

// version is the release tag. Release builds set it with
// -ldflags "-X github.com/devidevio/gitone/internal/cli.version=<tag>".
var version string

// releaseVersion reports the release tag of the running binary: the value
// stamped into a release build, otherwise the module version recorded by
// "go install <module>/cmd/gitone@<version>".
func releaseVersion() string {
	if version != "" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		return info.Main.Version
	}
	return "devel"
}

// supportedAdd reports whether the add arguments are accepted input: -A, -u,
// or one or more relative paths. Flag and path arguments never mix, and every
// other Git pathspec form is rejected instead of being forwarded.
func supportedAdd(arguments []string) bool {
	if len(arguments) == 0 {
		return false
	}
	if len(arguments) == 1 && (arguments[0] == "-A" || arguments[0] == "-u") {
		return true
	}
	return !slices.ContainsFunc(arguments, func(argument string) bool { return strings.HasPrefix(argument, "-") })
}

func supportedUnstage(arguments []string) bool {
	if len(arguments) == 0 {
		return false
	}
	if len(arguments) == 1 && arguments[0] == "-A" {
		return true
	}
	return !slices.ContainsFunc(arguments, func(argument string) bool { return strings.HasPrefix(argument, "-") })
}

// parseRestore accepts the restore input: one or more relative paths, each
// preceded by any of --staged, --worktree and --source HEAD, given at most
// once and in any order. It resolves the native defaults, so the returned
// target states the destinations and the source the restore really uses.
// Every other source, revisions, patch mode, --ours/--theirs, absolute paths,
// globs and every other Git pathspec are rejected instead of being forwarded.
func parseRestore(arguments []string) ([]string, stage.RestoreTarget, bool) {
	staged, worktree, head := false, false, false
	paths := arguments
	for len(paths) != 0 {
		switch {
		case paths[0] == "--staged" && !staged:
			staged, paths = true, paths[1:]
			continue
		case paths[0] == "--worktree" && !worktree:
			worktree, paths = true, paths[1:]
			continue
		case paths[0] == "--source=HEAD" && !head:
			head, paths = true, paths[1:]
			continue
		case paths[0] == "--source" && !head && len(paths) > 1 && paths[1] == "HEAD":
			head, paths = true, paths[2:]
			continue
		}
		break
	}
	if len(paths) == 0 {
		return nil, stage.RestoreTarget{}, false
	}
	if slices.ContainsFunc(paths, func(argument string) bool { return strings.HasPrefix(argument, "-") }) {
		return nil, stage.RestoreTarget{}, false
	}
	// Native defaults: without a destination flag the working tree is the
	// destination, and the source is HEAD once the index is one too.
	return paths, stage.RestoreTarget{Index: staged, Worktree: worktree || !staged, Head: head || staged}, true
}

// parseCommit accepts the commit input: exactly one message source, -m
// <message> or -F -, and any number of explicit relative paths before or
// behind it. A second message source, an editor message, a pathspec and every
// other Git commit flag are rejected instead of being forwarded.
func parseCommit(arguments []string) (message string, standardInput bool, paths []string, ok bool) {
	source := false
	for i := 0; i < len(arguments); i++ {
		argument := arguments[i]
		switch {
		case argument == "-m" && !source && i+1 < len(arguments):
			i++
			message, source = arguments[i], true
		case argument == "-F" && !source && i+1 < len(arguments) && arguments[i+1] == "-":
			i++
			standardInput, source = true, true
		case argument != "" && !strings.HasPrefix(argument, "-"):
			paths = append(paths, argument)
		default:
			return "", false, nil, false
		}
	}
	return message, standardInput, paths, source
}

// parseBackup accepts the backup input: "list", "git [--backup <id>] --
// <log|show|diff|status> [arguments...]" and "restore [--backup <id>]
// [--yes]". It returns the empty command for every other input, so a missing
// "--", an unlisted Git command and every Git option before it are rejected
// instead of being forwarded. The arguments behind the allowed Git command
// are passed on unparsed.
func parseBackup(arguments []string) (command, id string, rest []string, yes bool) {
	if len(arguments) == 0 {
		return "", "", nil, false
	}
	command, arguments = arguments[0], arguments[1:]
	if command == "list" && len(arguments) == 0 {
		return command, "", nil, false
	}
	if command != "git" && command != "restore" {
		return "", "", nil, false
	}
	if len(arguments) >= 2 && arguments[0] == "--backup" {
		if id = arguments[1]; id == "" || strings.HasPrefix(id, "-") {
			return "", "", nil, false
		}
		arguments = arguments[2:]
	}
	if command == "restore" {
		if len(arguments) == 0 {
			return command, id, nil, false
		}
		if len(arguments) == 1 && arguments[0] == "--yes" {
			return command, id, nil, true
		}
		return "", "", nil, false
	}
	if len(arguments) < 2 || arguments[0] != "--" || !slices.Contains(migrate.GitCommands, arguments[1]) {
		return "", "", nil, false
	}
	return command, id, arguments[1:], false
}

func parseShow(arguments []string) (repository, source, path string, ok bool) {
	if len(arguments) != 7 || arguments[0] != "show" || arguments[1] != "--repository" ||
		arguments[3] != "--source" || !slices.Contains(show.Sources, arguments[4]) || arguments[5] != "--" {
		return "", "", "", false
	}
	return arguments[2], arguments[4], arguments[6], true
}

// parseDiff accepts the diff input: no target, "all" or one repository name,
// each optionally with --staged. Paths, revisions and every other Git diff
// option are rejected instead of being forwarded.
func parseDiff(arguments []string) (target string, staged, ok bool) {
	return parseTargetFlag(arguments, "--staged")
}

// parseLog accepts the log input: no target, "all" or one repository name,
// each optionally with -n <count>. Revisions, paths, formats, graph output
// and every other Git log option are rejected instead of being forwarded.
func parseLog(arguments []string) (target string, count int, ok bool) {
	count = log.DefaultCount
	limited := false
	for i := 0; i < len(arguments); i++ {
		argument := arguments[i]
		switch {
		case argument == "-n" && !limited && i+1 < len(arguments):
			i++
			parsed, err := strconv.Atoi(arguments[i])
			if err != nil || parsed < 1 || parsed > log.MaxCount {
				return "", 0, false
			}
			count, limited = parsed, true
		case argument != "" && !strings.HasPrefix(argument, "-") && target == "":
			target = argument
		default:
			return "", 0, false
		}
	}
	return target, count, true
}

// parseBranch accepts the branch input: no argument to list every local
// branch, exactly one name to create that branch everywhere, or -d/--delete
// with exactly one name to delete it everywhere. Every other Git branch
// option, including -D, -m, -c, -f, --list, several names, a start point and
// a remote branch, is rejected instead of being forwarded.
func parseBranch(arguments []string) (name string, deleting, ok bool) {
	if len(arguments) != 0 && (arguments[0] == branch.DeleteShortFlag || arguments[0] == branch.DeleteFlag) {
		if name, ok = parseFetch(arguments[1:]); !ok || name == "" {
			return "", false, false
		}
		return name, true, true
	}
	name, ok = parseFetch(arguments)
	return name, false, ok
}

// parseSwitch accepts the switch input: exactly one branch name, optionally
// with -c or --create and with --accept-config-change and --accept-new-paths.
// Each accept flag approves exactly one reviewed change and nothing else.
// -C, an explicit start point, --detach, --force, --merge, a pathspec and
// repository-selective switching are rejected instead of being forwarded.
func parseSwitch(arguments []string) (name string, create, accepted, assigned, ok bool) {
	name, enabled, ok := parseTargetFlags(arguments, switching.CreateShortFlag, switching.CreateFlag,
		switching.AcceptFlag, switching.AcceptPathsFlag)
	if !ok || name == "" {
		return "", false, false, false, false
	}
	return name, enabled[switching.CreateShortFlag] || enabled[switching.CreateFlag],
		enabled[switching.AcceptFlag], enabled[switching.AcceptPathsFlag], true
}

// parseFetch accepts the fetch and pull input: no target, "all" or one
// repository name. Remotes, refspecs, pruning, rebase and every other Git
// fetch or pull option are rejected instead of being forwarded.
func parseFetch(arguments []string) (target string, ok bool) {
	if len(arguments) == 0 {
		return "", true
	}
	if len(arguments) == 1 && arguments[0] != "" && !strings.HasPrefix(arguments[0], "-") {
		return arguments[0], true
	}
	return "", false
}

// parsePull accepts the pull input like parseFetch, each form optionally with
// --accept-config-change and --accept-new-paths. Each flag approves exactly
// one reviewed change and nothing else; every other flag is rejected.
func parsePull(arguments []string) (target string, accepted, assigned, ok bool) {
	var enabled map[string]bool
	target, enabled, ok = parseTargetFlags(arguments, pull.AcceptFlag, pull.AcceptPathsFlag)
	return target, enabled[pull.AcceptFlag], enabled[pull.AcceptPathsFlag], ok
}

// parseReconfigure accepts the reconfigure input: no argument at all, or one
// configuration source - --pull or --switch <branch> - optionally with
// --accept-repository-change, --accept-config-change, --accept-new-paths and
// any number of --rename <old>=<new>, each declaring that one disappeared and
// one appeared name are the same repository. Each accept flag approves
// exactly one reviewed class and nothing else.
//
// A repository target is rejected instead of reconfiguring half a project,
// which has no meaning: a configured remote is validated against the whole
// configuration. Two configuration sources are rejected as well; a
// reconfiguration reads .gitone.yml from exactly one place.
func parseReconfigure(arguments []string) (source reconfigure.Source, accepted reconfigure.Accepted,
	renames []reconfigure.Rename, ok bool) {
	refuse := func() (reconfigure.Source, reconfigure.Accepted, []reconfigure.Rename, bool) {
		return reconfigure.Source{}, reconfigure.Accepted{}, nil, false
	}
	for index := 0; index < len(arguments); index++ {
		switch argument := arguments[index]; {
		case argument == reconfigure.PullFlag && !source.Pull && source.Branch == "":
			source.Pull = true
		case argument == reconfigure.SwitchFlag && !source.Pull && source.Branch == "" && index+1 < len(arguments):
			index++
			if source.Branch = arguments[index]; source.Branch == "" || strings.HasPrefix(source.Branch, "-") {
				return refuse()
			}
		case argument == reconfigure.AcceptFlag && !accepted.Repository:
			accepted.Repository = true
		case argument == policy.AcceptFlag && !accepted.Config:
			accepted.Config = true
		case argument == assign.Flag && !accepted.Paths:
			accepted.Paths = true
		case argument == reconfigure.RenameFlag && index+1 < len(arguments):
			index++
			declared, valid := reconfigure.ParseRename(arguments[index])
			if !valid {
				return refuse()
			}
			renames = append(renames, declared)
		default:
			return refuse()
		}
	}
	return source, accepted, renames, true
}

// parsePush accepts the push input: no target, "all" or one repository name,
// each optionally with --yes. Every other flag, including a Git remote,
// refspec or force option, is rejected instead of being forwarded.
func parsePush(arguments []string) (target string, yes, ok bool) {
	return parseTargetFlag(arguments, "--yes")
}

func parseTargetFlag(arguments []string, flag string) (target string, enabled, ok bool) {
	target, given, ok := parseTargetFlags(arguments, flag)
	return target, given[flag], ok
}

// parseTargetFlags accepts an optional target followed by any subset of the
// given flags, each at most once and in any order.
func parseTargetFlags(arguments []string, flags ...string) (target string, enabled map[string]bool, ok bool) {
	enabled = map[string]bool{}
	for _, argument := range arguments {
		switch {
		case slices.Contains(flags, argument) && !enabled[argument]:
			enabled[argument] = true
		case argument != "" && !strings.HasPrefix(argument, "-") && target == "":
			target = argument
		default:
			return "", nil, false
		}
	}
	return target, enabled, true
}

// interactive reports whether a question can be asked, which requires a
// terminal on standard input.
func interactive(stdin io.Reader) bool {
	return ui.Terminal(stdin)
}

// printStatus reports the whole project. Ordinary changes exit with 0; every
// detected issue is reported before exiting non-zero.
func printStatus(command string, configuration *config.Config, root string, stdout, stderr io.Writer) int {
	result, err := status.Collect(configuration, root)
	if err != nil {
		printError(stderr, err)
		return 1
	}
	switch command {
	case "status --porcelain":
		result.WritePorcelain(stdout)
	case "status --json":
		if err := result.WriteJSON(stdout); err != nil {
			printError(stderr, err)
			return 1
		}
	default:
		result.WriteHuman(stdout)
	}
	if len(result.Issues) != 0 {
		return 1
	}
	return 0
}

func printInitialization(output io.Writer, configuration *config.Config) {
	fmt.Fprintln(output, "GitOne initialized")
	fmt.Fprintln(output)
	ui.Repositories(output, configuration)
	fmt.Fprintf(output, "\nNothing was staged or committed.\n")
}

func printValidation(output io.Writer, configuration *config.Config) {
	style := ui.For(output)
	fmt.Fprintln(output, "GitOne configuration")
	fmt.Fprintln(output)
	for _, line := range []string{
		"✓ YAML valid",
		fmt.Sprintf("✓ %d repositories", len(configuration.Repositories)),
		"✓ ownership and protected path patterns valid",
		"✓ no duplicate ownership patterns",
		"✓ push policies valid",
		"✓ repository metadata valid",
	} {
		fmt.Fprintln(output, style.Success.Render(line))
	}
	fmt.Fprintln(output, "\nConfiguration is valid.")
}

func printError(output io.Writer, value any) {
	fmt.Fprintln(output, ui.RenderLines(ui.For(output).Error, fmt.Sprint(value)))
}

// printRepositories lists the configured repositories and, below them, the
// retired ones: .gitone/retired/ is never cleaned up automatically, so this
// is where a person discovers that it exists at all.
func printRepositories(output io.Writer, configuration *config.Config, root string) error {
	fmt.Fprintln(output, "Repositories")
	fmt.Fprintln(output)
	writer := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "NAME\tVISIBILITY\tBRANCH\tPUSH\tREMOTE")
	for _, name := range configuration.RepositoryNames() {
		repository := configuration.Repositories[name]
		remotes := "-"
		if remoteNames := repository.RemoteNames(); len(remoteNames) != 0 {
			remotes = strings.Join(remoteNames, ",")
		}
		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n", name, repository.Visibility, configuration.DefaultBranch, repository.Push, remotes)
	}
	_ = writer.Flush()
	return printRetired(output, root)
}

// printRetired lists the metadata gitone reconfigure moved out of the
// configuration. GitOne never deletes one, so removing a directory listed
// here stays a deliberate human action.
func printRetired(output io.Writer, root string) error {
	retired, err := repository.Retired(root)
	if err != nil || len(retired) == 0 {
		return err
	}
	fmt.Fprintln(output, "\nRetired repositories")
	fmt.Fprintln(output)
	writer := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "NAME\tDIRECTORY")
	for _, directory := range retired {
		stamp, name, _ := strings.Cut(path.Base(directory), "-")
		fmt.Fprintf(writer, "%s\t%s\n", or(name, stamp), directory)
	}
	_ = writer.Flush()
	fmt.Fprintln(output, "\nRead one with: git --git-dir=<directory> log")
	fmt.Fprintln(output, "GitOne never removes a retired repository; deleting one is up to you.")
	return nil
}

// or names a retired directory that does not carry the <stamp>-<name> form
// GitOne writes by the only part it has.
func or(name, fallback string) string {
	if name == "" {
		return fallback
	}
	return name
}
