package ui

import (
	"image"
	"image/color"

	"gioui.org/io/event"
	"gioui.org/io/pointer"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"
)

// PeerSelector is a dropdown selector for choosing a peer.
type PeerSelector struct {
	Selected string // selected peer name or ID
	open     bool
	mainBtn  widget.Clickable
	items    []widget.Clickable
	hint     string
}

type peerOption struct {
	Name string
	Mode string
}

func NewPeerSelector(hint string) *PeerSelector {
	return &PeerSelector{hint: hint}
}

func (ps *PeerSelector) Text() string {
	return ps.Selected
}

// Layout renders the peer selector dropdown.
func (ps *PeerSelector) Layout(gtx layout.Context, th *material.Theme, a *App) layout.Dimensions {
	// Filter out self
	var available []peerOption
	if a.Client != nil {
		for _, p := range a.Peers {
			if p.ID != a.Client.MyID {
				name := p.Name
				if name == "" {
					name = shortID(p.ID)
				}
				mode := a.Client.PeerMode(p.ID)
				available = append(available, peerOption{Name: name, Mode: mode})
			}
		}
	}

	// Ensure enough clickable items
	for len(ps.items) < len(available) {
		ps.items = append(ps.items, widget.Clickable{})
	}

	// Handle main button click → toggle dropdown
	if ps.mainBtn.Clicked(gtx) {
		ps.open = !ps.open
	}

	// Handle item clicks
	for i := range available {
		if i < len(ps.items) && ps.items[i].Clicked(gtx) {
			ps.Selected = available[i].Name
			ps.open = false
		}
	}

	// Close dropdown on outside click
	if ps.open {
		area := clip.Rect{Max: image.Pt(gtx.Constraints.Max.X, gtx.Constraints.Max.Y)}.Push(gtx.Ops)
		event.Op(gtx.Ops, &ps.open)
		for {
			ev, ok := gtx.Event(pointer.Filter{Target: &ps.open, Kinds: pointer.Press})
			if !ok {
				break
			}
			if _, ok := ev.(pointer.Event); ok {
				ps.open = false
			}
		}
		area.Pop()
	}

	return layout.Stack{Alignment: layout.NW}.Layout(gtx,
		// Main button (always visible)
		layout.Stacked(func(gtx layout.Context) layout.Dimensions {
			return ps.mainBtn.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.Stack{}.Layout(gtx,
					layout.Expanded(func(gtx layout.Context) layout.Dimensions {
						rr := clip.UniformRRect(image.Rect(0, 0, gtx.Constraints.Max.X, gtx.Constraints.Min.Y), gtx.Dp(unit.Dp(4)))
						paint.FillShape(gtx.Ops, InputBg, rr.Op(gtx.Ops))
						return layout.Dimensions{Size: image.Pt(gtx.Constraints.Max.X, gtx.Constraints.Min.Y)}
					}),
					layout.Stacked(func(gtx layout.Context) layout.Dimensions {
						return layout.UniformInset(unit.Dp(8)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
								layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
									text := ps.Selected
									c := TextColor
									if text == "" {
										text = ps.hint
										c = DimColor
									}
									lbl := material.Body2(th, text)
									lbl.Color = c
									return lbl.Layout(gtx)
								}),
								layout.Rigid(func(gtx layout.Context) layout.Dimensions {
									arrow := "▼"
									if ps.open {
										arrow = "▲"
									}
									lbl := material.Body2(th, arrow)
									lbl.Color = DimColor
									return lbl.Layout(gtx)
								}),
							)
						})
					}),
				)
			})
		}),
		// Dropdown overlay (when open)
		layout.Expanded(func(gtx layout.Context) layout.Dimensions {
			if !ps.open || len(available) == 0 {
				return layout.Dimensions{}
			}
			// Offset below the main button
			defer op.Offset(image.Pt(0, gtx.Dp(unit.Dp(38)))).Push(gtx.Ops).Pop()
			return ps.layoutDropdown(gtx, th, available)
		}),
	)
}

func (ps *PeerSelector) layoutDropdown(gtx layout.Context, th *material.Theme, peers []peerOption) layout.Dimensions {
	maxH := gtx.Dp(unit.Dp(200))
	dropBg := color.NRGBA{R: 40, G: 42, B: 54, A: 255}
	hoverBg := color.NRGBA{R: 60, G: 63, B: 80, A: 255}

	return layout.Stack{}.Layout(gtx,
		layout.Expanded(func(gtx layout.Context) layout.Dimensions {
			rr := clip.UniformRRect(image.Rect(0, 0, gtx.Constraints.Max.X, gtx.Constraints.Min.Y), gtx.Dp(unit.Dp(4)))
			paint.FillShape(gtx.Ops, dropBg, rr.Op(gtx.Ops))
			return layout.Dimensions{Size: image.Pt(gtx.Constraints.Max.X, gtx.Constraints.Min.Y)}
		}),
		layout.Stacked(func(gtx layout.Context) layout.Dimensions {
			gtx.Constraints.Max.Y = maxH
			var children []layout.FlexChild
			for i, p := range peers {
				idx := i
				peer := p
				children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return ps.items[idx].Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						bg := dropBg
						if ps.items[idx].Hovered() {
							bg = hoverBg
						}
						return layout.Stack{}.Layout(gtx,
							layout.Expanded(func(gtx layout.Context) layout.Dimensions {
								rect := clip.Rect{Max: image.Pt(gtx.Constraints.Max.X, gtx.Constraints.Min.Y)}
								paint.FillShape(gtx.Ops, bg, rect.Op())
								return layout.Dimensions{Size: image.Pt(gtx.Constraints.Max.X, gtx.Constraints.Min.Y)}
							}),
							layout.Stacked(func(gtx layout.Context) layout.Dimensions {
								return layout.Inset{Top: unit.Dp(6), Bottom: unit.Dp(6), Left: unit.Dp(10), Right: unit.Dp(10)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
									return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
										layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
											lbl := material.Body2(th, peer.Name)
											lbl.Color = TextColor
											return lbl.Layout(gtx)
										}),
										layout.Rigid(func(gtx layout.Context) layout.Dimensions {
											mode := peer.Mode
											if mode == "" {
												mode = "relay"
											}
											lbl := material.Caption(th, mode)
											if mode == "direct" || mode == "p2p" {
												lbl.Color = SuccessColor
											} else {
												lbl.Color = DimColor
											}
											return lbl.Layout(gtx)
										}),
									)
								})
							}),
						)
					})
				}))
			}
			return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
		}),
	)
}
