package scripts_test

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallerPathGuidanceSelectsInstalledBinary(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprintf("existing=%t", existing), func(t *testing.T) {
			root := t.TempDir()
			bin := filepath.Join(root, "bin")
			install := filepath.Join(root, "new bin")
			if err := os.Mkdir(bin, 0o755); err != nil {
				t.Fatal(err)
			}
			write := func(path, content string) {
				t.Helper()
				if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			binary := "#!/bin/sh\nprintf 'gitone v0.1.0-test\\n'\n"
			asset := "gitone_v0.1.0-test_linux_amd64"
			write(filepath.Join(root, asset), binary)
			write(filepath.Join(root, "checksums.txt"), fmt.Sprintf("%x  %s\n", sha256.Sum256([]byte(binary)), asset))
			write(filepath.Join(bin, "uname"), "#!/bin/sh\ncase \"$1\" in -s) echo Linux;; -m) echo x86_64;; esac\n")
			write(filepath.Join(bin, "curl"), `#!/bin/sh
output=
url=
while [ "$#" -gt 0 ]; do
 case "$1" in
  -o) output=$2; shift 2 ;;
  -*) shift ;;
  *) url=$1; shift ;;
 esac
done
[ -n "$output" ] && [ -n "$url" ] || exit 1
cp "$GITONE_TEST_DOWNLOADS/${url##*/}" "$output"
`)
			if existing {
				write(filepath.Join(bin, "gitone"), "#!/bin/sh\necho old-version\n")
			}
			path := bin + ":/usr/bin:/bin"
			command := exec.Command("sh", "../install.sh")
			command.Env = append(os.Environ(), "PATH="+path, "GITONE_VERSION=v0.1.0-test",
				"GITONE_INSTALL_DIR="+install, "GITONE_TEST_DOWNLOADS="+root)
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("installer: %v\n%s", err, output)
			}
			if !strings.Contains(string(output), "Then verify:\n  gitone --version") {
				t.Fatalf("missing verification introduction:\n%s", output)
			}
			guidance := `export PATH="` + install + `:$PATH"`
			if !strings.Contains(string(output), guidance) {
				t.Fatalf("missing %q in:\n%s", guidance, output)
			}
			if existing && !strings.Contains(string(output), "Your PATH selects "+filepath.Join(bin, "gitone")) {
				t.Fatalf("missing shadowed installation explanation:\n%s", output)
			}
			command = exec.Command("sh", "-c", guidance+"\ngitone --version")
			command.Env = append(os.Environ(), "PATH="+path)
			if output, err := command.CombinedOutput(); err != nil || string(output) != "gitone v0.1.0-test\n" {
				t.Fatalf("PATH repair selected the wrong binary: %v\n%s", err, output)
			}
		})
	}
}
