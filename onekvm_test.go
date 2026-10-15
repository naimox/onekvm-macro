package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeKVM stands in for a real One-KVM: it implements just the two endpoints
// this service uses, and records exactly what arrives.
type fakeKVM struct {
	t *testing.T
	// initialStatus is the byte the real server sends right after the HID
	// WebSocket connects: 0x00 ready, 0x01 HID unavailable.
	initialStatus byte

	mu            sync.Mutex
	loginBodies   []map[string]string
	wsCookie      string
	frames        [][]byte
	loginRejects  bool
	totpChallenge string
	totpCode      string
	srv           *httptest.Server
}

func newFakeKVM(t *testing.T, initialStatus byte) *fakeKVM {
	t.Helper()
	f := &fakeKVM{t: t, initialStatus: initialStatus}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/auth/login", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.loginBodies = append(f.loginBodies, body)
		reject := f.loginRejects
		challenge := f.totpChallenge
		f.mu.Unlock()

		if reject {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"success":false,"message":"Invalid username or password"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if challenge != "" {
			_, _ = w.Write([]byte(`{"next":"totp","challenge_id":"` + challenge + `"}`))
			return
		}
		http.SetCookie(w, &http.Cookie{Name: oneKvmCookieName, Value: "session-abc", Path: "/"})
		_, _ = w.Write([]byte(`{"next":"authenticated"}`))
	})

	mux.HandleFunc("/api/auth/login/totp", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.totpCode = body["code"]
		f.mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: oneKvmCookieName, Value: "session-totp", Path: "/"})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"next":"authenticated"}`))
	})

	mux.HandleFunc("/api/ws/hid", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.wsCookie = r.Header.Get("Cookie")
		f.mu.Unlock()

		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()

		// Real server behaviour: one status byte on connect, then no per-frame ack.
		if err := conn.Write(r.Context(), websocket.MessageBinary, []byte{f.initialStatus}); err != nil {
			return
		}
		ctx := context.Background()
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			f.mu.Lock()
			f.frames = append(f.frames, append([]byte(nil), data...))
			f.mu.Unlock()
		}
	})

	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeKVM) waitFrames(n int, timeout time.Duration) [][]byte {
	f.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		if len(f.frames) >= n {
			out := append([][]byte(nil), f.frames...)
			f.mu.Unlock()
			return out
		}
		f.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t.Fatalf("等待 %d 帧超时，只收到 %d 帧", n, len(f.frames))
	return nil
}

func testConfig(baseURL string) Config {
	return Config{
		OneKVMURL:       baseURL,
		OneKVMUser:      "admin",
		OneKVMPassword:  "secret",
		DefaultTemplate: "ScrollLock ScrollLock {port} Enter",
		Channels:        16,
		StepDelayMs:     1,
		HoldMs:          1,
		SessionTTLHours: 24,
	}
}

func TestSendWritesExactFrames(t *testing.T) {
	fake := newFakeKVM(t, 0x00)
	client := NewOneKVM(testConfig(fake.srv.URL))
	t.Cleanup(client.Close)

	steps, err := ParseSequence("ScrollLock ScrollLock 3 Enter")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.Send(ctx, steps, Timing{StepDelayMs: 1, HoldMs: 1}); err != nil {
		t.Fatalf("发送失败: %v", err)
	}

	got := fake.waitFrames(8, 3*time.Second)
	want := [][]byte{
		{0x01, 0x00, 0x47, 0x00}, {0x01, 0x01, 0x47, 0x00},
		{0x01, 0x00, 0x47, 0x00}, {0x01, 0x01, 0x47, 0x00},
		{0x01, 0x00, 0x20, 0x00}, {0x01, 0x01, 0x20, 0x00},
		{0x01, 0x00, 0x28, 0x00}, {0x01, 0x01, 0x28, 0x00},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("线缆上的字节不符\n实际: %s\n期望: %s", dump(got), dump(want))
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.loginBodies) != 1 {
		t.Fatalf("期望 1 次登录，实际 %d 次", len(fake.loginBodies))
	}
	if fake.loginBodies[0]["username"] != "admin" || fake.loginBodies[0]["password"] != "secret" {
		t.Errorf("登录正文不符: %v", fake.loginBodies[0])
	}
	if !strings.Contains(fake.wsCookie, oneKvmCookieName+"=session-abc") {
		t.Errorf("WebSocket 握手没有带上会话 cookie，实际为 %q", fake.wsCookie)
	}
}

func TestConnectionIsReusedAcrossSends(t *testing.T) {
	fake := newFakeKVM(t, 0x00)
	client := NewOneKVM(testConfig(fake.srv.URL))
	t.Cleanup(client.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for i := 0; i < 3; i++ {
		steps, _ := ParseSequence("Escape")
		if err := client.Send(ctx, steps, Timing{StepDelayMs: 1, HoldMs: 1}); err != nil {
			t.Fatalf("第 %d 次发送失败: %v", i+1, err)
		}
	}
	fake.waitFrames(6, 3*time.Second)

	fake.mu.Lock()
	defer fake.mu.Unlock()
	// One login and one handshake for all three sends: reconnecting each time
	// would make One-KVM reset HID and drop keys the user is holding.
	if len(fake.loginBodies) != 1 {
		t.Errorf("登录了 %d 次，期望 1 次（连接应被复用）", len(fake.loginBodies))
	}
	if !client.Snapshot().Connected {
		t.Error("客户端应保持连接")
	}
}

func TestHIDUnavailableIsReported(t *testing.T) {
	fake := newFakeKVM(t, 0x01)
	client := NewOneKVM(testConfig(fake.srv.URL))
	t.Cleanup(client.Close)

	steps, _ := ParseSequence("Escape")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := client.Send(ctx, steps, Timing{StepDelayMs: 1, HoldMs: 1})
	if err == nil {
		t.Fatal("HID 不可用时应返回错误")
	}
	if !strings.Contains(err.Error(), "HID 不可用") {
		t.Errorf("错误信息应说明 HID 不可用，实际为 %q", err)
	}
	if client.Snapshot().HIDReady {
		t.Error("HIDReady 应为 false")
	}
}

func TestReLoginAfterSessionGoesStale(t *testing.T) {
	fake := newFakeKVM(t, 0x00)
	client := NewOneKVM(testConfig(fake.srv.URL))
	t.Cleanup(client.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	steps, _ := ParseSequence("Escape")
	if err := client.Send(ctx, steps, Timing{StepDelayMs: 1, HoldMs: 1}); err != nil {
		t.Fatalf("首次发送失败: %v", err)
	}
	fake.waitFrames(2, 3*time.Second)

	// Simulate the socket dying (One-KVM restart, network blip).
	client.mu.Lock()
	client.closeConnLocked()
	client.mu.Unlock()

	if err := client.Send(ctx, steps, Timing{StepDelayMs: 1, HoldMs: 1}); err != nil {
		t.Fatalf("重连后发送失败: %v", err)
	}
	fake.waitFrames(4, 3*time.Second)
}

func TestLoginRejectionSurfacesServerMessage(t *testing.T) {
	fake := newFakeKVM(t, 0x00)
	fake.loginRejects = true
	client := NewOneKVM(testConfig(fake.srv.URL))
	t.Cleanup(client.Close)

	steps, _ := ParseSequence("Escape")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := client.Send(ctx, steps, Timing{StepDelayMs: 1, HoldMs: 1})
	if err == nil {
		t.Fatal("登录被拒时应返回错误")
	}
	if !strings.Contains(err.Error(), "Invalid username or password") {
		t.Errorf("应把服务端的错误原文带出来，实际为 %q", err)
	}
}

func TestTOTPLoginFlow(t *testing.T) {
	fake := newFakeKVM(t, 0x00)
	fake.totpChallenge = "challenge-1"
	cfg := testConfig(fake.srv.URL)
	cfg.OneKVMTOTPSecret = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	client := NewOneKVM(cfg)
	t.Cleanup(client.Close)

	steps, _ := ParseSequence("Escape")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.Send(ctx, steps, Timing{StepDelayMs: 1, HoldMs: 1}); err != nil {
		t.Fatalf("两步验证流程失败: %v", err)
	}
	fake.waitFrames(2, 3*time.Second)

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.totpCode) != 6 {
		t.Errorf("应提交 6 位验证码，实际为 %q", fake.totpCode)
	}
}

func TestFixedSessionSkipsLogin(t *testing.T) {
	fake := newFakeKVM(t, 0x00)
	cfg := testConfig(fake.srv.URL)
	cfg.OneKVMUser, cfg.OneKVMPassword = "", ""
	cfg.OneKVMSession = "preset-session-token"
	client := NewOneKVM(cfg)
	t.Cleanup(client.Close)

	steps, _ := ParseSequence("Escape")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.Send(ctx, steps, Timing{StepDelayMs: 1, HoldMs: 1}); err != nil {
		t.Fatalf("使用固定会话令牌时发送失败: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.loginBodies) != 0 {
		t.Errorf("配置了 ONEKVM_SESSION 时不应登录，实际登录 %d 次", len(fake.loginBodies))
	}
	if !strings.Contains(fake.wsCookie, "preset-session-token") {
		t.Errorf("握手应使用配置的令牌，实际 cookie 为 %q", fake.wsCookie)
	}
}

// --- HTTP layer ---

func TestIndexPageIsServed(t *testing.T) {
	// Guards the go:embed wiring: a broken embed compiles fine but ships a
	// blank page.
	fake := newFakeKVM(t, 0x00)
	cfg := testConfig(fake.srv.URL)
	server := &Server{cfg: cfg, kvm: NewOneKVM(cfg)}
	t.Cleanup(server.kvm.Close)
	api := httptest.NewServer(server.routes())
	t.Cleanup(api.Close)

	resp, err := http.Get(api.URL + "/")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 %d，期望 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(body, []byte("One-KVM")) {
		t.Error("首页内容不像是控制页面")
	}
	if !bytes.Contains(body, []byte(`src="/app.js"`)) {
		t.Error("首页没有引用 /app.js")
	}
	// The CSP forbids inline script and style, so neither may appear in the page.
	if bytes.Contains(body, []byte("<script>")) || bytes.Contains(body, []byte("<style")) {
		t.Error("首页含有内联脚本或样式，会被 CSP 拦掉")
	}
	if bytes.Contains(body, []byte("style=")) {
		t.Error("首页含有内联 style 属性，会被 CSP 拦掉")
	}

	jsResp, err := http.Get(api.URL + "/app.js")
	if err != nil {
		t.Fatalf("请求 app.js 失败: %v", err)
	}
	defer jsResp.Body.Close()
	if jsResp.StatusCode != http.StatusOK {
		t.Fatalf("app.js 状态码 %d，期望 200", jsResp.StatusCode)
	}
	jsBody, _ := io.ReadAll(jsResp.Body)
	if !bytes.Contains(jsBody, []byte("/api/switch")) {
		t.Error("app.js 没有引用 /api/switch")
	}
}

// newAuthedAPI boots the real handlers in front of a fake One-KVM and hands
// back a client holding a valid session. Everything except the login endpoints
// needs one now.
func newAuthedAPI(t *testing.T) (*fakeKVM, *http.Client, string) {
	t.Helper()
	fake := newFakeKVM(t, 0x00)
	cfg := testConfig(fake.srv.URL)
	cfg.AuthMode = "manual"
	cfg.AuthUser = "admin"
	cfg.AuthPassword = "correct-horse-battery-staple"
	cfg.SessionTTLHours = 24

	server := newServer(cfg)
	api := httptest.NewServer(server.routes())
	t.Cleanup(func() {
		api.Close()
		server.kvm.Close()
	})
	return fake, loginClient(t, api, "admin", "correct-horse-battery-staple"), api.URL
}

func postJSON(t *testing.T, client *http.Client, url, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	return resp
}

func TestSwitchEndpointEndToEnd(t *testing.T) {
	fake, client, base := newAuthedAPI(t)

	resp := postJSON(t, client, base+"/api/switch", `{"port":3,"preset":"scrolllock"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 %d，期望 200", resp.StatusCode)
	}

	var out struct {
		OK    bool     `json:"ok"`
		Steps []string `json:"steps"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if !out.OK {
		t.Fatal("响应 ok 应为 true")
	}
	want := []string{"ScrollLock", "ScrollLock", "Digit3", "Enter"}
	if !reflect.DeepEqual(out.Steps, want) {
		t.Errorf("步骤为 %v，期望 %v", out.Steps, want)
	}

	got := fake.waitFrames(8, 3*time.Second)
	if got[4][2] != 0x20 {
		t.Errorf("第三个按键的键码为 0x%02X，期望 Digit3 的 0x20", got[4][2])
	}
}

func TestMacroEndpointRejectsUnknownKey(t *testing.T) {
	_, client, base := newAuthedAPI(t)

	resp := postJSON(t, client, base+"/api/macro", `{"sequence":"NotAKey"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("状态码 %d，期望 400", resp.StatusCode)
	}
}

func TestSwitchRejectsOutOfRangePort(t *testing.T) {
	_, client, base := newAuthedAPI(t)

	resp := postJSON(t, client, base+"/api/switch", `{"port":99}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("状态码 %d，期望 400", resp.StatusCode)
	}
}

// The active channel lives on the server so that every logged-in browser shows
// the same one.
func TestSwitchUpdatesTheSharedChannel(t *testing.T) {
	fake, client, base := newAuthedAPI(t)

	readCurrent := func() float64 {
		t.Helper()
		resp, err := client.Get(base + "/api/status")
		if err != nil {
			t.Fatalf("请求失败: %v", err)
		}
		defer resp.Body.Close()
		var out struct {
			CurrentPort float64 `json:"currentPort"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		return out.CurrentPort
	}

	if got := readCurrent(); got != 0 {
		t.Fatalf("还没切换过时 currentPort 应为 0，实际 %v", got)
	}

	// Channel 16 also exercises the multi-digit expansion end to end.
	resp := postJSON(t, client, base+"/api/switch", `{"port":16}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 %d，期望 200", resp.StatusCode)
	}
	var out struct {
		CurrentPort int `json:"currentPort"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if out.CurrentPort != 16 {
		t.Errorf("切换响应里的 currentPort 为 %d，期望 16", out.CurrentPort)
	}

	if got := readCurrent(); got != 16 {
		t.Errorf("状态接口回报的 currentPort 为 %v，期望 16", got)
	}

	// 2 (ScrollLock down/up, same again) + 2 (Digit1) + 2 (Digit6) + 2 (Enter)
	got := fake.waitFrames(10, 3*time.Second)
	if got[4][2] != 0x1e || got[6][2] != 0x23 {
		t.Errorf("通道 16 应发出 Digit1(0x1e) 后 Digit6(0x23)，实际 0x%02X 0x%02X",
			got[4][2], got[6][2])
	}
}

// A free macro is not necessarily a channel change, so it must leave the shared
// marker alone.
func TestMacroDoesNotChangeTheSharedChannel(t *testing.T) {
	_, client, base := newAuthedAPI(t)

	if resp := postJSON(t, client, base+"/api/switch", `{"port":3}`); resp != nil {
		resp.Body.Close()
	}

	resp := postJSON(t, client, base+"/api/macro", `{"sequence":"Escape"}`)
	resp.Body.Close()

	statusResp, err := client.Get(base + "/api/status")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer statusResp.Body.Close()
	var out struct {
		CurrentPort int `json:"currentPort"`
	}
	if err := json.NewDecoder(statusResp.Body).Decode(&out); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if out.CurrentPort != 3 {
		t.Errorf("自由宏之后 currentPort 变成 %d，应仍为 3", out.CurrentPort)
	}
}
