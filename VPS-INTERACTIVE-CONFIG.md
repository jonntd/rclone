# STRM-Mount VPS 交互式配置指南

## 🎯 配置方式对比

| 配置方式 | 适用场景 | 特点 |
|---------|---------|------|
| **config-wizard** | 首次部署 | 🚀 快速向导，预设场景 |
| **config** | 精细调整 | ⚙️ 交互式菜单，逐项配置 |
| **config-set** | 脚本自动化 | 🤖 命令行直接设置 |
| **config-edit** | 高级用户 | 📝 直接编辑配置文件 |

## 🚀 快速配置向导

### 使用场景
首次部署时，快速选择常见配置场景。

```bash
sudo ./strm-vps.sh config-wizard
```

### 配置场景

#### 1. 默认配置
```
基础挂载目录: /mnt/cloud
缓存目录: /var/cache/rclone-strm
日志目录: /var/log/rclone-strm
持久化缓存: /var/lib/rclone/strm-cache
```

#### 2. 数据盘配置
```
输入数据盘挂载点: /data

结果:
基础挂载目录: /data/cloud
缓存目录: /data/cache/rclone
日志目录: /data/logs/rclone
持久化缓存: /data/lib/rclone/cache
```

#### 3. 高性能配置
```
输入SSD挂载点: /ssd
输入HDD挂载点: /hdd

结果:
基础挂载目录: /hdd/cloud      (大容量存储)
缓存目录: /ssd/rclone-cache   (高速缓存)
日志目录: /hdd/logs/rclone    (日志存储)
持久化缓存: /ssd/rclone-persistent (高速持久化)
```

#### 4. 自定义配置
进入交互式配置菜单，逐项设置。

## ⚙️ 交互式配置菜单

### 使用场景
需要精细调整配置时使用。

```bash
sudo ./strm-vps.sh config
```

### 菜单界面
```
=== STRM-Mount 配置管理 ===

当前配置:
  1. rclone 路径: /usr/local/bin/rclone
  2. 基础挂载目录: /mnt/cloud
  3. 缓存目录: /var/cache/rclone-strm
  4. 日志目录: /var/log/rclone-strm
  5. 持久化缓存目录: /var/lib/rclone/strm-cache
  6. 运行用户: rclone

操作选项:
  [1-6] 修改对应配置项
  [s]   保存配置并退出
  [r]   重置为默认配置
  [q]   退出不保存

请选择操作 [1-6/s/r/q]:
```

### 操作说明

#### 修改配置项 [1-6]
- 选择数字修改对应配置
- 输入新值或回车保持不变
- 自动验证输入有效性

#### 保存配置 [s]
- 将当前配置保存到文件
- 提示需要重启服务

#### 重置配置 [r]
- 恢复所有默认值
- 需要确认操作

#### 退出 [q]
- 不保存直接退出

## 🤖 命令行配置

### 使用场景
脚本自动化或快速修改单个配置项。

```bash
# 设置基础挂载目录
sudo ./strm-vps.sh config-set BASE_MOUNT_DIR "/data/cloud"

# 设置日志目录
sudo ./strm-vps.sh config-set LOG_DIR "/data/logs/rclone"

# 设置缓存目录
sudo ./strm-vps.sh config-set CACHE_DIR "/ssd/cache"
```

### 可配置项
- `BASE_MOUNT_DIR`: 基础挂载目录
- `LOG_DIR`: 日志目录
- `CACHE_DIR`: 缓存目录
- `PERSISTENT_CACHE_DIR`: 持久化缓存目录
- `RCLONE_USER`: 运行用户
- `RCLONE`: rclone可执行文件路径

## 📝 直接编辑配置文件

### 使用场景
高级用户需要批量修改或添加自定义配置。

```bash
sudo ./strm-vps.sh config-edit
```

### 配置文件位置
```
/etc/rclone-strm/config
```

### 配置文件格式
```bash
# Rclone STRM-Mount VPS 配置文件

# rclone 可执行文件路径
RCLONE="/usr/local/bin/rclone"

# 基础挂载目录
BASE_MOUNT_DIR="/data/cloud"

# 缓存目录
CACHE_DIR="/data/cache/rclone-strm"

# 日志目录
LOG_DIR="/data/logs/rclone"

# 持久化缓存目录
PERSISTENT_CACHE_DIR="/data/lib/rclone/strm-cache"

# 系统用户
RCLONE_USER="rclone"
RCLONE_GROUP="rclone"

# 网盘配置
REMOTES="115:115网盘:50M 123:123网盘:50M"
```

## 📊 配置查看

### 查看当前配置
```bash
sudo ./strm-vps.sh config-show
```

### 输出示例
```
=== 当前配置 ===

配置文件: /etc/rclone-strm/config
RCLONE="/usr/local/bin/rclone"
BASE_MOUNT_DIR="/data/cloud"
CACHE_DIR="/data/cache/rclone-strm"
LOG_DIR="/data/logs/rclone"
PERSISTENT_CACHE_DIR="/data/lib/rclone/strm-cache"
RCLONE_USER="rclone"
RCLONE_GROUP="rclone"

运行时配置:
  rclone 路径: /usr/local/bin/rclone
  基础挂载目录: /data/cloud
  115 挂载点: /data/cloud/115
  123 挂载点: /data/cloud/123
  缓存目录: /data/cache/rclone-strm
  日志目录: /data/logs/rclone
  运行用户: rclone
```

## 🔄 完整配置流程

### 1. 首次部署
```bash
# 1. 初始化环境
sudo ./strm-vps.sh init

# 2. 快速配置向导
sudo ./strm-vps.sh config-wizard

# 3. 配置rclone认证
sudo -u rclone rclone config

# 4. 启动服务
sudo ./strm-vps.sh start
```

### 2. 配置调整
```bash
# 1. 交互式配置
sudo ./strm-vps.sh config

# 2. 重启服务使配置生效
sudo ./strm-vps.sh restart

# 3. 验证状态
sudo ./strm-vps.sh status
```

### 3. 自动化部署
```bash
# 脚本化配置
sudo ./strm-vps.sh init
sudo ./strm-vps.sh config-set BASE_MOUNT_DIR "/data/cloud"
sudo ./strm-vps.sh config-set LOG_DIR "/data/logs"
sudo ./strm-vps.sh config-set CACHE_DIR "/ssd/cache"

# 启动服务
sudo ./strm-vps.sh start
sudo ./strm-vps.sh install-service
sudo ./strm-vps.sh enable-service
```

## 💡 最佳实践

### 1. 配置规划
- **挂载点**: 使用大容量存储 (HDD)
- **缓存**: 使用高速存储 (SSD)
- **日志**: 可以使用普通存储
- **持久化缓存**: 推荐使用SSD

### 2. 目录权限
- 确保目录存在且可写
- rclone用户需要访问权限
- 避免使用系统关键目录

### 3. 配置验证
```bash
# 配置后验证
sudo ./strm-vps.sh config-show
sudo ./strm-vps.sh status

# 测试挂载
sudo ./strm-vps.sh start
ls -la /your/mount/point/
```

### 4. 故障排除
```bash
# 检查配置文件
cat /etc/rclone-strm/config

# 检查目录权限
ls -la /your/mount/dir/
ls -la /your/cache/dir/

# 重新初始化
sudo ./strm-vps.sh init
```
