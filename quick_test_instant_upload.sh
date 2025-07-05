#!/bin/bash

# rclone 115/123网盘秒传功能快速测试脚本
# 版本: v1.0
# 用途: 快速验证双向秒传功能是否正常工作

set -e

# 配置
RCLONE_BIN="./rclone_115_instant"
TEST_FILE_50MB="test_50mb_new.bin"
TEST_FILE_200MB="test_medium_file.bin"

# 颜色输出
RED='\033[0;31m'
GREEN='\033[0;32m'
BLUE='\033[0;34m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

print_header() {
    echo -e "\n${BLUE}=== $1 ===${NC}"
}

print_success() {
    echo -e "${GREEN}✅ $1${NC}"
}

print_error() {
    echo -e "${RED}❌ $1${NC}"
}

print_info() {
    echo -e "${YELLOW}📋 $1${NC}"
}

# 检查rclone是否可用
check_rclone() {
    if [[ ! -x "$RCLONE_BIN" ]]; then
        print_error "rclone可执行文件不存在: $RCLONE_BIN"
        exit 1
    fi
    
    print_success "rclone可执行文件检查通过"
}

# 检查网盘配置
check_configs() {
    if ! $RCLONE_BIN config show | grep -q "\[115\]"; then
        print_error "115网盘配置不存在"
        exit 1
    fi
    
    if ! $RCLONE_BIN config show | grep -q "\[123\]"; then
        print_error "123网盘配置不存在"
        exit 1
    fi
    
    print_success "网盘配置检查通过"
}

# 快速测试函数
quick_test() {
    local test_name="$1"
    local source="$2"
    local dest="$3"
    local expected_result="$4"
    
    print_info "测试: $test_name"
    echo "命令: $RCLONE_BIN copy \"$source\" \"$dest\""
    
    local start_time=$(date +%s)
    local temp_log="/tmp/quick_test_$$.log"
    
    if timeout 60s $RCLONE_BIN copy "$source" "$dest" \
        --progress \
        --log-level INFO \
        --stats 1s > "$temp_log" 2>&1; then
        
        local end_time=$(date +%s)
        local duration=$((end_time - start_time))
        
        # 检查是否触发秒传
        if grep -q "秒传成功" "$temp_log"; then
            print_success "$test_name - 秒传成功 (${duration}秒)"
            if [[ "$expected_result" == "instant" ]]; then
                echo "  ✅ 符合预期：触发秒传"
            else
                echo "  ⚠️  意外：预期传统传输但触发了秒传"
            fi
        else
            print_info "$test_name - 传统传输 (${duration}秒)"
            if [[ "$expected_result" == "normal" ]]; then
                echo "  ✅ 符合预期：传统传输"
            else
                echo "  ⚠️  意外：预期秒传但使用了传统传输"
            fi
        fi
        
        # 显示传输速度
        local speed=$(grep -o "[0-9.]\+ [KMG]iB/s" "$temp_log" | tail -1 || echo "N/A")
        echo "  📊 传输速度: $speed"
        
        rm -f "$temp_log"
        return 0
    else
        print_error "$test_name - 传输失败"
        echo "错误日志:"
        tail -5 "$temp_log" | sed 's/^/  /'
        rm -f "$temp_log"
        return 1
    fi
}

# 主测试流程
main() {
    print_header "rclone 115/123网盘秒传功能快速测试"
    
    # 环境检查
    print_header "环境检查"
    check_rclone
    check_configs
    
    # 显示版本信息
    print_info "rclone版本: $($RCLONE_BIN version | head -1)"
    print_info "测试时间: $(date)"
    
    # 测试计数器
    local total_tests=0
    local passed_tests=0
    
    print_header "开始快速测试"
    
    # 测试1: 123→115 50MB文件 (预期秒传)
    print_header "测试1: 123→115 小文件秒传"
    total_tests=$((total_tests + 1))
    if quick_test \
        "123→115 (50MB)" \
        "123:test1/$TEST_FILE_50MB" \
        "115:quick_test/" \
        "instant"; then
        passed_tests=$((passed_tests + 1))
    fi
    
    sleep 2
    
    # 测试2: 115→123 50MB文件 (预期秒传)
    print_header "测试2: 115→123 小文件秒传"
    total_tests=$((total_tests + 1))
    if quick_test \
        "115→123 (50MB)" \
        "115:quick_test/$TEST_FILE_50MB" \
        "123:quick_test/" \
        "instant"; then
        passed_tests=$((passed_tests + 1))
    fi
    
    sleep 2
    
    # 测试3: 123→115 200MB文件 (预期秒传)
    print_header "测试3: 123→115 中等文件秒传"
    total_tests=$((total_tests + 1))
    if quick_test \
        "123→115 (200MB)" \
        "123:test_instant_transfer/$TEST_FILE_200MB" \
        "115:quick_test/" \
        "instant"; then
        passed_tests=$((passed_tests + 1))
    fi
    
    sleep 2
    
    # 测试4: 115→123 200MB文件 (预期秒传)
    print_header "测试4: 115→123 中等文件秒传"
    total_tests=$((total_tests + 1))
    if quick_test \
        "115→123 (200MB)" \
        "115:quick_test/$TEST_FILE_200MB" \
        "123:quick_test/" \
        "instant"; then
        passed_tests=$((passed_tests + 1))
    fi
    
    # 测试结果统计
    print_header "测试结果统计"
    echo "总测试数: $total_tests"
    echo "成功测试: $passed_tests"
    echo "失败测试: $((total_tests - passed_tests))"
    echo "成功率: $(( passed_tests * 100 / total_tests ))%"
    
    if [[ $passed_tests -eq $total_tests ]]; then
        print_success "🎉 所有测试通过！双向秒传功能正常工作"
    else
        print_error "⚠️ 存在失败的测试，请检查配置和网络连接"
        exit 1
    fi
    
    print_header "快速测试完成"
    echo "如需详细测试，请运行: ./test_115_123_bidirectional.sh"
}

# 帮助信息
show_help() {
    echo "rclone 115/123网盘秒传功能快速测试脚本"
    echo ""
    echo "用法: $0 [选项]"
    echo ""
    echo "选项:"
    echo "  -h, --help     显示帮助信息"
    echo "  --version      显示版本信息"
    echo "  --check-only   仅检查环境，不执行测试"
    echo ""
    echo "环境要求:"
    echo "  - rclone可执行文件: $RCLONE_BIN"
    echo "  - 115网盘配置: [115]"
    echo "  - 123网盘配置: [123]"
    echo "  - 测试文件: 123:test1/$TEST_FILE_50MB"
    echo "  - 测试文件: 123:test_instant_transfer/$TEST_FILE_200MB"
    echo ""
    echo "示例:"
    echo "  $0                # 执行快速测试"
    echo "  $0 --check-only   # 仅检查环境"
    echo ""
}

# 参数处理
case "${1:-}" in
    -h|--help)
        show_help
        exit 0
        ;;
    --version)
        echo "快速测试脚本 v1.0"
        exit 0
        ;;
    --check-only)
        print_header "环境检查模式"
        check_rclone
        check_configs
        print_success "环境检查完成，可以执行测试"
        exit 0
        ;;
    "")
        # 无参数，执行主程序
        main
        ;;
    *)
        echo "未知参数: $1"
        echo "使用 --help 查看帮助信息"
        exit 1
        ;;
esac
