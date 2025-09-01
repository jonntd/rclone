package strmmount

import (
	"context"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/hash"
)

// STRMPersistentCache 持久化缓存管理器
type STRMPersistentCache struct {
	mu           sync.RWMutex
	backend      string        // 后端类型 (123/115)
	remotePath   string        // 远程路径
	configHash   string        // 配置哈希
	cacheDir     string        // 缓存目录
	cacheFile    string        // 缓存文件路径
	metaFile     string        // 元数据文件路径
	lockFile     string        // 锁文件路径
	ttl          time.Duration // 缓存过期时间
	enabled      bool          // 是否启用
	lastSync     time.Time     // 上次同步时间
	syncInterval time.Duration // 同步间隔
	maxCacheSize int64         // 最大缓存大小
	compression  bool          // 是否启用压缩
}

// CacheData 缓存数据结构
type CacheData struct {
	Version     string            `json:"version"`
	Backend     string            `json:"backend"`
	RemotePath  string            `json:"remote_path"`
	ConfigHash  string            `json:"config_hash"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
	ExpiresAt   time.Time         `json:"expires_at"`
	FileCount   int               `json:"file_count"`
	TotalSize   int64             `json:"total_size"`
	Directories []CachedDirectory `json:"directories"`
	Metadata    CacheMetadata     `json:"metadata"`
}

// CachedDirectory 缓存的目录信息
type CachedDirectory struct {
	Path      string       `json:"path"`
	DirID     string       `json:"dir_id"`
	ModTime   time.Time    `json:"mod_time"`
	FileCount int          `json:"file_count"`
	TotalSize int64        `json:"total_size"`
	Files     []CachedFile `json:"files"`
}

// CachedFile 缓存的文件信息
type CachedFile struct {
	Name     string    `json:"name"`
	Size     int64     `json:"size"`
	ModTime  time.Time `json:"mod_time"`
	FileID   string    `json:"file_id"`
	PickCode string    `json:"pick_code"`
	Hash     string    `json:"hash"`
	MimeType string    `json:"mime_type"`
}

// CacheMetadata 缓存元数据
type CacheMetadata struct {
	APICallsCount    int          `json:"api_calls_count"`
	CacheHitRate     float64      `json:"cache_hit_rate"`
	LastSyncDuration string       `json:"last_sync_duration"` // 存储为字符串格式
	SyncHistory      []SyncRecord `json:"sync_history"`
}

// SyncRecord 同步记录
type SyncRecord struct {
	Timestamp time.Time `json:"timestamp"`
	Duration  string    `json:"duration"` // 存储为字符串格式
	Added     int       `json:"added"`
	Modified  int       `json:"modified"`
	Deleted   int       `json:"deleted"`
	APICount  int       `json:"api_count"`
}

// SyncChanges 同步变更
type SyncChanges struct {
	Added         int
	Deleted       int
	Modified      int
	AddedFiles    []CachedFile
	DeletedFiles  []string
	ModifiedFiles []CachedFile
}

// NewSTRMPersistentCache 创建新的持久化缓存实例
func NewSTRMPersistentCache(backend, remotePath string, minFileSize int64, videoExts []string, urlFormat string) (*STRMPersistentCache, error) {
	// 生成配置哈希
	configHash := generateConfigHash(backend, remotePath, minFileSize, videoExts, urlFormat)

	// 获取缓存目录
	cacheDir := getSTRMCacheDir()
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create cache directory: %w", err)
	}

	// 生成文件路径
	baseName := fmt.Sprintf("%s_%s_%s", backend, sanitizePath(remotePath), configHash[:8])
	cacheFile := filepath.Join(cacheDir, baseName+".json")
	metaFile := filepath.Join(cacheDir, "metadata", baseName+".meta")
	lockFile := filepath.Join(cacheDir, "locks", baseName+".lock")

	// 创建子目录
	os.MkdirAll(filepath.Dir(metaFile), 0755)
	os.MkdirAll(filepath.Dir(lockFile), 0755)

	spc := &STRMPersistentCache{
		backend:      backend,
		remotePath:   remotePath,
		configHash:   configHash,
		cacheDir:     cacheDir,
		cacheFile:    cacheFile,
		metaFile:     metaFile,
		lockFile:     lockFile,
		ttl:          5 * time.Minute, // 默认5分钟
		enabled:      true,
		syncInterval: 1 * time.Minute,
		maxCacheSize: 100 * 1024 * 1024, // 100MB
		compression:  true,
	}

	fs.Infof(nil, "🏗️ [CACHE] 初始化持久化缓存: %s", baseName)
	return spc, nil
}

// getSTRMCacheDir 获取 STRM 缓存目录
func getSTRMCacheDir() string {
	cacheDir := config.GetCacheDir()
	return filepath.Join(cacheDir, "strm-cache")
}

// generateConfigHash 生成配置哈希
func generateConfigHash(backend, remotePath string, minFileSize int64, videoExts []string, urlFormat string) string {
	data := fmt.Sprintf("%s:%s:%v:%v:%s",
		backend, remotePath, minFileSize, videoExts, urlFormat)
	hash := md5.Sum([]byte(data))
	return fmt.Sprintf("%x", hash)
}

// sanitizePath 清理路径用于文件名
func sanitizePath(path string) string {
	// 替换不安全的字符
	safe := strings.ReplaceAll(path, "/", "_")
	safe = strings.ReplaceAll(safe, "\\", "_")
	safe = strings.ReplaceAll(safe, ":", "_")
	safe = strings.ReplaceAll(safe, "*", "_")
	safe = strings.ReplaceAll(safe, "?", "_")
	safe = strings.ReplaceAll(safe, "\"", "_")
	safe = strings.ReplaceAll(safe, "<", "_")
	safe = strings.ReplaceAll(safe, ">", "_")
	safe = strings.ReplaceAll(safe, "|", "_")

	// 限制长度
	if len(safe) > 50 {
		safe = safe[:50]
	}

	return safe
}

// LoadOrCreate 加载或创建缓存
func (spc *STRMPersistentCache) LoadOrCreate(ctx context.Context, fsys fs.Fs) (*CacheData, error) {
	if !spc.enabled {
		return spc.createFreshCache(ctx, fsys)
	}

	// 获取文件锁
	if err := spc.acquireLock(); err != nil {
		fs.Logf(nil, "⚠️ [CACHE] 获取锁失败，使用无缓存模式: %v", err)
		return spc.createFreshCache(ctx, fsys)
	}
	defer spc.releaseLock()

	// 检查缓存文件是否存在
	if !spc.cacheFileExists() {
		fs.Infof(nil, "🆕 [CACHE] 首次启动，创建新缓存")
		return spc.createFreshCache(ctx, fsys)
	}

	// 加载现有缓存
	cacheData, err := spc.loadFromDisk()
	if err != nil {
		fs.Logf(nil, "⚠️ [CACHE] 加载失败，创建新缓存: %v", err)
		return spc.createFreshCache(ctx, fsys)
	}

	// 验证缓存有效性
	if !spc.validateCache(cacheData) {
		fs.Infof(nil, "❌ [CACHE] 缓存无效，创建新缓存")
		return spc.createFreshCache(ctx, fsys)
	}

	// 分层缓存策略：持久化缓存永不过期，但有安全限制
	cacheAge := time.Since(cacheData.CreatedAt)
	maxSafeAge := 7 * 24 * time.Hour // 最大安全年龄：7天

	if cacheAge > maxSafeAge {
		fs.Logf(nil, "⚠️ [CACHE] 缓存年龄过大 (%v > %v)，建议手动刷新", cacheAge, maxSafeAge)
	}

	fs.Infof(nil, "♾️ [CACHE] 使用持久化缓存 (%d 个文件, 创建时间: %v, 年龄: %v)",
		cacheData.FileCount, cacheData.CreatedAt.Format("15:04:05"), cacheAge)
	fs.Infof(nil, "📂 [CACHE] 按需同步模式：访问目录时才同步新文件")

	return cacheData, nil
}

// cacheFileExists 检查缓存文件是否存在
func (spc *STRMPersistentCache) cacheFileExists() bool {
	_, err := os.Stat(spc.cacheFile)
	return err == nil
}

// validateCache 验证缓存有效性
func (spc *STRMPersistentCache) validateCache(cacheData *CacheData) bool {
	// 检查版本兼容性
	if cacheData.Version != "1.0" {
		return false
	}

	// 检查后端匹配
	if cacheData.Backend != spc.backend {
		return false
	}

	// 检查路径匹配
	if cacheData.RemotePath != spc.remotePath {
		return false
	}

	// 检查配置哈希
	if cacheData.ConfigHash != spc.configHash {
		return false
	}

	return true
}

// acquireLock 获取文件锁
func (spc *STRMPersistentCache) acquireLock() error {
	// 简单的文件锁实现
	lockFile, err := os.OpenFile(spc.lockFile, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("failed to acquire lock: %w", err)
	}

	// 写入进程信息
	fmt.Fprintf(lockFile, "%d\n%s\n", os.Getpid(), time.Now().Format(time.RFC3339))
	lockFile.Close()

	return nil
}

// releaseLock 释放文件锁
func (spc *STRMPersistentCache) releaseLock() {
	os.Remove(spc.lockFile)
}

// createCachedFile 从 fs.Object 创建 CachedFile
func (spc *STRMPersistentCache) createCachedFile(obj fs.Object) CachedFile {
	cached := CachedFile{
		Name:    filepath.Base(obj.Remote()),
		Size:    obj.Size(),
		ModTime: obj.ModTime(context.Background()),
	}

	// 获取文件ID和PickCode (根据后端类型)
	switch spc.backend {
	case "123":
		if obj123, ok := obj.(interface{ GetID() string }); ok {
			cached.FileID = obj123.GetID()
		}
	case "115":
		if obj115, ok := obj.(interface{ GetPickCode() string }); ok {
			cached.PickCode = obj115.GetPickCode()
		}
	}

	// 获取哈希值
	if hashValue, err := obj.Hash(context.Background(), hash.SHA1); err == nil && hashValue != "" {
		cached.Hash = "sha1:" + hashValue
	}

	return cached
}

// SetEnabled 启用或禁用持久化缓存
func (spc *STRMPersistentCache) SetEnabled(enabled bool) {
	spc.mu.Lock()
	defer spc.mu.Unlock()
	spc.enabled = enabled
}

// SetTTL 设置缓存过期时间
func (spc *STRMPersistentCache) SetTTL(ttl time.Duration) {
	spc.mu.Lock()
	defer spc.mu.Unlock()
	spc.ttl = ttl
}

// createFreshCache 创建新的缓存
func (spc *STRMPersistentCache) createFreshCache(ctx context.Context, fsys fs.Fs) (*CacheData, error) {
	startTime := time.Now()
	fs.Infof(nil, "🆕 [CACHE] 开始创建新缓存...")

	// 获取远程文件列表
	remoteFiles, err := spc.fetchRemoteFiles(ctx, fsys)
	if err != nil {
		return nil, fmt.Errorf("获取远程文件失败: %w", err)
	}

	// 创建缓存数据
	now := time.Now()
	cacheData := &CacheData{
		Version:     "1.0",
		Backend:     spc.backend,
		RemotePath:  spc.remotePath,
		ConfigHash:  spc.configHash,
		CreatedAt:   now,
		UpdatedAt:   now,
		ExpiresAt:   now.Add(spc.ttl),
		FileCount:   len(remoteFiles),
		Directories: spc.organizeFilesByDirectory(remoteFiles),
		Metadata: CacheMetadata{
			APICallsCount:    1, // 一次 List 调用
			CacheHitRate:     0.0,
			LastSyncDuration: time.Since(startTime).String(),
			SyncHistory: []SyncRecord{
				{
					Timestamp: now,
					Duration:  time.Since(startTime).String(),
					Added:     len(remoteFiles),
					Modified:  0,
					Deleted:   0,
					APICount:  1,
				},
			},
		},
	}

	// 计算总大小
	for _, dir := range cacheData.Directories {
		cacheData.TotalSize += dir.TotalSize
	}

	// 保存到磁盘
	if err := spc.saveToDisk(cacheData); err != nil {
		return nil, fmt.Errorf("保存缓存失败: %w", err)
	}

	duration := time.Since(startTime)
	fs.Infof(nil, "✅ [CACHE] 新缓存创建完成: %d 个文件, 耗时 %v",
		cacheData.FileCount, duration)

	return cacheData, nil
}

// fetchRemoteFiles 获取远程文件列表（智能限制扫描范围）
func (spc *STRMPersistentCache) fetchRemoteFiles(ctx context.Context, fsys fs.Fs) ([]fs.Object, error) {
	// 🛡️ 智能QPS保护：只扫描根目录，避免深度递归
	fs.Infof(nil, "🔍 [CACHE] 智能扫描模式：仅扫描根目录，避免深度递归")

	var files []fs.Object

	// 只列出根目录，不递归子目录
	entries, err := fsys.List(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("列出根目录失败: %w", err)
	}

	// 只处理根目录中的视频文件
	for _, entry := range entries {
		if obj, ok := entry.(fs.Object); ok {
			if spc.isVideoFile(obj) {
				files = append(files, obj)
			}
		}
	}

	fs.Infof(nil, "📁 [CACHE] 根目录扫描完成: %d 个视频文件", len(files))
	return files, nil
}

// isVideoFile 检查是否为视频文件
func (spc *STRMPersistentCache) isVideoFile(obj fs.Object) bool {
	name := obj.Remote()
	size := obj.Size()

	// 检查文件大小 (这里需要从配置获取，暂时硬编码)
	minSize := int64(100 * 1024 * 1024) // 100MB
	if size < minSize {
		return false
	}

	// 检查扩展名
	ext := strings.ToLower(filepath.Ext(name))
	videoExts := []string{".iso", ".mp4", ".mkv", ".avi", ".mov", ".wmv", ".flv", ".webm", ".m4v", ".3gp", ".ts", ".m2ts"}

	for _, videoExt := range videoExts {
		if ext == videoExt {
			return true
		}
	}

	return false
}

// organizeFilesByDirectory 按目录组织文件
func (spc *STRMPersistentCache) organizeFilesByDirectory(files []fs.Object) []CachedDirectory {
	dirMap := make(map[string]*CachedDirectory)

	for _, obj := range files {
		dirPath := filepath.Dir(obj.Remote())
		if dirPath == "." {
			dirPath = ""
		}

		// 获取或创建目录条目
		dir, exists := dirMap[dirPath]
		if !exists {
			dir = &CachedDirectory{
				Path:      dirPath,
				ModTime:   obj.ModTime(context.Background()),
				FileCount: 0,
				TotalSize: 0,
				Files:     []CachedFile{},
			}
			dirMap[dirPath] = dir
		}

		// 添加文件
		cachedFile := spc.createCachedFile(obj)
		dir.Files = append(dir.Files, cachedFile)
		dir.FileCount++
		dir.TotalSize += cachedFile.Size

		// 更新目录修改时间为最新文件的时间
		if cachedFile.ModTime.After(dir.ModTime) {
			dir.ModTime = cachedFile.ModTime
		}
	}

	// 转换为切片
	var directories []CachedDirectory
	for _, dir := range dirMap {
		directories = append(directories, *dir)
	}

	return directories
}

// GetStats 获取缓存统计信息
func (spc *STRMPersistentCache) GetStats() map[string]interface{} {
	spc.mu.RLock()
	defer spc.mu.RUnlock()

	stats := map[string]interface{}{
		"enabled":       spc.enabled,
		"backend":       spc.backend,
		"remote_path":   spc.remotePath,
		"cache_file":    spc.cacheFile,
		"ttl":           spc.ttl.String(),
		"last_sync":     spc.lastSync.Format(time.RFC3339),
		"sync_interval": spc.syncInterval.String(),
	}

	// 添加文件大小信息
	if info, err := os.Stat(spc.cacheFile); err == nil {
		stats["cache_size"] = info.Size()
		stats["cache_mod_time"] = info.ModTime().Format(time.RFC3339)
	}

	return stats
}

// loadFromDisk 从磁盘加载缓存数据
func (spc *STRMPersistentCache) loadFromDisk() (*CacheData, error) {
	data, err := os.ReadFile(spc.cacheFile)
	if err != nil {
		return nil, fmt.Errorf("读取缓存文件失败: %w", err)
	}

	var cacheData CacheData
	if err := json.Unmarshal(data, &cacheData); err != nil {
		return nil, fmt.Errorf("解析缓存数据失败: %w", err)
	}

	return &cacheData, nil
}

// saveToDisk 保存缓存数据到磁盘
func (spc *STRMPersistentCache) saveToDisk(cacheData *CacheData) error {
	// 更新时间戳
	cacheData.UpdatedAt = time.Now()

	// 序列化为 JSON
	data, err := json.MarshalIndent(cacheData, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化缓存数据失败: %w", err)
	}

	// 写入临时文件
	tempFile := spc.cacheFile + ".tmp"
	if err := os.WriteFile(tempFile, data, 0644); err != nil {
		return fmt.Errorf("写入临时文件失败: %w", err)
	}

	// 原子性重命名
	if err := os.Rename(tempFile, spc.cacheFile); err != nil {
		os.Remove(tempFile) // 清理临时文件
		return fmt.Errorf("重命名缓存文件失败: %w", err)
	}

	fs.Debugf(nil, "💾 [CACHE] 缓存已保存: %d 个文件, 大小: %d 字节",
		cacheData.FileCount, len(data))
	return nil
}

// incrementalSync 执行增量同步（智能限制扫描范围）
func (spc *STRMPersistentCache) incrementalSync(ctx context.Context, fsys fs.Fs, oldCache *CacheData) (*CacheData, error) {
	startTime := time.Now()
	fs.Infof(nil, "🔄 [SYNC] 开始智能增量同步（仅根目录）...")

	// 🛡️ 智能QPS保护：只获取根目录文件，避免深度递归
	remoteFiles, err := spc.fetchRemoteFiles(ctx, fsys)
	if err != nil {
		return nil, fmt.Errorf("获取远程文件失败: %w", err)
	}

	// 检测变更
	changes := spc.detectChanges(oldCache, remoteFiles)

	// 应用变更
	newCache := spc.applyChanges(oldCache, changes)
	newCache.UpdatedAt = time.Now()
	newCache.ExpiresAt = time.Now().Add(spc.ttl)

	// 更新元数据
	syncRecord := SyncRecord{
		Timestamp: time.Now(),
		Duration:  time.Since(startTime).String(),
		Added:     changes.Added,
		Modified:  changes.Modified,
		Deleted:   changes.Deleted,
		APICount:  1, // 一次根目录List调用
	}

	newCache.Metadata.SyncHistory = append(newCache.Metadata.SyncHistory, syncRecord)
	if len(newCache.Metadata.SyncHistory) > 10 {
		newCache.Metadata.SyncHistory = newCache.Metadata.SyncHistory[1:]
	}

	newCache.Metadata.LastSyncDuration = syncRecord.Duration
	newCache.Metadata.APICallsCount += syncRecord.APICount

	// 保存新缓存
	if err := spc.saveToDisk(newCache); err != nil {
		return nil, fmt.Errorf("保存缓存失败: %w", err)
	}

	// 更新最后同步时间
	spc.updateLastSyncTime()

	duration := time.Since(startTime)
	fs.Infof(nil, "✅ [SYNC] 智能同步完成: +%d -%d ~%d (耗时 %v, 仅根目录)",
		changes.Added, changes.Deleted, changes.Modified, duration)

	return newCache, nil
}

// detectChanges 检测文件变更
func (spc *STRMPersistentCache) detectChanges(oldCache *CacheData, remoteFiles []fs.Object) *SyncChanges {
	changes := &SyncChanges{}

	// 创建本地文件映射 (path -> CachedFile)
	localFiles := make(map[string]CachedFile)
	for _, dir := range oldCache.Directories {
		for _, file := range dir.Files {
			fullPath := filepath.Join(dir.Path, file.Name)
			localFiles[fullPath] = file
		}
	}

	// 创建远程文件映射
	remoteFileMap := make(map[string]fs.Object)
	for _, obj := range remoteFiles {
		remoteFileMap[obj.Remote()] = obj
	}

	// 检测新增和修改的文件
	for remotePath, remoteObj := range remoteFileMap {
		if localFile, exists := localFiles[remotePath]; exists {
			// 文件存在，检查是否修改
			if spc.isFileModified(localFile, remoteObj) {
				changes.ModifiedFiles = append(changes.ModifiedFiles,
					spc.createCachedFile(remoteObj))
				changes.Modified++
			}
		} else {
			// 新文件
			changes.AddedFiles = append(changes.AddedFiles,
				spc.createCachedFile(remoteObj))
			changes.Added++
		}
	}

	// 安全的删除检测：只有在有远程文件数据时才检测删除
	if len(remoteFiles) > 0 {
		// 有远程文件数据，可以安全地检测删除
		for localPath := range localFiles {
			if _, exists := remoteFileMap[localPath]; !exists {
				changes.DeletedFiles = append(changes.DeletedFiles, localPath)
				changes.Deleted++
				fs.Debugf(nil, "🗑️ [DETECT] 检测到删除文件: %s", localPath)
			}
		}
	} else {
		// 没有远程文件数据，可能是API调用失败，不执行删除检测避免误删
		fs.Debugf(nil, "⚠️ [DETECT] 没有远程文件数据，跳过删除检测以避免误删")
	}

	return changes
}

// isFileModified 检查文件是否被修改
func (spc *STRMPersistentCache) isFileModified(local CachedFile, remote fs.Object) bool {
	// 比较文件大小
	if local.Size != remote.Size() {
		return true
	}

	// 比较修改时间
	if !local.ModTime.Equal(remote.ModTime(context.Background())) {
		return true
	}

	// 比较哈希值 (如果可用)
	if hashValue, err := remote.Hash(context.Background(), hash.SHA1); err == nil && hashValue != "" {
		expectedHash := "sha1:" + hashValue
		if local.Hash != expectedHash {
			return true
		}
	}

	return false
}

// applyChanges 应用增量变更到缓存
func (spc *STRMPersistentCache) applyChanges(oldCache *CacheData, changes *SyncChanges) *CacheData {
	// 创建新的缓存数据，复制原有数据
	newCache := &CacheData{
		Version:     oldCache.Version,
		Backend:     oldCache.Backend,
		RemotePath:  oldCache.RemotePath,
		ConfigHash:  oldCache.ConfigHash,
		CreatedAt:   oldCache.CreatedAt,
		Metadata:    oldCache.Metadata,
		Directories: make([]CachedDirectory, len(oldCache.Directories)),
	}

	// 复制原有目录结构
	copy(newCache.Directories, oldCache.Directories)

	// 应用增量变更
	fs.Infof(nil, "🔄 [APPLY] 应用增量变更: +%d ~%d -%d",
		changes.Added, changes.Modified, changes.Deleted)

	// 1. 添加新文件
	for _, addedFile := range changes.AddedFiles {
		spc.addFileToDirectories(&newCache.Directories, addedFile)
	}

	// 2. 更新修改的文件
	for _, modifiedFile := range changes.ModifiedFiles {
		spc.updateFileInDirectories(&newCache.Directories, modifiedFile)
	}

	// 3. 删除已删除的文件
	for _, deletedPath := range changes.DeletedFiles {
		spc.removeFileFromDirectories(&newCache.Directories, deletedPath)
	}

	// 重新计算统计信息
	newCache.FileCount = 0
	newCache.TotalSize = 0
	for _, dir := range newCache.Directories {
		newCache.FileCount += dir.FileCount
		newCache.TotalSize += dir.TotalSize
	}

	fs.Infof(nil, "✅ [APPLY] 增量变更应用完成: %d 个文件, 总大小 %d",
		newCache.FileCount, newCache.TotalSize)

	return newCache
}

// shouldPerformBackgroundSync 判断是否需要执行后台增量同步
func (spc *STRMPersistentCache) shouldPerformBackgroundSync(cacheData *CacheData) bool {
	spc.mu.RLock()
	defer spc.mu.RUnlock()

	// 如果缓存被禁用，不执行同步
	if !spc.enabled {
		return false
	}

	// 检查距离上次同步的时间
	timeSinceLastSync := time.Since(spc.lastSync)

	// 如果距离上次同步超过同步间隔，执行同步
	if timeSinceLastSync > spc.syncInterval {
		fs.Debugf(nil, "🕐 [CACHE] 距离上次同步 %v，超过间隔 %v，需要同步",
			timeSinceLastSync, spc.syncInterval)
		return true
	}

	// 检查缓存年龄，如果缓存很旧，也执行同步
	cacheAge := time.Since(cacheData.UpdatedAt)
	maxCacheAge := 24 * time.Hour // 最大缓存年龄24小时

	if cacheAge > maxCacheAge {
		fs.Debugf(nil, "📅 [CACHE] 缓存年龄 %v 超过最大年龄 %v，需要同步",
			cacheAge, maxCacheAge)
		return true
	}

	fs.Debugf(nil, "⏭️ [CACHE] 无需同步：距离上次同步 %v，缓存年龄 %v",
		timeSinceLastSync, cacheAge)
	return false
}

// updateLastSyncTime 更新最后同步时间
func (spc *STRMPersistentCache) updateLastSyncTime() {
	spc.mu.Lock()
	defer spc.mu.Unlock()
	spc.lastSync = time.Now()
}

// OnDemandSync 按需同步：访问目录时触发同步（带防重复机制）
func (spc *STRMPersistentCache) OnDemandSync(ctx context.Context, fsys fs.Fs, dirPath string) error {
	fs.Infof(nil, "🎯 [CACHE] 按需同步开始 - 处理目录: %s", dirPath)

	if !spc.enabled {
		fs.Infof(nil, "⏭️ [CACHE] 持久化缓存功能未启用 - 跳过同步操作")
		return nil
	}

	spc.mu.Lock()
	defer spc.mu.Unlock()

	// 智能跳过机制：多重检查减少不必要的API调用
	if !spc.shouldSyncDirectory(dirPath) {
		fs.Infof(nil, "✅ [CACHE] 缓存命中 - 目录 %s 缓存仍然有效，跳过同步", dirPath)
		return nil
	}

	timeSinceLastSync := time.Since(spc.lastSync)
	minSyncInterval := spc.getDirectorySyncInterval(dirPath)

	fs.Infof(nil, "🔄 [CACHE] 缓存失效，开始同步 - %s (上次同步: %v前, 缓存间隔: %v)",
		dirPath, timeSinceLastSync, minSyncInterval)

	// 加载当前缓存
	cacheData, err := spc.loadFromDisk()
	if err != nil {
		fs.Errorf(nil, "❌ [ON-DEMAND] 加载缓存失败: %v", err)
		return err
	}

	// 执行精确的目录同步，而不是总是扫描根目录
	newCache, err := spc.incrementalSyncDirectory(ctx, fsys, cacheData, dirPath)
	if err != nil {
		fs.Errorf(nil, "❌ [ON-DEMAND] 同步失败: %v", err)
		return err
	}

	// 保存更新后的缓存
	if err := spc.saveToDisk(newCache); err != nil {
		fs.Errorf(nil, "❌ [ON-DEMAND] 保存缓存失败: %v", err)
		return err
	}

	// 更新最后同步时间
	spc.lastSync = time.Now()

	fs.Infof(nil, "✅ [SYNC] 目录同步成功完成 - %s (数据已更新)", dirPath)
	return nil
}

// addFileToDirectories 真正添加文件到目录列表
func (spc *STRMPersistentCache) addFileToDirectories(directories *[]CachedDirectory, file CachedFile) {
	// 查找或创建根目录
	var rootDir *CachedDirectory
	for i := range *directories {
		if (*directories)[i].Path == "" {
			rootDir = &(*directories)[i]
			break
		}
	}

	if rootDir == nil {
		// 创建根目录
		*directories = append(*directories, CachedDirectory{
			Path:  "",
			Files: []CachedFile{},
		})
		rootDir = &(*directories)[len(*directories)-1]
	}

	// 检查文件是否已存在
	for i, existingFile := range rootDir.Files {
		if existingFile.Name == file.Name {
			// 更新现有文件
			rootDir.Files[i] = file
			fs.Debugf(nil, "📝 [ADD] 更新现有文件: %s", file.Name)
			return
		}
	}

	// 添加新文件
	rootDir.Files = append(rootDir.Files, file)
	rootDir.FileCount++
	rootDir.TotalSize += file.Size
	fs.Debugf(nil, "📁 [ADD] 添加新文件: %s (大小: %d)", file.Name, file.Size)
}

// updateFileInDirectories 真正更新目录列表中的文件
func (spc *STRMPersistentCache) updateFileInDirectories(directories *[]CachedDirectory, file CachedFile) {
	// 先删除旧文件，再添加新文件
	spc.removeFileFromDirectories(directories, file.Name)
	spc.addFileToDirectories(directories, file)
	fs.Debugf(nil, "📝 [UPDATE] 更新文件: %s", file.Name)
}

// removeFileFromDirectories 从目录列表中删除文件
func (spc *STRMPersistentCache) removeFileFromDirectories(directories *[]CachedDirectory, filePath string) {
	fileName := filepath.Base(filePath)
	fs.Debugf(nil, "🗑️ [DELETE] 删除文件: %s", fileName)

	// 遍历所有目录，找到并删除文件
	for i := range *directories {
		dir := &(*directories)[i]
		for j, file := range dir.Files {
			if file.Name == fileName {
				// 删除文件
				dir.Files = append(dir.Files[:j], dir.Files[j+1:]...)
				dir.FileCount--
				dir.TotalSize -= file.Size
				fs.Debugf(nil, "✅ [DELETE] 已删除文件: %s", fileName)
				return
			}
		}
	}
}

// getDirectorySyncInterval 根据目录类型返回智能缓存间隔
func (spc *STRMPersistentCache) getDirectorySyncInterval(dirPath string) time.Duration {
	// 根据目录特性设置不同的缓存时间，大幅减少API调用
	switch {
	case dirPath == "" || dirPath == "/":
		// 根目录：变化较少，设置长缓存时间
		return 4 * time.Hour
	case strings.Contains(strings.ToLower(dirPath), "download"):
		// 下载目录：可能有新文件，但不需要太频繁检查
		return 1 * time.Hour
	case strings.Contains(strings.ToLower(dirPath), "temp") || strings.Contains(strings.ToLower(dirPath), "tmp"):
		// 临时目录：变化频繁，但用户访问少
		return 30 * time.Minute
	case strings.Count(dirPath, "/") > 2:
		// 深层目录：很少变化，设置最长缓存时间
		return 8 * time.Hour
	default:
		// 普通目录：中等缓存时间
		return 2 * time.Hour
	}
}

// incrementalSyncDirectory 精确同步指定目录，减少不必要的API调用
func (spc *STRMPersistentCache) incrementalSyncDirectory(ctx context.Context, fsys fs.Fs, oldCache *CacheData, targetDir string) (*CacheData, error) {
	startTime := time.Now()
	fs.Debugf(nil, "🎯 [SYNC] 开始精确同步 - 目标目录: %s (检查文件变更)", targetDir)

	// 精确获取指定目录的文件，而不是总是扫描根目录
	remoteFiles, err := spc.fetchRemoteFilesFromDirectory(ctx, fsys, targetDir)
	if err != nil {
		return nil, fmt.Errorf("获取目录 %s 文件失败: %w", targetDir, err)
	}

	// 检测变更
	changes := spc.detectChanges(oldCache, remoteFiles)

	// 应用变更
	newCache := spc.applyChanges(oldCache, changes)
	newCache.UpdatedAt = time.Now()

	// 更新元数据
	syncRecord := SyncRecord{
		Timestamp: time.Now(),
		Duration:  time.Since(startTime).String(),
		Added:     changes.Added,
		Modified:  changes.Modified,
		Deleted:   changes.Deleted,
		APICount:  1, // 一次精确目录调用
	}

	newCache.Metadata.SyncHistory = append(newCache.Metadata.SyncHistory, syncRecord)
	if len(newCache.Metadata.SyncHistory) > 10 {
		newCache.Metadata.SyncHistory = newCache.Metadata.SyncHistory[1:]
	}

	newCache.Metadata.LastSyncDuration = syncRecord.Duration
	newCache.Metadata.APICallsCount += syncRecord.APICount

	// 保存新缓存
	if err := spc.saveToDisk(newCache); err != nil {
		return nil, fmt.Errorf("保存缓存失败: %w", err)
	}

	// 更新最后同步时间
	spc.updateLastSyncTime()

	duration := time.Since(startTime)
	fs.Infof(nil, "✅ [SYNC] 精确同步完成 %s: +%d -%d ~%d (耗时 %v)",
		targetDir, changes.Added, changes.Deleted, changes.Modified, duration)

	return newCache, nil
}

// fetchRemoteFilesFromDirectory 精确获取指定目录的文件列表
func (spc *STRMPersistentCache) fetchRemoteFilesFromDirectory(ctx context.Context, fsys fs.Fs, targetDir string) ([]fs.Object, error) {
	fs.Infof(nil, "🔍 [CACHE] 精确扫描开始 - 目录: %s (查找视频文件)", targetDir)

	var files []fs.Object

	// 精确列出指定目录，而不是总是扫描根目录
	entries, err := fsys.List(ctx, targetDir)
	if err != nil {
		return nil, fmt.Errorf("列出目录 %s 失败: %w", targetDir, err)
	}

	// 只处理该目录中的视频文件
	for _, entry := range entries {
		if obj, ok := entry.(fs.Object); ok {
			if spc.isVideoFile(obj) {
				files = append(files, obj)
			}
		}
	}

	fs.Infof(nil, "📁 [CACHE] 扫描完成 - 目录: %s, 发现 %d 个视频文件", targetDir, len(files))
	return files, nil
}

// shouldSyncDirectory 智能判断是否需要同步目录，大幅减少API调用
func (spc *STRMPersistentCache) shouldSyncDirectory(dirPath string) bool {
	// 1. 检查全局同步时间
	timeSinceLastSync := time.Since(spc.lastSync)
	minSyncInterval := spc.getDirectorySyncInterval(dirPath)

	if timeSinceLastSync < minSyncInterval {
		fs.Debugf(nil, "⏭️ [SKIP] 跳过同步 %s：距离上次同步 %v < %v",
			dirPath, timeSinceLastSync, minSyncInterval)
		return false
	}

	// 2. 检查缓存数据是否存在且较新
	cacheData, err := spc.loadFromDisk()
	if err == nil && cacheData != nil {
		cacheAge := time.Since(cacheData.UpdatedAt)
		maxCacheAge := spc.getDirectorySyncInterval(dirPath)

		if cacheAge < maxCacheAge {
			fs.Debugf(nil, "⏭️ [SKIP] 跳过同步 %s：缓存年龄 %v < %v",
				dirPath, cacheAge, maxCacheAge)
			return false
		}
	}

	// 3. 对于深层目录，更加保守
	if strings.Count(dirPath, "/") > 3 {
		// 深层目录很少变化，延长检查间隔
		if timeSinceLastSync < 12*time.Hour {
			fs.Debugf(nil, "⏭️ [SKIP] 跳过深层目录同步 %s：%v < 12h",
				dirPath, timeSinceLastSync)
			return false
		}
	}

	// 4. 对于特殊目录名，跳过同步
	lowerPath := strings.ToLower(dirPath)
	skipDirs := []string{"temp", "tmp", "cache", "log", "logs", ".git", ".svn"}
	for _, skipDir := range skipDirs {
		if strings.Contains(lowerPath, skipDir) {
			fs.Debugf(nil, "⏭️ [SKIP] 跳过特殊目录同步: %s", dirPath)
			return false
		}
	}

	fs.Debugf(nil, "✅ [SYNC] 需要同步目录: %s", dirPath)
	return true
}
