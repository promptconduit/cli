//go:build windows

package outbound

// inodeOf returns 0 on Windows — there is no portable inode equivalent
// in stdlib. Tail falls back to size-shrink detection for rotation,
// which is adequate for this observability surface.
func inodeOf(path string) (uint64, error) {
	return 0, nil
}
