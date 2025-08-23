#!/bin/bash

# STRM-Mount 卸载脚本
# 功能：安全地卸载所有 STRM 挂载点并终止相关进程
# 作者：STRM-Mount 项目
# 版本：1.0.0

set -e  # 遇到错误时退出

# 颜色定义
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
PURPLE='\033[0;35m'
CYAN='\033[0;36m'
NC='\033[0m' # No Color

# 日志函数
log_info() {
    echo -e "${BLUE}[INFO]${NC} $1"
}

log_success() {
    echo -e "${GREEN}[SUCCESS]${NC} $1"
}

log_warning() {
    echo -e "${YELLOW}[WARNING]${NC} $1"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

log_step() {
    echo -e "${PURPLE}[STEP]${NC} $1"
}

# 显示脚本头部信息
show_header() {
    echo -e "${CYAN}"
    echo "╔══════════════════════════════════════════════════════════════╗"
    echo "║                    STRM-Mount 卸载脚本                        ║"
    echo "║                                                              ║"
    echo "║  功能：安全卸载所有 STRM 挂载点并终止相关进程                  ║"
    echo "║  版本：1.0.0                                                 ║"
    echo "╚══════════════════════════════════════════════════════════════╝"
    echo -e "${NC}"
}

# 检查是否为 root 用户（某些操作可能需要）
check_permissions() {
    if [[ $EUID -eq 0 ]]; then
        log_warning "检测到 root 权限，请谨慎操作"
    fi
}

# 查找所有 STRM 相关的挂载点
find_strm_mounts() {
    log_step "🔍 查找 STRM 相关挂载点..."
    
    # 查找包含 strm、123、115 等关键词的挂载点
    local mounts=$(mount | grep -E "(strm|123:|115:)" | awk '{print $3}' || true)
    
    if [[ -z "$mounts" ]]; then
        log_info "未发现 STRM 相关挂载点"
        return 1
    else
        log_info "发现以下 STRM 挂载点："
        echo "$mounts" | while read -r mount_point; do
            echo -e "  ${YELLOW}→${NC} $mount_point"
        done
        echo "$mounts"
        return 0
    fi
}

# 查找所有 rclone 相关进程
find_rclone_processes() {
    log_step "🔍 查找 rclone 相关进程..."
    
    # 查找 rclone strm-mount 进程
    local pids=$(pgrep -f "rclone.*strm-mount" || true)
    
    if [[ -z "$pids" ]]; then
        log_info "未发现 rclone strm-mount 进程"
        return 1
    else
        log_info "发现以下 rclone 进程："
        echo "$pids" | while read -r pid; do
            local cmd=$(ps -p "$pid" -o command= 2>/dev/null || echo "进程已退出")
            echo -e "  ${YELLOW}PID $pid:${NC} $cmd"
        done
        echo "$pids"
        return 0
    fi
}

# 优雅地卸载挂载点
graceful_unmount() {
    local mount_point="$1"
    log_info "尝试优雅卸载: $mount_point"
    
    if umount "$mount_point" 2>/dev/null; then
        log_success "✅ 成功卸载: $mount_point"
        return 0
    else
        log_warning "⚠️  优雅卸载失败: $mount_point"
        return 1
    fi
}

# 强制卸载挂载点
force_unmount() {
    local mount_point="$1"
    log_warning "尝试强制卸载: $mount_point"
    
    if umount -f "$mount_point" 2>/dev/null; then
        log_success "✅ 强制卸载成功: $mount_point"
        return 0
    else
        log_error "❌ 强制卸载失败: $mount_point"
        return 1
    fi
}

# 懒惰卸载（最后手段）
lazy_unmount() {
    local mount_point="$1"
    log_warning "尝试懒惰卸载: $mount_point"
    
    if umount -l "$mount_point" 2>/dev/null; then
        log_success "✅ 懒惰卸载成功: $mount_point"
        return 0
    else
        log_error "❌ 懒惰卸载失败: $mount_point"
        return 1
    fi
}

# 卸载所有 STRM 挂载点
unmount_all_strm() {
    log_step "🔌 开始卸载 STRM 挂载点..."
    
    local mounts
    if ! mounts=$(find_strm_mounts); then
        return 0
    fi
    
    local failed_mounts=()
    
    echo "$mounts" | while read -r mount_point; do
        if [[ -z "$mount_point" ]]; then
            continue
        fi
        
        # 尝试三种卸载方式
        if ! graceful_unmount "$mount_point"; then
            if ! force_unmount "$mount_point"; then
                if ! lazy_unmount "$mount_point"; then
                    failed_mounts+=("$mount_point")
                fi
            fi
        fi
    done
    
    # 检查是否有卸载失败的挂载点
    if [[ ${#failed_mounts[@]} -gt 0 ]]; then
        log_error "以下挂载点卸载失败："
        for mount_point in "${failed_mounts[@]}"; do
            echo -e "  ${RED}✗${NC} $mount_point"
        done
        return 1
    else
        log_success "✅ 所有 STRM 挂载点已成功卸载"
        return 0
    fi
}

# 优雅地终止进程
graceful_kill() {
    local pid="$1"
    log_info "尝试优雅终止进程: PID $pid"
    
    if kill -TERM "$pid" 2>/dev/null; then
        # 等待进程退出
        local count=0
        while kill -0 "$pid" 2>/dev/null && [[ $count -lt 10 ]]; do
            sleep 1
            ((count++))
        done
        
        if ! kill -0 "$pid" 2>/dev/null; then
            log_success "✅ 进程已优雅退出: PID $pid"
            return 0
        else
            log_warning "⚠️  进程未在预期时间内退出: PID $pid"
            return 1
        fi
    else
        log_warning "⚠️  无法发送 TERM 信号: PID $pid"
        return 1
    fi
}

# 强制终止进程
force_kill() {
    local pid="$1"
    log_warning "尝试强制终止进程: PID $pid"
    
    if kill -KILL "$pid" 2>/dev/null; then
        log_success "✅ 进程已强制终止: PID $pid"
        return 0
    else
        log_error "❌ 无法强制终止进程: PID $pid"
        return 1
    fi
}

# 终止所有 rclone 进程
kill_all_rclone() {
    log_step "🔪 开始终止 rclone 进程..."
    
    local pids
    if ! pids=$(find_rclone_processes); then
        return 0
    fi
    
    local failed_pids=()
    
    echo "$pids" | while read -r pid; do
        if [[ -z "$pid" ]]; then
            continue
        fi
        
        # 尝试优雅终止，失败则强制终止
        if ! graceful_kill "$pid"; then
            if ! force_kill "$pid"; then
                failed_pids+=("$pid")
            fi
        fi
    done
    
    # 检查是否有终止失败的进程
    if [[ ${#failed_pids[@]} -gt 0 ]]; then
        log_error "以下进程终止失败："
        for pid in "${failed_pids[@]}"; do
            echo -e "  ${RED}✗${NC} PID $pid"
        done
        return 1
    else
        log_success "✅ 所有 rclone 进程已成功终止"
        return 0
    fi
}

# 清理临时文件和目录
cleanup_temp_files() {
    log_step "🧹 清理临时文件和目录..."
    
    # 清理 /tmp 下的 strm 相关目录
    local temp_dirs=$(find /tmp -maxdepth 1 -name "strm-*" -type d 2>/dev/null || true)
    
    if [[ -n "$temp_dirs" ]]; then
        log_info "发现临时目录："
        echo "$temp_dirs" | while read -r dir; do
            echo -e "  ${YELLOW}→${NC} $dir"
        done
        
        echo "$temp_dirs" | while read -r dir; do
            if rm -rf "$dir" 2>/dev/null; then
                log_success "✅ 已清理: $dir"
            else
                log_warning "⚠️  清理失败: $dir"
            fi
        done
    else
        log_info "未发现需要清理的临时目录"
    fi
    
    # 清理临时日志文件
    local temp_logs=$(find /tmp -maxdepth 1 -name "*strm*.log" -o -name "*cache*.log" 2>/dev/null || true)
    
    if [[ -n "$temp_logs" ]]; then
        log_info "发现临时日志文件："
        echo "$temp_logs" | while read -r log_file; do
            echo -e "  ${YELLOW}→${NC} $log_file"
        done
        
        echo "$temp_logs" | while read -r log_file; do
            if rm -f "$log_file" 2>/dev/null; then
                log_success "✅ 已清理: $log_file"
            else
                log_warning "⚠️  清理失败: $log_file"
            fi
        done
    else
        log_info "未发现需要清理的临时日志文件"
    fi
}

# 验证清理结果
verify_cleanup() {
    log_step "🔍 验证清理结果..."
    
    local remaining_mounts=$(mount | grep -E "(strm|123:|115:)" || true)
    local remaining_processes=$(pgrep -f "rclone.*strm-mount" || true)
    
    if [[ -z "$remaining_mounts" ]] && [[ -z "$remaining_processes" ]]; then
        log_success "✅ 清理验证通过：无残留挂载点和进程"
        return 0
    else
        log_warning "⚠️  发现残留项目："
        
        if [[ -n "$remaining_mounts" ]]; then
            log_warning "残留挂载点："
            echo "$remaining_mounts" | while read -r mount; do
                echo -e "  ${YELLOW}→${NC} $mount"
            done
        fi
        
        if [[ -n "$remaining_processes" ]]; then
            log_warning "残留进程："
            echo "$remaining_processes" | while read -r pid; do
                local cmd=$(ps -p "$pid" -o command= 2>/dev/null || echo "进程已退出")
                echo -e "  ${YELLOW}PID $pid:${NC} $cmd"
            done
        fi
        
        return 1
    fi
}

# 显示使用帮助
show_help() {
    echo "用法: $0 [选项]"
    echo ""
    echo "选项:"
    echo "  -h, --help     显示此帮助信息"
    echo "  -f, --force    强制模式，跳过确认提示"
    echo "  -v, --verbose  详细模式，显示更多信息"
    echo "  -q, --quiet    安静模式，只显示错误信息"
    echo ""
    echo "示例:"
    echo "  $0              # 交互式卸载"
    echo "  $0 -f           # 强制卸载，无需确认"
    echo "  $0 -v           # 详细模式卸载"
    echo ""
}

# 主函数
main() {
    local force_mode=false
    local verbose_mode=false
    local quiet_mode=false
    
    # 解析命令行参数
    while [[ $# -gt 0 ]]; do
        case $1 in
            -h|--help)
                show_help
                exit 0
                ;;
            -f|--force)
                force_mode=true
                shift
                ;;
            -v|--verbose)
                verbose_mode=true
                shift
                ;;
            -q|--quiet)
                quiet_mode=true
                shift
                ;;
            *)
                log_error "未知选项: $1"
                show_help
                exit 1
                ;;
        esac
    done
    
    # 安静模式下重定向输出
    if [[ "$quiet_mode" == true ]]; then
        exec 1>/dev/null
    fi
    
    show_header
    check_permissions
    
    # 非强制模式下询问确认
    if [[ "$force_mode" != true ]]; then
        echo -e "${YELLOW}此操作将：${NC}"
        echo "  1. 卸载所有 STRM 相关挂载点"
        echo "  2. 终止所有 rclone strm-mount 进程"
        echo "  3. 清理临时文件和目录"
        echo ""
        read -p "确认继续？(y/N): " -n 1 -r
        echo
        if [[ ! $REPLY =~ ^[Yy]$ ]]; then
            log_info "操作已取消"
            exit 0
        fi
    fi
    
    local exit_code=0
    
    # 执行清理步骤
    if ! unmount_all_strm; then
        exit_code=1
    fi
    
    if ! kill_all_rclone; then
        exit_code=1
    fi
    
    cleanup_temp_files
    
    if ! verify_cleanup; then
        exit_code=1
    fi
    
    # 显示最终结果
    echo ""
    if [[ $exit_code -eq 0 ]]; then
        log_success "🎉 STRM-Mount 卸载完成！所有挂载点和进程已清理"
    else
        log_warning "⚠️  STRM-Mount 卸载完成，但存在部分问题，请检查上述警告信息"
    fi
    
    exit $exit_code
}

# 脚本入口点
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
    main "$@"
fi
