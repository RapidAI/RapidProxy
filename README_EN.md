# RapidProxy

[中文说明](README.md) | **English**

Turn **WorkBuddy** (international), **CodeBuddy** (China), **LobsterAI**, **Trae** (China / International), **Qoder** (China / International) and **OpenCode** accounts — plus any **xAI / Claude / ChatGPT / Zhipu** or arbitrary OpenAI- or Anthropic-compatible endpoint — into a local **OpenAI-compatible API**. Sign in once, and any client that supports a custom OpenAI endpoint (Cherry Studio, ChatBox, LobeChat, Open WebUI, Cursor, Cline, various SDKs and scripts, …) can call the models directly.

- Language / framework: Go + Wails v2 (native WebView, small installer, low memory footprint)
- Platforms: Windows / macOS / Linux
- UI: clean graphical interface + system tray; closing the window only minimizes to the tray, real exit lives in the tray menu

> The upstream protocol is based on reverse-engineering results from
> [BevalZ/workbuddy-proxy](https://github.com/BevalZ/workbuddy-proxy)
> (a CLIProxyAPI plugin). This project is an independent rewrite: it does not
> depend on CLIProxyAPI and ships its own UI, tray icon, and HTTP service.

---

## Supported providers

Each signable provider is one **group**: accounts inside a group are served with weighted round-robin and act as each other's failover. When a vendor ships China / International editions, they are two independent groups (accounts are signed in separately). If any account in a group runs into trouble (rate-limited, out of credits, upstream unavailable, model not enabled on it, auth failure…), the proxy skips it on the forwarding side and moves to the next account in the group — the error is not passed through to the client. Only when no account in the whole group can serve the request does the client see an error.

| Group (provider ID) | Vendor | Sign-in | Notes |
|---|---|---|---|
| `workbuddy` | WorkBuddy (International) | Browser authorization | Polling sign-in, tokens auto-refreshed |
| `codebuddy` | CodeBuddy (China) | Browser authorization | Same protocol family as WorkBuddy |
| `lobsterai` | LobsterAI (NetEase Youdao) | Browser callback | Local callback port exchanges the token, streaming only |
| `trae-cn` | Trae (China) | Browser authorization callback | SOLO protocol, dual-token rotation |
| `trae-global` | Trae (International) | Browser authorization callback | Singapore deployment, separate from the China edition |
| `qoder-cn` | Qoder (China) | Device code (PKCE) | COSY-signed chat, approve in the browser |
| `qoder-global` | Qoder (International) | Device code (PKCE) | Separate from the China edition |
| `opencode` | OpenCode Zen (opencode.ai) | API key | Imports the key saved by the local OpenCode CLI and waits for confirmation (paste a new key to override), or paste a key directly; a proxy is recommended |
| `xai` | xAI (Grok) | API key | OpenAI wire protocol, `api.x.ai` |
| `claude` | Claude (Anthropic) | API key | Anthropic wire protocol, `api.anthropic.com` |
| `chatgpt` | ChatGPT (OpenAI) | API key | OpenAI wire protocol, `api.openai.com` |
| `zhipu` | Zhipu Coding (GLM) | API key | OpenAI wire protocol, coding-plan gateway `open.bigmodel.cn/api/coding/paas/v4`, client identity `OpenCode` |
| `zcode` | Zhipu Coding (ZCode sign-in) | Browser authorization | ZCode online OAuth (China `zcode.chatglm.site` / international `zcode.z.ai`) resolves a coding-plan API key and calls it via `/api/coding/paas/v4` as `OpenCode` |

> **Why `/api/coding/paas/v4` + `OpenCode`?** Zhipu's gifted quota belongs to the "coding plan" channel: it only applies when a recognized coding client (User-Agent `OpenCode`) calls the coding-specific gateway. Using the regular open-platform gateway (`/api/paas/v4`) or the plain Anthropic gateway (`/api/anthropic`) is billed as pay-as-you-go and returns `[1113] insufficient balance or no available resource pack` on an uncharged account. `user_agent` can be adjusted in `config.json` if needed.

### Custom models (each is its own group)

Beyond the fixed presets, the backend method `AddCustomProvider({name, baseUrl, apiKey, protocol, model})` adds **any** OpenAI- or Anthropic-compatible endpoint (DeepSeek, Moonshot, Qwen, Volcengine Ark, self-hosted gateways, …). **Every call creates an independent group**, so each custom model is its own group and never mixes with the other providers; adding the same name again reuses that group (merging models, keeping existing accounts). `baseUrl` must be a full `http(s)://` URL.

- `protocol` is `openai` (default) or `anthropic`; `baseURL` may include `/v1` or not (it is completed automatically).
- Zhipu's gifted quota is decided by the request **User-Agent** (coding-client identity), so the `zhipu` / `zcode` presets send `opencode` by default; if an account's quota requires a different client identity (e.g. `claude code`, `codex`), change `user_agent` in that profile in `config.json`.
- With the Anthropic wire protocol the proxy performs full **OpenAI ⇄ Anthropic** translation at the boundary (system, tools/tool_calls, thinking, streamed SSE deltas), so clients only ever see an OpenAI-compatible API.
- You can also edit `config.json` directly and add `{"id":"custom-xxx","kind":"custom","protocol":"anthropic","base_url":"...","extra_models":["..."]}` to `profiles`.

---

## Features

- **Graphical sign-in**: built-in presets for WorkBuddy, CodeBuddy, LobsterAI, Trae (China / International), Qoder (China / International), OpenCode, xAI, Claude, ChatGPT and Zhipu Coding (including ZCode sign-in). Click "Sign in" to authorize in your system browser or paste an API key; tokens/keys are saved and refreshed automatically. Sign-in status is shown on the Overview tab.
- **xAI / Claude / ChatGPT / Zhipu Coding**: use the fixed presets, or call `AddCustomProvider` to add any OpenAI- or Anthropic-compatible endpoint; the proxy translates requests and SSE streams both ways for the Anthropic wire protocol, transparently to the client.
- **ZCode sign-in**: Zhipu Coding supports ZCode online OAuth (with automatic China / international fallback), resolves a coding-plan API key after authorization, and calls it with a coding-client User-Agent so the gifted ZCode / coding-plan quota applies.
- **OpenAI-compatible service**: exposes `/v1/models`, `/v1/chat/completions` (plus equivalent paths without `/v1`) and `/healthz`. Both streaming and non-streaming requests are supported (streaming-only upstreams are aggregated server-side).
- **Multi-account load balancing**: sign in with several accounts per provider; requests are served by weighted round-robin. Expired tokens are refreshed automatically and 401/403 responses are retried once.
- **In-group failure auto-skip**: whenever an account runs into trouble, the proxy moves to the next account in the group instead of passing the error to the client — rate limits / exhausted credits / upstream outages cool the account down and switch; "this account can't serve this request" errors (model not enabled, bad parameters…) switch without cooling; for streaming requests, an upstream that reports failure at the very first SSE event (HTTP 200 + error frame) also triggers a switch before a single byte is written downstream; when a window-limited error states its reset time in the message body (e.g. code=6004 "usage will reset at …"), the cooldown lasts until that moment instead of a few dozen seconds. Only after every account in the group has been tried does the last error come back with its original status code (retryable failures are summarized as 429 + Retry-After).
- **Multi-provider routing**: use the bare model name (auto-matched), or prefix it with `provider:model` / `provider/model` (the International edition accepts the `intl:` shorthand); you can also force routing with the `X-RapidProxy-Provider` header (`intl` accepted) or `?provider=`, and pin an account with `X-RapidProxy-Account`.
- **Multi-provider model disambiguation**: when several providers are enabled, International (workbuddy) models are listed as `intl:model`; for the remaining providers, a bare model name belongs to the first provider (config order) that exposes it, and every other provider's copy is listed as `provider:model`; with a single provider enabled, all IDs stay plain.
- **Virtual model groups**: give a custom model name (e.g. `fast`) to a pool of models drawn from different providers. Clients request only that name; the proxy balances across members by weight and skips any member that is unavailable. Groups can be created, renamed, enabled/disabled and deleted, and they get their own traffic dimension plus a per-member distribution breakdown.
- **API key auth**: multiple keys supported with one-click generation; with no key configured, requests are unauthenticated (recommended only for loopback listening).
- **Built-in + auto-synced model list**: a built-in whitelist keeps models callable even when the upstream config endpoint under-reports; new upstream models are pulled periodically. Manual "extra models" and "disabled models" are also supported.
- **Deep thinking & tool calls**: `hy3` models automatically request the highest reasoning effort; `delta.reasoning_content` is passed through; OpenAI-style `tool_calls` deltas are merged correctly by `index`.
- **Request sanitization**: minimal word-level rewrites of fixed system-template sentences blacklisted upstream, reducing the chance of content blocking; max thinking effort can be forced globally.
- **System tray**: show the main window / copy endpoint URL / reset window position / start-stop service / quit. The close button minimizes to the tray (with a first-time hint).
- **Launch at login**: on by default; after you sign in to the OS, the app runs silently in the tray (no window). Toggle it off in Settings; the system autostart entry is cleaned up automatically and re-pointed after an update moves the install path.
- **Single instance**: launching a second copy detects the running one, shows a notice, and exits — no duplicate processes fighting over the port or config.
- **About page**: the sidebar "About" tab shows version, platform, author and copyright, plus the in-app update entry.
- **Optional CORS**: off by default; when enabled, only local pages (localhost/127.0.0.1) may call cross-origin, preventing random websites from abusing your local proxy.
- **Window auto-fit**: the window size is converged to the current screen (size + DPI scale) and centered, so it never overflows on small screens or high-DPI setups.
- **In-app updates**: new versions are checked on GitHub Releases automatically; the installer is downloaded, launched, and the app exits (NSIS wizard on Windows, PKG installer on macOS, in-place AppImage replacement on Linux).
- **Logging**: real-time in-UI logs + file logs (keys redacted), ring buffer to prevent unbounded growth.

---

## UI Overview

Six tabs in the left sidebar:

| Tab | Contents |
|---|---|
| **Overview** | Access info card (Base URL, API Key show/hide/regenerate, model list entry, curl example), service status, provider list, live logs |
| **Accounts** | Pick a provider to sign in, copy the sign-in link, login status polling, manage/delete signed-in accounts |
| **Models** | Available models (name, context length, output limit, source provider) with search and a manual "sync models" button |
| **Virtual** | Manage virtual model groups: create / edit / rename / enable / delete, set member weights, and see live availability per member |
| **Traffic** | Token usage by group (provider or virtual group) and model, switchable between day / week / month; selecting a virtual group also shows its per-member distribution |
| **Settings** | Listen address, model auto-sync interval, sanitization / forced-thinking toggles, CORS toggle, provider on-off and proxy, API key management, data file locations |

Top bar: status pill, listen address, "Minimize to tray", "Reset window", "Start/Stop service". "Quit" sits at the bottom of the sidebar.

---

## Installation

### Option 1: Download a prebuilt package (recommended)

Grab the **installer** (or portable archive) for your platform from [Releases](https://github.com/znsoftm/RapidProxy/releases):

| Platform | Installer | Portable | Notes |
|---|---|---|---|
| Windows | `RapidProxy-windows-setup.exe` (dual-arch NSIS installer, auto-detects x86/x64) | `RapidProxy-windows-amd64.zip` / `RapidProxy-windows-386.zip` | Requires WebView2 Runtime (preinstalled on Win10/11); the finish page has "Run RapidProxy" checked by default |
| macOS (Intel + Apple Silicon) | `RapidProxy-darwin-universal.pkg` (double-click, installs into Applications) | `RapidProxy-darwin-universal.tar.gz` | Universal binary; for the portable variant, drag `RapidProxy.app` into Applications |
| Linux | `RapidProxy-linux-amd64.AppImage` (no install; `chmod +x` and run) | `RapidProxy-linux-amd64.tar.gz` | Requires WebKitGTK (`libwebkit2gtk-4.0`); the AppImage does not bundle the system WebView runtime |

### Option 2: Build from source

You need Go 1.25+ and the [Wails v2](https://wails.io) prerequisites for your platform (Windows: WebView2; macOS: Xcode Command Line Tools; Linux: WebKitGTK dev packages). The frontend is a plain static page — **no Node.js required**.

```bash
git clone https://github.com/znsoftm/RapidProxy.git
cd RapidProxy
wails build -clean
# Output: build/bin/RapidProxy(.exe)
```

Or with the Go toolchain alone (icons/assets are provided via go:embed):

```bash
CGO_ENABLED=1 go build -o RapidProxy .
```

The repo ships a GitHub Actions workflow: pushes to `main` run tests and build checks automatically; pushing a `v*` tag publishes a three-platform release.

---

## Getting Started

1. Start the app (`RapidProxy.exe` on Windows). The proxy service starts automatically and listens on `http://127.0.0.1:8787`.
2. Go to the **Accounts** tab, pick "WorkBuddy (International)" or "CodeBuddy (China)", and click **Sign in**:
   - The official authorization page (Google / GitHub / WeChat QR) opens automatically in your **system default browser**; once you finish there, credentials are saved and the app picks up the result automatically;
   - If the browser does not open by itself, copy the sign-in link into any browser (including on your phone); the app polls for the result automatically;
   - The **Overview** tab shows each provider's sign-in status live (`Signed in · N accounts` / `Not signed in`);
   - Once signed in, the account appears in the list and its tokens are refreshed automatically.
3. Back on the **Overview** tab you will find the access info:

```
Base URL : http://127.0.0.1:8787/v1
API Key  : sk-wbp-xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx   (regenerate/add/remove in Settings)
```

> After changing the listen address the running service restarts on the new address automatically. When exposing `0.0.0.0` to the network, always configure an API key. The UI rewrites `0.0.0.0` to `127.0.0.1` for display and copy.

---

## Client Integration

### Cherry Studio / ChatBox / LobeChat / Open WebUI, etc.

Settings → Model provider → OpenAI compatible / Custom:

- **API URL**: `http://127.0.0.1:8787` (some clients want it to end at `/v1` — follow their hint)
- **API Key**: the key shown on the "Overview" tab (anything works when auth is disabled)
- Click "Fetch" to pull the model list automatically

### curl examples

```bash
# Streaming
curl http://127.0.0.1:8787/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer sk-wbp-xxxx" \
  -d '{"model":"gpt-5.4","stream":true,"messages":[{"role":"user","content":"Hello"}]}'

# Non-streaming (SSE from upstream is aggregated server-side)
curl http://127.0.0.1:8787/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer sk-wbp-xxxx" \
  -d '{"model":"glm-5.3","messages":[{"role":"user","content":"Hello"}]}'

# List models
curl http://127.0.0.1:8787/v1/models -H "Authorization: Bearer sk-wbp-xxxx"
```

### Python (openai SDK)

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://127.0.0.1:8787/v1",
    api_key="sk-wbp-xxxx",
)
resp = client.chat.completions.create(
    model="gpt-5.4",
    messages=[{"role": "user", "content": "Hello"}],
)
print(resp.choices[0].message.content)
```

### Multi-provider / multi-account routing

```bash
# Pin a provider (workbuddy / codebuddy); both spellings are equivalent
curl ... -d '{"model":"codebuddy:glm-5.3", ...}'
curl ... -d '{"model":"codebuddy/glm-5.3", ...}'

# Or use a header / query parameter
curl ... -H "X-RapidProxy-Provider: codebuddy" -d '{"model":"glm-5.3", ...}'

# Pin a specific account
curl ... -H "X-RapidProxy-Account: <account-id>" -d '{"model":"glm-5.3", ...}'
```

---

## Models

- **Built-in whitelist**: each provider ships with a hand-tested model list (context/output limits), so calls keep working even if the upstream config endpoint is temporarily unavailable.
- **Auto sync**: the upstream model list is pulled every 6 hours by default (change in Settings; `0` disables auto sync). New upstream models are added automatically.
- **Extra / disabled models**: manage by model ID (comma-separated) under Settings → Providers.
- **Deep thinking**: `hy3*` models automatically request the highest reasoning effort; the thinking text is returned in `reasoning_content`.

### Virtual model groups

A virtual model name pools models from several providers, so different vendors' models can behave as one availability pool:

- **Create**: open the Virtual tab, pick a name (e.g. `fast`), then add models from any provider that has a signed-in account. Whether a provider is globally enabled does not affect whether its models can join a virtual group. Each member has its own weight (default 1; higher weight gets more traffic).
- **Call it**: set `model` to the group name — it works exactly like a real model ID:

  ```bash
  curl http://127.0.0.1:8787/v1/chat/completions     -H "Authorization: Bearer sk-wbp-xxxx"     -d '{"model":"fast","messages":[{"role":"user","content":"hello"}]}'
  ```

- **Balancing and skipping**: members are served by smooth weighted round-robin. When a member as a whole is unavailable (provider disabled / not signed in / all accounts paused / all cooling down / the upstream does not accept that model), the request moves to the next member and the client never sees the error. Only after every member has been tried does the proxy return an error, following the existing rules (client errors keep their status code, retryable failures become 429 + Retry-After).
- **Naming rules**: no `:`, `/` or spaces (both are force-routing separators), and the name must not collide with a provider ID or `intl`. If it collides with a real model ID, the virtual group wins.
- **Precedence**: explicit force-routing (`provider:model`, `X-RapidProxy-Provider`, `?provider=`) takes precedence over a virtual group, so you can always bypass it temporarily.
- **Relationship to provider switches**: a virtual group is an explicit choice of "which provider's which model", and outranks the provider's global enable switch — members keep working even if the provider is disabled in Settings. To cut a group's traffic, disable the group itself, so bare model calls to that provider are unaffected.
- **Traffic stats**: each group has its own dimension, while every request is still recorded against the real provider and real model (for reconciling upstream quota). Selecting a virtual group also shows the per-member breakdown.
- **Rename and delete**: renaming migrates the traffic history to the new name; deleting clears the history so a later same-named group does not inherit old data.

---

## Settings Reference

| Setting | Default | Description |
|---|---|---|
| Listen address | `127.0.0.1:8787` | Use `0.0.0.0:8787` for LAN access (an API key is then a must) |
| Model sync interval | `6` hours | `0` disables auto sync |
| Request sanitization | On | Rewrites upstream-blacklisted template sentences to reduce blocking |
| Force max thinking | Off | Sends `reasoning_effort=high` on every request |
| CORS | Off | When on, only local pages (localhost/127.0.0.1) may call cross-origin |
| Provider proxy | Empty | Optional `http://` / `socks5://` proxy per provider |
| API Key | Empty | Empty = no auth (loopback use only); multiple keys and one-click generation supported |
| Launch at login | On | Runs silently in the tray after OS login (`--hidden`); turning it off removes the system autostart entry |

### In-app updates

The app silently checks [GitHub Releases](https://github.com/znsoftm/RapidProxy/releases) once at startup; you can also click "Check for updates" under **Settings → Software update**. When a new version is found, click **Download & install**:

- **Windows**: downloads `*-setup.exe`, launches the NSIS wizard, then exits the current app (the finish page has "Run RapidProxy" checked by default — click Finish to start the new version);
- **macOS**: downloads `*-universal.pkg`, opens the system installer, then exits (installs into `/Applications`);
- **Linux**: when running as an AppImage, the file is replaced in place and the app restarts; for other run modes the `*.AppImage` is downloaded into `<data dir>/update/` with a hint to run it manually.

Installers are stored under `<data dir>/update/`, using a `.part` temp file + atomic rename, so an interrupted download leaves no half-written file. A failed update check (e.g. GitHub unreachable) does not affect anything else.

---

## Data Files

| Platform | Location |
|---|---|
| Windows | `%USERPROFILE%\.rapidproxy` |
| macOS / Linux | `~/.rapidproxy` |

```
~/.rapidproxy
├── config.json                    # config (contains API keys — keep it private)
├── accounts/<provider>/<id>.json  # sign-in credentials (mode 0600)
└── logs/proxy.log                 # runtime log (keys redacted)
```

- Set the `RAPIDPROXY_DATA_DIR` environment variable to relocate the data directory;
- `RAPIDPROXY_*` environment variables override same-named config entries.

---

## How It Works

```
┌───────────────┐  OpenAI protocol  ┌────────────────────────────┐  private API  ┌─────────────────────┐
│ Any client    │ ────────────────▶ │ RapidProxy (127.0.0.1)     │ ────────────▶ │ WorkBuddy/CodeBuddy │
└───────────────┘  /v1/chat/...     │  auth·routing·RR·SSE agg.  │  /v2/...      └─────────────────────┘
                                    └────────────────────────────┘
```

- Client requests are rewritten to `stream: true` before forwarding (the upstream only supports streaming); when the client asked for non-streaming, SSE events are aggregated into a single `chat.completion` response.
- Tokens are refreshed 5 minutes before expiry; on 401/403 the proxy refreshes once and retries.
- Accounts are served by weighted round-robin (smooth WRR); requests for the same account are serialized to avoid concurrent refresh races.
- In-group failures are auto-skipped: any account error switches to the next account in the group rather than surfacing to the client; only a fully exhausted group returns the last error with its original status code (retryable ones as 429 + Retry-After).
- The upstream blocks certain fixed system-template sentences; request sanitization performs minimal word-level rewrites before forwarding.

---

## FAQ

**A third-party client gets HTTP 404 "unknown path /v1/models/chat/completions"?**
Do **not** put `/models` in the API URL — that is a complete "list models" endpoint, not a base URL.
Use `http://127.0.0.1:8787/v1` (or `http://127.0.0.1:8787`; the client appends paths itself).
The server is also tolerant of this misconfiguration: any path ending with `/chat/completions` (POST) or `/models` (GET) is routed to the corresponding endpoint.

**Sign-in seems stuck / keeps spinning?**
Make sure you actually completed the sign-in in the browser. If your browser blocks the redirect, use "Copy sign-in link" and finish on your phone or another browser — the app polls for the result automatically.

**I clicked Sign in for OpenCode but it didn't sign in right away?**
When a key saved by the local OpenCode CLI is detected, it is no longer saved automatically: submit with the box empty to sign in with that key, or paste a new key to sign in with it instead — no need to delete the local `auth.json` first. The key is verified online before saving, and a bad key is reported immediately.

**Service fails to start (port in use)?**
Pick another listen address (e.g. `127.0.0.1:8788`); a running service restarts on the new address automatically.

**Window overflows the screen / can't find the window?**
The window is auto-fit to the screen at startup. If it still misbehaves, use "Reset window" in the top bar or the tray menu. The log (Overview → Live log) contains a `screen xxxx (physical xxxx, scale xx%)` line — please include it in bug reports.

**UI won't open / white screen?**
On Windows without the WebView2 Runtime the app degrades to "tray + background service" mode; logs are at `~/.rapidproxy/logs/proxy.log`. Install the WebView2 Runtime and restart.

**How do I quit completely?**
The close button only minimizes to the tray; use the tray menu "Quit" or "Quit" at the bottom of the sidebar.

**Are my settings kept after an upgrade?**
Data lives separately from the program; replacing the executable does not touch config and credentials under `~/.rapidproxy`.

---

## Disclaimer

- This is a third-party tool for personal study and research. Not affiliated with WorkBuddy, CodeBuddy, or Tencent.
- Use in compliance with the relevant terms of service. Credentials are stored locally only — never share `config.json` or the `accounts/` directory.
- License: [MIT](LICENSE)
