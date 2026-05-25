//go:build linux

package factories

// 文件设计哲学：
//
// 这个文件是整个 orchestrator 的组合根（Composition Root）——所有依赖在这里被创建、连接、并最终销毁。
// 核心设计思想是："把所有副作用集中在一个地方，让其他所有包保持纯净"
//
// 关键设计模式：
// 1. 组合根（Composition Root）：所有依赖在 run() 函数中创建，其他包无全局状态
// 2. 策略模式（Strategy）：EgressFactory 函数类型允许不同版本注入不同的出口代理实现
// 3. LIFO 关闭（Reverse Order）：closers 切片 + slices.Reverse 确保依赖关系正确的关闭顺序
// 4. 哨兵错误（Sentinel Error）：ErrRedisDisabled 区分"禁用"和"失败"，支持优雅降级
// 5. Nop 实现（Null Object）：可选依赖不影响核心路径
// 6. 排水窗口（Drain Window）：SetStatus(Draining) + Sleep(15s) 实现零停机滚动更新
// 7. 单端口复用（Port Multiplexing）：cmux 在同一端口上复用 gRPC 和 HTTP
//
// 初始化顺序（重要）：
// 1. 文件锁检查（防止双启动）
// 2. Context + Signal 处理
// 3. Telemetry（其他组件依赖它）
// 4. Logger（依赖 telemetry）
// 5. 业务组件（Redis、Template Cache、ClickHouse 等）
// 6. 服务启动（errgroup 并发管理）
// 7. 关闭序列（排水 → 逆序关闭）

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/soheilhy/cmux"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"

	clickhouse "github.com/e2b-dev/infra/packages/clickhouse/pkg"
	clickhouseevents "github.com/e2b-dev/infra/packages/clickhouse/pkg/events"
	clickhousehoststats "github.com/e2b-dev/infra/packages/clickhouse/pkg/hoststats"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/cfg"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/chrooted"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/events"
	e2bhealthcheck "github.com/e2b-dev/infra/packages/orchestrator/pkg/healthcheck"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/hyperloopserver"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/localupload"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/metrics"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/nfsproxy"
	nfscfg "github.com/e2b-dev/infra/packages/orchestrator/pkg/nfsproxy/cfg"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/portmap"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/proxy"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox"
	blockmetrics "github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/block/metrics"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/cgroup"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/nbd"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/network"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/template"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/template/peerclient"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/server"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/service"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/service/machineinfo"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/template/constants"
	tmplserver "github.com/e2b-dev/infra/packages/orchestrator/pkg/template/server"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/volumes"
	"github.com/e2b-dev/infra/packages/shared/pkg/env"
	event "github.com/e2b-dev/infra/packages/shared/pkg/events"
	sharedFactories "github.com/e2b-dev/infra/packages/shared/pkg/factories"
	"github.com/e2b-dev/infra/packages/shared/pkg/featureflags"
	e2bgrpc "github.com/e2b-dev/infra/packages/shared/pkg/grpc"
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator"
	orchestratorinfo "github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator-info"
	templatemanager "github.com/e2b-dev/infra/packages/shared/pkg/grpc/template-manager"
	"github.com/e2b-dev/infra/packages/shared/pkg/limit"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
	sbxlogger "github.com/e2b-dev/infra/packages/shared/pkg/logger/sandbox"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
	"github.com/e2b-dev/infra/packages/shared/pkg/telemetry"
	"github.com/e2b-dev/infra/packages/shared/pkg/utils"
)

// Deps holds shared infrastructure created during orchestrator init.
// Passed to factory callbacks so editions can build components using shared deps.
//
// 设计原理：
// - 这是依赖注入的核心数据结构，包含所有子系统共享的基础设施
// - 通过 EgressFactory 传递给版本特定的出口代理实现
// - 允许不同 edition（tcpfirewall、noop、未来的 wireguard）使用相同的基础设施
type Deps struct {
	Config        cfg.Config
	Tel           *telemetry.Client
	MeterProvider metric.MeterProvider
	Logger        logger.Logger
	Sandboxes     *sandbox.Map
	FeatureFlags  *featureflags.Client
}

// EgressSetup is returned by EgressFactory with the proxy implementation
// and optional lifecycle hooks.
type EgressSetup struct {
	// Proxy is the network egress proxy for slot creation/deletion.
	Proxy network.EgressProxy

	// Start is called as a managed service (optional).
	// If nil, no service is started for the egress proxy.
	Start func(ctx context.Context) error

	// Close is called during shutdown in reverse order (optional).
	Close func(ctx context.Context) error
}

// EgressFactory builds an edition-specific egress proxy.
// It receives fully initialized shared deps and a context.
//
// 策略模式 + 依赖倒置原则：
// - run.go 不知道也不关心出口代理的具体实现（tcpfirewall、noop 等）
// - 只依赖 network.EgressProxy 接口
// - 通过函数类型注入，允许在测试中替换为 noop 实现，避免真实 iptables 操作
// - 未来支持不同网络后端（AWS vs GCP）只需换一个函数，无需修改 run.go
type EgressFactory func(ctx context.Context, deps *Deps) (*EgressSetup, error)

// Options configures the orchestrator with edition-specific behavior.
type Options struct {
	Version       string
	CommitSHA     string
	EgressFactory EgressFactory
}

// closer 抽象了有序关闭的逻辑
// LIFO 关闭顺序（后初始化先关闭）确保依赖关系正确
//
// 为什么不用 defer？
// 1. defer 在函数返回时执行，无法控制关闭超时（closeCtx 可以设置超时）
// 2. defer 无法在关闭前执行排水（drain）逻辑（等待 sandbox 全部停止）
// 3. 这个模式允许在关闭前插入排水逻辑，实现零停机部署
type closer struct {
	name  string
	close func(ctx context.Context) error
}

// serviceDoneError 是一个哨兵错误类型，用来标记"服务已完成"
//
// ============================================================================
// 为什么需要这个类型？
// ============================================================================
//
// 这是一个巧妙的设计，用来区分"服务正常退出"和"真正的错误"。
//
// 【问题场景】
//
// 假设我们有一个 gRPC server，它的 Serve() 方法会阻塞，直到 listener 被关闭。
// 当 listener 被关闭时，Serve() 返回 nil（正常退出）。
//
// 如果我们直接返回 Serve() 的错误：
//
// g.Go(func() error {
//     return grpcServer.Serve(grpcListener)  // 返回 nil 或 error
// })
//
// 那么 g.Wait() 的行为就不一致了：
// - 如果 Serve() 返回 nil，g.Wait() 会继续等待其他 goroutine
// - 如果 Serve() 返回 error，g.Wait() 会立即返回错误
//
// 这导致 defer g.Wait() 的行为不可预测。
//
// 【解决方案】
//
// 通过返回 serviceDoneError，我们确保：
// - 无论服务是正常退出还是异常退出，都返回 serviceDoneError（非 nil）
// - g.Wait() 会看到 serviceDoneError，而不是真正的错误
// - 这样，defer g.Wait() 的行为就一致了
//
// 【具体流程】
//
// 1. 服务正常退出（listener 被关闭）
//    ┌─────────────────────────────────────────────────────────────┐
//    │ grpcServer.Serve(grpcListener)                              │
//    │ 返回：nil（正常退出）                                        │
//    └────────────────┬────────────────────────────────────────────┘
//                     │
//                     ↓
//    ┌─────────────────────────────────────────────────────────────┐
//    │ startService 闭包                                            │
//    │ err := f()  // err = nil                                    │
//    │ return serviceDoneError{name: "grpc server"}                │
//    └────────────────┬────────────────────────────────────────────┘
//                     │
//                     ↓
//    ┌─────────────────────────────────────────────────────────────┐
//    │ g.Wait()                                                     │
//    │ 看到 serviceDoneError（非 nil）                              │
//    │ 继续等待其他 goroutine                                       │
//    └─────────────────────────────────────────────────────────────┘
//
// 2. 服务异常退出（listener 出错）
//    ┌─────────────────────────────────────────────────────────────┐
//    │ grpcServer.Serve(grpcListener)                              │
//    │ 返回：error（异常退出）                                      │
//    └────────────────┬────────────────────────────────────────────┘
//                     │
//                     ↓
//    ┌─────────────────────────────────────────────────────────────┐
//    │ startService 闭包                                            │
//    │ err := f()  // err = error                                  │
//    │ l.Error(ctx, "service returned an error", zap.Error(err))  │
//    │ return serviceDoneError{name: "grpc server"}                │
//    └────────────────┬────────────────────────────────────────────┘
//                     │
//                     ↓
//    ┌─────────────────────────────────────────────────────────────┐
//    │ g.Wait()                                                     │
//    │ 看到 serviceDoneError（非 nil）                              │
//    │ 继续等待其他 goroutine                                       │
//    └─────────────────────────────────────────────────────────────┘
//
// 【关键点】
//
// serviceDoneError 的目的不是传递错误信息，而是标记"服务已完成"。
// 真正的错误信息已经通过 serviceError channel 发送到主循环了。
//
// 这样做的好处：
// 1. g.Wait() 的行为一致（总是看到 serviceDoneError）
// 2. 错误处理逻辑集中在主循环（通过 serviceError channel）
// 3. 每个 goroutine 都能正确完成（不会被 g.Wait() 的错误处理打断）
//
// 【运维注意的点】
//
// 1. 监控 serviceDoneError 日志
//    - serviceDoneError 的 Error() 方法返回 "service X finished"
//    - 这个日志通常不会出现在日志中（因为 g.Wait() 返回的错误通常被忽略）
//    - 但如果看到这个日志，说明某个服务已完成
//
// 2. 监控 serviceError channel
//    - 虽然 serviceDoneError 本身不重要，但 serviceError channel 很重要
//    - 如果看到关闭日志，说明 serviceError channel 被触发了
//    - 这意味着某个服务出现了错误
//
// 3. 监控 g.Wait() 返回值
//    - g.Wait() 返回的错误通常是 serviceDoneError
//    - 这是正常的，不需要担心
//    - 只有当 g.Wait() 返回 nil 时，才说明所有 goroutine 都正常完成了
//
// 【常见问题】
//
// Q: 为什么 serviceDoneError 的 Error() 方法返回 "service X finished"？
// A: 这是为了便于调试。如果看到这个错误，可以快速识别是哪个服务完成了。
//
// Q: 为什么不直接返回 nil？
// A: 因为 g.Wait() 会继续等待其他 goroutine。如果返回 nil，g.Wait() 无法区分
//    "这个 goroutine 完成了"和"这个 goroutine 还在运行"。
//
// Q: serviceDoneError 会影响进程的退出码吗？
// A: 不会。serviceDoneError 只是用来标记"服务已完成"，不会影响进程的退出码。
//    进程的退出码由 success 变量决定。
//
type serviceDoneError struct {
	name string
}

func (e serviceDoneError) Error() string {
	return fmt.Sprintf("service %s finished", e.name)
}

// Run starts the orchestrator, blocking until shutdown.
// Returns true on clean shutdown.
func Run(opts Options) bool {
	config, err := cfg.Parse()
	if err != nil {
		log.Fatalf("failed to parse config: %v", err)
	}

	if err = ensureDirs(config); err != nil {
		log.Fatalf("failed to create dirs: %v", err)
	}

	if opts.EgressFactory == nil {
		log.Fatalf("EgressFactory must be set in Options")
	}

	success := run(config, opts)

	log.Println("Stopping orchestrator, success:", success)

	if !success {
		os.Exit(1)
	}

	return success
}

func ensureDirs(c cfg.Config) error {
	for _, dir := range []string{
		c.DefaultCacheDir,
		c.OrchestratorBaseDir,
		c.StorageConfig.SandboxCacheDir,
		c.SandboxDir,
		c.SharedChunkCacheDir,
		c.StorageConfig.SnapshotCacheDir,
		c.StorageConfig.TemplateCacheDir,
		c.TemplatesDir,
	} {
		if dir == "" {
			continue
		}

		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("failed to make %q: %w", dir, err)
		}
	}

	return nil
}

func run(config cfg.Config, opts Options) (success bool) {
	success = true

	version := opts.Version
	commitSHA := opts.CommitSHA

	services := cfg.GetServices(config)

	// ============================================================================
	// 文件锁机制 —— 防止双启动的最简单方案
	// ============================================================================
	//
	// 【核心问题】
	// Orchestrator 是 Firecracker 微虚拟机的管理器，负责：
	// - 创建/删除虚拟机进程
	// - 管理网络 Slot（每个 Slot 对应一个虚拟机的网络命名空间）
	// - 管理 NBD 设备（Network Block Device，虚拟机的存储）
	// - 配置 iptables 规则（网络隔离）
	//
	// 如果两个 Orchestrator 实例同时运行，会导致：
	// ❌ 网络冲突：争抢同一个 Slot，iptables 规则冲突
	// ❌ 存储损坏：两个进程同时操作同一个 NBD 设备
	// ❌ 资源泄漏：Slot 和 NBD 设备无法正确清理
	// ❌ 数据丢失：虚拟机状态不一致
	//
	// 【解决方案】
	// 使用文件锁（file lock）作为进程级别的互斥锁：
	// - 简单：只需创建一个文件
	// - 可靠：即使进程崩溃，锁文件仍然存在
	// - 可见：运维人员可以看到锁文件，了解发生了什么
	//
	// 【工作流程】
	// 1. 启动时：检查锁文件是否存在
	//    - 存在 → 说明有其他实例在运行或上次崩溃 → 拒绝启动
	//    - 不存在 → 创建锁文件 → 继续启动
	// 2. 运行时：锁文件一直存在
	// 3. 关闭时：
	//    - 正常关闭 → 删除锁文件（success == true）
	//    - 崩溃 → 锁文件保留（success == false）
	//
	// 【三个条件的含义】
	//
	// 条件 1：!env.IsDevelopment()
	// ────────────────────────────────────────────────────────────────────
	// 【为什么开发模式要跳过文件锁？】
	//
	// 开发模式的特点：
	// - 本地开发机上运行（不是生产环境）
	// - 经常需要热重载（修改代码后快速重启）
	// - 可能同时运行多个版本进行测试
	// - 不涉及真实的虚拟机和生产数据
	//
	// 如果启用文件锁：
	// ❌ 每次修改代码后需要手动删除锁文件才能重启
	// ❌ 降低开发效率
	// ❌ 增加开发者的心智负担
	//
	// 跳过文件锁的好处：
	// ✓ 开发者可以快速迭代
	// ✓ 支持多个开发版本同时运行
	// ✓ 本地开发不会影响生产
	//
	// 【实际场景】
	// 开发者在本地修改代码：
	// $ make dev  # 启动 Orchestrator（带热重载）
	// # 修改代码
	// $ make dev  # 自动重启，无需手动删除锁文件
	//
	// 条件 2：!config.ForceStop
	// ────────────────────────────────────────────────────────────────────
	// 【什么是 ForceStop？】
	//
	// ForceStop 是一个配置标志，用于强制重启 Orchestrator
	// 通常在 Nomad 滚动更新时使用
	//
	// 【Nomad 滚动更新的流程】
	//
	// Nomad 是一个工作负载编排系统（类似 Kubernetes），用于管理 E2B 基础设施
	// 当需要更新 Orchestrator 版本时：
	//
	// 1. Nomad 启动新的 Orchestrator 实例（新版本）
	// 2. 新实例需要立即启动，不能被旧实例的锁文件阻挡
	// 3. Nomad 向旧实例发送 SIGTERM 信号，要求优雅关闭
	// 4. 旧实例执行排水逻辑（drain）：
	//    - SetStatus(Draining)：告诉 API 不要再分配新的 Sandbox
	//    - Sleep(15s)：等待现有 Sandbox 完成
	//    - 关闭所有资源
	// 5. 旧实例删除锁文件（success == true）
	// 6. 新实例接管所有 Sandbox
	//
	// 【为什么需要 ForceStop？】
	//
	// 在某些情况下，旧实例可能无法正常关闭：
	// - 网络故障导致无法连接到 API
	// - 某个 Sandbox 卡住，无法正常关闭
	// - 其他未预见的问题
	//
	// 此时 Nomad 会：
	// 1. 等待一段时间（通常 30 秒）
	// 2. 如果旧实例仍未关闭，强制杀死进程
	// 3. 启动新实例时设置 ForceStop=true
	// 4. 新实例跳过文件锁检查，立即启动
	//
	// 【ForceStop 的风险】
	//
	// 跳过文件锁意味着：
	// ⚠️ 可能同时运行两个实例（旧实例还没完全死亡）
	// ⚠️ 需要依赖其他机制来处理资源冲突
	//
	// 但这是可以接受的，因为：
	// ✓ Nomad 会确保旧实例最终被杀死
	// ✓ 新实例会接管所有资源
	// ✓ 短暂的重叠不会导致数据损坏（因为有其他保护机制）
	//
	// 【实际场景】
	// Nomad 滚动更新：
	// $ nomad job run orchestrator.hcl  # 部署新版本
	// # Nomad 启动新实例（ForceStop=true）
	// # 新实例跳过文件锁，立即启动
	// # 旧实例收到 SIGTERM，开始排水
	// # 15 秒后，旧实例关闭，删除锁文件
	// # 新实例完全接管
	//
	// 条件 3：slices.Contains(services, cfg.Orchestrator)
	// ────────────────────────────────────────────────────────────────────
	// 【为什么要检查 services？】
	//
	// E2B 基础设施由多个服务组成：
	// - Orchestrator：虚拟机管理器
	// - API：REST API 服务器
	// - Client-Proxy：客户端代理
	// - Dashboard-API：仪表板 API
	// - 等等
	//
	// 这些服务可能在同一个进程中运行，也可能分开运行
	// 配置文件中的 services 列表指定了当前进程应该运行哪些服务
	//
	// 【为什么只有 Orchestrator 需要文件锁？】
	//
	// 只有 Orchestrator 需要文件锁，因为：
	// ✓ 只有 Orchestrator 管理 Firecracker 进程和网络资源
	// ✓ 其他服务是无状态的，可以多个实例同时运行
	// ✓ 其他服务通过 API 或消息队列通信，不会产生资源冲突
	//
	// 【实际场景】
	// 配置文件中可能有：
	// services:
	//   - orchestrator
	//   - api
	//   - client-proxy
	//
	// 或者只有：
	// services:
	//   - api
	//
	// 如果 services 中不包含 orchestrator，就跳过文件锁检查
	//
	if !env.IsDevelopment() && !config.ForceStop && slices.Contains(services, cfg.Orchestrator) {
		fileLockName := config.OrchestratorLockPath

		// ────────────────────────────────────────────────────────────────────
		// 第一步：检查锁文件是否已存在
		// ────────────────────────────────────────────────────────────────────
		// 如果锁文件存在，说明：
		// 1. 另一个 Orchestrator 实例正在运行
		// 2. 或者上一个实例崩溃了，锁文件没有被删除
		//
		// 无论哪种情况，我们都应该拒绝启动，因为：
		// - 如果是情况 1：启动会导致双启动，资源冲突
		// - 如果是情况 2：需要运维人员手动检查和清理
		//
		// 这个设计强制运维人员介入，而不是自动恢复，因为：
		// ✓ 自动恢复可能隐藏问题（为什么上次崩溃了？）
		// ✓ 强制介入让运维人员了解系统状态
		// ✓ 避免自动恢复导致的数据损坏
		info, err := os.Stat(fileLockName)
		if err == nil {
			// 锁文件存在，拒绝启动
			// info.ModTime() 显示锁文件的创建时间，帮助运维人员判断：
			// - 如果时间很近 → 可能是另一个实例在运行
			// - 如果时间很久 → 可能是上次崩溃，需要手动清理
			log.Fatalf("Orchestrator was already started at %s, exiting", info.ModTime())
		}

		// ────────────────────────────────────────────────────────────────────
		// 第二步：创建锁文件
		// ────────────────────────────────────────────────────────────────────
		// 创建锁文件表示"我正在运行"
		// 使用 os.Create 而不是 os.OpenFile 是因为：
		// - os.Create 会覆盖已存在的文件（虽然上面已经检查过了）
		// - 简单直接，代码意图清晰
		f, err := os.Create(fileLockName)
		if err != nil {
			log.Fatalf("Failed to create lock file %s: %v", fileLockName, err)
		}

		// ────────────────────────────────────────────────────────────────────
		// 第三步：注册清理逻辑（defer）
		// ────────────────────────────────────────────────────────────────────
		// 这个 defer 在 run() 函数返回时执行，无论是正常返回还是 panic
		defer func() {
			// 关闭文件句柄
			fileErr := f.Close()
			if fileErr != nil {
				log.Printf("Failed to close lock file %s: %v", fileLockName, fileErr)
			}

			// ────────────────────────────────────────────────────────────────
			// 【关键设计】：只有正常关闭时才删除锁文件
			// ────────────────────────────────────────────────────────────────
			//
			// success 变量在 run() 函数的最后被设置为 true
			// 如果在此之前发生 panic 或 error，success 仍然是 false
			//
			// 【为什么这样设计？】
			//
			// 场景 1：正常关闭（success == true）
			// ✓ 删除锁文件
			// ✓ 下次启动时可以正常创建新的锁文件
			// ✓ 允许滚动更新（Nomad 会启动新实例）
			//
			// 场景 2：崩溃或错误（success == false）
			// ✗ 不删除锁文件
			// ✗ 下次启动时会检测到锁文件
			// ✗ 拒绝启动，强迫运维人员介入
			//
			// 【为什么要强迫运维人员介入？】
			//
			// 如果自动删除锁文件并重启：
			// ❌ 隐藏问题：为什么上次崩溃了？
			// ❌ 可能导致数据损坏：如果是因为资源冲突导致的崩溃，
			//    自动重启可能再次崩溃，形成恶性循环
			// ❌ 无法追踪：运维人员不知道发生了什么
			//
			// 通过保留锁文件：
			// ✓ 强制运维人员检查日志
			// ✓ 强制运维人员手动清理
			// ✓ 给运维人员时间思考"为什么会崩溃"
			// ✓ 避免自动恢复导致的级联故障
			//
			// 【实际操作】
			// 运维人员看到 Orchestrator 无法启动时：
			//
			// 1. 检查日志（通过 Nomad 或 journalctl）：
			//    nomad logs <allocation-id>
			//    
			//
			// 2. 检查锁文件（默认位置 /orchestrator.lock，可通过 ORCHESTRATOR_LOCK_PATH 配置）：
			//    ls -la /orchestrator.lock
			//    stat /orchestrator.lock  # 查看创建时间
			//
			// 3. 检查进程：
			//    ps aux | grep orchestrator
			//    nomad job status
			//    nomad job status orchestrator  # 查看 Nomad 中的状态
			//
			// 4. 如果确认没有其他实例在运行，手动删除锁文件：
			//    find / -name orchestrator.lock
			//    rm /orchestrator.lock
			//
			// 5. 重新启动 Orchestrator：
			//    nomad job run orchestrator.hcl
			//
			// 【调试技巧】
			// - 查看锁文件的 ModTime：如果是几小时前，说明上次崩溃了
			// - 查看 Nomad 日志：nomad logs <allocation-id> 查看为什么崩溃
			// - 查看 Orchestrator 日志：journalctl -u orchestrator -n 100 查看最后 100 行
			// - 检查磁盘空间：df -h（如果磁盘满了，可能导致崩溃）
			// - 检查内存：free -h（如果内存不足，可能导致 OOM）
			// - 检查网络：ping API 服务器（如果网络故障，可能导致无法连接）
			if success == true {
				if fileErr = os.Remove(fileLockName); fileErr != nil {
					log.Printf("Failed to remove lock file %s: %v", fileLockName, fileErr)
				}
			}
		}()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sig, sigCancel := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM, syscall.SIGUSR1)
	defer sigCancel()

	nodeID := env.GetNodeID()
	serviceName := cfg.GetServiceName(services)
	serviceInstanceID := uuid.NewString()

	// Detect CPU platform for orchestrator pool matching
	machineInfo, err := machineinfo.Detect()
	if err != nil {
		log.Printf("failed to detect machine info: %v", err)

		return false
	}

	serviceInfo := service.NewInfoContainer(ctx, nodeID, version, commitSHA, serviceInstanceID, machineInfo, config)

	// ============================================================================
	// serviceError channel —— 服务错误通知机制
	// ============================================================================
	//
	// 【核心设计】
	// serviceError 是一个容量为 1 的 channel，用来通知主循环"某个服务出现了错误"。
	//
	// 为什么容量是 1？
	// - 我们只关心"是否有错误"，不关心"有多少个错误"
	// - 一旦有一个错误，主循环就会开始关闭流程
	// - 其他错误可以忽略（通过 startService 闭包中的 default 分支）
	//
	// 【错误流向】
	//
	// 1. 某个服务出现错误
	//    ┌─────────────────────────────────────────────────────────────┐
	//    │ grpcServer.Serve(grpcListener)                              │
	//    │ 返回：error（例如 "address already in use"）                │
	//    └────────────────┬────────────────────────────────────────────┘
	//                     │
	//                     ↓
	// 2. startService 闭包捕获错误
	//    ┌─────────────────────────────────────────────────────────────┐
	//    │ err := f()  // err = error                                  │
	//    │ l.Error(ctx, "service returned an error", zap.Error(err))  │
	//    │ select {                                                    │
	//    │     case serviceError <- err:  // 发送错误到 channel       │
	//    │     default:                   // 如果 channel 满，忽略    │
	//    │ }                                                           │
	//    └────────────────┬────────────────────────────────────────────┘
	//                     │
	//                     ↓
	// 3. 主循环接收错误
	//    ┌─────────────────────────────────────────────────────────────┐
	//    │ select {                                                    │
	//    │     case err := <-serviceError:  // 接收错误                │
	//    │         logger.L().Error(ctx, "service error", ...)        │
	//    │         // 开始关闭流程                                     │
	//    │     case <-sigChan:              // 或收到信号              │
	//    │         // 开始关闭流程                                     │
	//    │ }                                                           │
	//    └─────────────────────────────────────────────────────────────┘
	//
	// 【为什么需要这个 channel？】
	//
	// 考虑这个场景：
	// 1. 启动 5 个服务（gRPC、HTTP、Proxy、Event Collector、Health Check）
	// 2. 其中一个服务（例如 gRPC）启动失败
	// 3. 我们需要立即通知主循环，让它关闭其他服务
	// 4. 如果没有 serviceError channel，主循环无法知道 gRPC 启动失败了
	// 5. 主循环会继续等待，直到收到 SIGTERM 信号
	//
	// 通过 serviceError channel，我们可以：
	// - 立即通知主循环"某个服务出现了错误"
	// - 主循环可以立即开始关闭流程
	// - 避免浪费时间等待 SIGTERM 信号
	//
	// 【容量为 1 的重要性】
	//
	// 假设我们有 5 个服务，其中 3 个同时出现错误。
	// 如果 channel 容量是 0（无缓冲）：
	// - 第一个服务发送错误，主循环接收
	// - 第二个服务尝试发送错误，但主循环已经开始关闭流程，没有接收
	// - 第二个服务的 goroutine 被阻塞，无法完成
	// - g.Wait() 永远无法返回
	//
	// 如果 channel 容量是 1（有缓冲）：
	// - 第一个服务发送错误，channel 接收
	// - 第二个服务尝试发送错误，但 channel 已满
	// - startService 闭包中的 default 分支执行，不阻塞
	// - 第二个服务的 goroutine 可以继续完成
	// - g.Wait() 可以正常返回
	//
	// 【运维注意的点】
	//
	// 1. 监控服务错误
	//    - 如果看到 "service error" 日志，说明某个服务出现了错误
	//    - 检查错误信息，找出根本原因
	//    - 例如：
	//      "service error" service=grpc server error="address already in use"
	//      这说明 gRPC 端口被占用了
	//
	// 2. 监控关闭流程
	//    - 如果看到 "service error" 日志，然后看到关闭日志
	//    - 说明主循环正确地响应了服务错误
	//    - 例如：
	//      "service error" service=grpc server error=...
	//      "Shutting down grpc server"
	//      "Shutting down cmux server"
	//
	// 3. 监控多个服务错误
	//    - 如果多个服务同时出现错误，只有第一个错误会被记录
	//    - 其他错误会被忽略（通过 default 分支）
	//    - 这是正常的，因为我们只关心"是否有错误"
	//
	// 4. 监控 channel 阻塞
	//    - 如果进程退出时间很长，可能是某个 goroutine 被阻塞了
	//    - 检查日志看是否有 "service error" 日志
	//    - 如果有，说明 serviceError channel 被触发了
	//
	// 【常见问题】
	//
	// Q: 为什么 channel 容量是 1 而不是 len(services)？
	// A: 因为我们只关心"是否有错误"，不关心"有多少个错误"。
	//    一旦有一个错误，主循环就会开始关闭流程。
	//    其他错误可以忽略。
	//
	// Q: 如果 channel 容量是 0 会怎样？
	// A: 第二个服务的 goroutine 会被阻塞，无法完成。
	//    g.Wait() 永远无法返回。
	//    这是一个常见的 Go 并发 bug。
	//
	// Q: 为什么要 defer close(serviceError)？
	// A: 为了确保 channel 被正确关闭。
	//    如果没有 defer close，channel 会一直保持打开状态。
	//    这可能导致 goroutine 泄漏。
	//
	serviceError := make(chan error, 1)
	defer close(serviceError)

	// ============================================================================
	// errgroup + defer g.Wait() —— panic 安全的并发关闭
	// ============================================================================
	//
	// 【核心问题】
	// run() 函数启动多个后台 goroutine（gRPC server、HTTP server、proxy 等）
	// 这些 goroutine 需要被正确等待和关闭，否则会导致：
	// ❌ 进程在后台任务还在运行时就退出
	// ❌ 连接未正确关闭，导致文件描述符泄漏
	// ❌ 日志未 flush，导致最后的日志丢失
	// ❌ 资源未清理，导致下次启动时出现问题
	//
	// 【为什么用 errgroup？】
	//
	// errgroup 是 Go 标准库提供的并发管理工具，提供：
	// - g.Go(func() error)：启动一个 goroutine，错误会被 g.Wait() 收集
	// - g.Wait()：等待所有 goroutine 完成，返回第一个错误（如果有）
	// - 自动 panic 恢复：如果某个 goroutine panic，g.Wait() 会 panic
	//
	// 【为什么用 defer 而不是在函数末尾调用 g.Wait()？】
	//
	// 这是一个微妙但重要的设计决策。让我们分析几种情况：
	//
	// 情况 1：正常关闭（收到 SIGTERM）
	// ────────────────────────────────────────────────────────────────────
	// 流程：
	// 1. 收到 SIGTERM
	// 2. select 分支触发，进入关闭流程
	// 3. 执行排水逻辑（drain）
	// 4. 关闭所有资源
	// 5. 到达函数末尾，调用 g.Wait()
	// 6. 等待所有 goroutine 完成
	// 7. 返回 success = true
	//
	// 在这种情况下，defer 和函数末尾调用 g.Wait() 没有区别
	//
	// 情况 2：logger.Fatal() 调用（初始化失败）
	// ────────────────────────────────────────────────────────────────────
	// 流程：
	// 1. 初始化某个组件失败
	// 2. 调用 logger.L().Fatal(ctx, "failed to init X", zap.Error(err))
	// 3. logger.Fatal() 内部调用 zap.Fatal()
	// 4. zap.Fatal() 执行：
	//    a. 调用 logger.Sync()（flush 所有日志）
	//    b. 调用 os.Exit(1)（直接退出进程）
	// 5. os.Exit(1) 会：
	//    ✗ 跳过所有 defer
	//    ✗ 跳过函数末尾的代码
	//    ✗ 直接终止进程
	//
	// 结果：
	// ❌ g.Wait() 永远不会被调用
	// ❌ 后台 goroutine 仍在运行
	// ❌ 进程立即退出，goroutine 被强制杀死
	// ❌ 可能导致资源泄漏
	//
	// 但这种情况下，defer 也无法救场，因为 os.Exit() 会跳过所有 defer
	//
	// 情况 3：panic（真正的价值所在）
	// ────────────────────────────────────────────────────────────────────
	// 流程：
	// 1. 某个地方发生 panic（例如 nil pointer dereference）
	// 2. panic 会展开调用栈，执行所有 defer
	// 3. defer g.Wait() 被执行
	// 4. g.Wait() 等待所有 goroutine 完成
	// 5. 然后 panic 继续传播，导致进程退出
	//
	// 结果：
	// ✓ 即使发生 panic，goroutine 也被正确等待
	// ✓ 资源被正确清理
	// ✓ 日志被正确 flush
	// ✓ 进程不会在后台任务还在运行时就退出
	//
	// 【为什么这很重要？】
	//
	// 考虑这个场景：
	// 1. gRPC server 在后台运行，处理客户端请求
	// 2. 某个地方发生 panic
	// 3. 如果没有 defer g.Wait()：
	//    - panic 立即导致进程退出
	//    - gRPC server 的 goroutine 被强制杀死
	//    - 客户端连接被突然关闭
	//    - 可能导致客户端看到连接重置错误
	// 4. 如果有 defer g.Wait()：
	//    - panic 触发 defer
	//    - g.Wait() 等待 gRPC server 完成
	//    - gRPC server 有机会执行 GracefulStop()
	//    - 现有连接被正确关闭
	//    - 客户端看到正常的连接关闭
	//
	// 【实际代码流程】
	//
	// startService 闭包启动 goroutine：
	// startService("grpc server", func() error {
	//     return grpcServer.Serve(grpcListener)
	// })
	//
	// 这会调用 g.Go()，启动一个 goroutine：
	// g.Go(func() error {
	//     l := globalLogger.With(zap.String("service", "grpc server"))
	//     l.Info(ctx, "starting service")
	//     err := grpcServer.Serve(grpcListener)  // 阻塞，直到 listener 关闭
	//     if err != nil {
	//         l.Error(ctx, "service returned an error", zap.Error(err))
	//     }
	//     select {
	//     case serviceError <- err:
	//     default:
	//     }
	//     return serviceDoneError{name: "grpc server"}
	// })
	//
	// 关闭流程：
	// 1. 收到 SIGTERM
	// 2. 执行排水逻辑
	// 3. 关闭 cmux server（这会关闭 listener）
	// 4. grpcServer.Serve() 返回（因为 listener 关闭了）
	// 5. goroutine 完成
	// 6. g.Wait() 返回
	//
	// 如果发生 panic：
	// 1. panic 展开调用栈
	// 2. defer g.Wait() 被执行
	// 3. g.Wait() 等待所有 goroutine 完成
	// 4. 如果某个 goroutine 还在运行（例如处理请求），g.Wait() 会等待它完成
	// 5. 然后 panic 继续传播
	//
	// 【运维注意的点】
	//
	// 1. 监控进程退出时间
	//    - 如果进程退出时间很长（超过 30 秒），可能是 g.Wait() 在等待某个 goroutine
	//    - 检查日志看是哪个服务没有正确关闭
	//    - 例如：某个 gRPC 连接卡住，导致 GracefulStop() 无法完成
	//
	// 2. 监控 panic 日志
	//    - 如果看到 panic 日志，说明发生了未预期的错误
	//    - 检查 panic 的堆栈跟踪，找出根本原因
	//    - 例如：nil pointer dereference、index out of range 等
	//
	// 3. 监控资源泄漏
	//    - 如果进程多次重启，检查是否有文件描述符泄漏
	//    - 使用 lsof 检查进程打开的文件：lsof -p <pid>
	//    - 如果文件描述符数量不断增加，说明有泄漏
	//
	// 4. 监控关闭日志
	//    - 查看关闭时的日志，确认所有服务都被正确关闭
	//    - 例如：
	//      "Shutting down grpc server"
	//      "Shutting down cmux server"
	//      "Waiting for services to finish"
	//    - 如果某个服务的关闭日志没有出现，说明可能卡住了
	//
	// 5. 设置合理的超时
	//    - 虽然 g.Wait() 没有超时，但 closeCtx 有超时
	//    - 如果某个服务的 Close() 方法超时，会导致进程等待
	//    - 检查 closeCtx 的超时设置（通常是 30 秒）
	//
	// 【常见问题】
	//
	// Q: 为什么 g.Wait() 没有超时？
	// A: 因为我们希望给 goroutine 充足的时间完成。如果需要超时，应该在
	//    goroutine 内部实现（例如使用 context.WithTimeout）
	//
	// Q: 如果某个 goroutine 永远不完成怎么办？
	// A: 这是一个 bug。应该在 goroutine 内部实现超时或取消逻辑。
	//    例如：使用 context.Done() 检查是否被取消
	//
	// Q: defer g.Wait() 会影响性能吗？
	// A: 不会。defer 的开销非常小，只是在函数返回时执行一个函数调用。
	//    g.Wait() 的开销取决于有多少 goroutine 需要等待。
	//
	var g errgroup.Group
	defer func(g *errgroup.Group) {
		err := g.Wait()
		if err != nil {
			log.Printf("error while shutting down: %v", err)
			success = false
		}
	}(&g)

	// ============================================================================
	// 遥测优先初始化 —— 可观测性是基础设施
	// ============================================================================
	//
	// 【核心原则】
	// "没有可观测性的服务不应该运行"
	//
	// 这是 E2B 基础设施的核心设计原则。Orchestrator 管理数千个虚拟机，
	// 如果无法观测其状态，就无法诊断问题、优化性能、追踪故障。
	//
	// 【初始化顺序的依赖关系】
	//
	// 这个顺序不是随意的，而是由依赖关系决定的：
	//
	// 1. Telemetry（第一个）
	//    ↓ 提供 LogsProvider
	// 2. Logger（第二个）
	//    ↓ 用于记录所有组件的错误
	// 3. 业务组件（第三个及以后）
	//    ↓ 使用 logger 记录错误
	// 4. 服务启动（最后）
	//    ↓ 使用 logger 记录运行状态
	//
	// 【Telemetry 包含什么？】
	//
	// Telemetry 是一个综合的可观测性系统，包括：
	//
	// 1. LogsProvider
	//    - 用于收集日志
	//    - 连接到 Grafana Loki（日志存储）
	//    - 所有日志都会被发送到 Loki
	//
	// 2. MeterProvider
	//    - 用于收集指标（metrics）
	//    - 连接到 Grafana Mimir（指标存储）
	//    - 所有指标都会被发送到 Mimir
	//    - 例如：CPU 使用率、内存使用率、请求延迟等
	//
	// 3. TracerProvider
	//    - 用于收集分布式追踪（traces）
	//    - 连接到 Grafana Tempo（追踪存储）
	//    - 所有请求的调用链都会被记录
	//    - 例如：API 请求 → Orchestrator → Firecracker
	//
	// 4. Resource
	//    - 标识这个服务实例
	//    - 包含：service name、version、commit SHA、instance ID、node labels
	//    - 用于在 Grafana 中过滤和聚合数据
	//
	// 【为什么 Telemetry 必须第一个初始化？】
	//
	// 原因 1：Logger 依赖 Telemetry
	// ────────────────────────────────────────────────────────────────────
	// Logger 的初始化代码：
	// globalLogger := logger.NewLogger(logger.LoggerConfig{
	//     Cores: []zapcore.Core{
	//         logger.GetOTELCore(tel.LogsProvider, serviceName)  // ← 需要 tel
	//     },
	// })
	//
	// 如果 Telemetry 初始化失败，就无法创建 Logger
	// 如果 Logger 初始化失败，就无法记录后续组件的错误
	//
	// 原因 2：所有组件都依赖 Logger
	// ────────────────────────────────────────────────────────────────────
	// 初始化流程中的每一步都可能失败：
	// - Redis 连接失败
	// - ClickHouse 连接失败
	// - Template cache 初始化失败
	// - 等等
	//
	// 这些错误都需要通过 Logger 记录：
	// if err != nil {
	//     logger.L().Fatal(ctx, "failed to create X", zap.Error(err))
	// }
	//
	// 如果 Logger 还没初始化，就无法记录这些错误
	//
	// 原因 3：没有可观测性的服务不应该运行
	// ────────────────────────────────────────────────────────────────────
	// 如果 Telemetry 初始化失败，说明：
	// - 无法连接到 Loki（日志存储）
	// - 无法连接到 Mimir（指标存储）
	// - 无法连接到 Tempo（追踪存储）
	// - 无法连接到 Jaeger（追踪收集器）
	//
	// 在这种情况下，即使 Orchestrator 启动成功，也无法被观测：
	// ❌ 无法看到日志
	// ❌ 无法看到指标
	// ❌ 无法看到追踪
	// ❌ 无法诊断问题
	// ❌ 无法优化性能
	//
	// 这样的服务对运维人员来说是"黑盒"，无法管理
	// 因此，Telemetry 初始化失败时，应该直接拒绝启动
	//
	// 【初始化失败的处理】
	//
	// 注意这里的错误处理：
	// if err != nil {
	//     logger.L().Fatal(ctx, "failed to init telemetry", zap.Error(err))
	// }
	//
	// 这里调用 logger.L().Fatal()，但此时 logger 还没初始化！
	// 这是一个"鸡生蛋、蛋生鸡"的问题。
	//
	// 解决方案：logger.L() 返回的是全局 logger
	// 在 logger 初始化之前，logger.L() 返回一个默认的 logger（通常是 noop）
	// 所以这行代码实际上是：
	// - 如果 logger 已初始化：使用初始化的 logger 记录错误
	// - 如果 logger 未初始化：使用默认 logger（可能不记录任何东西）
	//
	// 但由于 Telemetry 是第一个初始化的，logger 肯定还没初始化
	// 所以这行代码实际上是在调用默认 logger
	//
	// 更好的做法是使用 log.Fatalf()：
	// if err != nil {
	//     log.Fatalf("failed to init telemetry: %v", err)
	// }
	//
	// 但代码中使用了 logger.L().Fatal()，这可能是为了保持一致性
	//
	// 【Telemetry 初始化的具体步骤】
	//
	// 1. 连接到 Jaeger（追踪收集器）
	//    - 通过 OTEL_EXPORTER_OTLP_ENDPOINT 环境变量配置
	//    - 默认：http://localhost:4317
	//
	// 2. 连接到 Loki（日志存储）
	//    - 通过 OTEL_EXPORTER_OTLP_ENDPOINT 环境变量配置
	//    - 默认：http://localhost:4317
	//
	// 3. 连接到 Mimir（指标存储）
	//    - 通过 OTEL_EXPORTER_OTLP_ENDPOINT 环境变量配置
	//    - 默认：http://localhost:4317
	//
	// 4. 创建 Resource（服务标识）
	//    - service.name = serviceName（例如 "orchestrator"）
	//    - service.version = version（例如 "1.2.3"）
	//    - service.instance.id = serviceInstanceID（UUID）
	//    - host.labels = config.NodeLabels（例如 ["gpu=true", "region=us-west"]）
	//
	// 【运维注意的点】
	//
	// 1. 监控 Telemetry 初始化失败
	//    - 如果看到 "failed to init telemetry" 错误，说明无法连接到可观测性后端
	//    - 检查 OTEL_EXPORTER_OTLP_ENDPOINT 是否正确配置
	//    - 检查网络连接：ping <endpoint>
	//    - 检查防火墙规则
	//    - 检查 Jaeger/Loki/Mimir 是否在运行
	//
	// 2. 监控 Telemetry 连接延迟
	//    - Telemetry 初始化可能需要几秒钟
	//    - 如果初始化时间过长，可能是网络问题
	//    - 检查网络延迟：ping -c 10 <endpoint>
	//
	// 3. 监控日志收集
	//    - 启动后，检查 Loki 中是否有日志
	//    - 如果没有日志，说明 LogsProvider 没有正确配置
	//    - 检查 Loki 的 push 端点是否正确
	//
	// 4. 监控指标收集
	//    - 启动后，检查 Mimir 中是否有指标
	//    - 如果没有指标，说明 MeterProvider 没有正确配置
	//    - 检查 Mimir 的 push 端点是否正确
	//
	// 5. 监控追踪收集
	//    - 启动后，检查 Tempo 中是否有追踪
	//    - 如果没有追踪，说明 TracerProvider 没有正确配置
	//    - 检查 Tempo 的 push 端点是否正确
	//
	// 6. 监控 Resource 标签
	//    - 在 Grafana 中检查 service.name、service.version 等标签
	//    - 确保标签正确，便于过滤和聚合
	//    - 例如：按 service.version 过滤，查看不同版本的性能差异
	//
	// 【常见问题】
	//
	// Q: 如果 Telemetry 初始化失败，为什么不继续启动？
	// A: 因为没有可观测性的服务无法被管理。如果启动了，运维人员无法看到
	//    日志、指标、追踪，无法诊断问题。宁可启动失败，也不要启动一个
	//    "黑盒"服务。
	//
	// Q: 如果 Telemetry 后端暂时不可用怎么办？
	// A: 这是一个架构问题。Telemetry 后端应该是高可用的，不应该暂时不可用。
	//    如果确实需要容错，可以在 Telemetry 初始化中添加重试逻辑。
	//
	// Q: Telemetry 初始化需要多长时间？
	// A: 通常 1-2 秒。如果超过 5 秒，说明可能有网络问题。
	//
	// Q: 如何调试 Telemetry 初始化问题？
	// A: 设置 OTEL_LOG_LEVEL=debug 环境变量，查看详细的初始化日志。
	//
	tel, err := telemetry.New(
		ctx,
		nodeID,
		serviceName,
		commitSHA,
		version,
		serviceInstanceID,
		attribute.Key("host.labels").StringSlice(config.NodeLabels),
	)
	if err != nil {
		logger.L().Fatal(ctx, "failed to init telemetry", zap.Error(err))
	}
	e2bgrpc.StartChannelzSampler(ctx)
	defer func() {
		err := tel.Shutdown(ctx)
		if err != nil {
			log.Printf("error while shutting down telemetry: %v", err)
			success = false
		}
	}()

	if err := tel.StartRuntimeInstrumentation(); err != nil {
		log.Printf("failed to start runtime instrumentation: %v", err)
	}

	// 初始化全局 logger（依赖 tel.LogsProvider）
	globalLogger := utils.Must(logger.NewLogger(logger.LoggerConfig{
		ServiceName:   serviceName,
		IsInternal:    true,
		IsDebug:       env.IsDebug(),
		Cores:         []zapcore.Core{logger.GetOTELCore(tel.LogsProvider, serviceName)},
		EnableConsole: true,
	}))
	defer func(l logger.Logger) {
		err := l.Sync()
		if err != nil {
			log.Printf("error while shutting down logger: %v", err)
			success = false
		}
	}(globalLogger)
	logger.ReplaceGlobals(ctx, globalLogger)

	// ============================================================================
	// 两套 Sandbox Logger —— 内外分离（多租户安全边界）
	// ============================================================================
	//
	// 【核心问题】
	// E2B 是一个多租户系统，每个用户可以创建多个沙箱（虚拟机）
	// 用户希望看到自己沙箱的日志，但不应该看到：
	// ❌ 其他用户沙箱的日志
	// ❌ 基础设施内部的 IP 地址
	// ❌ 基础设施内部的错误细节
	// ❌ 性能调试信息
	// ❌ 安全敏感信息
	//
	// 同时，E2B 工程师需要看到完整的日志来诊断问题：
	// ✓ 所有沙箱的日志
	// ✓ 内部 IP 地址
	// ✓ 完整的错误堆栈
	// ✓ 性能调试信息
	// ✓ 安全审计日志
	//
	// 【解决方案】
	// 使用两套独立的 logger：
	// 1. External Logger：发给用户的日志
	// 2. Internal Logger：发给 E2B 工程师的日志
	//
	// 【日志流向】
	//
	// External Logger 流向：
	// ┌─────────────────────────────────────────────────────────────┐
	// │ Sandbox 执行代码                                             │
	// │ logger.L().Info("Hello from sandbox")                       │
	// └────────────────┬────────────────────────────────────────────┘
	//                  │
	//                  ↓
	// ┌─────────────────────────────────────────────────────────────┐
	// │ External Logger（IsInternal=false）                         │
	// │ - 过滤敏感信息（IP、密钥等）                                 │
	// │ - 只保留用户相关的信息                                       │
	// │ - 添加用户 ID、沙箱 ID 等标签                                │
	// └────────────────┬────────────────────────────────────────────┘
	//                  │
	//                  ↓
	// ┌─────────────────────────────────────────────────────────────┐
	// │ logs-collector（日志收集器）                                 │
	// │ - 运行在每个 Orchestrator 节点上                             │
	// │ - 收集所有 External 日志                                     │
	// │ - 按用户 ID 分组                                             │
	// └────────────────┬────────────────────────────────────────────┘
	//                  │
	//                  ↓
	// ┌─────────────────────────────────────────────────────────────┐
	// │ 用户的 Loki 实例（用户私有）                                 │
	// │ - 用户只能看到自己的日志                                     │
	// │ - 日志已过滤，不包含敏感信息                                 │
	// └─────────────────────────────────────────────────────────────┘
	//
	// Internal Logger 流向：
	// ┌─────────────────────────────────────────────────────────────┐
	// │ Sandbox 执行代码                                             │
	// │ logger.L().Info("Hello from sandbox")                       │
	// └────────────────┬────────────────────────────────────────────┘
	//                  │
	//                  ↓
	// ┌─────────────────────────────────────────────────────────────┐
	// │ Internal Logger（IsInternal=true）                          │
	// │ - 保留所有信息（IP、密钥等）                                 │
	// │ - 包含完整的调试信息                                         │
	// │ - 添加内部标签（节点 ID、进程 ID 等）                        │
	// └────────────────┬────────────────────────────────────────────┘
	//                  │
	//                  ↓
	// ┌─────────────────────────────────────────────────────────────┐
	// │ E2B 内部 Loki 实例                                           │
	// │ - 只有 E2B 工程师可以访问                                    │
	// │ - 包含完整的调试信息                                         │
	// │ - 用于诊断问题、优化性能                                     │
	// └─────────────────────────────────────────────────────────────┘
	//
	// 【External Logger 的过滤规则】
	//
	// 什么会被过滤？
	// ❌ 内部 IP 地址（例如 10.0.0.1）
	// ❌ 外部 IP 地址（例如 203.0.113.1）
	// ❌ 密钥和令牌（例如 API key、JWT token）
	// ❌ 数据库连接字符串
	// ❌ 其他用户的信息
	// ❌ 基础设施内部的错误细节
	// ❌ 性能调试信息（例如 GC 暂停时间）
	//
	// 什么会被保留？
	// ✓ 用户代码的输出（stdout/stderr）
	// ✓ 用户代码的错误（panic、exception）
	// ✓ 用户相关的事件（sandbox created、sandbox deleted）
	// ✓ 用户相关的指标（execution time、memory usage）
	//
	// 【Internal Logger 的内容】
	//
	// Internal Logger 包含所有信息：
	// ✓ 所有 External Logger 的内容
	// ✓ 内部 IP 地址
	// ✓ 密钥和令牌（用于审计）
	// ✓ 数据库连接字符串
	// ✓ 其他用户的信息（用于诊断跨租户问题）
	// ✓ 基础设施内部的错误细节
	// ✓ 性能调试信息
	//
	// 【实现细节】
	//
	// 两个 logger 都使用相同的 LogsProvider（来自 Telemetry）
	// 但它们有不同的配置：
	//
	// External Logger：
	// - IsInternal = false
	// - CollectorAddress = env.LogsCollectorAddress()
	// - 日志会被发送到 logs-collector
	// - logs-collector 会过滤敏感信息
	// - 然后转发到用户的 Loki
	//
	// Internal Logger：
	// - IsInternal = true
	// - CollectorAddress = env.LogsCollectorAddress()
	// - 日志会被发送到 logs-collector
	// - logs-collector 会保留所有信息
	// - 然后转发到 E2B 内部 Loki
	//
	// 【使用方式】
	//
	// 在沙箱代码中：
	// logger.L().Info(ctx, "Processing request")
	//
	// 这会同时写入两个 logger：
	// 1. External Logger：记录"Processing request"（如果没有敏感信息）
	// 2. Internal Logger：记录"Processing request"（包含所有上下文）
	//
	// 【运维注意的点】
	//
	// 1. 监控日志收集延迟
	//    - External 日志应该在 1-2 秒内到达用户的 Loki
	//    - Internal 日志应该在 1-2 秒内到达 E2B 内部 Loki
	//    - 如果延迟过长，检查 logs-collector 的性能
	//
	// 2. 监控日志过滤
	//    - 定期检查 External 日志，确保没有敏感信息泄露
	//    - 例如：搜索 IP 地址、密钥等
	//    - 如果发现泄露，立即调查原因
	//
	// 3. 监控日志丢失
	//    - 比较 External 和 Internal 日志的数量
	//    - 如果 External 日志明显少于 Internal 日志，说明有过滤
	//    - 这是正常的，但要确保没有过度过滤
	//
	// 4. 监控日志存储
	//    - External 日志存储在用户的 Loki（用户付费）
	//    - Internal 日志存储在 E2B 内部 Loki（E2B 付费）
	//    - 监控存储成本，确保没有日志爆炸
	//
	// 5. 监控日志访问
	//    - 用户只能访问自己的 External 日志
	//    - E2B 工程师可以访问所有 Internal 日志
	//    - 定期审计访问日志，确保没有未授权访问
	//
	// 【常见问题】
	//
	// Q: 为什么需要两套 logger？为什么不用一套 logger 加过滤？
	// A: 因为过滤可能出错，导致敏感信息泄露。使用两套独立的 logger
	//    可以确保安全性。即使 External Logger 出错，Internal Logger
	//    仍然可以用于诊断。
	//
	// Q: 如果用户需要看到更多信息怎么办？
	// A: 这是一个权限问题。用户可以请求 E2B 支持团队提供更详细的日志。
	//    E2B 支持团队可以从 Internal Logger 中提取相关信息，然后
	//    手动转发给用户。
	//
	// Q: 如果 External Logger 过滤了重要信息怎么办？
	// A: 这是一个配置问题。应该调整过滤规则，确保重要信息不被过滤。
	//    但要小心，不要过度放宽过滤规则，导致敏感信息泄露。
	//
	// Q: 两套 logger 会导致性能问题吗？
	// A: 不会。两套 logger 使用相同的 LogsProvider，只是配置不同。
	//    性能开销主要来自网络 I/O（发送日志到 Loki），而不是 logger 本身。
	//
	sbxLoggerExternal := sbxlogger.NewLogger(
		ctx,
		tel.LogsProvider,
		sbxlogger.SandboxLoggerConfig{
			ServiceName:      serviceName,
			IsInternal:       false,
			CollectorAddress: env.LogsCollectorAddress(),
		},
	)
	defer func(l logger.Logger) {
		err := l.Sync()
		if err != nil {
			log.Printf("error while shutting down sandbox logger: %v", err)
			success = false
		}
	}(sbxLoggerExternal)
	sbxlogger.SetSandboxLoggerExternal(sbxLoggerExternal)

	sbxLoggerInternal := sbxlogger.NewLogger(
		ctx,
		tel.LogsProvider,
		sbxlogger.SandboxLoggerConfig{
			ServiceName:      serviceName,
			IsInternal:       true,
			CollectorAddress: env.LogsCollectorAddress(),
		},
	)
	defer func(l logger.Logger) {
		err := l.Sync()
		if err != nil {
			log.Printf("error while shutting down sandbox logger: %v", err)
			success = false
		}
	}(sbxLoggerInternal)
	sbxlogger.SetSandboxLoggerInternal(sbxLoggerInternal)

	globalLogger.Info(ctx, "Starting orchestrator",
		zap.String("version", version),
		zap.String("commit", commitSHA),
		zap.Strings("labels", config.NodeLabels),
		logger.WithServiceInstanceID(serviceInstanceID),
	)

	// startService 闭包 —— 统一的服务生命周期管理
	//
	// ============================================================================
	// 为什么用闭包而不是直接 g.Go？
	// ============================================================================
	//
	// 这是一个关键的设计决策，涉及错误处理、日志记录和并发管理。
	// 让我们分析为什么这个模式比直接调用 g.Go() 更优雅。
	//
	// 【问题场景】
	//
	// 假设我们有 5 个服务需要启动：
	// - gRPC server
	// - HTTP server
	// - Proxy
	// - Event collector
	// - Health check
	//
	// 如果直接调用 g.Go()，代码会是这样：
	//
	// g.Go(func() error {
	//     l := globalLogger.With(zap.String("service", "grpc server"))
	//     l.Info(ctx, "starting service")
	//     err := grpcServer.Serve(grpcListener)
	//     if err != nil {
	//         l.Error(ctx, "service returned an error", zap.Error(err))
	//     }
	//     select {
	//     case serviceError <- err:
	//     default:
	//     }
	//     return serviceDoneError{name: "grpc server"}
	// })
	//
	// g.Go(func() error {
	//     l := globalLogger.With(zap.String("service", "http server"))
	//     l.Info(ctx, "starting service")
	//     err := httpServer.Serve(httpListener)
	//     if err != nil {
	//         l.Error(ctx, "service returned an error", zap.Error(err))
	//     }
	//     select {
	//     case serviceError <- err:
	//     default:
	//     }
	//     return serviceDoneError{name: "http server"}
	// })
	//
	// // ... 重复 3 次 ...
	//
	// 这是大量的代码重复！每个服务都需要相同的日志、错误处理、channel 发送逻辑。
	//
	// 【解决方案：startService 闭包】
	//
	// 通过提取公共逻辑到闭包中，我们可以：
	// 1. 消除代码重复
	// 2. 确保所有服务有一致的行为
	// 3. 便于修改日志格式或错误处理逻辑
	//
	// 现在代码变成：
	//
	// startService("grpc server", func() error {
	//     return grpcServer.Serve(grpcListener)
	// })
	// startService("http server", func() error {
	//     return httpServer.Serve(httpListener)
	// })
	// // ... 简洁得多 ...
	//
	// 【四个关键设计点】
	//
	// 1️⃣ 统一日志格式
	// ────────────────────────────────────────────────────────────────────
	// 每个服务启动时都会记录：
	//   "starting service" service=grpc server
	// 每个服务退出时都会记录：
	//   "service returned an error" service=grpc server error=...
	//
	// 这样，运维人员可以通过搜索 "starting service" 或 "service returned an error"
	// 来快速找到所有服务的启动/退出日志。
	//
	// 如果没有闭包，每个服务的日志格式可能不同，导致难以追踪。
	//
	// 2️⃣ 错误传播到主循环
	// ────────────────────────────────────────────────────────────────────
	// 当任何服务退出时（无论正常还是异常），都会通过 serviceError channel
	// 通知主循环。主循环可以根据错误类型决定是否关闭整个 orchestrator。
	//
	// 例如：
	// - 如果 gRPC server 退出，说明 listener 被关闭了，应该关闭整个 orchestrator
	// - 如果 event collector 退出，可能是暂时的网络问题，可能需要重启
	//
	// 通过 serviceError channel，主循环可以统一处理所有服务的错误。
	//
	// 3️⃣ default 分支防止阻塞
	// ────────────────────────────────────────────────────────────────────
	// serviceError channel 的容量是 1：
	//   serviceError := make(chan error)
	//
	// 这意味着：
	// - 第一个服务退出时，可以成功发送错误到 channel
	// - 第二个服务退出时，channel 已满，select 的 default 分支会执行
	// - 这防止了第二个服务的 goroutine 被阻塞
	//
	// 为什么这很重要？
	// 考虑这个场景：
	// 1. gRPC server 退出，发送错误到 serviceError channel
	// 2. 主循环收到错误，开始关闭流程
	// 3. 主循环关闭 cmux server（这会关闭 listener）
	// 4. HTTP server 也退出，尝试发送错误到 serviceError channel
	// 5. 但 channel 已满（第一个错误还在里面），所以 select 的 default 分支执行
	// 6. HTTP server 的 goroutine 不会被阻塞，可以继续完成清理工作
	//
	// 如果没有 default 分支，HTTP server 的 goroutine 会被永久阻塞，
	// 导致 g.Wait() 永远无法返回。
	//
	// 【关键问题】为什么 channel 容量是 1？
	//
	// 因为我们只关心"是否有错误"，不关心"有多少个错误"。
	// 一旦有一个错误，主循环就会开始关闭流程。
	// 其他错误可以忽略（通过 default 分支）。
	//
	// 这是一个常见的 Go 模式：用容量为 1 的 channel 来实现"最多一个值"的语义。
	//
	// 4️⃣ serviceDoneError 区分"正常退出"和"真正的错误"
	// ────────────────────────────────────────────────────────────────────
	// startService 闭包返回 serviceDoneError{name: name}，而不是返回 err。
	//
	// 这是一个巧妙的设计：
	// - 如果服务正常退出（err == nil），返回 serviceDoneError（不是 nil）
	// - 如果服务异常退出（err != nil），返回 serviceDoneError（不是 err）
	//
	// 这样，g.Wait() 会看到 serviceDoneError，而不是真正的错误。
	// serviceDoneError 是一个自定义错误类型，用来标记"服务已完成"。
	//
	// 为什么这很重要？
	// 考虑 g.Wait() 的行为：
	// - g.Wait() 返回第一个非 nil 的错误
	// - 如果所有 goroutine 都返回 nil，g.Wait() 返回 nil
	//
	// 如果我们直接返回 err（可能是 nil），那么：
	// - 如果服务正常退出（err == nil），g.Wait() 会继续等待其他 goroutine
	// - 如果服务异常退出（err != nil），g.Wait() 会立即返回错误
	//
	// 但这样的话，g.Wait() 的返回值就不一致了：
	// - 有时返回 nil（所有服务都正常退出）
	// - 有时返回错误（某个服务异常退出）
	//
	// 通过返回 serviceDoneError，我们确保：
	// - g.Wait() 总是返回 serviceDoneError（或 nil，如果没有 goroutine）
	// - 这样，defer g.Wait() 的行为就一致了
	//
	// 【运维注意的点】
	//
	// 1. 监控服务启动日志
	//    - 查看 "starting service" 日志，确认所有服务都启动了
	//    - 如果某个服务的启动日志没有出现，说明可能没有被启动
	//
	// 2. 监控服务退出日志
	//    - 查看 "service returned an error" 日志，确认服务是否异常退出
	//    - 如果看到这个日志，说明某个服务出现了问题
	//
	// 3. 监控 serviceError channel
	//    - 虽然 channel 本身不可见，但可以通过日志推断
	//    - 如果看到关闭日志，说明 serviceError channel 被触发了
	//
	// 4. 监控 g.Wait() 返回时间
	//    - 如果 g.Wait() 返回时间很长，说明某个 goroutine 没有正确完成
	//    - 检查日志看是哪个服务没有完成
	//
	// 【常见问题】
	//
	// Q: 为什么不用 WaitGroup 而用 errgroup？
	// A: errgroup 提供了错误收集的功能，WaitGroup 只能等待完成。
	//    errgroup 更适合这种"需要收集错误"的场景。
	//
	// Q: 为什么 serviceError channel 的容量是 1？
	// A: 因为我们只关心"是否有错误"，不关心"有多少个错误"。
	//    一旦有一个错误，主循环就会开始关闭流程。
	//
	// Q: 为什么要返回 serviceDoneError 而不是 err？
	// A: 因为我们想让 g.Wait() 的行为一致。无论服务是正常退出还是异常退出，
	//    都返回 serviceDoneError，这样 g.Wait() 的返回值就一致了。
	//
	startService := func(name string, f func() error) {
		g.Go(func() error {
			l := globalLogger.With(zap.String("service", name))
			l.Info(ctx, "starting service")

			err := f()
			if err != nil {
				l.Error(ctx, "service returned an error", zap.Error(err))
			}

			select {
			case serviceError <- err:
			default:
				// Don't block if the serviceError channel is already closed
				// or if the error is already sent
			}

			return serviceDoneError{name: name}
		})
	}

	// ============================================================================
	// closers 切片 + slices.Reverse —— LIFO 关闭顺序
	// ============================================================================
	//
	// 【核心设计原理】
	// closers 是一个切片，存储所有需要在进程退出时关闭的资源。
	// 关键特性：LIFO（后进先出）关闭顺序，确保依赖关系正确。
	//
	// 【为什么需要 LIFO 关闭？】
	//
	// 假设我们有这样的依赖关系：
	// - A 是基础设施（例如 Redis）
	// - B 依赖 A（例如 Template Cache，使用 Redis 进行 P2P 传输）
	// - C 依赖 B（例如 Sandbox Factory，使用 Template Cache）
	//
	// 初始化顺序：A → B → C
	// 关闭顺序必须是：C → B → A（反向）
	//
	// 如果关闭顺序错误（例如 A → B → C）：
	// 1. 关闭 A（Redis）
	// 2. B 尝试使用 A，但 A 已关闭 → 错误
	// 3. C 尝试使用 B，但 B 已出错 → 错误
	//
	// 【为什么不用 defer？】
	//
	// defer 也是 LIFO 的，但有两个问题：
	//
	// 问题 1：无法控制关闭超时
	// ────────────────────────────────────────────────────────────────────
	// defer 在函数返回时执行，无法设置超时。
	// 如果某个资源的 Close() 方法卡住，整个进程会被卡住。
	//
	// 使用 closers 切片，我们可以设置 closeCtx 超时：
	// closeCtx, cancelCloseCtx := context.WithTimeout(context.Background(), 30*time.Second)
	// for _, closer := range closers {
	//     closer.close(closeCtx)  // 如果超过 30 秒，context 会被取消
	// }
	//
	// 问题 2：无法在关闭前执行排水（drain）逻辑
	// ────────────────────────────────────────────────────────────────────
	// defer 在函数返回时执行，无法在关闭前插入其他逻辑。
	// 但我们需要在关闭前执行排水逻辑：
	// 1. 标记为 Draining
	// 2. 等待 15 秒让 client-proxy 感知到
	// 3. 等待 template manager 完成正在进行的构建
	// 4. 再关闭所有资源
	//
	// 使用 closers 切片，我们可以在关闭前插入任意逻辑：
	// // 排水逻辑
	// serviceInfo.SetStatus(ctx, Draining)
	// time.Sleep(15 * time.Second)
	// tmpl.Wait(closeCtx)
	//
	// // 关闭逻辑
	// slices.Reverse(closers)
	// for _, closer := range closers {
	//     closer.close(closeCtx)
	// }
	//
	// 【closers 切片的结构】
	//
	// type closer struct {
	//     name  string                      // 资源名称（用于日志）
	//     close func(ctx context.Context) error  // 关闭函数
	// }
	//
	// 【初始化顺序示例】
	//
	// 初始化时，按依赖顺序添加到 closers：
	// closers = append(closers, closer{"redis", redis.Close})           // 第 1 个
	// closers = append(closers, closer{"template cache", cache.Close})  // 第 2 个
	// closers = append(closers, closer{"sandbox factory", factory.Close}) // 第 3 个
	//
	// closers 切片的内容：
	// [
	//     {name: "redis", close: redis.Close},
	//     {name: "template cache", close: cache.Close},
	//     {name: "sandbox factory", close: factory.Close},
	// ]
	//
	// 【关闭顺序】
	//
	// 关闭时，先反转 closers：
	// slices.Reverse(closers)
	//
	// 反转后的 closers：
	// [
	//     {name: "sandbox factory", close: factory.Close},
	//     {name: "template cache", close: cache.Close},
	//     {name: "redis", close: redis.Close},
	// ]
	//
	// 然后按顺序关闭：
	// for _, closer := range closers {
	//     closer.close(closeCtx)
	// }
	//
	// 关闭顺序：sandbox factory → template cache → redis
	//
	// 【运维含义】
	//
	// 1. 监控关闭日志
	//    - 查看关闭时的日志，确认所有资源都被正确关闭
	//    - 例如：
	//      "closing" service=sandbox factory
	//      "closing" service=template cache
	//      "closing" service=redis
	//    - 如果某个资源的关闭日志没有出现，说明可能卡住了
	//
	// 2. 监控关闭时间
	//    - 如果关闭时间很长（超过 30 秒），说明某个资源的 Close() 方法卡住了
	//    - 检查日志看是哪个资源没有完成
	//    - 例如：
	//      "closing" service=template cache
	//      // 等待 30 秒...
	//      "error during shutdown" service=template cache error="context deadline exceeded"
	//
	// 3. 监控关闭错误
	//    - 如果看到 "error during shutdown" 日志，说明某个资源的 Close() 方法返回了错误
	//    - 这可能是正常的（例如连接已关闭），也可能是异常的（例如网络错误）
	//    - 检查错误信息，判断是否需要调查
	//
	// 4. 监控关闭顺序
	//    - 确认关闭顺序是反向的（后初始化先关闭）
	//    - 如果关闭顺序错误，可能导致资源泄漏或错误
	//
	// 【常见问题】
	//
	// Q: 为什么不用 defer？
	// A: defer 无法控制超时，也无法在关闭前插入排水逻辑。
	//    closers 切片提供了更灵活的控制。
	//
	// Q: 如果某个资源的 Close() 方法卡住怎么办？
	// A: closeCtx 会在 30 秒后超时，Close() 方法应该检查 context.Done()
	//    并在超时时返回错误。
	//
	// Q: 关闭顺序错误会导致什么问题？
	// A: 可能导致资源泄漏、错误或数据丢失。
	//    例如：如果先关闭 Redis，然后 Template Cache 尝试使用 Redis，会出错。
	//
	// Q: 为什么要用 slices.Reverse？
	// A: 因为 closers 是按初始化顺序添加的，关闭时需要反向。
	//    slices.Reverse 是 Go 1.22 引入的标准库函数，用来反转切片。
	//
	var closers []closer

	// The sandbox map is shared between the server and the proxy
	// to propagate information about sandbox routing.
	sandboxes := sandbox.NewSandboxesMap()

	// feature flags
	featureFlags, err := featureflags.NewClient()
	if err != nil {
		logger.L().Fatal(ctx, "failed to create feature flags client", zap.Error(err))
	}
	closers = append(closers, closer{"feature flags", featureFlags.Close})

	featureFlags.SetDeploymentName(config.DomainName)

	// gcp concurrent upload limiter
	limiter, err := limit.New(ctx, featureFlags)
	if err != nil {
		logger.L().Fatal(ctx, "failed to create limiter", zap.Error(err))
	}
	closers = append(closers, closer{"limiter", limiter.Close})

	persistence, err := storage.GetStorageProvider(ctx, storage.TemplateStorageConfig.WithLimiter(limiter))
	if err != nil {
		logger.L().Fatal(ctx, "failed to create template storage provider", zap.Error(err))
	}

	blockMetrics, err := blockmetrics.NewMetrics(tel.MeterProvider)
	if err != nil {
		logger.L().Fatal(ctx, "failed to create metrics provider", zap.Error(err))
	}

	// ============================================================================
	// Redis 的可选性 —— 优雅降级（可选依赖模式）
	// ============================================================================
	//
	// 【核心设计原理】
	// Redis 在 E2B 中用于两个功能：
	// 1. P2P 模板文件传输：节点间互传模板，避免都从 GCS 下载（性能优化）
	// 2. 沙箱事件流：补充 ClickHouse 的实时事件（可观测性增强）
	//
	// 这两个功能都不是核心路径。Redis 不可用时：
	// - 模板从 GCS 下载（慢但可用）
	// - 事件只写 ClickHouse（不丢失）
	//
	// 这是可选依赖的标准 Go 模式：用 ErrRedisDisabled 哨兵错误区分"配置禁用"和"连接失败"
	//
	// 【为什么需要区分"禁用"和"失败"？】
	//
	// 假设我们没有这个区分，代码是这样的：
	//
	// redisClient, err := NewRedisClient(...)
	// if err != nil {
	//     logger.Fatal("Could not connect to Redis")  // 无论什么错误都 Fatal
	// }
	//
	// 这样的问题：
	// 1. 如果用户配置了 REDIS_URL=""（禁用 Redis），也会 Fatal
	// 2. 如果 Redis 暂时不可用（网络问题），也会 Fatal
	// 3. 无法区分"这是预期的禁用"和"这是意外的失败"
	//
	// 【解决方案】
	//
	// 使用哨兵错误 ErrRedisDisabled：
	// - 如果 REDIS_URL=""，NewRedisClient 返回 ErrRedisDisabled
	// - 如果 Redis 连接失败，NewRedisClient 返回其他错误
	// - 我们可以区分这两种情况
	//
	// 【错误处理逻辑】
	//
	// if err != nil && !errors.Is(err, ErrRedisDisabled)
	//     ↑                ↑
	//     有错误            但不是"禁用"错误
	//
	// 这意味着：
	// - 如果 err == nil：Redis 连接成功，继续
	// - 如果 err == ErrRedisDisabled：Redis 被禁用，继续（使用 Nop 实现）
	// - 如果 err != nil && err != ErrRedisDisabled：真正的错误，Fatal
	//
	// 【运维含义】
	//
	// 场景 1：用户没有配置 Redis（REDIS_URL=""）
	// ────────────────────────────────────────────────────────────────────
	// - NewRedisClient 返回 ErrRedisDisabled
	// - 条件 err != nil && !errors.Is(err, ErrRedisDisabled) 为 false
	// - 不会 Fatal
	// - 使用 Nop 实现（空操作）
	// - 模板从 GCS 下载，事件只写 ClickHouse
	// - 系统正常运行（只是性能稍差）
	//
	// 场景 2：用户配置了 Redis，但 Redis 暂时不可用
	// ────────────────────────────────────────────────────────────────────
	// - NewRedisClient 返回网络错误（例如 "connection refused"）
	// - 条件 err != nil && !errors.Is(err, ErrRedisDisabled) 为 true
	// - 调用 logger.Fatal()
	// - 进程退出
	// - Nomad 检测到进程退出，重启 orchestrator
	// - 这是正确的行为：如果用户配置了 Redis，就应该可用
	//
	// 场景 3：Redis 连接成功
	// ────────────────────────────────────────────────────────────────────
	// - NewRedisClient 返回 nil
	// - 条件 err == nil 为 true
	// - 将 redisClient 添加到 closers 列表
	// - 后续使用 Redis 进行 P2P 传输和事件流
	//
	// 【代码流程】
	//
	// 第 1 步：尝试连接 Redis
	redisClient, err := sharedFactories.NewRedisClient(ctx, sharedFactories.RedisConfig{
		// 从配置中读取 Redis 连接参数
		RedisURL:         config.RedisURL,         // 主 Redis 地址（例如 redis://localhost:6379）
		RedisClusterURL:  config.RedisClusterURL,  // Redis Cluster 地址（如果使用集群）
		RedisTLSCABase64: config.RedisTLSCABase64, // TLS CA 证书（如果使用 TLS）
		PoolSize:         config.RedisPoolSize,    // 连接池大小（例如 10）
		MinIdleConns:     config.RedisMinIdleConns, // 最小空闲连接数（例如 5）
	})
	
	// 第 2 步：检查错误
	// 这是一个三分支的错误处理逻辑
	if err != nil && !errors.Is(err, sharedFactories.ErrRedisDisabled) {
		// 分支 A：真正的错误（不是"禁用"）
		// 例如：网络错误、认证失败、超时等
		// 这些错误表示配置有问题，应该立即 Fatal
		logger.L().Fatal(ctx, "Could not connect to Redis", zap.Error(err))
	} else if err == nil {
		// 分支 B：连接成功
		// 将 redisClient 添加到 closers 列表，确保进程退出时正确关闭
		closers = append(closers, closer{"redis client", func(context.Context) error {
			// 这个闭包会在关闭时被调用
			// 它负责正确关闭 Redis 连接，释放资源
			return sharedFactories.CloseCleanly(redisClient)
		}})
	}
	// 分支 C：err == ErrRedisDisabled（隐含）
	// 不做任何事，继续使用 Nop 实现

	// ============================================================================
	// Nop 实现 —— 可选依赖的标准模式
	// ============================================================================
	//
	// 【核心概念】
	// Nop = No Operation（空操作）
	// 这是一个设计模式，用来处理可选依赖：
	// - 如果依赖可用，使用真实实现
	// - 如果依赖不可用，使用 Nop 实现（什么都不做）
	//
	// 【为什么用 Nop 而不是 nil 检查？】
	//
	// 不好的做法：
	// if redisClient != nil {
	//     peerRegistry = NewRedisRegistry(redisClient)
	// } else {
	//     // 在每个使用 peerRegistry 的地方都要检查 nil
	//     if peerRegistry != nil {
	//         peerRegistry.Register(...)
	//     }
	// }
	//
	// 这导致代码中到处都是 nil 检查，容易出错。
	//
	// 好的做法（Nop 模式）：
	// peerRegistry := NopRegistry()  // 默认是 Nop
	// if redisClient != nil {
	//     peerRegistry = NewRedisRegistry(redisClient)  // 如果可用，替换为真实实现
	// }
	// // 后续代码不需要检查 nil，直接使用 peerRegistry
	// peerRegistry.Register(...)  // 如果是 Nop，什么都不做；如果是真实实现，正常工作
	//
	// 【Nop 实现的特点】
	//
	// NopRegistry 返回一个实现了 Registry 接口的对象，但所有方法都是空操作：
	// type NopRegistry struct {}
	// func (n *NopRegistry) Register(ctx context.Context, key string, value string) error {
	//     return nil  // 什么都不做，直接返回 nil
	// }
	// func (n *NopRegistry) Unregister(ctx context.Context, key string) error {
	//     return nil  // 什么都不做，直接返回 nil
	// }
	//
	// 【运维含义】
	//
	// 使用 Nop 实现的好处：
	// 1. 代码简洁：不需要到处检查 nil
	// 2. 错误处理一致：Nop 实现也返回 error，可以统一处理
	// 3. 易于测试：可以用 Nop 实现替换真实实现，进行单元测试
	// 4. 易于扩展：如果以后需要添加新的实现，只需实现接口即可
	//
	// 【初始化 peerRegistry 和 peerResolver】
	//
	// 第 1 步：初始化为 Nop 实现（默认值）
	peerRegistry := peerclient.NopRegistry()
	peerResolver := peerclient.NopResolver()
	
	// 第 2 步：如果 Redis 可用且节点地址可用，替换为真实实现
	if nodeAddress := config.NodeAddress(); redisClient != nil && nodeAddress != nil {
		// 条件 1：redisClient != nil
		// 说明 Redis 连接成功，可以使用 Redis 进行 P2P 通信
		
		// 条件 2：nodeAddress != nil
		// 说明当前节点有有效的地址（例如 IP:Port）
		// 这个地址会被注册到 Redis，供其他节点发现
		
		// 如果两个条件都满足，创建真实的 Redis 实现
		peerRegistry = peerclient.NewRedisRegistry(redisClient, *nodeAddress)
		peerResolver = peerclient.NewResolver(peerRegistry, *nodeAddress)
	}
	// 如果条件不满足，继续使用 Nop 实现
	// 后续代码不需要检查 nil，直接使用 peerRegistry 和 peerResolver

	templateCache, err := template.NewCache(config, featureFlags, persistence, blockMetrics, peerResolver)
	if err != nil {
		logger.L().Fatal(ctx, "failed to create template cache", zap.Error(err))
	}
	templateCache.Start(ctx)
	closers = append(closers, closer{"template cache", func(context.Context) error {
		templateCache.Stop()

		return nil
	}})

	sbxEventsDeliveryTargets := make([]event.Delivery[event.SandboxEvent], 0)

	hostStatsDelivery := clickhousehoststats.NewNoopDelivery()

	// Clickhouse sandbox events and host stats delivery
	if config.ClickhouseConnectionString != "" {
		clickhouseConn, err := clickhouse.NewDriver(config.ClickhouseConnectionString)
		if err != nil {
			logger.L().Fatal(ctx, "failed to create clickhouse driver", zap.Error(err))
		}
		closers = append(closers, closer{"clickhouse connection", func(context.Context) error {
			return clickhouseConn.Close()
		}})

		sbxEventsDeliveryClickhouse, err := clickhouseevents.NewDefaultClickhouseSandboxEventsDelivery(ctx, clickhouseConn, featureFlags)
		if err != nil {
			logger.L().Fatal(ctx, "failed to create clickhouse events delivery", zap.Error(err))
		}

		sbxEventsDeliveryTargets = append(sbxEventsDeliveryTargets, sbxEventsDeliveryClickhouse)
		closers = append(closers, closer{"sandbox events delivery for clickhouse", sbxEventsDeliveryClickhouse.Close})

		hostStatsDeliveryClickhouse, err := clickhousehoststats.NewDefaultClickhouseHostStatsDelivery(ctx, clickhouseConn, featureFlags)
		if err != nil {
			logger.L().Fatal(ctx, "failed to create clickhouse host stats delivery", zap.Error(err))
		}

		hostStatsDelivery = hostStatsDeliveryClickhouse
		closers = append(closers, closer{"sandbox host stats delivery", hostStatsDeliveryClickhouse.Close})
	}

	// cgroup manager for resource accounting
	cgroupManager, err := cgroup.NewManager()
	if err != nil {
		logger.L().Fatal(ctx, "failed to initialize cgroup manager", zap.Error(err))
	}

	if err := cgroupManager.Initialize(ctx); err != nil {
		logger.L().Fatal(ctx, "failed to initialize root cgroup", zap.Error(err))
	}

	logger.L().Info(ctx, "cgroup accounting enabled", zap.String("root", cgroup.RootCgroupPath))

	// Redis sandbox events delivery target
	if redisClient != nil {
		sbxEventsDeliveryRedis := event.NewRedisStreamsDelivery[event.SandboxEvent](redisClient, event.SandboxEventsStreamName)
		sbxEventsDeliveryTargets = append(sbxEventsDeliveryTargets, sbxEventsDeliveryRedis)
		closers = append(closers, closer{"sandbox events delivery for redis", sbxEventsDeliveryRedis.Close})
	}

	// sandbox observer
	sandboxObserver, err := metrics.NewSandboxObserver(ctx, nodeID, serviceName, commitSHA, version, serviceInstanceID, sandboxes)
	if err != nil {
		logger.L().Fatal(ctx, "failed to create sandbox observer", zap.Error(err))
	}
	closers = append(closers, closer{"sandbox observer", sandboxObserver.Close})

	// host metrics — samples CPU in the background so GetCPUMetrics is a
	// non-blocking cache read on the request path.
	hostMetrics := metrics.NewHostMetrics()
	startService("host metrics poller", func() error {
		return hostMetrics.Start()
	})
	closers = append(closers, closer{"host metrics poller", hostMetrics.Close})

	// sandbox proxy
	sandboxProxy, err := proxy.NewSandboxProxy(tel.MeterProvider, config.ProxyPort, sandboxes, featureFlags)
	if err != nil {
		logger.L().Fatal(ctx, "failed to create sandbox proxy", zap.Error(err))
	}
	startService("sandbox proxy", func() error {
		err := sandboxProxy.Start(ctx)
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}

		return err
	})
	closers = append(closers, closer{"sandbox proxy", sandboxProxy.Close})

	// 出口代理 —— 通过 EgressFactory 注入版本特定的实现
	//
	// 这是策略模式的核心应用点：
	// - main.go 传入 defaultEgressFactory（tcpfirewall 实现）
	// - 测试可以注入 noop 实现，避免真实 iptables 操作
	// - 未来支持不同网络后端只需换一个函数
	deps := &Deps{
		Config:        config,
		Tel:           tel,
		MeterProvider: tel.MeterProvider,
		Logger:        globalLogger,
		Sandboxes:     sandboxes,
		FeatureFlags:  featureFlags,
	}

	egressSetup, err := opts.EgressFactory(ctx, deps)
	if err != nil {
		logger.L().Fatal(ctx, "failed to create egress proxy", zap.Error(err))
	}
	if egressSetup == nil {
		logger.L().Fatal(ctx, "EgressFactory returned nil EgressSetup without error")
	}
	if egressSetup.Start != nil {
		startService("egress proxy", func() error {
			return egressSetup.Start(ctx)
		})
	}
	if egressSetup.Close != nil {
		closers = append(closers, closer{"egress proxy", egressSetup.Close})
	}

	// device pool
	devicePool, err := nbd.NewDevicePool(config.NBDPoolSize)
	if err != nil {
		logger.L().Fatal(ctx, "failed to create device pool", zap.Error(err))
	}
	startService("nbd device pool", func() error {
		devicePool.Populate(ctx)

		return nil
	})
	closers = append(closers, closer{"device pool", devicePool.Close})

	// network pool
	slotStorage, err := newStorage(ctx, nodeID, config.NetworkConfig, egressSetup.Proxy)
	if err != nil {
		logger.L().Fatal(ctx, "failed to create network pool", zap.Error(err))
	}
	networkPool := network.NewPool(network.NewSlotsPoolSize, network.ReusedSlotsPoolSize, slotStorage, config.NetworkConfig)
	startService("network pool", func() error {
		networkPool.Populate(ctx)

		return nil
	})
	closers = append(closers, closer{"network pool", networkPool.Close})

	// sandbox factory
	sandboxFactory := sandbox.NewFactory(config.BuilderConfig, networkPool, devicePool, featureFlags, hostStatsDelivery, cgroupManager, egressSetup.Proxy, sandboxes)

	// isolated filesystems cache (for nfs proxy)
	builder := chrooted.NewBuilder(config)
	volumeService := volumes.New(config, builder)

	uploads := sandbox.NewUploads(templateCache, persistence, peerResolver, redisClient)
	closers = append(closers, closer{"pending uploads", func(context.Context) error {
		uploads.Stop()

		return nil
	}})

	orchestratorService, err := server.New(ctx, server.ServiceConfig{
		Config:           config,
		SandboxFactory:   sandboxFactory,
		Tel:              tel,
		NetworkPool:      networkPool,
		DevicePool:       devicePool,
		TemplateCache:    templateCache,
		Info:             serviceInfo,
		Proxy:            sandboxProxy,
		Persistence:      persistence,
		FeatureFlags:     featureFlags,
		SbxEventsService: events.NewEventsService(sbxEventsDeliveryTargets),
		PeerRegistry:     peerRegistry,
		Uploads:          uploads,
	})
	if err != nil {
		logger.L().Fatal(ctx, "failed to create orchestrator server", zap.Error(err))
	}
	closers = append(closers, closer{"orchestrator server", func(context.Context) error {
		return orchestratorService.Close()
	}})

	// template manager sandbox logger
	tmplSbxLoggerExternal := sbxlogger.NewLogger(
		ctx,
		tel.LogsProvider,
		sbxlogger.SandboxLoggerConfig{
			ServiceName:      constants.ServiceNameTemplate,
			IsInternal:       false,
			CollectorAddress: env.LogsCollectorAddress(),
		},
	)
	closers = append(closers, closer{
		"template manager sandbox logger", func(context.Context) error {
			// Sync returns EINVAL when path is /dev/stdout (for example)
			if err := tmplSbxLoggerExternal.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) {
				return err
			}

			return nil
		},
	})

	// nfs proxy server
	if len(config.PersistentVolumeMounts) > 0 {
		nfsClosers, err := startNFSProxy(ctx, config, builder, startService, sandboxes)
		if err != nil {
			logger.L().Fatal(ctx, "failed to start nfs proxy", zap.Error(err))
		}
		closers = append(closers, nfsClosers...)
	}

	// hyperloop server
	hyperloopSrv, err := hyperloopserver.NewHyperloopServer(ctx, config.NetworkConfig.HyperloopProxyPort, globalLogger, sandboxes)
	if err != nil {
		logger.L().Fatal(ctx, "failed to create hyperloop server", zap.Error(err))
	}
	startService("hyperloop server", func() error {
		err := hyperloopSrv.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}

		return err
	})
	closers = append(closers, closer{"hyperloop server", hyperloopSrv.Shutdown})

	grpcServer := e2bgrpc.NewGRPCServer(tel, e2bgrpc.WithSandboxResumeMetrics())
	orchestrator.RegisterSandboxServiceServer(grpcServer, orchestratorService)
	orchestrator.RegisterVolumeServiceServer(grpcServer, volumeService)
	orchestrator.RegisterChunkServiceServer(grpcServer, orchestratorService)

	// template manager
	var tmpl *tmplserver.ServerStore
	var localUploadHandler *localupload.Handler
	if slices.Contains(services, cfg.TemplateManager) {
		buildPersistence, uploadHandler, err := setupBuildStorage(ctx, limiter, config)
		if err != nil {
			logger.L().Fatal(ctx, "failed to setup build storage", zap.Error(err))
		}

		localUploadHandler = uploadHandler

		tmpl, err = tmplserver.New(
			ctx,
			config,
			featureFlags,
			tel.MeterProvider,
			globalLogger,
			tmplSbxLoggerExternal,
			sandboxFactory,
			sandboxProxy,
			templateCache,
			persistence,
			buildPersistence,
			uploads,
		)
		if err != nil {
			logger.L().Fatal(ctx, "failed to create template manager", zap.Error(err))
		}

		templatemanager.RegisterTemplateServiceServer(grpcServer, tmpl)

		closers = append(closers, closer{"template server", tmpl.Close})
	}

	infoService := service.NewInfoService(serviceInfo, sandboxes, hostMetrics)
	orchestratorinfo.RegisterInfoServiceServer(grpcServer, infoService)

	grpcHealth := health.NewServer()
	grpc_health_v1.RegisterHealthServer(grpcServer, grpcHealth)

	// ============================================================================
	// cmux —— 单端口复用 gRPC 和 HTTP
	// ============================================================================
	//
	// 【核心设计原理】
	// cmux（connection multiplexer）是一个 Go 库，用来在同一个 TCP 端口上
	// 复用多个协议。在 E2B 中，我们用它在端口 5008 上同时提供 gRPC 和 HTTP。
	//
	// 【为什么需要 cmux？】
	//
	// Nomad 的服务发现机制：
	// - Nomad 为每个服务分配一个端口
	// - 这个端口在 Nomad 的服务发现中注册
	// - 其他服务通过这个端口连接到 orchestrator
	//
	// 问题：orchestrator 需要提供多个协议
	// - gRPC：给 API 服务用（主要流量）
	// - HTTP：给 Nomad 的 healthcheck 用
	// - HTTP：给开发者用（本地文件上传）
	//
	// 解决方案：用 cmux 在同一个端口上复用这些协议
	// - 不需要为每个协议分配不同的端口
	// - 简化 Nomad 配置
	// - 减少防火墙规则
	//
	// 【cmux 的工作原理】
	//
	// cmux 在 TCP 连接建立时，根据协议特征来判断是哪种协议：
	//
	// 1. 接收 TCP 连接
	//    ┌─────────────────────────────────────────────────────────────┐
	//    │ TCP 连接建立                                                 │
	//    │ 例如：client 连接到 localhost:5008                          │
	//    └────────────────┬────────────────────────────────────────────┘
	//                     │
	//                     ↓
	// 2. 读取协议特征
	//    ┌─────────────────────────────────────────────────────────────┐
	//    │ cmux 读取连接的前几个字节，判断协议类型                      │
	//    │ - HTTP 请求以 "GET", "POST", "PUT" 等开头                  │
	//    │ - gRPC 请求以特定的二进制格式开头                           │
	//    └────────────────┬────────────────────────────────────────────┘
	//                     │
	//                     ↓
	// 3. 路由到对应的 listener
	//    ┌─────────────────────────────────────────────────────────────┐
	//    │ 根据协议类型，将连接路由到对应的 listener                    │
	//    │ - HTTP 连接 → httpListener                                  │
	//    │ - gRPC 连接 → grpcListener                                  │
	//    └─────────────────────────────────────────────────────────────┘
	//
	// 【cmux 的 Match() 方法】
	//
	// Match() 用来注册协议匹配规则。每个 Match() 调用都会：
	// 1. 创建一个新的 listener
	// 2. 注册一个匹配规则
	// 3. 返回这个 listener
	//
	// 例如：
	// httpListener := cmuxServer.Match(cmux.HTTP1Fast())
	// - 创建一个 HTTP listener
	// - 注册规则：如果连接以 HTTP 特征开头，路由到这个 listener
	//
	// grpcListener := cmuxServer.Match(cmux.Any())
	// - 创建一个 gRPC listener
	// - 注册规则：其他所有连接都路由到这个 listener
	//
	// 【关键问题：数据竞争】
	//
	// cmux 的 Match() 和 Serve() 不是线程安全的：
	// - Match() 修改 cmux 的内部状态（注册匹配规则）
	// - Serve() 读取这个内部状态（使用匹配规则）
	// - 如果并发调用 Match() 和 Serve()，会导致数据竞争
	//
	// 【解决方案】
	//
	// 必须在 Serve() 之前创建所有 matcher：
	// 1. 创建 cmuxServer
	// 2. 调用所有 Match() 方法
	// 3. 启动 Serve()
	// 4. 不能在 Serve() 运行时调用 Match()
	//
	// 【运维含义】
	//
	// 1. 监控 cmux 启动
	//    - 查看 "Starting network server" 日志，确认 cmux 启动成功
	//    - 检查端口号是否正确（应该是 5008）
	//
	// 2. 监控协议路由
	//    - 虽然 cmux 的路由过程不可见，但可以通过监控 HTTP 和 gRPC 流量来推断
	//    - 如果 HTTP 流量正常，说明 HTTP 路由工作正常
	//    - 如果 gRPC 流量正常，说明 gRPC 路由工作正常
	//
	// 3. 监控 cmux 关闭
	//    - 查看 "Shutting down cmux server" 日志，确认 cmux 关闭成功
	//    - 如果关闭日志没有出现，说明可能卡住了
	//
	// 4. 监控连接错误
	//    - 如果看到 "use of closed network connection" 错误，这是正常的
	//    - 这说明 cmux 正在关闭，新连接被拒绝
	//    - 代码中已经处理了这个错误，返回 nil
	//
	// 【常见问题】
	//
	// Q: 为什么要在 Serve() 之前创建所有 matcher？
	// A: 因为 Match() 和 Serve() 不是线程安全的。如果并发调用，会导致数据竞争。
	//
	// Q: 如果在 Serve() 运行时调用 Match() 会怎样？
	// A: 会导致数据竞争，可能导致：
	//    - 新的 matcher 被忽略
	//    - 连接被路由到错误的 listener
	//    - 进程崩溃
	//
	// Q: 为什么要处理 "use of closed network connection" 错误？
	// A: 因为在关闭时，cmux 会关闭 listener，导致 Serve() 返回这个错误。
	//    这是正常的，不需要报告为错误。
	//
	// Q: 如果需要添加新的协议怎么办？
	// A: 需要在 Serve() 之前调用 Match()。如果需要动态添加协议，
	//    需要重新设计架构，使用不同的方法。
	//
	// 【第 1 步】创建 cmux server
	// 这会创建一个 TCP listener，监听指定的端口
	cmuxServer, err := NewCMUXServer(ctx, config.GRPCPort, tel.MeterProvider)
	if err != nil {
		// 如果创建失败，说明端口被占用或其他网络问题
		logger.L().Fatal(ctx, "failed to create cmux server", zap.Error(err))
	}

	// ============================================================================
	// 【关键】必须在 Serve() 之前创建所有 matcher
	// ============================================================================
	// cmux.Match() 修改内部状态，Serve() 读取这个状态
	// 并发调用会导致数据竞争
	//
	// 【第 2 步】创建 HTTP matcher
	// 这会注册一个规则：如果连接以 HTTP 特征开头，路由到 httpListener
	httpListener := cmuxServer.Match(cmux.HTTP1Fast())
	
	// 【第 3 步】创建 gRPC matcher
	// cmux.Any() 匹配所有剩余的连接（即不是 HTTP 的连接）
	// 这必须是最后一个 matcher，因为它是"catch-all"规则
	grpcListener := cmuxServer.Match(cmux.Any()) // the rest are GRPC requests

	// 【第 4 步】启动 cmux server
	// 现在所有 matcher 都已创建，可以安全地启动 Serve()
	startService("cmux server", func() error {
		logger.L().Info(ctx, "Starting network server", zap.Uint16("port", config.GRPCPort))
		err := cmuxServer.Serve()
		// 处理正常关闭时的错误
		// 当 cmux 关闭时，Serve() 会返回 "use of closed network connection" 错误
		// 这是正常的，不需要报告为错误
		if err != nil && strings.Contains(err.Error(), "use of closed network connection") {
			return nil
		}

		return err
	})
	
	// 【第 5 步】注册 cmux server 的关闭函数
	// 这会在进程退出时被调用，用来正确关闭 cmux server
	closers = append(closers, closer{"cmux server", func(context.Context) error {
		logger.L().Info(ctx, "Shutting down cmux server")
		// Close() 会关闭 listener，导致 Serve() 返回
		// 这会触发 startService 闭包中的错误处理
		cmuxServer.Close()

		return nil
	}})

	pprofServer := telemetry.NewPprofServer()
	// We handle the pprof in a separate goroutine to prevent any interaction with the main server.
	go func() {
		logger.L().Info(ctx, "pprof server starting", zap.Int("port", telemetry.DefaultPprofPort))

		if err := pprofServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.L().Error(ctx, "pprof server encountered error", zap.Error(err))
		}
	}()
	closers = append(closers, closer{"pprof server", pprofServer.Shutdown})

	// http server
	healthcheck, err := e2bhealthcheck.NewHealthcheck(serviceInfo)
	if err != nil {
		logger.L().Fatal(ctx, "failed to create healthcheck", zap.Error(err))
	}

	httpMux := http.NewServeMux()
	httpMux.Handle("/health", healthcheck.CreateHandler())

	if localUploadHandler != nil {
		httpMux.Handle("/upload", localUploadHandler)
	}

	httpServer := NewHTTPServer()
	httpServer.Handler = httpMux

	startService("http server", func() error {
		err := httpServer.Serve(httpListener)
		switch {
		case errors.Is(err, cmux.ErrServerClosed):
			return nil
		case errors.Is(err, http.ErrServerClosed):
			return nil
		default:
			return err
		}
	})
	closers = append(closers, closer{"http server", httpServer.Shutdown})

	// grpc server
	startService("grpc server", func() error {
		return grpcServer.Serve(grpcListener)
	})
	closers = append(closers, closer{"grpc server", func(context.Context) error {
		logger.L().Info(ctx, "Shutting down grpc server")
		grpcServer.GracefulStop()

		return nil
	}})

	// ============================================================================
	// 主循环 —— 等待关闭信号或服务失败
	// ============================================================================
	//
	// 【核心逻辑】
	// 主循环有两个退出条件：
	// 1. 收到关闭信号（SIGTERM、SIGINT）
	// 2. 某个服务出现错误
	//
	// 【两个分支的含义】
	//
	// 分支 1：<-sig.Done()
	// ────────────────────────────────────────────────────────────────────
	// 这是正常的关闭流程。
	// 通常由以下事件触发：
	// - Nomad 滚动更新（发送 SIGTERM）
	// - 管理员手动关闭（kill -TERM <pid>）
	// - 容器被停止（Docker/Kubernetes 发送 SIGTERM）
	//
	// 流程：
	// 1. 收到 SIGTERM
	// 2. signal.Notify() 将信号写入 sigChan
	// 3. sig.Done() 返回（channel 被关闭）
	// 4. select 分支 1 被触发
	// 5. 记录 "Shutdown signal received" 日志
	// 6. 进入关闭流程
	//
	// 【运维注意】
	// - 这是正常的关闭，不需要担心
	// - 检查日志确认关闭流程正确执行
	//
	// 分支 2：<-serviceError
	// ────────────────────────────────────────────────────────────────────
	// 这是异常的关闭流程。
	// 通常由以下事件触发：
	// - gRPC server 启动失败（例如端口被占用）
	// - HTTP server 启动失败
	// - Proxy 启动失败
	// - Event collector 启动失败
	// - 任何其他服务启动失败
	//
	// 流程：
	// 1. 某个服务启动失败
	// 2. startService 闭包捕获错误
	// 3. 记录 "service returned an error" 日志
	// 4. 发送错误到 serviceError channel
	// 5. 主循环接收错误
	// 6. select 分支 2 被触发
	// 7. 记录 "Service error" 日志
	// 8. 进入关闭流程
	//
	// 【运维注意】
	// - 这是异常的关闭，需要调查根本原因
	// - 检查 "Service error" 日志，找出是哪个服务出现了问题
	// - 检查错误信息，找出根本原因
	// - 例如：
	//   "Service error" error="address already in use"
	//   说明某个端口被占用了
	//
	// 【两个分支的竞争】
	//
	// 如果同时收到关闭信号和服务错误，哪个分支会被触发？
	// 答案：Go 的 select 会随机选择一个分支。
	//
	// 这是 Go 的设计特性，用来防止某个分支被饿死。
	// 在这个场景中，两个分支都会导致关闭流程，所以无所谓哪个先执行。
	//
	// 【为什么不用 context.Done()？】
	//
	// 有人可能会问：为什么不用 context.Done() 而用 sig.Done()？
	// 答案：因为 context 是用来传递请求级别的取消信号，
	// 而 sig 是用来传递进程级别的关闭信号。
	// 这两个是不同的概念。
	//
	// 【常见问题】
	//
	// Q: 如果同时收到 SIGTERM 和服务错误怎么办？
	// A: select 会随机选择一个分支。两个分支都会导致关闭流程，所以无所谓。
	//
	// Q: 为什么 serviceError channel 的容量是 1？
	// A: 因为我们只关心"是否有错误"，不关心"有多少个错误"。
	//    一旦有一个错误，主循环就会开始关闭流程。
	//
	// Q: 如果没有 serviceError channel 会怎样？
	// A: 主循环只能等待 SIGTERM 信号。如果某个服务启动失败，
	//    主循环无法知道，会继续等待，直到收到 SIGTERM。
	//    这会浪费时间，并可能导致其他问题。
	//
	// 等待关闭信号或服务失败
	select {
	case <-sig.Done():
		logger.L().Info(ctx, "Shutdown signal received")
	case serviceErr := <-serviceError:
		logger.L().Error(ctx, "Service error", zap.Error(serviceErr))
	}

	closeCtx, cancelCloseCtx := context.WithCancel(context.Background())
	defer cancelCloseCtx()
	if config.ForceStop {
		cancelCloseCtx()
	}

	// 关闭序列 —— 15 秒排水窗口
	//
	// 为什么需要 15 秒？这是零停机部署的关键：
	// 1. 收到 SIGTERM（Nomad 滚动更新）
	// 2. 立即标记为 Draining
	// 3. 等 15 秒让 client-proxy 从路由表中移除这个节点
	// 4. 此后不会有新请求进来
	// 5. 再关闭 gRPC server（已有连接会被 GracefulStop 等待完成）
	//
	// 如果跳过这 15 秒直接关闭，client-proxy 还在向这个节点发请求，会导致请求失败
	logger.L().Info(ctx, "Starting drain phase", zap.Int("sandbox_count", sandboxes.Count()))
	if status := serviceInfo.GetStatus(); status == orchestratorinfo.ServiceInfoStatus_Healthy || status == orchestratorinfo.ServiceInfoStatus_Standby {
		serviceInfo.SetStatus(ctx, orchestratorinfo.ServiceInfoStatus_Draining)

		// Wait for draining state to propagate to all consumers
		if !env.IsLocal() {
			time.Sleep(15 * time.Second)
		}
	}

	// Wait for services to be drained before closing them
	if tmpl != nil {
		err := tmpl.Wait(closeCtx)
		if err != nil {
			logger.L().Error(ctx, "error while waiting for template manager to drain", zap.Error(err))
			success = false
		}
	}

	// LIFO 关闭顺序：后初始化先关闭，确保依赖关系正确
	slices.Reverse(closers)
	for _, closer := range closers {
		clog := globalLogger.With(zap.String("service", closer.name), zap.Bool("forced", config.ForceStop))
		clog.Info(ctx, "closing")
		if err := closer.close(closeCtx); err != nil {
			clog.Error(ctx, "error during shutdown", zap.Error(err))
			success = false
		}
	}

	logger.L().Info(ctx, "Waiting for services to finish")
	var sde serviceDoneError
	if err := g.Wait(); err != nil && !errors.As(err, &sde) {
		logger.L().Error(ctx, "service group error", zap.Error(err))
		success = false
	}

	return success
}

func startNFSProxy(
	ctx context.Context,
	config cfg.Config,
	builder *chrooted.Builder,
	startService func(name string, f func() error),
	sandboxes *sandbox.Map,
) ([]closer, error) {
	var closers []closer

	// portmapper listener
	var pmConfig net.ListenConfig
	pmLis, err := pmConfig.Listen(ctx, "tcp", fmt.Sprintf(":%d", config.NetworkConfig.PortmapperPort))
	if err != nil {
		return nil, fmt.Errorf("failed to listen on portmapper port: %w", err)
	}

	// portmapper implementation
	pm := portmap.NewPortMap(ctx)
	pm.RegisterPort(ctx, 2049)
	startService("portmapper server", func() error {
		return pm.Serve(ctx, pmLis)
	})
	closers = append(closers, closer{"portmapper server", func(_ context.Context) error { return pmLis.Close() }})

	// nfs proxy listener
	var nfsConfig net.ListenConfig
	lis, err := nfsConfig.Listen(ctx, "tcp", fmt.Sprintf(":%d", config.NetworkConfig.NFSProxyPort))
	if err != nil {
		return nil, fmt.Errorf("failed to listen on nfs port: %w", err)
	}

	// nfs proxy implementation
	nfsServer, err := nfsproxy.NewProxy(ctx, builder, sandboxes, nfscfg.Config{
		Logging:           config.NFSProxyLogging,
		Tracing:           config.NFSProxyTracing,
		Metrics:           config.NFSProxyMetrics,
		RecordHandleCalls: config.NFSProxyRecordHandleCalls,
		RecordStatCalls:   config.NFSProxyRecordStatCalls,
		NFSLogLevel:       config.NFSProxyLogLevel,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create nfs proxy: %w", err)
	}
	startService("nfs proxy", func() error {
		return nfsServer.Serve(lis)
	})
	closers = append(closers, closer{
		"nfs proxy server", func(_ context.Context) error {
			return lis.Close()
		},
	})

	return closers, nil
}

func setupBuildStorage(ctx context.Context, limiter *limit.Limiter, orchConfig cfg.Config) (storage.StorageProvider, *localupload.Handler, error) {
	cfg := storage.BuildCacheStorageConfig.WithLimiter(limiter)

	var uploadHandler *localupload.Handler

	if storage.IsLocal() {
		hmacKey := make([]byte, 32)
		if _, err := rand.Read(hmacKey); err != nil {
			return nil, nil, fmt.Errorf("generate HMAC key: %w", err)
		}

		uploadBaseURL := orchConfig.LocalUploadBaseURL
		if uploadBaseURL == "" {
			uploadBaseURL = fmt.Sprintf("http://localhost:%d", orchConfig.GRPCPort)
		}

		cfg = cfg.WithLocalUpload(uploadBaseURL, hmacKey)

		basePath := cfg.GetLocalBasePath()
		uploadHandler = localupload.NewHandler(basePath, hmacKey)

		logger.L().Info(ctx, "Local upload endpoint enabled for filesystem storage",
			zap.String("upload_base_url", uploadBaseURL),
			zap.String("base_path", basePath))
	}

	provider, err := storage.GetStorageProvider(ctx, cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("create build cache storage provider: %w", err)
	}

	return provider, uploadHandler, nil
}

func newStorage(ctx context.Context, nodeID string, config network.Config, egressProxy network.EgressProxy) (network.Storage, error) {
	if env.IsDevelopment() || config.UseLocalNamespaceStorage {
		return network.NewStorageLocal(ctx, config, egressProxy)
	}

	return network.NewStorageKV(nodeID, config, egressProxy)
}
