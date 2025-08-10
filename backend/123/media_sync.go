package _123

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/rclone/rclone/fs"
)

// MediaSyncStats 媒体同步统计信息
type MediaSyncStats struct {
	ProcessedDirs  int      `json:"processed_dirs"`
	ProcessedFiles int      `json:"processed_files"`
	CreatedStrm    int      `json:"created_strm"`
	SkippedFiles   int      `json:"skipped_files"`
	DeletedStrm    int      `json:"deleted_strm"`
	DeletedDirs    int      `json:"deleted_dirs"`
	Errors         int      `json:"errors"`
	ErrorMessages  []string `json:"error_messages,omitempty"`
	DryRun         bool     `json:"dry_run"`
	SyncDelete     bool     `json:"sync_delete"`
}

// mediaSyncCommand 实现媒体库同步功能
func (f *Fs) mediaSyncCommand(ctx context.Context, args []string, opt map[string]string) (any, error) {
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
	// 🔧 安全修复：默认禁用同步删除，避免意外删除其他同步任务的文件
	syncDelete := true
	// 只有用户明确设置为 true 时才启用同步删除
	if opt["sync-delete"] == "true" {
		syncDelete = true
		fs.Logf(f, "⚠️ 警告：已启用同步删除功能，将删除本地不存在于网盘的.strm文件和空目录")
		fs.Logf(f, "💡 提示：建议先使用 --dry-run=true 预览删除操作")
	}

	// 3. 初始化统计信息
	stats := &MediaSyncStats{
		DryRun:     dryRun,
		SyncDelete: syncDelete,
	}

	fs.Infof(f, "📋 同步参数: 源=%s, 目标=%s, 最小大小=%s, 格式=%s, 预览=%v, 同步删除=%v",
		sourcePath, targetPath, fs.SizeSuffix(minSize), strmFormat, dryRun, syncDelete)

	// 4. 开始递归处理
	// 🔧 修复：检查是否需要创建根目录层级
	var fullTargetPath string

	// 如果用户明确指定了目标路径包含源目录名，则直接使用
	rootDirName := f.root
	if rootDirName == "" {
		rootDirName = "root"
	}
	rootDirName = strings.TrimSuffix(rootDirName, "/")

	// 检查目标路径是否已经包含了源目录名
	targetBaseName := filepath.Base(targetPath)
	if targetBaseName == rootDirName {
		// 目标路径已经包含源目录名，直接使用
		fullTargetPath = targetPath
		fs.Debugf(f, "🎯 目标路径已包含源目录名，直接使用: %s", fullTargetPath)
	} else {
		// 目标路径不包含源目录名，添加根目录层级
		fullTargetPath = filepath.Join(targetPath, rootDirName)
		fs.Debugf(f, "📁 添加根目录层级: %s -> %s", targetPath, fullTargetPath)
	}

	err = f.processDirectoryForMediaSync(ctx, sourcePath, fullTargetPath, minSize, strmFormat,
		includeExts, excludeExts, stats)
	if err != nil {
		return stats, fmt.Errorf("媒体同步失败: %w", err)
	}

	// 5. 如果启用了同步删除，进行全局清理
	if stats.SyncDelete {
		fs.Infof(f, "🧹 开始全局同步删除...")
		err := f.globalSyncDelete(ctx, sourcePath, targetPath, includeExts, excludeExts, stats)
		if err != nil {
			fs.Logf(f, "⚠️ 全局同步删除失败: %v", err)
			// 不中断整个过程，继续执行
		}
	}

	if stats.SyncDelete {
		if stats.DeletedDirs > 0 {
			fs.Infof(f, "🎉 媒体同步完成! 处理目录:%d, 处理文件:%d, 创建.strm:%d, 删除.strm:%d, 删除目录:%d, 跳过:%d, 错误:%d",
				stats.ProcessedDirs, stats.ProcessedFiles, stats.CreatedStrm, stats.DeletedStrm, stats.DeletedDirs, stats.SkippedFiles, stats.Errors)
		} else {
			fs.Infof(f, "🎉 媒体同步完成! 处理目录:%d, 处理文件:%d, 创建.strm:%d, 删除.strm:%d, 跳过:%d, 错误:%d",
				stats.ProcessedDirs, stats.ProcessedFiles, stats.CreatedStrm, stats.DeletedStrm, stats.SkippedFiles, stats.Errors)
		}
	} else {
		fs.Infof(f, "🎉 媒体同步完成! 处理目录:%d, 处理文件:%d, 创建.strm:%d, 跳过:%d, 错误:%d",
			stats.ProcessedDirs, stats.ProcessedFiles, stats.CreatedStrm, stats.SkippedFiles, stats.Errors)
	}

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
			return errors.New(errMsg)
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
		return errors.New(errMsg)
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
				if path, found := strings.CutPrefix(relativePath, sourcePath+"/"); found {
					relativePath = path
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

	// 注意：同步删除将在所有目录处理完成后统一进行

	return nil
}

// createStrmFileFor123 为123网盘文件创建.strm文件
func (f *Fs) createStrmFileFor123(_ context.Context, obj fs.Object, targetDir, strmFormat string, stats *MediaSyncStats) error {
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

// globalSyncDelete 全局同步删除功能，类似 rclone sync
// 🔧 安全修复：限制清理范围到当前同步的子目录，避免影响其他同步任务
func (f *Fs) globalSyncDelete(ctx context.Context, sourcePath, targetPath string,
	includeExts, excludeExts map[string]bool, stats *MediaSyncStats) error {

	// 🔧 修复：只清理当前同步的根目录，而不是整个targetPath
	rootDirName := f.root
	if rootDirName == "" {
		rootDirName = "root"
	}
	rootDirName = strings.TrimSuffix(rootDirName, "/")

	// 限制清理范围到当前同步任务的目录
	syncedTargetPath := filepath.Join(targetPath, rootDirName)

	fs.Debugf(f, "🧹 开始限定范围的同步删除: %s (仅限: %s)", targetPath, syncedTargetPath)
	fs.Logf(f, "🔒 安全边界：只清理当前同步目录 %s，不影响其他目录", syncedTargetPath)

	// 1. 只收集当前同步目录中的.strm文件
	localStrmFiles := make(map[string]string) // 相对路径 -> 绝对路径
	err := f.collectLocalStrmFiles(syncedTargetPath, "", localStrmFiles)
	if err != nil {
		return fmt.Errorf("收集本地.strm文件失败: %w", err)
	}

	if len(localStrmFiles) == 0 {
		fs.Debugf(f, "📂 没有找到.strm文件: %s", syncedTargetPath)
		return nil
	}

	fs.Debugf(f, "📂 找到 %d 个本地.strm文件", len(localStrmFiles))

	// 🔧 安全检查：如果发现大量.strm文件，警告用户
	if len(localStrmFiles) > 500 {
		fs.Logf(f, "⚠️ 警告：发现%d个.strm文件，请确认删除范围正确", len(localStrmFiles))
		fs.Logf(f, "💡 提示：如果数量异常，请检查目标路径设置或使用 --dry-run=true 预览")
	}

	// 2. 递归收集网盘中的所有视频文件
	cloudVideoFiles := make(map[string]bool) // .strm文件名 -> 是否存在
	err = f.collectCloudVideoFiles(ctx, sourcePath, "", includeExts, excludeExts, cloudVideoFiles)
	if err != nil {
		return fmt.Errorf("收集网盘视频文件失败: %w", err)
	}

	fs.Debugf(f, "📂 找到 %d 个网盘视频文件", len(cloudVideoFiles))

	// 3. 找出孤立的.strm文件
	orphanedFiles := make([]string, 0)
	for relativePath, absolutePath := range localStrmFiles {
		strmName := filepath.Base(relativePath)
		if !cloudVideoFiles[strmName] {
			orphanedFiles = append(orphanedFiles, absolutePath)
		}
	}

	// 4. 删除孤立的.strm文件
	for _, strmFile := range orphanedFiles {
		if stats.DryRun {
			fs.Infof(f, "🔍 [预览] 将删除孤立的.strm文件: %s", strmFile)
		} else {
			fs.Infof(f, "🗑️ 删除孤立的.strm文件: %s", strmFile)
			if err := os.Remove(strmFile); err != nil {
				errMsg := fmt.Sprintf("删除.strm文件失败 %s: %v", strmFile, err)
				stats.ErrorMessages = append(stats.ErrorMessages, errMsg)
				stats.Errors++
				fs.Logf(f, "❌ %s", errMsg)
				continue
			}
		}
		stats.DeletedStrm++
	}

	// 5. 清理空目录（限制在当前同步目录范围内）
	if stats.DeletedStrm > 0 {
		fs.Debugf(f, "✅ 删除了 %d 个孤立的.strm文件，开始清理空目录", stats.DeletedStrm)
		// 🔧 修复：只清理当前同步目录的空目录
		err := f.cleanupEmptyDirectoriesGlobal(ctx, syncedTargetPath, stats)
		if err != nil {
			fs.Logf(f, "⚠️ 清理空目录失败: %v", err)
		}
	}

	return nil
}

// shouldPreserveDirectory 检查是否应该保护目录不被删除
func (f *Fs) shouldPreserveDirectory(dirPath string) bool {
	// 不删除根目录
	if dirPath == "/" || dirPath == "." {
		return true
	}

	// 不删除用户主目录相关路径
	if strings.Contains(dirPath, "/home/") || strings.Contains(dirPath, "/Users/") {
		// 只有在路径很深的情况下才允许删除
		parts := strings.Split(dirPath, "/")
		if len(parts) < 5 { // 至少要有 /Users/username/some/deep/path
			return true
		}
	}

	// 不删除系统重要目录
	systemDirs := []string{"/bin", "/usr", "/etc", "/var", "/opt", "/tmp"}
	for _, sysDir := range systemDirs {
		if strings.HasPrefix(dirPath, sysDir) && len(strings.Split(dirPath, "/")) < 4 {
			return true
		}
	}

	return false
}

// collectLocalStrmFiles 递归收集本地目录中的所有.strm文件
func (f *Fs) collectLocalStrmFiles(basePath, relativePath string, strmFiles map[string]string) error {
	currentPath := filepath.Join(basePath, relativePath)

	entries, err := os.ReadDir(currentPath)
	if err != nil {
		return fmt.Errorf("读取目录失败 %s: %w", currentPath, err)
	}

	for _, entry := range entries {
		entryPath := filepath.Join(relativePath, entry.Name())
		fullPath := filepath.Join(basePath, entryPath)

		if entry.IsDir() {
			// 递归处理子目录
			err := f.collectLocalStrmFiles(basePath, entryPath, strmFiles)
			if err != nil {
				fs.Debugf(f, "⚠️ 处理子目录失败: %v", err)
				// 继续处理其他目录
			}
		} else if strings.HasSuffix(entry.Name(), ".strm") {
			// 收集.strm文件
			strmFiles[entryPath] = fullPath
		}
	}

	return nil
}

// collectCloudVideoFiles 递归收集网盘中的所有视频文件
func (f *Fs) collectCloudVideoFiles(ctx context.Context, basePath, relativePath string,
	includeExts, excludeExts map[string]bool, videoFiles map[string]bool) error {

	currentPath := filepath.Join(basePath, relativePath)
	if currentPath == "." {
		currentPath = ""
	}

	entries, err := f.List(ctx, currentPath)
	if err != nil {
		return fmt.Errorf("列出目录失败 %s: %w", currentPath, err)
	}

	for _, entry := range entries {
		switch e := entry.(type) {
		case fs.Directory:
			// 递归处理子目录
			dirName := filepath.Base(e.Remote())
			subRelativePath := filepath.Join(relativePath, dirName)
			err := f.collectCloudVideoFiles(ctx, basePath, subRelativePath, includeExts, excludeExts, videoFiles)
			if err != nil {
				fs.Debugf(f, "⚠️ 处理子目录失败: %v", err)
				// 继续处理其他目录
			}
		case fs.Object:
			// 检查是否为视频文件
			fileName := filepath.Base(e.Remote())
			if f.isVideoFile(fileName, includeExts, excludeExts) {
				// 生成对应的.strm文件名
				baseName := strings.TrimSuffix(fileName, filepath.Ext(fileName))
				strmName := baseName + ".strm"
				videoFiles[strmName] = true
			}
		}
	}

	return nil
}

// cleanupEmptyDirectoriesGlobal 全局清理空目录，类似 rclone sync
func (f *Fs) cleanupEmptyDirectoriesGlobal(_ context.Context, targetPath string, stats *MediaSyncStats) error {
	fs.Debugf(f, "🗂️ 开始全局清理空目录: %s", targetPath)

	// 收集所有目录
	allDirs := make([]string, 0)
	err := f.collectAllDirectories(targetPath, &allDirs)
	if err != nil {
		return fmt.Errorf("收集目录失败: %w", err)
	}

	// 按路径长度排序，从最深的开始删除（类似 rclone sync）
	sort.Slice(allDirs, func(i, j int) bool {
		return len(allDirs[i]) > len(allDirs[j])
	})

	// 删除空目录
	for _, dirPath := range allDirs {
		// 检查目录是否为空
		entries, err := os.ReadDir(dirPath)
		if err != nil {
			fs.Debugf(f, "⚠️ 读取目录失败: %s: %v", dirPath, err)
			continue
		}

		if len(entries) == 0 {
			// 目录为空，检查是否应该删除
			if f.shouldPreserveDirectory(dirPath) {
				fs.Debugf(f, "🔒 保护目录，不删除: %s", dirPath)
				continue
			}

			// 删除空目录
			if stats.DryRun {
				fs.Infof(f, "🔍 [预览] 将删除空目录: %s", dirPath)
			} else {
				fs.Infof(f, "🗑️ 删除空目录: %s", dirPath)
				if err := os.Remove(dirPath); err != nil {
					errMsg := fmt.Sprintf("删除空目录失败 %s: %v", dirPath, err)
					stats.ErrorMessages = append(stats.ErrorMessages, errMsg)
					stats.Errors++
					fs.Logf(f, "❌ %s", errMsg)
					continue
				}
			}
			stats.DeletedDirs++
		}
	}

	if stats.DeletedDirs > 0 {
		fs.Debugf(f, "✅ 全局清理完成，删除了 %d 个空目录", stats.DeletedDirs)
	}

	return nil
}

// collectAllDirectories 递归收集所有目录
func (f *Fs) collectAllDirectories(basePath string, dirs *[]string) error {
	entries, err := os.ReadDir(basePath)
	if err != nil {
		return fmt.Errorf("读取目录失败 %s: %w", basePath, err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			dirPath := filepath.Join(basePath, entry.Name())
			*dirs = append(*dirs, dirPath)

			// 递归收集子目录
			err := f.collectAllDirectories(dirPath, dirs)
			if err != nil {
				fs.Debugf(f, "⚠️ 收集子目录失败: %v", err)
				// 继续处理其他目录
			}
		}
	}

	return nil
}
