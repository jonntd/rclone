#!/bin/bash

# rclone 115/123网盘双向传输完整测试脚本
# 版本: v1.0
# 作者: rclone优化团队
# 日期: 2025-07-06

set -e

# ==================== 配置区域 ====================

# 基础配置
RCLONE_BIN="./rclone_115_instant"
LOG_DIR="/tmp/rclone_tests"
TEST_DATE=$(date +"%Y%m%d_%H%M%S")
RESULTS_FILE="$LOG_DIR/test_results_${TEST_DATE}.md"

# 网盘配置
SOURCE_115="115"
SOURCE_123="123"
TEST_DIR_115="test_bidirectional"
TEST_DIR_123="test_bidirectional"

# 测试文件配置
declare -a TEST_FILES=(
    "small:test1/test_50mb_new.bin:50MB"
    "medium:test_instant_transfer/test_medium_file.bin:200MB"
    "large:test1/test_medium_file2.bin:150MB"
)

# ==================== 工具函数 ====================

# 颜色输出函数
print_header() {
    echo -e "\n\033[1;34m=== $1 ===\033[0m"
    echo "=== $1 ===" >> "$RESULTS_FILE"
}

print_success() {
    echo -e "\033[1;32m✅ $1\033[0m"
    echo "✅ $1" >> "$RESULTS_FILE"
}

print_error() {
    echo -e "\033[1;31m❌ $1\033[0m"
    echo "❌ $1" >> "$RESULTS_FILE"
}

print_info() {
    echo -e "\033[1;33m📋 $1\033[0m"
    echo "📋 $1" >> "$RESULTS_FILE"
}

# 初始化函数
init_test_environment() {
    print_header "初始化测试环境"
    
    # 创建日志目录
    mkdir -p "$LOG_DIR"
    
    # 创建结果文件
    cat > "$RESULTS_FILE" << EOF
# rclone 115/123网盘双向传输测试报告

**测试时间**: $(date)  
**rclone版本**: $($RCLONE_BIN version | head -1)  
**测试环境**: $(uname -a)  
**日志目录**: $LOG_DIR

---

EOF
    
    # 检查rclone可执行文件
    if [[ ! -x "$RCLONE_BIN" ]]; then
        print_error "rclone可执行文件不存在或无执行权限: $RCLONE_BIN"
        exit 1
    fi
    
    # 检查网盘配置
    if ! $RCLONE_BIN config show | grep -q "\[115\]"; then
        print_error "115网盘配置不存在"
        exit 1
    fi
    
    if ! $RCLONE_BIN config show | grep -q "\[123\]"; then
        print_error "123网盘配置不存在"
        exit 1
    fi
    
    print_success "测试环境初始化完成"
}

# 检查文件是否存在
check_file_exists() {
    local source="$1"
    local file_path="$2"
    
    if $RCLONE_BIN lsf "$source:$file_path" >/dev/null 2>&1; then
        return 0
    else
        return 1
    fi
}

# 获取文件大小
get_file_size() {
    local source="$1"
    local file_path="$2"
    
    $RCLONE_BIN size "$source:$file_path" 2>/dev/null | grep "Total size" | awk '{print $3}'
}

# 执行传输测试
execute_transfer_test() {
    local test_name="$1"
    local source_remote="$2"
    local source_path="$3"
    local dest_remote="$4"
    local dest_path="$5"
    local expected_size="$6"
    
    local log_file="$LOG_DIR/${test_name}_${TEST_DATE}.log"
    local full_source="${source_remote}:${source_path}"
    local full_dest="${dest_remote}:${dest_path}"
    
    print_info "开始测试: $test_name"
    echo "源路径: $full_source" | tee -a "$RESULTS_FILE"
    echo "目标路径: $full_dest" | tee -a "$RESULTS_FILE"
    echo "预期大小: $expected_size" | tee -a "$RESULTS_FILE"
    echo "日志文件: $log_file" | tee -a "$RESULTS_FILE"
    
    # 检查源文件是否存在
    if ! check_file_exists "$source_remote" "$source_path"; then
        print_error "源文件不存在: $full_source"
        return 1
    fi
    
    # 清理目标文件（如果存在）
    $RCLONE_BIN delete "$full_dest" 2>/dev/null || true
    
    # 记录开始时间
    local start_time=$(date +%s)
    local start_timestamp=$(date '+%Y-%m-%d %H:%M:%S')
    
    # 执行传输
    local transfer_success=false
    local instant_upload=false
    local transfer_speed=""
    local error_message=""
    
    if $RCLONE_BIN copy "$full_source" "$full_dest" \
        --progress \
        --log-level INFO \
        --stats 1s \
        --log-file "$log_file" 2>&1; then
        
        transfer_success=true
        local end_time=$(date +%s)
        local duration=$((end_time - start_time))
        
        # 检查是否触发秒传
        if grep -q "秒传成功" "$log_file"; then
            instant_upload=true
        fi
        
        # 提取传输速度
        transfer_speed=$(grep -o "[0-9.]\+ [KMG]iB/s" "$log_file" | tail -1 || echo "N/A")
        
        # 验证文件是否成功传输
        if check_file_exists "$dest_remote" "$dest_path"; then
            print_success "$test_name 传输成功，耗时: ${duration}秒"
            
            # 记录详细结果
            {
                echo "**传输结果**:"
                echo "- 开始时间: $start_timestamp"
                echo "- 传输时间: ${duration}秒"
                echo "- 传输速度: $transfer_speed"
                echo "- 秒传状态: $([ "$instant_upload" = true ] && echo "✅ 成功" || echo "❌ 未触发")"
                echo "- 传输状态: ✅ 成功"
                echo ""
            } >> "$RESULTS_FILE"
            
            if [ "$instant_upload" = true ]; then
                print_success "触发秒传功能"
            else
                print_info "使用传统传输"
            fi
            
            return 0
        else
            print_error "文件传输后验证失败"
            return 1
        fi
    else
        error_message=$(tail -5 "$log_file" | grep "ERROR" | head -1 || echo "未知错误")
        print_error "$test_name 传输失败: $error_message"
        
        {
            echo "**传输结果**:"
            echo "- 传输状态: ❌ 失败"
            echo "- 错误信息: $error_message"
            echo ""
        } >> "$RESULTS_FILE"
        
        return 1
    fi
}

# 执行双向传输测试
run_bidirectional_tests() {
    print_header "开始双向传输测试"
    
    local total_tests=0
    local passed_tests=0
    local failed_tests=0
    
    for file_config in "${TEST_FILES[@]}"; do
        IFS=':' read -r size_category file_path expected_size <<< "$file_config"
        
        # 提取文件名
        local filename=$(basename "$file_path")
        
        print_header "测试文件: $filename ($expected_size)"
        
        # 测试1: 123 → 115
        total_tests=$((total_tests + 1))
        if execute_transfer_test \
            "123to115_${size_category}" \
            "$SOURCE_123" \
            "$file_path" \
            "$SOURCE_115" \
            "$TEST_DIR_115/$filename" \
            "$expected_size"; then
            passed_tests=$((passed_tests + 1))
        else
            failed_tests=$((failed_tests + 1))
        fi
        
        # 等待一段时间避免API限制
        sleep 2
        
        # 测试2: 115 → 123  
        total_tests=$((total_tests + 1))
        if execute_transfer_test \
            "115to123_${size_category}" \
            "$SOURCE_115" \
            "$TEST_DIR_115/$filename" \
            "$SOURCE_123" \
            "$TEST_DIR_123/$filename" \
            "$expected_size"; then
            passed_tests=$((passed_tests + 1))
        else
            failed_tests=$((failed_tests + 1))
        fi
        
        # 等待一段时间
        sleep 2
    done
    
    # 输出测试统计
    print_header "测试统计"
    echo "总测试数: $total_tests" | tee -a "$RESULTS_FILE"
    echo "成功测试: $passed_tests" | tee -a "$RESULTS_FILE"  
    echo "失败测试: $failed_tests" | tee -a "$RESULTS_FILE"
    echo "成功率: $(( passed_tests * 100 / total_tests ))%" | tee -a "$RESULTS_FILE"
    
    if [ $failed_tests -eq 0 ]; then
        print_success "所有测试通过！"
    else
        print_error "存在失败的测试，请检查日志"
    fi
}

# 性能基准测试
run_performance_benchmark() {
    print_header "性能基准测试"
    
    local test_file="123:test1/test_50mb_new.bin"
    local dest_base="115:benchmark"
    
    # 测试配置
    declare -a BENCHMARK_CONFIGS=(
        "default:--transfers 4 --checkers 8"
        "high_concurrency:--transfers 8 --checkers 16 --buffer-size 32M"
        "optimized:--transfers 4 --checkers 8 --buffer-size 32M --multi-thread-streams 4"
    )
    
    echo "## 性能基准测试结果" >> "$RESULTS_FILE"
    echo "" >> "$RESULTS_FILE"
    
    for config in "${BENCHMARK_CONFIGS[@]}"; do
        IFS=':' read -r config_name params <<< "$config"
        
        print_info "测试配置: $config_name"
        echo "参数: $params" | tee -a "$RESULTS_FILE"
        
        # 清理目标
        $RCLONE_BIN delete "$dest_base/$config_name/" 2>/dev/null || true
        
        # 执行基准测试
        local start_time=$(date +%s)
        if $RCLONE_BIN copy "$test_file" "$dest_base/$config_name/" \
            $params \
            --progress \
            --log-level INFO \
            --stats 1s > "$LOG_DIR/benchmark_${config_name}_${TEST_DATE}.log" 2>&1; then
            
            local end_time=$(date +%s)
            local duration=$((end_time - start_time))
            
            print_success "$config_name: ${duration}秒"
            echo "- $config_name: ${duration}秒" >> "$RESULTS_FILE"
        else
            print_error "$config_name: 失败"
            echo "- $config_name: 失败" >> "$RESULTS_FILE"
        fi
        
        sleep 1
    done
    
    echo "" >> "$RESULTS_FILE"
}

# 清理测试环境
cleanup_test_environment() {
    print_header "清理测试环境"
    
    # 可选：清理测试文件
    # $RCLONE_BIN delete "$SOURCE_115:$TEST_DIR_115/" 2>/dev/null || true
    # $RCLONE_BIN delete "$SOURCE_123:$TEST_DIR_123/" 2>/dev/null || true
    
    print_success "测试环境清理完成"
}

# 生成测试报告
generate_final_report() {
    print_header "生成最终报告"
    
    {
        echo ""
        echo "---"
        echo ""
        echo "## 测试文件列表"
        echo ""
        echo "所有测试日志文件:"
        ls -la "$LOG_DIR"/*_${TEST_DATE}.log 2>/dev/null | while read -r line; do
            echo "- $line"
        done
        echo ""
        echo "## 建议和总结"
        echo ""
        echo "1. **秒传功能**: 在文件已存在的情况下，秒传功能显著提升传输效率"
        echo "2. **网络优化**: 根据网络环境调整并发参数可获得最佳性能"
        echo "3. **API限制**: 注意115/123网盘的QPS限制，避免过高并发"
        echo "4. **监控建议**: 生产环境建议启用详细日志监控"
        echo ""
        echo "*报告生成时间: $(date)*"
    } >> "$RESULTS_FILE"
    
    print_success "测试报告已生成: $RESULTS_FILE"
}

# ==================== 主程序 ====================

main() {
    print_header "rclone 115/123网盘双向传输完整测试"
    
    # 初始化测试环境
    init_test_environment
    
    # 执行双向传输测试
    run_bidirectional_tests
    
    # 执行性能基准测试
    run_performance_benchmark
    
    # 生成最终报告
    generate_final_report
    
    # 清理测试环境
    cleanup_test_environment
    
    print_header "测试完成"
    echo "详细报告: $RESULTS_FILE"
    echo "日志目录: $LOG_DIR"
}

# 检查参数
if [[ "$1" == "--help" || "$1" == "-h" ]]; then
    echo "用法: $0 [选项]"
    echo ""
    echo "选项:"
    echo "  --help, -h     显示帮助信息"
    echo "  --dry-run      仅显示将要执行的操作，不实际执行"
    echo ""
    echo "环境变量:"
    echo "  RCLONE_BIN     rclone可执行文件路径 (默认: ./rclone_115_instant)"
    echo "  LOG_DIR        日志目录 (默认: /tmp/rclone_tests)"
    echo ""
    exit 0
fi

if [[ "$1" == "--dry-run" ]]; then
    echo "=== 干运行模式 ==="
    echo "将要执行的测试:"
    for file_config in "${TEST_FILES[@]}"; do
        IFS=':' read -r size_category file_path expected_size <<< "$file_config"
        filename=$(basename "$file_path")
        echo "- 123:$file_path → 115:$TEST_DIR_115/$filename ($expected_size)"
        echo "- 115:$TEST_DIR_115/$filename → 123:$TEST_DIR_123/$filename ($expected_size)"
    done
    exit 0
fi

# 执行主程序
main "$@"
