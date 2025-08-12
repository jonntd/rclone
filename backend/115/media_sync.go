package _115

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
	fs.Infof(f, "🎬 开始115网盘媒体库同步...")

	// 1. 参数解析和验证
	var sourcePath, targetPath string

	// rclone backend 命令的参数解析：
	// 当使用 "rclone backend media-sync 115:/path /target" 时
	// 115:/path 会被解析为后端路径，args 中只包含 /target
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
	// 默认启用同步删除，类似 rclone sync 的行为
	syncDelete := true
	// 用户可以通过 sync-delete=false 来禁用同步删除
	if opt["sync-delete"] == "false" {
		syncDelete = false
		fs.Logf(f, "🔒 安全模式：同步删除已禁用，只会创建.strm文件，不会删除任何文件")
	} else {
		fs.Logf(f, "🧹 同步删除已启用，将删除本地不存在于网盘的.strm文件和空目录")
		fs.Logf(f, "💡 提示：如需禁用删除功能，请添加 -o sync-delete=false 选项")
	}

	// 3. 初始化统计信息
	stats := &MediaSyncStats{
		DryRun:     dryRun,
		SyncDelete: syncDelete,
	}

	fs.Infof(f, "📋 同步参数: 源=%s, 目标=%s, 最小大小=%s, 格式=%s, 预览=%v, 同步删除=%v",
		sourcePath, targetPath, fs.SizeSuffix(minSize), strmFormat, dryRun, syncDelete)

	// 4. 开始递归处理
	// 🔧 修复路径重复问题：直接使用用户指定的目标路径
	// 用户已经在命令中明确指定了完整的目标路径，不需要再添加额外的目录层级
	fullTargetPath := targetPath
	fs.Debugf(f, "🎯 使用用户指定的目标路径: %s", fullTargetPath)

	err = f.processDirectoryForMediaSync(ctx, sourcePath, fullTargetPath, minSize, strmFormat,
		includeExts, excludeExts, stats)
	if err != nil {
		return stats, fmt.Errorf("媒体同步失败: %w", err)
	}

	// 5. 如果启用了同步删除，进行全局清理
	if stats.SyncDelete {
		fs.Infof(f, "🧹 开始全局同步删除...")
		err := f.globalSyncDelete115(ctx, sourcePath, targetPath, includeExts, excludeExts, stats)
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
			err := f.createStrmFileFor115(ctx, e, targetPath, strmFormat, stats)
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

	// 4. 如果启用了同步删除，检查并删除本地不存在于网盘的.strm文件
	if stats.SyncDelete {
		err := f.cleanupOrphanedStrmFiles115(targetPath, entries, includeExts, excludeExts, stats)
		if err != nil {
			fs.Logf(f, "⚠️ 清理孤立.strm文件失败: %v", err)
			// 不中断整个过程，继续执行
		}
	}

	return nil
}

// createStrmFileFor115 为115网盘文件创建.strm文件
func (f *Fs) createStrmFileFor115(ctx context.Context, obj fs.Object, targetDir, strmFormat string, stats *MediaSyncStats) error {
	// 1. 生成 .strm 文件路径
	fileName := filepath.Base(obj.Remote()) // 只使用文件名，不包含路径
	baseName := strings.TrimSuffix(fileName, filepath.Ext(fileName))
	strmPath := filepath.Join(targetDir, baseName+".strm")

	// 2. 生成 .strm 文件内容
	var content string
	switch strmFormat {
	case "true":
		// 使用优化格式：115://pick_code
		if o, ok := obj.(*Object); ok && o.pickCode != "" {
			content = fmt.Sprintf("115://%s\n", o.pickCode)
		} else {
			// 如果无法从对象获取pick_code，尝试通过路径获取
			pickCode, err := f.GetPickCodeByPath(ctx, obj.Remote())
			if err == nil && pickCode != "" {
				content = fmt.Sprintf("115://%s\n", pickCode)
			} else {
				// 如果无法获取pick_code，回退到路径模式
				fs.Debugf(f, "⚠️ 无法获取pick_code，使用路径模式: %s", obj.Remote())
				content = obj.Remote() + "\n"
			}
		}
	case "false":
		// 使用路径格式
		content = obj.Remote() + "\n"
	case "fileid":
		// 兼容旧格式名称
		if o, ok := obj.(*Object); ok && o.pickCode != "" {
			content = fmt.Sprintf("115://%s\n", o.pickCode)
		} else {
			pickCode, err := f.GetPickCodeByPath(ctx, obj.Remote())
			if err == nil && pickCode != "" {
				content = fmt.Sprintf("115://%s\n", pickCode)
			} else {
				fs.Debugf(f, "⚠️ 无法获取pick_code，使用路径模式: %s", obj.Remote())
				content = obj.Remote() + "\n"
			}
		}
	case "pickcode":
		// 兼容旧格式名称
		if o, ok := obj.(*Object); ok && o.pickCode != "" {
			content = fmt.Sprintf("115://%s\n", o.pickCode)
		} else {
			pickCode, err := f.GetPickCodeByPath(ctx, obj.Remote())
			if err == nil && pickCode != "" {
				content = fmt.Sprintf("115://%s\n", pickCode)
			} else {
				fs.Debugf(f, "⚠️ 无法获取pick_code，使用路径模式: %s", obj.Remote())
				content = obj.Remote() + "\n"
			}
		}
	case "path":
		// 兼容模式：使用文件路径
		content = obj.Remote() + "\n"
	default:
		// 默认使用优化格式
		if o, ok := obj.(*Object); ok && o.pickCode != "" {
			content = fmt.Sprintf("115://%s\n", o.pickCode)
		} else {
			// 尝试通过路径获取pick_code
			pickCode, err := f.GetPickCodeByPath(ctx, obj.Remote())
			if err == nil && pickCode != "" {
				content = fmt.Sprintf("115://%s\n", pickCode)
			} else {
				content = obj.Remote() + "\n"
			}
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

// cleanupOrphanedStrmFiles115 清理本地不存在于网盘的.strm文件
func (f *Fs) cleanupOrphanedStrmFiles115(targetPath string, cloudEntries []fs.DirEntry,
	includeExts, excludeExts map[string]bool, stats *MediaSyncStats) error {

	fs.Debugf(f, "🧹 开始清理孤立的.strm文件: %s", targetPath)

	// 1. 读取本地目录中的所有.strm文件
	localStrmFiles, err := filepath.Glob(filepath.Join(targetPath, "*.strm"))
	if err != nil {
		return fmt.Errorf("读取本地.strm文件失败: %w", err)
	}

	// 如果当前目录没有.strm文件，检查父目录
	if len(localStrmFiles) == 0 {
		parentDir := filepath.Dir(targetPath)
		if parentDir != targetPath && parentDir != "." && parentDir != "/" {
			fs.Debugf(f, "📂 当前目录中没有.strm文件，检查父目录: %s -> %s", targetPath, parentDir)
			parentStrmFiles, err := filepath.Glob(filepath.Join(parentDir, "*.strm"))
			if err != nil {
				fs.Debugf(f, "⚠️ 读取父目录.strm文件失败: %v", err)
			} else if len(parentStrmFiles) > 0 {
				fs.Debugf(f, "📂 在父目录中找到%d个.strm文件，使用父目录进行清理", len(parentStrmFiles))
				localStrmFiles = parentStrmFiles
				targetPath = parentDir // 更新目标路径为父目录
			}
		}
	}

	if len(localStrmFiles) == 0 {
		fs.Debugf(f, "📂 目录中没有.strm文件: %s", targetPath)
		return nil
	}

	// 2. 构建网盘中视频文件的映射（文件名 -> 是否存在）
	cloudVideoFiles := make(map[string]bool)
	for _, entry := range cloudEntries {
		if obj, ok := entry.(fs.Object); ok {
			fileName := filepath.Base(obj.Remote())
			if f.isVideoFile(fileName, includeExts, excludeExts) {
				// 生成对应的.strm文件名
				baseName := strings.TrimSuffix(fileName, filepath.Ext(fileName))
				strmName := baseName + ".strm"
				cloudVideoFiles[strmName] = true
			}
		}
	}

	// 3. 检查每个本地.strm文件是否对应网盘中的视频文件
	for _, strmFile := range localStrmFiles {
		strmName := filepath.Base(strmFile)

		// 检查对应的视频文件是否还在网盘中
		if !cloudVideoFiles[strmName] {
			// 这个.strm文件对应的视频文件已经不在网盘中了
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
	}

	if stats.DeletedStrm > 0 {
		fs.Debugf(f, "✅ 清理完成，删除了 %d 个孤立的.strm文件", stats.DeletedStrm)

		// 清理空目录
		err := f.cleanupEmptyDirectories115(targetPath, stats)
		if err != nil {
			fs.Logf(f, "⚠️ 清理空目录失败: %v", err)
		}
	}

	return nil
}

// cleanupEmptyDirectories115 清理空目录
func (f *Fs) cleanupEmptyDirectories115(startPath string, stats *MediaSyncStats) error {
	fs.Debugf(f, "🗂️ 开始清理空目录: %s", startPath)

	// 递归清理空目录，从最深层开始
	return f.cleanupEmptyDirectoriesRecursive115(startPath, stats, 0)
}

// cleanupEmptyDirectoriesRecursive115 递归清理空目录
func (f *Fs) cleanupEmptyDirectoriesRecursive115(dirPath string, stats *MediaSyncStats, depth int) error {
	// 防止无限递归，最多向上清理5层
	if depth > 5 {
		return nil
	}

	// 检查目录是否存在
	if _, err := os.Stat(dirPath); os.IsNotExist(err) {
		return nil
	}

	// 读取目录内容
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return fmt.Errorf("读取目录失败 %s: %w", dirPath, err)
	}

	// 如果目录不为空，不删除
	if len(entries) > 0 {
		fs.Debugf(f, "📁 目录不为空，保留: %s (%d个项目)", dirPath, len(entries))
		return nil
	}

	// 目录为空，检查是否应该删除
	// 不删除根目录和用户指定的主要目录
	if f.shouldPreserveDirectory115(dirPath) {
		fs.Debugf(f, "🔒 保护目录，不删除: %s", dirPath)
		return nil
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
			return nil // 不中断整个过程
		}
	}
	stats.DeletedDirs++

	// 递归检查父目录
	parentDir := filepath.Dir(dirPath)
	if parentDir != dirPath && parentDir != "." && parentDir != "/" {
		return f.cleanupEmptyDirectoriesRecursive115(parentDir, stats, depth+1)
	}

	return nil
}

// shouldPreserveDirectory115 检查是否应该保护目录不被删除
func (f *Fs) shouldPreserveDirectory115(dirPath string) bool {
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

// globalSyncDelete115 全局同步删除功能，类似 rclone sync
// 🔧 安全修复：限制清理范围到当前同步的子目录，避免影响其他同步任务
func (f *Fs) globalSyncDelete115(ctx context.Context, sourcePath, targetPath string,
	includeExts, excludeExts map[string]bool, stats *MediaSyncStats) error {

	// 🔧 修复：只清理当前同步的根目录，而不是整个targetPath
	rootDirName := f.root
	if rootDirName == "" {
		rootDirName = "root"
	}
	rootDirName = strings.TrimSuffix(rootDirName, "/")

	// 🔧 修复路径重复问题：检查targetPath是否已经以rootDirName结尾
	var syncedTargetPath string
	if strings.HasSuffix(targetPath, rootDirName) {
		// targetPath已经包含rootDirName，直接使用
		syncedTargetPath = targetPath
		fs.Debugf(f, "🎯 目标路径已包含根目录名，直接使用: %s", syncedTargetPath)
	} else {
		// targetPath不包含rootDirName，需要添加
		syncedTargetPath = filepath.Join(targetPath, rootDirName)
		fs.Debugf(f, "📁 添加根目录到目标路径: %s + %s = %s", targetPath, rootDirName, syncedTargetPath)
	}

	fs.Debugf(f, "🧹 开始限定范围的同步删除: %s (仅限: %s)", targetPath, syncedTargetPath)
	fs.Logf(f, "🔒 安全边界：只清理当前同步目录 %s，不影响其他目录", syncedTargetPath)

	// 1. 只收集当前同步目录中的.strm文件
	localStrmFiles := make(map[string]string) // 相对路径 -> 绝对路径
	err := f.collectLocalStrmFiles115(syncedTargetPath, "", localStrmFiles)
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
	err = f.collectCloudVideoFiles115(ctx, sourcePath, "", includeExts, excludeExts, cloudVideoFiles)
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

	// 5. 删除其他多余文件（非.strm文件）
	err = f.cleanupExtraFiles115(ctx, syncedTargetPath, cloudVideoFiles, stats)
	if err != nil {
		fs.Logf(f, "⚠️ 清理多余文件失败: %v", err)
	}

	// 6. 清理空目录（限制在当前同步目录范围内）
	if stats.DeletedStrm > 0 {
		fs.Debugf(f, "✅ 删除了 %d 个孤立的.strm文件，开始清理空目录", stats.DeletedStrm)
		// 🔧 修复：只清理当前同步目录的空目录
		err := f.cleanupEmptyDirectoriesGlobal115(ctx, syncedTargetPath, stats)
		if err != nil {
			fs.Logf(f, "⚠️ 清理空目录失败: %v", err)
		}
	}

	return nil
}

// collectLocalStrmFiles115 递归收集本地目录中的所有.strm文件
func (f *Fs) collectLocalStrmFiles115(basePath, relativePath string, strmFiles map[string]string) error {
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
			err := f.collectLocalStrmFiles115(basePath, entryPath, strmFiles)
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

// collectCloudVideoFiles115 递归收集网盘中的所有视频文件
func (f *Fs) collectCloudVideoFiles115(ctx context.Context, basePath, relativePath string,
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
			err := f.collectCloudVideoFiles115(ctx, basePath, subRelativePath, includeExts, excludeExts, videoFiles)
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

// cleanupEmptyDirectoriesGlobal115 全局清理空目录，类似 rclone sync
func (f *Fs) cleanupEmptyDirectoriesGlobal115(_ context.Context, targetPath string, stats *MediaSyncStats) error {
	fs.Debugf(f, "🗂️ 开始全局清理空目录: %s", targetPath)

	// 收集所有目录
	allDirs := make([]string, 0)
	err := f.collectAllDirectories115(targetPath, &allDirs)
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
			if f.shouldPreserveDirectory115(dirPath) {
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

// collectAllDirectories115 递归收集所有目录
func (f *Fs) collectAllDirectories115(basePath string, dirs *[]string) error {
	entries, err := os.ReadDir(basePath)
	if err != nil {
		return fmt.Errorf("读取目录失败 %s: %w", basePath, err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			dirPath := filepath.Join(basePath, entry.Name())
			*dirs = append(*dirs, dirPath)

			// 递归收集子目录
			err := f.collectAllDirectories115(dirPath, dirs)
			if err != nil {
				fs.Debugf(f, "⚠️ 收集子目录失败: %v", err)
				// 继续处理其他目录
			}
		}
	}

	return nil
}

// cleanupExtraFiles115 删除目标目录中不属于当前同步范围的所有文件
func (f *Fs) cleanupExtraFiles115(ctx context.Context, targetPath string, expectedStrmFiles map[string]bool, stats *MediaSyncStats) error {
	fs.Debugf(f, "🧹 开始清理多余文件: %s", targetPath)

	return f.cleanupExtraFilesRecursive115(targetPath, "", expectedStrmFiles, stats)
}

// cleanupExtraFilesRecursive115 递归清理多余文件
func (f *Fs) cleanupExtraFilesRecursive115(basePath, relativePath string, expectedStrmFiles map[string]bool, stats *MediaSyncStats) error {
	currentPath := filepath.Join(basePath, relativePath)

	entries, err := os.ReadDir(currentPath)
	if err != nil {
		return fmt.Errorf("读取目录失败 %s: %w", currentPath, err)
	}

	// 先收集当前目录中的所有.strm文件（有效的）
	validStrmBasenames := make(map[string]bool)
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".strm") {
			strmName := entry.Name()
			if expectedStrmFiles[strmName] {
				// 这是一个有效的.strm文件，提取基础名称
				baseName := strings.TrimSuffix(strmName, ".strm")
				validStrmBasenames[baseName] = true
			}
		}
	}

	for _, entry := range entries {
		entryPath := filepath.Join(relativePath, entry.Name())
		fullPath := filepath.Join(basePath, entryPath)

		if entry.IsDir() {
			// 递归处理子目录
			err := f.cleanupExtraFilesRecursive115(basePath, entryPath, expectedStrmFiles, stats)
			if err != nil {
				fs.Debugf(f, "⚠️ 处理子目录失败: %v", err)
				// 继续处理其他目录
			}
		} else {
			// 处理文件
			fileName := entry.Name()

			// 如果是.strm文件，跳过（已经在前面处理过了）
			if strings.HasSuffix(fileName, ".strm") {
				continue
			}

			// 非.strm文件，检查是否应该保留
			if f.shouldKeepExtraFile115(fileName, validStrmBasenames) {
				fs.Debugf(f, "🔒 保留相关文件: %s", fullPath)
				continue
			}

			// 删除多余文件
			if stats.DryRun {
				fs.Infof(f, "🔍 [预览] 将删除多余文件: %s", fullPath)
			} else {
				fs.Infof(f, "🗑️ 删除多余文件: %s", fullPath)
				if err := os.Remove(fullPath); err != nil {
					errMsg := fmt.Sprintf("删除多余文件失败 %s: %v", fullPath, err)
					stats.ErrorMessages = append(stats.ErrorMessages, errMsg)
					stats.Errors++
					fs.Logf(f, "❌ %s", errMsg)
					continue
				}
			}
			stats.DeletedStrm++ // 复用这个计数器
		}
	}

	return nil
}

// shouldKeepExtraFile115 判断是否应该保留额外的文件
func (f *Fs) shouldKeepExtraFile115(fileName string, validStrmBasenames map[string]bool) bool {
	ext := strings.ToLower(filepath.Ext(fileName))
	lowerName := strings.ToLower(fileName)

	// 1. 智能检查是否与当前目录中的有效视频文件相关
	if f.isRelatedToValidVideo115(fileName, validStrmBasenames) {
		return true
	}

	// 2. 检查是否是目录级别的通用文件（不与特定视频相关）
	generalFiles := []string{
		"readme", "license", "changelog", "version",
		"index", "description", "info",
	}

	for _, generalFile := range generalFiles {
		if strings.Contains(lowerName, generalFile) {
			return true
		}
	}

	// 3. 检查是否是目录级别的媒体文件（如目录海报）
	if strings.Contains(lowerName, "poster") ||
		strings.Contains(lowerName, "fanart") ||
		strings.Contains(lowerName, "banner") ||
		strings.Contains(lowerName, "folder") {
		mediaExtensions := map[string]bool{
			".jpg": true, ".jpeg": true, ".png": true, ".webp": true,
		}
		if mediaExtensions[ext] {
			return true
		}
	}

	// 其他文件不保留
	return false
}

// isRelatedToValidVideo115 智能检查文件是否与有效视频相关
func (f *Fs) isRelatedToValidVideo115(fileName string, validStrmBasenames map[string]bool) bool {
	baseName := strings.TrimSuffix(fileName, filepath.Ext(fileName))
	ext := strings.ToLower(filepath.Ext(fileName))

	// 定义相关文件的扩展名
	relatedExtensions := map[string]bool{
		".nfo":  true, // 媒体信息文件
		".jpg":  true, // 海报图片
		".jpeg": true,
		".png":  true,
		".webp": true,
		".srt":  true, // 字幕文件
		".ass":  true,
		".ssa":  true,
		".vtt":  true,
		".sub":  true,
		".idx":  true,
	}

	// 如果不是相关扩展名，直接返回false
	if !relatedExtensions[ext] {
		return false
	}

	// 策略1：完全匹配（传统方式）
	if validStrmBasenames[baseName] {
		return true
	}

	// 策略2：后缀匹配（处理 基础名-类型.扩展名 的情况）
	mediaSuffixes := []string{
		"-poster", "-fanart", "-banner", "-thumb", "-clearlogo",
		"-landscape", "-disc", "-logo", "-clearart", "-backdrop",
	}

	for _, suffix := range mediaSuffixes {
		if strings.HasSuffix(baseName, suffix) {
			// 去掉后缀，检查是否有对应的视频
			videoBaseName := strings.TrimSuffix(baseName, suffix)
			if validStrmBasenames[videoBaseName] {
				return true
			}
		}
	}

	// 策略3：前缀匹配（处理长文件名的情况）
	for validBaseName := range validStrmBasenames {
		// 检查当前文件是否以某个有效视频的基础名开头
		if strings.HasPrefix(baseName, validBaseName) {
			// 检查剩余部分是否是媒体后缀
			remaining := strings.TrimPrefix(baseName, validBaseName)
			for _, suffix := range mediaSuffixes {
				if remaining == suffix {
					return true
				}
			}
		}
	}

	// 策略4：模糊匹配（处理文件名中有细微差异的情况）
	for validBaseName := range validStrmBasenames {
		if f.isSimilarBaseName115(baseName, validBaseName, mediaSuffixes) {
			return true
		}
	}

	return false
}

// isSimilarBaseName115 检查两个基础名是否相似（处理细微差异）
func (f *Fs) isSimilarBaseName115(fileName, validBaseName string, mediaSuffixes []string) bool {
	// 先检查是否有媒体后缀
	actualBaseName := fileName
	for _, suffix := range mediaSuffixes {
		if strings.HasSuffix(fileName, suffix) {
			actualBaseName = strings.TrimSuffix(fileName, suffix)
			break
		}
	}

	// 标准化比较（去掉空格、点号等差异）
	normalize := func(s string) string {
		s = strings.ReplaceAll(s, " ", "")
		s = strings.ReplaceAll(s, ".", "")
		s = strings.ReplaceAll(s, "-", "")
		s = strings.ReplaceAll(s, "_", "")
		return strings.ToLower(s)
	}

	normalizedActual := normalize(actualBaseName)
	normalizedValid := normalize(validBaseName)

	// 检查标准化后是否相同
	if normalizedActual == normalizedValid {
		return true
	}

	// 检查是否一个是另一个的前缀（处理版本差异）
	if len(normalizedActual) > len(normalizedValid) {
		return strings.HasPrefix(normalizedActual, normalizedValid)
	} else {
		return strings.HasPrefix(normalizedValid, normalizedActual)
	}
}
