package main

import (
	"sync"
)

// HotkeyPreset describes how a switch expects its hotkeys to be introduced.
//
// Every hotkey on a given switch shares the same lead-in, so the presets are
// modelled as a prefix plus the parts that vary. That way picking a preset
// changes port switching and the function keys together, instead of leaving the
// function keys pointing at the old prefix.
type HotkeyPreset struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Prefix string `json:"prefix"`
	// Digits is "" for the top-row number keys, or "Numpad" for the keypad ones.
	Digits string `json:"digits"`
	// Enter is the key that confirms the command: "Enter" or "NumpadEnter".
	Enter string `json:"enter"`
	Note  string `json:"note,omitempty"`
}

// PortTemplate is the full sequence for switching to {port}.
func (p HotkeyPreset) PortTemplate() string {
	return p.Prefix + " " + p.Digits + "{port} " + p.Enter
}

// FunctionTemplate is the sequence for one of the "double-tap prefix, then a
// letter, then confirm" hotkeys.
func (p HotkeyPreset) FunctionTemplate(key string) string {
	return p.Prefix + " " + key + " " + p.Enter
}

// Presets covers the hotkey styles seen on hotkey-switchable KVM switches.
// Which one a given switch wants cannot be detected, so the page lets you try
// them in turn.
var Presets = []HotkeyPreset{
	{
		ID:     "scrolllock",
		Name:   "ScrollLock ×2",
		Prefix: "ScrollLock ScrollLock",
		Digits: "",
		Enter:  "Enter",
		Note:   "最常见的写法，先试这个",
	},
	{
		ID:     "scrolllock-numpad",
		Name:   "ScrollLock ×2 + 小键盘",
		Prefix: "ScrollLock ScrollLock",
		Digits: "Numpad",
		Enter:  "NumpadEnter",
		Note:   "部分切换器只认小键盘，且要求 NumLock 亮着",
	},
	{
		ID:     "ctrl",
		Name:   "Ctrl ×2",
		Prefix: "ControlLeft ControlLeft",
		Digits: "",
		Enter:  "Enter",
	},
	{
		ID:     "ctrl-numpad",
		Name:   "Ctrl ×2 + 小键盘",
		Prefix: "ControlLeft ControlLeft",
		Digits: "Numpad",
		Enter:  "NumpadEnter",
	},
	{
		ID:     "alt",
		Name:   "Alt ×2",
		Prefix: "AltLeft AltLeft",
		Digits: "",
		Enter:  "Enter",
	},
}

func FindPreset(id string) (HotkeyPreset, bool) {
	for _, p := range Presets {
		if p.ID == id {
			return p, true
		}
	}
	return HotkeyPreset{}, false
}

// DefaultPreset is what a request uses when it names neither a preset nor a
// template.
func DefaultPreset() HotkeyPreset {
	return Presets[0]
}

// SwitchFunction is one of the on/off features a KVM switch exposes over its
// hotkeys.
type SwitchFunction struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Key is the letter in the middle of the sequence, spelled the way the
	// switch manual does.
	Key  string `json:"key"`
	Note string `json:"note,omitempty"`
}

// Functions are the toggle-style hotkeys. Switching ports is deliberately not
// one of them: a port is a choice among many, not a switch to flip.
var Functions = []SwitchFunction{
	{
		ID:   "autoscan",
		Name: "自动扫描",
		Key:  "S",
		Note: "切换器定时轮询各个通道的画面",
	},
	{
		ID:   "buzzer",
		Name: "蜂鸣器",
		Key:  "B",
		Note: "切换时是否发出提示音",
	},
}

func FindFunction(id string) (SwitchFunction, bool) {
	for _, f := range Functions {
		if f.ID == id {
			return f, true
		}
	}
	return SwitchFunction{}, false
}

// Toggle states. "unknown" is a real state, not a placeholder: a KVM switch
// never reports whether its buzzer is on, so after toggling from an unknown
// state it is still unknown. Pretending otherwise would show the user a value
// that is wrong half the time.
const (
	StateUnknown = "unknown"
	StateOn      = "on"
	StateOff     = "off"
)

// functionStates is the shared record of what each toggle was last set to.
// It lives on the server so every logged-in browser sees the same value.
type functionStates struct {
	mu     sync.Mutex
	states map[string]string
}

func newFunctionStates() *functionStates {
	s := &functionStates{states: make(map[string]string, len(Functions))}
	for _, f := range Functions {
		s.states[f.ID] = StateUnknown
	}
	return s
}

func (s *functionStates) get(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st, ok := s.states[id]; ok {
		return st
	}
	return StateUnknown
}

func (s *functionStates) set(id, state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.states[id] = state
}

// toggle flips a known state and leaves an unknown one unknown.
func (s *functionStates) toggle(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.states[id] {
	case StateOn:
		s.states[id] = StateOff
	case StateOff:
		s.states[id] = StateOn
	default:
		s.states[id] = StateUnknown
	}
	return s.states[id]
}

func (s *functionStates) snapshot() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]string, len(s.states))
	for k, v := range s.states {
		out[k] = v
	}
	return out
}

func validState(state string) bool {
	return state == StateOn || state == StateOff
}

// PresetStatus is the view of a hotkey preset sent to the page.
type PresetStatus struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	PortTemplate string `json:"portTemplate"`
	Note         string `json:"note,omitempty"`
}

func presetStatuses() []PresetStatus {
	out := make([]PresetStatus, 0, len(Presets))
	for _, p := range Presets {
		out = append(out, PresetStatus{
			ID:           p.ID,
			Name:         p.Name,
			PortTemplate: p.PortTemplate(),
			Note:         p.Note,
		})
	}
	return out
}

// FunctionStatus is the view of a function sent to the page.
type FunctionStatus struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Key      string `json:"key"`
	Note     string `json:"note,omitempty"`
	State    string `json:"state"`
	Template string `json:"template"`
}

// functionStatuses resolves each function against a preset (and any per-function
// template override) for the status endpoint.
func functionStatuses(states map[string]string, preset HotkeyPreset, overrides map[string]string) []FunctionStatus {
	out := make([]FunctionStatus, 0, len(Functions))
	for _, f := range Functions {
		state, ok := states[f.ID]
		if !ok {
			state = StateUnknown
		}
		template := preset.FunctionTemplate(f.Key)
		if custom, ok := overrides[f.ID]; ok && custom != "" {
			template = custom
		}
		out = append(out, FunctionStatus{
			ID:       f.ID,
			Name:     f.Name,
			Key:      f.Key,
			Note:     f.Note,
			State:    state,
			Template: template,
		})
	}
	return out
}
