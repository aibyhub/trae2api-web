<div align="center">

<img src="./docs/logo.svg" alt="trae2api-web logo" width="110" height="110" />


# trae2api-web

**TRAE SOLO 逆向工程服务 · OpenAI 兼容 API · 可视化账号管理面板**

[![Go Version](https://img.shields.io/badge/Go-1.22+-00ADD8?style=flat-square&logo=go&logoColor=white)](https://golang.org/)
[![Docker Ready](https://img.shields.io/badge/Docker-Ready-2496ED?style=flat-square&logo=docker&logoColor=white)](https://www.docker.com/)
[![OpenAI Compatible](https://img.shields.io/badge/API-OpenAI%20Compatible-412991?style=flat-square&logo=openai&logoColor=white)](https://platform.openai.com/)
[![Web Admin](https://img.shields.io/badge/Web%20Admin-Built--in-10b981?style=flat-square)](http://localhost:7864/admin)
[![License](https://img.shields.io/badge/License-MIT-blue?style=flat-square)](LICENSE)

</div>

---

> 基于上游 [https://github.com/Sliverkiss/traework2api](https://github.com/Sliverkiss/traework2api) 改进。
> 
## 概述

`trae2api-web` 是一个将 TRAE SOLO 对话通道包装为标准 OpenAI 协议（`/v1/chat/completions` 与 `/v1/models`）的高性能反向代理服务。基于纯 Go 标准库构建，具备极低资源消耗与高并发处理能力。

本项目内置了轻量级 Web 控制台，支持多账号凭证管理、额度监控、自动化轮换保活以及开箱即用的一键网页登录闭环。

## 核心特性

- **OpenAI 协议兼容**：提供标准 `/v1/chat/completions`（支持流式 Streaming 与非流式）与 `/v1/models` 端点，无缝接入 NextChat、Chatbox、Claude Code、Cline 等客户端。
- **可视化 Web 管理控制台**：内置轻量 Web 界面（`GET /admin`），实时展示账号配额（剩余/已用/总量）、签到状态与健康度，支持多账号并发查询与自动刷新。
- **凭证全生命周期管理**：提供 Web 凭证导入、软启停开关、昵称修改、删除及一键 Web 登录闭环（无需手动抓包或提取 Token）。
- **多账号智能调度池**：基于账号可用积分降序挑选，自动处理 1005、429、401、5xx 等异常状态，支持动态冷却与故障自动轮转。
- **自动化运维与保活**：每日定时自动签到，并在 Token 过期前 24 小时自动预刷新与原子落盘，保障长周期稳定可用。
- **新模型支持**：同步支持 glm-5.3、glm-5.2 等新版模型调度，适配最新协议版本。
- **内置登录页（可选）**：设 `TW2A_ADMIN_PASSWORD` 后，控制台先出登录页（会话 Cookie 7 天），`/admin/api/*` 读写全部要登录；Bearer API Key 通道保留给脚本。不设则保持原有行为。
- **自动签到看板**：面板首页直接显示今日进度（已签/未签/失败 + 具体是哪些账号）、自动成功率、下次签到窗口、近 7 天日报与逐条明细（区分自动/手动）；可配 webhook 在失败时推送（企业微信/钉钉/飞书）。
- **日志保留与清理**：`usage.jsonl` / `checkin.jsonl` 默认只保留 90 天（可配），启动与每天自动裁剪；面板「使用记录 → 数据维护」可查看大小/条数/时间范围并手动清理（30 天 / 90 天 / 180 天 / 清空）。
- **每账号独立出口代理池**：集中管理若干代理（`http/https/socks5/socks5h`），账号在面板从池中选择出口；新账号自动均衡分配，重新导入/重登保留原有出口。内置「检测出口 IP / 延迟」与启动自检，出口故障只临时隔离、不惩罚账号。
- **纯净轻量**：纯 Go 标准库开发，零第三方运行时依赖，静态编译产物小巧，内存占用极低。

## 快速开始

### Docker Compose 部署（推荐）

1. **准备目录与配置文件**

```bash
mkdir -p auths data
cp .env.example .env
```

编辑 `.env` 文件，配置自定义的管理鉴权密钥：

```env
TW2A_API_KEY=your_secure_api_key
```

2. **构建并启动服务**

```bash
docker compose up -d --build
```

3. **接口健康检查与模型验证**

```bash
# 健康检查
curl http://127.0.0.1:7864/healthz

# 查看可用模型列表
curl http://127.0.0.1:7864/v1/models

# 查看账号池状态
curl http://127.0.0.1:7864/status
```

### 本地直接运行

环境要求：Go 1.22+

```bash
# 设置访问密钥
export TW2A_API_KEY="your_secure_api_key"

# 编译并启动服务
go build -o trae2api-web ./cmd/server
./trae2api-web
```

## Web 管理面板

服务启动后，访问 `http://127.0.0.1:7864/admin` 即可进入可视化管理后台：

- **账号总览**：实时查看各账号的剩余积分、配额总量、已用积分、权益包数及签到状态。
- **Web 登录闭环**：在面板点击发起登录，浏览器完成验证后回调自动回传至服务并完成 Token 换取与热加载，无需手动复制凭证。
- **凭证管理**：支持粘贴 JSON 凭证或回调链接直接导入账号，支持随时启停软开关、修改备注昵称或删除失效账号。
- **安全脱敏**：前端展示严格脱敏（仅显示前缀与长度），写操作（导入、删除、修改）均受 `TW2A_API_KEY` 保护。

## 代理池（每账号独立出口）

面板「代理池」页集中管理出口代理，账号在「代理」弹窗里按名称选用（不同账号用不同出口 IP，降低风控关联）。

| 操作 | 位置 | 说明 |
|---|---|---|
| 添加 | 代理池页 | 支持 `http(s)://`、`socks5://`、`socks5h://`；添加后立即探测一次，表格马上有出口 IP |
| 批量添加 | 代理池页 | 每行一条：`名称 地址` / `名称,地址` / `名称=地址`，也可只写地址（自动命名 `代理N`）；`#` 开头为注释，失败的行走行单独报错 |
| 编辑 | 每行「编辑」 | 改**名称 / 地址 / 启用状态**；勾选「同步更新引用此代理的账号」会一并改写账号文件（不勾则那些账号继续用旧地址）。编辑时会**按需回显当前完整地址**（含凭据，仅带 API Key 可用），不用重新手打 |
| 停用 / 启用 | 每行「停用」 | 停用后**不再参与新账号自动分配**，但已在用的账号不受影响（比直接删除安全） |
| 检测 / 全部检测 | 代理池页 | 走**与真实请求完全相同的客户端构造**（`upstream.ProbeProxy`），显示真实出口 IP、延迟、健康；启动时自动自检一次，结果落盘 `proxies.json`，重启后仍可见 |
| 删除 | 每行「删除」 | 确认框会提示有多少账号在引用；已引用账号仍继续用原地址 |
| 指定账号出口 | 账号行「代理」 | 选池内名称（含出口 IP、已停用标记），或填自定义地址；留空 = 直连 |
| 自动分配 | 导入 / 网页登录时 | 未指定出口时从池里挑**被引用最少的已启用**代理；同一账号重新导入/重登会**保留**原出口 |

面板「账号」页还会直接显示每个账号的**出口代理名**与**最近签到结果**（哪个号今天没签一目了然）。

行为细节：

- 设备注册（`device_register`）与后续请求走**同一个出口 IP**，避免「注册 IP ≠ 使用 IP」被风控。
- 出口/网络故障（代理不可达、连接被重置、畸形响应等）**不会**累计账号错误次数，只把该出口临时隔离 5 分钟并从选号中摘掉；任一成功请求立即恢复。
- 代理地址 scheme 的校验规则与传输层完全一致（`upstream.ValidProxyScheme`），不会出现「保存成功但实际静默直连」。
- 全局兜底：环境变量 `TW2A_PROXY_URL`（账号未配 `proxyUrl` 时生效，优先级低于账号自身配置）。

### TLS 指纹复刻（v1.3.8 起，默认开启）

出站 ClientHello 复刻真实 Trae 客户端（ahaNet 栈）形态：TLS 1.2 封顶、18 cipher、7 扩展、无 ALPN，
消除 Go 默认栈在 JA3/JA4 层的可识别性（参数为 2026-10-02 真机透明中继字节级抓包，见
`internal/tlsfp`）。直连与 SOCKS5/HTTP 代理出口均生效（代理隧道内做指纹握手，出口 IP 与指纹一致呈现）。

- 回退开关：环境变量 `TW2A_TLS_FINGERPRINT=off` + 重启，即恢复历史传输行为（直连自动 h2 / 代理强制 h1.1）。
- 也可以回退镜像版本（上一 tag 的镜像保留在 GHCR）。
- https 代理出口不做指纹（TLS-to-proxy 双层握手），自动回退标准传输并打日志。

## 面板登录（可选，替代 Nginx Basic Auth）

在 1Panel 的环境变量里加一条即可（只在 env 读，不落盘、不进日志）：

```env
TW2A_ADMIN_PASSWORD=你的面板密码
```

- `/admin` 打开先是登录页；登录后 7 天免登录（HMAC 签名的 HttpOnly Cookie，重启后仍有效）。
- `/admin/api/*` 的**读和写**都要登录；`Authorization: Bearer <TW2A_API_KEY>` 依然可用（curl/脚本不受影响）。
- 顶栏会显示「已登录（会话）」和「退出登录」按钮；密码错误会延迟 400ms，同一来源 10 分钟内失败 10 次后返回 429。
- **不设这个变量 = 与旧版完全一致**（只读开放、写操作要 API Key），可随时回退。
- 建议仍放在 HTTPS/反代之后；登录页只做访问门槛，不替代传输加密。

## 自动签到看板

面板「账号管理」页顶部常驻一张卡片，用来回答「每天的自动签到到底正常吗」：

| 展示项 | 含义 |
|---|---|
| 今日已签到 x / y | 按账号统计今日最终结果（领取 / 今日已签 / 失败 / 未执行） |
| 需要关注 | 失败 + 今日无任何记录的账号，直接在下面列出账号名与原因 |
| 自动成功率 | 今日**定时任务**的成功率（手动签到单独统计，避免掩盖定时失败） |
| 下次自动签到 | 下一个签到时点 + 抖动窗口（如 21:00，每号随机 0-60 分钟） |
| 近 7 天 | 每天的 成功/总数，含上游未开放等状态 |
| 明细 | 最近 40 条：时间 / 账号 / 结果 / 触发（自动 or 手动）/ 原因 |

与「额度监控」页的分工：额度卡片的「**上游：已签到 / 未签到**」读的是 TRAE 上游真实状态（**结果**）；本看板记录**我们做过什么**（自动/手动/跳过/失败原因）。两边对不上时以**上游状态**为准。看板里「未执行 / 失败」的账号可直接点「**补签**」；额度卡片右上也有「签到」按钮。

> `runCheckinNow` / 定时窗口里因「没有 refreshToken」等原因被跳过的账号，现在也会写进签到日志（状态 `skipped` + 原因），不会再出现「等了一天却毫无记录」的情况。

数据落盘 `data/checkin.jsonl`，重启不丢；可用 webhook 在**有失败或未执行**时推送：

```env
TW2A_CHECKIN_WEBHOOK=https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=xxx   # 或钉钉/飞书机器人
TW2A_CHECKIN_NOTIFY=fail    # fail（默认，仅在有问题时推）/ always（每个窗口都推）/ never
```

## 数据维护（日志保留与清理）

面板「使用记录」页底部「数据维护」：显示两个日志文件的大小 / 条数 / 最早最新时间，并可一键清理。

```env
TW2A_LOG_RETENTION_DAYS=90   # 默认 90 天；启动与每 24h 自动裁剪；0 = 关闭自动清理
```

- 自动裁剪：启动时一次 + 每天一次，`usage.jsonl`、`checkin.jsonl` 及其轮转文件 `.1` 一起处理。
- 手动清理：「保留 30 天 / 90 天 / 180 天 / 清空全部」；清理不可恢复，请按需操作。

## API 调用示例

### 对话补全 (Chat Completions)

```bash
curl -X POST http://127.0.0.1:7864/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer ${TW2A_API_KEY}" \
  -d '{
    "model": "glm-5.2",
    "messages": [
      {
        "role": "user",
        "content": "请用简短的一句话介绍你自己。"
      }
    ],
    "stream": false
  }'
```

## 运维脚本

项目根目录下提供了便捷的 CLI 运维工具：

```bash
# 全账号批量签到与 Token 保活
./signin.sh

# 账号积分与使用情况报表
./credit.sh

# 输出 JSON 格式报表
./credit.sh -json

# 查看指定 UID 账号
./credit.sh <UID>
```

## 配置项参考

所有配置项均可通过环境变量或 `config.json` 进行调整：

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `TW2A_API_KEY` | (必填) | 服务鉴权密钥，用于 API 调用与控制台写操作 |
| `TW2A_LISTEN` | `:7864` | 主服务监听地址及端口 |
| `TW2A_AUTH_DIR` | `./auths` | 账号凭证存储目录 |
| `TW2A_STATE_FILE` | `./data/state.json` | 账号池状态持久化文件 |
| `TW2A_DEFAULT_MODEL` | `glm-5.2` | 默认回退请求模型 |
| `TW2A_CALLBACK_PORT` | `18080` | 本地 OAuth 回调端口（设为 0 可关闭） |
| `TW2A_CALLBACK_BASE` | (空) | 登录回调 base URL（远程部署用，如 `http://1.2.3.4:18080`）；空 = `http://127.0.0.1:<port>/authorize` |
| `TW2A_ADMIN_PASSWORD` | (空) | 面板登录密码；设置后 `/admin/api/*` 需登录（或 Bearer Key），空 = 不启用登录页 |
| `TW2A_LOG_RETENTION_DAYS` | `90` | 日志保留天数（usage.jsonl / checkin.jsonl）；`0` = 关闭自动清理 |
| `TW2A_CHECKIN_WEBHOOK` | (空) | 签到结果推送地址（企业微信/钉钉/飞书机器人）；空 = 不推送 |
| `TW2A_CHECKIN_NOTIFY` | `fail` | 推送策略：`fail` / `always` / `never` |
| `TW2A_TIMEOUT_SECONDS` | `120` | 上游请求超时时长（秒） |
| `TW2A_ERR_THRESHOLD` | `3` | 触发冷却前的连续错误次数 |
| `TW2A_ERR_COOLDOWN` | `300` | 错误冷却时长（秒） |

## 常见问题排查

**① 走代理的账号签到/额度查询失败：`malformed HTTP response "\x00\x00\x12\x04..."`**

v1.2.8 起已修复：Go 的 `Transport.Clone()` 会把带 `h2` 的 ALPN 配置复制给代理客户端，却不复制 `TLSNextProto`，导致该客户端「宣告 HTTP/2 却没有 HTTP/2 实现」，把上游的 HTTP/2 SETTINGS 帧按 HTTP/1.1 解析。现在**走代理的出口一律只用 HTTP/1.1**（直连仍用 HTTP/2）。若在老版本遇到：先升级镜像，或在容器加 `GODEBUG=http2client=0` 应急。

**② 面板「测试」通过，但账号请求仍失败**

v1.2.9 起代理池「检测」直接复用真实请求的客户端构造（`upstream.ProbeProxy`），并给出**真实出口 IP**，「测试通过 ≠ 真实可用」的情况已被消除。请以账号行「代理 → 用该账号测试连通」与「全部检测」的结果为准。

**③ 面板账号列表的「出口代理」显示为「直连」**

说明该账号没有配 `proxyUrl`（例如旧版本通过「粘贴回调链接导入」或网页登录添加的账号）。在新版重新导入该账号即可自动分配；也可以直接在「代理」弹窗里选择池内代理。

**④ 网页登录（一键登录）在远程部署下收不到回调**

回调地址默认是 `http://127.0.0.1:18080/authorize`，只在浏览器与服务器同机时可用。远程部署请：发布回调端口（`docker-compose.yml` 增加 `- "18081:18080"`）+ 设置 `TW2A_CALLBACK_BASE`；或直接把回调链接粘贴到面板的导入框（服务端解析，同样能落盘）。

**⑤ 某个出口挂了，会不会把好账号禁用？**

不会。出口/网络类错误（`upstream.IsTransportError`）不累计 `err_count`、不触发冷却/禁用，只做 5 分钟出口隔离；只有上游真正返回业务错误（401/1005/429 等）才影响账号状态。

## 安全声明

- **本地存储与脱敏**：所有账号凭证仅保存于本地 `auths/` 目录，控制台及日志中所有 Token 均严格脱敏输出。
- **环境隔离**：凭证文件、状态数据及环境配置文件默认加入 `.gitignore`，防止误提交泄漏。

## 开源协议

本项目基于 [MIT License](LICENSE) 许可发布。
