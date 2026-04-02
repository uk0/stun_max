//go:build windows

package core

import (
	_ "embed"
	"os"
	"path/filepath"
)

//go:embed wintun.dll
var wintunDLL []byte

func init() {
	// Extract wintun.dll next to the executable if not already present
	exe, err := os.Executable()
	if err != nil {
		return
	}
	dllPath := filepath.Join(filepath.Dir(exe), "wintun.dll")
	if _, err := os.Stat(dllPath); err == nil {
		return // already exists
	}
	os.WriteFile(dllPath, wintunDLL, 0644)
}
