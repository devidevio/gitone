package cli

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/devidevio/gitone/internal/guidance"
)

// topic is one help page: what the command does, how it is called, the
// arguments and options it accepts and, where the syntax needs it, extra
// rules and one example.
type topic struct {
	purpose  string
	usage    string
	sections []section
	notes    []string
}

// section is a titled list of label and description lines, such as the
// options of a command.
type section struct {
	title string
	items [][2]string
}

// helpCommands is every command in the order the global help lists them.
var helpCommands = []string{
	"setup", "clone", "init", "migrate", "backup", "backup list", "backup git", "backup restore",
	"repo", "repo validate", "repo list", "status",
	"add", "unstage", "restore", "commit", "show", "diff", "log", "branch", "switch", "fetch", "pull", "push", "reconfigure", "recover", "abort", "doctor",
	"agents", "agents update",
	"vscode", "vscode info", "vscode install",
}

// helpGroups are the subcommands each command group lists on its own help
// page. A group has no syntax of its own beyond the commands below it.
var helpGroups = map[string][]string{
	"backup": {"backup list", "backup git", "backup restore"},
	"repo":   {"repo validate", "repo list"},
	"vscode": {"vscode info", "vscode install"},
}

// helpTopics documents exactly the input the CLI accepts, keyed by the
// command it documents. The command groups and the global help reference
// these pages instead of repeating them.
var helpTopics = map[string]topic{
	"repo": {
		purpose: "Inspect the merged configuration.",
		usage:   "gitone repo <command>",
	},
	"vscode": {
		purpose: "Integrate GitOne with Visual Studio Code.",
		usage:   "gitone vscode <command>",
	},
	"backup": {
		purpose: "Inspect and restore the retained migration backups.",
		usage:   "gitone backup <command>",
	},
	"backup list": {
		purpose: "List the retained migration backups.",
		usage:   "gitone backup list",
		notes: []string{
			"Every direct backup directory below .gitone/migration-backup/ is listed newest first with its full ID, its UTC time and whether it is a valid Git repository. A project without any backup prints one line and exits 0.",
		},
	},
	"backup git": {
		purpose: "Read a migration backup with native Git.",
		usage:   "gitone backup git [--backup <id>] -- <log|show|diff|status> [arguments...]",
		sections: []section{
			{title: "Options", items: [][2]string{
				{"--backup <id>", "The full backup ID. Required as soon as the project has more than one backup."},
			}},
			{title: "Arguments", items: [][2]string{
				{"<log|show|diff|status>", "The only accepted Git commands. Everything behind it is passed to Git unchanged."},
			}},
		},
		notes: []string{
			"The separating -- is required. Every other Git command, and every Git option before the command, is rejected with CLI001 before Git runs.",
			"The backup is the Git directory and the project root is the working tree, so status and diff compare the retained repository with your current files. Optional locks are disabled, so no inspection writes into the backup.",
			"Git output is streamed unchanged and the exit code of Git is returned.",
			"Example:\n  gitone backup git --backup 20260826T133234Z-223441957 -- log --all",
		},
	},
	"backup restore": {
		purpose: "Copy a migration backup back to the root .git/.",
		usage:   "gitone backup restore [--backup <id>] [--yes]",
		sections: []section{{title: "Options", items: [][2]string{
			{"--backup <id>", "The full backup ID. Required as soon as the project has more than one backup."},
			{"--yes", "Skip the confirmation question. Required without a terminal."},
		}}},
		notes: []string{
			"Only the complete .git/ is restored. Project files, the ignore entries, the configuration and the managed repositories are never changed, and the backup itself is kept.",
			"An existing root .git, whether a directory, a file or a symbolic link, is refused. The backup is copied and validated first and only then published, so a failure leaves neither a partial root repository nor a temporary one.",
			"While a root .git/ exists the ordinary GitOne commands are unavailable; gitone backup list and gitone backup git still work.",
		},
	},
	"setup": {
		purpose: "Ask for the missing configuration, then initialize or migrate the project.",
		usage:   "gitone setup",
		notes: []string{
			"setup takes no arguments, prints the generated configuration and asks once before it changes the project. After success it may show how to install the VS Code extension from a release VSIX. It needs a terminal on standard input and fails with SETUP001 without one.",
		},
	},
	"clone": {
		purpose: "Create a project from a repository that commits .gitone.yml.",
		usage:   "gitone clone <url> [directory]",
		sections: []section{
			{title: "Arguments", items: [][2]string{
				{"<url>", "The bootstrap repository, cloned through native Git."},
				{"<directory>", "The destination. Defaults to the repository name of the URL."},
			}},
		},
		notes: []string{
			"This command does not require a GitOne project. The destination must not exist; the project is built in a temporary directory next to it and only published once it validates. A failure leaves no destination and never changes an existing path.",
			"The committed .gitone.yml must be owned by exactly one repository, and the cloned URL must be that repository's configured origin. That repository is adopted with its complete native history.",
			"Every other repository of the committed configuration is initialized. One with an origin is fetched and checked out on its default branch; one without stays unborn and is reported with its next steps.",
			"Repositories that exist only in .gitone.local.yml are not part of a clone and cannot be discovered from one. Add them locally afterwards.",
			"--branch, --depth, partial clone, sparse checkout and submodules are not accepted.",
			"Example:\n  gitone clone git@github.com:example/website.git",
		},
	},
	"init": {
		purpose: "Create the managed repositories and the ignore entries.",
		usage:   "gitone init",
		notes: []string{
			`init needs a valid configuration and no root .git/. Convert an existing repository with "gitone migrate".`,
		},
	},
	"migrate": {
		purpose: "Convert a root .git/ into managed repositories.",
		usage:   "gitone migrate [--yes]",
		sections: []section{{title: "Options", items: [][2]string{
			{"--yes", "Skip the confirmation question. Required without a terminal."},
		}}},
		notes: []string{
			"The complete old .git/ is copied to .gitone/migration-backup/ before anything changes, and the backup is kept.",
		},
	},
	"repo validate": {
		purpose: "Check the merged configuration.",
		usage:   "gitone repo validate",
	},
	"repo list": {
		purpose: "List the repositories, visibility, branch, push policy and remotes.",
		usage:   "gitone repo list",
	},
	"status": {
		purpose: "Report the whole project, also when called in a subdirectory.",
		usage:   "gitone status [--porcelain | --json]",
		sections: []section{{title: "Options", items: [][2]string{
			{"--porcelain", "Print the same state as tab-separated records."},
			{"--json", "Print the same state as a versioned JSON document."},
		}}},
		notes: []string{
			"Ordinary changes exit 0. Detected issues are listed and exit 1.",
		},
	},
	"add": {
		purpose: "Stage changes in every affected repository.",
		usage:   "gitone add [-A | -u | <path>...]",
		sections: []section{
			{title: "Arguments", items: [][2]string{
				{"<path>...", `One or more files or directories relative to the current directory. A directory and "." stage their subtree.`},
			}},
			{title: "Options", items: [][2]string{
				{"-A", "Stage every change in the project."},
				{"-u", "Stage every changed or deleted tracked file."},
			}},
		},
		notes: []string{
			"Flags and paths never mix. Absolute paths, literal globs and every other Git pathspec are rejected.",
			"Example:\n  gitone add README.md src/site.css",
		},
	},
	"unstage": {
		purpose: "Restore staged paths from HEAD without changing the working tree.",
		usage:   "gitone unstage [-A | <path>...]",
		sections: []section{
			{title: "Arguments", items: [][2]string{
				{"<path>...", `One or more files or directories relative to the current directory. A directory and "." unstage their subtree.`},
			}},
			{title: "Options", items: [][2]string{
				{"-A", "Unstage every staged change in the project."},
			}},
		},
		notes: []string{
			"Flags and paths never mix. The working tree is never changed.",
		},
	},
	"restore": {
		purpose: "Discard changes to selected files, in the working tree, the index or both.",
		usage:   "gitone restore [--source HEAD] [--staged] [--worktree] <path>...",
		sections: []section{
			{title: "Arguments", items: [][2]string{
				{"<path>...", `One or more files or directories relative to the current directory. A directory and "." restore their subtree.`},
			}},
			{title: "Options", items: [][2]string{
				{"--source HEAD", "Read the version each owning repository has in its own HEAD instead of in its index. --source=HEAD is the same flag."},
				{"--staged", `Restore the index. Alone this is "gitone unstage".`},
				{"--worktree", "Restore the working-tree files. This is what happens without a destination flag."},
			}},
		},
		notes: []string{
			"The flags come before the paths, each at most once and in any order. Without a destination flag the working tree is the destination, and the source is HEAD as soon as --staged is given, otherwise the index. So \"--staged --worktree\" makes both match HEAD, and \"--source HEAD\" alone makes only the working tree match it.",
			"Every restored path is overwritten with the version its owning repository holds. This discards the changes in those files and cannot be undone once the command finished. A selected path the source does not hold, such as a newly staged file, is removed from the destination.",
			"HEAD and the index are the only sources. Other revisions, patch mode, --ours, --theirs, absolute paths, literal globs and every other Git pathspec are not accepted. Untracked files are never restored, and no directory is ever deleted.",
			"Every requested path, the projected configuration and the complete working tree are validated before the first index or file changes. All source versions are checked out beside the working tree first, file modes included, and are swapped in together with the prepared indexes only afterwards. A restored .gitone.yml that would add, remove or rename a repository or change a remote is refused and names \"gitone reconfigure\".",
			"A failure after the first replacement restores what this restore already changed, or leaves recovery state for \"gitone recover\" and \"gitone abort\". An index or file that changed outside GitOne is never overwritten.",
			"Example:\n  gitone restore src/site.css\n  gitone restore --source=HEAD --staged --worktree src/site.css",
		},
	},
	"commit": {
		purpose: "Commit the staged changes of every repository, or only selected files.",
		usage:   "gitone commit <-m <message> | -F -> [<path>...]",
		sections: []section{
			{title: "Options", items: [][2]string{
				{"-m <message>", "The commit message. Required, and accepted only once."},
				{"-F -", "Read the complete commit message from standard input."},
			}},
			{title: "Arguments", items: [][2]string{
				{"<path>...", "The files to commit, before or behind the message. Without any, every staged change is committed."},
			}},
		},
		notes: []string{
			"With paths this is a native path-limited commit: the current working-tree version of exactly those files is committed, even when a different version was staged. Every unselected staged change stays staged, and a repository without a selected change gets no commit.",
			"Each path is resolved relative to the current directory and must name one managed file Git already knows, so a new file has to be staged once before it can be selected. Directories, \".\", literal globs, Git pathspecs and absolute paths are not accepted.",
			"A commit spanning two or more repositories gives every created commit one shared GitOne-Group trailer. commit never pushes.",
			"Example:\n  gitone commit src/site.css notes/plan.md -m \"Adjust the layout\"",
		},
	},
	"show": {
		purpose: "Print one owned path exactly as stored in HEAD or the index.",
		usage:   "gitone show --repository <name> --source <head|index> -- <path>",
		sections: []section{
			{title: "Options", items: [][2]string{
				{"--repository <name>", "Read from this managed repository."},
				{"--source <head|index>", "Read the committed or staged content."},
			}},
			{title: "Arguments", items: [][2]string{
				{"<path>", "One owned path relative to the current directory."},
			}},
		},
		notes: []string{
			"Raw file bytes are written to standard output. A path absent from the selected source fails with SHOW001 and exit code 2.",
		},
	},
	"diff": {
		purpose: "Show the changes of every repository as one native Git patch view.",
		usage:   "gitone diff [all | <repository>] [--staged]",
		sections: []section{
			{title: "Arguments", items: [][2]string{
				{"all", "Diff every repository. Same as no argument."},
				{"<repository>", "Diff only that repository."},
			}},
			{title: "Options", items: [][2]string{
				{"--staged", "Compare the index against HEAD instead of the working tree."},
			}},
		},
		notes: []string{
			"Repositories without matching changes are omitted, and no change at all prints nothing. Untracked files are not part of a diff; use \"gitone status\".",
			"Paths, revisions and every other Git diff option are not accepted.",
			"Example:\n  gitone diff website --staged",
		},
	},
	"log": {
		purpose: "Show the recent commits of every repository.",
		usage:   "gitone log [all | <repository>] [-n <count>]",
		sections: []section{
			{title: "Arguments", items: [][2]string{
				{"all", "Log every repository. Same as no argument."},
				{"<repository>", "Log only that repository."},
			}},
			{title: "Options", items: [][2]string{
				{"-n <count>", "Show at most count commits per repository. 1 to 1000, 20 by default."},
			}},
		},
		notes: []string{
			"Every repository gets its own section in configuration order, with the abbreviated commit ID, the author date, the ref decorations and the subject exactly as native Git prints them. A repository without commits is labeled instead of failing the command.",
			"Each repository is logged on its own; GitOne-Group trailers are not resolved into one shared history.",
			"Revisions, paths, --graph, custom formats, patches and paging are not accepted.",
			"Example:\n  gitone log website -n 5",
		},
	},
	"branch": {
		purpose: "List the local branches, or create or delete one in every repository.",
		usage:   "gitone branch [<name> | -d <branch>]",
		sections: []section{
			{title: "Arguments", items: [][2]string{
				{"<name>", "Create this branch in every repository at its current HEAD."},
			}},
			{title: "Options", items: [][2]string{
				{"-d, --delete", "Delete this merged branch from every repository."},
			}},
		},
		notes: []string{
			"Without an argument the branches of all repositories are listed in one table, followed by the current branch of every repository. A branch only part of the project has, a repository not on a branch, and repositories that disagree about the current branch are stated below the list.",
			"Creation needs every repository to have a commit, to be on the same branch, and not to have the requested branch yet. Names are checked by native Git's own branch-name rules. Any refusal leaves every repository unchanged.",
			"A created branch is only a new ref: no repository switches branches, and no HEAD, index or working-tree file is touched. Use \"gitone switch\" to move to it.",
			"Deletion needs every repository to have the branch, to be on another branch, and to consider it merged by native Git's own rule for \"git branch -d\": contained in its configured upstream, or in HEAD where none is configured. Every repository is decided on its own, and every blocking one is reported with the ref it was compared with.",
			"Only local refs are read: GitOne never fetches to decide this, so a configured upstream this repository has not fetched is refused instead of compared with something else, and no remote branch is ever deleted. The branch settings go with the branch, like native Git.",
			"A failure after the first ref was written or deleted returns the project to its starting state, or leaves recovery state for \"gitone recover\" and \"gitone abort\". A branch that changed or was recreated outside GitOne is never overwritten or deleted.",
			"Force deletion (-D), renaming, copying, force-resetting, upstreams, start points, several names per call, remote branches and repository-selective creation or deletion are not accepted.",
			"Example:\n  gitone branch feature/auth\n  gitone branch -d feature/auth",
		},
	},
	"switch": {
		purpose: "Move every repository to the same branch, creating it where it is missing.",
		usage:   "gitone switch [-c | --create] <branch> [--accept-config-change] [--accept-new-paths]",
		sections: []section{
			{title: "Arguments", items: [][2]string{
				{"<branch>", "The branch every repository switches to."},
			}},
			{title: "Options", items: [][2]string{
				{"-c, --create", "Create <branch> in every repository at its current HEAD first."},
				{"--accept-config-change", "Accept the previewed target policy change without asking."},
				{"--accept-new-paths", "Accept the previewed ownership for new target paths without asking."},
			}},
		},
		notes: []string{
			"Without -c, a repository missing the branch locally gets it from its own fetched \"origin/<branch>\" and tracks it; nothing is fetched implicitly and no start point is guessed from another repository or from HEAD. -c refuses when a repository already has the name or has no commit yet.",
			"Every repository must be clean. Local work is never discarded or stashed, and native Git still refuses to overwrite untracked files.",
			"Target configuration and ownership are validated before the first ref is written. Each --accept-* option skips only its matching confirmation.",
			"A failure restores the starting state or leaves recovery state for \"gitone recover\" and \"gitone abort\". Only branches and upstreams this switch created are undone.",
			"Documentation: " + guidance.SwitchingBranches,
		},
	},
	"fetch": {
		purpose: "Update the origin remote-tracking refs of every repository.",
		usage:   "gitone fetch [all | <repository>]",
		sections: []section{
			{title: "Arguments", items: [][2]string{
				{"all", "Fetch every repository. Same as no argument."},
				{"<repository>", "Fetch only that repository."},
			}},
		},
		notes: []string{
			"Branches, HEADs, indexes and working-tree files are never changed; only refs below refs/remotes/origin/ are updated. Git still stores fetched objects and updates FETCH_HEAD. Ordinary staged and unstaged changes are allowed.",
			"A repository without a configured origin is skipped by an all-repository fetch and rejected as a target.",
			"Repositories are fetched one after another and refs already updated are never rolled back, so a failure reports which repositories were updated before it.",
			"Remotes, refspecs, --prune, --force, tags and shallow options are not accepted.",
			"Example:\n  gitone fetch website",
		},
	},
	"pull": {
		purpose: "Fast-forward every repository to its origin branch.",
		usage:   "gitone pull [all | <repository>] [--accept-config-change] [--accept-new-paths]",
		sections: []section{
			{title: "Arguments", items: [][2]string{
				{"all", "Pull every repository. Same as no argument."},
				{"<repository>", "Pull only that repository."},
			}},
			{title: "Options", items: [][2]string{
				{"--accept-config-change", "Accept the previewed incoming policy change without asking."},
				{"--accept-new-paths", "Accept the previewed ownership for new incoming paths without asking."},
			}},
		},
		notes: []string{
			"Every pull fetches first and then only fast-forwards. It never merges, rebases, stashes or discards local work.",
			"Incoming configuration and ownership are validated before any branch moves. Each --accept-* option skips only its matching confirmation.",
			"A repository without a configured origin is skipped by an all-repository pull and rejected as a target.",
			"A failure restores branches already moved or leaves recovery state. Fetched remote-tracking refs stay updated.",
			"Documentation: " + guidance.Pulling,
		},
	},
	"reconfigure": {
		purpose: "Make the managed repositories match the configuration.",
		usage:   "gitone reconfigure [--pull | --switch <branch>] [--accept-repository-change] [--accept-config-change] [--accept-new-paths] [--rename <old>=<new>]...",
		sections: []section{
			{title: "Options", items: [][2]string{
				{"--pull", "Take the configuration from the incoming commits of origin and fast-forward every repository."},
				{"--switch <branch>", "Take the configuration from <branch> and move every repository to it."},
				{"--accept-repository-change", "Accept the previewed repository reconfiguration without asking."},
				{"--accept-config-change", "Accept the ordinary incoming policy change of the same source without asking."},
				{"--accept-new-paths", "Accept the ownership generated for the new paths the source carries."},
				{"--rename <old>=<new>", "Declare that the dropped name <old> and the added name <new> are the same repository. Repeatable."},
			}},
		},
		notes: []string{
			"Without --pull or --switch, the working-tree configuration is the source. --pull uses the incoming configuration; --switch uses the named branch.",
			"The complete plan is previewed before it is applied. Each --accept-* option skips only its matching confirmation.",
			"Renames are never inferred. Declare each one explicitly with the repeatable --rename option.",
			"Documentation: " + guidance.ReconfiguringRepositories,
			"Examples:\n  gitone reconfigure --rename notes=journal\n  gitone reconfigure --pull",
		},
	},
	"push": {
		purpose: "Push every repository that has outgoing commits.",
		usage:   "gitone push [all | <repository>] [--yes]",
		sections: []section{
			{title: "Arguments", items: [][2]string{
				{"all", "Push every repository. Same as no argument."},
				{"<repository>", "Push only that repository."},
			}},
			{title: "Options", items: [][2]string{
				{"--yes", "Skip the confirmation. Required without a terminal."},
			}},
		},
		notes: []string{
			"Repositories configured with push: disabled are skipped by an all-repository push and refused as an explicit target. A configured clean-worktree rule refuses staged, unstaged and untracked changes before contacting a remote.",
			"Remotes, refspecs, tags and force pushes are not accepted.",
			"Example:\n  gitone push website --yes",
		},
	},
	"recover": {
		purpose: "Finish an interrupted add, unstage, restore, commit, branch, switch, pull or reconfigure; report an interrupted push.",
		usage:   "gitone recover",
	},
	"abort": {
		purpose: "Undo an interrupted add, unstage, restore, commit, branch, switch, pull or reconfigure; report an interrupted push.",
		usage:   "gitone abort",
	},
	"doctor": {
		purpose: "Report project health without changing anything.",
		usage:   "gitone doctor",
	},
	"agents": {
		purpose: "Check the GitOne instructions in the project AGENTS.md.",
		usage:   "gitone agents",
		notes: []string{
			"This command needs a valid configuration but no initialized repository. It takes no arguments and no flags, changes nothing and has no JSON mode.",
			"It exits 0 only when exactly one repository owns AGENTS.md and the file holds the current GitOne instruction block, either between the gitone:agents markers or as the exact text without them.",
			"Ownership failures are reported as PATH001, PATH002 or PATH003. A missing, outdated or unrecognizable file, and one that is not a regular file, is AGENT001.",
		},
	},
	"agents update": {
		purpose: "Write the current GitOne instructions into the project AGENTS.md.",
		usage:   "gitone agents update",
		notes: []string{
			"The command itself is the authorization: there is no confirmation, no --yes and no terminal required. It takes no arguments and no flags.",
			"Only <project-root>/AGENTS.md is managed. A missing file is created, an ordinary one gets the block appended, an exact unmarked block is wrapped in the markers, and a marked block is replaced. An already current file is left unchanged.",
			"Everything outside the marked block is preserved, an existing file keeps its permissions, and the result is published atomically. A symbolic link, another non-regular file, unmarked instructions that are not the current block, duplicate blocks and incomplete markers are refused with AGENT001 instead of being overwritten.",
			"It never edits the configuration, stages, commits, initializes, migrates or contacts a remote. Assign AGENTS.md in .gitone.yml or .gitone.local.yml yourself.",
		},
	},
	"vscode info": {
		purpose: "Print the VS Code protocol and GitOne version as JSON.",
		usage:   "gitone vscode info --json",
		notes: []string{
			"This command does not require a GitOne project.",
		},
	},
	"vscode install": {
		purpose: "Install the GitOne extension through the VS Code CLI.",
		usage:   "gitone vscode install [--vsix <path>] [--force]",
		sections: []section{{title: "Options", items: [][2]string{
			{"--vsix <path>", "Install this local VSIX instead of the Marketplace extension."},
			{"--force", "Force installation or replacement. Never implied by setup."},
		}}},
		notes: []string{
			"This command does not require a GitOne project. It uses the code command from PATH.",
		},
	},
}

// helpPage returns the help page the arguments ask for: no argument at all,
// a bare command group, or the exact "--help" form of a supported command, so
// "gitone commit -m --help" stays an ordinary commit and every other
// help-like input falls through to the CLI001 rejection.
func helpPage(args []string) (topic, bool) {
	command := strings.Join(args, " ")
	// Neither "gitone" itself nor a command group has an action of its own,
	// so calling one without a subcommand answers with its help instead of a
	// rejection.
	_, group := helpGroups[command]
	if len(args) != 0 && !group {
		if args[len(args)-1] != "--help" {
			return topic{}, false
		}
		command = strings.Join(args[:len(args)-1], " ")
	}
	if command == "" {
		return globalHelp(), true
	}
	page, ok := helpTopics[command]
	if commands, isGroup := helpGroups[command]; isGroup {
		page.sections = []section{{title: "Commands", items: commandItems(commands)}}
	}
	return page, ok
}

// helpHint names the page that answers a rejected input: the help of the
// command it names, otherwise the global help. It reads the same registry the
// help pages come from, so there is no second command catalog.
func helpHint(args []string) string {
	// A group subcommand is the more useful page, so the two-word form is
	// tried before the command itself.
	for _, words := range []int{2, 1} {
		if len(args) < words {
			continue
		}
		if _, ok := helpTopics[strings.Join(args[:words], " ")]; ok {
			return fmt.Sprintf("Run %q for the arguments it accepts.", "gitone "+strings.Join(args[:words], " ")+" --help")
		}
	}
	return fmt.Sprintf("Run %q for the supported commands.", "gitone --help")
}

func globalHelp() topic {
	return topic{
		purpose: "GitOne manages several Git repositories in one working directory, with ownership decided by path.",
		usage:   "gitone <command> [arguments]",
		sections: []section{
			{title: "Commands", items: commandItems(helpCommands)},
			{title: "Options", items: [][2]string{
				{"--help", "Print this help, or the help of a command."},
				{"--version", "Print the version of the binary."},
			}},
		},
		notes: []string{
			`Run "gitone <command> --help" for the arguments a command accepts.`,
			`Commands that work on repositories need an initialized project. Run "gitone setup" without a configuration, "gitone init" once the project is configured, or "gitone migrate" when a real root .git/ directory exists. "gitone doctor" explains unusable managed metadata and unsupported root .git forms.`,
		},
	}
}

// commandItems lists the named commands with their accepted syntax and their
// purpose, both taken from the command's own help page.
func commandItems(commands []string) [][2]string {
	items := make([][2]string, 0, len(commands))
	for _, command := range commands {
		page := helpTopics[command]
		items = append(items, [2]string{strings.TrimPrefix(page.usage, "gitone "), page.purpose})
	}
	return items
}

func writeHelp(output io.Writer, page topic) {
	fmt.Fprintf(output, "%s\n\nUsage:\n  %s\n", page.purpose, page.usage)
	for _, part := range page.sections {
		fmt.Fprintf(output, "\n%s:\n", part.title)
		writer := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
		for _, item := range part.items {
			fmt.Fprintf(writer, "  %s\t%s\n", item[0], item[1])
		}
		_ = writer.Flush()
	}
	for _, note := range page.notes {
		fmt.Fprintf(output, "\n%s\n", note)
	}
}
