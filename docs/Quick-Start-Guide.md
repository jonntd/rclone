# STRM-Mount 快速开始指南

## 🚀 **5分钟快速部署**

### **步骤1: 安装 rclone**
```bash
# Linux/macOS
curl https://rclone.org/install.sh | sudo bash

# 或手动下载
wget https://github.com/rclone/rclone/releases/latest/download/rclone-linux-amd64.zip
unzip rclone-linux-amd64.zip
sudo cp rclone-*/rclone /usr/local/bin/
```

### **步骤2: 配置网盘**
```bash
# 运行配置向导
rclone config

# 选择网盘类型
# 123网盘: 选择 "123"
# 115网盘: 选择 "115"
# 按提示输入账号密码
```

### **步骤3: 启动 STRM-Mount**
```bash
# 基本启动 (前台运行)
rclone strm-mount 123:Movies /mnt/strm --persistent-cache=true --min-size=50M

# 后台运行 (推荐)
rclone strm-mount 123:Movies /mnt/strm \
  --persistent-cache=true \
  --min-size=50M \
  --daemon \
  --log-file=/var/log/strm-mount.log
```

### **步骤4: 验证效果**
```bash
# 检查挂载
mount | grep strm

# 查看生成的 STRM 文件
ls -la /mnt/strm/

# 检查 STRM 内容
cat /mnt/strm/movie.strm
# 输出: 123://fileID
```

### **步骤5: 配置 Emby**
```bash
# 1. 添加媒体库
路径: /mnt/strm
类型: 电影/电视节目

# 2. 关键设置
实时监控: 禁用 ❌
定期扫描: 禁用 ❌  
启动扫描: 禁用 ❌
扫描间隔: 24小时

# 3. 手动扫描
在 Emby 管理界面手动触发扫描
```

---

## ⚙️ **推荐配置**

### **123网盘 (极保守)**
```bash
rclone strm-mount 123:Movies /mnt/strm \
  --persistent-cache=true \
  --cache-ttl=3h \
  --min-size=50M \
  --checkers=1 \
  --transfers=1 \
  --daemon \
  --log-file=/var/log/strm-123.log
```

### **115网盘 (保守)**
```bash
rclone strm-mount 115:Videos /mnt/strm \
  --persistent-cache=true \
  --cache-ttl=2h \
  --min-size=100M \
  --checkers=2 \
  --transfers=1 \
  --daemon \
  --log-file=/var/log/strm-115.log
```

---

## 🛡️ **QPS 保护说明**

### **自动保护机制**
- ✅ **智能限制**: 自动检测网盘类型，应用最佳 QPS 参数
- ✅ **缓存优先**: 优先使用缓存，减少 API 调用
- ✅ **动态调整**: 根据使用情况动态调整刷新频率
- ✅ **实时监控**: 监控 QPS 状态，自动阻止过频调用

### **保护效果**
```bash
# 正常运行状态
📊 [REFRESH-LIMIT] 统计: QPS=0.23 (安全)
💾 [CACHE] 缓存命中: 响应时间 2ms
🛡️ [QPS] 保护已启用: 123网盘, QPS阈值=0.2
```

---

## 📏 **文件大小过滤**

### **过滤规则**
```bash
--min-size=50M   # 小于50MB的文件完全隐藏
                 # 大于50MB的视频文件转换为STRM
                 # 大于50MB的非视频文件正常显示
```

### **推荐设置**
| 场景 | 推荐大小 | 效果 |
|------|----------|------|
| **电影库** | `--min-size=500M` | 只显示完整电影 |
| **电视剧库** | `--min-size=100M` | 过滤片头片尾 |
| **综合库** | `--min-size=50M` | 平衡效果 |

---

## 🎬 **Emby 优化配置**

### **关键设置**
```json
{
  "LibraryOptions": {
    "EnableRealtimeMonitor": false,           // 🔴 必须禁用
    "EnablePeriodicScanning": false,          // 🔴 必须禁用
    "ScanOnStartup": false,                   // 🔴 必须禁用
    "EnableChapterImageExtraction": false,    // 禁用缩略图
    "EnableTrickplayImageExtraction": false   // 禁用预览图
  },
  "ScheduledTasks": {
    "LibraryScan": {
      "IntervalHours": 24                     // 24小时扫描一次
    }
  }
}
```

### **应用配置**
```bash
# 1. 备份现有配置
cp /path/to/emby/config.json /path/to/emby/config.json.backup

# 2. 应用优化配置
# 将上述 JSON 配置应用到 Emby 设置中

# 3. 重启 Emby
systemctl restart emby-server
```

---

## 🔍 **常见问题**

### **Q: STRM 文件无法播放？**
```bash
# 检查 STRM 文件格式
cat /mnt/strm/movie.strm
# 应该输出: 123://fileID 或 115://pickCode

# 检查网盘连接
rclone ls 123:Movies | head -5
```

### **Q: Emby 扫描很慢？**
```bash
# 确认已禁用实时监控
grep "EnableRealtimeMonitor" /path/to/emby/config.json
# 应该显示: "EnableRealtimeMonitor": false

# 检查扫描并发数
"MaxConcurrentScans": 1
```

### **Q: 出现 QPS 限制？**
```bash
# 检查当前 QPS
grep "QPS" /var/log/strm-mount.log | tail -5

# 增加缓存时间
--cache-ttl=6h

# 减少并发
--checkers=1 --transfers=1
```

### **Q: 缓存命中率低？**
```bash
# 检查缓存状态
grep -E "(Hit|Miss)" /var/log/strm-mount.log | tail -10

# 增加缓存时间
--cache-ttl=3h

# 启用持久化缓存
--persistent-cache=true
```

---

## 📊 **监控命令**

### **基本监控**
```bash
# 检查挂载状态
mount | grep strm

# 查看实时日志
tail -f /var/log/strm-mount.log

# 检查 QPS 状态
grep "QPS" /var/log/strm-mount.log | tail -5

# 检查缓存效果
grep -E "(Hit|Miss)" /var/log/strm-mount.log | tail -10
```

### **性能监控**
```bash
# 响应时间统计
grep "took.*µs" /var/log/strm-mount.log | tail -10

# 错误检查
grep -E "(ERROR|CRITICAL)" /var/log/strm-mount.log

# 系统资源
htop
df -h /mnt/strm
```

---

## 🏆 **成功指标**

### **健康状态**
- ✅ **QPS < 0.5**: API 调用频率安全
- ✅ **缓存命中率 > 90%**: 缓存效率高
- ✅ **响应时间 < 100ms**: 目录读取快速
- ✅ **错误率 < 1%**: 系统稳定运行

### **正常日志示例**
```bash
✅ [CACHE] 新缓存创建完成: 288 个文件, 耗时 11.3s
📊 [REFRESH-LIMIT] 统计: QPS=0.23, 缓存命中率=95%
💾 [CACHE] 缓存命中: /Movies (响应时间: 2ms)
🛡️ [QPS] 保护已启用: 123网盘, 当前安全
```

---

## 🎯 **下一步**

### **进阶配置**
- 📖 阅读完整指南: `docs/STRM-Mount-Complete-Guide.md`
- 🔧 系统服务配置: 设置开机自启动
- 📊 监控告警: 配置 QPS 监控脚本
- 🔄 自动化运维: 设置定期维护任务

### **扩展功能**
- 🎬 多媒体库支持: 电影、电视剧、动漫分离
- 🌐 多网盘支持: 同时挂载多个网盘
- 📱 移动端优化: 针对移动设备的配置优化
- 🔐 安全加固: 访问控制和数据加密

**恭喜！您已经成功部署了 STRM-Mount 系统！** 🎉

现在您可以享受：
- 🎬 **无缝播放**: 网盘视频直接在 Emby 中播放
- 🛡️ **QPS 保护**: 自动避免网盘 API 限制
- ⚡ **极速响应**: 缓存机制提供毫秒级响应
- 🧹 **清洁界面**: 只显示有意义的视频文件

如有问题，请参考完整指南或查看日志进行排查。
