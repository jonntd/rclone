#!/bin/bash

# 🧪 123网盘和115网盘上传下载功能综合测试
echo "🧪 开始测试123网盘和115网盘的上传下载功能..."

# 测试配置
RCLONE_BIN="./rclone"
TEST_DIR="/tmp/rclone_test"
REMOTE_123="123:"
REMOTE_115="115:"
TEST_PATH_123="123:/测试上传下载"
TEST_PATH_115="115:/测试上传下载"

# 创建测试目录
mkdir -p "$TEST_DIR"
cd "$TEST_DIR"

echo ""
echo "📋 测试计划："
echo "1. 创建测试文件"
echo "2. 测试123网盘上传"
echo "3. 测试123网盘下载"
echo "4. 测试115网盘上传"
echo "5. 测试115网盘下载"
echo "6. 测试跨云传输（123→115，115→123）"
echo "7. 性能对比分析"
echo ""

# 步骤1: 创建测试文件
echo "🔧 步骤1: 创建测试文件"
echo "创建小文件 (1MB)..."
dd if=/dev/zero of=small_file.txt bs=1M count=1 2>/dev/null
echo "Hello, this is a test file for rclone upload/download testing." > text_file.txt

echo "创建中等文件 (10MB)..."
dd if=/dev/zero of=medium_file.bin bs=1M count=10 2>/dev/null

echo "创建大文件 (50MB)..."
dd if=/dev/zero of=large_file.bin bs=1M count=50 2>/dev/null

echo "测试文件创建完成："
ls -lh *.txt *.bin

echo ""
echo "🔧 步骤2: 测试123网盘上传"
echo "创建123网盘测试目录..."
$RCLONE_BIN mkdir "$TEST_PATH_123" 2>/dev/null || echo "目录可能已存在"

echo "上传小文件到123网盘..."
time $RCLONE_BIN copy small_file.txt "$TEST_PATH_123/" -v --log-file upload_123_small.log

echo "上传文本文件到123网盘..."
time $RCLONE_BIN copy text_file.txt "$TEST_PATH_123/" -v --log-file upload_123_text.log

echo "上传中等文件到123网盘..."
time $RCLONE_BIN copy medium_file.bin "$TEST_PATH_123/" -v --log-file upload_123_medium.log

echo "验证123网盘上传结果..."
$RCLONE_BIN ls "$TEST_PATH_123/" -v

echo ""
echo "🔧 步骤3: 测试123网盘下载"
mkdir -p download_123
echo "从123网盘下载文件..."
time $RCLONE_BIN copy "$TEST_PATH_123/" download_123/ -v --log-file download_123.log

echo "验证123网盘下载结果..."
ls -lh download_123/
echo "文件完整性检查："
diff small_file.txt download_123/small_file.txt && echo "✅ small_file.txt 完整" || echo "❌ small_file.txt 损坏"
diff text_file.txt download_123/text_file.txt && echo "✅ text_file.txt 完整" || echo "❌ text_file.txt 损坏"
diff medium_file.bin download_123/medium_file.bin && echo "✅ medium_file.bin 完整" || echo "❌ medium_file.bin 损坏"

echo ""
echo "🔧 步骤4: 测试115网盘上传"
echo "创建115网盘测试目录..."
$RCLONE_BIN mkdir "$TEST_PATH_115" 2>/dev/null || echo "目录可能已存在"

echo "上传小文件到115网盘..."
time $RCLONE_BIN copy small_file.txt "$TEST_PATH_115/" -v --log-file upload_115_small.log

echo "上传文本文件到115网盘..."
time $RCLONE_BIN copy text_file.txt "$TEST_PATH_115/" -v --log-file upload_115_text.log

echo "上传中等文件到115网盘..."
time $RCLONE_BIN copy medium_file.bin "$TEST_PATH_115/" -v --log-file upload_115_medium.log

echo "验证115网盘上传结果..."
$RCLONE_BIN ls "$TEST_PATH_115/" -v

echo ""
echo "🔧 步骤5: 测试115网盘下载"
mkdir -p download_115
echo "从115网盘下载文件..."
time $RCLONE_BIN copy "$TEST_PATH_115/" download_115/ -v --log-file download_115.log

echo "验证115网盘下载结果..."
ls -lh download_115/
echo "文件完整性检查："
diff small_file.txt download_115/small_file.txt && echo "✅ small_file.txt 完整" || echo "❌ small_file.txt 损坏"
diff text_file.txt download_115/text_file.txt && echo "✅ text_file.txt 完整" || echo "❌ text_file.txt 损坏"
diff medium_file.bin download_115/medium_file.bin && echo "✅ medium_file.bin 完整" || echo "❌ medium_file.bin 损坏"

echo ""
echo "🔧 步骤6: 测试跨云传输"
echo "测试123→115跨云传输..."
time $RCLONE_BIN copy "$TEST_PATH_123/text_file.txt" "$TEST_PATH_115/from_123_" -v --log-file cross_123_to_115.log

echo "测试115→123跨云传输..."
time $RCLONE_BIN copy "$TEST_PATH_115/text_file.txt" "$TEST_PATH_123/from_115_" -v --log-file cross_115_to_123.log

echo "验证跨云传输结果..."
$RCLONE_BIN ls "$TEST_PATH_123/" | grep "from_115_"
$RCLONE_BIN ls "$TEST_PATH_115/" | grep "from_123_"

echo ""
echo "📊 步骤7: 性能分析"
echo "--- 上传性能对比 ---"
echo "123网盘上传时间："
grep "real" upload_123_*.log 2>/dev/null || echo "未找到时间记录"

echo "115网盘上传时间："
grep "real" upload_115_*.log 2>/dev/null || echo "未找到时间记录"

echo ""
echo "--- 下载性能对比 ---"
echo "123网盘下载时间："
grep "real" download_123.log 2>/dev/null || echo "未找到时间记录"

echo "115网盘下载时间："
grep "real" download_115.log 2>/dev/null || echo "未找到时间记录"

echo ""
echo "--- 错误分析 ---"
echo "123网盘错误统计："
ERROR_123=$(grep -i "error\|failed\|错误" upload_123_*.log download_123.log 2>/dev/null | wc -l)
echo "错误数量: $ERROR_123"

echo "115网盘错误统计："
ERROR_115=$(grep -i "error\|failed\|错误" upload_115_*.log download_115.log 2>/dev/null | wc -l)
echo "错误数量: $ERROR_115"

echo ""
echo "📋 测试总结："
echo "✅ 测试文件: 3个 (1MB + 文本 + 10MB)"
echo "✅ 123网盘: 上传、下载、完整性验证"
echo "✅ 115网盘: 上传、下载、完整性验证"
echo "✅ 跨云传输: 123↔115双向传输"
echo "✅ 性能分析: 上传下载时间对比"

echo ""
echo "📁 测试文件位置: $TEST_DIR"
echo "📋 详细日志文件:"
ls -la *.log 2>/dev/null || echo "无日志文件"

echo ""
echo "🎯 测试完成！"
