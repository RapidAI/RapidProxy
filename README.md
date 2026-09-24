# RapidProxy

把 **WorkBuddy**（国际版）与 **CodeBuddy**（国内版）账号背后的模型，转换成 **OpenAI 兼容 API** 的桌面代理程序。登录一次，任何支持「自定义 OpenAI 接口」的客户端（Cherry Studio、ChatBox、LobeChat、Open WebUI、Cursor、Cline、各种 SDK 与脚本……）都能直接调用。

- 语言/框架：Go + Wails v2（原生 WebView，安装包小、内存占用低）
- 平台：Windows / macOS / Linux
- 界面：简洁图形界面 + 系统托盘；关闭窗口只是最小化到托盘，从托盘菜单真正退出

> 协议实现参考自 [BevalZ/workbuddy-proxy](https://github.com/BevalZ/workbuddy-proxy)
> （CLIProxyAPI 插件版）的逆向结论，本项目是**独立进程**的重写：不依赖 CLIProxyAPI，
> 自带界面、托盘与 HTTP 服务。

---

## 功能特性

- **图形化登录**：内置 WorkBuddy（`www.workbuddy.ai`）与 CodeBuddy（`copilot.tencent.com`）两个上游预设，点「登录」打开浏览器完成授权（Google / GitHub / 扫码），令牌自动保存并自动刷新。
- **OpenAI 兼容服务**：提供 `/v1/models`、`/v1/chat/completions`（及不带 `/v1` 的等价路径）与 `/healthz`，流式与非流式请求都支持（上游仅提供流式接口，程序在服务端自动聚合）。
- **多账号负载均衡**：同一上游可登录多个账号，请求按序轮询；单个账号令牌失效自动刷新，401/403 自动重试。
- **多上游路由**：模型名直接写（自动匹配）、或用 `上游ID:模型名` / `上游ID/模型名` 前缀显式指定；也可用请求头 `X-RapidProxy-Provider` 或 `?provider=` 强制路由，`X-RapidProxy-Account` 指定账号。
- **API Key 鉴权**：可配置多个 Key，支持一键生成；不配置则无需密钥（建议仅本机监听时使用）。
- **内置 + 自动同步模型清单**：内置白名单保证上游配置接口漏报时仍可调用；定期从上游拉取新模型，也支持手动「额外模型」与「禁用模型」。
- **深度思考与工具调用**：`hy3` 系列自动按最高思考档请求；`delta.reasoning_content` 原样透传；OpenAI 风格 `tool_calls` 增量按 `index` 正确合并。
- **请求净化**：可对上游黑名单的固定模板句做最小改写，降低触发内容拦截的概率；可全局强制开启最大思考档。
- **系统托盘**：显示主界面 / 复制接口地址 / 重置窗口位置 / 启停服务 / 退出；关闭按钮收进托盘（首次有提示）。
- **CORS 可选**：默认关闭；开启时仅对本地页面（localhost/127.0.0.1）放行，避免任意网页盗用本机代理。
- **窗口自适应**：按屏幕尺寸与系统缩放比例自动收敛窗口大小并居中，小屏 / 高 DPI 不会超出屏幕。
- **日志**：界面实时日志 + 文件日志（自动脱敏密钥），环形缓冲防膨胀。

---

## 界面说明

左侧边栏四个页签：

| 页签 | 内容 |
|---|---|
| **概览** | 接入信息卡（Base URL、API Key 显隐/重新生成、模型列表入口、curl 调用示例）、服务状态、上游列表、实时运行日志 |
| **账号** | 选择上游发起登录、复制登录链接、登录状态轮询、已登录账号管理与删除 |
| **模型** | 当前可用模型清单（名称、上下文长度、输出上限、来源上游），支持搜索与手动「同步模型」 |
| **设置** | 监听地址、模型自动同步间隔、请求净化/强制思考开关、CORS 开关、上游启停与代理地址、API Key 管理、数据文件位置 |

顶栏：服务状态胶囊、监听地址、「最小化到托盘」、「重置窗口」、「启动/停止服务」。侧边栏底部「退出程序」。

---

## 安装

### 方式一：下载预编译包（推荐）

到 [Releases](https://github.com/znsoftm/RapidProxy/releases) 下载对应平台压缩包，解压即用：

| 平台 | 文件 | 备注 |
|---|---|---|
| Windows | `RapidProxy-windows-amd64.zip` | 需要 WebView2 Runtime（Win10/11 一般自带） |
| macOS (Intel + Apple Silicon) | `RapidProxy-darwin-universal.tar.gz` | 通用二进制，解压后把 `RapidProxy.app` 拖入「应用程序」 |
| Linux | `RapidProxy-linux-amd64.tar.gz` | 需要 WebKitGTK（`libwebkit2gtk-4.0`） |

### 方式二：源码构建

需要 Go 1.25+ 与 [Wails v2](https://wails.io) 各平台依赖（Windows: WebView2；macOS: Xcode Command Line Tools；Linux: WebKitGTK 开发包）。前端为纯静态页面，**无需 Node.js**。

```bash
git clone https://github.com/znsoftm/RapidProxy.git
cd RapidProxy
wails build -clean
# 产物：build/bin/RapidProxy(.exe)
```

也可以只用 Go 工具链（图标/资源由 go:embed 提供）：

```bash
CGO_ENABLED=1 go build -o RapidProxy .
```

仓库自带 GitHub Actions workflow：推送到 `main` 自动跑测试与构建验证，打 `v*` 标签自动发布三平台 Release。

---

## 首次使用

1. 启动程序（Windows 下为 `RapidProxy.exe`）。代理服务默认自动启动，监听 `http://127.0.0.1:8787`。
2. 切到 **账号** 页签，选择「WorkBuddy（国际版）」或「CodeBuddy（国内版）」，点 **开始登录**：
   - 程序会打开系统浏览器进入登录页；
   - 也可以复制登录链接，到任意浏览器（含手机）打开，登录成功后回到程序等待自动完成；
   - 登录完成后账号出现在列表里，令牌由程序自动续期。
3. 回到 **概览** 页签，即可看到接入信息：

```
Base URL : http://127.0.0.1:8787/v1
API Key  : sk-wbp-xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx   （可在设置中重新生成/增删）
```

> 修改监听地址后，运行中的服务会自动以新地址重启；监听 `0.0.0.0` 对外网开放时请务必配置 API Key。界面展示与复制时 `0.0.0.0` 会自动替换为 `127.0.0.1`。

---

## 客户端接入

### Cherry Studio / ChatBox / LobeChat / Open WebUI 等

设置 → 模型服务商 → OpenAI 兼容 / 自定义：

- **API 地址**：`http://127.0.0.1:8787`（有的客户端要求填到 `/v1`，按其提示填）
- **API Key**：程序「概览」页显示的 Key（未启用鉴权时随便填）
- 模型列表点「获取」自动拉取

### curl 示例

```bash
# 流式
curl http://127.0.0.1:8787/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer sk-wbp-xxxx" \
  -d '{"model":"gpt-5.4","stream":true,"messages":[{"role":"user","content":"你好"}]}'

# 非流式（服务端自动聚合上游 SSE）
curl http://127.0.0.1:8787/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer sk-wbp-xxxx" \
  -d '{"model":"glm-5.3","messages":[{"role":"user","content":"你好"}]}'

# 查看模型
curl http://127.0.0.1:8787/v1/models -H "Authorization: Bearer sk-wbp-xxxx"
```

### Python（openai SDK）

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://127.0.0.1:8787/v1",
    api_key="sk-wbp-xxxx",
)
resp = client.chat.completions.create(
    model="gpt-5.4",
    messages=[{"role": "user", "content": "你好"}],
)
print(resp.choices[0].message.content)
```

### 多上游 / 多账号路由

```bash
# 显式指定上游（workbuddy / codebuddy），两种写法等价
curl ... -d '{"model":"codebuddy:glm-5.3", ...}'
curl ... -d '{"model":"codebuddy/glm-5.3", ...}'

# 或用请求头 / 查询参数
curl ... -H "X-RapidProxy-Provider: codebuddy" -d '{"model":"glm-5.3", ...}'

# 指定某个账号
curl ... -H "X-RapidProxy-Account: <账号ID>" -d '{"model":"glm-5.3", ...}'
```

---

## 模型

- **内置白名单**：每个上游内置一份实测模型清单（含上下文/输出上限），即使上游配置接口暂时取不到也能照常调用。
- **自动同步**：默认每 6 小时拉取一次上游模型清单（可在设置修改；填 `0` 关闭自动同步），上游新增模型自动加入。
- **额外模型 / 禁用模型**：在「设置 → 上游配置」里按模型 ID 手动增删（逗号分隔）。
- **深度思考**：`hy3` 等 `hy3*` 模型自动按最高思考档请求，思考内容在 `reasoning_content` 字段返回。

---

## 设置说明

| 设置项 | 默认值 | 说明 |
|---|---|---|
| 监听地址 | `127.0.0.1:8787` | 改成 `0.0.0.0:8787` 可局域网访问（需配 API Key） |
| 模型同步间隔 | `6` 小时 | 填 `0` 关闭自动同步 |
| 请求净化 | 开 | 改写上游黑名单模板句，降低拦截概率 |
| 强制最大思考 | 关 | 所有请求 `reasoning_effort=high` |
| CORS | 关 | 开启后仅本地页面（localhost/127.0.0.1）可跨域调用 |
| 上游代理 | 空 | 可为某个上游单独填 `http://` / `socks5://` 代理 |
| API Key | 空 | 为空则不鉴权（仅建议本机监听时使用）；可添加多条、一键生成 |

---

## 数据文件

| 平台 | 位置 |
|---|---|
| Windows | `%USERPROFILE%\.rapidproxy` |
| macOS / Linux | `~/.rapidproxy` |

```
~/.rapidproxy
├── config.json                 配置（含 API Key，注意保密）
├── accounts/<上游>/<账号>.json  登录凭据（权限 0600）
└── logs/proxy.log              运行日志（密钥已脱敏）
```

- 可用环境变量 `RAPIDPROXY_DATA_DIR` 把数据目录指到别处；
- `RAPIDPROXY_*` 环境变量可覆盖同名配置项。

---

## 工作原理

```
┌────────────┐   OpenAI 协议    ┌──────────────────────────────┐   私有协议    ┌───────────────────┐
│ 任意客户端  │ ───────────────▶ │ RapidProxy（本机 127.0.0.1） │ ───────────▶ │ WorkBuddy/CodeBuddy │
└────────────┘  /v1/chat/...    │  鉴权·路由·轮询·SSE 聚合     │  /v2/...      └───────────────────┘
                                └──────────────────────────────┘
```

- 客户端请求统一改写为 `stream: true` 转发上游（上游只支持流式）；客户端要非流式时，服务端把 SSE 事件聚合成单个 `chat.completion` 响应。
- 令牌过期前 5 分钟自动刷新；请求遇到 401/403 先刷新再重试一次。
- 多账号按轮询（cursor）分发；同一账号的请求串行化，避免并发刷新竞态。
- 上游对部分固定 system 模板句做了拦截，请求净化会在转发前做单词级最小改写。

---

## 常见问题

**第三方客户端报 HTTP 404「未知路径 /v1/models/chat/completions」？**
API 地址里**不要带 `/models`**——那是用来「获取模型列表」的完整端点，不是 base_url。
正确填法：`http://127.0.0.1:8787/v1`（或 `http://127.0.0.1:8787`，客户端会自己拼接路径）。
服务端对这类误配也做了容错：任意以 `/chat/completions`（POST）或 `/models`（GET）结尾的路径都会按对应端点处理。

**登录没反应 / 一直转圈？**
确认浏览器已完成登录；部分浏览器拦截跳转时，用「复制登录链接」到手机或其它浏览器完成登录，程序会自动轮询到结果。

**服务启动失败（端口占用）？**
换一个监听地址（如 `127.0.0.1:8788`），运行中的服务会自动以新地址重启。

**窗口超出屏幕 / 找不到窗口？**
程序启动时按屏幕尺寸与缩放比例自动收敛窗口并居中；仍异常时用顶栏「重置窗口」或托盘右键「重置窗口位置」。日志（概览 → 运行日志）里有 `屏幕 xxxx（物理 xxxx，缩放 xx%）` 一行，反馈问题时请附上。

**界面打不开 / 白屏？**
Windows 缺 WebView2 运行时时会退化到「仅托盘 + 后台服务」模式，日志见 `~/.rapidproxy/logs/proxy.log`；安装 WebView2 Runtime 后重启即可。

**想彻底退出？**
关闭按钮只是收进托盘；请用托盘菜单「退出」或侧边栏底部「退出程序」。

**升级后旧配置还在吗？**
数据目录与程序分离，升级覆盖可执行文件不影响 `~/.rapidproxy` 下的配置与凭据。

---

## 声明

- 本项目为个人学习与研究用途的第三方工具，与 WorkBuddy、CodeBuddy、腾讯官方无关。
- 请遵守相关服务条款使用；凭据仅保存在本机，请勿分享 `config.json` 与 `accounts/` 目录。
- 许可证：[MIT](LICENSE)
