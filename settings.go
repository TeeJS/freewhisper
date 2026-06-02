// settings.go: a modal settings dialog built with github.com/lxn/walk.
//
// Why walk: it's the most pragmatic Go GUI option for a Windows-only tool.
// Native Win32 widgets via syscall under the hood, no CGO, no embedded
// browser, and adds only ~2MB to the binary. The declarative subpackage
// lets us describe the layout as nested structs rather than imperative
// CreateWindowEx + dialog procedure boilerplate.
//
// Threading: walk's message loop and systray's message loop don't play
// nicely on the same OS thread. We solve it by spawning each settings
// dialog on its own goroutine with runtime.LockOSThread — walk's
// Dialog.Run() then pumps messages on that thread for the dialog's
// lifetime and returns when the user clicks OK/Cancel. systray continues
// undisturbed on the main goroutine.
//
// We guard against opening multiple settings windows at once with an
// atomic flag — second clicks of "Settings" while a dialog is open are
// no-ops.

package main

import (
	"log"
	"runtime"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"
)

// settingsOpen tracks whether a settings dialog is currently displayed.
// We use atomic ops instead of a mutex because we only need a simple
// "is open / set open / set closed" check, not synchronized critical
// sections.
var settingsOpen atomic.Bool

// OpenSettings shows the settings dialog. Safe to call from any goroutine.
// The call returns immediately; the dialog itself runs on a dedicated
// goroutine. If a settings dialog is already open, this call is a no-op
// (the existing window stays focused).
func OpenSettings() {
	if !settingsOpen.CompareAndSwap(false, true) {
		log.Print("settings: dialog already open; ignoring duplicate request")
		return
	}
	go func() {
		defer settingsOpen.Store(false)
		// LockOSThread because walk's message loop talks to user32 directly
		// and Win32 windows are owned by the thread that created them.
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		if err := runSettingsDialog(); err != nil {
			log.Printf("settings: dialog error: %v", err)
		}
	}()
}

// runSettingsDialog builds and runs the modal settings window. Returns
// nil on Cancel/OK normally; non-nil on Win32 errors creating the window.
//
// The form mirrors Config one-for-one. On OK we validate the new values,
// write them to disk, swap appConfig over, and re-register the hotkey
// (since modifiers/key may have changed).
func runSettingsDialog() error {
	var dlg *walk.Dialog
	var serverEdit, portEdit, languageEdit, silenceEdit, hotkeyKeyCombo *walk.LineEdit
	var keyCombo, micCombo *walk.ComboBox
	var modCtrl, modAlt, modShift, modWin *walk.CheckBox
	var notifyColor, notifyBeep, restoreClipboard, manageVolume, showMeter *walk.CheckBox
	var volSlider *walk.Slider
	var levelBar *walk.ProgressBar
	var okBtn, cancelBtn *walk.PushButton
	_ = hotkeyKeyCombo // silence linter — placeholder if we ever swap LineEdit for ComboBox

	// Snapshot the current config so the form starts populated correctly.
	current := currentConfig()
	hasMod := map[string]bool{}
	for _, m := range current.HotkeyModifiers {
		hasMod[m] = true
	}

	// Enumerate microphones for the device picker. "System Default" (empty ID)
	// is always first; the rest come from WASAPI. micNames drives the dropdown;
	// micIDs is the parallel list of endpoint IDs we actually persist. If
	// enumeration fails we just offer "System Default" and log it.
	micNames := []string{"System Default"}
	micIDs := []string{""}
	if devs, derr := listCaptureDevices(); derr != nil {
		log.Printf("settings: could not list microphones: %v", derr)
	} else {
		for _, d := range devs {
			label := d.Name
			if label == "" {
				label = d.ID
			}
			micNames = append(micNames, label)
			micIDs = append(micIDs, d.ID)
		}
	}
	micIndex := 0
	for i, id := range micIDs {
		if id == current.MicDeviceID {
			micIndex = i
			break
		}
	}

	// Microphone input-level slider. When we're managing the level, seed it
	// from config; otherwise show the device's current actual level so that
	// turning "manage" on captures where it already sits rather than jumping.
	volValue := current.MicVolume
	if !current.MicVolumeManage {
		if v, verr := readCaptureVolume(current.MicDeviceID); verr == nil {
			volValue = v
		}
	}

	// Live "test mic" level meter. Not persisted — it's a momentary test mode.
	// While on, a levelMonitor captures from the selected device and a ticker
	// goroutine pushes the peak into the progress bar (on the GUI thread via
	// dlg.Synchronize). Torn down when unchecked, when the device changes, and
	// when the dialog closes (the post-Run stopMeter() below).
	var meter *levelMonitor
	var meterTickStop chan struct{}
	meterRunning := false

	// dialogReady gates handlers that would otherwise fire while the dialog is
	// still being constructed (e.g. the volume slider's initial Value set must
	// NOT push a level change to the system mic). Flipped true after Create().
	dialogReady := false

	stopMeter := func() {
		if !meterRunning {
			return
		}
		meterRunning = false
		close(meterTickStop)
		if meter != nil {
			meter.Stop()
			meter = nil
		}
	}
	startMeter := func() {
		if meterRunning {
			return
		}
		id := ""
		if i := micCombo.CurrentIndex(); i >= 0 && i < len(micIDs) {
			id = micIDs[i]
		}
		meter = startLevelMonitor(id)
		meterTickStop = make(chan struct{})
		meterRunning = true
		go func(stop chan struct{}, mon *levelMonitor) {
			t := time.NewTicker(80 * time.Millisecond)
			defer t.Stop()
			for {
				select {
				case <-stop:
					// Final frame: zero the bar (runs only if the dialog is
					// still alive; a no-op once it's closed).
					dlg.Synchronize(func() {
						if levelBar != nil {
							levelBar.SetValue(0)
						}
					})
					return
				case <-t.C:
					lvl := mon.Level()
					dlg.Synchronize(func() {
						if levelBar != nil {
							levelBar.SetValue(lvl)
						}
					})
				}
			}
		}(meterTickStop, meter)
	}

	err := (Dialog{
		AssignTo:      &dlg,
		Title:         "FreeWhisper Settings",
		MinSize:       Size{Width: 380, Height: 540},
		Layout:        VBox{},
		DefaultButton: &okBtn,
		CancelButton:  &cancelBtn,
		Children: []Widget{
			GroupBox{
				Title:  "Whisper Server",
				Layout: Grid{Columns: 2},
				Children: []Widget{
					Label{Text: "Host (IP or DNS):"},
					LineEdit{AssignTo: &serverEdit, Text: current.WhisperHost},
					Label{Text: "Port:"},
					LineEdit{AssignTo: &portEdit, Text: strconv.Itoa(current.WhisperPort)},
					Label{Text: "Language:"},
					LineEdit{AssignTo: &languageEdit, Text: current.Language},
				},
			},
			GroupBox{
				Title:  "Microphone",
				Layout: VBox{},
				Children: []Widget{
					Label{Text: "Capture device:"},
					ComboBox{
						AssignTo:     &micCombo,
						Model:        micNames,
						CurrentIndex:  micIndex,
						OnCurrentIndexChanged: func() {
							// Re-point the live meter at the newly selected
							// device so you can compare mics on the fly.
							if meterRunning {
								stopMeter()
								startMeter()
							}
						},
					},
					Label{Text: "(\"System Default\" follows your Windows default mic. Applies on your next dictation.)"},
					CheckBox{AssignTo: &manageVolume, Text: "Set input level on each recording", Checked: current.MicVolumeManage},
					Slider{
						AssignTo: &volSlider,
						MinValue: 0,
						MaxValue: 100,
						Value:    volValue,
						OnValueChanged: func() {
							// Apply the level live (Tracking is off, so this
							// fires on thumb release) so the meter reflects it as
							// you tune, and the slider acts as a real mic-level
							// control — not just a value saved on OK. Guarded so
							// the initial Value set during construction is inert.
							if !dialogReady {
								return
							}
							id := ""
							if i := micCombo.CurrentIndex(); i >= 0 && i < len(micIDs) {
								id = micIDs[i]
							}
							if verr := applyCaptureVolume(id, volSlider.Value()); verr != nil {
								log.Printf("settings: live mic level apply failed: %v", verr)
							}
						},
					},
					Label{Text: "(System-wide: changes the Windows mic level for every app, not just FreeWhisper.)"},
					CheckBox{
						AssignTo: &showMeter,
						Text:     "Test microphone (show live input level)",
						OnClicked: func() {
							if showMeter.Checked() {
								startMeter()
							} else {
								stopMeter()
							}
						},
					},
					ProgressBar{AssignTo: &levelBar, MinValue: 0, MaxValue: 100, Value: 0},
					Label{Text: "(Speak and watch the bar. Flat bar = the mic isn't hearing you; raise the level or pick another device.)"},
				},
			},
			GroupBox{
				Title:  "Hotkey",
				Layout: VBox{},
				Children: []Widget{
					Label{Text: "Modifiers (at least one required):"},
					Composite{
						Layout: HBox{},
						Children: []Widget{
							CheckBox{AssignTo: &modCtrl, Text: "Ctrl", Checked: hasMod["Ctrl"]},
							CheckBox{AssignTo: &modAlt, Text: "Alt", Checked: hasMod["Alt"]},
							CheckBox{AssignTo: &modShift, Text: "Shift", Checked: hasMod["Shift"]},
							CheckBox{AssignTo: &modWin, Text: "Win", Checked: hasMod["Win"]},
						},
					},
					Label{Text: "Key:"},
					ComboBox{
						AssignTo:     &keyCombo,
						Model:        AllowedKeys,
						CurrentIndex: indexOf(AllowedKeys, current.HotkeyKey),
					},
					Label{Text: "(Restart FreeWhisper to apply hotkey changes.)"},
				},
			},
			GroupBox{
				Title:  "Streaming",
				Layout: Grid{Columns: 2},
				Children: []Widget{
					Label{Text: "Silence duration (ms):"},
					LineEdit{AssignTo: &silenceEdit, Text: strconv.Itoa(current.SilenceDurationMs)},
					Label{Text: "(How long you must pause for a chunk to be sent. Shorter = more responsive, less accurate. ~400ms is a good starting point.)", ColumnSpan: 2},
				},
			},
			GroupBox{
				Title:  "Notifications",
				Layout: VBox{},
				Children: []Widget{
					CheckBox{AssignTo: &notifyColor, Text: "Change tray-icon color while recording", Checked: current.NotifyColorChange},
					CheckBox{AssignTo: &notifyBeep, Text: "Beep on record start/stop", Checked: current.NotifyBeep},
				},
			},
			GroupBox{
				Title:  "Pasting",
				Layout: VBox{},
				Children: []Widget{
					CheckBox{AssignTo: &restoreClipboard, Text: "Restore previous clipboard after pasting", Checked: current.RestoreClipboard},
					Label{Text: "(Leave OFF for reliable pasting. When ON, restoring your old clipboard can race the paste on some PCs and cause the wrong text to land.)"},
				},
			},
			Composite{
				Layout: HBox{},
				Children: []Widget{
					HSpacer{},
					PushButton{
						AssignTo: &okBtn,
						Text:     "Save",
						OnClicked: func() {
							// Gather form values into a candidate Config.
							next := current
							next.WhisperHost = serverEdit.Text()
							p, err := strconv.Atoi(portEdit.Text())
							if err != nil {
								walk.MsgBox(dlg, "Invalid port", "Port must be a number.", walk.MsgBoxIconError)
								return
							}
							next.WhisperPort = p
							next.Language = languageEdit.Text()
							// micCombo's index lines up with micIDs ([0] = "" = system default).
							if i := micCombo.CurrentIndex(); i >= 0 && i < len(micIDs) {
								next.MicDeviceID = micIDs[i]
							}
							next.MicVolumeManage = manageVolume.Checked()
							next.MicVolume = volSlider.Value()
							// Apply the level right away so the change is visible
							// in Windows immediately, not only on the next record.
							if next.MicVolumeManage {
								if verr := applyCaptureVolume(next.MicDeviceID, next.MicVolume); verr != nil {
									log.Printf("settings: could not apply mic level: %v", verr)
								}
							}
							var mods []string
							if modCtrl.Checked() {
								mods = append(mods, "Ctrl")
							}
							if modAlt.Checked() {
								mods = append(mods, "Alt")
							}
							if modShift.Checked() {
								mods = append(mods, "Shift")
							}
							if modWin.Checked() {
								mods = append(mods, "Win")
							}
							if len(mods) == 0 {
								walk.MsgBox(dlg, "No modifier", "Select at least one modifier (Ctrl/Alt/Shift/Win).", walk.MsgBoxIconError)
								return
							}
							next.HotkeyModifiers = mods
							if keyCombo.CurrentIndex() >= 0 {
								next.HotkeyKey = AllowedKeys[keyCombo.CurrentIndex()]
							}
							next.NotifyColorChange = notifyColor.Checked()
							next.NotifyBeep = notifyBeep.Checked()
							next.RestoreClipboard = restoreClipboard.Checked()
							sil, err := strconv.Atoi(silenceEdit.Text())
							if err != nil || sil < 50 {
								walk.MsgBox(dlg, "Invalid silence duration",
									"Silence duration must be a number ≥ 50 (milliseconds).",
									walk.MsgBoxIconError)
								return
							}
							next.SilenceDurationMs = sil
							// Persist + apply.
							if err := next.Save(); err != nil {
								walk.MsgBox(dlg, "Save failed", err.Error(), walk.MsgBoxIconError)
								return
							}
							applyConfigChange(next)
							dlg.Accept()
						},
					},
					PushButton{
						AssignTo:  &cancelBtn,
						Text:      "Cancel",
						OnClicked: func() { dlg.Cancel() },
					},
				},
			},
		},
	}).Create(nil)
	if err != nil {
		return err
	}
	// The dialog is fully built; interactive handlers (live volume apply) may
	// now act.
	dialogReady = true

	// Center on screen and run the modal loop. Run() returns once the dialog
	// is dismissed (OK/Cancel/Esc/X), so this covers every close path.
	dlg.Run()

	// Tear down the level meter if it was running — stop the capture thread and
	// release the device. Safe to call when it's already stopped.
	stopMeter()
	return nil
}

// indexOf returns the position of needle in haystack, or 0 if not found.
// Used to seed ComboBox CurrentIndex from the current config string.
func indexOf(haystack []string, needle string) int {
	for i, s := range haystack {
		if s == needle {
			return i
		}
	}
	return 0
}
