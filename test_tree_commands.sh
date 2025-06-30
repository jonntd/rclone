#!/bin/bash

# 115网盘和123网盘目录树测试脚本
# 测试rclone的目录树功能

echo "=========================================="
echo "🔄 开始测试115网盘和123网盘目录树功能"
echo "=========================================="

echo ""
echo "📁 115网盘测试开始..."
echo "===================="

echo ""
echo "1️⃣ 列出115网盘根目录："
echo "命令: ./rclone lsd 116:"
./rclone lsd 116:

echo ""
echo "2️⃣ 列出115网盘test115目录文件："
echo "命令: ./rclone ls 116:test115/"
./rclone ls 116:test115/

echo ""
echo "3️⃣ 显示115网盘目录树："
echo "命令: ./rclone tree115 116:"
./rclone tree115 116:

echo ""
echo "=========================================="

echo ""
echo "📁 123网盘测试开始..."
echo "===================="

echo ""
echo "1️⃣ 列出123网盘根目录："
echo "命令: ./rclone lsd 123test:"
./rclone lsd 123test:

echo ""
echo "2️⃣ 列出123网盘test123目录文件："
echo "命令: ./rclone ls 123test:test123/"
./rclone ls 123test:test123/

echo ""
echo "3️⃣ 显示123网盘目录树："
echo "命令: ./rclone tree123 123test:"
./rclone tree123 123test:

echo ""
echo "=========================================="
echo "✅ 测试完成！"
echo "=========================================="
