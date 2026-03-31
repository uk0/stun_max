//go:build !cli

package main

import (
	"os"

	"gioui.org/app"

	"stun_max/client/ui"
)

func main() {
	go func() {
		a := ui.NewApp()
		if err := a.Run(); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}()
	app.Main()
}
