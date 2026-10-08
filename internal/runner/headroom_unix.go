//go:build unix

package runner

import "syscall"

// freeDisk is the space available to this user under dir.
//
// Bavail, not Bfree: on every unix filesystem a slice is reserved for root,
// and counting it would let the loop start a run into space it cannot use.
func freeDisk(dir string) (uint64, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, false
	}
	// Bsize is uint32 on darwin and int64 on linux; Bavail is uint64 on both.
	return uint64(st.Bavail) * uint64(st.Bsize), true
}

// totalDisk is the size of the filesystem holding dir.
//
// Blocks, not Bavail: this is the denominator a fleet view divides the free
// half by, so it must count the whole filesystem including the root reserve
// and everything already used. The free half deliberately excludes the
// reserve, which means free is never quite "total minus used" — that is
// correct, and it is the difference between what exists and what a run can
// actually write into.
func totalDisk(dir string) (uint64, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, false
	}
	if st.Blocks == 0 || st.Bsize == 0 {
		return 0, false
	}
	return uint64(st.Blocks) * uint64(st.Bsize), true
}
