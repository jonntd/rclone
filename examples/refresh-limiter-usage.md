# STRM-Mount 刷新限制器使用指南

## 🎯 **概述**

刷新限制器是 STRM-Mount 内置的智能 QPS 保护机制，可以从源头控制文件夹刷新触发，避免网盘 API 限制。

---

## 🔧 **基本使用**

### **1. 默认配置 (推荐)**

```bash
# 使用默认的智能刷新限制
rclone strm-mount 123:Movies /mnt/strm \
  --persistent-cache=true \
  --cache-ttl=3h
```

**默认行为**:
- ✅ **自动检测网盘类型**: 123网盘使用极保守配置，115网盘使用保守配置
- ✅ **智能间隔控制**: 最小1-2小时，最大6-12小时
- ✅ **QPS 阈值保护**: 0.2-0.5 QPS 自动限制
- ✅ **变更率分析**: 基于历史变更率智能跳过

### **2. 自定义配置**

```bash
# 极保守配置 (123网盘推荐)
rclone strm-mount 123:Movies /mnt/strm \
  --persistent-cache=true \
  --cache-ttl=6h \
  --refresh-limit-min-interval=3h \
  --refresh-limit-max-interval=24h \
  --refresh-limit-qps-threshold=0.1

# 平衡配置 (115网盘推荐)
rclone strm-mount 115:Videos /mnt/strm \
  --persistent-cache=true \
  --cache-ttl=2h \
  --refresh-limit-min-interval=1h \
  --refresh-limit-max-interval=8h \
  --refresh-limit-qps-threshold=0.3

# 禁用刷新限制 (测试环境)
rclone strm-mount 123:Test /mnt/strm \
  --persistent-cache=true \
  --cache-ttl=30m \
  --refresh-limit-enabled=false
```

---

## 📊 **配置参数详解**

### **基础参数**

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `--refresh-limit-enabled` | `true` | 启用/禁用刷新限制 |
| `--refresh-limit-min-interval` | `1h` (123: `2h`) | 最小刷新间隔 |
| `--refresh-limit-max-interval` | `6h` (123: `12h`) | 最大刷新间隔 |
| `--refresh-limit-qps-threshold` | `0.5` (123: `0.2`) | QPS 阈值 |
| `--refresh-limit-change-rate-threshold` | `0.1` (123: `0.05`) | 变更率阈值 |

### **高级参数**

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `--refresh-limit-request-merging` | `true` | 启用请求合并 |
| `--refresh-limit-predictive-cache` | `false` | 启用预测性缓存 |
| `--refresh-limit-access-window` | `24h` | 访问模式分析窗口 |
| `--refresh-limit-stats-interval` | `10m` | 统计报告间隔 |

---

## 🛡️ **保护机制详解**

### **1. 时间间隔限制**

```
上次刷新时间: 14:00:00
当前时间:     15:30:00
间隔:         1.5小时
最小间隔:     2小时

结果: 🚫 阻止刷新 (间隔不足)
```

### **2. QPS 动态限制**

```
当前 QPS: 0.8
阈值:     0.5

结果: 🚫 阻止刷新 (QPS 过高)
动态间隔: 原间隔 × (0.8/0.5) = 原间隔 × 1.6
```

### **3. 智能历史分析**

```
目录: /Movies/Action/
总刷新次数: 20
实际变更次数: 1
变更率: 5%
阈值: 10%

结果: 🚫 智能跳过 (变更率过低)
```

### **4. 访问模式预测**

```
最近访问: [09:00, 12:00, 15:00, 18:00]
访问频率: 3小时
预测下次: 21:00
当前时间: 19:30

结果: 🚫 模式跳过 (非预期访问时间)
```

---

## 📈 **监控和调试**

### **1. 实时统计**

```bash
# 查看实时日志
tail -f /var/log/strm-mount.log | grep "REFRESH-LIMIT"

# 输出示例
📊 [REFRESH-LIMIT] 统计: 目录=5, 允许=12, 阻止=38, QPS=0.23, 平均变更率=8.5%, 访问模式=5
✅ [REFRESH-LIMIT] 允许刷新: /Movies (QPS: 0.15)
🚫 [REFRESH-LIMIT] 间隔限制: /TV Shows (上次: 45m前, 需要: 2h)
⚠️ [REFRESH-LIMIT] QPS 限制: 0.65 > 0.50, 延迟刷新: /Anime
🧠 [REFRESH-LIMIT] 智能跳过: /Documentaries (低变更率)
```

### **2. 统计信息解读**

```
📊 [REFRESH-LIMIT] 统计: 目录=5, 允许=12, 阻止=38, QPS=0.23, 平均变更率=8.5%, 访问模式=5
                          ↑      ↑       ↑       ↑        ↑                ↑
                        监控目录  允许刷新  阻止刷新  当前QPS   平均变更率      访问模式数
```

**健康指标**:
- ✅ **阻止率 > 70%**: 保护机制有效工作
- ✅ **QPS < 0.5**: API 调用频率安全
- ✅ **变更率 < 20%**: 大部分刷新是不必要的

---

## 🎯 **最佳实践**

### **1. 网盘特定配置**

#### **123网盘 (极保守)**
```bash
rclone strm-mount 123:Movies /mnt/strm \
  --persistent-cache=true \
  --cache-ttl=6h \
  --refresh-limit-min-interval=4h \
  --refresh-limit-max-interval=24h \
  --refresh-limit-qps-threshold=0.1 \
  --refresh-limit-change-rate-threshold=0.03
```

#### **115网盘 (保守)**
```bash
rclone strm-mount 115:Videos /mnt/strm \
  --persistent-cache=true \
  --cache-ttl=3h \
  --refresh-limit-min-interval=2h \
  --refresh-limit-max-interval=12h \
  --refresh-limit-qps-threshold=0.3 \
  --refresh-limit-change-rate-threshold=0.08
```

### **2. 场景特定配置**

#### **生产环境 (稳定优先)**
```bash
--refresh-limit-min-interval=6h
--refresh-limit-max-interval=48h
--refresh-limit-qps-threshold=0.1
--refresh-limit-change-rate-threshold=0.02
```

#### **开发环境 (灵活优先)**
```bash
--refresh-limit-min-interval=30m
--refresh-limit-max-interval=4h
--refresh-limit-qps-threshold=1.0
--refresh-limit-change-rate-threshold=0.5
```

#### **测试环境 (禁用限制)**
```bash
--refresh-limit-enabled=false
```

---

## 🔍 **故障排除**

### **常见问题**

#### **1. 缓存更新太慢**
**症状**: 新文件很久才出现在 STRM 挂载点
**原因**: 刷新间隔设置过长
**解决**:
```bash
# 减少最小刷新间隔
--refresh-limit-min-interval=30m

# 或临时禁用限制
--refresh-limit-enabled=false
```

#### **2. QPS 限制过于频繁**
**症状**: 大量 "QPS 限制" 日志
**原因**: QPS 阈值设置过低
**解决**:
```bash
# 提高 QPS 阈值
--refresh-limit-qps-threshold=1.0

# 或增加刷新间隔
--refresh-limit-min-interval=3h
```

#### **3. 智能跳过过于激进**
**症状**: 有变更的目录不刷新
**原因**: 变更率阈值设置过高
**解决**:
```bash
# 降低变更率阈值
--refresh-limit-change-rate-threshold=0.3

# 或禁用智能跳过
--refresh-limit-change-rate-threshold=1.0
```

### **调试模式**

```bash
# 启用详细日志
rclone strm-mount 123:Movies /mnt/strm \
  --persistent-cache=true \
  --log-level=DEBUG \
  --log-file=/tmp/strm-debug.log

# 查看刷新限制相关日志
grep "REFRESH-LIMIT" /tmp/strm-debug.log
```

---

## 📋 **配置模板**

### **极保守模板 (123网盘推荐)**
```bash
#!/bin/bash
rclone strm-mount 123:Movies /mnt/strm \
  --persistent-cache=true \
  --cache-ttl=8h \
  --refresh-limit-enabled=true \
  --refresh-limit-min-interval=6h \
  --refresh-limit-max-interval=48h \
  --refresh-limit-qps-threshold=0.05 \
  --refresh-limit-change-rate-threshold=0.02 \
  --refresh-limit-request-merging=true \
  --refresh-limit-stats-interval=30m \
  --log-level=INFO \
  --log-file=/var/log/strm-mount-123.log \
  --daemon
```

### **平衡模板 (115网盘推荐)**
```bash
#!/bin/bash
rclone strm-mount 115:Videos /mnt/strm \
  --persistent-cache=true \
  --cache-ttl=4h \
  --refresh-limit-enabled=true \
  --refresh-limit-min-interval=2h \
  --refresh-limit-max-interval=16h \
  --refresh-limit-qps-threshold=0.2 \
  --refresh-limit-change-rate-threshold=0.05 \
  --refresh-limit-request-merging=true \
  --refresh-limit-stats-interval=15m \
  --log-level=INFO \
  --log-file=/var/log/strm-mount-115.log \
  --daemon
```

**通过这些内置的刷新限制机制，可以在 rclone 内部智能地控制 QPS，提供多层保护！** 🛡️✨
