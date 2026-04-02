package ui

import (
	"fmt"
	"image"
	"image/color"
	"strings"

	"gioui.org/layout"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"stun_max/client/core"
)

// VPNPanel manages the TUN VPN UI.
type VPNPanel struct {
	PeerEditor   widget.Editor
	RoutesEditor widget.Editor // comma or space separated subnets
	StartBtn     widget.Clickable
	StopBtn      widget.Clickable
	List         widget.List
	Error        string
	inited       bool
}

func (v *VPNPanel) init() {
	if v.inited {
		return
	}
	v.inited = true
	v.PeerEditor.SingleLine = true
	v.RoutesEditor.SingleLine = true
	v.List.Axis = layout.Vertical
}

// Layout renders the VPN panel.
func (v *VPNPanel) Layout(gtx layout.Context, th *material.Theme, a *App) layout.Dimensions {
	v.init()

	var tunInfo core.TunInfo
	if a.Client != nil {
		tunInfo = a.Client.TunStatus()
	}

	// Handle start button
	if v.StartBtn.Clicked(gtx) && a.Client != nil {
		peer := strings.TrimSpace(v.PeerEditor.Text())
		routesStr := strings.TrimSpace(v.RoutesEditor.Text())
		if peer == "" {
			v.Error = "Enter a peer ID or name"
		} else {
			v.Error = ""
			// Parse routes: comma or space separated
			var routes []string
			if routesStr != "" {
				for _, r := range strings.FieldsFunc(routesStr, func(c rune) bool {
					return c == ',' || c == ' '
				}) {
					r = strings.TrimSpace(r)
					if r != "" {
						routes = append(routes, r)
					}
				}
			}
			go func() {
				if err := a.Client.StartTun(peer, routes); err != nil {
					v.Error = err.Error()
					a.Window.Invalidate()
				}
			}()
		}
	}

	// Handle stop button
	if v.StopBtn.Clicked(gtx) && a.Client != nil {
		go func() {
			if err := a.Client.StopTun(); err != nil {
				v.Error = err.Error()
				a.Window.Invalidate()
			}
		}()
	}

	return material.List(th, &v.List).Layout(gtx, 1, func(gtx layout.Context, _ int) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			// Control card
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return v.layoutControlCard(gtx, th, tunInfo)
			}),
			// Status card
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return v.layoutStatusCard(gtx, th, tunInfo)
			}),
			// Info card
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return v.layoutInfoCard(gtx, th)
			}),
		)
	})
}

func (v *VPNPanel) layoutControlCard(gtx layout.Context, th *material.Theme, info core.TunInfo) layout.Dimensions {
	return layout.Stack{}.Layout(gtx,
		layout.Expanded(func(gtx layout.Context) layout.Dimensions {
			rr := clip.UniformRRect(image.Rect(0, 0, gtx.Constraints.Max.X, gtx.Constraints.Min.Y), gtx.Dp(unit.Dp(8)))
			paint.FillShape(gtx.Ops, CardColor, rr.Op(gtx.Ops))
			return layout.Dimensions{Size: image.Pt(gtx.Constraints.Max.X, gtx.Constraints.Min.Y)}
		}),
		layout.Stacked(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(16), Bottom: unit.Dp(16), Left: unit.Dp(16), Right: unit.Dp(16)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						lbl := material.Body1(th, "TUN VPN")
						lbl.Color = TextColor
						return lbl.Layout(gtx)
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return layout.Inset{Top: unit.Dp(12)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
								layout.Flexed(0.35, func(gtx layout.Context) layout.Dimensions {
									return layoutInputField(gtx, th, &v.PeerEditor, "Peer ID or name")
								}),
								layout.Rigid(layout.Spacer{Width: unit.Dp(8)}.Layout),
								layout.Flexed(0.45, func(gtx layout.Context) layout.Dimensions {
									return layoutInputField(gtx, th, &v.RoutesEditor, "Subnets (e.g. 10.88.51.0/24)")
								}),
								layout.Rigid(layout.Spacer{Width: unit.Dp(8)}.Layout),
								layout.Rigid(func(gtx layout.Context) layout.Dimensions {
									if info.Enabled {
										btn := material.Button(th, &v.StopBtn, "Stop VPN")
										btn.Background = ErrorColor
										btn.Color = color.NRGBA{R: 255, G: 255, B: 255, A: 255}
										btn.CornerRadius = unit.Dp(4)
										btn.Inset = layout.Inset{Top: unit.Dp(8), Bottom: unit.Dp(8), Left: unit.Dp(20), Right: unit.Dp(20)}
										return btn.Layout(gtx)
									}
									btn := material.Button(th, &v.StartBtn, "Start VPN")
									btn.Background = SuccessColor
									btn.Color = color.NRGBA{A: 255}
									btn.CornerRadius = unit.Dp(4)
									btn.Inset = layout.Inset{Top: unit.Dp(8), Bottom: unit.Dp(8), Left: unit.Dp(20), Right: unit.Dp(20)}
									return btn.Layout(gtx)
								}),
							)
						})
					}),
					// Error
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						if v.Error == "" {
							return layout.Dimensions{}
						}
						return layout.Inset{Top: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							lbl := material.Caption(th, v.Error)
							lbl.Color = ErrorColor
							return lbl.Layout(gtx)
						})
					}),
				)
			})
		}),
	)
}

func (v *VPNPanel) layoutStatusCard(gtx layout.Context, th *material.Theme, info core.TunInfo) layout.Dimensions {
	if !info.Enabled {
		return layout.Inset{Top: unit.Dp(16)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			lbl := material.Body2(th, "No active VPN connection")
			lbl.Color = DimColor
			return lbl.Layout(gtx)
		})
	}

	return layout.Inset{Top: unit.Dp(16)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Stack{}.Layout(gtx,
			layout.Expanded(func(gtx layout.Context) layout.Dimensions {
				rr := clip.UniformRRect(image.Rect(0, 0, gtx.Constraints.Max.X, gtx.Constraints.Min.Y), gtx.Dp(unit.Dp(8)))
				paint.FillShape(gtx.Ops, CardColor, rr.Op(gtx.Ops))
				return layout.Dimensions{Size: image.Pt(gtx.Constraints.Max.X, gtx.Constraints.Min.Y)}
			}),
			layout.Stacked(func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Top: unit.Dp(12), Bottom: unit.Dp(12), Left: unit.Dp(16), Right: unit.Dp(16)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
								layout.Rigid(func(gtx layout.Context) layout.Dimensions {
									lbl := material.Body2(th, "VPN Active")
									lbl.Color = SuccessColor
									return lbl.Layout(gtx)
								}),
								layout.Rigid(func(gtx layout.Context) layout.Dimensions {
									return layout.Spacer{Width: unit.Dp(12)}.Layout(gtx)
								}),
								layout.Rigid(func(gtx layout.Context) layout.Dimensions {
									lbl := material.Caption(th, fmt.Sprintf("Peer: %s", info.PeerName))
									lbl.Color = DimColor
									return lbl.Layout(gtx)
								}),
							)
						}),
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							return layout.Inset{Top: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
								return layout.Flex{Axis: layout.Horizontal}.Layout(gtx,
									layout.Flexed(0.33, func(gtx layout.Context) layout.Dimensions {
										return vpnStatItem(gtx, th, "Local IP", info.VirtualIP)
									}),
									layout.Flexed(0.33, func(gtx layout.Context) layout.Dimensions {
										return vpnStatItem(gtx, th, "Peer IP", info.PeerIP)
									}),
									layout.Flexed(0.34, func(gtx layout.Context) layout.Dimensions {
										return vpnStatItem(gtx, th, "Subnet", info.Subnet)
									}),
								)
							})
						}),
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							return layout.Inset{Top: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
								traffic := fmt.Sprintf("Up: %s (%s/s)  Down: %s (%s/s)",
									fmtSize(info.BytesUp), fmtSize(int64(info.RateUp)),
									fmtSize(info.BytesDown), fmtSize(int64(info.RateDown)))
								lbl := material.Caption(th, traffic)
								lbl.Color = AccentColor
								return lbl.Layout(gtx)
							})
						}),
					)
				})
			}),
		)
	})
}

func vpnStatItem(gtx layout.Context, th *material.Theme, label, value string) layout.Dimensions {
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			lbl := material.Caption(th, label)
			lbl.Color = DimColor
			return lbl.Layout(gtx)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			lbl := material.Body2(th, value)
			lbl.Color = TextColor
			return lbl.Layout(gtx)
		}),
	)
}

func (v *VPNPanel) layoutInfoCard(gtx layout.Context, th *material.Theme) layout.Dimensions {
	return layout.Inset{Top: unit.Dp(16)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Stack{}.Layout(gtx,
			layout.Expanded(func(gtx layout.Context) layout.Dimensions {
				rr := clip.UniformRRect(image.Rect(0, 0, gtx.Constraints.Max.X, gtx.Constraints.Min.Y), gtx.Dp(unit.Dp(8)))
				paint.FillShape(gtx.Ops, CardColor, rr.Op(gtx.Ops))
				return layout.Dimensions{Size: image.Pt(gtx.Constraints.Max.X, gtx.Constraints.Min.Y)}
			}),
			layout.Stacked(func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Top: unit.Dp(12), Bottom: unit.Dp(12), Left: unit.Dp(16), Right: unit.Dp(16)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							lbl := material.Caption(th, "TUN VPN creates a virtual network interface for direct IP-level connectivity with a peer.")
							lbl.Color = DimColor
							return lbl.Layout(gtx)
						}),
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							return layout.Inset{Top: unit.Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
								lbl := material.Caption(th, "Requires root/admin privileges. Traffic is compressed and relayed through the signaling server.")
								lbl.Color = DimColor
								return lbl.Layout(gtx)
							})
						}),
					)
				})
			}),
		)
	})
}
