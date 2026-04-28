# E2B HA Phase 1 实现指南 - Drain 流程自动化

## 概述

本文档描述了 E2B 高可用性（HA）第一阶段的实现，包括自动化 drain 流程编排、drain 完成检测和 drain API 端点。

## 实现的功能

### 1. Drain 状态监控模块 (`drain_monitor.go`)

**位置**: `packages/api/internal/orchestrator/drain_monitor.go`

**功能**:
- 跟踪节点的 drain 状态
- 监控 sandbox 计数变化
- 记录 drain 开始和完成时间
- 支持多节点并发 drain

**关键类型**:
```go
type DrainMonitor struct {
    drainStates map[string]*DrainState
}

type DrainState struct {
    NodeID              string
    Status              DrainStatus
    StartTime           time.Time
    CompletionTime      *time.Time
    InitialSandboxCount int
    CurrentSandboxCount int
    LastUpdated         time.Time
    Error               error
}

type DrainStatus string
// 可能的值: DrainStatusNotStarted, DrainStatusInProgress, DrainStatusCompleted, DrainStatusFailed
```

**主要方法**:
- `NewDrainMonitor()` - 创建新的 drain 监控器
- `StartDrain(ctx, nodeID, initialCount)` - 开始 drain
- `UpdateDrainProgress(ctx, nodeID, currentCount)` - 更新 drain 进度
- `MarkDrainFailed(ctx, nodeID, err)` - 标记 drain 失败
- `GetDrainState(nodeID)` - 获取 drain 状态
- `IsDrainCompleted(nodeID)` - 检查 drain 是否完成
- `ClearDrainState(ctx, nodeID)` - 清除 drain 状态
- `MonitorNodeDrain(ctx, node, pollInterval)` - 持续监控节点 drain

### 2. Drain 完成检测模块 (`drain.go`)

**位置**: `packages/api/internal/orchestrator/nodemanager/drain.go`

**功能**:
- 等待节点完成 drain（所有 sandbox 完成）
- 支持自定义超时配置
- 轮询检查 sandbox 计数

**关键类型**:
```go
type DrainWaitConfig struct {
    PollInterval time.Duration  // 轮询间隔，默认 2 秒
    MaxWaitTime  time.Duration  // 最大等待时间，默认 5 分钟
}
```

**主要方法**:
- `WaitForDrain(ctx, config)` - 等待 drain 完成
- `WaitForDrainWithContext(ctx, config)` - 使用自定义 context 等待 drain
- `GetSandboxCount(ctx)` - 获取当前 sandbox 计数
- `IsDrained(ctx)` - 检查节点是否已 drain

### 3. Drain API 端点

**位置**: `packages/api/internal/handlers/admin.go`

**新增端点**:

#### 3.1 启动 Drain
```
POST /admin/nodes/{nodeID}/drain
```

**请求**:
```bash
curl -X POST \
  -H "Authorization: Bearer <admin-token>" \
  http://localhost:3000/admin/nodes/node-123/drain
```

**响应** (200 OK):
```json
{
  "node_id": "node-123",
  "status": "draining",
  "message": "Node drain initiated. Use GET /admin/nodes/{nodeID}/drain-status to check progress."
}
```

#### 3.2 查询 Drain 状态
```
GET /admin/nodes/{nodeID}/drain-status
```

**请求**:
```bash
curl -X GET \
  -H "Authorization: Bearer <admin-token>" \
  http://localhost:3000/admin/nodes/node-123/drain-status
```

**响应** (200 OK):
```json
{
  "node_id": "node-123",
  "status": "draining",
  "sandbox_count": 3,
  "is_drained": false,
  "last_updated": "2026-04-27T10:30:45Z"
}
```

#### 3.3 等待 Drain 完成
```
POST /admin/nodes/{nodeID}/drain-wait?timeout=300
```

**请求**:
```bash
curl -X POST \
  -H "Authorization: Bearer <admin-token>" \
  "http://localhost:3000/admin/nodes/node-123/drain-wait?timeout=600"
```

**响应** (200 OK):
```json
{
  "node_id": "node-123",
  "status": "drained",
  "drain_duration": "2m30s",
  "message": "Node has been successfully drained. All sandboxes have completed."
}
```

**错误响应** (408 Request Timeout):
```json
{
  "error": "Drain wait timeout after 5m0s: context deadline exceeded"
}
```

### 4. OpenAPI 规范更新

**位置**: `spec/openapi.yml`

**新增**:
- 3 个新的 API 端点定义
- 4 个新的响应 schema：
  - `DrainInitiateResponse`
  - `DrainStatusResponse`
  - `DrainCompleteResponse`
  - `ErrorResponse`

## 验证流程

### 1. 单元测试

#### 运行 Drain Monitor 测试
```bash
cd /root/shaoll/infra
go test -v ./packages/api/internal/orchestrator -run TestDrainMonitor
```

**测试覆盖**:
- ✅ 启动 drain
- ✅ 更新 drain 进度
- ✅ 标记 drain 失败
- ✅ 检查 drain 完成状态
- ✅ 清除 drain 状态
- ✅ 多节点并发 drain
- ✅ 非存在节点处理

#### 运行 Drain Wait 测试
```bash
cd /root/shaoll/infra
go test -v ./packages/api/internal/orchestrator/nodemanager -run TestWaitForDrain
```

**测试覆盖**:
- ✅ 已 drain 的节点
- ✅ 超时处理
- ✅ Context 取消
- ✅ 获取 sandbox 错误处理
- ✅ Sandbox 计数
- ✅ Drain 状态检查
- ✅ 渐进式 drain

### 2. 集成测试

#### 运行完整 Drain 流程测试
```bash
cd /root/shaoll/infra
go test -v ./tests/integration -run TestDrainFlow
```

**测试场景**:
- ✅ 单节点 drain 流程
- ✅ 多节点并发 drain
- ✅ Drain 状态转换
- ✅ Drain 失败场景
- ✅ Drain 指标收集
- ✅ 并发 drain 操作

### 3. 手动验证

#### 3.1 启动本地开发环境
```bash
cd /root/shaoll/infra
make local-infra
```

#### 3.2 获取 Admin Token
```bash
# 从环境变量或配置中获取
export ADMIN_TOKEN="your-admin-token"
```

#### 3.3 获取节点列表
```bash
curl -X GET \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  http://localhost:3000/admin/nodes
```

#### 3.4 测试 Drain 流程

**步骤 1**: 启动 drain
```bash
NODE_ID="<node-id-from-list>"

curl -X POST \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  http://localhost:3000/admin/nodes/$NODE_ID/drain
```

**步骤 2**: 检查 drain 状态
```bash
curl -X GET \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  http://localhost:3000/admin/nodes/$NODE_ID/drain-status
```

**步骤 3**: 等待 drain 完成（可选）
```bash
curl -X POST \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  "http://localhost:3000/admin/nodes/$NODE_ID/drain-wait?timeout=600"
```

### 4. 代码质量检查

#### 运行所有测试
```bash
cd /root/shaoll/infra
make test
```

#### 运行 lint 检查
```bash
cd /root/shaoll/infra
make lint
```

#### 运行 fmt 检查
```bash
cd /root/shaoll/infra
make fmt
```

### 5. 性能验证

#### 测试大量 sandbox 的 drain
```bash
# 创建测试脚本
cat > /tmp/test_drain_performance.sh << 'EOF'
#!/bin/bash

NODE_ID="<node-id>"
ADMIN_TOKEN="<admin-token>"

# 启动 drain
echo "Starting drain..."
START_TIME=$(date +%s)

curl -X POST \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  http://localhost:3000/admin/nodes/$NODE_ID/drain

# 等待 drain 完成
echo "Waiting for drain completion..."
curl -X POST \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  "http://localhost:3000/admin/nodes/$NODE_ID/drain-wait?timeout=600"

END_TIME=$(date +%s)
DURATION=$((END_TIME - START_TIME))

echo "Drain completed in $DURATION seconds"
EOF

chmod +x /tmp/test_drain_performance.sh
/tmp/test_drain_performance.sh
```

## 使用示例

### 示例 1: 简单的 Drain 流程

```bash
#!/bin/bash

NODE_ID="orchestrator-1"
ADMIN_TOKEN="your-admin-token"
API_URL="http://localhost:3000"

# 1. 启动 drain
echo "1. Starting drain on node $NODE_ID..."
curl -X POST \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  $API_URL/admin/nodes/$NODE_ID/drain

# 2. 等待 drain 完成（最多 10 分钟）
echo "2. Waiting for drain completion..."
RESPONSE=$(curl -X POST \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  "$API_URL/admin/nodes/$NODE_ID/drain-wait?timeout=600")

echo "Drain result: $RESPONSE"

# 3. 验证节点已 drain
echo "3. Verifying node is drained..."
curl -X GET \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  $API_URL/admin/nodes/$NODE_ID/drain-status
```

### 示例 2: K8s 滚动更新集成

```bash
#!/bin/bash

# 在 PreStop hook 中调用
NODE_ID=$(hostname)
ADMIN_TOKEN=$ADMIN_API_TOKEN
API_URL=$ADMIN_API_URL

# 启动 drain 并等待完成
curl -X POST \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  "$API_URL/admin/nodes/$NODE_ID/drain-wait?timeout=300"

# 如果 drain 成功，Pod 可以安全关闭
exit $?
```

## 故障排查

### 问题 1: Drain 超时

**症状**: `Drain wait timeout after 5m0s`

**原因**: 
- Sandbox 未能在超时时间内完成
- 网络连接问题
- Orchestrator 无响应

**解决方案**:
1. 增加超时时间: `?timeout=900` (15 分钟)
2. 检查 orchestrator 日志
3. 检查网络连接
4. 手动杀死 sandbox: `POST /admin/teams/{teamID}/sandboxes/kill`

### 问题 2: Drain 状态不更新

**症状**: `sandbox_count` 不变

**原因**:
- Sandbox 未正确报告状态
- Orchestrator 连接问题

**解决方案**:
1. 检查 orchestrator 是否在线: `GET /admin/nodes/{nodeID}`
2. 查看 orchestrator 日志
3. 重启 orchestrator

### 问题 3: API 返回 404

**症状**: `404 Not Found`

**原因**:
- 节点 ID 不正确
- 节点不存在

**解决方案**:
1. 获取正确的节点 ID: `GET /admin/nodes`
2. 验证节点是否在线

## 文件清单

| 文件 | 描述 | 状态 |
|------|------|------|
| `packages/api/internal/orchestrator/drain_monitor.go` | Drain 状态监控 | ✅ 新建 |
| `packages/api/internal/orchestrator/drain_monitor_test.go` | Drain 监控单元测试 | ✅ 新建 |
| `packages/api/internal/orchestrator/nodemanager/drain.go` | Drain 完成检测 | ✅ 新建 |
| `packages/api/internal/orchestrator/nodemanager/drain_test.go` | Drain 检测单元测试 | ✅ 新建 |
| `packages/api/internal/handlers/admin.go` | Drain API 端点 | ✅ 修改 |
| `spec/openapi.yml` | OpenAPI 规范 | ✅ 修改 |
| `tests/integration/drain_test.go` | Drain 集成测试 | ✅ 新建 |

## 下一步

### Phase 2: K8s 集成
- [ ] PreStop Hook 实现
- [ ] Helm Chart 更新
- [ ] 集成测试

### Phase 3: 高级功能
- [ ] Sandbox 迁移机制
- [ ] 连接保活机制
- [ ] 监控和告警

## 参考资源

- [E2B 官方文档](https://e2b.dev)
- [Orchestrator 源代码](../../orchestrator/)
- [API 文档](../../../spec/openapi.yml)
