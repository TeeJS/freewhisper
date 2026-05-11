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
	var keyCombo *walk.ComboBox
	var modCtrl, modAlt, modShift, modWin *walk.CheckBox
	var notifyColor, notifyBeep *walk.CheckBox
	var okBtn, cancelBtn *walk.PushButton
	_ = hotkeyKeyCombo // silence linter — placeholder if we ever swap LineEdit for ComboBox

	// Snapshot the current config so the form starts populated correctly.
	current := appConfig
	hasMod := map[string]bool{}
	for _, m := range current.HotkeyModifiers {
		hasMod[m] = true
	}

	err := (Dialog{
		AssignTo:      &dlg,
		Title:         "FreeWhisper Settings",
		MinSize:       Size{Width: 380, Height: 400},
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

	// Center on screen and run the modal loop.
	dlg.Run()
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
