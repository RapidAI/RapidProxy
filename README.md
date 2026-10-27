# RapidProxy

**中文** | [English](README_EN.md)

把 **WorkBuddy**（国际版）、**CodeBuddy**（国内版）、**LobsterAI**、**Trae**（国内/国际版）、**Qoder**（国内/国际版）、**OpenCode** 账号背后的模型，以及 **xAI / Claude / ChatGPT / 智谱编程** 等任意 OpenAI 或 Anthropic 兼容端点，转换成 **OpenAI 兼容 API** 的桌面代理程序。登录一次，任何支持「自定义 OpenAI 接口」的客户端（Cherry Studio、ChatBox、LobeChat、Open WebUI、Cursor、Cline、各种 SDK 与脚本……）都能直接调用。

- 语言/框架：Go + Wails v2（原生 WebView，安装包小、内存占用低）
- 平台：Windows / macOS / Linux
- 界面：简洁图形界面 + 系统托盘；关闭窗口只是最小化到托盘，从托盘菜单真正退出

> 协议实现参考自 [BevalZ/workbuddy-proxy](https://github.com/BevalZ/workbuddy-proxy)
> （CLIProxyAPI 插件版）的逆向结论，本项目是**独立进程**的重写：不依赖 CLIProxyAPI，
> 自带界面、托盘与 HTTP 服务。

---

## 支持的上游

每个可登录的上游就是一个**分组**：同组内多账号加权轮询、互为故障转移；同一服务商分国内/国际版时是两个独立分组（各自登录各自的账号）。组内任一账号出问题（限流、额度耗尽、上游不可用、模型未开通、认证失败等）都会在转发侧自动跳过并换组内下一个账号，错误不透传给下游——只有整组都不可用时，客户端才会看到错误。

| 分组（上游 ID） | 服务商 | 登录方式 | 说明 |
|---|---|---|---|
| `workbuddy` | WorkBuddy（国际版） | 浏览器授权 | 轮询登录，令牌自动刷新 |
| `codebuddy` | CodeBuddy（国内版） | 浏览器授权 | 与 WorkBuddy 同协议族 |
| `lobsterai` | LobsterAI（网易有道 龙虾） | 浏览器回调 | 本地回调端口换取令牌，仅流式 |
| `trae-cn` | Trae（国内版） | 浏览器授权回调 | SOLO 协议，双 token 轮换 |
| `trae-global` | Trae（国际版） | 浏览器授权回调 | 新加坡部署，与国内版互不通用 |
| `qoder-cn` | Qoder（国内版） | 设备码（PKCE） | COSY 签名对话，须在浏览器批准 |
| `qoder-global` | Qoder（国际版） | 设备码（PKCE） | 与国内版互不通用 |
| `opencode` | OpenCode Zen（opencode.ai） | API Key | 导入本机 OpenCode CLI 的密钥后等待确认（可粘贴新密钥覆盖），或直接粘贴密钥；建议配置代理 |
| `xai` | xAI（Grok） | API Key | OpenAI 线协议，`api.x.ai` |
| `claude` | Claude（Anthropic） | API Key | Anthropic 线协议，`api.anthropic.com` |
| `chatgpt` | ChatGPT（OpenAI） | API Key | OpenAI 线协议，`api.openai.com` |
| `zhipu` | 智谱编程（GLM） | API Key | OpenAI 线协议，编程套餐网关 `open.bigmodel.cn/api/coding/paas/v4`，客户端身份 `OpenCode` |
| `zcode` | 智谱编程（ZCode 登录） | 浏览器授权 | ZCode 在线 OAuth（国内站 `zcode.chatglm.site` / 国际站 `zcode.z.ai`），自动换出编程套餐 API Key，同样走 `/api/coding/paas/v4` + `OpenCode` 身份 |

> **智谱编程为什么必须用 `/api/coding/paas/v4` + `OpenCode` 身份？** 智谱的赠送额度绑定「编程套餐」通道：只有编程客户端身份（User-Agent `OpenCode`）访问编程专用网关时才会计入套餐。若误用普通开放平台网关（`/api/paas/v4`）或普通 Anthropic 网关（`/api/anthropic`），请求会走按量付费，在未充值的账号上返回 `[1113] 余额不足或无可用资源包`。`user_agent` 可在 `config.json` 中按需调整。

### 自定义模型（每个自成一组）

除上面的固定预设外，还可以通过后端方法 `AddCustomProvider({name, baseUrl, apiKey, protocol, model})` 添加**任意** OpenAI 或 Anthropic 兼容端点（DeepSeek、Moonshot、Qwen、火山方舟、自建网关……）。**每次调用都会创建一个独立分组**，即「每个自定义模型自成一组」，与其它服务商互不影响；同名重复添加会复用同一组（合并模型、保留已有账号）。`baseUrl` 必须是 `http(s)://` 开头的完整地址。

- `protocol` 取 `openai`（默认）或 `anthropic`；`baseURL` 可带或不带 `/v1`（自动补全）。
- 智谱的赠送额度按请求的 **User-Agent**（编程客户端身份）判定，故 `zhipu` / `zcode` 预设默认发送 `opencode`；若某些账号的赠送额度需要别的客户端身份（如 `claude code`、`codex`），在 `config.json` 的对应 profile 里改 `user_agent` 即可。
- Anthropic 线协议会由代理在边界完成 **OpenAI ⇄ Anthropic** 双向翻译（system、tools/tool_calls、thinking、SSE 流式增量），客户端始终只看到 OpenAI 兼容接口。
- 也可直接编辑 `config.json`，在 `profiles` 里加入 `{"id":"custom-xxx","kind":"custom","protocol":"anthropic","base_url":"...","extra_models":["..."]}`。

---

## 功能特性

- **图形化登录**：内置 WorkBuddy、CodeBuddy、LobsterAI、Trae（国内/国际）、Qoder（国内/国际）、OpenCode、xAI、Claude、ChatGPT、智谱编程（含 ZCode 登录）等上游预设，点「登录」打开浏览器完成授权或粘贴 API Key，令牌/密钥自动保存并自动刷新。
- **xAI / Claude / ChatGPT / 智谱编程**：既可用固定预设，也可通过 `AddCustomProvider` 添加任意 OpenAI 或 Anthropic 兼容端点；Anthropic 线协议由代理自动完成请求与 SSE 流双向翻译，客户端无感知。
- **ZCode 登录**：智谱编程支持 ZCode 在线 OAuth（国内站 / 国际站自动兜底），授权后自动换出 coding-plan API Key，并以编程客户端身份（User-Agent）调用，从而使用 ZCode / 编程套餐赠送额度。
- **OpenAI 兼容服务**：提供 `/v1/models`、`/v1/chat/completions`（及不带 `/v1` 的等价路径）与 `/healthz`，流式与非流式请求都支持（上游仅提供流式接口时，程序在服务端自动聚合）。
- **多账号负载均衡**：同一上游可登录多个账号，请求按权重轮询；单个账号令牌失效自动刷新，401/403 自动重试。
- **组内故障自动跳过**：账号出问题即换同组下一个，错误不透传给下游——限流 / 额度耗尽 / 上游不可用会冷却账号后换号；模型未开通、参数不当这类「该账号接不了这单」的错误同样换号（不冷却）；流式请求在上游以 200 + SSE 于流首报错时，也会在写出一个字节前换号重试；错误正文里带额度窗口重置时刻的窗口限流（如 code=6004 的 usage will reset at …），冷却会持续到该时刻，而不是几十秒后就回到轮询。只有整组都试过仍无人能接，才把最后的错误按原状态码带回（可退避的错误汇总为 429 + Retry-After）。
- **多上游路由**：模型名直接写（自动匹配）、或用 `上游ID:模型名` / `上游ID/模型名` 前缀显式指定（国际版可用 `intl:` 简写）；也可用请求头 `X-RapidProxy-Provider`（兼容 `intl`）或 `?provider=` 强制路由，`X-RapidProxy-Account` 指定账号。
- **多上游模型名消歧**：多个上游同时启用时，国际版（workbuddy）的模型统一以 `intl:模型名` 呈现；其它上游之间的同名模型，裸名归配置顺序里第一个暴露它的上游，其余以 `上游ID:模型名` 呈现；仅启用单个上游时全部保持原 ID。
- **虚拟模型组**：自定义一个模型名（如 `fast`），把不同服务商下的具体模型挂进去；客户端只请求这个名字，代理在成员之间按权重分摊，某个成员不可用时自动跳过。支持新增、改名、启停、删除，并在流量统计里有独立的组维度与成员分摊明细。
- **API Key 鉴权**：可配置多个 Key，支持一键生成；不配置则无需密钥（建议仅本机监听时使用）。
- **内置 + 自动同步模型清单**：内置白名单保证上游配置接口漏报时仍可调用；定期从上游拉取新模型，也支持手动「额外模型」与「禁用模型」。
- **深度思考与工具调用**：`hy3` 系列自动按最高思考档请求；`delta.reasoning_content` 原样透传；OpenAI 风格 `tool_calls` 增量按 `index` 正确合并。
- **请求净化**：可对上游黑名单的固定模板句做最小改写，降低触发内容拦截的概率；可全局强制开启最大思考档。
- **系统托盘**：显示主界面 / 复制接口地址 / 重置窗口位置 / 启停服务 / 退出；关闭按钮收进托盘（首次有提示）。
- **界面中英双语**：默认跟随系统语言，可在设置页手动切换（简体中文 / English），修改立即生效，托盘菜单同步切换；安装包安装向导同样支持中英文。
- **单实例运行**：重复启动时自动检测，直接把已有实例的界面弹到前台后退出，不会出现多份程序争抢端口和配置。
- **关于页**：侧边栏「关于」显示版本、平台、作者与版权信息，并提供在线更新入口。
- **开机自启动**：默认开启，登录系统后静默运行在系统托盘（不弹窗口）；可在设置页关闭。程序升级换路径后自动修正自启动指向。
- **CORS 可选**：默认关闭；开启时仅对本地页面（localhost/127.0.0.1）放行，避免任意网页盗用本机代理。
- **窗口自适应**：按屏幕尺寸与系统缩放比例自动收敛窗口大小并居中，小屏 / 高 DPI 不会超出屏幕。
- **在线更新**：自动检查 GitHub Releases 新版本，下载安装包后启动安装并退出（Windows 运行 NSIS 向导、macOS 打开 PKG 安装器、Linux 直接替换 AppImage 并重启）。
- **日志**：界面实时日志 + 文件日志（自动脱敏密钥），环形缓冲防膨胀。

---

## 界面说明

左侧边栏六个页签：

| 页签 | 内容 |
|---|---|
| **概览** | 接入信息卡（Base URL、API Key 显隐/重新生成、模型列表入口、curl 调用示例）、服务状态、上游列表、实时运行日志 |
| **账号** | 选择上游发起登录、复制登录链接、登录状态轮询、已登录账号管理与删除 |
| **模型** | 当前可用模型清单（名称、上下文长度、输出上限、来源上游），支持搜索与手动「同步模型」 |
| **虚拟模型** | 虚拟模型组管理：新建 / 编辑 / 改名 / 启停 / 删除，组内成员按权重配置，实时显示每个成员是否可用 |
| **流量统计** | 按组（上游或虚拟组）与模型查看 token 用量，支持日 / 周 / 月切换；选中虚拟组时额外显示组内成员分摊明细 |
| **设置** | 监听地址、模型自动同步间隔、请求净化/强制思考开关、CORS 开关、上游启停与代理地址、API Key 管理、数据文件位置 |

顶栏：服务状态胶囊、监听地址、「最小化到托盘」、「重置窗口」、「启动/停止服务」。侧边栏底部「退出程序」。

---

## 安装

### 方式一：下载预编译包（推荐）

到 [Releases](https://github.com/znsoftm/RapidProxy/releases) 下载对应平台的**安装包**（或便携压缩包）：

| 平台 | 安装包 | 便携包 | 备注 |
|---|---|---|---|
| Windows | `RapidProxy-windows-setup.exe`（NSIS 双架构安装包，自动检测 x86/x64） | `RapidProxy-windows-amd64.zip` / `RapidProxy-windows-386.zip` | 需要 WebView2 Runtime（Win10/11 一般自带）；安装完成页默认勾选「运行 RapidProxy」 |
| macOS (Intel + Apple Silicon) | `RapidProxy-darwin-universal.pkg`（安装向导，双击安装到「应用程序」） | `RapidProxy-darwin-universal.tar.gz` | 通用二进制；便携方式解压后把 `RapidProxy.app` 拖入「应用程序」 |
| Linux | `RapidProxy-linux-amd64.AppImage`（免安装，`chmod +x` 后直接运行） | `RapidProxy-linux-amd64.tar.gz` | 需要 WebKitGTK（`libwebkit2gtk-4.0`）；AppImage 未捆绑系统 WebView 运行库 |

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
2. 切到 **账号** 页签，选择「WorkBuddy（国际版）」或「CodeBuddy（国内版）」，点 **登录**：
   - 程序会自动在**系统默认浏览器**中打开官方授权页（Google / GitHub / 微信扫码），在浏览器里完成登录后凭据自动保存，程序会自动感知登录结果；
   - 若浏览器没有自动打开，可复制登录链接到任意浏览器（含手机）手动打开，登录成功后回到程序等待自动完成；
   - **概览**页的「上游」卡片会实时显示每个上游的登录状态（`已登录 · N 账号` / `未登录`）；
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
# 显式指定上游（国际版可用 intl 简写），两种写法等价
curl ... -d '{"model":"codebuddy:glm-5.3", ...}'
curl ... -d '{"model":"codebuddy/glm-5.3", ...}'
curl ... -d '{"model":"intl:claude-sonnet-4-5", ...}'

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

### 虚拟模型组

一个虚拟模型名聚合多个上游的模型，适合「把不同服务商的模型当成同一个可用池」：

- **建组**：在「虚拟模型」页新建，填一个名字（如 `fast`），再从「所有已登录账号」的上游里挑选模型加入组内，每个成员可单独设权重（默认 1，权重越大分到越多）。上游在设置里是否启用不影响能否加入虚拟组。
- **调用**：客户端把 `model` 填成组名即可，与真实模型名用法完全一致：

  ```bash
  curl http://127.0.0.1:8787/v1/chat/completions     -H "Authorization: Bearer sk-wbp-xxxx"     -d '{"model":"fast","messages":[{"role":"user","content":"你好"}]}'
  ```

- **分摊与跳过**：组内成员按权重平滑轮询；某个成员整体不可用（上游未启用 / 未登录 / 账号全暂停 / 全部冷却 / 上游不接受该模型）时自动换下一个，错误不透传给客户端。只有组内所有成员都试过仍无人能接，才按原有口径返回错误（客户端错误保留原状态码，可退避的错误汇总为 429 + Retry-After）。
- **命名规则**：不能含 `:`、`/` 或空格（这两个是强制路由分隔符），也不能与上游 ID 或 `intl` 重名；与真实模型 ID 撞名时虚拟组优先。
- **优先级**：显式强制路由（`上游ID:模型名`、`X-RapidProxy-Provider`、`?provider=`）优先于虚拟组，便于临时绕过。
- **与上游开关的关系**：虚拟组是「用哪个上游的哪个模型」的显式选择，优先级高于上游的全局启用开关——上游在设置里停用后，组内成员仍可正常转发。要掐断某个组的流量，停用这个组本身即可，这样不会连带影响该上游裸模型名的调用。
- **流量统计**：虚拟组有独立的组维度，同时每次请求仍会记到真实上游与真实模型上（用于核对上游额度）；选中虚拟组时统计页额外显示组内成员分摊明细。
- **改名与删除**：改名时历史流量自动迁移到新名字；删除时历史流量一并清除，避免重建同名组时继承旧数据。

---

## 设置说明

| 设置项 | 默认值 | 说明 |
|---|---|---|
| 语言 | 跟随系统 | 界面语言：跟随系统 / 简体中文 / English；修改立即生效（含托盘菜单），保存在配置文件 `language` 字段 |
| 监听地址 | `127.0.0.1:8787` | 改成 `0.0.0.0:8787` 可局域网访问（需配 API Key） |
| 模型同步间隔 | `6` 小时 | 填 `0` 关闭自动同步 |
| 请求净化 | 开 | 改写上游黑名单模板句，降低拦截概率 |
| 强制最大思考 | 关 | 所有请求 `reasoning_effort=high` |
| CORS | 关 | 开启后仅本地页面（localhost/127.0.0.1）可跨域调用 |
| 上游代理 | 空 | 可为某个上游单独填 `http://` / `socks5://` 代理 |
| API Key | 空 | 为空则不鉴权（仅建议本机监听时使用）；可添加多条、一键生成 |
| 开机自动启动 | 开 | 登录系统后静默运行在托盘（`--hidden`）；关闭后自动清理系统自启动条目 |

### 在线更新

程序启动后会静默检查一次 [GitHub Releases](https://github.com/znsoftm/RapidProxy/releases)；也可以到 **设置 → 软件更新** 手动「检查更新」。发现新版本后点 **下载并安装**：

- **Windows**：下载 `*-setup.exe`，自动运行 NSIS 安装向导，随后退出当前程序（安装完成页默认勾选「安装完成后运行」，点完成即可启动新版）；
- **macOS**：下载 `*-universal.pkg`，自动打开系统安装器，随后退出当前程序（安装位置 `/Applications`）；
- **Linux**：以 AppImage 方式运行时自动替换当前文件并重启；其它运行方式会把 `*.AppImage` 下载到数据目录的 `update/` 下并提示手动运行。

安装包下载到 `<数据目录>/update/`，采用 `.part` 临时文件 + 原子改名，中断不会留下半成品。更新检查失败（如无法访问 GitHub）不影响其它功能。

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
- 多账号按权重（平滑加权轮询）分发；同一账号的请求串行化，避免并发刷新竞态。
- 组内故障自动跳过：任一账号出错即换同组下一个，错误不透传给下游；只有整组都不可用时才把最后的错误按原状态码带回（可退避的汇总为 429 + Retry-After）。
- 上游对部分固定 system 模板句做了拦截，请求净化会在转发前做单词级最小改写。

---

## 常见问题

**第三方客户端报 HTTP 404「未知路径 /v1/models/chat/completions」？**
API 地址里**不要带 `/models`**——那是用来「获取模型列表」的完整端点，不是 base_url。
正确填法：`http://127.0.0.1:8787/v1`（或 `http://127.0.0.1:8787`，客户端会自己拼接路径）。
服务端对这类误配也做了容错：任意以 `/chat/completions`（POST）或 `/models`（GET）结尾的路径都会按对应端点处理。

**登录没反应 / 一直转圈？**
确认浏览器已完成登录；部分浏览器拦截跳转时，用「复制登录链接」到手机或其它浏览器完成登录，程序会自动轮询到结果。

**OpenCode 点了登录却没有直接登录？**
检测到本机 OpenCode CLI 保存的密钥时不会自动落库：留空提交「登录」即使用该密钥，也可以直接粘贴新密钥换号登录——想换钥匙不必先删本机的 `auth.json`。提交前会联机校验密钥，错键会立即提示。

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
