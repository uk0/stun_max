package ui

import (
	"fmt"
	"image"
	"image/color"
	"os"
	"path/filepath"
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

// FilesPanel manages file transfer UI.
type FilesPanel struct {
	PathEditor widget.Editor
	PeerSel    *PeerSelector
	SendBtn    widget.Clickable
	List       widget.List
	Error      string
	inited     bool

	// Pending incoming offers
	pendingOffers []core.FileOfferEvent
	acceptBtns    map[string]*widget.Clickable
	rejectBtns    map[string]*widget.Clickable
	cancelBtns    map[string]*widget.Clickable

	// Transfer progress cache
	progressCache map[string]core.FileProgressEvent

	mu sync.Mutex
}

// PLACEHOLDER_INIT_AND_HANDLERS

func (f *FilesPanel) init() {
	if f.inited {
		return
	}
	f.inited = true
	f.PathEditor.SingleLine = true
	f.PeerSel = NewPeerSelector("Select peer")
	f.List.Axis = layout.Vertical
	f.acceptBtns = make(map[string]*widget.Clickable)
	f.rejectBtns = make(map[string]*widget.Clickable)
	f.cancelBtns = make(map[string]*widget.Clickable)
	f.progressCache = make(map[string]core.FileProgressEvent)
}

func (f *FilesPanel) handleOffer(offer core.FileOfferEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pendingOffers = append(f.pendingOffers, offer)
	f.acceptBtns[offer.TransferID] = new(widget.Clickable)
	f.rejectBtns[offer.TransferID] = new(widget.Clickable)
}

func (f *FilesPanel) handleProgress(p core.FileProgressEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.progressCache[p.TransferID] = p
}

func (f *FilesPanel) handleComplete(c core.FileCompleteEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Remove from pending offers
	for i, o := range f.pendingOffers {
		if o.TransferID == c.TransferID {
			f.pendingOffers = append(f.pendingOffers[:i], f.pendingOffers[i+1:]...)
			break
		}
	}
	delete(f.progressCache, c.TransferID)
}

func (f *FilesPanel) handleError(e core.FileErrorEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, o := range f.pendingOffers {
		if o.TransferID == e.TransferID {
			f.pendingOffers = append(f.pendingOffers[:i], f.pendingOffers[i+1:]...)
			break
		}
	}
	delete(f.progressCache, e.TransferID)
}

func defaultDownloadDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Downloads", "StunMax")
}

// PLACEHOLDER_LAYOUT

// Layout renders the files panel.
func (f *FilesPanel) Layout(gtx layout.Context, th *material.Theme, a *App) layout.Dimensions {
	f.init()

	// Handle send button
	if f.SendBtn.Clicked(gtx) && a.Client != nil {
		peer := strings.TrimSpace(f.PeerSel.Text())
		path := strings.TrimSpace(f.PathEditor.Text())
		if peer == "" || path == "" {
			f.Error = "Enter both peer and file path"
		} else {
			f.Error = ""
			go func() {
				_, err := a.Client.SendFile(peer, path)
				if err != nil {
					f.mu.Lock()
					f.Error = err.Error()
					f.mu.Unlock()
					a.Window.Invalidate()
				}
			}()
		}
	}

	// Handle accept/reject buttons
	f.mu.Lock()
	offers := make([]core.FileOfferEvent, len(f.pendingOffers))
	copy(offers, f.pendingOffers)
	f.mu.Unlock()

	for _, offer := range offers {
		if btn, ok := f.acceptBtns[offer.TransferID]; ok && btn.Clicked(gtx) {
			tid := offer.TransferID
			fileName := offer.FileName
			go func() {
				savePath := filepath.Join(defaultDownloadDir(), fileName)
				if err := a.Client.AcceptFile(tid, savePath); err != nil {
					f.mu.Lock()
					f.Error = err.Error()
					f.mu.Unlock()
					a.Window.Invalidate()
				}
			}()
		}
		if btn, ok := f.rejectBtns[offer.TransferID]; ok && btn.Clicked(gtx) {
			tid := offer.TransferID
			go func() {
				a.Client.RejectFile(tid)
			}()
			f.mu.Lock()
			for i, o := range f.pendingOffers {
				if o.TransferID == tid {
					f.pendingOffers = append(f.pendingOffers[:i], f.pendingOffers[i+1:]...)
					break
				}
			}
			f.mu.Unlock()
		}
	}

	// Handle cancel buttons for active transfers
	var transfers []core.FileTransferInfo
	if a.Client != nil {
		transfers = a.Client.FileTransfers()
	}
	for _, t := range transfers {
		if t.Status == "active" || t.Status == "pending" {
			if _, ok := f.cancelBtns[t.TransferID]; !ok {
				f.cancelBtns[t.TransferID] = new(widget.Clickable)
			}
			if btn := f.cancelBtns[t.TransferID]; btn.Clicked(gtx) {
				tid := t.TransferID
				go func() {
					a.Client.CancelFileTransfer(tid)
				}()
			}
		}
	}

	f.mu.Lock()
	errMsg := f.Error
	f.mu.Unlock()

	return material.List(th, &f.List).Layout(gtx, 1, func(gtx layout.Context, _ int) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			// Send form card
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return f.layoutSendForm(gtx, th, a, errMsg)
			}),
			// Pending offers
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return f.layoutPendingOffers(gtx, th, offers)
			}),
			// Active transfers
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return f.layoutTransfers(gtx, th, transfers)
			}),
		)
	})
}

// PLACEHOLDER_SEND_FORM

func (f *FilesPanel) layoutSendForm(gtx layout.Context, th *material.Theme, a *App, errMsg string) layout.Dimensions {
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
						lbl := material.Body1(th, "Send File")
						lbl.Color = TextColor
						return lbl.Layout(gtx)
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return layout.Inset{Top: unit.Dp(12)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
								layout.Flexed(0.3, func(gtx layout.Context) layout.Dimensions {
									return f.PeerSel.Layout(gtx, th, a)
								}),
								layout.Rigid(func(gtx layout.Context) layout.Dimensions {
									return layout.Spacer{Width: unit.Dp(8)}.Layout(gtx)
								}),
								layout.Flexed(0.5, func(gtx layout.Context) layout.Dimensions {
									return layoutInputField(gtx, th, &f.PathEditor, "File path")
								}),
								layout.Rigid(func(gtx layout.Context) layout.Dimensions {
									return layout.Spacer{Width: unit.Dp(8)}.Layout(gtx)
								}),
								layout.Rigid(func(gtx layout.Context) layout.Dimensions {
									btn := material.Button(th, &f.SendBtn, "Send")
									btn.Background = AccentColor
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
						if errMsg == "" {
							return layout.Dimensions{}
						}
						return layout.Inset{Top: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							lbl := material.Caption(th, errMsg)
							lbl.Color = ErrorColor
							return lbl.Layout(gtx)
						})
					}),
					// Download dir info
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return layout.Inset{Top: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							lbl := material.Caption(th, "Downloads save to: "+defaultDownloadDir())
							lbl.Color = DimColor
							return lbl.Layout(gtx)
						})
					}),
				)
			})
		}),
	)
}

// PLACEHOLDER_PENDING_OFFERS

func (f *FilesPanel) layoutPendingOffers(gtx layout.Context, th *material.Theme, offers []core.FileOfferEvent) layout.Dimensions {
	if len(offers) == 0 {
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
				return layout.Inset{Top: unit.Dp(12), Bottom: unit.Dp(12), Left: unit.Dp(16), Right: unit.Dp(16)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					children := []layout.FlexChild{
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							lbl := material.Body2(th, "Incoming Files")
							lbl.Color = WarningColor
							return lbl.Layout(gtx)
						}),
					}
					for _, offer := range offers {
						o := offer
						children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							return f.layoutOfferRow(gtx, th, o)
						}))
					}
					return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
				})
			}),
		)
	})
}

func (f *FilesPanel) layoutOfferRow(gtx layout.Context, th *material.Theme, offer core.FileOfferEvent) layout.Dimensions {
	return layout.Inset{Top: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				txt := fmt.Sprintf("%s from %s (%s)", offer.FileName, offer.PeerName, fmtSize(offer.FileSize))
				lbl := material.Body2(th, txt)
				lbl.Color = TextColor
				return lbl.Layout(gtx)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				if btn, ok := f.acceptBtns[offer.TransferID]; ok {
					b := material.Button(th, btn, "Accept")
					b.Background = SuccessColor
					b.Color = color.NRGBA{A: 255}
					b.CornerRadius = unit.Dp(4)
					b.TextSize = unit.Sp(12)
					b.Inset = layout.Inset{Top: unit.Dp(4), Bottom: unit.Dp(4), Left: unit.Dp(10), Right: unit.Dp(10)}
					return b.Layout(gtx)
				}
				return layout.Dimensions{}
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Spacer{Width: unit.Dp(4)}.Layout(gtx)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				if btn, ok := f.rejectBtns[offer.TransferID]; ok {
					b := material.Button(th, btn, "Reject")
					b.Background = ErrorColor
					b.Color = color.NRGBA{R: 255, G: 255, B: 255, A: 255}
					b.CornerRadius = unit.Dp(4)
					b.TextSize = unit.Sp(12)
					b.Inset = layout.Inset{Top: unit.Dp(4), Bottom: unit.Dp(4), Left: unit.Dp(10), Right: unit.Dp(10)}
					return b.Layout(gtx)
				}
				return layout.Dimensions{}
			}),
		)
	})
}

// PLACEHOLDER_TRANSFERS_LAYOUT

func (f *FilesPanel) layoutTransfers(gtx layout.Context, th *material.Theme, transfers []core.FileTransferInfo) layout.Dimensions {
	if len(transfers) == 0 {
		return layout.Inset{Top: unit.Dp(16)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			lbl := material.Body2(th, "No active file transfers")
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
					children := []layout.FlexChild{
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							lbl := material.Body2(th, "Transfers")
							lbl.Color = TextColor
							return lbl.Layout(gtx)
						}),
					}
					for _, t := range transfers {
						tr := t
						children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							return f.layoutTransferRow(gtx, th, tr)
						}))
					}
					return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
				})
			}),
		)
	})
}

func (f *FilesPanel) layoutTransferRow(gtx layout.Context, th *material.Theme, t core.FileTransferInfo) layout.Dimensions {
	return layout.Inset{Top: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			// File info line
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						dir := "SEND"
						dirColor := AccentColor
						if t.Direction == "receive" {
							dir = "RECV"
							dirColor = SuccessColor
						}
						lbl := material.Caption(th, dir)
						lbl.Color = dirColor
						return lbl.Layout(gtx)
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return layout.Spacer{Width: unit.Dp(8)}.Layout(gtx)
					}),
					layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
						txt := fmt.Sprintf("%s  (%s)  %s", t.FileName, fmtSize(t.FileSize), t.PeerName)
						lbl := material.Body2(th, txt)
						lbl.Color = TextColor
						return lbl.Layout(gtx)
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						statusColor := DimColor
						switch t.Status {
						case "active":
							statusColor = AccentColor
						case "complete":
							statusColor = SuccessColor
						case "error":
							statusColor = ErrorColor
						case "pending":
							statusColor = WarningColor
						}
						lbl := material.Caption(th, t.Status)
						lbl.Color = statusColor
						return lbl.Layout(gtx)
					}),
				)
			}),
			// Progress bar (only for active transfers)
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				if t.Status != "active" {
					return layout.Dimensions{}
				}
				return layout.Inset{Top: unit.Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							bar := material.ProgressBar(th, float32(t.Progress))
							bar.Color = AccentColor
							bar.TrackColor = InputBg
							return bar.Layout(gtx)
						}),
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							return layout.Inset{Top: unit.Dp(2)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
								txt := fmt.Sprintf("%.0f%%  %s / %s", t.Progress*100, fmtSize(t.BytesDone), fmtSize(t.FileSize))
								if t.Speed > 0 {
									txt += fmt.Sprintf("  %s/s", fmtSize(int64(t.Speed)))
								}
								lbl := material.Caption(th, txt)
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

func fmtSize(b int64) string {
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
