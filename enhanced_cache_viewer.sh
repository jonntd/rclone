#!/bin/bash

# 🔍 增强版缓存查看器 - 显示完整路径层次
echo "🔍 增强版缓存查看器 - 显示完整路径层次"

RCLONE_BIN="./rclone"
CACHE_DIR="$HOME/Library/Caches/rclone/115drive"

echo ""
echo "📊 115网盘缓存层次结构："
echo ""

# 分析path_resolve缓存（路径映射）
echo "🗂️  路径解析缓存 (path_resolve):"
if [ -d "$CACHE_DIR/path_resolve" ]; then
    PATH_COUNT=$(find "$CACHE_DIR/path_resolve" -name "*.vlog" | wc -l)
    echo "   └── 缓存条目数: $PATH_COUNT"
    
    # 尝试显示一些路径信息
    echo "   └── 示例路径:"
    find "$CACHE_DIR/path_resolve" -name "*.vlog" | head -3 | while read file; do
        filename=$(basename "$file" .vlog)
        echo "       ├── $filename"
    done
else
    echo "   └── 无缓存数据"
fi

echo ""

# 分析dir_list缓存（目录列表）
echo "📁 目录列表缓存 (dir_list):"
if [ -d "$CACHE_DIR/dir_list" ]; then
    DIR_COUNT=$(find "$CACHE_DIR/dir_list" -name "*.vlog" | wc -l)
    echo "   └── 缓存目录数: $DIR_COUNT"
    
    echo "   └── 缓存的目录ID:"
    find "$CACHE_DIR/dir_list" -name "*.vlog" | head -5 | while read file; do
        filename=$(basename "$file" .vlog)
        # 提取目录ID（去掉dirlist_前缀和_后缀）
        dir_id=$(echo "$filename" | sed 's/^dirlist_//' | sed 's/_$//')
        if [ "$dir_id" = "0" ]; then
            echo "       ├── 根目录 (ID: 0)"
        else
            echo "       ├── 目录ID: $dir_id"
        fi
    done
else
    echo "   └── 无缓存数据"
fi

echo ""

# 使用rclone命令获取更详细的信息
echo "🔍 使用rclone获取详细缓存信息:"
echo ""

echo "--- 缓存统计摘要 ---"
$RCLONE_BIN backend cache-info 115: -o format=info 2>/dev/null | head -15

echo ""
echo "--- 当前缓存的目录树 ---"
$RCLONE_BIN backend cache-info 115: -o format=tree 2>/dev/null

echo ""
echo "📋 缓存工作原理说明："
echo ""
echo "🔧 115网盘缓存采用分层设计："
echo "   ├── path_resolve: 路径 → 目录ID 的映射"
echo "   ├── dir_list:     目录ID → 文件列表 的映射"
echo "   ├── metadata:     文件ID → 元数据 的映射"
echo "   └── file_id:      文件验证缓存"
echo ""
echo "🎯 为什么不显示完整路径？"
echo "   ├── 缓存按目录ID分层存储，不是按路径存储"
echo "   ├── 只有访问过的目录才会被缓存"
echo "   ├── 缓存查看器显示的是当前缓存的内容"
echo "   └── 要看到更多层次，需要访问更多目录"
echo ""
echo "💡 如何查看特定路径的缓存："
echo "   1. 访问目录: ./rclone ls \"115:/路径/\" > /dev/null"
echo "   2. 查看缓存: ./rclone backend cache-info 115: -o format=tree"
echo "   3. 验证效果: 再次访问同一路径应该更快"

echo ""
echo "🎉 缓存查看完成！"
