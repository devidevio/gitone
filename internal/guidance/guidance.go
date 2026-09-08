// Package guidance names the public guides shared by CLI help and repair messages.
package guidance

const (
	Usage                     = "https://gitone.io/docs/usage"
	SwitchingBranches         = Usage + "#switching-branches"
	Pulling                   = Usage + "#pulling"
	ReconfiguringRepositories = Usage + "#reconfiguring-the-repositories"
	RepairingMissingBranches  = Usage + "#repairing-missing-branches"
	InterruptedBranchDeletion = Usage + "#resolving-an-interrupted-branch-deletion"
	ResolvingDivergedHistory  = Usage + "#resolving-diverged-history"
	AdoptingRemoteHistory     = Usage + "#adopting-history-into-an-empty-repository"

	UnbornRepair = "For new local work, add owned files and commit them with gitone add and gitone commit. " +
		"To adopt existing remote history, do not create an unrelated first commit. Follow " + AdoptingRemoteHistory + "."
)
