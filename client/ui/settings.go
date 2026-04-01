package ui

import (
	"image"
	"image/color"
	"strings"

	"gioui.org/layout"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"
)

// publicSTUNServers is the built-in list of well-known STUN servers.
var publicSTUNServers = []string{
	"stun.cloudflare.com:3478",
	"stun.miwifi.com:3478",
	"stun.chat.bilibili.com:3478",
	"stun.l.google.com:19302",
	"stun.stunprotocol.org:3478",
}

// SettingsPanel manages access control settings.
type SettingsPanel struct {
	AllowForward widget.Bool
	LocalOnly    widget.Bool
	Autostart    widget.Bool
	AutoConnect  widget.Bool
	AutoLogin    widget.Bool

	// Auto login
	AutoLoginPassEditor widget.Editor
	AutoLoginSaveBtn    widget.Clickable
	AutoLoginUser       string
	AutoLoginMsg        string

	// STUN server state
	STUNToggles      map[string]*widget.Bool
	CustomSTUNEditor widget.Editor
	AddSTUNBtn       widget.Clickable
	customSTUNList   []string

	inited bool
}

func (s *SettingsPanel) init(a *App) {
	if s.inited {
		return
	}
	s.inited = true
	s.AllowForward.Value = true
	s.LocalOnly.Value = true
	s.Autostart.Value = GetAutostart()
	if cfg := LoadConfig(); cfg != nil {
		s.AutoConnect.Value = cfg.AutoConnect
	}

	// Auto login
	s.AutoLoginPassEditor.SingleLine = true
	s.AutoLoginPassEditor.Mask = '*'
	enabled, user := GetAutoLogin()
	s.AutoLogin.Value = enabled
	s.AutoLoginUser = user

	s.CustomSTUNEditor.SingleLine = true
	s.STUNToggles = make(map[string]*widget.Bool)
	for _, srv := range publicSTUNServers {
		b := &widget.Bool{}
		b.Value = true // enabled by default
		s.STUNToggles[srv] = b
	}

	// Load saved STUN config
	if cfg := LoadConfig(); cfg != nil {
		if len(cfg.STUNServers) > 0 {
			// Mark only saved servers as enabled
			for _, b := range s.STUNToggles {
				b.Value = false
			}
			for _, saved := range cfg.STUNServers {
				if b, ok := s.STUNToggles[saved]; ok {
					b.Value = true
				} else {
					// custom server
					s.customSTUNList = append(s.customSTUNList, saved)
				}
			}
		}
	}
}

// selectedSTUNServers returns the currently active STUN server list.
func (s *SettingsPanel) selectedSTUNServers() []string {
	var out []string
	for _, srv := range publicSTUNServers {
		if b, ok := s.STUNToggles[srv]; ok && b.Value {
			out = append(out, srv)
		}
	}
	out = append(out, s.customSTUNList...)
	return out
}

func (s *SettingsPanel) saveSTUNConfig() {
	cfg := LoadConfig()
	if cfg == nil {
		cfg = &SavedConfig{}
	}
	cfg.STUNServers = s.selectedSTUNServers()
	SaveConfig(cfg)
}

// Layout renders the settings panel.
func (s *SettingsPanel) Layout(gtx layout.Context, th *material.Theme, a *App) layout.Dimensions {
	s.init(a)

	if s.AllowForward.Update(gtx) {
		if a.Client != nil {
			a.Client.SetAllowForward(s.AllowForward.Value)
		}
	}
	if s.LocalOnly.Update(gtx) {
		if a.Client != nil {
			a.Client.SetLocalOnly(s.LocalOnly.Value)
		}
	}
	if s.Autostart.Update(gtx) {
		SetAutostart(s.Autostart.Value)
		if cfg := LoadConfig(); cfg != nil {
			cfg.Autostart = s.Autostart.Value
			SaveConfig(cfg)
		}
	}
	if s.AutoConnect.Update(gtx) {
		if cfg := LoadConfig(); cfg != nil {
			cfg.AutoConnect = s.AutoConnect.Value
			SaveConfig(cfg)
		}
	}
	if s.AutoLogin.Update(gtx) {
		if !s.AutoLogin.Value {
			// Disable auto login
			SetAutoLogin("", "")
			s.AutoLoginMsg = "Auto login disabled"
			s.AutoLoginUser = ""
		}
		// Enable requires password — handled by Save button
	}
	if s.AutoLoginSaveBtn.Clicked(gtx) {
		pw := strings.TrimSpace(s.AutoLoginPassEditor.Text())
		if pw == "" {
			s.AutoLoginMsg = "Enter your Windows password"
		} else {
			user := GetCurrentUsername()
			if err := SetAutoLogin(user, pw); err != nil {
				s.AutoLoginMsg = "Failed: " + err.Error()
			} else {
				s.AutoLogin.Value = true
				s.AutoLoginUser = user
				s.AutoLoginMsg = "Auto login enabled for " + user
				s.AutoLoginPassEditor.SetText("")
			}
		}
	}

	// Handle STUN toggle changes
	for _, b := range s.STUNToggles {
		if b.Update(gtx) {
			s.saveSTUNConfig()
		}
	}

	// Handle Add custom STUN button
	if s.AddSTUNBtn.Clicked(gtx) {
		addr := strings.TrimSpace(s.CustomSTUNEditor.Text())
		if addr != "" {
			// Avoid duplicates
			found := false
			for _, existing := range s.customSTUNList {
				if existing == addr {
					found = true
					break
				}
			}
			if !found {
				s.customSTUNList = append(s.customSTUNList, addr)
				s.saveSTUNConfig()
			}
			s.CustomSTUNEditor.SetText("")
		}
	}

	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return s.layoutSettingCard(gtx, th,
				"Allow Incoming Forwards",
				"When enabled, other peers can open tunnels to your local services.",
				&s.AllowForward,
			)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(12)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return s.layoutSettingCard(gtx, th,
					"Local Only Mode",
					"Only allow tunnels to localhost/127.0.0.1. Prevents LAN access.",
					&s.LocalOnly,
				)
			})
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(12)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return s.layoutSettingCard(gtx, th,
					"Start on Boot (Windows)",
					"Launch STUN Max as administrator when Windows starts. Uses Task Scheduler with highest privileges.",
					&s.Autostart,
				)
			})
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(12)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return s.layoutSettingCard(gtx, th,
					"Auto Connect",
					"Automatically connect to the last room on launch. If connection fails, stays on the login screen.",
					&s.AutoConnect,
				)
			})
		}),
		// Auto Login (Windows only)
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			if !AutoLoginSupported() {
				return layout.Dimensions{}
			}
			return layout.Inset{Top: unit.Dp(12)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return s.layoutAutoLoginCard(gtx, th)
			})
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(12)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return s.layoutSTUNCard(gtx, th)
			})
		}),
	)
}

func (s *SettingsPanel) layoutAutoLoginCard(gtx layout.Context, th *material.Theme) layout.Dimensions {
	return layout.Stack{}.Layout(gtx,
		layout.Expanded(func(gtx layout.Context) layout.Dimensions {
			rr := clip.UniformRRect(image.Rect(0, 0, gtx.Constraints.Max.X, gtx.Constraints.Min.Y), gtx.Dp(unit.Dp(8)))
			paint.FillShape(gtx.Ops, CardColor, rr.Op(gtx.Ops))
			return layout.Dimensions{Size: image.Pt(gtx.Constraints.Max.X, gtx.Constraints.Min.Y)}
		}),
		layout.Stacked(func(gtx layout.Context) layout.Dimensions {
			return layout.UniformInset(unit.Dp(16)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
					// Title + toggle
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return layout.Flex{Alignment: layout.Middle, Spacing: layout.SpaceBetween}.Layout(gtx,
							layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
								return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
									layout.Rigid(func(gtx layout.Context) layout.Dimensions {
										lbl := material.Body1(th, "Windows Auto Login")
										lbl.Color = TextColor
										return lbl.Layout(gtx)
									}),
									layout.Rigid(func(gtx layout.Context) layout.Dimensions {
										return layout.Inset{Top: unit.Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
											desc := "Skip the Windows login screen on boot. Enter your Windows password below to enable."
											if s.AutoLogin.Value {
												desc = "Auto login enabled for user: " + s.AutoLoginUser
											}
											lbl := material.Caption(th, desc)
											lbl.Color = DimColor
											return lbl.Layout(gtx)
										})
									}),
								)
							}),
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								return layout.Inset{Left: unit.Dp(16)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
									sw := material.Switch(th, &s.AutoLogin, "Auto Login")
									sw.Color.Enabled = AccentColor
									sw.Color.Disabled = DimColor
									return sw.Layout(gtx)
								})
							}),
						)
					}),
					// Password input + Save button (only when not enabled)
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						if s.AutoLogin.Value {
							return layout.Dimensions{}
						}
						return layout.Inset{Top: unit.Dp(10)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
								layout.Rigid(func(gtx layout.Context) layout.Dimensions {
									lbl := material.Body2(th, "Password: ")
									lbl.Color = DimColor
									return lbl.Layout(gtx)
								}),
								layout.Rigid(func(gtx layout.Context) layout.Dimensions {
									gtx.Constraints.Max.X = gtx.Dp(unit.Dp(200))
									return layout.Stack{}.Layout(gtx,
										layout.Expanded(func(gtx layout.Context) layout.Dimensions {
											rr := clip.UniformRRect(image.Rect(0, 0, gtx.Constraints.Max.X, gtx.Constraints.Min.Y), gtx.Dp(unit.Dp(4)))
											paint.FillShape(gtx.Ops, InputBg, rr.Op(gtx.Ops))
											return layout.Dimensions{Size: image.Pt(gtx.Constraints.Max.X, gtx.Constraints.Min.Y)}
										}),
										layout.Stacked(func(gtx layout.Context) layout.Dimensions {
											return layout.UniformInset(unit.Dp(8)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
												ed := material.Editor(th, &s.AutoLoginPassEditor, "Windows password")
												ed.Color = TextColor
												ed.HintColor = DimColor
												return ed.Layout(gtx)
											})
										}),
									)
								}),
								layout.Rigid(func(gtx layout.Context) layout.Dimensions {
									return layout.Inset{Left: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
										btn := material.Button(th, &s.AutoLoginSaveBtn, "Enable")
										btn.Background = SuccessColor
										btn.Color = color.NRGBA{A: 255}
										btn.CornerRadius = unit.Dp(4)
										btn.TextSize = unit.Sp(13)
										btn.Inset = layout.Inset{Top: unit.Dp(6), Bottom: unit.Dp(6), Left: unit.Dp(14), Right: unit.Dp(14)}
										return btn.Layout(gtx)
									})
								}),
							)
						})
					}),
					// Status message
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						if s.AutoLoginMsg == "" {
							return layout.Dimensions{}
						}
						return layout.Inset{Top: unit.Dp(6)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							lbl := material.Caption(th, s.AutoLoginMsg)
							lbl.Color = SuccessColor
							return lbl.Layout(gtx)
						})
					}),
				)
			})
		}),
	)
}

func (s *SettingsPanel) layoutSTUNCard(gtx layout.Context, th *material.Theme) layout.Dimensions {
	return layout.Stack{}.Layout(gtx,
		layout.Expanded(func(gtx layout.Context) layout.Dimensions {
			rr := clip.UniformRRect(image.Rect(0, 0, gtx.Constraints.Max.X, gtx.Constraints.Min.Y), gtx.Dp(unit.Dp(8)))
			paint.FillShape(gtx.Ops, CardColor, rr.Op(gtx.Ops))
			return layout.Dimensions{Size: image.Pt(gtx.Constraints.Max.X, gtx.Constraints.Min.Y)}
		}),
		layout.Stacked(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(16), Bottom: unit.Dp(16), Left: unit.Dp(16), Right: unit.Dp(16)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
					// Section title
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						lbl := material.Body1(th, "STUN Servers")
						lbl.Color = AccentColor
						return lbl.Layout(gtx)
					}),
					// Description
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return layout.Inset{Top: unit.Dp(4), Bottom: unit.Dp(12)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							lbl := material.Caption(th, "Select STUN servers for NAT traversal. Add your own server if needed.")
							lbl.Color = DimColor
							return lbl.Layout(gtx)
						})
					}),
					// Public server toggles
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return s.layoutPublicSTUNList(gtx, th)
					}),
					// Custom servers list
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return s.layoutCustomSTUNList(gtx, th)
					}),
					// Add custom server row
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return layout.Inset{Top: unit.Dp(10)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							return s.layoutAddSTUNRow(gtx, th)
						})
					}),
				)
			})
		}),
	)
}

func (s *SettingsPanel) layoutPublicSTUNList(gtx layout.Context, th *material.Theme) layout.Dimensions {
	children := make([]layout.FlexChild, 0, len(publicSTUNServers))
	for i, srv := range publicSTUNServers {
		srv := srv // capture
		b := s.STUNToggles[srv]
		top := unit.Dp(0)
		if i > 0 {
			top = unit.Dp(8)
		}
		children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: top}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle, Spacing: layout.SpaceBetween}.Layout(gtx,
					layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
						lbl := material.Body2(th, srv)
						lbl.Color = TextColor
						return lbl.Layout(gtx)
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return layout.Inset{Left: unit.Dp(12)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							sw := material.Switch(th, b, srv)
							sw.Color.Enabled = AccentColor
							sw.Color.Disabled = DimColor
							return sw.Layout(gtx)
						})
					}),
				)
			})
		}))
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
}

func (s *SettingsPanel) layoutCustomSTUNList(gtx layout.Context, th *material.Theme) layout.Dimensions {
	if len(s.customSTUNList) == 0 {
		return layout.Dimensions{}
	}
	children := make([]layout.FlexChild, 0, len(s.customSTUNList))
	for _, srv := range s.customSTUNList {
		srv := srv
		children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				lbl := material.Body2(th, "• "+srv)
				lbl.Color = SuccessColor
				return lbl.Layout(gtx)
			})
		}))
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
}

func (s *SettingsPanel) layoutAddSTUNRow(gtx layout.Context, th *material.Theme) layout.Dimensions {
	return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			return layout.Stack{}.Layout(gtx,
				layout.Expanded(func(gtx layout.Context) layout.Dimensions {
					rr := clip.UniformRRect(image.Rect(0, 0, gtx.Constraints.Max.X, gtx.Constraints.Min.Y), gtx.Dp(unit.Dp(6)))
					paint.FillShape(gtx.Ops, InputBg, rr.Op(gtx.Ops))
					return layout.Dimensions{Size: image.Pt(gtx.Constraints.Max.X, gtx.Constraints.Min.Y)}
				}),
				layout.Stacked(func(gtx layout.Context) layout.Dimensions {
					return layout.Inset{Top: unit.Dp(8), Bottom: unit.Dp(8), Left: unit.Dp(10), Right: unit.Dp(10)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						ed := material.Editor(th, &s.CustomSTUNEditor, "host:port")
						ed.Color = TextColor
						ed.HintColor = DimColor
						return ed.Layout(gtx)
					})
				}),
			)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Left: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				btn := material.Button(th, &s.AddSTUNBtn, "Add")
				btn.Background = AccentColor
				btn.Color = CardColor
				btn.CornerRadius = unit.Dp(6)
				return btn.Layout(gtx)
			})
		}),
	)
}

func (s *SettingsPanel) layoutSettingCard(gtx layout.Context, th *material.Theme, title, desc string, toggle *widget.Bool) layout.Dimensions {
	return layout.Stack{}.Layout(gtx,
		layout.Expanded(func(gtx layout.Context) layout.Dimensions {
			rr := clip.UniformRRect(image.Rect(0, 0, gtx.Constraints.Max.X, gtx.Constraints.Min.Y), gtx.Dp(unit.Dp(8)))
			paint.FillShape(gtx.Ops, CardColor, rr.Op(gtx.Ops))
			return layout.Dimensions{Size: image.Pt(gtx.Constraints.Max.X, gtx.Constraints.Min.Y)}
		}),
		layout.Stacked(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(16), Bottom: unit.Dp(16), Left: unit.Dp(16), Right: unit.Dp(16)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle, Spacing: layout.SpaceBetween}.Layout(gtx,
					layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
						return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								lbl := material.Body1(th, title)
								lbl.Color = TextColor
								return lbl.Layout(gtx)
							}),
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								return layout.Inset{Top: unit.Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
									lbl := material.Caption(th, desc)
									lbl.Color = DimColor
									return lbl.Layout(gtx)
								})
							}),
						)
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return layout.Inset{Left: unit.Dp(16)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							sw := material.Switch(th, toggle, title)
							sw.Color.Enabled = AccentColor
							sw.Color.Disabled = DimColor
							return sw.Layout(gtx)
						})
					}),
				)
			})
		}),
	)
}
