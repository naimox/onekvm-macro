'use strict';

const $ = id => document.getElementById(id);
const STORE = 'onekvm-macro-config';

// UI preferences only. The channel count, the active channel and the switch
// function states all come from the server, and so does the key timing unless
// you override it in the advanced settings.
//
// Timing is deliberately NOT given a default here. The page used to ship its
// own hardcoded value and always send it, which silently overrode HOLD_MS and
// STEP_DELAY_MS — setting those in the environment did nothing for the web UI.
const defaults = {
  template: 'ScrollLock ScrollLock {port} Enter',
  preset: 'scrolllock',
  macros: [
    { name: 'Ctrl+Alt+Del', seq: 'Ctrl+Alt+Del' },
    { name: 'Esc', seq: 'Escape' },
  ],
};

const saved = JSON.parse(localStorage.getItem(STORE) || '{}');
const state = Object.assign({}, defaults, saved);
delete state.channels;
delete state.current;

// Which timing fields this browser has explicitly overridden. A field that is
// false keeps tracking the server's STEP_DELAY_MS / HOLD_MS on every load, so
// changing the container environment still takes effect.
const timingOverrides = {
  stepDelayMs: saved.stepDelayMs !== undefined,
  holdMs: saved.holdMs !== undefined,
};

let presets = [];
let loggedIn = false;
let pollTimer = null;
let channels = 0;
let currentPort = 0;

// Bumped whenever an action starts that changes state on the server. A status
// response that was already in flight when the action began describes the world
// from before it: applying that payload paints the old channel back over the new
// one until the next poll, which looks exactly like the switch bouncing.
let actionSeq = 0;

// True while a key sequence is on its way to the switch. One sequence takes
// about a second, and without a visible in-flight state the only feedback is the
// highlight — so an impatient second click stacks a second real sequence on top
// of the first, and the switch really does move twice.
let busy = false;

// Toggle functions. The elements are kept per id so a poll can update the badge
// in place instead of rebuilding the cards.
let fnIds = '';
let fnEls = {};
let fnNames = {};

// The last timing the server reported, kept so the reset button can restore it
// without another round trip.
let serverTiming = null;

const STATE_LABELS = { unknown: '未知', on: '开启', off: '关闭' };
function stateLabel(s) { return STATE_LABELS[s] || s; }

function show(el) { el.classList.remove('hidden'); }
function hide(el) { el.classList.add('hidden'); }

function save() {
  const out = Object.assign({}, state);
  // Never persist a value that merely mirrors the server, or it would freeze
  // the server's setting in place for this browser.
  if (!timingOverrides.stepDelayMs) delete out.stepDelayMs;
  if (!timingOverrides.holdMs) delete out.holdMs;
  try {
    localStorage.setItem(STORE, JSON.stringify(out));
  } catch (e) {
    // Storage can be unavailable (private mode, quota); the tool still works
    // for this session, the settings just will not persist.
    console.warn('无法保存设置：', e);
  }
}

function showResult(text, ok) {
  const el = $('result');
  el.textContent = text;
  el.className = 'result show ' + (ok ? 'ok' : 'err');
  clearTimeout(showResult.timer);
  showResult.timer = setTimeout(() => el.classList.remove('show'), 6000);
}

function showLoginError(text) {
  const el = $('login-error');
  el.textContent = text;
  el.className = 'result show err';
}

function clearLoginError() {
  $('login-error').className = 'result';
  $('login-error').textContent = '';
}

// --- one action at a time ---

// setBusy greys out every control that can send keystrokes, so the page cannot
// quietly queue a second sequence behind the one still in flight.
function setBusy(on) {
  busy = on;
  const controls = document.querySelectorAll(
    '#channels button, #functions button, #send-template, #free-send, #test, #quick-port');
  for (let i = 0; i < controls.length; i++) controls[i].disabled = on;
}

// beginAction returns false when something is already being sent. Every caller
// is a button or a keypress, so refusing is the right answer — the alternative
// is two sequences racing to the switch.
function beginAction(message) {
  if (busy) {
    showResult('上一条指令还在发送，请稍候。', false);
    return false;
  }
  actionSeq++;
  setBusy(true);
  if (message) showResult(message, true);
  return true;
}

// endAction always re-reads the status, which is also what settles the
// optimistic highlight on whatever the server actually recorded.
function endAction() {
  setBusy(false);
  return refreshStatus();
}

function showLogin(message) {
  loggedIn = false;
  setBusy(false); // an action that died with the session must not leave the UI locked
  clearInterval(pollTimer);
  pollTimer = null;
  hide($('app-view'));
  show($('login-view'));
  if (message) showLoginError(message);
  $('login-pass').value = '';
  $('login-user').focus();
}

function showApp() {
  loggedIn = true;
  hide($('login-view'));
  show($('app-view'));
  refreshStatus();
  if (!pollTimer) pollTimer = setInterval(refreshStatus, 5000);
}

async function api(path, body) {
  const headers = {};
  if (body) headers['Content-Type'] = 'application/json';

  const res = await fetch(path, {
    method: body ? 'POST' : 'GET',
    headers,
    credentials: 'same-origin',
    body: body ? JSON.stringify(body) : undefined,
  });

  const data = await res.json().catch(() => ({}));
  if (!res.ok) {
    // A 401 means the session is gone (or was never there). Drop back to the
    // login screen rather than leaving the user clicking buttons that fail.
    if (res.status === 401) {
      if (data.authMode) setLoginHint(data.authMode);
      showLogin(loggedIn ? '登录状态已失效，请重新登录。' : null);
    }
    throw new Error(data.error || ('HTTP ' + res.status));
  }
  return data;
}

function setLoginHint(authMode) {
  $('login-hint').textContent = authMode === 'onekvm'
    ? '请使用 One-KVM 的账号密码登录。'
    : '请使用配置的账号登录。';
}

function setDot(id, cls) { $(id).className = 'dot ' + (cls || ''); }

// applyServerState folds a /api/status payload into the page. The channel count
// and the active channel are whatever the server says — the browser keeps no
// opinion of its own, which is what makes several devices agree.
function applyServerState(s) {
  if (s.presets && s.presets.length) {
    presets = s.presets;
    renderPresets();
  }

  // The server's timing is the default; a field this browser has edited is an
  // override and keeps its own value.
  const t = s.timing || {};
  serverTiming = t;
  if (!timingOverrides.stepDelayMs && typeof t.stepDelayMs === 'number') {
    state.stepDelayMs = t.stepDelayMs;
    $('step-delay').value = t.stepDelayMs;
  }
  if (!timingOverrides.holdMs && typeof t.holdMs === 'number') {
    state.holdMs = t.holdMs;
    $('hold').value = t.holdMs;
  }

  const n = Number(s.channels) || 0;
  if (n && n !== channels) {
    channels = n;
    $('quick-port').max = n;
    renderChannels();
  }
  applyCurrent(Number(s.currentPort) || 0);

  if (s.functions && s.functions.length) {
    const ids = s.functions.map(f => f.id).join(',');
    if (ids !== fnIds) {
      fnIds = ids;
      renderFunctions(s.functions);
    }
    applyFunctionStates(s.functions);
  }
}

async function refreshStatus() {
  if (!loggedIn) return;
  const seq = actionSeq;
  try {
    const s = await api('/api/status');
    // Anything that changed while this request was on the wire leaves the
    // payload out of date. Drop it and let the next poll (or the action's own
    // completion) supply the current picture.
    if (busy || seq !== actionSeq) return;
    applyServerState(s);

    setDot('s-backend', 'ok');
    $('s-backend-text').textContent = '正常';
    setDot('s-kvm', s.connected ? 'ok' : 'err');
    $('s-kvm-text').textContent = s.connected ? s.oneKvmUrl : '未连接';
    $('s-hid-text').textContent = s.hidReady ? '可用' : (s.connected ? '不可用' : '未知');

    if (s.lastError) showResult(s.lastError, false);
  } catch (e) {
    if (loggedIn) {
      setDot('s-backend', 'err');
      $('s-backend-text').textContent = e.message;
      setDot('s-kvm', 'err');
    }
  }
}

function renderPresets() {
  const sel = $('preset');
  sel.textContent = '';
  presets.forEach(p => {
    const o = document.createElement('option');
    o.value = p.id;
    o.textContent = p.name;
    sel.appendChild(o);
  });
  sel.value = state.preset;
}

function renderChannels() {
  const box = $('channels');
  box.textContent = '';
  for (let i = 1; i <= channels; i++) {
    const b = document.createElement('button');
    b.type = 'button';
    b.className = 'chan';
    b.textContent = i;
    b.onclick = () => switchTo(i);
    box.appendChild(b);
  }
  applyCurrent(currentPort);
}

// applyCurrent updates the highlight in place instead of rebuilding the grid,
// so the five-second poll does not make the buttons flicker.
function applyCurrent(port) {
  currentPort = port;
  const kids = $('channels').children;
  for (let i = 0; i < kids.length; i++) {
    kids[i].className = 'chan' + (i + 1 === port ? ' active' : '');
  }
}

function renderMacros() {
  const box = $('macros');
  box.textContent = '';
  if (!state.macros.length) {
    const p = document.createElement('p');
    p.className = 'hint hint-flush';
    p.textContent = '还没有自定义宏，点下面的「管理宏」添加。';
    box.appendChild(p);
    return;
  }
  state.macros.forEach(m => {
    const b = document.createElement('button');
    b.type = 'button';
    b.className = 'macro-btn';

    const name = document.createElement('span');
    name.textContent = m.name || '(未命名)';
    const seq = document.createElement('small');
    seq.textContent = m.seq;

    b.append(name, seq);
    b.onclick = () => sendMacro(m.seq, m.name);
    box.appendChild(b);
  });
}

function renderEditor() {
  const box = $('editor');
  box.textContent = '';

  state.macros.forEach((m, idx) => {
    const row = document.createElement('div');
    row.className = 'editor-item';

    const name = document.createElement('input');
    name.type = 'text';
    name.value = m.name;
    name.placeholder = '名称';
    name.oninput = () => { state.macros[idx].name = name.value; save(); renderMacros(); };

    const seq = document.createElement('input');
    seq.type = 'text';
    seq.value = m.seq;
    seq.placeholder = '序列';
    seq.spellcheck = false;
    seq.oninput = () => { state.macros[idx].seq = seq.value; save(); renderMacros(); };

    const del = document.createElement('button');
    del.type = 'button';
    del.className = 'del';
    del.textContent = '删除';
    del.onclick = () => {
      state.macros.splice(idx, 1);
      save(); renderMacros(); renderEditor();
    };

    row.append(name, seq, del);
    box.appendChild(row);
  });

  const add = document.createElement('button');
  add.type = 'button';
  add.textContent = '＋ 新增宏';
  add.onclick = () => {
    state.macros.push({ name: '新宏', seq: 'ScrollLock ScrollLock 1 Enter' });
    save(); renderMacros(); renderEditor();
  };
  box.appendChild(add);
}

// --- toggle functions (auto scan, buzzer) ---

function renderFunctions(list) {
  const box = $('functions');
  box.textContent = '';
  fnEls = {};
  fnNames = {};

  list.forEach(f => {
    const card = document.createElement('div');
    card.className = 'fn-card';

    const head = document.createElement('div');
    head.className = 'fn-head';
    const name = document.createElement('span');
    name.className = 'fn-name';
    name.textContent = f.name;
    const badge = document.createElement('span');
    badge.className = 'fn-state ' + f.state;
    head.append(name, badge);

    const note = document.createElement('p');
    note.className = 'hint hint-flush';
    note.textContent = f.note || '';

    const seq = document.createElement('code');
    seq.className = 'fn-seq';
    seq.textContent = f.template;

    const toggle = document.createElement('button');
    toggle.type = 'button';
    toggle.textContent = '切换';
    toggle.onclick = () => toggleFunction(f.id);

    const fix = document.createElement('div');
    fix.className = 'fn-fix';
    const fixLabel = document.createElement('span');
    fixLabel.textContent = '实际是：';
    const on = document.createElement('button');
    on.type = 'button';
    on.textContent = '开启';
    on.onclick = () => correctFunction(f.id, 'on');
    const off = document.createElement('button');
    off.type = 'button';
    off.textContent = '关闭';
    off.onclick = () => correctFunction(f.id, 'off');
    fix.append(fixLabel, on, off);

    card.append(head, note, seq, toggle, fix);
    box.appendChild(card);

    fnEls[f.id] = { badge, seq };
    fnNames[f.id] = f.name;
  });
}

function applyFunctionStates(list) {
  list.forEach(f => {
    const e = fnEls[f.id];
    if (!e) return;
    e.badge.textContent = stateLabel(f.state);
    e.badge.className = 'fn-state ' + f.state;
    if (e.seq.textContent !== f.template) e.seq.textContent = f.template;
  });
}

async function toggleFunction(id) {
  if (!beginAction('正在发送切换指令…')) return;
  try {
    const r = await api('/api/function', {
      name: id,
      action: 'toggle',
      // Only the preset, never the port template: that one carries {port} and
      // would be nonsense as a function hotkey.
      preset: state.preset,
      stepDelayMs: state.stepDelayMs,
      holdMs: state.holdMs,
    });
    if (r.stillUnknown) {
      showResult('「' + (fnNames[id] || id) + '」已发送切换指令。切换器不回报状态，' +
        '请看一眼或听一声，然后用「实际是」把记录纠正过来。', true);
    } else {
      showResult('「' + (fnNames[id] || id) + '」已切换，现在记录为：' + stateLabel(r.state), true);
    }
  } catch (e) {
    showResult('发送失败：' + e.message, false);
  } finally {
    endAction();
  }
}

async function correctFunction(id, st) {
  // Sends no keystrokes, but it does change what every browser shows, so a
  // status response already in flight must not paint the old value back.
  actionSeq++;
  try {
    await api('/api/function', { name: id, state: st });
    showResult('「' + (fnNames[id] || id) + '」已记录为：' + stateLabel(st), true);
    refreshStatus();
  } catch (e) {
    showResult('记录失败：' + e.message, false);
  }
}

async function switchTo(port) {
  if (!beginAction('正在切换到通道 ' + port + '…')) return;
  applyCurrent(port); // optimistic; the response settles it

  const body = { port, stepDelayMs: state.stepDelayMs, holdMs: state.holdMs };
  if (state.template) body.template = state.template;
  if (state.preset) body.preset = state.preset;

  try {
    const r = await api('/api/switch', body);
    if (r.currentPort) applyCurrent(Number(r.currentPort));
    showResult('已切换到通道 ' + port + '：' + r.steps.join(' → '), true);
  } catch (e) {
    showResult('切换失败：' + e.message, false);
  } finally {
    endAction();
  }
}

async function sendMacro(seq, name) {
  if (!beginAction('正在发送…')) return;
  try {
    const r = await api('/api/macro', {
      sequence: seq,
      stepDelayMs: state.stepDelayMs,
      holdMs: state.holdMs,
    });
    showResult((name ? '「' + name + '」' : '') + '已发送：' + r.steps.join(' → '), true);
  } catch (e) {
    showResult('发送失败：' + e.message, false);
  } finally {
    endAction();
  }
}

// --- keyboard channel selection ---

// Up to 9 channels a single key is enough. Above that the digits are buffered:
// "1" then "6" reaches channel 16, while "2" commits immediately because no
// channel can start with 2 beyond 29.
let pendingDigits = '';
let pendingTimer = null;

function updateDigitHint() {
  $('chan-buffer').textContent = pendingDigits ? '输入中：' + pendingDigits + '…' : '';
}

function commitDigits() {
  clearTimeout(pendingTimer);
  pendingTimer = null;
  const n = Number(pendingDigits);
  pendingDigits = '';
  updateDigitHint();
  if (n >= 1 && n <= channels) switchTo(n);
}

function onDigitKey(d) {
  clearTimeout(pendingTimer);
  pendingDigits += d;

  // Out of range means the earlier digits were a false start, e.g. "1" then "9"
  // on a 16-channel setup. Keep just the newest digit.
  if (Number(pendingDigits) > channels) pendingDigits = d;

  const n = Number(pendingDigits);
  if (n < 1 || n > channels) {
    pendingDigits = '';
    updateDigitHint();
    return;
  }

  // A longer number could still land on a real channel, so wait for it.
  if (n * 10 <= channels) {
    updateDigitHint();
    pendingTimer = setTimeout(commitDigits, 700);
    return;
  }
  commitDigits();
}

// --- login ---

$('login-form').onsubmit = async e => {
  e.preventDefault();
  clearLoginError();

  const user = $('login-user').value.trim();
  const pass = $('login-pass').value;
  if (!user || !pass) {
    showLoginError('请填写用户名和密码。');
    return;
  }

  $('login-submit').disabled = true;
  try {
    const res = await fetch('/api/login', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      credentials: 'same-origin',
      body: JSON.stringify({ username: user, password: pass }),
    });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) {
      showLoginError(data.error || ('登录失败（HTTP ' + res.status + '）'));
      $('login-pass').value = '';
      return;
    }
    showApp();
  } catch (err) {
    showLoginError('无法连接后端：' + err.message);
  } finally {
    $('login-submit').disabled = false;
  }
};

$('logout').onclick = async () => {
  try {
    await fetch('/api/logout', { method: 'POST', credentials: 'same-origin' });
  } catch (e) {
    // Even if the call fails, drop the local view back to the login screen.
  }
  showLogin('已退出登录。');
};

// --- wiring ---

$('preset').onchange = e => {
  state.preset = e.target.value;
  const p = presets.find(x => x.id === state.preset);
  if (p) {
    state.template = p.portTemplate;
    $('template').value = p.portTemplate;
  }
  save();
  // The function hotkeys share the same prefix, so re-read their resolved
  // sequences from the server.
  refreshStatus();
};

$('template').oninput = e => { state.template = e.target.value; save(); };
$('step-delay').oninput = e => {
  state.stepDelayMs = Number(e.target.value) || 0;
  timingOverrides.stepDelayMs = true;
  save();
};
$('hold').oninput = e => {
  state.holdMs = Number(e.target.value) || 0;
  timingOverrides.holdMs = true;
  save();
};

// Drops this browser's overrides and goes back to whatever the server is
// configured with. Without it, an old saved value from a previous version would
// keep shadowing the server's setting with no way to clear it.
$('timing-reset').onclick = () => {
  timingOverrides.stepDelayMs = false;
  timingOverrides.holdMs = false;
  delete state.stepDelayMs;
  delete state.holdMs;
  save();

  if (serverTiming) {
    state.stepDelayMs = serverTiming.stepDelayMs;
    state.holdMs = serverTiming.holdMs;
    $('step-delay').value = serverTiming.stepDelayMs;
    $('hold').value = serverTiming.holdMs;
    showResult(`已恢复为服务端的默认时序（步进 ${serverTiming.stepDelayMs}ms，保持 ${serverTiming.holdMs}ms）。`, true);
  } else {
    showResult('已清除本机的覆盖值。', true);
  }
  refreshStatus();
};

$('test').onclick = async () => {
  try {
    await api('/api/status');
    try {
      await api('/api/macro', { sequence: 'wait:1' });
      showResult('后端正常，One-KVM 的 HID 通道可用。', true);
    } catch (e) {
      showResult('后端正常，但 One-KVM 通道有问题：' + e.message, false);
    }
  } catch (e) {
    showResult('连接失败：' + e.message, false);
  }
  refreshStatus();
};

$('send-template').onclick = () => switchTo(Number($('quick-port').value) || 1);
$('free-send').onclick = () => sendMacro($('free').value, '');
$('free').onkeydown = e => { if (e.key === 'Enter') $('free-send').click(); };

$('toggle-editor').onclick = () => {
  const box = $('editor');
  const opening = box.classList.contains('hidden');
  if (opening) {
    renderEditor();
    show(box);
    $('toggle-editor').textContent = '收起';
  } else {
    hide(box);
    $('toggle-editor').textContent = '管理宏';
  }
};

document.addEventListener('keydown', e => {
  if (!loggedIn) return;
  const tag = (e.target.tagName || '').toLowerCase();
  if (tag === 'input' || tag === 'textarea' || tag === 'select') return;
  if (e.ctrlKey || e.altKey || e.metaKey) return;
  if (e.key >= '0' && e.key <= '9') {
    e.preventDefault();
    onDigitKey(e.key);
  }
});

// --- init ---

const isLocal = ['localhost', '127.0.0.1', '[::1]'].includes(location.hostname);
if (location.protocol !== 'https:' && !isLocal) show($('warn-insecure'));

$('template').value = state.template;
if (state.stepDelayMs !== undefined) $('step-delay').value = state.stepDelayMs;
if (state.holdMs !== undefined) $('hold').value = state.holdMs;

renderMacros();

// Nothing is drawn until the server reports how many channels exist. A 401 here
// is the normal "not logged in yet" path.
(async () => {
  try {
    const s = await api('/api/status');
    applyServerState(s);
    showApp();
  } catch (e) {
    if (!loggedIn) showLogin();
  }
})();
