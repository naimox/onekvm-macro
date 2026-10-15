package main

import "strings"

// HID usage codes, copied verbatim from One-KVM's frontend table in
// web/src/lib/keyboardMappings.ts. The byte sent in a keyboard frame is this
// usage code, not the canonical key name.
var keyUsages = map[string]byte{
	"KeyA": 0x04, "KeyB": 0x05, "KeyC": 0x06, "KeyD": 0x07, "KeyE": 0x08,
	"KeyF": 0x09, "KeyG": 0x0a, "KeyH": 0x0b, "KeyI": 0x0c, "KeyJ": 0x0d,
	"KeyK": 0x0e, "KeyL": 0x0f, "KeyM": 0x10, "KeyN": 0x11, "KeyO": 0x12,
	"KeyP": 0x13, "KeyQ": 0x14, "KeyR": 0x15, "KeyS": 0x16, "KeyT": 0x17,
	"KeyU": 0x18, "KeyV": 0x19, "KeyW": 0x1a, "KeyX": 0x1b, "KeyY": 0x1c,
	"KeyZ": 0x1d,

	"Digit1": 0x1e, "Digit2": 0x1f, "Digit3": 0x20, "Digit4": 0x21,
	"Digit5": 0x22, "Digit6": 0x23, "Digit7": 0x24, "Digit8": 0x25,
	"Digit9": 0x26, "Digit0": 0x27,

	"Enter": 0x28, "Escape": 0x29, "Backspace": 0x2a, "Tab": 0x2b,
	"Space": 0x2c, "Minus": 0x2d, "Equal": 0x2e, "BracketLeft": 0x2f,
	"BracketRight": 0x30, "Backslash": 0x31, "Semicolon": 0x33, "Quote": 0x34,
	"Backquote": 0x35, "Comma": 0x36, "Period": 0x37, "Slash": 0x38,
	"CapsLock": 0x39,

	"F1": 0x3a, "F2": 0x3b, "F3": 0x3c, "F4": 0x3d, "F5": 0x3e, "F6": 0x3f,
	"F7": 0x40, "F8": 0x41, "F9": 0x42, "F10": 0x43, "F11": 0x44, "F12": 0x45,
	"PrintScreen": 0x46, "ScrollLock": 0x47, "Pause": 0x48, "Insert": 0x49,
	"Home": 0x4a, "PageUp": 0x4b, "Delete": 0x4c, "End": 0x4d,
	"PageDown": 0x4e, "ArrowRight": 0x4f, "ArrowLeft": 0x50,
	"ArrowDown": 0x51, "ArrowUp": 0x52,

	"NumLock": 0x53, "NumpadDivide": 0x54, "NumpadMultiply": 0x55,
	"NumpadSubtract": 0x56, "NumpadAdd": 0x57, "NumpadEnter": 0x58,
	"Numpad1": 0x59, "Numpad2": 0x5a, "Numpad3": 0x5b, "Numpad4": 0x5c,
	"Numpad5": 0x5d, "Numpad6": 0x5e, "Numpad7": 0x5f, "Numpad8": 0x60,
	"Numpad9": 0x61, "Numpad0": 0x62, "NumpadDecimal": 0x63,

	"IntlBackslash": 0x64, "ContextMenu": 0x65,
	"F13": 0x68, "F14": 0x69, "F15": 0x6a, "F16": 0x6b, "F17": 0x6c,
	"F18": 0x6d, "F19": 0x6e, "F20": 0x6f, "F21": 0x70, "F22": 0x71,
	"F23": 0x72, "F24": 0x73,

	"ControlLeft": 0xe0, "ShiftLeft": 0xe1, "AltLeft": 0xe2, "MetaLeft": 0xe3,
	"ControlRight": 0xe4, "ShiftRight": 0xe5, "AltRight": 0xe6,
	"MetaRight": 0xe7,
}

// Modifier bit in the frame's 4th byte, keyed by HID usage code.
var modifierBits = map[byte]byte{
	0xe0: 0x01, // left ctrl
	0xe1: 0x02, // left shift
	0xe2: 0x04, // left alt
	0xe3: 0x08, // left meta
	0xe4: 0x10, // right ctrl
	0xe5: 0x20, // right shift
	0xe6: 0x40, // right alt
	0xe7: 0x80, // right meta
}

// Shorthands so sequences stay readable. Keys are compared lowercased.
var keyAliases = map[string]string{
	"ctrl": "ControlLeft", "control": "ControlLeft", "ctrlleft": "ControlLeft",
	"ctrlright": "ControlRight", "shift": "ShiftLeft", "shiftleft": "ShiftLeft",
	"shiftright": "ShiftRight", "alt": "AltLeft", "altleft": "AltLeft",
	"altright": "AltRight", "win": "MetaLeft", "meta": "MetaLeft",
	"super": "MetaLeft", "cmd": "MetaLeft", "winleft": "MetaLeft",
	"winright": "MetaRight", "metaright": "MetaRight",

	"del": "Delete", "ins": "Insert", "esc": "Escape", "return": "Enter",
	"bksp": "Backspace", "back": "Backspace", "spacebar": "Space",
	"scrllock": "ScrollLock", "scroll": "ScrollLock", "print": "PrintScreen",
	"pagedn": "PageDown", "pgdn": "PageDown", "pageup": "PageUp", "pgup": "PageUp",
	"numenter": "NumpadEnter", "npenter": "NumpadEnter",
}

func init() {
	// Bare digits 0-9 map to the top-row number keys. "3" and "Digit3" are the
	// same thing, which keeps templates such as "ScrollLock ScrollLock {port} Enter"
	// readable no matter how the port number arrives.
	for _, d := range "0123456789" {
		keyAliases[string(d)] = "Digit" + string(d)
	}
	// Single letters map to their key, so a switch hotkey like "… S Enter" can be
	// written the way the manual spells it instead of "… KeyS Enter".
	for _, c := range "abcdefghijklmnopqrstuvwxyz" {
		keyAliases[string(c)] = "Key" + strings.ToUpper(string(c))
	}
	// Numpad shorthands: N1..N9, N0.
	for _, d := range "0123456789" {
		keyAliases["n"+string(d)] = "Numpad" + string(d)
	}
	keyAliases["numenter"] = "NumpadEnter"
	keyAliases["decimal"] = "NumpadDecimal"
}

// resolveKey turns a user-typed token into a canonical key name and its HID
// usage code. Lookup is case-insensitive.
func resolveKey(token string) (name string, usage byte, ok bool) {
	if token == "" {
		return "", 0, false
	}
	if canonical, ok := keyAliases[strings.ToLower(token)]; ok {
		usage, found := keyUsages[canonical]
		return canonical, usage, found
	}
	// Canonical names are matched case-insensitively too, so "scrolllock" works.
	for canonical, usage := range keyUsages {
		if strings.EqualFold(canonical, token) {
			return canonical, usage, true
		}
	}
	return "", 0, false
}

// KeyNameList returns every canonical key name, for the page's autocomplete.
func KeyNameList() []string {
	names := make([]string, 0, len(keyUsages))
	for name := range keyUsages {
		names = append(names, name)
	}
	return names
}
