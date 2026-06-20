# Gaitwaie, a Multi-Tenant AI Router Gateway & CLI Tool (Go)

## 项目简介

这是一个专为个人 Power User 打造的极简、高能、多租户（Receiver）隔离的专属 OpenAI 兼容格式路由网关。项目放弃繁重的 WebUI，采用 **Go 原生网络库 + SQLite + CLI 命令 + YAML 配置 + systemd 服务** 的硬核架构。

静态配置固化在 YAML 中，动态状态（Key 熔断）与数据统计持久化在 SQLite 中，SQLite 同时充当 CLI 和后台服务的通信媒介（免去复杂 IPC）。

---

## 1. 核心设计原则

1. **零外部 Web 框架依赖：** 使用 Go 1.22+ 自带的增强版 `net/http`（支持 `POST /v1/...` 路由语法），追求极致轻量和单二进制文件部署。
2. **极速流式转发：** 对 `/v1/chat/completions` 的流式（Stream）请求不进行全局 JSON 解包，通过按行扫描（Bufio Scanner）透传 SSE 字节流，仅在流尾通过高效匹配抓取 Token 计数。
3. **分离「静态配置」与「动态状态」：** 网关绝不反向修改用户的 YAML 文件。所有 Key 的禁用/启用状态、失败计数全部记录在 SQLite 中。
4. **无缝路由映射：** 客户端请求的模型 ID 格式为 `{provider_alias}/{upstream_model_name}`（例如 `ds/deepseek-chat`），网关自动解析别名、剥离前缀并分发给对应 Provider。

---

## 2. 配置文件设计 (`config.yaml`)

```yaml
database_path: "./gateway.db"     # 可选，若为空则默认与此 yaml 同目录
listen_addr: ":8080"               # 可选，HTTP 监听地址，默认 :8080
tolerance: 3                       # 连续失败几次后自动在 SQLite 中标记为禁用
max_concurrent_tasks: 5            # purge 测试时的最大并发连接数

# 供应方（真正提供算力的 AI 平台）
providers:
  ds:
    base_url: "https://api.deepseek.com/v1"
    keys:
      - "sk-ds-main-xxxx"
      - "sk-ds-backup-yyyy"
  mimo:
    base_url: "https://api.mimo.com/v1"
    keys:
      - "tk-mimo-zzzz"

# 接收方（合法的客户端 Bearer Token，格式为 名字: Key）
receivers:
  user1: "sk-client-11111"
  user2: "sk-client-22222"

```

---

## 3. 数据库表结构（SQLite）

### `request_logs` (请求日志表)

```sql
CREATE TABLE IF NOT EXISTS request_logs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
    status_code INTEGER,              -- 200, 404, 500 等
    prompt_tokens INTEGER,
    completion_tokens INTEGER,
    total_tokens INTEGER,
    provider_alias TEXT,              -- 对应 provider 别名，如 "ds"
    requested_model TEXT,             -- 客户端请求的完整模型名，如 "ds/deepseek-chat"
    assigned_key TEXT,                -- 最终分发出去的完整上游 API Key
    receiver_name TEXT,               -- 发起请求的 Receiver 名字，如 "user1"
    receiver_key TEXT,                -- 发起请求的 Receiver 的完整 Bearer Key
    is_test_request INTEGER DEFAULT 0 -- 是否为 purge 测试发起的请求（1=是, 0=否）
);

```

### `key_states` (Key 状态表)

```sql
CREATE TABLE IF NOT EXISTS key_states (
    key_value TEXT PRIMARY KEY,   -- 完整的上游 API Key
    provider_alias TEXT,
    fail_count INTEGER DEFAULT 0, -- 连续失败次数
    is_active INTEGER DEFAULT 1,  -- 1=可用, 0=被自动剔除/手动关闭
    cool_down_until DATETIME,     -- 熔断后可选的冷却截止时间，到期自动恢复
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

```

---

## 4. 路由与核心业务逻辑

### 鉴权中间件

* 除了 `GET /v1/models` 免鉴权外，其余所有请求必须校验 HTTP Header 中的 `Authorization: Bearer <key>`。
* 匹配失败直接返回标准 OpenAI 错误格式（401 Unauthorized），通过则放行。
* 鉴权通过后，记录 `receiver_name` 和 `receiver_key`，随请求上下文传入后续流程，用于记录日志。

### `GET /v1/models` (启动时异步聚合)

* **不要**在收到用户请求时动态查询上游。
* **流程：** 程序启动时，异步遍历 YAML 中的 `providers`。对每个 provider，轮询其下的 keys 发送上游的 `/v1/models` 请求。
* **Fallback 逻辑：** 如果某个 Key 失败，顺延尝试下一个。若该 Provider 下所有 Key 均失败，则本次启动不聚合该 Provider 的模型。
* **重命名并缓存：** 成功后，将上游模型 ID 改写为 `{provider_alias}/{upstream_model_name}`，并存入**内存全局变量**中。用户请求时直接秒回内存中的 JSON。
* **缓存刷新：** 当 `purge` 命令导致 Key 被禁用或熔断触发时，模型列表缓存**不自动刷新**（模型列表在启动时聚合即可，Key 熔断不影响已聚合的模型列表）。

### `POST /v1/chat/completions` (核心转发)

1. **注入选项：** 为确保流式传输能稳定拿到 Token 计数，网关在将请求体（JSON）转发给上游前，必须在 Body 中注入 `"stream_options": {"include_usage": true}`。

2. **Key 路由与轮询：** 解析请求的 Model 字段获取 `provider_alias`，去 SQLite 的 `key_states` 中筛选出该 provider 下所有 `is_active = 1` 的 Key，采用 Round-Robin（轮询）或随机挑一个使用。
   * **并发安全：** Round-Robin 计数器使用 `sync/atomic` 包实现，确保多协程并发安全。
   * 若当前 Provider 无任何可用 Key，向客户端返回标准 OpenAI 错误 JSON（502 状态码）。

3. **Stream Token 提取（核心修正）：**
   * **不要**同时使用 `io.Copy` 和 `bufio.Scanner` 读同一流 —— 这样无法工作（一个消耗了数据，另一个就空了）。
   * **正确做法：** 只用 `bufio.Scanner` 逐行读取上游 SSE 响应。对每一行：
     a. 写入客户端 `ResponseWriter` 并调用 `flusher.Flush()` 以透传。
     b. 同时检测该行是否包含 `"usage":` 字样（取**最后一次出现**的行）。
   * 流结束后，从最后匹配的行中用正则或微型结构体反序列化抠出 `prompt_tokens` 和 `completion_tokens`。
   * 将 Token 计数 + **当前鉴权通过的 receiver_name / receiver_key** 一起写入 `request_logs`。

4. **熔断状态机（Tolerance）：**

   | 上游响应 | 行为 |
   |---|---|
   | `200` | 该 Key 的 `fail_count` 立刻清零 |
   | `429` (Too Many Requests) | `fail_count` 不变，**不计数**（限流非 Key 不可用） |
   | `401` / `403` | **直接熔断**：将 `is_active` 置为 `0`（Key 已失效，无需计数） |
   | `5xx` / 网络超时 / 连接错误 | `fail_count++`。若 `fail_count >= tolerance`，将 `is_active` 置为 `0` |
   | 其他 4xx | `fail_count++`。达到上限则熔断 |

   * **可选自动恢复：** 当 Key 被熔断时，可设置 `cool_down_until = now + 5min`。后台协程定期扫描 `key_states`，将 `cool_down_until < now` 且 `is_active = 0` 的 Key 恢复为 `is_active = 1`，`fail_count` 清零。这可以应对瞬时的上游故障。

---

## 5. CLI 命令设计

使用 `[github.com/spf13/cobra](https://github.com/spf13/cobra)` 构建命令行，编译出的 Binary 包含以下核心命令：

### A. 后台服务启动

* `.`**`/gateway start --config ./config.yaml`**
（由 systemd 守护进程调用的主服务启动命令）
* 可通过 `--listen` 参数覆盖配置文件的监听地址：**`/gateway start --listen 0.0.0.0:9090`**

### B. 数据统计 (`stat`)

仅统计 SQLite 中 `status_code = 200` 的成功请求（排除 `is_test_request = 1` 的测试请求）。支持时间参数或无限定（全部）。

* `.`**`/gateway stat`**

输出示例：
```text
user1: 
  requests: 123
  prompt: 10000 (10k)
  completion: 1000000 (1M)
  total: 1010000 (1.01M)
user2:
  requests: 45
  prompt: 1000 (1k)
  completion: 10000 (10k)
  total: 11000 (11k)
--- total ---
  requests: 168
  prompt: 11000 (11k)
  completion: 1010000 (1.01M)
  total: 1021000 (1.02M)

```

* `.`**`/gateway stat 1h/7d/1M/...`** （从1h前/7天前，一个月前等到现在的用量，这个不是写死的时间，用户可以输入任意数和单位，包括秒分钟小时天月年）

输出示例：
```text
user1: 
  requests: 10
  prompt: 1000 (1k)
  completion: 10000 (10k)
  total: 11000 (11k)
user2:
  requests: 5
  prompt: 200 (200)
  completion: 3000 (3k)
  total: 3200 (3.2k)
--- total ---
  requests: 15
  prompt: 1200 (1.2k)
  completion: 13000 (13k)
  total: 14200 (14.2k)

```

### C. 手动净化 (`purge`)

* `.`**`/gateway purge --config ./config.yaml`**

**逻辑：** 独立于后台服务运行。相比旧方案，本方案引入**交互式模型选择**，因为测试时必须知道用哪个模型发请求。

**完整交互流程：**

1. CLI 读取 YAML 中所有 provider 列表。
2. **对每个 provider**，从内存模型列表（`GET /v1/models` 缓存的同名格式）中获取该 provider 下所有可用模型，在终端列出让用户选择：

   ```
   Provider [ds] - 请选择测试用的模型：
   1. ds/deepseek-chat
   2. ds/deepseek-reasoner
   Enter number (default 1): 2
   
   Provider [mimo] - 请选择测试用的模型：
   1. mimo/gpt-4o-mini
   Enter number (default 1): 1
   ```

3. 全部选择完成后，CLI 逐个 provider 处理：读取该 provider 下所有 `is_active = 1` 的 Key，控制最大并发数（从 yaml 的 `max_concurrent_tasks` 读取），使用用户选择的模型向对应的上游 Base URL 发送一条极简的测试聊天请求（如 `{"messages":[{"role":"user","content":"hi"}]}`）。

4. 测试时，对流式响应的 Token 计数**正常记录**到 `request_logs`，但标记 `is_test_request = 1`（以便 `stat` 排除）。

5. **每个 provider 测试完成后，立即展示该 provider 的结果，并询问是否移除失败的 Key：**

   ```
   >>> Testing ds with model: ds/deepseek-chat
   sk-ds-main-xxxx  ✅  10 tokens
   sk-ds-backup-yyyy  ❌  timeout
   Remove failed key(s) for provider [ds]? (y/N): y

   >>> Testing mimo with model: mimo/gpt-4o-mini
   tk-mimo-zzzz  ❌  timeout
   Remove failed key(s) for provider [mimo]? (y/N): n
   ```

6. 用户确认后，立即更新 SQLite 中该 provider 下对应 Key 的 `is_active = 0`。正在运行的 systemd 进程在下一次路由时读到数据库变更，自然不再分发给这些 Key。
