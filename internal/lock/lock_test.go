package lock_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/devidevio/gitone/internal/lock"
)

func project(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".gitone"), 0o700); err != nil {
		t.Fatal(err)
	}
	return root
}

func lockFile(root string) string {
	return filepath.Join(root, ".gitone", lock.File)
}

func TestAcquireAndRelease(t *testing.T) {
	root := project(t)

	release, err := lock.Acquire(root, "gitone init")
	if err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(lockFile(root))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contents), `"command":"gitone init"`) || !strings.Contains(string(contents), `"pid":`) {
		t.Fatalf("lock = %s, want process and command metadata", contents)
	}

	release()
	if _, err := os.Stat(lockFile(root)); !os.IsNotExist(err) {
		t.Fatalf("lock still exists after release: %v", err)
	}
}

func TestAcquireRejectsLiveLock(t *testing.T) {
	root := project(t)

	release, err := lock.Acquire(root, "gitone init")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	_, err = lock.Acquire(root, "gitone commit")
	if err == nil || !strings.HasPrefix(err.Error(), "LOCK001 ") {
		t.Fatalf("error = %v, want LOCK001", err)
	}
	if !strings.Contains(err.Error(), "gitone init") {
		t.Fatalf("error = %v, want the holding command", err)
	}
}

// writeStaleLock stores a lock owned by a process that no longer exists. PID 1
// would be alive, so a freshly reaped child PID is used instead.
func writeStaleLock(t *testing.T, root, host string) {
	t.Helper()
	deadPID := deadProcess(t)
	contents := `{"pid":` + strconv.Itoa(deadPID) + `,"host":"` + host + `","command":"gitone commit"}`
	if err := os.WriteFile(lockFile(root), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAcquireReclaimsStaleLock(t *testing.T) {
	root := project(t)
	writeStaleLock(t, root, hostname(t))

	release, err := lock.Acquire(root, "gitone init")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	contents, err := os.ReadFile(lockFile(root))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contents), `"command":"gitone init"`) {
		t.Fatalf("lock = %s, want the new owner", contents)
	}
}

func TestOnlyOneAcquireReclaimsStaleLock(t *testing.T) {
	root := project(t)
	writeStaleLock(t, root, hostname(t))

	type result struct {
		release func()
		err     error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	for range 2 {
		go func() {
			<-start
			release, err := lock.Acquire(root, "gitone init")
			results <- result{release, err}
		}()
	}
	close(start)

	acquired := 0
	for range 2 {
		result := <-results
		if result.err == nil {
			acquired++
			defer result.release()
		} else if !strings.HasPrefix(result.err.Error(), "LOCK001 ") {
			t.Fatalf("error = %v, want LOCK001", result.err)
		}
	}
	if acquired != 1 {
		t.Fatalf("acquired = %d, want 1", acquired)
	}
}

func TestAcquireKeepsStaleLockWithRecoveryState(t *testing.T) {
	root := project(t)
	writeStaleLock(t, root, hostname(t))
	if err := os.Mkdir(filepath.Join(root, ".gitone", lock.RecoveryDirectory), 0o700); err != nil {
		t.Fatal(err)
	}

	_, err := lock.Acquire(root, "gitone init")
	if err == nil || !strings.HasPrefix(err.Error(), "REC001 ") {
		t.Fatalf("error = %v, want REC001", err)
	}
	if _, err := os.Stat(lockFile(root)); err != nil {
		t.Fatalf("lock was removed: %v", err)
	}
}

func TestAcquireKeepsForeignLock(t *testing.T) {
	root := project(t)
	writeStaleLock(t, root, "another-host")

	if _, err := lock.Acquire(root, "gitone init"); err == nil || !strings.HasPrefix(err.Error(), "LOCK001 ") {
		t.Fatalf("error = %v, want LOCK001", err)
	}
}

func TestAcquireKeepsUnreadableLock(t *testing.T) {
	root := project(t)
	if err := os.WriteFile(lockFile(root), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := lock.Acquire(root, "gitone init"); err == nil || !strings.HasPrefix(err.Error(), "LOCK001 ") {
		t.Fatalf("error = %v, want LOCK001", err)
	}
}

// deadProcess returns the PID of a process that has already exited and been
// reaped, so no other process can share it during the test.
func deadProcess(t *testing.T) int {
	t.Helper()
	command := exec.Command("true")
	if err := command.Run(); err != nil {
		t.Fatal(err)
	}
	return command.Process.Pid
}

func hostname(t *testing.T) string {
	t.Helper()
	name, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	return name
}

func TestAcquireReadDoesNotCreateOrRemoveTheLock(t *testing.T) {
	root := project(t)

	releaseRead, err := lock.AcquireRead(root)
	if err != nil {
		t.Fatalf("check without a lock = %v", err)
	}
	if _, err := os.Stat(lockFile(root)); !os.IsNotExist(err) {
		t.Fatalf("check created the lock file: %v", err)
	}
	if _, err := lock.Acquire(root, "gitone commit"); err == nil || !strings.HasPrefix(err.Error(), "LOCK001") {
		t.Fatalf("mutation while reading = %v, want LOCK001", err)
	}
	releaseRead()

	release, err := lock.Acquire(root, "gitone commit")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lock.AcquireRead(root); err == nil || !strings.HasPrefix(err.Error(), "LOCK001") {
		t.Fatalf("check while held = %v, want LOCK001", err)
	}
	release()

	releaseRead, err = lock.AcquireRead(root)
	if err != nil {
		t.Fatalf("check after release = %v", err)
	}
	releaseRead()
}

func TestAcquireReadReportsRequiredRecovery(t *testing.T) {
	root := project(t)
	// A lock left behind by a dead process together with recovery state.
	writeStaleLock(t, root, hostname(t))
	if err := os.Mkdir(filepath.Join(root, ".gitone", lock.RecoveryDirectory), 0o700); err != nil {
		t.Fatal(err)
	}

	if _, err := lock.AcquireRead(root); err == nil || !strings.HasPrefix(err.Error(), "REC001") {
		t.Fatalf("check = %v, want REC001", err)
	}
}

// A lock this machine cannot attribute must not be reported as a running
// operation: waiting for it would never end. Both refusal and Inspect have to
// say so, for a foreign host and for a file that cannot be read.
func TestUnattributableLockIsNotReportedAsRunning(t *testing.T) {
	tests := []struct {
		name     string
		contents string
		want     string
	}{
		{
			name:     "another host",
			contents: `{"pid":1,"host":"some-other-machine","command":"gitone commit"}`,
			want:     `belongs to host "some-other-machine"`,
		},
		{
			name:     "unreadable",
			contents: "{not json",
			want:     "names no process this machine can check",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := project(t)
			if err := os.WriteFile(lockFile(root), []byte(test.contents), 0o600); err != nil {
				t.Fatal(err)
			}

			_, err := lock.Acquire(root, "gitone commit")
			if err == nil || !strings.HasPrefix(err.Error(), "LOCK001") {
				t.Fatalf("acquire = %v, want LOCK001", err)
			}
			if strings.Contains(err.Error(), "currently running") {
				t.Fatalf("acquire = %v, want no claim that an operation is running", err)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("acquire = %v, want it to contain %q", err, test.want)
			}

			state, err := lock.Inspect(root)
			if err != nil {
				t.Fatal(err)
			}
			if !state.Unattributed || state.Active || state.Stale {
				t.Fatalf("state = %+v, want unattributed only", state)
			}
		})
	}
}

// An interrupted acquire can leave a lock file behind before it records an
// owner. Acquire reclaims it without help, so inspection must not report a
// state that asks anyone to repair it.
func TestEmptyLockNeedsNoRepair(t *testing.T) {
	root := project(t)
	if err := os.WriteFile(lockFile(root), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	state, err := lock.Inspect(root)
	if err != nil {
		t.Fatal(err)
	}
	if state.Unattributed || state.Active {
		t.Fatalf("state = %+v, want a reclaimable leftover", state)
	}
	if !state.Stale {
		t.Fatalf("state = %+v, want stale", state)
	}

	release, err := lock.Acquire(root, "gitone commit")
	if err != nil {
		t.Fatalf("acquire over an empty lock = %v, want it reclaimed", err)
	}
	release()
}

// The counterpart: a lock this machine can attribute to a live process is a
// running operation and must keep saying exactly that.
func TestLiveLockIsStillReportedAsRunning(t *testing.T) {
	root := project(t)
	release, err := lock.Acquire(root, "gitone commit")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	state, err := lock.Inspect(root)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Active || state.Unattributed {
		t.Fatalf("state = %+v, want active", state)
	}
}
