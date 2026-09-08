// Package lock guards a project against concurrent mutating GitOne
// operations. Only one such operation may run at a time.
package lock

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

const (
	// File is the project lock, relative to the internal state directory.
	File = "lock"
	// RecoveryDirectory holds the state of an interrupted operation.
	RecoveryDirectory = "recovery"
	// StateFile is the recorded state of an interrupted operation, relative
	// to RecoveryDirectory. Every operation writes the same envelope into it.
	StateFile = "state.json"

	internalDirectory = ".gitone"
)

// state is the owner metadata written into the lock file.
type state struct {
	PID     int    `json:"pid"`
	Host    string `json:"host"`
	Command string `json:"command"`
}

// Acquire takes the project lock for command and returns the release
// function. A lock held by a live process fails with LOCK001. A lock left
// behind by a dead process is reclaimed unless recovery state exists, which
// fails with REC001 because the interrupted operation must be resolved first.
func Acquire(root, command string) (func(), error) {
	return acquire(root, command, true)
}

// AcquireRecovery takes the project lock like Acquire but does not stop at
// existing recovery state, because resolving it is exactly what the calling
// command does.
func AcquireRecovery(root, command string) (func(), error) {
	return acquire(root, command, false)
}

func acquire(root, command string, stopOnRecovery bool) (func(), error) {
	name := filepath.Join(root, internalDirectory, File)
	contents, err := json.Marshal(state{PID: os.Getpid(), Host: hostname(), Command: command})
	if err != nil {
		return nil, err
	}

	guard, err := os.Open(root)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(guard.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = guard.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			owner, _, _ := readOwner(name)
			return nil, held(owner)
		}
		return nil, err
	}

	internal := filepath.Join(root, internalDirectory)
	createdInternal := false
	if err := os.Mkdir(internal, 0o700); err == nil {
		createdInternal = true
	} else if !errors.Is(err, os.ErrExist) {
		unlock(guard)
		return nil, err
	}

	file, err := os.OpenFile(name, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		if createdInternal {
			_ = os.Remove(internal)
		}
		unlock(guard)
		return nil, err
	}

	previous, exists, err := decodeOwner(file)
	if err != nil || exists && !reclaimable(previous) {
		_ = file.Close()
		unlock(guard)
		return nil, blocked(previous)
	}
	if stopOnRecovery {
		if err := recovery(root); err != nil {
			_ = file.Close()
			unlock(guard)
			return nil, err
		}
	}
	err = file.Chmod(0o600)
	if err == nil {
		err = file.Truncate(0)
	}
	if err == nil {
		_, err = file.Seek(0, 0)
	}
	if err == nil {
		_, err = file.Write(contents)
	}
	if err != nil {
		_ = os.Remove(name)
		_ = file.Close()
		if createdInternal {
			_ = os.Remove(internal)
		}
		unlock(guard)
		return nil, err
	}
	_ = file.Close()

	var once sync.Once
	return func() {
		once.Do(func() {
			_ = os.Remove(name)
			if createdInternal {
				_ = os.Remove(internal)
			}
			unlock(guard)
		})
	}, nil
}

// AcquireRead holds a shared project lock until the returned release function
// is called. It never creates, writes or removes a file.
func AcquireRead(root string) (func(), error) {
	name := filepath.Join(root, internalDirectory, File)
	guard, err := os.Open(root)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(guard.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		_ = guard.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			owner, _, _ := readOwner(name)
			return nil, held(owner)
		}
		return nil, err
	}

	file, err := os.Open(name)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		unlock(guard)
		return nil, err
	}
	if err == nil {
		owner, exists, err := decodeOwner(file)
		_ = file.Close()
		if err != nil || exists && !reclaimable(owner) {
			unlock(guard)
			return nil, blocked(owner)
		}
	}
	if err := recovery(root); err != nil {
		unlock(guard)
		return nil, err
	}
	return release(guard), nil
}

// State is the read-only view of the project lock and the recorded state of
// an interrupted operation.
type State struct {
	// Active reports an operation currently holding the lock and Stale a lock
	// file whose process is gone, which the next operation may reclaim.
	// Unattributed is neither: a lock file naming another host or one that
	// cannot be read names no process this machine can wait for.
	Active       bool
	Stale        bool
	Unattributed bool
	PID          int
	Host         string
	Command      string
	// Recovery reports that an interrupted operation left state behind, and
	// Interrupted names the command that recorded it when it is readable.
	Recovery    bool
	Interrupted string
}

// envelope is the part of a recorded operation state every operation shares.
// The operation packages own the rest of their own state.
type envelope struct {
	Command string `json:"command"`
}

// Inspect reads the lock and recovery state without acquiring, creating,
// writing or removing anything, so it can report a running operation instead
// of refusing to run beside it.
func Inspect(root string) (State, error) {
	var result State
	guard, err := os.Open(root)
	if err != nil {
		return result, err
	}
	defer guard.Close()
	if err := syscall.Flock(int(guard.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return result, err
		}
		result.Active = true
	} else {
		defer func() { _ = syscall.Flock(int(guard.Fd()), syscall.LOCK_UN) }()
	}

	name := filepath.Join(root, internalDirectory, File)
	switch _, err := os.Stat(name); {
	case err == nil:
		owner, recorded, err := readOwner(name)
		result.PID, result.Host, result.Command = owner.PID, owner.Host, owner.Command
		// This mirrors what acquire decides, so doctor never demands a repair
		// for a lock the next operation reclaims by itself. A file recording
		// no owner is the leftover of an interrupted acquire; one whose
		// process has ended here is reclaimable too. A live process on this
		// machine is a running operation, and anything else names no process
		// this machine can check, so reporting it as running would send the
		// reader off to wait for something that never ends. The flock still
		// decides first: it proves a live owner even while the file it names
		// is still being written.
		switch {
		case err != nil:
			result.Unattributed = !result.Active
		case !recorded || reclaimable(owner):
			result.Stale = true
		case attributable(owner):
			result.Active = true
		default:
			result.Unattributed = !result.Active
		}
	case !errors.Is(err, os.ErrNotExist):
		return result, err
	}

	switch contents, err := os.ReadFile(filepath.Join(RecoveryPath(root), StateFile)); {
	case err == nil:
		result.Recovery = true
		var recorded envelope
		if json.Unmarshal(contents, &recorded) == nil {
			result.Interrupted = recorded.Command
		}
	case errors.Is(err, os.ErrNotExist):
		// Prepared state without a recorded command still has to be resolved.
		if _, err := os.Stat(RecoveryPath(root)); err == nil {
			result.Recovery = true
		} else if !errors.Is(err, os.ErrNotExist) {
			return result, err
		}
	default:
		return result, err
	}
	return result, nil
}

// RecoveryPath is the directory holding the recorded state of an interrupted
// operation.
func RecoveryPath(root string) string {
	return filepath.Join(root, internalDirectory, RecoveryDirectory)
}

// WriteState atomically replaces the recorded state shared by GitOne's
// recoverable operations.
//
// ponytail: no fsync, this survives a process crash but not a power loss.
func WriteState(directory string, saved any) error {
	contents, err := json.Marshal(saved)
	if err != nil {
		return err
	}
	temporary := filepath.Join(directory, StateFile+".gitone")
	if err := os.WriteFile(temporary, contents, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, filepath.Join(directory, StateFile))
}

// recovery reports REC001 when an interrupted operation left state behind.
func recovery(root string) error {
	_, err := os.Stat(RecoveryPath(root))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("REC001 an interrupted GitOne operation must be recovered first: run gitone recover or gitone abort")
}

func decodeOwner(file *os.File) (state, bool, error) {
	contents, err := io.ReadAll(file)
	if err != nil || len(contents) == 0 {
		return state{}, false, err
	}
	var owner state
	if err := json.Unmarshal(contents, &owner); err != nil {
		return state{}, true, err
	}
	return owner, true, nil
}

func readOwner(name string) (state, bool, error) {
	file, err := os.Open(name)
	if err != nil {
		return state{}, false, err
	}
	defer file.Close()
	return decodeOwner(file)
}

// attributable reports whether the lock file names a process this machine can
// actually check. Only then does the file say anything about a running
// operation; otherwise the answer is unknown, not "running".
func attributable(owner state) bool {
	host := hostname()
	return owner.PID > 0 && host != "" && owner.Host == host
}

func reclaimable(owner state) bool {
	return attributable(owner) && !alive(owner.PID)
}

func unlock(file *os.File) {
	_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	_ = file.Close()
}

func release(file *os.File) func() {
	var once sync.Once
	return func() { once.Do(func() { unlock(file) }) }
}

func held(owner state) error {
	if owner.PID <= 0 {
		return fmt.Errorf("LOCK001 another GitOne operation is currently running")
	}
	return fmt.Errorf("LOCK001 another GitOne operation is currently running: PID %d, %s", owner.PID, owner.Command)
}

// blocked refuses on the lock file alone, with no flock behind it to prove a
// live owner. Waiting only ever ends when that owner is a process on this
// machine, so a file from another host or one that cannot be read is reported
// as what it is instead of as a running operation.
func blocked(owner state) error {
	if attributable(owner) {
		return held(owner)
	}
	path := filepath.Join(internalDirectory, File)
	if owner.Host != "" {
		return fmt.Errorf("LOCK001 %s belongs to host %q, not to this machine, so no running operation can be confirmed: run gitone doctor", path, owner.Host)
	}
	return fmt.Errorf("LOCK001 %s names no process this machine can check: run gitone doctor", path)
}

// alive reports whether pid still names a running process. A process owned by
// another user answers with EPERM and is therefore alive too.
func alive(pid int) bool {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = process.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}

func hostname() string {
	name, err := os.Hostname()
	if err != nil {
		return ""
	}
	return name
}
