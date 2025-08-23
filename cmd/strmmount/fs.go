//go:build cmount && ((linux && cgo) || (darwin && cgo) || (freebsd && cgo) || windows)

package strmmount

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rclone/rclone/cmd/mountlib"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/fserrors"
	"github.com/rclone/rclone/fs/log"
	"github.com/rclone/rclone/vfs"
	"github.com/winfsp/cgofuse/fuse"
)

// Config holds the configuration for strm-mount (duplicate from strmmount.go for this file)
type Config struct {
	VideoExtensions []string
	MinFileSize     fs.SizeSuffix
	URLFormat       string
	CacheTimeout    fs.Duration
	MaxCacheSize    int

	// 持久化缓存配置
	PersistentCache   bool          // 是否启用持久化缓存
	CacheDir          string        // 缓存目录
	CacheTTL          fs.Duration   // 缓存过期时间
	MaxPersistentSize fs.SizeSuffix // 最大持久化缓存大小
	SyncInterval      fs.Duration   // 同步间隔
	EnableCompression bool          // 是否启用压缩
	BackgroundSync    bool          // 是否启用后台同步
}

const fhUnset = ^uint64(0)

// STRMFS represents the STRM filesystem that virtualizes video files
type STRMFS struct {
	VFS       *vfs.VFS
	f         fs.Fs
	opt       *mountlib.Options
	config    *Config
	ready     chan struct{}
	mu        sync.Mutex
	handles   []vfs.Handle
	destroyed atomic.Int32

	// STRM-specific caches
	strmCache      map[string]string // path -> strm content
	strmCacheMu    sync.RWMutex
	lastCacheClean time.Time

	// 持久化缓存
	persistentCache *STRMPersistentCache
	cacheData       *CacheData
	cacheMu         sync.RWMutex

	// QPS 保护
	rateLimiter        *APIRateLimiter
	concurrencyLimiter *ConcurrencyLimiter
	refreshLimiter     *RefreshLimiter
}

// NewSTRMFS creates a new STRM filesystem
func NewSTRMFS(VFS *vfs.VFS, opt *mountlib.Options, config *Config) *STRMFS {
	fsys := &STRMFS{
		VFS:       VFS,
		f:         VFS.Fs(),
		opt:       opt,
		config:    config,
		ready:     make(chan struct{}),
		strmCache: make(map[string]string),
	}

	// 初始化持久化缓存
	fsys.initPersistentCache()

	// 初始化 QPS 保护
	fsys.initQPSProtection()

	return fsys
}

// initPersistentCache 初始化持久化缓存
func (fsys *STRMFS) initPersistentCache() {
	// 获取后端类型
	backend := fsys.getBackendType()
	if backend == "" {
		fs.Debugf(nil, "⚠️ [CACHE] 未知后端类型，禁用持久化缓存")
		return
	}

	// 获取远程路径
	remotePath := fsys.f.Root()

	// 创建持久化缓存实例
	persistentCache, err := NewSTRMPersistentCache(
		backend,
		remotePath,
		int64(fsys.config.MinFileSize),
		fsys.config.VideoExtensions,
		fsys.config.URLFormat,
	)
	if err != nil {
		fs.Logf(nil, "⚠️ [CACHE] 持久化缓存初始化失败: %v", err)
		return
	}

	fsys.persistentCache = persistentCache

	// 异步加载缓存数据
	go fsys.loadCacheData()
}

// getBackendType 获取后端类型
func (fsys *STRMFS) getBackendType() string {
	fsType := fsys.f.Name()
	switch fsType {
	case "123":
		return "123"
	case "115":
		return "115"
	default:
		return ""
	}
}

// loadCacheData 加载缓存数据
func (fsys *STRMFS) loadCacheData() {
	if fsys.persistentCache == nil {
		return
	}

	ctx := context.Background()
	cacheData, err := fsys.persistentCache.LoadOrCreate(ctx, fsys.f)
	if err != nil {
		fs.Logf(nil, "⚠️ [CACHE] 加载缓存数据失败: %v", err)
		return
	}

	fsys.cacheMu.Lock()
	fsys.cacheData = cacheData
	fsys.cacheMu.Unlock()

	fs.Infof(nil, "✅ [CACHE] 持久化缓存已加载: %d 个文件", cacheData.FileCount)
}

// initQPSProtection 初始化 QPS 保护
func (fsys *STRMFS) initQPSProtection() {
	// 获取后端类型
	backend := fsys.getBackendType()
	if backend == "" {
		fs.Debugf(nil, "⚠️ [QPS] 未知后端类型，禁用 QPS 保护")
		return
	}

	// 初始化速率限制器
	fsys.rateLimiter = NewAPIRateLimiter(backend)

	// 初始化并发限制器
	maxConcurrent := 3 // 默认最多3个并发 API 调用
	if backend == "123" {
		maxConcurrent = 2 // 123网盘更保守
	}
	fsys.concurrencyLimiter = NewConcurrencyLimiter(maxConcurrent)

	// 初始化刷新限制器
	minInterval := time.Hour     // 默认最小间隔1小时
	maxInterval := time.Hour * 6 // 默认最大间隔6小时
	qpsThreshold := 0.5          // 默认QPS阈值0.5

	if backend == "123" {
		minInterval = time.Hour * 2 // 123网盘更保守
		qpsThreshold = 0.2
	}

	fsys.refreshLimiter = NewRefreshLimiter(minInterval, maxInterval, qpsThreshold)

	fs.Infof(nil, "🛡️ [QPS] QPS 保护已启用: %s 网盘, 最大并发: %d, 刷新间隔: %v-%v",
		backend, maxConcurrent, minInterval, maxInterval)

	// 启动统计日志定时器
	go fsys.startQPSStatsLogger()
}

// startQPSStatsLogger 启动 QPS 统计日志定时器
func (fsys *STRMFS) startQPSStatsLogger() {
	ticker := time.NewTicker(5 * time.Minute) // 每5分钟记录一次统计
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if fsys.rateLimiter != nil {
				fsys.rateLimiter.LogStats()
			}
			if fsys.refreshLimiter != nil {
				fsys.refreshLimiter.LogStats()
			}
		}
	}
}

// getCachedFileInfo 从持久化缓存中获取文件信息
func (fsys *STRMFS) getCachedFileInfo(filePath string) *CachedFile {
	fsys.cacheMu.RLock()
	defer fsys.cacheMu.RUnlock()

	if fsys.cacheData == nil {
		return nil
	}

	// 分离目录和文件名
	dirPath := filepath.Dir(filePath)
	fileName := filepath.Base(filePath)

	if dirPath == "." {
		dirPath = ""
	}

	// 在缓存中查找
	for _, dir := range fsys.cacheData.Directories {
		if dir.Path == dirPath {
			for _, file := range dir.Files {
				if file.Name == fileName {
					return &file
				}
			}
		}
	}

	return nil
}

// generateSTRMContentFromCache 从缓存生成 STRM 内容
func (fsys *STRMFS) generateSTRMContentFromCache(filePath string) string {
	cachedFile := fsys.getCachedFileInfo(filePath)
	if cachedFile == nil {
		return ""
	}

	// 根据后端类型生成内容
	backend := fsys.getBackendType()
	switch backend {
	case "123":
		if cachedFile.FileID != "" {
			return fmt.Sprintf("123://%s", cachedFile.FileID)
		}
	case "115":
		if cachedFile.PickCode != "" {
			return fmt.Sprintf("115://%s", cachedFile.PickCode)
		}
	}

	return ""
}

// Init initializes the filesystem
func (fsys *STRMFS) Init() {
	close(fsys.ready)
}

// Destroy is called when the filesystem is being destroyed
func (fsys *STRMFS) Destroy() {
	fsys.destroyed.Store(1)
}

// Statfs returns filesystem statistics
func (fsys *STRMFS) Statfs(path string, stat *fuse.Statfs_t) int {
	defer log.Trace(path, "")("stat=%+v", stat)

	// 🚀 优化：避免调用 VFS.Statfs()，这会触发 About() API 调用
	// 直接返回合理的固定值，避免不必要的网络请求
	fs.Debugf(nil, "📊 [STATFS] Using optimized stats (avoiding About API call)")

	const blockSize = 4096
	// 使用固定的合理值而不是调用 fsys.VFS.Statfs()
	const totalBlocks = 1000000000 // 约4TB
	const freeBlocks = 500000000   // 约2TB

	stat.Blocks = totalBlocks
	stat.Bfree = freeBlocks
	stat.Bavail = freeBlocks
	stat.Files = 1e9
	stat.Ffree = 1e9
	stat.Bsize = blockSize
	stat.Frsize = blockSize
	stat.Namemax = 255

	mountlib.ClipBlocks(&stat.Blocks)
	mountlib.ClipBlocks(&stat.Bfree)
	mountlib.ClipBlocks(&stat.Bavail)

	return 0
}

// Getattr gets file attributes
func (fsys *STRMFS) Getattr(filePath string, stat *fuse.Stat_t, fh uint64) int {
	startTime := time.Now()
	defer func() {
		duration := time.Since(startTime)
		if duration > 10*time.Millisecond {
			fs.Infof(nil, "⏱️ [PERF] Getattr(%s) took %v", filePath, duration)
		}
		log.Trace(filePath, "fh=0x%X", fh)("stat=%+v, duration=%v", stat, duration)
	}()

	// Handle root directory
	if filePath == "/" {
		stat.Mode = fuse.S_IFDIR | 0755
		stat.Nlink = 1
		fs.Debugf(nil, "📁 [ACCESS] Root directory access")
		return 0
	}

	// Check if this is a virtual .strm file
	if strings.HasSuffix(filePath, ".strm") {
		return fsys.getSTRMAttr(filePath, stat)
	}

	// Handle regular files and directories through VFS
	node, err := fsys.VFS.Stat(filePath)
	if err != nil {
		return translateError(err)
	}

	// Check if this is a video file that should be hidden (virtualized)
	if !node.IsDir() && isVideoFile(node.Name(), node.Size(), fsys.config) {
		// Hide the original video file, only show .strm version
		return -fuse.ENOENT
	}

	fillStat(node, stat)
	return 0
}

// getSTRMAttr gets attributes for a virtual .strm file
func (fsys *STRMFS) getSTRMAttr(strmPath string, stat *fuse.Stat_t) int {
	// Convert .strm path to original video file path
	originalPath := fsys.strmToOriginalPath(strmPath)
	if originalPath == "" {
		return -fuse.ENOENT
	}

	// Check if original video file exists
	node, err := fsys.VFS.Stat(originalPath)
	if err != nil {
		return translateError(err)
	}

	// Verify it's a video file
	if !isVideoFile(node.Name(), node.Size(), fsys.config) {
		return -fuse.ENOENT
	}

	// 🚀 优化：尝试直接从 VFS File 获取内容，避免 NewObject 调用
	var content string
	if vfsFile, ok := node.(*vfs.File); ok {
		// 从 VFS File 获取底层的 fs.Object
		if dirEntry := vfsFile.DirEntry(); dirEntry != nil {
			if obj, ok := dirEntry.(fs.Object); ok {
				// 直接使用已有的 Object 生成内容
				content = fsys.getSTRMContentFromObject(originalPath, obj)
				fs.Debugf(nil, "🚀 [GETATTR-FAST] Using existing Object for %s", originalPath)
			} else {
				// 回退到原来的方法
				content = fsys.getSTRMContent(originalPath, node)
				fs.Debugf(nil, "⚠️ [GETATTR-SLOW] DirEntry is not Object for %s", originalPath)
			}
		} else {
			// 回退到原来的方法
			content = fsys.getSTRMContent(originalPath, node)
			fs.Debugf(nil, "⚠️ [GETATTR-SLOW] VFS File has no DirEntry for %s", originalPath)
		}
	} else {
		// 回退到原来的方法
		content = fsys.getSTRMContent(originalPath, node)
		fs.Debugf(nil, "⚠️ [GETATTR-SLOW] Not a VFS File for %s (type: %T)", originalPath, node)
	}

	// Fill stat for .strm file
	stat.Mode = fuse.S_IFREG | 0644
	stat.Nlink = 1
	stat.Size = int64(len(content))
	stat.Mtim.Sec = node.ModTime().Unix()
	stat.Mtim.Nsec = int64(node.ModTime().Nanosecond())
	stat.Ctim = stat.Mtim
	stat.Atim = stat.Mtim

	return 0
}

// Readdir reads directory contents
func (fsys *STRMFS) Readdir(dirPath string,
	fill func(name string, stat *fuse.Stat_t, ofst int64) bool,
	ofst int64,
	fh uint64) int {

	startTime := time.Now()
	var totalFiles, videoFiles, strmFiles int

	defer func() {
		duration := time.Since(startTime)
		fs.Infof(nil, "📂 [PERF] Readdir(%s): %d total, %d videos→%d strm files, took %v",
			dirPath, totalFiles, videoFiles, strmFiles, duration)
		log.Trace(dirPath, "ofst=%d, fh=0x%X", ofst, fh)("duration=%v", duration)
	}()

	// We don't support seeking in directories
	if ofst > 0 {
		fs.Debugf(nil, "⚠️ [WARN] Directory seeking not supported for %s", dirPath)
		return -fuse.ESPIPE
	}

	// Get directory contents from VFS
	dir, err := fsys.VFS.Stat(dirPath)
	if err != nil {
		return translateError(err)
	}

	if !dir.IsDir() {
		return -fuse.ENOTDIR
	}

	// Read directory entries
	vfsDir := dir.(*vfs.Dir)
	nodes, err := vfsDir.ReadDirAll()
	if err != nil {
		return translateError(err)
	}

	// Add . and .. entries
	var dotStat, dotdotStat fuse.Stat_t
	dotStat.Mode = fuse.S_IFDIR | 0755
	dotdotStat.Mode = fuse.S_IFDIR | 0755
	fill(".", &dotStat, 0)
	fill("..", &dotdotStat, 0)

	// Process each entry
	for _, node := range nodes {
		var stat fuse.Stat_t
		totalFiles++

		if node.IsDir() {
			// Directory - show as-is
			fillStat(node, &stat)
			fill(node.Name(), &stat, 0)
			fs.Debugf(nil, "📁 [DIR] %s", node.Name())
		} else {
			// File - check if it should be virtualized
			if isVideoFile(node.Name(), node.Size(), fsys.config) {
				videoFiles++
				strmFiles++

				// Show as .strm file instead of original
				strmName := fsys.originalToSTRMName(node.Name())
				stat.Mode = fuse.S_IFREG | 0644
				stat.Nlink = 1

				// 🚀 优化：直接使用已有的 Object，避免重复 API 调用
				contentStartTime := time.Now()
				originalPath := path.Join(dirPath, node.Name())

				// 尝试直接从 VFS File 获取底层的 fs.Object，避免 NewObject 调用
				var content string
				if vfsFile, ok := node.(*vfs.File); ok {
					// 从 VFS File 获取底层的 fs.Object (通过 DirEntry 接口)
					if dirEntry := vfsFile.DirEntry(); dirEntry != nil {
						if obj, ok := dirEntry.(fs.Object); ok {
							// 直接使用已有的 Object 生成内容
							content = fsys.getSTRMContentFromObject(originalPath, obj)
							fs.Debugf(nil, "🚀 [FAST-PATH] Using existing Object for %s", originalPath)
						} else {
							// DirEntry 不是 Object，回退到原来的方法
							content = fsys.getSTRMContent(originalPath, node)
							fs.Debugf(nil, "⚠️ [SLOW-PATH] DirEntry is not Object for %s", originalPath)
						}
					} else {
						// VFS File 没有底层 DirEntry，回退到原来的方法
						content = fsys.getSTRMContent(originalPath, node)
						fs.Debugf(nil, "⚠️ [SLOW-PATH] VFS File has no DirEntry for %s", originalPath)
					}
				} else {
					// 不是 VFS File，回退到原来的方法
					content = fsys.getSTRMContent(originalPath, node)
					fs.Debugf(nil, "⚠️ [SLOW-PATH] Not a VFS File for %s (type: %T)", originalPath, node)
				}
				contentDuration := time.Since(contentStartTime)

				stat.Size = int64(len(content))
				stat.Mtim.Sec = node.ModTime().Unix()
				stat.Mtim.Nsec = int64(node.ModTime().Nanosecond())
				stat.Ctim = stat.Mtim
				stat.Atim = stat.Mtim

				fill(strmName, &stat, 0)

				if contentDuration > 5*time.Millisecond {
					fs.Infof(nil, "🎬 [STRM] %s → %s (size: %s→%dB, content gen: %v)",
						node.Name(), strmName, fs.SizeSuffix(node.Size()), len(content), contentDuration)
				} else {
					fs.Debugf(nil, "🎬 [STRM] %s → %s (cached)", node.Name(), strmName)
				}
			} else {
				// Regular file - check if we should hide it completely
				if shouldHideFile(node.Name(), node.Size(), fsys.config) {
					// 文件太小，完全隐藏
					fs.Debugf(nil, "🙈 [HIDE] 文件太小，完全隐藏: %s (%s)", node.Name(), fs.SizeSuffix(node.Size()))
				} else if fsys.shouldShowNonVideoFile(node.Name()) {
					// 文件足够大但不是视频，显示原文件
					fillStat(node, &stat)
					fill(node.Name(), &stat, 0)
					fs.Debugf(nil, "📄 [FILE] %s (%s)", node.Name(), fs.SizeSuffix(node.Size()))
				} else {
					fs.Debugf(nil, "🚫 [SKIP] Hiding non-video file: %s", node.Name())
				}
			}
		}
	}

	return 0
}

// Open opens a file
func (fsys *STRMFS) Open(filePath string, flags int) (int, uint64) {
	defer log.Trace(filePath, "flags=0x%X", flags)("")

	// Check if this is a virtual .strm file
	if strings.HasSuffix(filePath, ".strm") {
		return fsys.openSTRMFile(filePath, flags)
	}

	// 🔒 SECURITY: Block direct access to original video files
	// Users should only access video content through .strm files
	if fsys.shouldBlockDirectAccess(filePath) {
		fs.Debugf(nil, "🚫 [SECURITY] Blocked direct access to video file: %s", filePath)
		return -fuse.ENOENT, fhUnset
	}

	// Handle regular files through VFS
	handle, err := fsys.VFS.OpenFile(filePath, flags, 0777)
	if err != nil {
		return translateError(err), fhUnset
	}

	return 0, fsys.openHandle(handle)
}

// openSTRMFile opens a virtual .strm file
func (fsys *STRMFS) openSTRMFile(strmPath string, flags int) (int, uint64) {
	// Convert .strm path to original path
	originalPath := fsys.strmToOriginalPath(strmPath)
	if originalPath == "" {
		return -fuse.ENOENT, fhUnset
	}

	// Check if original file exists and is a video
	node, err := fsys.VFS.Stat(originalPath)
	if err != nil {
		return translateError(err), fhUnset
	}

	if !isVideoFile(node.Name(), node.Size(), fsys.config) {
		return -fuse.ENOENT, fhUnset
	}

	// Create a virtual handle for .strm content
	// 🚀 优化：尝试直接从 VFS File 获取内容，避免 NewObject 调用
	var content string
	if vfsFile, ok := node.(*vfs.File); ok {
		// 从 VFS File 获取底层的 fs.Object
		if dirEntry := vfsFile.DirEntry(); dirEntry != nil {
			if obj, ok := dirEntry.(fs.Object); ok {
				// 直接使用已有的 Object 生成内容
				content = fsys.getSTRMContentFromObject(originalPath, obj)
				fs.Debugf(nil, "🚀 [OPEN-FAST] Using existing Object for %s", originalPath)
			} else {
				// 回退到原来的方法
				content = fsys.getSTRMContent(originalPath, node)
				fs.Debugf(nil, "⚠️ [OPEN-SLOW] DirEntry is not Object for %s", originalPath)
			}
		} else {
			// 回退到原来的方法
			content = fsys.getSTRMContent(originalPath, node)
			fs.Debugf(nil, "⚠️ [OPEN-SLOW] VFS File has no DirEntry for %s", originalPath)
		}
	} else {
		// 回退到原来的方法
		content = fsys.getSTRMContent(originalPath, node)
		fs.Debugf(nil, "⚠️ [OPEN-SLOW] Not a VFS File for %s (type: %T)", originalPath, node)
	}
	handle := &STRMHandle{
		content: []byte(content),
		offset:  0,
	}

	return 0, fsys.openSTRMHandle(handle)
}

// STRMHandle represents a handle to a virtual .strm file
type STRMHandle struct {
	content []byte
	offset  int64
}

// Read reads from the .strm handle
func (h *STRMHandle) Read(buf []byte, offset int64) int {
	if offset >= int64(len(h.content)) {
		return 0 // EOF
	}

	n := copy(buf, h.content[offset:])
	return n
}

// openHandle opens a VFS handle and returns its ID
func (fsys *STRMFS) openHandle(handle vfs.Handle) uint64 {
	fsys.mu.Lock()
	defer fsys.mu.Unlock()

	var i int
	var oldHandle vfs.Handle
	for i, oldHandle = range fsys.handles {
		if oldHandle == nil {
			break
		}
	}

	if i >= len(fsys.handles) {
		fsys.handles = append(fsys.handles, handle)
	} else {
		fsys.handles[i] = handle
	}

	return uint64(i)
}

// openSTRMHandle opens a STRM handle and returns its ID
func (fsys *STRMFS) openSTRMHandle(handle *STRMHandle) uint64 {
	fsys.mu.Lock()
	defer fsys.mu.Unlock()

	// For simplicity, we'll store STRM handles in a separate way
	// In a real implementation, you'd want a unified handle system
	fsys.handles = append(fsys.handles, nil) // placeholder
	return uint64(len(fsys.handles) - 1)
}

// Read reads from a file handle
func (fsys *STRMFS) Read(path string, buf []byte, ofst int64, fh uint64) int {
	startTime := time.Now()

	defer func() {
		duration := time.Since(startTime)
		if duration > 5*time.Millisecond {
			fs.Infof(nil, "📖 [PERF] Read(%s) offset=%d, len=%d, took %v", path, ofst, len(buf), duration)
		}
		log.Trace(path, "len=%d, ofst=%d, fh=0x%X", len(buf), ofst, fh)("duration=%v", duration)
	}()

	// Handle .strm files specially
	if strings.HasSuffix(path, ".strm") {
		fs.Debugf(nil, "🎬 [READ] Reading STRM file: %s (offset=%d, len=%d)", path, ofst, len(buf))

		originalPath := fsys.strmToOriginalPath(path)
		if originalPath == "" {
			fs.Debugf(nil, "❌ [ERROR] Cannot find original path for STRM: %s", path)
			return -fuse.ENOENT
		}

		node, err := fsys.VFS.Stat(originalPath)
		if err != nil {
			fs.Debugf(nil, "❌ [ERROR] Cannot stat original file %s: %v", originalPath, err)
			return translateError(err)
		}

		contentStartTime := time.Now()
		// 🚀 优化：尝试直接从 VFS File 获取内容，避免 NewObject 调用
		var content string
		if vfsFile, ok := node.(*vfs.File); ok {
			// 从 VFS File 获取底层的 fs.Object
			if dirEntry := vfsFile.DirEntry(); dirEntry != nil {
				if obj, ok := dirEntry.(fs.Object); ok {
					// 直接使用已有的 Object 生成内容
					content = fsys.getSTRMContentFromObject(originalPath, obj)
					fs.Debugf(nil, "🚀 [READ-FAST] Using existing Object for %s", originalPath)
				} else {
					// 回退到原来的方法
					content = fsys.getSTRMContent(originalPath, node)
					fs.Debugf(nil, "⚠️ [READ-SLOW] DirEntry is not Object for %s", originalPath)
				}
			} else {
				// 回退到原来的方法
				content = fsys.getSTRMContent(originalPath, node)
				fs.Debugf(nil, "⚠️ [READ-SLOW] VFS File has no DirEntry for %s", originalPath)
			}
		} else {
			// 回退到原来的方法
			content = fsys.getSTRMContent(originalPath, node)
			fs.Debugf(nil, "⚠️ [READ-SLOW] Not a VFS File for %s (type: %T)", originalPath, node)
		}
		contentDuration := time.Since(contentStartTime)
		contentBytes := []byte(content)

		if ofst >= int64(len(contentBytes)) {
			fs.Debugf(nil, "📖 [READ] EOF reached for %s (offset=%d >= len=%d)", path, ofst, len(contentBytes))
			return 0 // EOF
		}

		n := copy(buf, contentBytes[ofst:])

		if contentDuration > time.Millisecond {
			fs.Infof(nil, "🎬 [READ] STRM content: %s (%dB, content gen: %v, read: %dB)",
				path, len(contentBytes), contentDuration, n)
		} else {
			fs.Debugf(nil, "🎬 [READ] STRM content: %s (%dB cached, read: %dB)", path, len(contentBytes), n)
		}

		return n
	}

	// 🔒 SECURITY: Block direct read access to original video files
	// This prevents accidental large downloads when users try to access video files directly
	if fsys.shouldBlockDirectAccess(path) {
		fs.Debugf(nil, "🚫 [SECURITY] Blocked direct read access to video file: %s", path)
		return -fuse.ENOENT
	}

	// Handle regular files through VFS
	fsys.mu.Lock()
	handle := fsys.getHandle(fh)
	fsys.mu.Unlock()

	if handle == nil {
		return -fuse.EBADF
	}

	n, err := handle.ReadAt(buf, ofst)
	if err != nil && err != io.EOF {
		return translateError(err)
	}

	return n
}

// Release closes a file handle
func (fsys *STRMFS) Release(path string, fh uint64) int {
	defer log.Trace(path, "fh=0x%X", fh)("")

	fsys.mu.Lock()
	defer fsys.mu.Unlock()

	handle := fsys.getHandle(fh)
	if handle != nil {
		_ = handle.Close()
		fsys.handles[fh] = nil
	}

	return 0
}

// getHandle gets a handle by ID
func (fsys *STRMFS) getHandle(fh uint64) vfs.Handle {
	if fh >= uint64(len(fsys.handles)) {
		return nil
	}
	return fsys.handles[fh]
}

// Access checks file access permissions
func (fsys *STRMFS) Access(path string, mask uint32) int {
	defer log.Trace(path, "mask=0%o", mask)("")
	// This is a no-op for rclone - we allow all access
	return 0
}

// Chmod changes the permission bits of a file
func (fsys *STRMFS) Chmod(path string, mode uint32) int {
	defer log.Trace(path, "mode=0%o", mode)("")
	// This is a no-op for rclone (read-only filesystem)
	return 0
}

// Chown changes the owner and group of a file
func (fsys *STRMFS) Chown(path string, uid uint32, gid uint32) int {
	defer log.Trace(path, "uid=%d, gid=%d", uid, gid)("")
	// This is a no-op for rclone
	return 0
}

// Create creates and opens a file
func (fsys *STRMFS) Create(path string, flags int, mode uint32) (int, uint64) {
	defer log.Trace(path, "flags=0x%X, mode=0%o", flags, mode)("")
	// Read-only filesystem
	return -fuse.EROFS, fhUnset
}

// Flush flushes an open file descriptor
func (fsys *STRMFS) Flush(path string, fh uint64) int {
	defer log.Trace(path, "fh=0x%X", fh)("")
	// No-op for read-only filesystem
	return 0
}

// Fsync synchronizes file contents
func (fsys *STRMFS) Fsync(path string, datasync bool, fh uint64) int {
	defer log.Trace(path, "datasync=%v, fh=0x%X", datasync, fh)("")
	// No-op for read-only filesystem
	return 0
}

// Fsyncdir synchronizes directory contents
func (fsys *STRMFS) Fsyncdir(path string, datasync bool, fh uint64) int {
	defer log.Trace(path, "datasync=%v, fh=0x%X", datasync, fh)("")
	// No-op for read-only filesystem
	return 0
}

// Getxattr gets extended attributes
func (fsys *STRMFS) Getxattr(path string, name string) (int, []byte) {
	defer log.Trace(path, "name=%q", name)("")
	return -fuse.ENOSYS, nil
}

// Link creates a hard link
func (fsys *STRMFS) Link(oldpath string, newpath string) int {
	defer log.Trace(oldpath, "newpath=%q", newpath)("")
	return -fuse.ENOSYS
}

// Listxattr lists extended attributes
func (fsys *STRMFS) Listxattr(path string, fill func(name string) bool) int {
	defer log.Trace(path, "")("")
	return -fuse.ENOSYS
}

// Mkdir creates a directory
func (fsys *STRMFS) Mkdir(path string, mode uint32) int {
	defer log.Trace(path, "mode=0%o", mode)("")
	// Read-only filesystem
	return -fuse.EROFS
}

// Mknod creates a file node
func (fsys *STRMFS) Mknod(path string, mode uint32, dev uint64) int {
	defer log.Trace(path, "mode=0x%X, dev=0x%X", mode, dev)("")
	return -fuse.ENOSYS
}

// Opendir opens a directory
func (fsys *STRMFS) Opendir(path string) (int, uint64) {
	defer log.Trace(path, "")("")

	// Check if directory exists
	node, err := fsys.VFS.Stat(path)
	if err != nil {
		return translateError(err), fhUnset
	}

	if !node.IsDir() {
		return -fuse.ENOTDIR, fhUnset
	}

	// For directories, we don't need a real handle since we generate content on-demand
	return 0, 0
}

// Readlink reads the target of a symbolic link
func (fsys *STRMFS) Readlink(path string) (int, string) {
	defer log.Trace(path, "")("")
	linkPath, err := fsys.VFS.Readlink(path)
	return translateError(err), linkPath
}

// Releasedir closes a directory
func (fsys *STRMFS) Releasedir(path string, fh uint64) int {
	defer log.Trace(path, "fh=0x%X", fh)("")
	// No-op since we don't use real handles for directories
	return 0
}

// Removexattr removes extended attributes
func (fsys *STRMFS) Removexattr(path string, name string) int {
	defer log.Trace(path, "name=%q", name)("")
	return -fuse.ENOSYS
}

// Rename renames a file
func (fsys *STRMFS) Rename(oldpath string, newpath string) int {
	defer log.Trace(oldpath, "newpath=%q", newpath)("")
	// Read-only filesystem
	return -fuse.EROFS
}

// Rmdir removes a directory
func (fsys *STRMFS) Rmdir(path string) int {
	defer log.Trace(path, "")("")
	// Read-only filesystem
	return -fuse.EROFS
}

// Setxattr sets extended attributes
func (fsys *STRMFS) Setxattr(path string, name string, value []byte, flags int) int {
	defer log.Trace(path, "name=%q, value=%q, flags=%d", name, value, flags)("")
	return -fuse.ENOSYS
}

// Symlink creates a symbolic link
func (fsys *STRMFS) Symlink(target string, newpath string) int {
	defer log.Trace(target, "newpath=%q", newpath)("")
	return translateError(fsys.VFS.Symlink(target, newpath))
}

// Truncate truncates a file to size
func (fsys *STRMFS) Truncate(path string, size int64, fh uint64) int {
	defer log.Trace(path, "size=%d, fh=0x%X", size, fh)("")
	// Read-only filesystem
	return -fuse.EROFS
}

// Unlink removes a file
func (fsys *STRMFS) Unlink(path string) int {
	defer log.Trace(path, "")("")
	// Read-only filesystem
	return -fuse.EROFS
}

// Utimens changes the access and modification times of a file
func (fsys *STRMFS) Utimens(path string, tmsp []fuse.Timespec) int {
	defer log.Trace(path, "tmsp=%+v", tmsp)("")
	// Read-only filesystem
	return -fuse.EROFS
}

// Write writes data to a file
func (fsys *STRMFS) Write(path string, buff []byte, ofst int64, fh uint64) int {
	defer log.Trace(path, "len=%d, ofst=%d, fh=0x%X", len(buff), ofst, fh)("")
	// Read-only filesystem
	return -fuse.EROFS
}

// Helper methods for STRM virtualization

// originalToSTRMName converts original filename to .strm filename
func (fsys *STRMFS) originalToSTRMName(originalName string) string {
	ext := filepath.Ext(originalName)
	baseName := strings.TrimSuffix(originalName, ext)
	return baseName + ".strm"
}

// strmToOriginalPath converts .strm path back to original video file path
func (fsys *STRMFS) strmToOriginalPath(strmPath string) string {
	if !strings.HasSuffix(strmPath, ".strm") {
		return ""
	}

	// Remove .strm extension
	basePath := strings.TrimSuffix(strmPath, ".strm")

	// Try to find the original video file with any supported extension
	dir := filepath.Dir(basePath)
	baseName := filepath.Base(basePath)

	// Try each video extension
	for _, ext := range fsys.config.VideoExtensions {
		originalPath := filepath.Join(dir, baseName+"."+ext)
		if _, err := fsys.VFS.Stat(originalPath); err == nil {
			return originalPath
		}
	}

	return ""
}

// getSTRMContent gets or generates .strm file content
func (fsys *STRMFS) getSTRMContent(originalPath string, node os.FileInfo) string {
	startTime := time.Now()

	// 1. 检查内存缓存
	fsys.strmCacheMu.RLock()
	if content, found := fsys.strmCache[originalPath]; found {
		fsys.strmCacheMu.RUnlock()
		fs.Debugf(nil, "💾 [MEMORY-CACHE] Hit for %s (%dB, %v)", originalPath, len(content), time.Since(startTime))
		return content
	}
	fsys.strmCacheMu.RUnlock()

	// 2. 尝试从持久化缓存获取
	if content := fsys.generateSTRMContentFromCache(originalPath); content != "" {
		// 存储到内存缓存
		fsys.strmCacheMu.Lock()
		fsys.strmCache[originalPath] = content
		fsys.strmCacheMu.Unlock()

		fs.Debugf(nil, "💾 [PERSISTENT-CACHE] Hit for %s (%dB, %v)", originalPath, len(content), time.Since(startTime))
		return content
	}

	fs.Debugf(nil, "💾 [CACHE] Miss for %s, generating content...", originalPath)

	// Generate content
	objStartTime := time.Now()
	obj, err := fsys.VFS.Fs().NewObject(context.Background(), originalPath)
	objDuration := time.Since(objStartTime)

	if err != nil {
		fs.Debugf(fsys.f, "❌ [ERROR] Failed to get object for %s: %v (took %v)", originalPath, err, objDuration)
		return originalPath // fallback to path
	}

	contentStartTime := time.Now()
	content := generateSTRMContent(context.Background(), obj, fsys.config)
	contentDuration := time.Since(contentStartTime)

	fs.Infof(nil, "🔗 [GENERATE] %s → %s (obj: %v, content: %v, total: %v)",
		originalPath, content, objDuration, contentDuration, time.Since(startTime))

	// Cache the content
	fsys.strmCacheMu.Lock()
	fsys.strmCache[originalPath] = content
	cacheSize := len(fsys.strmCache)

	// Clean cache if it's getting too large
	if cacheSize > fsys.config.MaxCacheSize {
		cleanStartTime := time.Now()
		fsys.cleanCache()
		cleanDuration := time.Since(cleanStartTime)
		fs.Infof(nil, "🧹 [CACHE] Cleaned cache in %v (was %d entries)", cleanDuration, cacheSize)
	}
	fsys.strmCacheMu.Unlock()

	fs.Debugf(nil, "💾 [CACHE] Stored %s (%dB, cache size: %d/%d)",
		originalPath, len(content), cacheSize, fsys.config.MaxCacheSize)

	return content
}

// getSTRMContentFromObject gets or generates .strm file content using an existing Object
// This avoids the expensive NewObject() call and API request
func (fsys *STRMFS) getSTRMContentFromObject(originalPath string, obj fs.Object) string {
	startTime := time.Now()

	// Check cache first
	fsys.strmCacheMu.RLock()
	if content, found := fsys.strmCache[originalPath]; found {
		fsys.strmCacheMu.RUnlock()
		fs.Debugf(nil, "💾 [CACHE] Hit for %s (%dB, %v)", originalPath, len(content), time.Since(startTime))
		return content
	}
	fsys.strmCacheMu.RUnlock()

	fs.Debugf(nil, "💾 [CACHE] Miss for %s, generating content from existing object...", originalPath)

	// 🚀 直接使用已有的 Object 生成内容，无需 NewObject 调用
	contentStartTime := time.Now()
	content := generateSTRMContent(context.Background(), obj, fsys.config)
	contentDuration := time.Since(contentStartTime)

	fs.Infof(nil, "🔗 [GENERATE-FAST] %s → %s (no API call, content: %v, total: %v)",
		originalPath, content, contentDuration, time.Since(startTime))

	// Cache the content
	fsys.strmCacheMu.Lock()
	fsys.strmCache[originalPath] = content
	cacheSize := len(fsys.strmCache)

	// Clean cache if it's getting too large
	if cacheSize > fsys.config.MaxCacheSize {
		cleanStartTime := time.Now()
		fsys.cleanCache()
		cleanDuration := time.Since(cleanStartTime)
		fs.Infof(nil, "🧹 [CACHE] Cleaned cache in %v (was %d entries)", cleanDuration, cacheSize)
	}
	fsys.strmCacheMu.Unlock()

	fs.Debugf(nil, "💾 [CACHE] Stored %s (%dB, cache size: %d/%d)",
		originalPath, len(content), cacheSize, fsys.config.MaxCacheSize)

	return content
}

// cleanCache cleans old entries from the cache
func (fsys *STRMFS) cleanCache() {
	startTime := time.Now()
	initialSize := len(fsys.strmCache)

	// Simple cleanup: remove half the entries
	// In a real implementation, you'd use LRU or TTL
	if time.Since(fsys.lastCacheClean) < time.Minute {
		fs.Debugf(nil, "🧹 [CACHE] Skipping cleanup (last clean was %v ago)", time.Since(fsys.lastCacheClean))
		return // Don't clean too frequently
	}

	count := 0
	target := fsys.config.MaxCacheSize / 2

	fs.Infof(nil, "🧹 [CACHE] Starting cleanup: %d entries → target %d", initialSize, target)

	for path := range fsys.strmCache {
		if count >= target {
			break
		}
		delete(fsys.strmCache, path)
		count++
	}

	fsys.lastCacheClean = time.Now()
	duration := time.Since(startTime)
	finalSize := len(fsys.strmCache)

	fs.Infof(nil, "🧹 [CACHE] Cleanup complete: %d→%d entries (removed %d) in %v",
		initialSize, finalSize, count, duration)
}

// fillStat fills a fuse.Stat_t from a vfs Node
func fillStat(node os.FileInfo, stat *fuse.Stat_t) {
	if node.IsDir() {
		stat.Mode = fuse.S_IFDIR | 0755
	} else {
		stat.Mode = fuse.S_IFREG | 0644
	}

	stat.Nlink = 1
	stat.Size = node.Size()
	stat.Mtim.Sec = node.ModTime().Unix()
	stat.Mtim.Nsec = int64(node.ModTime().Nanosecond())
	stat.Ctim = stat.Mtim
	stat.Atim = stat.Mtim
}

// translateError translates VFS errors to FUSE errors
func translateError(err error) int {
	if err == nil {
		return 0
	}

	_, uErr := fserrors.Cause(err)
	switch uErr {
	case vfs.OK:
		return 0
	case vfs.ENOENT, fs.ErrorDirNotFound, fs.ErrorObjectNotFound:
		return -fuse.ENOENT
	case vfs.EEXIST, fs.ErrorDirExists:
		return -fuse.EEXIST
	case vfs.EPERM, fs.ErrorPermissionDenied:
		return -fuse.EPERM
	case vfs.ECLOSED:
		return -fuse.EBADF
	case vfs.ENOTEMPTY:
		return -fuse.ENOTEMPTY
	case vfs.ESPIPE:
		return -fuse.ESPIPE
	case vfs.EBADF:
		return -fuse.EBADF
	case vfs.EROFS:
		return -fuse.EROFS
	case vfs.ENOSYS, fs.ErrorNotImplemented:
		return -fuse.ENOSYS
	case vfs.EINVAL:
		return -fuse.EINVAL
	default:
		return -fuse.EIO
	}
}

// isVideoFile checks if a file is a video file based on extension and size
func isVideoFile(name string, size int64, config *Config) bool {
	// 统一大小检查：小于配置大小的文件不创建STRM
	if size < int64(config.MinFileSize) {
		fs.Debugf(nil, "🚫 [SIZE-LIMIT] 文件小于配置限制: %s (大小: %s < %s)",
			name, fs.SizeSuffix(size), fs.SizeSuffix(int64(config.MinFileSize)))
		return false
	}

	// Check extension
	ext := strings.ToLower(filepath.Ext(name))
	for _, videoExt := range config.VideoExtensions {
		if ext == "."+strings.ToLower(videoExt) {
			fs.Debugf(nil, "✅ [VIDEO-FILE] 符合STRM条件: %s (大小: %s, 格式: %s)",
				name, fs.SizeSuffix(size), ext)
			return true
		}
	}

	fs.Debugf(nil, "🚫 [EXT-LIMIT] 不支持的文件格式: %s (格式: %s)", name, ext)
	return false
}

// shouldHideFile checks if a file should be completely hidden (not shown at all)
func shouldHideFile(name string, size int64, config *Config) bool {
	// 小于配置大小的文件完全隐藏
	if size < int64(config.MinFileSize) {
		fs.Debugf(nil, "🙈 [HIDE-FILE] 文件太小，完全隐藏: %s (大小: %s < %s)",
			name, fs.SizeSuffix(size), fs.SizeSuffix(int64(config.MinFileSize)))
		return true
	}
	return false
}

// shouldBlockDirectAccess checks if direct access to a file should be blocked
// This prevents users from directly accessing original video files, forcing them
// to use .strm files instead, which prevents accidental large downloads
func (fsys *STRMFS) shouldBlockDirectAccess(filePath string) bool {
	// Check if file exists
	node, err := fsys.VFS.Stat(filePath)
	if err != nil {
		return false // If file doesn't exist, let VFS handle the error
	}

	// If it's a directory, allow access
	if node.IsDir() {
		return false
	}

	// If it's a video file that should be virtualized, block direct access
	if isVideoFile(node.Name(), node.Size(), fsys.config) {
		fs.Debugf(nil, "🔒 [BLOCK] Video file should be accessed via .strm: %s (size: %s)",
			filePath, fs.SizeSuffix(node.Size()))
		return true
	}

	// Allow access to non-video files
	return false
}

// shouldShowNonVideoFile determines if a non-video file should be shown in STRM mount
func (fsys *STRMFS) shouldShowNonVideoFile(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))

	// 🚫 隐藏常见的非媒体文件类型
	hiddenExtensions := []string{
		".torrent",                           // BT种子文件
		".zip", ".rar", ".7z", ".tar", ".gz", // 压缩文件
		".txt", ".nfo", ".md", ".log", // 文本文件
		".jpg", ".jpeg", ".png", ".gif", ".bmp", // 图片文件
		".exe", ".msi", ".dmg", ".pkg", // 可执行文件
		".img", ".bin", // 镜像文件
		".pdf", ".doc", ".docx", ".xls", // 文档文件
		".tmp", ".temp", ".cache", // 临时文件
		".ds_store", ".thumbs.db", // 系统文件
	}

	for _, hiddenExt := range hiddenExtensions {
		if ext == hiddenExt {
			return false
		}
	}

	// 🚫 隐藏以点开头的隐藏文件
	if strings.HasPrefix(name, ".") {
		return false
	}

	// 🚫 隐藏常见的非媒体文件名模式
	lowerName := strings.ToLower(name)
	hiddenPatterns := []string{
		"readme", "license", "changelog", "install",
		"setup", "config", "settings", "preferences",
	}

	for _, pattern := range hiddenPatterns {
		if strings.Contains(lowerName, pattern) {
			return false
		}
	}

	// ✅ 默认显示其他文件（如字幕文件 .srt, .ass 等可能有用的文件）
	return true
}

// generateSTRMContent generates .strm file content based on backend and object
func generateSTRMContent(ctx context.Context, obj fs.Object, config *Config) string {
	startTime := time.Now()
	backendName := obj.Fs().Name()

	defer func() {
		duration := time.Since(startTime)
		if duration > 2*time.Millisecond {
			fs.Debugf(nil, "🔗 [GENERATE] Content generation for %s took %v", obj.Remote(), duration)
		}
	}()

	fs.Debugf(nil, "🔗 [GENERATE] Generating content for %s (backend: %s, format: %s)",
		obj.Remote(), backendName, config.URLFormat)

	switch config.URLFormat {
	case "auto":
		return generateSTRMContentAuto(ctx, obj, backendName)
	case "123":
		return generateSTRMContent123(ctx, obj)
	case "115":
		return generateSTRMContent115(ctx, obj)
	case "path":
		fs.Debugf(nil, "🔗 [GENERATE] Using path format for %s", obj.Remote())
		return obj.Remote()
	default:
		fs.Debugf(nil, "🔗 [GENERATE] Unknown format %s, using path for %s", config.URLFormat, obj.Remote())
		return obj.Remote()
	}
}

// generateSTRMContentAuto automatically detects backend and generates appropriate content
func generateSTRMContentAuto(ctx context.Context, obj fs.Object, backendName string) string {
	switch backendName {
	case "123":
		return generateSTRMContent123(ctx, obj)
	case "115":
		return generateSTRMContent115(ctx, obj)
	default:
		return obj.Remote()
	}
}

// generateSTRMContent123 generates .strm content for 123 backend
func generateSTRMContent123(ctx context.Context, obj fs.Object) string {
	startTime := time.Now()

	fs.Debugf(nil, "🔗 [123] Generating content for %s", obj.Remote())

	// Try to get fileId from Object directly (most efficient)
	if o123, ok := obj.(interface{ GetID() string }); ok {
		if fileID := o123.GetID(); fileID != "" {
			result := fmt.Sprintf("123://%s", fileID)
			fs.Infof(nil, "✅ [123] Got fileId for %s: %s (took %v)", obj.Remote(), result, time.Since(startTime))
			return result
		}
		fs.Debugf(nil, "⚠️ [123] GetID() returned empty for %s", obj.Remote())
	} else {
		fs.Debugf(nil, "⚠️ [123] Object doesn't implement GetID() interface for %s", obj.Remote())
	}

	// Alternative: try type assertion to 123 Object
	// This requires importing the 123 backend, but we'll use reflection to avoid import cycles
	if objValue := obj; objValue != nil {
		// Use reflection or interface to get the id field
		if idGetter, ok := objValue.(interface{ ID() string }); ok {
			if fileID := idGetter.ID(); fileID != "" {
				result := fmt.Sprintf("123://%s", fileID)
				fs.Infof(nil, "✅ [123] Got fileId via ID() for %s: %s (took %v)", obj.Remote(), result, time.Since(startTime))
				return result
			}
			fs.Debugf(nil, "⚠️ [123] ID() returned empty for %s", obj.Remote())
		} else {
			fs.Debugf(nil, "⚠️ [123] Object doesn't implement ID() interface for %s", obj.Remote())
		}
	}

	// Fallback to path mode
	fs.Infof(nil, "⚠️ [123] Cannot get fileId for %s, using path mode (took %v)", obj.Remote(), time.Since(startTime))
	return obj.Remote()
}

// generateSTRMContent115 generates .strm content for 115 backend
func generateSTRMContent115(ctx context.Context, obj fs.Object) string {
	startTime := time.Now()

	fs.Debugf(nil, "🔗 [115] Generating content for %s", obj.Remote())

	// Try to get pickCode from Object directly (most efficient)
	if o115, ok := obj.(interface{ GetPickCode() string }); ok {
		if pickCode := o115.GetPickCode(); pickCode != "" {
			result := fmt.Sprintf("115://%s", pickCode)
			fs.Infof(nil, "✅ [115] Got pickCode for %s: %s (took %v)", obj.Remote(), result, time.Since(startTime))
			return result
		}
		fs.Debugf(nil, "⚠️ [115] GetPickCode() returned empty for %s", obj.Remote())
	} else {
		fs.Debugf(nil, "⚠️ [115] Object doesn't implement GetPickCode() interface for %s", obj.Remote())
	}

	// Alternative: try to get pickCode through backend method
	if fs115, ok := obj.Fs().(interface {
		GetPickCodeByPath(context.Context, string) (string, error)
	}); ok {
		apiStartTime := time.Now()
		if pickCode, err := fs115.GetPickCodeByPath(ctx, obj.Remote()); err == nil && pickCode != "" {
			result := fmt.Sprintf("115://%s", pickCode)
			fs.Infof(nil, "✅ [115] Got pickCode via API for %s: %s (API: %v, total: %v)",
				obj.Remote(), result, time.Since(apiStartTime), time.Since(startTime))
			return result
		} else {
			fs.Debugf(nil, "⚠️ [115] GetPickCodeByPath failed for %s: %v (took %v)",
				obj.Remote(), err, time.Since(apiStartTime))
		}
	} else {
		fs.Debugf(nil, "⚠️ [115] Backend doesn't implement GetPickCodeByPath for %s", obj.Remote())
	}

	// Fallback to path mode
	fs.Infof(nil, "⚠️ [115] Cannot get pickCode for %s, using path mode (took %v)", obj.Remote(), time.Since(startTime))
	return obj.Remote()
}
