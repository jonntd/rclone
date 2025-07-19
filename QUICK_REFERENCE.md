# 123网盘测试脚本快速参考

## 🚀 快速开始

```bash
# 基本测试
./test_123_standard.sh

# 快速测试
./test_123_standard.sh --quick

# 性能测试
./test_123_standard.sh --performance
```

## 📋 常用配置

### 文件大小配置
```bash
# 小文件测试
SMALL_FILE_SIZE=5 ./test_123_standard.sh --quick

# 自定义所有大小
SMALL_FILE_SIZE=20 MEDIUM_FILE_SIZE=50 LARGE_FILE_SIZE=100 HUGE_FILE_SIZE=200 ./test_123_standard.sh
```

### 目录管理
```bash
# 自定义统一目录
UNIFIED_TEST_DIR=my_test ./test_123_standard.sh

# 测试后清理远程目录
CLEAN_REMOTE=true ./test_123_standard.sh

# 保留本地文件
KEEP_FILES=true ./test_123_standard.sh
```

### 详细模式
```bash
# 详细日志
VERBOSE_LEVEL=verbose ./test_123_standard.sh

# 并发测试
CONCURRENT_TRANSFERS=4 ./test_123_standard.sh
```

## 🔍 查看结果

### 查看远程文件
```bash
# 默认统一目录
./rclone_test ls 123:123_test_unified/

# 自定义目录
./rclone_test ls 123:my_test/
```

### 查看日志
```bash
# 最新日志目录
ls -la logs_*/

# 查看测试总结
cat logs_*/summary.log

# 查看主日志
cat logs_*/main.log
```

## 🧹 清理操作

### 清理远程文件
```bash
# 手动清理默认目录
./rclone_test delete 123:123_test_unified/

# 自动清理（测试时）
CLEAN_REMOTE=true ./test_123_standard.sh
```

### 清理本地文件
```bash
# 清理测试文件
rm -f test_123_*.bin

# 清理日志
rm -rf logs_*
```

## 📊 文件命名规则

```
test_123_{类型}_{大小}MB.bin

示例：
- test_123_small_10MB.bin      # 小文件
- test_123_medium_100MB.bin    # 中等文件
- test_123_large_500MB.bin     # 大文件
- test_123_huge_1200MB.bin     # 超大文件
- test_123_quick_10MB.bin      # 快速测试
- test_123_perf_1_50MB.bin     # 性能测试
```

## ⚙️ 环境变量

| 变量名 | 默认值 | 说明 |
|--------|--------|------|
| `SMALL_FILE_SIZE` | 10 | 小文件大小(MB) |
| `MEDIUM_FILE_SIZE` | 100 | 中等文件大小(MB) |
| `LARGE_FILE_SIZE` | 500 | 大文件大小(MB) |
| `HUGE_FILE_SIZE` | 1200 | 超大文件大小(MB) |
| `UNIFIED_TEST_DIR` | 123_test_unified | 统一测试目录 |
| `CLEAN_REMOTE` | false | 清理远程目录 |
| `VERBOSE_LEVEL` | normal | 详细程度 |
| `KEEP_FILES` | false | 保留本地文件 |
| `CONCURRENT_TRANSFERS` | 1 | 并发传输数 |

## 🎯 测试场景

### 开发调试
```bash
# 快速验证
./test_123_standard.sh --quick

# 详细调试
VERBOSE_LEVEL=verbose KEEP_FILES=true ./test_123_standard.sh --quick
```

### 性能测试
```bash
# 基准测试
./test_123_standard.sh --performance

# 高并发测试
CONCURRENT_TRANSFERS=8 ./test_123_standard.sh --performance
```

### 回归测试
```bash
# 完整测试
UNIFIED_TEST_DIR=regression ./test_123_standard.sh

# 清理测试
CLEAN_REMOTE=true ./test_123_standard.sh
```

### 压力测试
```bash
# 大文件测试
LARGE_FILE_SIZE=1000 HUGE_FILE_SIZE=2000 ./test_123_standard.sh

# 多并发测试
CONCURRENT_TRANSFERS=10 ./test_123_standard.sh
```

## 🔧 故障排除

### 连接问题
```bash
# 检查配置
./rclone_test config show 123:

# 测试连接
./rclone_test about 123:
```

### 空间问题
```bash
# 小文件测试
SMALL_FILE_SIZE=1 MEDIUM_FILE_SIZE=5 ./test_123_standard.sh
```

### 权限问题
```bash
# 确保执行权限
chmod +x test_123_standard.sh
```

## 📈 检查项目

测试脚本会自动检查：
- ✅ .partial机制状态
- ✅ 上传策略选择
- ✅ API接口使用
- ✅ 回收站问题
- ✅ 秒传行为
- ✅ 文件大小验证
- ✅ 错误统计

## 🎉 成功标准

- **完美成功**: 所有文件上传成功，无.partial机制，无回收站问题
- **基本成功**: 大部分文件上传成功，主要功能正常
- **需要修复**: 多项测试失败，需要进一步分析

## 📝 日志位置

```
logs_YYYYMMDD_HHMMSS/
├── main.log           # 主日志
├── summary.log        # 测试总结
├── upload_*.log       # 详细上传日志
└── upload_*_console.log # 控制台输出
```

## 🔄 常用组合

```bash
# 快速开发测试
VERBOSE_LEVEL=verbose ./test_123_standard.sh --quick

# 完整性能测试
CONCURRENT_TRANSFERS=4 ./test_123_standard.sh --performance

# 自定义大小测试
SMALL_FILE_SIZE=50 LARGE_FILE_SIZE=200 ./test_123_standard.sh

# 清理测试
CLEAN_REMOTE=true KEEP_FILES=false ./test_123_standard.sh

# 保留所有文件
KEEP_FILES=true CLEAN_REMOTE=false ./test_123_standard.sh
```
