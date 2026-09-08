package policy

import (
	"os"
	"strings"
	"testing"

	"github.com/devidevio/gitone/internal/config"
	"github.com/devidevio/gitone/internal/worktree"
)

func TestProjectedIgnoresUsesTheCommandFailureContext(t *testing.T) {
	for _, command := range []Command{
		{Code: "PULL001 pull failed.", Unmodified: "No branch or file was changed."},
		{Code: "SWITCH001 switch failed.", Unmodified: "No repository was switched."},
	} {
		t.Run(command.Code, func(t *testing.T) {
			overlay, err := command.projectedIgnores(t.TempDir(), &worktree.Result{}, map[string]map[string]string{
				"public": {config.ProjectIgnoreFile: "missing-object"},
			})
			defer os.RemoveAll(overlay)

			if err == nil {
				t.Fatal("missing blob was accepted")
			}
			for _, wanted := range []string{command.Code, command.Unmodified} {
				if !strings.Contains(err.Error(), wanted) {
					t.Fatalf("error = %q, want %q", err, wanted)
				}
			}
		})
	}
}
