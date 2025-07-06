#!/bin/bash

# 快速Linux磁盘清理脚本 - 一键清理
echo "🚀 快速磁盘清理工具"
echo "==================="

# 显示清理前状态
echo "📊 清理前磁盘状态："
df -h /
echo ""

echo "🧹 开始自动清理..."

# 1. 清理包管理器缓存
echo "1. 清理包缓存..."
if command -v apt-get >/dev/null 2>&1; then
    sudo apt-get clean >/dev/null 2>&1
    sudo apt-get autoclean >/dev/null 2>&1
    sudo apt-get autoremove -y >/dev/null 2>&1
    echo "   ✅ APT缓存已清理"
elif command -v yum >/dev/null 2>&1; then
    sudo yum clean all >/dev/null 2>&1
    echo "   ✅ YUM缓存已清理"
elif command -v dnf >/dev/null 2>&1; then
    sudo dnf clean all >/dev/null 2>&1
    echo "   ✅ DNF缓存已清理"
fi

# 2. 清理系统日志
echo "2. 清理系统日志..."
sudo find /var/log -type f -name "*.log" -mtime +7 -delete 2>/dev/null
sudo find /var/log -type f -name "*.gz" -mtime +7 -delete 2>/dev/null
if command -v journalctl >/dev/null 2>&1; then
    sudo journalctl --vacuum-time=7d >/dev/null 2>&1
fi
echo "   ✅ 旧日志已清理"

# 3. 清理临时文件
echo "3. 清理临时文件..."
sudo find /tmp -type f -atime +3 -delete 2>/dev/null
sudo find /var/tmp -type f -atime +3 -delete 2>/dev/null
echo "   ✅ 临时文件已清理"

# 4. 清理用户缓存
echo "4. 清理用户缓存..."
rm -rf ~/.cache/* 2>/dev/null
echo "   ✅ 用户缓存已清理"

# 5. Docker清理（如果有）
if command -v docker >/dev/null 2>&1; then
    echo "5. 清理Docker资源..."
    docker system prune -f >/dev/null 2>&1
    echo "   ✅ Docker资源已清理"
fi

# 6. 清理缩略图缓存
echo "6. 清理缩略图缓存..."
rm -rf ~/.thumbnails/* 2>/dev/null
rm -rf ~/.cache/thumbnails/* 2>/dev/null
echo "   ✅ 缩略图缓存已清理"

echo ""
echo "🎉 清理完成！"
echo ""
echo "📊 清理后磁盘状态："
df -h /
echo ""

# 显示最大的文件
echo "💡 当前占用空间最大的目录："
du -h --max-depth=1 / 2>/dev/null | sort -hr | head -5
echo ""
echo "如需更详细的清理，请运行: ./linux_disk_cleanup.sh"
