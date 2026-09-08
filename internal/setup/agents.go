package setup

import (
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/devidevio/gitone/internal/agents"
	"github.com/devidevio/gitone/internal/config"
)

// agentsPlan is the optional AGENTS.md step of one setup run. The block is
// written only after the core result succeeded, so a failed instruction
// update can never take the finished project down with it.
type agentsPlan struct {
	// write updates AGENTS.md once the core action succeeded.
	write bool
	// create reports that AGENTS.md does not exist yet, so the mode has to
	// give the generated file an owner.
	create bool
	// later prints the steps that add the block after this run.
	later bool
}

// offerAgents asks the one optional AGENTS.md question of a setup run.
// canOwn reports whether this mode can still make sure the file ends up owned
// by exactly one repository; without that, adding the block would only create
// an unassigned path, so the steps are printed instead of asked.
func offerAgents(root string, canOwn bool, asked *prompt, output io.Writer) (*agentsPlan, bool) {
	state, err := agents.Inspect(root)
	return offerAgentsForState(state, err, canOwn, asked, output)
}

// offerTemplateAgents only offers a write when Template mode can safely add
// the new file to its placeholder repository. Existing files remain untouched
// until the user assigns them explicitly.
func offerTemplateAgents(root string, asked *prompt, output io.Writer) (*agentsPlan, bool) {
	state, err := agents.Inspect(root)
	return offerAgentsForState(state, err, state == agents.Missing, asked, output)
}

func offerAgentsForState(state agents.State, err error, canOwn bool, asked *prompt, output io.Writer) (*agentsPlan, bool) {
	if err != nil {
		// GitOne does not know what such a file means, so setup reports it
		// and never edits it.
		fmt.Fprintf(output, "\n%v\n", err)
		return &agentsPlan{}, true
	}
	if state == agents.Current || state == agents.Unmarked {
		return &agentsPlan{}, true
	}
	if !canOwn {
		return &agentsPlan{later: true}, true
	}
	fmt.Fprintln(output)
	if state == agents.Stale {
		answer, ok := asked.confirm("Update GitOne instructions?", true)
		return &agentsPlan{write: answer, later: !answer}, ok
	}
	answer, ok := asked.confirm("Add the GitOne instructions for AI agents to "+agents.File+"?", false)
	return &agentsPlan{write: answer, create: answer && state == agents.Missing, later: !answer}, ok
}

// ownAgents gives an AGENTS.md the wizard will create an owner. An entered
// pattern that already covers it is kept, one repository is used directly and
// several are an explicit question without a default. Unlike the project
// files, a local repository is a valid answer, so private instructions never
// have to be published to be owned.
func ownAgents(entries []*entry, asked *prompt, output io.Writer) bool {
	if len(claimants(entries, agents.File)) != 0 {
		return true
	}
	chosen := entries[0]
	if len(entries) > 1 {
		candidates := slices.Clone(entries)
		slices.SortFunc(candidates, func(a, b *entry) int { return strings.Compare(a.name, b.name) })
		names := make([]string, len(candidates))
		for index, candidate := range candidates {
			names[index] = candidate.name
		}
		fmt.Fprintf(output, "\n%s is created and needs an owner. A repository in %s keeps\ninstructions that must stay private off every public remote.\n",
			agents.File, config.LocalFile)
		answer, ok := asked.selectValue("Repository owning "+agents.File, "", "", names, oneOf(names))
		if !ok {
			return false
		}
		chosen = candidates[slices.Index(names, answer)]
	}
	chosen.derived = append(chosen.derived, agents.File)
	chosen.Paths = append(chosen.Paths, agents.File)
	slices.Sort(chosen.Paths)
	return true
}

// applyAgents performs the optional AGENTS.md step after the core setup
// result succeeded, or prints how to add the block later. owned reports
// whether the resulting configuration already gives the file exactly one
// owner; without one, the assignment is the missing prerequisite.
func applyAgents(plan *agentsPlan, owned bool, root string, output io.Writer) error {
	switch {
	case plan.write:
		if _, err := agents.Update(root); err != nil {
			return fmt.Errorf("%w\n\nGitOne project setup is complete; only the optional %s update failed.", err, agents.File)
		}
		fmt.Fprintf(output, "\n%s holds the current GitOne instructions.\n", agents.File)
	case plan.later:
		fmt.Fprintf(output, "\nThe GitOne instructions for AI agents are not in %s.\n", agents.File)
		if owned {
			fmt.Fprintln(output, "Add them later with:")
		} else {
			fmt.Fprintf(output, "Assign %s to exactly one repository in %s or\n%s, then run:\n",
				agents.File, config.PublicFile, config.LocalFile)
		}
		fmt.Fprintf(output, "  gitone agents update\n%s\n", agents.Link)
	}
	return nil
}
