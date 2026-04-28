# E2B HA Phase 1 快速开始指南

## 5 分钟快速开始

### 1. 编译代码

```bash
cd /root/shaoll/infra

# 编译 API 服务
go build -o api ./packages/api

# 编译 Orchestrator
go build -o orchestrator ./packages/orchestrator
```

### 2. 运行单元测试

```bash
# 测试 Drain Monitor
go test -v ./packages/api/internal/orchestrator -run TestDrainMonitor

# 测试 Drain Wait
go test -v ./packages/api/internal/orchestrator/nodemanager -run TestWaitForDrain
```

### 3. 运行集成测试

```bash
# 运行所有 Drain 集成测试
go test -v ./tests/integration -run TestDrain
```

### 4. 启动本地环境

```bash
# 启动本地基础设施（PostgreSQL, Redis, ClickHouse 等）
make local-infra

# 在另一个终端启动 API 服务
cd packages/api
make run-local

# 在第三个终端启动 Orchestrator
cd packages/orchestrator
make run-local
```

### 5. 测试 API 端点

```bash
# 获取节点列表
curl -X GET \
  -H "Authorization: Bearer your-admin-token" \
  http://localhost:3000/admin/nodes

# 启动 drain
NODE_ID="<from-above>"
curl -X POST \
  -H "Authorization: Bearer your-admin-token" \
  http://localhost:3000/admin/nodes/$NODE_ID/drain

# 查询 drain 状态
curl -X GET \
  -H "Authorization: Bearer your-admin-token" \
  http://localhost:3000/admin/nodes/$NODE_ID/drain-status

# 等待 drain 完成
curl -X POST \
  -H "Authorization: Bearer your-admin-token" \
  "http://localhost:3000/admin/nodes/$NODE_ID/drain-wait?timeout=300"
```

## 文件结构

```
packages/api/
├── internal/
│   ├── handlers/
│   │   └── admin.go                    # ✅ 新增 Drain API 端点
│   └── orchestrator/
│       ├── drain_monitor.go            # ✅ 新建 Drain 监控
│       ├── drain_monitor_test.go       # ✅ 新建 Drain 监控测试
│       └── nodemanager/
│           ├── drain.go                # ✅ 新建 Drain 等待
│           └── drain_test.go           # ✅ 新建 Drain 等待测试
spec/
└── openapi.yml                         # ✅ 新增 Drain API 定义
tests/
└── integration/
    └── drain_test.go                   # ✅ 新建 Drain 集成测试
```

## 核心概念

### Drain 状态机

```
┌─────────────────┐
│  Not Started    │
└────────┬────────┘
         │ StartDrain()
         ▼
┌─────────────────┐
│  In Progress    │◄──────────────┐
└────────┬────────┘               │
         │ UpdateDrainProgress()   │
         │ (sandbox_count > 0)     │
         └───────────────────────┘
         │
         │ UpdateDrainProgress()
         │ (sandbox_count == 0)
         ▼
┌─────────────────┐
│   Completed     │
└─────────────────┘

或者

┌─────────────────┐
│  In Progress    │
└────────┬────────┘
         │ MarkDrainFailed()
         ▼
┌─────────────────┐
│    Failed       │
└─────────────────┘
```

### API 流程

```
1. POST /admin/nodes/{nodeID}/drain
   └─> 设置节点状态为 Draining
   └─> 返回 200 OK

2. GET /admin/nodes/{nodeID}/drain-status
   └─> 获取当前 sandbox 计数
   └─> 返回 200 OK

3. POST /admin/nodes/{nodeID}/drain-wait?timeout=300
   └─> 轮询检查 sandbox 计数
   └─> 当 sandbox_count == 0 时返回 200 OK
   └─> 超时时返回 408 Request Timeout
```

## 常见命令

### 查看所有节点
```bash
curl -X GET \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  http://localhost:3000/admin/nodes | jq
```

### 查看节点详情
```bash
curl -X GET \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  http://localhost:3000/admin/nodes/$NODE_ID | jq
```

### 启动 drain 并等待完成
```bash
# 启动 drain
curl -X POST \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  http://localhost:3000/admin/nodes/$NODE_ID/drain

# 等待完成（最多 10 分钟）
curl -X POST \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  "http://localhost:3000/admin/nodes/$NODE_ID/drain-wait?timeout=600"
```

### 监控 drain 进度
```bash
# 每 2 秒检查一次状态
while true; do
  curl -s -X GET \
    -H "Authorization: Bearer $ADMIN_TOKEN" \
    http://localhost:3000/admin/nodes/$NODE_ID/drain-status | jq '.sandbox_count'
  sleep 2
done
```

## 故障排查

### 问题: 无法连接到 API
```bash
# 检查 API 是否运行
curl http://localhost:3000/health

# 检查日志
tail -f /tmp/api.log
```

### 问题: Drain 超时
```bash
# 增加超时时间
curl -X POST \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  "http://localhost:3000/admin/nodes/$NODE_ID/drain-wait?timeout=1200"

# 检查 orchestrator 日志
tail -f /tmp/orchestrator.log
```

### 问题: 节点不存在
```bash
# 获取正确的节点 ID
curl -X GET \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  http://localhost:3000/admin/nodes | jq '.[] | .id'
```

## 下一步

1. **阅读完整文档**: `HA-PHASE1-IMPLEMENTATION.md`
2. **查看验证清单**: `HA-PHASE1-VERIFICATION.md`
3. **运行所有测试**: `make test`
4. **部署到生产**: 参考 `self-host.md`

## 获取帮助

- 📖 [E2B 文档](https://e2b.dev)
- 🐛 [GitHub Issues](https://github.com/e2b-dev/infra/issues)
- 💬 [社区讨论](https://github.com/e2b-dev/infra/discussions)

## 许可证

MIT License - 详见 LICENSE 文件
