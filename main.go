package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

//go:embed web/index.html
var indexHTML []byte

//go:embed web/app.css
var appCSS []byte

//go:embed web/app.js
var appJS []byte

// The page loads nothing but its own two assets, so the policy can be this
// tight. That matters because the session cookie rides along with every
// request the page makes.
const contentSecurityPolicy = "default-src 'none'; " +
	"script-src 'self'; style-src 'self'; connect-src 'self'; " +
	"img-src 'self' data:; form-action 'none'; " +
	"frame-ancestors 'none'; base-uri 'none'"

const (
	sessionCookieName = "okm_session"
	minManualPassword = 8
)

const minTokenLength = 16

type Config struct {
	OneKVMURL        string
	OneKVMUser       string
	OneKVMPassword   string
	OneKVMTOTPSecret string
	OneKVMSession    string

	BindAddr string
	Port     int

	// AuthMode decides where the web login credentials come from.
	//   onekvm — reuse the One-KVM username and password already configured here
	//   manual — a separate AUTH_USER / AUTH_PASSWORD pair
	AuthMode     string
	AuthUser     string
	AuthPassword string

	SessionTTLHours      int
	LoginRateLimitPerMin int

	// APIToken is optional. When set, scripts may call the API with
	// "Authorization: Bearer <token>" instead of logging in first.
	APIToken string

	TLSCert     string
	TLSKey      string
	AllowedNets []*net.IPNet
	TrustProxy  bool

	RateLimitPerMin       int
	ActionRateLimitPerMin int

	DefaultTemplate string
	Channels        int
	StepDelayMs     int
	HoldMs          int

	// FunctionTemplates overrides the sequence for a toggle function. Empty
	// entries fall back to the sequence derived from the selected preset.
	FunctionTemplates map[string]string
}

func loadConfig() (Config, error) {
	cfg := Config{
		OneKVMURL:        strings.TrimSpace(os.Getenv("ONEKVM_URL")),
		OneKVMUser:       envOr("ONEKVM_USER", ""),
		OneKVMPassword:   envOr("ONEKVM_PASSWORD", ""),
		OneKVMTOTPSecret: envOr("ONEKVM_TOTP_SECRET", ""),
		OneKVMSession:    strings.TrimSpace(os.Getenv("ONEKVM_SESSION")),

		BindAddr: strings.TrimSpace(envOr("BIND_ADDR", "0.0.0.0")),

		SessionTTLHours:      envInt("SESSION_TTL_HOURS", 24),
		LoginRateLimitPerMin: envInt("LOGIN_RATE_LIMIT_PER_MIN", 5),

		APIToken: strings.TrimSpace(os.Getenv("API_TOKEN")),

		TLSCert:    strings.TrimSpace(os.Getenv("TLS_CERT")),
		TLSKey:     strings.TrimSpace(os.Getenv("TLS_KEY")),
		TrustProxy: envBool("TRUST_PROXY", false),

		RateLimitPerMin:       envInt("RATE_LIMIT_PER_MIN", 120),
		ActionRateLimitPerMin: envInt("ACTION_RATE_LIMIT_PER_MIN", 30),

		DefaultTemplate: envOr("DEFAULT_TEMPLATE", "ScrollLock ScrollLock {port} Enter"),
		FunctionTemplates: map[string]string{
			"autoscan": strings.TrimSpace(os.Getenv("AUTOSCAN_TEMPLATE")),
			"buzzer":   strings.TrimSpace(os.Getenv("BUZZER_TEMPLATE")),
		},
		Port:        envInt("PORT", 8081),
		Channels:    envInt("CHANNELS", 16),
		StepDelayMs: envInt("STEP_DELAY_MS", 100),
		HoldMs:      envInt("HOLD_MS", 100),
	}

	if cfg.OneKVMURL == "" {
		return cfg, fmt.Errorf("必须设置 ONEKVM_URL，例如 http://192.168.1.50:8080")
	}
	if cfg.OneKVMSession == "" && (cfg.OneKVMUser == "" || cfg.OneKVMPassword == "") {
		return cfg, fmt.Errorf("必须设置 ONEKVM_USER 和 ONEKVM_PASSWORD，或者提供 ONEKVM_SESSION")
	}
	if (cfg.TLSCert == "") != (cfg.TLSKey == "") {
		return cfg, fmt.Errorf("TLS_CERT 和 TLS_KEY 必须同时设置")
	}

	if err := resolveAuth(&cfg); err != nil {
		return cfg, err
	}

	nets, err := parseAllowedNets(envOr("ALLOWED_IPS", ""))
	if err != nil {
		return cfg, err
	}
	cfg.AllowedNets = nets

	if cfg.Channels < 1 || cfg.Channels > 64 {
		cfg.Channels = 16
	}
	cfg.StepDelayMs = clampInt(cfg.StepDelayMs, 0, 5000)
	cfg.HoldMs = clampInt(cfg.HoldMs, 5, 2000)
	cfg.RateLimitPerMin = clampInt(cfg.RateLimitPerMin, 0, 100000)
	cfg.ActionRateLimitPerMin = clampInt(cfg.ActionRateLimitPerMin, 0, 100000)
	cfg.LoginRateLimitPerMin = clampInt(cfg.LoginRateLimitPerMin, 0, 1000)
	cfg.SessionTTLHours = clampInt(cfg.SessionTTLHours, 1, 24*30)

	if cfg.APIToken != "" && len(cfg.APIToken) < minTokenLength {
		return cfg, fmt.Errorf("API_TOKEN 太短（%d 字符，至少 %d）；它只给自动化脚本用，"+
			"不需要的话直接不设即可", len(cfg.APIToken), minTokenLength)
	}

	return cfg, nil
}

// resolveAuth works out which username and password the web login accepts.
func resolveAuth(cfg *Config) error {
	mode := strings.ToLower(strings.TrimSpace(envOr("AUTH_MODE", "onekvm")))
	switch mode {
	case "onekvm":
		// Reuse the One-KVM account. Nothing extra to configure, and there is
		// only one place to change the password.
		if cfg.OneKVMUser == "" || cfg.OneKVMPassword == "" {
			return fmt.Errorf(
				"AUTH_MODE=onekvm 需要用 One-KVM 的账号密码登录网页，但现在只配置了 ONEKVM_SESSION，没有账号密码。\n" +
					"  请补上 ONEKVM_USER 和 ONEKVM_PASSWORD，或者改用独立账号：\n" +
					"      AUTH_MODE=manual\n" +
					"      AUTH_USER=admin\n" +
					"      AUTH_PASSWORD=<你自己设的密码>")
		}
		cfg.AuthMode = "onekvm"
		cfg.AuthUser = cfg.OneKVMUser
		cfg.AuthPassword = cfg.OneKVMPassword

	case "manual":
		cfg.AuthMode = "manual"
		cfg.AuthUser = envOr("AUTH_USER", "admin")
		cfg.AuthPassword = os.Getenv("AUTH_PASSWORD")
		if cfg.AuthPassword == "" {
			return fmt.Errorf("AUTH_MODE=manual 时必须设置 AUTH_PASSWORD")
		}
		if len(cfg.AuthPassword) < minManualPassword {
			return fmt.Errorf("AUTH_PASSWORD 太短（%d 字符，至少 %d）",
				len(cfg.AuthPassword), minManualPassword)
		}

	default:
		return fmt.Errorf("AUTH_MODE 只能是 onekvm 或 manual，收到 %q", mode)
	}
	return nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		log.Printf("环境变量 %s=%q 不是整数，改用 %d", key, v, fallback)
		return fallback
	}
	return n
}

func envBool(key string, fallback bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	switch v {
	case "":
		return fallback
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		log.Printf("环境变量 %s=%q 不是布尔值，改用 %v", key, v, fallback)
		return fallback
	}
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func isLoopbackHost(addr string) bool {
	if addr == "" {
		return false
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func parseAllowedNets(raw string) ([]*net.IPNet, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var out []*net.IPNet
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if !strings.Contains(part, "/") {
			ip := net.ParseIP(part)
			if ip == nil {
				return nil, fmt.Errorf("ALLOWED_IPS 里的 %q 既不是 IP 也不是 CIDR", part)
			}
			if ip.To4() != nil {
				part += "/32"
			} else {
				part += "/128"
			}
		}
		_, ipnet, err := net.ParseCIDR(part)
		if err != nil {
			return nil, fmt.Errorf("ALLOWED_IPS 里的 %q 不是合法的 CIDR: %w", part, err)
		}
		out = append(out, ipnet)
	}
	return out, nil
}

func generateToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "<生成失败，请自行生成一个至少 16 位的随机字符串>"
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// --- sessions ---

// sessionStore keeps login sessions in memory. They do not survive a restart,
// which is a fine trade for a tool this size: a restart just means logging in
// again.
type sessionStore struct {
	mu       sync.Mutex
	sessions map[string]time.Time
	ttl      time.Duration
}

func newSessionStore(ttl time.Duration) *sessionStore {
	return &sessionStore{
		sessions: make(map[string]time.Time),
		ttl:      ttl,
	}
}

func (s *sessionStore) create() (string, time.Time, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}, err
	}
	id := base64.RawURLEncoding.EncodeToString(raw)
	expiry := time.Now().Add(s.ttl)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	s.sessions[id] = expiry
	return id, expiry, nil
}

// valid checks the session and slides its expiry forward, so an active user is
// not logged out mid-session.
func (s *sessionStore) valid(id string) bool {
	if id == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	expiry, ok := s.sessions[id]
	if !ok {
		return false
	}
	if time.Now().After(expiry) {
		delete(s.sessions, id)
		return false
	}
	s.sessions[id] = time.Now().Add(s.ttl)
	return true
}

func (s *sessionStore) delete(id string) {
	if id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, id)
}

func (s *sessionStore) pruneLocked() {
	now := time.Now()
	for id, expiry := range s.sessions {
		if now.After(expiry) {
			delete(s.sessions, id)
		}
	}
}

type Server struct {
	cfg        Config
	kvm        *OneKVM
	tlsEnabled bool
	sessions   *sessionStore
	generalRL  *rateLimiter
	actionRL   *rateLimiter
	loginRL    *rateLimiter

	// currentPort is the last channel this service was told to switch to. It is
	// held server-side so that every logged-in browser shows the same channel
	// instead of each keeping its own guess in localStorage.
	//
	// It is a record of intent, not a reading: KVM switches do not report their
	// active port, so nothing here can tell whether someone used the switch's
	// own button or another keyboard in the meantime.
	currentPort atomic.Int32

	// fnStates is the same idea for the toggle functions, except that a toggle
	// starts out unknown and can stay unknown indefinitely.
	fnStates *functionStates
}

// newServer is the only place a Server is assembled, so a new field cannot be
// forgotten in one caller and remembered in another.
func newServer(cfg Config) *Server {
	return &Server{
		cfg:        cfg,
		kvm:        NewOneKVM(cfg),
		tlsEnabled: cfg.TLSCert != "",
		sessions:   newSessionStore(time.Duration(cfg.SessionTTLHours) * time.Hour),
		generalRL:  newRateLimiter(cfg.RateLimitPerMin),
		actionRL:   newRateLimiter(cfg.ActionRateLimitPerMin),
		loginRL:    newRateLimiter(cfg.LoginRateLimitPerMin),
		fnStates:   newFunctionStates(),
	}
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleRoot)
	mux.HandleFunc("/app.css", s.handleCSS)
	mux.HandleFunc("/app.js", s.handleJS)
	mux.HandleFunc("/healthz", s.handleHealth)

	// Public: the login endpoints, plus the health probe.
	mux.HandleFunc("/api/login", s.withLoginLimit(s.handleLogin))
	mux.HandleFunc("/api/logout", s.handleLogout)

	// Everything else needs a session.
	mux.HandleFunc("/api/status", s.withAuth(s.handleStatus))
	mux.HandleFunc("/api/keys", s.withAuth(s.handleKeys))
	mux.HandleFunc("/api/switch", s.withAuth(s.withActionLimit(s.handleSwitch)))
	mux.HandleFunc("/api/macro", s.withAuth(s.withActionLimit(s.handleMacro)))
	mux.HandleFunc("/api/function", s.withAuth(s.withActionLimit(s.handleFunction)))

	var h http.Handler = mux
	h = s.withRateLimit(s.generalRL, h)
	h = s.withIPAllowlist(h)
	h = s.withSecurityHeaders(h)
	h = s.withRequestLog(h)
	return h
}

// --- middleware ---

func (s *Server) withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Robots-Tag", "noindex, nofollow")
		h.Set("Permissions-Policy", "geolocation=(), microphone=(), camera=()")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		if s.tlsEnabled {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

// withRequestLog records only the path — never the query string, headers or
// bodies, so credentials cannot end up in the logs.
func (s *Server) withRequestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			log.Printf("%s %s from %s", r.Method, r.URL.Path, s.clientIP(r))
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) withIPAllowlist(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(s.cfg.AllowedNets) == 0 {
			next.ServeHTTP(w, r)
			return
		}
		ip := net.ParseIP(s.clientIP(r))
		// Loopback is always allowed so the container's own healthcheck keeps
		// working when the allowlist is in use.
		if ip != nil && (ip.IsLoopback() || ipInAny(ip, s.cfg.AllowedNets)) {
			next.ServeHTTP(w, r)
			return
		}
		s.audit(r, "allowlist", "rejected", false)
		writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "来源地址不在允许列表内"})
	})
}

func ipInAny(ip net.IP, nets []*net.IPNet) bool {
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

func (s *Server) withRateLimit(rl *rateLimiter, next http.Handler) http.Handler {
	if rl == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		if retryAfter, ok := rl.allow(s.clientIP(r)); !ok {
			s.audit(r, "ratelimit", "rejected", false)
			writeTooMany(w, retryAfter, "请求过于频繁，请 %d 秒后重试")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// withActionLimit applies the tighter budget to the endpoints that actually
// inject keystrokes, so a flood cannot be used to hammer the controlled machine.
func (s *Server) withActionLimit(next http.HandlerFunc) http.HandlerFunc {
	if s.actionRL == nil {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if retryAfter, ok := s.actionRL.allow(s.clientIP(r)); !ok {
			s.audit(r, "action-ratelimit", "rejected", false)
			writeTooMany(w, retryAfter, "发送按键过于频繁，请 %d 秒后重试")
			return
		}
		next(w, r)
	}
}

// withLoginLimit throttles guesses. A login form on the open internet will be
// brute-forced, so this is not optional.
func (s *Server) withLoginLimit(next http.HandlerFunc) http.HandlerFunc {
	if s.loginRL == nil {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			next(w, r)
			return
		}
		if retryAfter, ok := s.loginRL.allow(s.clientIP(r)); !ok {
			s.audit(r, "login-ratelimit", "rejected", false)
			writeTooMany(w, retryAfter, "登录尝试过于频繁，请 %d 秒后重试")
			return
		}
		next(w, r)
	}
}

func writeTooMany(w http.ResponseWriter, retryAfter int, format string) {
	w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	writeJSON(w, http.StatusTooManyRequests, map[string]any{
		"ok":    false,
		"error": fmt.Sprintf(format, retryAfter),
	})
}

// withAuth accepts either a logged-in session cookie or, when configured, the
// automation bearer token.
//
// Sessions ride in a SameSite=Lax cookie, which browsers refuse to attach to
// cross-site POSTs — that plus the POST-only, application/json-only rule on the
// mutating endpoints is what keeps this safe from cross-site request forgery.
func (s *Server) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.APIToken != "" {
			if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
				presented := strings.TrimPrefix(h, "Bearer ")
				if subtle.ConstantTimeCompare([]byte(presented), []byte(s.cfg.APIToken)) == 1 {
					next(w, r)
					return
				}
			}
		}
		if c, err := r.Cookie(sessionCookieName); err == nil && s.sessions.valid(c.Value) {
			next(w, r)
			return
		}
		s.audit(r, "auth", "rejected", false)
		// The auth mode is echoed so the login screen can say which account to
		// use. Only the mode, never the username.
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"ok":           false,
			"error":        "请先登录",
			"requiresAuth": true,
			"authMode":     s.cfg.AuthMode,
		})
	}
}

func (s *Server) clientIP(r *http.Request) string {
	// Only honour forwarding headers when the operator has said a trusted proxy
	// sits in front; otherwise anyone could spoof them and dodge the limit.
	if s.cfg.TrustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if first := strings.TrimSpace(strings.Split(xff, ",")[0]); first != "" {
				return first
			}
		}
		if real := strings.TrimSpace(r.Header.Get("X-Real-IP")); real != "" {
			return real
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) audit(r *http.Request, action, detail string, ok bool) {
	result := "ok"
	if !ok {
		result = "denied"
	}
	log.Printf("AUDIT ip=%s action=%s detail=%q result=%s", s.clientIP(r), action, detail, result)
}

// --- static assets ---

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/index.html" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(indexHTML)
}

func (s *Server) handleCSS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(appCSS)
}

func (s *Server) handleJS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(appJS)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// --- login ---

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "请用 POST"})
		return
	}

	var req loginRequest
	if err := decodeJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	// Both comparisons always run, so a wrong username and a wrong password
	// take the same amount of time.
	userOK := subtle.ConstantTimeCompare([]byte(req.Username), []byte(s.cfg.AuthUser)) == 1
	passOK := subtle.ConstantTimeCompare([]byte(req.Password), []byte(s.cfg.AuthPassword)) == 1

	if !userOK || !passOK {
		// Deliberately not logging the submitted username: people sometimes
		// type their password into the wrong box.
		s.audit(r, "login", "failed", false)
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"ok":    false,
			"error": "用户名或密码不正确",
		})
		return
	}

	id, expiry, err := s.sessions.create()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"ok":    false,
			"error": "创建会话失败",
		})
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    id,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.tlsEnabled,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(time.Until(expiry).Seconds()),
	})

	s.audit(r, "login", "succeeded", true)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"user":      s.cfg.AuthUser,
		"expiresAt": expiry.Format(time.RFC3339),
	})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "请用 POST"})
		return
	}
	if c, err := r.Cookie(sessionCookieName); err == nil {
		s.sessions.delete(c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   s.tlsEnabled,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
	s.audit(r, "logout", "succeeded", true)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// --- API ---

func (s *Server) handleKeys(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":   true,
		"keys": KeyNameList(),
	})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	snap := s.kvm.Snapshot()
	resp := map[string]any{
		"ok":              true,
		"user":            s.cfg.AuthUser,
		"authMode":        s.cfg.AuthMode,
		"oneKvmUrl":       s.kvm.BaseURL(),
		"kvmAuthMode":     s.kvm.AuthMode(),
		"connected":       snap.Connected,
		"hidReady":        snap.HIDReady,
		"presets":         presetStatuses(),
		"channels":        s.cfg.Channels,
		"currentPort":     int(s.currentPort.Load()),
		"functions":       functionStatuses(s.fnStates.snapshot(), DefaultPreset(), s.cfg.FunctionTemplates),
		"defaultTemplate": s.cfg.DefaultTemplate,
		"timing": Timing{
			StepDelayMs: s.cfg.StepDelayMs,
			HoldMs:      s.cfg.HoldMs,
		},
		"tls": s.tlsEnabled,
	}
	if snap.LastError != "" {
		resp["lastError"] = snap.LastError
		if !snap.LastErrorAt.IsZero() {
			resp["lastErrorAt"] = snap.LastErrorAt.Format(time.RFC3339)
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

type switchRequest struct {
	Port        int    `json:"port"`
	Preset      string `json:"preset"`
	Template    string `json:"template"`
	StepDelayMs int    `json:"stepDelayMs"`
	HoldMs      int    `json:"holdMs"`
}

func (s *Server) handleSwitch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "请用 POST"})
		return
	}

	var req switchRequest
	if err := decodeJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if req.Port < 1 || req.Port > s.cfg.Channels {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok":    false,
			"error": fmt.Sprintf("通道号必须在 1 到 %d 之间", s.cfg.Channels),
		})
		return
	}

	template, err := s.portTemplate(req.Preset, req.Template)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	s.runSequence(w, r, ApplyPort(template, req.Port), req.StepDelayMs, req.HoldMs,
		req.Port, fmt.Sprintf("switch port=%d", req.Port))
}

type macroRequest struct {
	Sequence    string `json:"sequence"`
	Port        int    `json:"port"`
	StepDelayMs int    `json:"stepDelayMs"`
	HoldMs      int    `json:"holdMs"`
}

func (s *Server) handleMacro(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "请用 POST"})
		return
	}

	var req macroRequest
	if err := decodeJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if strings.TrimSpace(req.Sequence) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "sequence 不能为空"})
		return
	}

	raw := ApplyPort(req.Sequence, req.Port)
	// Port 0: a free macro is not necessarily a channel change, so it leaves the
	// shared channel marker alone.
	s.runSequence(w, r, raw, req.StepDelayMs, req.HoldMs, 0, fmt.Sprintf("macro %q", raw))
}

// portTemplate resolves the sequence used to switch to a port.
func (s *Server) portTemplate(presetID, template string) (string, error) {
	if template != "" {
		return template, nil
	}
	if presetID == "" {
		return s.cfg.DefaultTemplate, nil
	}
	p, ok := FindPreset(presetID)
	if !ok {
		return "", fmt.Errorf("未知的热键预设 %q", presetID)
	}
	return p.PortTemplate(), nil
}

// functionTemplate resolves the sequence for a toggle function. An explicit
// template wins, then a per-function environment override, then the sequence
// derived from the chosen preset.
func (s *Server) functionTemplate(fn SwitchFunction, presetID, template string) (string, error) {
	if template != "" {
		return template, nil
	}
	if custom := s.cfg.FunctionTemplates[fn.ID]; custom != "" {
		return custom, nil
	}
	if presetID == "" {
		return DefaultPreset().FunctionTemplate(fn.Key), nil
	}
	p, ok := FindPreset(presetID)
	if !ok {
		return "", fmt.Errorf("未知的热键预设 %q", presetID)
	}
	return p.FunctionTemplate(fn.Key), nil
}

// sendSequence parses and sends, writing an error response and returning false
// if anything goes wrong.
func (s *Server) sendSequence(w http.ResponseWriter, r *http.Request, raw string, stepDelayMs, holdMs int, auditDetail string) ([]Step, Timing, bool) {
	steps, err := ParseSequence(raw)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return nil, Timing{}, false
	}

	timing := Timing{
		StepDelayMs: resolveTiming(stepDelayMs, s.cfg.StepDelayMs, 0, 5000),
		HoldMs:      resolveTiming(holdMs, s.cfg.HoldMs, 5, 2000),
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	if err := s.kvm.Send(ctx, steps, timing); err != nil {
		s.audit(r, "send", auditDetail+" -> "+err.Error(), false)
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()})
		return nil, Timing{}, false
	}
	s.audit(r, "send", auditDetail, true)
	return steps, timing, true
}

func stepLabels(steps []Step) []string {
	labels := make([]string, 0, len(steps))
	for _, st := range steps {
		labels = append(labels, st.Label)
	}
	return labels
}

// runSequence sends a sequence and reports. port is the channel the sequence
// represents, or 0 when it is not a channel switch.
func (s *Server) runSequence(w http.ResponseWriter, r *http.Request, raw string, stepDelayMs, holdMs, port int, auditDetail string) {
	steps, timing, ok := s.sendSequence(w, r, raw, stepDelayMs, holdMs, auditDetail)
	if !ok {
		return
	}

	// Remember the channel only once the keystrokes actually went out, so a
	// failed switch does not leave every browser pointing at a channel that was
	// never reached.
	if port > 0 {
		s.currentPort.Store(int32(port))
	}

	resp := map[string]any{
		"ok":         true,
		"sequence":   raw,
		"steps":      stepLabels(steps),
		"timing":     timing,
		"durationMs": EstimatedDurationMs(steps, timing),
	}
	if port > 0 {
		resp["currentPort"] = port
	}
	writeJSON(w, http.StatusOK, resp)
}

type functionRequest struct {
	Name        string `json:"name"`
	Action      string `json:"action"`
	State       string `json:"state"`
	Preset      string `json:"preset"`
	Template    string `json:"template"`
	StepDelayMs int    `json:"stepDelayMs"`
	HoldMs      int    `json:"holdMs"`
}

// handleFunction covers the toggle-style switch features.
//
// It deliberately does two different things:
//
//   - action "toggle" presses the hotkey and flips the recorded state. Toggling
//     from an unknown state leaves it unknown, because that is the truth — the
//     switch never reports whether its buzzer is on, so guessing would be wrong
//     half the time.
//   - state "on"/"off" records what you can see or hear for yourself, sending no
//     keystrokes at all. That is the only way the record ever gets corrected.
func (s *Server) handleFunction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "请用 POST"})
		return
	}

	var req functionRequest
	if err := decodeJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	fn, ok := FindFunction(req.Name)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok":    false,
			"error": fmt.Sprintf("未知的功能 %q", req.Name),
		})
		return
	}

	switch {
	case req.Action == "toggle" && req.State == "":
		template, err := s.functionTemplate(fn, req.Preset, req.Template)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		steps, timing, ok := s.sendSequence(w, r, template, req.StepDelayMs, req.HoldMs,
			fmt.Sprintf("%s toggle", fn.ID))
		if !ok {
			return
		}
		state := s.fnStates.toggle(fn.ID)
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":         true,
			"name":       fn.ID,
			"state":      state,
			"sequence":   template,
			"steps":      stepLabels(steps),
			"timing":     timing,
			"durationMs": EstimatedDurationMs(steps, timing),
			// Spelled out so the page can say so plainly rather than implying
			// the switch confirmed anything.
			"stillUnknown": state == StateUnknown,
		})

	case req.Action == "" && req.State != "":
		if !validState(req.State) {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"ok":    false,
				"error": "state 只能是 on 或 off",
			})
			return
		}
		s.fnStates.set(fn.ID, req.State)
		s.audit(r, "function", fmt.Sprintf("%s marked %s", fn.ID, req.State), true)
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":       true,
			"name":     fn.ID,
			"state":    req.State,
			"recorded": true,
		})

	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok":    false,
			"error": `请提供 action: "toggle"，或者提供 state: "on"/"off" 来修正记录`,
		})
	}
}

func resolveTiming(requested, fallback, lo, hi int) int {
	if requested <= 0 {
		return fallback
	}
	return clampInt(requested, lo, hi)
}

// decodeJSON insists on a JSON content type and rejects trailing data. The
// content-type rule is load-bearing for CSRF: a cross-site HTML form can only
// send a safelisted type such as text/plain, so it cannot reach these handlers
// even in a browser that ignores SameSite.
func decodeJSON(r *http.Request, dst any) error {
	ct := r.Header.Get("Content-Type")
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil || mt != "application/json" {
		return fmt.Errorf("Content-Type 必须是 application/json")
	}

	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	// Rejecting unknown fields turns a typo like "stepDelay" into a clear 400
	// instead of a silent no-op.
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("请求正文不是合法的 JSON: %w", err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("请求正文包含多余内容")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// --- rate limiting ---

type rateLimiter struct {
	mu       sync.Mutex
	perMin   int
	capacity float64
	buckets  map[string]*bucket
	lastGC   time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newRateLimiter(perMin int) *rateLimiter {
	if perMin <= 0 {
		return nil
	}
	return &rateLimiter{
		perMin:   perMin,
		capacity: float64(perMin),
		buckets:  make(map[string]*bucket),
		lastGC:   time.Now(),
	}
}

// allow reports whether the caller may proceed, plus how many seconds to wait
// if not.
func (rl *rateLimiter) allow(key string) (int, bool) {
	now := time.Now()

	rl.mu.Lock()
	defer rl.mu.Unlock()

	if now.Sub(rl.lastGC) > 10*time.Minute {
		for k, b := range rl.buckets {
			if now.Sub(b.last) > 10*time.Minute {
				delete(rl.buckets, k)
			}
		}
		rl.lastGC = now
	}

	b, ok := rl.buckets[key]
	if !ok {
		b = &bucket{tokens: rl.capacity, last: now}
		rl.buckets[key] = b
	}

	// Refill at a steady per-minute rate, capped at the burst capacity.
	b.tokens += now.Sub(b.last).Minutes() * float64(rl.perMin)
	if b.tokens > rl.capacity {
		b.tokens = rl.capacity
	}
	b.last = now

	if b.tokens < 1 {
		missing := 1 - b.tokens
		retry := int(missing / (float64(rl.perMin) / 60.0))
		if retry < 1 {
			retry = 1
		}
		return retry, false
	}
	b.tokens--
	return 0, true
}

// --- main ---

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("配置错误：\n  %v", err)
	}

	srv := newServer(cfg)

	addr := net.JoinHostPort(cfg.BindAddr, strconv.Itoa(cfg.Port))
	httpServer := &http.Server{
		Addr:              addr,
		Handler:           srv.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          log.Default(),
	}
	if srv.tlsEnabled {
		httpServer.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}

	scheme := "http"
	if srv.tlsEnabled {
		scheme = "https"
	}
	exposed := !isLoopbackHost(cfg.BindAddr)

	log.Printf("One-KVM 键盘宏服务启动，监听 %s://%s", scheme, addr)
	log.Printf("目标 One-KVM: %s（鉴权方式：%s）", cfg.OneKVMURL, srv.kvm.AuthMode())
	if cfg.AuthMode == "onekvm" {
		log.Printf("网页登录：使用 One-KVM 的账号 %q（改密码请同时改 ONEKVM_PASSWORD）", cfg.AuthUser)
	} else {
		log.Printf("网页登录：使用独立账号 %q", cfg.AuthUser)
	}

	// TLS is not just nice here: without it the login password crosses the
	// network in the clear.
	if exposed && !srv.tlsEnabled {
		log.Printf("⚠ 重要：对外监听却没有启用 TLS，登录密码会以明文经过网络。" +
			"请设置 TLS_CERT / TLS_KEY，或在前面放一个反向代理终止 TLS")
	}
	if cfg.APIToken != "" {
		log.Printf("已启用 API_TOKEN，自动化脚本可以用 Bearer 令牌直接调接口")
	}
	if exposed && len(cfg.AllowedNets) == 0 {
		log.Printf("提示：如需进一步收紧，可用 ALLOWED_IPS 限制来源网段（可选）")
	}
	if cfg.OneKVMSession == "" {
		log.Printf("提示：本服务用账号密码登录 One-KVM。若它关闭了「允许多会话登录」，" +
			"本服务登录会把你浏览器的控制台挤下线，建议打开该选项或改用 ONEKVM_SESSION")
	}

	errCh := make(chan error, 1)
	go func() {
		if srv.tlsEnabled {
			errCh <- httpServer.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey)
		} else {
			errCh <- httpServer.ListenAndServe()
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	select {
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP 服务异常退出: %v", err)
		}
	case <-stop:
		log.Printf("正在关闭…")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(ctx)
	}
	srv.kvm.Close()
}
