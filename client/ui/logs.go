package ui

import (
	"fmt"
	"image"
	"strings"

	"gioui.org/layout"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"
)

// LogsPanel displays the log viewer with selectable/copyable text.
type LogsPanel struct {
	List      widget.List
	Editor    widget.Editor
	lastCount int
}

// Layout renders the logs panel.
func (l *LogsPanel) Layout(gtx layout.Context, th *material.Theme, a *App) layout.Dimensions {
	l.List.Axis = layout.Vertical

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

	// Rebuild editor text when new logs arrive
	if len(logs) != l.lastCount {
		l.lastCount = len(logs)
		var sb strings.Builder
		for _, entry := range logs {
			fmt.Fprintf(&sb, "%s  [%s]  %s\n", entry.Time, entry.Level, entry.Message)
		}
		l.Editor.SetText(sb.String())
		// Move cursor to end
		l.Editor.SetCaret(len(l.Editor.Text()), len(l.Editor.Text()))
	}

	l.Editor.ReadOnly = true

	return layout.Stack{}.Layout(gtx,
		layout.Expanded(func(gtx layout.Context) layout.Dimensions {
			rr := clip.UniformRRect(image.Rect(0, 0, gtx.Constraints.Max.X, gtx.Constraints.Max.Y), gtx.Dp(unit.Dp(8)))
			paint.FillShape(gtx.Ops, CardColor, rr.Op(gtx.Ops))
			return layout.Dimensions{Size: gtx.Constraints.Max}
		}),
		layout.Stacked(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(8), Bottom: unit.Dp(8), Left: unit.Dp(12), Right: unit.Dp(12)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				ed := material.Editor(th, &l.Editor, "")
				ed.Color = TextColor
				ed.HintColor = DimColor
				ed.TextSize = unit.Sp(12)
				return ed.Layout(gtx)
			})
		}),
	)
}
