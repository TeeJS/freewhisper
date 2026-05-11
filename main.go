// Package main is the freewhisper entry point.
//
// Milestone 5 (MVP-complete) scope: while Ctrl+` is held, capture mic audio
// (recorder.go); on release, send the PCM to the Wyoming whisper server
// (transcriber.go), then paste the resulting text into the active window
// (paster.go). The captured audio is still also written to test.wav for
// sanity-checking on next-time-at-keyboard debug sessions.
//
// Endpoint and language come from config.json (loaded at startup, falls
// back to compiled defaults if missing).
//
// We log to a file (not stdout) because the app is built with `-H windowsgui`,
// which suppresses the console window. Anything written to stdout/stderr in
// such a build is silently dropped, so file logging is the only way to see
// what the program is doing.
package main

import (
	_ "embed"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/getlantern/systray"
	"golang.design/x/hotkey"
)

// Two tray-icon variants embedded at compile time. The idle icon (blue F)
// shows when the app is sitting waiting for a hotkey; the recording icon
// (red F) replaces it while the user is holding the chord, but only when
// the NotifyColorChange setting is on. We swap by calling systray.SetIcon
// with the appropriate byte slice at runtime.

//go:embed icon.ico
var iconIdle []byte

//go:embed icon_recording.ico
var iconRecording []byte

// appConfig holds the runtime configuration. Loaded once at startup and
// then read-only — we treat config as immutable for the process lifetime.
// (Hot-reload is a feature we'd add later, after there's any need for it.)
var appConfig Config

// configExisted tracks whether config.json was present at startup. The
// settings GUI uses this to auto-open on first run (when false).
var configExisted bool

func main() {
	setupLogging()
	log.Print("freewhisper starting")
	cfgPath := ""
	appConfig, cfgPath, configExisted = LoadConfig()
	if !configExisted {
		// First run: write the defaults to disk so the user has a file
		// to inspect/edit and to anchor the settings GUI's auto-open
		// behavior. Failure to write is logged but non-fatal.
		if err := appConfig.Save(); err != nil {
			log.Printf("config: first-run save failed: %v", err)
		} else {
			log.Printf("config: wrote default config to %s (first run)", cfgPath)
		}
	}
	log.Printf("config: endpoint=%s language=%s hotkey=%s (from %s)",
		appConfig.Endpoint(),
		appConfig.Language,
		HotkeyDisplay(appConfig.HotkeyModifiers, appConfig.HotkeyKey),
		cfgPath,
	)
	systray.Run(onReady, onExit)
}

// setupLogging redirects Go's default logger to %LOCALAPPDATA%\freewhisper\debug.log.
//
// %LOCALAPPDATA% (typically C:\Users\<you>\AppData\Local) is the standard
// Windows location for per-user, non-roaming application data. Unlike
// %APPDATA%, it doesn't sync to other machines or to OneDrive, so log files
// stay local and don't bloat your cloud storage.
//
// If anything goes wrong here (env var missing, disk full, permissions), we
// silently fall back to the default logger destination (stderr, which in a
// `-H windowsgui` build goes nowhere). That's a known limitation of GUI-only
// Windows binaries — there's no good place to surface bootstrap errors.
// We'll address it later if it ever bites us; for now, "no logs" is an
// acceptable failure mode for milestone 2.
func setupLogging() {
	appData := os.Getenv("LOCALAPPDATA")
	if appData == "" {
		return
	}
	dir := filepath.Join(appData, "freewhisper")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return
	}
	logPath := filepath.Join(dir, "debug.log")
	// os.O_APPEND so each run adds to the existing file instead of overwriting.
	// 0644 = rw for owner, r for group/other (Windows largely ignores this but
	// it's good Unix hygiene and Go's os package wants the mode argument).
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	log.SetOutput(f)
	// LstdFlags = date + seconds. Lmicroseconds gives us sub-second precision,
	// useful when reasoning about hotkey timing later.
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
}

func onReady() {
	systray.SetIcon(iconIdle)
	systray.SetTitle("FreeWhisper")
	systray.SetTooltip("FreeWhisper (idle)")

	// Menu items, in display order: Settings, then a separator, then Quit.
	// AddMenuItemCheckbox / separator aren't needed yet — keep it minimal.
	mSettings := systray.AddMenuItem("Settings…", "Open settings window")
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("Quit", "Exit FreeWhisper")

	go func() {
		for range mSettings.ClickedCh {
			OpenSettings()
		}
	}()
	go func() {
		<-mQuit.ClickedCh
		systray.Quit()
	}()

	// registerHotkey is in its own function to keep onReady readable.
	// We spawn the event loop as a goroutine so it runs concurrently with
	// the systray menu without blocking either side.
	go registerHotkey()

	// First-run UX: if config.json didn't exist at startup, the user
	// almost certainly hasn't set the whisper endpoint yet. Pop the
	// settings window automatically so they don't have to discover the
	// tray menu first.
	if !configExisted {
		OpenSettings()
	}
}

// applyConfigChange swaps the running config to next. Server/language/
// notification settings take effect immediately because all the code
// paths that consume them read appConfig fresh on each operation
// (transcribe, indicator). The hotkey, by contrast, was registered with
// Win32 once at startup and the press loop is blocked on the old object's
// channels — changing the chord requires a restart. The settings GUI
// surfaces this caveat in its layout, so we don't duplicate the warning
// here.
func applyConfigChange(next Config) {
	oldHotkey := HotkeyDisplay(appConfig.HotkeyModifiers, appConfig.HotkeyKey)
	newHotkey := HotkeyDisplay(next.HotkeyModifiers, next.HotkeyKey)
	appConfig = next
	log.Printf("config updated: endpoint=%s language=%s hotkey=%s notify(color=%v beep=%v)",
		next.Endpoint(), next.Language, newHotkey,
		next.NotifyColorChange, next.NotifyBeep)
	if oldHotkey != newHotkey {
		log.Printf("hotkey changed (%s → %s); restart required to apply", oldHotkey, newHotkey)
	}
}

func onExit() {
	log.Print("freewhisper exiting")
}

// registerHotkey registers the user-configured push-to-talk chord as a
// global hotkey and reports debounced press/release events to the log.
//
// Modifier/key values come from appConfig (see config.go) and get parsed
// via the lookup tables in hotkeymap.go. Default chord is Ctrl+`
// (Ctrl+backtick): one-handed reachable, top-left of the keyboard, no
// collision with Excel's Ctrl+Space (column-select), VS Code's Ctrl+Space
// (autocomplete), or Alt+Space (window system menu).
//
// Why a debounce window: under the hood, golang.design/x/hotkey calls Win32's
// RegisterHotKey, which causes WM_HOTKEY messages to be re-posted at the
// keyboard auto-repeat rate (~10ms apart on default settings) while the chord
// is held. The library detects "release" by polling GetAsyncKeyState after
// each WM_HOTKEY, which produces a spurious UP→DOWN pair on every repeat
// cycle. Without debouncing, holding the chord for 2 seconds produces ~200
// fake press cycles instead of one continuous press — disastrous for
// push-to-talk semantics.
//
// The debounce: only the first DOWN of a press burst and the last UP of that
// burst are reported. We do this by waiting `debounceWindow` after each UP
// to see if another DOWN follows. If one does, it was auto-repeat noise; if
// it doesn't, the release is real.
func registerHotkey() {
	// 80ms comfortably exceeds the keyboard auto-repeat interval (default
	// Windows repeat rate is ~30/sec = 33ms; max-fast is around 30ms). If a
	// user is genuinely tapping the chord faster than 12 times per second to
	// trigger separate press cycles, they will be merged — acceptable trade.
	const debounceWindow = 80 * time.Millisecond

	mods, err := ParseModifiers(appConfig.HotkeyModifiers)
	if err != nil {
		log.Printf("hotkey register: %v", err)
		return
	}
	if len(mods) == 0 {
		log.Print("hotkey register: at least one modifier (Ctrl/Alt/Shift/Win) required; check config.json")
		return
	}
	key, err := ParseKey(appConfig.HotkeyKey)
	if err != nil {
		log.Printf("hotkey register: %v", err)
		return
	}
	display := HotkeyDisplay(appConfig.HotkeyModifiers, appConfig.HotkeyKey)

	hk := hotkey.New(mods, key)
	if err := hk.Register(); err != nil {
		log.Printf("hotkey register failed (%s): %v", display, err)
		return
	}
	log.Printf("hotkey registered: %s", display)

	for {
		// Wait for the first DOWN that starts a press cycle, then kick off
		// recording on a worker goroutine inside the Recorder.
		<-hk.Keydown()
		log.Print("hotkey DOWN — recording started")
		startRecordingIndicator()
		rec := StartRecording()

		// Drain UP/DOWN pairs caused by auto-repeat until we see an UP
		// that *isn't* followed by another DOWN within debounceWindow.
		// That UP is the genuine release.
	drain:
		for {
			<-hk.Keyup()
			timer := time.NewTimer(debounceWindow)
			select {
			case <-hk.Keydown():
				// Auto-repeat blip — key is still held. Discard both
				// events and keep waiting for the real release.
				if !timer.Stop() {
					<-timer.C
				}
			case <-timer.C:
				// Genuine release: stop the recorder, persist to disk for
				// debugging, and ship the PCM off to whisper. The indicator
				// stops regardless of recording outcome so the icon never
				// gets stuck in the red state.
				pcm, err := rec.Stop()
				stopRecordingIndicator()
				if err != nil {
					log.Printf("recording failed: %v", err)
					break drain
				}
				saveCapturedWAV(pcm)
				transcribeAndPaste(pcm)
				break drain
			}
		}
	}
}

// saveCapturedWAV writes the captured PCM bytes to test.wav in
// %LOCALAPPDATA%\freewhisper\, alongside debug.log. We log the byte count,
// the implied duration, and the full path so the user can find and play
// back the file to sanity-check the capture.
func saveCapturedWAV(pcm []byte) {
	appData := os.Getenv("LOCALAPPDATA")
	if appData == "" {
		log.Printf("LOCALAPPDATA not set; can't save WAV")
		return
	}
	path := filepath.Join(appData, "freewhisper", "test.wav")
	if err := writeWAV(path, pcm, captureSampleRate, captureChannels, captureBitsPerSample); err != nil {
		log.Printf("WAV write failed: %v", err)
		return
	}
	// Duration = bytes / (sampleRate * blockAlign).
	// blockAlign = channels * bitsPerSample/8 = 1 * 2 = 2.
	durSec := float64(len(pcm)) / float64(captureSampleRate*uint32(captureChannels*captureBitsPerSample/8))
	log.Printf("hotkey UP — recorded %d bytes (%.2fs) → %s", len(pcm), durSec, path)
}

// transcribeAndPaste ships the captured PCM to the configured whisper
// endpoint, logs the recognized text, and pastes it into the focused
// window. We measure wall-clock latency on transcription (the slow step)
// so regressions are visible at a glance in the log.
//
// The whole thing runs on the hotkey goroutine (synchronously). That's
// fine for a push-to-talk app — the user is already waiting after release
// to see the text appear. Going async would only add complexity without
// improving the perceived experience.
func transcribeAndPaste(pcm []byte) {
	if !appConfig.EndpointConfigured() {
		log.Print("transcribe skipped: whisper_host/whisper_port not set in config.json (right-click tray icon → Settings)")
		return
	}
	start := time.Now()
	text, err := Transcribe(appConfig.Endpoint(), appConfig.Language, pcm)
	elapsed := time.Since(start)
	if err != nil {
		log.Printf("transcribe failed (%.2fs): %v", elapsed.Seconds(), err)
		return
	}
	log.Printf("transcript (%.2fs): %q", elapsed.Seconds(), text)

	// Whisper sometimes returns text with a leading space (its tokenizer
	// quirk). It looks ugly when pasted into the middle of an existing
	// sentence, so trim it. Trailing whitespace is fine to leave.
	if len(text) > 0 && text[0] == ' ' {
		text = text[1:]
	}
	if text == "" {
		return // nothing to paste
	}
	if err := Paste(text); err != nil {
		log.Printf("paste failed: %v", err)
	}
}
