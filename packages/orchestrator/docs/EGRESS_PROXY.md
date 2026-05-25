# EgressProxy 实现方式总结

## 接口定义
**位置**: `pkg/sandbox/network/egressproxy.go`

```go
type EgressProxy interface {
    OnSlotCreate(s *Slot, tables *iptables.IPTables) error
    OnSlotDelete(s *Slot, tables *iptables.IPTables) error
    CABundle() string
}
```

**职责**:
- `OnSlotCreate`: 当创建网络 Slot 时，配置出口代理规则（通常是 iptables 规则）
- `OnSlotDelete`: 当删除网络 Slot 时，清理出口代理规则
- `CABundle`: 返回 CA 证书束，用于 HTTPS 流量拦截

---

## 实现方式

### 1. ⭐ **tcpfirewall** - 生产环境实现
**位置**: `pkg/tcpfirewall/proxy.go`

**使用场景**: 生产环境，需要真实的网络流量控制

**特点**:
- 使用 `tcpproxy` 库实现 TCP 代理
- 支持三种流量类型的分离：
  - HTTP (端口 80) - 通过 Host header 检查
  - TLS (端口 443) - 通过 SNI 检查
  - 其他 TCP - 仅通过 CIDR 检查
- 实现连接限制 (`connlimit.ConnectionLimiter`)
- 支持指标收集 (`Metrics`)
- 订阅 sandbox 事件，动态更新代理规则

**关键方法**:
```go
func New(logger logger.Logger, networkConfig network.Config, 
         sandboxes *sandbox.Map, meterProvider metric.MeterProvider, 
         featureFlags *featureflags.Client) *Proxy

func (p *Proxy) Start(ctx context.Context) error
func (p *Proxy) OnSlotCreate(s *Slot, tables *iptables.IPTables) error
func (p *Proxy) OnSlotDelete(s *Slot, tables *iptables.IPTables) error
func (p *Proxy) CABundle() string
```

**在 main.go 中的注入**:
```go
func defaultEgressFactory(_ context.Context, deps *factories.Deps) (*factories.EgressSetup, error) {
    fw := tcpfirewall.New(
        deps.Logger,
        deps.Config.NetworkConfig,
        deps.Sandboxes,
        deps.MeterProvider,
        deps.FeatureFlags,
    )

    return &factories.EgressSetup{
        Proxy: fw,
        Start: fw.Start,
        Close: fw.Close,
    }, nil
}
```

---

### 2. ⭐⭐ **NoopEgressProxy** - 测试/开发实现
**位置**: `pkg/sandbox/network/egressproxy.go`

**使用场景**: 单元测试、基准测试、开发环境

**特点**:
- 空操作实现，所有方法都是 no-op
- 不执行任何网络配置
- 不返回 CA 证书

**实现**:
```go
type NoopEgressProxy struct{}

func (NoopEgressProxy) OnSlotCreate(_ *Slot, _ *iptables.IPTables) error {
    return nil
}

func (NoopEgressProxy) OnSlotDelete(_ *Slot, _ *iptables.IPTables) error {
    return nil
}

func (NoopEgressProxy) CABundle() string {
    return ""
}
```

**使用位置**:
- `benchmarks/benchmark_test.go` - 基准测试
- `benchmarks/concurrent_benchmark_test.go` - 并发基准测试
- `cmd/create-build/main.go` - 构建命令
- `cmd/smoketest/smoke_test.go` - 烟雾测试

---

### 3. ⭐ **noEgressProxy** - 自定义实现（仅移除默认路由）
**位置**: `cmd/resume-build/main.go`

**使用场景**: `resume-build` 命令，需要特殊的网络配置

**特点**:
- 继承 `NoopEgressProxy`
- 覆盖 `OnSlotCreate` 方法
- 移除默认路由，实现网络隔离

**实现**:
```go
// noEgressProxy is an EgressProxy that removes the default route from the
// namespace to prevent internet access.
type noEgressProxy struct {
    network.NoopEgressProxy
}

func (noEgressProxy) OnSlotCreate(s *network.Slot, _ *iptables.IPTables) error {
    // 移除默认路由的实现
    // ...
}
```

**使用**:
```go
var egressProxy network.EgressProxy = network.NoopEgressProxy{}
if !allowInternet {
    egressProxy = noEgressProxy{}
}
```

---

### 4. ⭐ **mockEgressProxy** - 测试 Mock 实现
**位置**: `pkg/sandbox/envd_test.go`

**使用场景**: 单元测试中的 mock

**特点**:
- 用于测试 envd 相关功能
- 返回固定的 CA 证书束

**实现**:
```go
type mockEgressProxy struct {
    bundle string
}

func (m *mockEgressProxy) OnSlotCreate(_ *network.Slot, _ *iptables.IPTables) error { 
    return nil 
}

func (m *mockEgressProxy) OnSlotDelete(_ *network.Slot, _ *iptables.IPTables) error { 
    return nil 
}

func (m *mockEgressProxy) CABundle() string { 
    return m.bundle 
}
```

---

## 实现方式对比表

| 实现 | 位置 | 用途 | 复杂度 | 网络操作 | CA 证书 |
|------|------|------|--------|---------|---------|
| **tcpfirewall** | `pkg/tcpfirewall/` | 生产环境 | ⭐⭐⭐⭐⭐ | ✅ 完整 | ✅ 支持 |
| **NoopEgressProxy** | `pkg/sandbox/network/` | 测试/开发 | ⭐ | ❌ 无 | ❌ 无 |
| **noEgressProxy** | `cmd/resume-build/` | 特殊场景 | ⭐⭐ | ⭐ 部分 | ❌ 无 |
| **mockEgressProxy** | `pkg/sandbox/envd_test.go` | 单元测试 | ⭐ | ❌ 无 | ✅ Mock |

---

## 策略模式的应用

### 为什么这样设计？

1. **解耦**: `run.go` 不知道具体的实现，只依赖 `EgressProxy` 接口
2. **可测试**: 测试时可以注入 `NoopEgressProxy`，避免真实的 iptables 操作
3. **可扩展**: 未来可以轻松添加新的实现（如 wireguard、eBPF 等）
4. **灵活**: 不同的命令可以使用不同的实现

### 注入点

```go
// 在 factories/run.go 中
egressSetup, err := opts.EgressFactory(ctx, deps)

// 在 main.go 中
factories.Run(factories.Options{
    EgressFactory: defaultEgressFactory,  // 注入 tcpfirewall
})

// 在测试中可以注入其他实现
factories.Run(factories.Options{
    EgressFactory: func(ctx context.Context, deps *factories.Deps) (*factories.EgressSetup, error) {
        return &factories.EgressSetup{
            Proxy: network.NoopEgressProxy{},
        }, nil
    },
})
```

---

## 关键设计决策

### 1. 为什么分离 HTTP/TLS/Other 三种流量？
- **HTTP (80)**: 通过 Host header 识别目标沙箱
- **TLS (443)**: 通过 SNI (Server Name Indication) 识别目标沙箱
- **Other**: 仅通过 CIDR 识别，避免协议检测阻塞

### 2. 为什么需要 CABundle？
- 用于 HTTPS 流量拦截
- 沙箱内的应用需要信任这个 CA 证书
- 允许 orchestrator 进行 MITM 检查

### 3. 为什么 OnSlotCreate/OnSlotDelete 需要 iptables 参数？
- 允许实现者自定义 iptables 规则
- 支持不同的网络隔离策略
- 便于测试（可以传入 mock iptables）

---

## 代码流程图

```
main.go
  ↓
factories.Run(Options{
  EgressFactory: defaultEgressFactory
})
  ↓
run(config, opts)
  ↓
opts.EgressFactory(ctx, deps)  ← 策略模式注入点
  ↓
tcpfirewall.New(...)  ← 生产环境实现
  ↓
EgressSetup{
  Proxy: fw,
  Start: fw.Start,
  Close: fw.Close
}
  ↓
egressSetup.Proxy.OnSlotCreate(...)  ← 创建网络 Slot 时调用
egressSetup.Proxy.OnSlotDelete(...)  ← 删除网络 Slot 时调用
egressSetup.Proxy.CABundle()         ← 获取 CA 证书
```

---

## 关键文件清单

| 文件 | 行数 | 说明 |
|------|------|------|
| `pkg/sandbox/network/egressproxy.go` | 35 | 接口定义 + NoopEgressProxy |
| `pkg/tcpfirewall/proxy.go` | 200+ | tcpfirewall 实现 |
| `pkg/tcpfirewall/handlers.go` | 200+ | 流量处理逻辑 |
| `pkg/tcpfirewall/listener.go` | 100+ | 监听器实现 |
| `pkg/tcpfirewall/metrics.go` | 100+ | 指标收集 |
| `main.go` | 40 | 注入点 |
| `pkg/factories/run.go` | 1000+ | 组合根 |
| `cmd/resume-build/main.go` | 1500+ | 自定义实现 |
| `pkg/sandbox/envd_test.go` | 50+ | Mock 实现 |

---

## 未来扩展可能性

1. **eBPF 实现**: 使用 eBPF 替代 iptables，性能更好
2. **Wireguard 实现**: 使用 Wireguard 隧道代替 TCP 代理
3. **Cilium 集成**: 使用 Cilium 进行网络策略管理
4. **AWS VPC 实现**: 在 AWS 上使用 VPC 网络隔离

所有这些都可以通过实现 `EgressProxy` 接口并注入 `EgressFactory` 来实现，无需修改 `run.go`。

---

## 社区贡献建议

### 如果要添加新的 EgressProxy 实现：

1. **创建新包**: `pkg/wireguard/proxy.go`
2. **实现接口**:
   ```go
   type Proxy struct { ... }
   
   func (p *Proxy) OnSlotCreate(s *Slot, tables *iptables.IPTables) error { ... }
   func (p *Proxy) OnSlotDelete(s *Slot, tables *iptables.IPTables) error { ... }
   func (p *Proxy) CABundle() string { ... }
   ```
3. **添加工厂函数**:
   ```go
   func wireguardEgressFactory(ctx context.Context, deps *factories.Deps) (*factories.EgressSetup, error) {
       proxy := wireguard.New(...)
       return &factories.EgressSetup{
           Proxy: proxy,
           Start: proxy.Start,
           Close: proxy.Close,
       }, nil
   }
   ```
4. **在 main.go 中选择**:
   ```go
   var factory factories.EgressFactory
   if os.Getenv("USE_WIREGUARD") == "true" {
       factory = wireguardEgressFactory
   } else {
       factory = defaultEgressFactory
   }
   
   factories.Run(factories.Options{
       EgressFactory: factory,
   })
   ```

### 优势：
- ✅ 无需修改 `run.go`
- ✅ 无需修改 `factories` 包
- ✅ 完全解耦
- ✅ 易于测试
- ✅ 易于切换
