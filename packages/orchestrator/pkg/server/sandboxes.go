//go:build linux

package server

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jellydator/ttlcache/v3"
	"github.com/launchdarkly/go-sdk-common/v3/ldcontext"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/fc"
	sbxtemplate "github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/template"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/template/metadata"
	"github.com/e2b-dev/infra/packages/shared/pkg/events"
	"github.com/e2b-dev/infra/packages/shared/pkg/featureflags"
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
	sbxlogger "github.com/e2b-dev/infra/packages/shared/pkg/logger/sandbox"
	sandbox_network "github.com/e2b-dev/infra/packages/shared/pkg/sandbox-network"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
	"github.com/e2b-dev/infra/packages/shared/pkg/telemetry"
	"github.com/e2b-dev/infra/packages/shared/pkg/utils"
)

// tracer 是 OpenTelemetry 分布式追踪的全局实例
// 用于在 gRPC 方法中创建 span，记录请求的执行路径和性能指标
var tracer = otel.Tracer("github.com/e2b-dev/infra/packages/orchestrator/pkg/server")

const (
	// requestTimeout 是所有 gRPC 请求的最大执行时间
	// 防止长时间运行的操作阻塞 gRPC 线程池
	requestTimeout = 60 * time.Second

	// acquireTimeout 是获取恢复沙箱快照信号量的最大等待时间
	// 用于限制并发恢复操作数量，防止节点过载
	acquireTimeout = 15 * time.Second

	// uploadTimeout 是上传快照文件到远程存储的最大允许时间
	// 包括内存快照、根文件系统快照和元数据的上传
	uploadTimeout = 20 * time.Minute

	// redisPeerKeyTTL 是 Redis 对等节点键的生存时间
	// 略长于 uploadTimeout，确保在整个上传窗口内键仍然有效
	// 用于点对点快照传输的服务发现
	redisPeerKeyTTL = uploadTimeout + 2*time.Minute

	// executionEventDataKey 是 webhook 事件数据中沙箱执行指标的键
	// 包含 CPU 数量、内存大小、执行时间等信息
	executionEventDataKey = "execution"
)

// Create 是 gRPC 方法，用于创建新的沙箱或恢复已暂停的沙箱
// 
// 设计模式：
// 1. 超时管理：使用 context.WithTimeoutCause 设置 60 秒超时，防止请求无限期挂起
// 2. 分布式追踪：创建 OpenTelemetry span 记录请求执行路径和性能指标
// 3. 并发控制：通过信号量限制同时启动的沙箱数量，防止节点过载
// 4. 快照恢复：支持从暂停状态恢复沙箱，保持执行 ID 不变以维持 API 路由稳定性
// 5. 事件发布：异步发布沙箱创建/恢复事件到事件总线，用于 webhook 和分析
//
// 流程：
// - 验证资源限制（最大运行沙箱数、启动中沙箱数）
// - 获取或创建模板快照
// - 创建沙箱配置（CPU、内存、网络、存储）
// - 调用 sandboxFactory.ResumeSandbox 启动 Firecracker VM
// - 设置沙箱生命周期管理（清理 goroutine）
// - 发布事件到事件总线
func (s *Server) Create(ctx context.Context, req *orchestrator.SandboxCreateRequest) (_ *orchestrator.SandboxCreateResponse, createErr error) {
	// 为此请求设置最大超时时间
	// 使用 WithTimeoutCause 可以在超时时获取具体的超时原因
	ctx, cancel := context.WithTimeoutCause(ctx, requestTimeout, errors.New("request timed out"))
	defer cancel()

	// 设置分布式追踪 span
	// 记录此 gRPC 方法的执行路径、性能指标和错误信息
	ctx, childSpan := tracer.Start(ctx, "sandbox-create")
	defer childSpan.End()

	// 判断是否为恢复操作（从快照恢复）
	isResume := req.GetSandbox().GetSnapshot()
	createStart := time.Now()
	
	// 延迟函数：记录沙箱创建耗时指标
	// 只在成功创建时记录，失败时不记录（避免污染指标）
	defer func() {
		if createErr != nil {
			return
		}

		// 记录到 OpenTelemetry 指标系统
		// 包含 resume 标签用于区分新建和恢复操作
		s.sandboxCreateDuration.Record(ctx, time.Since(createStart).Milliseconds(),
			metric.WithAttributes(
				attribute.Bool("sandbox.resume", isResume),
			),
		)
	}()

	// 为 span 添加关键属性用于追踪和分析
	// 这些属性会被导出到 Grafana Tempo 用于分布式追踪可视化
	childSpan.SetAttributes(
		telemetry.WithBuildID(req.GetSandbox().GetBuildId()),
		telemetry.WithTeamID(req.GetSandbox().GetTeamId()),
		telemetry.WithTemplateID(req.GetSandbox().GetTemplateId()),
		telemetry.WithKernelVersion(req.GetSandbox().GetKernelVersion()),
		telemetry.WithSandboxID(req.GetSandbox().GetSandboxId()),
		telemetry.WithEnvdVersion(req.GetSandbox().GetEnvdVersion()),
	)

	// 设置 LaunchDarkly 特性标志上下文
	// 允许根据沙箱属性（模板、内核版本、Firecracker 版本）进行动态特性开关
	// 支持灰度发布和 A/B 测试
	ctx = featureflags.AddToContext(
		ctx,
		// 沙箱级别的上下文：用于沙箱特定的特性开关
		ldcontext.NewBuilder(req.GetSandbox().GetSandboxId()).
			Kind(featureflags.SandboxKind).
			SetString(featureflags.SandboxTemplateAttribute, req.GetSandbox().GetTemplateId()).
			SetString(featureflags.SandboxKernelVersionAttribute, req.GetSandbox().GetKernelVersion()).
			SetString(featureflags.SandboxFirecrackerVersionAttribute, req.GetSandbox().GetFirecrackerVersion()).
			Build(),
		// 团队级别的上下文：用于团队特定的特性开关
		ldcontext.NewBuilder(req.GetSandbox().GetTeamId()).
			Kind(featureflags.TeamKind).
			Build(),
		// 版本上下文：用于客户端版本相关的特性开关
		featureflags.VersionContext(s.info.ClientId, s.info.SourceCommit),
	)

	// 获取此节点允许的最大运行沙箱数
	// 这是一个动态配置，可以通过 LaunchDarkly 调整
	maxRunningSandboxesPerNode := s.featureFlags.IntFlag(ctx, featureflags.MaxSandboxesPerNode)

	// 检查是否已达到最大运行沙箱数
	// 这是一个硬限制，防止节点过载
	runningSandboxes := s.sandboxFactory.Sandboxes.Count()
	if runningSandboxes >= maxRunningSandboxesPerNode {
		telemetry.ReportEvent(ctx, "max number of running sandboxes reached")

		// 返回 ResourceExhausted 错误，客户端应该重试或选择其他节点
		return nil, status.Errorf(codes.ResourceExhausted, "max number of running sandboxes on node reached (%d), please retry", maxRunningSandboxesPerNode)
	}

	// 并发控制：限制同时启动的沙箱数量
	// 恢复操作和新建操作使用不同的策略：
	// - 恢复操作：使用 waitForAcquire 等待信号量（可能等待较长时间）
	// - 新建操作：使用 TryAcquire 立即返回（不等待）
	if req.GetSandbox().GetSnapshot() {
		// 恢复操作：等待获取信号量
		// 这允许恢复操作排队等待，因为恢复通常比新建快
		err := s.waitForAcquire(ctx)
		if err != nil {
			return nil, err
		}
	} else {
		// 新建操作：尝试立即获取信号量
		// 如果无法获取，立即返回错误（不排队）
		acquired := s.startingSandboxes.TryAcquire(1)
		if !acquired {
			telemetry.ReportEvent(ctx, "too many starting sandboxes on node")

			return nil, status.Errorf(codes.ResourceExhausted, "too many sandboxes starting on this node, please retry")
		}
	}
	// 确保在函数返回时释放信号量
	// 这是 RAII 模式的应用，保证资源正确释放
	defer s.startingSandboxes.Release(1)

	// 获取模板快照数据
	// 模板包含预构建的根文件系统、内核和 Firecracker 配置
	// 如果是恢复操作，会获取之前暂停时保存的快照
	template, err := s.templateCache.GetTemplate(
		ctx,
		req.GetSandbox().GetBuildId(),
		req.GetSandbox().GetSnapshot(),
		false,
		sbxtemplate.GetTemplateOpts{MaxSandboxLengthHours: req.GetSandbox().GetMaxSandboxLength()},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to get template snapshot data: %w", err)
	}

	// 克隆网络配置以避免修改原始请求
	// protobuf 的 proto.CloneOf 进行深拷贝，确保配置隔离
	network := proto.CloneOf(req.GetSandbox().GetNetwork())

	// 处理互联网访问权限
	// TODO: 临时基于全局配置设置，应该后续移除
	// https://linear.app/e2b/issue/ENG-3291
	// （应该从 API 传递网络配置）
	allowInternet := s.config.AllowSandboxInternet
	if req.GetSandbox().AllowInternetAccess != nil {
		allowInternet = req.GetSandbox().GetAllowInternetAccess()
	}
	
	// 如果不允许互联网访问，添加出站流量限制规则
	// 使用 CIDR 0.0.0.0/0 拒绝所有互联网流量
	if !allowInternet {
		if network == nil {
			network = &orchestrator.SandboxNetworkConfig{}
		}
		if network.GetEgress() == nil {
			network.Egress = &orchestrator.SandboxNetworkEgressConfig{}
		}
		network.Egress.DeniedCidrs = []string{sandbox_network.AllInternetTrafficCIDR}
	}

	// 解析 Firecracker 版本
	// 支持通过特性标志进行版本灰度发布
	resolvedFCVersion := featureflags.ResolveFirecrackerVersion(ctx, s.featureFlags, req.GetSandbox().GetFirecrackerVersion())
	
	// 转换卷挂载配置
	// 将 API 请求中的卷挂载转换为内部模型
	volumeMounts, err := createVolumeMountModelsFromAPI(req.GetSandbox().GetVolumeMounts())
	if err != nil {
		return nil, fmt.Errorf("failed to convert volume mounts: %w", err)
	}

	// 构建沙箱配置对象
	// 这个配置包含了启动 Firecracker VM 所需的所有参数
	config := sandbox.NewConfig(sandbox.Config{
		BaseTemplateID: req.GetSandbox().GetBaseTemplateId(),

		// 计算资源配置
		Vcpu:            req.GetSandbox().GetVcpu(),
		RamMB:           req.GetSandbox().GetRamMb(),
		TotalDiskSizeMB: req.GetSandbox().GetTotalDiskSizeMb(),
		HugePages:       req.GetSandbox().GetHugePages(),

		// 网络配置（包含出站流量限制）
		Network: network,

		// Envd 守护进程配置
		// Envd 是在 VM 内运行的进程管理守护进程
		Envd: sandbox.EnvdMetadata{
			Version:     req.GetSandbox().GetEnvdVersion(),
			AccessToken: req.GetSandbox().EnvdAccessToken,
			Vars:        req.GetSandbox().GetEnvVars(),
		},

		// Firecracker 虚拟机配置
		FirecrackerConfig: fc.Config{
			KernelVersion:      req.GetSandbox().GetKernelVersion(),
			FirecrackerVersion: resolvedFCVersion,
		},

		// 存储配置
		VolumeMounts:          volumeMounts,
		MaxSandboxLengthHours: req.GetSandbox().GetMaxSandboxLength(),
	})
	
	// 为 span 添加 Firecracker 版本属性
	childSpan.SetAttributes(
		telemetry.WithFirecrackerVersion(config.FirecrackerConfig.FirecrackerVersion),
	)

	// 构建运行时元数据
	// 这些信息用于追踪、日志和事件发布
	runtime := sandbox.RuntimeMetadata{
		TemplateID:  req.GetSandbox().GetTemplateId(),
		SandboxID:   req.GetSandbox().GetSandboxId(),
		ExecutionID: req.GetSandbox().GetExecutionId(),
		TeamID:      req.GetSandbox().GetTeamId(),
		BuildID:     req.GetSandbox().GetBuildId(),
		SandboxType: sandbox.SandboxTypeSandbox,
	}

	// 启动沙箱（创建或恢复）
	// 这是最关键的步骤，涉及：
	// 1. 分配网络资源（IP 地址、网络命名空间）
	// 2. 启动 Firecracker 进程
	// 3. 加载内核和根文件系统
	// 4. 启动 Envd 守护进程
	// 5. 建立与 VM 的 gRPC 连接
	sbx, err := s.sandboxFactory.ResumeSandbox(
		ctx,
		template,
		config,
		runtime,
		req.GetStartTime().AsTime(),
		req.GetEndTime().AsTime(),
		req.GetSandbox(),
	)
	if err != nil {
		// 处理特定的错误情况
		if errors.Is(err, storage.ErrObjectNotExist) {
			// 快照数据未找到
			// 这通常意味着快照文件还未上传到远程存储
			telemetry.ReportError(ctx, "sandbox files not found", err, telemetry.WithSandboxID(req.GetSandbox().GetSandboxId()))

			return nil, status.Errorf(codes.FailedPrecondition, "sandbox files for '%s' not found", req.GetSandbox().GetSandboxId())
		}

		// 通用错误处理
		// 合并超时错误和其他错误信息
		err = errors.Join(err, context.Cause(ctx))
		telemetry.ReportCriticalError(ctx, "failed to create sandbox", err)
		
		// 详细的错误日志，包含所有相关的沙箱配置信息
		logger.L().Error(ctx, "failed to create sandbox", zap.Error(err),
			logger.WithSandboxID(runtime.SandboxID),
			logger.WithBuildID(runtime.BuildID),
			logger.WithTemplateID(runtime.TemplateID),
			logger.WithEnvdVersion(config.Envd.Version),
			logger.WithKernelVersion(config.FirecrackerConfig.KernelVersion),
			logger.WithFirecrackerVersion(config.FirecrackerConfig.FirecrackerVersion),
		)

		return nil, status.Errorf(codes.Internal, "failed to create sandbox: %s", err)
	}

	// 设置沙箱生命周期管理
	// 这会启动一个后台 goroutine 监听沙箱停止事件并进行清理
	s.setupSandboxLifecycle(ctx, sbx)

	// 确定事件类型
	// 区分新建和恢复操作，用于 webhook 和分析
	eventType := events.SandboxCreatedEventPair
	if req.GetSandbox().GetSnapshot() {
		eventType = events.SandboxResumedEventPair
	}

	// 准备事件数据
	teamID, buildId, eventData := s.prepareSandboxEventData(ctx, sbx)
	
	// 异步发布事件到事件总线
	// 使用 context.WithoutCancel 确保即使 gRPC 请求超时，事件仍然会被发布
	// 这是一个重要的设计模式：不让事件发布阻塞 gRPC 响应
	go s.sbxEventsService.Publish(
		context.WithoutCancel(ctx),
		teamID,
		events.SandboxEvent{
			Version:   events.StructureVersionV2,
			ID:        uuid.New(),
			Type:      eventType.Type,
			Timestamp: time.Now().UTC(),

			EventData:          eventData,
			SandboxID:          sbx.Runtime.SandboxID,
			SandboxExecutionID: sbx.Runtime.ExecutionID,
			SandboxTemplateID:  sbx.Config.BaseTemplateID,
			SandboxBuildID:     buildId,
			SandboxTeamID:      teamID,
		},
	)

	// 返回成功响应
	// 包含客户端 ID 用于追踪和日志关联
	return &orchestrator.SandboxCreateResponse{
		ClientId: s.info.ClientId,
	}, nil
}

func createVolumeMountModelsFromAPI(volumeMounts []*orchestrator.SandboxVolumeMount) ([]sandbox.VolumeMountConfig, error) {
	var errs []error

	results := make([]sandbox.VolumeMountConfig, 0, len(volumeMounts))

	for _, v := range volumeMounts {
		volumeID, err := uuid.Parse(v.GetId())
		if err != nil {
			errs = append(errs, fmt.Errorf("invalid volume id %q: %w", v.GetId(), err))

			continue
		}

		results = append(results, sandbox.VolumeMountConfig{
			ID:   volumeID,
			Name: v.GetName(),
			Path: v.GetPath(),
			Type: v.GetType(),
		})
	}

	return results, errors.Join(errs...)
}

func (s *Server) Update(ctx context.Context, req *orchestrator.SandboxUpdateRequest) (*emptypb.Empty, error) {
	ctx, childSpan := tracer.Start(ctx, "sandbox-update")
	defer childSpan.End()

	childSpan.SetAttributes(
		telemetry.WithSandboxID(req.GetSandboxId()),
		attribute.String("client.id", s.info.ClientId),
	)

	sbx, ok := s.sandboxFactory.Sandboxes.Get(req.GetSandboxId())
	if !ok {
		telemetry.ReportCriticalError(ctx, "sandbox not found", nil)

		return nil, status.Error(codes.NotFound, "sandbox not found")
	}

	var updates []utils.UpdateFunc

	if req.GetEndTime() != nil {
		updates = append(updates, func(_ context.Context) (func(context.Context), error) {
			old := sbx.GetEndAt()
			sbx.SetEndAt(req.GetEndTime().AsTime())

			return func(_ context.Context) { sbx.SetEndAt(old) }, nil
		})
	}

	if req.GetEgress() != nil {
		updates = append(updates, func(ctx context.Context) (func(context.Context), error) {
			oldEgress := sbx.Config.GetNetworkEgress()

			if err := sbx.Slot.UpdateInternet(ctx, req.GetEgress()); err != nil {
				return nil, fmt.Errorf("failed to update sandbox network: %w", err)
			}

			egress := req.GetEgress()
			if len(egress.GetAllowedCidrs()) == 0 && len(egress.GetDeniedCidrs()) == 0 && len(egress.GetAllowedDomains()) == 0 && len(egress.GetRules()) == 0 {
				sbx.Config.SetNetworkEgress(nil)
			} else {
				sbx.Config.SetNetworkEgress(egress)
			}

			return func(ctx context.Context) {
				_ = sbx.Slot.UpdateInternet(ctx, oldEgress)
				sbx.Config.SetNetworkEgress(oldEgress)
			}, nil
		})
	}

	if err := utils.ApplyAllOrNone(ctx, updates); err != nil {
		telemetry.ReportCriticalError(ctx, "failed to update sandbox", err)

		return nil, status.Errorf(codes.Internal, "failed to update sandbox: %s", err)
	}

	// Publish event if any updates were applied.
	if len(updates) > 0 {
		teamID, buildId, eventData := s.prepareSandboxEventData(ctx, sbx)
		if req.GetEndTime() != nil {
			eventData["set_timeout"] = req.GetEndTime().AsTime().Format(time.RFC3339)
		}
		if egress := req.GetEgress(); egress != nil {
			eventData["network_egress"] = map[string]any{
				"allowed_cidrs":   egress.GetAllowedCidrs(),
				"denied_cidrs":    egress.GetDeniedCidrs(),
				"allowed_domains": egress.GetAllowedDomains(),
			}
		}

		go s.sbxEventsService.Publish(
			context.WithoutCancel(ctx),
			teamID,
			events.SandboxEvent{
				Version:   events.StructureVersionV2,
				ID:        uuid.New(),
				Type:      events.SandboxUpdatedEventPair.Type,
				Timestamp: time.Now().UTC(),

				EventData:          eventData,
				SandboxID:          sbx.Runtime.SandboxID,
				SandboxExecutionID: sbx.Runtime.ExecutionID,
				SandboxTemplateID:  sbx.Config.BaseTemplateID,
				SandboxBuildID:     buildId,
				SandboxTeamID:      teamID,
			},
		)
	}

	return &emptypb.Empty{}, nil
}

func (s *Server) List(ctx context.Context, _ *emptypb.Empty) (*orchestrator.SandboxListResponse, error) {
	_, childSpan := tracer.Start(ctx, "sandbox-list")
	defer childSpan.End()

	items := s.sandboxFactory.Sandboxes.Items()

	sandboxes := make([]*orchestrator.RunningSandbox, 0, len(items))

	for _, sbx := range items {
		if sbx == nil {
			continue
		}

		if sbx.APIStoredConfig == nil {
			continue
		}

		startedAt := sbx.GetStartedAt()
		sandboxes = append(sandboxes, &orchestrator.RunningSandbox{
			Config:    sbx.APIStoredConfig,
			ClientId:  s.info.ClientId,
			StartTime: timestamppb.New(startedAt),
			EndTime:   timestamppb.New(sbx.GetEndAt()),
		})
	}

	return &orchestrator.SandboxListResponse{
		Sandboxes: sandboxes,
	}, nil
}

// Delete 是 gRPC 方法，用于删除（杀死）运行中的沙箱
//
// 设计模式：
// 1. 两阶段关闭：先标记为停止状态，再异步执行清理
// 2. 状态隔离：停止状态的沙箱从 Get/Items/Count 查询中排除，但仍可通过 IP 查询
// 3. 异步清理：不阻塞 gRPC 响应，在后台进行 Firecracker 进程清理
// 4. 健康检查：在停止前收集健康指标用于分析
// 5. 事件发布：异步发布沙箱杀死事件
//
// 流程：
// - 查找沙箱
// - 标记为停止状态（防止重复同步到 API）
// - 收集健康指标
// - 异步停止 Firecracker 进程
// - 发布事件
// - 立即返回成功响应
func (s *Server) Delete(ctxConn context.Context, in *orchestrator.SandboxDeleteRequest) (*emptypb.Empty, error) {
	// 设置请求超时
	ctx, cancel := context.WithTimeoutCause(ctxConn, requestTimeout, errors.New("request timed out"))
	defer cancel()

	// 创建追踪 span
	ctx, childSpan := tracer.Start(ctx, "sandbox-delete")
	defer childSpan.End()

	// 添加追踪属性
	childSpan.SetAttributes(
		telemetry.WithSandboxID(in.GetSandboxId()),
		attribute.String("client.id", s.info.ClientId),
	)

	// 查找沙箱
	sbx, ok := s.sandboxFactory.Sandboxes.Get(in.GetSandboxId())
	if !ok {
		telemetry.ReportCriticalError(ctx, "sandbox not found", nil, telemetry.WithSandboxID(in.GetSandboxId()))

		return nil, status.Errorf(codes.NotFound, "sandbox '%s' not found", in.GetSandboxId())
	}

	// 标记沙箱为停止状态
	// 这是一个关键的设计模式：
	// - 从 Get/Items/Count 查询中排除（防止 API 再次同步）
	// - 仍然可以通过 GetByHostPort 查询（用于日志路由）
	// - 允许清理 goroutine 继续运行直到完成
	marked := s.sandboxFactory.Sandboxes.MarkStopping(ctx, sbx.Runtime.SandboxID, sbx.LifecycleID)
	if !marked {
		telemetry.ReportCriticalError(ctx, "failed to mark sandbox as stopping", nil, telemetry.WithSandboxID(in.GetSandboxId()))

		return nil, status.Errorf(codes.Internal, "failed to delete sandbox '%s'", in.GetSandboxId())
	}

	// 记录沙箱杀死事件
	sbxlogger.E(sbx).Info(ctx, "Killing sandbox")

	// 在停止前检查健康指标
	// 这些指标用于分析沙箱的运行状态和性能
	sbx.Checks.Healthcheck(ctx, true)

	// 异步停止沙箱
	// 这是一个重要的设计模式：不阻塞 gRPC 响应
	// 初始的 kill 请求应该是 Stop 中的第一个操作，此时沙箱已经从路由中移除
	// 我们不在这里等待整个清理完成
	go func() {
		err := sbx.Stop(context.WithoutCancel(ctx))
		if err != nil {
			sbxlogger.I(sbx).Error(ctx, "error stopping sandbox", logger.WithSandboxID(in.GetSandboxId()), zap.Error(err))
		}
	}()

	// 准备事件数据
	teamID, buildId, eventData := s.prepareSandboxEventData(ctx, sbx)
	
	// 如果启用了执行指标 webhook 标志，添加执行数据
	// 这允许通过特性标志控制是否在 webhook 中包含执行指标
	if s.featureFlags.BoolFlag(ctx, featureflags.ExecutionMetricsOnWebhooksFlag) {
		eventData[executionEventDataKey] = s.getSandboxExecutionData(sbx)
	}

	// 发布沙箱杀死事件
	eventType := events.SandboxKilledEventPair
	go s.sbxEventsService.Publish(
		context.WithoutCancel(ctx),
		teamID,
		events.SandboxEvent{
			Version:   events.StructureVersionV2,
			ID:        uuid.New(),
			Type:      eventType.Type,
			Timestamp: time.Now().UTC(),

			EventData:          eventData,
			SandboxID:          sbx.Runtime.SandboxID,
			SandboxExecutionID: sbx.Runtime.ExecutionID,
			SandboxTemplateID:  sbx.Config.BaseTemplateID,
			SandboxBuildID:     buildId,
			SandboxTeamID:      teamID,
		},
	)

	// 立即返回成功响应
	// 实际的清理工作在后台进行
	return &emptypb.Empty{}, nil
}

func (s *Server) Pause(ctx context.Context, in *orchestrator.SandboxPauseRequest) (*emptypb.Empty, error) {
	ctx, childSpan := tracer.Start(ctx, "sandbox-pause")
	defer childSpan.End()

	ctx = featureflags.AddToContext(
		ctx,
		ldcontext.NewBuilder(in.GetSandboxId()).
			Kind(featureflags.SandboxKind).
			SetString(featureflags.SandboxTemplateAttribute, in.GetTemplateId()).
			Build(),
	)

	sbx, ok := s.sandboxFactory.Sandboxes.Get(in.GetSandboxId())
	if !ok {
		telemetry.ReportCriticalError(ctx, "sandbox not found", nil, telemetry.WithSandboxID(in.GetSandboxId()))

		return nil, status.Error(codes.NotFound, "sandbox not found")
	}

	marked := s.sandboxFactory.Sandboxes.MarkStopping(ctx, sbx.Runtime.SandboxID, sbx.LifecycleID)
	if !marked {
		telemetry.ReportCriticalError(ctx, "failed to mark sandbox as stopping", nil, telemetry.WithSandboxID(in.GetSandboxId()))

		return nil, status.Error(codes.Internal, "failed to pause sandbox")
	}

	sbxlogger.E(sbx).Info(ctx, "Pausing sandbox")

	// Stop the old sandbox in background after we're done
	defer s.stopSandboxAsync(context.WithoutCancel(ctx), sbx)

	// Fire and forget - upload completes in the background
	res, err := s.snapshotAndCacheSandbox(ctx, sbx, in.GetBuildId())
	if err != nil {
		telemetry.ReportCriticalError(ctx, "error snapshotting sandbox", err, telemetry.WithSandboxID(in.GetSandboxId()))

		return nil, status.Errorf(codes.Internal, "error snapshotting sandbox '%s': %s", in.GetSandboxId(), err)
	}

	s.uploadSnapshotAsync(ctx, sbx, res)

	teamID, buildId, eventData := s.prepareSandboxEventData(ctx, sbx)
	if s.featureFlags.BoolFlag(ctx, featureflags.ExecutionMetricsOnWebhooksFlag) {
		eventData[executionEventDataKey] = s.getSandboxExecutionData(sbx)
	}

	eventType := events.SandboxPausedEventPair
	go s.sbxEventsService.Publish(
		context.WithoutCancel(ctx),
		teamID,
		events.SandboxEvent{
			Version:   events.StructureVersionV2,
			ID:        uuid.New(),
			Type:      eventType.Type,
			Timestamp: time.Now().UTC(),

			EventData:          eventData,
			SandboxID:          sbx.Runtime.SandboxID,
			SandboxExecutionID: sbx.Runtime.ExecutionID,
			SandboxTemplateID:  sbx.Config.BaseTemplateID,
			SandboxBuildID:     buildId,
			SandboxTeamID:      teamID,
		},
	)

	return &emptypb.Empty{}, nil
}

// Checkpoint 是 gRPC 方法，用于创建沙箱快照并恢复新的沙箱实例
//
// 设计模式：
// 1. 原子操作：快照 + 恢复作为一个原子操作，保证一致性
// 2. 生命周期管理：旧沙箱停止，新沙箱接管，防止资源泄漏
// 3. 预取优化：收集内存预取数据以加速后续恢复
// 4. 异步上传：支持同步和异步快照上传（通过特性标志控制）
// 5. 点对点传输：支持对等节点从此节点拉取快照块
//
// 流程：
// - 验证 Envd 版本兼容性
// - 获取启动信号量
// - 标记旧沙箱为停止状态
// - 创建快照并缓存到本地
// - 恢复新沙箱（保持 ExecutionID 不变）
// - 收集内存预取数据
// - 上传快照到远程存储
// - 发布事件
func (s *Server) Checkpoint(ctx context.Context, in *orchestrator.SandboxCheckpointRequest) (*orchestrator.SandboxCheckpointResponse, error) {
	// 创建追踪 span
	ctx, childSpan := tracer.Start(ctx, "sandbox-checkpoint")
	defer childSpan.End()

	// 添加特性标志上下文
	ctx = featureflags.AddToContext(
		ctx,
		ldcontext.NewBuilder(in.GetSandboxId()).
			Kind(featureflags.SandboxKind).
			Build(),
	)

	// 查找沙箱
	sbx, ok := s.sandboxFactory.Sandboxes.Get(in.GetSandboxId())
	if !ok {
		telemetry.ReportCriticalError(ctx, "sandbox not found", nil, telemetry.WithSandboxID(in.GetSandboxId()))

		return nil, status.Errorf(codes.NotFound, "sandbox '%s' not found", in.GetSandboxId())
	}

	// 检查 Envd 版本兼容性
	// 某些 Envd 版本可能不支持快照功能
	if err := utils.CheckEnvdVersionForSnapshot(sbx.Config.Envd.Version); err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "%s", err.Error())
	}

	// 获取启动信号量
	// 这与 Create/Pause 操作相同，限制并发恢复操作数量
	if err := s.waitForAcquire(ctx); err != nil {
		return nil, err
	}
	defer s.startingSandboxes.Release(1)

	// 标记旧沙箱为停止状态
	marked := s.sandboxFactory.Sandboxes.MarkStopping(ctx, sbx.Runtime.SandboxID, sbx.LifecycleID)
	if !marked {
		telemetry.ReportCriticalError(ctx, "failed to mark sandbox as stopping", nil, telemetry.WithSandboxID(in.GetSandboxId()))

		return nil, status.Errorf(codes.Internal, "failed to checkpoint sandbox '%s'", in.GetSandboxId())
	}

	// 确保旧沙箱被停止
	// 这是一个关键的设计模式：
	// - 成功时：新恢复的沙箱接管
	// - 失败时：防止泄漏的沙箱（运行但无法寻址）
	// Stop 是幂等的，可以安全地多次调用
	defer s.stopSandboxAsync(context.WithoutCancel(ctx), sbx)

	// 记录检查点操作
	sbxlogger.E(sbx).Info(ctx, "Checkpointing sandbox")

	// 创建快照并缓存到本地
	res, err := s.snapshotAndCacheSandbox(ctx, sbx, in.GetBuildId())
	if err != nil {
		telemetry.ReportCriticalError(ctx, "error snapshotting sandbox for checkpoint", err, telemetry.WithSandboxID(in.GetSandboxId()))

		return nil, status.Errorf(codes.Internal, "error snapshotting sandbox '%s': %s", in.GetSandboxId(), err)
	}

	// 获取用于恢复的模板
	// 这是一个快照模板，包含刚刚创建的快照数据
	template, err := s.templateCache.GetTemplate(ctx, in.GetBuildId(), true, false,
		sbxtemplate.GetTemplateOpts{MaxSandboxLengthHours: sbx.Config.MaxSandboxLengthHours})
	if err != nil {
		telemetry.ReportCriticalError(ctx, "error getting template for resume after checkpoint", err, telemetry.WithSandboxID(in.GetSandboxId()))

		return nil, status.Errorf(codes.Internal, "error getting template for resume: %s", err)
	}

	// 恢复沙箱
	// 这是一个关键的设计模式：
	// - 保持相同的 ExecutionID：为 API、路由目录和分析提供稳定的身份
	// - 使用新的 LifecycleID：防止旧沙箱的清理 goroutine 意外从映射中驱逐新沙箱
	// 这确保了从客户端的角度来看，沙箱是连续的，但内部状态是全新的
	resumedSbx, err := s.sandboxFactory.ResumeSandbox(
		ctx,
		template,
		sbx.Config,
		sandbox.RuntimeMetadata{
			TemplateID:  sbx.Runtime.TemplateID,
			SandboxID:   sbx.Runtime.SandboxID,
			ExecutionID: sbx.Runtime.ExecutionID,  // 保持相同的 ExecutionID
			TeamID:      sbx.Runtime.TeamID,
			BuildID:     sbx.Runtime.BuildID,
			SandboxType: sbx.Runtime.SandboxType,
		},
		sbx.GetStartedAt(),
		sbx.GetEndAt(),
		sbx.APIStoredConfig,
	)
	if err != nil {
		telemetry.ReportCriticalError(ctx, "error resuming sandbox after checkpoint", err, telemetry.WithSandboxID(in.GetSandboxId()))

		return nil, status.Errorf(codes.Internal, "error resuming sandbox after checkpoint: %s", err)
	}

	// 立即收集内存预取数据
	// 在恢复后立即收集时最准确，因为内存页面仍在缓存中
	// 预取数据用于优化后续恢复操作的性能
	prefetchData, prefetchErr := resumedSbx.MemoryPrefetchData(ctx)
	if prefetchErr != nil {
		sbxlogger.I(resumedSbx).Warn(ctx, "failed to get prefetch data for checkpoint", zap.Error(prefetchErr))
	}

	// 设置恢复沙箱的生命周期管理
	s.setupSandboxLifecycle(ctx, resumedSbx)

	// 将预取数据嵌入到元数据中
	// 这样预取数据会与快照文件一起上传，一次性完成
	if prefetchErr == nil {
		prefetchMapping := metadata.PrefetchEntriesToMapping(slices.Collect(maps.Values(prefetchData.BlockEntries)), prefetchData.BlockSize)
		if prefetchMapping != nil {
			res.meta = res.meta.WithPrefetch(&metadata.Prefetch{
				Memory: prefetchMapping,
			})

			if err := s.templateCache.UpdateMetadata(in.GetBuildId(), res.meta); err != nil {
				sbxlogger.I(resumedSbx).Warn(ctx, "failed to update local metadata with prefetch", zap.Error(err))
			}
		}
	}

	// 根据特性标志选择同步或异步上传
	if s.featureFlags.BoolFlag(ctx, featureflags.PeerToPeerAsyncCheckpointFlag) {
		// 异步模式：立即返回
		// 对等节点可以在上传窗口内从此节点拉取快照块
		// 这允许更快的响应时间，但需要对等节点支持
		s.uploadSnapshotAsync(ctx, resumedSbx, res)
	} else {
		// 同步模式：等待上传完成后再返回
		// 这样失败的上传会立即反馈给调用者
		// 如果上传失败，需要清理恢复的沙箱，因为没有持久化的快照，无法后续暂停或恢复
		uploadCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), uploadTimeout)
		defer cancel()

		err := res.upload.Run(uploadCtx)
		defer res.completeUpload(uploadCtx, err)

		if err != nil {
			telemetry.ReportCriticalError(ctx, "error uploading snapshot for checkpoint", err, telemetry.WithSandboxID(in.GetSandboxId()))

			// 清理恢复的沙箱
			s.sandboxFactory.Sandboxes.MarkStopping(ctx, resumedSbx.Runtime.SandboxID, resumedSbx.LifecycleID)
			s.stopSandboxAsync(context.WithoutCancel(ctx), resumedSbx)

			return nil, status.Errorf(codes.Internal, "error uploading snapshot for checkpoint '%s': %s", in.GetSandboxId(), err)
		}
	}

	// 发布检查点完成事件
	s.publishSandboxEvent(ctx, resumedSbx, events.SandboxCheckpointedEvent)

	// 报告检查点完成
	telemetry.ReportEvent(ctx, "Checkpoint completed")

	return &orchestrator.SandboxCheckpointResponse{}, nil
}

// Extracts common data needed for sandbox events
// prepareSandboxEventData 提取沙箱事件所需的通用数据
//
// 这个方法从沙箱对象中提取事件发布所需的信息：
// - 团队 ID：用于事件路由和权限检查
// - 构建 ID：用于追踪和分析
// - 事件数据：包含沙箱元数据和其他上下文信息
//
// 设计模式：
// - 浅拷贝元数据映射以避免竞态条件
// - 错误处理：解析失败时记录但不中断流程
func (s *Server) prepareSandboxEventData(ctx context.Context, sbx *sandbox.Sandbox) (uuid.UUID, string, map[string]any) {
	// 解析团队 ID
	teamID, err := uuid.Parse(sbx.Runtime.TeamID)
	if err != nil {
		sbxlogger.I(sbx).Error(ctx, "error parsing team ID", logger.WithSandboxID(sbx.Runtime.SandboxID), zap.Error(err))
	}

	buildId := ""
	eventData := make(map[string]any)
	if sbx.APIStoredConfig != nil {
		buildId = sbx.APIStoredConfig.GetBuildId()
		if sbx.APIStoredConfig.Metadata != nil {
			// Copy the map to avoid race conditions
			// 浅拷贝映射以避免竞态条件
			// 这是必要的，因为事件发布是异步的，原始映射可能被修改
			eventData["sandbox_metadata"] = utils.ShallowCopyMap(sbx.APIStoredConfig.GetMetadata())
		}
	}

	return teamID, buildId, eventData
}

// getSandboxExecutionData 收集沙箱执行指标
//
// 返回包含以下信息的映射：
// - started_at：沙箱启动时间（RFC3339 格式）
// - vcpu_count：分配的 CPU 核心数
// - memory_mb：分配的内存大小（MB）
// - execution_time：沙箱运行时长（毫秒）
//
// 这些指标用于 webhook 事件和分析，帮助用户了解沙箱的资源使用情况
func (s *Server) getSandboxExecutionData(sbx *sandbox.Sandbox) map[string]any {
	startedAt := sbx.GetStartedAt()

	return map[string]any{
		"started_at":     startedAt.UTC().Format(time.RFC3339),
		"vcpu_count":     sbx.Config.Vcpu,
		"memory_mb":      sbx.Config.RamMB,
		"execution_time": time.Since(startedAt).Milliseconds(),
	}
}

// snapshotResult holds the data produced by snapshotAndCacheSandbox that
// callers need to start the background remote storage upload.
// snapshotResult holds the data produced by snapshotAndCacheSandbox that
// callers need to start the background remote storage upload.
// snapshotResult 保存 snapshotAndCacheSandbox 产生的数据
// 调用者需要这些数据来启动后台远程存储上传
//
// 字段说明：
// - meta：快照元数据，包含构建 ID、内核版本等
// - upload：上传操作对象，用于执行远程存储上传
// - completeUpload：完成回调函数，用于清理和注销对等节点
type snapshotResult struct {
	meta           metadata.Template
	upload         *sandbox.Upload
	completeUpload func(ctx context.Context, uploadErr error)
}

// snapshotAndCacheSandbox creates a snapshot of a sandbox and adds it to the
// local template cache. The caller is responsible for starting the remote
// storage upload via uploadSnapshotAsync.
// snapshotAndCacheSandbox 创建沙箱快照并将其添加到本地模板缓存
//
// 这个方法执行以下步骤：
// 1. 获取模板元数据
// 2. 暂停沙箱（创建内存和根文件系统快照）
// 3. 将快照添加到本地缓存
// 4. 注册上传操作
// 5. 注册对等节点地址（如果启用了点对点传输）
//
// 调用者负责通过 uploadSnapshotAsync 启动远程存储上传
//
// 设计模式：
// - 两阶段提交：先添加到本地缓存，再注册上传
// - 对等节点支持：通过 Redis 注册此节点作为快照源
// - 错误恢复：如果上传失败，对等节点会自动注销
func (s *Server) snapshotAndCacheSandbox(
	ctx context.Context,
	sbx *sandbox.Sandbox,
	buildID string,
) (*snapshotResult, error) {
	// 获取模板元数据
	meta, err := sbx.Template.Metadata()
	if err != nil {
		return nil, fmt.Errorf("no metadata found in template: %w", err)
	}

	// 更新元数据以反映新的构建 ID 和版本信息
	meta = meta.SameVersionTemplate(metadata.TemplateMetadata{
		BuildID:            buildID,
		KernelVersion:      sbx.Config.FirecrackerConfig.KernelVersion,
		FirecrackerVersion: sbx.Config.FirecrackerConfig.FirecrackerVersion,
	})

	// 暂停沙箱以创建快照
	// 这会：
	// 1. 冻结 Firecracker 进程
	// 2. 创建内存快照（memfile）
	// 3. 创建根文件系统快照（rootfs）
	// 4. 创建元数据文件
	snapshot, err := sbx.Pause(ctx, meta, sandbox.SnapshotUseCasePause)
	if err != nil {
		return nil, fmt.Errorf("error snapshotting sandbox: %w", err)
	}

	// 将快照添加到本地模板缓存
	// 这允许立即从此快照恢复沙箱，无需等待远程上传
	err = s.templateCache.AddSnapshot(
		ctx,
		meta.Template.BuildID,
		snapshot.MemfileDiffHeader,
		snapshot.RootfsDiffHeader,
		snapshot.Snapfile,
		snapshot.Metafile,
		snapshot.MemfileDiff,
		snapshot.RootfsDiff,
	)
	if err != nil {
		return nil, fmt.Errorf("error adding snapshot to template cache: %w", err)
	}

	// 准备对象元数据
	// 用于在远程存储中标记此快照属于哪个团队
	objectMetadata := storage.ObjectMetadata{
		storage.ObjectMetadataTeamID: sbx.Runtime.TeamID,
	}

	// 注册上传操作
	// 只在快照添加到本地缓存后才注册，这样失败的 AddSnapshot 不会留下孤立的上传任务
	upload, err := sandbox.NewUpload(ctx, s.uploads, snapshot, s.persistence, s.config.StorageConfig.CompressConfig, s.featureFlags, storage.UseCasePause, objectMetadata)
	if err != nil {
		return nil, fmt.Errorf("register upload: %w", err)
	}

	// 报告快照添加到缓存的事件
	telemetry.ReportEvent(ctx, "added snapshot to template cache")

	// 捕获对等节点启用状态
	// 这样 Register 和 completeUpload 中的 Unregister 不会因为标志在上传中途改变而不一致
	peerEnabled := s.featureFlags.BoolFlag(ctx, featureflags.PeerToPeerChunkTransferFlag)

	// 定义完成回调函数
	// 这会在上传完成（成功或失败）时调用
	completeUpload := func(ctx context.Context, uploadErr error) {
		// 完成上传操作
		upload.Finish(ctx, uploadErr)

		if !peerEnabled {
			return
		}

		// 标记此构建已上传
		s.uploadedBuilds.Set(meta.Template.BuildID, struct{}{}, ttlcache.DefaultTTL)

		// 从对等节点注册表中注销此节点
		// 这样其他节点就不会尝试从此节点拉取快照块
		if err := s.peerRegistry.Unregister(ctx, meta.Template.BuildID); err != nil {
			logger.L().Warn(ctx, "failed to unregister peer address from routing", zap.String("build_id", meta.Template.BuildID), zap.Error(err))
		}
	}

	// 如果启用了点对点传输，注册此节点作为快照源
	if peerEnabled {
		if err := s.peerRegistry.Register(ctx, meta.Template.BuildID, redisPeerKeyTTL); err != nil {
			logger.L().Warn(ctx, "failed to register peer address for routing", zap.String("build_id", meta.Template.BuildID), zap.Error(err))
		}
	}

	return &snapshotResult{
		meta:           meta,
		upload:         upload,
		completeUpload: completeUpload,
	}, nil
}

// uploadSnapshotAsync uploads snapshot files to remote storage in the
// background and cleans up the Redis peer key once done. Used by the Pause
// handler where no prefetch data is available.
// uploadSnapshotAsync 在后台上传快照文件到远程存储
//
// 这个方法用于异步上传快照，不阻塞调用者
// 上传完成后会清理 Redis 对等节点键
//
// 使用场景：
// - Pause 操作：暂停沙箱后异步上传快照
// - Checkpoint 操作（异步模式）：恢复新沙箱后异步上传快照
//
// 设计模式：
// - 独立的超时上下文：上传有自己的 20 分钟超时，不受原始请求超时影响
// - 后台 goroutine：不阻塞调用者
// - 完成回调：确保对等节点注册表被正确清理
func (s *Server) uploadSnapshotAsync(ctx context.Context, sbx *sandbox.Sandbox, res *snapshotResult) {
	// 创建独立的上下文，有自己的超时
	// 这样即使原始请求超时，上传仍然可以继续
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), uploadTimeout)

	go func() {
		defer cancel()

		// 创建追踪 span
		ctx, span := tracer.Start(ctx, "upload snapshot")
		defer span.End()

		// 执行上传
		err := res.upload.Run(ctx)
		if err != nil {
			sbxlogger.I(sbx).Error(ctx, "error uploading snapshot files", zap.Error(err))
		} else {
			sbxlogger.I(sbx).Info(ctx, "snapshot finished uploading successfully")
		}

		// 调用完成回调
		// 这会清理对等节点注册表
		res.completeUpload(ctx, err)
	}()
}

// setupSandboxLifecycle sets up the cleanup goroutine for a sandbox.
// setupSandboxLifecycle 为沙箱设置清理 goroutine
//
// 这是一个关键的设计模式，用于管理沙箱的完整生命周期：
// 1. 等待沙箱停止（通过 sbx.Wait）
// 2. 执行清理操作（关闭文件、释放资源）
// 3. 从连接池中移除沙箱
// 4. 从沙箱映射中移除（由 Close 完成）
//
// 这个 goroutine 在后台运行，不阻塞 gRPC 响应
// 使用 context.WithoutCancel 确保即使原始请求超时，清理仍然继续
// 使用 trace.WithNewRoot() 创建独立的追踪树，不依赖于原始请求的上下文
func (s *Server) setupSandboxLifecycle(ctx context.Context, sbx *sandbox.Sandbox) {
	go func() {
		// 创建独立的追踪 span，不受原始请求超时影响
		ctx, childSpan := tracer.Start(context.WithoutCancel(ctx), "stop sandbox-lifecycle", trace.WithNewRoot())
		defer childSpan.End()

		// 等待沙箱停止
		// 这会阻塞直到 Firecracker 进程退出
		waitErr := sbx.Wait(ctx)
		if waitErr != nil {
			sbxlogger.I(sbx).Error(ctx, "failed to wait for sandbox, cleaning up", zap.Error(waitErr))
		}

		// 执行沙箱清理
		// 包括：
		// - 关闭网络命名空间
		// - 释放 IP 地址
		// - 关闭 NBD 连接
		// - 从沙箱映射中移除
		cleanupErr := sbx.Close(ctx)
		if cleanupErr != nil {
			sbxlogger.I(sbx).Error(ctx, "failed to cleanup sandbox, will remove from cache", zap.Error(cleanupErr))
		}

		// 从连接池中移除沙箱
		// 这会关闭所有到此沙箱的活跃连接
		closeErr := s.proxy.RemoveFromPool(sbx.LifecycleID)
		if closeErr != nil {
			sbxlogger.I(sbx).Warn(ctx, "errors when manually closing connections to sandbox", zap.Error(closeErr))
		}

		// 记录沙箱停止事件
		sbxlogger.E(sbx).Info(ctx, "Sandbox stopped")
	}()
}

// stopSandboxAsync stops the sandbox in a background goroutine.
// stopSandboxAsync 在后台 goroutine 中停止沙箱
//
// 这个方法用于异步停止沙箱，不阻塞调用者
// 通常在以下情况下使用：
// - Delete 操作：标记为停止后异步停止
// - Checkpoint 操作：恢复新沙箱后异步停止旧沙箱
// - Pause 操作：暂停后异步停止
//
// 使用 context.WithoutCancel 确保即使原始请求超时，停止操作仍然继续
func (s *Server) stopSandboxAsync(ctx context.Context, sbx *sandbox.Sandbox) {
	go func() {
		// 创建独立的追踪 span
		ctx, childSpan := tracer.Start(context.WithoutCancel(ctx), "stop sandbox-async", trace.WithNewRoot())
		defer childSpan.End()

		// 停止沙箱
		// 这会向 Firecracker 进程发送 SIGTERM，等待其优雅关闭
		err := sbx.Stop(ctx)
		if err != nil {
			sbxlogger.I(sbx).Error(ctx, "error stopping sandbox", zap.Error(err))
		}
	}()
}

// publishSandboxEvent publishes a sandbox event in the background.
// publishSandboxEvent 在后台发布沙箱事件
//
// 这个方法用于异步发布沙箱事件到事件总线
// 不阻塞调用者，确保 gRPC 响应快速返回
//
// 事件类型包括：
// - SandboxCreatedEvent：沙箱创建
// - SandboxResumedEvent：沙箱恢复
// - SandboxUpdatedEvent：沙箱更新
// - SandboxPausedEvent：沙箱暂停
// - SandboxCheckpointedEvent：沙箱检查点
// - SandboxKilledEvent：沙箱杀死
func (s *Server) publishSandboxEvent(ctx context.Context, sbx *sandbox.Sandbox, eventType string) {
	// 准备事件数据
	teamID, buildId, eventData := s.prepareSandboxEventData(ctx, sbx)

	// 异步发布事件
	go s.sbxEventsService.Publish(
		context.WithoutCancel(ctx),
		teamID,
		events.SandboxEvent{
			Version:   events.StructureVersionV2,
			ID:        uuid.New(),
			Type:      eventType,
			Timestamp: time.Now().UTC(),

			EventData:          eventData,
			SandboxID:          sbx.Runtime.SandboxID,
			SandboxExecutionID: sbx.Runtime.ExecutionID,
			SandboxTemplateID:  sbx.Config.BaseTemplateID,
			SandboxBuildID:     buildId,
			SandboxTeamID:      teamID,
		},
	)
}
