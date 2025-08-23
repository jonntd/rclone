# STRM-Mount QPS 安全部署指南

## 🎯 **概述**

本指南提供完整的 STRM-Mount + Emby 部署方案，确保不触发网盘 QPS 限制，适用于生产环境。

---

## ⚠️ **风险评估**

### **QPS 限制对比**
| 网盘 | 估计QPS限制 | 风险等级 | 推荐策略 |
|------|-------------|----------|----------|
| **123网盘** | 10-20/秒 | 🔴 高风险 | 极保守配置 |
| **115网盘** | 20-50/秒 | 🟡 中风险 | 保守配置 |
| **其他网盘** | 未知 | 🔴 高风险 | 最保守配置 |

### **风险场景**
- ✅ **低风险**: <100个文件，手动扫描，单用户
- ⚠️ **中风险**: 100-1000个文件，定时扫描，少量用户
- 🔴 **高风险**: >1000个文件，实时监控，多用户并发

---

## 🚀 **快速部署**

### **步骤1: 使用 QPS 安全启动脚本**

```bash
# 123网盘示例
./scripts/strm-mount-qps-safe.sh 123 Movies /mnt/123-movies 50M

# 115网盘示例  
./scripts/strm-mount-qps-safe.sh 115 /电影 /mnt/115-movies 100M
```

### **步骤2: 应用 Emby 优化配置**

```bash
# 备份现有配置
cp /path/to/emby/config.json /path/to/emby/config.json.backup

# 应用优化配置
cp configs/emby-strm-optimized.json /path/to/emby/config.json

# 重启 Emby 服务
systemctl restart emby-server
```

### **步骤3: 启动 QPS 监控**

```bash
# 启动实时监控
./scripts/monitor-qps.sh

# 后台监控
nohup ./scripts/monitor-qps.sh > /tmp/qps-monitor.log 2>&1 &
```

---

## 🔧 **详细配置**

### **STRM-Mount 参数优化**

#### **123网盘 (极保守配置)**
```bash
rclone strm-mount 123:Movies /mnt/strm \
  --persistent-cache=true \
  --cache-ttl=3h \              # 3小时缓存
  --min-size=50M \              # 50MB最小文件
  --checkers=1 \                # 单线程检查
  --transfers=1 \               # 单线程传输
  --vfs-cache-mode=minimal \    # 最小缓存
  --buffer-size=0 \             # 无缓冲
  --vfs-read-ahead=0 \          # 无预读
  --log-level=INFO \
  --daemon
```

#### **115网盘 (保守配置)**
```bash
rclone strm-mount 115:Videos /mnt/strm \
  --persistent-cache=true \
  --cache-ttl=2h \              # 2小时缓存
  --min-size=100M \             # 100MB最小文件
  --checkers=2 \                # 双线程检查
  --transfers=1 \               # 单线程传输
  --vfs-cache-mode=minimal \
  --buffer-size=0 \
  --vfs-read-ahead=0 \
  --log-level=INFO \
  --daemon
```

### **Emby 关键配置**

#### **必须禁用的功能**
```json
{
  "EnableRealtimeMonitor": false,           // 🔴 关键：禁用实时监控
  "EnableChapterImageExtraction": false,    // 禁用章节图像
  "EnableTrickplayImageExtraction": false,  // 禁用预览图
  "EnablePeriodicScanning": false,          // 禁用定期扫描
  "ScanOnStartup": false                    // 禁用启动扫描
}
```

#### **扫描策略优化**
```json
{
  "ScheduledTasks": {
    "LibraryScan": {
      "IntervalHours": 24,                  // 24小时扫描一次
      "MaxConcurrentScans": 1               // 限制并发扫描
    }
  }
}
```

---

## 📊 **监控和告警**

### **QPS 监控指标**

| 指标 | 安全阈值 | 警告阈值 | 危险阈值 |
|------|----------|----------|----------|
| **1分钟QPS** | <3 | 3-5 | >5 |
| **5分钟平均QPS** | <2 | 2-4 | >4 |
| **缓存命中率** | >90% | 80-90% | <80% |

### **监控命令**

```bash
# 实时监控面板
./scripts/monitor-qps.sh

# 检查 API 调用频率
grep -E "\[API\]|\[SYNC\]" /tmp/strm-mount-qps-safe.log | tail -20

# 检查缓存命中率
grep -E "(Hit|Miss)" /tmp/strm-mount-qps-safe.log | tail -20

# 检查错误日志
grep -E "(ERROR|CRITICAL)" /tmp/strm-mount-qps-safe.log | tail -10
```

### **告警脚本**

```bash
#!/bin/bash
# qps-alert.sh - QPS 告警脚本

QPS_THRESHOLD=5
LOG_FILE="/tmp/strm-mount-qps-safe.log"

# 计算最近1分钟的 QPS
recent_qps=$(grep -E "\[API\]|\[SYNC\]" "$LOG_FILE" | \
  tail -60 | wc -l)

if [[ $recent_qps -gt $QPS_THRESHOLD ]]; then
    echo "🚨 QPS 告警: 当前 QPS = $recent_qps (阈值: $QPS_THRESHOLD)"
    # 发送邮件/短信/Webhook 通知
    # curl -X POST "https://your-webhook-url" -d "QPS Alert: $recent_qps"
fi
```

---

## 🛡️ **应急处理**

### **QPS 限制触发时的处理步骤**

#### **立即响应 (5分钟内)**
```bash
# 1. 停止 Emby 扫描
systemctl stop emby-server

# 2. 停止 STRM-Mount
./scripts/unmount-strm.sh -f

# 3. 检查网盘账户状态
rclone ls 123:Movies --max-depth 1  # 测试是否被限制
```

#### **短期缓解 (1小时内)**
```bash
# 1. 增加缓存时间
--cache-ttl=6h  # 增加到6小时

# 2. 减少并发
--checkers=1 --transfers=1

# 3. 重新启动（保守模式）
./scripts/strm-mount-qps-safe.sh 123 Movies /mnt/strm 100M
```

#### **长期优化 (24小时内)**
```bash
# 1. 分析访问模式
grep -E "\[API\]" /tmp/strm-mount-qps-safe.log | \
  awk '{print $1, $2}' | sort | uniq -c

# 2. 优化文件结构
# 将大量小文件合并到子目录

# 3. 实施分批扫描
# 分时段扫描不同的媒体库
```

---

## 📈 **性能优化**

### **缓存策略优化**

#### **小型媒体库 (<100个文件)**
```bash
--cache-ttl=1h          # 1小时缓存
--min-size=10M          # 10MB最小文件
```

#### **中型媒体库 (100-1000个文件)**
```bash
--cache-ttl=3h          # 3小时缓存
--min-size=50M          # 50MB最小文件
```

#### **大型媒体库 (>1000个文件)**
```bash
--cache-ttl=6h          # 6小时缓存
--min-size=100M         # 100MB最小文件
```

### **Emby 扫描策略**

#### **分时段扫描**
```bash
# 凌晨2点扫描电影
0 2 * * * /usr/bin/emby-scan-library "Movies"

# 凌晨3点扫描电视剧
0 3 * * * /usr/bin/emby-scan-library "TV Shows"

# 凌晨4点扫描动漫
0 4 * * * /usr/bin/emby-scan-library "Anime"
```

#### **增量扫描**
```bash
# 只扫描新增和修改的文件
emby-scan-library --incremental "Movies"
```

---

## 🔍 **故障排除**

### **常见问题**

#### **1. QPS 限制触发**
**症状**: API 调用返回 429 错误或请求超时
**解决**:
```bash
# 检查当前 QPS
./scripts/monitor-qps.sh

# 增加缓存时间
--cache-ttl=12h

# 等待限制解除 (通常1-24小时)
```

#### **2. 缓存命中率低**
**症状**: 大量 API 调用，响应缓慢
**解决**:
```bash
# 检查缓存配置
grep "cache-ttl" /tmp/strm-mount-qps-safe.log

# 增加缓存时间
--cache-ttl=6h

# 检查文件访问模式
grep "Hit\|Miss" /tmp/strm-mount-qps-safe.log | tail -50
```

#### **3. Emby 扫描过于频繁**
**症状**: 持续的高 QPS
**解决**:
```bash
# 检查 Emby 扫描日志
tail -f /var/log/emby/emby.log | grep -i scan

# 禁用实时监控
"EnableRealtimeMonitor": false

# 增加扫描间隔
"IntervalHours": 48
```

---

## 📋 **部署检查清单**

### **部署前检查**
- [ ] 网盘配置正确 (`rclone config show`)
- [ ] 挂载点目录存在且可写
- [ ] 防火墙允许相关端口
- [ ] 磁盘空间充足 (缓存文件)

### **配置检查**
- [ ] STRM-Mount 使用 QPS 安全参数
- [ ] Emby 禁用实时监控
- [ ] Emby 禁用缩略图生成
- [ ] 扫描间隔设置为24小时以上
- [ ] 启用持久化缓存

### **监控检查**
- [ ] QPS 监控脚本运行正常
- [ ] 日志文件正确生成
- [ ] 告警机制配置完成
- [ ] 缓存命中率 >80%

### **测试检查**
- [ ] 小规模测试 (<10个文件)
- [ ] 监控 QPS 变化
- [ ] 验证 Emby 扫描正常
- [ ] 验证视频播放正常

---

## 🎯 **最佳实践总结**

### **配置原则**
1. **保守优先**: 宁可性能差一点，也不要触发限制
2. **监控为王**: 实时监控 QPS，及时发现问题
3. **分阶段部署**: 从小规模开始，逐步扩大
4. **应急预案**: 准备好应急处理流程

### **运维建议**
1. **定期检查**: 每周检查 QPS 趋势
2. **配置调优**: 根据使用情况调整参数
3. **日志分析**: 定期分析访问模式
4. **备份配置**: 保存工作正常的配置

### **性能平衡**
- **高性能**: 短缓存时间，快速更新，高 QPS 风险
- **高安全**: 长缓存时间，慢速更新，低 QPS 风险
- **推荐**: 中等缓存时间 (2-3小时)，平衡性能和安全

**遵循本指南可以最大程度避免 QPS 限制，确保 STRM-Mount 稳定运行！** 🎉
