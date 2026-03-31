package ui

import (
	"image"
	"image/color"
	"io"
	"strings"

	"gioui.org/io/clipboard"
	"gioui.org/layout"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"
)

// RDPState holds the state of the RDP setup.
type RDPState struct {
	Enabled  bool
	Username string
	Password string // only set after user provides it
	Error    string
}

// ToolsPanel provides Windows-specific tools (RDP, etc).
type ToolsPanel struct {
	PasswordEditor widget.Editor
	EnableBtn      widget.Clickable
	DisableBtn     widget.Clickable
	CopyPassBtn    widget.Clickable
	CopyUserBtn    widget.Clickable
	RDP            RDPState
	CopyMsg        string
	hasPassword    bool // current user has a password
	inited         bool
}

func (t *ToolsPanel) init() {
	if t.inited {
		return
	}
	t.inited = true
	t.RDP.Enabled = IsRDPEnabled()
	t.RDP.Username = GetCurrentUsername()
	t.PasswordEditor.SingleLine = true
	t.hasPassword = HasPassword(t.RDP.Username)
}

// Layout renders the tools panel.
func (t *ToolsPanel) Layout(gtx layout.Context, th *material.Theme, a *App) layout.Dimensions {
	t.init()

	if !RDPSupported() {
		return layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			lbl := material.Body1(th, "Tools are only available on Windows")
			lbl.Color = DimColor
			return lbl.Layout(gtx)
		})
	}

	// Handle Enable RDP
	if t.EnableBtn.Clicked(gtx) && !t.RDP.Enabled {
		pw := strings.TrimSpace(t.PasswordEditor.Text())
		t.RDP.Error = ""
		t.CopyMsg = ""

		if !t.hasPassword && pw == "" {
			t.RDP.Error = "Your account has no password. Please enter one to enable RDP. This will become your Windows login password."
		} else {
			go func() {
				if pw != "" {
					// Set new password
					if err := SetUserPassword(t.RDP.Username, pw); err != nil {
						t.RDP.Error = "Set password: " + err.Error()
						a.Window.Invalidate()
						return
					}
					t.RDP.Password = pw
					t.hasPassword = true
				}
				if err := EnableRDP(); err != nil {
					t.RDP.Error = "Enable RDP: " + err.Error()
				} else {
					t.RDP.Enabled = true
				}
				a.Window.Invalidate()
			}()
		}
	}

	// Handle Disable RDP
	if t.DisableBtn.Clicked(gtx) && t.RDP.Enabled {
		t.RDP.Error = ""
		t.CopyMsg = ""
		go func() {
			if err := DisableRDP(); err != nil {
				t.RDP.Error = "Disable RDP: " + err.Error()
			} else {
				t.RDP.Enabled = false
			}
			a.Window.Invalidate()
		}()
	}

	// Handle copy
	if t.CopyUserBtn.Clicked(gtx) {
		gtx.Execute(clipboard.WriteCmd{Type: "application/text", Data: io.NopCloser(strings.NewReader(t.RDP.Username))})
		t.CopyMsg = "Username copied!"
	}
	if t.CopyPassBtn.Clicked(gtx) && t.RDP.Password != "" {
		gtx.Execute(clipboard.WriteCmd{Type: "application/text", Data: io.NopCloser(strings.NewReader(t.RDP.Password))})
		t.CopyMsg = "Password copied!"
	}

	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return t.layoutRDPCard(gtx, th)
		}),
		// Error
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			if t.RDP.Error == "" {
				return layout.Dimensions{}
			}
			return layout.Inset{Top: unit.Dp(10)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				lbl := material.Body2(th, t.RDP.Error)
				lbl.Color = ErrorColor
				return lbl.Layout(gtx)
			})
		}),
	)
}

func (t *ToolsPanel) layoutRDPCard(gtx layout.Context, th *material.Theme) layout.Dimensions {
	return layout.Stack{}.Layout(gtx,
		layout.Expanded(func(gtx layout.Context) layout.Dimensions {
			rr := clip.UniformRRect(image.Rect(0, 0, gtx.Constraints.Max.X, gtx.Constraints.Min.Y), gtx.Dp(unit.Dp(8)))
			paint.FillShape(gtx.Ops, CardColor, rr.Op(gtx.Ops))
			return layout.Dimensions{Size: image.Pt(gtx.Constraints.Max.X, gtx.Constraints.Min.Y)}
		}),
		layout.Stacked(func(gtx layout.Context) layout.Dimensions {
			return layout.UniformInset(unit.Dp(16)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
					// Title + status
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								lbl := material.Body1(th, "Remote Desktop (RDP)")
								lbl.Color = TextColor
								return lbl.Layout(gtx)
							}),
							layout.Rigid(layout.Spacer{Width: unit.Dp(12)}.Layout),
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								status := "Stopped"
								c := ErrorColor
								if t.RDP.Enabled {
									status = "Running (port 3389)"
									c = SuccessColor
								}
								lbl := material.Caption(th, status)
								lbl.Color = c
								return lbl.Layout(gtx)
							}),
						)
					}),
					// Description
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return layout.Inset{Top: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							msg := "Your account already has a password. RDP will use your existing Windows login credentials."
							if !t.hasPassword {
								msg = "Your account has no password. Enter a password below before enabling RDP. This will be set as your Windows login password."
							}
							if t.RDP.Enabled {
								msg = "RDP is active. Use the credentials below to connect via Remote Desktop."
							}
							lbl := material.Caption(th, msg)
							lbl.Color = DimColor
							return lbl.Layout(gtx)
						})
					}),
					// Username display
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return layout.Inset{Top: unit.Dp(10)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
								layout.Rigid(func(gtx layout.Context) layout.Dimensions {
									lbl := material.Body2(th, "User:  ")
									lbl.Color = DimColor
									return lbl.Layout(gtx)
								}),
								layout.Rigid(func(gtx layout.Context) layout.Dimensions {
									lbl := material.Body1(th, t.RDP.Username)
									lbl.Color = TextColor
									return lbl.Layout(gtx)
								}),
								layout.Rigid(func(gtx layout.Context) layout.Dimensions {
									return layout.Inset{Left: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
										btn := material.Button(th, &t.CopyUserBtn, "Copy")
										btn.Background = DimColor
										btn.Color = color.NRGBA{R: 255, G: 255, B: 255, A: 255}
										btn.CornerRadius = unit.Dp(3)
										btn.TextSize = unit.Sp(11)
										btn.Inset = layout.Inset{Top: unit.Dp(2), Bottom: unit.Dp(2), Left: unit.Dp(8), Right: unit.Dp(8)}
										return btn.Layout(gtx)
									})
								}),
							)
						})
					}),
					// Password section
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return layout.Inset{Top: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							// Case 1: RDP enabled and we set a password — show it
							if t.RDP.Enabled && t.RDP.Password != "" {
								return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
									layout.Rigid(func(gtx layout.Context) layout.Dimensions {
										lbl := material.Body2(th, "Pass:  ")
										lbl.Color = DimColor
										return lbl.Layout(gtx)
									}),
									layout.Rigid(func(gtx layout.Context) layout.Dimensions {
										lbl := material.Body1(th, t.RDP.Password)
										lbl.Color = SuccessColor
										return lbl.Layout(gtx)
									}),
									layout.Rigid(func(gtx layout.Context) layout.Dimensions {
										return layout.Inset{Left: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
											btn := material.Button(th, &t.CopyPassBtn, "Copy")
											btn.Background = AccentColor
											btn.Color = color.NRGBA{A: 255}
											btn.CornerRadius = unit.Dp(3)
											btn.TextSize = unit.Sp(11)
											btn.Inset = layout.Inset{Top: unit.Dp(2), Bottom: unit.Dp(2), Left: unit.Dp(8), Right: unit.Dp(8)}
											return btn.Layout(gtx)
										})
									}),
								)
							}
							// Case 2: RDP enabled, using existing password
							if t.RDP.Enabled && t.hasPassword {
								lbl := material.Caption(th, "Pass:  (your existing Windows password)")
								lbl.Color = DimColor
								return lbl.Layout(gtx)
							}
							// Case 3: Has password, not enabled — just show hint
							if t.hasPassword && !t.RDP.Enabled {
								return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
									layout.Rigid(func(gtx layout.Context) layout.Dimensions {
										lbl := material.Caption(th, "Pass:  Using your existing password. Or enter a new one:")
										lbl.Color = DimColor
										return lbl.Layout(gtx)
									}),
									layout.Rigid(func(gtx layout.Context) layout.Dimensions {
										return layout.Inset{Left: unit.Dp(6)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
											gtx.Constraints.Max.X = gtx.Dp(unit.Dp(160))
											return t.layoutPasswordInput(gtx, th, "(optional)")
										})
									}),
								)
							}
							// Case 4: No password, must enter one
							return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
								layout.Rigid(func(gtx layout.Context) layout.Dimensions {
									lbl := material.Body2(th, "Pass:  ")
									lbl.Color = WarningColor
									return lbl.Layout(gtx)
								}),
								layout.Rigid(func(gtx layout.Context) layout.Dimensions {
									gtx.Constraints.Max.X = gtx.Dp(unit.Dp(200))
									return t.layoutPasswordInput(gtx, th, "Enter password (required)")
								}),
							)
						})
					}),
					// Enable / Disable button
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return layout.Inset{Top: unit.Dp(12)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							if t.RDP.Enabled {
								btn := material.Button(th, &t.DisableBtn, "Stop RDP")
								btn.Background = ErrorColor
								btn.Color = color.NRGBA{R: 255, G: 255, B: 255, A: 255}
								btn.CornerRadius = unit.Dp(4)
								btn.Inset = layout.Inset{Top: unit.Dp(6), Bottom: unit.Dp(6), Left: unit.Dp(16), Right: unit.Dp(16)}
								return btn.Layout(gtx)
							}
							btn := material.Button(th, &t.EnableBtn, "Enable RDP")
							btn.Background = SuccessColor
							btn.Color = color.NRGBA{A: 255}
							btn.CornerRadius = unit.Dp(4)
							btn.Inset = layout.Inset{Top: unit.Dp(6), Bottom: unit.Dp(6), Left: unit.Dp(16), Right: unit.Dp(16)}
							return btn.Layout(gtx)
						})
					}),
					// Copy feedback
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						if t.CopyMsg == "" {
							return layout.Dimensions{}
						}
						return layout.Inset{Top: unit.Dp(6)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							lbl := material.Caption(th, t.CopyMsg)
							lbl.Color = SuccessColor
							return lbl.Layout(gtx)
						})
					}),
				)
			})
		}),
	)
}

func (t *ToolsPanel) layoutPasswordInput(gtx layout.Context, th *material.Theme, hint string) layout.Dimensions {
	return layout.Stack{}.Layout(gtx,
		layout.Expanded(func(gtx layout.Context) layout.Dimensions {
			rr := clip.UniformRRect(image.Rect(0, 0, gtx.Constraints.Max.X, gtx.Constraints.Min.Y), gtx.Dp(unit.Dp(4)))
			paint.FillShape(gtx.Ops, InputBg, rr.Op(gtx.Ops))
			return layout.Dimensions{Size: image.Pt(gtx.Constraints.Max.X, gtx.Constraints.Min.Y)}
		}),
		layout.Stacked(func(gtx layout.Context) layout.Dimensions {
			return layout.UniformInset(unit.Dp(8)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				ed := material.Editor(th, &t.PasswordEditor, hint)
				ed.Color = TextColor
				ed.HintColor = DimColor
				return ed.Layout(gtx)
			})
		}),
	)
}
