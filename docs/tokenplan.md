# Intern-ai / TokenPlan（书生·端砚）接入分析

> 管理台显示名：**Intern-ai**；配置与 API 里的 `provider` 值仍是 `tokenplan`（稳定内部 id，不随展示名变化）。

平台入口：[discovery.intern-ai.org.cn/token-plan](https://discovery.intern-ai.org.cn/token-plan/home) · 文档中心：[docs.intern-ai.org.cn](https://discovery.intern-ai.org.cn/docs/)

## 1. 平台定位

Intern-ai 把 Intern 系列自研模型与若干第三方模型（DeepSeek、GLM、Kimi、MiniMax、Qwen）打包成**统一额度**对外提供，计量单位是**墨点**而不是 Token。同一把 API Key 即可调用全部已开通模型，峰值并发受 RPM / TPM 与 5 小时、7 天两个窗口的墨点上限共同约束。

## 2. 接入边界

| 维度 | 结论 |
| --- | --- |
| 源站 | `https://discovery-api.intern-ai.org.cn`（**只填源站，不含 `/v1`**） |
| 鉴权 | `Authorization: Bearer <API Key>`；Anthropic 路径也接受 `x-api-key` + `anthropic-version` |
| 凭证有效期 | 6 个月，需自行轮换；单人最多 10 把 Key |
| 协议 | OpenAI Chat Completions / Responses、Anthropic Messages、SystemOne 决策接口 |
| 额度查询 | **不可用**：墨点余额只在登录态控制台可见，API Key 没有余额端点 |
| 模型无权限 | `model_not_available` 返回 **404**（不是 403），因此网关不会自动把它降级为「该号无此模型」 |

## 3. 端点一览

| 端点 | 方法 | 说明 |
| --- | --- | --- |
| `/v1/chat/completions` | POST | OpenAI 对话补全，主流用法 |
| `/v1/responses` | POST | Responses API；**无状态**，`previous_response_id` 会 404 |
| `/v1/messages` | POST | Anthropic Messages 协议 |
| `/v1/models` | GET | 模型发现（见下） |
| `/v1/systemone` | POST | 结构化决策接口，仅决策模型可用 |

## 4. 模型发现

`GET /v1/models` 一个响应里携带两段模型列表：

```json
{
  "object": "list",
  "data": [{"id": "deepseek-v4-flash-0731", "object": "model"}],
  "models": [{"name": "intern-decision-4b", "description": "…", "release_date": "…"}]
}
```

- `data[].id`：生成模型，可用于 Chat / Responses / Messages。
- `models[].name`：决策模型，**只能**走 `/v1/systemone`，上下文 8192、不支持流式。

网关把两段合并去重，决策模型额外标注 `systemone_only: true`。生成模型典型清单：`intern-s2`、`Atria-Dawn`、`Agents-A1`、`deepseek-v4-flash-0731`、`deepseek-v4-flash-vision`、`deepseek-v4-pro-0813`、`glm-5.3`、`kimi-k2.6`、`minimax-m3`、`qwen3.8-27b`。

> 模型出现在目录里 ≠ 每个端点都能用；以实际调用结果为准。

## 5. 限流与错误

| 限制 | 数值 |
| --- | --- |
| 生成 RPM | 50 / 用户 |
| 决策 RPM | 200 / 用户 |
| TPM | 2,000,000 / 用户 |

RPM / TPM 按**用户**统计并由该用户所有 Key 共享，因此：

- 429 `rate_limit_exceeded` 可重试 → 网关按 `Retry-After` 冷却整个账号（不做单模型隔离，因为限制本来就是账号级）。
- 429 `quota_exceeded` 墨点耗尽，不可重试。
- 上游不返回 `x-ratelimit-*` 响应头，网关不会伪造剩余量。
- 403 一族（`api_key_disabled`、`user_disabled`、`whitelist_required`）是账号级错误，不映射为单模型无权限。
- Anthropic 路径的错误体没有 `error.code` 字段，只有 `error.type` / `message`。

## 6. 额度观测

墨点与 Token 之间没有公开的换算比例（不同模型倍率不同），而且余额接口位于登录态 Web 侧（`/api/tokenplan/v1/credits/balance`），API Key 无法访问。所以 provider 的 `Quota` 返回 `provider.ErrQuotaUnsupported`：

- 账号面板显示「真实余额暂不可查询」，而不是 0；
- 需要精确额度时，用管理台的「更新额度快照」手动上报，或配置可信的 `quota_feed_url`。

## 7. 实现位置

- `internal/provider/tokenplan.go`：`Name / Authorize / Models / Quota / ModelRateLimit / ModelUnavailable`
- `internal/config/providers.go`：`RegisteredProviders` 与默认源站
- `internal/gateway/server.go`：`reg.Register("tokenplan", provider.TokenPlan{})`
- `internal/gateway/platforms.go`：管理台展示元数据（显示名 `Intern-ai`、logo、凭证文案、默认源站）

## 8. 已知限制

- Key 6 个月到期，需换 Key，不考虑自动续期。
- `/v1/responses` 无状态，长对话需客户端自行拼上下文。
- 不支持 reasoning effort 一类参数透传差异（网关原样透传，由上游决定是否接受）。
- 图片输入限制：最多 8 张、单张 ≤ 10 MB、合计 ≤ 40 MB、单边 ≤ 8640×3840。

## 9. 结论

Intern-ai 是**纯 API Key + OpenAI/Anthropic 双协议**的平台，接入成本最低的一类：不需要 OAuth、不需要设备指纹、不需要改协议。真正需要额外处理的只有两点——模型发现要合并两段列表，以及额度只能靠人工/外部 Feed 观测。这也再次说明：**换平台不等于换 URL**，模型发现、限流语义和额度可得性都是平台特有的。
