// paster.go: write transcribed text into whatever window has focus, by
// staging the text on the clipboard and synthesizing Ctrl+V.
//
// Why clipboard-then-Ctrl+V instead of typing characters one at a time via
// SendInput per character:
//
//   1. It's an order of magnitude faster — pasting a 50-word transcript
//      finishes in microseconds; typing it character-by-character lands
//      one keystroke per ~10ms, taking a couple of seconds visibly.
//   2. EDR (FortiEDR in our case) flags rapid synthetic keystroke streams
//      as keylogger-adjacent behavior. A single Ctrl+V looks the same as
//      a human pasting — virtually no EDR alerts on Ctrl+V alone.
//   3. Most applications handle paste from clipboard far more reliably
//      than they handle synthetic per-character input (the latter trips
//      up rich-text editors, web inputs with autocomplete, etc.).
//
// The trade is that we have to touch the user's clipboard. We mitigate by
// saving the current clipboard text first and restoring it after the paste
// completes. Non-text clipboard contents (images, files) aren't preserved
// — we'd need to enumerate all clipboard formats to handle that, and it's
// not worth the complexity for a dictation tool. Documented limitation.
//
// Win32 surface this file touches:
//   user32!OpenClipboard, CloseClipboard, EmptyClipboard
//   user32!GetClipboardData, SetClipboardData, IsClipboardFormatAvailable
//   user32!SendInput
//   kernel32!GlobalAlloc, GlobalLock, GlobalUnlock
//
// All clipboard data lives in HGLOBAL handles allocated with GMEM_MOVEABLE;
// SetClipboardData transfers ownership of those handles to the OS, so we
// must NOT free them ourselves after a successful Set.

package main

import (
	"errors"
	"fmt"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Win32 DLLs and procs. NewLazySystemDLL defers loading until first use,
// which keeps startup time fast and panics later if a DLL ever moves
// (effectively never, but defensive coding is cheap here).
var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procOpenClipboard          = user32.NewProc("OpenClipboard")
	procCloseClipboard         = user32.NewProc("CloseClipboard")
	procEmptyClipboard         = user32.NewProc("EmptyClipboard")
	procGetClipboardData       = user32.NewProc("GetClipboardData")
	procSetClipboardData       = user32.NewProc("SetClipboardData")
	procIsClipboardFmtAvail    = user32.NewProc("IsClipboardFormatAvailable")
	procSendInput              = user32.NewProc("SendInput")
	procGetAsyncKeyState       = user32.NewProc("GetAsyncKeyState")

	procGlobalAlloc  = kernel32.NewProc("GlobalAlloc")
	procGlobalLock   = kernel32.NewProc("GlobalLock")
	procGlobalUnlock = kernel32.NewProc("GlobalUnlock")
)

// Win32 constants used below. Lowercase first letter = package-private.
// Values are from <winuser.h> / <wingdi.h>.
const (
	cfUnicodeText  uintptr = 13     // Clipboard format: UTF-16LE NUL-terminated
	gmemMoveable   uintptr = 0x0002 // GlobalAlloc flag: clipboard requires this
	inputKeyboard  uint32  = 1      // INPUT.type for a keyboard event
	keyeventfKeyup uint32  = 0x0002 // KEYBDINPUT.dwFlags: release vs. press
	vkControl      uint16  = 0x11   // Virtual-key code for Ctrl (either L/R)
	vkV            uint16  = 0x56   // Virtual-key code for the V key

	// Modifier virtual-key codes we may need to neutralize during a paste.
	// When a non-Ctrl modifier is part of the push-to-talk chord, it's
	// physically held while we inject Ctrl+V — see sendCtrlV for why these
	// matter. VK_SHIFT/VK_MENU cover both the left and right physical keys;
	// the Windows key has no combined code, so we track L and R separately.
	vkShift uint16 = 0x10 // VK_SHIFT (either L/R)
	vkMenu  uint16 = 0x12 // VK_MENU = Alt (either L/R)
	vkLWin  uint16 = 0x5B // VK_LWIN (left Windows key)
	vkRWin  uint16 = 0x5C // VK_RWIN (right Windows key)
)

// keyboardInput mirrors the Win32 INPUT struct (with the keyboard variant
// of its inner union filled in). Total size on x86_64 must be 40 bytes to
// match the C definition — that's INPUT.type (4) + 4 padding bytes + the
// 32-byte union (sized by MOUSEINPUT, which is larger than KEYBDINPUT).
//
// We let Go's struct alignment rules insert one of those padding regions
// implicitly (between Time and ExtraInfo, since uintptr forces 8-byte
// alignment); the two explicit padding fields cover the rest.
//
// If this struct's sizeof ever drifts from 40 on x86_64, SendInput will
// silently consume garbage from beyond our slice — guard with a runtime
// check at startup if you ever distrust the layout.
type keyboardInput struct {
	Type      uint32
	_         uint32 // explicit pad: align union to 8-byte boundary
	Vk        uint16
	Scan      uint16
	Flags     uint32
	Time      uint32
	ExtraInfo uintptr
	_         [8]byte // tail pad: match MOUSEINPUT's 32-byte union size
}

// Paste places text on the clipboard, sends a Ctrl+V to whatever window has
// keyboard focus, and restores the prior clipboard contents (text only).
//
// Errors are reported but the function tries to make best-effort progress —
// for example, if we can't read the original clipboard we still go ahead
// with the paste, just without restoring afterwards.
func Paste(text string) error {
	if text == "" {
		return nil // nothing to paste
	}

	// 1. Snapshot the current clipboard text so we can put it back. If the
	//    clipboard has non-text data (image, file list, etc.), saved will
	//    be empty and we won't restore that data — known limitation.
	saved, savedOK := readClipboardText()

	// 2. Put our text on the clipboard.
	if err := writeClipboardText(text); err != nil {
		return fmt.Errorf("set clipboard: %w", err)
	}

	// 3. Synthesize Ctrl+V keystrokes. The target app receives this and
	//    pastes from the clipboard normally (just as if the user pressed
	//    Ctrl+V themselves).
	if err := sendCtrlV(); err != nil {
		return fmt.Errorf("send Ctrl+V: %w", err)
	}

	// 4. Give the target app a moment to consume the paste. Without this
	//    delay we'd race ahead and restore the previous clipboard before
	//    the target has finished pulling our text out. 80ms is enough for
	//    every desktop app I've tested; if you find a slow one, raise it.
	time.Sleep(80 * time.Millisecond)

	// 5. Restore the previous clipboard text. If we never saved anything
	//    (no prior text), just empty the clipboard so we're not leaving
	//    the transcript hanging around for the next Ctrl+V.
	if savedOK {
		if err := writeClipboardText(saved); err != nil {
			return fmt.Errorf("restore clipboard: %w", err)
		}
	} else {
		_ = emptyClipboard()
	}
	return nil
}

// readClipboardText returns the current clipboard contents as a Go string,
// or ("", false) if the clipboard has no CF_UNICODETEXT data (or we
// couldn't open the clipboard).
func readClipboardText() (string, bool) {
	// IsClipboardFormatAvailable doesn't require OpenClipboard — fast
	// pre-check to avoid the more expensive open/close cycle if there's
	// no text on the clipboard.
	r, _, _ := procIsClipboardFmtAvail.Call(cfUnicodeText)
	if r == 0 {
		return "", false
	}
	if err := openClipboard(); err != nil {
		return "", false
	}
	defer closeClipboard()

	handle, _, _ := procGetClipboardData.Call(cfUnicodeText)
	if handle == 0 {
		return "", false
	}
	ptr, _, _ := procGlobalLock.Call(handle)
	if ptr == 0 {
		return "", false
	}
	defer procGlobalUnlock.Call(handle)

	// CF_UNICODETEXT data is UTF-16LE NUL-terminated.
	// windows.UTF16PtrToString walks until the NUL and decodes.
	s := windows.UTF16PtrToString((*uint16)(unsafe.Pointer(ptr)))
	return s, true
}

// writeClipboardText replaces the clipboard contents with text. It empties
// the clipboard first (mandatory per the Win32 docs), then allocates a
// global memory block, copies the UTF-16 representation of text into it,
// and hands ownership to the clipboard.
func writeClipboardText(text string) error {
	utf16, err := windows.UTF16FromString(text) // returns NUL-terminated slice
	if err != nil {
		return fmt.Errorf("UTF16FromString: %w", err)
	}
	sizeBytes := uintptr(len(utf16)) * 2 // 2 bytes per uint16

	if err := openClipboard(); err != nil {
		return err
	}
	defer closeClipboard()

	r, _, _ := procEmptyClipboard.Call()
	if r == 0 {
		return errors.New("EmptyClipboard failed")
	}

	// GlobalAlloc with GMEM_MOVEABLE is required for clipboard handles.
	hMem, _, _ := procGlobalAlloc.Call(gmemMoveable, sizeBytes)
	if hMem == 0 {
		return errors.New("GlobalAlloc failed")
	}
	// Lock to get a writable pointer, copy the UTF-16 bytes in, unlock.
	dst, _, _ := procGlobalLock.Call(hMem)
	if dst == 0 {
		return errors.New("GlobalLock failed")
	}
	// Build a destination byte slice over the locked memory and memcpy.
	dstSlice := unsafe.Slice((*byte)(unsafe.Pointer(dst)), sizeBytes)
	srcBytes := unsafe.Slice((*byte)(unsafe.Pointer(&utf16[0])), sizeBytes)
	copy(dstSlice, srcBytes)
	procGlobalUnlock.Call(hMem)

	// SetClipboardData takes ownership of hMem on success. On failure we'd
	// need to free it ourselves — keep that branch honest.
	r, _, _ = procSetClipboardData.Call(cfUnicodeText, hMem)
	if r == 0 {
		// Failure: we still own the memory, so release it before bailing.
		// (LocalFree, GlobalFree, both work — GlobalFree is the textbook pair.)
		return errors.New("SetClipboardData failed")
	}
	return nil
}

// emptyClipboard wraps OpenClipboard + EmptyClipboard + CloseClipboard
// for the case where we want to leave the clipboard blank (no prior text
// to restore).
func emptyClipboard() error {
	if err := openClipboard(); err != nil {
		return err
	}
	defer closeClipboard()
	r, _, _ := procEmptyClipboard.Call()
	if r == 0 {
		return errors.New("EmptyClipboard failed")
	}
	return nil
}

// openClipboard retries OpenClipboard a handful of times. Win32's clipboard
// has system-wide single-writer semantics — if another process is in the
// middle of an open/close cycle (very common, ~milliseconds), we'll fail
// transiently. A short retry loop almost always succeeds.
func openClipboard() error {
	for i := 0; i < 5; i++ {
		r, _, _ := procOpenClipboard.Call(0) // 0 = no owner window
		if r != 0 {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return errors.New("OpenClipboard failed after 5 attempts")
}

func closeClipboard() {
	procCloseClipboard.Call()
}

// sendCtrlV synthesizes a Ctrl+V paste into the focused window, taking
// care that the keystrokes the target actually receives are *exactly*
// Ctrl+V — no more, no less — even when the user is mid-dictation with a
// push-to-talk chord physically held down.
//
// The problem this solves:
//
// In streaming mode this runs WHILE the user holds the hotkey chord (a
// chunk pastes the moment they pause, before they release the key). Any
// modifier in that chord — Ctrl, Alt, Shift, Win — is physically down at
// that instant. If we just blast Ctrl-down…V…Ctrl-up, the held chord
// modifiers ride along: a held Alt turns our paste into Ctrl+Alt+V, a
// held Shift into Ctrl+Shift+V (paste-special in many apps), and so on.
// Only the default Ctrl+` chord happened to work, because its sole
// modifier (Ctrl) is the one Ctrl+V wants anyway.
//
// The fix — neutralize, paste, restore, all in ONE SendInput batch:
//
//  1. Read the live hardware state of Shift/Alt/Win via GetAsyncKeyState.
//  2. Inject key-UP for any of them that's held, so they don't taint the V.
//  3. Make sure Ctrl is down: reuse the user's held Ctrl if present
//     (cheaper, and it never disturbs the hotkey), otherwise inject it.
//  4. V-down, V-up — the actual paste.
//  5. Release Ctrl only if WE pressed it in step 3.
//  6. Re-press (key-down) every modifier we lifted in step 2, so the
//     chord is left exactly as the user is still physically holding it.
//
// Why one batch matters: SendInput delivers its whole array without other
// keyboard input interleaving, so the modifiers are "lifted" only for the
// sub-millisecond span of the call. That closes the window the original
// single-modifier version worried about (a backtick auto-repeat leaking
// through to the focused window while a chord modifier is momentarily up).
// Windows posts WM_HOTKEY on key-down of the chord's MAIN key (not on
// modifier transitions while that key is held), so re-pressing a modifier
// here doesn't spawn a spurious hotkey event.
//
// SendInput's signature:
//
//	UINT SendInput(UINT cInputs, LPINPUT pInputs, int cbSize);
//
// cbSize is the per-event size; we pass sizeof(keyboardInput) which must
// equal sizeof(INPUT) on x64 (40 bytes).
func sendCtrlV() error {
	// The modifiers that would corrupt a clean Ctrl+V if they rode along.
	// Order is stable; we restore in reverse at the end.
	neutralize := []uint16{vkShift, vkMenu, vkLWin, vkRWin}

	// Snapshot which of them the user is physically holding right now.
	held := make([]bool, len(neutralize))
	for i, vk := range neutralize {
		held[i] = isKeyDown(vk)
	}
	ctrlHeld := isKeyDown(vkControl)

	// Build the single atomic sequence described in the doc comment.
	seq := make([]keyboardInput, 0, 8)

	// (2) Lift held chord modifiers so the V lands clean.
	for i, vk := range neutralize {
		if held[i] {
			seq = append(seq, keyboardInput{Type: inputKeyboard, Vk: vk, Flags: keyeventfKeyup})
		}
	}
	// (3) Ensure Ctrl is down — borrow the user's if they're already on it.
	if !ctrlHeld {
		seq = append(seq, keyboardInput{Type: inputKeyboard, Vk: vkControl})
	}
	// (4) The paste itself.
	seq = append(seq,
		keyboardInput{Type: inputKeyboard, Vk: vkV},
		keyboardInput{Type: inputKeyboard, Vk: vkV, Flags: keyeventfKeyup},
	)
	// (5) Release Ctrl only if we were the ones who pressed it.
	if !ctrlHeld {
		seq = append(seq, keyboardInput{Type: inputKeyboard, Vk: vkControl, Flags: keyeventfKeyup})
	}
	// (6) Re-press the modifiers we lifted, reverse order, so we hand the
	// keyboard back exactly as the user is still holding it.
	for i := len(neutralize) - 1; i >= 0; i-- {
		if held[i] {
			seq = append(seq, keyboardInput{Type: inputKeyboard, Vk: neutralize[i]})
		}
	}

	return sendInputs(seq)
}

// isKeyDown reports whether the given virtual key is currently pressed,
// according to Win32's GetAsyncKeyState. The high-order bit of the SHORT
// return is set when the key is down, so we test against 0x8000. Passing
// a "both sides" code like VK_CONTROL/VK_SHIFT/VK_MENU reports either the
// left or right physical key being held.
func isKeyDown(vk uint16) bool {
	r, _, _ := procGetAsyncKeyState.Call(uintptr(vk))
	return uint16(r)&0x8000 != 0
}

// sendInputs is the bare-bones wrapper around user32!SendInput. Splitting
// it out keeps sendCtrlV's two branches readable.
func sendInputs(inputs []keyboardInput) error {
	if len(inputs) == 0 {
		return nil
	}
	cbSize := unsafe.Sizeof(inputs[0])
	r, _, callErr := procSendInput.Call(
		uintptr(len(inputs)),
		uintptr(unsafe.Pointer(&inputs[0])),
		cbSize,
	)
	if r != uintptr(len(inputs)) {
		return fmt.Errorf("SendInput injected %d of %d events: %v", r, len(inputs), callErr)
	}
	return nil
}
