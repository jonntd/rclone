# VPS专用115网盘配置指南

## 🎯 VPS配置对照表

| VPS类型 | 内存 | 硬盘 | 推荐配置 | 说明 |
|---------|------|------|----------|------|
| **超低配VPS** | 500MB-1GB | 5-10GB | 极省内存模式 | 你的情况 |
| **低配VPS** | 1-2GB | 10-20GB | 省内存模式 | 便宜VPS |
| **中配VPS** | 2-4GB | 20-50GB | 平衡模式 | 一般VPS |
| **高配VPS** | ≥4GB | ≥50GB | 高性能模式 | 贵VPS |

## ⚡ VPS专用命令

### 🔥 超低配VPS（你的500MB内存+5GB硬盘）
```bash
# 极省内存模式 - 专为VPS优化
rclone copy "你的文件" 115:目标文件夹/ \
  --buffer-size=4M \
  --max-buffer-memory=32M \
  --transfers=1 \
  --checkers=1 \
  --low-level-retries=1 \
  -v
```

### 💻 低配VPS（1-2GB内存）
```bash
# 省内存模式
rclone copy "你的文件" 115:目标文件夹/ \
  --buffer-size=8M \
  --max-buffer-memory=64M \
  --transfers=1 \
  --checkers=2 \
  -v
```

### 🖥️ 中配VPS（2-4GB内存）
```bash
# 平衡模式
rclone copy "你的文件" 115:目标文件夹/ \
  --buffer-size=16M \
  --max-buffer-memory=128M \
  --transfers=2 \
  --checkers=4 \
  -v
```

### 🚀 高配VPS（≥4GB内存）
```bash
# 高性能模式
rclone copy "你的文件" 115:目标文件夹/ \
  --buffer-size=32M \
  --max-buffer-memory=256M \
  --transfers=2 \
  --checkers=8 \
  -v
```

## 📊 参数说明（VPS专用）

| 参数 | 作用 | 超低配 | 低配 | 中配 | 高配 |
|------|------|--------|------|------|------|
| `--buffer-size` | 每次读取数据块大小 | 4MB | 8MB | 16MB | 32MB |
| `--max-buffer-memory` | 最大内存使用 | 32MB | 64MB | 128MB | 256MB |
| `--transfers` | 同时传输文件数 | 1个 | 1个 | 2个 | 2个 |
| `--checkers` | 检查文件线程数 | 1个 | 2个 | 4个 | 8个 |

## ⚠️ 重要说明

### 关于多线程参数
```bash
# ❌ 这些参数对115网盘无效（115不支持多线程上传）
--multi-thread-cutoff=200M     # 无效
--multi-thread-streams=4       # 无效
```

**原因**：115网盘使用单线程分片上传，不支持rclone的多线程上传功能。

### 有效的参数列表
```bash
# ✅ 这些参数对115网盘有效
--buffer-size=4M              # 缓冲区大小
--max-buffer-memory=32M       # 最大内存限制
--transfers=1                 # 同时传输文件数
--checkers=1                  # 文件检查线程数
--low-level-retries=1         # 底层重试次数
--retries=3                   # 高层重试次数
--timeout=5m                  # 超时时间
--contimeout=60s              # 连接超时
-v                           # 详细日志
--log-file=upload.log         # 日志文件
```

## 🛠️ VPS实用命令

### 监控内存使用
```bash
# 上传前检查内存
free -h

# 上传时监控内存（另开终端）
watch -n 1 'free -h && ps aux | grep rclone'
```

### 后台上传
```bash
# 后台运行上传
nohup rclone copy "你的文件" 115:目标文件夹/ \
  --buffer-size=4M \
  --max-buffer-memory=32M \
  --transfers=1 \
  --checkers=1 \
  --log-file=upload.log \
  -v > upload_output.log 2>&1 &

# 查看进度
tail -f upload.log
```

### 断点续传
```bash
# 115网盘自动跳过已存在文件，天然支持断点续传
rclone copy "文件夹" 115:目标文件夹/ \
  --buffer-size=4M \
  --max-buffer-memory=32M \
  --transfers=1 \
  -v
```

## 🔧 VPS优化技巧

### 1. 内存不够时
```bash
# 创建swap文件（临时增加虚拟内存）
sudo fallocate -l 1G /swapfile
sudo chmod 600 /swapfile
sudo mkswap /swapfile
sudo swapon /swapfile

# 确认swap生效
free -h
```

### 2. 硬盘空间不够时
```bash
# 流式上传（不占用本地硬盘）
rclone copy remote1:source/ 115:target/ \
  --buffer-size=4M \
  --max-buffer-memory=32M \
  --transfers=1 \
  -v
```

### 3. 网络不稳定时
```bash
# 增加重试和超时设置
rclone copy "你的文件" 115:目标文件夹/ \
  --buffer-size=4M \
  --max-buffer-memory=32M \
  --transfers=1 \
  --retries=5 \
  --low-level-retries=3 \
  --timeout=10m \
  --contimeout=60s \
  -v
```

## 📝 配置文件方式

创建 `~/.config/rclone/rclone.conf`：

```ini
[115]
type = 115
# ... 你的115网盘登录信息 ...

# VPS超低配优化设置
buffer_size = 4M
max_buffer_memory = 32M
transfers = 1
checkers = 1
low_level_retries = 1
```

## ✅ 快速开始

**你的VPS（500MB内存+5GB硬盘）直接用这个命令**：

```bash
rclone copy "你的文件" 115:目标文件夹/ \
  --buffer-size=4M \
  --max-buffer-memory=32M \
  --transfers=1 \
  --checkers=1 \
  -v
```

这个配置只会使用约32MB内存，完全适合你的VPS！
