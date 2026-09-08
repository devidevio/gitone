// Package atomicfs contains the platform-specific filesystem operations that
// must not degrade into check-then-act races.
package atomicfs

import "golang.org/x/sys/unix"

// RenameNoReplace atomically renames source while refusing an existing target.
func RenameNoReplace(source, target string) error {
	return unix.RenameatxNp(unix.AT_FDCWD, source, unix.AT_FDCWD, target, unix.RENAME_EXCL)
}

// RenameExchange atomically swaps two existing paths.
func RenameExchange(first, second string) error {
	return unix.RenameatxNp(unix.AT_FDCWD, first, unix.AT_FDCWD, second, unix.RENAME_SWAP)
}
