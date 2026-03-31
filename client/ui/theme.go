package ui

import (
	"image/color"

	"gioui.org/widget/material"
)

var (
	BgColor      = color.NRGBA{R: 10, G: 10, B: 15, A: 255}
	CardColor    = color.NRGBA{R: 17, G: 17, B: 34, A: 255}
	BorderColor  = color.NRGBA{R: 34, G: 34, B: 34, A: 255}
	AccentColor  = color.NRGBA{R: 0, G: 212, B: 255, A: 255}
	SuccessColor = color.NRGBA{R: 0, G: 200, B: 83, A: 255}
	WarningColor = color.NRGBA{R: 255, G: 152, B: 0, A: 255}
	ErrorColor   = color.NRGBA{R: 255, G: 68, B: 68, A: 255}
	TextColor    = color.NRGBA{R: 224, G: 224, B: 224, A: 255}
	DimColor     = color.NRGBA{R: 136, G: 136, B: 136, A: 255}
	InputBg      = color.NRGBA{R: 26, G: 26, B: 46, A: 255}
)

func NewTheme() *material.Theme {
	th := material.NewTheme()
	th.Palette.Bg = BgColor
	th.Palette.Fg = TextColor
	th.Palette.ContrastBg = AccentColor
	th.Palette.ContrastFg = color.NRGBA{R: 0, G: 0, B: 0, A: 255}
	return th
}
