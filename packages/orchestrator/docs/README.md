# Orchestrator 文档

## 📚 文档导航

### 核心架构

#### [EgressProxy 系统](./EGRESS_PROXY.md)
完整的 EgressProxy 架构文档，包括：
- 接口定义和设计原理
- 4 种实现方式详解
- 策略模式应用
- 设计决策和权衡
- 代码流程示例
- 未来扩展方向

**适合:** 想要理解网络代理系统的开发者

---

#### [EgressProxy 架构图](./EGRESS_PROXY_ARCHITECTURE.md)
可视化架构文档，包括：
- 整体架构图
- 依赖关系图
- 流程序列图
- 类图
- 网络流量处理流程
- 生命周期状态图
- 实现选择决策树
- 未来扩展点

**适合:** 视觉学习者，需要快速理解系统结构

---

#### [EgressProxy 快速参考](./EGRESS_PROXY_QUICK_REF.md)
快速查询指南，包括：
- 四种实现方式速览
- 接口定义
- 注入点
- 对比表
- 添加新实现的步骤
- 设计模式总结
- 关键方法说明
- 文件结构
- 学习路径
- 常见问题

**适合:** 需要快速查询信息的开发者

---

### 社区贡献

#### [EgressProxy 社区贡献指南](./EGRESS_PROXY_CONTRIBUTING.md)
详细的贡献指南，包括：
- 4 类贡献机会（新实现、性能优化、功能增强、测试文档）
- 具体的改进建议和工作量估计
- 完整的贡献流程
- 学习资源链接
- 开发环境设置
- 测试指南
- 代码风格规范
- 优先级指南
- 检查清单

**适合:** 想要为项目做贡献的开发者

---

## 🎯 快速开始

### 我想...

#### 理解 EgressProxy 系统
1. 先读 [快速参考](./EGRESS_PROXY_QUICK_REF.md) (5 分钟)
2. 再看 [架构图](./EGRESS_PROXY_ARCHITECTURE.md) (10 分钟)
3. 最后读 [完整文档](./EGRESS_PROXY.md) (30 分钟)

#### 添加新的网络实现
1. 读 [快速参考](./EGRESS_PROXY_QUICK_REF.md) 了解接口
2. 查看 [完整文档](./EGRESS_PROXY.md) 中的代码示例
3. 参考 [贡献指南](./EGRESS_PROXY_CONTRIBUTING.md) 中的实现步骤

#### 优化现有实现
1. 读 [贡献指南](./EGRESS_PROXY_CONTRIBUTING.md) 中的优化建议
2. 查看 [完整文档](./EGRESS_PROXY.md) 中的设计决策
3. 参考性能基准测试部分

#### 为项目做贡献
1. 阅读 [贡献指南](./EGRESS_PROXY_CONTRIBUTING.md)
2. 选择感兴趣的任务
3. 按照流程提交 PR

---

## 📖 文档结构

```
docs/
├── README.md                              # 本文件
├── EGRESS_PROXY.md                        # 完整文档 (30 分钟)
├── EGRESS_PROXY_ARCHITECTURE.md           # 架构图 (10 分钟)
├── EGRESS_PROXY_QUICK_REF.md              # 快速参考 (5 分钟)
└── EGRESS_PROXY_CONTRIBUTING.md           # 贡献指南 (15 分钟)
```

---

## 🔍 按主题查找

### 架构和设计
- [EgressProxy 完整文档](./EGRESS_PROXY.md) - 设计决策、权衡、模式
- [架构图](./EGRESS_PROXY_ARCHITECTURE.md) - 可视化系统结构

### 实现细节
- [快速参考](./EGRESS_PROXY_QUICK_REF.md) - 四种实现方式
- [完整文档](./EGRESS_PROXY.md) - 代码示例和流程

### 开发和贡献
- [贡献指南](./EGRESS_PROXY_CONTRIBUTING.md) - 如何参与项目
- [快速参考](./EGRESS_PROXY_QUICK_REF.md) - 学习路径

### 性能和优化
- [贡献指南](./EGRESS_PROXY_CONTRIBUTING.md) - 优化建议
- [完整文档](./EGRESS_PROXY.md) - 性能考虑

---

## 📊 文档对比

| 文档 | 长度 | 深度 | 用途 |
|------|------|------|------|
| 快速参考 | 5 分钟 | 浅 | 快速查询 |
| 架构图 | 10 分钟 | 中 | 可视化理解 |
| 完整文档 | 30 分钟 | 深 | 深入学习 |
| 贡献指南 | 15 分钟 | 中 | 参与项目 |

---

## 🎓 学习路径

### 初学者
1. [快速参考](./EGRESS_PROXY_QUICK_REF.md) - 了解基础
2. [架构图](./EGRESS_PROXY_ARCHITECTURE.md) - 理解结构
3. [完整文档](./EGRESS_PROXY.md) - 深入学习

### 开发者
1. [快速参考](./EGRESS_PROXY_QUICK_REF.md) - 快速查询
2. [完整文档](./EGRESS_PROXY.md) - 理解设计
3. 查看源代码 - 学习实现

### 贡献者
1. [贡献指南](./EGRESS_PROXY_CONTRIBUTING.md) - 了解流程
2. [完整文档](./EGRESS_PROXY.md) - 理解系统
3. [快速参考](./EGRESS_PROXY_QUICK_REF.md) - 快速查询

---

## 🔗 相关代码

### 接口定义
```
packages/orchestrator/pkg/sandbox/network/egressproxy.go
```

### 生产实现
```
packages/orchestrator/pkg/tcpfirewall/proxy.go
packages/orchestrator/pkg/tcpfirewall/handlers.go
packages/orchestrator/pkg/tcpfirewall/listener.go
packages/orchestrator/pkg/tcpfirewall/metrics.go
```

### 注入点
```
packages/orchestrator/main.go
packages/orchestrator/pkg/factories/run.go
```

### 测试实现
```
packages/orchestrator/pkg/sandbox/envd_test.go
packages/orchestrator/cmd/resume-build/main.go
```

---

## ❓ 常见问题

**Q: 从哪里开始？**
A: 如果你是第一次接触，从 [快速参考](./EGRESS_PROXY_QUICK_REF.md) 开始。

**Q: 如何添加新的实现？**
A: 查看 [贡献指南](./EGRESS_PROXY_CONTRIBUTING.md) 中的"新的网络实现"部分。

**Q: 如何优化现有实现？**
A: 查看 [贡献指南](./EGRESS_PROXY_CONTRIBUTING.md) 中的"性能优化"部分。

**Q: 如何参与项目？**
A: 阅读 [贡献指南](./EGRESS_PROXY_CONTRIBUTING.md) 的完整内容。

**Q: 代码在哪里？**
A: 查看上面的"相关代码"部分。

---

## 📝 文档维护

这些文档与代码同步维护。如果发现不一致，请提交 Issue 或 PR。

### 更新文档的步骤
1. 修改相关文档
2. 验证代码示例
3. 更新架构图（如需要）
4. 提交 PR

---

## 🎉 贡献者

感谢所有为这些文档做出贡献的开发者！

---

## 📞 获取帮助

- 📖 查看相关文档
- 🔍 搜索 GitHub Issues
- 💬 在 Discussions 中提问
- 📧 联系 maintainer

---

**最后更新**: 2024 年
**维护者**: E2B 社区
