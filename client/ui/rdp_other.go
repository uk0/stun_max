//go:build !windows

package ui

func EnableRDP() error                    { return nil }
func DisableRDP() error                   { return nil }
func SetUserPassword(_, _ string) error   { return nil }
func GetCurrentUsername() string           { return "" }
func IsRDPEnabled() bool                  { return false }
func UserExists(_ string) bool            { return false }
func RDPSupported() bool                  { return false }
func HasPassword(_ string) bool           { return true }
