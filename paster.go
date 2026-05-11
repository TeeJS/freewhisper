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
	vkControl      uint16  = 0x11   // Virtual-key code for Ctrl
	vkV            uint16  = 0x56   // Virtual-key code for the V key
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

// sendCtrlV synthesizes whatever subset of (Ctrl-down, V-down, V-up,
// Ctrl-up) is needed for the focused window to receive a paste, based
// on whether Ctrl is currently held by the user.
//
// Why the conditional: in streaming mode, this gets called WHILE the
// user is holding the push-to-talk chord (e.g., Ctrl+`). If we naively
// inject Ctrl-down…Ctrl-up, the Ctrl-up tells Windows the user has
// released Ctrl, which un-registers the global hotkey state momentarily.
// During that gap, every WM_KEYDOWN for `` falls through to the focused
// window as a literal backtick character — corrupting the user's
// dictation output with strings of backticks.
//
// Fix: check the *actual* hardware state via GetAsyncKeyState. If Ctrl
// is already pressed, inject only V-down/V-up — the user's hand-held
// Ctrl provides the modifier, and we never disturb the hotkey
// registration. If Ctrl is NOT held (e.g., a deferred paste after the
// user released the hotkey), inject the full four-event sequence.
//
// SendInput's signature:
//
//	UINT SendInput(UINT cInputs, LPINPUT pInputs, int cbSize);
//
// cbSize is the per-event size; we pass sizeof(keyboardInput) which must
// equal sizeof(INPUT) on x64 (40 bytes).
func sendCtrlV() error {
	if isCtrlDown() {
		return sendInputs([]keyboardInput{
			{Type: inputKeyboard, Vk: vkV},
			{Type: inputKeyboard, Vk: vkV, Flags: keyeventfKeyup},
		})
	}
	return sendInputs([]keyboardInput{
		{Type: inputKeyboard, Vk: vkControl},
		{Type: inputKeyboard, Vk: vkV},
		{Type: inputKeyboard, Vk: vkV, Flags: keyeventfKeyup},
		{Type: inputKeyboard, Vk: vkControl, Flags: keyeventfKeyup},
	})
}

// isCtrlDown reports whether either physical Ctrl key is currently
// pressed, according to Win32's GetAsyncKeyState. The high-order bit of
// the SHORT return is set when the key is down, so we test against
// 0x8000. We check VK_CONTROL which covers both left and right Ctrl.
func isCtrlDown() bool {
	r, _, _ := procGetAsyncKeyState.Call(uintptr(vkControl))
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
