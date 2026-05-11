// Package main is the freewhisper entry point.
//
// Milestone 2 scope: register a global push-to-talk hotkey (Ctrl+`) and log
// key-down / key-up events to a debug file. No audio capture yet — this
// milestone proves we can reliably observe the user's hotkey activity from
// anywhere in Windows, regardless of which window has focus.
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

//go:embed icon.ico
var iconData []byte

func main() {
	setupLogging()
	log.Print("freewhisper starting")
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
	systray.SetIcon(iconData)
	systray.SetTitle("FreeWhisper")
	systray.SetTooltip("FreeWhisper (idle)")

	mQuit := systray.AddMenuItem("Quit", "Exit FreeWhisper")
	go func() {
		<-mQuit.ClickedCh
		systray.Quit()
	}()

	// registerHotkey is in its own function to keep onReady readable.
	// We spawn the event loop as a goroutine so it runs concurrently with
	// the systray menu without blocking either side.
	go registerHotkey()
}

func onExit() {
	log.Print("freewhisper exiting")
}

// registerHotkey registers Ctrl+` (Ctrl+backtick) as a global push-to-talk
// hotkey and reports debounced press/release events to the log.
//
// Why backtick: it's a one-handed two-key chord (Ctrl + the key under Esc),
// doesn't collide with Excel's Ctrl+Space (column-select), VS Code's
// Ctrl+Space (autocomplete), or Alt+Space (window system menu).
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
	// 0xC0 is Win32 VK_OEM_3 — the backtick/tilde key in the top-left corner of
	// a US keyboard. The hotkey package's public Key constants don't include
	// the OEM keys, but its Windows implementation passes the Key value
	// straight through to RegisterHotKey (see hotkey_windows.go:57), so
	// casting the raw VK is safe.
	const vkBacktick hotkey.Key = 0xC0

	// 80ms comfortably exceeds the keyboard auto-repeat interval (default
	// Windows repeat rate is ~30/sec = 33ms; max-fast is around 30ms). If a
	// user is genuinely tapping the chord faster than 12 times per second to
	// trigger separate press cycles, they will be merged — acceptable trade.
	const debounceWindow = 80 * time.Millisecond

	hk := hotkey.New([]hotkey.Modifier{hotkey.ModCtrl}, vkBacktick)
	if err := hk.Register(); err != nil {
		log.Printf("hotkey register failed: %v", err)
		return
	}
	log.Print("hotkey registered: Ctrl+`")

	for {
		// Wait for the first DOWN that starts a press cycle.
		<-hk.Keydown()
		log.Print("hotkey DOWN (recording would start here)")

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
				// No DOWN within the window → release is real.
				log.Print("hotkey UP (recording would stop here)")
				break drain
			}
		}
	}
}
