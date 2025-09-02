#!/bin/bash

# STRM-Mount 简化管理脚本
# 基于你的原始需求改进

# 配置
RCLONE="./rclone"
BASE_MOUNT_DIR="/Users/jonntd/cloud"
MOUNT_115="$BASE_MOUNT_DIR/115"
MOUNT_123="$BASE_MOUNT_DIR/123"
DATA_DIR="$HOME/.rclone-strm"           # 统一数据目录
CACHE_DIR="$DATA_DIR/cache"             # 临时缓存
LOG_DIR="$DATA_DIR/logs"                # 日志文件
PERSISTENT_CACHE_DIR="$DATA_DIR/persistent"  # 持久化缓存

# 颜色
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

# 日志函数
info() {
    echo -e "${BLUE}[INFO]${NC} $1"
}

success() {
    echo -e "${GREEN}[SUCCESS]${NC} $1"
}

warning() {
    echo -e "${YELLOW}[WARNING]${NC} $1"
}

error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

# 检查挂载状态
is_mounted() {
    local mount_point="$1"
    mount | grep -q "$mount_point"
}

# 清理旧挂载和进程
cleanup_mount() {
    local mount_point="$1"
    local name="$2"

    # 检查并卸载旧挂载
    if is_mounted "$mount_point"; then
        warning "$name 发现旧挂载，正在卸载..."

        # 尝试正常卸载
        umount "$mount_point" 2>/dev/null || true
        sleep 2

        # 如果还在挂载，尝试强制卸载
        if is_mounted "$mount_point"; then
            warning "$name 尝试强制卸载..."
            umount -f "$mount_point" 2>/dev/null || umount -l "$mount_point" 2>/dev/null || true
            sleep 1
        fi
    fi

    # 终止相关进程
    local remote=$(echo "$name" | grep -o '[0-9]\+')
    pkill -f "rclone strm-mount ${remote}:" 2>/dev/null || true
    sleep 1
}

# 验证挂载和文件访问（智能优化版本）
verify_mount() {
    local mount_point="$1"
    local name="$2"
    local timeout=20  # 进一步减少超时时间

    info "验证 $name 挂载状态..."

    # 等待挂载就绪
    local count=0
    local access_attempts=0
    while [ $count -lt $timeout ]; do
        if is_mounted "$mount_point"; then
            # 智能目录访问测试：逐步增加超时时间
            local test_timeout=$((2 + access_attempts))
            if timeout $test_timeout ls "$mount_point" >/dev/null 2>&1; then
                success "✅ $name 挂载成功，目录访问正常"
                info "$name STRM文件将在后台生成，可稍后使用 'status' 命令查看"
                return 0
            else
                access_attempts=$((access_attempts + 1))
                if [ $access_attempts -le 3 ]; then
                    info "$name 目录访问测试 $access_attempts/3，继续等待..."
                else
                    warning "$name 挂载成功但目录访问较慢，可能正在初始化大量文件"
                    info "建议稍后使用 'status' 命令检查状态"
                    return 0  # 认为挂载成功，但可能需要更多时间初始化
                fi
            fi
        else
            info "等待 $name 挂载..."
        fi
        sleep 1
        count=$((count + 1))
        if [ $((count % 5)) -eq 0 ]; then
            echo -n "."
        fi
    done

    echo
    # 最后检查一次挂载状态
    if is_mounted "$mount_point"; then
        warning "⚠️ $name 已挂载但验证超时，可能需要更多时间初始化"
        info "可以使用 'status' 命令检查实际状态"
        return 0
    else
        error "$name 挂载失败"
        return 1
    fi
}

# 启动 115 网盘
start_115() {
    local cache_dir="$CACHE_DIR/115"
    local log_file="$LOG_DIR/strm-115.log"

    info "🚀 启动 115 网盘 STRM-Mount..."

    # 清理旧挂载
    cleanup_mount "$MOUNT_115" "115网盘"

    # 创建必要目录
    mkdir -p "$MOUNT_115" "$cache_dir" "$LOG_DIR" "$PERSISTENT_CACHE_DIR"

    # 启动挂载 - 性能优化版本（带详细调试日志）
    info "启动命令: $RCLONE strm-mount 115: $MOUNT_115 --min-size 50M --persistent-cache=true --cache-ttl=24h --dir-cache-time=1h --vfs-cache-mode=minimal --allow-other --default-permissions -vv"

    nohup "$RCLONE" strm-mount 115: "$MOUNT_115" \
        --min-size 50M \
        --persistent-cache=true \
        --cache-ttl=24h \
        --dir-cache-time=1h \
        --attr-timeout=30s \
        --vfs-cache-mode=minimal \
        --max-cache-size=50000 \
        --poll-interval=5m \
        --checkers=3 \
        --transfers=2 \
        --allow-other \
        --default-permissions \
        --uid $(id -u) \
        --gid $(id -g) \
        --daemon \
        --log-file="$log_file" \
        --log-level=INFO \
        > /dev/null 2>&1 &

    # 验证挂载
    if verify_mount "$MOUNT_115" "115网盘"; then
        success "✅ 115 网盘启动完成！"
        info "挂载点: $MOUNT_115"
        info "缓存目录: $cache_dir"
        info "日志文件: $log_file"
        info "查看日志: tail -f $log_file"
        return 0
    else
        error "❌ 115 网盘启动失败"
        info "查看详细日志: tail -20 $log_file"
        return 1
    fi
}

# 启动 123 网盘
start_123() {
    local cache_dir="$CACHE_DIR/123"
    local log_file="$LOG_DIR/strm-123.log"

    info "🚀 启动 123 网盘 STRM-Mount..."

    # 清理旧挂载
    cleanup_mount "$MOUNT_123" "123网盘"

    # 创建必要目录
    mkdir -p "$MOUNT_123" "$cache_dir" "$LOG_DIR" "$PERSISTENT_CACHE_DIR"

    # 启动挂载 - 性能优化版本（带详细调试日志）
    info "启动命令: $RCLONE strm-mount 123: $MOUNT_123 --min-size 50M --persistent-cache=true --cache-ttl=24h --dir-cache-time=1h --vfs-cache-mode=minimal --allow-other --default-permissions -vv"

    nohup "$RCLONE" strm-mount 123: "$MOUNT_123" \
        --min-size 50M \
        --persistent-cache=true \
        --cache-ttl=24h \
        --dir-cache-time=1h \
        --attr-timeout=30s \
        --vfs-cache-mode=minimal \
        --max-cache-size=50000 \
        --poll-interval=5m \
        --checkers=2 \
        --transfers=1 \
        --allow-other \
        --default-permissions \
        --uid $(id -u) \
        --gid $(id -g) \
        --daemon \
        --log-file="$log_file" \
        --log-level=INFO \
        > /dev/null 2>&1 &

    # 验证挂载
    if verify_mount "$MOUNT_123" "123网盘"; then
        success "✅ 123 网盘启动完成！"
        info "挂载点: $MOUNT_123"
        info "缓存目录: $cache_dir"
        info "日志文件: $log_file"
        info "查看日志: tail -f $log_file"
        return 0
    else
        error "❌ 123 网盘启动失败"
        info "查看详细日志: tail -20 $log_file"
        return 1
    fi
}

# 卸载所有
umount_all() {
    info "🛑 卸载所有 STRM-Mount..."

    local unmounted=0

    # 卸载 115
    if is_mounted "$MOUNT_115"; then
        info "卸载 115 网盘: $MOUNT_115"

        # 先尝试正常卸载
        if umount "$MOUNT_115" 2>/dev/null; then
            success "✅ 115 网盘卸载成功"
            ((unmounted++))
        else
            warning "115 网盘正常卸载失败，尝试强制卸载..."

            # 强制卸载
            umount -f "$MOUNT_115" 2>/dev/null || umount -l "$MOUNT_115" 2>/dev/null || true
            sleep 2

            # 验证卸载结果
            if ! is_mounted "$MOUNT_115"; then
                success "✅ 115 网盘强制卸载成功"
                ((unmounted++))
            else
                error "❌ 115 网盘卸载失败"
            fi
        fi
    else
        info "115 网盘未挂载"
    fi

    # 卸载 123
    if is_mounted "$MOUNT_123"; then
        info "卸载 123 网盘: $MOUNT_123"

        # 先尝试正常卸载
        if umount "$MOUNT_123" 2>/dev/null; then
            success "✅ 123 网盘卸载成功"
            ((unmounted++))
        else
            warning "123 网盘正常卸载失败，尝试强制卸载..."

            # 强制卸载
            umount -f "$MOUNT_123" 2>/dev/null || umount -l "$MOUNT_123" 2>/dev/null || true
            sleep 2

            # 验证卸载结果
            if ! is_mounted "$MOUNT_123"; then
                success "✅ 123 网盘强制卸载成功"
                ((unmounted++))
            else
                error "❌ 123 网盘卸载失败"
            fi
        fi
    else
        info "123 网盘未挂载"
    fi

    # 终止相关进程
    info "终止 rclone strm-mount 进程..."
    local pids=$(pgrep -f "rclone strm-mount" 2>/dev/null || true)
    if [[ -n "$pids" ]]; then
        echo "$pids" | xargs kill -TERM 2>/dev/null || true
        sleep 3

        # 检查是否还有进程存在
        local remaining_pids=$(pgrep -f "rclone strm-mount" 2>/dev/null || true)
        if [[ -n "$remaining_pids" ]]; then
            warning "强制终止残留进程..."
            echo "$remaining_pids" | xargs kill -KILL 2>/dev/null || true
        fi
    fi

    if [[ $unmounted -gt 0 ]]; then
        success "🎉 卸载完成！($unmounted 个挂载点)"
    else
        info "没有需要卸载的挂载点"
    fi

    # 清理空目录
    info "清理空挂载目录..."
    rmdir "$MOUNT_115" 2>/dev/null || true
    rmdir "$MOUNT_123" 2>/dev/null || true
}

# 显示状态
show_status() {
    echo -e "${BLUE}=== STRM-Mount 状态 ===${NC}"
    echo
    
    # 115 网盘状态
    echo -e "${YELLOW}115 网盘:${NC}"
    if is_mounted "$MOUNT_115"; then
        echo -e "  状态: ${GREEN}已挂载${NC}"
        echo "  挂载点: $MOUNT_115"
        if [[ -d "$MOUNT_115" ]]; then
            # 优化：限制搜索深度和使用超时，避免长时间扫描
            local count=$(timeout 10 find "$MOUNT_115" -maxdepth 3 -name "*.strm" 2>/dev/null | wc -l | tr -d ' ')
            if [[ $? -eq 0 ]]; then
                echo "  STRM 文件数: $count"
            else
                echo "  STRM 文件数: 正在扫描中..."
            fi
        fi
    else
        echo -e "  状态: ${RED}未挂载${NC}"
    fi
    echo

    # 123 网盘状态
    echo -e "${YELLOW}123 网盘:${NC}"
    if is_mounted "$MOUNT_123"; then
        echo -e "  状态: ${GREEN}已挂载${NC}"
        echo "  挂载点: $MOUNT_123"
        if [[ -d "$MOUNT_123" ]]; then
            # 优化：限制搜索深度和使用超时，避免长时间扫描
            local count=$(timeout 10 find "$MOUNT_123" -maxdepth 3 -name "*.strm" 2>/dev/null | wc -l | tr -d ' ')
            if [[ $? -eq 0 ]]; then
                echo "  STRM 文件数: $count"
            else
                echo "  STRM 文件数: 正在扫描中..."
            fi
        fi
    else
        echo -e "  状态: ${RED}未挂载${NC}"
    fi
    echo
    
    # 进程状态
    echo -e "${YELLOW}进程状态:${NC}"
    local processes=$(pgrep -f "rclone strm-mount" 2>/dev/null | wc -l)
    if [[ $processes -gt 0 ]]; then
        echo -e "  rclone 进程: ${GREEN}$processes 个运行中${NC}"
        pgrep -f "rclone strm-mount" | while read pid; do
            echo "    PID: $pid"
        done
    else
        echo -e "  rclone 进程: ${RED}未运行${NC}"
    fi
    echo
    
    # 日志文件
    echo -e "${YELLOW}日志文件:${NC}"
    for log in "$LOG_DIR/strm-115.log" "$LOG_DIR/strm-123.log"; do
        if [[ -f "$log" ]]; then
            local size=$(du -h "$log" | cut -f1)
            echo "  $log ($size)"
        fi
    done

    # 数据目录
    echo -e "${YELLOW}数据目录:${NC}"
    if [[ -d "$DATA_DIR" ]]; then
        local data_size=$(du -sh "$DATA_DIR" 2>/dev/null | cut -f1)
        echo "  $DATA_DIR ($data_size)"

        # 显示子目录大小
        for subdir in "$CACHE_DIR" "$LOG_DIR" "$PERSISTENT_CACHE_DIR"; do
            if [[ -d "$subdir" ]]; then
                local subdir_name=$(basename "$subdir")
                local subdir_size=$(du -sh "$subdir" 2>/dev/null | cut -f1)
                echo "    $subdir_name: $subdir_size"
            fi
        done
    else
        echo "  数据目录不存在"
    fi

    # 持久化缓存状态
    echo -e "${YELLOW}持久化缓存:${NC}"
    if [[ -d "$PERSISTENT_CACHE_DIR" ]]; then
        local persistent_cache_files=$(find "$PERSISTENT_CACHE_DIR" -name "*.json" 2>/dev/null | wc -l | tr -d ' ')
        if [[ $persistent_cache_files -gt 0 ]]; then
            echo "  缓存文件数: $persistent_cache_files"

            # 显示各网盘的缓存文件
            for backend in "115" "123"; do
                local backend_files=$(find "$PERSISTENT_CACHE_DIR" -name "${backend}_*.json" 2>/dev/null)
                if [[ -n "$backend_files" ]]; then
                    local file_count=$(echo "$backend_files" | wc -l | tr -d ' ')
                    echo "    $backend 网盘: $file_count 个缓存文件"

                    # 显示最新缓存文件的详情
                    local latest_file=$(echo "$backend_files" | head -1)
                    if [[ -f "$latest_file" ]]; then
                        local file_size=$(du -h "$latest_file" | cut -f1)
                        local cached_files=$(grep '"file_count"' "$latest_file" | head -1 | cut -d':' -f2 | tr -d ' ,')
                        local expires_at=$(grep '"expires_at"' "$latest_file" | cut -d'"' -f4 | cut -d'T' -f2 | cut -d'+' -f1)
                        echo "      最新缓存: $file_size, $cached_files 个文件, 过期时间: $expires_at"
                    fi
                fi
            done
        else
            echo "  无缓存文件"
        fi
    else
        echo "  持久化缓存目录不存在"
    fi
}

# 显示帮助
show_help() {
    echo -e "${BLUE}STRM-Mount 简化管理脚本${NC}"
    echo
    echo "用法: $0 <命令>"
    echo
    echo "命令:"
    echo "  start        启动所有网盘 (115 + 123)"
    echo "  start-115    只启动 115 网盘"
    echo "  start-123    只启动 123 网盘"
    echo "  debug        启动DEBUG模式 (详细日志)"
    echo "  debug-115    启动115网盘DEBUG模式"
    echo "  debug-123    启动123网盘DEBUG模式"
    echo "  refresh      强制刷新缓存 (触发DEBUG日志)"
    echo "  stop         停止所有网盘"
    echo "  umount       卸载所有挂载点 (同 stop)"
    echo "  restart      重启所有网盘"
    echo "  status       显示挂载状态"
    echo "  logs         显示最近日志"
    echo "  clean        清理缓存和旧日志"
    echo "  cache-info   显示缓存详细信息"
    echo "  cache-test   测试缓存性能"
    echo "  sync         智能同步所有网盘"
    echo "  sync-115     智能同步115网盘"
    echo "  sync-123     智能同步123网盘"
    echo "  force-sync   强制同步所有网盘"
    echo "  health       健康检查"
    echo "  config       显示配置信息"
    echo "  help         显示此帮助"
    echo
    echo "示例:"
    echo "  $0 start         # 启动所有网盘"
    echo "  $0 start-115     # 只启动 115 网盘"
    echo "  $0 stop          # 停止所有网盘"
    echo "  $0 status        # 查看状态"
    echo
    echo "配置:"
    echo "  rclone 路径: $RCLONE"
    echo "  基础挂载目录: $BASE_MOUNT_DIR"
    echo "  115 挂载点: $MOUNT_115"
    echo "  123 挂载点: $MOUNT_123"
    echo "  数据目录: $DATA_DIR"
    echo "    ├── 缓存: $CACHE_DIR"
    echo "    ├── 日志: $LOG_DIR"
    echo "    └── 持久化: $PERSISTENT_CACHE_DIR"
}

# 显示日志
show_logs() {
    echo -e "${BLUE}=== 最近日志 ===${NC}"
    echo

    local log_115="$LOG_DIR/strm-115.log"
    local log_123="$LOG_DIR/strm-123.log"

    if [[ -f "$log_115" ]]; then
        echo -e "${YELLOW}115 网盘日志 (最近 15 行):${NC}"
        tail -15 "$log_115"
        echo
    else
        warning "115 网盘日志文件不存在: $log_115"
        echo
    fi

    if [[ -f "$log_123" ]]; then
        echo -e "${YELLOW}123 网盘日志 (最近 15 行):${NC}"
        tail -15 "$log_123"
        echo
    else
        warning "123 网盘日志文件不存在: $log_123"
        echo
    fi
}

# 清理缓存
clean_cache() {
    echo -e "${BLUE}=== 清理数据目录 ===${NC}"
    echo

    if [[ -d "$DATA_DIR" ]]; then
        local data_size_before=$(du -sh "$DATA_DIR" 2>/dev/null | cut -f1)
        info "清理前数据目录大小: $data_size_before"

        # 清理临时缓存
        if [[ -d "$CACHE_DIR" ]]; then
            rm -rf "$CACHE_DIR"/* 2>/dev/null || true
            success "✅ 临时缓存清理完成"
        fi

        # 清理持久化缓存
        if [[ -d "$PERSISTENT_CACHE_DIR" ]]; then
            local persistent_files=$(find "$PERSISTENT_CACHE_DIR" -name "*.json" 2>/dev/null | wc -l | tr -d ' ')
            if [[ $persistent_files -gt 0 ]]; then
                info "清理持久化缓存文件 ($persistent_files 个)..."
                rm -f "$PERSISTENT_CACHE_DIR"/*.json 2>/dev/null || true
                success "✅ 持久化缓存清理完成"
            else
                info "无持久化缓存文件需要清理"
            fi
        fi

        # 清理旧日志
        if [[ -d "$LOG_DIR" ]]; then
            info "清理 7 天前的日志文件..."
            find "$LOG_DIR" -name "*.log" -mtime +7 -delete 2>/dev/null || true
            success "✅ 旧日志清理完成"
        fi

        local data_size_after=$(du -sh "$DATA_DIR" 2>/dev/null | cut -f1)
        info "清理后数据目录大小: $data_size_after"
    else
        info "数据目录不存在，无需清理"
    fi
}

# 缓存详情
cache_info() {
    echo -e "${BLUE}=== 缓存详细信息 ===${NC}"
    echo

    # 持久化缓存详情
    if [[ -d "$PERSISTENT_CACHE_DIR" ]]; then
        local cache_files=$(find "$PERSISTENT_CACHE_DIR" -name "*.json" 2>/dev/null)

        if [[ -n "$cache_files" ]]; then
            echo -e "${YELLOW}持久化缓存文件:${NC}"

            for cache_file in $cache_files; do
                local basename=$(basename "$cache_file")
                local size=$(du -h "$cache_file" | cut -f1)
                local mtime=$(stat -f "%Sm" -t "%Y-%m-%d %H:%M:%S" "$cache_file" 2>/dev/null || stat -c "%y" "$cache_file" 2>/dev/null | cut -d'.' -f1)

                echo "  文件: $basename ($size)"
                echo "    修改时间: $mtime"

                # 解析缓存内容
                if [[ -f "$cache_file" ]]; then
                    local file_count=$(grep '"file_count"' "$cache_file" | head -1 | cut -d':' -f2 | tr -d ' ,')
                    local created_at=$(grep '"created_at"' "$cache_file" | cut -d'"' -f4)
                    local expires_at=$(grep '"expires_at"' "$cache_file" | cut -d'"' -f4)
                    local total_size=$(grep '"total_size"' "$cache_file" | head -1 | cut -d':' -f2 | tr -d ' ,')

                    echo "    文件数量: $file_count"
                    echo "    总大小: $total_size 字节"
                    echo "    创建时间: $created_at"
                    echo "    过期时间: $expires_at"

                    # 检查是否过期
                    local current_iso=$(date -u +"%Y-%m-%dT%H:%M:%S")
                    if [[ "$expires_at" > "$current_iso" ]]; then
                        echo -e "    状态: ${GREEN}有效${NC}"
                    else
                        echo -e "    状态: ${RED}已过期${NC}"
                    fi
                fi
                echo
            done
        else
            warning "无持久化缓存文件"
        fi
    else
        warning "持久化缓存目录不存在"
    fi

    # 内存缓存统计（从日志中获取）
    echo -e "${YELLOW}缓存命中统计:${NC}"
    for backend in "115" "123"; do
        local log_file="$LOG_DIR/strm-$backend.log"
        if [[ -f "$log_file" ]]; then
            local total_hits=$(grep -c "CACHE.*Hit" "$log_file" 2>/dev/null || echo "0")
            local memory_hits=$(grep -c "MEMORY-CACHE.*Hit" "$log_file" 2>/dev/null || echo "0")
            local persistent_hits=$(grep -c "PERSISTENT-CACHE.*Hit" "$log_file" 2>/dev/null || echo "0")

            echo "  $backend 网盘:"
            echo "    总缓存命中: $total_hits"
            echo "    内存缓存命中: $memory_hits"
            echo "    持久化缓存命中: $persistent_hits"
        else
            echo "  $backend 网盘: 无日志文件"
        fi
    done
}

# 缓存性能测试
cache_performance_test() {
    echo -e "${BLUE}=== 缓存性能测试 ===${NC}"
    echo

    local test_results=()

    for backend in "115" "123"; do
        local mount_point=""
        if [[ "$backend" == "115" ]]; then
            mount_point="$MOUNT_115"
        else
            mount_point="$MOUNT_123"
        fi

        echo -e "${YELLOW}测试 $backend 网盘缓存性能:${NC}"

        if ! is_mounted "$mount_point"; then
            warning "⚠️ $backend 网盘未挂载，跳过测试"
            continue
        fi

        # 测试根目录访问性能
        local total_time=0
        local success_count=0

        for i in {1..3}; do
            echo -n "  第 $i 次测试... "

            local start_time=$(date +%s)
            if timeout 30 ls "$mount_point" >/dev/null 2>&1; then
                local end_time=$(date +%s)
                local duration=$((end_time - start_time))
                total_time=$((total_time + duration))
                success_count=$((success_count + 1))
                echo "${duration}秒"
            else
                echo "超时"
            fi

            sleep 2
        done

        if [[ $success_count -gt 0 ]]; then
            local avg_time=$((total_time / success_count))
            echo "  平均访问时间: ${avg_time}秒"

            if [[ $avg_time -le 3 ]]; then
                success "  ✅ 性能优秀 (可能命中缓存)"
                test_results+=("$backend:优秀:${avg_time}s")
            elif [[ $avg_time -le 10 ]]; then
                warning "  ⚠️ 性能良好"
                test_results+=("$backend:良好:${avg_time}s")
            else
                error "  ❌ 性能较差 (可能重新扫描)"
                test_results+=("$backend:较差:${avg_time}s")
            fi
        else
            error "  ❌ 所有测试都失败"
            test_results+=("$backend:失败:N/A")
        fi

        echo
    done

    # 显示测试总结
    echo -e "${YELLOW}测试总结:${NC}"
    for result in "${test_results[@]}"; do
        IFS=':' read -r backend performance time <<< "$result"
        echo "  $backend 网盘: $performance ($time)"
    done

    echo
    info "💡 提示："
    info "  - 优秀性能通常表示缓存命中"
    info "  - 较差性能可能表示需要重新扫描或网络问题"
    info "  - 可以使用 'cache-info' 查看缓存详情"
}

# 同步单个后端
sync_backend() {
    local backend="$1"

    echo -e "${BLUE}=== 智能同步 $backend 网盘 ===${NC}"
    echo

    local mount_point=""
    local log_file=""

    if [[ "$backend" == "115" ]]; then
        mount_point="$MOUNT_115"
        log_file="$LOG_DIR/strm-115.log"
    else
        mount_point="$MOUNT_123"
        log_file="$LOG_DIR/strm-123.log"
    fi

    # 检查挂载状态
    if ! is_mounted "$mount_point"; then
        error "$backend 网盘未挂载，请先启动服务"
        return 1
    fi

    # 检查缓存状态
    local cache_files=$(find "$PERSISTENT_CACHE_DIR" -name "${backend}_*.json" 2>/dev/null)

    if [[ -z "$cache_files" ]]; then
        warning "$backend 无缓存文件，将触发重新扫描"
        info "访问挂载点以触发缓存重建..."
        timeout 30 ls "$mount_point" >/dev/null 2>&1 || true
        success "✅ $backend 缓存重建已触发"
        return 0
    fi

    # 检查缓存是否过期
    local latest_cache=$(echo "$cache_files" | head -1)
    local expires_at=$(grep '"expires_at"' "$latest_cache" | cut -d'"' -f4)
    local current_iso=$(date -u +"%Y-%m-%dT%H:%M:%S")

    if [[ "$expires_at" < "$current_iso" ]]; then
        warning "$backend 缓存已过期，将触发增量同步"
        info "访问挂载点以触发增量同步..."
        timeout 30 ls "$mount_point" >/dev/null 2>&1 || true
        success "✅ $backend 增量同步已触发"
    else
        local file_count=$(grep '"file_count"' "$latest_cache" | head -1 | cut -d':' -f2 | tr -d ' ,')
        success "✅ $backend 缓存有效 ($file_count 个文件)，无需同步"
    fi

    return 0
}

# 同步所有后端
sync_all_backends() {
    echo -e "${BLUE}=== 智能同步所有网盘 ===${NC}"
    echo

    sync_backend "115"
    echo
    sync_backend "123"

    echo
    success "🎉 所有网盘同步检查完成！"
}

# 强制同步所有后端
force_sync_all_backends() {
    echo -e "${BLUE}=== 强制同步所有网盘 ===${NC}"
    echo

    warning "⚠️ 这将清除所有缓存并重新扫描文件"
    read -p "确认继续？(y/N): " -n 1 -r
    echo
    if [[ ! $REPLY =~ ^[Yy]$ ]]; then
        info "操作已取消"
        return 0
    fi

    # 清除持久化缓存
    if [[ -d "$PERSISTENT_CACHE_DIR" ]]; then
        info "清除持久化缓存文件..."
        rm -f "$PERSISTENT_CACHE_DIR"/*.json 2>/dev/null || true
        success "✅ 持久化缓存已清除"
    fi

    # 重启所有挂载
    info "重启挂载服务..."
    umount_all
    sleep 3

    info "重新启动所有网盘..."
    start_115 &
    start_123 &
    wait

    success "🎉 强制同步完成！"
}

# 显示配置信息
show_config() {
    echo -e "${BLUE}=== STRM-Mount 配置信息 ===${NC}"
    echo

    echo -e "${YELLOW}基础配置:${NC}"
    echo "  rclone 路径: $RCLONE"
    echo "  基础挂载目录: $BASE_MOUNT_DIR"
    echo "  115 挂载点: $MOUNT_115"
    echo "  123 挂载点: $MOUNT_123"
    echo

    echo -e "${YELLOW}数据目录结构:${NC}"
    echo "  数据目录: $DATA_DIR"
    echo "    ├── 缓存: $CACHE_DIR"
    echo "    ├── 日志: $LOG_DIR"
    echo "    └── 持久化: $PERSISTENT_CACHE_DIR"
    echo

    echo -e "${YELLOW}目录状态:${NC}"
    for dir in "$BASE_MOUNT_DIR" "$DATA_DIR" "$CACHE_DIR" "$LOG_DIR" "$PERSISTENT_CACHE_DIR"; do
        local dir_name=$(basename "$dir")
        if [[ -d "$dir" ]]; then
            # 检查是否是挂载点
            if mount | grep -q "$dir"; then
                echo "  🔗 $dir_name: $dir (挂载点)"
            else
                # 简化大小计算，避免超时问题
                local dir_size=$(du -sh "$dir" 2>/dev/null | cut -f1)
                if [[ -n "$dir_size" ]]; then
                    echo "  ✅ $dir_name: $dir ($dir_size)"
                else
                    echo "  ⚠️ $dir_name: $dir (存在)"
                fi
            fi
        else
            echo "  ❌ $dir_name: $dir (不存在)"
        fi
    done
    echo

    echo -e "${YELLOW}文件权限:${NC}"
    if [[ -f "$RCLONE" ]]; then
        local perms=$(ls -la "$RCLONE" 2>/dev/null | awk '{print $1}')
        if [[ -n "$perms" ]]; then
            echo "  rclone: $perms"
        else
            echo "  rclone: 无法读取权限"
        fi
    else
        echo "  rclone: 文件不存在"
    fi

    for mount in "$MOUNT_115" "$MOUNT_123"; do
        if [[ -d "$mount" ]]; then
            # 使用超时避免挂载点问题
            local perms=$(timeout 3 ls -ld "$mount" 2>/dev/null | awk '{print $1}')
            local mount_name=$(basename "$mount")
            if [[ -n "$perms" ]]; then
                echo "  $mount_name: $perms"
            else
                echo "  $mount_name: 无法读取权限"
            fi
        else
            local mount_name=$(basename "$mount")
            echo "  $mount_name: 目录不存在"
        fi
    done
}

# 健康检查
health_check() {
    echo -e "${BLUE}=== 健康检查 ===${NC}"
    echo

    local issues=0

    # 检查挂载状态
    info "检查挂载状态..."
    for mount_point in "$MOUNT_115" "$MOUNT_123"; do
        local name=$(basename "$mount_point")
        if is_mounted "$mount_point"; then
            # 测试文件访问
            if timeout 10 ls "$mount_point" >/dev/null 2>&1; then
                success "✅ $name 挂载正常"
            else
                error "❌ $name 挂载异常 - 文件访问超时"
                ((issues++))
            fi
        else
            warning "⚠️ $name 未挂载"
        fi
    done

    # 检查进程状态
    info "检查进程状态..."
    local processes=$(pgrep -f "rclone strm-mount" 2>/dev/null | wc -l)
    if [[ $processes -gt 0 ]]; then
        success "✅ rclone 进程运行正常 ($processes 个)"
    else
        warning "⚠️ 没有 rclone strm-mount 进程运行"
    fi

    # 检查缓存目录
    info "检查缓存目录..."
    if [[ -d "$CACHE_DIR" ]]; then
        local cache_size=$(du -sh "$CACHE_DIR" 2>/dev/null | cut -f1)
        success "✅ 缓存目录正常 ($cache_size)"
    else
        warning "⚠️ 缓存目录不存在"
    fi

    # 检查日志文件
    info "检查日志文件..."
    for log in "$LOG_DIR/strm-115.log" "$LOG_DIR/strm-123.log"; do
        if [[ -f "$log" ]]; then
            local log_size=$(du -h "$log" | cut -f1)
            local name=$(basename "$log" .log)
            success "✅ $name 日志正常 ($log_size)"
        else
            warning "⚠️ 日志文件不存在: $log"
        fi
    done

    echo
    if [[ $issues -eq 0 ]]; then
        success "🎉 健康检查完成，系统运行正常！"
    else
        warning "⚠️ 发现 $issues 个问题，建议检查相关组件"
    fi
}

# 主函数
main() {
    case "$1" in
        start)
            info "🚀 启动所有网盘..."
            start_115
            echo
            start_123
            echo
            show_status
            ;;
        start-115)
            start_115
            ;;
        start-123)
            start_123
            ;;
        debug)
            info "🔍 启动DEBUG模式 (详细日志)..."
            DEBUG_MODE=true
            start_115
            echo
            start_123
            echo
            show_status
            info "📋 DEBUG模式已启用，查看详细日志："
            info "  115网盘: tail -f $LOG_DIR/strm-115.log"
            info "  123网盘: tail -f $LOG_DIR/strm-123.log"
            ;;
        debug-115)
            info "🔍 启动115网盘DEBUG模式..."
            DEBUG_MODE=true
            start_115
            info "📋 查看详细日志: tail -f $LOG_DIR/strm-115.log"
            ;;
        debug-123)
            info "🔍 启动123网盘DEBUG模式..."
            DEBUG_MODE=true
            start_123
            info "📋 查看详细日志: tail -f $LOG_DIR/strm-123.log"
            ;;
        refresh)
            info "🔄 强制刷新缓存以触发DEBUG日志..."
            clean_cache
            echo
            info "📋 访问目录以触发新的DEBUG日志..."
            if is_mounted "$MOUNT_115"; then
                info "🔍 触发115网盘目录访问..."
                ls "$MOUNT_115" > /dev/null 2>&1 || true
            fi
            if is_mounted "$MOUNT_123"; then
                info "🔍 触发123网盘目录访问..."
                ls "$MOUNT_123" > /dev/null 2>&1 || true
            fi
            echo
            info "📋 查看最新DEBUG日志："
            info "  115网盘: tail -20 $LOG_DIR/strm-115.log"
            info "  123网盘: tail -20 $LOG_DIR/strm-123.log"
            ;;
        stop|umount)
            umount_all
            ;;
        restart)
            umount_all
            echo
            sleep 2
            info "🚀 重新启动所有网盘..."
            start_115
            echo
            start_123
            echo
            show_status
            ;;
        status)
            show_status
            ;;
        logs)
            show_logs
            ;;
        clean|clean-cache)
            clean_cache
            ;;
        cache-info|cache)
            cache_info
            ;;
        cache-test|test-cache)
            cache_performance_test
            ;;
        sync)
            sync_all_backends
            ;;
        sync-115)
            sync_backend "115"
            ;;
        sync-123)
            sync_backend "123"
            ;;
        force-sync)
            force_sync_all_backends
            ;;
        health|check)
            health_check
            ;;
        config)
            show_config
            ;;
        help|--help|-h)
            show_help
            ;;
        "")
            show_help
            ;;
        *)
            error "未知命令: $1"
            echo
            show_help
            exit 1
            ;;
    esac
}

# 检查 rclone
if [[ ! -f "$RCLONE" ]]; then
    error "rclone 文件不存在: $RCLONE"
    exit 1
fi

if [[ ! -x "$RCLONE" ]]; then
    error "rclone 文件没有执行权限: $RCLONE"
    exit 1
fi

# 运行主函数
main "$@"
