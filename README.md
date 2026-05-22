# FastClaw Memory Proxy

> 上下文卸载中间件 — 为 FastClaw AI Agent 提供 SSE 流拦截、工具结果卸载、Token 节省能力

## 解决的问题

AI Agent 每轮对话都会携带大量工具调用结果（web_search、web_fetch 等返回的全文），导致：

- ❌ 上下文迅速膨胀
- ❌ Token 消耗高
- ❌ 响应速度变慢

**Memory Proxy** 作为反向代理插入在客户端（Web UI / 微信）和 FastClaw API 之间，自动拦截流式响应中的工具结果，将其卸载到本地文件系统，上下文中只保留一行摘要引用。

## 实测效果

| 指标 | 数值 |
|------|------|
| 原始工具结果 | ~850 字符 |
| 卸载后上下文摘要 | ~50 字符 |
| **Token 节省率** | **~94%** |

## 架构

```
用户 (Web UI / 微信)
       │
       ▼
┌──────────────────────┐
│  Memory Proxy (:18954) │
│                      │
│  ┌─ SSE 拦截 ─────┐  │
│  │ tool_result →   │  │
│  │ refs/*.md +    │  │
│  │ 摘要替换        │  │
│  └────────────────┘  │
│                      │
│  ┌─ Mermaid 画布 ─┐ │
│  └────────────────┘ │
│                      │
│  ┌─ Token 统计 ───┐ │
│  └────────────────┘ │
│                      │
│  ┌─ 长期记忆 ─────┐ │
│  │ SQLite 存储     │  │
│  └────────────────┘ │
└──────────┬───────────┘
           │
           ▼
  FastClaw Chat API (:18953)
  POST /api/chat/stream (SSE)
```

## 快速开始

### 1. 编译

```bash
git clone https://github.com/shyky/fastclaw-memory-proxy.git
cd fastclaw-memory-proxy
go build -o memory-proxy ./cmd/main.go
```

### 2. 配置

```bash
cp config.yaml.example config.yaml
# 编辑 config.yaml 填入你的 FastClaw API Key 和 Agent ID
```

### 3. 运行

```bash
./memory-proxy -config config.yaml
```

### 4. 使用 systemd（推荐）

```bash
sudo cp memory-proxy.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now memory-proxy
```

### 5. 将客户端指向代理

修改客户端（或 lt-forwarder）请求地址：

```
http://localhost:18953  →  http://localhost:18954
```

## 存储结构

```
~/.fastclaw/memory-proxy/
├── refs/{agentId}/     # 工具结果原文（按 Agent 分类）
├── canvas/{agentId}/   # Mermaid 任务画布
├── tasks/{agentId}/    # 执行记录（JSONL）
├── memory.db           # 长期记忆（SQLite）
└── stats/{agentId}/    # Token 统计报告
```

## 管理

```bash
# 健康检查
curl http://localhost:18954/health

# 查看日志
sudo journalctl -u memory-proxy -f

# 重启
sudo systemctl restart memory-proxy
```

## 项目结构

```
├── cmd/main.go                     # 入口
├── internal/
│   ├── config/config.go            # 配置加载
│   ├── proxy/proxy.go              # SSE 代理核心
│   ├── canvas/canvas.go            # Mermaid 画布引擎
│   ├── memory/memory.go            # SQLite 长期记忆
│   └── stats/stats.go              # Token 统计
├── config.yaml.example             # 配置示例
├── memory-proxy.service            # systemd 服务文件
└── go.mod
```

## License

MIT
