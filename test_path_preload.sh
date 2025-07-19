#!/bin/bash

# 🚀 测试路径预填充功能
echo "🚀 测试115网盘路径预填充功能..."

# 测试配置
TEST_DIR="115:/教程/翼狐  mari全能/"
RCLONE_BIN="./rclone"

echo ""
echo "📋 测试计划："
echo "1. 清理现有缓存"
echo "2. 访问深层目录，触发路径预填充"
echo "3. 验证路径缓存是否被预填充"
echo "4. 测试父目录访问是否更快"
echo ""

# 清理缓存
echo "🔧 步骤1: 清理现有缓存"
rm -rf ~/Library/Caches/rclone/115drive/path_resolve/*
echo "缓存已清理"

echo ""
echo "🔧 步骤2: 访问深层目录（应该触发路径预填充）"
echo "命令: $RCLONE_BIN ls \"$TEST_DIR\" -vv"
$RCLONE_BIN ls "$TEST_DIR" -vv --log-file test_path_preload.log 2>&1 | head -15

echo ""
echo "🔧 步骤3: 分析路径预填充效果"

# 检查日志中的路径预填充信息
echo "--- 路径预填充日志 ---"
grep -E "🎯.*路径层次信息|🎯.*预填充路径缓存" test_path_preload.log || echo "未找到路径预填充日志"

echo ""
echo "--- 缓存保存日志 ---"
grep -E "已保存路径类型到缓存" test_path_preload.log | head -5 || echo "未找到路径缓存保存日志"

echo ""
echo "🔧 步骤4: 测试父目录访问（应该从缓存读取）"
echo "访问父目录: 115:/教程/"
time $RCLONE_BIN ls "115:/教程/" -v --log-file test_parent_access.log > /dev/null 2>&1

echo ""
echo "--- 父目录访问缓存命中 ---"
grep -E "🎯.*缓存命中|从缓存获取" test_parent_access.log || echo "未找到缓存命中日志"

echo ""
echo "🔧 步骤5: 查看缓存统计"
echo "--- 路径解析缓存统计 ---"
$RCLONE_BIN backend cache-info 115: -o format=info | grep -A 10 "path_resolve" || echo "无法获取缓存统计"

echo ""
echo "📊 测试结果分析："

# 统计API调用次数
API_CALLS=$(grep -c "CallOpenAPI成功" test_path_preload.log || echo "0")
PATH_PRELOAD=$(grep -c "🎯.*预填充路径缓存" test_path_preload.log || echo "0")
CACHE_SAVES=$(grep -c "已保存路径类型到缓存" test_path_preload.log || echo "0")

echo "API调用次数: $API_CALLS"
echo "路径预填充次数: $PATH_PRELOAD"
echo "路径缓存保存次数: $CACHE_SAVES"

if [ "$PATH_PRELOAD" -gt "0" ]; then
    echo "✅ 路径预填充功能正常工作！"
else
    echo "❌ 路径预填充功能可能未生效"
fi

echo ""
echo "📋 详细日志文件："
echo "主要测试日志: test_path_preload.log"
echo "父目录访问日志: test_parent_access.log"

echo ""
echo "🎯 测试完成！"
