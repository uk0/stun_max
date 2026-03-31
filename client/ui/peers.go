package ui

import (
	"image"
	"image/color"

	"gioui.org/layout"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"
)

// PeersPanel displays the list of peers in the room.
type PeersPanel struct {
	List widget.List
}

// Layout renders the peers panel.
func (p *PeersPanel) Layout(gtx layout.Context, th *material.Theme, a *App) layout.Dimensions {
	p.List.Axis = layout.Vertical

	a.mu.Lock()
	peers := make([]peerRow, 0, len(a.Peers))
	for _, peer := range a.Peers {
		mode := "-"
		if a.Client != nil {
			if peer.ID == a.Client.MyID {
				mode = "YOU"
			} else {
				mode = a.Client.PeerMode(peer.ID)
			}
		}
		name := peer.Name
		if name == "" {
			name = shortID(peer.ID)
		}
		peers = append(peers, peerRow{
			ID:       peer.ID,
			Name:     name,
			Status:   peer.Status,
			Mode:     mode,
			Endpoint: peer.Endpoint,
		})
	}
	a.mu.Unlock()

	if len(peers) == 0 {
		return layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			lbl := material.Body1(th, "No peers in room")
			lbl.Color = DimColor
			return lbl.Layout(gtx)
		})
	}

	list := material.List(th, &p.List)
	return list.Layout(gtx, len(peers), func(gtx layout.Context, i int) layout.Dimensions {
		return layout.Inset{Bottom: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return layoutPeerCard(gtx, th, peers[i])
		})
	})
}

type peerRow struct {
	ID       string
	Name     string
	Status   string
	Mode     string
	Endpoint string
}

func layoutPeerCard(gtx layout.Context, th *material.Theme, peer peerRow) layout.Dimensions {
	return layout.Stack{}.Layout(gtx,
		layout.Expanded(func(gtx layout.Context) layout.Dimensions {
			rr := clip.UniformRRect(image.Rect(0, 0, gtx.Constraints.Max.X, gtx.Constraints.Min.Y), gtx.Dp(unit.Dp(8)))
			paint.FillShape(gtx.Ops, CardColor, rr.Op(gtx.Ops))
			return layout.Dimensions{Size: image.Pt(gtx.Constraints.Max.X, gtx.Constraints.Min.Y)}
		}),
		layout.Stacked(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(12), Bottom: unit.Dp(12), Left: unit.Dp(16), Right: unit.Dp(16)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle, Spacing: layout.SpaceBetween}.Layout(gtx,
					layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
						return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								lbl := material.Body1(th, peer.Name)
								lbl.Color = TextColor
								return lbl.Layout(gtx)
							}),
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								id := peer.ID
								if len(id) > 12 {
									id = id[:12] + "..."
								}
								ep := id
								if peer.Endpoint != "" {
									ep = id + " | " + peer.Endpoint
								}
								lbl := material.Caption(th, ep)
								lbl.Color = DimColor
								return lbl.Layout(gtx)
							}),
						)
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return layoutBadge(gtx, th, peer.Mode)
					}),
				)
			})
		}),
	)
}

func layoutBadge(gtx layout.Context, th *material.Theme, mode string) layout.Dimensions {
	var bg color.NRGBA
	label := mode
	switch mode {
	case "direct", "P2P":
		bg = SuccessColor
		label = "P2P"
	case "relay", "RELAY":
		bg = WarningColor
		label = "RELAY"
	case "YOU":
		bg = AccentColor
		label = "YOU"
	default:
		bg = DimColor
		label = "..."
	}

	return layout.Stack{Alignment: layout.Center}.Layout(gtx,
		layout.Expanded(func(gtx layout.Context) layout.Dimensions {
			rr := clip.UniformRRect(image.Rect(0, 0, gtx.Constraints.Min.X, gtx.Constraints.Min.Y), gtx.Dp(unit.Dp(4)))
			paint.FillShape(gtx.Ops, bg, rr.Op(gtx.Ops))
			return layout.Dimensions{Size: image.Pt(gtx.Constraints.Min.X, gtx.Constraints.Min.Y)}
		}),
		layout.Stacked(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(4), Bottom: unit.Dp(4), Left: unit.Dp(10), Right: unit.Dp(10)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				lbl := material.Caption(th, label)
				lbl.Color = color.NRGBA{A: 255}
				return lbl.Layout(gtx)
			})
		}),
	)
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
