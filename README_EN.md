# RapidProxy

[中文说明](README.md) | **English**

Turn **WorkBuddy** (international) and **CodeBuddy** (China) accounts into a local **OpenAI-compatible API**. Sign in once, and any client that supports a custom OpenAI endpoint (Cherry Studio, ChatBox, LobeChat, Open WebUI, Cursor, Cline, various SDKs and scripts, …) can call the models directly.

- Language / framework: Go + Wails v2 (native WebView, small installer, low memory footprint)
- Platforms: Windows / macOS / Linux
- UI: clean graphical interface + system tray; closing the window only minimizes to the tray, real exit lives in the tray menu

> The upstream protocol is based on reverse-engineering results from
> [BevalZ/workbuddy-proxy](https://github.com/BevalZ/workbuddy-proxy)
> (a CLIProxyAPI plugin). This project is an independent rewrite: it does not
> depend on CLIProxyAPI and ships its own UI, tray icon, and HTTP service.

---

## Features

- **Graphical sign-in**: built-in presets for WorkBuddy (`www.workbuddy.ai`) and CodeBuddy (`copilot.tencent.com`). Click "Sign in" and the official authorization page (Google / GitHub / QR code) opens in your system browser; tokens are saved and refreshed automatically. Sign-in status is shown on the Overview tab.
- **OpenAI-compatible service**: exposes `/v1/models`, `/v1/chat/completions` (plus equivalent paths without `/v1`) and `/healthz`. Both streaming and non-streaming requests are supported (the upstream only offers streaming; the proxy aggregates SSE server-side).
- **Multi-account load balancing**: sign in with several accounts per provider; requests are served round-robin. Expired tokens are refreshed automatically and 401/403 responses are retried once.
- **Multi-provider routing**: use the bare model name (auto-matched), or prefix it with `provider:model` / `provider/model`; you can also force routing with the `X-RapidProxy-Provider` header or `?provider=`, and pin an account with `X-RapidProxy-Account`.
- **API key auth**: multiple keys supported with one-click generation; with no key configured, requests are unauthenticated (recommended only for loopback listening).
- **Built-in + auto-synced model list**: a built-in whitelist keeps models callable even when the upstream config endpoint under-reports; new upstream models are pulled periodically. Manual "extra models" and "disabled models" are also supported.
- **Deep thinking & tool calls**: `hy3` models automatically request the highest reasoning effort; `delta.reasoning_content` is passed through; OpenAI-style `tool_calls` deltas are merged correctly by `index`.
- **Request sanitization**: minimal word-level rewrites of fixed system-template sentences blacklisted upstream, reducing the chance of content blocking; max thinking effort can be forced globally.
- **System tray**: show the main window / copy endpoint URL / reset window position / start-stop service / quit. The close button minimizes to the tray (with a first-time hint).
- **Launch at login**: on by default; after you sign in to the OS, the app runs silently in the tray (no window). Toggle it off in Settings; the system autostart entry is cleaned up automatically and re-pointed after an update moves the install path.
- **Single instance**: launching a second copy detects the running one, shows a notice, and exits — no duplicate processes fighting over the port or config.
- **Optional CORS**: off by default; when enabled, only local pages (localhost/127.0.0.1) may call cross-origin, preventing random websites from abusing your local proxy.
- **Window auto-fit**: the window size is converged to the current screen (size + DPI scale) and centered, so it never overflows on small screens or high-DPI setups.
- **In-app updates**: new versions are checked on GitHub Releases automatically; the installer is downloaded, launched, and the app exits (NSIS wizard on Windows, PKG installer on macOS, in-place AppImage replacement on Linux).
- **Logging**: real-time in-UI logs + file logs (keys redacted), ring buffer to prevent unbounded growth.

---

## UI Overview

Four tabs in the left sidebar:

| Tab | Contents |
|---|---|
| **Overview** | Access info card (Base URL, API Key show/hide/regenerate, model list entry, curl example), service status, provider list, live logs |
| **Accounts** | Pick a provider to sign in, copy the sign-in link, login status polling, manage/delete signed-in accounts |
| **Models** | Available models (name, context length, output limit, source provider) with search and a manual "sync models" button |
| **Settings** | Listen address, model auto-sync interval, sanitization / forced-thinking toggles, CORS toggle, provider on-off and proxy, API key management, data file locations |

Top bar: status pill, listen address, "Minimize to tray", "Reset window", "Start/Stop service". "Quit" sits at the bottom of the sidebar.

---

## Installation

### Option 1: Download a prebuilt package (recommended)

Grab the **installer** (or portable archive) for your platform from [Releases](https://github.com/znsoftm/RapidProxy/releases):

| Platform | Installer | Portable | Notes |
|---|---|---|---|
| Windows | `RapidProxy-windows-amd64-setup.exe` (NSIS wizard) | `RapidProxy-windows-amd64.zip` | Requires WebView2 Runtime (preinstalled on Win10/11); the finish page has "Run RapidProxy" checked by default |
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
- Accounts are served round-robin (cursor); requests for the same account are serialized to avoid concurrent refresh races.
- The upstream blocks certain fixed system-template sentences; request sanitization performs minimal word-level rewrites before forwarding.

---

## FAQ

**A third-party client gets HTTP 404 "unknown path /v1/models/chat/completions"?**
Do **not** put `/models` in the API URL — that is a complete "list models" endpoint, not a base URL.
Use `http://127.0.0.1:8787/v1` (or `http://127.0.0.1:8787`; the client appends paths itself).
The server is also tolerant of this misconfiguration: any path ending with `/chat/completions` (POST) or `/models` (GET) is routed to the corresponding endpoint.

**Sign-in seems stuck / keeps spinning?**
Make sure you actually completed the sign-in in the browser. If your browser blocks the redirect, use "Copy sign-in link" and finish on your phone or another browser — the app polls for the result automatically.

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
