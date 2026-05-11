// Package main is the freewhisper entry point.
//
// Milestone 1 scope: put an icon in the Windows system tray with a single
// "Quit" menu item. No hotkey, no audio, no HTTP — just proof that the Go
// toolchain (compile, link, -H windowsgui suppression of the console window,
// embedded asset, systray library) all work end-to-end.
package main

import (
	// `embed` is part of Go's standard library. The `//go:embed` directive
	// below tells the compiler to read icon.ico off disk at *build* time and
	// stuff its bytes into the iconData variable. The result: the .exe is
	// self-contained — there's no separate icon.ico file to ship alongside it.
	//
	// The leading underscore (`_ "embed"`) means "import for its side effects
	// only; I'm not calling any function from this package." Without the
	// underscore, Go would refuse to compile because we'd be importing a
	// package we don't actually reference.
	_ "embed"

	// getlantern/systray is the canonical Go library for tray icons.
	// It abstracts over Windows / macOS / Linux — on Windows it calls the
	// Shell_NotifyIcon Win32 API under the hood.
	"github.com/getlantern/systray"
)

// iconData holds the raw bytes of icon.ico, baked into the binary at compile
// time by the `//go:embed` directive on the line immediately above.
//
//go:embed icon.ico
var iconData []byte

// main is the program entry point. systray.Run() takes two callbacks:
//   - onReady: invoked once after the OS tray is initialized; this is where we
//     register our icon and menu items.
//   - onExit:  invoked when systray.Quit() is called; cleanup goes here.
//
// systray.Run() blocks the main goroutine for the lifetime of the program — it
// pumps Windows messages internally. When Quit() is called, Run() returns and
// main() exits.
func main() {
	systray.Run(onReady, onExit)
}

// onReady fires once, on the goroutine systray manages, immediately after the
// tray icon is ready to receive setup calls. Do all your initial setup here.
func onReady() {
	systray.SetIcon(iconData)
	systray.SetTitle("FreeWhisper") // text label on macOS/Linux; ignored on Windows
	systray.SetTooltip("FreeWhisper (idle)")

	// AddMenuItem returns a handle whose ClickedCh field is a Go channel that
	// receives an event each time the menu item is clicked.
	mQuit := systray.AddMenuItem("Quit", "Exit FreeWhisper")

	// We spawn a goroutine to wait for the Quit click. Goroutines are Go's
	// lightweight concurrency primitive — think "a function running in the
	// background alongside everything else." `<-mQuit.ClickedCh` blocks this
	// goroutine until something is sent on that channel (i.e., a click). Then
	// we call systray.Quit(), which makes systray.Run() in main() return.
	go func() {
		<-mQuit.ClickedCh
		systray.Quit()
	}()
}

// onExit fires after systray.Quit() is called and the tray icon is removed.
// We have nothing to clean up yet — when we add audio capture and HTTP
// clients later, this is where we'll release those resources gracefully.
func onExit() {
}
