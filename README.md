# Safuno 安诺 · 多账号模型 API 代理

Go 标准库实现的单实例代理。已接入 SenseNova、WorkBuddy / CodeBuddy 与 Intern-ai（Intern / TokenPlan，书生·端砚），将账号轮询、模型发现、用量统计、额度观测放在统一网关后面。客户端只需要一个网关 Key 和 Base URL。

**新增平台（两种方式）**：
- **运行时热注册，无需重启（供 App 自动更新，不对个人用户开放界面）**：`POST /admin/v1/providers` 注册任意 OpenAI 兼容平台（Base URL、名称、简介、身份头、区域、账号 ID 前缀），保存即生效——立即纳入路由与账号校验、出现于平台选择器、可马上添加账号并发现模型；`DELETE` 停止调度并解绑其账号（账号仍加密保存，重新注册同名平台即恢复）。注册持久化在 config.json 的 `provider_overrides`，重启后自动重放。管理台**不提供**平台管理页面，这是给自动更新流程使用的管理 API。
- **编译内置**：实现 `provider.Provider`（可选 `provider.Descriptor` 自描述平台卡片），包内 `init` 可声明默认源站与区域词表（`config.SetEndpoint` / `config.SetRealmPlatform`）；适配器自己描述自己，网关以实时注册表渲染平台目录，不再需要额外的展示元数据表。运行时注册使用通用适配器 `provider.Generic`；OAuth 登录、协议改写、额度接口等平台特有能力仍需编译内置适配器。

**定位**：对标 [CC Switch](https://github.com/farion1231/cc-switch)（管配置）与 [Magpie](https://github.com/yetone/magpie)（管网关），并在两者都不覆盖的**内容出口**上融合脱敏与还原能力。完整说明见 [融合脱敏的个人 AI 代理网关](docs/agent-gateway-guide-zh.md)。

## 已实现

- `/v1/*` HTTP 透传：Chat Completions、Responses、Anthropic Messages、图片生成、JSON 图片编辑以及其他上游路径。未知字段、multipart、二进制正文、查询参数和 SSE 原样保留，不转换上游协议；`GET /v1/models` 返回各账号发现模型的去重并集。
- 账号选择支持 `round_robin` 与 `smart`（额度窗口优先）两种策略，可按路由分组分别覆盖；跳过停用、401 无效凭证、冷却、并发已满、模型不匹配、已知额度耗尽的账号；冷却或并发占满时进入有界等待队列（`max_waiting_requests`），恢复后继续处理，排队时间另有独立预算，不会吃掉生成本身的时间窗口。
- `model_rename` 把不同平台的原生模型 id 归一成同一个客户端规范名：路由、`GET /v1/models` 与响应中的 `model` 都用规范名，发给各平台的请求体再改写回它自己的 id，因此同一模型可以在多个平台之间聚合、轮询与故障转移。
- 429 按 `Retry-After` 冷却；明确的模型 TPM/RPM 限流仅冷却该号的对应模型，其他额度限制冷却整个账号。5xx 短暂冷却。明确拒绝的生成请求可切换账号，已开始的流式响应和结果不确定的 POST 不重放。
- 从非流式 JSON 和 SSE 的 `usage` 统计每号实际输入、输出、缓存、总 Token，兼容三种协议；没有 `usage` 时记录未知，不假设消耗为零。
- 内置 `/admin/` Web 管理台，支持添加、编辑、移除、启停、单号检查、搜索筛选、模型目录、用量及额度观测。
- 第一版 macOS 桌面壳（Wails v3）：同进程启动网关，原生窗口 + 菜单栏；Dashboard 直接加载 `/admin/`，管理台前端零改动。
- 管理 API、Prometheus 指标、每号额度快照及可选的外部额度 Feed 轮询。
- 会话归档、输入／输出／缓存 Token、TTFT／TPS、客户端工具耗时上报；识别常用智能体的原生会话标识，缺失时支持有证据的工具调用链续接。
- 统计、额度、管理端启停状态原子持久化；配置账号支持环境变量凭证，网页新增凭证使用 AES-256-GCM 加密，不进入管理响应或统计状态文件。

**余额能力边界：** [SenseNova 官方文档](https://platform.sensenova.cn/docs)当前介绍的是积分额度，并没有公开 API Key 可调用的余额查询接口。因此本服务不能仅凭 `sk-` Key 自动读取官方控制台余额。默认 `quota_status=unsupported`、`quota=null`。可手动上报控制台观测，或配置你提供的可信额度 Feed 自动同步。服务不会把 Token 数换算成积分，也不会编造余额。分析见 [docs/sensenova.md](docs/sensenova.md)。

## 本地启动

需要 Go 1.23 或更新版本；网关二进制不需要 Node 或前端构建步骤。macOS 桌面壳是独立的 Go module（`desktop/go.mod`，工具链要求更高，见后文「macOS 桌面 App」）。

```bash
cp config.example.json config.json
export GATEWAY_API_KEY='替换为至少16字符的随机客户端密钥'
export GATEWAY_ADMIN_KEY='替换为至少16字符且不同的随机管理密钥'
export SENSENOVA_ACCOUNT_1_KEY='sk-第一个账号的真实Key'
export SENSENOVA_ACCOUNT_2_KEY='sk-第二个账号的真实Key'
go run ./cmd/gateway -config config.json
```

只有一个账号时，删除示例配置中的第二个账号。每条 `accounts` 应对应一个真实额度主体，不要把同一账号的多个 Key 当成多份独立余额。`base_url` 默认为 `https://token.sensenova.cn`，只填源站，不带 `/v1`。可用 `models` 数组设置账号模型白名单，省略则使用动态发现结果。`max_in_flight` 控制每号最大并发，修改凭证环境变量或账号清单后重启服务。

改完代码或配置后，用 `make restart` 一步完成「备份旧产物 → 编译 → SIGTERM 优雅停机（落盘）→ 加载 `.env` 启动新进程 → 健康检查」；`bin/gateway.before-*` 是回滚用的旧快照。注意：重启会中断此刻正在进行的流式请求，客户端需要自行重试。

### 关键配置项

| 字段 | 默认 | 说明 |
| --- | --- | --- |
| `request_timeout_seconds` | `300`（示例与桌面默认 `600`） | 单个请求的总预算，覆盖排队与所有重试；应大于客户端自身的读超时／空闲超时 |
| `pool_wait_timeout_seconds` | `120` | 限流排队的独立预算，`-1` 关闭；耗尽返回 `503 pool_wait_budget_exhausted` + `Retry-After` |
| `max_waiting_requests` | `128` | 等待队列长度（1–10000），满则 `503 pool_queue_full` |
| `max_attempts` | `3` | 非 429 的换号重试上限；429 安全重试只受截止时间约束 |
| `routing` / `group_routing` | `round_robin` | 账号选择策略：`round_robin` 或 `smart`；可按分组覆盖 |
| `model_rename` | 空 | 按平台把上游模型 id 映射为客户端规范名，实现跨平台聚合 |
| `sessions.retention_days` / `max_body_mb` | `0` / `0`（不裁剪） | 会话正文的保留天数与容量上限；示例配置与桌面端为 `30` / `2048` |
| `max_request_bytes` | `33554432` | 请求体内存缓存上限，超限返回 `413` |

## 脱敏与还原

配置 `redaction.enabled=true` 后，网关在请求出网前将敏感文本替换为稳定占位符，并在响应返回客户端时还原。JSON 按字符串值处理，嵌套的工具参数先解码再处理；模型名、会话 ID、工具标识和 JSON Schema 保留协议含义。gzip 请求与响应、表单和普通文本响应均有对应处理。multipart、二进制及媒体数据原样透传，不识别其中的敏感内容。

流式还原按 choice、工具调用、Responses 输出项和 Messages 内容块分别维护通道，支持跨事件分片、CRLF 与多行 data。工具参数在对应调用结束时收齐并按完整 JSON 还原，再发送参数增量；未知或未完成的工具占位符会在结束事件发出前中止调用。源码中的字面占位符示例单独编码，避免误判。`max_restore_buffer` 限制普通响应、SSE 事件及累计工具参数，超过限制会中止响应，不返回截断成功。

占位符映射使用 AES-256-GCM 加密，首次启动即检查密钥；新映射落盘成功后才发送请求，并在后台及退出时保存更新。映射满时不淘汰尚未过期的值；默认 `fail_closed=true` 会拒绝无法安全脱敏的请求。保存失败保留待重试状态，损坏的映射文件不会被覆盖。备份时需保留映射文件及其 `.key`，或保留配置引用的密钥环境变量。

管理台规则保存会先校验并原子写入启动所用的配置文件，再热更新；写入失败保留当前规则。危险工具防护当前为 observe 模式，普通与流式响应都会记录匹配结果。

每一条掩码（出网前）与还原（返回时）都会输出 `redaction span` 日志，记录命中规则、原文、占位符与 `request_id`，可逐个核对涉密记录在拦截与还原两侧的完整映射；格式与过滤示例见「隐私脱敏层 → 脱敏映射日志」。

检查范围、回归与实际接口结果见 [脱敏检查记录](docs/redaction-audit-2026-10-19.json)。

## 接入 WorkBuddy / CodeBuddy（腾讯账号，v0.6.0）

把腾讯 **WorkBuddy（www.workbuddy.ai，国际版）** 与 **CodeBuddy（copilot.tencent.com / www.codebuddy.cn，国内版）** 的原生登录账号封装成 OpenAI 兼容 API。上游对话端点本身是 OpenAI Chat Completions 协议（`/v2/chat/completions`），网关在透传之上补齐：账号身份头族、会话头派生、路径改写、token 自动续期、积分余额观测与每日护栏。

- **账号凭证**：每号需要 `uid`（X-User-Id）、`access_token`（有效期短的 JWT）与 `refresh_token`（用于续期）。三选一：① 管理台一键 **浏览器 OAuth 授权**（推荐，自动获取并入库，见下节）；② 网页表单粘贴或管理端批量导入；③ 放进环境变量（`api_key_env` / `refresh_token_env` / `uid_env`）。`uid` 是必需项。
- **区域**：`realm` 为 `intl`（默认）或 `cn`；根据 `base_url` 域名（`copilot.tencent.com` / `codebuddy.cn` / `workbuddy.cn`）可自动推断。区域决定上游端点、身份版本与管理头（`X-Domain`、UA、`Accept-Language`、`X-Auth-Refresh-Source`）。
- **客户端身份**：`product` 可选 `desktop`（默认，对齐 5.8.4 桌面端抓包）、`vscode`（官方扩展 4.12）、`cli`（官方 CodeBuddy CLI 2.168.2）。CLI / VSCode 身份的对话请求体会做 **gzip + 紧凑 JSON**（官方 RequestBodyGzip），请求头会带 `Content-Encoding: gzip`。换身份 = 换一套 UA / `X-IDE-*` 指纹。
- **会话头**：缺 `X-Conversation-ID` 系列上游会回 400 / code 11-128。网关按（账号, 网关会话 ID）稳定派生对话 ID（同一会话多轮复用、账号之间互不关联），请求/消息 ID 每次新生成；`X-Root-Request-ID` 与 `X-Conversation-Request-ID` 同值。未开启会话归档时也会解析并返回会话 ID。
- **token 存活**：后台每 5 分钟扫描一次，JWT 到期前 1 小时自动刷新并回写加密账号存储；上游 401 时按账号互斥刷新一次并重试（有状态会话也只在原账号重试）。刷新端点 `POST {origin}/v2/plugin/auth/token/refresh`，上游会轮换 refresh token。
- **模型目录**：内置与官方桌面端对齐的精筛清单（国际版 17 个、国内版 14 个：剔除补全模型、`-sg`/`-x` 变体、多云专线和付费副本），并尝试 `GET {chat_upstream}/v3/config` 实时同步（失败自动回落内置快照）。模型元数据含上下文窗口、输出上限、视觉/工具/推理能力与积分倍率。
- **余额**：`Quota()` 走计费接口 `POST {billing_upstream}/v2/billing/meter/get-user-resource` 读取积分套餐（国内计费在 codebuddy.cn）。积分为浮点余额，映射为 `unit=credits` 的额度快照，每 10 分钟最多刷新一次，失败 2 分钟退避重试；不把 Token 换算成积分。上游明确回 `402` 时将该号停放到本地次日 04:00。
- **错误语义**：429 + code `6004` 记为模型级限流；403 + code `11140` 视为账号级风控，冷却整个区域（同区域其他账号共享派生设备指纹，避免连坐）；`402` 为额度耗尽。401 触发自动续期。其他 4xx 原样透传，不在账号之间猜测性重放。

### 配置示例

```json
{
  "default_provider": "workbuddy",
  "default_realm": "intl",
  "guards": {
    "reserve_credits": 0,
    "daily_tokens": 0,
    "daily_credits": 0,
    "per_model_daily_tokens": 0,
    "reset_hour": 0
  },
  "accounts": [
    {
      "id": "wb-intl-1",
      "provider": "workbuddy",
      "api_key_env": "WORKBUDDY_INTL_1_ACCESS",
      "refresh_token_env": "WORKBUDDY_INTL_1_REFRESH",
      "uid_env": "WORKBUDDY_INTL_1_UID",
      "realm": "intl",
      "product": "desktop",
      "max_in_flight": 4
    }
  ]
}
```

也可以从空账号池启动，在管理台“概览 → 添加账号”选择 **WorkBuddy / CodeBuddy** 平台：可点“**浏览器授权登录**”一键完成官方登录并自动入库（推荐，见上节），或直接粘贴 UID、Access Token、Refresh Token（留空则无法自动续期）。批量导入走 `POST /admin/v1/accounts/import`：

```bash
curl -X POST http://127.0.0.1:8080/admin/v1/accounts/import \
  -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"accounts":[{"id":"wb-1","name":"国际版主号","uid":"...","access_token":"...","refresh_token":"...","realm":"intl","product":"desktop"}]}'
```

返回 `{"imported":N,"created":[...],"errors":[...]}`，失败的账号单独列出，不影响其余导入。

### 浏览器 OAuth 授权登录

管理台在“概览 → 添加账号”打开“接入新的账号”，平台选 **WorkBuddy / CodeBuddy** 后出现“**浏览器授权登录**”面板：点“开始浏览器授权”会打开官方登录页，网关负责把 `uid`、`access_token`、`refresh_token` 和有效期自动提取出来并加密入库，全程不需要复制粘贴任何凭证。流程与 workbuddy2api-hub 的看板 OAuth 一致：

1. 网关请求 `POST {chat_upstream}/v2/plugin/auth/state?platform=...`（带该区域官方浏览器头族），拿到一次性 `state` 与官方 `authUrl`；
2. 网页打开 `authUrl`，用户扫码/账号密码登录；
3. 网关轮询 `GET {chat_upstream}/v2/plugin/auth/token?state=...`：`code 11217` 表示仍在登录中；`code 0` 表示完成，从 access token 的 JWT `sub` 解析 `uid`，并请求 `/v2/plugin/login/account` 取昵称；
4. 网关以 `wb-<uid>` 为账号 ID 自动建号（同 ID 再次登录则原地轮换凭证，不产生重复账号）。

管理 API（均需 `GATEWAY_ADMIN_KEY`）：

| 接口 | 说明 |
|---|---|
| `POST /admin/v1/workbuddy/login/start` `{"realm":"intl\|cn","platform":"CLI"}` | 开始授权，返回 `{state, auth_url, realm, platform}` |
| `POST /admin/v1/workbuddy/login/poll` `{"state":...}` | 轮询状态：`pending` / `ok`（含自动建号结果）/ `error` / `expired` / `unknown` |
| `POST /admin/v1/workbuddy/login/cancel` `{"state":...}` | 放弃登录 |

登录状态只保存在网关内存（10 分钟未完成自动失效），每 IP 每分钟最多发起 `6` 次；凭证由网关直接写入 AES-256-GCM 加密的账号存储，浏览器和客户端都看不到 token。

### 每日签到与福利任务（领取积分）

对照 workbuddy2api-hub 的调度器，网关每天 **09:15 / 21:15（本地时区）** 自动领取官方每日福利，每个账号每天只领一次（成功日期按 “2006-01-02” 落盘到状态文件，重启不会重复领取）：

| 任务 | 适用区域 | 上游请求 | 说明 |
|---|---|---|---|
| 每日签到 | 国内版（cn） | `POST {billing}/v2/billing/meter/daily-checkin` | 领取当日签到积分；上游回 `10001 / 已签到` 视为当日已完成，不重复计数 |
| 活跃上报 | 国内版（cn） | `POST {chat}/v2/report` + `GET /activity/growth/streak` | 复刻桌面端 `chat_request_send` 事件，点亮 growth 连登；成功后回读连登天数 |
| 每日活跃打卡 | 国际版（intl） | `POST {chat}/v2/chat/completions`（免费模型 `deepseek-v4.1-flash`，`max_tokens=10`） | 桌面身份轻量对话（官方每日活跃 30/50 积分以网页通道会话为准，见边界） |

自动调度会跳过停用、凭证无效、冷却中、额度耗尽停放的账号；账号之间错峰 2 秒，避免同设备连坐风控。国内版账号每次调度还会顺带刷新一次签到状态快照（今日是否已签、连登天数、每日积分）。

管理台账号详情（抽屉）对 WorkBuddy 账号展示“每日福利”状态，并提供手动触发按钮；管理 API 与 OAuth 一致均需 `GATEWAY_ADMIN_KEY`：

```bash
# 国内版：每日签到 / 活跃上报
curl -X POST http://127.0.0.1:8080/admin/v1/accounts/wb-cn/tasks/checkin  -H "Authorization: Bearer $GATEWAY_ADMIN_KEY"
curl -X POST http://127.0.0.1:8080/admin/v1/accounts/wb-cn/tasks/activity -H "Authorization: Bearer $GATEWAY_ADMIN_KEY"
# 国际版：每日活跃打卡
curl -X POST http://127.0.0.1:8080/admin/v1/accounts/wb-intl/tasks/daily-chat -H "Authorization: Bearer $GATEWAY_ADMIN_KEY"
```

手动触发与自动调度共用同一套“每天一次”日期闸门：当天已领过再点会直接返回成功/已签到，不会重复扣积分。签到领取的积分会随下一次额度刷新（`get-user-resource`）反映到账号额度。

### 每日护栏（guards）

| 字段 | 作用 |
|---|---|
| `reserve_credits` | 新鲜余额低于该值时不再接单（避免余额耗尽触发上游提醒） |
| `daily_tokens` | 账号当日 Token 达到该值后暂停，请求自动切到其他账号 |
| `daily_credits` | 按模型积分倍率折算的当日积分预算，达到后只服务免费模型（`x0.00`） |
| `per_model_daily_tokens` | 账号在单个模型上的当日 Token 上限，超出只禁该模型 |
| `reset_hour` | 本地时间窗口翻转整点（默认 0 点） |

计数按账号独立、随状态文件持久化；`0` 表示关闭。免费/付费以该账号发现的模型目录 `credits` 倍率为准，未知模型按付费处理。请求路径生效，管理端检查不受影响。

### 双区域调度

国际版与国内版按独立出口处理。`default_realm` 配置调度偏好，也可用请求头 `X-WorkBuddy-Realm: intl|cn` 按请求覆盖；偏好区域没有可用账号时回落到另一区域。会话绑定优先于区域偏好。请求头不转发上游。

### 边界

- 目前只原生透传 `/v1/chat/completions`（重写到 `/v2/chat/completions`）；`/v1/responses` 等其它路径原样转发，上游可能返回 400/404。hub 的 Responses↔Chat 协议互译与 Anthropic Messages 桥按需另行评估。
- 浏览器 OAuth 授权只负责**获取并入库**账号凭证，不生成 Turing Shield 设备 token（国内版 `X-Device-Token` 缺省；VSCode 身份按官方失败态发 `X-Device-Token-Error: sdk_not_initialized`）。首次对话前建议到管理台对账号做一次“检查”以完成模型发现。
- 国际版“每日活跃打卡”目前只做**桌面身份轻量对话**这一条腿；hub 里能计入官方 30/50 每日福利的**网页通道会话**（建会话 → ACP 沙箱联调 → 轮询 completed）尚未移植，因此该任务的真实到账以上游“每日活跃”口径为准，国内版每日签到与活跃上报则与 hub 同接口、同口径。
- 上游是商业客户端，逆向与反代违反其服务条款；建议仅自有账号、小流量自用，任何自造指纹都可能被上游 WAF 识别。


## 接入 Intern-ai（TokenPlan / 书生·端砚，v0.6.1）

管理台里的平台标签是 **Intern-ai**（provider id 仍为 `tokenplan`）。Intern-ai 是把 Intern 系列与第三方模型打包成「墨点」额度的平台（[控制台](https://discovery.intern-ai.org.cn/token-plan/home)、[文档](https://discovery.intern-ai.org.cn/docs/)）。同一个源站同时提供三种协议，网关原样透传：

| 用途 | 路径 | 说明 |
| --- | --- | --- |
| 生成（OpenAI） | `/v1/chat/completions`、`/v1/responses`、`/v1/models` | OpenAI 兼容 |
| 生成（Anthropic） | `/v1/messages` | 需 `anthropic-version` 头 |
| 结构化决策 | `/v1/systemone` | `intern-decision-4b` 等决策模型专用 |

- **账号凭证**：`Authorization: Bearer <API Key>`（也支持 Anthropic 的 `x-api-key`）。Key 单人最多 10 把、有效期 6 个月，需在控制台自行轮换。
- **模型发现**：`GET /v1/models` 一份响应里返回两段——`data[].id` 是生成模型，`models[].name` 是决策模型（`systemone_only: true`）；网关取并集去重。
- **限流**：生成 50 RPM、决策 200 RPM、2,000,000 TPM，按**用户**共享（多把 Key 不叠加）。429 的 `rate_limit_exceeded` 可重试、`quota_exceeded` 不可重试，因此网关统一按 `Retry-After` 冷却整号，而不是单个模型。
- **模型无权限**：`model_not_available` 返回 **404** 而非 403；403 一族（`api_key_disabled`、`user_disabled`、`whitelist_required`）是账号级，不对应单个模型。
- **额度边界**：墨点余额只在登录态控制台可见，API Key 调不到余额接口，故 `quota_status=unsupported`。与 SenseNova 相同，可手动上报快照或配置可信额度 Feed。

**实测的容量特征（2026-10-22 复测）**：除文档口径的 RPM／TPM 之外，平台还会返回 **`429 Channel rate limit reached for model`**——这是**模型通道级**限制，实测同一账号同一模型基本只放行一个长生成，紧随其后的请求立即被拒（不是排队）；被接纳的长流吞吐在 **11–84 tok/s** 之间大幅波动（高峰时段掉到 11–14 tok/s，TTFT 长尾可达 39–70 秒），过载时在途流可能被上游提前断开，表现为会话记录里的 `stream / response_incomplete`。因此建议：给该模型配置可承接的备用账号（或 `model_rename` 映射到其他平台），把 `request_timeout_seconds` 设得大于客户端超时，并让 `pool_wait_timeout_seconds` 控制排队上限。证据、复现步骤与提交给上游的报障信息见 [上游流式中断分析报告](docs/upstream-stream-truncation-2026-10-22.md)。

```json
{
  "default_provider": "sensenova",
  "accounts": [
    {
      "id": "tp-1",
      "name": "Intern-ai 主号",
      "provider": "tokenplan",
      "base_url": "https://discovery-api.intern-ai.org.cn",
      "api_key_env": "TOKENPLAN_ACCOUNT_1_KEY",
      "max_in_flight": 4
    }
  ]
}
```

`base_url` 只填**源站**（不含 `/v1`）：网关会在其后拼接客户端原始路径。详细接口分析见 [docs/tokenplan.md](docs/tokenplan.md)。

### 添加账号：平台用 logo 卡片而不是下拉框

管理台的「接入新的账号」把平台选择做成了**带 logo 的卡片单选组**，而不是下拉框：

- 平台 logo 是视觉锚点，一眼可辨；下拉框在平台变多后只能靠文字查找，超过 8～10 项就要滚动。
- 卡片是二维网格，平台数量增长时依然可扫视；新增平台由服务端平台目录自动渲染，前端零改动。
- 切换平台时下方字段（凭证名、占位符、源站默认值、WorkBuddy 的区域/身份字段与 OAuth 面板）随目录自动切换；编辑已有账号时平台卡片锁定，避免误改。
- 无障碍：`role="radiogroup"` + 方向键切换，键盘可用。

平台目录来自 `GET /admin/v1/info` 的 `platforms` 字段（也可单独取 `GET /admin/v1/platforms`），每项包含 `id / name / tagline / logo / base_url / credential / placeholder / hint / auth / requires_uid / quota`。


## Web 管理台

也可以从空账号池启动，再通过网页添加账号：

```bash
cp config.admin.example.json config.json
export GATEWAY_API_KEY='替换为至少16字符的随机客户端密钥'
export GATEWAY_ADMIN_KEY='替换为至少16字符且不同的随机管理密钥'
go run ./cmd/gateway -config config.json
```

打开 `http://127.0.0.1:8080/admin/` 即可直接进入管理台，无需登录；`GATEWAY_ADMIN_KEY` 只用于 curl、脚本等外部管理调用。已有编译产物时，可直接运行 `./bin/gateway -config config.json`，无需 Node 或前端构建步骤。

### macOS 桌面 App（第一版）

第一版桌面壳使用 Wails v3 提供原生窗口和菜单栏，网关仍在同一进程内启动；Dashboard 直接加载网关自己的 `/admin/`，管理台前端零改动。

```bash
make app          # 编译 bin/safuno
make app-package  # 生成 dist/Safuno.app（本地 ad-hoc 签名）
open dist/Safuno.app
```

首次启动会在 `~/Library/Application Support/Safuno/` 生成空账号池配置和随机 Key，Key 存入 macOS Keychain。要接管现有 `config.json` 和 `.env`，先停掉占用 8080 的 CLI 网关，再运行 `make app-start-existing`。详见 [desktop/README.md](desktop/README.md)。

当前限制：仅 macOS、未签名/未公证、没有自动更新（管理台已免登录）；Windows/Linux 壳后续按同一架构扩展。

管理台包含概览、账号池、模型目录、会话管理、用量统计和接口接入页。概览页集中展示接入账号、可用账号、累计请求、Token 总用量以及「刷新状态 / 添加账号」入口；其余页面只保留各自的业务内容，不再重复这些汇总指标和操作。添加账号后会调用模型发现接口检查连接，不发起生成请求。账号名称、并发、源站和模型白名单可在线修改；编辑时 API Key 留空保留当前凭证，也可输入新 Key 轮换。网页保存无需重启。检查结果展示成功或失败的时间，失效凭证、冷却、停用和权限限制单独呈现。存在进行中的业务请求时，修改和移除返回 409，可先停用，等待请求结束后重试。

管理台不需要登录：网关返回 `/admin/` 时自动设置一个 HttpOnly、SameSite=Strict 的同源会话 Cookie，打开页面即可管理账号池。所有管理 API 仍接受 `GATEWAY_ADMIN_KEY`，curl、脚本和远程自动化继续用 Bearer Key 调用；会话 Cookie 由网关进程随机生成，网关重启后页面会自动刷新取得新会话。由于打开页面就能获得管理权限，请保持默认的 `127.0.0.1` 监听，或自行在反向代理层加访问控制。

在“接口接入”页点击“显示 Key”或“复制 Key”可取得当前生效的客户端网关 Key；“复制示例”会填入该 Key。默认隐藏，离开此页后清除页面内存中的 Key。客户端密钥由启动环境变量 `GATEWAY_API_KEY` 设置，修改后需重新加载启动环境并重启网关；本机启动时使用的值保存在忽略提交的 `.env`。Base URL 是 SDK 配置项，检查接入可调用 `GET /v1/models`。

网页账号及对配置账号的修改保存到 `account_store_file`，默认 `data/accounts.enc`。默认首次保存时生成 `data/accounts.enc.key`，两者权限均为 `0600`。部署时应一起备份，或通过 `account_store_key_env` 指定的环境变量 `GATEWAY_ACCOUNT_STORE_KEY` 提供固定的 **32 字节标准 Base64 编码**主密钥；丢失或更换主密钥后将无法解密旧账号存储。加密密钥不会通过 API 返回。环境变量账号仅保存变量引用，未轮换为网页凭证时不会复制其明文到账号记录。

移除配置文件中存在的账号会保存删除标记，重启后不会自动恢复；可通过网页重新添加同 ID 账号。清单变动采用先落盘、再更新调度的顺序，存储失败不会激活未保存的账号。一个账号存储和状态文件仍只支持一个网关进程。

页面支持手动记录选中积分池的余额，作为 `partial=true` 的局部观测；它不用于断言未观测积分池已经耗尽。该操作会替换旧快照，页面明确展示来源、时间及过期状态。需要完整的多池自动同步时使用额度 Feed。

```bash
curl http://127.0.0.1:8080/v1/models \
  -H "Authorization: Bearer $GATEWAY_API_KEY"

curl http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer $GATEWAY_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"sensenova-6.8-flash-lite","messages":[{"role":"user","content":"你好"}],"stream":true}'

curl http://127.0.0.1:8080/admin/v1/accounts \
  -H "Authorization: Bearer $GATEWAY_ADMIN_KEY"
```

OpenAI 兼容客户端使用 `base_url=http://127.0.0.1:8080/v1` 和网关 Key。Anthropic 兼容客户端使用 `base_url=http://127.0.0.1:8080`，可以通过 Bearer 或 `X-API-Key` 传网关 Key。上游凭证由网关替换。

## 会话管理

客户端用 `X-Session-ID` 为同一会话的每次请求提供相同 ID，例如：

```bash
curl http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer $GATEWAY_API_KEY" \
  -H 'X-Session-ID: project-review-001' \
  -H 'Content-Type: application/json' \
  -d '{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"你好"}],"stream":true,"stream_options":{"include_usage":true}}'
```

ID 支持 1–128 位英文字母、数字、点、冒号、下划线和短横线。统一请求头 `X-Session-ID` 始终优先；响应头返回最终 `X-Session-ID` 和 `X-Session-Source`。也可使用 JSON `metadata.session_id`。网关会原样转发请求，客户端仍需提交消息历史；本功能不会自动把历史消息拼回模型输入。

### 智能体会话识别（v0.4.0）

不同智能体没有统一的会话 HTTP 标准。网关按实际收到的字段识别，规则与模型平台无关：

| 客户端／约定 | 可识别字段 |
| --- | --- |
| 通用／自建智能体 | `X-Session-ID`，`metadata.session_id` |
| Claude Code | `X-Claude-Code-Session-Id` |
| OpenCode | `X-OpenCode-Session` |
| Codex／兼容客户端 | `thread-id`／`thread_id`，其次 `session-id`／`session_id` |
| DSH | `X-DeepSeek-Harness-Session-Id` 或 `dsh_session_log.session.id` |
| Pi／兼容适配器 | `X-Session-Affinity`，以及 JSON 字符串 `metadata.user_id` 内明确的 `session_id` |

原生 ID 使用 `claude-code:`、`opencode:`、`dsh:`、`compat:` 命名空间；超长或包含不适合 URL 的字符时使用稳定 SHA-256 ID，原始 ID 仅保存在加密归档内。兼容别名共享 `compat:`，统一 `X-Session-ID` 和 `metadata.session_id` 保持原值。Codex 同时携带 thread 与 session 标识时优先 thread，因为 session 标识可能是缓存亲和键。不同原生头给出冲突的会话值时返回 `400 invalid_session_id`；使用统一头可明确选择。重复、空或非法会话头也返回 400。原生请求头继续透传，网关自己的 `X-Session-ID`、`X-Session-Source` 不转发。

缺少标识时，已知 `previous_response_id` 仍按归档定位原会话和账号。对于完整历史的 Chat Completions、Messages 和数组形式 Responses，紧接工具结果的请求可用“上游实际返回的工具调用 ID + 完整前序输入历史哈希 + 平台与协议”定位唯一成功且完整的记录，并归入原会话。该证据随加密元数据持久化，重启后仍可续接；模糊匹配、失败、未完成、过大或缺失证据均不猜测。当前观察最多保留 128 个、每个不超过 256 字节的工具 ID。普通自然语言下一轮、压缩历史、重写历史、辅助标题请求或不同协议表示可能无法自动续接，仍应让客户端持续发送稳定的会话 ID。

若无明确标识或唯一证据，则创建新会话并在响应头返回 ID。**相同用户、IP、API Key、User-Agent、提示词前缀、`X-Client-Request-ID`、`prompt_cache_key` 均不用于自动合并。** 不要在所有对话中配置同一个固定会话 ID。DSH 的发送行为取决于所用适配器；本机自定义 OpenAI 路径未默认发送会话头，工具链续接只能补足符合条件的模型轮次。网关识别规则兼容不代表上游支持该客户端的模型、协议或工具特性。

会话列表显示客户端和来源，支持按客户端／来源搜索；请求详情提供 `identity.source`、`identity.client`、`client_session_id`、`linked_request_id` 及新建原因。日志记录 `session_source`、`session_client` 和已关联请求 ID，不记录提示词。历史记录没有来源证据时显示“历史来源未知”，不会补造原始请求头或自动合并旧记录。管理端 `GET /admin/v1/session-adapters` 返回当前适配表，`GET /admin/v1/info` 也包含 `session_adapters`。

规则依据见 [会话兼容性说明](docs/session-compatibility.md)。

### 标题请求识别与自动命名（v0.5.0）

客户端额外发起的标题生成请求与会话识别共用同一条归档和调度链路：识别用途 → 定位会话 → 原样代理 → 收齐标题 → 更新会话显示名。网关不额外调用模型。会话 ID 始终固定，`name` 单独更新，标题调用的请求、响应、尝试和 Token 均计入原会话；`title_requests`、`title_usage` 同时提供分类统计，不重复加入总量。

识别 DSH 的用户消息 JSON 数组格式、OpenCode 标题代理的专用系统提示词、本机 Claude Code 已核对的 JSON 标题格式，以及以生成会话标题为专用指令且明确要求只输出标题的兼容系统提示词。普通用户消息“给文章起标题”、普通响应包含 `title` 字段或通用 `auxiliary` 标记不会单独触发会话改名。客户端也可明确传入网关请求头 `X-Request-Purpose: session-title` 或 JSON `metadata.purpose="session-title"`；网关用途头不转发上游，请求体保持原样。

关联优先使用已有 `X-Session-ID`／原生会话头／Responses 续接标识；无明确标识时，根据标题请求携带的**完整原始用户消息**指纹，在同一平台查找唯一会话，可覆盖正在执行的主请求。标题模型可以与主模型不同。相同文本对应多个会话、输入经过截断或改写、多模态正文不能完整对应时保留为“待关联”，不按模型、IP、账号、时间间隔或文本相似度猜测。历史归档中不超过 1 MiB 的完整请求可在启动时解密并在内存重建指纹，不改写原正文。

标题先到且双方均缺少会话头时，后到的主请求可接续唯一匹配的标题临时会话，完成命名和累计统计；明确的会话 ID 始终优先。已有不同显式 ID 的记录不会因此自动搬迁或合并。尚无匹配主请求或存在歧义时保留待关联记录，客户端应持续传入稳定 ID。

支持 Chat Completions、Messages、Responses 的 JSON／SSE 标题文本与 `{"title":"名称"}` 输出。只在成功、完整、未截断、无工具调用且标题为 1–120 字可见单行文本时更新；推理内容不用于命名，超出 8 KiB 标题观察上限时不采用。手动命名优先，历史非 ID 名称也按手动名称保护；较早请求晚返回时不覆盖较新的自动标题。名称来源和请求记录加密保存，重启恢复。

管理台在会话名下显示自动命名来源，时间线标记“标题生成”，请求详情展示识别规则、关联依据、生成标题和采用状态。API 对应字段为 `name_info`、`purpose`、`title.detector`、`title.association`、`title.status`、`title.linked_request_id`。日志只记录用途和规则／状态，不记录生成的标题或用户消息。

管理台“会话管理”提供搜索、分页、重命名、归档／恢复、每次请求与最终响应的正文预览及完整下载。按请求显示平台、模型、账号、状态、错误分类、耗时、输入／输出／合计／缓存命中 Token，以及内部重试的每次账号、状态与 usage。会话合计累加各次上游尝试已报告的用量，包括明确返回 usage 的失败尝试；缓存命中属于输入的一部分，不再叠加到合计。最终响应逐字节归档，包含 SSE；自动重试过程中的中间响应保留状态、诊断及 usage 元数据，不保存其完整正文。

未返回 usage、仅提供部分字段、流中断分别标记；未知字段显示“—”，已知合计可能不完整，绝不按字符估算 Token。请求列表显示的是客户端调用次数，尝试列表和账号统计包含内部重试次数。`GET /v1/models` 等网关本地响应也可归入会话，但没有上游 Token 消耗。建议对支持该选项的 Chat 流请求设置 `stream_options.include_usage=true`；网关不会为取得统计而修改请求体。超出观察上限、压缩或非 JSON 响应可能无法解析 usage，但归档不受 64 KiB 预览／4 MiB usage 观察上限截断。

会话优先绑定同一平台的同一账号，绑定会持久化。完整历史的已知无状态生成接口可在账号不可用时换号；收到明确 401／429 时可按既有重试规则切换。POST 网络错误、5xx、流已开始或中断时不重放。包含 `previous_response_id`、`conversation`、`thread_id` 或操作其他上游状态资源时保持绑定，不跨账号续接。已归档的 Responses ID 可以自动定位原会话和原账号；指定的会话 ID 与已知 Responses 所属会话冲突时返回 `409 session_conflict`。未知 `previous_response_id` 且无已绑定会话时返回 `409 unknown_response_session`，需要提供创建该响应时使用的 `X-Session-ID`。Codex `X-Codex-Turn-State` 使用上游响应中已归档的状态哈希定位签发账号；即使之后无状态请求换号，也不把旧状态发送给新账号。未知、歧义或与指定会话冲突的状态返回 409，需重新开始不携带旧状态的请求。归档仅改变列表分类，同 ID 后续调用仍可继续写入。

默认 `session_store_dir=data/sessions`，元数据与正文均使用 AES-256-GCM 加密；正文使用有序认证分块，检测篡改、错置和截断。首次启动自动生成 `data/sessions.key`（0600），会话目录权限为 0700。也可由 `session_store_key_env`（默认 `GATEWAY_SESSION_STORE_KEY`）指定环境变量，提供 **32 字节标准 Base64 编码**主密钥。备份应包含整个会话目录和对应主密钥；已有数据丢失密钥时拒绝生成替代密钥。配置 `session_store_dir=""` 可关闭归档和会话绑定。

归档开始失败时在调用上游前返回 503；传输过程中磁盘写入失败会保留明确的 `capture_error` 并记录存储故障，已发出的上游请求不能撤回。异常退出时未完成记录在重启后标记为 `interrupted`。正文只保存经过网关鉴权和路由校验的业务请求，不归档管理密钥、Authorization、Cookie 等敏感请求头；应用正文中本身携带的数据按原样加密保存。所有会话查询／下载接口要求管理 Key。完整正文会持续占用磁盘；当前为单机文件存储，无自动删除周期或跨进程共享调度。该版本启用前未保存的请求／响应内容无法补回。

### 会话统计与 Token 构成

会话详情与单次请求详情均返回 `performance` 和 `token_details`，管理台按“会话统计”和“Token 用量”两张卡展示。所有时长在 API 中使用毫秒；页面显示秒。缺少观测数据时返回 `null`，页面显示“未知”。

| 指标 | 计算与边界 |
|---|---|
| 模型用时 `performance.model_time_ms` | 各次上游尝试的累计耗时，包含网络、失败重试和响应转发；并发请求的时长相加，不代表会话墙钟时长 |
| 首 token 平均 `performance.ttft_ms` | 上游尝试开始到首个有效文本、推理或工具参数增量；排除空内容、角色、心跳、创建事件和 usage。按实际观测的流式尝试求算术平均 |
| 输出速度 `performance.tps` | 已报告输出 Token 数 ÷ 首末有效增量到达时间之差；会话使用这些样本的输出 Token 合计 ÷ 生成时长合计，不直接平均每次 TPS |
| 工具用时 `performance.tool_time_ms` | 客户端上报的各次工具执行时间合计，包含失败和取消；并行工具的用时也相加。未上报为未知，上报 0 毫秒才显示 0 |
| 缓存命中 `token_details.cache_hit_rate` | 同时返回有效输入和缓存读取字段的尝试，按缓存读取合计 ÷ 对应输入合计 × 100；输入合计为 0 时比例未知 |
| 未缓存输入 `token_details.uncached_input_tokens` | 上述同一组尝试的输入 Token 减缓存读取 Token，再求和；缓存创建仍属于非读取部分 |
| 缓存读取／输出／总 Token | 沿用上游报告的 usage；缓存读取已包含在输入中，不重复加到总量 |

TTFT 和 TPS 支持 Chat Completions、Responses 及 Anthropic 文本／推理／工具参数流。非流式响应不能测量真实首 Token 时间；整包到达、缺少输出 usage、观察缓冲溢出、失败或中断的流不提供 TPS。单次网络读取中批量到达的增量使用同一时间戳，避免把解析 CPU 时间误算成高速生成。指标均为网关观测，包含网络和传输影响，不是平台内部算力测速。`ttft_samples`、`tps_samples` 展示统计样本量，`generation_tokens` 和 `generation_ms` 保留 TPS 的分子、分母。

缓存统计通过 `cache_samples`、`cache_unknown_attempts` 展示覆盖范围，缺失缓存字段不会被当作零命中。若只覆盖部分请求，比例和未缓存输入只描述已核对的部分。旧记录可根据已保存的尝试耗时和 usage 补算模型用时及缓存构成；此前没有保存增量时间戳的历史请求，其 TTFT／TPS 保持未知。

### 客户端上报工具耗时

工具在 dsh 等客户端内执行，网关无法直接观测执行时长。客户端可在工具完成后调用以下独立接口，使用网关访问 Key；该请求不会转发到模型上游，也不增加模型请求次数或 Token。

```bash
curl -X POST http://127.0.0.1:8080/gateway/v1/sessions/chat-001/tool-events \
  -H "Authorization: Bearer $GATEWAY_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"id":"call_search_001","request_id":"0123456789abcdef01234567","name":"search","duration_ms":300,"status":"success"}'
```

将 `chat-001` 换成对应会话 ID，`request_id` 换成**触发该工具调用的模型请求响应头 `X-Request-ID`**，`id` 使用会话内唯一且稳定的工具调用 ID；客户端可能重复使用原始 tool_call_id 时，可与模型请求 ID 组合。`duration_ms` 必填，支持 0–86400000 毫秒；建议由客户端单调时钟记录。`status` 支持 `success`、`error`、`cancelled`，省略默认 `success`。一个工具调用 ID 在同一会话中只记一次：首次返回 201，相同数据重试返回 200，重复 ID 的不同数据返回 409。请求必须属于指定会话；不能通过上报创建不存在的会话或模型请求。每个会话最多 10000 条工具事件。

服务端强制标记 `source=client_reported` 并记录接收时间，管理台明确标注“客户端上报”。上报数据与会话一样加密保存；不会把工具执行的开始和结束之间的客户端空闲时间猜成工具耗时。只有工具执行明细和时长，不保存工具参数或结果正文。会话和请求详情返回最近 30 条 `tool_events` 及 `tool_events_total`，全部记录可通过管理接口分页查询，并可用 `request_id` 筛选。

## 隐私脱敏层（Redaction）

开启后，请求中的敏感内容在**出网前**替换为稳定占位符，上游只看到占位符；模型回复中的占位符在**返回客户端时**还原为原文，客户端无感。设计参考 [silencer](https://github.com/liuarui/silencer)：同一原文在同一作用域内永远映射到同一个占位符，多轮上下文和会话证据保持稳定。

```json
{
  "redaction": {
    "enabled": true,
    "scope": "default",
    "store_file": "data/redaction.map.enc",
    "store_key_env": "GATEWAY_REDACTION_STORE_KEY",
    "token_ttl_seconds": 2592000,
    "max_restore_buffer": 33554432,
    "fail_closed": true,
    "tool_guard": { "enabled": true, "mode": "observe" },
    "disabled_rules": [],
    "custom_words": { "CODENAME": ["鹰眼系统", "re:[0-9]{4}-X-内部"] }
  }
}
```

- **占位符**：`{{LABEL_6位}}`（无元音随机后缀），如 `{{EMAIL_kwtvxd}}`。作用域默认全实例，跨会话、跨重启稳定。
- **规则集**：内置 26 类，密钥类（API Key、云厂商 Access Key、JWT、Bearer、凭据赋值、连接串、Basic Auth、Cookie、URL 查询凭据、PEM 私钥）与 PII 类（手机号、座机、邮箱、身份证、银行卡 Luhn、IBAN、USCC、车牌、港澳通行证、MAC、IPv4/私网 IPv6），含中文关键词（密码/口令/密钥/凭据/私钥…）与语义校验；可通过 `disabled_rules` 逐条关闭、`custom_words` 增加字面词或 `re:` 正则。规则在管理台热更新，无需重启。
- **持久化**：反向映射（占位符 → 原文）以 AES-256-GCM 加密存于 `store_file`，密钥来自 `store_key_env`（32 字节标准 Base64）或自动生成的 `store_file.key`（0600）。备份时需一并保留。
- **流式还原**：SSE 按逻辑通道增量还原（文本与工具参数独立通道），占位符跨事件拆分会自动缓冲拼接；工具参数按 JSON 内层转义还原。
- **fail-closed**：掩码失败时请求返回 `500 redaction_failed` 或透传明文（由 `fail_closed` 决定，默认拒绝）；工具参数中出现**未解析占位符**（模型虚构的占位符形态 token，或未闭合的占位符前缀）会终止流，绝不把损坏的工具调用交给客户端。检测按占位符语法（`{{LABEL_6位无元音}}`）判断，正文与工具参数中的普通字面 `{{`（模板、嵌套 JSON 等）不受影响；未知占位符在普通文本中原样保留。
- **工具防护**：`tool_guard` 检测 `rm -rf`、`base64 -d|sh`、`curl|sh`、fork bomb 等危险模式，`observe` 模式只计数并记日志，不阻断（`rewrite`/`block` 预留）。
- **边界**：仅处理 JSON / 文本类请求体，multipart 与二进制（图片等）原样透传，避免破坏文件；非流式 JSON 响应在缓冲上限内整体还原，超大响应按原样转发并告警。请求归档存掩码后的正文，`request_completed` 日志记录 `masked_spans` 汇总计数；每一条掩码/还原映射另见下文「脱敏映射日志」。

### 脱敏映射日志

每次出网前替换（`kind=masked`，原文 → 占位符）与返回时还原（`kind=restored`，占位符 → 原文）都会输出一条结构化日志，可在日志中逐个核对每一条涉密记录及其占位符：

```json
{"time":"...","level":"INFO","msg":"redaction span","event":"redaction_span","kind":"masked","label":"CUSTOM","placeholder":"{{CUSTOM_abcdef}}","original":"示例原文","request_id":"3f6e..."}
{"time":"...","level":"INFO","msg":"redaction span","event":"redaction_span","kind":"restored","label":"CUSTOM","placeholder":"{{CUSTOM_abcdef}}","original":"示例原文","request_id":"3f6e..."}
```

- `kind=masked` / `kind=restored` 区分掩码与还原方向；`label` 为命中规则（EMAIL / PHONE / CUSTOM 等）；`placeholder` 与 `original` 构成完整映射，还原日志的 `placeholder` 与同请求掩码日志一一对应。
- `request_id` 与请求日志（`gateway request` / `upstream_attempt`）关联，可用同一请求 ID 对照掩码与还原全过程。
- `original` 先经日志脱敏器处理（账号 Key 替换为 `[REDACTED]`、常见凭证格式清洗、控制字符清理、超长截断），再写入日志；SSE 流式还原按占位符实际解析时刻逐条输出。
- 排查示例：`rg '"msg":"redaction span"' data/gateway.log`，或按 `"request_id":"..."` / `"label":"CUSTOM"` 过滤查看某条记录的完整映射。

管理台「脱敏」页展示掩码/还原/未解析/工具告警统计、实体命中分布、内置规则开关、自定义词编辑与工具防护开关；管理接口 `GET /admin/v1/redaction`、`PUT /admin/v1/redaction/rules`。`/metrics` 增加 `gateway_redaction_*` 指标。

## Agent 接入（M1 · 已实现 · 让本机 Agent 直接用上账号池）

管理台新增 **Agent 接入** 页：内置本机常用 CLI Agent 的目录（DeepSeek Harness、OpenCode、Claude Code、Codex、Cline），为每个 Agent 选择模型、写入配置、自检、还原。参考 [magpie](https://github.com/yetone/magpie) 的“任意 Agent 任意模型”思路，单机自用、不对外。

```json
{
  "agent_store_file": "data/agents.json",
  "agents": {
    "enabled": true,
    "write_mode": "auto",
    "gateway_url": "http://127.0.0.1:8080"
  }
}
```

- **写入模式**：`auto`（默认）自动写入 Agent 配置文件——先备份原文件 → 只增改必要的键（注释、顺序、无关字段原样保留）→ 写入前可预览 diff → 一键还原；`snippet` 仅生成可粘贴的配置片段，不碰文件。DSH 基于环境变量，默认走片段模式（可配置 `target_file` 后自动写入）。
- **漂移保护**：检测到配置文件被外部工具修改后标记“配置漂移”，拒绝覆盖，必须先还原。
- **接入自检**：确认所选模型在网关模型目录中，并发一个 `max_tokens=8` 的最小请求验证连通（消耗极少 token）。
- **内置 Agent 目录**：

| Agent | 写入内容 | 前提 |
|---|---|---|
| DeepSeek Harness | 环境变量片段（`ANTHROPIC_BASE_URL`/`OPENAI_BASE_URL` + API Key） | 无需 |
| OpenCode | `opencode.json` 的 `model` 字段 | 无需 |
| Claude Code | `settings.json` 的 `env.ANTHROPIC_BASE_URL/AUTH_TOKEN` + `model` | 需开启 `bridges.messages` |
| Codex | `config.toml` 的 `model_providers.magpie` 段 | 需开启 `bridges.responses` |
| Cline | VS Code `settings.json` 的 `cline.*` 键 | 无需 |

## 协议桥与方案（M2 · 已实现 · Claude Code / Codex 原生接入）

网关的账号池讲 OpenAI Chat 协议，而 Claude Code 讲 Anthropic Messages、Codex 讲 Responses。M2 增加可选协议互译桥：入站 `/v1/messages`、`/v1/responses` 转换为 Chat 请求转发，响应流再翻译回客户端协议（流式、工具调用、usage 归账均保留，会话/脱敏照常工作）。默认关闭，保持原有透传行为：

```json
{
  "bridges": { "messages": true, "responses": true }
}
```

**方案（Profiles）**：把“每个 Agent 用什么模型”整套选择存成一个名字（如“省钱”“专注”），在 Agent 接入页一键保存/应用/删除。方案是纯模型快照，不含凭证与白名单；应用前对全部 Agent 预检（漂移/不可生成则整体中止），写入失败时尽量还原已切换的 Agent。

**协议桥的已知限制**（单机自用定位，默认关闭）：
- 文本与 tools 完整转换；`reasoning_content`（思考链）与图片/文档等非文本内容块会被丢弃（网关是文本透传定位）；Multi-turn 工具调用以 `role:tool` + `tool_call_id` 映射，id 原样保留，证据续接可用。
- 客户端请求 `stream:false` 而上游坚持返回 SSE（或反之）时，桥接会原样透传上游响应，让 Agent 看到真实失败而不是静默悬挂。
- 上游 4xx/5xx 错误体按原样透传（OpenAI 错误形态），HTTP 状态与 message 可读；`message_start` 的输入 token 先报 0，随 `message_delta` 给出完整 usage。

## 路由决策与用量知情（M3 · 已实现）

- **路由决策数据**：`GET /admin/v1/routing` 返回每次请求的决策时间线——请求→尝试链（每个账号的 HTTP 状态/原因/重试）→等待时长，以及当前冷却中的账号/模型与恢复时间、正在等待的请求数。数据来自网关内存环形记录（最多 512 条），会话归档仍是持久记录。**管理台不提供路由决策页面**，该数据供脚本与自动诊断使用。
- **用量页升级**：按客户端（DSH/Claude Code/Codex/OpenCode…，来自会话身份识别）、模型、账号三个维度聚合 token，并给出**花费估算**：配置 `pricing` 中的价格表（每 1M token，人民币）优先；WorkBuddy 模型无价格时按官方积分倍率近似估算并标注“估算”；两者皆无则标注“未计价”，绝不编造。

```json
{
  "pricing": {
    "deepseek-v4.1-flash": { "input": 2.0, "output": 8.0, "cache_read": 0.5 }
  }
}
```

定位与对标说明见 [docs/agent-gateway-guide-zh.md](docs/agent-gateway-guide-zh.md)（CC Switch + Magpie + 脱敏三层关系与能力对照），产品需求与拆解见 [docs/prd-agent-gateway-zh.md](docs/prd-agent-gateway-zh.md)，Magpie 分析见 [docs/magpie-analysis-zh.md](docs/magpie-analysis-zh.md)。

## M4 增值项（可选能力，按需开启）

全部默认关闭，开启后不影响既有透传行为：

```json
{
  "routing": "smart",
  "model_rename": { "sensenova": { "deepseek-flash": "deepseek-v4.1-flash-1017" } },
  "middleware": {
    "model_map": { "fast": "deepseek-v4.1-flash" },
    "system_prompt": [ { "match_agent": "claude-code", "text": "你是接入网关后的助手。" } ],
    "think_tags": { "strip": true }
  },
  "prewarm": { "enabled": true, "hour": 4, "timeout": 30 },
  "intent": {
    "enabled": true,
    "classifier_model": "deepseek-v4.1-flash",
    "simple_models": ["deepseek-v4.1-flash"],
    "strong_models": ["hy4-preview"],
    "models": ["intent"]
  }
}
```

- **smart 路由**（`routing: "smart"`）：账号排序从轮询升级为"额度窗口快重置的优先"，减少窗口重置时的浪费（参考 Magpie smart 语义）。无窗口信息的账号保持轮询顺序。策略可按分组覆盖：`group_routing: {"分组名": "smart" | "round_robin"}`，未列出的分组跟随全局 `routing`；概览页每个分组卡片上的开关就是设置这个值（热更新并写回 config.json）。
- **中间件**（New API 语义）：`model_map` 把客户端模型名换名发出（响应里改回客户端别名）；`system_prompt` 按客户端/模型注入系统提示词（幂等，不重复注入）；`think_tags.strip` 在 Chat 透传/桥接响应里去掉 `reasoning_content`。中间件在桥接转换之后、provider 之前生效，对四协议客户端一致。
- **模型改名**（`model_rename`）：按平台把上游模型 id 改成客户端要用的规范名。`{"sensenova": {"deepseek-flash": "deepseek-v4.1-flash-1017"}}` 表示 SenseNova 的 `deepseek-flash` 对外以 `deepseek-v4.1-flash-1017` 出现。路由、`GET /v1/models`、响应里的 `model` 都用规范名；发给 SenseNova 的请求体仍改写回它自己的 `deepseek-flash`。因此同组内 TokenPlan 原生的 `deepseek-v4.1-flash-1017` 会和它自动聚合成同一个模型、一起参与轮询，客户端只需要一个名字。旧名字仍然可用（会被归一成规范名）。链式改名、同一原名改成不同规范名会被拒绝。管理接口：`GET /admin/v1/middleware` 查看，`PUT /admin/v1/middleware/model_rename` 热更新并写回 config.json。
- **每日预热**（`prewarm`）：每个窗口重置时刻（默认 04:00 本地，对齐 WorkBuddy 5 小时窗口）对每个可用账号发一个 `max_tokens=1` 的直连请求，让额度窗口提前开启；不占用池并发、不污染统计。
- **意图路由**（`intent`）：客户端请求 `intent` 模型时，网关先用 `classifier_model` 做一次小模型分类（走同一网关，会话 `intent-classifier`），把请求路由到 `simple_models[0]` 或 `strong_models[0]`；分类失败自动回退强模型。`GET /v1/models` 会列出 `intent` 入口。分类器模型禁止出现在 `intent.models`（防递归）。
- **Agent 版本提示**：Agents 页每行"CLI 版本"列可手动探测（`POST /admin/v1/agents/{id}/version`，5 秒超时、内存缓存 1 小时），支持 opencode / claude / codex。

## 统一 API

| 路径 | 方法 | 作用 | 凭证 |
|---|---|---|---|
| `/gateway/v1/sessions/{id}/tool-events` | POST | 客户端幂等上报工具执行耗时；不请求上游 | 网关 Key |
| `/v1/*` | 上游支持的方法 | 默认平台透传；模型列表为聚合结果 | 网关 Key |
| `/providers/sensenova/v1/*` | 同上 | 显式选择平台 | 网关 Key |
| `/admin/v1/accounts` | GET | 每号状态、用量、额度、模型和发现错误 | 管理 Key |
| `/admin/v1/accounts` | POST | 新增账号；接收 `id`、`name`、`api_key`、`base_url`、`max_in_flight`、`models`，WorkBuddy 额外接收 `uid`、`refresh_token`、`realm`、`product` | 管理 Key |
| `/admin/v1/accounts/import` | POST | 批量导入 WorkBuddy OAuth 账号（`accounts[]: id/name/uid/access_token/refresh_token/realm/product`） | 管理 Key |
| `/admin/v1/accounts/{id}` | PUT / DELETE | 编辑／移除账号；`api_key` 留空保留 | 管理 Key |
| `/admin/v1/accounts/{id}/check` | POST | 仅检查指定账号并更新模型与额度源 | 管理 Key |
| `/admin/v1/info` | GET | 网关实例信息与管理台元数据 | 管理 Key |
| `/admin/v1/client-key` | GET | 显式读取当前客户端网关 Key，响应禁止缓存 | 管理 Key |
| `/admin/v1/sessions` | GET | 会话列表；`q` 搜索，`archived=true/false`，`offset`／`limit`（1–100）分页 | 管理 Key |
| `/admin/v1/sessions/{id}` | GET / PUT | 会话汇总和请求分页；PUT `name`、`archived` | 管理 Key |
| `/admin/v1/sessions/{id}/tool-events` | GET | 工具明细，支持 `request_id`、`offset`／`limit` | 管理 Key |
| `/admin/v1/sessions/{id}/requests/{request_id}` | GET | 请求、尝试明细与正文预览（最多 64 KiB） | 管理 Key |
| `/admin/v1/sessions/{id}/requests/{request_id}/request` | GET | 完整原始请求正文下载 | 管理 Key |
| `/admin/v1/sessions/{id}/requests/{request_id}/response` | GET | 完整最终响应正文下载；进行中返回 409 | 管理 Key |
| `/admin/v1/providers` | GET | 平台目录（即实时注册表：内置 + 热注册，含额度能力） | 管理 Key |
| `/admin/v1/providers` | POST | 热注册 OpenAI 兼容平台 `{"id","name","tagline","base_url","auth","uid_field","realms","id_prefix","credential","placeholder","hint","quota"}`；写回 `provider_overrides`，失败回滚 | 管理 Key |
| `/admin/v1/providers/{id}` | GET / PUT / DELETE | 查看 / 修改描述（部分字段；平台 ID 不可改）/ 移除平台（停止调度、解绑其账号；进行中请求存在时 409） | 管理 Key |
| `/admin/v1/refresh` | POST | 立即刷新模型和已配置的额度 Feed | 管理 Key |
| `/admin/v1/accounts/{id}/enabled` | PUT | `{"enabled":false}` 停用或 `true` 启用 | 管理 Key |
| `/admin/v1/accounts/{id}/quota` | PUT / DELETE | 设置／清除额度快照 | 管理 Key |
| `/admin/v1/redaction` | GET | 脱敏层状态、统计与当前规则 | 管理 Key |
| `/admin/v1/redaction/rules` | PUT | 热更新脱敏规则（关闭项、自定义词、工具防护） | 管理 Key |
| `/admin/v1/agents` | GET | Agent 接入：目录、绑定、接入状态与方案列表 | 管理 Key |
| `/admin/v1/agents` | PUT | 修改默认写入模式（`write_mode`: auto/snippet） | 管理 Key |
| `/admin/v1/agents/{id}/bind` | POST | 绑定模型并写入/生成配置；`preview:true` 时只返回内容不写入 | 管理 Key |
| `/admin/v1/agents/{id}/restore` | POST | 还原该 Agent 上次自动写入前的配置备份 | 管理 Key |
| `/admin/v1/agents/{id}/check` | POST | 接入自检：确认模型在目录中并发一个最小请求（消耗极少 token） | 管理 Key |
| `/admin/v1/agents/{id}/snippet` | GET | 获取该 Agent 的接入配置片段 | 管理 Key |
| `/admin/v1/agents/profiles` | GET / PUT | 列出方案；保存方案（`{name, agents:{agent:model}}`，纯模型快照） | 管理 Key |
| `/admin/v1/agents/profiles/{name}` | DELETE | 删除方案 | 管理 Key |
| `/admin/v1/agents/profiles/{name}/apply` | POST | 一键应用方案（预检全部 Agent 后才写入，漂移/失败整体中止）；`preview:true` 只预览 | 管理 Key |
| `/admin/v1/agents/{id}/version` | POST | 探测本机该 CLI 的版本（5 秒超时，缓存 1 小时） | 管理 Key |
| `/admin/v1/routing` | GET | 最近路由决策（请求→尝试链→原因）+ 实时冷却/等待状态；控制台不展示，供脚本诊断 | 管理 Key |
| `/admin/v1/usage` | GET | 按客户端/模型/账号聚合用量与花费估算（`window=24h|7d|all`） | 管理 Key |
| `/metrics` | GET | Prometheus 指标 | 管理 Key |
| `/healthz` | GET | 进程存活检查，不代表上游可用 | 无 |

普通 `/v1/*` 请求也可通过 `X-Provider: sensenova` 选择平台。统一的是入口、鉴权和调度；请求及响应保留所用协议，第一版不跨协议转换。扩展平台有两种方式（见文首）：管理 API 热注册 OpenAI 兼容平台，生效即可用、无需重启（界面不暴露，供 App 自动更新使用）；平台特有能力（OAuth 身份、路径/请求体改写、额度接口）则实现 `internal/provider.Provider` 编译内置。

## 额度监控

额度是**带观测时间的余额快照**，Token 消耗是**经过此代理的请求统计**。两者不是同一数据，其他客户端的用量不在本地统计里。`remaining_tokens` 仅在可信来源确实提供剩余 Token 数时填写；SenseNova 积分用 `buckets` 表示。

上报格式如下，时间为 RFC3339；使用时换成当前观测时间和短期有效时间。下例日期只用于展示结构。

```json
{
  "source": "console-observation",
  "observed_at": "2026-10-17T08:00:00Z",
  "valid_until": "2026-10-17T08:05:00Z",
  "remaining_tokens": null,
  "buckets": [
    {"name":"general-5h","pool":"general","unit":"credits","remaining":42000,"limit":60000},
    {"name":"general-week","pool":"general","unit":"credits","remaining":480000,"limit":600000},
    {"name":"flash-5h","pool":"flash-lite","unit":"credits","remaining":10000,"limit":60000,"models":["sensenova-6.8-flash-lite"]},
    {"name":"flash-week","pool":"flash-lite","unit":"credits","remaining":90000,"limit":600000,"models":["sensenova-6.8-flash-lite"]}
  ]
}
```

将有效快照保存为 `quota.json` 后上报：

```bash
curl -X PUT http://127.0.0.1:8080/admin/v1/accounts/sense-account-1/quota \
  -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" \
  -H 'Content-Type: application/json' --data-binary @quota.json
```

- 同一 `pool` 内多个周期约束必须都有余额；多个适用 `pool` 可替代。例如 Flash-Lite 专属池耗尽，还可使用通用池。返赠积分应由采集方作为独立可用池上报，避免与滚动额度混算。
- `models` 省略表示适用于全部模型；填写则只用于列出的模型。`remaining_tokens=0` 表示账号整体 Token 额度耗尽。
- `quota_status=reported` 表示最近上报的观测值；达到 `valid_until` 或任意桶的 `resets_at` 后显示 `stale`。过期快照不阻塞调度、不暴露为新鲜余额指标；请求仍可能收到上游 429。
- 积分快照不会随请求自动扣减，因为公开文档没有完整的扣分公式。本地用量只能作为对账参考。较旧观测不能覆盖更新快照。

如果已有能合法获取余额的采集器，让它提供 GET JSON 接口，返回同样结构，并在对应账号配置：

```json
{
  "id": "sense-account-1",
  "provider": "sensenova",
  "api_key_env": "SENSENOVA_ACCOUNT_1_KEY",
  "max_in_flight": 4,
  "quota_feed_url": "https://your-quota-service.example/accounts/1",
  "quota_feed_key_env": "SENSE_ACCOUNT_1_QUOTA_FEED_KEY"
}
```

Feed 使用独立密钥，不会收到 SenseNova API Key，也不跟随跳转。后台按照刷新周期轮询；失败保留旧观测并报告 `quota_sync_error`。采集器本身不包含在此版本中，平台未来若公布余额 API，可直接在 Provider 中替换实现。

## 调度与请求语义

未绑定的会话按轮询挑选符合模型、并发和状态约束的账号；已绑定会话优先复用原账号，未发现过模型的账号可以尝试请求。模型每 60 秒自动刷新，聚合列表最多使用三个刷新周期内的缓存；发现结果不代表平台一定授予某号全部模型权限。401 账号在下一次成功探测后恢复，429 冷却不因模型探测成功提前取消。

实际调用明确返回“当前套餐不支持该模型”时，网关将该号的对应模型排除 15 分钟，并从聚合模型列表过滤；管理员可查看 `unavailable_models`。普通 403 权限／策略错误不触发这一判断。`model_cooldowns` 展示按模型的临时限流状态，发现接口刷新不会提前结束冷却。

该拒绝只否定当前账号，不否定整个路由组：同一请求会继续在组内尝试其他候选账号（可跨平台），确认没有可用候选后才向客户端返回 `403 model_not_in_token_plan`。这类 403 未产生生成结果，重放安全，因此不占用 `max_attempts` 生成重试预算。账号级风控 403（如 WorkBuddy `11140`）不属于模型权限，仍按账号级冷却处理、不换号。

同组全部候选账号都被已确认的套餐模型权限限制排除后（含 15 分钟缓存命中），请求直接返回 `403 model_not_in_token_plan`，提示从 `GET /v1/models` 选择可用模型。存在冷却、恢复节流或并发占满的候选账号时，在原请求中等待；停用、凭证无效、模型不支持、已知额度耗尽等没有可恢复候选的情况仍返回 `503 pool_unavailable`。日志按原因给出账号计数，例如 `model_cooldown=1`。

### 限流等待与重试（v0.5.1）

已知生成接口或 GET／HEAD 在上游明确返回 HTTP 429、且尚未向客户端提交响应时，先释放账号并发槽，再优先尝试其他可用账号；没有其他可用账号时等待共享冷却结束，可再次使用原账号。等待和所有尝试共用 `request_timeout_seconds` 总预算，默认 600 秒，客户端取消立即停止，不会每次重试重置超时。等待另有独立预算 `pool_wait_timeout_seconds`，默认 120 秒，`-1` 表示关闭该独立预算、只受总预算约束；预算按整个请求累计，不因换号或重试重置。这样限流排队不会吞掉生成本身的时间窗口（客户端常见的读取／空闲超时通常在 300 秒量级，总预算应大于它）。网关无法预先获知客户端自己的超时；客户端可设置合适的读取超时，两个预算也可在配置中调整。

优先遵守 `Retry-After`；缺失或无效时连续 429 使用 30／60／120 秒退避，后续最多 120 秒，最短等待 1 秒，避免 `Retry-After: 0` 形成紧密重试。冷却由账号池共享，不为每个等待请求分别计时。冷却结束后的 60 秒恢复期内，该受限账号／模型范围每次只放行一个请求，发起间隔至少 1 秒，避免并发等待者同时重试；这是恢复节流策略，不代表推算出了官方 RPM／TPM。

`max_waiting_requests` 默认 128，可配置 1–10000；队列满返回 `503 pool_queue_full`。等待预算耗尽返回 `503 pool_wait_budget_exhausted`（`Retry-After: 5`），不必等到总预算到期；总预算到期返回 `504 pool_wait_timeout`；客户端取消记录为 `client_cancelled`。等待时不提前发送 200 或 SSE 心跳，最终响应仍保持原协议。普通状态相关资源、未知写接口、流已开始、POST 网络错误及 5xx 不因本策略自动重放；已知生成接口的状态续接在明确 429 后只重试原所属账号。

日志增加 `pool_wait_started`／`pool_wait_finished`，最终日志与会话请求详情保存 `pool_wait_ms` 和 `pool_waits`；上游每次 429 和最终成功分别作为尝试保存，已报告的 usage 均计入会话，等待本身不增加 Token。

| 上游结果 | 默认处理 |
|---|---|
| 401 | 标记 Key 无效；已知无状态生成接口可换号 |
| 429 | 明确的模型 TPM/RPM 错误仅冷却该模型；其他情况冷却账号；已知生成接口优先换可用号，否则等待冷却后重试；有状态续接只重试原账号 |
| 403 / 400 / 404 | 已知生成接口上明确套餐不支持的 403：记录该号模型不可用并换号，全部候选排除后返回 `403 model_not_in_token_plan`；其他 403 / 400 / 404 原样返回，不换号重放 |
| 5xx | 冷却；GET / HEAD 的 502、503、504 可换号；POST 原样返回 |
| 网络失败 | GET / HEAD 可换号；POST 返回 502／504 |
| 流中断／流内错误 | 记录失败，不重试；截断流中止下游连接 |

有上游状态依赖的会话请求不使用表中的跨账号重试。非 429 的失败切换受 `max_attempts` 限制，不重复选择已失败账号（已确认的模型权限 403 除外：它会一直换号到候选耗尽，再返回模型不可用）；HTTP 429 的安全重试受整个请求的截止时间约束，可在共享冷却后重用账号。失败尝试也计入 `requests`；模型探测不计入业务请求统计。流结束只统计一次累计 usage，避免重复计算。观察缓冲上限 4 MiB，超大 JSON／SSE 单事件、压缩响应或上游不返回 usage 时可能无法记账，见 `requests_without_usage` 指标。

## 请求日志与排查

### SSE 完成判定（v0.5.2）

Chat 的 `[DONE]`、Messages 的 `message_stop` 和 Responses 的终态事件必须组成完整 SSE 事件（以空行结束），并且所在数据块已完整写出、刷新成功，才确认协议转发完成。确认后立即结束转发并关闭上游响应体，不继续等待 HTTP EOF。客户端在结束事件后关闭连接不会增加失败计数，正常记录可继续用于工具链续接、自动标题和有效 TPS 统计。

终态事件仍在上游读取缓冲中、未完整写出、刷新失败、缺少结束事件、事件未封口或提前取消均不算完成。`response.failed`／`response.incomplete` 和流内错误仍记录失败，即使 HTTP 状态为 200。已开始的流不自动重放。日志增加 `sse_terminal_received` 与 `sse_terminal_forwarded`，分别表示观察到完整终态事件、该事件所在数据块已成功转发并刷新；收到不等于送达。

历史记录缺少“终态事件已刷新成功”的证据，不会仅因归档正文含 `[DONE]` 就自动改判、合并会话或补算 TPS。历史 TPS 无法根据无增量时间戳的正文可靠重建。

网关向标准输出写入 JSON 行日志。本机当前后台进程将输出追加到 `data/gateway.log`。每次代理调用记录 `request_completed`，每次上游尝试记录 `upstream_attempt`；自动换号的尝试共用一个 `request_id`，与响应头 `X-Request-ID` 对应。启用会话后同时记录 `session_id`，可关联加密会话归档。

日志包含请求方法、路径（不含查询参数）、模型、平台、账号 ID、尝试次数、HTTP 状态、耗时、可取得的上游请求 ID、错误分类与已报告的 Token 用量。成功的管理台轮询和健康检查不记录，管理鉴权失败仍会记录。

v0.5.3 起，每条 `upstream_attempt` 还保存以下限流分析字段；即使 429 被内部重试、最终返回成功，原尝试的信息仍独立保留，并写入加密会话记录的 `attempts[]`：

| 字段 | 含义 |
|---|---|
| `attempt_started_at` | 精确的上游尝试发起时间；按账号和模型计算滚动 60 秒窗口时使用此字段 |
| `rate_limit_headers` | 上游响应的 `Retry-After`、`RateLimit` / `RateLimit-Policy`，以及已支持的 `RateLimit-*`、`X-RateLimit-*`、`X-Rate-Limit-*`、`Anthropic-RateLimit-*` 限额、剩余量、重置字段；保留原始值格式和重复值数组，不推断秒数或绝对时间 |
| `rate_limit_headers_truncated` | 限流头经过长度、数量或总量限制时为 `true`；总采样预算 4 KiB，每项最多 4 个值，单值正文最多 256 字符、超长附截断标记，`Retry-After` 优先保留 |
| `request_output_limits` | JSON 请求实际指定的 `max_tokens`、`max_completion_tokens`、`max_output_tokens`，以及 `generationConfig` / `generation_config` 中的输出上限；多个字段分别保存，不推断优先级，也不改写请求 |
| `cached_input_tokens` | 上游明确报告的缓存输入 Token；未报告时为 `null`，明确报告 0 时为 `0` |
| `uncached_input_tokens` | 同时取得有效输入和缓存用量时，记录两者差值；否则为 `null`。缓存已包含在输入中，不重复加入总 Token |
| `usage_reported_fields` | 本次上游实际报告的用量字段，辅助区分缺失字段和真实零值 |

没有收到上游响应时，日志中的 `rate_limit_headers` 为 `null`；收到响应但未提供受支持的限流头时为 `{}`。输出上限缺失、非整数或正文不是 JSON 时不补造默认值。会话归档沿用 `started_at` 保存发起时间，沿用 `usage` 和 `usage.reported_fields` 保存用量及来源，其他采样字段同名保存；旧记录不补写这些新字段。限流头按字段白名单采样并脱敏，不采样鉴权、Cookie 或任意其他响应头。网关自身生成的 `Retry-After` 不作为上游证据。

平台 TPM 是否计入缓存、是否按输出上限预留、窗口和突发容量如何计算，仍需结合真实观测确认；这些字段仅记录证据，不修改既有冷却、重试和调度规则。

| `error_layer` / `reason` | 含义 |
|---|---|
| `gateway_auth` / `unauthorized` | 请求未通过网关鉴权，未请求模型上游 |
| `upstream` / `upstream_auth_failed` | 上游返回 401，拒绝账号凭证 |
| `upstream` / `model_not_in_token_plan` | 上游明确拒绝当前套餐使用该模型，通常为 403 |
| `upstream` / `model_rate_limit` | 上游明确报告模型 TPM/RPM 限制，通常为 429 |
| `upstream` / `upstream_rate_or_quota_limit` | 其他 429，不能直接断言余额为零 |
| `gateway` / `pool_unavailable` | 调度时没有符合条件的账号 |
| `gateway` / `model_not_in_token_plan` | 同组全部候选账号均被确认的模型权限限制排除（含缓存命中），直接返回 403 |
| `stream` / `response_incomplete` | 上游在协议终止事件前结束了流（干净关闭、重置或 GOAWAY） |
| `timeout` / `upstream_timeout` | 响应读到一半时网关自己的 `request_timeout_seconds` 到期；不是上游断流，需调大预算或排查上游卡顿 |
| `pool` / `pool_wait_budget_exhausted` | 限流排队超过 `pool_wait_timeout_seconds`，快速失败以便客户端重试 |
| `client` / `client_cancelled` | 客户端取消或断开（含客户端自身的读取／空闲超时） |

流内错误即使 HTTP 状态为 200，也记录 `outcome=error`。错误响应的 `error.type`、`error.code` 会在可解析时记录；消息只保留已识别的固定错误文本或安全摘要，不记录任意上游原始错误消息。请求正文、生成内容、鉴权头、Cookie 和查询参数均不写入日志；已知凭证及常见 Key 格式会从诊断元数据中脱敏。脱敏拦截的 `redaction span` 日志（见「隐私脱敏层」）是刻意保留的例外：它按命中规则记录每条涉密记录与占位符的完整映射，原文同样经过凭证脱敏与长度截断，用于核对拦截与还原。响应在协议终止事件前结束时额外记录一条 `stream_copy_failed`：`read_error` 给出稳定分类（`upstream_closed`／`eof`／`unexpected_eof`／`deadline_exceeded`／`canceled`／`network_timeout`／`network_error`／`http2_goaway`），`read_error_message` 是经过脱敏截断的读取错误原文；据此可直接区分上游断流、网关超时与网络重置。

调度失败日志额外记录 `pool_rejections`，列出每个相关账号被跳过的首要原因，包括 `model_not_in_token_plan`、`model_cooldown`、`account_cooldown`、`concurrency_limit`、`model_not_supported`、`quota_exhausted` 等；无需再根据通用的 503 文本猜测。

```bash
# 定位模型权限问题
rg '"reason":"model_not_in_token_plan"' data/gateway.log
# 按模型或响应头中的请求 ID 查找相关尝试
rg '"model":"deepseek-v4.1-flash"' data/gateway.log
rg '"request_id":"替换为响应头的请求ID"' data/gateway.log
```

日志从更新后的进程启动时开始记录；旧版只保存了账号最后状态的请求，无法恢复完整历史。

## Docker

```bash
cp .env.example .env
# 编辑 .env 填入真实凭证，再生成容器配置
python3 - <<'PY'
import json
c = json.load(open('config.example.json'))
c['listen'] = '0.0.0.0:8080'
with open('config.docker.json', 'w') as f:
    json.dump(c, f, indent=2)
PY
docker compose up -d --build
```

容器端口只映射到宿主机回环地址。远程接入可在外层部署 HTTPS 反向代理，并为 SSE 关闭响应缓冲。

## 验证与运行边界

```bash
make test      # go test -race ./...
make vet       # go vet ./...
make build     # go build -trimpath -o bin/gateway ./cmd/gateway
./scripts/test-desktop.sh   # 桌面壳（独立 module）的构建与冒烟检查
```

集成测试用模拟上游验证轮询与 smart 路由、失败切换、SSE 及时刷新、三种协议的用量、multipart、模型发现、鉴权隔离、额度 Feed、状态持久化和并发约束；流式失败归类（客户端取消／网关超时／上游断流）、池等待预算、`model_rename` 跨平台聚合，以及「账号池重建后仍保留 guards / routing / model_rename」均有回归用例。真实账号验证需要你在本地填入 Key；当前项目没有内置可用凭证。

会话功能已通过普通 Chat 与 SSE 的真实联调：同一会话两次请求使用同一账号，输入 198、输出 31、合计 229 Token，归档下载与传输字节数一致。报告见 [session-live-test-2026-10-18.json](docs/session-live-test-2026-10-18.json)。模拟测试另外覆盖多账号固定、限流切换、状态续接隔离、并发、存储重启、加密校验、正文下载鉴权与分页。

性能统计真实联调另见 [insights-live-test-2026-10-18.json](docs/insights-live-test-2026-10-18.json)：验证流式 TTFT／TPS、缓存构成、归档校验工具的实际耗时上报及重复上报幂等性。该报告中的速度为当次网关观测值。

真实联调可使用 `python3 scripts/live_test.py`，通过隐藏提示输入 Key，或由 `SENSENOVA_LIVE_KEY` 环境变量提供。加 `--images` 会实际生成和编辑图片并消耗相应用量。脚本启动临时本机网关，验证发现、各文本模型、三种协议、SSE、工具调用、图片理解、鉴权和记账，最后自动关闭进程。报告保存到 `docs/live-test-2026-10-17.json`，不保存凭证或下载生成图片。只有一个真实账号时，不将模拟多号测试表述为真实多号验证。

状态默认每 5 秒保存，正常退出时再保存一次；异常断电可能丢失最后一个周期。单状态文件只支持一个进程，暂未实现分布式额度／并发控制。WebSocket 升级、CONNECT 和非 `/v1/` 控制台请求不在代理范围。请求体在内存中缓存，默认最多 32 MiB；单个请求的总预算由 `request_timeout_seconds` 决定（默认 300 秒，示例配置与桌面端为 600 秒），长回答需要客户端自己的读超时与之匹配（网关到期会记为 `timeout / upstream_timeout`，不再冒充上游断流）。模型和 quota 探测按账号依次进行，账号多或上游慢时实际刷新周期可能变长。管理端刷新是同步操作，应按账号数量设置客户端超时。

项目结构：`cmd/gateway` 负责启动（配置加载、信号处理、优雅停机），`internal/app` 把网关装配成可运行的服务与监听器（CLI 与桌面端共用），`internal/provider` 负责平台适配（`internal/provider/workbuddy` 是 WorkBuddy/CodeBuddy 适配包，含三套客户端身份、模型目录快照与 token 刷新），`internal/pool` 负责调度、额度与每日护栏（含 `model_rename` 跨平台聚合），`internal/gateway` 负责协议透传、管理 API 与请求流水线编排，`internal/observe` 是独立的响应观测层（用量统计、流式 TTFT/TPS、工具调用证据、标题收集与 SSE 脱敏恢复）。

`desktop/` 是独立的 Go module（Wails v3 桌面壳，含 `internal/bootstrap` 的默认配置与 Keychain 集成），与主模块分开管理依赖；`scripts/*-desktop.sh` 负责编译、打包与以现有配置启动。`internal/gateway/stream_diagnostic.go` 集中处理流式失败的归类（客户端取消 / 网关超时 / 上游断流）与读取错误分类。

`internal/accounts` 负责加密账号存储；管理台 HTML、CSS 和 JavaScript 位于 `internal/gateway/web`，通过 `go:embed` 内置到同一个可执行文件。

`internal/sessions` 负责会话元数据、完整正文的加密归档及持久化账号绑定。会话详情接口返回 `data`（汇总）、`requests`、`total`、`offset`、`limit`；记录中的 `reported_fields` 标记确实观测到的 Token 字段，`usage_complete` 说明各次尝试的输入／输出统计是否完整。

平台是插件式、可热更新的注册表：`internal/provider.Registry` 维护名称到实现与展示描述的映射（并发安全，支持 Register/Swap/Replace/Remove，并在变更后触发网关重新发布平台词表、重载有效账号集）。`gateway.New` 注册内置 `sensenova`、`workbuddy`、`tokenplan`，`gateway.NewWithProviders` 允许注入自定义注册表；管理 API 的增删平台走同一注册表，热注册的平台在进程内使用 `provider.Generic`（OpenAI 兼容）适配器，也可通过 `Swap` 换成重新构建的适配器而中断在途请求。账号校验（`config.Registered`）、默认源站（`config.DefaultBaseURL`）与区域／客户端身份词表（`config.RealmValues`/`Products`）都由注册表在启动和变更时发布，`config` 不再硬编码平台名。
