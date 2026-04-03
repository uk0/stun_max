package ui

import (
	"fmt"
	"image"
	"image/color"
	"strings"
	"sync"

	"gioui.org/layout"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"stun_max/client/core"
)

// SpeedTestPanel manages speed test UI.
type SpeedTestPanel struct {
	PeerSel    *PeerSelector
	SizeBtns   [6]widget.Clickable
	SizeMB     int
	RunBtn     widget.Clickable
	Running    bool
	Progress   float64
	Phase      string
	Transport  string // actual transport being used
	Result     *core.SpeedTestResult
	Error      string
	List       widget.List
	mu         sync.Mutex
	inited     bool
}

var speedTestSizes = [6]int{1, 5, 10, 25, 50, 100}

func (s *SpeedTestPanel) init() {
	if s.inited {
		return
	}
	s.inited = true
	s.PeerSel = NewPeerSelector("Select peer")
	s.SizeMB = 10 // default
	s.List.Axis = layout.Vertical
}

func (s *SpeedTestPanel) handleResult(r core.SpeedTestResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Result = &r
	s.Running = false
	s.Progress = 0
	s.Phase = ""
}

func (s *SpeedTestPanel) handleProgress(p core.SpeedTestProgressEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Progress = p.Progress
	s.Phase = p.Phase
	if p.Transport != "" {
		s.Transport = p.Transport
	}
}

func (s *SpeedTestPanel) Layout(gtx layout.Context, th *material.Theme, a *App) layout.Dimensions {
	s.init()

	// Handle size button clicks
	for i := range s.SizeBtns {
		if s.SizeBtns[i].Clicked(gtx) {
			s.SizeMB = speedTestSizes[i]
		}
	}

	// Handle run button
	if s.RunBtn.Clicked(gtx) && !s.Running && a.Client != nil {
		peer := s.PeerSel.Text()
		if peer == "" {
			s.Error = "Select a peer"
		} else {
			s.Error = ""
			s.Result = nil
			s.Running = true
			s.Progress = 0
			s.Phase = "starting"
			s.Transport = ""
			sizeMB := s.SizeMB
			go func() {
				_, err := a.Client.StartSpeedTest(peer, sizeMB, "p2p")
				if err != nil {
					s.mu.Lock()
					s.Error = err.Error()
					s.Running = false
					s.mu.Unlock()
					a.Window.Invalidate()
				}
			}()
		}
	}

	s.mu.Lock()
	running := s.Running
	progress := s.Progress
	phase := s.Phase
	transport := s.Transport
	result := s.Result
	errMsg := s.Error
	s.mu.Unlock()

	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		// Form card
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
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
								lbl := material.Body1(th, "Speed Test")
								lbl.Color = TextColor
								return lbl.Layout(gtx)
							}),
							// Peer selector + size buttons + run buttons
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								return layout.Inset{Top: unit.Dp(12)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
									return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
										layout.Flexed(0.25, func(gtx layout.Context) layout.Dimensions {
											return s.PeerSel.Layout(gtx, th, a)
										}),
										layout.Rigid(layout.Spacer{Width: unit.Dp(8)}.Layout),
										layout.Flexed(0.45, func(gtx layout.Context) layout.Dimensions {
											return s.layoutSizeSelector(gtx, th)
										}),
										layout.Rigid(layout.Spacer{Width: unit.Dp(8)}.Layout),
										layout.Rigid(func(gtx layout.Context) layout.Dimensions {
											label := fmt.Sprintf("P2P %dMB", s.SizeMB)
											btn := material.Button(th, &s.RunBtn, label)
											btn.Background = SuccessColor
											btn.Color = color.NRGBA{A: 255}
											btn.CornerRadius = unit.Dp(4)
											btn.Inset = layout.Inset{Top: unit.Dp(8), Bottom: unit.Dp(8), Left: unit.Dp(16), Right: unit.Dp(16)}
											if running {
												btn.Background = DimColor
												btn.Text = "Running..."
											}
											return btn.Layout(gtx)
										}),
									)
								})
							}),
							// Error
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								if errMsg == "" {
									return layout.Dimensions{}
								}
								return layout.Inset{Top: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
									lbl := material.Caption(th, errMsg)
									lbl.Color = ErrorColor
									return lbl.Layout(gtx)
								})
							}),
						)
					})
				}),
			)
		}),
		// Progress bar
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			if !running {
				return layout.Dimensions{}
			}
			return layout.Inset{Top: unit.Dp(16)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						transportLabel := ""
						if transport != "" {
							transportLabel = fmt.Sprintf(" [%s]", strings.ToUpper(transport))
						}
						txt := fmt.Sprintf("%s (%.0f%%)%s", phase, progress*100, transportLabel)
						lbl := material.Body2(th, txt)
						lbl.Color = AccentColor
						return lbl.Layout(gtx)
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return layout.Inset{Top: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							bar := material.ProgressBar(th, float32(progress))
							bar.Color = AccentColor
							bar.TrackColor = InputBg
							return bar.Layout(gtx)
						})
					}),
				)
			})
		}),
		// Results
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			if result == nil {
				return layout.Dimensions{}
			}
			return layout.Inset{Top: unit.Dp(16)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
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
									return layout.Flex{Axis: layout.Horizontal, Spacing: layout.SpaceEvenly}.Layout(gtx,
										layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
											return layoutSpeedResult(gtx, th, "Upload", result.UploadMbps)
										}),
										layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
											return layoutSpeedResult(gtx, th, "Download", result.DownloadMbps)
										}),
									)
								}),
								layout.Rigid(func(gtx layout.Context) layout.Dimensions {
									if result.Transport == "" {
										return layout.Dimensions{}
									}
									return layout.Inset{Top: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
										lbl := material.Caption(th, "via "+strings.ToUpper(result.Transport))
										if result.Transport == "p2p" {
											lbl.Color = SuccessColor
										} else {
											lbl.Color = DimColor
										}
										return layout.Center.Layout(gtx, lbl.Layout)
									})
								}),
							)
						})
					}),
				)
			})
		}),
	)
}

func (s *SpeedTestPanel) layoutSizeSelector(gtx layout.Context, th *material.Theme) layout.Dimensions {
	var children []layout.FlexChild
	for i, size := range speedTestSizes {
		idx := i
		sz := size
		children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			label := fmt.Sprintf("%dMB", sz)
			btn := material.Button(th, &s.SizeBtns[idx], label)
			btn.CornerRadius = unit.Dp(4)
			btn.Inset = layout.Inset{Top: unit.Dp(6), Bottom: unit.Dp(6), Left: unit.Dp(8), Right: unit.Dp(8)}
			btn.TextSize = unit.Sp(11)
			if sz == s.SizeMB {
				btn.Background = AccentColor
				btn.Color = color.NRGBA{A: 255}
			} else {
				btn.Background = InputBg
				btn.Color = DimColor
			}
			return layout.Inset{Right: unit.Dp(4)}.Layout(gtx, btn.Layout)
		}))
	}
	return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx, children...)
}

func layoutSpeedResult(gtx layout.Context, th *material.Theme, label string, mbps float64) layout.Dimensions {
	return layout.Flex{Axis: layout.Vertical, Alignment: layout.Middle}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			lbl := material.Caption(th, label)
			lbl.Color = DimColor
			return lbl.Layout(gtx)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			txt := fmt.Sprintf("%.1f Mbps", mbps)
			lbl := material.H6(th, txt)
			lbl.Color = AccentColor
			return lbl.Layout(gtx)
		}),
	)
}
