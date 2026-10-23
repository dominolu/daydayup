# 上游流式响应中断分析（tokenplan / deepseek-v4.1-flash-1017，2026-10-22）

本文档整理 2026-10-21 23:50 至 2026-10-22 16:24 期间，DayDayUp 网关会话记录里出现的
`response_incomplete · response ended before completion` 与 `client_cancelled · client cancelled the request`
两类记录的证据、归因和待办。结论基于网关归档（`data/gateway.db`）、网关运行日志
（`logs/gateway.log`）与 DSH 客户端会话记录三方对齐。

## 1. 结论

| 记录 | 数量 | 归因 |
|---|---|---|
| `stream` / `response_incomplete` | 7 | 其中 **5 条为上游在生成中途关闭 SSE**（无 `finish_reason`、无 `[DONE]`、无 usage）；**2 条为网关自身 300s 请求超时被误标**（本次已修） |
| `client` / `client_cancelled` | 7 | 4 条用户手动停止；3 条为限流排队约 300s 触发客户端 300s 空闲超时（本次已改为提前 `503` 快速失败） |
| `upstream_unreachable` | 1 | 上游连接失败（90s 后），与同一时段的流中断同源 |

`deepseek-v4.1-flash-1017` 当时**只有 `sense-baa1ebe8`（tokenplan）一个账号在服务**，
其余 5 个账号（4 个 sensenova、1 个 workbuddy）不声明该 id，因此这条链路是单点：
上游一旦 429 或断流，没有第二个账号可以承接。

## 2. 证据：上游中断的 5 条（+1 条连接失败）

网关记录均为 `HTTP 200` + `outcome=error`（响应头已发出，无法再改状态码），
响应正文停在**完整 SSE 事件边界**上，最后一个事件仍是 `finish_reason: null`，
没有 `[DONE]`，也没有 usage 块——说明是上游对端关闭了连接，而不是网关截断或本地缓冲。

| 请求开始 | 网关 request_id | 上游 request_id | 时长 | 响应字节 | 客户端同刻事件 |
|---|---|---|---|---|---|
| 10-21 23:50:45 | `8fb0a72d87a2c3745a43322f` | `req_a31eb575-9c99-46a9-a743-5dd44f09e913` | 87.6s | 156,799 | 23:52:13 `TRANSPORT terminated` |
| 10-22 15:38:23 | `1721e67a2107055e6c5ceb74` | `req_2e03dcf5-7617-4418-8ac1-763bdf28f6cb` | 184.6s | 104,558 | 15:41:27 `TRANSPORT terminated` |
| 10-22 16:03:35 | `d96783ba646d9ae501ce6dce` | `req_30a82bb9-a757-4547-8388-d047d90c6742` | 118.0s | 109,836 | 16:05:33 `TRANSPORT terminated` |
| 10-22 16:16:22 | `e5c0b48b69a55a2c3b337712` | `req_c289b25f-1051-43fd-b385-b0061c1ba520` | 167.6s | 68,312 | 16:19:10 `TRANSPORT terminated` |
| 10-22 16:19:49 | `ebbd8c1059a73fd0560a0e56` | `req_5bda7729-dbfe-4b04-bc89-2d7b1ee1785e` | 252.0s | 157,579 | 16:24:01 `TRANSPORT terminated` |
| 10-22 16:05:34 | `39c478b8ccfbaeffc7d1df56` | —（未取得响应） | 95.5s | 101（网关错误体） | 16:07:09 `SERVER 502 upstream_unreachable` |

同日另有 2 条 `response_incomplete` 属于网关自己到期，不属于上游问题：

| 请求开始 | 网关 request_id | attempt 时长 | 说明 |
|---|---|---|---|
| 10-22 15:47:04 | `ee28750878a05e794d59f636` | 299.996s | 网关 `request_timeout_seconds=300` 到期 |
| 10-22 15:54:30 | `da496260e01ba3d3ce15e147` | 300.033s | 同上 |

**失败率**：10-22 当天真正到达上游的请求 33 个，26 个成功；上游中断 5 个、连接失败 1 个
（约 18%），另 2 个被网关 300s 预算截断。中断发生在 87s–252s 的任意位置，与请求体大小
（480–570KB）、上下文长度（12–15 万 token）无单调关系，前后相邻请求成功率正常。

## 3. 已排除的本地因素

- **并发压力**：16:03–16:27 的日志显示同一时刻只有一个在途请求，无并发抢占。
- **客户端取消**：中断记录的 `error_layer` 为 `stream` 而非 `client`；客户端在网关 abort
  之后才记录 `terminated`，方向是上游 → 网关 → 客户端。
- **网关缓冲/脱敏**：正文停在完整事件边界；`redaction.max_restore_buffer` 为 32MB，
  实际正文均 < 400KB。
- **本地限流**：中断请求没有 429 尝试记录，账号处于正常冷却窗口之外。

## 4. 待上游确认（Intern-ai / tokenplan）

报障时附上上表 5 个 `upstream_request_id`、模型 `deepseek-v4.1-flash-1017`、
账号 Key 尾号（`sense-baa1ebe8`），并说明：

1. 响应 HTTP 200、SSE 正常开始，生成中途（87–252s 内）无 `finish_reason` 直接关闭；
2. 无 `data: [DONE]`、无 usage 块，属于协议级中断；
3. 同一时段另有 1 次连接失败（90s 后无响应）；
4. 请求为顺序单并发，上下文 12–15 万 token，`max_completion_tokens=32768`。

## 5. 对照实验（确认是否为上游时长/长度相关）

1. 同一模型把 `max_completion_tokens` 降到 4096/8192，观察中断率是否下降；
2. 非流式（`stream:false`）跑同规模请求，区分「SSE 链路」与「模型进程」问题；
3. 关掉 reasoning 输出（若平台参数支持），对比 reasoning 长流的中断率；
4. 用 `stream_copy_failed` 日志的 `read_error` 分类统计（`upstream_closed` / `eof` /
   `reset` / `network_error`），区分「对端正常关闭」与「网络重置」。

## 6. 复发时的取证路径

- 网关日志：`request_completed`（`reason=response_incomplete`）、`stream_copy_failed`
  （本次新增，含 `read_error` 分类与脱敏后的 `read_error_message`）、`upstream_attempt`
  （含 `upstream_request_id`）；
- 会话归档：管理台「会话记录 → 请求明细」可看响应正文尾部与每次尝试；
- 客户端：DSH 会话记录里的 `llm/retry`（`TRANSPORT terminated` / `TIMEOUT`）与时间戳。

## 7. 账号覆盖：单点问题

截至 2026-10-22 的账号/模型矩阵（管理台 `GET /admin/v1/accounts`）：

| 账号 | 平台 | 是否声明 `deepseek-v4.1-flash-1017` | 备注 |
|---|---|---|---|
| `sense-baa1ebe8` | tokenplan | ✅ 是 | 唯一承接方；429 时全组排队 |
| `sense-0a9de49f` / `sense-dda49232` / `sense-e413ac65` / `sense-f2240d9d` | sensenova | ❌ 否（声明 `deepseek-v4.1-flash`） | 有同类模型，需映射后才可承接 |
| `wb-f430b404-…` | workbuddy | ❌ 否（声明 `deepseek-v4.1-flash`） | 同上 |

两个可选方案（需产品/计费确认）：

1. **加账号**：再加一个声明该模型的 tokenplan 账号，保持平台与计费不变；
2. **做模型映射**：把 sensenova / workbuddy 的 `deepseek-v4.1-flash` 映射为
   canonical `deepseek-v4.1-flash-1017`，让同类模型承接溢出流量（会改变实际承接平台与计费）：

**已采用方案 2，但只保留 workbuddy**（2026-10-22 22:50 首次应用，23:05 收窄）：
实测 4 个 sensenova 账号虽然**发现列表里声明** `deepseek-v4.1-flash`，实际调用返回
`403 model is not available in the current token plan`（同账号调 `deepseek-v4-flash`
正常，说明是套餐权限而非网络问题），所以 sensenova 的映射会让请求先吃一次 403。
最终映射只保留 workbuddy，路由组内该模型由 2 个账号（tokenplan + workbuddy）承接；
客户端请求 `deepseek-v4.1-flash` 仍可用（规范化为 canonical 名），映射可由管理台
`PUT /admin/v1/middleware/model_rename` 热更新或改回。

```json
"model_rename": {
  "workbuddy": { "deepseek-v4.1-flash": "deepseek-v4.1-flash-1017" }
}
```

## 8. 本次代码/配置变更（对应 P0）

- `request_timeout_seconds`：300 → **600**（`config.json`、示例配置、桌面默认），必须大于
  客户端常见的 300s 空闲超时，否则网关会先掐断长回答；
- 新增 `pool_wait_timeout_seconds`（默认 **120**，`-1` 关闭）：限流排队使用独立预算，
  耗尽后立即返回 `503 pool_wait_budget_exhausted` + `Retry-After`，不再占用生成窗口；
- 流式诊断：网关自身超时记为 `timeout / upstream_timeout`（不再冒充
  `stream / response_incomplete`），并新增 `stream_copy_failed` 日志记录读取错误分类；
- `client.Do` 阶段客户端断开优先返回 `499 client_cancelled`，不再与 504 混淆；
- **修复账号池重建丢配置**：`LoadAccounts()` 会用 `pool.New(accounts)` 替换账号池，
  但替换后没有重新应用 guards / `routing` / `group_routing` / `model_rename`，导致
  开机后实际按默认 round_robin 运行、改名表也不生效（本次映射首次重启后没生效即此原因）。
  现统一走 `configurePool()`，并新增回归测试
  `TestLoadAccountsKeepsRuntimePoolSettings`。

## 9. 生效验证（2026-10-22 22:50，重启后）

- `GET /v1/models` → `deepseek-v4.1-flash-1017` 的 `platforms` 为
  `["sensenova","tokenplan","workbuddy"]`（5 个账号），原始 `deepseek-v4.1-flash`
  不再单独出现；
- `GET /admin/v1/groups` → `default` 组 `routing=smart`（修复前实际是 round_robin）；
- `GET /admin/v1/info` → `request_timeout_seconds=600`；
- 实测请求（DSH 会话）：`model=deepseek-v4.1-flash-1017` 被路由到 workbuddy 账号，
  `upstream_model=deepseek-v4.1-flash`，`sse_terminal_received/forwarded=true`，
  响应 200、usage 完整。

## 10. 复测结果（2026-10-22 23:00–23:17）

用独立探针绕过网关直连 Intern-ai（使用账号自己的凭证，`max_tokens` 按用例设置），
并对照网关路径，结论如下。

### 直连上游：被接纳的长流全部正常结束

| 用例 | 结果 |
|---|---|
| 短输出 `max_tokens=32`（首次） | **429** `Channel rate limit reached for model` |
| 短输出（20s 后重试） | 200，4.6s，`finish_reason=length`，`[DONE]`，usage 完整 |
| 长输出 #1 `max_tokens=8192` | 200，51.9s，4300 tokens，终态完整 |
| 长输出 #2 | 200，96.2s，终态完整 |
| 长输出 #3（紧接上一条） | **429** |
| 间隔 30s 后长输出 #4 | 200，105.6s，终态完整（TTFT 0.7s） |
| 紧接上一条 #5 | **429** |
| 间隔 30s 后长输出 #6 | 200，64.2s，终态完整（TTFT 17.9s，说明在平台侧排队） |
| 紧接上一条 #7 | **429** |
| 大上下文（117KB prompt）连续 2 次 | 两次都是 **429**（未入流） |

**结论**：

1. 只要请求被平台接纳，长流（52–106s、4k+ 输出）全部正常以 `finish_reason` +
   `[DONE]` + usage 结束，**本次复测没有复现任何中途断流**；
2. 当前可稳定复现的上游问题变成了 **429 `Channel rate limit reached for model`**：
   该模型通道大约只允许 1 个长生成同时在途，紧随其后的请求立即被拒（不是排队，
   而是直接 429）；TTFT 17.9s 的用例说明高峰期会在平台侧排队；
3. 之前的“长流中断”很可能就是同一通道过载下的另一种表现（在途流被断开），
   但需要 Intern-ai 侧日志才能坐实；本报告第 4 节的 `upstream_request_id` 可直接提交。

### 经网关：预算与故障转移符合预期

| 用例 | 结果 |
|---|---|
| 钉住 tokenplan（`/providers/tokenplan/...`）遇到连续 429 | 等待 30s+60s+30s 后，**120s 预算到点返回 `503 pool_wait_budget_exhausted`**，不再耗到 300s |
| 同一请求在通道恢复后重试 | 200，90.9s（网关排队约 85s），说明预算留出了恢复空间 |
| 不钉平台的正常流量 | 18/19 路由到 workbuddy 并成功；tokenplan 429 时自动承接 |
| sensenova 账号 | 该模型 403，按账号级模型权限处理并从路由中排除（15 分钟缓存） |

### 顺带修复：错误响应被中间件包装后记录错标签

启用 `model_rename` 后，响应改写中间件会把 `http.ResponseWriter` 包一层
（`chatRewriteWriter`），而 `markGatewayError` 只认最外层的 `*logResponseWriter`，
导致 `503` 等网关错误在会话记录里显示成**上一次上游尝试的原因**（实测
`pool_wait_budget_exhausted` 被记成 `upstream_rate_or_quota_limit`）。已为
`chatRewriteWriter` / `bridgeWriter` 补 `Unwrap()`，并让 `markGatewayError`
沿包装链查找；`TestPoolWaitBudgetStopsRateLimitWaiting` 现在会走包装链，
去掉修复即失败。

### 复测命令（如后续需要重跑）

探针源码未保留在仓库中；复现步骤：从 `data/gateway.db` 的账号库读取对应平台凭证，
直接 POST `<base_url>/v1/chat/completions`（`stream:true`,
`stream_options.include_usage:true`），记录 `finish_reason`、`[DONE]` 与 usage，
并用 `/providers/{provider}/v1/chat/completions` 走网关对照。
