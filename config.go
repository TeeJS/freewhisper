// config.go: load and save freewhisper's runtime settings in config.json.
//
// All persistent user-facing settings live in this struct. Code-shaped
// decisions (audio sample rate, debounce window, polling interval) stay
// hardcoded — they're tuning constants, not user knobs.
//
// File location: next to the .exe. The user can drop both `freewhisper.exe`
// and `config.json` in any directory and run from there.
//
// Save() round-trips the struct back to disk so the settings GUI can write
// new values without us hand-rolling JSON encoding at the call site.

package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
)

// Config mirrors config.json. Field tags map JSON keys to Go fields.
// All fields exported (capitalized) so encoding/json can read+write them.
type Config struct {
	// WhisperHost is the IP or DNS name of the Wyoming-protocol whisper
	// server. Empty == unset; the app will log an error and skip
	// transcription until configured.
	WhisperHost string `json:"whisper_host"`

	// WhisperPort is the TCP port the server listens on. Standard Wyoming
	// whisper port is 10300. Zero == unset.
	WhisperPort int `json:"whisper_port"`

	// Language is the BCP-47-ish code passed to whisper for forced language
	// selection. "en" for English. Empty lets whisper auto-detect.
	Language string `json:"language"`

	// MicDeviceID is the WASAPI endpoint ID of the capture device to record
	// from. Empty = follow the Windows default microphone. The value is the
	// opaque ID string from IMMDevice::GetId; the settings GUI shows friendly
	// names and stores the matching ID. If the saved device is missing at
	// record time, the recorder falls back to the default (see audiodevices.go).
	MicDeviceID string `json:"mic_device_id"`

	// MicVolumeManage, when true, makes FreeWhisper set the microphone's input
	// level to MicVolume before each recording. This is the *Windows* mic level
	// (IAudioEndpointVolume) — system-wide, so it affects every app, not just
	// us. Default false: we leave the system level untouched.
	MicVolumeManage bool `json:"manage_mic_volume"`

	// MicVolume is the input level (0–100%) applied when MicVolumeManage is on.
	MicVolume int `json:"mic_volume"`

	// HotkeyModifiers lists the modifier keys that must be held alongside
	// HotkeyKey. Valid entries: "Ctrl", "Alt", "Shift", "Win" (case-
	// insensitive on load). RegisterHotKey requires at least one modifier;
	// an empty list will cause hotkey registration to fail (logged, not
	// crashed).
	HotkeyModifiers []string `json:"hotkey_modifiers"`

	// HotkeyKey is the non-modifier key, written as a recognizable name.
	// Recognized values (case-insensitive): "A"–"Z", "0"–"9", "Space",
	// "Backtick" (or "`"), "F1"–"F12". The string-form is friendlier for
	// the config GUI and the JSON file than raw Win32 virtual-key codes.
	HotkeyKey string `json:"hotkey_key"`

	// NotifyColorChange swaps the tray icon to a "recording" variant
	// while the hotkey is held. Default false to stay quiet.
	NotifyColorChange bool `json:"notify_color_change"`

	// NotifyBeep plays a short audio chime on record start and stop.
	// Default false to stay quiet.
	NotifyBeep bool `json:"notify_beep"`

	// RestoreClipboard controls whether we put the user's previous clipboard
	// text back after pasting a transcript. Default false: we leave the
	// transcript on the clipboard. Restoring is a nicety, but on machines
	// with clipboard-monitoring/DLP software (or any slow clipboard handoff)
	// the restore can race the target app's paste and cause the OLD clipboard
	// to land instead of the transcript. Leaving the transcript on the
	// clipboard makes the paste reliable; set true only if you specifically
	// want your prior clipboard preserved and your machine is fast enough.
	RestoreClipboard bool `json:"restore_clipboard"`

	// SilenceDurationMs is how long the user must pause speaking before
	// the recorder cuts the current chunk and ships it to whisper for
	// transcription. Shorter = more responsive but more chunks (and
	// whisper is less accurate on short fragments). Longer = closer to
	// the old batch behavior. Default 400ms is a reasonable compromise.
	// Range: anything > 100ms is sensible; we don't enforce.
	SilenceDurationMs int `json:"silence_duration_ms"`
}

// configFilename is the name we look for next to the .exe and create on
// first run. Centralized so the GUI and the loader agree on the path.
const configFilename = "config.json"

// DefaultConfig returns a Config suitable for a brand-new install. It
// intentionally leaves WhisperHost/Port blank so first-run users see an
// obvious "needs configuring" state in the settings window rather than
// having the app silently try to dial an inappropriate default.
func DefaultConfig() Config {
	return Config{
		WhisperHost:       "",
		WhisperPort:       10300, // safe to default — same on every Wyoming install
		Language:          "en",
		MicDeviceID:       "",    // follow the Windows default mic
		MicVolumeManage:   false, // don't touch the system mic level by default
		MicVolume:         75,    // sensible starting level if the user enables it
		HotkeyModifiers:   []string{"Ctrl"},
		HotkeyKey:         "Backtick",
		NotifyColorChange: false,
		NotifyBeep:        false,
		RestoreClipboard:  false, // reliable default: leave transcript on clipboard
		SilenceDurationMs: 400,
	}
}

// configPath returns the absolute path to config.json (next to the .exe).
// Falls back to the current working directory if os.Executable fails —
// shouldn't happen in practice but we don't want to crash over it.
func configPath() string {
	exePath, err := os.Executable()
	if err != nil {
		return configFilename
	}
	return filepath.Join(filepath.Dir(exePath), configFilename)
}

// LoadConfig reads config.json. Returns:
//   - the populated Config (defaults filled in for any missing fields)
//   - the path it tried (for logging)
//   - whether the file existed (false on first run)
//
// Parse errors fall back to defaults and log loudly — friendlier than
// refusing to start.
func LoadConfig() (Config, string, bool) {
	cfg := DefaultConfig()
	path := configPath()

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, path, false
		}
		log.Printf("config: read %s failed (%v); using defaults", path, err)
		return cfg, path, false
	}

	// Decode into a temporary so a partial config doesn't overwrite
	// our defaults with zero values.
	var fromFile Config
	if err := json.Unmarshal(data, &fromFile); err != nil {
		log.Printf("config: parse %s failed (%v); using defaults", path, err)
		return cfg, path, true // file exists but unparseable; treat as existing
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
	// Empty MicDeviceID means "system default", which is also the zero value,
	// so non-empty-overwrite is exactly right here.
	if fromFile.MicDeviceID != "" {
		cfg.MicDeviceID = fromFile.MicDeviceID
	}
	if len(fromFile.HotkeyModifiers) > 0 {
		cfg.HotkeyModifiers = fromFile.HotkeyModifiers
	}
	if fromFile.HotkeyKey != "" {
		cfg.HotkeyKey = fromFile.HotkeyKey
	}
	// Booleans always overwrite — we can't distinguish "user set false"
	// from "field absent." A field absent in JSON decodes to false, and
	// that's the same as the safe default, so this is fine.
	cfg.NotifyColorChange = fromFile.NotifyColorChange
	cfg.NotifyBeep = fromFile.NotifyBeep
	cfg.RestoreClipboard = fromFile.RestoreClipboard
	cfg.MicVolumeManage = fromFile.MicVolumeManage
	// MicVolume: keep the default (75) when the field is absent/zero; any real
	// 1–100 value overrides. Muting via this slider isn't a use case — users
	// would just uncheck "manage" instead.
	if fromFile.MicVolume > 0 {
		cfg.MicVolume = fromFile.MicVolume
	}

	// SilenceDurationMs == 0 is "absent or invalid" — keep the default.
	if fromFile.SilenceDurationMs > 0 {
		cfg.SilenceDurationMs = fromFile.SilenceDurationMs
	}

	return cfg, path, true
}

// Save writes the Config to disk as indented JSON. Used by the settings
// GUI when the user clicks OK/Save.
func (c Config) Save() error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	path := configPath()
	// Trailing newline so the file ends cleanly when viewed in editors.
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// Endpoint returns the host:port string used for dialing the whisper server.
func (c Config) Endpoint() string {
	return fmt.Sprintf("%s:%d", c.WhisperHost, c.WhisperPort)
}

// EndpointConfigured reports whether the user has filled in the whisper
// host. We use this to decide whether to attempt transcription on hotkey
// release or skip it with a friendly log message.
func (c Config) EndpointConfigured() bool {
	return c.WhisperHost != "" && c.WhisperPort != 0
}
