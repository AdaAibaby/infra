#!/bin/bash

echo "====================================="
echo " E2B Admin Token 自动获取 + 测试脚本"
echo "====================================="
echo ""

# 1. 寻找环境变量文件（自动找 .env / .env.local / docker-compose.yml）
echo "[1/4] 正在查找 E2B 环境配置..."
ENV_FILES=($(find ~ /opt /e2b /infra -name ".env*" -o -name "*.env" 2>/dev/null | grep -v cache | head -5))

if [ ${#ENV_FILES[@]} -eq 0 ]; then
    echo "❌ 未找到环境文件，请进入 e2b 目录再运行"
    exit 1
fi

echo "✅ 找到配置文件: ${ENV_FILES[0]}"
echo ""

# 2. 提取 ADMIN_TOKEN
echo "[2/4] 提取 Admin Token..."
ADMIN_TOKEN=$(grep -i "ADMIN_TOKEN\|ADMIN_JWT" ${ENV_FILES[0]} | head -1 | cut -d '=' -f2 | xargs)
API_KEY=$(grep -i "E2B_API_KEY" ${ENV_FILES[0]} | head -1 | cut -d '=' -f2 | xargs)

if [ -n "$ADMIN_TOKEN" ]; then
    TOKEN=$ADMIN_TOKEN
    echo "✅ 提取到 E2B_ADMIN_TOKEN"
elif [ -n "$API_KEY" ]; then
    TOKEN=$API_KEY
    echo "⚠️  使用 E2B_API_KEY 作为管理员令牌"
else
    echo "❌ 无法获取 token，请手动检查 .env 文件"
    exit 1
fi

echo ""
echo "========= 你的 ADMIN TOKEN ========="
echo "$TOKEN"
echo "===================================="
echo ""

# 3. 获取当前运行的 node 列表
echo "[3/4] 正在获取 Orchestrator 节点列表..."
NODES=$(curl -s -H "Authorization: Bearer $TOKEN" http://localhost:3000/admin/nodes 2>/dev/null)
if echo "$NODES" | grep -q "node-"; then
    echo "✅ 获取节点成功"
    echo "当前节点："
    echo "$NODES" | jq -r '.nodes[]?.id' 2>/dev/null || echo "$NODES"
else
    echo "⚠️  无法获取节点列表（可能API端口不是3000）"
fi
echo ""

# 4. 自动生成测试命令
echo "[4/4] 生成可直接使用的 drain 命令"
echo ""
echo "===================================="
echo "🔥 直接复制运行这个命令即可 drain 节点"
echo "===================================="
echo "curl -X POST \\"
echo "  -H \"Authorization: Bearer $TOKEN\" \\"
echo "  http://localhost:3000/admin/nodes/【你的节点ID】/drain"
echo ""
echo "===================================="
echo "✅ 脚本执行完成！"
