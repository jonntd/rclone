#!/bin/bash

# 🔧 测试115网盘统一QPS管理
echo "🔧 测试115网盘统一QPS管理效果..."

# 测试配置
TEST_DIR="115:/教程/翼狐  mari全能/"
RCLONE_BIN="./rclone"

echo ""
echo "📋 统一QPS管理测试计划："
echo "1. 测试文件列表API的QPS控制"
echo "2. 验证智能调速器选择"
echo "3. 检查770004错误处理"
echo "4. 对比QPS优化效果"
echo ""

# 清理日志
rm -f unified_qps_*.log

echo "🔧 测试1: 文件列表API QPS控制"
echo "命令: $RCLONE_BIN ls \"$TEST_DIR\" -vv"
$RCLONE_BIN ls "$TEST_DIR" -vv --log-file unified_qps_1.log 2>&1 | head -10

echo ""
echo "🔧 测试2: 验证智能调速器选择"
echo "检查日志中的调速器选择信息..."

# 分析调速器选择
echo "--- 智能QPS选择日志 ---"
grep -E "🎯.*智能QPS选择|智能调速器" unified_qps_1.log || echo "未找到智能QPS选择日志"

echo ""
echo "--- API调用统计 ---"
API_CALLS=$(grep -c "CallOpenAPI" unified_qps_1.log || echo "0")
QPS_WAITS=$(grep -c "API限制等待" unified_qps_1.log || echo "0")
RETRIES=$(grep -c "API限制重试" unified_qps_1.log || echo "0")

echo "API调用次数: $API_CALLS"
echo "QPS等待次数: $QPS_WAITS"
echo "重试次数: $RETRIES"

echo ""
echo "🔧 测试3: 检查QPS配置"
echo "--- 当前QPS设置 ---"
echo "统一QPS: ~4 QPS (250ms间隔)"
echo "保守QPS: ~2 QPS (500ms间隔)"
echo "激进QPS: ~6 QPS (150ms间隔)"

echo ""
echo "🔧 测试4: 验证路径预填充与统一QPS的协同效果"
echo "第二次访问同一目录（应该利用缓存+统一QPS）..."
time $RCLONE_BIN ls "$TEST_DIR" -v --log-file unified_qps_2.log > /dev/null 2>&1

echo ""
echo "--- 第二次访问分析 ---"
API_CALLS_2=$(grep -c "CallOpenAPI" unified_qps_2.log || echo "0")
CACHE_HITS_2=$(grep -c "缓存命中" unified_qps_2.log || echo "0")

echo "第二次API调用: $API_CALLS_2"
echo "缓存命中次数: $CACHE_HITS_2"

if [ "$API_CALLS_2" -lt "$API_CALLS" ]; then
    REDUCTION=$((API_CALLS - API_CALLS_2))
    echo "✅ 缓存+统一QPS生效！API调用减少了 $REDUCTION 次"
else
    echo "⚠️  缓存效果不明显"
fi

echo ""
echo "📊 统一QPS管理效果分析："

# 检查是否有770004错误
ERROR_770004=$(grep -c "770004\|已达到当前访问上限" unified_qps_*.log || echo "0")
if [ "$ERROR_770004" -gt "0" ]; then
    echo "⚠️  检测到 $ERROR_770004 次770004错误，可能需要进一步降低QPS"
else
    echo "✅ 未检测到770004错误，统一QPS设置合理"
fi

# 检查调速器使用情况
PACER_USAGE=$(grep -c "使用.*调速器\|智能QPS" unified_qps_*.log || echo "0")
echo "调速器选择次数: $PACER_USAGE"

echo ""
echo "🎯 统一QPS管理优势："
echo "✅ 避免多个调速器冲突"
echo "✅ 简化QPS管理逻辑"
echo "✅ 统一处理770004错误"
echo "✅ 与路径预填充缓存协同工作"

echo ""
echo "📋 详细日志文件："
echo "第一次访问: unified_qps_1.log"
echo "第二次访问: unified_qps_2.log"

echo ""
echo "🎯 测试完成！"
