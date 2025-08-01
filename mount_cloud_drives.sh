#!/bin/bash

# 云盘挂载脚本 - 支持123和115网盘
# 使用方法: ./mount_cloud_drives.sh [命令]

set -e

# 配置
MOUNT_BASE_DIR="$HOME/CloudDrives"
RCLONE_PATH="./rclone"
LOG_DIR="$HOME/.rclone/logs"

# 颜色输出
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# 日志函数
log_info() {
    echo -e "${GREEN}[INFO]${NC} $1"
}

log_warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

# 检查依赖
check_dependencies() {
    log_info "检查依赖..."
    
    # 检查rclone
    if [ ! -f "$RCLONE_PATH" ]; then
        log_error "rclone未找到: $RCLONE_PATH"
        exit 1
    fi
    
    # 检查mount命令是否可用
    if ! $RCLONE_PATH mount --help >/dev/null 2>&1; then
        log_error "rclone mount功能不可用，请重新编译: go build -tags cmount -o rclone ./"
        exit 1
    fi
    
    # 检查macFUSE (macOS)
    if [[ "$OSTYPE" == "darwin"* ]]; then
        if ! command -v mount_macfuse &> /dev/null; then
            log_warn "macFUSE未安装，请安装: brew install macfuse"
        fi
    fi
    
    log_info "依赖检查完成"
}

# 创建目录
create_directories() {
    log_info "创建挂载目录..."
    
    mkdir -p "$MOUNT_BASE_DIR"/{123pan,115pan,123video,115tutorials}
    mkdir -p "$LOG_DIR"
    
    log_info "目录创建完成"
}

# 检查挂载状态
check_mount_status() {
    local mount_point="$1"
    if mount | grep -q "$mount_point"; then
        return 0  # 已挂载
    else
        return 1  # 未挂载
    fi
}

# 挂载123网盘
mount_123pan() {
    local mount_point="$MOUNT_BASE_DIR/123pan"
    local log_file="$LOG_DIR/123pan.log"
    
    log_info "挂载123网盘到: $mount_point"
    
    if check_mount_status "$mount_point"; then
        log_warn "123网盘已经挂载"
        return
    fi
    
    $RCLONE_PATH mount 123: "$mount_point" \
        --daemon \
        --vfs-cache-mode writes \
        --vfs-cache-max-size 10G \
        --vfs-read-chunk-size 128M \
        --buffer-size 64M \
        --log-file "$log_file" \
        --log-level INFO \
        --allow-other 2>/dev/null || \
    $RCLONE_PATH mount 123: "$mount_point" \
        --daemon \
        --vfs-cache-mode writes \
        --vfs-cache-max-size 10G \
        --vfs-read-chunk-size 128M \
        --buffer-size 64M \
        --log-file "$log_file" \
        --log-level INFO
    
    sleep 3
    
    if check_mount_status "$mount_point"; then
        log_info "123网盘挂载成功"
    else
        log_error "123网盘挂载失败，请检查日志: $log_file"
    fi
}

# 挂载115网盘
mount_115pan() {
    local mount_point="$MOUNT_BASE_DIR/115pan"
    local log_file="$LOG_DIR/115pan.log"
    
    log_info "挂载115网盘到: $mount_point"
    
    if check_mount_status "$mount_point"; then
        log_warn "115网盘已经挂载"
        return
    fi
    
    $RCLONE_PATH mount 115: "$mount_point" \
        --daemon \
        --vfs-cache-mode writes \
        --vfs-cache-max-size 10G \
        --vfs-read-chunk-size 128M \
        --buffer-size 64M \
        --log-file "$log_file" \
        --log-level INFO \
        --allow-other 2>/dev/null || \
    $RCLONE_PATH mount 115: "$mount_point" \
        --daemon \
        --vfs-cache-mode writes \
        --vfs-cache-max-size 10G \
        --vfs-read-chunk-size 128M \
        --buffer-size 64M \
        --log-file "$log_file" \
        --log-level INFO
    
    sleep 3
    
    if check_mount_status "$mount_point"; then
        log_info "115网盘挂载成功"
    else
        log_error "115网盘挂载失败，请检查日志: $log_file"
    fi
}

# 挂载123视频目录
mount_123video() {
    local mount_point="$MOUNT_BASE_DIR/123video"
    local log_file="$LOG_DIR/123video.log"
    
    log_info "挂载123视频目录到: $mount_point"
    
    if check_mount_status "$mount_point"; then
        log_warn "123视频目录已经挂载"
        return
    fi
    
    $RCLONE_PATH mount 123:/video "$mount_point" \
        --daemon \
        --vfs-cache-mode writes \
        --vfs-cache-max-size 5G \
        --vfs-read-chunk-size 64M \
        --buffer-size 32M \
        --log-file "$log_file" \
        --log-level INFO \
        --allow-other 2>/dev/null || \
    $RCLONE_PATH mount 123:/video "$mount_point" \
        --daemon \
        --vfs-cache-mode writes \
        --vfs-cache-max-size 5G \
        --vfs-read-chunk-size 64M \
        --buffer-size 32M \
        --log-file "$log_file" \
        --log-level INFO
    
    sleep 3
    
    if check_mount_status "$mount_point"; then
        log_info "123视频目录挂载成功"
    else
        log_error "123视频目录挂载失败，请检查日志: $log_file"
    fi
}

# 挂载115教程目录
mount_115tutorials() {
    local mount_point="$MOUNT_BASE_DIR/115tutorials"
    local log_file="$LOG_DIR/115tutorials.log"
    
    log_info "挂载115教程目录到: $mount_point"
    
    if check_mount_status "$mount_point"; then
        log_warn "115教程目录已经挂载"
        return
    fi
    
    $RCLONE_PATH mount 115:/教程 "$mount_point" \
        --daemon \
        --vfs-cache-mode writes \
        --vfs-cache-max-size 5G \
        --vfs-read-chunk-size 64M \
        --buffer-size 32M \
        --log-file "$log_file" \
        --log-level INFO \
        --allow-other 2>/dev/null || \
    $RCLONE_PATH mount 115:/教程 "$mount_point" \
        --daemon \
        --vfs-cache-mode writes \
        --vfs-cache-max-size 5G \
        --vfs-read-chunk-size 64M \
        --buffer-size 32M \
        --log-file "$log_file" \
        --log-level INFO
    
    sleep 3
    
    if check_mount_status "$mount_point"; then
        log_info "115教程目录挂载成功"
    else
        log_error "115教程目录挂载失败，请检查日志: $log_file"
    fi
}

# 卸载所有挂载
unmount_all() {
    log_info "卸载所有云盘挂载..."
    
    for mount_point in "$MOUNT_BASE_DIR"/*; do
        if [ -d "$mount_point" ] && check_mount_status "$mount_point"; then
            log_info "卸载: $mount_point"
            if [[ "$OSTYPE" == "darwin"* ]]; then
                umount "$mount_point" 2>/dev/null || diskutil unmount "$mount_point" 2>/dev/null || true
            else
                fusermount -u "$mount_point" 2>/dev/null || umount "$mount_point" 2>/dev/null || true
            fi
        fi
    done
    
    log_info "卸载完成"
}

# 显示挂载状态
show_status() {
    log_info "云盘挂载状态:"
    echo
    
    for mount_point in "$MOUNT_BASE_DIR"/*; do
        if [ -d "$mount_point" ]; then
            local name=$(basename "$mount_point")
            if check_mount_status "$mount_point"; then
                echo -e "  ${GREEN}✓${NC} $name: 已挂载 -> $mount_point"
                # 显示挂载点内容数量
                local count=$(ls -1 "$mount_point" 2>/dev/null | wc -l | tr -d ' ')
                echo -e "    包含 $count 个项目"
            else
                echo -e "  ${RED}✗${NC} $name: 未挂载 -> $mount_point"
            fi
        fi
    done
    echo
}

# 主函数
main() {
    case "${1:-help}" in
        "mount-all")
            check_dependencies
            create_directories
            mount_123pan
            mount_115pan
            mount_123video
            mount_115tutorials
            show_status
            ;;
        "mount-123")
            check_dependencies
            create_directories
            mount_123pan
            show_status
            ;;
        "mount-115")
            check_dependencies
            create_directories
            mount_115pan
            show_status
            ;;
        "mount-video")
            check_dependencies
            create_directories
            mount_123video
            show_status
            ;;
        "mount-tutorials")
            check_dependencies
            create_directories
            mount_115tutorials
            show_status
            ;;
        "unmount")
            unmount_all
            show_status
            ;;
        "status")
            show_status
            ;;
        "help"|*)
            echo "🚀 云盘挂载脚本"
            echo
            echo "用法: $0 [命令]"
            echo
            echo "命令:"
            echo "  mount-all       挂载所有云盘"
            echo "  mount-123       只挂载123网盘"
            echo "  mount-115       只挂载115网盘"
            echo "  mount-video     只挂载123视频目录"
            echo "  mount-tutorials 只挂载115教程目录"
            echo "  unmount         卸载所有挂载"
            echo "  status          显示挂载状态"
            echo "  help            显示此帮助"
            echo
            echo "挂载点: $MOUNT_BASE_DIR"
            echo "日志目录: $LOG_DIR"
            echo
            echo "示例:"
            echo "  $0 mount-all    # 挂载所有云盘"
            echo "  $0 status       # 查看挂载状态"
            echo "  $0 unmount      # 卸载所有挂载"
            ;;
    esac
}

main "$@"
