# Safuno 产品需求文档（PRD）：个人订阅转 API 网关 · 让本机 Agent 直接适配

| 项 | 内容 |
|---|---|
| 版本 | v1.1（2026-10-19 四项决策定稿） |
| 日期 | 2026-10-19 |
| 产品定位 | **单机、单用户**：把自己的订阅（WorkBuddy / CodeBuddy / SenseNova）统一转成 API，并让本机常用 Agent（DSH、Claude Code、Codex、OpenCode、Cline 等）直接适配使用 |
| 参考对象 | [Magpie（yetone/magpie）README 中文版](https://github.com/yetone/magpie/blob/main/README.zh-CN.md) 及 `docs/reference.md`；分析见 [docs/magpie-analysis-zh.md](docs/magpie-analysis-zh.md) |
| 明确不做 | 对外售卖、多客户、多租户计费、局域网/远程共享、数据同步、订阅收编（把本机其他 Agent 的登录态变成 provider） |

---

## 1. 背景与问题

Safuno 现在是一个"多账号模型 API 代理"（[README.md](README.md)）：账号轮询、冷却、额度观测、会话归档、脱敏、管理台都已具备，并已把 WorkBuddy/CodeBuddy 订阅账号包装成 OpenAI 兼容 API。

当前形态的问题是 **"只提供 API"**：

1. 使用入口是 `base_url + Key`，只对"能自己填 base_url 的工具"友好；Claude Code、Codex 这类默认绑定官方端点的 Agent 无法直接用上账号池。
2. 协议只有 `/v1/chat/completions` 原生透传；`/v1/responses`、Anthropic Messages 不互译（[README.md](README.md)「边界」已自述），而 Claude Code 讲 Anthropic 协议、Codex 讲 Responses。
3. 缺"方案"这类个人日常操作：在不同模型组合（省钱 / 专注）之间切换要手动改配置。
4. 用量有精确 token，但没有"花费/窗口"知情视图，说不清"哪个 Agent 在烧积分、额度窗口何时重置"。

目标：参考 Magpie 的"所有 Agent 的模型，一处搞定"，让本机个人 Agent 直接适配到自己的订阅池，单机运行，不对外。

---

## 2. 产品定位与目标

> 一段话：把自己的 WorkBuddy / CodeBuddy / SenseNova 订阅变成**本机所有 Agent 共用的模型池**；在管理台给每个 Agent 选模型、一键切换方案；额度用完自动切下一个账号；看得见每个 Agent 烧了多少、每个额度窗口还剩多少。

目标指标（验收口径）：

| 目标 | 可衡量标准 |
|---|---|
| 指定 Agent 直接可用 | 本机已装的 Claude Code、Codex、DSH、OpenCode 等，按其原生协议连上网关后 0 改动可用（不必手动改协议/端点） |
| 一处选模型 | 管理台为任意已接入 Agent 改模型 ≤ 2 次点击，改动原子、可一键还原 |
| 用尽不断 | 单账号额度/限流耗尽时，自动切换账号，Agent 无感知（沿用现有 pool 能力，做界面化） |
| 知情 | 能回答"哪个 Agent 消耗多少 token / 花费估算 / 额度窗口何时重置" |

## 3. 非目标（明确不做）

| 不做 | 原因 |
|---|---|
| 对外售卖 / 多客户 / 每客户限额计费 | 上游（WorkBuddy 等）为商业客户端，对外售卖账号池违反其 ToS（[README.md](README.md)「边界」） |
| 局域网共享、远程 magpie、WebDAV/S3 同步 | 单机使用，无多设备需求 |
| 订阅收编（把本机 Claude Code 登录态变成 provider） | 涉及更多上游逆向与 ToS 风险，非自用必需 |
| 每客户账单 / OTLP 导出 | 无对外计费需求（Prometheus 指标保留） |
| 多网关进程 / 分布式状态 | 单机单进程是既定边界（README「验证与运行边界」） |

---

## 4. 功能需求

优先级：**P0** 核心体验（不做就没有"让指定 Agent 适配"）；**P1** 知情与体验（做了才算完整个人工具）；**P2** 增值与舒适。

---

### P0-1　Agent 接入与配置管理（Agents 页）

**描述**：管理台新增"Agents"页——列出本机已安装的 Agent，展示其当前绑定模型与接入状态；为每个 Agent 选择模型、写入/还原配置、接入自检。对应 Magpie「任意模型，任意 Agent」+ Agents 表（`~/.claude/settings.json`、`~/.codex/config.toml` 等）。

**内置 Agent 目录（v1 范围）**：

| Agent | 配置文件/环境 | 网关要写的字段 | 依赖的协议桥 |
|---|---|---|---|
| DeepSeek Harness | `ANTHROPIC_BASE_URL` / `OPENAI_BASE_URL` + `ANTHROPIC_API_KEY` | base_url、api_key | 无需（已支持） |
| OpenCode | `~/.config/opencode/opencode.json(c)` | model | 无需（OpenAI Chat） |
| Cline | VS Code 扩展设置 `cline.*` | apiProvider、apiModelId、baseUrl | 无需（OpenAI Chat） |
| Gemini CLI | ~~`~/.gemini/settings.json` + `.env`~~ | ~~需 Gemini 协议桥~~ **已砍（见第 8 节）** | — |
| Claude Code | `~/.claude/settings.json` | provider、model、env 里的 base_url/api_key | **需 Messages 桥（P0-2）** |
| Codex | `~/.codex/config.toml` | `[model_providers.magpie]` + model | **需 Responses 桥（P0-2）** |

**功能要求**：

1. **本机探测**：读取 Agent 常见配置路径与环境变量，列出"已安装/未安装"；探测失败给出排查提示（沿用现有"检查结果展示成功或失败的时间"的交互风格）。
2. **写入模式：默认自动写入**（已确认）：
   - 自动写入（默认）：先备份原文件 → 只增改必要的键 → **保留注释、顺序、无关字段原样**（对齐 Magpie "原子地改写 Agent 的配置"）→ 写入前展示 diff 预览、可取消 → 提供一键还原到备份；
   - 仅生成（可选项）：管理台展示可直接粘贴的配置片段（带 diff 预览），用户自己写入。
3. **模型选择器**：每个 Agent 一行，点击弹出可搜索列表（`provider/model` 格式，数据来自现有聚合模型目录）；选后保存即生效。
4. **接入自检**：写入后向网关发一个最小请求（`GET /v1/models` + 一个 `max_tokens=8` 的 chat 请求），返回"可用 / 失败原因"，失败不覆盖用户原配置。
5. **配置漂移检测**：检测到 Agent 配置文件被外部工具修改（与网关最后写入内容不一致）时标记"漂移"，提示重新接入或还原；**绝不静默覆盖**。
6. **状态徽标**：未接入 / 已接入 / 配置漂移 / 写入失败。

**验收标准**：
- 对每个内置 Agent：生成片段 → 自动写入 → 自检通过 → 还原，全程不动无关字段与注释；
- 写入前文件被外部改动时，操作被拒绝并提示漂移；
- 还原后文件与写入前逐字节一致。

**涉及模块**：新增 `internal/agents`（Agent 目录、探测、写入/备份/还原引擎）；管理台 `internal/gateway/web` 新增 Agents 页。

---

### P0-2　协议互译网关（让 Claude Code / Codex 原生接入的前提）

**描述**：网关在现有 `/v1/chat/completions` 透传之上，增加两个方向的协议桥，对齐 Magpie "网关同时支持 OpenAI Chat、Responses、Anthropic Messages，并互相转换，流式、工具调用和推理都支持"。

**功能要求**：

1. **Chat ↔ Anthropic Messages 桥**（优先，Claude Code 是最大客户群）：
   - 入站 Messages 请求（`/v1/messages`）→ 转 Chat Completions 发给账号池 → 回复转回 Messages 流式事件（`content_block_start/delta/stop`、`message_stop`）；
   - 工具调用双向转换（Anthropic `tool_use`/`tool_result` ↔ OpenAI `tool_calls`/`tool` role），缓存/推理（`thinking`）保留语义；
2. **Chat ↔ Responses 桥**（Codex 用）：
   - 入站 `/v1/responses` → Chat → 回复转 Responses 事件（`response.output_item.added` 等）；
   - `previous_response_id` 续接继续走现有会话绑定逻辑。
3. **转换不丢账**：`internal/observe` 的三种协议 usage 解析与 TTFT/TPS 观测在转换后依然准确（现状已能解析三种协议，只需保证桥接后归账不变）。
4. **会话识别兼容**：Claude Code / Codex 原生会话头在桥接后仍能被识别并归档（现有 `internal/sessions` 适配表已覆盖，做回归验证）。
5. 桥接为**可选开关**（`config` 内 `bridges` 段，默认关），关闭时保持现有"原样透传"行为，不影响已有 `/v1/*` 用户。

**验收标准**：
- Claude Code 经 Messages 桥完成一次真实对话 + 工具调用 + 流式输出，管理台会话详情记录完整 usage 与 TTFT；
- Codex 经 Responses 桥完成真实对话，`thread_id`/`previous_response_id` 续接正常；
- 桥接关闭时 `/v1/responses`、`/v1/messages` 行为与现在完全一致。

**涉及模块**：`internal/gateway`（新增 bridge 处理器，复用 `internal/provider` 与 `internal/observe`）。

---

### P0-3　方案（Profiles）

**描述**：把"每个 Agent 用什么模型"整套配置存成一个名字（如"省钱""专注"），一键切换/还原。对应 Magpie 「方案（Profile）把所有 Agent 的配置存成一个名字，一键全部切回去」。

**功能要求**：
1. 创建/更新/删除方案；方案内容 = 各已接入 Agent 的模型选择 + 写入模式下的配置文件快照引用；
2. 一键应用：按方案内容批量写入各 Agent 配置（走 P0-1 的备份/还原机制）；应用前预览将发生哪些改动；
3. 当前生效方案标记 ✓；无方案时显示"未套用任何方案"；
4. 方案不复制凭证、**不含模型白名单（已确认）**，只存"每个 Agent 的模型选择 + 配置引用"。

**验收标准**：A、B 两方案间来回切换各 2 轮，每轮切换后各 Agent 自检通过；切换失败（某 Agent 文件被外部改动）时整体中止并提示漂移 Agent，不产生半套配置。

**涉及模块**：`internal/agents`（profiles 存储）+ Agents 页。

---

### P1-1　路由决策可视化（Routing 页）

**描述**：实时/近实时展示每次请求的路由结果——"谁先谁后、为什么"，对应 Magpie 路由页 "实时展示每一次决策"。

**功能要求**：
1. 新增"路由"页：时间线/列表展示最近请求 → 命中的账号、跳过的账号及原因（现有 `pool_rejections` 原因枚举：`model_not_in_token_plan`、`model_cooldown`、`account_cooldown`、`concurrency_limit`、`quota_exhausted`…）；
2. 单请求详情：展示内部每次尝试（账号、状态、耗时、usage）与等待过程（`pool_wait_ms`、正在等待哪个账号冷却、预计恢复时间）；
3. 支持按账号/模型/原因筛选。

**验收标准**：构造一个"首号冷却 + 二号可用"的请求，页面能展示首号被冷却跳过、二号命中的完整链路；等待中的请求能看到"等待中（账号 x 冷却至 …）"。

**涉及模块**：`internal/gateway` 请求流水线已产生所需日志/会话字段，主要做 `internal/gateway/web` 新页面 + 管理 API 查询接口。

---

### P1-2　用量知情页（个人版：token、花费估算、额度窗口）

**描述**：管理台用量页从"每账号 token"升级为"每个 Agent / 每个模型 / 每个额度窗口"的知情视图。对应 Magpie 「用量和花费」——token、花费（美元或人民币）、额度窗口、会话列表。

**功能要求**：

1. **按 Agent 聚合**：基于现有会话识别（`identity.client`：dsh、claude-code、codex、opencode…）与 `identity_sources`，按客户端聚合 token（输入/输出/缓存命中）；无识别的归"其他"。
2. **花费估算（按倍率近似，已确认）**：
   - 价格表：模型 → input/output/cache read/cache write 四参数价格，默认取官方公开价；
   - **WorkBuddy/CodeBuddy：按官方积分倍率近似估算积分消耗**，UI 明确标注"估算"（倍率来自内置模型目录，见 [README.md](README.md)「模型目录」；按倍率近似是对余额的知情参考，不承诺与官方账单分毫不差——"余额能力边界"原则保留：不把 Token 数冒充官方余额）；
   - SenseNova 等无公开换算公式的平台仍**标注"未计价"**，绝不编造；
   - 用户可对任意模型自定义价格覆盖；无价调用不计入总额但标注 unpriced 次数；
   - 展示币种可选（默认人民币）。
3. **额度窗口卡片**：每个账号展示其额度窗口（WorkBuddy 积分套餐/5h 窗口、SenseNova 快照桶）——剩余、上限、**何时重置**；"窗口还剩很多就要重置"时提醒（Magpie「额度」语义）。
4. **会话列表加"花费"列**：现有会话列表增加估算花费与模型列；支持一键在终端重开该会话（生成 `curl`/CLI 片段，对齐 Magpie "一键在终端里重新打开"）。
5. 不改变现有"绝不按字符估算 Token、未知不假设为零"的记账口径。

**验收标准**：同一 DSH 会话跑 3 轮后，用量页能按客户端/模型/账号三种维度各出一张聚合表；对自定义价格模型显示花费，对无价模型显示"未计价"并给出 unpriced 次数；窗口卡片显示剩余与重置时间。

**涉及模块**：`internal/observe`（价格表、花费计算）、`internal/sessions`（会话花费字段）、管理台用量页。

---

### P1-3　限流/等待体验界面化

**描述**：把现有"限流等待与重试"（[README.md](README.md) v0.5.1）从日志变成界面可见。

**功能要求**：
1. 管理台概览/路由页显示当前"正在等待"的请求数与原因分布；
2. 请求详情展示 `Retry-After` 原始值与计算出的恢复时间（现有采样字段已保存）；
3. 客户端 429/503 响应体维持协议原样，但管理台请求详情提供可读的错误分类（现有 `error_layer/reason` 枚举已有）。

**验收标准**：构造 429 场景，请求详情能展示冷却账号、Retry-After、恢复时间与最终换号结果。

---

### P2 增值（可选，按需排期）

| 编号 | 功能 | 说明（参考 Magpie） |
|---|---|---|
| P2-1 | **意图路由** | 小模型读每轮新消息，复杂问题交强模型、简单问题交便宜模型；路由页展示每轮决策 |
| P2-2 | **中间件插件** | model-map（模型别名）、system-prompt、think-tags（去 `<think>`）等，JSON 语义对齐 New API（Magpie 现成中间件） |
| P2-3 | **smart 窗口路由增强** | 账号排序从"轮询"升级为"额度窗口快重置的优先"（Magpie smart/pace），减少窗口重置浪费 |
| P2-4 | **每日预热调度** | 在窗口重置时刻自动启动账号（WorkBuddy 每日福利任务已有雏形，补"5h 窗口重置预热"） |
| P2-5 | **Agent 版本提示** | Agents 页显示各 CLI 当前版本与更新命令（Magpie "Agent 保持最新"） |
| P2-6 | **搜索/生图 MCP**（远期） | 给任意 Agent 一个 `web_search`/生图工具，走本网关模型 |

---

## 5. 交互设计要点

1. **Agents 页为新的第一入口**：一行一个 Agent（logo、状态徽标、当前模型、操作），模型选择器全页唯一组件；写入前统一"预览 diff → 确认 → 备份 → 写入 → 自检"四步弹窗，全程可取消。
2. **"方案"在 Agents 页顶栏**：下拉选择/保存/新建，切换前预览影响面。
3. **用量页卡片化**：Agent 消耗榜（谁烧得多）、额度窗口卡片（剩余/重置倒计时）、未计价提示统一使用现有"未知 → 显示 —"的视觉语言。
4. **路由页时间线**：请求 → 尝试链（账号/状态/耗时/原因），等待中的节点显示"冷却至 HH:MM"。
5. 所有新页面沿用现有管理台安全约束：打开 `/admin/` 自动取得 HttpOnly 同源会话、不落 localStorage（[README.md](README.md)「Web 管理台」）。

## 6. 非功能需求

| 类别 | 要求 |
|---|---|
| 安全 | 改写 Agent 配置前必须备份且失败回滚；探测只读不写；凭证仍走 AES-256-GCM 加密存储，不新增明文落盘 |
| 兼容 | 桥接默认关闭；关闭时 `/v1/*` 行为与现状逐字节一致；新增功能不影响纯 API 用法 |
| 可靠性 | Agents/Profiles 状态与账号/会话一样文件持久化，异常退出后重启可恢复；写入中断不留半套配置 |
| 性能 | 单机个人负载（≤ 几十请求/分钟），协议桥延迟增加应 < 5ms（纯转换，无额外模型调用） |
| 可观测 | 新功能均输出 JSON 行日志与 Prometheus 指标，沿用现有 `request_id` 关联 |

## 7. 里程碑

| 里程碑 | 内容 | 出口条件 |
|---|---|---|
| **M1（P0 接入）** | Agents 页 + Agent 目录（DSH/OpenCode/Cline）+ 写入/还原 + 模型选择器 + 接入自检 | 3 个 Agent 完成接入→选模型→还原全流程，漂移检测生效 |
| **M2（P0 协议 + 方案）** | Messages 桥（Claude Code）、Responses 桥（Codex）、Profiles | Claude Code/Codex 真实对话 + 工具调用 + 会话归账正确；方案往返切换 2 轮通过 |
| **M3（P1 知情）** | 路由可视化、用量知情页（Agent 聚合/花费/窗口）、等待体验界面化 | P1-1/2/3 验收标准全过 |
| **M4（P2 增值，可选）** | 意图路由、中间件、smart 窗口路由、Agent 版本提示、搜索/生图 MCP 按需挑选 | 对应功能验收 |

## 8. 已确认的产品决策（2026-10-19）

| # | 决策 | 影响范围 |
|---|---|---|
| 1 | 配置写入**默认自动写入**（备份 → diff 预览 → 写入 → 一键还原），"仅生成"保留为可选项 | P0-1（已更新） |
| 2 | 花费估算接受 **WorkBuddy/CodeBuddy 按官方积分倍率近似估算**，UI 标注"估算"；SenseNova 等无公开公式仍标"未计价" | P1-2（已更新） |
| 3 | **去掉 Gemini 协议桥**，Gemini CLI 移出内置 Agent 目录 | P0-1 Agent 目录、P2 清单（已更新） |
| 4 | **Profiles 不需要模型白名单**，纯"每个 Agent 的模型选择"快照 | P0-3（已更新） |
