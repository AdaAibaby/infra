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

	// 文件锁机制 —— 防止双启动的最简单方案
	// 
	// 为什么需要这个？
	// - Orchestrator 管理 Firecracker 进程，两个实例同时运行会争抢同一批网络 Slot 和 NBD 设备
	// - 导致数据损坏、网络冲突、存储不一致
	//
	// 关键细节：
	// - 崩溃时锁文件不删除（success == false 时跳过 os.Remove）
	// - 下次启动会检测到并拒绝启动，强迫运维人员介入
	// - 开发模式跳过，方便本地热重载
	// - ForceStop 跳过，允许强制重启（Nomad 滚动更新场景）
	if !env.IsDevelopment() && !config.ForceStop && slices.Contains(services, cfg.Orchestrator) {
		fileLockName := config.OrchestratorLockPath
		info, err := os.Stat(fileLockName)
		if err == nil {
			log.Fatalf("Orchestrator was already started at %s, exiting", info.ModTime())
		}

		f, err := os.Create(fileLockName)
		if err != nil {
			log.Fatalf("Failed to create lock file %s: %v", fileLockName, err)
		}
		defer func() {
			fileErr := f.Close()
			if fileErr != nil {
				log.Printf("Failed to close lock file %s: %v", fileLockName, fileErr)
			}

			// Remove the lock file on graceful shutdown
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

	serviceError := make(chan error)
	defer close(serviceError)

	// errgroup + defer g.Wait() —— panic 安全的并发关闭
	//
	// 为什么用 defer 而不是在函数末尾调用 g.Wait()？
	// - run() 函数中有大量 logger.L().Fatal() 调用
	// - logger.Fatal() 内部调用 zap.Fatal()，会先 Sync() 日志再 os.Exit(1)，跳过所有 defer
	// - 但这个 defer g.Wait() 的真正价值是：如果 run() 因为 panic 退出，goroutine 仍然能被等待
	// - 防止进程在后台 goroutine 还在运行时就退出
	var g errgroup.Group
	defer func(g *errgroup.Group) {
		err := g.Wait()
		if err != nil {
			log.Printf("error while shutting down: %v", err)
			success = false
		}
	}(&g)

	// 遥测优先初始化 —— 可观测性是基础设施
	//
	// 为什么 telemetry 必须第一个初始化？
	// - logger 的 OTEL core 需要 tel.LogsProvider
	// - 后续所有组件的错误都需要通过 logger 上报
	// - 如果 telemetry 初始化失败，整个进程直接 Fatal
	// - 没有可观测性的服务不应该运行
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

	// 两套 Sandbox Logger —— 内外分离
	//
	// 为什么要分两套？
	// - External：发给用户的日志（通过 logs-collector 转发到用户的 Loki）
	//   不能包含内部 IP、内部错误细节
	// - Internal：发给 E2B 工程师的日志，包含完整的调试信息
	// - 这是多租户系统的安全边界：用户只能看到自己沙箱的日志，且不包含基础设施细节
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
	// 为什么用闭包而不是直接 g.Go？
	// 1. 统一日志：每个服务启动/退出都有一致的日志格式
	// 2. 错误传播：任何服务退出（无论正常还是异常）都通过 serviceError channel 通知主循环
	// 3. default 分支：防止第二个服务退出时阻塞（channel 容量为 1，第一个错误已经触发关闭流程）
	// 4. serviceDoneError：让 g.Wait() 能区分"服务正常退出"和"真正的错误"
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

	// Redis 的可选性 —— 优雅降级
	//
	// Redis 用于：
	// - P2P 模板文件传输（节点间互传，避免都从 GCS 下载）
	// - 沙箱事件流（ClickHouse 的补充）
	//
	// 这两个功能不是核心路径。Redis 不可用时：
	// - 模板从 GCS 下载（慢但可用）
	// - 事件只写 ClickHouse（不丢失）
	//
	// 这是可选依赖的标准 Go 模式：用 ErrRedisDisabled 哨兵错误区分"配置禁用"和"连接失败"
	redisClient, err := sharedFactories.NewRedisClient(ctx, sharedFactories.RedisConfig{
		RedisURL:         config.RedisURL,
		RedisClusterURL:  config.RedisClusterURL,
		RedisTLSCABase64: config.RedisTLSCABase64,
		PoolSize:         config.RedisPoolSize,
		MinIdleConns:     config.RedisMinIdleConns,
	})
	if err != nil && !errors.Is(err, sharedFactories.ErrRedisDisabled) {
		logger.L().Fatal(ctx, "Could not connect to Redis", zap.Error(err))
	} else if err == nil {
		closers = append(closers, closer{"redis client", func(context.Context) error {
			return sharedFactories.CloseCleanly(redisClient)
		}})
	}

	// 当 Redis 不可用时，使用 Nop 实现（空操作），不影响核心路径
	peerRegistry := peerclient.NopRegistry()
	peerResolver := peerclient.NopResolver()
	if nodeAddress := config.NodeAddress(); redisClient != nil && nodeAddress != nil {
		peerRegistry = peerclient.NewRedisRegistry(redisClient, *nodeAddress)
		peerResolver = peerclient.NewResolver(peerRegistry, *nodeAddress)
	}

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

	// cmux —— 单端口复用 gRPC 和 HTTP
	//
	// 为什么用 cmux？
	// - Nomad 的服务发现只注册一个端口（5008 GRPC_PORT）
	// - 用 cmux 在同一个 TCP 端口上同时提供：
	//   * GET /health → HTTP healthcheck（给 Nomad 用）
	//   * POST /upload → 本地文件上传（开发模式）
	//   * 其他 → gRPC（给 API 服务用）
	// - 减少 Nomad 端口配置复杂度
	//
	// 关键注意：必须在 Serve() 之前创建所有 matcher（避免数据竞争）
	// cmux 的 Match() 修改内部状态，Serve() 读取这个状态，并发调用会有数据竞争
	cmuxServer, err := NewCMUXServer(ctx, config.GRPCPort, tel.MeterProvider)
	if err != nil {
		logger.L().Fatal(ctx, "failed to create cmux server", zap.Error(err))
	}

	// Create all matchers BEFORE starting Serve() to avoid data race.
	// cmux.Match() modifies internal state that Serve() reads from.
	httpListener := cmuxServer.Match(cmux.HTTP1Fast())
	grpcListener := cmuxServer.Match(cmux.Any()) // the rest are GRPC requests

	startService("cmux server", func() error {
		logger.L().Info(ctx, "Starting network server", zap.Uint16("port", config.GRPCPort))
		err := cmuxServer.Serve()
		if err != nil && strings.Contains(err.Error(), "use of closed network connection") {
			return nil
		}

		return err
	})
	closers = append(closers, closer{"cmux server", func(context.Context) error {
		logger.L().Info(ctx, "Shutting down cmux server")
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
