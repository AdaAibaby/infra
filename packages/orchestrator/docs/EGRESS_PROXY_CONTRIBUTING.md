# EgressProxy 社区贡献指南

## 🎯 贡献机会

### 1. 新的网络实现

#### eBPF 内核级实现
```
难度: ⭐⭐⭐⭐⭐
影响: 性能提升 10-100x
工作量: 2-4 周
```

**为什么有价值:**
- 避免用户态/内核态切换
- 支持更高的吞吐量
- 更低的延迟

**实现步骤:**
1. 创建 `pkg/ebpf/proxy.go`
2. 编写 eBPF 程序 (C 语言)
3. 实现 `EgressProxy` 接口
4. 添加单元测试
5. 性能基准测试

**参考资源:**
- Cilium eBPF 库
- Linux eBPF 文档
- 内核网络栈

---

#### Wireguard 隧道实现
```
难度: ⭐⭐⭐⭐
影响: 支持 VPN 隧道
工作量: 1-2 周
```

**为什么有价值:**
- 支持跨云部署
- 加密网络流量
- 支持多区域

**实现步骤:**
1. 创建 `pkg/wireguard/proxy.go`
2. 集成 wireguard-go 库
3. 实现 `EgressProxy` 接口
4. 添加集成测试
5. 文档和示例

---

#### Cilium 网络策略实现
```
难度: ⭐⭐⭐
影响: 支持 Kubernetes 网络策略
工作量: 1-2 周
```

**为什么有价值:**
- 与 Kubernetes 集成
- 支持 NetworkPolicy
- 支持 L7 策略

**实现步骤:**
1. 创建 `pkg/cilium/proxy.go`
2. 集成 Cilium API
3. 实现 `EgressProxy` 接口
4. 添加 K8s 集成测试

---

### 2. 性能优化

#### 连接池优化
```
难度: ⭐⭐⭐
影响: 降低延迟 20-30%
工作量: 3-5 天
```

**当前问题:**
- 每个连接创建新的 goroutine
- 没有连接复用
- 内存占用高

**改进方案:**
```go
// 当前
for conn := range listener {
    go handleConnection(conn)  // 每个连接一个 goroutine
}

// 改进
pool := NewConnectionPool(maxWorkers)
for conn := range listener {
    pool.Handle(conn)  // 复用 goroutine
}
```

---

#### 规则匹配优化
```
难度: ⭐⭐
影响: 降低 CPU 使用 15-25%
工作量: 2-3 天
```

**当前问题:**
- 线性搜索规则列表
- 重复的正则表达式编译
- 没有缓存

**改进方案:**
```go
// 使用 trie 树或 radix 树
type RuleMatcher struct {
    trie *RadixTree
    cache *LRUCache
}

func (m *RuleMatcher) Match(host string) *Rule {
    if cached, ok := m.cache.Get(host); ok {
        return cached
    }
    rule := m.trie.Search(host)
    m.cache.Set(host, rule)
    return rule
}
```

---

#### 内存优化
```
难度: ⭐⭐
影响: 降低内存占用 30-40%
工作量: 2-3 天
```

**当前问题:**
- 每个 Slot 创建新的 map
- 没有对象池
- 字符串重复

**改进方案:**
```go
// 使用对象池
type SlotPool struct {
    pool sync.Pool
}

func (p *SlotPool) Get() *Slot {
    if v := p.pool.Get(); v != nil {
        return v.(*Slot)
    }
    return &Slot{}
}

func (p *SlotPool) Put(s *Slot) {
    s.Reset()
    p.pool.Put(s)
}
```

---

### 3. 功能增强

#### 流量统计和分析
```
难度: ⭐⭐
影响: 更好的可观测性
工作量: 3-5 天
```

**新增功能:**
- 按协议统计流量
- 按目标 IP 统计
- 按 Slot 统计
- 实时流量仪表板

**实现:**
```go
type TrafficStats struct {
    BytesIn, BytesOut uint64
    PacketsIn, PacketsOut uint64
    ProtocolBreakdown map[string]uint64
    TopDestinations []Destination
}

func (p *Proxy) GetStats(slotID string) *TrafficStats {
    // 实现统计逻辑
}
```

---

#### 动态规则更新
```
难度: ⭐⭐⭐
影响: 支持热更新规则
工作量: 3-5 天
```

**当前问题:**
- 规则只能在启动时配置
- 更新规则需要重启

**改进方案:**
```go
type RuleManager struct {
    rules sync.Map
    version int64
}

func (m *RuleManager) UpdateRules(newRules []Rule) error {
    // 验证规则
    // 原子更新
    // 通知所有连接
}

func (m *RuleManager) Watch() <-chan RuleUpdate {
    // 返回规则变化通知
}
```

---

#### 故障转移和高可用
```
难度: ⭐⭐⭐⭐
影响: 支持多个代理实例
工作量: 1-2 周
```

**新增功能:**
- 多个代理实例
- 自动故障转移
- 负载均衡
- 健康检查

---

### 4. 测试和文档

#### 集成测试套件
```
难度: ⭐⭐
影响: 提高代码质量
工作量: 3-5 天
```

**测试场景:**
- 正常流量转发
- 规则匹配
- 连接限制
- 故障恢复
- 并发连接

---

#### 性能基准测试
```
难度: ⭐⭐
影响: 性能追踪
工作量: 2-3 天
```

**基准测试:**
- 吞吐量 (connections/sec)
- 延迟 (p50, p95, p99)
- 内存占用
- CPU 使用率

---

#### 文档完善
```
难度: ⭐
影响: 降低学习曲线
工作量: 1-2 天
```

**文档需求:**
- 架构设计文档
- API 文档
- 配置指南
- 故障排查指南
- 性能调优指南

---

## 📋 贡献流程

### 第一步: 选择任务

1. 查看上面的贡献机会
2. 在 GitHub Issues 中搜索相关任务
3. 如果没有，创建新的 Issue 讨论

### 第二步: 设计和讨论

1. 在 Issue 中提出设计方案
2. 等待 maintainer 反馈
3. 根据反馈调整方案
4. 获得 "ready to implement" 标签

### 第三步: 实现

1. Fork 仓库
2. 创建特性分支: `git checkout -b feat/egress-ebpf`
3. 实现功能
4. 添加测试
5. 添加文档

### 第四步: 提交 PR

1. 确保所有测试通过
2. 运行 `make fmt` 和 `make lint`
3. 提交 PR，引用相关 Issue
4. 等待 code review

### 第五步: 合并

1. 解决 review 意见
2. 获得至少 2 个 approve
3. Maintainer 合并 PR

---

## 🎓 学习资源

### 网络编程
- [Linux 网络编程](https://man7.org/linux/man-pages/man7/socket.7.html)
- [Go net 包文档](https://pkg.go.dev/net)
- [iptables 教程](https://www.netfilter.org/documentation/)

### eBPF
- [Cilium eBPF 库](https://github.com/cilium/ebpf)
- [Linux eBPF 文档](https://ebpf.io/)
- [BCC 工具](https://github.com/iovisor/bcc)

### Wireguard
- [Wireguard 官网](https://www.wireguard.com/)
- [wireguard-go](https://github.com/WireGuard/wireguard-go)

### Cilium
- [Cilium 文档](https://docs.cilium.io/)
- [Cilium GitHub](https://github.com/cilium/cilium)

---

## 💻 开发环境设置

```bash
# 克隆仓库
git clone https://github.com/e2b-dev/infra.git
cd infra

# 切换到 main-ada 分支
git checkout main-ada

# 安装依赖
make tidy

# 运行测试
make test

# 运行 linter
make lint

# 构建
make build/orchestrator
```

---

## 🧪 测试指南

### 单元测试
```bash
cd packages/orchestrator
go test -v ./pkg/tcpfirewall/...
```

### 集成测试
```bash
cd packages/orchestrator
go test -v -tags=integration ./tests/...
```

### 性能测试
```bash
cd packages/orchestrator
go test -bench=. -benchmem ./pkg/tcpfirewall/...
```

---

## 📝 代码风格

### 命名规范
```go
// 接口: 以 er 结尾
type EgressProxy interface {}

// 实现: 具体名称
type tcpfirewall struct {}

// 方法: 动词开头
func (p *tcpfirewall) OnSlotCreate() {}

// 常量: 大写
const DefaultTimeout = 30 * time.Second

// 变量: 驼峰
var maxConnections int
```

### 注释规范
```go
// Package tcpfirewall 实现基于 TCP 防火墙的网络代理
package tcpfirewall

// Proxy 是 EgressProxy 的 TCP 防火墙实现
type Proxy struct {
    // logger 用于记录日志
    logger Logger
}

// Start 启动代理服务
func (p *Proxy) Start(ctx context.Context) error {
    // 实现
}
```

---

## 🚀 优先级指南

### 高优先级 (立即开始)
- [ ] 连接池优化
- [ ] 规则匹配优化
- [ ] 集成测试套件

### 中优先级 (1-2 周)
- [ ] 流量统计
- [ ] 动态规则更新
- [ ] 性能基准测试

### 低优先级 (未来)
- [ ] eBPF 实现
- [ ] Wireguard 实现
- [ ] Cilium 集成

---

## 📞 获取帮助

- **GitHub Issues**: 提问和讨论
- **Discussions**: 设计和架构讨论
- **Slack**: 实时沟通 (如果有)
- **Email**: 联系 maintainer

---

## ✅ 检查清单

提交 PR 前，请确保:

- [ ] 代码通过 `make fmt`
- [ ] 代码通过 `make lint`
- [ ] 所有测试通过 `make test`
- [ ] 添加了新的测试
- [ ] 添加了文档
- [ ] 提交信息清晰
- [ ] 没有 merge conflicts
- [ ] 引用了相关 Issue

---

## 🎉 感谢

感谢您对 E2B 基础设施的贡献！

您的贡献帮助我们构建更好的沙箱执行环境。
