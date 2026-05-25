# EgressProxy 快速参考

## 📍 四种实现方式

### 1️⃣ tcpfirewall (生产环境)
```
位置: pkg/tcpfirewall/proxy.go
用途: 生产环境网络流量控制
特点: ✅ 完整网络操作 ✅ CA证书 ✅ 连接限制 ✅ 指标
```

### 2️⃣ NoopEgressProxy (测试)
```
位置: pkg/sandbox/network/egressproxy.go
用途: 单元测试、基准测试
特点: ❌ 无操作 ❌ 无CA证书
```

### 3️⃣ noEgressProxy (特殊场景)
```
位置: cmd/resume-build/main.go
用途: resume-build 命令
特点: ⭐ 移除默认路由
```

### 4️⃣ mockEgressProxy (单元测试)
```
位置: pkg/sandbox/envd_test.go
用途: envd 相关测试
特点: ✅ Mock CA证书
```

---

## 🔌 接口定义

```go
type EgressProxy interface {
    OnSlotCreate(s *Slot, tables *iptables.IPTables) error
    OnSlotDelete(s *Slot, tables *iptables.IPTables) error
    CABundle() string
}
```

---

## 🎯 注入点

```go
// main.go
factories.Run(factories.Options{
    EgressFactory: defaultEgressFactory,  // 注入 tcpfirewall
})

// factories/run.go
egressSetup, err := opts.EgressFactory(ctx, deps)
```

---

## 📊 对比表

| 实现 | 网络操作 | CA证书 | 用途 |
|------|---------|--------|------|
| tcpfirewall | ✅ | ✅ | 生产 |
| NoopEgressProxy | ❌ | ❌ | 测试 |
| noEgressProxy | ⭐ | ❌ | 特殊 |
| mockEgressProxy | ❌ | ✅ | Mock |

---

## 🚀 添加新实现的步骤

1. 创建新包: `pkg/wireguard/proxy.go`
2. 实现 `EgressProxy` 接口
3. 创建工厂函数
4. 在 `main.go` 中选择使用

**无需修改 `run.go` 和 `factories` 包！**

---

## 💡 设计模式

- **策略模式**: 不同的实现可以互换
- **依赖倒置**: 依赖接口而不是具体实现
- **工厂模式**: 通过 `EgressFactory` 创建实例

---

## 🔍 关键方法

### OnSlotCreate
- 创建网络 Slot 时调用
- 配置 iptables 规则
- 设置网络隔离

### OnSlotDelete
- 删除网络 Slot 时调用
- 清理 iptables 规则
- 释放网络资源

### CABundle
- 返回 CA 证书
- 用于 HTTPS 流量拦截
- 沙箱内应用需要信任

---

## 📁 文件结构

```
orchestrator/
├── main.go                          # 注入点
├── pkg/
│   ├── factories/
│   │   └── run.go                  # 组合根
│   ├── tcpfirewall/
│   │   ├── proxy.go                # 生产实现
│   │   ├── handlers.go
│   │   ├── listener.go
│   │   └── metrics.go
│   └── sandbox/
│       └── network/
│           └── egressproxy.go      # 接口 + Noop
└── cmd/
    └── resume-build/
        └── main.go                 # 自定义实现
```

---

## 🎓 学习路径

1. 阅读接口定义: `egressproxy.go`
2. 学习 Noop 实现: `NoopEgressProxy`
3. 研究生产实现: `tcpfirewall/proxy.go`
4. 查看注入点: `main.go` 和 `factories/run.go`
5. 理解特殊实现: `cmd/resume-build/main.go`

---

## ❓ 常见问题

**Q: 为什么需要 EgressProxy？**
A: 控制沙箱的网络出口流量，实现网络隔离和安全策略。

**Q: 为什么分离 HTTP/TLS/Other？**
A: 不同协议需要不同的识别方式（Host header/SNI/CIDR）。

**Q: 为什么需要 CABundle？**
A: 用于 HTTPS 流量拦截和检查。

**Q: 如何添加新的实现？**
A: 实现 `EgressProxy` 接口，创建工厂函数，在 `main.go` 中注入。

---

## 🔗 相关文件

- 接口: `pkg/sandbox/network/egressproxy.go`
- 生产: `pkg/tcpfirewall/proxy.go`
- 注入: `main.go` + `pkg/factories/run.go`
- 测试: `pkg/sandbox/envd_test.go`
- 特殊: `cmd/resume-build/main.go`
- 详细文档: `docs/EGRESS_PROXY.md`
- 架构图: `docs/EGRESS_PROXY_ARCHITECTURE.md`
