# 115网盘媒体库管理完整指南

## 概述

115网盘后端现在支持完整的媒体库管理功能，包括：
- `media-sync`：将网盘中的视频文件同步到本地目录，创建 `.strm` 文件
- `get-download-url`：从 `.strm` 文件内容获取实际的下载链接

这些功能特别适用于媒体库管理，如 Plex、Jellyfin、Emby 等媒体服务器。

## 功能特点

### Media-Sync 功能
- ✅ **智能文件过滤**：自动识别视频文件，支持自定义扩展名
- ✅ **优化的 .strm 内容**：使用 115网盘的 pick_code 格式 (`115://pick_code`)
- ✅ **递归目录处理**：自动处理子目录结构
- ✅ **大小过滤**：可设置最小文件大小，跳过小文件
- ✅ **预览模式**：支持 dry-run 预览将要创建的文件
- ✅ **详细统计**：提供完整的处理统计信息
- ✅ **pick_code 自动获取**：智能获取文件的 pick_code
- ✅ **安全的同步删除**：默认禁用，限制清理范围，多重安全保护

### Get-Download-URL 功能
- ✅ **多格式输入支持**：
  - `.strm` 文件内容（`115://pick_code`）
  - 纯 pick_code
  - 文件路径（自动转换为 pick_code）
- ✅ **自定义 User-Agent**：完全支持自定义 UA 字符串
- ✅ **双重处理策略**：
  - pick_code 输入：原始HTTP实现，直接在请求头中使用自定义UA
  - 路径输入：UA方式，处理302重定向，完全支持自定义UA
- ✅ **返回格式一致**：与 `getdownloadurlua` 保持一致的URL字符串返回
- ✅ **智能回退机制**：确保最佳兼容性和稳定性

## 基本用法

### 命令格式
```bash
rclone backend media-sync 115:source_path target_path [options]
```

### 基本示例
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
rclone backend media-sync 115:Movies /local/media/movies -o min-size=200M

# 只处理大于 1GB 的文件
rclone backend media-sync 115:Movies /local/media/movies -o min-size=1G
```

### 文件类型过滤
```bash
# 只包含特定格式
rclone backend media-sync 115:Movies /local/media/movies -o include=mp4,mkv,avi

# 排除特定格式
rclone backend media-sync 115:Movies /local/media/movies -o exclude=wmv,flv
```

### .strm 文件格式
```bash
# 使用 fileid 格式（推荐，默认，实际使用 pick_code）
rclone backend media-sync 115:Movies /local/media/movies -o strm-format=fileid

# 使用文件路径格式（兼容模式）
rclone backend media-sync 115:Movies /local/media/movies -o strm-format=path

# 兼容旧格式名称
rclone backend media-sync 115:Movies /local/media/movies -o strm-format=pickcode
```

### 预览模式
```bash
# 预览将要创建的文件，不实际创建
rclone backend media-sync 115:Movies /local/media/movies -o dry-run=true
```

### 同步删除功能（安全模式）
```bash
# 🔧 安全修复：默认禁用同步删除，避免意外删除其他同步任务的文件
# 只创建.strm文件，不删除任何内容（默认安全模式）
rclone backend media-sync 115:Movies /local/media/movies

# 启用同步删除功能（需要明确指定）
rclone backend media-sync 115:Movies /local/media/movies -o sync-delete=true

# 🔒 安全边界：只清理当前同步目录，不影响其他目录
# 例如：只清理 /local/media/movies/Movies/ 目录，不影响其他子目录

# 建议先预览删除操作
rclone backend media-sync 115:Movies /local/media/movies -o sync-delete=true -o dry-run=true
```

## Media-Sync 完整选项列表

| 选项 | 默认值 | 说明 |
|------|--------|------|
| `min-size` | `100M` | 最小文件大小过滤 |
| `strm-format` | `fileid` | .strm文件内容格式：`fileid`/`path` (兼容: `pickcode`) |
| `include` | `mp4,mkv,avi,mov,wmv,flv,webm,m4v,3gp,ts,m2ts` | 包含的文件扩展名 |
| `exclude` | _(空)_ | 排除的文件扩展名 |
| `dry-run` | `false` | 预览模式，不实际创建文件 |
| `sync-delete` | `false` | 同步删除：删除本地不存在于网盘的.strm文件和空目录（安全模式） |
| `target-path` | _(空)_ | 目标路径（如果不在参数中指定） |

## Get-Download-URL 功能

### 概述
`get-download-url` 命令可以从 `.strm` 文件内容或 pick_code 获取实际的下载链接，特别适用于媒体服务器和自动化脚本。115网盘的实现具有独特的优势，支持原始HTTP实现和UA方式处理302重定向。

### 命令格式
```bash
rclone backend get-download-url 115: "input" [options]
```

### 支持的输入格式

#### .strm 文件内容格式
```bash
# 从 .strm 文件内容获取下载链接
rclone backend get-download-url 115: "115://eybr9y4jowdenzff0"
```

#### 纯 pick_code
```bash
# 直接使用 pick_code
rclone backend get-download-url 115: "eybr9y4jowdenzff0"
```

#### 文件路径（UA方式，处理302重定向）
```bash
# 使用文件路径，自动处理302重定向
rclone backend get-download-url 115: "/Movies/Action/Movie.mp4"
```

#### 自定义 User-Agent
```bash
# pick_code 输入 + 自定义UA（原始HTTP实现）
rclone backend get-download-url 115: "115://eybr9y4jowdenzff0" \
  -o user-agent="Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1"

# 路径输入 + 自定义UA（UA方式，处理302重定向）
rclone backend get-download-url 115: "/Movies/Action/Movie.mp4" \
  -o user-agent="Mozilla/5.0 (Android 14; Mobile; rv:109.0) Gecko/109.0 Firefox/119.0"
```

### Get-Download-URL 选项

| 选项 | 说明 |
|------|------|
| `user-agent` | 自定义User-Agent字符串，支持所有输入格式 |

### 技术实现特点

#### 双重处理策略
1. **pick_code 输入**：使用原始HTTP实现
   - 直接发送POST请求到 `/open/ufile/downurl`
   - 手动设置 Authorization 和 User-Agent 头
   - 使用自定义响应结构解析
   - 避免框架层复杂性，更稳定可靠

2. **路径输入**：使用UA方式
   - 调用 `getDownloadURLByUA` 方法
   - 处理302重定向
   - 获取最终可播放的URL

#### 智能选择机制
- 根据输入格式自动选择最佳处理方式
- pick_code 输入优先使用原始HTTP实现
- 路径输入自动使用UA方式处理302重定向

### 返回格式
返回简单的URL字符串，与 `getdownloadurlua` 保持一致：
```
https://cdnfhnfile.115cdn.net/5c9752149bbd80536668dd06e9f833f9ce019e86/...
```

## 115网盘特色功能

### pick_code 优势
115网盘的 pick_code 是文件的唯一标识符，具有以下优势：
- **永久有效**：不会因为文件移动而失效
- **直接访问**：可以直接通过 pick_code 访问文件
- **高效播放**：媒体服务器可以直接使用 pick_code 播放

### 智能 pick_code 获取
```bash
# 系统会自动尝试多种方式获取 pick_code：
# 1. 从文件对象直接获取（最快）
# 2. 通过 GetPickCodeByPath API 获取
# 3. 回退到文件路径模式（兼容性）
```

## 安全使用指南

### 🔒 同步删除安全机制

#### 默认安全模式
从最新版本开始，`sync-delete` 功能默认**禁用**，确保不会意外删除文件：

```bash
# 默认行为：只创建.strm文件，不删除任何内容
rclone backend media-sync 115:Movies /media/movies
# 输出：🎉 媒体同步完成! 处理目录:5, 处理文件:20, 创建.strm:18, 跳过:2, 错误:0
```

#### 启用删除功能的安全检查
当明确启用 `sync-delete=true` 时，系统会显示多重警告：

```bash
rclone backend media-sync 115:Movies /media/movies -o sync-delete=true
# 输出：
# ⚠️ 警告：已启用同步删除功能，将删除本地不存在于网盘的.strm文件和空目录
# 💡 提示：建议先使用 --dry-run=true 预览删除操作
# 🔒 安全边界：只清理当前同步目录 /media/movies/Movies，不影响其他目录
```

#### 清理范围限制
修复后的清理范围是**高度受限和安全的**：

| 同步命令 | 清理范围 | 安全性 |
|----------|----------|--------|
| `115:Movies → /media/movies` | 仅 `/media/movies/Movies/` | ✅ 安全 |
| `115:Videos → /media/movies` | 仅 `/media/movies/Videos/` | ✅ 安全 |
| `115:Movies/Action → /media/movies` | 仅 `/media/movies/Movies/Action/` | ✅ 安全 |

#### 多网盘同步安全示例
```bash
# 现在是完全安全的！不同网盘同步到同一目录
rclone backend media-sync 115:Movies /media/library -o sync-delete=true
# 清理范围：/media/library/Movies/

rclone backend media-sync 123:Videos /media/library -o sync-delete=true
# 清理范围：/media/library/Videos/

# 两个任务完全隔离，不会互相影响！
```

#### 预览模式强烈推荐
```bash
# 强烈建议先预览删除操作
rclone backend media-sync 115:Movies /media/movies -o sync-delete=true -o dry-run=true
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
rclone backend media-sync 115:Movies /media/115-movies -o sync-delete=true
rclone backend media-sync 123:Movies /media/123-movies -o sync-delete=true
# 完全隔离，绝对安全
```

## 使用场景

### 1. Plex 媒体库
```bash
# 同步电影
rclone backend media-sync 115:Movies /var/lib/plexmediaserver/Movies -o min-size=500M

# 同步电视剧
rclone backend media-sync 115:TVShows /var/lib/plexmediaserver/TVShows -o min-size=100M
```

### 2. Jellyfin 媒体库
```bash
# 同步到 Jellyfin 目录
rclone backend media-sync 115:Media /var/lib/jellyfin/media -o strm-format=fileid
```

### 3. 定期同步脚本
```bash
#!/bin/bash
# sync-115-media.sh

echo "开始同步115网盘媒体库..."

# 同步电影
echo "同步电影..."
rclone backend media-sync 115:Movies /media/movies -o min-size=500M

# 同步电视剧
echo "同步电视剧..."
rclone backend media-sync 115:TVShows /media/tvshows -o min-size=100M

# 同步纪录片
echo "同步纪录片..."
rclone backend media-sync 115:Documentaries /media/documentaries -o min-size=200M

echo "同步完成！"
```

### 4. 媒体服务器集成

#### 获取播放链接
```bash
# 从 .strm 文件内容获取实际播放链接
STRM_CONTENT=$(cat /media/movies/Action/Movie.strm)
DOWNLOAD_URL=$(rclone backend get-download-url 115: "$STRM_CONTENT")
echo "播放链接: $DOWNLOAD_URL"

# 使用自定义User-Agent
DOWNLOAD_URL=$(rclone backend get-download-url 115: "$STRM_CONTENT" \
  -o user-agent="MyMediaServer/1.0")
echo "播放链接: $DOWNLOAD_URL"
```

#### 批量获取下载链接
```bash
#!/bin/bash
# get-115-download-links.sh

echo "批量获取115网盘下载链接..."

# 遍历所有 .strm 文件
find /media -name "*.strm" | while read strm_file; do
    echo "处理文件: $strm_file"

    # 读取 .strm 文件内容
    strm_content=$(cat "$strm_file")

    # 检查是否为115网盘格式
    if [[ $strm_content == 115://* ]]; then
        # 使用原始HTTP实现获取下载链接
        download_url=$(rclone backend get-download-url 115: "$strm_content")
        echo "115网盘链接: $download_url"

        # 保存到文件
        echo "$download_url" > "${strm_file%.strm}.url"
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

#### 桌面浏览器模拟
```bash
# Windows Chrome
rclone backend get-download-url 115: "eybr9y4jowdenzff0" \
  -o user-agent="Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

# macOS Safari
rclone backend get-download-url 115: "/Movies/Action/Movie.mp4" \
  -o user-agent="Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.2 Safari/605.1.15"
```

#### 自定义应用标识
```bash
# 自定义媒体服务器标识
rclone backend get-download-url 115: "115://eybr9y4jowdenzff0" \
  -o user-agent="MyMediaServer/1.0 (115 Download Client)"

# 爬虫标识
rclone backend get-download-url 115: "/Movies/Action/Movie.mp4" \
  -o user-agent="MediaBot/2.0 (+https://example.com/bot)"
```

### 6. 高级调试和监控

#### 详细日志查看
```bash
# 查看User-Agent处理过程
rclone backend get-download-url 115: "115://eybr9y4jowdenzff0" \
  -o user-agent="Custom-UA" -vv

# 查看302重定向处理
rclone backend get-download-url 115: "/Movies/Action/Movie.mp4" -vv
```

#### 性能测试
```bash
#!/bin/bash
# performance-test.sh

echo "115网盘下载链接获取性能测试..."

# 测试pick_code方式（原始HTTP实现）
start_time=$(date +%s.%N)
rclone backend get-download-url 115: "115://eybr9y4jowdenzff0" > /dev/null
end_time=$(date +%s.%N)
pickcode_time=$(echo "$end_time - $start_time" | bc)
echo "pick_code方式耗时: ${pickcode_time}秒"

# 测试路径方式（UA方式）
start_time=$(date +%s.%N)
rclone backend get-download-url 115: "/Movies/Action/Movie.mp4" > /dev/null
end_time=$(date +%s.%N)
path_time=$(echo "$end_time - $start_time" | bc)
echo "路径方式耗时: ${path_time}秒"
```

## 输出示例

### 成功执行
```
2024/01/15 10:30:00 INFO  : 🎬 开始115网盘媒体库同步...
2024/01/15 10:30:00 INFO  : 📋 同步参数: 源=Movies, 目标=/local/media/movies, 最小大小=100M, 格式=pickcode, 预览=false
2024/01/15 10:30:01 INFO  : 📁 处理目录: Movies -> /local/media/movies
2024/01/15 10:30:02 INFO  : ✅ 创建.strm文件: /local/media/movies/Action/Movie1.strm (大小: 2.5G, 内容: 115://abc123def456)
2024/01/15 10:30:03 INFO  : ✅ 创建.strm文件: /local/media/movies/Comedy/Movie2.strm (大小: 1.8G, 内容: 115://xyz789uvw012)
2024/01/15 10:30:05 INFO  : 🎉 媒体同步完成! 处理目录:3, 处理文件:25, 创建.strm:20, 跳过:5, 错误:0
```

### pick_code 获取过程
```
2024/01/15 10:30:02 DEBUG : 📁 处理目录: Movies/Action -> /local/media/movies/Action
2024/01/15 10:30:02 DEBUG : 🔍 获取pick_code: Movie1.mp4
2024/01/15 10:30:02 INFO  : ✅ 创建.strm文件: /local/media/movies/Action/Movie1.strm (大小: 2.5G, 内容: 115://abc123def456)
```

### 回退到路径模式
```
2024/01/15 10:30:03 DEBUG : ⚠️ 无法获取pick_code，使用路径模式: Movie2.mp4
2024/01/15 10:30:03 INFO  : ✅ 创建.strm文件: /local/media/movies/Movie2.strm (大小: 1.8G, 内容: Movies/Movie2.mp4)
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

1. **Cookie 有效性**：确保 115网盘的 Cookie 有效且未过期
2. **API 限制**：115网盘有 QPS 限制，大量文件处理时会自动控制速度
3. **pick_code 获取**：某些文件可能无法获取 pick_code，会自动回退到路径模式
4. **网络稳定性**：建议在网络稳定的环境下执行
5. **存储空间**：.strm 文件很小，但仍需要确保有足够的存储空间
6. **🔒 同步删除安全**：
   - 默认禁用同步删除功能，确保数据安全
   - 启用时只影响当前同步的子目录，不会误删其他文件
   - 强烈建议使用 `dry-run=true` 预览删除操作
   - 多网盘同步到同一目录时完全隔离，不会互相影响

## 故障排除

### Media-Sync 常见问题

1. **Cookie 过期**
   ```
   Error: 401 Unauthorized
   ```
   解决：更新 115网盘的 Cookie 配置

2. **QPS 限制**
   ```
   Error: 429 Too Many Requests
   ```
   解决：系统会自动重试，或增加 `--115-pacer-min-sleep` 参数

3. **无法获取 pick_code**
   ```
   WARN: ⚠️ 无法获取pick_code，使用路径模式: filename.mp4
   ```
   解决：这是正常的回退机制，不影响功能

### Get-Download-URL 常见问题

1. **无效的 pick_code 格式**
   ```
   Error: 无效的pick_code: invalid_input
   ```
   解决：检查输入格式是否正确（`115://pick_code` 或纯 pick_code）

2. **路径转换失败**
   ```
   Error: 路径转换pick_code失败 "/path/to/file": file not found
   ```
   解决：确保文件路径存在且可访问

3. **原始HTTP请求失败**
   ```
   Error: 创建请求失败: invalid request
   Error: 请求失败: network error
   ```
   解决：检查网络连接和115网盘认证状态

4. **解析响应失败**
   ```
   Error: 解析响应失败: invalid JSON
   ```
   解决：可能是API响应格式变化，尝试使用路径输入方式

5. **未找到下载URL**
   ```
   Error: 未找到下载URL
   ```
   解决：文件可能不存在或无权限访问，检查pick_code有效性

6. **302重定向处理失败**
   ```
   Error: UA方式获取下载URL失败: redirect error
   ```
   解决：尝试使用不同的User-Agent或检查文件路径

### 调试模式

#### Media-Sync 调试
```bash
# 启用详细日志
rclone backend media-sync 115:Movies /local/media/movies -v

# 启用调试日志
rclone backend media-sync 115:Movies /local/media/movies -vv
```

#### Get-Download-URL 调试
```bash
# 查看详细的处理过程
rclone backend get-download-url 115: "115://eybr9y4jowdenzff0" -v

# 查看完整的调试信息（包括HTTP请求详情）
rclone backend get-download-url 115: "115://eybr9y4jowdenzff0" \
  -o user-agent="Custom-UA" -vv

# 查看302重定向处理过程
rclone backend get-download-url 115: "/Movies/Action/Movie.mp4" -vv
```

## 功能对比

### Media-Sync: 115网盘 vs 123网盘

| 特性 | 123网盘 | 115网盘 | 说明 |
|------|---------|---------|------|
| 文件标识符 | fileId | pick_code | 115的pick_code更稳定 |
| .strm 格式 | `123://fileId` | `115://pick_code` | 不同的协议前缀 |
| API 限制 | 相对宽松 | 较严格的QPS限制 | 115需要更保守的调用频率 |
| 获取方式 | 直接从对象 | 多重回退机制 | 115有更复杂的获取逻辑 |
| 稳定性 | 高 | 高 | 两者都很稳定 |

### Get-Download-URL: 115网盘独特优势

| 特性 | 115网盘实现 | 优势说明 |
|------|-------------|----------|
| 输入格式支持 | pick_code + 路径 + .strm内容 | 最全面的输入格式支持 |
| 自定义UA | 完全支持所有输入格式 | 任何输入都可使用自定义UA |
| 技术实现 | 原始HTTP + UA方式双重策略 | 最佳兼容性和稳定性 |
| 302重定向 | 完美处理 | 路径输入自动处理302重定向 |
| 错误处理 | 智能回退机制 | 多重策略确保成功率 |
| 调试支持 | 详细的日志记录 | 完整的处理过程可视化 |

### Get-Download-URL vs getdownloadurlua

| 特性 | getdownloadurlua | get-download-url (115) |
|------|------------------|----------------------|
| 输入格式 | 仅路径 | 路径 + pick_code + .strm内容 |
| 自定义UA | 支持 | 完全支持 + 智能选择 |
| 返回格式 | URL字符串 | URL字符串（一致） |
| 错误处理 | 基础 | 增强的错误处理和回退 |
| 技术实现 | 单一方式 | 双重策略（原始HTTP + UA方式） |
| 调试支持 | 有限 | 详细的日志和调试信息 |

## 最佳实践

### Media-Sync 最佳实践
1. **批量处理**：对于大量文件，建议分批处理
2. **定期同步**：设置定时任务定期同步新文件
3. **监控日志**：关注 pick_code 获取失败的情况
4. **备份配置**：定期备份 rclone 配置文件
5. **测试环境**：先在小范围测试，确认效果后再大规模使用

### Get-Download-URL 最佳实践
1. **输入格式选择**：
   - 批量处理：使用 pick_code 格式（原始HTTP实现，更快）
   - 单个文件：使用路径格式（UA方式，处理302重定向）
   - .strm文件：直接使用文件内容
2. **User-Agent 策略**：
   - 移动设备：使用移动浏览器UA
   - 桌面应用：使用桌面浏览器UA
   - 自定义应用：使用自定义标识
3. **错误处理**：
   - 启用详细日志（-vv）查看处理过程
   - 网络问题时重试
   - 使用不同输入格式作为备选方案
4. **性能优化**：
   - pick_code 方式性能最佳
   - 路径方式适合处理302重定向
   - 根据实际需求选择合适的方式

## 总结

115网盘的媒体库管理功能包括：

1. **Media-Sync**：创建 `.strm` 文件，使用稳定的 pick_code 格式
2. **Get-Download-URL**：从 `.strm` 内容获取实际下载链接，支持多种输入格式和自定义UA

### 核心优势

- ✅ **pick_code 稳定性**：115网盘的 pick_code 是永久有效的文件标识符
- ✅ **双重处理策略**：原始HTTP实现 + UA方式，确保最佳兼容性
- ✅ **完全的UA支持**：所有输入格式都支持自定义User-Agent
- ✅ **智能回退机制**：多重策略确保高成功率
- ✅ **详细的调试支持**：完整的日志记录和错误处理
- ✅ **数据安全**：默认安全模式，限制清理范围，多重保护机制

### 实际价值

这套功能为115网盘用户提供了：
- 强大的媒体库管理能力
- 稳定可靠的 .strm 文件创建
- 灵活的下载链接获取方式
- 完整的自定义和调试支持

**现在您可以使用 rclone 原生功能轻松管理115网盘的媒体库！** 🎉
