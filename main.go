// Package main is the freewhisper entry point.
//
// Milestone 2 scope: register a global push-to-talk hotkey (Ctrl+Shift+Space)
// and log key-down / key-up events to a debug file. No audio capture yet —
// this milestone proves we can reliably observe the user's hotkey activity
// from anywhere in Windows, regardless of which window has focus.
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

	"github.com/getlantern/systray"
	"golang.design/x/hotkey"
)

//go:embed icon.ico
var iconData []byte

// main wires everything together: set up logging first (so any later errors
// land in a file we can read), then hand control to systray.Run() which
// blocks for the program's lifetime.
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

// registerHotkey registers Ctrl+Shift+Space as a global hotkey and loops
// forever logging press/release events.
//
// Under the hood the golang.design/x/hotkey package calls Win32's
// RegisterHotKey API, which routes hotkey messages to a hidden window the
// library manages. RegisterHotKey requires at least one modifier — single
// keys (like Right Alt by itself) aren't directly supported without a
// low-level keyboard hook, which would look more like a keylogger to EDR.
//
// We initially tried Ctrl+Alt+Space but found it was already registered by
// another process on the dev machine (likely an Office or OEM utility).
// Ctrl+Shift+Space is less commonly grabbed and works on the same hand
// position. The choice is hardcoded for now; we'll move it to config.json
// in a later milestone.
func registerHotkey() {
	hk := hotkey.New([]hotkey.Modifier{hotkey.ModCtrl, hotkey.ModShift}, hotkey.KeySpace)
	if err := hk.Register(); err != nil {
		log.Printf("hotkey register failed: %v", err)
		return
	}
	log.Print("hotkey registered: Ctrl+Shift+Space")

	// Keydown() and Keyup() each return a receive-only channel of events.
	// We block on Keydown(), log, then block on Keyup(), log, then loop.
	// This naturally models push-to-talk: each iteration is one press cycle.
	for {
		<-hk.Keydown()
		log.Print("hotkey DOWN (recording would start here)")
		<-hk.Keyup()
		log.Print("hotkey UP (recording would stop here)")
	}
}
