package _123

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/rclone/rclone/fs"
)

// MediaSyncStats 媒体同步统计信息
type MediaSyncStats struct {
	ProcessedDirs  int      `json:"processed_dirs"`
	ProcessedFiles int      `json:"processed_files"`
	CreatedStrm    int      `json:"created_strm"`
	SkippedFiles   int      `json:"skipped_files"`
	Errors         int      `json:"errors"`
	ErrorMessages  []string `json:"error_messages,omitempty"`
	DryRun         bool     `json:"dry_run"`
}

// mediaSyncCommand 实现媒体库同步功能
func (f *Fs) mediaSyncCommand(ctx context.Context, args []string, opt map[string]string) (interface{}, error) {
	fs.Infof(f, "🎬 开始123网盘媒体库同步...")

	// 1. 参数解析和验证
	var sourcePath, targetPath string

	// rclone backend 命令的参数解析：
	// 当使用 "rclone backend media-sync 123:/path /target" 时
	// 123:/path 会被解析为后端路径，args 中只包含 /target
	if len(args) >= 1 {
		targetPath = args[0]
		// 源路径使用当前文件系统的根路径
		sourcePath = ""
	} else {
		// 如果没有参数，检查是否通过选项指定
		if tp, ok := opt["target-path"]; ok {
			targetPath = tp
			sourcePath = ""
		} else {
			return nil, fmt.Errorf("需要提供目标路径作为参数或通过 --target-path 选项指定")
		}
	}

	// 2. 选项解析
	minSize, err := f.parseSize(opt["min-size"], "100M")
	if err != nil {
		return nil, fmt.Errorf("解析 min-size 失败: %w", err)
	}

	strmFormat := opt["strm-format"]
	if strmFormat == "" {
		strmFormat = "true"
	}

	includeExts := f.parseExtensions(opt["include"], "mp4,mkv,avi,mov,wmv,flv,webm,m4v,3gp,ts,m2ts")
	excludeExts := f.parseExtensions(opt["exclude"], "")

	dryRun := opt["dry-run"] == "true"

	// 3. 初始化统计信息
	stats := &MediaSyncStats{
		DryRun: dryRun,
	}

	fs.Infof(f, "📋 同步参数: 源=%s, 目标=%s, 最小大小=%s, 格式=%s, 预览=%v",
		sourcePath, targetPath, fs.SizeSuffix(minSize), strmFormat, dryRun)

	// 4. 开始递归处理
	// 获取源路径的根目录名称，并添加到目标路径中
	rootDirName := f.root
	if rootDirName == "" {
		rootDirName = "root"
	}
	// 清理路径，去掉末尾的斜杠
	rootDirName = strings.TrimSuffix(rootDirName, "/")

	// 构建包含根目录的目标路径
	fullTargetPath := filepath.Join(targetPath, rootDirName)

	err = f.processDirectoryForMediaSync(ctx, sourcePath, fullTargetPath, minSize, strmFormat,
		includeExts, excludeExts, stats)
	if err != nil {
		return stats, fmt.Errorf("媒体同步失败: %w", err)
	}

	fs.Infof(f, "🎉 媒体同步完成! 处理目录:%d, 处理文件:%d, 创建.strm:%d, 跳过:%d, 错误:%d",
		stats.ProcessedDirs, stats.ProcessedFiles, stats.CreatedStrm, stats.SkippedFiles, stats.Errors)

	return stats, nil
}

// parseSize 解析大小字符串
func (f *Fs) parseSize(sizeStr, defaultSize string) (int64, error) {
	if sizeStr == "" {
		sizeStr = defaultSize
	}
	sizeSuffix := fs.SizeSuffix(0)
	err := sizeSuffix.Set(sizeStr)
	if err != nil {
		return 0, err
	}
	return int64(sizeSuffix), nil
}

// parseExtensions 解析文件扩展名列表
func (f *Fs) parseExtensions(extStr, defaultExts string) map[string]bool {
	if extStr == "" {
		extStr = defaultExts
	}

	extMap := make(map[string]bool)
	if extStr != "" {
		exts := strings.Split(extStr, ",")
		for _, ext := range exts {
			ext = strings.TrimSpace(strings.ToLower(ext))
			if ext != "" {
				// 确保扩展名以点开头
				if !strings.HasPrefix(ext, ".") {
					ext = "." + ext
				}
				extMap[ext] = true
			}
		}
	}
	return extMap
}

// isVideoFile 检查是否为视频文件
func (f *Fs) isVideoFile(filename string, includeExts, excludeExts map[string]bool) bool {
	ext := strings.ToLower(filepath.Ext(filename))

	// 检查排除列表
	if len(excludeExts) > 0 && excludeExts[ext] {
		return false
	}

	// 检查包含列表
	if len(includeExts) > 0 {
		return includeExts[ext]
	}

	// 默认视频扩展名
	defaultVideoExts := map[string]bool{
		".mp4": true, ".mkv": true, ".avi": true, ".mov": true,
		".wmv": true, ".flv": true, ".webm": true, ".m4v": true,
		".3gp": true, ".ts": true, ".m2ts": true,
	}
	return defaultVideoExts[ext]
}

// processDirectoryForMediaSync 递归处理目录进行媒体同步
func (f *Fs) processDirectoryForMediaSync(ctx context.Context, sourcePath, targetPath string,
	minSize int64, strmFormat string, includeExts, excludeExts map[string]bool, stats *MediaSyncStats) error {

	fs.Debugf(f, "📁 处理目录: %s -> %s", sourcePath, targetPath)
	stats.ProcessedDirs++

	// 1. 创建目标目录
	if !stats.DryRun {
		if err := os.MkdirAll(targetPath, 0755); err != nil {
			errMsg := fmt.Sprintf("创建目录失败 %s: %v", targetPath, err)
			stats.ErrorMessages = append(stats.ErrorMessages, errMsg)
			stats.Errors++
			return fmt.Errorf(errMsg)
		}
	} else {
		fs.Infof(f, "🔍 [预览] 将创建目录: %s", targetPath)
	}

	// 2. 列出源目录内容
	entries, err := f.List(ctx, sourcePath)
	if err != nil {
		errMsg := fmt.Sprintf("列出目录失败 %s: %v", sourcePath, err)
		stats.ErrorMessages = append(stats.ErrorMessages, errMsg)
		stats.Errors++
		return fmt.Errorf(errMsg)
	}

	// 3. 处理每个条目
	for _, entry := range entries {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		switch e := entry.(type) {
		case fs.Directory:
			// 递归处理子目录
			// e.Remote() 返回相对于文件系统根的完整路径
			// 我们需要计算相对于当前 sourcePath 的路径
			relativePath := e.Remote()
			if sourcePath != "" {
				// 如果有源路径前缀，去掉它来获取相对路径
				if strings.HasPrefix(relativePath, sourcePath+"/") {
					relativePath = strings.TrimPrefix(relativePath, sourcePath+"/")
				} else if relativePath == sourcePath {
					// 如果完全匹配，说明这是当前目录本身，跳过
					continue
				}
			}

			subSourcePath := e.Remote() // 使用完整路径作为源路径
			subTargetPath := filepath.Join(targetPath, relativePath)

			err := f.processDirectoryForMediaSync(ctx, subSourcePath, subTargetPath,
				minSize, strmFormat, includeExts, excludeExts, stats)
			if err != nil {
				fs.Logf(f, "⚠️ 处理子目录失败: %v", err)
				// 继续处理其他目录，不中断整个过程
			}

		case fs.Object:
			// 处理文件
			stats.ProcessedFiles++

			// 检查文件类型和大小
			fileName := filepath.Base(e.Remote())
			if !f.isVideoFile(fileName, includeExts, excludeExts) {
				fs.Debugf(f, "⏭️ 跳过非视频文件: %s", fileName)
				stats.SkippedFiles++
				continue
			}

			if e.Size() < minSize {
				fs.Debugf(f, "⏭️ 跳过小文件 (%s): %s", fs.SizeSuffix(e.Size()), fileName)
				stats.SkippedFiles++
				continue
			}

			// 创建 .strm 文件
			err := f.createStrmFileFor123(ctx, e, targetPath, strmFormat, stats)
			if err != nil {
				errMsg := fmt.Sprintf("创建.strm文件失败 %s: %v", fileName, err)
				stats.ErrorMessages = append(stats.ErrorMessages, errMsg)
				stats.Errors++
				fs.Logf(f, "❌ %s", errMsg)
			} else {
				stats.CreatedStrm++
			}
		}
	}

	return nil
}

// createStrmFileFor123 为123网盘文件创建.strm文件
func (f *Fs) createStrmFileFor123(ctx context.Context, obj fs.Object, targetDir, strmFormat string, stats *MediaSyncStats) error {
	// 1. 生成 .strm 文件路径
	fileName := filepath.Base(obj.Remote()) // 只使用文件名，不包含路径
	baseName := strings.TrimSuffix(fileName, filepath.Ext(fileName))
	strmPath := filepath.Join(targetDir, baseName+".strm")

	// 2. 生成 .strm 文件内容
	var content string
	switch strmFormat {
	case "true":
		// 使用优化格式：123://fileId
		if o, ok := obj.(*Object); ok && o.id != "" {
			content = fmt.Sprintf("123://%s\n", o.id)
		} else {
			// 如果无法获取fileId，回退到路径模式
			fs.Debugf(f, "⚠️ 无法获取fileId，使用路径模式: %s", obj.Remote())
			content = obj.Remote() + "\n"
		}
	case "false":
		// 使用路径格式
		content = obj.Remote() + "\n"
	case "fileid":
		// 兼容旧格式名称
		if o, ok := obj.(*Object); ok && o.id != "" {
			content = fmt.Sprintf("123://%s\n", o.id)
		} else {
			fs.Debugf(f, "⚠️ 无法获取fileId，使用路径模式: %s", obj.Remote())
			content = obj.Remote() + "\n"
		}
	case "path":
		// 兼容模式：使用文件路径
		content = obj.Remote() + "\n"
	default:
		// 默认使用优化格式
		if o, ok := obj.(*Object); ok && o.id != "" {
			content = fmt.Sprintf("123://%s\n", o.id)
		} else {
			content = obj.Remote() + "\n"
		}
	}

	// 3. 检查是否为预览模式
	if stats.DryRun {
		fs.Infof(f, "🔍 [预览] 将创建.strm文件: %s (内容: %s)", strmPath, strings.TrimSpace(content))
		return nil
	}

	// 4. 检查文件是否已存在
	if _, err := os.Stat(strmPath); err == nil {
		fs.Debugf(f, "📄 .strm文件已存在，将覆盖: %s", strmPath)
	}

	// 5. 创建 .strm 文件
	file, err := os.Create(strmPath)
	if err != nil {
		return fmt.Errorf("创建.strm文件失败: %w", err)
	}
	defer file.Close()

	_, err = file.WriteString(content)
	if err != nil {
		return fmt.Errorf("写入.strm文件失败: %w", err)
	}

	fs.Infof(f, "✅ 创建.strm文件: %s (大小: %s, 内容: %s)",
		strmPath, fs.SizeSuffix(obj.Size()), strings.TrimSpace(content))

	return nil
}
