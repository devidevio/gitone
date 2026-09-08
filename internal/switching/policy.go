package switching

import (
	"github.com/devidevio/gitone/internal/assign"
	"github.com/devidevio/gitone/internal/config"
	"github.com/devidevio/gitone/internal/policy"
	"github.com/devidevio/gitone/internal/worktree"
)

const (
	// AcceptFlag is the explicit approval of a reviewed target policy change.
	AcceptFlag = policy.AcceptFlag

	// AcceptPathsFlag is the explicit approval of the ownership a switch
	// generates for the new paths its target branch carries.
	AcceptPathsFlag = assign.Flag
)

// reviewed describes the switch to the shared policy review and to the shared
// path assignment, which gitone pull reuses with its own names.
func reviewed(name string) policy.Command {
	return policy.Command{
		Code:   switchFailed,
		Retry:  Command + " " + name,
		Repair: "Repair or revert " + config.PublicFile + " on branch " + name + " and run " + Command + " " + name + " again.",
		// A structural target change is not applied here: it is reviewed and
		// applied by the one command that also reconciles the repositories
		// with it, on the same branch.
		Reconfigure: ReconfigureCommand + " " + name,
		Unmodified:  unmodified,
		Aborted:     "Switch aborted.",
	}
}

// review builds the projected policy of a switch. The target branch carries
// its own .gitone.yml and .gitignore files and is untrusted policy input
// exactly like an incoming pull, so every moving repository names the branch
// and commit its policy would come from. A repository already on the branch
// replaces nothing and is therefore no incoming change.
func review(configuration *config.Config, root, name string, inventory *worktree.Result, participants []*participant) (*policy.Policy, error) {
	repositories := make([]policy.Repository, 0, len(participants))
	for _, current := range participants {
		entry := policy.Repository{Name: current.name}
		if current.state != unchanged {
			entry.Head, entry.Target = current.head, current.target
			entry.Source = name + " at " + short(current.target)
		}
		repositories = append(repositories, entry)
	}
	return policy.Review(reviewed(name), configuration, root, inventory, repositories)
}
