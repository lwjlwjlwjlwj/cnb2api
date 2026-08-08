# cnb2api

> CNB (`cnb.cool`) NPC 聊天接口的 OpenAI 兼容反向代理，Go 实现。

逆向自 `cnb.cool` 前端 `_app.js` 的 NPC 聊天接口，将其封装为标准 OpenAI 兼容 API，
免登录即可调用 `npc/CodeBuddy(deepseek-v4-flash)` 等 CNB NPC。

## 功能特性

- 🔓 **免登录** — 自动从 CNB 首页获取 CSRF 凭证（`csrfkey` cookie + `csrftoken` header 配对），无需账号即可调用
- 🤖 **双模型支持** — `deepseek-v4-flash` + `deepseek-v4-pro`（均透传至 CNB 上游）
- 🔄 **弹性凭证池** — 并发获取多个独立会话凭证，round-robin 轮转，天然支持并发请求
- 🔧 **自动维护** — 凭证过期自动淘汰、补充、健康检查、连续失败自动失效
- 📡 **SSE 流式** — 流式透传上游 SSE；非流式自动聚合 `content` + `reasoning_content`
- 🎭 **多协议支持** — OpenAI Chat Completions + **Anthropic Messages**（`/v1/messages`、`/anthropic/v1/messages`）+ **OpenAI Responses**（`/v1/responses`，typed SSE events）
- 🔑 **可选鉴权** — 配置 `api_key` 后需 Bearer token 访问
- 🏗 **Go 单二进制** — 无外部依赖，`go build` 即得

> ⚠️ **原生工具调用受限** — CNB 上游禁止原生 `tools` 参数（403 `Agent calls are not allowed`）。
> 客户端声明的工具会被透传，但模型返回的 `tool_calls` 不经过解析/执行/桥接。
>
> ✅ **推荐方案：搭配 ToolForge 中间件**（见下方 [Docker 编排](#docker-编排toolforge--cnb2api)）— 通过 XYML 提示词注入实现完整工具调用支持。

## 快速开始

### 1. 构建 & 配置

```bash
git clone https://github.com/lwjlwjlwjlwj/cnb2api.git
cd cnb2api
go build -o cnb2api ./cmd/server
cp config.example.json config.json
# 编辑 config.json，设置 api_key（可留空 = 不鉴权）
```

### 2. 启动服务

```bash
./cnb2api -config config.json
```

或直接用环境变量（无需配置文件）：

```bash
CNB2API_LISTEN=:7863 CNB2API_MODEL=deepseek-v4-flash ./cnb2api
```

### 3. 验证

```bash
# 健康检查
curl -s http://localhost:7863/healthz

# 模型列表
curl -s http://localhost:7863/v1/models -H "Authorization: Bearer your-api-key"

# 聊天（非流式）
curl -s http://localhost:7863/v1/chat/completions \
  -H "Authorization: Bearer your-api-key" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"你好"}]}'

# 聊天（流式）
curl -N http://localhost:7863/v1/chat/completions \
  -H "Authorization: Bearer your-api-key" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek-v4-flash","stream":true,"messages":[{"role":"user","content":"数到3"}]}'

# 凭证池状态
curl -s http://localhost:7863/pool
```

## 配置说明

```json
{
  "listen": ":7863",
  "api_key": "your-api-key",
  "model": "deepseek-v4-flash",
  "models": ["deepseek-v4-flash", "deepseek-v4-pro"],
  "pool_min": 2,
  "pool_max": 8,
  "ttl_minutes": 30
}
```

| 字段 | 环境变量 | 默认值 | 说明 |
|---|---|---|---|
| `listen` | `CNB2API_LISTEN` | `:7863` | 监听地址 |
| `api_key` | `CNB2API_API_KEY` | 空 | API 鉴权 key（空=不鉴权） |
| `model` | `CNB2API_MODEL` | `deepseek-v4-flash` | 默认模型 |
| `models` | — | `[flash, pro]` | 支持的模型列表 |
| `pool_min` | `CNB2API_POOL_MIN` | `2` | 凭证池最小凭证数 |
| `pool_max` | `CNB2API_POOL_MAX` | `8` | 凭证池最大凭证数（并发上限） |
| `ttl_minutes` | `CNB2API_TTL_MINUTES` | `30` | 凭证有效期（分钟） |

### 模型说明

| 模型 | 说明 |
|---|---|
| `deepseek-v4-flash` | 默认模型 |
| `deepseek-v4-pro` | 已支持，但**上游实际仍调用 flash**（CNB 上游仅暴露 flash 接口，pro 为前端映射），输出行为与 flash 有差异（如写诗风格） |

## API

### `POST /v1/chat/completions`

OpenAI 兼容。支持 `stream`（SSE）、`max_tokens`、`temperature`、`top_p`。

### `GET /v1/models`

返回配置的模型。

### `GET /pool`

查看凭证池状态（每个凭证的 csrfkey、token 前缀、有效期、错误计数）。

### `GET /healthz`

健康检查。

## 鉴权机制（逆向说明）

CNB 的 NPC 聊天接口 `POST /ai/chat/completions` 采用 CSRF 双因子校验：

1. `GET https://cnb.cool/` 首页：
   - 响应 `Set-Cookie: csrfkey=<32位hex>`（HTTPOnly）
   - HTML 内嵌 `<script id="cnb-csrftoken-script">window.csrftoken="<40位hex>"</script>`
2. 调用 chat 接口需同时携带：
   - `Cookie: csrfkey=<csrfkey>`
   - `Header: Csrftoken: <csrftoken>`

两者必须配对（同一会话签发的）。缺失其一或值不匹配 → `401 {"errcode":16,"errmsg":"CSRF 校验失败"}`。

本项目的 `internal/auth` 包每次用独立 cookie jar 建立新会话获取配对凭证，
多个凭证组成池供并发请求轮转使用。

## Docker 编排（ToolForge + cnb2api）

[**ToolForge**](https://github.com/YuJunZhiXue/toolforge) 是通用 LLM 工具调用中间件（原生 FC 透传 + XYML 提示词回退），
开源仓库：<https://github.com/YuJunZhiXue/toolforge>

一键启动完整链路：**客户端 → ToolForge（XYML 工具调用中间件）→ cnb2api → CNB**。
ToolForge 作为前置，通过提示词注入（XYML）实现 CNB 不原生支持的工具调用。

```
┌────────┐   tools请求   ┌────────────┐   XYML注入   ┌───────────┐    ┌─────┐
│ 客户端  │ ───────────▶ │  ToolForge  │ ──────────▶ │  cnb2api  │ ──▶ │ CNB │
│        │ ◀─────────── │  (:18080)   │ ◀────────── │  (:7863)  │ ◀── │     │
└────────┘   tool_calls  └────────────┘   标准响应   └───────────┘    └─────┘
```

### 启动

```bash
# 拉取子模块（ToolForge 源码）
git submodule update --init --recursive

# 编辑配置：docker/config.yaml 中的 allowed_keys（客户端访问 key）
# 和 api_key（cnb2api 的鉴权 key，与 config.example.json 一致）

# 一键启动
 docker compose up -d --build
```

### 服务端口

| 服务 | 端口 | 说明 |
|------|------|------|
| ToolForge | `18080` | OpenAI 兼容入口（带 tools 支持） |
| cnb2api | `7863` | 内部网关（仅容器网络内访问） |

客户端接入：`base_url: http://<host>:18080/v1`，`api_key: <docker/config.yaml 的 allowed_keys>`。

### 说明

- ToolForge 以 git submodule 引入（`docker/toolforge` → `YuJunZhiXue/toolforge`）
- 两服务共享 `cnb2api-net` 网络，ToolForge 通过容器名 `cnb2api:7863` 访问网关
- 支持非流式 + 流式工具调用（标准 OpenAI `tool_calls` 格式）
- 国内网络环境：Dockerfile 已使用清华 pip 镜像源，避免拉取超时

## 目录结构

```
cnb2api/
├── cmd/server/main.go            # 入口：配置、凭证池初始化、HTTP 服务
├── internal/auth/csrf.go         # CSRF 凭证获取 + 凭证池（核心）
├── internal/upstream/client.go   # 上游请求构造 + SSE 读取
├── internal/server/handler.go    # OpenAI 兼容 HTTP handler
├── internal/server/anthropic.go  # Anthropic Messages 适配
├── internal/server/responses.go  # OpenAI Responses 适配
├── config.example.json
└── go.mod
```

## 免责声明

本项目仅供学习和研究使用。请遵守 CNB 平台服务条款，自行承担使用风险。作者不对任何因使用本项目产生的直接或间接损失负责。

## License

MIT
