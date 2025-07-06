#!/bin/bash

# Linux rclone临时文件清理脚本
echo "🧹 清理rclone临时文件"
echo "===================="

# 检查是否为root用户
if [ "$EUID" -ne 0 ]; then
    echo "⚠️  需要root权限来删除这些文件"
    echo "请运行: sudo $0"
    exit 1
fi

# 显示当前磁盘使用情况
echo "📊 清理前磁盘使用情况："
df -h /tmp
echo ""

# 检查是否有rclone进程正在运行
echo "🔍 检查正在运行的rclone进程..."
RUNNING_PROCESSES=$(ps aux | grep rclone | grep -v grep | grep -v cleanup)
if [ -n "$RUNNING_PROCESSES" ]; then
    echo "⚠️  发现正在运行的rclone进程："
    echo "$RUNNING_PROCESSES"
    echo ""
    read -p "是否要停止所有rclone进程？(y/N): " -n 1 -r
    echo
    if [[ $REPLY =~ ^[Yy]$ ]]; then
        echo "🛑 停止rclone进程..."
        pkill -f rclone
        sleep 3
        echo "✅ rclone进程已停止"
    else
        echo "⚠️  警告：在rclone运行时删除临时文件可能导致传输失败"
        read -p "是否继续清理？(y/N): " -n 1 -r
        echo
        if [[ ! $REPLY =~ ^[Yy]$ ]]; then
            echo "❌ 取消清理操作"
            exit 0
        fi
    fi
fi

echo ""
echo "🔍 搜索rclone相关临时文件..."

# 搜索rclone临时文件
TEMP_FILES=(
    "/tmp/cross_cloud_optimized_*.tmp"
    "/tmp/cross_cloud_simple_*.tmp"
    "/tmp/rclone-*"
    "/tmp/*123pan*"
    "/tmp/v2upload_*.tmp"
    "/tmp/concurrent_download_*.tmp"
)

FOUND_FILES=()
TOTAL_SIZE=0

for pattern in "${TEMP_FILES[@]}"; do
    for file in $pattern; do
        if [ -f "$file" ]; then
            size=$(stat -c%s "$file" 2>/dev/null || echo 0)
            FOUND_FILES+=("$file")
            TOTAL_SIZE=$((TOTAL_SIZE + size))
            echo "  📄 $(basename "$file") ($(numfmt --to=iec $size))"
        fi
    done
done

# 搜索rclone进度目录
PROGRESS_DIRS=(
    "/tmp/rclone-123pan-progress"
    "/tmp/rclone-*-progress"
)

for pattern in "${PROGRESS_DIRS[@]}"; do
    for dir in $pattern; do
        if [ -d "$dir" ]; then
            dir_size=$(du -sb "$dir" 2>/dev/null | cut -f1 || echo 0)
            FOUND_FILES+=("$dir")
            TOTAL_SIZE=$((TOTAL_SIZE + dir_size))
            echo "  📂 $(basename "$dir")/ ($(numfmt --to=iec $dir_size))"
        fi
    done
done

echo ""
echo "📊 统计结果："
echo "  找到项目数: ${#FOUND_FILES[@]}"
echo "  总大小: $(numfmt --to=iec $TOTAL_SIZE)"

if [ ${#FOUND_FILES[@]} -eq 0 ]; then
    echo "✅ 没有找到rclone临时文件！"
    exit 0
fi

echo ""
echo "⚠️  这些文件将被删除，释放 $(numfmt --to=iec $TOTAL_SIZE) 空间"
echo "包括您提到的4个7.8GB文件"
read -p "确认删除这些文件？(y/N): " -n 1 -r
echo

if [[ $REPLY =~ ^[Yy]$ ]]; then
    echo "🗑️  开始清理..."
    
    DELETED_COUNT=0
    DELETED_SIZE=0
    
    for item in "${FOUND_FILES[@]}"; do
        if [ -f "$item" ]; then
            size=$(stat -c%s "$item" 2>/dev/null || echo 0)
            echo "  🗑️  删除文件: $(basename "$item") ($(numfmt --to=iec $size))"
            if rm "$item" 2>/dev/null; then
                echo "     ✅ 删除成功"
                DELETED_COUNT=$((DELETED_COUNT + 1))
                DELETED_SIZE=$((DELETED_SIZE + size))
            else
                echo "     ❌ 删除失败"
            fi
        elif [ -d "$item" ]; then
            dir_size=$(du -sb "$item" 2>/dev/null | cut -f1 || echo 0)
            echo "  🗑️  删除目录: $(basename "$item")/ ($(numfmt --to=iec $dir_size))"
            if rm -rf "$item" 2>/dev/null; then
                echo "     ✅ 删除成功"
                DELETED_COUNT=$((DELETED_COUNT + 1))
                DELETED_SIZE=$((DELETED_SIZE + dir_size))
            else
                echo "     ❌ 删除失败"
            fi
        fi
    done
    
    echo ""
    echo "🎉 清理完成！"
    echo "  删除项目数: $DELETED_COUNT"
    echo "  释放空间: $(numfmt --to=iec $DELETED_SIZE)"
    
    echo ""
    echo "📊 清理后磁盘使用情况："
    df -h /tmp
    
else
    echo "❌ 取消清理操作"
fi

echo ""
echo "💡 提示："
echo "  - 这些临时文件是rclone跨云传输时产生的"
echo "  - 正常情况下程序退出时会自动清理"
echo "  - 如果程序异常退出，需要手动清理"
echo "  - 建议定期检查/tmp目录避免空间不足"
