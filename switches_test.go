package main

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// The presets were refactored from whole templates into a prefix plus parts.
// This pins the resulting port sequences so the refactor cannot quietly change
// what goes on the wire.
func TestPresetPortTemplatesAreUnchanged(t *testing.T) {
	want := map[string]string{
		"scrolllock":        "ScrollLock ScrollLock {port} Enter",
		"scrolllock-numpad": "ScrollLock ScrollLock Numpad{port} NumpadEnter",
		"ctrl":              "ControlLeft ControlLeft {port} Enter",
		"ctrl-numpad":       "ControlLeft ControlLeft Numpad{port} NumpadEnter",
		"alt":               "AltLeft AltLeft {port} Enter",
	}
	for id, wantTemplate := range want {
		p, ok := FindPreset(id)
		if !ok {
			t.Errorf("找不到预设 %s", id)
			continue
		}
		if got := p.PortTemplate(); got != wantTemplate {
			t.Errorf("预设 %s 的通道模板为 %q，期望 %q", id, got, wantTemplate)
		}
	}
	if len(Presets) != len(want) {
		t.Errorf("预设数量为 %d，期望 %d", len(Presets), len(want))
	}
}

func TestFunctionTemplatesFollowThePreset(t *testing.T) {
	// The whole point of the prefix/suffix split: picking a different preset has
	// to move the function hotkeys too, otherwise the page would switch ports
	// with Ctrl ×2 while toggling the buzzer with ScrollLock ×2.
	cases := []struct {
		preset string
		key    string
		want   string
	}{
		{"scrolllock", "S", "ScrollLock ScrollLock S Enter"},
		{"scrolllock", "B", "ScrollLock ScrollLock B Enter"},
		{"scrolllock-numpad", "S", "ScrollLock ScrollLock S NumpadEnter"},
		{"ctrl", "B", "ControlLeft ControlLeft B Enter"},
		{"ctrl-numpad", "S", "ControlLeft ControlLeft S NumpadEnter"},
		{"alt", "B", "AltLeft AltLeft B Enter"},
	}
	for _, c := range cases {
		p, ok := FindPreset(c.preset)
		if !ok {
			t.Fatalf("找不到预设 %s", c.preset)
		}
		if got := p.FunctionTemplate(c.key); got != c.want {
			t.Errorf("预设 %s 的 %s 序列为 %q，期望 %q", c.preset, c.key, got, c.want)
		}
	}
}

func TestFunctionSequencesParse(t *testing.T) {
	for _, p := range Presets {
		for _, f := range Functions {
			raw := p.FunctionTemplate(f.Key)
			steps, err := ParseSequence(raw)
			if err != nil {
				t.Errorf("预设 %s 的功能 %s 解析失败: %v（序列 %q）", p.ID, f.ID, err, raw)
				continue
			}
			// prefix (2 keys) + letter + confirm
			if len(steps) != 4 {
				t.Errorf("预设 %s 的功能 %s 有 %d 步，期望 4 步（序列 %q）",
					p.ID, f.ID, len(steps), raw)
			}
		}
	}
}

func TestFunctionStateStartsUnknown(t *testing.T) {
	s := newFunctionStates()
	for _, f := range Functions {
		if got := s.get(f.ID); got != StateUnknown {
			t.Errorf("功能 %s 的初始状态为 %q，期望 %q", f.ID, got, StateUnknown)
		}
	}
}

// The important one: toggling from an unknown state does not produce knowledge.
func TestTogglingFromUnknownStaysUnknown(t *testing.T) {
	s := newFunctionStates()

	if got := s.toggle("buzzer"); got != StateUnknown {
		t.Errorf("从未知状态切换后为 %q，期望仍是 %q —— 切换器没有回报，"+
			"猜一个值会有一半时间是错的", got, StateUnknown)
	}
	if got := s.toggle("buzzer"); got != StateUnknown {
		t.Errorf("连按两次之后为 %q，期望仍是 %q", got, StateUnknown)
	}
}

func TestTogglingAKnownStateFlipsIt(t *testing.T) {
	s := newFunctionStates()
	s.set("buzzer", StateOn)
	if got := s.toggle("buzzer"); got != StateOff {
		t.Errorf("从开启切换后为 %q，期望 %q", got, StateOff)
	}
	if got := s.toggle("buzzer"); got != StateOn {
		t.Errorf("再切换一次后为 %q，期望 %q", got, StateOn)
	}
}

// --- HTTP layer ---

type functionStatusView struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Key   string `json:"key"`
	State string `json:"state"`
}

func readFunctions(t *testing.T, client *http.Client, base string) map[string]functionStatusView {
	t.Helper()
	resp, err := client.Get(base + "/api/status")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	var out struct {
		Functions []functionStatusView `json:"functions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	byID := make(map[string]functionStatusView, len(out.Functions))
	for _, f := range out.Functions {
		byID[f.ID] = f
	}
	return byID
}

func TestFunctionEndpoint(t *testing.T) {
	fake, client, base := newAuthedAPI(t)

	got := readFunctions(t, client, base)
	if len(got) != len(Functions) {
		t.Fatalf("状态里报了 %d 个功能，期望 %d 个", len(got), len(Functions))
	}
	for _, f := range Functions {
		if got[f.ID].State != StateUnknown {
			t.Errorf("功能 %s 的初始状态为 %q，期望 unknown", f.ID, got[f.ID].State)
		}
	}

	// Toggle the buzzer: the hotkey goes out, but the state stays unknown.
	resp := postJSON(t, client, base+"/api/function", `{"name":"buzzer","action":"toggle"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("切换蜂鸣器状态码 %d，期望 200", resp.StatusCode)
	}
	var toggleOut struct {
		State        string   `json:"state"`
		StillUnknown bool     `json:"stillUnknown"`
		Steps        []string `json:"steps"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&toggleOut); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if toggleOut.State != StateUnknown || !toggleOut.StillUnknown {
		t.Errorf("从未知状态切换后 state=%q stillUnknown=%v，两者都应为 unknown/true",
			toggleOut.State, toggleOut.StillUnknown)
	}

	// ScrollLock down/up ×2, then KeyB, then Enter.
	frames := fake.waitFrames(8, 3*time.Second)
	if frames[4][2] != 0x05 {
		t.Errorf("第四个按键的键码为 0x%02X，期望 KeyB 的 0x05", frames[4][2])
	}
	if frames[6][2] != 0x28 {
		t.Errorf("最后一个按键的键码为 0x%02X，期望 Enter 的 0x28", frames[6][2])
	}

	// Correcting the record sends no keystrokes.
	correct := postJSON(t, client, base+"/api/function", `{"name":"buzzer","state":"on"}`)
	correct.Body.Close()
	if correct.StatusCode != http.StatusOK {
		t.Fatalf("修正状态码 %d，期望 200", correct.StatusCode)
	}
	if got := readFunctions(t, client, base)["buzzer"].State; got != StateOn {
		t.Errorf("修正之后状态为 %q，期望 on", got)
	}

	// Now that it is known, a toggle flips it.
	flip := postJSON(t, client, base+"/api/function", `{"name":"buzzer","action":"toggle"}`)
	flip.Body.Close()
	if got := readFunctions(t, client, base)["buzzer"].State; got != StateOff {
		t.Errorf("切换之后状态为 %q，期望 off", got)
	}

	// Auto-scan is tracked independently.
	if got := readFunctions(t, client, base)["autoscan"].State; got != StateUnknown {
		t.Errorf("自动扫描的状态被蜂鸣器的操作影响了，实际为 %q", got)
	}
}

func TestFunctionEndpointRejectsBadRequests(t *testing.T) {
	_, client, base := newAuthedAPI(t)

	cases := []struct {
		name string
		body string
	}{
		{"未知功能", `{"name":"nope","action":"toggle"}`},
		{"两者都给了", `{"name":"buzzer","action":"toggle","state":"on"}`},
		{"两者都没给", `{"name":"buzzer"}`},
		{"非法状态值", `{"name":"buzzer","state":"maybe"}`},
	}
	for _, c := range cases {
		resp := postJSON(t, client, base+"/api/function", c.body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s：状态码 %d，期望 400", c.name, resp.StatusCode)
		}
	}

	// A toggle with a bogus preset is refused before any keys are sent.
	resp := postJSON(t, client, base+"/api/function",
		`{"name":"buzzer","action":"toggle","preset":"nonexistent"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("非法预设：状态码 %d，期望 400", resp.StatusCode)
	}
}

func TestFunctionEndpointRequiresLogin(t *testing.T) {
	_, _, base := newAuthedAPI(t)

	resp := postJSON(t, &http.Client{}, base+"/api/function", `{"name":"buzzer","action":"toggle"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("未登录时状态码 %d，期望 401", resp.StatusCode)
	}
}
