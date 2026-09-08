package atomicfs

import (
	"os"

	"golang.org/x/sys/unix"
)

// OpenNoFollow opens a file without following a symbolic link in its final
// path component.
func OpenNoFollow(name string) (*os.File, error) {
	descriptor, err := unix.Open(name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: name, Err: err}
	}
	return os.NewFile(uintptr(descriptor), name), nil
}
