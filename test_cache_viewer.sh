#!/bin/bash

# 🔍 缓存查看器功能测试脚本
echo "🧪 开始测试缓存查看器功能..."

# 测试配置
RCLONE_BIN="./rclone"
REMOTE_115="115:"
REMOTE_123="123:"

echo ""
echo "📋 缓存查看器使用方法："
echo "1. rclone backend cache-info 115: --format=tree   # 显示目录树"
echo "2. rclone backend cache-info 115: --format=stats  # 显示缓存统计"
echo "3. rclone backend cache-info 115: --format=info   # 显示缓存摘要"
echo ""

# 测试1: 115网盘缓存目录树
echo "🔧 测试1: 115网盘缓存目录树"
echo "命令: $RCLONE_BIN backend cache-info $REMOTE_115 --format=tree"
$RCLONE_BIN backend cache-info $REMOTE_115 --format=tree 2>&1 | head -20

echo ""
echo "🔧 测试2: 115网盘缓存统计"
echo "命令: $RCLONE_BIN backend cache-info $REMOTE_115 --format=stats"
$RCLONE_BIN backend cache-info $REMOTE_115 --format=stats 2>&1 | head -10

echo ""
echo "🔧 测试3: 115网盘缓存摘要"
echo "命令: $RCLONE_BIN backend cache-info $REMOTE_115 --format=info"
$RCLONE_BIN backend cache-info $REMOTE_115 --format=info 2>&1 | head -10

echo ""
echo "🔧 测试4: 检查是否支持123网盘"
echo "命令: $RCLONE_BIN backend cache-info $REMOTE_123 --format=tree"
if $RCLONE_BIN backend cache-info $REMOTE_123 --format=tree 2>/dev/null | head -5; then
    echo "✅ 123网盘也支持缓存查看器"
else
    echo "❌ 123网盘可能不支持缓存查看器或未配置"
fi

echo ""
echo "🔧 测试5: 传统缓存统计命令（已弃用）"
echo "命令: $RCLONE_BIN backend stats cache: (如果有cache remote)"
echo "注意: 这是传统的cache backend统计，与我们的缓存查看器不同"

echo ""
echo "📊 缓存查看器功能说明："
echo "✅ cache-info命令支持的格式："
echo "   - tree:  显示目录树结构（默认）"
echo "   - stats: 显示详细的缓存统计信息"
echo "   - info:  显示缓存摘要信息"
echo ""
echo "✅ 支持的云盘："
echo "   - 115网盘: 完全支持"
echo "   - 123网盘: 可能支持（需要验证）"
echo ""
echo "✅ 缓存类型："
echo "   - path_resolve: 路径解析缓存"
echo "   - dir_list:     目录列表缓存"
echo "   - metadata:     文件元数据缓存"
echo "   - file_id:      文件ID验证缓存"

echo ""
echo "📋 高级用法示例："
echo "# 查看特定格式的缓存信息"
echo "$RCLONE_BIN backend cache-info 115: --format=tree"
echo "$RCLONE_BIN backend cache-info 115: --format=stats"
echo "$RCLONE_BIN backend cache-info 115: --format=info"
echo ""
echo "# 结合其他命令使用"
echo "$RCLONE_BIN backend cache-info 115: --format=stats | jq '.'"
echo "$RCLONE_BIN backend cache-info 115: --format=tree > cache_tree.txt"

echo ""
echo "🎯 测试完成！"
