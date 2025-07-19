#!/bin/bash

# 🔍 缓存验证脚本
echo "🧪 验证115网盘缓存是否生效..."

# 测试配置
TEST_DIR="115:/教程/翼狐  mari全能/"
RCLONE_BIN="./rclone"

echo ""
echo "📋 验证方法说明："
echo "1. 第一次调用 - 建立缓存"
echo "2. 第二次调用 - 验证缓存命中"
echo "3. 查看缓存统计信息"
echo ""

# 清理日志
rm -f verify_*.log

echo "🔧 步骤1: 第一次调用（建立缓存）"
echo "时间: $(date)"
time $RCLONE_BIN ls "$TEST_DIR" -v --log-file verify_1.log > /dev/null 2>&1
echo "完成时间: $(date)"

echo ""
echo "🔧 步骤2: 第二次调用（验证缓存）"
echo "时间: $(date)"
time $RCLONE_BIN ls "$TEST_DIR" -v --log-file verify_2.log > /dev/null 2>&1
echo "完成时间: $(date)"

echo ""
echo "📊 缓存验证结果："

# 分析API调用次数
API_CALLS_1=$(grep -c "CallOpenAPI成功" verify_1.log || echo "0")
API_CALLS_2=$(grep -c "CallOpenAPI成功" verify_2.log || echo "0")

echo "第一次调用API次数: $API_CALLS_1"
echo "第二次调用API次数: $API_CALLS_2"

# 分析缓存命中
CACHE_HIT=$(grep -c "缓存命中\|List缓存命中" verify_2.log || echo "0")
CACHE_MISS=$(grep -c "缓存未命中" verify_2.log || echo "0")

echo "第二次调用缓存命中: $CACHE_HIT 次"
echo "第二次调用缓存未命中: $CACHE_MISS 次"

# 判断缓存效果
if [ "$API_CALLS_2" -lt "$API_CALLS_1" ] && [ "$CACHE_HIT" -gt "0" ]; then
    REDUCTION=$((API_CALLS_1 - API_CALLS_2))
    echo "✅ 缓存生效！API调用减少了 $REDUCTION 次"
else
    echo "❌ 缓存可能未生效"
fi

echo ""
echo "🔍 步骤3: 查看缓存统计"
echo "--- 缓存目录树 ---"
$RCLONE_BIN backend cache-info 115: -o format=tree | head -10

echo ""
echo "--- 缓存统计信息 ---"
$RCLONE_BIN backend cache-info 115: -o format=stats | head -10

echo ""
echo "--- 缓存摘要 ---"
$RCLONE_BIN backend cache-info 115: -o format=info

echo ""
echo "📋 详细验证方法："
echo "1. 查看日志文件: verify_1.log 和 verify_2.log"
echo "2. 对比API调用次数和响应时间"
echo "3. 检查缓存命中日志"
echo "4. 使用 cache-info 命令查看缓存状态"
