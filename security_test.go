package main

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// clearEnv removes every variable loadConfig reads, so the developer's own
// environment cannot influence the assertions.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"ONEKVM_URL", "ONEKVM_USER", "ONEKVM_PASSWORD", "ONEKVM_TOTP_SECRET",
		"ONEKVM_SESSION", "BIND_ADDR", "API_TOKEN", "AUTH_MODE", "AUTH_USER",
		"AUTH_PASSWORD", "SESSION_TTL_HOURS", "LOGIN_RATE_LIMIT_PER_MIN",
		"TLS_CERT", "TLS_KEY", "TRUST_PROXY", "RATE_LIMIT_PER_MIN",
		"ACTION_RATE_LIMIT_PER_MIN", "DEFAULT_TEMPLATE", "PORT", "CHANNELS",
		"STEP_DELAY_MS", "HOLD_MS", "ALLOWED_IPS",
	} {
		t.Setenv(k, "")
	}
	t.Setenv("ONEKVM_URL", "http://127.0.0.1:9")
	t.Setenv("ONEKVM_USER", "admin")
	t.Setenv("ONEKVM_PASSWORD", "secret")
}

// --- config validation ---

func TestAuthDefaultsToReusingOneKVMAccount(t *testing.T) {
	clearEnv(t)

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("默认配置不应报错: %v", err)
	}
	if cfg.AuthMode != "onekvm" {
		t.Errorf("默认认证模式为 %q，期望 onekvm", cfg.AuthMode)
	}
	if cfg.AuthUser != "admin" || cfg.AuthPassword != "secret" {
		t.Errorf("应复用 One-KVM 的账号密码，实际为 %q / %q", cfg.AuthUser, cfg.AuthPassword)
	}
}

func TestOneKVMReuseNeedsAPasswordNotJustASession(t *testing.T) {
	clearEnv(t)
	t.Setenv("ONEKVM_USER", "")
	t.Setenv("ONEKVM_PASSWORD", "")
	t.Setenv("ONEKVM_SESSION", "some-existing-session-token")

	_, err := loadConfig()
	if err == nil {
		t.Fatal("只有 ONEKVM_SESSION 时无法复用账号密码，应当报错")
	}
	if !strings.Contains(err.Error(), "AUTH_MODE=manual") {
		t.Errorf("错误信息应提示可以改用 manual 模式，实际为 %q", err)
	}
}

func TestManualAuthModeRequiresItsOwnPassword(t *testing.T) {
	clearEnv(t)
	t.Setenv("AUTH_MODE", "manual")

	if _, err := loadConfig(); err == nil {
		t.Fatal("manual 模式没设 AUTH_PASSWORD 时应报错")
	}

	t.Setenv("AUTH_PASSWORD", "short")
	if _, err := loadConfig(); err == nil {
		t.Fatal("AUTH_PASSWORD 过短时应报错")
	}

	t.Setenv("AUTH_PASSWORD", "a-decent-password")
	t.Setenv("AUTH_USER", "someone")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("合法配置不应报错: %v", err)
	}
	if cfg.AuthUser != "someone" || cfg.AuthPassword != "a-decent-password" {
		t.Errorf("独立账号未被采用: %q / %q", cfg.AuthUser, cfg.AuthPassword)
	}
}

func TestUnknownAuthModeIsRejected(t *testing.T) {
	clearEnv(t)
	t.Setenv("AUTH_MODE", "whatever")

	if _, err := loadConfig(); err == nil {
		t.Fatal("未知的 AUTH_MODE 应被拒绝")
	}
}

func TestParseAllowedNets(t *testing.T) {
	nets, err := parseAllowedNets("192.168.1.5, 10.0.0.0/8")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(nets) != 2 {
		t.Fatalf("期望 2 条规则，得到 %d", len(nets))
	}
	if !ipInAny(net.ParseIP("192.168.1.5"), nets) {
		t.Error("裸 IP 应被当作 /32 匹配自身")
	}
	if ipInAny(net.ParseIP("192.168.1.6"), nets) {
		t.Error("裸 IP 不应匹配邻居地址")
	}
	if !ipInAny(net.ParseIP("10.1.2.3"), nets) {
		t.Error("CIDR 应匹配网段内的地址")
	}
	if _, err := parseAllowedNets("not-an-ip"); err == nil {
		t.Error("非法条目应报错")
	}
}

func TestLoginAndSession(t *testing.T) {
	_, ts := newTestServer(t, nil)

	// Not logged in: the API refuses and tells the page to show the login form.
	resp, err := http.Get(ts.URL + "/api/status")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	var unauth map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&unauth)
	resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("未登录时状态码 %d，期望 401", resp.StatusCode)
	}
	if unauth["requiresAuth"] != true {
		t.Errorf("401 响应应带 requiresAuth，实际为 %v", unauth)
	}
	if unauth["oneKvmUrl"] != nil {
		t.Error("未登录的请求不应拿到 One-KVM 地址")
	}

	// Wrong password: refused, no cookie handed out.
	badClient := &http.Client{}
	badResp := postWith(t, badClient, ts.URL+"/api/login", "application/json",
		`{"username":"admin","password":"nope"}`)
	badResp.Body.Close()
	if badResp.StatusCode != http.StatusUnauthorized {
		t.Errorf("密码错误时状态码 %d，期望 401", badResp.StatusCode)
	}
	if len(badResp.Cookies()) != 0 {
		t.Error("登录失败时不应下发 cookie")
	}

	// Correct credentials.
	client := loginClient(t, ts, "admin", "correct-horse-battery-staple")

	okResp, err := client.Get(ts.URL + "/api/status")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer okResp.Body.Close()
	if okResp.StatusCode != http.StatusOK {
		t.Fatalf("登录后状态码 %d，期望 200", okResp.StatusCode)
	}

	// Logout revokes the session.
	logoutResp := postWith(t, client, ts.URL+"/api/logout", "", "")
	logoutResp.Body.Close()
	if logoutResp.StatusCode != http.StatusOK {
		t.Fatalf("登出状态码 %d，期望 200", logoutResp.StatusCode)
	}
	afterResp, err := client.Get(ts.URL + "/api/status")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	afterResp.Body.Close()
	if afterResp.StatusCode != http.StatusUnauthorized {
		t.Errorf("登出后状态码 %d，期望 401", afterResp.StatusCode)
	}
}

func TestSessionCookieAttributes(t *testing.T) {
	_, ts := newTestServer(t, nil)

	resp := postWith(t, &http.Client{}, ts.URL+"/api/login", "application/json",
		`{"username":"admin","password":"correct-horse-battery-staple"}`)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("登录状态码 %d，期望 200", resp.StatusCode)
	}

	var session *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookieName {
			session = c
		}
	}
	if session == nil {
		t.Fatalf("响应里没有 %s cookie", sessionCookieName)
	}
	if !session.HttpOnly {
		t.Error("会话 cookie 必须是 HttpOnly，否则脚本能读走它")
	}
	if session.SameSite != http.SameSiteLaxMode {
		t.Errorf("会话 cookie 的 SameSite 为 %v，期望 Lax —— "+
			"这是挡住跨站表单 POST 的关键", session.SameSite)
	}
	if session.Path != "/" {
		t.Errorf("cookie Path 为 %q", session.Path)
	}
	if session.MaxAge <= 0 {
		t.Errorf("cookie 应有正的 Max-Age，实际 %d", session.MaxAge)
	}
}

func TestForgedSessionCookieIsRejected(t *testing.T) {
	_, ts := newTestServer(t, nil)

	client := &http.Client{}
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/status", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "made-up-session-id"})
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("伪造会话状态码 %d，期望 401", resp.StatusCode)
	}
}

func TestExpiredSessionIsRejected(t *testing.T) {
	s, ts := newTestServer(t, nil)

	id, _, err := s.sessions.create()
	if err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
	// Force it into the past.
	s.sessions.mu.Lock()
	s.sessions.sessions[id] = time.Now().Add(-time.Minute)
	s.sessions.mu.Unlock()

	client := &http.Client{}
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/status", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: id})
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("过期会话状态码 %d，期望 401", resp.StatusCode)
	}
}

func TestLoginRateLimit(t *testing.T) {
	_, ts := newTestServer(t, func(c *Config) { c.LoginRateLimitPerMin = 3 })

	var last *http.Response
	for i := 0; i < 4; i++ {
		last = postWith(t, &http.Client{}, ts.URL+"/api/login", "application/json",
			`{"username":"admin","password":"guess"}`)
		last.Body.Close()
	}
	if last.StatusCode != http.StatusTooManyRequests {
		t.Errorf("第 4 次登录尝试的状态码为 %d，期望 429", last.StatusCode)
	}
	if last.Header.Get("Retry-After") == "" {
		t.Error("429 响应应带 Retry-After")
	}
}

// --- request-level hardening ---

func newTestServer(t *testing.T, mutate func(*Config)) (*Server, *httptest.Server) {
	t.Helper()
	fake := newFakeKVM(t, 0x00)
	cfg := testConfig(fake.srv.URL)
	cfg.BindAddr = "127.0.0.1"
	cfg.AuthMode = "manual"
	cfg.AuthUser = "admin"
	cfg.AuthPassword = "correct-horse-battery-staple"
	cfg.SessionTTLHours = 24
	if mutate != nil {
		mutate(&cfg)
	}
	s := newServer(cfg)
	ts := httptest.NewServer(s.routes())
	t.Cleanup(func() {
		ts.Close()
		s.kvm.Close()
	})
	return s, ts
}

func postWith(t *testing.T, client *http.Client, url, contentType, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	return resp
}

// loginClient returns an HTTP client carrying a valid session cookie.
func loginClient(t *testing.T, ts *httptest.Server, user, pass string) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("创建 cookie jar 失败: %v", err)
	}
	client := &http.Client{Jar: jar}

	body, _ := json.Marshal(map[string]string{"username": user, "password": pass})
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/login", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("构造登录请求失败: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("登录请求失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("登录失败，状态码 %d", resp.StatusCode)
	}
	return client
}

// The cross-site form attack: enctype="text/plain" is a CORS-safelisted content
// type, so no preflight blocks it. It must be refused on the content type alone,
// independently of the cookie.
func TestCrossSiteFormBodyIsRejected(t *testing.T) {
	fake := newFakeKVM(t, 0x00)
	cfg := testConfig(fake.srv.URL)
	cfg.AuthMode = "manual"
	cfg.AuthUser = "admin"
	cfg.AuthPassword = "correct-horse-battery-staple"
	s := newServer(cfg)
	ts := httptest.NewServer(s.routes())
	t.Cleanup(func() { ts.Close(); s.kvm.Close() })

	// Log in so that only the content type is standing between the request and
	// the keystroke injection.
	client := loginClient(t, ts, "admin", "correct-horse-battery-staple")

	resp := postWith(t, client, ts.URL+"/api/switch", "text/plain", `{"port":3}=`)
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusBadGateway {
		t.Fatal("请求已经越过解析走到发送按键那一步，CSRF 防护失效")
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("text/plain 正文的状态码为 %d，期望 400", resp.StatusCode)
	}
}

func TestTrailingDataAfterJSONIsRejected(t *testing.T) {
	_, ts := newTestServer(t, nil)
	client := loginClient(t, ts, "admin", "correct-horse-battery-staple")

	resp := postWith(t, client, ts.URL+"/api/switch", "application/json", `{"port":3}=`)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("带尾随内容的 JSON 状态码为 %d，期望 400", resp.StatusCode)
	}
}

func TestActionRateLimit(t *testing.T) {
	_, ts := newTestServer(t, func(c *Config) { c.ActionRateLimitPerMin = 3 })
	client := loginClient(t, ts, "admin", "correct-horse-battery-staple")

	var last *http.Response
	for i := 0; i < 4; i++ {
		last = postWith(t, client, ts.URL+"/api/macro", "application/json", `{"sequence":"wait:1"}`)
		last.Body.Close()
	}
	if last.StatusCode != http.StatusTooManyRequests {
		t.Errorf("第 4 次发送按键的状态码为 %d，期望 429", last.StatusCode)
	}
}

// API_TOKEN is optional now — it exists so scripts can skip the login dance.
func TestOptionalBearerTokenStillWorks(t *testing.T) {
	_, ts := newTestServer(t, func(c *Config) { c.APIToken = "automation-token-value" })

	resp := postWith(t, &http.Client{}, ts.URL+"/api/switch", "application/json", `{"port":3}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("没带令牌时状态码 %d，期望 401", resp.StatusCode)
	}

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/switch",
		strings.NewReader(`{"port":3}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer automation-token-value")
	okResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer okResp.Body.Close()
	if okResp.StatusCode != http.StatusOK {
		t.Errorf("带令牌时状态码 %d，期望 200", okResp.StatusCode)
	}
}

func TestSecurityHeaders(t *testing.T) {
	_, ts := newTestServer(t, nil)

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()

	want := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
		"Cache-Control":          "no-store",
	}
	for k, v := range want {
		if got := resp.Header.Get(k); got != v {
			t.Errorf("响应头 %s = %q，期望 %q", k, got, v)
		}
	}
	csp := resp.Header.Get("Content-Security-Policy")
	for _, directive := range []string{"default-src 'none'", "script-src 'self'", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, directive) {
			t.Errorf("CSP 缺少 %q，实际为 %q", directive, csp)
		}
	}
	if strings.Contains(csp, "unsafe-inline") {
		t.Error("CSP 不应允许 unsafe-inline")
	}
}

func TestLoginPageIsPublicButAPIIsNot(t *testing.T) {
	_, ts := newTestServer(t, nil)

	// The shell and its assets carry no secrets, so they stay reachable — the
	// login form has to load before anyone can authenticate.
	for _, path := range []string{"/", "/app.js", "/app.css"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("请求 %s 失败: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s 状态码 %d，期望 200", path, resp.StatusCode)
		}
	}

	resp, err := http.Get(ts.URL + "/api/keys")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("/api/keys 未登录时状态码 %d，期望 401", resp.StatusCode)
	}
}

func TestIsLoopbackHost(t *testing.T) {
	for _, addr := range []string{"127.0.0.1", "127.0.0.1:8081", "localhost:8081", "::1"} {
		if !isLoopbackHost(addr) {
			t.Errorf("%q 应被识别为回环地址", addr)
		}
	}
	for _, addr := range []string{"0.0.0.0", "0.0.0.0:8081", "192.168.1.10", ""} {
		if isLoopbackHost(addr) {
			t.Errorf("%q 不应被识别为回环地址", addr)
		}
	}
}
