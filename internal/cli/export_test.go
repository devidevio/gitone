package cli

// The source lists the external test package checks its own tables against,
// so no command can be documented or gated without a test behind it.
var (
	HelpCommands            = helpCommands
	HelpGroups              = helpGroups
	RepositoryStateCommands = repositoryStateCommands
)
