package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Frame is one 4-byte keyboard message on the wire:
// [0x01, down?0x00:0x01, usage, modifierMask]
type Frame struct {
	Down      bool
	Usage     byte
	Modifiers byte
}

// Step is one token of a sequence: either a key chord (one or more frames) or a
// pause.
type Step struct {
	Label  string  `json:"label"`
	Frames []Frame `json:"-"`
	WaitMs int     `json:"waitMs,omitempty"`
}

// Timing controls how a sequence is paced on the wire.
type Timing struct {
	StepDelayMs int `json:"stepDelayMs"`
	HoldMs      int `json:"holdMs"`
}

// ApplyPort substitutes the {port} placeholder. It accepts {port} in any case
// so "{Port}" also works.
func ApplyPort(template string, port int) string {
	if port <= 0 {
		return template
	}
	out := template
	for _, ph := range []string{"{port}", "{Port}", "{PORT}"} {
		out = strings.ReplaceAll(out, ph, strconv.Itoa(port))
	}
	return out
}

// ParseSequence turns a space-separated sequence into steps.
//
// Grammars accepted per token:
//
//	ScrollLock           single key
//	3                    shorthand for Digit3
//	16                   port above 9: expands to Digit1 then Digit6
//	Numpad16             same, but on the numeric keypad
//	Ctrl+Alt+Del         chord: modifiers held while the last key is tapped
//	ControlLeft          a bare modifier, pressed and released on its own
//	                     (this is what "Ctrl ×2" switch hotkeys need)
//	wait:150             pause 150ms
//
// The chord expansion mirrors One-KVM's own frontend implementation
// (executeMacro in web/src/components/VirtualKeyboard.vue): press each modifier
// in order while accumulating the modifier mask, press the main key with the
// full mask, release the main key, then release the modifiers in reverse.
func ParseSequence(raw string) ([]Step, error) {
	rawTokens := strings.Fields(raw)
	if len(rawTokens) == 0 {
		return nil, fmt.Errorf("序列为空")
	}

	// A port above 9 arrives as a single token ("16") because {port} is
	// substituted as plain text, but a KVM switch wants the digits pressed in
	// sequence. Expand before parsing so the rest of the loop is unchanged.
	tokens := make([]string, 0, len(rawTokens))
	for _, t := range rawTokens {
		tokens = append(tokens, expandPortDigits(t)...)
	}

	steps := make([]Step, 0, len(tokens))
	for _, token := range tokens {
		if strings.HasPrefix(strings.ToLower(token), "wait:") {
			msStr := token[len("wait:"):]
			ms, err := strconv.Atoi(msStr)
			if err != nil || ms < 0 {
				return nil, fmt.Errorf("无效的停顿 %q，应为 wait:毫秒数", token)
			}
			steps = append(steps, Step{
				Label:  fmt.Sprintf("停顿 %dms", ms),
				WaitMs: ms,
			})
			continue
		}

		// A chord is "held modifiers + one tapped key". The tapped key is always
		// the last component, and it may itself be a modifier — "Ctrl ×2" style
		// KVM hotkeys need exactly that, a bare Ctrl pressed and released.
		parts := strings.Split(token, "+")
		type modRef struct{ usage, bit byte }

		var held []modRef
		var tapUsage, tapBit byte
		var tapName string

		for i, part := range parts {
			part = strings.TrimSpace(part)
			name, usage, ok := resolveKey(part)
			if !ok {
				return nil, fmt.Errorf("无法识别的按键 %q", part)
			}
			bit, isModifier := modifierBits[usage]

			if i == len(parts)-1 {
				tapUsage, tapName = usage, name
				if isModifier {
					tapBit = bit
				}
				break
			}
			if !isModifier {
				return nil, fmt.Errorf(
					"组合键 %q 里 %q 不是修饰键；只有最后一位可以是非修饰键", token, part)
			}
			held = append(held, modRef{usage, bit})
		}

		frames := make([]Frame, 0, len(held)*2+2)
		mask := byte(0)
		for _, m := range held {
			mask |= m.bit
			frames = append(frames, Frame{Down: true, Usage: m.usage, Modifiers: mask})
		}
		// The tapped key carries its own modifier bit while down, and drops it on
		// release, which is what One-KVM's own keyboard sends for a modifier tap.
		full := mask | tapBit
		frames = append(frames, Frame{Down: true, Usage: tapUsage, Modifiers: full})
		frames = append(frames, Frame{Down: false, Usage: tapUsage, Modifiers: full &^ tapBit})
		for i := len(held) - 1; i >= 0; i-- {
			mask &^= held[i].bit
			frames = append(frames, Frame{Down: false, Usage: held[i].usage, Modifiers: mask})
		}

		stepLabel := tapName
		if len(held) > 0 {
			names := make([]string, 0, len(held)+1)
			for _, m := range held {
				names = append(names, usageName(m.usage))
			}
			names = append(names, tapName)
			stepLabel = strings.Join(names, "+")
		}

		steps = append(steps, Step{Label: stepLabel, Frames: frames})
	}

	return steps, nil
}

// Matches a run of digits, optionally prefixed with Numpad or Digit: "16",
// "Numpad16", "Digit16". A single trailing digit is left alone.
var portDigitRun = regexp.MustCompile(`(?i)^(numpad|digit)?([0-9]{2,})$`)

// expandPortDigits splits a multi-digit port into one token per digit so that
// port 16 becomes Digit1 followed by Digit6.
//
// A token that is already a real key name is never touched, which is what keeps
// F16 (a genuine key) from being mangled into F1 F6.
func expandPortDigits(token string) []string {
	if _, _, ok := resolveKey(token); ok {
		return []string{token}
	}
	m := portDigitRun.FindStringSubmatch(token)
	if m == nil {
		return []string{token}
	}
	prefix, digits := m[1], m[2]
	out := make([]string, 0, len(digits))
	for _, d := range digits {
		out = append(out, prefix+string(d))
	}
	return out
}

func usageName(usage byte) string {
	for name, u := range keyUsages {
		if u == usage {
			return name
		}
	}
	return fmt.Sprintf("0x%02X", usage)
}

// EncodeFrame renders a frame as the 4 bytes One-KVM expects.
func EncodeFrame(f Frame) []byte {
	eventType := byte(0x01) // up
	if f.Down {
		eventType = 0x00
	}
	return []byte{0x01, eventType, f.Usage, f.Modifiers}
}

// EstimatedDurationMs reports roughly how long a sequence will take to send.
func EstimatedDurationMs(steps []Step, t Timing) int {
	total := 0
	for _, s := range steps {
		if s.WaitMs > 0 {
			total += s.WaitMs
			continue
		}
		total += t.HoldMs + t.StepDelayMs
	}
	return total
}
