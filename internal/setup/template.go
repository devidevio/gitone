package setup

import (
	"fmt"
	"io"
	"strings"

	"github.com/devidevio/gitone/internal/agents"
	"github.com/devidevio/gitone/internal/config"
)

// publicTemplate and localTemplate are the commented configuration files of
// the template mode. The committed one is deliberately invalid: its only
// repository is named <repository-name>, so an untouched template can never
// initialize a plausible example repository.
const publicTemplate = `# GitOne project configuration - committed, so everything here is public.
#
# Replace every TODO below, then check the result with:
#   gitone repo validate

version: 1

# Required: the branch gitone init creates in every repository.
default_branch: main

# Optional project rules. Remove the leading # to use them.
# rules:
#   # Paths that must never be owned, tracked or pushed.
#   # .gitone.yml and .gitignore can never be protected.
#   protected_paths:
#     - .env
#     - secrets/**
#   push:
#     # true refuses a push while the working tree has changes.
#     # Default: false
#     require_clean_worktree: true

repositories:
  # TODO: replace <repository-name> with the real name.
  # Names match [a-z][a-z0-9_-]*, and "all" is reserved.
  <repository-name>:
    # Required: public or private.
    visibility: public

    # Optional: allowed (default) or disabled, which skips this repository
    # during gitone push and refuses it as an explicit push target.
    # push: disabled

    # Optional: shorthand for remotes.origin.
    # remote: git@github.com:example/website.git

    # Optional: named remotes. Only origin is fetched, pulled and pushed.
    # remotes:
    #   origin: git@github.com:example/website.git
    #   backup: git@gitlab.com:example/website.git

    # Required: every file of the project belongs to exactly one repository.
    # These two project files have to be owned here.
    paths:
      - .gitignore
      - .gitone.yml
      # TODO: add the paths this repository owns.
      # - README.md        # one exact file
      # - src/**           # everything below src/, at any depth
      # - "*.md"           # every Markdown file in the project root
      # - docs/**/*.md     # every Markdown file below docs/
`

const localTemplate = `# GitOne local configuration - never committed. Setup keeps it out of Git
# through .gitignore, so private repository names, paths and remotes stay on
# this machine.
#
# It merges into .gitone.yml by repository name:
#   - a repository only this file names is added
#   - default_branch and visibility are replaced
#   - protected_paths extend the committed list, they never remove entries
#   - push rules can only be tightened: push: disabled and
#     require_clean_worktree: true take effect, the weaker values never do
#   - remotes merge per remote name
#   - paths replace the whole committed list of that repository
#
# The example below stays commented out on purpose: this file alone
# configures nothing, and an untouched template must not create a repository.

version: 1

# repositories:
#   notes:
#     visibility: private
#     push: disabled
#     remote: git@internal.example.com:ops/notes.git
#     paths:
#       - notes/**
`

// agentsTemplate owns a generated AGENTS.md in the placeholder repository.
// Template mode has no repository to choose from, so the comment names the
// one decision the committed file cannot make for the reader.
const agentsTemplate = `      # Setup wrote the GitOne instructions for AI agents into AGENTS.md.
      # Instructions that must stay private belong to a repository in
      # .gitone.local.yml instead, so move this path there if they do.
      - AGENTS.md
`

// template writes the two commented configuration files and stops. It never
// inventories paths, initializes or migrates anything, so the project stays
// exactly as it is until the templates have been edited.
func template(root string, asked *prompt, output io.Writer) error {
	fmt.Fprintf(output, "\nTemplate mode writes commented configuration files for you to edit.\n")
	plan, ok := offerTemplateAgents(root, asked, output)
	if !ok {
		return asked.cancelled(output)
	}
	fmt.Fprintln(output)
	fmt.Fprintf(output, "  %s is created\n", config.PublicFile)
	fmt.Fprintf(output, "  %s is created\n", config.LocalFile)
	fmt.Fprintf(output, "  %s is %s with the GitOne entries\n", config.ProjectIgnoreFile, ignoreAction(root))
	if plan.write {
		fmt.Fprintf(output, "  %s %s the GitOne instructions for AI agents\n", agents.File, agentsAction(plan))
	}
	fmt.Fprintf(output, "  no repository is initialized or migrated\n\n")

	confirmed, ok := asked.confirm("Continue?", false)
	if !ok || !confirmed {
		return asked.cancelled(output)
	}
	public := publicTemplate
	if plan.create {
		public = strings.Replace(public, "    paths:\n", "    paths:\n"+agentsTemplate, 1)
	}
	if err := publish(root, public, localTemplate); err != nil {
		return err
	}
	fmt.Fprintf(output, "\n%s and %s were created.\n\n", config.PublicFile, config.LocalFile)
	fmt.Fprintf(output, "Edit %s and optionally %s.\nThen run:\n  gitone repo validate\n  gitone setup\n",
		config.PublicFile, config.LocalFile)
	// The template owns AGENTS.md only when setup created the file itself;
	// an existing one still has to be assigned by hand.
	return applyAgents(plan, plan.create, root, output)
}

// agentsAction names what the optional instruction write does to AGENTS.md.
func agentsAction(plan *agentsPlan) string {
	if plan.create {
		return "is created with"
	}
	return "is updated with"
}
