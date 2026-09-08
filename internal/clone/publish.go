package clone

import "github.com/devidevio/gitone/internal/atomicfs"

// publish atomically refuses to replace a destination created after the
// initial existence check.
func publish(project, destination string) error {
	return atomicfs.RenameNoReplace(project, destination)
}
