// indicators.go: feedback the user can see/hear while recording.
//
// Two channels:
//
//   * Visual: swap the tray icon from idle (blue) to recording (red)
//     while the hotkey is held. systray.SetIcon takes the raw ICO bytes
//     and tells Windows to redraw the tray entry.
//
//   * Audible: a short two-tone chime — higher pitch on start, lower on
//     stop. Uses kernel32!Beep, which on modern Windows goes through the
//     default audio device (no need for a PC speaker).
//
// Both are gated by config flags (NotifyColorChange, NotifyBeep), default
// off, controlled from the settings GUI.

package main

import (
	"github.com/getlantern/systray"
	"golang.org/x/sys/windows"
)

// procBeep is the kernel32!Beep entry. Loaded lazily, same pattern as the
// clipboard procs in paster.go.
var procBeep = windows.NewLazySystemDLL("kernel32.dll").NewProc("Beep")

// Frequencies in Hz and durations in ms for the start/stop chimes.
// 800 → 400 is a comfortable interval; 120 ms is short enough to not feel
// laggy when push-to-talking quickly.
const (
	beepStartFreqHz uint32 = 800
	beepStopFreqHz  uint32 = 400
	beepDurationMs  uint32 = 120
)

// startRecordingIndicator is called immediately after we observe a real
// hotkey-DOWN. Both effects are best-effort — errors are swallowed because
// indicators are non-essential UX (we shouldn't tank a recording over a
// failed Beep call).
func startRecordingIndicator() {
	cfg := currentConfig()
	if cfg.NotifyColorChange {
		systray.SetIcon(iconRecording)
		systray.SetTooltip("FreeWhisper (recording…)")
	}
	if cfg.NotifyBeep {
		beep(beepStartFreqHz, beepDurationMs)
	}
}

// stopRecordingIndicator is called when the hotkey is released, after the
// recorder has stopped. It must always restore the idle icon if the
// color-change setting is on, even if the recording errored out, so the
// user isn't left looking at a stuck red icon.
func stopRecordingIndicator() {
	cfg := currentConfig()
	if cfg.NotifyColorChange {
		systray.SetIcon(iconIdle)
		systray.SetTooltip("FreeWhisper (idle)")
	}
	if cfg.NotifyBeep {
		beep(beepStopFreqHz, beepDurationMs)
	}
}

// beep is a thin wrapper over kernel32!Beep. The call blocks for
// `durationMs` — fine for a 120ms beep on the hotkey goroutine, since the
// user is also waiting (they just pressed/released the chord). If we
// later want non-blocking beeps, spawn a goroutine here.
func beep(freqHz, durationMs uint32) {
	procBeep.Call(uintptr(freqHz), uintptr(durationMs))
}
