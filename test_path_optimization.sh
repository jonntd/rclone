#!/bin/bash

# 🚀 测试路径优化效果
echo "🚀 测试115网盘路径预填充优化效果..."

RCLONE_BIN="./rclone"

echo ""
echo "📊 对比测试：路径预填充前后的性能差异"
echo ""

# 清理缓存
echo "🔧 清理缓存..."
rm -rf ~/Library/Caches/rclone/115drive/path_resolve/*

echo ""
echo "🔧 测试1: 访问深层目录（触发路径预填充）"
echo "命令: $RCLONE_BIN ls \"115:/教程/翼狐  mari全能/\" > /dev/null"
time $RCLONE_BIN ls "115:/教程/翼狐  mari全能/" > /dev/null 2>&1

echo ""
echo "🔧 测试2: 访问父目录（应该利用预填充的缓存）"
echo "命令: $RCLONE_BIN ls \"115:/教程/\" > /dev/null"
time $RCLONE_BIN ls "115:/教程/" > /dev/null 2>&1

echo ""
echo "🔧 测试3: 再次访问深层目录（应该更快）"
echo "命令: $RCLONE_BIN ls \"115:/教程/翼狐  mari全能/\" > /dev/null"
time $RCLONE_BIN ls "115:/教程/翼狐  mari全能/" > /dev/null 2>&1

echo ""
echo "📊 查看缓存状态："
echo "--- 路径解析缓存 ---"
$RCLONE_BIN backend cache-info 115: -o format=info | grep -A 5 "path_resolve"

echo ""
echo "--- 缓存目录树 ---"
$RCLONE_BIN backend cache-info 115: -o format=tree | head -15

echo ""
echo "🎯 路径预填充优化测试完成！"
echo ""
echo "💡 预期效果："
echo "- 第一次访问：建立缓存和路径预填充"
echo "- 第二次访问父目录：应该利用预填充的路径缓存，更快"
echo "- 第三次访问：应该利用完整缓存，最快"
