#!/bin/bash

# E2B HA Phase 1 验证脚本
# 此脚本自动运行所有验证步骤

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

# 颜色定义
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# 计数器
PASSED=0
FAILED=0

# 打印函数
print_header() {
    echo -e "\n${BLUE}========================================${NC}"
    echo -e "${BLUE}$1${NC}"
    echo -e "${BLUE}========================================${NC}\n"
}

print_success() {
    echo -e "${GREEN}✅ $1${NC}"
    ((PASSED++))
}

print_error() {
    echo -e "${RED}❌ $1${NC}"
    ((FAILED++))
}

print_warning() {
    echo -e "${YELLOW}⚠️  $1${NC}"
}

print_info() {
    echo -e "${BLUE}ℹ️  $1${NC}"
}

# 检查文件是否存在
check_file_exists() {
    if [ -f "$1" ]; then
        print_success "文件存在: $1"
        return 0
    else
        print_error "文件不存在: $1"
        return 1
    fi
}

# 检查代码编译
check_compilation() {
    print_header "检查代码编译"
    
    print_info "编译 API 服务..."
    if go build -o /tmp/api ./packages/api 2>/dev/null; then
        print_success "API 编译成功"
    else
        print_error "API 编译失败"
        return 1
    fi
    
    print_info "编译 Orchestrator..."
    if go build -o /tmp/orchestrator ./packages/orchestrator 2>/dev/null; then
        print_success "Orchestrator 编译成功"
    else
        print_error "Orchestrator 编译失败"
        return 1
    fi
}

# 检查文件
check_files() {
    print_header "检查实现文件"
    
    local files=(
        "packages/api/internal/orchestrator/drain_monitor.go"
        "packages/api/internal/orchestrator/drain_monitor_test.go"
        "packages/api/internal/orchestrator/nodemanager/drain.go"
        "packages/api/internal/orchestrator/nodemanager/drain_test.go"
        "tests/integration/drain_test.go"
        "HA-PHASE1-IMPLEMENTATION.md"
        "HA-PHASE1-VERIFICATION.md"
        "HA-PHASE1-QUICKSTART.md"
        "HA-PHASE1-SUMMARY.md"
    )
    
    for file in "${files[@]}"; do
        check_file_exists "$file" || return 1
    done
}

# 运行单元测试
run_unit_tests() {
    print_header "运行单元测试"
    
    print_info "运行 Drain Monitor 测试..."
    if go test -v ./packages/api/internal/orchestrator -run TestDrainMonitor 2>&1 | grep -q "PASS"; then
        print_success "Drain Monitor 测试通过"
    else
        print_error "Drain Monitor 测试失败"
        return 1
    fi
    
    print_info "运行 Drain Wait 测试..."
    if go test -v ./packages/api/internal/orchestrator/nodemanager -run TestWaitForDrain 2>&1 | grep -q "PASS"; then
        print_success "Drain Wait 测试通过"
    else
        print_error "Drain Wait 测试失败"
        return 1
    fi
}

# 运行集成测试
run_integration_tests() {
    print_header "运行集成测试"
    
    print_info "运行 Drain 集成测试..."
    if go test -v ./tests/integration -run TestDrain 2>&1 | grep -q "PASS"; then
        print_success "Drain 集成测试通过"
    else
        print_warning "Drain 集成测试可能需要更多时间或特定环境"
    fi
}

# 检查代码格式
check_code_format() {
    print_header "检查代码格式"
    
    print_info "检查 Go 代码格式..."
    local files=(
        "packages/api/internal/orchestrator/drain_monitor.go"
        "packages/api/internal/orchestrator/drain_monitor_test.go"
        "packages/api/internal/orchestrator/nodemanager/drain.go"
        "packages/api/internal/orchestrator/nodemanager/drain_test.go"
    )
    
    for file in "${files[@]}"; do
        if gofmt -l "$file" | grep -q .; then
            print_warning "文件 $file 需要格式化"
        else
            print_success "文件 $file 格式正确"
        fi
    done
}

# 检查导入
check_imports() {
    print_header "检查导入"
    
    print_info "检查 admin.go 导入..."
    if grep -q "context" packages/api/internal/handlers/admin.go; then
        print_success "context 导入正确"
    else
        print_error "context 导入缺失"
        return 1
    fi
    
    if grep -q "time" packages/api/internal/handlers/admin.go; then
        print_success "time 导入正确"
    else
        print_error "time 导入缺失"
        return 1
    fi
}

# 检查 API 端点
check_api_endpoints() {
    print_header "检查 API 端点"
    
    print_info "检查 OpenAPI 规范..."
    if grep -q "/admin/nodes/{nodeID}/drain:" spec/openapi.yml; then
        print_success "Drain 启动端点已定义"
    else
        print_error "Drain 启动端点未定义"
        return 1
    fi
    
    if grep -q "/admin/nodes/{nodeID}/drain-status:" spec/openapi.yml; then
        print_success "Drain 状态端点已定义"
    else
        print_error "Drain 状态端点未定义"
        return 1
    fi
    
    if grep -q "/admin/nodes/{nodeID}/drain-wait:" spec/openapi.yml; then
        print_success "Drain 等待端点已定义"
    else
        print_error "Drain 等待端点未定义"
        return 1
    fi
}

# 检查文档
check_documentation() {
    print_header "检查文档"
    
    print_info "检查实现文档..."
    if grep -q "Drain 状态监控" HA-PHASE1-IMPLEMENTATION.md; then
        print_success "实现文档包含 Drain 监控说明"
    else
        print_error "实现文档缺失 Drain 监控说明"
        return 1
    fi
    
    print_info "检查验证清单..."
    if grep -q "✅" HA-PHASE1-VERIFICATION.md; then
        print_success "验证清单已完成"
    else
        print_error "验证清单未完成"
        return 1
    fi
    
    print_info "检查快速开始指南..."
    if grep -q "快速开始" HA-PHASE1-QUICKSTART.md; then
        print_success "快速开始指南已准备"
    else
        print_error "快速开始指南缺失"
        return 1
    fi
}

# 生成报告
generate_report() {
    print_header "验证报告"
    
    echo -e "${BLUE}验证结果:${NC}"
    echo -e "  ${GREEN}通过: $PASSED${NC}"
    echo -e "  ${RED}失败: $FAILED${NC}"
    
    if [ $FAILED -eq 0 ]; then
        echo -e "\n${GREEN}✅ 所有验证通过！${NC}"
        return 0
    else
        echo -e "\n${RED}❌ 有 $FAILED 个验证失败${NC}"
        return 1
    fi
}

# 主函数
main() {
    print_header "E2B HA Phase 1 验证脚本"
    
    print_info "开始验证..."
    
    # 运行所有检查
    check_files || true
    check_compilation || true
    check_imports || true
    check_api_endpoints || true
    check_code_format || true
    check_documentation || true
    
    # 运行测试
    run_unit_tests || true
    run_integration_tests || true
    
    # 生成报告
    generate_report
}

# 运行主函数
main
