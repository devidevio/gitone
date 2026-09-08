package pull

import (
	"github.com/devidevio/gitone/internal/config"
	"github.com/devidevio/gitone/internal/policy"
	"github.com/devidevio/gitone/internal/worktree"
)

// AcceptFlag is the explicit approval of a reviewed incoming policy change.
const AcceptFlag = policy.AcceptFlag

// reviewed describes the pull to the shared policy review, which is what
// gitone switch reuses with its own names.
func reviewed(participants []*participant) policy.Command {
	byName := map[string]*participant{}
	for _, current := range participants {
		byName[current.Name] = current
	}
	return policy.Command{
		Code:   pullFailed,
		Retry:  Command,
		Repair: "Repair or revert the remote file and run " + Command + " again.",
		// A structural incoming change is not applied here: it is reviewed
		// and applied by the one command that also reconciles the
		// repositories with it.
		Reconfigure: ReconfigureCommand,
		Unmodified:  unmodified,
		Aborted:     "Pull aborted.",
		Failed: func(name, summary string) {
			if current := byName[name]; current != nil {
				current.state, current.detail = failed, summary
			}
		},
	}
}

// review builds the projected policy of a pull. Every repository that
// fast-forwards carries its incoming ref and commit as the source a refusal
// and the preview name.
func review(configuration *config.Config, root string, inventory *worktree.Result, participants []*participant) (*policy.Policy, error) {
	repositories := make([]policy.Repository, 0, len(participants))
	for _, current := range participants {
		entry := policy.Repository{Name: current.Name, Head: current.head, Target: current.target}
		if current.target != "" {
			entry.Source = config.OriginRemote + "/" + current.branch + " at " + short(current.target)
		}
		repositories = append(repositories, entry)
	}
	return policy.Review(reviewed(participants), configuration, root, inventory, repositories)
}
