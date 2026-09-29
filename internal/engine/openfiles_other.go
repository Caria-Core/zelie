//go:build !linux

package engine

// openFilesCeiling cannot be read off Linux, where nothing runs anyway.
func openFilesCeiling(string) (uint64, bool) { return 0, false }
