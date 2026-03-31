package ui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
)

// SavedConfig persists connection and forward settings across restarts.
type SavedConfig struct {
	ServerURL string            `json:"server_url"`
	Room      string            `json:"room"`
	Password  string            `json:"password"`
	Name      string            `json:"name"`
	STUNServers []string        `json:"stun_servers,omitempty"`
	NoSTUN    bool              `json:"no_stun,omitempty"`
	Autostart bool              `json:"autostart,omitempty"`
	Forwards  []SavedForward    `json:"forwards,omitempty"`
}

// SavedForward persists a forward rule.
type SavedForward struct {
	PeerName   string `json:"peer"`       // peer name or ID prefix
	RemoteHost string `json:"remote_host"`
	RemotePort int    `json:"remote_port"`
	LocalPort  int    `json:"local_port"`
	Enabled    bool   `json:"enabled"`
}

func configDir() string {
	if runtime.GOOS == "windows" {
		appdata := os.Getenv("APPDATA")
		if appdata != "" {
			return filepath.Join(appdata, "StunMax")
		}
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".stun_max")
}

func configPath() string {
	return filepath.Join(configDir(), "config.json")
}

// LoadConfig reads saved config from disk.
func LoadConfig() *SavedConfig {
	data, err := os.ReadFile(configPath())
	if err != nil {
		return nil
	}
	var cfg SavedConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil
	}
	return &cfg
}

// SaveConfig writes config to disk.
func SaveConfig(cfg *SavedConfig) error {
	dir := configDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(configPath(), data, 0600)
}
