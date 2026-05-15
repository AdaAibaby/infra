# Sandbox 创建过程中的数据传输分析

## 📊 总体流程

```
┌─────────────────────────────────────────────────────────────────┐
│                    创建 Sandbox 流程                             │
└─────────────────────────────────────────────────────────────────┘
                              │
                              ▼
        ┌─────────────────────────────────────────┐
        │  1. 资源初始化 (Resources Init)         │
        │     - 获取网络 IP 地址                  │
        │     - 初始化 Rootfs (NBD/Direct)       │
        │     - 加载 Memfile                      │
        │     - 创建 Cgroup                       │
        └─────────────────────────────────────────┘
                              │
                              ▼
        ┌─────────────────────────────────────────┐
        │  2. Firecracker 启动 (FC Boot)         │
        │     - 创建 FC 进程                      │
        │     - 配置 VM 参数                      │
        │     - 启动内核                          │
        └─────────────────────────────────────────┘
                              │
                              ▼
        ┌─────────────────────────────────────────┐
        │  3. Envd 初始化 (Envd Init)            │
        │     - HTTP POST /init 请求              │
        │     - 传输初始化数据                    │
        │     - 挂载 NFS 卷                       │
        │     - 设置环境变量                      │
        └─────────────────────────────────────────┘
                              │
                              ▼
        ┌─────────────────────────────────────────┐
        │  4. Sandbox 就绪 (Ready)                │
        └─────────────────────────────────────────┘
```

---

## 🔄 详细数据传输分析

### 第 1 阶段：资源初始化

#### 1.1 网络资源
```
Orchestrator → Network Pool
├─ 获取 IP 地址
├─ 配置网络接口
└─ 设置 iptables 规则
```

**数据量**：极小（几 KB）

#### 1.2 Rootfs 初始化
```
Template → Rootfs Provider
├─ NBD 方式：
│  ├─ 连接 NBD 服务器
│  ├─ 挂载块设备
│  └─ 初始化 overlay
│
└─ Direct 方式：
   ├─ 直接读取缓存文件
   └─ 初始化 overlay
```

**数据量**：取决于模板大小（通常 100MB - 5GB）

#### 1.3 Memfile 加载
```
Template → Memory
├─ 读取内存快照文件
└─ 准备内存恢复数据
```

**数据量**：取决于内存大小（通常 512MB - 2GB）

---

### 第 2 阶段：Firecracker 启动

#### 2.1 Firecracker 配置
```
Orchestrator → Firecracker API
├─ 设置 VM 配置
│  ├─ CPU 数量
│  ├─ 内存大小
│  ├─ 块设备
│  └─ 网络接口
│
├─ 加载内核
│  └─ 传输内核镜像 (~10MB)
│
└─ 启动 VM
   └─ 加载内存快照（如果有）
```

**数据量**：
- 内核镜像：~10MB
- 内存快照：取决于内存大小
- 配置数据：几 KB

#### 2.2 内核启动
```
Firecracker → Linux Kernel
├─ 初始化硬件
├─ 挂载根文件系统
├─ 启动 init 进程
└─ 执行初始化脚本
```

**数据量**：无网络传输（本地操作）

---

### 第 3 阶段：Envd 初始化 ⚠️ **关键阶段**

这是最容易超时的阶段！

#### 3.1 HTTP POST /init 请求

**请求方向**：Orchestrator → Envd (在 VM 内)

**请求体 (PostInitJSONBody)**：
```json
{
  "lifecycleID": "uuid-string",
  "envVars": {
    "KEY1": "value1",
    "KEY2": "value2",
    ...
  },
  "hyperloopIP": "10.0.0.1",
  "accessToken": "secure-token",
  "defaultUser": "root",
  "defaultWorkdir": "/home/user",
  "volumeMounts": [
    {
      "nfs_target": "10.0.0.1:/volume1",
      "path": "/mnt/volume1"
    },
    {
      "nfs_target": "10.0.0.1:/volume2",
      "path": "/mnt/volume2"
    }
  ],
  "caBundle": "-----BEGIN CERTIFICATE-----\n...\n-----END CERTIFICATE-----",
  "timestamp": "2026-05-15T11:00:47Z"
}
```

**数据量估算**：
```
基础字段：                    ~500 bytes
环境变量（假设 50 个）：      ~5-10 KB
卷挂载信息（假设 5 个）：     ~1-2 KB
CA Bundle（证书链）：        ~5-50 KB
总计：                       ~10-60 KB
```

#### 3.2 Envd 内部处理

**Envd 接收到请求后的操作**：

```
1. 验证请求
   ├─ 检查 AccessToken
   └─ 验证签名

2. 设置环境变量
   ├─ 导出所有 EnvVars
   └─ 更新 /etc/environment

3. 挂载 NFS 卷 ⚠️ **耗时操作**
   ├─ 对每个 VolumeMount：
   │  ├─ 创建挂载点目录
   │  ├─ 执行 mount 命令
   │  ├─ 等待 NFS 连接建立
   │  └─ 验证挂载成功
   └─ 总耗时：可能 1-10 秒（取决于 NFS 服务器响应）

4. 安装 CA 证书
   ├─ 解析 PEM 格式
   ├─ 写入系统信任存储
   └─ 更新证书缓存

5. 设置默认用户和工作目录
   ├─ 验证用户存在
   └─ 设置 HOME 和 PWD

6. 返回 204 No Content
```

#### 3.3 网络通信延迟

```
总延迟 = 网络往返时间 + Envd 处理时间

网络往返时间（RTT）：
├─ Orchestrator → VM 网络：1-5ms
├─ HTTP 请求序列化：<1ms
├─ HTTP 响应序列化：<1ms
└─ 总计：2-10ms

Envd 处理时间：
├─ 验证请求：<10ms
├─ 设置环境变量：<50ms
├─ 挂载 NFS 卷：500ms - 10s ⚠️ **主要瓶颈**
├─ 安装证书：<100ms
└─ 总计：500ms - 10s+
```

---

## 📈 数据传输大小总结

| 阶段 | 组件 | 数据方向 | 大小 | 耗时 |
|------|------|---------|------|------|
| 1 | 网络配置 | Orch → Pool | ~1 KB | <10ms |
| 1 | Rootfs | Template → NBD | 100MB-5GB | 1-30s |
| 1 | Memfile | Template → Memory | 512MB-2GB | 1-10s |
| 2 | 内核镜像 | Orch → FC | ~10MB | <100ms |
| 2 | VM 配置 | Orch → FC | ~10 KB | <10ms |
| 2 | 内存恢复 | FC → Memory | 512MB-2GB | 1-10s |
| 3 | Init 请求 | Orch → Envd | 10-60 KB | 2-10ms |
| 3 | NFS 挂载 | Envd → NFS | 控制信号 | 500ms-10s ⚠️ |
| 3 | 证书安装 | Envd → System | 5-50 KB | <100ms |

---

## ⏱️ 超时问题分析

### 当前超时配置

```go
// 单个请求超时
EnvdInitRequestTimeout = 50000ms (50 秒)

// 整体初始化超时
EnvdTimeout = 10s (10 秒)
```

### 问题场景

```
时间轴：
0ms    ├─ WaitForEnvd 启动，设置 10s 总超时
       │
100ms  ├─ Firecracker 启动
       │
2000ms ├─ 内核启动完成
       │
2500ms ├─ Envd 进程启动
       │
2600ms ├─ 第 1 次 HTTP 请求发送
       │  └─ 响应超时（NFS 挂载慢）
       │
2605ms ├─ 重试（间隔 5ms）
       │
...    ├─ 多次重试...
       │
10000ms├─ ⚠️ 总超时触发！
       │  └─ "syncing took too long"
       │
10500ms├─ 实际 NFS 挂载完成（太晚了！）
```

### 为什么会超时？

1. **NFS 挂载延迟**
   - 网络延迟
   - NFS 服务器响应慢
   - 多个卷挂载串行执行

2. **单个请求超时太长**
   - 50s 的单个请求超时
   - 但总超时只有 10s
   - 导致无法完成即使快速的请求

3. **重试机制**
   - 每次重试间隔 5ms
   - 179 次重试 ≈ 895ms
   - 加上网络延迟，容易超过 10s

---

## ✅ 解决方案

### 方案 1：增加 EnvdTimeout（推荐）

```bash
# 在 .env 文件中
ENVD_TIMEOUT=30s  # 改成 30 秒
```

**优点**：
- 给 NFS 挂载充足时间
- 不影响单个请求超时
- 简单易行

**缺点**：
- 如果 envd 真的卡住，需要等 30s 才能发现

### 方案 2：优化 NFS 挂载

```go
// 并行挂载多个卷，而不是串行
// 可以将 5 个卷的挂载时间从 5s 减少到 1s
```

**优点**：
- 根本解决问题
- 提高整体性能

**缺点**：
- 需要修改 envd 代码
- 需要测试

### 方案 3：调整两个超时

```go
// 特性标志
EnvdInitRequestTimeout = 5000ms (5 秒)  // 减少单个请求超时

// 环境变量
ENVD_TIMEOUT = 30s  // 增加总超时
```

**优点**：
- 更合理的超时配置
- 快速发现真正的问题

**缺点**：
- 需要同时修改两个地方

---

## 📝 建议

**立即采取**：
1. 修改 `ENVD_TIMEOUT` 从 10s 改为 30s
2. 重新编译 orchestrator
3. 重新启动服务

**后续优化**：
1. 分析 NFS 挂载的具体延迟
2. 考虑并行挂载卷
3. 添加详细的日志记录挂载时间

---

## 🔍 调试建议

### 查看 Envd 日志

```bash
# 在 VM 内查看 envd 日志
journalctl -u envd -f

# 或查看 envd 的标准输出
tail -f /var/log/envd.log
```

### 添加性能指标

```go
// 在 envd 中添加每个操作的耗时
start := time.Now()
// 挂载 NFS
duration := time.Since(start)
logger.Infof("NFS mount took %dms", duration.Milliseconds())
```

### 监控网络延迟

```bash
# 从 orchestrator 到 VM 的 ping 延迟
ping <vm-ip>

# 检查 NFS 服务器响应
nfsstat -s
```
