# STRM-Mount 完整使用指南

## 🎯 **概述**

STRM-Mount 是一个专为网盘媒体库设计的智能虚拟化工具，可以将网盘中的视频文件虚拟化为 .strm 文件，完美兼容 Emby、Jellyfin、Plex 等媒体服务器，同时提供企业级的 QPS 保护机制。

### **核心特性**
- 🎬 **智能视频识别**: 自动识别视频文件并生成 .strm 文件
- 🛡️ **QPS 保护**: 多层智能限制，避免网盘 API 限制
- 💾 **双层缓存**: 内存+持久化缓存，极速响应
- 📏 **大小过滤**: 统一大小限制，隐藏小文件
- 🔄 **增量同步**: 智能检测文件变更，减少 API 调用
- 📊 **实时监控**: 详细的性能统计和日志

---

## 🚀 **快速开始**

### **1. 基本安装**

```bash
# 下载最新版本
wget https://github.com/rclone/rclone/releases/latest/download/rclone-linux-amd64.zip
unzip rclone-linux-amd64.zip
sudo cp rclone-*/rclone /usr/local/bin/
sudo chmod +x /usr/local/bin/rclone

# 配置网盘
rclone config
```

### **2. 基本使用**

```bash
# 最简单的使用方式
rclone strm-mount 123:Movies /mnt/strm --persistent-cache=true --min-size=50M

# 后台运行
rclone strm-mount 123:Movies /mnt/strm \
  --persistent-cache=true \
  --min-size=50M \
  --daemon \
  --log-file=/var/log/strm-mount.log
```

### **3. 验证效果**

```bash
# 检查挂载状态
mount | grep strm

# 查看生成的 STRM 文件
ls -la /mnt/strm/

# 检查 STRM 文件内容
cat /mnt/strm/movie.strm
# 输出: 123://fileID 或 115://pickCode
```

---

## ⚙️ **详细配置**

### **核心参数**

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `--persistent-cache` | `false` | 启用持久化缓存 |
| `--cache-ttl` | `5m` | 缓存生存时间 |
| `--min-size` | `100M` | 最小文件大小限制 |
| `--daemon` | `false` | 后台运行模式 |
| `--log-file` | - | 日志文件路径 |
| `--log-level` | `NOTICE` | 日志级别 |

### **网盘特定配置**

#### **123网盘 (极保守配置)**
```bash
rclone strm-mount 123:Movies /mnt/strm \
  --persistent-cache=true \
  --cache-ttl=3h \
  --min-size=50M \
  --checkers=1 \
  --transfers=1 \
  --vfs-cache-mode=minimal \
  --buffer-size=0 \
  --vfs-read-ahead=0 \
  --daemon \
  --log-file=/var/log/strm-123.log
```

#### **115网盘 (保守配置)**
```bash
rclone strm-mount 115:Videos /mnt/strm \
  --persistent-cache=true \
  --cache-ttl=2h \
  --min-size=100M \
  --checkers=2 \
  --transfers=1 \
  --vfs-cache-mode=minimal \
  --buffer-size=0 \
  --vfs-read-ahead=0 \
  --daemon \
  --log-file=/var/log/strm-115.log
```

### **高级配置**

#### **生产环境配置**
```bash
rclone strm-mount 123:Movies /mnt/strm \
  --persistent-cache=true \
  --cache-ttl=6h \
  --min-size=100M \
  --checkers=1 \
  --transfers=1 \
  --vfs-cache-mode=minimal \
  --buffer-size=0 \
  --vfs-read-ahead=0 \
  --allow-other \
  --daemon \
  --log-file=/var/log/strm-mount.log \
  --log-level=INFO \
  --syslog
```

#### **开发测试配置**
```bash
rclone strm-mount 123:Test /mnt/strm \
  --persistent-cache=true \
  --cache-ttl=30m \
  --min-size=10M \
  --log-level=DEBUG \
  --log-file=/tmp/strm-debug.log
```

---

## 🛡️ **QPS 保护机制**

### **智能保护特性**

#### **1. 多层限制**
- **时间间隔限制**: 最小刷新间隔 (1-6小时)
- **QPS 实时监控**: 动态 QPS 阈值 (0.1-0.5)
- **智能历史分析**: 基于变更率预测
- **访问模式识别**: 基于访问频率优化
- **请求合并**: 避免重复 API 调用

#### **2. 网盘特定优化**
```bash
# 123网盘 - 极保守
最小间隔: 2小时, 最大间隔: 12小时, QPS阈值: 0.2

# 115网盘 - 保守  
最小间隔: 1小时, 最大间隔: 8小时, QPS阈值: 0.4

# 未知网盘 - 最保守
最小间隔: 3小时, 最大间隔: 24小时, QPS阈值: 0.1
```

#### **3. 监控日志**
```bash
# 正常运行日志
📊 [REFRESH-LIMIT] 统计: 目录=5, 允许=12, 阻止=38, QPS=0.23
✅ [REFRESH-LIMIT] 允许刷新: /Movies (QPS: 0.15)
🚫 [REFRESH-LIMIT] 间隔限制: /TV Shows (需要: 2h)
🧠 [REFRESH-LIMIT] 智能跳过: /Docs (低变更率)

# 缓存性能日志
💾 [CACHE] 缓存命中: /Movies (响应时间: 2ms)
⏰ [CACHE] 缓存已过期，执行增量同步
✅ [CACHE] 增量同步完成: +2 -1 ~5 (耗时 3.2s)
```

---

## 📏 **文件大小过滤**

### **统一大小限制**

基于 `--min-size` 参数的统一过滤逻辑：

```bash
小于配置大小 → 完全隐藏（不显示任何文件）
大于配置大小 + 是视频 → 生成 STRM 文件  
大于配置大小 + 非视频 → 显示原文件（如果允许）
```

### **推荐配置**

| 场景 | 推荐大小 | 说明 |
|------|----------|------|
| **电影库** | `--min-size=500M` | 过滤预告片、花絮等小文件 |
| **电视剧库** | `--min-size=100M` | 过滤片头、片尾等小文件 |
| **综合库** | `--min-size=50M` | 平衡过滤效果和内容完整性 |
| **测试环境** | `--min-size=10M` | 显示更多文件用于测试 |

### **效果示例**

```bash
# 原始文件列表
01 Introduction.mp4     (24.7MB)  # 小于50MB
02 Getting Started.mp4  (80.3MB)  # 大于50MB
03 Advanced Topics.mp4  (52.9MB)  # 大于50MB

# 使用 --min-size=50M 后的结果
02 Getting Started.strm  (23 bytes)  # 转换为STRM
03 Advanced Topics.strm  (23 bytes)  # 转换为STRM
# 01 Introduction.mp4 完全隐藏
```

---

## 💾 **缓存系统**

### **双层缓存架构**

```
用户请求 → 内存缓存 → 持久化缓存 → 远程API
          (毫秒级)   (秒级)      (分钟级)
```

### **缓存配置优化**

#### **TTL 设置建议**
```bash
# 高性能场景 (减少刷新频率)
--cache-ttl=6h

# 数据敏感场景 (增加刷新频率)  
--cache-ttl=2h

# 开发测试场景 (实时刷新)
--cache-ttl=30m
```

#### **缓存性能监控**
```bash
# 检查缓存状态
grep -E "\[CACHE\]" /var/log/strm-mount.log | tail -10

# 监控缓存命中率
grep -E "(Hit|Miss)" /var/log/strm-mount.log | tail -20

# 检查增量同步
grep -E "\[SYNC\]" /var/log/strm-mount.log | tail -5
```

---

## 📊 **性能监控**

### **关键指标**

| 指标 | 健康范围 | 说明 |
|------|----------|------|
| **QPS** | <0.5 | API 调用频率 |
| **缓存命中率** | >90% | 缓存效率 |
| **响应时间** | <100ms | 目录读取速度 |
| **阻止率** | >70% | 保护机制有效性 |

### **监控命令**

```bash
# 实时日志监控
tail -f /var/log/strm-mount.log | grep -E "(QPS|CACHE|REFRESH)"

# 性能统计
grep -E "took.*µs" /var/log/strm-mount.log | tail -10

# 错误检查
grep -E "(ERROR|CRITICAL)" /var/log/strm-mount.log | tail -5

# 挂载状态
mount | grep strm
```

### **性能优化建议**

#### **高性能配置**
```bash
--cache-ttl=6h          # 长缓存时间
--vfs-cache-mode=off    # 禁用VFS缓存
--checkers=1            # 限制并发
--transfers=1           # 限制传输
```

#### **低延迟配置**
```bash
--cache-ttl=1h          # 中等缓存时间
--vfs-cache-mode=minimal # 最小VFS缓存
--buffer-size=0         # 无缓冲
--vfs-read-ahead=0      # 无预读
```

---

## 🎬 **Emby 配置优化**

### **核心优化原则**

为了与 STRM-Mount 完美配合，避免触发网盘 QPS 限制，需要对 Emby 进行以下关键优化：

#### **1. 禁用实时监控**
```json
{
  "LibraryOptions": {
    "EnableRealtimeMonitor": false,
    "EnablePeriodicScanning": false,
    "ScanOnStartup": false
  }
}
```

#### **2. 优化扫描策略**
```json
{
  "ScheduledTasks": {
    "LibraryScan": {
      "IntervalHours": 24,
      "MaxConcurrentScans": 1,
      "EnablePeriodicScanning": false
    }
  }
}
```

#### **3. 禁用资源密集型功能**
```json
{
  "LibraryOptions": {
    "EnableChapterImageExtraction": false,
    "EnableTrickplayImageExtraction": false,
    "EnableVideoImageExtraction": false,
    "EnableEmbeddedTitles": false,
    "EnableEmbeddedEpisodeInfos": false
  }
}
```

### **完整 Emby 配置文件**

#### **生产环境配置 (emby-strm-optimized.json)**
```json
{
  "ServerConfiguration": {
    "LibraryOptions": {
      "EnableRealtimeMonitor": false,
      "EnableChapterImageExtraction": false,
      "EnableTrickplayImageExtraction": false,
      "EnableVideoImageExtraction": false,
      "SkipSubtitlesIfEmbeddedSubtitlesPresent": true,
      "RequirePerfectSubtitleMatch": false,
      "SaveLocalMetadata": true,
      "PreferredMetadataLanguage": "zh-CN",
      "AutomaticRefreshIntervalDays": 30,
      "EnablePhotos": false,
      "EnableInternetProviders": true,
      "EnableAutomaticSeriesGrouping": false,
      "MetadataRefreshMode": "ValidationOnly",
      "ImageRefreshMode": "ValidationOnly",
      "ReplaceExistingImages": false,
      "EnableEmbeddedTitles": false,
      "EnableEmbeddedEpisodeInfos": false,
      "EnablePeriodicScanning": false,
      "ScanOnStartup": false
    },
    "ScheduledTasks": {
      "LibraryScan": {
        "IntervalHours": 24,
        "MaxConcurrentScans": 1,
        "EnablePeriodicScanning": false,
        "ScanOnStartup": false
      },
      "ChapterImageExtraction": {
        "Enabled": false
      },
      "TrickplayImageExtraction": {
        "Enabled": false
      },
      "RefreshLibrary": {
        "IntervalHours": 168
      }
    },
    "EncodingOptions": {
      "EnableThrottling": true,
      "ThrottleDelaySeconds": 180,
      "HardwareAccelerationType": "none",
      "EnableHardwareDecoding": false,
      "EnableHardwareEncoding": false
    },
    "LibraryMonitorDelay": 60,
    "EnableDashboardResponseCaching": true,
    "MaxConcurrentTranscodes": 1,
    "EnableFolderView": false,
    "EnableGroupingIntoCollections": true,
    "DisplaySpecialsWithinSeasons": true
  }
}
```

### **Emby 媒体库设置**

#### **1. 媒体库创建**
```bash
# 添加媒体库时的关键设置
内容类型: 电影/电视节目
文件夹: /mnt/strm/Movies
实时监控: 禁用 ❌
定期扫描: 禁用 ❌
启动时扫描: 禁用 ❌
```

#### **2. 扫描设置**
```json
{
  "ScanSettings": {
    "EnableRealtimeMonitor": false,
    "EnablePeriodicScanning": false,
    "ScanOnStartup": false,
    "IntervalHours": 24,
    "MaxConcurrentScans": 1
  }
}
```

#### **3. 元数据设置**
```json
{
  "MetadataSettings": {
    "SaveLocalMetadata": true,
    "PreferredMetadataLanguage": "zh-CN",
    "MetadataRefreshMode": "ValidationOnly",
    "ImageRefreshMode": "ValidationOnly",
    "AutomaticRefreshIntervalDays": 30
  }
}
```

### **手动扫描策略**

#### **推荐扫描方式**
```bash
# 1. 完全手动扫描 (推荐)
- 禁用所有自动扫描
- 在低峰时段手动触发扫描
- 分批扫描大型媒体库

# 2. 低频定时扫描 (备选)
- 设置24小时或更长间隔
- 限制并发扫描数量为1
- 监控 QPS 使用情况
```

#### **分批扫描脚本**
```bash
#!/bin/bash
# emby-batch-scan.sh - 分批扫描脚本

EMBY_URL="http://localhost:8096"
API_KEY="your-api-key"

# 获取所有媒体库
libraries=$(curl -s "$EMBY_URL/emby/Library/VirtualFolders?api_key=$API_KEY" | jq -r '.[].ItemId')

# 分批扫描，每次间隔30分钟
for lib_id in $libraries; do
    echo "扫描媒体库: $lib_id"
    curl -X POST "$EMBY_URL/emby/Library/Refresh?api_key=$API_KEY" \
         -H "Content-Type: application/json" \
         -d "{\"Id\":\"$lib_id\"}"

    echo "等待30分钟..."
    sleep 1800
done
```

### **Emby 性能优化**

#### **1. 转码设置**
```json
{
  "EncodingOptions": {
    "EnableThrottling": true,
    "ThrottleDelaySeconds": 180,
    "MaxConcurrentTranscodes": 1,
    "HardwareAccelerationType": "none"
  }
}
```

#### **2. 网络设置**
```json
{
  "NetworkSettings": {
    "EnableDashboardResponseCaching": true,
    "SlowResponseThresholdMs": 500,
    "EnableSlowResponseWarning": true
  }
}
```

#### **3. 日志设置**
```json
{
  "LogSettings": {
    "ActivityLogRetentionDays": 30,
    "EnableDebugLogging": false,
    "LogLevel": "Information"
  }
}
```

### **Emby 与 STRM-Mount 集成测试**

#### **1. 功能测试**
```bash
# 检查 STRM 文件识别
ls /mnt/strm/*.strm | head -5

# 检查 Emby 识别状态
curl "$EMBY_URL/emby/Items?api_key=$API_KEY" | jq '.Items[].Name'

# 测试播放功能
# 在 Emby 界面中播放一个视频文件
```

#### **2. 性能测试**
```bash
# 监控扫描期间的 QPS
tail -f /var/log/strm-mount.log | grep -E "(QPS|API)"

# 检查扫描时间
grep "Library scan" /var/log/emby/emby.log | tail -5

# 监控系统资源
htop
```

#### **3. 问题排查**
```bash
# 检查 STRM 文件格式
head -1 /mnt/strm/movie.strm
# 应该输出: 123://fileID 或 115://pickCode

# 检查 Emby 错误日志
grep -E "(ERROR|WARN)" /var/log/emby/emby.log | tail -10

# 检查网盘连接
rclone ls 123:Movies --max-depth 1
```

---

## 🔍 **故障排除**

### **常见问题**

#### **1. QPS 限制触发**
**症状**: API 调用返回 429 错误或请求超时
```bash
# 检查当前 QPS
grep -E "QPS.*[5-9]\." /var/log/strm-mount.log | tail -5

# 解决方案
--cache-ttl=6h          # 增加缓存时间
--checkers=1            # 减少并发
--transfers=1           # 限制传输
```

#### **2. 缓存命中率低**
**症状**: 大量 API 调用，响应缓慢
```bash
# 检查缓存命中率
grep -E "(Hit|Miss)" /var/log/strm-mount.log | tail -20

# 解决方案
--cache-ttl=3h          # 增加缓存时间
--persistent-cache=true # 启用持久化缓存
```

#### **3. Emby 扫描过于频繁**
**症状**: 持续的高 QPS
```bash
# 检查 Emby 扫描日志
tail -f /var/log/emby/emby.log | grep -i scan

# 解决方案
"EnableRealtimeMonitor": false    # 禁用实时监控
"IntervalHours": 48              # 增加扫描间隔
```

#### **4. STRM 文件无法播放**
**症状**: Emby 中视频无法播放
```bash
# 检查 STRM 文件格式
cat /mnt/strm/movie.strm
# 应该是: 123://fileID 或 115://pickCode

# 检查网盘连接
rclone ls 123:Movies | head -5

# 解决方案
重新挂载 STRM-Mount
检查网盘配置
```

### **日志分析**

#### **正常运行日志**
```bash
✅ [CACHE] 新缓存创建完成: 288 个文件, 耗时 11.3s
📊 [REFRESH-LIMIT] 统计: 目录=5, 允许=12, 阻止=38, QPS=0.23
💾 [CACHE] 缓存命中: /Movies (响应时间: 2ms)
📂 [PERF] Readdir(/Movies): 46 total, 46 videos→46 strm files, took 731µs
```

#### **问题日志**
```bash
❌ [ERROR] API调用失败: 429 Too Many Requests
⚠️ [REFRESH-LIMIT] QPS 限制: 0.65 > 0.50, 延迟刷新
🚫 [CACHE] 缓存加载失败: 文件损坏
⏰ [TIMEOUT] API调用超时: 30s
```

### **性能调优**

#### **高性能配置**
```bash
# 适用于: 大型媒体库，稳定网络
rclone strm-mount 123:Movies /mnt/strm \
  --persistent-cache=true \
  --cache-ttl=6h \
  --min-size=100M \
  --checkers=1 \
  --transfers=1 \
  --vfs-cache-mode=off \
  --daemon
```

#### **低延迟配置**
```bash
# 适用于: 小型媒体库，快速响应
rclone strm-mount 123:Movies /mnt/strm \
  --persistent-cache=true \
  --cache-ttl=2h \
  --min-size=50M \
  --checkers=2 \
  --transfers=1 \
  --vfs-cache-mode=minimal \
  --daemon
```

#### **调试配置**
```bash
# 适用于: 问题排查，开发测试
rclone strm-mount 123:Movies /mnt/strm \
  --persistent-cache=true \
  --cache-ttl=30m \
  --min-size=10M \
  --log-level=DEBUG \
  --log-file=/tmp/strm-debug.log
```

---

## 🏆 **最佳实践**

### **部署策略**

#### **1. 分阶段部署**
```bash
# 阶段1: 小规模测试 (<100个文件)
--min-size=10M --cache-ttl=1h

# 阶段2: 中等规模 (100-1000个文件)
--min-size=50M --cache-ttl=2h

# 阶段3: 大规模部署 (>1000个文件)
--min-size=100M --cache-ttl=6h
```

#### **2. 监控和告警**
```bash
# 设置监控脚本
*/5 * * * * /usr/local/bin/check-strm-qps.sh

# 设置日志轮转
/var/log/strm-mount/*.log {
    daily
    rotate 30
    compress
    delaycompress
    missingok
    notifempty
}
```

#### **3. 备份和恢复**
```bash
# 备份缓存文件
cp /var/cache/rclone/strm-cache-*.json /backup/

# 备份配置文件
cp /etc/systemd/system/strm-mount.service /backup/

# 恢复时直接复制回原位置
```

### **运维建议**

#### **1. 定期维护**
```bash
# 每周检查 (crontab)
0 2 * * 0 /usr/local/bin/strm-maintenance.sh

# 维护脚本内容
#!/bin/bash
# 检查挂载状态
mount | grep strm || systemctl restart strm-mount

# 检查日志大小
find /var/log -name "strm-*.log" -size +100M -delete

# 检查缓存文件
find /var/cache/rclone -name "*.json" -mtime +30 -delete
```

#### **2. 性能监控**
```bash
# 关键指标监控
QPS < 0.5              # API 调用频率
缓存命中率 > 90%        # 缓存效率
响应时间 < 100ms       # 目录读取速度
错误率 < 1%            # 系统稳定性
```

#### **3. 容量规划**
```bash
# 缓存空间需求
小型库 (<1000文件): 10MB 缓存空间
中型库 (1000-10000文件): 100MB 缓存空间
大型库 (>10000文件): 1GB 缓存空间

# 日志空间需求
正常运行: 10MB/天
调试模式: 100MB/天
```

### **安全建议**

#### **1. 访问控制**
```bash
# 限制挂载点访问权限
chmod 750 /mnt/strm
chown strm-user:strm-group /mnt/strm

# 使用专用用户运行
useradd -r -s /bin/false strm-mount
```

#### **2. 网络安全**
```bash
# 防火墙规则
iptables -A OUTPUT -d api.123pan.com -j ACCEPT
iptables -A OUTPUT -d webapi.115.com -j ACCEPT

# 限制出站连接
iptables -A OUTPUT -p tcp --dport 443 -j DROP
```

#### **3. 数据保护**
```bash
# 加密敏感配置
rclone config --config=/etc/rclone/rclone.conf

# 设置适当权限
chmod 600 /etc/rclone/rclone.conf
chown root:root /etc/rclone/rclone.conf
```

---

## 📋 **快速参考**

### **常用命令**
```bash
# 启动服务
systemctl start strm-mount

# 检查状态
systemctl status strm-mount

# 查看日志
journalctl -u strm-mount -f

# 重新挂载
umount /mnt/strm && systemctl restart strm-mount

# 检查性能
grep -E "took.*µs" /var/log/strm-mount.log | tail -10
```

### **配置模板**
```bash
# 生产环境
rclone strm-mount 123:Movies /mnt/strm --persistent-cache=true --cache-ttl=6h --min-size=100M --daemon

# 开发环境
rclone strm-mount 123:Test /mnt/strm --persistent-cache=true --cache-ttl=1h --min-size=10M --log-level=DEBUG

# 高性能环境
rclone strm-mount 115:Videos /mnt/strm --persistent-cache=true --cache-ttl=3h --min-size=50M --checkers=2 --daemon
```

### **故障排查清单**
- [ ] 检查挂载状态: `mount | grep strm`
- [ ] 检查进程状态: `ps aux | grep rclone`
- [ ] 检查日志错误: `grep ERROR /var/log/strm-mount.log`
- [ ] 检查网盘连接: `rclone ls 123:Movies --max-depth 1`
- [ ] 检查 QPS 状态: `grep QPS /var/log/strm-mount.log | tail -5`
- [ ] 检查缓存状态: `ls -la /var/cache/rclone/`
- [ ] 检查 Emby 配置: 确认禁用实时监控
- [ ] 测试 STRM 播放: 在 Emby 中播放视频

**通过遵循本指南，您可以构建一个稳定、高效、安全的 STRM-Mount 媒体库系统！** 🎉
