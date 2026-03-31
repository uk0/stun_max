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

// LogsPanel displays the log viewer.
type LogsPanel struct {
	List widget.List
}

// Layout renders the logs panel.
func (l *LogsPanel) Layout(gtx layout.Context, th *material.Theme, a *App) layout.Dimensions {
	l.List.Axis = layout.Vertical
	l.List.ScrollToEnd = true

	a.mu.Lock()
	logs := make([]LogEntry, len(a.Logs))
	copy(logs, a.Logs)
	a.mu.Unlock()

	if len(logs) == 0 {
		return layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			lbl := material.Body1(th, "No log entries")
			lbl.Color = DimColor
			return lbl.Layout(gtx)
		})
	}

	// Background card
	return layout.Stack{}.Layout(gtx,
		layout.Expanded(func(gtx layout.Context) layout.Dimensions {
			rr := clip.UniformRRect(image.Rect(0, 0, gtx.Constraints.Max.X, gtx.Constraints.Max.Y), gtx.Dp(unit.Dp(8)))
			paint.FillShape(gtx.Ops, CardColor, rr.Op(gtx.Ops))
			return layout.Dimensions{Size: gtx.Constraints.Max}
		}),
		layout.Stacked(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(8), Bottom: unit.Dp(8), Left: unit.Dp(12), Right: unit.Dp(12)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				list := material.List(th, &l.List)
				return list.Layout(gtx, len(logs), func(gtx layout.Context, i int) layout.Dimensions {
					return layoutLogEntry(gtx, th, logs[i])
				})
			})
		}),
	)
}

func layoutLogEntry(gtx layout.Context, th *material.Theme, entry LogEntry) layout.Dimensions {
	return layout.Inset{Top: unit.Dp(2), Bottom: unit.Dp(2)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Baseline}.Layout(gtx,
			// Timestamp
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				lbl := material.Caption(th, entry.Time)
				lbl.Color = DimColor
				return lbl.Layout(gtx)
			}),
			// Level badge
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Left: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					var c color.NRGBA
					switch entry.Level {
					case "error":
						c = ErrorColor
					case "warn":
						c = WarningColor
					default:
						c = AccentColor
					}
					lbl := material.Caption(th, "["+entry.Level+"]")
					lbl.Color = c
					return lbl.Layout(gtx)
				})
			}),
			// Message
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Left: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					lbl := material.Caption(th, entry.Message)
					lbl.Color = TextColor
					return lbl.Layout(gtx)
				})
			}),
		)
	})
}
