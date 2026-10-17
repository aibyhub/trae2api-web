# TRAE 反代研究笔记

> 本仓库的记录：TRAE（字节跳动 AI IDE）各通道的反代可行性、积分体系、限流机制与实测发现。
> 记录时间：2026-08。部分结论来自公开逆向调查（文末标注来源），部分为本地实测。

---

## 1. 上游反代项目全景

| 项目 | 通道 | 说明 |
|---|---|---|
| [Sliverkiss/traework2api](https://github.com/Sliverkiss/traework2api) | `solo_work_lite` | Go，本仓库上游。SOLO 免费对话 → OpenAI 兼容 |
| [Ttungx/trae-solo-local-api](https://github.com/Ttungx/trae-solo-local-api) | `solo_work_lite` | JS，同通道 |
| [laojichao/trae-local-api](https://github.com/laojichao/trae-local-api) | Trae IDE (CN/SOLO/SG) | Node，四版本，tc 加密 auth 自动解密 |
| [muskke/trae-api-proxy](https://github.com/muskke/trae-api-proxy) | Trae API | Go，Header 签名 + payload 转换 |
| [Sliverkiss/workbuddy2api](https://github.com/Sliverkiss/workbuddy2api) | 腾讯 WorkBuddy | **注意：WorkBuddy ≠ TRAE Work**，不同产品 |

所有可用的反代都走 **SOLO/IDE 免费通道（消耗 ide_credits）**，没有任何项目反代 work 通道（work_credits）。

## 2. SOLO 通道机制

- 对话端点：`POST https://trae-api-cn.mchost.guru/api/agent/v3/llm_utils_chat`
- 关键参数：`function: "solo_work_lite"`（实测：其他 function 值如 `work`/`solo`/`work_lite` 均无效）
- 模型表：`POST /api/ide/v1/get_detail_param`（`config_name` 列表，动态下发）
- 鉴权：`Cloud-IDE-JWT <accessToken>` + `X-Cloudide-Token` + `X-Uid` + `X-Machine-Id` / `X-Device-Id`
- token 获取：`refreshToken` → `ExchangeToken`（轮换 refresh）→ `GetUserInfo`（uid）
- 登录：`https://www.trae.cn/authorization` 强制回调 `127.0.0.1`（浏览器与服务器无需同机，回调链接粘贴即可）
- SSE 格式：`event:notify_usage` / `event:metadata` / `event:error` / `event:done`

### 版本号与模型解锁（实测）

- 上游按 `X-Ide-Version` / `X-App-Version-Code` 版本控制模型可用性
- 实测：`0.1.43` 请求 `glm-5.3` 报 `4001 param is invalid`；**`0.1.52`（20260811）正常对话**
- 模型列表随版本动态更新（`get_detail_param` 0.1.52 返回 35 个 config，含 glm-5.3）

## 3. 积分体系（实测）

对话流中 `event:notify_usage` 返回计费结构：

```json
"billing_mode": "credits",
"cn_credits_remain_info": {"ide_credits": 0, "work_credits": 2000}
```

| 类型 | 用途 | 反代可用 |
|---|---|---|
| `ide_credits` | SOLO 对话（`solo_work_lite`） | 可用（本项目采用） |
| `work_credits` | TRAE Work 编程 Agent | 不可用（见下文说明） |

- 额度查询：`POST /trae/api/v2/pay/ide_user_ent_usage`（聚合 `user_entitlement_pack_list[].entitlement_base_info.quota.credits_limit`，`usage.credits_amount` 为已用）
- **注意**：`ide_user_ent_usage` 聚合的是 entitlement 包（含 work 包），显示 `remain=2000` 实为 work_credits，**不代表 SOLO 通道可用额度**。SOLO 真正看 `notify_usage.cn_credits_remain_info.ide_credits`
- 签到：`POST /trae/api/v2/ug/checkin_credits/status` + `/claim`（每日 +200，高峰常返回 `9074 当前使用人数太多`）
- ide_credits 耗尽：对话报 `4008 Your requests have exceeded the quota`（视为配额限制，短时重试无效，等每日重置或签到）

## 4. work 通道为何不可反代

引用公开逆向调查（[rosemarycox5334-debug/PA_Agent → TRADE_WORK_CN_INVESTIGATION.md](https://github.com/rosemarycox5334-debug/PA_Agent/blob/main/TRADE_WORK_CN_INVESTIGATION.md)，2026-08，**未在本仓库复现，引用结论**）：

| 路径 | 结果 |
|---|---|
| `llm_raw_chat` v1/v2 | 外部调用严格限流（4011）；v2 仅服务器内部从 `create_agent_task` 调用不限流 |
| `create_agent_task`（work agent 编排） | 需要 `encrypted_prompt_set` 加密字段——只存在于本地 ai-agent 进程加密 DB，外部无法构造 |
| IPC（`lite/send_message`） | 需要完整客户端注册流程 + 绑定 session_id |

**结论：work 通道钥匙（encrypted_prompt_set）烧在本地 ai-agent 进程内，反代不可行**。TRAE Work 客户端本身是"本地 agent 构造加密任务 → 云端执行"架构。

## 5. 限流 / 错误码速查（实测 + 上游代码）

| code | 含义 | 处理 |
|---|---|---|
| 1001 | 认证失败（token 失效） | 重新登录（换 refreshToken） |
| 1005 | plan 权益不足 | 长冷却（12h） |
| 4001 | 参数无效（模型不存在/版本不匹配） | 升级 `IdeVersion` 或换模型 |
| 4008 | 配额超限（ide_credits 耗尽） | 等每日重置 / 签到 |
| 4011 | 请求频率超限 | 等限流窗口 |
| 9004 | 签到 claim 缺 `X-Device-Id` 头（"order parameters incorrect"） | 凭证必须有 device_id（登录流程生成并随 OAuth 注册）；导入账号用面板重登 |
| 9074 | 文案"人数太多"多为**设备号无效**（未随账号注册的假设备号实测恒 9074，见 [公开复盘](https://juejin.cn/post/7688669403517960202)）；高峰限流也复用此码 | 换有效设备对（重登）；确为高峰则稍后重试 |
| 429 | 软限流 | 短冷却（60s） |

> 签到 status/claim 的请求体实测为 `{"req_source":1}`（与官方客户端一致）；`X-Device-Id` 仅 claim 必须，status 可不带。业务码随 HTTP 200 返回，判成败必须解析 body。

## 6. Windows 环境坑（本仓库实测修复）

- **python3 商店占位别名**：`C:\Users\...\WindowsApps\python3` 指向 `AppInstallerPythonRedirector.exe`，非交互下恒 exit 49（连 `print('hi')` 都失败）。修复：检测后回退 `python`
- **GBK 写文件炸**：`open("w")` 默认 GBK，遇非 GBK 字符抛 `UnicodeEncodeError` 且**先清空已有文件**（实测损坏凭证）。修复：UTF-8 + tmp/rename 原子写
- **回调昵称双重编码**：TRAE 回调 `userInfo` 中文双重 URL 编码，`parse_qs` 一层解不干净 → 昵称乱码（实测 `Óû§8847309959`）。修复：`fix_mojibake` 回转失败则回退 `用户+uid末4位`

## 7. 结论

1. **SOLO 免费通道（ide_credits）是唯一可反代的 TRAE 通道**，上游刻意放行 `solo_work_lite`（免费引流，额度天花板 + 限流兜底）
2. **work_credits（TRAE Work）无法通过 API 反代**——加密配置锁在本地进程，商业上也不可能开放
3. 反代的价值场景：作为 **Claude Code / Cline / Codex 等自带 agent 编排客户端的模型后端**（OpenAI 兼容 + function calling），而非替代 work 的云端 agent

---

## 8. 2026-10 生产面补遗：客户端对接差距清单（对照真机行为，v1.4.0 复盘）

背景：生产 6 账号（4 出口：2×do / 2×cf-vps socks / 2 直连）7 天 476 行日志普查：
3859 次成功调用（p50=16s / p95=103s / p99=218s / max=924s）、152 次 `timeout awaiting response headers`
（120s 头等待悬挂，**直连出口同样中招 → 上游慢首字节，与代理无关**）、0.34% 业务错误率。
签到瞬时失败全部是 `tlsfp: handshake: context deadline exceeded` 形态（次日窗口自愈）。

### 8.1 v1.4.0 已落地的对接优化

| 差距点 | 真机行为 vs 旧版 | v1.4.0 处理 |
|---|---|---|
| 请求节奏 | 真机单会话顺序请求；反代多账号秒级精确选最老过期者 → 单号 44% 流量 | 过期按 UTC 日分桶 + 近 1h 成功笔数降权（cap 16, decay 0.25），流量摊开 |
| 慢首字节 | 真机 UI 有"思考中"过渡；反代在 SSE 头前无任何字节，客户端先超时 | stream 即落 200+SSE 头 + 5s 注释心跳，5m 轮换预算，终态流内 error 事件 |
| 签到瞬时抖动 | 真机偶发失败会手动重开重试 | status/claim 传输层瞬时错 3s→6s×2 重试（claim 幂等；9074 等业务码不重试） |
| 可观测性 | 真机天然呈现余额/到期；反代只有零散 jsonl | credits.jsonl 余额轨迹 + 面板「使用图表」（14 天调用量/余额曲线/到期倒计时） |

### 8.2 尚存差距（按性价比排序，未实施）

1. **IdeVersion 版本锁是硬编码**（`upstream.IdeVersion = 0.1.64`，2026-08-11 manifest 对齐真机）。
   上游用版本号控制模型解锁（§2），下次上游抬最低版本时需重编译。建议：`TW2A_IDE_VERSION`
   env 覆盖 + 面板显示当前值，把它变成运维旋钮而非代码常量。
2. **无每账号并发上限**：真机是单 agent 会话串行；反代客户端（Cline/Codex 并行 subagent）可对
   单账号并发十几路，是 4011/429 的直接诱因。建议每 uid 信号量 1-2（可配），超并发直接排队而非轮换。
3. **请求体长度不做前置校验**：客户端可发 >maxInputTokens 的上下文，白白烧上游一次 4001。
   FetchModels 已带 maxInputTokens，可在入口对 per-conversation 超限提前 400。
4. **9074 剩余语义**：v1.4.0 重试后剩余签到失败应集中在「设备号未注册/假设备」（§5），
   面板已引导按账号「设备」重注册；多账号**共享同一注册对**时名额互斥（9095），建议每号独立注册设备。
5. **指纹覆盖仍到 TLS 层为止**：utls 复刻 ClientHello（v1.3.8），HTTP/2 SETTINGS 帧序、ws 差异
   未复刻；真机 Electron/ahaNet 侧若有更高级指纹（h2 帧序列），属可探测差异面，暂无实测证据，记为已知边界。

### 8.3 数据留存口径备忘

- `usage.jsonl` 环形 5000 条（v1.4.0 前 2000）≈ 9 天调用量；跨月报表用 `data/call-details/YYYY-MM-DD.jsonl`（30 天）。
- `credits.jsonl`（v1.4.0 起）：余额变更才记行，签到/刷新都进 `source` 字段；与 usage/checkin 共用保留期。
- `expire_at` 为秒级 unix（2398/7272 相差 153s 的实测案例出现在 Oct 25 过期包，旧逻辑因此粘滞数周）。
