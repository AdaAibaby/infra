# EgressProxy 架构图

## 整体架构

```mermaid
graph TB
    subgraph "接口定义"
        A["EgressProxy Interface<br/>OnSlotCreate<br/>OnSlotDelete<br/>CABundle"]
    end
    
    subgraph "生产环境"
        B["tcpfirewall.Proxy<br/>pkg/tcpfirewall/proxy.go<br/>---<br/>✅ 完整网络操作<br/>✅ CA 证书支持<br/>✅ 连接限制<br/>✅ 指标收集"]
    end
    
    subgraph "测试/开发"
        C["NoopEgressProxy<br/>pkg/sandbox/network/<br/>---<br/>❌ 无操作<br/>❌ 无 CA 证书<br/>用于: 单元测试<br/>基准测试"]
        D["noEgressProxy<br/>cmd/resume-build/main.go<br/>---<br/>⭐ 移除默认路由<br/>用于: 特殊场景"]
        E["mockEgressProxy<br/>pkg/sandbox/envd_test.go<br/>---<br/>✅ Mock CA 证书<br/>用于: envd 测试"]
    end
    
    subgraph "注入点"
        F["EgressFactory<br/>factories/run.go<br/>---<br/>策略模式<br/>依赖倒置"]
    end
    
    subgraph "使用位置"
        G["main.go<br/>defaultEgressFactory<br/>→ tcpfirewall"]
        H["测试代码<br/>→ NoopEgressProxy"]
        I["resume-build<br/>→ noEgressProxy"]
    end
    
    A --> B
    A --> C
    A --> D
    A --> E
    
    B --> F
    C --> F
    D --> F
    E --> F
    
    F --> G
    F --> H
    F --> I
    
    style A fill:#e3f2fd
    style B fill:#c8e6c9
    style C fill:#fff9c4
    style D fill:#fff9c4
    style E fill:#fff9c4
    style F fill:#f3e5f5
    style G fill:#ffccbc
    style H fill:#ffccbc
    style I fill:#ffccbc
```

## 依赖关系图

```mermaid
graph LR
    A["main.go<br/>defaultEgressFactory"]
    B["factories.Run<br/>EgressFactory"]
    C["factories/run.go<br/>opts.EgressFactory"]
    D["tcpfirewall.Proxy<br/>生产实现"]
    E["network.EgressProxy<br/>接口"]
    F["Slot<br/>网络插槽"]
    G["iptables<br/>规则配置"]
    
    A -->|注入| B
    B -->|调用| C
    C -->|创建| D
    D -->|实现| E
    E -->|管理| F
    F -->|配置| G
    
    style A fill:#ffccbc
    style B fill:#f3e5f5
    style C fill:#f3e5f5
    style D fill:#c8e6c9
    style E fill:#e3f2fd
    style F fill:#fff9c4
    style G fill:#fff9c4
```

## 流程图

```mermaid
sequenceDiagram
    participant main as main.go
    participant factories as factories.Run
    participant run as run()
    participant factory as EgressFactory
    participant proxy as EgressProxy
    participant slot as Slot
    
    main->>factories: Run(Options{EgressFactory})
    factories->>run: run(config, opts)
    run->>factory: opts.EgressFactory(ctx, deps)
    factory->>proxy: tcpfirewall.New(...)
    proxy-->>factory: EgressSetup{Proxy, Start, Close}
    factory-->>run: EgressSetup
    
    Note over run: 启动服务
    run->>proxy: Start(ctx)
    proxy-->>run: nil
    
    Note over run: 创建网络 Slot
    run->>slot: NewSlot()
    slot->>proxy: OnSlotCreate(slot, iptables)
    proxy-->>slot: nil
    
    Note over run: 删除网络 Slot
    run->>slot: DeleteSlot()
    slot->>proxy: OnSlotDelete(slot, iptables)
    proxy-->>slot: nil
    
    Note over run: 获取 CA 证书
    run->>proxy: CABundle()
    proxy-->>run: "-----BEGIN CERTIFICATE-----..."
    
    Note over run: 关闭服务
    run->>proxy: Close(ctx)
    proxy-->>run: nil
```

## 类图

```mermaid
classDiagram
    class EgressProxy {
        <<interface>>
        +OnSlotCreate(s Slot, tables IPTables) error
        +OnSlotDelete(s Slot, tables IPTables) error
        +CABundle() string
    }
    
    class tcpfirewall_Proxy {
        -logger Logger
        -sandboxes SandboxMap
        -metrics Metrics
        -limiter ConnectionLimiter
        -featureFlags FeatureFlags
        -httpPort uint16
        -tlsPort uint16
        -otherPort uint16
        -proxyRules[] proxyRule
        -proxy TCPProxy
        +New() Proxy
        +Start(ctx Context) error
        +OnSlotCreate(s Slot, tables IPTables) error
        +OnSlotDelete(s Slot, tables IPTables) error
        +CABundle() string
    }
    
    class NoopEgressProxy {
        +OnSlotCreate(s Slot, tables IPTables) error
        +OnSlotDelete(s Slot, tables IPTables) error
        +CABundle() string
    }
    
    class noEgressProxy {
        -NoopEgressProxy
        +OnSlotCreate(s Slot, tables IPTables) error
    }
    
    class mockEgressProxy {
        -bundle string
        +OnSlotCreate(s Slot, tables IPTables) error
        +OnSlotDelete(s Slot, tables IPTables) error
        +CABundle() string
    }
    
    class EgressSetup {
        +Proxy EgressProxy
        +Start func(ctx Context) error
        +Close func(ctx Context) error
    }
    
    class EgressFactory {
        <<type>>
        +func(ctx Context, deps Deps) (EgressSetup, error)
    }
    
    EgressProxy <|.. tcpfirewall_Proxy
    EgressProxy <|.. NoopEgressProxy
    NoopEgressProxy <|-- noEgressProxy
    EgressProxy <|.. mockEgressProxy
    EgressSetup --> EgressProxy
    EgressFactory --> EgressSetup
```

## 网络流量处理流程

```mermaid
graph TD
    A["客户端请求<br/>来自沙箱"]
    B{"目标端口?"}
    C["HTTP 80"]
    D["TLS 443"]
    E["其他 TCP"]
    F["Host Header<br/>检查"]
    G["SNI<br/>检查"]
    H["CIDR<br/>检查"]
    I["查询 Sandbox Map"]
    J["允许/拒绝"]
    K["转发到目标"]
    
    A --> B
    B -->|80| C
    B -->|443| D
    B -->|其他| E
    
    C --> F
    D --> G
    E --> H
    
    F --> I
    G --> I
    H --> I
    
    I --> J
    J -->|允许| K
    J -->|拒绝| L["返回错误"]
    
    style A fill:#fff9c4
    style B fill:#f3e5f5
    style C fill:#e3f2fd
    style D fill:#e3f2fd
    style E fill:#e3f2fd
    style F fill:#c8e6c9
    style G fill:#c8e6c9
    style H fill:#c8e6c9
    style I fill:#ffccbc
    style J fill:#f3e5f5
    style K fill:#c8e6c9
    style L fill:#ffcdd2
```

## 生命周期图

```mermaid
stateDiagram-v2
    [*] --> Initialized: main.go<br/>defaultEgressFactory
    
    Initialized --> Running: Start()
    
    Running --> CreatingSlot: NewSlot()
    CreatingSlot --> SlotActive: OnSlotCreate()
    
    SlotActive --> DeletingSlot: DeleteSlot()
    DeletingSlot --> SlotDeleted: OnSlotDelete()
    
    SlotDeleted --> CreatingSlot: NewSlot()
    
    Running --> Closing: SIGTERM
    Closing --> Closed: Close()
    
    Closed --> [*]
    
    note right of Initialized
        EgressFactory 创建实例
        可以是 tcpfirewall 或 Noop
    end
    
    note right of Running
        代理服务运行
        监听网络流量
    end
    
    note right of SlotActive
        Slot 活跃
        处理流量
    end
    
    note right of Closing
        优雅关闭
        清理资源
    end
```

## 实现选择决策树

```mermaid
graph TD
    A["选择 EgressProxy 实现"]
    B{"环境?"}
    C{"需要网络操作?"}
    D{"需要 CA 证书?"}
    E{"特殊需求?"}
    
    A --> B
    B -->|生产环境| C
    B -->|测试环境| F["NoopEgressProxy"]
    B -->|特殊场景| G["noEgressProxy"]
    
    C -->|是| D
    C -->|否| F
    
    D -->|是| H["tcpfirewall.Proxy"]
    D -->|否| F
    
    E -->|Mock CA| I["mockEgressProxy"]
    E -->|移除默认路由| G
    E -->|其他| H
    
    style H fill:#c8e6c9
    style F fill:#fff9c4
    style G fill:#fff9c4
    style I fill:#fff9c4
```

## 扩展点

```mermaid
graph LR
    A["EgressProxy<br/>接口"]
    B["tcpfirewall<br/>TCP 代理"]
    C["eBPF<br/>内核级"]
    D["Wireguard<br/>隧道"]
    E["Cilium<br/>网络策略"]
    F["AWS VPC<br/>云原生"]
    
    A --> B
    A -.->|未来| C
    A -.->|未来| D
    A -.->|未来| E
    A -.->|未来| F
    
    style A fill:#e3f2fd
    style B fill:#c8e6c9
    style C fill:#fff9c4
    style D fill:#fff9c4
    style E fill:#fff9c4
    style F fill:#fff9c4
```
