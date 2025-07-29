# 统一 Media-Sync 功能使用指南

## 概述

rclone 的 `media-sync` backend command 为 123 和 115 网盘提供了统一的媒体库同步功能。该功能可以将网盘中的视频文件同步到本地目录，并创建对应的 `.strm` 文件，特别适用于 Plex、Jellyfin、Emby 等媒体服务器。

## 🎯 统一特性

### 通用命令格式
```bash
rclone backend media-sync "后端:源路径" "目标路径" [选项]
```

### 统一选项
| 选项 | 默认值 | 说明 |
|------|--------|------|
| `min-size` | `100M` | 最小文件大小过滤 |
| `strm-format` | `fileid` | .strm文件内容格式：`fileid`/`path` |
| `include` | `mp4,mkv,avi,mov,wmv,flv,webm,m4v,3gp,ts,m2ts` | 包含的文件扩展名 |
| `exclude` | _(空)_ | 排除的文件扩展名 |
| `dry-run` | `false` | 预览模式，不实际创建文件 |
| `target-path` | _(空)_ | 目标路径（如果不在参数中指定） |

## 🚀 快速开始

### 123网盘示例
```bash
# 基本同步
rclone backend media-sync "123:Movies" "/local/media/movies"

# 带选项的同步
rclone backend media-sync "123:Movies" "/local/media/movies" \
  -o min-size=200M \
  -o strm-format=fileid \
  -o dry-run=true
```

### 115网盘示例
```bash
# 基本同步
rclone backend media-sync "115:Videos" "/local/media/videos"

# 带选项的同步
rclone backend media-sync "115:Videos" "/local/media/videos" \
  -o min-size=200M \
  -o strm-format=fileid \
  -o dry-run=true
```

## 📊 .strm 文件格式对比

| 后端 | fileid 格式 | 实际内容 | 说明 |
|------|-------------|----------|------|
| **123网盘** | `123://fileId` | `123://17995550` | 使用文件的唯一ID |
| **115网盘** | `115://pick_code` | `115://abc123def456` | 使用文件的pick_code |

### 格式选项
```bash
# 推荐：使用 fileid 格式（统一名称）
-o strm-format=fileid

# 兼容：使用文件路径
-o strm-format=path

# 115网盘兼容旧格式名称
-o strm-format=pickcode  # 等同于 fileid
```

## 🎬 媒体服务器集成

### Plex 媒体库
```bash
# 电影库
rclone backend media-sync "123:Movies" "/var/lib/plexmediaserver/Movies" \
  -o min-size=500M -o strm-format=fileid

# 电视剧库
rclone backend media-sync "115:TVShows" "/var/lib/plexmediaserver/TVShows" \
  -o min-size=100M -o strm-format=fileid
```

### Jellyfin 媒体库
```bash
# 统一同步到 Jellyfin
rclone backend media-sync "123:Media" "/var/lib/jellyfin/media/123" \
  -o strm-format=fileid

rclone backend media-sync "115:Media" "/var/lib/jellyfin/media/115" \
  -o strm-format=fileid
```

### Emby 媒体库
```bash
# 同步到 Emby
rclone backend media-sync "123:Collections" "/var/lib/emby/media/123" \
  -o strm-format=fileid
```

## 🔧 高级用法

### 文件过滤
```bash
# 只处理大文件
rclone backend media-sync "123:Movies" "/local/media" \
  -o min-size=1G

# 只包含特定格式
rclone backend media-sync "115:Videos" "/local/media" \
  -o include=mp4,mkv,avi

# 排除特定格式
rclone backend media-sync "123:Downloads" "/local/media" \
  -o exclude=wmv,flv,3gp
```

### 预览模式
```bash
# 预览将要创建的文件
rclone backend media-sync "123:Movies" "/local/media" \
  -o dry-run=true -v
```

### 使用 target-path 选项
```bash
# 通过选项指定目标路径
rclone backend media-sync "115:Videos" \
  -o target-path="/local/media/videos" \
  -o strm-format=fileid
```

## 📁 目录结构保持

两个后端都会保持完整的网盘目录结构：

### 输入
```
123:/Movies/
├── Action/
│   ├── Movie1.mp4
│   └── Movie2.mkv
└── Comedy/
    └── Movie3.avi
```

### 输出
```
/local/media/
└── Movies/
    ├── Action/
    │   ├── Movie1.strm
    │   └── Movie2.strm
    └── Comedy/
        └── Movie3.strm
```

## 🔄 批量同步脚本

### 多后端同步脚本
```bash
#!/bin/bash
# unified-media-sync.sh

echo "开始统一媒体库同步..."

# 123网盘同步
echo "同步123网盘..."
rclone backend media-sync "123:Movies" "/media/123/movies" \
  -o min-size=500M -o strm-format=fileid

rclone backend media-sync "123:TVShows" "/media/123/tvshows" \
  -o min-size=100M -o strm-format=fileid

# 115网盘同步
echo "同步115网盘..."
rclone backend media-sync "115:Videos" "/media/115/videos" \
  -o min-size=200M -o strm-format=fileid

rclone backend media-sync "115:Series" "/media/115/series" \
  -o min-size=100M -o strm-format=fileid

echo "同步完成！"
```

## 📊 统计信息

所有命令都会返回统一格式的 JSON 统计信息：

```json
{
  "processed_dirs": 5,
  "processed_files": 25,
  "created_strm": 20,
  "skipped_files": 5,
  "errors": 0,
  "error_messages": [],
  "dry_run": false
}
```

## 🛠️ 故障排除

### 常见问题

1. **缺少目标路径**
   ```
   Error: 需要提供目标路径作为参数或通过 --target-path 选项指定
   ```
   解决：添加目标路径参数

2. **权限错误**
   ```
   Error: 创建目录失败: permission denied
   ```
   解决：检查目标目录的写入权限

3. **网络超时**
   ```
   Error: 列出目录失败: context deadline exceeded
   ```
   解决：检查网络连接，重试命令

### 调试模式
```bash
# 启用详细日志
rclone backend media-sync "123:Movies" "/local/media" -v

# 启用调试日志
rclone backend media-sync "115:Videos" "/local/media" -vv
```

## 🎉 优势总结

### 统一体验
- ✅ **相同的命令格式**：两个后端使用完全相同的语法
- ✅ **统一的选项名称**：`strm-format=fileid` 适用于所有后端
- ✅ **一致的行为**：相同的目录结构保持和错误处理

### 性能优势
- 🚀 **内部API调用**：比外部脚本快数倍
- 💾 **缓存利用**：充分利用 rclone 的缓存机制
- 🔄 **并发控制**：智能的并发和限流控制

### 功能丰富
- 🎯 **智能过滤**：大小、类型、扩展名多维度过滤
- 🔍 **预览模式**：安全的 dry-run 功能
- 📊 **详细统计**：完整的处理统计和错误报告
- 🛡️ **错误恢复**：单个文件错误不影响整体处理

这个统一的 media-sync 功能为用户提供了一致、高效、功能丰富的媒体库管理体验，无论使用哪个网盘后端都能享受相同的便利性。
