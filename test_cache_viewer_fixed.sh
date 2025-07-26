#!/bin/bash

# 🔍 缓存查看器功能测试脚本（修正版）
echo "🧪 开始测试缓存查看器功能..."

# 测试配置
RCLONE_BIN="./rclone"
REMOTE_115="115:"

echo ""
echo "📋 正确的缓存查看器使用方法："
echo "1. rclone backend cache-info 115: -o format=tree   # 显示目录树"
echo "2. rclone backend cache-info 115: -o format=stats  # 显示缓存统计"
echo "3. rclone backend cache-info 115: -o format=info   # 显示缓存摘要"
echo "4. rclone backend cache-info 115:                  # 默认显示目录树"
echo ""

# 测试1: 115网盘缓存目录树（默认）
echo "🔧 测试1: 115网盘缓存目录树（默认）"
echo "命令: $RCLONE_BIN backend cache-info $REMOTE_115"
$RCLONE_BIN backend cache-info $REMOTE_115 2>&1 | head -20

echo ""
echo "🔧 测试2: 115网盘缓存目录树（明确指定）"
echo "命令: $RCLONE_BIN backend cache-info $REMOTE_115 -o format=tree"
$RCLONE_BIN backend cache-info $REMOTE_115 -o format=tree 2>&1 | head -20

echo ""
echo "🔧 测试3: 115网盘缓存统计"
echo "命令: $RCLONE_BIN backend cache-info $REMOTE_115 -o format=stats"
$RCLONE_BIN backend cache-info $REMOTE_115 -o format=stats 2>&1 | head -15

echo ""
echo "🔧 测试4: 115网盘缓存摘要"
echo "命令: $RCLONE_BIN backend cache-info $REMOTE_115 -o format=info"
$RCLONE_BIN backend cache-info $REMOTE_115 -o format=info 2>&1 | head -15

echo ""
echo "🔧 测试5: JSON格式输出"
echo "命令: $RCLONE_BIN backend cache-info $REMOTE_115 -o format=info --json"
$RCLONE_BIN backend cache-info $REMOTE_115 -o format=info --json 2>&1 | head -10

echo ""
echo "📊 缓存查看器功能验证完成！"
echo ""
echo "✅ 正确的使用语法："
echo "   rclone backend cache-info 115: -o format=tree"
echo "   rclone backend cache-info 115: -o format=stats"
echo "   rclone backend cache-info 115: -o format=info"
echo ""
echo "✅ 可选参数："
echo "   --json    : 以JSON格式输出"
echo "   -v        : 详细输出"
echo "   --dry-run : 试运行模式"
