#!/bin/bash

# Linux VPS 磁盘空间清理脚本 - 小白友好版
# 安全清理常见的垃圾文件，释放磁盘空间

echo "🧹 Linux VPS 磁盘清理工具"
echo "=========================="
echo "这个脚本会帮您安全地清理磁盘空间"
echo ""

# 检查是否为root用户
if [ "$EUID" -ne 0 ]; then
    echo "⚠️  建议以root权限运行以获得最佳清理效果"
    echo "请运行: sudo $0"
    echo "或者继续以当前用户权限运行（清理范围有限）"
    read -p "继续？(y/N): " -n 1 -r
    echo
    if [[ ! $REPLY =~ ^[Yy]$ ]]; then
        exit 0
    fi
fi

# 显示当前磁盘使用情况
echo "📊 当前磁盘使用情况："
df -h
echo ""

# 计算清理前的可用空间
BEFORE_CLEANUP=$(df / | tail -1 | awk '{print $4}')

echo "🔍 开始分析可清理的文件..."
echo ""

# 1. 清理包管理器缓存
echo "1️⃣  清理包管理器缓存"
if command -v apt-get >/dev/null 2>&1; then
    echo "  检测到 APT 包管理器"
    APT_CACHE_SIZE=$(du -sh /var/cache/apt/archives 2>/dev/null | cut -f1 || echo "0")
    echo "  APT缓存大小: $APT_CACHE_SIZE"
    
    read -p "  清理APT缓存？(y/N): " -n 1 -r
    echo
    if [[ $REPLY =~ ^[Yy]$ ]]; then
        sudo apt-get clean
        sudo apt-get autoclean
        sudo apt-get autoremove -y
        echo "  ✅ APT缓存已清理"
    fi
elif command -v yum >/dev/null 2>&1; then
    echo "  检测到 YUM 包管理器"
    read -p "  清理YUM缓存？(y/N): " -n 1 -r
    echo
    if [[ $REPLY =~ ^[Yy]$ ]]; then
        sudo yum clean all
        echo "  ✅ YUM缓存已清理"
    fi
elif command -v dnf >/dev/null 2>&1; then
    echo "  检测到 DNF 包管理器"
    read -p "  清理DNF缓存？(y/N): " -n 1 -r
    echo
    if [[ $REPLY =~ ^[Yy]$ ]]; then
        sudo dnf clean all
        echo "  ✅ DNF缓存已清理"
    fi
fi
echo ""

# 2. 清理系统日志
echo "2️⃣  清理系统日志"
if [ -d "/var/log" ]; then
    LOG_SIZE=$(du -sh /var/log 2>/dev/null | cut -f1 || echo "0")
    echo "  系统日志大小: $LOG_SIZE"
    
    # 显示最大的日志文件
    echo "  最大的日志文件："
    find /var/log -type f -name "*.log" -exec du -h {} + 2>/dev/null | sort -hr | head -5
    
    read -p "  清理旧日志文件？(y/N): " -n 1 -r
    echo
    if [[ $REPLY =~ ^[Yy]$ ]]; then
        # 清理超过7天的日志
        sudo find /var/log -type f -name "*.log" -mtime +7 -delete 2>/dev/null
        # 清理压缩的旧日志
        sudo find /var/log -type f -name "*.gz" -mtime +7 -delete 2>/dev/null
        # 清理journal日志（保留最近7天）
        if command -v journalctl >/dev/null 2>&1; then
            sudo journalctl --vacuum-time=7d
        fi
        echo "  ✅ 旧日志文件已清理"
    fi
fi
echo ""

# 3. 清理临时文件
echo "3️⃣  清理临时文件"
TEMP_DIRS=("/tmp" "/var/tmp")
for temp_dir in "${TEMP_DIRS[@]}"; do
    if [ -d "$temp_dir" ]; then
        TEMP_SIZE=$(du -sh "$temp_dir" 2>/dev/null | cut -f1 || echo "0")
        echo "  $temp_dir 大小: $TEMP_SIZE"
    fi
done

read -p "  清理临时文件？(y/N): " -n 1 -r
echo
if [[ $REPLY =~ ^[Yy]$ ]]; then
    # 清理超过3天的临时文件
    sudo find /tmp -type f -atime +3 -delete 2>/dev/null
    sudo find /var/tmp -type f -atime +3 -delete 2>/dev/null
    echo "  ✅ 临时文件已清理"
fi
echo ""

# 4. 清理用户缓存
echo "4️⃣  清理用户缓存"
if [ -d "$HOME/.cache" ]; then
    CACHE_SIZE=$(du -sh "$HOME/.cache" 2>/dev/null | cut -f1 || echo "0")
    echo "  用户缓存大小: $CACHE_SIZE"
    
    read -p "  清理用户缓存？(y/N): " -n 1 -r
    echo
    if [[ $REPLY =~ ^[Yy]$ ]]; then
        rm -rf "$HOME/.cache"/*
        echo "  ✅ 用户缓存已清理"
    fi
fi
echo ""

# 5. 查找大文件
echo "5️⃣  查找占用空间最大的文件"
echo "  正在扫描根目录下的大文件（>100MB）..."
echo "  最大的10个文件："
find / -type f -size +100M -exec du -h {} + 2>/dev/null | sort -hr | head -10

echo ""
read -p "  是否要查看更详细的目录空间占用？(y/N): " -n 1 -r
echo
if [[ $REPLY =~ ^[Yy]$ ]]; then
    echo "  各目录空间占用排行："
    du -h --max-depth=1 / 2>/dev/null | sort -hr | head -10
fi
echo ""

# 6. Docker清理（如果安装了Docker）
if command -v docker >/dev/null 2>&1; then
    echo "6️⃣  Docker清理"
    DOCKER_SIZE=$(docker system df 2>/dev/null | tail -n +2 | awk '{sum += $3} END {print sum "MB"}' || echo "0MB")
    echo "  Docker占用空间: $DOCKER_SIZE"
    
    read -p "  清理Docker未使用的资源？(y/N): " -n 1 -r
    echo
    if [[ $REPLY =~ ^[Yy]$ ]]; then
        docker system prune -f
        echo "  ✅ Docker资源已清理"
    fi
    echo ""
fi

# 7. 清理旧内核（Ubuntu/Debian）
if command -v apt-get >/dev/null 2>&1; then
    echo "7️⃣  清理旧内核"
    OLD_KERNELS=$(dpkg -l | grep -E "linux-image-[0-9]" | grep -v $(uname -r) | wc -l)
    if [ "$OLD_KERNELS" -gt 0 ]; then
        echo "  发现 $OLD_KERNELS 个旧内核"
        read -p "  清理旧内核？(y/N): " -n 1 -r
        echo
        if [[ $REPLY =~ ^[Yy]$ ]]; then
            sudo apt-get autoremove --purge -y
            echo "  ✅ 旧内核已清理"
        fi
    else
        echo "  ✅ 没有发现旧内核"
    fi
    echo ""
fi

# 显示清理结果
echo "🎉 清理完成！"
echo ""
echo "📊 清理后磁盘使用情况："
df -h
echo ""

# 计算释放的空间
AFTER_CLEANUP=$(df / | tail -1 | awk '{print $4}')
FREED_SPACE=$((AFTER_CLEANUP - BEFORE_CLEANUP))
if [ $FREED_SPACE -gt 0 ]; then
    echo "✅ 成功释放了约 $(echo $FREED_SPACE | awk '{print $1/1024/1024 " GB"}') 空间"
else
    echo "ℹ️  清理完成，空间释放量较小"
fi

echo ""
echo "💡 额外建议："
echo "  1. 定期运行此脚本保持系统清洁"
echo "  2. 考虑设置日志轮转以防止日志文件过大"
echo "  3. 监控大文件的增长，及时清理不需要的文件"
echo "  4. 如果空间仍然不足，考虑升级VPS或添加存储"
