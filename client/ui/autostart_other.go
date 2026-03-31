//go:build !windows

package ui

// SetAutostart is a no-op on non-Windows platforms.
func SetAutostart(enable bool) error { return nil }

// GetAutostart always returns false on non-Windows platforms.
func GetAutostart() bool { return false }
