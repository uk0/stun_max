package ui

import (
	"fmt"
	"image"
	"image/color"
	"strconv"
	"strings"

	"gioui.org/layout"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"stun_max/client/core"
)

// ForwardsPanel manages port forwards.
type ForwardsPanel struct {
	PeerEditor  widget.Editor
	HostEditor  widget.Editor
	LocalEditor widget.Editor
	StartBtn    widget.Clickable
	StopBtns    map[int]*widget.Clickable // stop (pause) running forward
	DelBtns     map[int]*widget.Clickable // delete saved forward rule
	StartFwdBtns map[int]*widget.Clickable // start a stopped saved forward
	ModeBtns    map[int]*widget.Clickable
	List        widget.List
	Error       string
	inited      bool
}

func (f *ForwardsPanel) init() {
	if f.inited {
		return
	}
	f.inited = true
	f.PeerEditor.SingleLine = true
	f.HostEditor.SingleLine = true
	f.LocalEditor.SingleLine = true
	f.StopBtns = make(map[int]*widget.Clickable)
	f.DelBtns = make(map[int]*widget.Clickable)
	f.StartFwdBtns = make(map[int]*widget.Clickable)
	f.ModeBtns = make(map[int]*widget.Clickable)
	f.List.Axis = layout.Vertical
}

// Layout renders the forwards panel.
func (f *ForwardsPanel) Layout(gtx layout.Context, th *material.Theme, a *App) layout.Dimensions {
	f.init()

	// Handle create new forward
	if f.StartBtn.Clicked(gtx) && a.Client != nil {
		f.Error = ""
		peer := strings.TrimSpace(f.PeerEditor.Text())
		hostPort := strings.TrimSpace(f.HostEditor.Text())
		localStr := strings.TrimSpace(f.LocalEditor.Text())

		if peer == "" || hostPort == "" {
			f.Error = "Peer and host:port are required"
		} else {
			host, portStr, ok := splitHostPort(hostPort)
			if !ok {
				f.Error = "Invalid host:port format"
			} else {
				remotePort, err := strconv.Atoi(portStr)
				if err != nil || remotePort <= 0 || remotePort > 65535 {
					f.Error = "Invalid remote port"
				} else {
					localPort := remotePort
					if localStr != "" {
						lp, err := strconv.Atoi(localStr)
						if err != nil || lp <= 0 || lp > 65535 {
							f.Error = "Invalid local port"
						} else {
							localPort = lp
						}
					}
					if f.Error == "" {
						if err := a.Client.StartForward(peer, host, remotePort, localPort); err != nil {
							f.Error = err.Error()
						} else {
							// Save forward rule
							a.AddSavedForward(SavedForward{
								PeerName: peer, RemoteHost: host,
								RemotePort: remotePort, LocalPort: localPort,
								Enabled: true,
							})
							f.PeerEditor.SetText("")
							f.HostEditor.SetText("")
							f.LocalEditor.SetText("")
						}
					}
				}
			}
		}
	}

	// Get active forwards and saved forwards
	a.mu.Lock()
	forwards := make([]core.ForwardInfo, len(a.Forwards))
	copy(forwards, a.Forwards)
	savedFwds := make([]SavedForward, len(a.SavedForwards))
	copy(savedFwds, a.SavedForwards)
	a.mu.Unlock()

	// Build set of active local ports
	activeSet := make(map[int]bool)
	for _, fwd := range forwards {
		activeSet[fwd.LocalPort] = true
	}

	// Handle button clicks
	for _, fwd := range forwards {
		if f.stopBtn(fwd.LocalPort).Clicked(gtx) && a.Client != nil {
			a.Client.StopForward(fwd.LocalPort)
			a.SetSavedForwardEnabled(fwd.LocalPort, false)
		}
		if f.modeBtn(fwd.LocalPort).Clicked(gtx) && a.Client != nil {
			a.Client.SetForwardMode(fwd.LocalPort, !fwd.ForceRelay)
		}
	}
	for _, sf := range savedFwds {
		if !activeSet[sf.LocalPort] {
			if f.startFwdBtn(sf.LocalPort).Clicked(gtx) && a.Client != nil {
				a.Client.StartForward(sf.PeerName, sf.RemoteHost, sf.RemotePort, sf.LocalPort)
				a.SetSavedForwardEnabled(sf.LocalPort, true)
			}
		}
		if f.delBtn(sf.LocalPort).Clicked(gtx) {
			if activeSet[sf.LocalPort] && a.Client != nil {
				a.Client.StopForward(sf.LocalPort)
			}
			a.RemoveSavedForward(sf.LocalPort)
		}
	}

	// Build display list: active forwards + stopped saved forwards
	type displayItem struct {
		Active  bool
		Info    core.ForwardInfo
		Saved   SavedForward
	}
	var items []displayItem
	// Active first
	for _, fwd := range forwards {
		items = append(items, displayItem{Active: true, Info: fwd})
	}
	// Stopped saved forwards
	for _, sf := range savedFwds {
		if !activeSet[sf.LocalPort] {
			items = append(items, displayItem{Active: false, Saved: sf})
		}
	}

	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return f.layoutForm(gtx, th)
		}),
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(16)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				if len(items) == 0 {
					return layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						lbl := material.Body1(th, "No forwards configured")
						lbl.Color = DimColor
						return lbl.Layout(gtx)
					})
				}
				list := material.List(th, &f.List)
				return list.Layout(gtx, len(items), func(gtx layout.Context, i int) layout.Dimensions {
					return layout.Inset{Bottom: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						item := items[i]
						if item.Active {
							return f.layoutActiveCard(gtx, th, item.Info)
						}
						return f.layoutStoppedCard(gtx, th, item.Saved)
					})
				})
			})
		}),
	)
}

func (f *ForwardsPanel) stopBtn(port int) *widget.Clickable {
	if f.StopBtns[port] == nil {
		f.StopBtns[port] = new(widget.Clickable)
	}
	return f.StopBtns[port]
}

func (f *ForwardsPanel) modeBtn(port int) *widget.Clickable {
	if f.ModeBtns[port] == nil {
		f.ModeBtns[port] = new(widget.Clickable)
	}
	return f.ModeBtns[port]
}

func (f *ForwardsPanel) delBtn(port int) *widget.Clickable {
	if f.DelBtns[port] == nil {
		f.DelBtns[port] = new(widget.Clickable)
	}
	return f.DelBtns[port]
}

func (f *ForwardsPanel) startFwdBtn(port int) *widget.Clickable {
	if f.StartFwdBtns[port] == nil {
		f.StartFwdBtns[port] = new(widget.Clickable)
	}
	return f.StartFwdBtns[port]
}

func (f *ForwardsPanel) layoutForm(gtx layout.Context, th *material.Theme) layout.Dimensions {
	return layout.Stack{}.Layout(gtx,
		layout.Expanded(func(gtx layout.Context) layout.Dimensions {
			rr := clip.UniformRRect(image.Rect(0, 0, gtx.Constraints.Max.X, gtx.Constraints.Min.Y), gtx.Dp(unit.Dp(8)))
			paint.FillShape(gtx.Ops, CardColor, rr.Op(gtx.Ops))
			return layout.Dimensions{Size: image.Pt(gtx.Constraints.Max.X, gtx.Constraints.Min.Y)}
		}),
		layout.Stacked(func(gtx layout.Context) layout.Dimensions {
			return layout.UniformInset(unit.Dp(16)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						lbl := material.Body1(th, "New Forward")
						lbl.Color = TextColor
						return lbl.Layout(gtx)
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return layout.Inset{Top: unit.Dp(12)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							return layout.Flex{Axis: layout.Horizontal, Spacing: layout.SpaceEvenly}.Layout(gtx,
								layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
									return layoutInputField(gtx, th, &f.PeerEditor, "Peer ID or name")
								}),
								layout.Rigid(layout.Spacer{Width: unit.Dp(8)}.Layout),
								layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
									return layoutInputField(gtx, th, &f.HostEditor, "host:port")
								}),
								layout.Rigid(layout.Spacer{Width: unit.Dp(8)}.Layout),
								layout.Rigid(func(gtx layout.Context) layout.Dimensions {
									gtx.Constraints.Max.X = gtx.Dp(unit.Dp(80))
									return layoutInputField(gtx, th, &f.LocalEditor, "Local port")
								}),
								layout.Rigid(layout.Spacer{Width: unit.Dp(8)}.Layout),
								layout.Rigid(func(gtx layout.Context) layout.Dimensions {
									btn := material.Button(th, &f.StartBtn, "Start")
									btn.Background = SuccessColor
									btn.Color = color.NRGBA{A: 255}
									btn.CornerRadius = unit.Dp(4)
									btn.Inset = layout.Inset{Top: unit.Dp(8), Bottom: unit.Dp(8), Left: unit.Dp(16), Right: unit.Dp(16)}
									return btn.Layout(gtx)
								}),
							)
						})
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						if f.Error == "" {
							return layout.Dimensions{}
						}
						return layout.Inset{Top: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							lbl := material.Caption(th, f.Error)
							lbl.Color = ErrorColor
							return lbl.Layout(gtx)
						})
					}),
				)
			})
		}),
	)
}

func (f *ForwardsPanel) layoutActiveCard(gtx layout.Context, th *material.Theme, fwd core.ForwardInfo) layout.Dimensions {
	return layout.Stack{}.Layout(gtx,
		layout.Expanded(func(gtx layout.Context) layout.Dimensions {
			rr := clip.UniformRRect(image.Rect(0, 0, gtx.Constraints.Max.X, gtx.Constraints.Min.Y), gtx.Dp(unit.Dp(8)))
			paint.FillShape(gtx.Ops, CardColor, rr.Op(gtx.Ops))
			return layout.Dimensions{Size: image.Pt(gtx.Constraints.Max.X, gtx.Constraints.Min.Y)}
		}),
		layout.Stacked(func(gtx layout.Context) layout.Dimensions {
			return layout.UniformInset(unit.Dp(14)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
					// Row 1: route + badges + buttons
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
							layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
								txt := fmt.Sprintf(":%d  →  %s:%d", fwd.LocalPort, fwd.RemoteHost, fwd.RemotePort)
								lbl := material.Body1(th, txt)
								lbl.Color = TextColor
								return lbl.Layout(gtx)
							}),
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								return layout.Inset{Left: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
									return layoutBadge(gtx, th, fwd.Mode)
								})
							}),
							// Mode toggle button
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								return layout.Inset{Left: unit.Dp(6)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
									// Only show relay button when P2P is active
									label := ""
									bg := DimColor
									if fwd.Mode == "P2P" && !fwd.ForceRelay {
										label = "→ Relay"
										bg = WarningColor
									} else if fwd.ForceRelay {
										label = "→ P2P"
										bg = SuccessColor
									}
									if label == "" {
										return layout.Dimensions{} // no button if relay-only (no P2P available)
									}
									btn := material.Button(th, f.modeBtn(fwd.LocalPort), label)
									btn.Background = bg
									btn.Color = color.NRGBA{A: 255}
									btn.CornerRadius = unit.Dp(4)
									btn.TextSize = unit.Sp(11)
									btn.Inset = layout.Inset{Top: unit.Dp(3), Bottom: unit.Dp(3), Left: unit.Dp(8), Right: unit.Dp(8)}
									return btn.Layout(gtx)
								})
							}),
							// Stop button (pause, keeps config)
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								return layout.Inset{Left: unit.Dp(6)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
									btn := material.Button(th, f.stopBtn(fwd.LocalPort), "Stop")
									btn.Background = WarningColor
									btn.Color = color.NRGBA{A: 255}
									btn.CornerRadius = unit.Dp(4)
									btn.TextSize = unit.Sp(11)
									btn.Inset = layout.Inset{Top: unit.Dp(3), Bottom: unit.Dp(3), Left: unit.Dp(8), Right: unit.Dp(8)}
									return btn.Layout(gtx)
								})
							}),
							// Delete button (remove config)
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								return layout.Inset{Left: unit.Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
									btn := material.Button(th, f.delBtn(fwd.LocalPort), "Del")
									btn.Background = ErrorColor
									btn.Color = color.NRGBA{R: 255, G: 255, B: 255, A: 255}
									btn.CornerRadius = unit.Dp(4)
									btn.TextSize = unit.Sp(11)
									btn.Inset = layout.Inset{Top: unit.Dp(3), Bottom: unit.Dp(3), Left: unit.Dp(8), Right: unit.Dp(8)}
									return btn.Layout(gtx)
								})
							}),
						)
					}),
					// Row 2: peer + conns + traffic stats
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return layout.Inset{Top: unit.Dp(6)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
								layout.Rigid(func(gtx layout.Context) layout.Dimensions {
									txt := fmt.Sprintf("via %s  |  %d conns", fwd.PeerName, fwd.ConnCount)
									lbl := material.Caption(th, txt)
									lbl.Color = DimColor
									return lbl.Layout(gtx)
								}),
								layout.Flexed(1, layout.Spacer{}.Layout),
								// Traffic: total
								layout.Rigid(func(gtx layout.Context) layout.Dimensions {
									txt := fmt.Sprintf("↑ %s  ↓ %s", formatBytes(fwd.BytesUp), formatBytes(fwd.BytesDown))
									lbl := material.Caption(th, txt)
									lbl.Color = AccentColor
									return lbl.Layout(gtx)
								}),
								layout.Rigid(layout.Spacer{Width: unit.Dp(16)}.Layout),
								// Traffic: rate
								layout.Rigid(func(gtx layout.Context) layout.Dimensions {
									txt := fmt.Sprintf("↑ %s/s  ↓ %s/s", formatBytes(int64(fwd.RateUp)), formatBytes(int64(fwd.RateDown)))
									lbl := material.Caption(th, txt)
									lbl.Color = SuccessColor
									return lbl.Layout(gtx)
								}),
							)
						})
					}),
				)
			})
		}),
	)
}

// layoutStoppedCard renders a saved but stopped forward rule.
func (f *ForwardsPanel) layoutStoppedCard(gtx layout.Context, th *material.Theme, sf SavedForward) layout.Dimensions {
	return layout.Stack{}.Layout(gtx,
		layout.Expanded(func(gtx layout.Context) layout.Dimensions {
			rr := clip.UniformRRect(image.Rect(0, 0, gtx.Constraints.Max.X, gtx.Constraints.Min.Y), gtx.Dp(unit.Dp(8)))
			paint.FillShape(gtx.Ops, CardColor, rr.Op(gtx.Ops))
			return layout.Dimensions{Size: image.Pt(gtx.Constraints.Max.X, gtx.Constraints.Min.Y)}
		}),
		layout.Stacked(func(gtx layout.Context) layout.Dimensions {
			return layout.UniformInset(unit.Dp(14)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
					layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
						return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								txt := fmt.Sprintf(":%d  →  %s:%d", sf.LocalPort, sf.RemoteHost, sf.RemotePort)
								lbl := material.Body1(th, txt)
								lbl.Color = DimColor
								return lbl.Layout(gtx)
							}),
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								txt := fmt.Sprintf("via %s  |  STOPPED", sf.PeerName)
								lbl := material.Caption(th, txt)
								lbl.Color = DimColor
								return lbl.Layout(gtx)
							}),
						)
					}),
					// Start button
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return layout.Inset{Left: unit.Dp(6)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							btn := material.Button(th, f.startFwdBtn(sf.LocalPort), "Start")
							btn.Background = SuccessColor
							btn.Color = color.NRGBA{A: 255}
							btn.CornerRadius = unit.Dp(4)
							btn.TextSize = unit.Sp(11)
							btn.Inset = layout.Inset{Top: unit.Dp(3), Bottom: unit.Dp(3), Left: unit.Dp(8), Right: unit.Dp(8)}
							return btn.Layout(gtx)
						})
					}),
					// Delete button
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return layout.Inset{Left: unit.Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							btn := material.Button(th, f.delBtn(sf.LocalPort), "Del")
							btn.Background = ErrorColor
							btn.Color = color.NRGBA{R: 255, G: 255, B: 255, A: 255}
							btn.CornerRadius = unit.Dp(4)
							btn.TextSize = unit.Sp(11)
							btn.Inset = layout.Inset{Top: unit.Dp(3), Bottom: unit.Dp(3), Left: unit.Dp(8), Right: unit.Dp(8)}
							return btn.Layout(gtx)
						})
					}),
				)
			})
		}),
	)
}

func formatBytes(b int64) string {
	if b < 1024 {
		return fmt.Sprintf("%d B", b)
	}
	if b < 1024*1024 {
		return fmt.Sprintf("%.1f KB", float64(b)/1024)
	}
	if b < 1024*1024*1024 {
		return fmt.Sprintf("%.1f MB", float64(b)/(1024*1024))
	}
	return fmt.Sprintf("%.2f GB", float64(b)/(1024*1024*1024))
}

func layoutInputField(gtx layout.Context, th *material.Theme, editor *widget.Editor, hint string) layout.Dimensions {
	return layout.Stack{}.Layout(gtx,
		layout.Expanded(func(gtx layout.Context) layout.Dimensions {
			rr := clip.UniformRRect(image.Rect(0, 0, gtx.Constraints.Max.X, gtx.Constraints.Min.Y), gtx.Dp(unit.Dp(4)))
			paint.FillShape(gtx.Ops, InputBg, rr.Op(gtx.Ops))
			return layout.Dimensions{Size: image.Pt(gtx.Constraints.Max.X, gtx.Constraints.Min.Y)}
		}),
		layout.Stacked(func(gtx layout.Context) layout.Dimensions {
			return layout.UniformInset(unit.Dp(8)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				ed := material.Editor(th, editor, hint)
				ed.Color = TextColor
				ed.HintColor = DimColor
				return ed.Layout(gtx)
			})
		}),
	)
}

func splitHostPort(s string) (host, port string, ok bool) {
	idx := strings.LastIndex(s, ":")
	if idx < 0 {
		return "", "", false
	}
	return s[:idx], s[idx+1:], true
}
