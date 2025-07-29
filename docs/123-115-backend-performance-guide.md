# 123网盘和115网盘后端性能优化指南

## 概述

123网盘和115网盘后端都有自己的并发传输机制，**不支持 rclone 的 `--multi-thread-streams` 功能**。本文档详细介绍了两个后端的性能参数配置和优化建议。

## 为什么不支持 --multi-thread-streams？

| 后端 | NoMultiThreading | OpenChunkWriter/OpenWriterAt | 支持状态 | 原因 |
|------|------------------|------------------------------|----------|------|
| **123** | ❌ false | ❌ 未实现 | ❌ **不支持** | 使用自有 v2MultiThreadUpload 机制 |
| **115** | ❌ true | ❌ 未实现 | ❌ **不支持** | 使用 OSS 分片上传 + 统一并发下载器 |

两个后端都选择使用专门针对各自 API 特点优化的并发机制，而不是 rclone 的标准多线程传输。

## 123网盘后端配置参数

### 基础认证配置
```bash
# 必需参数
--123-client-id="your_client_id"           # 123网盘API客户端ID
--123-client-secret="your_client_secret"   # 123网盘API客户端密钥
--123-token="oauth_token_json"             # OAuth访问令牌（通常留空）
```

### 并发控制参数 ⚡
```bash
# 上传并发控制
--123-max-concurrent-uploads=8             # 最大并发上传数量（默认8）
                                          # 减少此值可降低内存使用和提高稳定性

# 下载并发控制  
--123-max-concurrent-downloads=4          # 最大并发下载数量（默认4）
                                          # 提高此值可提升下载速度，但会增加内存使用

# 分片大小控制
--123-chunk-size=100M                     # 分片上传的分块大小（默认100MB）
                                          # 范围：50MB - 500MB

--123-upload-cutoff=100M                  # 切换到分片上传的阈值（默认100MB）
                                          # 小于此大小使用单步上传
```

### QPS 控制参数 🎛️
```bash
# 通用API调速
--123-pacer-min-sleep=100ms               # 通用API调用间隔（默认100ms，~10 QPS）

# 专用API调速
--123-upload-pacer-min-sleep=50ms         # 上传API调用间隔（默认50ms，~20 QPS）
--123-download-pacer-min-sleep=100ms      # 下载API调用间隔（默认100ms）
--123-strict-pacer-min-sleep=200ms        # 严格API调用间隔（move, delete等）
```

### 缓存优化参数 💾
```bash
# 缓存大小控制
--123-cache-max-size=100M                 # 最大缓存大小（默认100MB）
--123-cache-target-size=64M               # 清理后目标大小（默认64MB）

# 缓存策略
--123-enable-smart-cleanup=false          # 启用LRU智能清理（默认false）
--123-cleanup-strategy="size"             # 清理策略：size/lru/priority_lru/time
```

### 高级性能参数 🔧
```bash
# 文件操作
--123-list-chunk=100                      # 列表请求文件数量（默认100）
--123-max-upload-parts=1000               # 最大上传分片数（默认1000）

# 网络超时
--123-conn-timeout=60s                    # 连接超时（默认60秒）
--123-timeout=300s                        # 请求超时（默认300秒）

# 进度显示
--123-enable-progress-display=true        # 启用进度显示（默认true）
--123-progress-update-interval=5s         # 进度更新间隔（默认5秒）
```

## 115网盘后端配置参数

### 基础认证配置
```bash
# 必需参数
--115-cookie="UID=...; CID=...; SEID=...;" # 登录Cookie（必需）
--115-user-agent="Mozilla/5.0..."          # HTTP用户代理
--115-app-id="your_app_id"                 # 自定义应用ID
```

### 上传策略参数 📤
```bash
# 上传模式选择（互斥选项）
--115-fast-upload=false                   # 智能上传策略（推荐）
--115-only-stream=false                   # 仅使用流式上传（≤5GB）
--115-upload-hash-only=false              # 仅尝试秒传

# 分片上传控制
--115-upload-cutoff=50M                   # 分片上传阈值（默认50MB）
--115-chunk-size=20M                      # 分片大小（默认20MB）
--115-max-upload-parts=10000              # 最大分片数（默认10000）

# 哈希计算优化
--115-hash-memory-limit=10M               # 内存哈希计算限制（默认10MB）
--115-nohash-size=100M                    # 小文件流式上传阈值（默认100MB）
```

### QPS 控制参数 🎛️
```bash
# 统一QPS控制（115网盘所有API共享QPS配额）
--115-pacer-min-sleep=300ms               # API调用间隔（默认300ms，~3.3 QPS）
                                          # 115网盘有严格的QPS限制，建议保守设置
```

### 下载优化参数 📥
```bash
# 115网盘使用统一并发下载器，自动优化下载性能
# 配置参数：
# - 最小文件大小：100MB（启用并发下载门槛）
# - 最大并发数：2线程（115网盘限制）
# - 默认分片大小：100MB
# - 超时时间：120秒/分片
```

### 缓存优化参数 💾
```bash
# 缓存大小控制
--115-cache-max-size=100M                 # 最大缓存大小（默认100MB）
--115-cache-target-size=64M               # 清理后目标大小（默认64MB）

# 缓存策略
--115-enable-smart-cleanup=false          # 启用LRU智能清理（默认false）
--115-cleanup-strategy="size"             # 清理策略：size/lru/priority_lru/time
```

### 高级选项 🔧
```bash
# OSS上传优化
--115-internal=false                      # 使用内部OSS端点
--115-dual-stack=false                    # 使用双栈OSS端点

# 调试选项
--115-no-check=false                      # 禁用上传后检查
--115-no-buffer=false                     # 跳过磁盘缓冲

# 文件列表
--115-list-chunk=1150                     # 列表分块大小（默认1150）
```

## 性能优化建议 🚀

### 123网盘优化策略
1. **上传优化**：
   - 大文件：增加 `--123-max-concurrent-uploads` 到 12-16
   - 网络好：减少 `--123-upload-pacer-min-sleep` 到 25ms
   - 内存足：增加 `--123-chunk-size` 到 200M

2. **下载优化**：
   - 增加 `--123-max-concurrent-downloads` 到 8
   - 使用 `--123-enable-smart-cleanup=true`

### 115网盘优化策略
1. **上传优化**：
   - 启用 `--115-fast-upload=true` 智能策略
   - 大文件：增加 `--115-chunk-size` 到 50M
   - 调整 `--115-upload-cutoff` 根据网络情况

2. **下载优化**：
   - 115网盘自动优化，无需手动调整
   - 大文件自动启用2线程并发下载

### 通用优化建议
1. **网络质量差**：增加各种 `pacer-min-sleep` 参数
2. **内存充足**：增加 `cache-max-size` 和 `chunk-size`
3. **CPU性能好**：启用 `enable-smart-cleanup=true`
4. **调试问题**：使用 `-vv` 查看详细日志

## 示例配置

### 高性能配置（网络好、内存足）
```bash
# 123网盘高性能配置
rclone sync local: 123:remote \
  --123-max-concurrent-uploads=12 \
  --123-max-concurrent-downloads=8 \
  --123-chunk-size=200M \
  --123-upload-pacer-min-sleep=25ms \
  --123-cache-max-size=500M \
  --123-enable-smart-cleanup=true

# 115网盘高性能配置  
rclone sync local: 115:remote \
  --115-fast-upload=true \
  --115-chunk-size=50M \
  --115-cache-max-size=500M \
  --115-enable-smart-cleanup=true
```

### 稳定性优先配置（网络差、内存少）
```bash
# 123网盘稳定配置
rclone sync local: 123:remote \
  --123-max-concurrent-uploads=4 \
  --123-max-concurrent-downloads=2 \
  --123-chunk-size=50M \
  --123-upload-pacer-min-sleep=100ms \
  --123-cache-max-size=50M

# 115网盘稳定配置
rclone sync local: 115:remote \
  --115-only-stream=true \
  --115-chunk-size=20M \
  --115-pacer-min-sleep=500ms \
  --115-cache-max-size=50M
```

## 注意事项 ⚠️

1. **QPS限制**：两个网盘都有严格的API调用频率限制，过快会导致429错误
2. **内存使用**：并发数 × 分片大小 = 内存占用，需要合理配置
3. **网络稳定性**：不稳定网络建议降低并发数和分片大小
4. **账号限制**：部分参数可能受账号类型限制（如VIP vs 普通用户）

## 故障排除 🔧

### 常见问题
- **429错误**：增加相应的 `pacer-min-sleep` 参数
- **内存不足**：减少并发数或分片大小
- **上传失败**：检查网络稳定性，尝试减少并发
- **下载慢**：检查是否启用了并发下载（文件>100MB）

### 调试命令
```bash
# 查看详细日志
rclone -vv sync local: 123:remote

# 查看传输统计
rclone sync local: 123:remote --stats=1s

# 测试配置
rclone test memory local: 123:remote
```

## 参数对比表 📊

### 并发控制对比
| 参数类型 | 123网盘 | 115网盘 | 说明 |
|----------|---------|---------|------|
| 上传并发 | `--123-max-concurrent-uploads=8` | 自动优化（OSS分片） | 123可调，115自动 |
| 下载并发 | `--123-max-concurrent-downloads=4` | 自动优化（2线程限制） | 123可调，115固定 |
| 分片大小 | `--123-chunk-size=100M` | `--115-chunk-size=20M` | 123更大分片 |
| 上传阈值 | `--123-upload-cutoff=100M` | `--115-upload-cutoff=50M` | 123阈值更高 |

### QPS控制对比
| API类型 | 123网盘 | 115网盘 | 推荐值 |
|---------|---------|---------|--------|
| 通用API | `--123-pacer-min-sleep=100ms` | `--115-pacer-min-sleep=300ms` | 115更保守 |
| 上传API | `--123-upload-pacer-min-sleep=50ms` | 统一QPS控制 | 123有专用控制 |
| 下载API | `--123-download-pacer-min-sleep=100ms` | 统一QPS控制 | 123有专用控制 |
| 严格API | `--123-strict-pacer-min-sleep=200ms` | 统一QPS控制 | 123有专用控制 |

## 高级配置技巧 🎯

### 123网盘专用技巧

#### 1. 动态并发调整
```bash
# 根据网络质量动态调整
# 网络好时（延迟<50ms）
--123-max-concurrent-uploads=16 \
--123-upload-pacer-min-sleep=25ms

# 网络一般时（延迟50-200ms）
--123-max-concurrent-uploads=8 \
--123-upload-pacer-min-sleep=50ms

# 网络差时（延迟>200ms）
--123-max-concurrent-uploads=4 \
--123-upload-pacer-min-sleep=100ms
```

#### 2. 大文件优化策略
```bash
# 超大文件（>10GB）优化
--123-chunk-size=500M \
--123-max-upload-parts=100 \
--123-upload-pacer-min-sleep=25ms \
--123-cache-max-size=1G
```

#### 3. 批量小文件优化
```bash
# 大量小文件优化
--123-upload-cutoff=50M \
--123-max-concurrent-uploads=20 \
--123-upload-pacer-min-sleep=25ms \
--123-list-chunk=200
```

### 115网盘专用技巧

#### 1. 上传策略选择
```bash
# 策略1：智能上传（推荐）
--115-fast-upload=true \
--115-nohash-size=100M \
--115-upload-cutoff=50M

# 策略2：纯秒传模式
--115-upload-hash-only=true \
--115-hash-memory-limit=50M

# 策略3：纯流式上传
--115-only-stream=true \
--115-nohash-size=5G
```

#### 2. OSS优化配置
```bash
# 内网环境优化
--115-internal=true \
--115-dual-stack=false \
--115-chunk-size=100M

# 公网环境优化
--115-internal=false \
--115-dual-stack=true \
--115-chunk-size=50M
```

#### 3. 哈希计算优化
```bash
# 内存充足时
--115-hash-memory-limit=100M \
--115-no-buffer=false

# 内存紧张时
--115-hash-memory-limit=10M \
--115-no-buffer=true
```

## 监控和调试 🔍

### 性能监控命令
```bash
# 实时监控传输状态
rclone sync local: 123:remote \
  --stats=1s \
  --stats-one-line \
  --progress

# 详细调试信息
rclone -vv sync local: 123:remote \
  --dump=headers \
  --dump=bodies

# 网络质量测试
rclone test memory local: 123:remote \
  --size=100M \
  --transfers=4
```

### 日志分析关键词
```bash
# 123网盘关键日志
grep "123网盘" rclone.log | grep -E "(并发|分片|QPS|限制)"

# 115网盘关键日志
grep "115网盘" rclone.log | grep -E "(OSS|分片|QPS|770004)"

# 性能相关日志
grep -E "(multi-thread|concurrent|chunk|pacer)" rclone.log
```

### 常见错误码
| 错误码 | 后端 | 含义 | 解决方案 |
|--------|------|------|----------|
| 429 | 123/115 | API调用过频 | 增加 pacer-min-sleep |
| 770004 | 115 | QPS限制 | 增加 pacer-min-sleep |
| 403 | 115 | 下载权限 | 自动降级到普通下载 |
| 500 | 123 | 服务器错误 | 重试或降低并发 |

## 最佳实践总结 ✅

### 配置原则
1. **保守起步**：从默认配置开始，逐步优化
2. **监控调整**：观察错误率和传输速度，动态调整
3. **分场景配置**：不同网络环境使用不同配置
4. **定期检查**：网盘API可能变化，需要调整参数

### 推荐配置模板

#### 日常使用配置
```bash
# ~/.config/rclone/rclone.conf
[123-daily]
type = 123
client_id = your_id
client_secret = your_secret
max_concurrent_uploads = 8
max_concurrent_downloads = 4
chunk_size = 100M
upload_pacer_min_sleep = 50ms

[115-daily]
type = 115
cookie = your_cookie
fast_upload = true
chunk_size = 20M
pacer_min_sleep = 300ms
```

#### 高性能配置
```bash
[123-performance]
type = 123
client_id = your_id
client_secret = your_secret
max_concurrent_uploads = 16
max_concurrent_downloads = 8
chunk_size = 200M
upload_pacer_min_sleep = 25ms
cache_max_size = 500M
enable_smart_cleanup = true

[115-performance]
type = 115
cookie = your_cookie
fast_upload = true
chunk_size = 50M
pacer_min_sleep = 200ms
cache_max_size = 500M
enable_smart_cleanup = true
```

## 版本兼容性 📋

### 支持的 rclone 版本
- **最低版本**：rclone v1.60+
- **推荐版本**：rclone v1.65+
- **测试版本**：基于最新开发版本

### 功能支持矩阵
| 功能 | 123网盘 | 115网盘 | 备注 |
|------|---------|---------|------|
| 基础上传下载 | ✅ | ✅ | 完全支持 |
| 并发传输 | ✅ | ✅ | 自有实现 |
| 断点续传 | ✅ | ✅ | 分片级别 |
| 秒传功能 | ✅ | ✅ | 都支持MD5/SHA1哈希秒传 |
| 目录同步 | ✅ | ✅ | 完全支持 |
| 元数据保持 | 部分 | 部分 | 时间戳支持有限 |
| rclone mount | ✅ | ✅ | 支持VFS |
| rclone serve | ✅ | ✅ | HTTP/WebDAV |

## Backend Commands 🔧

两个后端都支持专用的 backend 命令，用于高级管理和调试。

### 123网盘 Backend Commands

```bash
# 查看所有可用命令
rclone backend help 123:

# 通过文件路径获取下载URL
rclone backend getdownloadurlua 123: "/path/to/file" "VidHub/1.7.24"

# 手动触发缓存清理
rclone backend cache-cleanup 123: --strategy=lru

# 查看缓存统计信息
rclone backend cache-stats 123:

# 查看当前缓存配置
rclone backend cache-config 123:

# 重置缓存配置为默认值
rclone backend cache-reset 123:
```

### 115网盘 Backend Commands

```bash
# 查看所有可用命令
rclone backend help 115:

# 通过文件路径获取下载URL（支持User-Agent）
rclone backend getdownloadurlua 115: "/path/to/file" "VidHub/1.7.24"

# 手动触发缓存清理
rclone backend cache-cleanup 115: --strategy=lru

# 查看缓存统计信息
rclone backend cache-stats 115:

# 查看当前缓存配置
rclone backend cache-config 115:

# 重置缓存配置为默认值
rclone backend cache-reset 115:
```

### Backend Commands 使用场景

1. **性能调试**：使用 `cache-stats` 查看缓存命中率
2. **存储管理**：使用 `cache-cleanup` 释放缓存空间
3. **配置重置**：使用 `cache-reset` 恢复默认设置
4. **下载优化**：使用 `getdownloadurlua` 获取直链

## 秒传功能详解 ⚡

### 123网盘秒传机制
- **触发条件**：文件MD5已存在于服务器
- **检测时机**：每次上传前自动检测
- **支持格式**：所有文件类型
- **性能优势**：跳过实际传输，瞬间完成
- **实现方式**：通过 `checkInstantUpload` 函数检测

```bash
# 123网盘秒传示例日志
INFO  : ⚡ 检查123网盘秒传功能...
INFO  : 🎉 秒传成功！文件大小: 100M, MD5: abc123...
INFO  : ⚡ 123网盘秒传成功: 100M, 总用时: 2s (下载: 1s, MD5: 1s)
```

**重要提示**：123网盘的秒传功能是自动启用的，无需额外配置。每次上传时都会先计算MD5并检查是否可以秒传。

### 115网盘秒传机制
- **触发条件**：文件SHA1已存在于服务器
- **检测时机**：启用 `--115-fast-upload` 时自动检测
- **支持格式**：所有文件类型
- **性能优势**：避免重复上传相同内容

```bash
# 115网盘秒传配置
--115-fast-upload=true          # 启用智能上传（包含秒传）
--115-upload-hash-only=true     # 仅尝试秒传模式
```

---

**注意**：本文档基于当前代码版本编写，实际参数可能随版本更新而变化。建议使用 `rclone help backend 123` 和 `rclone help backend 115` 查看最新参数说明。
