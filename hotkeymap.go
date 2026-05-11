// hotkeymap.go: convert human-friendly hotkey names (from config.json) to
// golang.design/x/hotkey's Modifier and Key values.
//
// We keep these tables in one place so the settings GUI (settings.go) and
// the hotkey-registration code (main.go) agree on what's valid. If a user
// hand-edits config.json with a key name we don't recognize, ParseKey
// returns an error and the app logs it instead of crashing.
//
// Friendly names also serve as the source of truth for the settings GUI's
// dropdowns — the GUI iterates AllowedKeys / AllowedModifiers to build
// its lists.

package main

import (
	"fmt"
	"strings"

	"golang.design/x/hotkey"
)

// modifierByName maps lowercased modifier names to hotkey.Modifier values.
// Sorted by typical preference order: Ctrl > Alt > Shift > Win.
var modifierByName = map[string]hotkey.Modifier{
	"ctrl":  hotkey.ModCtrl,
	"alt":   hotkey.ModAlt,
	"shift": hotkey.ModShift,
	"win":   hotkey.ModWin,
}

// AllowedModifiers is the ordered list of modifier names the settings GUI
// surfaces as checkboxes. The order matches modifierByName above.
var AllowedModifiers = []string{"Ctrl", "Alt", "Shift", "Win"}

// keyByName maps lowercased key names to hotkey.Key values. The Win32
// virtual-key codes are stable across Windows versions — the OEM range
// (0xBA–0xDF) covers punctuation that varies by keyboard layout, which
// is why we name them friendly-style here ("Backtick" not "VK_OEM_3").
//
// Letters A–Z map to ASCII codes 0x41–0x5A; digits 0–9 to 0x30–0x39.
// F1–F12 are 0x70–0x7B. Space is 0x20.
var keyByName = func() map[string]hotkey.Key {
	m := map[string]hotkey.Key{
		"space":    hotkey.KeySpace,
		"backtick": 0xC0, // VK_OEM_3 — `~ key, not in the hotkey package's public Keys
		"`":        0xC0, // accept the literal too, for hand-editors
	}
	// Letters
	for i, name := byte(0), byte('A'); name <= 'Z'; i, name = i+1, name+1 {
		m[strings.ToLower(string(name))] = hotkey.Key(0x41 + i)
	}
	// Digits
	for i := 0; i < 10; i++ {
		m[fmt.Sprintf("%d", i)] = hotkey.Key(0x30 + i)
	}
	// Function keys F1–F12
	for i := 1; i <= 12; i++ {
		m[strings.ToLower(fmt.Sprintf("F%d", i))] = hotkey.Key(0x70 + i - 1)
	}
	return m
}()

// AllowedKeys is the ordered display list for the settings GUI dropdown.
// Order is "natural" for human scanning: punctuation/space first, then
// letters A-Z, digits 0-9, then F-keys.
var AllowedKeys = func() []string {
	out := []string{"Backtick", "Space"}
	for c := byte('A'); c <= 'Z'; c++ {
		out = append(out, string(c))
	}
	for i := 0; i < 10; i++ {
		out = append(out, fmt.Sprintf("%d", i))
	}
	for i := 1; i <= 12; i++ {
		out = append(out, fmt.Sprintf("F%d", i))
	}
	return out
}()

// ParseModifiers turns []string{"Ctrl","Shift"} into the matching slice of
// hotkey.Modifier values. Case-insensitive. Returns an error on the first
// unrecognized name (no partial parsing — better to fail loudly).
func ParseModifiers(names []string) ([]hotkey.Modifier, error) {
	mods := make([]hotkey.Modifier, 0, len(names))
	for _, n := range names {
		m, ok := modifierByName[strings.ToLower(strings.TrimSpace(n))]
		if !ok {
			return nil, fmt.Errorf("unknown modifier %q (valid: %s)", n, strings.Join(AllowedModifiers, ", "))
		}
		mods = append(mods, m)
	}
	return mods, nil
}

// ParseKey turns a friendly key name into a hotkey.Key. Case-insensitive.
func ParseKey(name string) (hotkey.Key, error) {
	k, ok := keyByName[strings.ToLower(strings.TrimSpace(name))]
	if !ok {
		return 0, fmt.Errorf("unknown key %q", name)
	}
	return k, nil
}

// HotkeyDisplay formats a hotkey for the log / tooltip ("Ctrl+Shift+`").
// Pure cosmetic — the registration uses the parsed values directly.
func HotkeyDisplay(mods []string, key string) string {
	if len(mods) == 0 {
		return key
	}
	return strings.Join(mods, "+") + "+" + key
}
