# SenseNova 接口分析

核查时间：2026-10-17。主要来源为浏览器中实际加载的 [SenseNova 官方文档](https://platform.sensenova.cn/docs)，补充核对 [官方 6.8 API 接入文档](https://github.com/OpenSenseNova/SenseNova6.8/blob/main/API_CN.md)。平台文档是动态页面，普通网页抓取超时后通过浏览器读取。

| 项目 | 核查结果 | 本项目处理 |
|---|---|---|
| Base URL | `https://token.sensenova.cn/v1` | Provider 绑定源站，透传 `/v1/*` |
| 鉴权 | `Authorization: Bearer sk-...` | 独立网关 Key，选号后替换上游凭证 |
| 对话 | `POST /v1/chat/completions` | 透传 JSON、工具调用和 SSE |
| Responses | `POST /v1/responses` | 保留原协议，观测终态事件中的 usage |
| Anthropic | `POST /v1/messages` | 保留原协议，合并开始／增量事件的 usage |
| 图片 | `POST /v1/images/generations`、`POST /v1/images/edits` | 透传 JSON 和 multipart |
| 模型 | `GET /v1/models` | 逐号发现、保留元数据、按 ID 去重 |
| 额度 | 两种积分池、滚动 5 小时／周约束；账户页面展示余额 | 以积分桶存储；没有公开余额 API 时返回未知 |
| 错误 | 429 同时可能表示速率或额度限制 | 冷却，不将每次 429 永久判定为余额归零 |

文档还说明 Responses 当前为无状态兼容，不支持存储、续写 ID、响应查询／删除等有状态能力。代理只转发调用，不补造上游未支持的接口。

模型应以运行时发现结果为准。第一版不固定模型清单，也不将公开模型目录等同于某号一定有权限。平台额度是积分，响应 usage 是 Token，公开材料不足以推导精确的剩余 Token 数。因此额度查询保留独立接口边界，通过人工快照或可信外部 Feed 接入；是否能自动取得官方余额仍需平台提供正式接口或由用户提供受支持的数据源。

2026-10-17 真实联调补充：此账号的模型列表同时返回了 `deepseek-flash` 和 `deepseek-v4.1-flash`，但后者调用实际返回 403、`permission_denied_error` 和当前 Token Plan 不可用的提示。因此发现列表只能作为候选目录，需结合调用结果过滤权限。模型引擎 TPM/RPM 限流的 429 可能同时使用 `insufficient_quota` 或 `RateLimitExceeded.EndpointTPMExceeded` 编码，不能只凭 `insufficient_quota` 判定账号余额耗尽。当前实现仅对明确的 `rate_limit_error` 和 `inference exceeds tpm/rpm limit` 组合采用模型级冷却，其他 429 保持账号级冷却。

2026-10-21 补充：该 403 只否定被命中的账号，不否定同一路由组的其他候选。已知生成接口（Chat Completions / Responses / Messages / 图像）收到该 403 后，网关会记录该号对应模型不可用并继续换号，直到某个候选成功或全部候选都被排除；全部排除后才向客户端返回 `403 model_not_in_token_plan`。该 403 未产生生成结果，重放安全，因此不占用 `max_attempts` 生成重试预算。实测同组 WorkBuddy 账号可正常提供 `deepseek-v4.1-flash`（仅支持流式，且首条消息必须是 system prompt），因此四个 SenseNova 账号都被排除后，同一模型请求会落到 WorkBuddy 并成功。

后续接入新平台时，保持客户端入口与鉴权不变，通过 Provider 实现该平台的鉴权、模型和额度查询。平台之间的协议差异需由对应适配器明确处理，不能仅更换 URL 后假设全部兼容。
