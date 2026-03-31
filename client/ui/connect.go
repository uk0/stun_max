package ui

import (
	"image"
	"image/color"
	"strings"

	"gioui.org/layout"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/text"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"stun_max/client/core"
)

// ConnectScreen holds state for the connection form.
type ConnectScreen struct {
	ServerEditor   widget.Editor
	RoomEditor     widget.Editor
	PasswordEditor widget.Editor
	NameEditor     widget.Editor
	ConnectBtn     widget.Clickable

	// Advanced / STUN section
	AdvancedToggle  widget.Clickable
	advancedExpanded bool

	Connecting bool
	inited     bool
}

func (s *ConnectScreen) init() {
	if s.inited {
		return
	}
	s.inited = true
	s.ServerEditor.SingleLine = true
	s.RoomEditor.SingleLine = true
	s.PasswordEditor.SingleLine = true
	s.PasswordEditor.Mask = '*'
	s.NameEditor.SingleLine = true

	// Load saved config
	if cfg := LoadConfig(); cfg != nil {
		if cfg.ServerURL != "" {
			s.ServerEditor.SetText(cfg.ServerURL)
		}
		s.RoomEditor.SetText(cfg.Room)
		s.PasswordEditor.SetText(cfg.Password)
		s.NameEditor.SetText(cfg.Name)
	}
}

// Layout renders the connect screen.
func (s *ConnectScreen) Layout(gtx layout.Context, th *material.Theme, a *App) layout.Dimensions {
	s.init()

	// Toggle advanced section
	if s.AdvancedToggle.Clicked(gtx) {
		s.advancedExpanded = !s.advancedExpanded
	}

	// Handle connect button
	if s.ConnectBtn.Clicked(gtx) && !s.Connecting {
		server := s.ServerEditor.Text()
		room := s.RoomEditor.Text()
		password := s.PasswordEditor.Text()
		name := s.NameEditor.Text()
		if server != "" && room != "" {
			s.Connecting = true
			a.Error = ""

			// Collect STUN servers from settings panel
			stunServers := a.Dashboard.Settings.selectedSTUNServers()

			a.DoConnect(core.ClientConfig{
				ServerURL:   server,
				Room:        room,
				Password:    password,
				Name:        name,
				STUNServers: stunServers,
			})
		}
	}

	return layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		gtx.Constraints.Max.X = gtx.Dp(unit.Dp(420))
		gtx.Constraints.Min.X = gtx.Constraints.Max.X

		return layout.Flex{Axis: layout.Vertical, Alignment: layout.Middle}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layoutCard(gtx, func(gtx layout.Context) layout.Dimensions {
					return layout.Inset{Top: unit.Dp(32), Bottom: unit.Dp(32), Left: unit.Dp(32), Right: unit.Dp(32)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						return layout.Flex{Axis: layout.Vertical, Alignment: layout.Middle}.Layout(gtx,
							// Title
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								title := material.H5(th, "STUN Max")
								title.Color = AccentColor
								title.Alignment = text.Middle
								return title.Layout(gtx)
							}),
							// Subtitle
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								return layout.Inset{Top: unit.Dp(4), Bottom: unit.Dp(24)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
									sub := material.Body2(th, "P2P Tunnel Client")
									sub.Color = DimColor
									sub.Alignment = text.Middle
									return sub.Layout(gtx)
								})
							}),
							// Server input
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								return s.layoutInput(gtx, th, &s.ServerEditor, "Server URL (ws://...)")
							}),
							// Room input
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								return layout.Inset{Top: unit.Dp(12)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
									return s.layoutInput(gtx, th, &s.RoomEditor, "Room name")
								})
							}),
							// Password input
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								return layout.Inset{Top: unit.Dp(12)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
									return s.layoutInput(gtx, th, &s.PasswordEditor, "Password")
								})
							}),
							// Name input
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								return layout.Inset{Top: unit.Dp(12)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
									return s.layoutInput(gtx, th, &s.NameEditor, "Your name")
								})
							}),
							// Advanced / STUN section
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								return layout.Inset{Top: unit.Dp(12)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
									return s.layoutAdvanced(gtx, th, a)
								})
							}),
							// Error message
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								if a.Error == "" {
									return layout.Dimensions{}
								}
								return layout.Inset{Top: unit.Dp(12)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
									lbl := material.Body2(th, a.Error)
									lbl.Color = ErrorColor
									lbl.Alignment = text.Middle
									return lbl.Layout(gtx)
								})
							}),
							// Connect button
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								return layout.Inset{Top: unit.Dp(20)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
									btn := material.Button(th, &s.ConnectBtn, "Connect")
									btn.Background = AccentColor
									btn.Color = color.NRGBA{A: 255}
									btn.CornerRadius = unit.Dp(6)
									if s.Connecting {
										btn.Background = DimColor
										btn.Text = "Connecting..."
									}
									return btn.Layout(gtx)
								})
							}),
						)
					})
				})
			}),
		)
	})
}

// layoutAdvanced renders the collapsible STUN settings section.
func (s *ConnectScreen) layoutAdvanced(gtx layout.Context, th *material.Theme, a *App) layout.Dimensions {
	arrow := "▶ Advanced"
	if s.advancedExpanded {
		arrow = "▼ Advanced"
	}

	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		// Toggle header
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			btn := material.Clickable(gtx, &s.AdvancedToggle, func(gtx layout.Context) layout.Dimensions {
				lbl := material.Caption(th, arrow)
				lbl.Color = AccentColor
				return lbl.Layout(gtx)
			})
			return btn
		}),
		// Expanded content
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			if !s.advancedExpanded {
				return layout.Dimensions{}
			}
			return layout.Inset{Top: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return s.layoutSTUNSummary(gtx, th, a)
			})
		}),
	)
}

// layoutSTUNSummary shows the currently selected STUN servers.
func (s *ConnectScreen) layoutSTUNSummary(gtx layout.Context, th *material.Theme, a *App) layout.Dimensions {
	servers := a.Dashboard.Settings.selectedSTUNServers()
	var summary string
	if len(servers) == 0 {
		summary = "No STUN servers selected (direct relay only)"
	} else {
		summary = strings.Join(servers, ", ")
	}

	return layout.Stack{}.Layout(gtx,
		layout.Expanded(func(gtx layout.Context) layout.Dimensions {
			rr := clip.UniformRRect(image.Rect(0, 0, gtx.Constraints.Max.X, gtx.Constraints.Min.Y), gtx.Dp(unit.Dp(6)))
			paint.FillShape(gtx.Ops, InputBg, rr.Op(gtx.Ops))
			return layout.Dimensions{Size: image.Pt(gtx.Constraints.Max.X, gtx.Constraints.Min.Y)}
		}),
		layout.Stacked(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(10), Bottom: unit.Dp(10), Left: unit.Dp(12), Right: unit.Dp(12)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						lbl := material.Caption(th, "STUN Servers")
						lbl.Color = DimColor
						return lbl.Layout(gtx)
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return layout.Inset{Top: unit.Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							lbl := material.Caption(th, summary)
							lbl.Color = TextColor
							return lbl.Layout(gtx)
						})
					}),
				)
			})
		}),
	)
}

func (s *ConnectScreen) layoutInput(gtx layout.Context, th *material.Theme, editor *widget.Editor, hint string) layout.Dimensions {
	return layout.Stack{}.Layout(gtx,
		layout.Expanded(func(gtx layout.Context) layout.Dimensions {
			rr := clip.UniformRRect(image.Rect(0, 0, gtx.Constraints.Max.X, gtx.Constraints.Min.Y), gtx.Dp(unit.Dp(6)))
			paint.FillShape(gtx.Ops, InputBg, rr.Op(gtx.Ops))
			return layout.Dimensions{Size: image.Pt(gtx.Constraints.Max.X, gtx.Constraints.Min.Y)}
		}),
		layout.Stacked(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(10), Bottom: unit.Dp(10), Left: unit.Dp(12), Right: unit.Dp(12)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				ed := material.Editor(th, editor, hint)
				ed.Color = TextColor
				ed.HintColor = DimColor
				return ed.Layout(gtx)
			})
		}),
	)
}

func layoutCard(gtx layout.Context, w layout.Widget) layout.Dimensions {
	return layout.Stack{}.Layout(gtx,
		layout.Expanded(func(gtx layout.Context) layout.Dimensions {
			rr := clip.UniformRRect(image.Rect(0, 0, gtx.Constraints.Max.X, gtx.Constraints.Min.Y), gtx.Dp(unit.Dp(12)))
			paint.FillShape(gtx.Ops, CardColor, rr.Op(gtx.Ops))
			return layout.Dimensions{Size: image.Pt(gtx.Constraints.Max.X, gtx.Constraints.Min.Y)}
		}),
		layout.Stacked(w),
	)
}
