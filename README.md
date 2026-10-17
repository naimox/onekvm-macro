# One-KVM 通道切换器

一个很小的网页 + Go 后端，用来向 One-KVM 的受控机发送键盘宏。接在受控机上的是 **KVM 切换器**，这个网页就是它的控制面板：

- **切换通道**：`ScrollLock ×2 → 通道号 → Enter`
- **自动扫描**：`ScrollLock ×2 → S → Enter`
- **蜂鸣器**：`ScrollLock ×2 → B → Enter`

所有状态记录在服务端，多个设备登录后看到的是同一份。

> **对外暴露之前先读本文的「安全模型」一节。** 这个服务等同于键盘权限，任何能登录它的人都可以在受控机上敲任意键，包括进 BIOS 改启动顺序。

## 为什么需要它

Rust 版 One-KVM 没有键盘宏功能。虚拟键盘顶部那几个组合键（Ctrl+Alt+Del 等）是写死在源码里的，界面上也没有任何录制或自定义宏的入口。而且 One-KVM **没有提供任何"发送一次按键"的 HTTP 接口**——唯一能注入按键的通道是 WebSocket `/api/ws/hid`，走一套二进制帧协议。

顺便说，为什么不做成纯前端的单文件网页：One-KVM 的会话 cookie 是 `SameSite=Lax`，而浏览器的 WebSocket API 又不允许自定义请求头，所以从别的站点打开的网页根本没法通过鉴权。让一个后端替你登录、替你发帧，就没有这个限制了。

## 它是怎么工作的

1. 用账号密码（或你给的会话令牌）登录 One-KVM，拿到 `one_kvm_session` cookie。
2. 用这个 cookie 打开一条 `/api/ws/hid` WebSocket，**并一直保持**。
3. 每次切换时，把按键序列编码成 4 字节的二进制帧写进去。

保持长连接是有意的：One-KVM 在 WebSocket 客户端断开时会执行 `hid.reset()`，如果每次切换都重新连一次，就会不断重置 HID 状态，把你在控制台里正按着的键给松开。

帧格式（来自 One-KVM 的 `src/hid/datachannel.rs`）：

```
键盘帧 = [0x01, 0x00 按下 / 0x01 抬起, HID usage 码, 修饰键位掩码]
```

## 快速开始

### 用 Docker Compose

```bash
cp docker-compose.example.yml docker-compose.yml
```

改掉 `ONEKVM_URL`、`ONEKVM_USER`、`ONEKVM_PASSWORD` 三项即可，然后：

```bash
docker compose up -d
```

打开 `http://<跑这个服务的机器IP>:8081`，用 **One-KVM 的账号密码**登录。

### 用 docker run

```bash
docker build -t onekvm-macro .
```

```bash
docker run -d --name onekvm-macro --restart unless-stopped -p 8081:8081 -e ONEKVM_URL=http://192.168.1.50:8080 -e ONEKVM_USER=admin -e ONEKVM_PASSWORD=change-me onekvm-macro
```

### 直接跑二进制

```bash
go build -trimpath -o onekvm-macro .
```

```bash
ONEKVM_URL=http://192.168.1.50:8080 ONEKVM_USER=admin ONEKVM_PASSWORD=change-me BIND_ADDR=127.0.0.1 ./onekvm-macro
```

这个容器和 One-KVM 的那个容器不一样：**不需要 `--privileged`，不需要挂载 `/dev` 或 `/sys`，也不需要 `--net=host`**。

## 登录与账号

网页用登录的方式进入，默认**直接复用 One-KVM 的账号密码**——不用再记一套，改密码也只有一个地方要改（`ONEKVM_PASSWORD`）。

**也就是说 `ONEKVM_USER` / `ONEKVM_PASSWORD` 只设一次就够了。** 配置里那组 `AUTH_USER` / `AUTH_PASSWORD` 属于下面说的另一种模式，默认不填、也不生效；翻配置文件时看到两组名字容易以为要重复设置，其实不是。

如果你想给这个服务一个独立的账号（比如想和别人共用一个只切换的弱权限入口），才需要换成下面这个模式：

```bash
AUTH_MODE=manual
AUTH_USER=admin
AUTH_PASSWORD=<你自己设的密码>
```

用了 `manual` 之后就与 One-KVM 的账号完全脱钩，改 One-KVM 的密码不会影响这个网页的登录。

登录状态放在一个 `HttpOnly` + `SameSite=Lax` 的会话 cookie 里，默认 24 小时，每次操作会自动续期。会话存在内存中，**重启容器需要重新登录**。

登录尝试默认限速每分钟 5 次，防止有人对着登录框爆破。

## 安全模型

### 这个服务的权限有多高

它能做两件事：向你指定的 One-KVM 登录，以及**向受控机注入任意按键**。第二件事等同于把键盘交给访问者——可以在 BIOS 里改启动顺序、在登录界面试密码、在系统里执行任意命令。

### 已经内置的防护

| 防护 | 说明 |
|---|---|
| 登录才可用 | 除登录页和静态资源外，所有接口都需要有效会话 |
| 登录限速 | 默认 5 次/分钟，返回 429 |
| 常数时间比较 | 用户名和密码都用 `crypto/subtle`，两次比较都执行，不泄漏时序 |
| 会话 cookie 加固 | `HttpOnly` + `SameSite=Lax` + （启用 TLS 时）`Secure` |
| 拒绝跨站表单 | 见下一节，这是关键的一条 |
| 内容类型校验 | 只接受 `application/json`，并拒绝正文里的尾随数据 |
| 操作限速 | 全部 `/api/*` 默认 120 次/分钟，发按键的接口收紧到 30 次/分钟 |
| IP 白名单 | 可选 `ALLOWED_IPS`，回环地址始终放行 |
| 严格 CSP | 页面没有内联脚本和样式，`default-src 'none'` |
| 安全响应头 | `nosniff`、`DENY`、`no-referrer`、`no-store`、HSTS（启用 TLS 时） |
| 审计日志 | 登录成功/失败、每次发送、鉴权拒绝都记录来源 IP，日志里不含密码和 cookie |
| 服务器超时 | Read/Write/Idle 全部设上限 |

### 为什么不用额外的 CSRF 令牌

因为已经有三道锁同时挡着了：

1. **会话 cookie 是 `SameSite=Lax`**，浏览器不会把它附加到跨站 POST 上。
2. **所有会改变状态的接口只接受 POST**，而跨站攻击最典型的载体（HTML 表单）恰恰只能发 GET 和 POST。
3. **所有接口强制 `Content-Type: application/json`**。HTML 表单只能发出 `text/plain`、`multipart/form-data` 这类安全清单内的内容类型，而这类请求**不会触发 CORS 预检**——也就是说浏览器不会替你拦。所以必须在服务端按内容类型拒绝，这一条是纵深防御的关键。

第 3 条我曾经在自己这版代码里漏掉过，实测确实能被绕过（伪造正文 `{"port":3}=` 会因为 Go 的 JSON 解码器忽略尾随内容而被当成合法请求），现在两处都补上了并有回归测试。

### 对外暴露怎么做

你现在是直接映射端口，这种部署下按重要性排：

1. **启用 TLS。** 你用的是登录表单，**密码要在网络上传输**。不加密就是明文过互联网——这比随机令牌还糟，因为密码往往是你别处在用的。设置 `TLS_CERT` 和 `TLS_KEY` 指向证书文件即可（自签也行，浏览器会警告但链路是加密的）。没配 TLS 时，启动日志会打一条醒目的警告，登录页也会显示提示。

   一个容易踩的坑：容器以 `nobody` 运行，**证书私钥必须对它可读**。root 属主、`0600` 权限的私钥文件会让服务启动失败。
2. **别用默认端口**，减少被扫描器命中的概率。
3. **把 `AUTH_MODE` 设成 `manual` 用一个独立密码**，避免这个服务的密码和 One-KVM 的密码是同一个——万一泄漏，损失可控。
4. **定期看审计日志**，`grep AUDIT` 就能看到谁在什么时候登录、切了哪个通道。
5. 可选但有效：配 `ALLOWED_IPS` 限制来源网段；更稳的做法是用 WireGuard / Tailscale / EasyTier 把服务藏起来，前面再放个反向代理。

另外：One-KVM 自己的 8080 同样不要直接暴露。

### 关于 One-KVM 的凭据

容器环境变量里的密码，`docker inspect` 就能看到。更好的选择是用 `ONEKVM_SESSION` 给一个会话令牌（浏览器 F12 → Application → Cookies 里复制 `one_kvm_session` 的值）。它是可撤销、会过期的，而且顺便解决下面那个多会话问题。注意这种模式下没有账号密码可用于网页登录，需要同时把 `AUTH_MODE` 设成 `manual`。

## 先看这一节：One-KVM 的「允许多会话登录」

One-KVM 默认**不允许多会话**。这个选项关着的时候，任何一次新的登录都会把已有的会话全部清空。本服务在启动时会登录一次用于建立 HID 连接，这会**把你浏览器里的 One-KVM 控制台挤下线**（提示 "Logged in elsewhere"）。

两个办法：

**办法一（推荐）**：在 One-KVM 网页的**设置**里打开「允许多会话登录」这个开关。

**办法二**：改用 `ONEKVM_SESSION`，本服务就不再登录 One-KVM。

注意这只影响本服务与 One-KVM 之间的连接；网页登录本服务（`AUTH_MODE=onekvm` 时）只是本地比对密码，**不会**再去登录一次 One-KVM，所以不会因为你在网页上登录而把控制台挤掉。

## 环境变量

| 变量 | 默认值 | 说明 |
|---|---|---|
| `ONEKVM_URL` | 必填 | One-KVM 地址，例如 `http://192.168.1.50:8080` |
| `ONEKVM_USER` | — | One-KVM 登录账号 |
| `ONEKVM_PASSWORD` | — | One-KVM 登录密码 |
| `ONEKVM_SESSION` | 空 | 改用会话令牌代替账号密码（此时需配 `AUTH_MODE=manual`） |
| `ONEKVM_TOTP_SECRET` | 空 | 账户开了两步验证时填 TOTP 密钥 |
| `AUTH_MODE` | `onekvm` | 网页登录用哪套账号：`onekvm` 复用上面两项，`manual` 用下面两项 |
| `AUTH_USER` | `admin` | `manual` 模式的登录用户名 |
| `AUTH_PASSWORD` | 必填（manual 时） | `manual` 模式的登录密码，至少 8 字符 |
| `SESSION_TTL_HOURS` | `24` | 登录状态保持多久 |
| `LOGIN_RATE_LIMIT_PER_MIN` | `5` | 登录尝试限速，`0` 表示不限 |
| `BIND_ADDR` | `0.0.0.0` | 监听地址。放在反向代理后面时用 `127.0.0.1` |
| `TLS_CERT` / `TLS_KEY` | 空 | 证书和私钥路径，设置后启用 HTTPS |
| `ALLOWED_IPS` | 空 | 可选的来源限制，支持裸 IP 和 CIDR，回环地址始终放行 |
| `TRUST_PROXY` | `false` | 是否信任 `X-Forwarded-For`。只有确实有可信反代时才开 |
| `API_TOKEN` | 空 | 可选。设了之后自动化脚本可以用 `Authorization: Bearer` 绕过登录 |
| `RATE_LIMIT_PER_MIN` | `120` | 全部 `/api/*` 的每分钟上限 |
| `ACTION_RATE_LIMIT_PER_MIN` | `30` | 发按键接口的每分钟上限 |
| `PORT` | `8081` | 监听端口 |
| `CHANNELS` | `16` | 切换器通道数（1–64）。超过 9 的通道会把十位、个位依次按下去 |
| `STEP_DELAY_MS` | `100` | 每步之间的间隔（毫秒）。页面「高级设置」里改的只覆盖当前浏览器 |
| `HOLD_MS` | `100` | 按键从按下到抬起的保持时间（毫秒）。同样可被页面临时覆盖 |
| `DEFAULT_TEMPLATE` | `ScrollLock ScrollLock {port} Enter` | 默认热键模板 |
| `AUTOSCAN_TEMPLATE` | 空 | 自动扫描的序列。留空则跟随所选预设（`<前缀> S <确认键>`） |
| `BUZZER_TEMPLATE` | 空 | 蜂鸣器的序列。留空则跟随所选预设（`<前缀> B <确认键>`） |

## 页面用法

- **通道切换**：点数字按钮即可。键盘也能用：通道不超过 9 时单个数字键直达；超过 9 时连按两位，例如先 `1` 再 `6` 就是通道 16（页面上会显示「输入中：16」，约 0.7 秒没有后续数字就按当前值提交）。连按出了一个越界的组合（比如 16 路下按了 `1` 再 `9`）会自动回退成最后一位。
- **当前通道**：高亮的是**最近一次切换到的通道**，记录在服务端，所有登录的设备看到的是同一个。详见下面「多设备同步」一节。
- **切换器功能**：自动扫描和蜂鸣器两张卡片，每张有「切换」按钮和一个状态徽标。状态同样是服务端共享的，但含义和通道**不一样**，见下面同一节。
- **热键序列**：下拉框里是几套常见切换器的写法，选中会同时改变通道和功能键的前缀。下面的序列框可手动改。
- **自定义宏**：点「管理宏」增删，每个宏是「名称 + 按键序列」。
- **自由发送**：随手敲一段序列发出去，排错时最有用。
- **高级设置**：步进延迟、按键保持。这两个值默认由服务端的 `STEP_DELAY_MS` / `HOLD_MS` 决定，在这里改了只覆盖**当前这个浏览器**（存在 localStorage 里），其它设备仍用服务端的值。想全局改就去改容器的环境变量；改完服务端之后，以前动过这两项的浏览器会继续用它自己的值，点「恢复服务端默认」即可跟上。

界面设置存在浏览器 localStorage 里（登录状态不在，它在 `HttpOnly` cookie 里）。

## 多设备同步

所有状态都记录在**服务端内存**里，不在浏览器里，所以多台设备天然看到同一份：

- `POST /api/switch` 发送成功后记下通道号，`POST /api/function` 处理后记下功能状态；
- `GET /api/status` 返回 `currentPort` 和 `functions[].state`；
- 页面本来就在每 5 秒轮询一次状态，所以任何一台设备改了，其余设备最多 5 秒后跟上——不需要 WebSocket、不需要 SSE，复用已有的轮询就是最简做法。

`/api/macro`（自由发送、自定义宏）**不会**改动任何记录。它发的是一串任意按键，未必是切通道或切功能，把它当成状态变更反而是错的。

### 通道

**记录的是"意图"，不是"读数"。** KVM 切换器不回报自己实际停在哪一路（没有读回接口，视频画面也读不出来），所以任何方案都只能做到这一步。后果是：如果你用切换器面板上的按钮或者另一个键盘切过去，这里显示的会过期。修正很简单——点一下你认为正确的那一路就行。

### 开关型功能

自动扫描和蜂鸣器的状态有三挡：**未知 / 开启 / 关闭**，不是两挡。"未知"是一个真实状态，不是占位符：

- 切换器同样不回报这两个功能的实际状态，而且它们比通道更难观察（蜂鸣器要听声音，自动扫描要盯着画面有没有在变）；
- 所以从"未知"出发按一次「切换」，结果**仍然是未知**——你并不知道原来是什么，翻一下并不能让你知道现在是什么。服务端就是这么实现的，响应里会带 `stillUnknown: true`，页面据此提示你去确认；
- 建立一个真实值的唯一办法是**你看过之后告诉它**。每张卡片下面的「实际是：开启 / 关闭」就是干这个的，它只改记录，**不发送任何按键**。

一旦状态已知，再按「切换」就会正常翻转（开启 ↔ 关闭），并且同步到所有设备。

### 几个取舍

- 状态**不落盘**，容器重启后回到"未记录"（通道为 0，功能为未知）。它本来就是推断，持久化一个可能已经错的值意义不大。真要保留，加个数据卷写进文件即可。
- 嫌 5 秒慢的话，可以改成 SSE 或 WebSocket 推送。代价是多维护一条长连接和它的重连逻辑，对这个用途我认为不值当。
- 如果你有多个 KVM 切换器，这些记录是全局单值；要按切换器分开，得再引入一个标识。

## HTTP 接口

浏览器用会话 cookie 访问。脚本可以用两种方式：带上会话 cookie，或者（如果配了 `API_TOKEN`）用 Bearer 令牌。

### 切通道

```bash
curl -X POST http://127.0.0.1:8081/api/switch -H "Content-Type: application/json" -H "Authorization: Bearer $API_TOKEN" -d '{"port":3}'
```

可选的 `preset`（`scrolllock` / `scrolllock-numpad` / `ctrl` / `ctrl-numpad` / `alt`）、`template`、`stepDelayMs`、`holdMs` 会覆盖默认值。

### 发送任意序列

```bash
curl -X POST http://127.0.0.1:8081/api/macro -H "Content-Type: application/json" -H "Authorization: Bearer $API_TOKEN" -d '{"sequence":"Ctrl+Alt+Del"}'
```

### 开关型功能

```bash
curl -X POST http://127.0.0.1:8081/api/function -H "Content-Type: application/json" -H "Authorization: Bearer $API_TOKEN" -d '{"name":"buzzer","action":"toggle"}'
```

发送切换热键并翻转记录。从"未知"出发时记录仍是未知，响应里会带 `"stillUnknown": true`。

```bash
curl -X POST http://127.0.0.1:8081/api/function -H "Content-Type: application/json" -H "Authorization: Bearer $API_TOKEN" -d '{"name":"buzzer","state":"on"}'
```

只修正记录，**不发送任何按键**。`state` 只能是 `on` 或 `off`。

`name` 目前是 `autoscan` 或 `buzzer`。这两个字段二选一，同时给或都不给都会返回 400。可选的 `preset` / `template` / `stepDelayMs` / `holdMs` 与切通道那边含义相同。

注意 `template` 是**功能键**的序列，不要把手输框里那个带 `{port}` 的通道模板填进来。

### 查状态

```bash
curl -H "Authorization: Bearer $API_TOKEN" http://127.0.0.1:8081/api/status
```

返回 `currentPort`、`functions[].state`、预设列表和当前时序。`/api/keys` 列出所有可用按键名。

### 没有 API_TOKEN 时的脚本用法

用 curl 的 cookie jar 先登录一次，后续请求复用即可：

```bash
curl -c /tmp/okm.jar -X POST http://127.0.0.1:8081/api/login -H "Content-Type: application/json" -d '{"username":"admin","password":"你的密码"}'
```

```bash
curl -b /tmp/okm.jar -X POST http://127.0.0.1:8081/api/switch -H "Content-Type: application/json" -d '{"port":3}'
```

`Content-Type` **必须**是 `application/json`，否则返回 400。`/api/keys` 列出所有可用按键名。

## 序列语法

空格分隔，每个词是"一步"：

| 写法 | 含义 |
|---|---|
| `ScrollLock` | 按下并抬起这个键 |
| `3` | 等于 `Digit3`，主键盘数字 |
| `Numpad3` / `N3` | 小键盘数字 |
| `16` | 双位通道：展开成 `Digit1` 再 `Digit6`（`{port}` 是纯文本替换，所以 10 以上会以单个词到达，解析时再拆开） |
| `Numpad16` | 小键盘版本：展开成 `Numpad1` 再 `Numpad6` |
| `S` / `B` | 单独的字母就是对应的字母键，等于 `KeyS` / `KeyB`（切换器手册一般就是这么写的） |
| `Ctrl+Alt+Del` | 组合键：按住前面的修饰键，点一下最后那个键 |
| `ControlLeft` | 单独按一下修饰键。`Ctrl ×2` 这类热键就是这么写的 |
| `wait:150` | 停顿 150 毫秒 |

修饰键名：`Ctrl`/`ControlLeft`/`ControlRight`、`Shift`、`Alt`、`Win`（也叫 `Meta`）。

组合键的展开方式跟 One-KVM 自己前端的宏实现完全一致：依次按下修饰键（掩码逐位累加）→ 按下主键（带完整掩码）→ 抬起主键 → 逆序抬起修饰键。所以 `Ctrl+Alt+Del` 展开成：

```
01 00 E0 01   Ctrl 按下，掩码 0x01
01 00 E2 05   Alt 按下，掩码 0x05
01 00 4C 05   Del 按下，掩码 0x05
01 01 4C 05   Del 抬起
01 01 E2 01   Alt 抬起，掩码 0x01
01 01 E0 00   Ctrl 抬起，掩码 0x00
```

`{port}` 占位符会在切换时替换成通道号。

拆数字的规则只在那个词**不是**一个真实按键名时生效，所以 `F16` 仍然是 F16 这一个键，不会被拆成 `F1` `F6`。

## 排错

**登录提示用户名或密码不正确。** 默认模式下要填 One-KVM 的账号密码。如果你给 One-KVM 开了两步验证，网页登录本服务不受影响（只是本地比对密码），但本服务连 One-KVM 需要 `ONEKVM_TOTP_SECRET`。

**登录返回 429。** 触发了登录限速，等 `Retry-After` 指定的秒数。

**用着用着跳回登录页。** 会话过期（默认 24 小时无操作）或容器重启过。会话存在内存里，重启即失效。

**切换器没反应。** 最可能是热键写法或时序不对。依次试：换预设（有些切换器只认 `Ctrl ×2`）；把「步进延迟」从 100 调到 200 甚至 300；换 `scrolllock-numpad`（有些只认小键盘数字，且要求 NumLock 亮着）。用「自由发送」只发一个 `ScrollLock` 可以缩小范围。

**点一下通道，切换器却在两个通道之间来回切。** 先用日志分辨：每发一次按键，容器日志里就有一行 `AUDIT ... action=send detail="switch port=N"`。

```bash
docker compose logs --tail=50 | grep 'action=send'
```

点一次只多一行，说明后端只发了一次，问题在切换器那头——最常见的是它的**自动扫描**开着，扫描时它本来就会自己轮询各个通道。页面上的「自动扫描」卡片显示「开启」的话，就是本服务打开的，按一下「切换」关掉；显示「未知」则可能是切换器面板上被打开过。

一次点击出现多行才是页面重复发了指令。旧版确实会这样：发送一次序列要一秒左右，期间界面没有任何进行中的提示，而一个晚到的轮询结果还会把通道高亮刷回旧值——看着像没生效，再点一下就是第二条真实指令。现在发送期间按钮会锁住、晚到的旧状态会被丢弃，连点只会发一条。

**状态显示 HID 不可用。** One-KVM 自己在连接时告诉我们的。去 One-KVM 网页的鼠键设置里看 HID 后端是否就绪。

**显示的通道不对。** 切换器不回报实际通道，界面显示的是"最近一次切换到的通道"。你用切换器的面板按钮或另一个键盘切过，它就会过期——点一下你认为正确的那一路即可对上。

**通道 10 以上按了没反应。** 确认 `CHANNELS` 配够了（它决定通道号上限），以及切换器本身支持那么多路。序列本身不用改，`{port}` 会自动拆成两位。

**自动扫描 / 蜂鸣器按了没反应。** 先确认你的切换器确实用 `S` 和 `B` 这两个字母（不同型号会不同）。用「自由发送」手输完整序列可以直接验证，比如 `ScrollLock ScrollLock B Enter`。型号不一样就用 `AUTOSCAN_TEMPLATE` / `BUZZER_TEMPLATE` 覆盖。

**功能状态一直是「未知」。** 这是正常的，不是 bug——切换器不回报这两个功能的状态。你确认过实际值之后，用卡片上的「实际是」告诉它，之后就能正常翻转了。

**返回 403。** 来源地址不在 `ALLOWED_IPS` 里。

## 开发与测试

```bash
go vet ./...
go test ./...
```

功能测试用 `httptest` 起一个假的 One-KVM，断言登录请求正文、握手带的 cookie、以及**实际写进去的每一个字节**，并覆盖 HID 不可用、会话失效后重登、两步验证、固定令牌等路径。通道相关的用例断言 16 路会发出 `Digit1` 再 `Digit6`，自由宏不会改动共享状态。

切换器功能相关的用例断言：五个预设生成的通道序列与重构前完全一致（防止重构悄悄改变发出去的字节）、功能键前缀跟随预设、从未知状态切换之后仍是未知、已知状态下切换会翻转、两个功能的状态互不干扰、非法请求（未知功能名、`action` 与 `state` 同时给或都不给、非法状态值）返回 400。

还有一个用例专门锁住「状态查询不能排在按键序列后面」：发送一条 1.5 秒的序列时，`Snapshot()` 必须立刻返回。它以前和发送共用一把锁，于是切换过程中发起的轮询会等整条序列发完，再带着切换**之前**的通道号返回，正好把页面上的高亮刷回旧值。

安全测试覆盖：三种认证模式的配置校验（含复用 One-KVM 账号但只有 `ONEKVM_SESSION` 时的明确报错）、登录成功/失败/限速、会话 cookie 的 `HttpOnly` 与 `SameSite=Lax` 属性、伪造和过期的会话被拒、登出后会话失效、跨站表单正文（`text/plain`）被拒、JSON 尾随数据被拒、未登录时拿不到 One-KVM 地址、安全响应头齐全且 CSP 不含 `unsafe-inline`。

需要 Go 1.23 或更高（依赖 `github.com/coder/websocket`）。构建镜像时模块代理默认走 `goproxy.cn`，可用 `--build-arg GOPROXY=...` 覆盖。

要在 x86 机器上给 ARM 设备构建镜像：

```bash
docker buildx build --platform linux/arm64 -t onekvm-macro:arm64 .
```
