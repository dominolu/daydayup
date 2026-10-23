# Magpie 分析：让指定 Agent 接入"做生意模型"，而不只是提供 API

> 分析对象：[magpie（yetone/magpie）中文 README](https://github.com/yetone/magpie/blob/main/README.zh-CN.md) 及英文参考文档 `docs/reference.md`（2026-10-19 抓取）
> 对照对象：本仓库 Safuno（多账号模型 API 代理网关）
> 结论先行：**可以**。Magpie 证明了一件关键的事——把"账号池 + API 网关"变成生意，靠的不是 API 本身，而是**面向特定 Agent 的适配层**（改配置、补协议、认会话、给限额、算账）。Safuno 目前是"只提供 API"的第一层，下面按 Magpie 的九大能力逐项拆解出差距和可参考的功能/交互。

---

## 1. Magpie 用 30 秒做什么

一句话：**"所有 Agent 的模型，一处搞定"** —— Claude Code 跑 Kimi、Codex 跑 DeepSeek、Gemini CLI 跑 GLM、OpenCode 用你的 ChatGPT 订阅；菜单栏一点切换；所有 Agent 共用一个本地网关（`127.0.0.1:3425`）；额度用完自动悄悄切下一个账号。

它支持 60+ 个 Agent（Claude Code、Codex、Gemini CLI、OpenCode、Cline、Cursor、Zed、VS Code Chat、WorkBuddy、CodeBuddy Code、**DeepSeek Harness**……），并且只显示"你电脑上装了的"。

三层结构（README「整体结构」）：

```
你的 Agent ──► 127.0.0.1:3425 网关（同时支持 OpenAI Chat / Responses / Anthropic Messages / Gemini，并互相转换）
                  │
            路由组 + 插件
                  │
        ┌─────────┼──────────┐
    API Key provider   订阅（Claude/ChatGPT/Copilot/Gemini/Grok 登录态）   本地模型（Ollama 等）
```

关键设计三句话（后面每条都围绕它们展开）：

1. **Agent 适配层**：不是让用户改自己的工具，而是 magpie 去读每个 Agent 的配置文件，只原子改"该改的那一个键"。
2. **订阅即 Provider**：你登录过的 Agent 背后就是一个带模型的订阅，magpie 把它当 provider 给其他所有 Agent 用。
3. **路由组 + 额度窗口**：把多个 key/账号/订阅抽象成一个"模型"，按智能路由分摊，用尽不断线。

---

## 2. 核心洞察：为什么"只提供 API"不够，Agent 适配才是生意的入口

**只提供 API**（Safuno 当前模式）＝ 一个 OpenAI 兼容端点 `/v1/*` + 一个网关 Key。它的边界是：

- 客户端必须是"能自己填 base_url 的工具"，且**自带上游凭证语义**（比如 Claude Code 默认只会打 Anthropic 官方，Codex 默认只打 OpenAI）。
- 买卖双方都只有"Key + URL"两个产品形态，**无法区分客户、无法按客户限额、无法计费**——一个 Key 分给十个人就是十份没有限额的用量。
- 用量是"网关视角"的统计，客户看不到自己的归属和账单，网关也说不清"这笔钱是哪个会话/哪个客户花的"。

**Magpie 证明的生意模型**是这三件事叠加：

1. **让指定 Agent 用上你的货源**（适配层）：magpie 逐 Agent 读配置，把"模型"这个键指向网关，`provider/model` 格式，注释、顺序、格式原样保留；换模型是原子改写，出问题一键改回。
2. **让每个客户有身份和额度**（租户层）：局域网共享时给每个客户端一个**命名网关 Key**，各自设每天/每周/每月的 token 上限和花费上限；Key 还能限定**只能用哪些模型、只能走哪些账号**；`GET /v1/magpie/limit` 让 Key 自己查自己的额度。
3. **让每笔用量能算账**（计费层）：token（输入/输出/缓存读/缓存写）、推理次数都按**官方价格或自定义价格**算花费，美元或人民币；任何模型都能自定义价格；按账号、按上游 Key 出账单，OTLP 导出到自己系统。

> 一句话：**API 是通路，Agent 适配层是入口，Key 限额是闸门，计费是收银台。** Safuno 缺的是后三块。

---

## 3. "让指定 Agent 适配到生意模型"——四条可落地的路径

Magpie 用上了全部四条，Safuno 目前只占了半条。按依赖深度排序：

### 路径 A：配置文件适配（最轻，magpie 的主入口）

magpie 维护一张"Agent → 配置文件 → 字段"的表（参考文档 `Agents` 表，节选）：

| Agent | 文件 | magpie 改的字段 |
|---|---|---|
| Claude Code | `~/.claude/settings.json` | provider、model、opus/sonnet/haiku/fable |
| Codex | `~/.codex/config.toml` | provider、model、effort |
| Gemini CLI | `~/.gemini/settings.json` + `.env` | auth、model |
| OpenCode | `~/.config/opencode/opencode.json(c)` | model、small |
| Zed | `~/.config/zed/settings.json` | model（写入一个 magpie OpenAI 兼容 provider） |
| VS Code Chat | settings.json + chatLanguageModels.json | chat.defaultModel |
| DeepSeek Harness | （base_url + api_key 环境变量） | 同为可填 base_url 的工具 |

交互：**Agents 页**一张表列出本机已装的 Agent，每行一个"模型选择器"；点一下弹出可搜索列表（`provider/model` 格式）；选中后**原子改写配置**，注释/顺序/格式不动；**方案（Profile）**把所有 Agent 的当前配置快照成一个名字（如"省钱""专注"），一键全部切回。

→ Safuno 用法：管理台加一个"Agent 接入"页，给每个支持的 Agent 生成一份可直接粘贴/写入的配置片段（Claude Code 的 `settings.json` 片段、Codex 的 `[model_providers.magpie]` 段、DSH 的 `ANTHROPIC_BASE_URL/API_KEY` 环境变量），并**自动探测本机已装 Agent** 的配置位置、做安全备份后再改写。这是"让指定 Agent 适配"成本最低的一步。

### 路径 B：协议互译（让任意 Agent 都能连，magpie 网关的核心）

magpie 网关同时对外讲 **OpenAI Chat、OpenAI Responses、Anthropic Messages、Gemini** 四种协议，并在它们之间互相转换（流式、工具调用、推理都支持）。所以"任何能填 base URL 的工具"三行环境变量就能接：

```sh
export OPENAI_BASE_URL=http://127.0.0.1:3425/v1     OPENAI_API_KEY=magpie
export ANTHROPIC_BASE_URL=http://127.0.0.1:3425     ANTHROPIC_API_KEY=magpie
export GOOGLE_GEMINI_BASE_URL=http://127.0.0.1:3425 GEMINI_API_KEY=magpie
```

→ Safuno 差距：README「边界」明确写着"目前只原生透传 `/v1/chat/completions`；`/v1/responses` 等其它路径原样转发，上游可能返回 400/404；Responses↔Chat 协议互译与 Anthropic Messages 桥按需另行评估"。**这是"让指定 Agent 适配"的技术第一个关键缺口**：Claude Code 默认讲 Anthropic 协议，Codex 默认讲 Responses，不做互译就只能靠客户手动改 Agent 配置去迁就 Chat 协议（能做，但不是"适配"，是"将就"）。

### 路径 C：把"登录态/订阅"变成 provider（货源扩展，magpie 的差异化）

magpie 读取本机 Agent 的登录信息（Claude Code/Codex 的文件、钥匙串），把**登录着的订阅**当 provider：`claude/claude-sonnet-5`、`codex/gpt-5.5`、`copilot/claude-sonnet-4.5` 出现在任意 Agent 的模型选择器里；多账号自动切换；token 由 magpie 按 Agent 的方式刷新并写回原位置。Claude 订阅甚至驱动本机真实 `claude` 程序、用 MCP 桥接调用方工具。隐私开关：设置→隐私→关闭「订阅账号」。

→ Safuno 已做了一大半的另一半：WorkBuddy/CodeBuddy 正是"把登录账号包成 OpenAI 兼容 API"，且已有浏览器 OAuth、token 自动续期、积分额度观测、每日签到任务。差距在于：没有"把 **本机其他 Agent 的登录态**（比如已登录的 Claude Code 账号）也收编成 provider"这件事——但这是 Magpie 的差异化，**不是做生意的必需项**，可列为远期。

### 路径 D：服务化（本地工具 → 可售卖的服务，magpie 的"生意化"开关）

| 能力 | 说明 |
|---|---|
| **局域网共享** | 打开开关，给每个客户端一个**命名网关 Key**，各自设 token/花费上限；remote 请求必须带 Key（Bearer / x-api-key / x-goog-api-key / `?key=`），无 Key 401 |
| **远程 magpie** | 笔记本用台式机的 provider/账号/路由组，本机配置自己的 Agent |
| **Docker** | `ghcr.io/yetone/magpie`，Web 界面管理，跑在服务器/NAS |
| **同步** | 备份到文件 / WebDAV（坚果云、Nextcloud）/ S3 |

→ 这是"只提供 API → 做生意"的**形态跃迁**：从"一个端口 + 一个 Key"变成"服务器形态 + 每客户命名 Key + 每客户限额"。Safuno 的 Docker 是通回环的（`0.0.0.0:8080` 只映射到宿主机回环），说明当前是"自用工具"心智；做生意需要显式的"共享模式 + 每客户 Key"。

**小结**：四条路径里 A（配置适配）和 B（协议互译）决定"指定的 Agent 能不能用上你的生意模型"，D（服务化）决定"能不能卖"，C（订阅收编）是差异化卖点。

---

## 4. 生意模型必需的六块拼图：Safuno 现状差距清单

Magpie 按"个人工具"的形态做齐了生意模型需要的全部构件。逐块对照：

| # | 拼图 | Magpie 做了什么 | Safuno 现状 | 差距 |
|---|---|---|---|---|
| 1 | **多租户身份** | 命名网关 Key（`magpie gateway-key add Family`），独立启停、轮换、删除，Key 自己可查自身额度 `GET /v1/magpie/limit` | 单一 `GATEWAY_API_KEY`，所有客户端共享一个身份 | **大**：无法区分客户、无法按客户归属用量 |
| 2 | **按客户限额** | 每个 Key 独立限额：token 总量 + 估算花费（美元），按天/周/月、任意 N 天、任意 N 小时（最 5 个窗口叠加，全部通过才放行）；预占 reservation 防超卖；429 用 API 原生错误形态点名"哪个 Key、哪个限额、何时重置"+ `Retry-After`；其他 Key 不受影响 | 有 `guards`（`reserve_credits`/`daily_tokens`/`daily_credits`/`per_model_daily_tokens`），但是**按账号**设的护栏，不是按"客户 Key"卖额度 | **大**：护栏管"别把账号用爆"，不管"客户买了多少"；需要一层 Key 级限额在 pool 之前拒绝 |
| 3 | **计费引擎** | token（未缓存输入/输出/缓存写 + 可选缓存读）按**有效价格**计费：默认取模型官方价，每个模型可自定义四参数价格（input/output/cache read/cache write）甚至长上下文分级价格；无价格的不计入总额、明确标注"unpriced"；美元或人民币 | 精确记录 input/output/cache/total token（含 `reported_fields`），**但没有"价"的概念**，更无花费/账单 | **大**：要做生意，`usage` 之上必须叠一层价格表 → 花费 → 账单 |
| 4 | **账单与导出** | 按账号、按上游 Key 统计花费；会话页每行显示"对话花费 + 标题"；OTLP 导出 | 有会话级 token、TTFT/TPS、Prometheus `/metrics`；管理 API 分页查询 | **中**：缺"按客户/按模型/按天的花费账单"和 OTLP/CSV 导出 |
| 5 | **货架权限（卖什么）** | Key 可限**模型白名单**（`openai/gpt-5 anthropic/*`，`/v1/models` 只列 Key 能用的；越权 403 且错误形态点名可用模型与账号）和**账号白名单**（Key 只能走指定账号） | `/v1/models` 是所有账号的去重并集，任何网关 Key 都能用所有模型 | **中**：缺少"给客户卖指定模型/指定账号"的货架隔离 |
| 6 | **客户自助** | Key 查询自身 `GET /v1/magpie/limit`；用量页展示余额、token、缓存命中率、花费、会话列表（一键在终端重开） | 管理端有全部明细，但客户端只有一个 Key，无自助查询接口 | **中**：需要"客户端 Key 查自己的额度/用量"（对应 README 已有 `X-Session-ID` 体系，只差 per-key 口径） |

> Safuno 已经远超"裸 API"的地方（不用改）：账号池轮询+冷却+恢复节流（`internal/pool`）、额度快照与 Feed、每日护栏、三种协议 usage 统计、会话归档与智能体会话识别、脱敏层、加密账号存储。这些是**成本侧**（管好上游账号），缺的是**收入侧**（卖给谁、卖多少、收多少钱）。

---

## 5. 可参考的功能清单（按优先级，映射到本仓库）

优先级说明：P0 = 不做就没法"做生意"；P1 = 做了才能"让指定 Agent 适配"；P2 = 增值/差异化。

### P0 · 商业化底座（对应 Magpie「共享」「用量和花费」两节）

1. **命名网关 Key（多租户）**——完全复刻 `magpie gateway-key` 语义：
   - `internal/accounts` 旁边新增 `keys` 存储（沿用 AES-256-GCM 文件格式）；
   - 管理台"账号池"页新增"网关 Key"子标签：添加/启用停用/轮换/删除；
   - 请求鉴权从"比对单一 `GATEWAY_API_KEY`"改为"认证 → 解析 Key 身份 → 记到 Key 名下"。
2. **每 Key 限额窗口**——复刻 `gateway-key limit` 的窗口语义（比 Safuno 现有 `guards` 更细，且是客户视角）：
   - 支持天/周/月/任意 N 天/任意 N 小时，**最多 5 个窗口叠加，全部有余量才放行**；
   - 窗口计数从**用量日志**读，重启不丢；请求在窗口里预占，防并发超卖；
   - 拒绝时 `429` 用 OpenAI 错误形态点名 Key、限额、重置时间 + `Retry-After`；
   - 这是 Safuno `guards.go` 的近亲，但守卫对象从"账号"换成"客户 Key"，判定位置在 pool 之前。
3. **价格表与花费计算**——在 `internal/observe` 的 usage 之上加一层：
   - 模型→四参数价格（input/output/cache read/cache write），默认官方价、可自定义、可清零表示免费；
   - `usage` 无对应价格时**不计入总额但明确标注 unpriced**（复刻 magpie：绝不把无价当免费）；
   - 币种美元/人民币可配。
4. **按客户账单与导出**——管理 API 增加按 Key/模型/账号/天 的花费聚合；CSV 导出 + 现有 Prometheus 之外补 OTLP：这与 `internal/sessions` 已有明细天然衔接（`usage` + `reported_fields` 已落盘，只差乘价格）。

### P1 · Agent 适配层（对应 Magpie「任意模型，任意 Agent」「整体结构」两节）

5. **Agent 接入向导（配置生成 + 探测）**：
   - 内置"Agent → 配置文件 → 字段"表（Claude Code `~/.claude/settings.json`、Codex `~/.codex/config.toml`、OpenCode、Cline、DSH 环境变量等）；
   - 管理台"Agent 接入"页：检测本机已装哪些 Agent → 生成对应配置片段 → 备份原文件 → 原子写入（复刻 magpie 的"只改该改的键、保留注释与格式"）；
   - **方案（Profile）**：把"我给哪些 Agent 配了网关、各用什么模型"存成命名快照，一键切回——这是 Safuno 没有的交互，恰恰是"让指定 agent 适配"的日常操作入口。
6. **协议互译网关**（Chat ↔ Responses ↔ Anthropic Messages）：
   - 这是让 Claude Code（Anthropic 协议）和 Codex（Responses 协议）真正"适配"的前提；
   - 注意 Safuno 现有 `internal/observe` 已经能同时解析三种协议的 usage/SSE 终态，说明协议解析基础已有，只差转换层。

### P2 · 增值与差异化（对应 Magpie「路由组」「插件」「还有」三节）

7. **路由可视化**：复刻路由页"每一次决策：谁先谁后、为什么"。Safuno 的 `pool_rejections` 日志已经记录了原因，只差一个实时视图（`/admin/v1/requests` 数据是现成的）。
8. **意图路由**：小模型每轮读新消息，复杂问题交强模型、简单问题交便宜模型（做生意的成本优化卖点）；
9. **网关中间件插件**：magpie 的现成中间件（`param-override`、`model-map`、`system-prompt`、`word-guard`、`think-tags`）对应 New API 的渠道能力且 JSON 格式一致——Safuno 已有 `internal/redact` 的脱敏/工具防护做底子，可以扩成"热插拔中间件"；
10. **每日预热 + 配额窗口路由**：magpie 在窗口重置时刻自动启动每个账号的 5 小时窗口；Safuno 的 WorkBuddy 每日签到/福利任务和 `smart` 思路已经在这条路上（README「每日签到与福利任务」），可补"窗口重置调度"；
11. **资料库/搜索/生图 MCP**：指令、MCP 服务器、技能按每个 Agent 的格式写入——远期增值，不建议现在投入。

---

## 6. 可参考的交互设计（用户能感知的部分）

| 交互 | Magpie 的做法 | 为什么值得抄 | Safuno 对应点 |
|---|---|---|---|
| **模型选择器** | Agents 页每行一个可搜索下拉，`provider/model` 格式，模型列表来自厂商实时刷新（models.dev 补全名称/推理档位） | "选模型"被做成一次点击，而不是改配置；今天发布的模型明天就在列表里 | 管理台"模型目录"已是并集视图，可加"给某 Agent 分配模型"入口 |
| **方案（Profile）** | 所有 Agent 配置快照成名字，一键全切 | "省钱/专注"这种切换是日常决策，快照把它变成单动作 | 无；配合 P1-5 一起做 |
| **路由实时视图** | 路由页实时展示每个请求走哪个账号、为什么 | 把调度从黑盒变成可解释，用户才敢把关键请求交给它 | `pool_rejections` 日志有数据，缺视图 |
| **限额编辑器的状态感** | Key 行显示 used / left / reset；`GET /v1/magpie/limit` 自助查询；429 点名限额与重置时间 | 额度是生意的最小商品，必须让买卖双方都看到"还剩多少、何时重置" | 账号额度快照有类似展示（`source/observed_at/valid_until`），需扩展到 Key 维度 |
| **用量页的"正"数法** | 余额、token、缓存命中率、花费并列；unpriced 明确标注"≈$1.20+，3 次调用未计价" | 诚实计费是信任基础：不编价、不把未知当免费 | 与现有"绝不用 Token 推算余额"的价值取向完全一致，直接沿用 |
| **隐私透明页** | 列出 magpie 自己发出的所有内容（示例+开关）+ `DO_NOT_TRACK=1` | 网关转发全部流量，透明是化解疑虑的标配 | README 已有详细隐私说明，可固化成管理台页面 |
| **多入口** | 菜单栏面板 / TUI / Web / CLI 同一套功能 | 生意对象（管理员、客户）用不同入口 | 目前只有 Web 管理台，CLI 可远期补 |

---

## 7. 落地路线建议（三阶段）

**阶段一（P0，先能卖）**：命名网关 Key + 每 Key 限额窗口 + 价格表/花费 + 按 Key 账单导出。
- 改动集中在 `internal/accounts`（新增 keys 存储）、`internal/gateway`（鉴权与 Key 限额判定，pool 之前）、`internal/observe`（价乘）与管理台 `web/admin`（Keys 页 + 限额编辑器）。
- 交互验收锚点：管理台给客户 A 建 Key、设为"每天 $2 或 50 万 token，仅 `sensenova-6.8-flash-lite`"，客户端超限收到点名额度的 429。
- **这一步做完，Safuno 就从"一个 Key 的 API"变成"能区分客户、能限额、能算账的生意"**。

**阶段二（P1，让指定 Agent 能用）**：Agent 接入向导 + 配置探测/原子改写 + 方案快照；协议互译（Chat↔Responses↔Messages，优先 Messages 桥，因为 Claude Code 是最大客户群）。
- 交互验收锚点：管理台点一下"接入 Claude Code"，本机 `~/.claude/settings.json` 被安全改写，Claude Code 新会话直接走账号池；"方案"一键切到 Codex 用另一组模型。

**阶段三（P2，差异化）**：路由可视化和意图路由、中间件插件、每日预热调度、资料库/搜索 MCP。

---

## 8. 风险与边界（做生意前必须知道）

1. **上游 ToS（最重要）**：仓库 README「边界」自己写着——"上游是商业客户端，逆向与反代违反其服务条款；建议仅自有账号、小流量自用，任何自造指纹都可能被上游 WAF 识别"。把 WorkBuddy 账号池**对外售卖**是把 ToS 风险商品化。**建议**：对外的生意模型走合法货源（SenseNova 官方 API Key 等），WorkBuddy/CodeBuddy 账号池只面向自有账户持有者或极信任客户；任何商业模式上线前请先确认货源合法性。
2. **单进程/单状态文件**：README「验证与运行边界」写明"单状态文件只支持一个进程，暂未实现分布式额度／并发控制"。多客户规模化时，Key 限额计数（参考 Magpie 是从用量日志读取、可重启恢复）要设计成独立持久化，别与账号状态文件耦合。
3. **本地 vs 远程的鉴权心智切换**：Magpie 的"回环宽松、远程必须 Key"（`MAGPIE_ADDR` 语义）值得抄——开放端口的第一天就要决定哪些路径必须 Key，避免把管理 API 暴露出去。
4. **会话/用量口径**：Magpie 计费"只算经过网关的请求、窗口按请求开始时间计、预占约一笔请求"——Safuno 现有"绝不按字符估算 Token、未知不假设为零"的记数原则完全兼容，计费层要继承这个诚实原则。

---

## 附：Magpie 功能 → 本仓库现状一页对照

| Magpie 功能 | Safuno 现状 |
|---|---|
| 任意 Agent 任意模型（配置改写） | ❌ 只提供 base_url 给能填 base_url 的工具 |
| Profile 方案一键切换 | ❌ 无 |
| 协议互译（Chat/Responses/Messages/Gemini） | ◐ 三种协议 usage 可解析；Responses/Messages 只透传不互译 |
| 订阅即 provider、账号发现 | ◐ WorkBuddy 账号收编已做；"收编本机其它 Agent 登录态"未做 |
| 路由组 smart/order/rotate/usage/pace | ○ 账号轮询+冷却+恢复节流已做；客户视角路由/意图路由未做 |
| 网关中间件插件（New API 兼容） | ◐ 脱敏/tool_guard 是弱中间件；热插拔中间件未做 |
| 用量与花费（自定义价格、USD/CNY） | ◐ 精确 token 统计；无价格表/花费 |
| 额度窗口（key/套餐/账号） | ◐ 账号额度快照+Feed+guards；无 Key 级额度 |
| 会话花费与标题、终端重开 | ○ 会话归档/标题/工具耗时已做；无花费、无终端重开 |
| 局域网共享 + 命名 Key + 每 Key 限额 | ❌ 单一网关 Key |
| 远程 magpie / Docker / WebDAV+S3 同步 | ◐ Docker 有（仅回环）；无远程形态、无同步 |
| 每日预热、签到福利 | ○ WorkBuddy 每日签到/活跃上报/402 停放已做 |
| OTLP 导出 | ◐ Prometheus 有；OTLP 导出未做 |

（○ 已有相当基础　◐ 部分已有　❌ 缺失）