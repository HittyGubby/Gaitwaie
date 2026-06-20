# Gaitwaie

Gaitwaie 是一个极简、高性能的多租户（Receiver）隔离 OpenAI 兼容格式路由网关，专为需要与朋友们共享 AI API Key 的场景设计。

## 特性

- **极速流式转发** — 使用 `bufio.Scanner` 逐行透传 SSE 字节流，零全局 JSON 解包，仅在流尾提取 Token 计数
- **多 Provider 路由** — 自动解析 `provider_alias/model_name` 格式的模型名，路由到对应的上游
- **多租户隔离** — 每个 Receiver（客户端）使用独立的 Bearer Token 鉴权，用量可追踪
- **熔断保护** — 自动检测失效 Key（401/403/5xx/超时），按状态机策略熔断并可选冷却后恢复
- **Key 轮询** — 同一 Provider 下多个 Key 自动 Round-Robin 负载均衡
- **零外部 Web 框架** — Go 1.22+ 原生 `net/http`，单二进制部署
- **分离静态配置与动态状态** — YAML 只读，Key 状态持久化在 SQLite 中
- **CLI 管理** — 内建统计与 Key 净化命令，无需 WebUI
- **兼容 OpenAI API** — 客户端无需修改即可接入

## 安装

### 一键安装（Linux）

```bash
curl -fsSL https://github.com/HittyGubby/gaitwaie/releases/latest/download/install.sh | sudo bash
```

### 手动编译

```bash
# 需要 Go 1.22+
git clone https://github.com/HittyGubby/gaitwaie.git
cd gaitwaie
go build -o gateway .
sudo ./gateway start --config /etc/gaitwaie/config.yaml
```

## 快速开始

### 1. 配置

编辑 `/etc/gaitwaie/config.yaml`：

```yaml
database_path: "/var/lib/gaitwaie/gateway.db"
listen_addr: ":8080"
tolerance: 3
max_concurrent_tasks: 5

providers:
  ds:
    base_url: "https://api.deepseek.com/v1"
    keys:
      - "sk-ds-main-xxxx"
      - "sk-ds-backup-yyyy"
  aoai:
    base_url: "https://your-resource.openai.azure.com/v1"
    keys:
      - "sk-aoai-zzzz"

receivers:
  alice: "sk-alice-token-xxxx"
  bob: "sk-bob-token-yyyy"
```

### 2. 启动服务

```bash
gateway start
# 或指定自定义配置
gateway start --config ./config.yaml
```

### 3. 客户端使用

客户端只需将 API Endpoint 指向网关地址，模型名使用 `provider_alias/model_name` 格式：

```bash
# 使用 curl
curl http://your-gateway:8080/v1/chat/completions \
  -H "Authorization: Bearer sk-alice-token-xxxx" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "ds/deepseek-chat",
    "messages": [{"role": "user", "content": "Hello!"}],
    "stream": true
  }'

# 或配置到任何 OpenAI 兼容客户端（如 ChatGPT-Next-Web, OpenCat 等）
# API Endpoint: http://your-gateway:8080
# API Key: sk-alice-token-xxxx
```

## CLI 命令

### `start` — 启动网关服务

```bash
gateway start                      # 使用默认配置 /etc/gaitwaie/config.yaml
gateway start --config ./dev.yaml  # 使用自定义配置
gateway start --listen :9090       # 覆盖监听地址
```

### `stat` — 用量统计

```bash
gateway stat          # 查看所有时间内的用量统计
gateway stat 7d       # 最近 7 天
gateway stat 1h       # 最近 1 小时
gateway stat 2M       # 最近 2 个月
```

输出示例：
```
alice:
  requests: 123
  prompt: 10000 (10k)
  completion: 1000000 (1M)
  total: 1010000 (1.01M)
bob:
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

### `purge` — 检测并移除失效 Key

```bash
gateway purge --config /etc/gaitwaie/config.yaml
```

交互式流程：
1. 对每个 Provider，自动查询可用模型列表让用户选择
2. 使用所选模型，对每个 Key 发送最小测试请求
3. 按 Provider 分别展示结果
4. 逐 Provider 询问是否移除失败的 Key

## 架构

```
┌─────────────┐     ┌──────────────┐     ┌─────────────────┐
│   Client    │────▶│   Gaitwaie   │────▶│  Provider: ds   │
│ (alice, bob)│     │   Gateway    │     │  (DeepSeek)     │
└─────────────┘     │   :8080      │     ├─────────────────┤
                    │              │────▶│  Provider: aoai │
                    │  ┌────────┐  │     │  (Azure OpenAI) │
                    │  │ SQLite │  │     └─────────────────┘
                    │  │  DB    │  │
                    │  └────────┘  │
                    └──────────────┘
```

- **静态配置**：YAML 文件（只读，绝不反向修改）
- **动态状态**：SQLite 数据库（Key 熔断状态、请求日志、用量统计）
- **模型缓存**：启动时异步聚合各 Provider 的模型列表，内存缓存

## 开发

```bash
git clone https://github.com/HittyGubby/gaitwaie.git
cd gaitwaie
go build -o gateway .      # 编译
./gateway start --config config.yaml  # 启动
```

## License

MIT
