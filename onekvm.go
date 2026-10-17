package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const oneKvmCookieName = "one_kvm_session"

var errHIDUnavailable = fmt.Errorf(
	"One-KVM 报告 HID 不可用：请在 One-KVM 网页的鼠键设置里检查 HID 后端是否就绪（USB OTG 或 CH9329）")

// OneKVM talks to a single One-KVM instance: it logs in, holds one long-lived
// HID WebSocket, and writes keyboard frames to it.
//
// The connection is kept open on purpose. One-KVM runs hid.reset() whenever a
// WebSocket HID client disconnects (src/hid/websocket.rs), so reconnecting for
// every macro would release any keys you happen to be holding in the web
// console. Staying connected avoids poking at your live session.
type OneKVM struct {
	baseURL    string
	username   string
	password   string
	totpSecret string
	// fixedToken, when set, is used as an existing session id and no login is
	// attempted. This matters because One-KVM's "allow multiple sessions"
	// option defaults to off, and a fresh login wipes every other session.
	fixedToken string

	http *http.Client

	// mu guards the session and the connection, and Send holds it for the whole
	// of a sequence — a second of sleeping between keystrokes. Nothing that has
	// to stay responsive may take it.
	mu      sync.Mutex
	session string
	conn    *websocket.Conn

	// status is reported by /api/status and therefore has its own lock: a poll
	// must not stall behind an in-flight switch, or the page ends up answering
	// with a snapshot from before the switch it just asked for.
	status statusState
}

type statusState struct {
	mu        sync.Mutex
	connected bool
	hidReady  bool
	lastErr   string
	lastErrAt time.Time
}

func NewOneKVM(cfg Config) *OneKVM {
	return &OneKVM{
		baseURL:    strings.TrimRight(cfg.OneKVMURL, "/"),
		username:   cfg.OneKVMUser,
		password:   cfg.OneKVMPassword,
		totpSecret: cfg.OneKVMTOTPSecret,
		fixedToken: cfg.OneKVMSession,
		http:       &http.Client{Timeout: 15 * time.Second},
	}
}

// AuthMode reports how this client authenticates, for the status panel.
func (k *OneKVM) AuthMode() string {
	if k.fixedToken != "" {
		return "session-token"
	}
	return "password"
}

func (k *OneKVM) BaseURL() string { return k.baseURL }

// Check forces a login and a HID connection, so the page's "test" button can
// surface problems before you try a real switch.
func (k *OneKVM) Check(ctx context.Context) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := k.ensureLocked(ctx); err != nil {
		k.recordErr(err)
		return err
	}
	return nil
}

// Send writes one sequence. On a connection error it reconnects once and
// retries, so a stale socket costs you a retry rather than a failed switch.
func (k *OneKVM) Send(ctx context.Context, steps []Step, timing Timing) error {
	k.mu.Lock()
	defer k.mu.Unlock()

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if err := k.ensureLocked(ctx); err != nil {
			lastErr = err
			break
		}
		// One-KVM tells us at handshake time whether HID is usable at all.
		// Reconnecting will not change that, so fail loudly instead of writing
		// frames the server will silently drop.
		if !k.hidIsReady() {
			lastErr = errHIDUnavailable
			break
		}
		if err := k.writeStepsLocked(ctx, steps, timing); err != nil {
			lastErr = err
			k.closeConnLocked()
			if ctx.Err() != nil {
				break
			}
			continue
		}
		return nil
	}
	k.recordErr(lastErr)
	return lastErr
}

func (k *OneKVM) Close() {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.closeConnLocked()
}

// --- internals, all of these expect k.mu to be held ---

func (k *OneKVM) ensureLocked(ctx context.Context) error {
	if k.session == "" {
		if k.fixedToken != "" {
			k.session = k.fixedToken
		} else if err := k.loginLocked(ctx); err != nil {
			return err
		}
	}
	if k.conn == nil {
		if err := k.connectLocked(ctx); err != nil {
			// A session can go stale (One-KVM restart clears them, and they are
			// held in memory only). Retry once with a fresh login.
			if k.fixedToken == "" {
				k.session = ""
				if err := k.loginLocked(ctx); err != nil {
					return err
				}
				if err := k.connectLocked(ctx); err != nil {
					return err
				}
			} else {
				return err
			}
		}
	}
	return nil
}

func (k *OneKVM) loginLocked(ctx context.Context) error {
	body, _ := json.Marshal(map[string]string{
		"username": k.username,
		"password": k.password,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		k.baseURL+"/api/auth/login", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := k.http.Do(req)
	if err != nil {
		return fmt.Errorf("连接 One-KVM 失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("登录被拒绝 (HTTP %d): %s", resp.StatusCode, apiMessage(raw))
	}

	var out struct {
		Next        string `json:"next"`
		ChallengeID string `json:"challenge_id"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("登录响应无法解析: %w", err)
	}

	if out.Next == "totp" {
		if k.totpSecret == "" {
			return fmt.Errorf("该账户启用了两步验证，请在配置里提供 ONEKVM_TOTP_SECRET，" +
				"或为此账户关闭两步验证")
		}
		code, err := totpCode(k.totpSecret, time.Now())
		if err != nil {
			return fmt.Errorf("生成两步验证码失败: %w", err)
		}
		totpBody, _ := json.Marshal(map[string]string{
			"challenge_id": out.ChallengeID,
			"code":         code,
		})
		treq, err := http.NewRequestWithContext(ctx, http.MethodPost,
			k.baseURL+"/api/auth/login/totp", bytes.NewReader(totpBody))
		if err != nil {
			return err
		}
		treq.Header.Set("Content-Type", "application/json")
		tresp, err := k.http.Do(treq)
		if err != nil {
			return fmt.Errorf("两步验证请求失败: %w", err)
		}
		defer tresp.Body.Close()
		traw, _ := io.ReadAll(io.LimitReader(tresp.Body, 1<<16))
		if tresp.StatusCode != http.StatusOK {
			return fmt.Errorf("两步验证被拒绝 (HTTP %d): %s", tresp.StatusCode, apiMessage(traw))
		}
		for _, c := range tresp.Cookies() {
			if c.Name == oneKvmCookieName {
				k.session = c.Value
			}
		}
		if k.session == "" {
			return fmt.Errorf("两步验证通过，但响应里没有 %s cookie", oneKvmCookieName)
		}
		return nil
	}

	for _, c := range resp.Cookies() {
		if c.Name == oneKvmCookieName {
			k.session = c.Value
		}
	}
	if k.session == "" {
		return fmt.Errorf("登录成功，但响应里没有 %s cookie", oneKvmCookieName)
	}
	return nil
}

func (k *OneKVM) connectLocked(ctx context.Context) error {
	wsURL := toWebSocketURL(k.baseURL) + "/api/ws/hid"
	header := http.Header{}
	header.Set("Cookie", oneKvmCookieName+"="+k.session)

	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	conn, resp, err := websocket.Dial(dialCtx, wsURL, &websocket.DialOptions{
		HTTPHeader: header,
	})
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusUnauthorized {
			return fmt.Errorf("打开 HID 通道被拒绝，会话无效或已过期: %w", err)
		}
		return fmt.Errorf("打开 HID 通道失败: %w", err)
	}
	conn.SetReadLimit(1 << 20)

	// The server opens with a single status byte: 0x00 ready, 0x01 HID unavailable.
	readCtx, readCancel := context.WithTimeout(ctx, 5*time.Second)
	_, data, err := conn.Read(readCtx)
	readCancel()
	if err != nil {
		conn.CloseNow()
		return fmt.Errorf("读取 HID 通道状态失败: %w", err)
	}
	k.setConnState(true, len(data) > 0 && data[0] == 0x00)
	k.conn = conn
	go k.drain(conn)
	return nil
}

// drain keeps reads flowing so control frames are processed, and notices when
// the socket dies. Without it the connection would look alive long after it
// is not.
func (k *OneKVM) drain(conn *websocket.Conn) {
	ctx := context.Background()
	for {
		if _, _, err := conn.Read(ctx); err != nil {
			k.mu.Lock()
			if k.conn == conn {
				k.conn = nil
				k.setConnState(false, false)
			}
			k.mu.Unlock()
			return
		}
	}
}

func (k *OneKVM) writeStepsLocked(ctx context.Context, steps []Step, timing Timing) error {
	for _, step := range steps {
		if step.WaitMs > 0 {
			if err := sleepCtx(ctx, time.Duration(step.WaitMs)*time.Millisecond); err != nil {
				return err
			}
			continue
		}
		for i, f := range step.Frames {
			// Hold the keys briefly: insert the pause at the down->up boundary.
			if !f.Down && i > 0 && step.Frames[i-1].Down {
				if err := sleepCtx(ctx, time.Duration(timing.HoldMs)*time.Millisecond); err != nil {
					return err
				}
			}
			if err := k.conn.Write(ctx, websocket.MessageBinary, EncodeFrame(f)); err != nil {
				return fmt.Errorf("发送按键失败: %w", err)
			}
		}
		if err := sleepCtx(ctx, time.Duration(timing.StepDelayMs)*time.Millisecond); err != nil {
			return err
		}
	}
	return nil
}

func (k *OneKVM) closeConnLocked() {
	if k.conn != nil {
		k.conn.CloseNow()
		k.conn = nil
	}
	k.setConnState(false, false)
}

// --- reported state, guarded by status.mu rather than mu ---

func (k *OneKVM) setConnState(connected, hidReady bool) {
	k.status.mu.Lock()
	k.status.connected = connected
	k.status.hidReady = hidReady
	k.status.mu.Unlock()
}

func (k *OneKVM) hidIsReady() bool {
	k.status.mu.Lock()
	defer k.status.mu.Unlock()
	return k.status.hidReady
}

func (k *OneKVM) recordErr(err error) {
	if err == nil {
		return
	}
	k.status.mu.Lock()
	k.status.lastErr = err.Error()
	k.status.lastErrAt = time.Now()
	k.status.mu.Unlock()
}

// Snapshot is a point-in-time view for the status endpoint.
type Snapshot struct {
	Connected   bool
	HIDReady    bool
	LastError   string
	LastErrorAt time.Time
}

func (k *OneKVM) Snapshot() Snapshot {
	k.status.mu.Lock()
	defer k.status.mu.Unlock()
	return Snapshot{
		Connected:   k.status.connected,
		HIDReady:    k.status.hidReady,
		LastError:   k.status.lastErr,
		LastErrorAt: k.status.lastErrAt,
	}
}

// --- helpers ---

func toWebSocketURL(base string) string {
	switch {
	case strings.HasPrefix(base, "https://"):
		return "wss://" + strings.TrimPrefix(base, "https://")
	case strings.HasPrefix(base, "http://"):
		return "ws://" + strings.TrimPrefix(base, "http://")
	default:
		return "ws://" + base
	}
}

func apiMessage(raw []byte) string {
	var out struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &out); err == nil && out.Message != "" {
		return out.Message
	}
	// The error body may also be a bare string.
	s := strings.TrimSpace(string(raw))
	if len(s) > 200 {
		s = s[:200]
	}
	if s == "" {
		return "(空响应)"
	}
	return s
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// totpCode implements RFC 6238 with SHA-1 and 6 digits, which is what
// authenticator apps use. It lets the service log in when the One-KVM account
// has two-factor auth enabled.
func totpCode(secret string, now time.Time) (string, error) {
	normalized := strings.ToUpper(strings.NewReplacer(" ", "", "-", "", "=", "").Replace(secret))
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(normalized)
	if err != nil {
		return "", fmt.Errorf("TOTP 密钥不是合法的 base32: %w", err)
	}

	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], uint64(now.Unix()/30))

	mac := hmac.New(sha1.New, key)
	mac.Write(counter[:])
	sum := mac.Sum(nil)

	offset := sum[len(sum)-1] & 0x0f
	value := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", value%1000000), nil
}
