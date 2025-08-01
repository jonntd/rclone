#!/bin/bash
# 跨网盘复制测试脚本: 115网盘 → 123网盘
# 版本: 1.0
# 用途: 测试115网盘到123网盘的跨平台数据传输能力

# =============================================================================
# 配置加载 - 支持配置文件和环境变量
# =============================================================================

# 加载配置文件（如果存在）
CONFIG_FILE=${CONFIG_FILE:-"test_cross_copy_config.conf"}
if [ -f "$CONFIG_FILE" ]; then
    echo "📋 加载配置文件: $CONFIG_FILE"
    source "$CONFIG_FILE"
else
    echo "📋 使用默认配置（未找到配置文件: $CONFIG_FILE）"
fi

# 测试配置
SOURCE_DIR=${SOURCE_DIR:-"115_test_unified"}      # 115网盘源目录
TARGET_DIR=${TARGET_DIR:-"123_test_unified"}      # 123网盘目标目录
CLEAN_TARGET=${CLEAN_TARGET:-false}               # 是否清理目标目录
VERIFY_INTEGRITY=${VERIFY_INTEGRITY:-true}        # 是否验证文件完整性
CONCURRENT_TRANSFERS=${CONCURRENT_TRANSFERS:-1}   # 并发传输数
SHOW_PROGRESS=${SHOW_PROGRESS:-true}              # 是否显示进度
TEST_TIMEOUT=${TEST_TIMEOUT:-3600}                # 测试超时时间(秒)

# 日志配置
LOG_DIR="cross_copy_logs_$(date +%Y%m%d_%H%M%S)"
MAIN_LOG="$LOG_DIR/115_to_123_main.log"
COPY_LOG="$LOG_DIR/115_to_123_copy.log"
VERIFY_LOG="$LOG_DIR/115_to_123_verify.log"

# =============================================================================
# 工具函数
# =============================================================================

# 日志函数
log_info() {
    echo "$(date '+%Y-%m-%d %H:%M:%S') [INFO] $1" | tee -a "$MAIN_LOG"
}

log_error() {
    echo "$(date '+%Y-%m-%d %H:%M:%S') [ERROR] $1" | tee -a "$MAIN_LOG" >&2
}

log_success() {
    echo "$(date '+%Y-%m-%d %H:%M:%S') [SUCCESS] $1" | tee -a "$MAIN_LOG"
}

log_warning() {
    echo "$(date '+%Y-%m-%d %H:%M:%S') [WARNING] $1" | tee -a "$MAIN_LOG"
}

# 创建目录
ensure_dir() {
    if [ ! -d "$1" ]; then
        mkdir -p "$1"
    fi
}

# 清理函数
cleanup() {
    log_info "开始清理测试环境..."
    
    # 清理目标目录
    if [ "$CLEAN_TARGET" = "true" ]; then
        log_info "清理目标目录: 123:$TARGET_DIR/"
        if ./rclone_test ls "123:$TARGET_DIR/" >/dev/null 2>&1; then
            ./rclone_test delete "123:$TARGET_DIR/" 2>/dev/null || true
            log_info "目标目录清理完成"
        else
            log_info "目标目录不存在，无需清理"
        fi
    else
        log_info "保留目标目录 (CLEAN_TARGET=false)"
    fi
    
    log_info "清理完成"
}

# 错误处理
handle_error() {
    local exit_code=$?
    log_error "脚本执行出错，退出码: $exit_code"
    cleanup
    exit $exit_code
}

# 设置错误处理
trap handle_error ERR
trap cleanup EXIT

# 获取目录文件列表和大小信息
get_directory_info() {
    local remote_path="$1"
    local info_file="$2"
    
    log_info "获取目录信息: $remote_path"
    ./rclone_test lsjson "$remote_path/" --recursive 2>/dev/null > "$info_file" || {
        log_error "无法获取目录信息: $remote_path"
        return 1
    }
    
    local file_count=$(jq length "$info_file" 2>/dev/null || echo "0")
    local total_size=$(jq '[.[] | .Size] | add' "$info_file" 2>/dev/null || echo "0")
    
    log_info "目录 $remote_path 包含 $file_count 个文件，总大小: $(numfmt --to=iec $total_size 2>/dev/null || echo $total_size) 字节"
    
    return 0
}

# 验证文件完整性
verify_file_integrity() {
    local source_info="$1"
    local target_info="$2"
    
    log_info "开始验证文件完整性..."
    
    local source_count=$(jq length "$source_info" 2>/dev/null || echo "0")
    local target_count=$(jq length "$target_info" 2>/dev/null || echo "0")
    
    log_info "源目录文件数: $source_count, 目标目录文件数: $target_count"
    
    if [ "$source_count" -ne "$target_count" ]; then
        log_error "文件数量不匹配: 源=$source_count, 目标=$target_count"
        return 1
    fi
    
    # 验证每个文件的大小
    local verified_count=0
    local failed_count=0
    
    while IFS= read -r source_file; do
        local filename=$(echo "$source_file" | jq -r '.Name')
        local source_size=$(echo "$source_file" | jq -r '.Size')
        
        local target_size=$(jq -r ".[] | select(.Name == \"$filename\") | .Size" "$target_info" 2>/dev/null || echo "null")
        
        if [ "$target_size" = "null" ]; then
            log_error "目标文件不存在: $filename"
            ((failed_count++))
        elif [ "$source_size" = "$target_size" ]; then
            log_info "文件验证通过: $filename ($source_size 字节)"
            ((verified_count++))
        else
            log_error "文件大小不匹配: $filename (源=$source_size, 目标=$target_size)"
            ((failed_count++))
        fi
    done < <(jq -c '.[]' "$source_info" 2>/dev/null)
    
    log_info "完整性验证结果: 通过=$verified_count, 失败=$failed_count"
    
    if [ $failed_count -eq 0 ]; then
        log_success "所有文件完整性验证通过！"
        return 0
    else
        log_error "有 $failed_count 个文件验证失败"
        return 1
    fi
}

# 执行跨网盘复制
perform_cross_copy() {
    local source="115:$SOURCE_DIR/"
    local target="123:$TARGET_DIR/"
    
    log_info "开始跨网盘复制: $source → $target"
    
    # 构建rclone命令
    local rclone_cmd="./rclone_test copy \"$source\" \"$target\""
    rclone_cmd="$rclone_cmd --transfers $CONCURRENT_TRANSFERS"
    rclone_cmd="$rclone_cmd --checkers 1"
    rclone_cmd="$rclone_cmd -vv"
    rclone_cmd="$rclone_cmd --log-file \"$COPY_LOG\""
    
    if [ "$SHOW_PROGRESS" = "true" ]; then
        rclone_cmd="$rclone_cmd -P"
    fi
    
    log_info "执行命令: $rclone_cmd"
    
    # 记录开始时间
    local start_time=$(date +%s)
    
    # 执行复制
    eval $rclone_cmd
    local copy_result=$?
    
    # 记录结束时间
    local end_time=$(date +%s)
    local duration=$((end_time - start_time))
    
    if [ $copy_result -eq 0 ]; then
        log_success "跨网盘复制完成，耗时: ${duration}秒"
        return 0
    else
        log_error "跨网盘复制失败，耗时: ${duration}秒，退出码: $copy_result"
        return 1
    fi
}

# =============================================================================
# 主要测试函数
# =============================================================================

# 主函数
main() {
    echo "=============================================="
    echo "🚀 跨网盘复制测试: 115网盘 → 123网盘"
    echo "=============================================="
    echo
    
    # 创建日志目录
    ensure_dir "$LOG_DIR"
    
    log_info "跨网盘复制测试开始"
    log_info "源目录: 115:$SOURCE_DIR/"
    log_info "目标目录: 123:$TARGET_DIR/"
    log_info "日志目录: $LOG_DIR"
    log_info "并发传输数: $CONCURRENT_TRANSFERS"

    # 步骤0: 编译最新代码
    log_info "步骤0: 编译最新的rclone代码"
    if go build -o rclone_test . 2>&1 | tee "$LOG_DIR/build.log"; then
        log_success "代码编译成功"
    else
        log_error "代码编译失败，请检查编译错误"
        cat "$LOG_DIR/build.log"
        exit 1
    fi

    # 步骤1: 验证网盘连接
    log_info "步骤1: 验证网盘连接"
    
    if ! ./rclone_test about 115: >/dev/null 2>&1; then
        log_error "115网盘连接失败，请检查配置"
        exit 1
    fi
    log_success "115网盘连接正常"
    
    if ! ./rclone_test about 123: >/dev/null 2>&1; then
        log_error "123网盘连接失败，请检查配置"
        exit 1
    fi
    log_success "123网盘连接正常"
    
    # 步骤2: 检查源目录
    log_info "步骤2: 检查源目录"
    if ! ./rclone_test ls "115:$SOURCE_DIR/" >/dev/null 2>&1; then
        log_error "源目录不存在: 115:$SOURCE_DIR/"
        log_info "请先运行 115网盘测试脚本生成测试文件"
        exit 1
    fi
    log_success "源目录存在: 115:$SOURCE_DIR/"
    
    # 获取源目录信息
    local source_info="$LOG_DIR/source_info.json"
    get_directory_info "115:$SOURCE_DIR" "$source_info" || exit 1
    
    # 步骤3: 准备目标目录
    log_info "步骤3: 准备目标目录"
    ./rclone_test mkdir "123:$TARGET_DIR/" 2>/dev/null || true
    log_success "目标目录准备完成: 123:$TARGET_DIR/"
    
    # 步骤4: 执行跨网盘复制
    log_info "步骤4: 执行跨网盘复制"
    if perform_cross_copy; then
        log_success "跨网盘复制成功完成"
    else
        log_error "跨网盘复制失败"
        exit 1
    fi
    
    # 步骤5: 验证文件完整性
    if [ "$VERIFY_INTEGRITY" = "true" ]; then
        log_info "步骤5: 验证文件完整性"
        
        local target_info="$LOG_DIR/target_info.json"
        get_directory_info "123:$TARGET_DIR" "$target_info" || exit 1
        
        if verify_file_integrity "$source_info" "$target_info"; then
            log_success "文件完整性验证通过"
        else
            log_error "文件完整性验证失败"
            exit 1
        fi
    else
        log_info "跳过文件完整性验证 (VERIFY_INTEGRITY=false)"
    fi
    
    # 步骤6: 生成测试报告
    log_info "步骤6: 生成测试报告"
    
    echo "=============================================="
    echo "🎯 跨网盘复制测试结果"
    echo "=============================================="
    echo "✅ 115网盘 → 123网盘 复制成功"
    echo "📝 详细日志: $LOG_DIR/"
    echo "📊 复制日志: $COPY_LOG"
    echo
    
    log_success "跨网盘复制测试完成！"
    exit 0
}

# 快速测试模式
quick_test() {
    echo "🚀 跨网盘复制快速测试模式"
    echo
    
    # 创建日志目录
    ensure_dir "$LOG_DIR"
    
    log_info "快速测试开始 (仅复制小文件)"
    
    # 编译代码
    log_info "编译最新的rclone代码"
    if go build -o rclone_test . 2>&1 | tee "$LOG_DIR/build.log"; then
        log_success "代码编译成功"
    else
        log_error "代码编译失败"
        exit 1
    fi
    
    # 连接验证
    if ! ./rclone_test about 115: >/dev/null 2>&1 || ! ./rclone_test about 123: >/dev/null 2>&1; then
        log_error "网盘连接失败"
        exit 1
    fi
    
    # 快速复制测试（只复制小文件）
    log_info "执行快速复制测试"
    if ./rclone_test copy "115:$SOURCE_DIR/" "123:$TARGET_DIR/" --include "*small*" -vv --log-file "$COPY_LOG"; then
        echo "✅ 快速复制测试通过！"
        exit 0
    else
        echo "❌ 快速复制测试失败！"
        exit 1
    fi
}

# =============================================================================
# 命令行参数处理
# =============================================================================

case "${1:-}" in
    -h|--help)
        cat << EOF
跨网盘复制测试脚本: 115网盘 → 123网盘

用法: $0 [选项]

选项:
  -h, --help          显示此帮助信息
  -q, --quick         快速测试模式 (仅复制小文件)

环境变量配置:
  SOURCE_DIR          115网盘源目录名, 默认: 115_test_unified
  TARGET_DIR          123网盘目标目录名, 默认: 123_test_unified
  CLEAN_TARGET        清理目标目录 (true/false), 默认: false
  VERIFY_INTEGRITY    验证文件完整性 (true/false), 默认: true
  CONCURRENT_TRANSFERS 并发传输数, 默认: 1
  SHOW_PROGRESS       显示进度 (true/false), 默认: true

示例:
  $0                                    # 完整复制测试
  $0 --quick                           # 快速复制测试
  CONCURRENT_TRANSFERS=3 $0             # 3并发复制
  VERIFY_INTEGRITY=false $0             # 跳过完整性验证
  CLEAN_TARGET=true $0                  # 测试后清理目标目录

EOF
        ;;
    -q|--quick)
        quick_test
        ;;
    *)
        # 执行主函数
        main "$@"
        ;;
esac
