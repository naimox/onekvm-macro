package main

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func framesOf(t *testing.T, raw string) [][]byte {
	t.Helper()
	steps, err := ParseSequence(raw)
	if err != nil {
		t.Fatalf("解析 %q 失败: %v", raw, err)
	}
	var out [][]byte
	for _, s := range steps {
		for _, f := range s.Frames {
			out = append(out, EncodeFrame(f))
		}
	}
	return out
}

func TestParseScrollLockSwitchSequence(t *testing.T) {
	// The sequence this whole tool exists for: ScrollLock, ScrollLock, 3, Enter.
	got := framesOf(t, "ScrollLock ScrollLock 3 Enter")

	want := [][]byte{
		{0x01, 0x00, 0x47, 0x00}, // ScrollLock down
		{0x01, 0x01, 0x47, 0x00}, // ScrollLock up
		{0x01, 0x00, 0x47, 0x00},
		{0x01, 0x01, 0x47, 0x00},
		{0x01, 0x00, 0x20, 0x00}, // Digit3 down (0x20)
		{0x01, 0x01, 0x20, 0x00},
		{0x01, 0x00, 0x28, 0x00}, // Enter down
		{0x01, 0x01, 0x28, 0x00},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("帧序列不符\n实际: %s\n期望: %s", dump(got), dump(want))
	}
}

func TestParseChordMatchesOneKVMExpansion(t *testing.T) {
	// Mirrors executeMacro() in One-KVM's VirtualKeyboard.vue: modifiers press
	// with an accumulating mask, main key taps with the full mask, modifiers
	// release in reverse.
	got := framesOf(t, "Ctrl+Alt+Del")

	want := [][]byte{
		{0x01, 0x00, 0xe0, 0x01}, // ctrl down,  mask 0x01
		{0x01, 0x00, 0xe2, 0x05}, // alt down,   mask 0x05
		{0x01, 0x00, 0x4c, 0x05}, // del down,   mask 0x05
		{0x01, 0x01, 0x4c, 0x05}, // del up
		{0x01, 0x01, 0xe2, 0x01}, // alt up,     mask 0x01
		{0x01, 0x01, 0xe0, 0x00}, // ctrl up,    mask 0x00
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("组合键展开不符\n实际: %s\n期望: %s", dump(got), dump(want))
	}
}

func TestParseBareModifierTap(t *testing.T) {
	// "Ctrl ×2" style hotkeys press Ctrl on its own. The modifier bit must be
	// set while down and cleared on release, matching One-KVM's own keyboard.
	got := framesOf(t, "ControlLeft ControlLeft Enter")

	want := [][]byte{
		{0x01, 0x00, 0xe0, 0x01}, // ctrl down, bit set
		{0x01, 0x01, 0xe0, 0x00}, // ctrl up, bit cleared
		{0x01, 0x00, 0xe0, 0x01},
		{0x01, 0x01, 0xe0, 0x00},
		{0x01, 0x00, 0x28, 0x00}, // enter
		{0x01, 0x01, 0x28, 0x00},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("独立修饰键展开不符\n实际: %s\n期望: %s", dump(got), dump(want))
	}
}

func TestParseModifierHeldOverAnotherModifier(t *testing.T) {
	// "Ctrl+Alt" holds Ctrl and taps Alt; the tapped modifier drops its own bit
	// on release while Ctrl stays down.
	got := framesOf(t, "Ctrl+Alt")

	want := [][]byte{
		{0x01, 0x00, 0xe0, 0x01}, // ctrl down
		{0x01, 0x00, 0xe2, 0x05}, // alt down, ctrl still held
		{0x01, 0x01, 0xe2, 0x01}, // alt up, ctrl still held
		{0x01, 0x01, 0xe0, 0x00}, // ctrl up
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("修饰键叠加展开不符\n实际: %s\n期望: %s", dump(got), dump(want))
	}
}

func TestParseShorthandsAndCase(t *testing.T) {
	cases := []struct {
		raw       string
		wantUsage byte
	}{
		{"7", 0x24},       // bare digit -> Digit7
		{"Digit7", 0x24},  // canonical name
		{"digit7", 0x24},  // case-insensitive
		{"Numpad7", 0x5f}, // keypad digit
		{"N7", 0x5f},      // keypad shorthand
		{"n7", 0x5f},      // keypad shorthand, lowercase
		{"Esc", 0x29},     // alias -> Escape
		{"esc", 0x29},     // alias, lowercase
		{"ScrollLock", 0x47},
		{"scrolllock", 0x47},
		{"NumpadEnter", 0x58},
		{"Del", 0x4c},
	}
	for _, c := range cases {
		frames := framesOf(t, c.raw)
		if len(frames) != 2 {
			t.Fatalf("%q 期望 2 帧，得到 %d", c.raw, len(frames))
		}
		if frames[0][2] != c.wantUsage || frames[1][2] != c.wantUsage {
			t.Errorf("%q 的键码为 0x%02X，期望 0x%02X", c.raw, frames[0][2], c.wantUsage)
		}
		if frames[0][1] != 0x00 || frames[1][1] != 0x01 {
			t.Errorf("%q 的按下/抬起顺序不对: %v %v", c.raw, frames[0][1], frames[1][1])
		}
	}
}

func TestParseWaitProducesNoFrames(t *testing.T) {
	steps, err := ParseSequence("Escape wait:150 Escape")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(steps) != 3 {
		t.Fatalf("期望 3 步，得到 %d", len(steps))
	}
	if steps[1].WaitMs != 150 || len(steps[1].Frames) != 0 {
		t.Errorf("中间那步应为 150ms 停顿，实际 %+v", steps[1])
	}
}

func TestApplyPort(t *testing.T) {
	cases := []struct {
		template string
		port     int
		want     string
	}{
		{"ScrollLock ScrollLock {port} Enter", 3, "ScrollLock ScrollLock 3 Enter"},
		{"ScrollLock ScrollLock Numpad{port} NumpadEnter", 4, "ScrollLock ScrollLock Numpad4 NumpadEnter"},
		{"{PORT} {Port} {port}", 2, "2 2 2"},
		{"Escape", 3, "Escape"},
		{"ScrollLock {port}", 0, "ScrollLock {port}"}, // port 0 leaves the placeholder
	}
	for _, c := range cases {
		if got := ApplyPort(c.template, c.port); got != c.want {
			t.Errorf("ApplyPort(%q, %d) = %q，期望 %q", c.template, c.port, got, c.want)
		}
	}
}

func TestMultiDigitPortExpandsPerDigit(t *testing.T) {
	// A 16-port switch wants the digits pressed one after another, but {port} is
	// substituted as plain text, so "16" arrives as a single token.
	got := framesOf(t, "ScrollLock ScrollLock 16 Enter")

	want := [][]byte{
		{0x01, 0x00, 0x47, 0x00}, {0x01, 0x01, 0x47, 0x00}, // ScrollLock
		{0x01, 0x00, 0x47, 0x00}, {0x01, 0x01, 0x47, 0x00},
		{0x01, 0x00, 0x1e, 0x00}, {0x01, 0x01, 0x1e, 0x00}, // Digit1
		{0x01, 0x00, 0x23, 0x00}, {0x01, 0x01, 0x23, 0x00}, // Digit6
		{0x01, 0x00, 0x28, 0x00}, {0x01, 0x01, 0x28, 0x00}, // Enter
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("16 通道展开不符\n实际: %s\n期望: %s", dump(got), dump(want))
	}
}

func TestMultiDigitPortOnTheKeypad(t *testing.T) {
	got := framesOf(t, "Numpad16")

	want := [][]byte{
		{0x01, 0x00, 0x59, 0x00}, {0x01, 0x01, 0x59, 0x00}, // Numpad1
		{0x01, 0x00, 0x5e, 0x00}, {0x01, 0x01, 0x5e, 0x00}, // Numpad6
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("小键盘 16 展开不符\n实际: %s\n期望: %s", dump(got), dump(want))
	}
}

func TestRealKeyNamesWithDigitsAreNotSplit(t *testing.T) {
	// F16 is a genuine key (HID usage 0x6f). The digit expansion must not turn it
	// into F1 followed by F6.
	got := framesOf(t, "F16")
	if len(got) != 2 {
		t.Fatalf("F16 应产生 2 帧，实际 %d 帧", len(got))
	}
	if got[0][2] != 0x6b {
		t.Errorf("F16 的键码为 0x%02X，期望 0x6b", got[0][2])
	}

	for _, name := range []string{"Numpad0", "Numpad9", "Digit0", "KeyA"} {
		if frames := framesOf(t, name); len(frames) != 2 {
			t.Errorf("%s 应产生 2 帧，实际 %d 帧", name, len(frames))
		}
	}
}

func TestSingleDigitPortIsUnchanged(t *testing.T) {
	got := framesOf(t, "3")
	want := [][]byte{
		{0x01, 0x00, 0x20, 0x00},
		{0x01, 0x01, 0x20, 0x00},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("个位数通道展开不符\n实际: %s\n期望: %s", dump(got), dump(want))
	}
}

func TestEveryPresetParses(t *testing.T) {
	for _, p := range Presets {
		for port := 1; port <= 64; port++ {
			raw := ApplyPort(p.PortTemplate(), port)
			if _, err := ParseSequence(raw); err != nil {
				t.Errorf("预设 %s 在通道 %d 上解析失败: %v（序列 %q）", p.ID, port, err, raw)
			}
		}
	}
}

func TestSingleLetterAliases(t *testing.T) {
	// Switch manuals spell their hotkeys as "… S Enter", so a bare letter has to
	// resolve to the letter key. Without this the buzzer and auto-scan hotkeys
	// simply cannot be sent.
	cases := map[string]byte{
		"S": 0x16, "s": 0x16,
		"B": 0x05, "b": 0x05,
		"A": 0x04, "z": 0x1d,
	}
	for token, want := range cases {
		frames := framesOf(t, token)
		if len(frames) != 2 {
			t.Fatalf("%q 期望 2 帧，得到 %d", token, len(frames))
		}
		if frames[0][2] != want {
			t.Errorf("%q 的键码为 0x%02X，期望 0x%02X", token, frames[0][2], want)
		}
	}
}

func TestParseErrors(t *testing.T) {
	cases := []struct{ raw, wantSubstring string }{
		{"", "序列为空"},
		{"KeyZZ", "无法识别的按键"},
		{"Ctrl+Del+Enter", "不是修饰键"},
		{"wait:abc", "无效的停顿"},
		{"wait:-5", "无效的停顿"},
	}
	for _, c := range cases {
		_, err := ParseSequence(c.raw)
		if err == nil {
			t.Errorf("ParseSequence(%q) 本应报错", c.raw)
			continue
		}
		if !strings.Contains(err.Error(), c.wantSubstring) {
			t.Errorf("ParseSequence(%q) 的错误为 %q，期望包含 %q", c.raw, err, c.wantSubstring)
		}
	}
}

func TestTOTPCode(t *testing.T) {
	// RFC 6238 test vector: secret "12345678901234567890" (ASCII) in base32,
	// at time 59s the 8-digit code is 94287082, so the 6-digit truncation is 287082.
	// base32("12345678901234567890") = GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ
	code, err := totpCode("GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ", time.Unix(59, 0))
	if err != nil {
		t.Fatalf("生成 TOTP 失败: %v", err)
	}
	if code != "287082" {
		t.Errorf("TOTP = %q，期望 287082", code)
	}
}

func dump(frames [][]byte) string {
	parts := make([]string, 0, len(frames))
	for _, f := range frames {
		parts = append(parts, strings.Join([]string{
			hex(f[0]), hex(f[1]), hex(f[2]), hex(f[3]),
		}, " "))
	}
	return "[" + strings.Join(parts, "] [") + "]"
}

func hex(b byte) string {
	const digits = "0123456789ABCDEF"
	return string([]byte{digits[b>>4], digits[b&0x0f]})
}
