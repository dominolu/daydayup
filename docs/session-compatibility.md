# 智能体会话兼容性

核对日期：2026-10-18。网关版本：0.5.0。

网关读取模型 HTTP 请求中的标识，不依赖客户端内部状态文件。当前按协议适配，不安装或修改用户的智能体。单个客户端的不同适配器、版本、代理配置可能发送不同字段，因此界面始终展示每条请求实际采用的来源。

| 规则 | 一手依据及限制 |
| --- | --- |
| Claude Code 原生会话头 | [官方网关兼容指南](https://code.claude.com/docs/en/llm-gateway-protocol)定义 `x-claude-code-session-id`；用户、agent、parent-agent 标识不是会话替代品。 |
| Codex thread 与 session 头 | [官方请求头实现](https://github.com/openai/codex/blob/main/codex-rs/codex-api/src/requests/headers.rs)发送 `thread-id` 和 `session-id`；[客户端实现](https://github.com/openai/codex/blob/main/codex-rs/core/src/client.rs)说明 session 可能用于缓存亲和。本服务因此优先 thread，兼容旧下划线字段。固定自定义头的配置参考 [OpenAI 官方高级配置](https://developers.openai.com/codex/config-advanced)，但不要把固定头当成自动生成的逐会话标识。 |
| OpenCode | [官方 Go 接入说明](https://github.com/anomalyco/opencode/blob/dev/packages/web/src/content/docs/go.mdx)要求每个对话发送稳定的 `x-opencode-session`。其他客户端被其接入列表列出，不等于在任何上游都默认发送这个头。 |
| DSH 原生标识 | [官方 wire extensions](https://github.com/deepseek-ai/deepseek-harness/blob/master/docs/deepseek-llm-api-wire-extensions.md)定义 session 头和 `dsh_session_log.session.id`，并明确 `llm-pi-ai` 不实现这些扩展。匿名用户 ID 标识安装环境，不能用于归并对话。 |
| Pi 兼容会话亲和 | [上游 Chat Completions 适配器源码](https://github.com/earendil-works/pi/blob/main/packages/ai/src/api/openai-completions.ts)根据兼容选项和目标平台选择发送会话头；不能假设自定义 Base URL 默认开启。本机 DSH 0.2.0-rc.2 内嵌 pi-ai 0.87.1 的自定义 `ddn` 路径已确认默认不开启。 |
| 通用协议 | 自建客户端优先持续传入 `X-Session-ID`。`metadata.session_id` 及结构化 `metadata.user_id` 中明确命名的 `session_id` 也可识别；普通 `metadata.user_id` 不识别为会话。 |

工具链续接仅在已成功完整归档的响应实际产生相应工具调用，并且后续请求的前序输入历史完整匹配时启用。历史通过规范 JSON（忽略对象字段顺序与空白，保留数组顺序）计算 SHA-256，工具结果必须紧接对应 assistant/tool-use/function-call。跨平台、跨协议、重复 ID 对应不同会话、缺少记录、流中断或历史改变都不直接续接。

这些能力已经通过本地模拟 HTTP 上游验证：各原生别名、统一头优先、Codex thread 优先、命名空间隔离、DSH 两轮工具续接、重启恢复、账号粘性、Token 累加、歧义隔离、来源归档及管理鉴权。未宣称实际运行过所有客户端或支持所有上游协议；SenseNova 当前是否接受某条模型请求由真实上游决定。

## 标题请求与会话名

标题识别不假设所有客户端都有相同的辅助请求。当前规则依据如下：

| 来源 | 已核对内容 |
| --- | --- |
| 本机 DSH 0.2.0-rc.2 | `dsh-session-title-llm` 使用独立系统指令和带 `seq`／`text` 的用户消息 JSON 数组，并传入内部 `purpose=session-title`；适配器可能未把用途传到 HTTP，因此同时识别请求格式。 |
| OpenCode v1.18.32 | [官方标题代理提示词](https://github.com/anomalyco/opencode/blob/v1.18.32/packages/opencode/src/agent/prompt/title.txt)明确要求仅返回线程标题；[主调用实现](https://github.com/anomalyco/opencode/blob/v1.18.32/packages/opencode/src/session/prompt.ts)在标题请求中加入引导消息和原对话输入。 |
| 本机 Claude Code | 已检查安装的官方可执行文件，标题模块使用专用系统提示词、`<session>` 数据和 JSON `title` 输出；截断的上下文无法作为无标识关联的完整证据，优先使用原生会话头。 |
| 其他兼容客户端 | 支持专用的会话标题系统指令及显式 `X-Request-Purpose`／`metadata.purpose`；自定义或版本变化导致格式未匹配时，可明确标记用途。只看到用户要求起标题时不视为管理命名请求。 |

原始用户消息按顺序计算完整文本哈希，最多 256 条、单条 1 MiB；忽略首尾空白及 CRLF 差异，不使用前几个字或相似度。无标识时必须唯一匹配同平台的会话；标题先到的临时会话可供后到且无标识的主请求接续。图像等非文本上下文不降级成纯文字进行匹配。多个候选保留为待关联。

自动标题的元数据先保存请求证据，再原子更新名称，最后保存采用状态；中断后的 pending 状态可在重启时恢复。手动名称和更晚发起的自动命名优先。总用量包含标题请求；分类 `title_usage` 用于解释这部分消耗，不再加到总量中。网关不会为了命名新增模型请求，也不会改变客户端收到的模型响应。
