package switching

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"github.com/devidevio/gitone/internal/config"
	"github.com/devidevio/gitone/internal/git"
	"github.com/devidevio/gitone/internal/repository"
)

// Whole-file snapshots preserve existing branch settings and make the two
// upstream keys one atomic change. Any external config edit requires review.
type upstreamConfig struct {
	Original []byte `json:"original"`
	Target   []byte `json:"target"`
}

func prepareUpstream(root, directory, name, branch string) (*upstreamConfig, error) {
	original, err := os.ReadFile(filepath.Join(repository.Directory(root, name), "config"))
	if err != nil {
		return nil, err
	}
	temporary := filepath.Join(directory, name+".upstream")
	if err := os.WriteFile(temporary, original, 0o600); err != nil {
		return nil, err
	}
	defer os.Remove(temporary)
	for _, setting := range []struct{ key, value string }{
		{"remote", config.OriginRemote},
		{"merge", "refs/heads/" + branch},
	} {
		if _, err := git.Run(root, "config", "--file", temporary, "--replace-all",
			"branch."+branch+"."+setting.key, setting.value); err != nil {
			return nil, err
		}
	}
	target, err := os.ReadFile(temporary)
	if err != nil {
		return nil, err
	}
	return &upstreamConfig{Original: original, Target: target}, nil
}

func (configuration *upstreamConfig) verify(root, name string) error {
	current, err := os.ReadFile(filepath.Join(repository.Directory(root, name), "config"))
	if err != nil {
		return err
	}
	if !bytes.Equal(current, configuration.Original) && !bytes.Equal(current, configuration.Target) {
		return fmt.Errorf("%s repository %q changed its Git configuration outside GitOne; keep the recovery state and resolve it manually", recoveryRequired, name)
	}
	return nil
}

func (configuration *upstreamConfig) write(root, name string, tracking bool) error {
	path := filepath.Join(repository.Directory(root, name), "config")
	// Use Git's own lock name so a native config writer cannot race the check.
	file, err := os.OpenFile(path+".lock", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		_ = file.Close()
		if !committed {
			_ = os.Remove(path + ".lock")
		}
	}()
	if err := configuration.verify(root, name); err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if err := file.Chmod(info.Mode().Perm()); err != nil {
		return err
	}
	contents := configuration.Original
	if tracking {
		contents = configuration.Target
	}
	if _, err := file.Write(contents); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(path+".lock", path); err != nil {
		return err
	}
	committed = true
	return nil
}
