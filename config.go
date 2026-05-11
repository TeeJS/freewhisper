// config.go: load freewhisper's runtime settings from config.json.
//
// We keep config tiny on purpose: just the things that vary by deployment.
// Hotkey choice, audio sample rate, and other code-shaped decisions stay
// hardcoded — they're more "tuning constants" than "knobs the user turns."
//
// File location: next to the .exe. That matches CLAUDE.md's repo layout
// and means a user only ever has to manage two files (freewhisper.exe and
// config.json) regardless of where they put them.
//
// If config.json is missing, we fall back to compile-time defaults so the
// app still runs (and complains loudly in the log) instead of refusing to
// start. That's friendlier than a hard fail for someone who just downloaded
// the binary and double-clicked it without reading the README.

package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
)

// Config mirrors config.json. Field tags map JSON keys to Go fields.
// All fields exported (capitalized) so encoding/json can populate them.
type Config struct {
	// WhisperHost is the IP or DNS name of the Wyoming-protocol whisper
	// server. RFC1918 (private LAN) addresses are expected — see CLAUDE.md
	// for why no TLS/auth.
	WhisperHost string `json:"whisper_host"`

	// WhisperPort is the TCP port the server listens on. Standard Wyoming
	// whisper port is 10300.
	WhisperPort int `json:"whisper_port"`

	// Language is the BCP-47-ish code passed to whisper for forced language
	// selection. "en" for English. Empty string lets whisper auto-detect,
	// which is a touch slower and occasionally wrong on short clips.
	Language string `json:"language"`
}

// Default values used when config.json is missing or fields are blank.
// Hard-coding the user's known endpoint avoids a chicken-and-egg "config
// must exist or app won't transcribe" problem during early development.
const (
	defaultWhisperHost = "192.168.1.25"
	defaultWhisperPort = 10300
	defaultLanguage    = "en"
)

// LoadConfig reads config.json from the same directory as the running .exe.
// Missing file is non-fatal; missing fields fall back to compiled defaults.
// Returns the populated Config and the path it tried (for logging).
func LoadConfig() (Config, string) {
	cfg := Config{
		WhisperHost: defaultWhisperHost,
		WhisperPort: defaultWhisperPort,
		Language:    defaultLanguage,
	}

	exePath, err := os.Executable()
	if err != nil {
		log.Printf("config: os.Executable failed (%v); using defaults", err)
		return cfg, ""
	}
	cfgPath := filepath.Join(filepath.Dir(exePath), "config.json")

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		if os.IsNotExist(err) {
			log.Printf("config: %s not found; using defaults", cfgPath)
		} else {
			log.Printf("config: read %s failed (%v); using defaults", cfgPath, err)
		}
		return cfg, cfgPath
	}

	// Decode into a temporary so a partial config (only some fields set)
	// doesn't overwrite our defaults with zero values.
	var fromFile Config
	if err := json.Unmarshal(data, &fromFile); err != nil {
		log.Printf("config: parse %s failed (%v); using defaults", cfgPath, err)
		return cfg, cfgPath
	}
	if fromFile.WhisperHost != "" {
		cfg.WhisperHost = fromFile.WhisperHost
	}
	if fromFile.WhisperPort != 0 {
		cfg.WhisperPort = fromFile.WhisperPort
	}
	if fromFile.Language != "" {
		cfg.Language = fromFile.Language
	}
	return cfg, cfgPath
}

// Endpoint returns the host:port string used for dialing the whisper server.
// Format matches what net.Dial expects: "host:port" with no scheme prefix
// (Wyoming is a raw TCP protocol, not HTTP, so http:// would be wrong).
func (c Config) Endpoint() string {
	return fmt.Sprintf("%s:%d", c.WhisperHost, c.WhisperPort)
}
