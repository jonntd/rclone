# 网盘媒体库管理完整指南

## 概述

123网盘和115网盘后端现在支持完整的媒体库管理功能，包括：
- `media-sync`：将网盘中的视频文件同步到本地目录，创建 `.strm` 文件
- `get-download-url`：从 `.strm` 文件内容获取实际的下载链接

这些功能特别适用于媒体库管理，如 Plex、Jellyfin、Emby 等媒体服务器。

## 功能特点

### Media-Sync 功能
- ✅ **双网盘支持**：123网盘和115网盘完全支持
- ✅ **智能文件过滤**：自动识别视频文件，支持自定义扩展名
- ✅ **优化的 .strm 内容**：
  - 123网盘：使用 fileId 格式 (`123://fileId`)
  - 115网盘：使用 pick_code 格式 (`115://pick_code`)
- ✅ **递归目录处理**：自动处理子目录结构
- ✅ **大小过滤**：可设置最小文件大小，跳过小文件
- ✅ **预览模式**：支持 dry-run 预览将要创建的文件
- ✅ **详细统计**：提供完整的处理统计信息
- ✅ **安全的同步删除**：默认禁用，限制清理范围，多重安全保护

### Get-Download-URL 功能
- ✅ **多格式输入支持**：
  - `.strm` 文件内容（`123://fileId` 或 `115://pick_code`）
  - 纯 ID（fileId 或 pick_code）
  - 文件路径（自动转换为 ID）
- ✅ **自定义 User-Agent**：支持自定义 UA 字符串
- ✅ **智能处理策略**：
  - 115网盘：pick_code 使用原始HTTP实现，路径使用UA方式处理302重定向
  - 123网盘：路径支持自定义UA，fileId 使用标准API
- ✅ **返回格式一致**：与 `getdownloadurlua` 保持一致的URL字符串返回

## Media-Sync 基本用法

### 命令格式
```bash
# 123网盘
rclone backend media-sync 123:source_path target_path [options]

# 115网盘
rclone backend media-sync 115:source_path target_path [options]
```

### 基本示例

#### 123网盘
```bash
# 同步电影目录
rclone backend media-sync 123:Movies /local/media/movies

# 同步电视剧目录
rclone backend media-sync 123:TVShows /local/media/tvshows

# 使用选项指定目标路径
rclone backend media-sync 123:Videos -o target-path=/local/media/videos
```

#### 115网盘
```bash
# 同步电影目录
rclone backend media-sync 115:Movies /local/media/movies

# 同步电视剧目录
rclone backend media-sync 115:TVShows /local/media/tvshows

# 使用选项指定目标路径
rclone backend media-sync 115:Videos -o target-path=/local/media/videos
```

## 高级选项

### 文件大小过滤
```bash
# 只处理大于 200MB 的文件
rclone backend media-sync 123:Movies /local/media/movies -o min-size=200M

# 只处理大于 1GB 的文件
rclone backend media-sync 123:Movies /local/media/movies -o min-size=1G
```

### 文件类型过滤
```bash
# 只包含特定格式
rclone backend media-sync 123:Movies /local/media/movies -o include=mp4,mkv,avi

# 排除特定格式
rclone backend media-sync 123:Movies /local/media/movies -o exclude=wmv,flv
```

### .strm 文件格式
```bash
# 使用 fileId 格式（推荐，默认）
rclone backend media-sync 123:Movies /local/media/movies -o strm-format=fileid

# 使用文件路径格式（兼容模式）
rclone backend media-sync 123:Movies /local/media/movies -o strm-format=path
```

### 预览模式
```bash
# 预览将要创建的文件，不实际创建
rclone backend media-sync 123:Movies /local/media/movies -o dry-run=true
```

### 同步删除功能（安全模式）
```bash
# 🔧 安全修复：默认禁用同步删除，避免意外删除其他同步任务的文件
# 只创建.strm文件，不删除任何内容（默认安全模式）
rclone backend media-sync 123:Movies /local/media/movies

# 启用同步删除功能（需要明确指定）
rclone backend media-sync 123:Movies /local/media/movies -o sync-delete=true

# 🔒 安全边界：只清理当前同步目录，不影响其他目录
# 例如：只清理 /local/media/movies/Movies/ 目录，不影响其他子目录

# 建议先预览删除操作
rclone backend media-sync 123:Movies /local/media/movies -o sync-delete=true -o dry-run=true
```

## Media-Sync 完整选项列表

| 选项 | 默认值 | 说明 |
|------|--------|------|
| `min-size` | `100M` | 最小文件大小过滤 |
| `strm-format` | `fileid`/`pickcode` | .strm文件内容格式：`fileid`/`pickcode`/`path` |
| `include` | `mp4,mkv,avi,mov,wmv,flv,webm,m4v,3gp,ts,m2ts` | 包含的文件扩展名 |
| `exclude` | _(空)_ | 排除的文件扩展名 |
| `dry-run` | `false` | 预览模式，不实际创建文件 |
| `sync-delete` | `false` | 同步删除：删除本地不存在于网盘的.strm文件和空目录（安全模式） |
| `target-path` | _(空)_ | 目标路径（如果不在参数中指定） |

## Get-Download-URL 功能

### 概述
`get-download-url` 命令可以从 `.strm` 文件内容或文件ID获取实际的下载链接，特别适用于媒体服务器和自动化脚本。

### 命令格式
```bash
# 123网盘
rclone backend get-download-url 123: "input" [options]

# 115网盘
rclone backend get-download-url 115: "input" [options]
```

### 支持的输入格式

#### 123网盘
```bash
# .strm 文件内容格式
rclone backend get-download-url 123: "123://17995550"

# 纯 fileId
rclone backend get-download-url 123: "17995550"

# 文件路径（自动转换为 fileId）
rclone backend get-download-url 123: "/Movies/Action/Movie.mp4"

# 使用自定义 User-Agent（仅路径输入支持）
rclone backend get-download-url 123: "/Movies/Action/Movie.mp4" -o user-agent="Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15"
```

#### 115网盘
```bash
# .strm 文件内容格式
rclone backend get-download-url 115: "115://eybr9y4jowdenzff0"

# 纯 pick_code
rclone backend get-download-url 115: "eybr9y4jowdenzff0"

# 文件路径（使用UA方式，处理302重定向）
rclone backend get-download-url 115: "/Movies/Action/Movie.mp4"

# 使用自定义 User-Agent
rclone backend get-download-url 115: "115://eybr9y4jowdenzff0" -o user-agent="Mozilla/5.0 (Android 14; Mobile; rv:109.0) Gecko/109.0 Firefox/119.0"
```

### Get-Download-URL 选项

| 选项 | 说明 | 支持的后端 |
|------|------|-----------|
| `user-agent` | 自定义User-Agent字符串 | 123网盘（路径输入）、115网盘（全部输入） |

### 技术实现特点

#### 123网盘
- **路径输入**：支持自定义UA，调用 `getDownloadURLByUA` 方法
- **fileId输入**：自动回退到标准API方法
- **智能选择**：根据输入类型自动选择最佳处理方式

#### 115网盘
- **pick_code输入**：使用原始HTTP实现，完全支持自定义UA
- **路径输入**：使用UA方式，处理302重定向，完全支持自定义UA
- **双重策略**：确保最佳兼容性和稳定性

### 返回格式
两个后端都返回简单的URL字符串，与 `getdownloadurlua` 保持一致：
```
https://download-cdn.cjjd19.com/123-438/233e4356/...
https://cdnfhnfile.115cdn.net/5c9752149bbd80536668dd06e9f833f9ce019e86/...
```

## 安全使用指南

### 🔒 同步删除安全机制

#### 默认安全模式
从最新版本开始，`sync-delete` 功能默认**禁用**，确保不会意外删除文件：

```bash
# 默认行为：只创建.strm文件，不删除任何内容
rclone backend media-sync 123:Movies /media/movies
# 输出：🎉 媒体同步完成! 处理目录:5, 处理文件:20, 创建.strm:18, 跳过:2, 错误:0
```

#### 启用删除功能的安全检查
当明确启用 `sync-delete=true` 时，系统会显示多重警告：

```bash
rclone backend media-sync 123:Movies /media/movies -o sync-delete=true
# 输出：
# ⚠️ 警告：已启用同步删除功能，将删除本地不存在于网盘的.strm文件和空目录
# 💡 提示：建议先使用 --dry-run=true 预览删除操作
# 🔒 安全边界：只清理当前同步目录 /media/movies/Movies，不影响其他目录
```

#### 清理范围限制
修复后的清理范围是**高度受限和安全的**：

| 同步命令 | 清理范围 | 安全性 |
|----------|----------|--------|
| `123:Movies → /media/movies` | 仅 `/media/movies/Movies/` | ✅ 安全 |
| `123:Videos → /media/movies` | 仅 `/media/movies/Videos/` | ✅ 安全 |
| `123:Movies/Action → /media/movies` | 仅 `/media/movies/Movies/Action/` | ✅ 安全 |

#### 多网盘同步安全示例
```bash
# 现在是完全安全的！不同网盘同步到同一目录
rclone backend media-sync 123:Movies /media/library -o sync-delete=true
# 清理范围：/media/library/Movies/

rclone backend media-sync 115:Videos /media/library -o sync-delete=true
# 清理范围：/media/library/Videos/

# 两个任务完全隔离，不会互相影响！
```

#### 预览模式强烈推荐
```bash
# 强烈建议先预览删除操作
rclone backend media-sync 123:Movies /media/movies -o sync-delete=true -o dry-run=true
# 输出：
# 🔍 [预览] 将删除孤立的.strm文件: /media/movies/Movies/old_movie.strm
# 🔍 [预览] 将删除空目录: /media/movies/Movies/empty_folder
```

#### 安全使用建议
1. **默认使用安全模式**：不指定 `sync-delete` 参数
2. **预览后再执行**：使用 `dry-run=true` 预览删除操作
3. **独立目标目录**：为不同网盘使用不同的目标目录
4. **定期备份**：重要的 .strm 文件建议定期备份

```bash
# 推荐的安全使用方式
rclone backend media-sync 123:Movies /media/123-movies -o sync-delete=true
rclone backend media-sync 115:Movies /media/115-movies -o sync-delete=true
# 完全隔离，绝对安全
```

## 使用场景

### 1. Plex 媒体库

#### 123网盘
```bash
# 同步电影
rclone backend media-sync 123:Movies /var/lib/plexmediaserver/Movies -o min-size=500M

# 同步电视剧
rclone backend media-sync 123:TVShows /var/lib/plexmediaserver/TVShows -o min-size=100M
```

#### 115网盘
```bash
# 同步电影
rclone backend media-sync 115:Movies /var/lib/plexmediaserver/Movies -o min-size=500M

# 同步电视剧
rclone backend media-sync 115:TVShows /var/lib/plexmediaserver/TVShows -o min-size=100M
```

### 2. Jellyfin 媒体库

#### 123网盘
```bash
# 同步到 Jellyfin 目录
rclone backend media-sync 123:Media /var/lib/jellyfin/media -o strm-format=fileid
```

#### 115网盘
```bash
# 同步到 Jellyfin 目录
rclone backend media-sync 115:Media /var/lib/jellyfin/media -o strm-format=pickcode
```

### 3. 定期同步脚本

#### 双网盘同步脚本
```bash
#!/bin/bash
# sync-media.sh

echo "开始同步网盘媒体库..."

# 123网盘同步
echo "同步123网盘..."
rclone backend media-sync 123:Movies /media/123/movies -o min-size=500M
rclone backend media-sync 123:TVShows /media/123/tvshows -o min-size=100M

# 115网盘同步
echo "同步115网盘..."
rclone backend media-sync 115:Movies /media/115/movies -o min-size=500M
rclone backend media-sync 115:TVShows /media/115/tvshows -o min-size=100M

echo "同步完成！"
```

### 4. 媒体服务器集成

#### 获取播放链接
```bash
# 从 .strm 文件内容获取实际播放链接
STRM_CONTENT=$(cat /media/movies/Action/Movie.strm)
DOWNLOAD_URL=$(rclone backend get-download-url 123: "$STRM_CONTENT")
echo "播放链接: $DOWNLOAD_URL"

# 115网盘示例
STRM_CONTENT=$(cat /media/movies/Action/Movie.strm)
DOWNLOAD_URL=$(rclone backend get-download-url 115: "$STRM_CONTENT")
echo "播放链接: $DOWNLOAD_URL"
```

#### 批量获取下载链接
```bash
#!/bin/bash
# get-download-links.sh

echo "批量获取下载链接..."

# 遍历所有 .strm 文件
find /media -name "*.strm" | while read strm_file; do
    echo "处理文件: $strm_file"

    # 读取 .strm 文件内容
    strm_content=$(cat "$strm_file")

    # 根据内容判断网盘类型并获取下载链接
    if [[ $strm_content == 123://* ]]; then
        download_url=$(rclone backend get-download-url 123: "$strm_content")
        echo "123网盘链接: $download_url"
    elif [[ $strm_content == 115://* ]]; then
        download_url=$(rclone backend get-download-url 115: "$strm_content")
        echo "115网盘链接: $download_url"
    fi
done
```

### 5. 自定义 User-Agent 应用

#### 移动设备模拟
```bash
# 模拟 iPhone Safari
rclone backend get-download-url 115: "115://eybr9y4jowdenzff0" \
  -o user-agent="Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1"

# 模拟 Android Chrome
rclone backend get-download-url 115: "/Movies/Action/Movie.mp4" \
  -o user-agent="Mozilla/5.0 (Linux; Android 14; SM-G998B) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Mobile Safari/537.36"
```

#### 自定义应用标识
```bash
# 自定义媒体服务器标识
rclone backend get-download-url 123: "/Movies/Action/Movie.mp4" \
  -o user-agent="MyMediaServer/1.0 (Custom Download Client)"
```

## 输出示例

### 成功执行
```
2024/01/15 10:30:00 INFO  : 🎬 开始123网盘媒体库同步...
2024/01/15 10:30:00 INFO  : 📋 同步参数: 源=Movies, 目标=/local/media/movies, 最小大小=100M, 格式=fileid, 预览=false
2024/01/15 10:30:01 INFO  : 📁 处理目录: Movies -> /local/media/movies
2024/01/15 10:30:02 INFO  : ✅ 创建.strm文件: /local/media/movies/Action/Movie1.strm (大小: 2.5G, 内容: 123://12345678)
2024/01/15 10:30:03 INFO  : ✅ 创建.strm文件: /local/media/movies/Comedy/Movie2.strm (大小: 1.8G, 内容: 123://87654321)
2024/01/15 10:30:05 INFO  : 🎉 媒体同步完成! 处理目录:3, 处理文件:25, 创建.strm:20, 跳过:5, 错误:0
```

### 预览模式
```
2024/01/15 10:30:00 INFO  : 🎬 开始123网盘媒体库同步...
2024/01/15 10:30:00 INFO  : 📋 同步参数: 源=Movies, 目标=/local/media/movies, 最小大小=100M, 格式=fileid, 预览=true
2024/01/15 10:30:01 INFO  : 🔍 [预览] 将创建目录: /local/media/movies
2024/01/15 10:30:02 INFO  : 🔍 [预览] 将创建.strm文件: /local/media/movies/Movie1.strm (内容: 123://12345678)
2024/01/15 10:30:03 INFO  : 🔍 [预览] 将创建.strm文件: /local/media/movies/Movie2.strm (内容: 123://87654321)
2024/01/15 10:30:05 INFO  : 🎉 媒体同步完成! 处理目录:3, 处理文件:25, 创建.strm:20, 跳过:5, 错误:0
```

## 返回的统计信息

命令执行后会返回 JSON 格式的统计信息：

```json
{
  "processed_dirs": 3,
  "processed_files": 25,
  "created_strm": 20,
  "skipped_files": 5,
  "errors": 0,
  "error_messages": [],
  "dry_run": false
}
```

## 注意事项

1. **权限要求**：确保对目标目录有写入权限
2. **网络稳定性**：大量文件处理时建议在网络稳定的环境下执行
3. **存储空间**：.strm 文件很小，但仍需要确保有足够的存储空间
4. **文件覆盖**：现有的 .strm 文件会被覆盖
5. **中断恢复**：如果中断，重新运行命令会继续处理
6. **🔒 同步删除安全**：
   - 默认禁用同步删除功能，确保数据安全
   - 启用时只影响当前同步的子目录，不会误删其他文件
   - 强烈建议使用 `dry-run=true` 预览删除操作
   - 多网盘同步到同一目录时完全隔离，不会互相影响

## 故障排除

### Media-Sync 常见问题

1. **权限错误**
   ```
   Error: 创建目录失败 /target/path: permission denied
   ```
   解决：检查目标目录的写入权限

2. **网络超时**
   ```
   Error: 列出目录失败 Movies: context deadline exceeded
   ```
   解决：检查网络连接，重试命令

3. **无法获取 fileId/pick_code**
   ```
   WARN: ⚠️ 无法获取fileId，使用路径模式: filename.mp4
   WARN: ⚠️ 无法获取pick_code，使用路径模式: filename.mp4
   ```
   解决：这是正常的回退机制，不影响功能

### Get-Download-URL 常见问题

1. **无效的输入格式**
   ```
   Error: 无效的pick_code: invalid_input
   Error: 无效的fileId: invalid_input
   ```
   解决：检查输入格式是否正确（`123://fileId` 或 `115://pick_code`）

2. **路径转换失败**
   ```
   Error: 路径转换pick_code失败 "/path/to/file": file not found
   ```
   解决：确保文件路径存在且可访问

3. **获取下载URL失败**
   ```
   Error: 获取下载URL失败: unauthorized
   ```
   解决：检查网盘认证状态，重新登录

4. **自定义UA不生效**
   ```
   WARN: ⚠️ 自定义UA仅支持路径输入，fileID输入将使用标准方法
   ```
   解决：对于123网盘，使用路径输入以支持自定义UA

### 调试模式

#### Media-Sync 调试
```bash
# 启用详细日志
rclone backend media-sync 123:Movies /local/media/movies -v
rclone backend media-sync 115:Movies /local/media/movies -v

# 启用调试日志
rclone backend media-sync 123:Movies /local/media/movies -vv
rclone backend media-sync 115:Movies /local/media/movies -vv
```

#### Get-Download-URL 调试
```bash
# 查看详细的处理过程
rclone backend get-download-url 123: "123://17995550" -v
rclone backend get-download-url 115: "115://eybr9y4jowdenzff0" -v

# 查看完整的调试信息（包括User-Agent处理）
rclone backend get-download-url 123: "/path/to/file.mp4" -o user-agent="Custom-UA" -vv
rclone backend get-download-url 115: "/path/to/file.mp4" -o user-agent="Custom-UA" -vv
```

## 功能对比

### Media-Sync vs 原始外部调用

| 特性 | 原始外部调用 | Backend Command |
|------|-------------|-----------------|
| 执行效率 | 低（外部进程） | 高（内部API） |
| .strm 内容 | 简单路径 | 优化的 fileId/pick_code |
| 错误处理 | 基础 | 完善的重试和恢复 |
| 配置灵活性 | 硬编码 | 丰富的选项 |
| 集成度 | 外部工具 | 原生 rclone 功能 |
| 性能监控 | 有限 | 详细的统计和日志 |
| 网盘支持 | 单一 | 123网盘 + 115网盘 |

### Get-Download-URL vs getdownloadurlua

| 特性 | getdownloadurlua | get-download-url |
|------|------------------|------------------|
| 输入格式 | 仅路径 | 路径 + ID + .strm内容 |
| 网盘支持 | 单一后端 | 123网盘 + 115网盘 |
| 自定义UA | 支持 | 完全支持 + 智能选择 |
| 返回格式 | URL字符串 | URL字符串（一致） |
| 错误处理 | 基础 | 增强的错误处理和回退 |
| 技术实现 | 单一方式 | 多重策略（原始HTTP + UA方式） |
| 调试支持 | 有限 | 详细的日志和调试信息 |

## 总结

这套完整的媒体库管理功能包括：

1. **Media-Sync**：创建 `.strm` 文件，支持123网盘和115网盘
2. **Get-Download-URL**：从 `.strm` 内容获取实际下载链接，支持多种输入格式

### 核心优势

- ✅ **双网盘支持**：123网盘和115网盘完全支持
- ✅ **完整工作流**：从同步到播放的完整解决方案
- ✅ **高性能**：原生 rclone 功能，无外部依赖
- ✅ **灵活配置**：丰富的选项和自定义能力
- ✅ **智能处理**：自动选择最佳策略和回退机制
- ✅ **详细监控**：完整的日志和统计信息
- ✅ **数据安全**：默认安全模式，限制清理范围，多重保护机制

### 实际价值

这套功能完全替代了原始的外部脚本调用方式，为媒体服务器提供了：
- 更好的性能和稳定性
- 更丰富的功能和配置选项
- 更好的用户体验和调试能力
- 统一的接口和一致的行为

**现在您可以使用 rclone 原生功能轻松管理123网盘和115网盘的媒体库！** 🎉
