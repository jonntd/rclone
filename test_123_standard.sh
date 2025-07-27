#!/bin/bash
# 123网盘Backend标准化测试脚本
# 版本: 1.0
# 用途: 验证123网盘backend的各项修复效果

# =============================================================================
# 配置加载 - 支持配置文件和环境变量
# =============================================================================

# 加载配置文件（如果存在）
CONFIG_FILE=${CONFIG_FILE:-"test_123_config.conf"}
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
TEST_BASE_DIR=${TEST_BASE_DIR:-"123_standard_test"}
UNIFIED_TEST_DIR=${UNIFIED_TEST_DIR:-"123_test_unified"}  # 统一测试目录
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

log_debug() {
    if [ "$VERBOSE_LEVEL" = "verbose" ]; then
        echo "$(date '+%Y-%m-%d %H:%M:%S') [DEBUG] $1" | tee -a "$MAIN_LOG"
    fi
}

# 进度显示函数
show_progress() {
    local current=$1
    local total=$2
    local desc=$3
    local percent=$((current * 100 / total))
    printf "\r🔄 进度: [%-20s] %d%% %s" $(printf "%0.s█" $(seq 1 $((percent/5)))) $percent "$desc"
    if [ $current -eq $total ]; then
        echo ""
    fi
}

# 文件大小验证函数
verify_file_size() {
    local local_file=$1
    local remote_path=$2
    
    local local_size=$(stat -f%z "$local_file" 2>/dev/null || stat -c%s "$local_file" 2>/dev/null)
    local remote_size=$(./rclone_test size "$remote_path" --json 2>/dev/null | jq -r '.bytes' 2>/dev/null || echo "unknown")
    
    if [ "$remote_size" = "$local_size" ]; then
        log_success "文件大小验证通过: $local_size 字节"
        return 0
    else
        log_error "文件大小不匹配: 本地=$local_size, 远程=$remote_size"
        return 1
    fi
}

# 创建测试文件函数 - 确保每次MD5不同，防止秒传
create_test_file() {
    local size_mb=$1
    local filename=$2
    local description=$3

    log_info "创建${description}测试文件: ${filename} (${size_mb}MB) - 防秒传设计"

    # 生成唯一标识符，确保每次文件内容都不同
    local unique_timestamp=$(date +%s%N)
    local unique_random=$(openssl rand -hex 32)
    local unique_pid=$$
    local unique_hostname=$(hostname)

    # 创建基础随机数据文件（减少1KB为元数据预留空间）
    local data_size=$((size_mb * 1024 - 1))
    dd if=/dev/urandom of="${filename}.tmp" bs=1K count=$data_size 2>/dev/null

    # 添加唯一标识信息到文件开头，确保MD5每次都不同
    cat > "$filename" << EOF
=== 123网盘防秒传测试文件 ===
文件类型: $description
文件大小: ${size_mb}MB
创建时间: $(date '+%Y-%m-%d %H:%M:%S.%N')
测试ID: $unique_timestamp
随机标识: $unique_random
进程ID: $unique_pid
主机名: $unique_hostname
脚本版本: 1.0
防秒传设计: 每次生成唯一内容
UUID: $(uuidgen 2>/dev/null || echo "uuid-$unique_timestamp-$unique_random")
额外随机数据: $(openssl rand -hex 64)
=== 文件内容开始 ===
EOF

    # 追加随机数据
    cat "${filename}.tmp" >> "$filename"

    # 在文件末尾添加更多唯一信息
    cat >> "$filename" << EOF

=== 文件内容结束 ===
结束时间: $(date '+%Y-%m-%d %H:%M:%S.%N')
文件校验: $(openssl rand -hex 16)
防秒传验证: UNIQUE_CONTENT_$(date +%s%N)
EOF

    # 清理临时文件
    rm -f "${filename}.tmp"

    # 计算并显示文件MD5（用于验证唯一性）
    local file_md5=$(md5sum "$filename" 2>/dev/null | cut -d' ' -f1 || md5 "$filename" 2>/dev/null | cut -d' ' -f4)

    log_success "文件创建完成: $(ls -lh "$filename" | awk '{print $5}'), MD5: ${file_md5:0:8}..."
    log_debug "完整MD5: $file_md5"
}

# 上传测试函数
upload_test() {
    local filename=$1
    local description=$2
    local test_dir=$3
    local log_suffix=$4
    
    local test_log="$LOG_DIR/upload_${log_suffix}.log"
    
    log_info "开始${description}上传测试: $filename"
    
    # 执行上传
    ./rclone copy "$filename" "123:$test_dir/" \
        --transfers $CONCURRENT_TRANSFERS \
        --checkers 1 \
        -vv \
        --log-file "$test_log" 2>&1 | tee "$LOG_DIR/upload_${log_suffix}_console.log"
    
    local upload_result=$?
    
    # 验证上传结果
    if [ $upload_result -eq 0 ] && ./rclone ls "123:$test_dir/$filename" >/dev/null 2>&1; then
        log_success "${description}上传成功"
        verify_file_size "$filename" "123:$test_dir/$filename"
        return 0
    else
        log_error "${description}上传失败"
        return 1
    fi
}

# 分析日志函数
analyze_upload_log() {
    local log_file=$1
    local description=$2
    
    echo "=== ${description}上传分析 ===" >> "$SUMMARY_LOG"
    
    # 检查.partial机制
    if grep -q "\.partial" "$log_file"; then
        echo "❌ 使用了.partial后缀" >> "$SUMMARY_LOG"
        grep "\.partial" "$log_file" | head -2 >> "$SUMMARY_LOG"
    else
        echo "✅ 没有使用.partial后缀" >> "$SUMMARY_LOG"
    fi
    
    # 检查上传策略
    if grep -q "统一上传策略.*单步上传" "$log_file"; then
        echo "✅ 选择了单步上传策略" >> "$SUMMARY_LOG"
        grep "统一上传策略.*单步上传" "$log_file" | head -1 >> "$SUMMARY_LOG"
    elif grep -q "统一上传策略.*分片上传" "$log_file"; then
        echo "✅ 选择了分片上传策略" >> "$SUMMARY_LOG"
        grep "统一上传策略.*分片上传" "$log_file" | head -1 >> "$SUMMARY_LOG"
    else
        echo "⚠️ 策略选择不明确" >> "$SUMMARY_LOG"
    fi
    
    # 检查API接口使用
    if grep -q "/upload/v2/file/single/create" "$log_file"; then
        echo "✅ 使用了单步上传接口" >> "$SUMMARY_LOG"
    elif grep -q "/upload/v2/file/slice" "$log_file"; then
        echo "✅ 使用了分片上传接口" >> "$SUMMARY_LOG"
    fi
    
    # 检查回收站问题
    if grep -q "回收站\|删除\|移动到回收站" "$log_file"; then
        echo "❌ 文件被移到回收站" >> "$SUMMARY_LOG"
        grep "回收站\|删除\|移动到回收站" "$log_file" | head -2 >> "$SUMMARY_LOG"
    else
        echo "✅ 文件没有被移到回收站" >> "$SUMMARY_LOG"
    fi

    # 检查秒传情况
    if grep -q "秒传\|instant.*upload\|文件已存在.*跳过" "$log_file"; then
        echo "⚠️ 检测到可能的秒传行为" >> "$SUMMARY_LOG"
        grep "秒传\|instant.*upload\|文件已存在.*跳过" "$log_file" | head -2 >> "$SUMMARY_LOG"
    else
        echo "✅ 没有检测到秒传，文件内容唯一" >> "$SUMMARY_LOG"
    fi
    
    # 错误统计
    local error_count=$(grep -c "ERROR\|Failed\|失败" "$log_file" 2>/dev/null || echo "0")
    echo "错误数量: $error_count" >> "$SUMMARY_LOG"
    
    if [ $error_count -gt 0 ]; then
        echo "主要错误:" >> "$SUMMARY_LOG"
        grep "ERROR\|Failed\|失败" "$log_file" | head -3 >> "$SUMMARY_LOG"
    fi
    
    echo "" >> "$SUMMARY_LOG"
}

# 清理函数
cleanup() {
    log_info "开始清理测试环境..."
    
    if [ "$KEEP_FILES" = false ]; then
        # 清理本地测试文件（包括带大小信息的文件名）
        rm -f test_123_*.bin test_123_*MB.bin 2>/dev/null
        log_debug "已清理本地测试文件"
        
        # 可选的远程目录清理
        if [ "$CLEAN_REMOTE" = true ]; then
            ./rclone delete "123:$TEST_DIR/" 2>/dev/null || true
            log_debug "已清理远程统一测试目录: 123:$TEST_DIR/"
        else
            log_debug "保留远程统一测试目录: 123:$TEST_DIR/ (设置CLEAN_REMOTE=true可清理)"
        fi
    else
        log_info "保留测试文件 (KEEP_FILES=true)"
    fi
    
    log_success "清理完成"
}

# =============================================================================
# 主测试流程
# =============================================================================

main() {
    # 初始化
    echo "🔧 123网盘Backend标准化测试"
    echo "=" * 60
    
    # 创建日志目录
    mkdir -p "$LOG_DIR"
    
    # 使用统一的测试目录
    TEST_DIR="$UNIFIED_TEST_DIR"

    log_info "测试开始"
    log_info "统一测试目录: $TEST_DIR"
    log_info "日志目录: $LOG_DIR"
    log_info "配置: 小文件=${SMALL_FILE_SIZE}MB, 中等=${MEDIUM_FILE_SIZE}MB, 大文件=${LARGE_FILE_SIZE}MB, 超大=${HUGE_FILE_SIZE}MB"

    # 确保统一测试目录存在
    log_info "确保统一测试目录存在: 123:$TEST_DIR/"
    ./rclone mkdir "123:$TEST_DIR/" 2>/dev/null || true
    
    # 设置错误处理
    set -e
    trap cleanup EXIT
    
    # 第一步：连接验证
    log_info "步骤1: 验证123网盘连接"
    if ! ./rclone about 123: >/dev/null 2>&1; then
        log_error "123网盘连接失败"
        exit 1
    fi
    log_success "123网盘连接正常"
    
    # 第二步：创建测试文件（防秒传设计）
    log_info "步骤2: 创建测试文件 - 每个文件MD5唯一，防止秒传影响测试"
    local timestamp=$(date +%H%M%S)

    # 定义文件名变量，确保创建和上传使用相同的文件名
    local small_file="test_123_small_${SMALL_FILE_SIZE}MB_${timestamp}.bin"
    local medium_file="test_123_medium_${MEDIUM_FILE_SIZE}MB_${timestamp}.bin"
    local large_file="test_123_large_${LARGE_FILE_SIZE}MB_${timestamp}.bin"
    local huge_file="test_123_huge_${HUGE_FILE_SIZE}MB_${timestamp}.bin"

    create_test_file $SMALL_FILE_SIZE "$small_file" "小文件"
    sleep 1  # 确保时间戳不同
    create_test_file $MEDIUM_FILE_SIZE "$medium_file" "中等文件"
    sleep 1
    create_test_file $LARGE_FILE_SIZE "$large_file" "大文件"
    sleep 1
    create_test_file $HUGE_FILE_SIZE "$huge_file" "超大文件"
    
    # 第三步：执行上传测试
    log_info "步骤3: 执行上传测试"
    
    local success_count=0
    local total_count=4
    
    # 小文件测试
    show_progress 1 4 "小文件(${SMALL_FILE_SIZE}MB)上传测试"
    if upload_test "$small_file" "小文件(${SMALL_FILE_SIZE}MB)" "$TEST_DIR" "small"; then
        ((success_count++))
    fi
    analyze_upload_log "$LOG_DIR/upload_small.log" "小文件(${SMALL_FILE_SIZE}MB)"

    # 中等文件测试
    show_progress 2 4 "中等文件(${MEDIUM_FILE_SIZE}MB)上传测试"
    if upload_test "$medium_file" "中等文件(${MEDIUM_FILE_SIZE}MB)" "$TEST_DIR" "medium"; then
        ((success_count++))
    fi
    analyze_upload_log "$LOG_DIR/upload_medium.log" "中等文件(${MEDIUM_FILE_SIZE}MB)"

    # 大文件测试
    show_progress 3 4 "大文件(${LARGE_FILE_SIZE}MB)上传测试"
    if upload_test "$large_file" "大文件(${LARGE_FILE_SIZE}MB)" "$TEST_DIR" "large"; then
        ((success_count++))
    fi
    analyze_upload_log "$LOG_DIR/upload_large.log" "大文件(${LARGE_FILE_SIZE}MB)"

    # 超大文件测试
    show_progress 4 4 "超大文件(${HUGE_FILE_SIZE}MB)上传测试"
    if upload_test "$huge_file" "超大文件(${HUGE_FILE_SIZE}MB)" "$TEST_DIR" "huge"; then
        ((success_count++))
    fi
    analyze_upload_log "$LOG_DIR/upload_huge.log" "超大文件(${HUGE_FILE_SIZE}MB)"
    
    # 第四步：生成测试报告
    log_info "步骤4: 生成测试报告"
    
    echo "🎯 测试结果总结" | tee -a "$SUMMARY_LOG"
    echo "总体成功率: $success_count/$total_count" | tee -a "$SUMMARY_LOG"
    
    if [ $success_count -eq $total_count ]; then
        echo "🎉 所有测试通过！123网盘backend工作正常" | tee -a "$SUMMARY_LOG"
    elif [ $success_count -gt $((total_count/2)) ]; then
        echo "👍 大部分测试通过，部分功能需要优化" | tee -a "$SUMMARY_LOG"
    else
        echo "❌ 多项测试失败，需要进一步修复" | tee -a "$SUMMARY_LOG"
    fi
    
    echo "" | tee -a "$SUMMARY_LOG"
    echo "📝 详细日志: $LOG_DIR/" | tee -a "$SUMMARY_LOG"
    echo "📊 测试报告: $SUMMARY_LOG" | tee -a "$SUMMARY_LOG"
    
    log_success "测试完成"
}

# =============================================================================
# 脚本入口
# =============================================================================

# 显示帮助信息
if [ "$1" = "--help" ] || [ "$1" = "-h" ]; then
    cat << EOF
123网盘Backend标准化测试脚本

用法: $0 [选项]

环境变量配置:
  SMALL_FILE_SIZE     小文件大小(MB), 默认: 10
  MEDIUM_FILE_SIZE    中等文件大小(MB), 默认: 100  
  LARGE_FILE_SIZE     大文件大小(MB), 默认: 500
  HUGE_FILE_SIZE      超大文件大小(MB), 默认: 1200
  TEST_BASE_DIR       测试目录前缀, 默认: 123_standard_test
  VERBOSE_LEVEL       详细程度 (quiet/normal/verbose), 默认: normal
  KEEP_FILES          保留测试文件 (true/false), 默认: false
  CONCURRENT_TRANSFERS 并发传输数, 默认: 1

示例:
  $0                                    # 使用默认配置
  VERBOSE_LEVEL=verbose $0              # 详细模式
  SMALL_FILE_SIZE=50 LARGE_FILE_SIZE=1000 $0  # 自定义文件大小
  KEEP_FILES=true $0                    # 保留测试文件

EOF
    exit 0
fi

# 快速测试模式
quick_test() {
    echo "🚀 123网盘Backend快速测试模式（防秒传）"
    echo "=" * 40

    mkdir -p "$LOG_DIR"
    TEST_DIR="$UNIFIED_TEST_DIR"

    log_info "快速测试开始 - 使用防秒传设计"
    log_info "使用统一测试目录: $TEST_DIR"

    # 确保统一测试目录存在
    ./rclone mkdir "123:$TEST_DIR/" 2>/dev/null || true

    # 只测试小文件，确保每次MD5不同
    local quick_size=10
    create_test_file $quick_size "test_123_quick_${quick_size}MB.bin" "快速测试"

    if upload_test "test_123_quick_${quick_size}MB.bin" "快速测试(${quick_size}MB)" "$TEST_DIR" "quick"; then
        echo "🎉 快速测试通过！"
        analyze_upload_log "$LOG_DIR/upload_quick.log" "快速测试"
    else
        echo "❌ 快速测试失败！"
    fi

    cleanup
}

# 性能测试模式
performance_test() {
    echo "⚡ 123网盘Backend性能测试模式（防秒传）"
    echo "=" * 40

    mkdir -p "$LOG_DIR"
    TEST_DIR="$UNIFIED_TEST_DIR"

    log_info "性能测试开始 - 使用防秒传设计"
    log_info "使用统一测试目录: $TEST_DIR"

    # 确保统一测试目录存在
    ./rclone mkdir "123:$TEST_DIR/" 2>/dev/null || true

    # 创建多个文件进行并发测试，每个文件MD5都不同
    local perf_size=50
    for i in {1..3}; do
        create_test_file $perf_size "test_123_perf_${i}_${perf_size}MB.bin" "性能测试${i}"
        # 添加额外延迟确保时间戳不同
        sleep 1
    done

    # 并发上传测试
    CONCURRENT_TRANSFERS=3
    local start_time=$(date +%s)

    log_info "开始并发上传测试（3个文件，每个${perf_size}MB，MD5各不相同）"

    for i in {1..3}; do
        upload_test "test_123_perf_${i}_${perf_size}MB.bin" "性能测试${i}(${perf_size}MB)" "$TEST_DIR" "perf_${i}" &
    done
    wait

    local end_time=$(date +%s)
    local duration=$((end_time - start_time))

    echo "🎯 性能测试结果: 3个${perf_size}MB文件（防秒传）并发上传耗时 ${duration}秒"

    cleanup
}

# 解析命令行参数
case "$1" in
    "--quick"|"-q")
        quick_test
        ;;
    "--performance"|"-p")
        performance_test
        ;;
    "--help"|"-h")
        cat << EOF
123网盘Backend标准化测试脚本

用法: $0 [选项]

选项:
  -h, --help          显示帮助信息
  -q, --quick         快速测试模式 (仅测试小文件)
  -p, --performance   性能测试模式 (并发上传测试)

环境变量配置:
  SMALL_FILE_SIZE     小文件大小(MB), 默认: 10
  MEDIUM_FILE_SIZE    中等文件大小(MB), 默认: 100
  LARGE_FILE_SIZE     大文件大小(MB), 默认: 500
  HUGE_FILE_SIZE      超大文件大小(MB), 默认: 1200
  UNIFIED_TEST_DIR    统一测试目录名, 默认: 123_test_unified
  CLEAN_REMOTE        清理远程目录 (true/false), 默认: false
  VERBOSE_LEVEL       详细程度 (quiet/normal/verbose), 默认: normal
  KEEP_FILES          保留测试文件 (true/false), 默认: false
  CONCURRENT_TRANSFERS 并发传输数, 默认: 1

示例:
  $0                                    # 完整测试
  $0 --quick                           # 快速测试
  $0 --performance                     # 性能测试
  VERBOSE_LEVEL=verbose $0              # 详细模式
  SMALL_FILE_SIZE=50 LARGE_FILE_SIZE=1000 $0  # 自定义文件大小
  KEEP_FILES=true $0                    # 保留测试文件
  UNIFIED_TEST_DIR=my_test_dir $0       # 自定义统一目录
  CLEAN_REMOTE=true $0                  # 测试后清理远程目录

EOF
        ;;
    *)
        # 执行主函数
        main "$@"
        ;;
esac
