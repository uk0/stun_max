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
	PeerSel      *PeerSelector
	RoutesEditor widget.Editor
	ExitIPEditor widget.Editor
	StartBtn     widget.Clickable
	StopAllBtn   widget.Clickable
	StopBtns     []widget.Clickable // per-VPN stop buttons
	List         widget.List
	Error        string
	inited       bool
}

func (v *VPNPanel) init() {
	if v.inited {
		return
	}
	v.inited = true
	v.PeerSel = NewPeerSelector("Select peer")
	v.RoutesEditor.SingleLine = true
	v.ExitIPEditor.SingleLine = true
	v.List.Axis = layout.Vertical

	// Restore VPN settings from config
	if cfg := LoadConfig(); cfg != nil {
		if cfg.VPNPeer != "" {
			v.PeerSel.Selected = cfg.VPNPeer
		}
		if len(cfg.VPNRoutes) > 0 {
			v.RoutesEditor.SetText(strings.Join(cfg.VPNRoutes, ", "))
		}
		if cfg.VPNExitIP != "" {
			v.ExitIPEditor.SetText(cfg.VPNExitIP)
		}
	}
}

// Layout renders the VPN panel.
func (v *VPNPanel) Layout(gtx layout.Context, th *material.Theme, a *App) layout.Dimensions {
	v.init()

	var vpnList []core.TunInfo
	if a.Client != nil {
		vpnList = a.Client.TunStatusAll()
	}

	// Ensure enough stop buttons
	for len(v.StopBtns) < len(vpnList) {
		v.StopBtns = append(v.StopBtns, widget.Clickable{})
	}

	// Handle per-VPN stop buttons
	for i := range vpnList {
		if i < len(v.StopBtns) && v.StopBtns[i].Clicked(gtx) && a.Client != nil {
			peerID := vpnList[i].PeerID
			go func() {
				if err := a.Client.StopTunPeer(peerID); err != nil {
					v.Error = err.Error()
					a.Window.Invalidate()
				}
			}()
		}
	}

	// Handle start button
	if v.StartBtn.Clicked(gtx) && a.Client != nil {
		peer := strings.TrimSpace(v.PeerSel.Text())
		routesStr := strings.TrimSpace(v.RoutesEditor.Text())
		exitIP := strings.TrimSpace(v.ExitIPEditor.Text())
		if peer == "" {
			v.Error = "Select a peer"
		} else {
			v.Error = ""
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
				if err := a.Client.StartTun(peer, routes, exitIP); err != nil {
					v.Error = err.Error()
					a.Window.Invalidate()
				} else {
					// Save VPN config
					if cfg := LoadConfig(); cfg != nil {
						cfg.VirtualIP = core.GetVirtualIP()
						cfg.VPNPeer = peer
						cfg.VPNRoutes = routes
						cfg.VPNExitIP = exitIP
						cfg.VPNAutoStart = true
						SaveConfig(cfg)
					}
				}
			}()
		}
	}

	// Handle stop all button
	if v.StopAllBtn.Clicked(gtx) && a.Client != nil {
		go func() {
			if err := a.Client.StopTun(); err != nil {
				v.Error = err.Error()
				a.Window.Invalidate()
			} else {
				if cfg := LoadConfig(); cfg != nil {
					cfg.VPNAutoStart = false
					SaveConfig(cfg)
				}
			}
		}()
	}

	return material.List(th, &v.List).Layout(gtx, 1, func(gtx layout.Context, _ int) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			// Control card (always visible — start new VPN)
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return v.layoutControlCard(gtx, th, a, vpnList)
			}),
			// Active VPN list
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return v.layoutVPNList(gtx, th, vpnList)
			}),
			// Info card
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return v.layoutInfoCard(gtx, th)
			}),
		)
	})
}

func (v *VPNPanel) layoutControlCard(gtx layout.Context, th *material.Theme, a *App, vpnList []core.TunInfo) layout.Dimensions {
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
								layout.Flexed(0.22, func(gtx layout.Context) layout.Dimensions {
									return v.PeerSel.Layout(gtx, th, a)
								}),
								layout.Rigid(layout.Spacer{Width: unit.Dp(6)}.Layout),
								layout.Flexed(0.30, func(gtx layout.Context) layout.Dimensions {
									return layoutInputField(gtx, th, &v.RoutesEditor, "Subnets (e.g. 10.88.51.0/24)")
								}),
								layout.Rigid(layout.Spacer{Width: unit.Dp(6)}.Layout),
								layout.Flexed(0.20, func(gtx layout.Context) layout.Dimensions {
									return layoutInputField(gtx, th, &v.ExitIPEditor, "Exit IP (auto)")
								}),
								layout.Rigid(layout.Spacer{Width: unit.Dp(6)}.Layout),
								layout.Rigid(func(gtx layout.Context) layout.Dimensions {
									btn := material.Button(th, &v.StartBtn, "Start VPN")
									btn.Background = SuccessColor
									btn.Color = color.NRGBA{A: 255}
									btn.CornerRadius = unit.Dp(4)
									btn.Inset = layout.Inset{Top: unit.Dp(8), Bottom: unit.Dp(8), Left: unit.Dp(12), Right: unit.Dp(12)}
									return btn.Layout(gtx)
								}),
								layout.Rigid(func(gtx layout.Context) layout.Dimensions {
									if len(vpnList) == 0 {
										return layout.Dimensions{}
									}
									return layout.Inset{Left: unit.Dp(6)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
										btn := material.Button(th, &v.StopAllBtn, "Stop All")
										btn.Background = ErrorColor
										btn.Color = color.NRGBA{R: 255, G: 255, B: 255, A: 255}
										btn.CornerRadius = unit.Dp(4)
										btn.Inset = layout.Inset{Top: unit.Dp(8), Bottom: unit.Dp(8), Left: unit.Dp(12), Right: unit.Dp(12)}
										return btn.Layout(gtx)
									})
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

func (v *VPNPanel) layoutVPNList(gtx layout.Context, th *material.Theme, vpnList []core.TunInfo) layout.Dimensions {
	if len(vpnList) == 0 {
		return layout.Inset{Top: unit.Dp(16)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			lbl := material.Body2(th, "No active VPN connections")
			lbl.Color = DimColor
			return lbl.Layout(gtx)
		})
	}

	var children []layout.FlexChild
	for i, info := range vpnList {
		idx := i
		vpn := info
		children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(12)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.Stack{}.Layout(gtx,
					layout.Expanded(func(gtx layout.Context) layout.Dimensions {
						rr := clip.UniformRRect(image.Rect(0, 0, gtx.Constraints.Max.X, gtx.Constraints.Min.Y), gtx.Dp(unit.Dp(8)))
						paint.FillShape(gtx.Ops, CardColor, rr.Op(gtx.Ops))
						return layout.Dimensions{Size: image.Pt(gtx.Constraints.Max.X, gtx.Constraints.Min.Y)}
					}),
					layout.Stacked(func(gtx layout.Context) layout.Dimensions {
						return layout.Inset{Top: unit.Dp(10), Bottom: unit.Dp(10), Left: unit.Dp(14), Right: unit.Dp(14)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
								// Header: role badge + peer name + stop button (right-aligned)
								layout.Rigid(func(gtx layout.Context) layout.Dimensions {
									return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
										// Role badge
										layout.Rigid(func(gtx layout.Context) layout.Dimensions {
											roleLabel := "OUT"
											roleColor := AccentColor
											if vpn.Role == "responder" {
												roleLabel = "IN"
												roleColor = DimColor
											}
											return layout.Inset{Right: unit.Dp(6)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
												return layout.Stack{}.Layout(gtx,
													layout.Expanded(func(gtx layout.Context) layout.Dimensions {
														rr := clip.UniformRRect(image.Rect(0, 0, gtx.Constraints.Min.X, gtx.Constraints.Min.Y), gtx.Dp(unit.Dp(3)))
														paint.FillShape(gtx.Ops, roleColor, rr.Op(gtx.Ops))
														return layout.Dimensions{Size: image.Pt(gtx.Constraints.Min.X, gtx.Constraints.Min.Y)}
													}),
													layout.Stacked(func(gtx layout.Context) layout.Dimensions {
														return layout.Inset{Top: unit.Dp(2), Bottom: unit.Dp(2), Left: unit.Dp(6), Right: unit.Dp(6)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
															lbl := material.Caption(th, roleLabel)
															lbl.Color = color.NRGBA{R: 255, G: 255, B: 255, A: 255}
															return lbl.Layout(gtx)
														})
													}),
												)
											})
										}),
										// Peer name
										layout.Rigid(func(gtx layout.Context) layout.Dimensions {
											lbl := material.Body2(th, vpn.PeerName)
											lbl.Color = SuccessColor
											return lbl.Layout(gtx)
										}),
										// Spacer → push stop to right edge
										layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
											return layout.Dimensions{Size: image.Pt(gtx.Constraints.Max.X, 0)}
										}),
										// Stop button
										layout.Rigid(func(gtx layout.Context) layout.Dimensions {
											if idx >= len(v.StopBtns) {
												return layout.Dimensions{}
											}
											btn := material.Button(th, &v.StopBtns[idx], "Stop")
											btn.Background = ErrorColor
											btn.Color = color.NRGBA{R: 255, G: 255, B: 255, A: 255}
											btn.CornerRadius = unit.Dp(4)
											btn.TextSize = unit.Sp(11)
											btn.Inset = layout.Inset{Top: unit.Dp(4), Bottom: unit.Dp(4), Left: unit.Dp(10), Right: unit.Dp(10)}
											return btn.Layout(gtx)
										}),
									)
								}),
								// Stats row
								layout.Rigid(func(gtx layout.Context) layout.Dimensions {
									return layout.Inset{Top: unit.Dp(6)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
										return layout.Flex{Axis: layout.Horizontal}.Layout(gtx,
											layout.Flexed(0.20, func(gtx layout.Context) layout.Dimensions {
												return vpnStatItem(gtx, th, "Local IP", vpn.VirtualIP)
											}),
											layout.Flexed(0.20, func(gtx layout.Context) layout.Dimensions {
												return vpnStatItem(gtx, th, "Peer IP", vpn.PeerIP)
											}),
											layout.Flexed(0.20, func(gtx layout.Context) layout.Dimensions {
												routeStr := "-"
												if len(vpn.Routes) > 0 {
													routeStr = strings.Join(vpn.Routes, ", ")
												}
												return vpnStatItem(gtx, th, "Routes", routeStr)
											}),
											layout.Flexed(0.20, func(gtx layout.Context) layout.Dimensions {
												traffic := fmt.Sprintf("↑%s ↓%s", fmtSize(vpn.BytesUp), fmtSize(vpn.BytesDown))
												return vpnStatItem(gtx, th, "Traffic", traffic)
											}),
											layout.Flexed(0.20, func(gtx layout.Context) layout.Dimensions {
												speed := fmt.Sprintf("↑%s/s ↓%s/s", fmtSize(int64(vpn.RateUp)), fmtSize(int64(vpn.RateDown)))
												return vpnStatItem(gtx, th, "Speed", speed)
											}),
										)
									})
								}),
							)
						})
					}),
				)
			})
		}))
	}

	return layout.Inset{Top: unit.Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
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
