#!/bin/bash
# 115网盘Backend标准化测试脚本
# 版本: 1.0
# 用途: 验证115网盘backend的各项修复效果

# =============================================================================
# 配置加载 - 支持配置文件和环境变量
# =============================================================================

# 加载配置文件（如果存在）
CONFIG_FILE=${CONFIG_FILE:-"test_115_config.conf"}
if [ -f "$CONFIG_FILE" ]; then
    echo "📋 加载配置文件: $CONFIG_FILE"
    source "$CONFIG_FILE"
else
    echo "📋 使用默认配置（未找到配置文件: $CONFIG_FILE）"
fi

# 测试文件大小配置 (MB) - 环境变量优先级最高
SMALL_FILE_SIZE=${SMALL_FILE_SIZE:-10}      # 小文件: 10MB (单步上传)
MEDIUM_FILE_SIZE=${MEDIUM_FILE_SIZE:-100}   # 中等文件: 100MB (单步上传)
LARGE_FILE_SIZE=${LARGE_FILE_SIZE:-500}     # 大文件: 500MB (单步上传)
HUGE_FILE_SIZE=${HUGE_FILE_SIZE:-1200}      # 超大文件: 1200MB (分片上传)

# 测试配置
TEST_BASE_DIR=${TEST_BASE_DIR:-"115_standard_test"}
UNIFIED_TEST_DIR=${UNIFIED_TEST_DIR:-"115_test_unified"}  # 统一测试目录
CLEAN_REMOTE=${CLEAN_REMOTE:-false}         # 是否清理远程统一目录
VERBOSE_LEVEL=${VERBOSE_LEVEL:-"normal"}     # quiet, normal, verbose
KEEP_FILES=${KEEP_FILES:-false}             # 是否保留测试文件
CONCURRENT_TRANSFERS=${CONCURRENT_TRANSFERS:-1}
TEST_TIMEOUT=${TEST_TIMEOUT:-1800}          # 测试超时时间

# 日志配置
LOG_DIR="logs_$(date +%Y%m%d_%H%M%S)"
MAIN_LOG="$LOG_DIR/main.log"
SUMMARY_LOG="$LOG_DIR/summary.log"

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
    
    # 清理本地测试文件
    if [ "$KEEP_FILES" != "true" ]; then
        log_info "清理本地测试文件..."
        rm -f test_115_small_*.bin test_115_medium_*.bin test_115_large_*.bin test_115_huge_*.bin 2>/dev/null || true
        log_info "本地测试文件清理完成"
    else
        log_info "保留本地测试文件 (KEEP_FILES=true)"
    fi
    
    # 清理远程统一目录
    if [ "$CLEAN_REMOTE" = "true" ]; then
        log_info "清理远程统一测试目录: 115:$UNIFIED_TEST_DIR/"
        if ./rclone_test ls "115:$UNIFIED_TEST_DIR/" >/dev/null 2>&1; then
            ./rclone_test delete "115:$UNIFIED_TEST_DIR/" 2>/dev/null || true
            log_info "远程统一目录清理完成"
        else
            log_info "远程统一目录不存在，无需清理"
        fi
    else
        log_info "保留远程统一目录 (CLEAN_REMOTE=false)"
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

# 生成防秒传文件
generate_unique_file() {
    local filename="$1"
    local size_mb="$2"
    local timestamp=$(date +%y%m%d_%H%M%S)
    
    log_info "生成防秒传文件: $filename (${size_mb}MB)"
    
    # 使用随机数据确保文件唯一性，防止秒传
    dd if=/dev/urandom of="$filename" bs=1M count="$size_mb" 2>/dev/null
    
    if [ $? -eq 0 ]; then
        log_success "文件生成成功: $filename"
        return 0
    else
        log_error "文件生成失败: $filename"
        return 1
    fi
}

# 文件大小验证
verify_file_size() {
    local filename="$1"
    local expected_size_mb="$2"
    
    if [ ! -f "$filename" ]; then
        log_error "文件不存在: $filename"
        return 1
    fi
    
    local actual_size=$(stat -f%z "$filename" 2>/dev/null || stat -c%s "$filename" 2>/dev/null)
    local expected_size=$((expected_size_mb * 1024 * 1024))
    local size_diff=$((actual_size - expected_size))
    
    # 允许1%的误差
    local tolerance=$((expected_size / 100))
    
    if [ ${size_diff#-} -le $tolerance ]; then
        log_success "文件大小验证通过: $filename ($actual_size bytes)"
        return 0
    else
        log_error "文件大小验证失败: $filename (期望: $expected_size, 实际: $actual_size)"
        return 1
    fi
}

# 上传测试函数
upload_test() {
    local filename="$1"
    local size_mb="$2"
    local log_suffix="$3"
    local test_dir="$4"
    
    local test_log="$LOG_DIR/upload_${log_suffix}.log"
    local start_time=$(date +%s)
    
    log_info "开始上传测试: $filename (${size_mb}MB) -> 115:$test_dir/"
    
    # 执行上传
    ./rclone_test copy "$filename" "115:$test_dir/" \
        --transfers $CONCURRENT_TRANSFERS \
        --checkers 1 \
        -vv \
        --log-file "$test_log" 2>&1 | tee "$LOG_DIR/upload_${log_suffix}_console.log"
    
    local upload_result=$?
    local end_time=$(date +%s)
    local duration=$((end_time - start_time))
    
    # 验证上传结果
    if [ $upload_result -eq 0 ] && ./rclone_test ls "115:$test_dir/$filename" >/dev/null 2>&1; then
        log_success "${log_suffix}文件(${size_mb}MB)上传成功"
        
        # 文件大小验证
        local remote_size=$(./rclone_test size "115:$test_dir/$filename" --json 2>/dev/null | jq -r '.bytes' 2>/dev/null || echo "unknown")
        if [ "$remote_size" != "unknown" ] && [ "$remote_size" -gt 0 ]; then
            local expected_size=$((size_mb * 1024 * 1024))
            if [ "$remote_size" -eq "$expected_size" ]; then
                log_success "文件大小验证通过: $remote_size 字节"
            else
                log_warning "文件大小不匹配: 期望 $expected_size, 实际 $remote_size"
            fi
        fi
        
        echo "$(date '+%Y-%m-%d %H:%M:%S') [SUCCESS] ${log_suffix}文件(${size_mb}MB)上传成功，耗时: ${duration}秒" >> "$SUMMARY_LOG"
        return 0
    else
        log_error "${log_suffix}文件(${size_mb}MB)上传失败"
        echo "$(date '+%Y-%m-%d %H:%M:%S') [ERROR] ${log_suffix}文件(${size_mb}MB)上传失败，耗时: ${duration}秒" >> "$SUMMARY_LOG"
        return 1
    fi
}

# 分析日志函数
analyze_log() {
    local log_file="$1"
    local test_name="$2"
    
    if [ ! -f "$log_file" ]; then
        log_warning "日志文件不存在: $log_file"
        return 1
    fi
    
    log_info "分析 $test_name 测试日志..."
    
    # 基本统计
    local total_lines=$(wc -l < "$log_file")
    local error_lines=$(grep -c "ERROR\|Failed\|失败" "$log_file" 2>/dev/null || echo "0")
    local warning_lines=$(grep -c "WARNING\|WARN" "$log_file" 2>/dev/null || echo "0")
    
    # 错误统计
    local error_count=$(grep -c "ERROR\|Failed\|失败" "$log_file" 2>/dev/null || echo "0")
    # 确保error_count是一个数字
    error_count=$(echo "$error_count" | tr -d ' ' | head -1)
    echo "错误数量: $error_count" >> "$SUMMARY_LOG"
    
    if [ "$error_count" -gt 0 ] 2>/dev/null; then
        echo "主要错误:" >> "$SUMMARY_LOG"
        grep "ERROR\|Failed\|失败" "$log_file" | head -3 >> "$SUMMARY_LOG" 2>/dev/null || true
    fi
    
    # 性能分析
    if grep -q "speed\|MB/s" "$log_file" 2>/dev/null; then
        echo "性能信息:" >> "$SUMMARY_LOG"
        grep "speed\|MB/s" "$log_file" | tail -3 >> "$SUMMARY_LOG" 2>/dev/null || true
    fi
    
    echo "日志统计: 总行数=$total_lines, 错误=$error_lines, 警告=$warning_lines" >> "$SUMMARY_LOG"
    echo "详细日志: $log_file" >> "$SUMMARY_LOG"
    echo "---" >> "$SUMMARY_LOG"
}

# =============================================================================
# 主要测试函数
# =============================================================================

# 主函数
main() {
    echo "=============================================="
    echo "🚀 115网盘Backend标准化测试"
    echo "=============================================="
    echo
    
    # 创建日志目录
    ensure_dir "$LOG_DIR"
    
    log_info "测试开始"
    log_info "统一测试目录: $UNIFIED_TEST_DIR"
    log_info "日志目录: $LOG_DIR"
    log_info "配置: 小文件=${SMALL_FILE_SIZE}MB, 中等=${MEDIUM_FILE_SIZE}MB, 大文件=${LARGE_FILE_SIZE}MB, 超大=${HUGE_FILE_SIZE}MB"

    # 步骤0: 编译最新代码
    log_info "步骤0: 编译最新的rclone代码"
    if go build -o rclone_test . 2>&1 | tee "$LOG_DIR/build.log"; then
        log_success "代码编译成功"
    else
        log_error "代码编译失败，请检查编译错误"
        cat "$LOG_DIR/build.log"
        exit 1
    fi

    # 确保统一测试目录存在
    log_info "确保统一测试目录存在: 115:$UNIFIED_TEST_DIR/"
    ./rclone_test mkdir "115:$UNIFIED_TEST_DIR/" 2>/dev/null || true

    # 第一步：连接验证
    log_info "步骤1: 验证115网盘连接"
    if ! ./rclone_test about 115: >/dev/null 2>&1; then
        log_error "115网盘连接失败，请检查配置"
        echo "请运行以下命令配置115网盘:"
        echo "  ./rclone_test config"
        exit 1
    fi
    log_success "115网盘连接正常"

    # 第二步：生成测试文件
    log_info "步骤2: 生成防秒传测试文件"
    local timestamp=$(date +%y%m%d_%H%M%S)

    local small_file="test_115_small_${SMALL_FILE_SIZE}MB_${timestamp}.bin"
    local medium_file="test_115_medium_${MEDIUM_FILE_SIZE}MB_${timestamp}.bin"
    local large_file="test_115_large_${LARGE_FILE_SIZE}MB_${timestamp}.bin"
    local huge_file="test_115_huge_${HUGE_FILE_SIZE}MB_${timestamp}.bin"

    # 生成测试文件
    generate_unique_file "$small_file" "$SMALL_FILE_SIZE" || exit 1
    generate_unique_file "$medium_file" "$MEDIUM_FILE_SIZE" || exit 1
    generate_unique_file "$large_file" "$LARGE_FILE_SIZE" || exit 1
    generate_unique_file "$huge_file" "$HUGE_FILE_SIZE" || exit 1

    # 验证文件大小
    verify_file_size "$small_file" "$SMALL_FILE_SIZE" || exit 1
    verify_file_size "$medium_file" "$MEDIUM_FILE_SIZE" || exit 1
    verify_file_size "$large_file" "$LARGE_FILE_SIZE" || exit 1
    verify_file_size "$huge_file" "$HUGE_FILE_SIZE" || exit 1

    log_success "所有测试文件生成完成"

    # 第三步：执行上传测试
    log_info "步骤3: 执行上传测试"

    local success_count=0
    local total_tests=4

    # 小文件测试
    if upload_test "$small_file" "$SMALL_FILE_SIZE" "small" "$UNIFIED_TEST_DIR"; then
        ((success_count++))
        analyze_log "$LOG_DIR/upload_small.log" "小文件"
    else
        analyze_log "$LOG_DIR/upload_small.log" "小文件"
    fi

    # 中等文件测试
    if upload_test "$medium_file" "$MEDIUM_FILE_SIZE" "medium" "$UNIFIED_TEST_DIR"; then
        ((success_count++))
        analyze_log "$LOG_DIR/upload_medium.log" "中等文件"
    else
        analyze_log "$LOG_DIR/upload_medium.log" "中等文件"
    fi

    # 大文件测试
    if upload_test "$large_file" "$LARGE_FILE_SIZE" "large" "$UNIFIED_TEST_DIR"; then
        ((success_count++))
        analyze_log "$LOG_DIR/upload_large.log" "大文件"
    else
        analyze_log "$LOG_DIR/upload_large.log" "大文件"
    fi

    # 超大文件测试
    if upload_test "$huge_file" "$HUGE_FILE_SIZE" "huge" "$UNIFIED_TEST_DIR"; then
        ((success_count++))
        analyze_log "$LOG_DIR/upload_huge.log" "超大文件"
    else
        analyze_log "$LOG_DIR/upload_huge.log" "超大文件"
    fi

    # 第四步：生成测试报告
    log_info "步骤4: 生成测试报告"

    echo "=============================================="
    echo "🎯 测试结果总结"
    echo "=============================================="
    echo "总体成功率: $success_count/$total_tests"
    echo "📝 详细日志: $LOG_DIR/"
    echo "📊 测试报告: $SUMMARY_LOG"
    echo

    # 写入总结报告
    {
        echo "=============================================="
        echo "115网盘Backend标准化测试报告"
        echo "=============================================="
        echo "测试时间: $(date)"
        echo "测试配置:"
        echo "  小文件: ${SMALL_FILE_SIZE}MB"
        echo "  中等文件: ${MEDIUM_FILE_SIZE}MB"
        echo "  大文件: ${LARGE_FILE_SIZE}MB"
        echo "  超大文件: ${HUGE_FILE_SIZE}MB"
        echo "  统一测试目录: $UNIFIED_TEST_DIR"
        echo "  并发传输数: $CONCURRENT_TRANSFERS"
        echo
        echo "测试结果:"
        echo "  总体成功率: $success_count/$total_tests"
        echo
    } >> "$SUMMARY_LOG"

    if [ $success_count -eq $total_tests ]; then
        echo "🎉 所有测试通过！115网盘backend工作正常"
        log_success "所有测试通过！115网盘backend工作正常"
        echo "✅ 115网盘backend工作正常" >> "$SUMMARY_LOG"
        exit 0
    else
        echo "⚠️  部分测试失败，请检查日志"
        log_warning "部分测试失败，请检查日志"
        echo "❌ 部分测试失败，需要检查和修复" >> "$SUMMARY_LOG"
        exit 1
    fi
}

# 快速测试模式
quick_test() {
    echo "🚀 115网盘快速测试模式"
    echo

    # 创建日志目录
    ensure_dir "$LOG_DIR"

    log_info "快速测试开始 (仅测试小文件)"

    # 编译代码
    log_info "编译最新的rclone代码"
    if go build -o rclone_test . 2>&1 | tee "$LOG_DIR/build.log"; then
        log_success "代码编译成功"
    else
        log_error "代码编译失败"
        exit 1
    fi

    # 连接验证
    if ! ./rclone_test about 115: >/dev/null 2>&1; then
        log_error "115网盘连接失败"
        exit 1
    fi

    # 确保测试目录存在
    ./rclone_test mkdir "115:$UNIFIED_TEST_DIR/" 2>/dev/null || true

    # 生成小文件
    local timestamp=$(date +%y%m%d_%H%M%S)
    local small_file="test_115_quick_${SMALL_FILE_SIZE}MB_${timestamp}.bin"

    generate_unique_file "$small_file" "$SMALL_FILE_SIZE" || exit 1

    # 上传测试
    if upload_test "$small_file" "$SMALL_FILE_SIZE" "quick" "$UNIFIED_TEST_DIR"; then
        echo "✅ 快速测试通过！"
        exit 0
    else
        echo "❌ 快速测试失败！"
        exit 1
    fi
}

# 性能测试模式
performance_test() {
    echo "🚀 115网盘性能测试模式"
    echo

    # 创建日志目录
    ensure_dir "$LOG_DIR"

    log_info "性能测试开始 (并发上传测试)"

    # 编译代码
    log_info "编译最新的rclone代码"
    if go build -o rclone_test . 2>&1 | tee "$LOG_DIR/build.log"; then
        log_success "代码编译成功"
    else
        log_error "代码编译失败"
        exit 1
    fi

    # 连接验证
    if ! ./rclone_test about 115: >/dev/null 2>&1; then
        log_error "115网盘连接失败"
        exit 1
    fi

    # 确保测试目录存在
    ./rclone_test mkdir "115:$UNIFIED_TEST_DIR/" 2>/dev/null || true

    # 生成多个测试文件
    local timestamp=$(date +%y%m%d_%H%M%S)
    local test_files=()

    for i in {1..3}; do
        local test_file="test_115_perf_${MEDIUM_FILE_SIZE}MB_${timestamp}_${i}.bin"
        generate_unique_file "$test_file" "$MEDIUM_FILE_SIZE" || exit 1
        test_files+=("$test_file")
    done

    # 并发上传测试
    log_info "开始并发上传测试 (并发数: $CONCURRENT_TRANSFERS)"
    local start_time=$(date +%s)

    local success_count=0
    for test_file in "${test_files[@]}"; do
        if upload_test "$test_file" "$MEDIUM_FILE_SIZE" "perf_$(basename $test_file)" "$UNIFIED_TEST_DIR"; then
            ((success_count++))
        fi
    done

    local end_time=$(date +%s)
    local total_duration=$((end_time - start_time))

    echo "性能测试结果:"
    echo "  成功上传: $success_count/${#test_files[@]}"
    echo "  总耗时: ${total_duration}秒"
    echo "  平均速度: $(( (MEDIUM_FILE_SIZE * success_count) / total_duration ))MB/s"

    if [ $success_count -eq ${#test_files[@]} ]; then
        echo "✅ 性能测试通过！"
        exit 0
    else
        echo "❌ 性能测试失败！"
        exit 1
    fi
}

# =============================================================================
# 命令行参数处理
# =============================================================================

case "${1:-}" in
    -h|--help)
        cat << EOF
115网盘Backend标准化测试脚本

用法: $0 [选项]

选项:
  -h, --help          显示此帮助信息
  -q, --quick         快速测试模式 (仅测试小文件)
  -p, --performance   性能测试模式 (并发上传测试)

环境变量配置:
  SMALL_FILE_SIZE     小文件大小(MB), 默认: 10
  MEDIUM_FILE_SIZE    中等文件大小(MB), 默认: 100
  LARGE_FILE_SIZE     大文件大小(MB), 默认: 500
  HUGE_FILE_SIZE      超大文件大小(MB), 默认: 1200
  UNIFIED_TEST_DIR    统一测试目录名, 默认: 115_test_unified
  CLEAN_REMOTE        清理远程目录 (true/false), 默认: false
  VERBOSE_LEVEL       详细程度 (quiet/normal/verbose), 默认: normal
  KEEP_FILES          保留测试文件 (true/false), 默认: false
  CONCURRENT_TRANSFERS 并发传输数, 默认: 1

示例:
  $0                                    # 完整测试
  $0 --quick                           # 快速测试
  $0 --performance                     # 性能测试
  VERBOSE_LEVEL=verbose $0              # 详细模式
  SMALL_FILE_SIZE=50 LARGE_FILE_SIZE=1050 $0  # 自定义文件大小
  KEEP_FILES=true $0                    # 保留测试文件
  UNIFIED_TEST_DIR=my_test_dir $0       # 自定义统一目录
  CLEAN_REMOTE=true $0                  # 测试后清理远程目录

EOF
        ;;
    -q|--quick)
        quick_test
        ;;
    -p|--performance)
        performance_test
        ;;
    *)
        # 执行主函数
        main "$@"
        ;;
esac
