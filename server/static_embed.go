package main

import "embed"

//go:embed all:web
var embeddedWeb embed.FS
