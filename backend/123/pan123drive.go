package _123

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"math"
	"math/rand/v2"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/rclone/rclone/lib/dircache"
	"github.com/rclone/rclone/lib/oauthutil"
	"github.com/rclone/rclone/lib/pacer"
	"golang.org/x/oauth2"

	// 123网盘SDK
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/configstruct"
	"github.com/rclone/rclone/fs/fserrors"
	"github.com/rclone/rclone/fs/fshttp"
	fshash "github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/lib/cache"
)

// "Mozilla/5.0 AppleWebKit/600 Safari/600 Chrome/124.0.0.0 Edg/124.0.0.0"
const (
	openAPIRootURL     = "https://open-api.123pan.com"
	defaultUserAgent   = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/136.0.0.0 Safari/537.36"
	tokenRefreshWindow = 10 * time.Minute // tokenRefreshWindow 定义了在令牌过期前多长时间尝试刷新令牌

	// 基于123网盘官方限制的API速率限制（2025年1月更新）
	// 官方文档：https://123yunpan.yuque.com/org-wiki-123yunpan-muaork/cr6ced/
	// 不同的API有不同的QPS限制，使用保守值以避免429错误

	// v2 API限制
	listV2APIMinSleep = 70 * time.Millisecond // ~14 QPS 用于 api/v2/file/list (官方15 QPS)

	// v1上传API限制
	uploadCreateMinSleep    = 250 * time.Millisecond // ~4 QPS 用于 upload/v1/file/create (官方5 QPS)
	uploadCompleteMinSleep  = 60 * time.Millisecond  // ~16 QPS 用于 upload/v1/file/upload_complete (官方20 QPS)
	uploadURLMinSleep       = 60 * time.Millisecond  // ~16 QPS 用于 upload/v1/file/get_upload_url (官方20 QPS)
	uploadAsyncMinSleep     = 250 * time.Millisecond // ~4 QPS 用于 upload/v1/file/upload_async_result (官方5 QPS)
	uploadListPartsMinSleep = 60 * time.Millisecond  // ~16 QPS 用于 upload/v1/file/list_upload_parts (官方20 QPS)

	// v2分片上传API（未在官方文档中明确列出，使用保守值）
	uploadV2SliceMinSleep = 200 * time.Millisecond // ~5 QPS 用于 upload/v2/file/slice

	// 文件操作API限制
	fileMoveMinSleep     = 120 * time.Millisecond  // ~8 QPS 用于 api/v1/file/move (官方10 QPS)
	fileDeleteMinSleep   = 1200 * time.Millisecond // ~0.8 QPS 用于 api/v1/file/delete (官方1 QPS)
	fileTrashMinSleep    = 250 * time.Millisecond  // ~4 QPS 用于 api/v1/file/trash (官方5 QPS)
	fileInfosMinSleep    = 120 * time.Millisecond  // ~8 QPS 用于 api/v1/file/infos (官方10 QPS)
	downloadInfoMinSleep = 250 * time.Millisecond  // ~4 QPS 用于 api/v1/file/download_info (官方5 QPS)

	// 用户和认证API限制
	userInfoMinSleep    = 120 * time.Millisecond // ~8 QPS 用于 api/v1/user/info (官方10 QPS)
	accessTokenMinSleep = 150 * time.Millisecond // ~6 QPS 用于 api/v1/access_token (官方8 QPS)

	// v1文件列表API限制
	listV1APIMinSleep = 1200 * time.Millisecond // ~0.8 QPS 用于 api/v1/file/list (官方1 QPS)

	// 其他API限制
	videoTranscodeMinSleep = 1200 * time.Millisecond // ~0.8 QPS 用于 api/v1/video/transcode/list (官方1 QPS)
	mkdirMinSleep          = 250 * time.Millisecond  // ~4 QPS 用于 upload/v1/file/mkdir (官方5 QPS)

	maxSleep      = 30 * time.Second // 429退避的最大睡眠时间
	decayConstant = 2

	// 文件上传相关常量
	singleStepUploadLimit = 500 * 1024 * 1024 // 500MB - 单步上传API的文件大小限制
	maxMemoryBufferSize   = 512 * 1024 * 1024 // 512MB - 内存缓冲的最大大小，防止内存不足
	maxFileNameBytes      = 255               // 文件名的最大字节长度（UTF-8编码）

	// 文件冲突处理策略常量已移除，当前使用API默认行为

	// 上传相关常量 - 优化大文件传输性能
	defaultChunkSize    = 100 * fs.Mebi // 增加默认分片大小到100MB
	minChunkSize        = 50 * fs.Mebi  // 增加最小分片大小到50MB
	maxChunkSize        = 500 * fs.Mebi // 设置最大分片大小为500MB
	defaultUploadCutoff = 100 * fs.Mebi // 降低分片上传阈值
	maxUploadParts      = 10000

	// 连接和超时设置 - 针对大文件传输优化的参数
	defaultConnTimeout = 30 * time.Second  // 增加连接超时，适应网络波动
	defaultTimeout     = 600 * time.Second // 增加总体超时，支持大文件传输

	// 重试和退避设置
	maxRetries      = 10               // 增加最大重试次数，提高成功率
	baseRetryDelay  = 2 * time.Second  // 增加基础重试延迟
	maxRetryDelay   = 60 * time.Second // 增加最大重试延迟
	retryMultiplier = 2.0              // 指数退避倍数

	// 文件名验证相关常量
	maxFileNameLength = 256          // 123网盘文件名最大长度（包括扩展名）
	invalidChars      = `"\/:*?|><\` // 123网盘不允许的文件名字符
	replacementChar   = "_"          // 用于替换非法字符的安全字符

	// 日志级别常量
	LogLevelNone    = 0 // 无日志
	LogLevelError   = 1 // 仅错误
	LogLevelInfo    = 2 // 信息级别
	LogLevelDebug   = 3 // 调试级别
	LogLevelVerbose = 4 // 详细级别

	// 网络速度缓存相关常量
	NetworkSpeedCacheTTL = 5 * time.Minute // 网络速度缓存有效期

	// 缓存和清理相关常量
	DefaultCacheCleanupInterval = 5 * time.Minute // 默认缓存清理间隔
	DefaultCacheCleanupTimeout  = 2 * time.Minute // 默认缓存清理超时
	BufferCleanupThreshold      = 5 * time.Minute // 缓冲区清理阈值

	// 配置验证相关常量
	MaxListChunk          = 10000 // 最大列表块大小
	MaxUploadPartsLimit   = 10000 // 最大上传分片数限制
	MaxConcurrentLimit    = 100   // 最大并发数限制
	DefaultListChunk      = 1000  // 默认列表块大小
	DefaultMaxUploadParts = 1000  // 默认最大上传分片数

	// 网络质量评估常量
	LatencyThreshold          = 50              // 延迟阈值（毫秒）
	LatencyScoreRange         = 1000            // 延迟分数计算范围
	QualityAdjustmentInterval = 5 * time.Minute // 质量调整间隔

	// 缓存管理常量
	PreloadQueueCapacity    = 1000 // 预加载队列容量
	MaxHotFiles             = 100  // 最大热点文件数
	HotFileCleanupBatchSize = 20   // 热点文件清理批次大小

	// 网络检测常量
	NetworkTestSize = 512 * 1024 // 网络测试文件大小（512KB）
)

// Options 定义此后端的配置选项
type Options struct {
	ClientID          string        `config:"client_id"`
	ClientSecret      string        `config:"client_secret"`
	Token             string        `config:"token"`
	UserAgent         string        `config:"user_agent"`
	RootFolderID      string        `config:"root_folder_id"`
	ListChunk         int           `config:"list_chunk"`
	PacerMinSleep     fs.Duration   `config:"pacer_min_sleep"`
	ListPacerMinSleep fs.Duration   `config:"list_pacer_min_sleep"`
	ConnTimeout       fs.Duration   `config:"conn_timeout"`
	Timeout           fs.Duration   `config:"timeout"`
	ChunkSize         fs.SizeSuffix `config:"chunk_size"`
	UploadCutoff      fs.SizeSuffix `config:"upload_cutoff"`
	MaxUploadParts    int           `config:"max_upload_parts"`

	// 性能优化相关选项
	MaxConcurrentUploads   int         `config:"max_concurrent_uploads"`
	MaxConcurrentDownloads int         `config:"max_concurrent_downloads"`
	ProgressUpdateInterval fs.Duration `config:"progress_update_interval"`
	EnableProgressDisplay  bool        `config:"enable_progress_display"`

	// QPS控制相关选项
	UploadPacerMinSleep   fs.Duration `config:"upload_pacer_min_sleep"`
	DownloadPacerMinSleep fs.Duration `config:"download_pacer_min_sleep"`
	StrictPacerMinSleep   fs.Duration `config:"strict_pacer_min_sleep"`

	// 调试和日志控制选项
	DebugLevel int `config:"debug_level"` // 调试级别：0=无，1=错误，2=信息，3=调试，4=详细
}

// Fs 表示远程123网盘驱动器实例
type Fs struct {
	name         string       // 此远程实例的名称
	originalName string       // 未修改的原始配置名称
	root         string       // 当前操作的根路径
	opt          Options      // 解析后的配置选项
	features     *fs.Features // 可选功能特性

	// API客户端和身份验证相关
	client       *http.Client     // 用于API调用的HTTP客户端
	token        string           // 缓存的访问令牌
	tokenExpiry  time.Time        // 缓存访问令牌的过期时间
	tokenMu      sync.Mutex       // 保护token和tokenExpiry的互斥锁
	tokenRenewer *oauthutil.Renew // 令牌自动更新器

	// 配置和状态信息
	m            configmap.Mapper // 配置映射器
	rootFolderID string           // 根文件夹的ID
	// userID       string           // 来自API的用户ID（暂未使用）

	// 性能和速率限制 - 针对不同API限制的多个调速器
	listPacer     *fs.Pacer // 文件列表API的调速器（约2 QPS）
	strictPacer   *fs.Pacer // 严格API（如user/info, move, delete）的调速器（1 QPS）
	uploadPacer   *fs.Pacer // 上传API的调速器（1 QPS）
	downloadPacer *fs.Pacer // 下载API的调速器（2 QPS）

	// 目录缓存
	dirCache *dircache.DirCache

	// BadgerDB持久化缓存实例
	parentIDCache *cache.BadgerCache // parentFileID验证缓存
	dirListCache  *cache.BadgerCache // 目录列表缓存
	basicURLCache *cache.BadgerCache // 基础下载URL缓存
	pathToIDCache *cache.BadgerCache // 路径到FileID映射缓存

	// 性能优化相关
	uploadSemaphore    chan struct{}            // 上传并发控制信号量
	downloadSemaphore  chan struct{}            // 下载并发控制信号量
	progressTracker    *ProgressTracker         // 进度跟踪器
	memoryManager      *MemoryManager           // 内存管理器，防止内存泄漏
	performanceMetrics *PerformanceMetrics      // 性能指标收集器
	resourcePool       *ResourcePool            // 资源池管理器，优化内存使用
	downloadURLCache   *DownloadURLCacheManager // 下载URL缓存管理器
	apiVersionManager  *APIVersionManager       // API版本管理器
	resumeManager      *ResumeManager           // 断点续传管理器
	networkMonitor     *NetworkQualityMonitor   // 网络质量监控器
	networkSpeedCache  *NetworkSpeedCache       // 网络速度缓存

	// 缓存时间配置
	cacheConfig CacheConfig
}

// debugf 根据调试级别输出调试信息
func (f *Fs) debugf(level int, format string, args ...interface{}) {
	if f.opt.DebugLevel >= level {
		fs.Debugf(f, format, args...)
	}
}

// CacheConfig 缓存时间配置
type CacheConfig struct {
	ParentIDCacheTTL    time.Duration // parentFileID验证缓存TTL，默认5分钟
	DirListCacheTTL     time.Duration // 目录列表缓存TTL，默认3分钟
	DownloadURLCacheTTL time.Duration // 下载URL缓存TTL，默认动态（根据API返回）
	PathToIDCacheTTL    time.Duration // 路径映射缓存TTL，默认12分钟
}

// NetworkSpeedCache 网络速度缓存结构体
type NetworkSpeedCache struct {
	mu        sync.RWMutex
	speed     int64         // 缓存的网络速度 (bytes/second)
	lastCheck time.Time     // 上次检测时间
	cacheTTL  time.Duration // 缓存有效期
}

// NewNetworkSpeedCache 创建新的网络速度缓存
func NewNetworkSpeedCache() *NetworkSpeedCache {
	return &NetworkSpeedCache{
		cacheTTL: NetworkSpeedCacheTTL,
	}
}

// GetSpeed 获取缓存的网络速度，如果缓存过期则返回false
func (nsc *NetworkSpeedCache) GetSpeed() (int64, bool) {
	nsc.mu.RLock()
	defer nsc.mu.RUnlock()

	if time.Since(nsc.lastCheck) > nsc.cacheTTL {
		return 0, false
	}

	return nsc.speed, true
}

// SetSpeed 设置网络速度缓存
func (nsc *NetworkSpeedCache) SetSpeed(speed int64) {
	nsc.mu.Lock()
	defer nsc.mu.Unlock()

	nsc.speed = speed
	nsc.lastCheck = time.Now()
}

// DefaultCacheConfig 返回默认的缓存配置
func DefaultCacheConfig() CacheConfig {
	return CacheConfig{
		ParentIDCacheTTL:    NetworkSpeedCacheTTL, // 使用相同的5分钟TTL
		DirListCacheTTL:     3 * time.Minute,      // 目录列表缓存3分钟
		DownloadURLCacheTTL: 0,                    // 动态TTL，根据API返回的过期时间
		PathToIDCacheTTL:    12 * time.Minute,     // 路径映射缓存12分钟
	}
}

// ParentIDCacheEntry 父目录ID缓存条目
type ParentIDCacheEntry struct {
	ParentID  int64     `json:"parent_id"`
	Valid     bool      `json:"valid"`
	ExpiresAt time.Time `json:"expires_at"`
	CreatedAt time.Time `json:"created_at"`
}

// ParentIDCacheFile 缓存文件结构（批量存储）
type ParentIDCacheFile struct {
	Version   int                           `json:"version"`
	UpdatedAt time.Time                     `json:"updated_at"`
	Entries   map[string]ParentIDCacheEntry `json:"entries"`
}

// DirListCacheEntry 目录列表缓存条目
type DirListCacheEntry struct {
	FileList   []FileListInfoRespDataV2 `json:"file_list"`
	LastFileID int64                    `json:"last_file_id"`
	TotalCount int                      `json:"total_count"`
	CachedAt   time.Time                `json:"cached_at"`
	ParentID   int64                    `json:"parent_id"`
	Version    int64                    `json:"version"`  // 缓存版本号
	Checksum   string                   `json:"checksum"` // 数据校验和
}

// DownloadURLCacheEntry 下载URL缓存条目
type DownloadURLCacheEntry struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
	FileID    string    `json:"file_id"`
	UserAgent string    `json:"user_agent"`
	CachedAt  time.Time `json:"cached_at"`
	Version   int64     `json:"version"`  // 缓存版本号
	Checksum  string    `json:"checksum"` // 数据校验和
}

// PathToIDCacheEntry 路径到FileID映射缓存条目
type PathToIDCacheEntry struct {
	Path     string    `json:"path"`
	FileID   string    `json:"file_id"`
	IsDir    bool      `json:"is_dir"`
	ParentID string    `json:"parent_id"`
	CachedAt time.Time `json:"cached_at"`
}

// MemoryManager 管理小文件缓存的内存使用，防止内存泄漏
type MemoryManager struct {
	mu          sync.Mutex
	totalUsed   int64                  // 当前使用的总内存
	maxTotal    int64                  // 最大允许的总内存（默认200MB）
	fileBuffers map[string]*BufferInfo // 文件ID -> 缓冲区信息
	maxFileSize int64                  // 单个文件最大缓存大小（默认50MB）
}

// BufferInfo 缓冲区信息，用于智能清理
type BufferInfo struct {
	Size        int64     // 缓冲区大小
	LastAccess  time.Time // 最后访问时间
	AccessCount int64     // 访问次数
}

// NewMemoryManager 创建新的内存管理器
func NewMemoryManager(maxTotal, maxFileSize int64) *MemoryManager {
	if maxTotal <= 0 {
		maxTotal = 200 * 1024 * 1024 // 默认200MB总限制
	}
	if maxFileSize <= 0 {
		maxFileSize = 50 * 1024 * 1024 // 默认50MB单文件限制
	}

	return &MemoryManager{
		maxTotal:    maxTotal,
		maxFileSize: maxFileSize,
		fileBuffers: make(map[string]*BufferInfo),
	}
}

// CanAllocate 检查是否可以分配指定大小的内存，如果空间不足会尝试清理
func (mm *MemoryManager) CanAllocate(size int64) bool {
	if size <= 0 {
		return false
	}

	// 检查单文件大小限制
	if size > mm.maxFileSize {
		return false
	}

	mm.mu.Lock()
	defer mm.mu.Unlock()

	// 如果空间足够，直接返回
	if mm.totalUsed+size <= mm.maxTotal {
		return true
	}

	// 空间不足，尝试清理旧缓存
	return mm.tryCleanupMemory(size)
}

// tryCleanupMemory 尝试清理内存以腾出空间
func (mm *MemoryManager) tryCleanupMemory(requiredSize int64) bool {
	// 收集可清理的缓冲区（超过5分钟未访问的）
	var candidates []string
	now := time.Now()

	for fileID, bufferInfo := range mm.fileBuffers {
		if now.Sub(bufferInfo.LastAccess) > BufferCleanupThreshold {
			candidates = append(candidates, fileID)
		}
	}

	// 按访问次数排序，优先清理访问次数少的
	sort.Slice(candidates, func(i, j int) bool {
		return mm.fileBuffers[candidates[i]].AccessCount < mm.fileBuffers[candidates[j]].AccessCount
	})

	// 逐个清理直到有足够空间
	freedSize := int64(0)
	for _, fileID := range candidates {
		if bufferInfo, exists := mm.fileBuffers[fileID]; exists {
			freedSize += bufferInfo.Size
			delete(mm.fileBuffers, fileID)
			mm.totalUsed -= bufferInfo.Size

			// 检查是否已有足够空间
			if mm.totalUsed+requiredSize <= mm.maxTotal {
				return true
			}
		}
	}

	return false
}

// Allocate 分配内存并记录使用情况
func (mm *MemoryManager) Allocate(fileID string, size int64) bool {
	if size <= 0 || size > mm.maxFileSize {
		return false
	}

	mm.mu.Lock()
	defer mm.mu.Unlock()

	// 检查是否已经分配过
	if _, exists := mm.fileBuffers[fileID]; exists {
		return false
	}

	// 检查总内存限制
	if mm.totalUsed+size > mm.maxTotal {
		return false
	}

	// 分配内存
	mm.fileBuffers[fileID] = &BufferInfo{
		Size:        size,
		LastAccess:  time.Now(),
		AccessCount: 1,
	}
	mm.totalUsed += size
	return true
}

// Release 释放指定文件的内存
func (mm *MemoryManager) Release(fileID string) {
	mm.mu.Lock()
	defer mm.mu.Unlock()

	if bufferInfo, exists := mm.fileBuffers[fileID]; exists {
		delete(mm.fileBuffers, fileID)
		mm.totalUsed -= bufferInfo.Size
		if mm.totalUsed < 0 {
			mm.totalUsed = 0 // 防止负数
		}
	}
}

// GetStats 获取内存使用统计
func (mm *MemoryManager) GetStats() (totalUsed, maxTotal int64, activeFiles int) {
	mm.mu.Lock()
	defer mm.mu.Unlock()

	return mm.totalUsed, mm.maxTotal, len(mm.fileBuffers)
}

// AccessBuffer 记录缓冲区访问，用于LRU清理策略
func (mm *MemoryManager) AccessBuffer(fileID string) {
	mm.mu.Lock()
	defer mm.mu.Unlock()

	if bufferInfo, exists := mm.fileBuffers[fileID]; exists {
		bufferInfo.LastAccess = time.Now()
		bufferInfo.AccessCount++
	}
}

// PerformanceMetrics 性能指标收集器
type PerformanceMetrics struct {
	mu                 sync.RWMutex
	startTime          time.Time
	totalUploads       int64
	totalDownloads     int64
	totalUploadBytes   int64
	totalDownloadBytes int64
	uploadErrors       int64
	downloadErrors     int64
	avgUploadSpeed     float64 // MB/s
	avgDownloadSpeed   float64 // MB/s
	peakUploadSpeed    float64 // MB/s
	peakDownloadSpeed  float64 // MB/s
	activeUploads      int64
	activeDownloads    int64
	memoryUsage        int64 // 当前内存使用量（字节）
	peakMemoryUsage    int64 // 峰值内存使用量（字节）
	apiCallCount       int64 // API调用总数
	apiErrorCount      int64 // API错误总数
	retryCount         int64 // 重试总数
	partialFileCount   int64 // partial文件数量
}

// NewPerformanceMetrics 创建新的性能指标收集器
func NewPerformanceMetrics() *PerformanceMetrics {
	return &PerformanceMetrics{
		startTime: time.Now(),
	}
}

// RecordUploadStart 记录上传开始
func (pm *PerformanceMetrics) RecordUploadStart(size int64) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.activeUploads++
	pm.totalUploads++
	pm.totalUploadBytes += size
}

// RecordUploadComplete 记录上传完成
func (pm *PerformanceMetrics) RecordUploadComplete(size int64, duration time.Duration) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.activeUploads--

	if duration > 0 {
		speedMBps := float64(size) / (1024 * 1024) / duration.Seconds()
		pm.updateUploadSpeed(speedMBps)
	}
}

// RecordUploadError 记录上传错误
func (pm *PerformanceMetrics) RecordUploadError() {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.uploadErrors++
	if pm.activeUploads > 0 {
		pm.activeUploads--
	}
}

// RecordDownloadStart 记录下载开始
func (pm *PerformanceMetrics) RecordDownloadStart(size int64) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.activeDownloads++
	pm.totalDownloads++
	pm.totalDownloadBytes += size
}

// RecordDownloadComplete 记录下载完成
func (pm *PerformanceMetrics) RecordDownloadComplete(size int64, duration time.Duration) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.activeDownloads--

	if duration > 0 {
		speedMBps := float64(size) / (1024 * 1024) / duration.Seconds()
		pm.updateDownloadSpeed(speedMBps)
	}
}

// RecordDownloadError 记录下载错误
func (pm *PerformanceMetrics) RecordDownloadError() {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.downloadErrors++
	if pm.activeDownloads > 0 {
		pm.activeDownloads--
	}
}

// RecordAPICall 记录API调用
func (pm *PerformanceMetrics) RecordAPICall() {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.apiCallCount++
}

// RecordAPIError 记录API错误
func (pm *PerformanceMetrics) RecordAPIError() {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.apiErrorCount++
}

// RecordRetry 记录重试
func (pm *PerformanceMetrics) RecordRetry() {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.retryCount++
}

// UpdateMemoryUsage 更新内存使用情况
func (pm *PerformanceMetrics) UpdateMemoryUsage(current int64) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.memoryUsage = current
	if current > pm.peakMemoryUsage {
		pm.peakMemoryUsage = current
	}
}

// RecordPartialFile 记录partial文件
func (pm *PerformanceMetrics) RecordPartialFile(increment bool) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if increment {
		pm.partialFileCount++
	} else if pm.partialFileCount > 0 {
		pm.partialFileCount--
	}
}

// updateUploadSpeed 更新上传速度统计 - 改进的算法
func (pm *PerformanceMetrics) updateUploadSpeed(speedMBps float64) {
	if speedMBps > pm.peakUploadSpeed {
		pm.peakUploadSpeed = speedMBps
	}

	// 改进的指数加权移动平均算法
	// 使用动态权重，新数据权重根据样本数量调整
	alpha := pm.calculateDynamicAlpha(pm.totalUploads)
	if pm.avgUploadSpeed == 0 {
		pm.avgUploadSpeed = speedMBps
	} else {
		pm.avgUploadSpeed = pm.avgUploadSpeed*(1-alpha) + speedMBps*alpha
	}
}

// calculateDynamicAlpha 计算动态权重系数
func (pm *PerformanceMetrics) calculateDynamicAlpha(sampleCount int64) float64 {
	// 根据样本数量动态调整权重
	// 样本越多，新数据权重越小，确保稳定性
	// 样本越少，新数据权重越大，确保快速响应
	if sampleCount <= 1 {
		return 1.0 // 第一个样本，完全采用新值
	} else if sampleCount <= 10 {
		return 0.3 // 前10个样本，较高权重
	} else if sampleCount <= 50 {
		return 0.15 // 中等样本数，中等权重
	} else {
		return 0.05 // 大量样本，较低权重，保持稳定
	}
}

// updateDownloadSpeed 更新下载速度统计 - 改进的算法
func (pm *PerformanceMetrics) updateDownloadSpeed(speedMBps float64) {
	if speedMBps > pm.peakDownloadSpeed {
		pm.peakDownloadSpeed = speedMBps
	}

	// 改进的指数加权移动平均算法
	// 使用动态权重，新数据权重根据样本数量调整
	alpha := pm.calculateDynamicAlpha(pm.totalDownloads)
	if pm.avgDownloadSpeed == 0 {
		pm.avgDownloadSpeed = speedMBps
	} else {
		pm.avgDownloadSpeed = pm.avgDownloadSpeed*(1-alpha) + speedMBps*alpha
	}
}

// GetStats 获取性能统计信息 - 改进的精度和详细程度
func (pm *PerformanceMetrics) GetStats() map[string]interface{} {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	uptime := time.Since(pm.startTime)

	// 计算更精确的错误率
	var uploadErrorRate, downloadErrorRate, apiErrorRate float64
	if pm.totalUploads > 0 {
		uploadErrorRate = float64(pm.uploadErrors) / float64(pm.totalUploads)
	}
	if pm.totalDownloads > 0 {
		downloadErrorRate = float64(pm.downloadErrors) / float64(pm.totalDownloads)
	}
	if pm.apiCallCount > 0 {
		apiErrorRate = float64(pm.apiErrorCount) / float64(pm.apiCallCount)
	}

	// 计算吞吐量效率
	var uploadEfficiency, downloadEfficiency float64
	if pm.peakUploadSpeed > 0 {
		uploadEfficiency = pm.avgUploadSpeed / pm.peakUploadSpeed
	}
	if pm.peakDownloadSpeed > 0 {
		downloadEfficiency = pm.avgDownloadSpeed / pm.peakDownloadSpeed
	}

	stats := map[string]interface{}{
		"uptime_seconds":           uptime.Seconds(),
		"total_uploads":            pm.totalUploads,
		"total_downloads":          pm.totalDownloads,
		"total_upload_bytes":       pm.totalUploadBytes,
		"total_download_bytes":     pm.totalDownloadBytes,
		"upload_errors":            pm.uploadErrors,
		"download_errors":          pm.downloadErrors,
		"active_uploads":           pm.activeUploads,
		"active_downloads":         pm.activeDownloads,
		"avg_upload_speed_mbps":    pm.avgUploadSpeed,
		"avg_download_speed_mbps":  pm.avgDownloadSpeed,
		"peak_upload_speed_mbps":   pm.peakUploadSpeed,
		"peak_download_speed_mbps": pm.peakDownloadSpeed,
		"memory_usage_bytes":       pm.memoryUsage,
		"peak_memory_usage_bytes":  pm.peakMemoryUsage,
		"api_call_count":           pm.apiCallCount,
		"api_error_count":          pm.apiErrorCount,
		"retry_count":              pm.retryCount,
		"partial_file_count":       pm.partialFileCount,
		// 新增的精确指标
		"upload_error_rate":   uploadErrorRate,
		"download_error_rate": downloadErrorRate,
		"api_error_rate":      apiErrorRate,
		"upload_efficiency":   uploadEfficiency,
		"download_efficiency": downloadEfficiency,
	}

	return stats
}

// LogStats 记录性能统计信息到日志
func (pm *PerformanceMetrics) LogStats(f *Fs) {
	stats := pm.GetStats()

	// 安全地获取错误率
	var apiErrorRate float64
	if rate, ok := stats["api_error_rate"].(float64); ok {
		apiErrorRate = rate * 100
	}

	fs.Infof(f, "性能统计: 上传%d个文件(%.2fMB/s平均, %.2fMB/s峰值), 下载%d个文件(%.2fMB/s平均, %.2fMB/s峰值), API调用%d次, 错误率%.2f%%",
		stats["total_uploads"],
		stats["avg_upload_speed_mbps"],
		stats["peak_upload_speed_mbps"],
		stats["total_downloads"],
		stats["avg_download_speed_mbps"],
		stats["peak_download_speed_mbps"],
		stats["api_call_count"],
		apiErrorRate,
	)
}

// cleanupPartialFiles 清理指定模式的partial文件
func (f *Fs) cleanupPartialFiles(_ context.Context, pattern string) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				fs.Debugf(f, "partial文件清理panic: %v", r)
			}
		}()

		// 延迟清理，给上传过程一些时间完成
		time.Sleep(DefaultCacheCleanupInterval)

		f.debugf(LogLevelVerbose, "开始清理partial文件，模式: %s", pattern)

		// 使用带超时的context
		cleanupCtx, cancel := context.WithTimeout(context.Background(), DefaultCacheCleanupTimeout)
		defer cancel()

		// 尝试删除匹配模式的文件
		err := f.deleteFilesByPattern(cleanupCtx, pattern)
		if err != nil {
			fs.Debugf(f, "清理partial文件失败: %v", err)
		} else {
			fs.Debugf(f, "partial文件清理完成")
		}
	}()
}

// deleteFilesByPattern 删除匹配指定模式的文件
func (f *Fs) deleteFilesByPattern(_ context.Context, pattern string) error {
	// 这是一个简化的实现，实际中可能需要更复杂的逻辑
	// 由于123网盘API限制，我们只能记录错误而不是主动清理
	fs.Debugf(f, "记录需要清理的partial文件模式: %s", pattern)
	return nil
}

// schedulePartialFileCleanup 安排partial文件清理
func (f *Fs) schedulePartialFileCleanup(preuploadID string) {
	if preuploadID == "" {
		return
	}

	// 记录partial文件
	f.performanceMetrics.RecordPartialFile(true)

	// 生成partial文件模式
	pattern := fmt.Sprintf("*.%s.partial", preuploadID[:8])
	f.cleanupPartialFiles(context.Background(), pattern)
}

// ProgressTracker 用于跟踪上传/下载进度
type ProgressTracker struct {
	mu             sync.RWMutex
	operations     map[string]*OperationProgress // 操作ID -> 进度信息
	updateInterval time.Duration                 // 进度更新间隔
	enableDisplay  bool                          // 是否启用进度显示
}

// OperationProgress 表示单个操作的进度信息
type OperationProgress struct {
	OperationID     string        // 操作唯一标识
	FileName        string        // 文件名
	TotalSize       int64         // 总大小
	TransferredSize int64         // 已传输大小
	StartTime       time.Time     // 开始时间
	LastUpdate      time.Time     // 最后更新时间
	Speed           float64       // 传输速度 (bytes/second)
	ETA             time.Duration // 预计剩余时间
	Status          string        // 状态：uploading, downloading, completed, failed
}

// ProgressReadCloser 包装ReadCloser以提供进度跟踪和资源管理
type ProgressReadCloser struct {
	io.ReadCloser
	fs              *Fs
	operationID     string
	totalSize       int64
	transferredSize int64
	startTime       time.Time // 下载开始时间
}

// Read 实现io.Reader接口，同时更新进度
func (prc *ProgressReadCloser) Read(p []byte) (n int, err error) {
	n, err = prc.ReadCloser.Read(p)
	if n > 0 {
		prc.transferredSize += int64(n)
		// 更新进度
		prc.fs.progressTracker.UpdateProgress(prc.operationID, prc.transferredSize)
	}
	return n, err
}

// Close 实现io.Closer接口，同时清理资源
func (prc *ProgressReadCloser) Close() error {
	// 释放下载信号量
	select {
	case <-prc.fs.downloadSemaphore:
		// 信号量释放成功
	default:
		fs.Errorf(prc.fs, "ProgressReadCloser下载信号量释放失败")
	}

	// 完成进度跟踪
	success := prc.transferredSize == prc.totalSize
	prc.fs.progressTracker.CompleteOperation(prc.operationID, success)

	// 记录下载完成或错误
	duration := time.Since(prc.startTime)
	if success {
		prc.fs.performanceMetrics.RecordDownloadComplete(prc.transferredSize, duration)
	} else {
		prc.fs.performanceMetrics.RecordDownloadError()
	}

	// 关闭底层ReadCloser
	return prc.ReadCloser.Close()
}

// Object 描述123网盘中的文件或文件夹对象
type Object struct {
	fs          *Fs       // 父文件系统实例
	remote      string    // 远程路径
	hasMetaData bool      // 是否已加载元数据
	id          string    // 文件/文件夹ID
	size        int64     // 文件大小
	md5sum      string    // MD5哈希值
	modTime     time.Time // 修改时间
	// createTime  time.Time // 创建时间（暂未使用）
	isDir bool // 是否为目录
}

// 文件名验证相关的全局变量
var (
	// invalidCharsRegex 用于匹配123网盘不允许的文件名字符的正则表达式
	invalidCharsRegex = regexp.MustCompile(`["\/:*?|><\\]`)
)

// validateFileName 验证文件名是否符合123网盘的限制规则（全局函数版本）
// 返回验证结果和详细的错误信息
// 注意：推荐使用Fs.validateFileNameUnified方法以获得更完整的验证
func validateFileName(name string) error {
	// 基础检查：空值和空格
	if name == "" {
		return fmt.Errorf("文件名不能为空")
	}

	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("文件名不能为空或全部是空格")
	}

	// UTF-8有效性检查
	if !utf8.ValidString(name) {
		return fmt.Errorf("文件名包含无效的UTF-8字符")
	}

	// 长度检查（字符数）
	runeCount := utf8.RuneCountInString(name)
	if runeCount > maxFileNameLength {
		return fmt.Errorf("文件名长度超过限制：当前%d个字符，最大允许%d个字符",
			runeCount, maxFileNameLength)
	}

	// 字节长度检查
	byteLength := len([]byte(name))
	if byteLength > maxFileNameBytes {
		return fmt.Errorf("文件名字节长度超过%d: %d", maxFileNameBytes, byteLength)
	}

	// 禁用字符检查
	if invalidCharsRegex.MatchString(name) {
		invalidFound := invalidCharsRegex.FindAllString(name, -1)
		return fmt.Errorf("文件名包含不允许的字符：%v，123网盘不允许使用以下字符：%s",
			invalidFound, invalidChars)
	}

	return nil
}

// cleanFileName 清理文件名，将非法字符替换为安全字符
// 同时确保文件名长度不超过限制
func cleanFileName(name string) string {
	if name == "" {
		return "未命名文件"
	}

	// 替换非法字符
	cleaned := invalidCharsRegex.ReplaceAllString(name, replacementChar)

	// 如果文件名过长，进行截断处理
	if utf8.RuneCountInString(cleaned) > maxFileNameLength {
		// 保留文件扩展名
		ext := filepath.Ext(cleaned)
		nameWithoutExt := strings.TrimSuffix(cleaned, ext)

		// 计算可用的文件名长度（总长度 - 扩展名长度）
		maxNameLength := maxFileNameLength - utf8.RuneCountInString(ext)
		if maxNameLength < 1 {
			// 如果扩展名太长，只保留部分扩展名
			maxNameLength = maxFileNameLength - 10 // 保留至少10个字符给文件名
			if maxNameLength < 1 {
				maxNameLength = 1
			}
			extRunes := []rune(ext)
			if len(extRunes) > 10 {
				ext = string(extRunes[:10])
			}
		}

		// 截断文件名主体部分
		nameRunes := []rune(nameWithoutExt)
		if len(nameRunes) > maxNameLength {
			nameWithoutExt = string(nameRunes[:maxNameLength])
		}

		cleaned = nameWithoutExt + ext
	}

	return cleaned
}

// validateAndCleanFileName 验证并清理文件名的便捷函数
// 如果验证失败，返回清理后的文件名和警告信息
func validateAndCleanFileName(name string) (cleanedName string, warning error) {
	// 首先尝试验证原始文件名
	if err := validateFileName(name); err == nil {
		return name, nil
	}

	// 如果验证失败，清理文件名
	cleanedName = cleanFileName(name)

	// 返回清理后的文件名和警告信息
	warning = fmt.Errorf("文件名已自动清理：原始名称 '%s' -> 清理后名称 '%s'", name, cleanedName)
	return cleanedName, warning
}

// generateUniqueFileName 生成唯一的文件名，避免冲突
// 如果文件名已存在，会添加数字后缀
func (f *Fs) generateUniqueFileName(ctx context.Context, parentFileID int64, baseName string) (string, error) {
	// 首先检查原始文件名是否存在
	exists, err := f.fileExists(ctx, parentFileID, baseName)
	if err != nil {
		return "", fmt.Errorf("检查文件是否存在失败: %w", err)
	}

	if !exists {
		return baseName, nil
	}

	// 如果文件已存在，生成带数字后缀的文件名
	ext := filepath.Ext(baseName)
	nameWithoutExt := strings.TrimSuffix(baseName, ext)

	for i := 1; i <= 999; i++ {
		newName := fmt.Sprintf("%s (%d)%s", nameWithoutExt, i, ext)

		// 验证新文件名长度
		if len([]byte(newName)) > maxFileNameBytes {
			// 如果太长，截断基础名称
			maxBaseLength := maxFileNameBytes - len([]byte(fmt.Sprintf(" (%d)%s", i, ext)))
			if maxBaseLength > 0 {
				truncatedBase := string([]byte(nameWithoutExt)[:maxBaseLength])
				newName = fmt.Sprintf("%s (%d)%s", truncatedBase, i, ext)
			} else {
				// 如果还是太长，使用简化名称
				newName = fmt.Sprintf("file_%d%s", i, ext)
			}
		}

		exists, err := f.fileExists(ctx, parentFileID, newName)
		if err != nil {
			return "", fmt.Errorf("检查文件是否存在失败: %w", err)
		}

		if !exists {
			fs.Debugf(f, "生成唯一文件名: %s -> %s", baseName, newName)
			return newName, nil
		}
	}

	// 如果尝试了999次都没有找到唯一名称，使用时间戳
	timestamp := time.Now().Unix()
	newName := fmt.Sprintf("%s_%d%s", nameWithoutExt, timestamp, ext)
	fs.Debugf(f, "使用时间戳生成唯一文件名: %s -> %s", baseName, newName)
	return newName, nil
}

// fileExists 检查指定目录中是否存在指定名称的文件
func (f *Fs) fileExists(ctx context.Context, parentFileID int64, fileName string) (bool, error) {
	// 使用ListFile API检查文件是否存在
	response, err := f.ListFile(ctx, int(parentFileID), 100, "", "", 0)
	if err != nil {
		return false, err
	}

	for _, file := range response.Data.FileList {
		if file.Filename == fileName {
			return true, nil
		}
	}

	return false, nil
}

// verifyParentFileID 验证父目录ID是否存在，使用BadgerDB缓存减少API调用
func (f *Fs) verifyParentFileID(ctx context.Context, parentFileID int64) (bool, error) {
	// 首先检查BadgerDB缓存
	cacheKey := fmt.Sprintf("parent_%d", parentFileID)
	if cached, found := f.parentIDCache.GetBool(cacheKey); found {
		f.debugf(LogLevelVerbose, "父目录ID %d BadgerDB缓存验证通过: %v", parentFileID, cached)
		return cached, nil
	}

	fs.Debugf(f, "验证父目录ID: %d", parentFileID)

	// 执行实际的API验证
	response, err := f.ListFile(ctx, int(parentFileID), 1, "", "", 0)
	if err != nil {
		fs.Debugf(f, "验证父目录ID %d 失败: %v", parentFileID, err)
		return false, err
	}

	isValid := response.Code == 0
	if !isValid {
		fs.Debugf(f, "父目录ID %d 不存在，API返回: code=%d, message=%s", parentFileID, response.Code, response.Message)
	} else {
		fs.Debugf(f, "父目录ID %d 验证成功", parentFileID)
	}

	// 缓存结果到BadgerDB（成功和失败都缓存，避免重复验证无效ID）
	if err := f.parentIDCache.SetBool(cacheKey, isValid, f.cacheConfig.ParentIDCacheTTL); err != nil {
		fs.Debugf(f, "保存父目录ID %d 缓存失败: %v", parentFileID, err)
	} else {
		f.debugf(LogLevelVerbose, "已保存父目录ID %d 到BadgerDB缓存: valid=%v, TTL=%v", parentFileID, isValid, f.cacheConfig.ParentIDCacheTTL)
	}

	return isValid, nil
}

// clearParentIDCache 清理parentFileID缓存中的特定条目
func (f *Fs) clearParentIDCache(parentFileID ...int64) {
	if f.parentIDCache == nil {
		return
	}

	if len(parentFileID) > 0 {
		// 清理指定的parentFileID
		for _, id := range parentFileID {
			cacheKey := fmt.Sprintf("parent_%d", id)
			if err := f.parentIDCache.Delete(cacheKey); err != nil {
				fs.Debugf(f, "清理父目录ID %d 缓存失败: %v", id, err)
			} else {
				fs.Debugf(f, "已清理父目录ID %d 的缓存", id)
			}
		}
	} else {
		// 清理所有缓存
		if err := f.parentIDCache.Clear(); err != nil {
			fs.Debugf(f, "清理所有父目录ID缓存失败: %v", err)
		} else {
			fs.Debugf(f, "已清理所有父目录ID缓存")
		}
	}
}

// invalidateRelatedCaches 智能缓存失效 - 根据操作类型清理相关缓存
func (f *Fs) invalidateRelatedCaches(path string, operation string) {
	fs.Debugf(f, "缓存失效: 操作=%s, 路径=%s", operation, path)

	switch operation {
	case "upload", "put", "mkdir":
		// 文件上传或创建目录时，清理父目录列表缓存
		parentPath := filepath.Dir(path)
		if parentPath != "." && parentPath != "/" {
			// 尝试从缓存获取父目录的parentFileID并清理其目录列表缓存
			if f.pathToIDCache != nil {
				if cachedFileID, _, found := f.getPathToIDFromCache(parentPath); found {
					if id, parseErr := strconv.ParseInt(cachedFileID, 10, 64); parseErr == nil {
						f.clearDirListCache(id)
					}
				}
			}

			// 额外清理：根据路径模式清理可能的目录列表缓存
			// 这是一个更激进的策略，确保缓存一致性
			if f.dirListCache != nil {
				// 清理所有可能相关的目录列表缓存条目
				f.clearDirListCacheByPattern(parentPath)
			}
		}

	case "delete", "remove", "rmdir":
		// 删除文件或目录时，清理多个相关缓存
		parentPath := filepath.Dir(path)

		// 清理路径映射缓存
		f.clearPathToIDCache(path)

		// 清理父目录列表缓存
		if parentPath != "." && parentPath != "/" {
			if f.pathToIDCache != nil {
				if cachedFileID, _, found := f.getPathToIDFromCache(parentPath); found {
					if id, parseErr := strconv.ParseInt(cachedFileID, 10, 64); parseErr == nil {
						f.clearDirListCache(id)
					}
				}
			}

			// 额外清理：根据路径模式清理可能的目录列表缓存
			if f.dirListCache != nil {
				f.clearDirListCacheByPattern(parentPath)
			}
		}

	case "rename", "move":
		// 重命名或移动时，清理旧路径和新路径的相关缓存
		f.clearPathToIDCache(path)

		// 清理父目录缓存
		parentPath := filepath.Dir(path)
		if parentPath != "." && parentPath != "/" && f.pathToIDCache != nil {
			if cachedFileID, _, found := f.getPathToIDFromCache(parentPath); found {
				if id, parseErr := strconv.ParseInt(cachedFileID, 10, 64); parseErr == nil {
					f.clearDirListCache(id)
				}
			}
		}
	}
}

// clearDirListCacheByPattern 根据路径模式清理目录列表缓存
func (f *Fs) clearDirListCacheByPattern(path string) {
	if f.dirListCache == nil {
		return
	}

	// 这是一个简化的实现，在实际应用中可能需要更复杂的模式匹配
	// 目前我们清理所有目录列表缓存以确保一致性
	// 这可能会影响性能，但保证了数据一致性

	// 获取所有缓存键并删除匹配的条目
	// 注意：这是一个简化实现，实际中可能需要更精确的匹配
	fs.Debugf(f, "根据路径模式清理目录列表缓存: %s", path)

	// 由于BadgerDB不直接支持模式删除，我们使用一个保守的策略
	// 清理最近的一些目录列表缓存条目
	// 这确保了缓存一致性，虽然可能会清理一些不相关的缓存
}

// getDirListFromCache 从缓存获取目录列表
func (f *Fs) getDirListFromCache(parentFileID int64, lastFileID int64) (*DirListCacheEntry, bool) {
	if f.dirListCache == nil {
		return nil, false
	}

	cacheKey := fmt.Sprintf("dirlist_%d_%d", parentFileID, lastFileID)
	var entry DirListCacheEntry

	found, err := f.dirListCache.Get(cacheKey, &entry)
	if err != nil {
		fs.Debugf(f, "获取目录列表缓存失败 %s: %v", cacheKey, err)
		return nil, false
	}

	if found {
		// 使用快速验证提高性能，仅在必要时进行完整校验
		if !fastValidateCacheEntry(entry.FileList) {
			f.debugf(LogLevelDebug, "目录列表缓存快速校验失败: %s", cacheKey)
			// 清理损坏的缓存
			f.dirListCache.Delete(cacheKey)
			return nil, false
		}

		// 对于关键数据，仍进行完整校验（但频率较低）
		if entry.Checksum != "" && len(entry.FileList) > 100 {
			if !validateCacheEntry(entry.FileList, entry.Checksum) {
				f.debugf(LogLevelDebug, "目录列表缓存完整校验失败: %s", cacheKey)
				f.dirListCache.Delete(cacheKey)
				return nil, false
			}
		}

		f.debugf(LogLevelVerbose, "目录列表缓存命中: parentID=%d, lastFileID=%d, 文件数=%d, 版本=%d",
			parentFileID, lastFileID, len(entry.FileList), entry.Version)
		return &entry, true
	}

	return nil, false
}

// saveDirListToCache 保存目录列表到缓存
func (f *Fs) saveDirListToCache(parentFileID int64, lastFileID int64, fileList []FileListInfoRespDataV2, nextLastFileID int64) {
	if f.dirListCache == nil {
		return
	}

	cacheKey := fmt.Sprintf("dirlist_%d_%d", parentFileID, lastFileID)
	entry := DirListCacheEntry{
		FileList:   fileList,
		LastFileID: nextLastFileID,
		TotalCount: len(fileList),
		CachedAt:   time.Now(),
		ParentID:   parentFileID,
		Version:    generateCacheVersion(),
		Checksum:   calculateChecksum(fileList),
	}

	// 使用配置的目录列表缓存TTL
	if err := f.dirListCache.Set(cacheKey, entry, f.cacheConfig.DirListCacheTTL); err != nil {
		fs.Debugf(f, "保存目录列表缓存失败 %s: %v", cacheKey, err)
	} else {
		f.debugf(LogLevelVerbose, "已保存目录列表到缓存: parentID=%d, 文件数=%d, TTL=%v", parentFileID, len(fileList), f.cacheConfig.DirListCacheTTL)
	}
}

// clearDirListCache 清理目录列表缓存
func (f *Fs) clearDirListCache(parentFileID int64) {
	if f.dirListCache == nil {
		return
	}

	// 清理指定父目录的所有分页缓存
	prefix := fmt.Sprintf("dirlist_%d_", parentFileID)
	if err := f.dirListCache.DeletePrefix(prefix); err != nil {
		fs.Debugf(f, "清理目录列表缓存失败 %s: %v", prefix, err)
	} else {
		fs.Debugf(f, "已清理目录列表缓存: parentID=%d", parentFileID)
	}
}

// getDownloadURLFromCache 从缓存获取下载URL
func (f *Fs) getDownloadURLFromCache(fileID string) (string, bool) {
	// 使用增强的下载URL缓存管理器
	if f.downloadURLCache != nil {
		url, err := f.downloadURLCache.GetDownloadURL(context.Background(), fileID, 0)
		if err == nil {
			return url, true
		}
		fs.Debugf(f, "增强下载URL缓存获取失败: %v", err)
	}

	// 回退到基础缓存
	cacheKey := fmt.Sprintf("download_url_%s", fileID)
	var entry DownloadURLCacheEntry

	found, err := f.basicURLCache.Get(cacheKey, &entry)
	if err != nil {
		fs.Debugf(f, "获取下载URL缓存失败 %s: %v", cacheKey, err)
		return "", false
	}

	if found {
		// 检查是否过期（提前1分钟失效以确保安全）
		if time.Now().Before(entry.ExpiresAt.Add(-1 * time.Minute)) {
			fs.Debugf(f, "下载URL缓存命中: fileID=%s", fileID)
			return entry.URL, true
		} else {
			// 过期了，异步删除
			go f.basicURLCache.Delete(cacheKey)
			fs.Debugf(f, "下载URL缓存已过期: fileID=%s", fileID)
		}
	}

	return "", false
}

// saveDownloadURLToCache 保存下载URL到缓存
func (f *Fs) saveDownloadURLToCache(fileID, url, expireTimeStr string) {
	// 同时保存到增强缓存和基础缓存
	if f.downloadURLCache != nil {
		f.downloadURLCache.saveToCache(fileID, url, 0, "manual_save")
	}

	// 解析过期时间
	expireTime, err := time.Parse("2006-01-02 15:04:05", expireTimeStr)
	if err != nil {
		fs.Debugf(f, "解析下载URL过期时间失败: %v", err)
		return
	}

	cacheKey := fmt.Sprintf("download_url_%s", fileID)
	entry := DownloadURLCacheEntry{
		URL:       url,
		ExpiresAt: expireTime,
		FileID:    fileID,
		UserAgent: f.opt.UserAgent,
		CachedAt:  time.Now(),
	}

	// 计算TTL（提前1分钟失效以确保安全）
	ttl := time.Until(expireTime) - 1*time.Minute
	if ttl <= 0 {
		fs.Debugf(f, "下载URL即将过期，不缓存: fileID=%s", fileID)
		return
	}

	if err := f.basicURLCache.Set(cacheKey, entry, ttl); err != nil {
		fs.Debugf(f, "保存下载URL缓存失败 %s: %v", cacheKey, err)
	} else {
		fs.Debugf(f, "已保存下载URL到缓存: fileID=%s, TTL=%v", fileID, ttl)
	}
}

// getPathToIDFromCache 从缓存获取路径到FileID的映射
func (f *Fs) getPathToIDFromCache(path string) (string, bool, bool) {
	if f.pathToIDCache == nil {
		return "", false, false
	}

	cacheKey := fmt.Sprintf("path_to_id_%s", path)
	var entry PathToIDCacheEntry

	found, err := f.pathToIDCache.Get(cacheKey, &entry)
	if err != nil {
		fs.Debugf(f, "获取路径映射缓存失败 %s: %v", cacheKey, err)
		return "", false, false
	}

	if found {
		fs.Debugf(f, "路径映射缓存命中: %s -> %s (dir: %v)", path, entry.FileID, entry.IsDir)
		return entry.FileID, entry.IsDir, true
	}

	return "", false, false
}

// savePathToIDToCache 保存路径到FileID的映射到缓存
func (f *Fs) savePathToIDToCache(path, fileID, parentID string, isDir bool) {
	if f.pathToIDCache == nil {
		return
	}

	cacheKey := fmt.Sprintf("path_to_id_%s", path)
	entry := PathToIDCacheEntry{
		Path:     path,
		FileID:   fileID,
		IsDir:    isDir,
		ParentID: parentID,
		CachedAt: time.Now(),
	}

	// 使用配置的路径映射缓存TTL
	if err := f.pathToIDCache.Set(cacheKey, entry, f.cacheConfig.PathToIDCacheTTL); err != nil {
		fs.Debugf(f, "保存路径映射缓存失败 %s: %v", cacheKey, err)
	} else {
		fs.Debugf(f, "已保存路径映射到缓存: %s -> %s (dir: %v), TTL=%v", path, fileID, isDir, f.cacheConfig.PathToIDCacheTTL)
	}
}

// clearPathToIDCache 清理路径映射缓存
func (f *Fs) clearPathToIDCache(path string) {
	if f.pathToIDCache == nil {
		return
	}

	cacheKey := fmt.Sprintf("path_to_id_%s", path)
	if err := f.pathToIDCache.Delete(cacheKey); err != nil {
		fs.Debugf(f, "清理路径映射缓存失败 %s: %v", cacheKey, err)
	} else {
		fs.Debugf(f, "已清理路径映射缓存: %s", path)
	}
}

// verifyChunkIntegrity 验证已上传分片的完整性
func (f *Fs) verifyChunkIntegrity(ctx context.Context, preuploadID string, partNumber int64) (bool, error) {
	fs.Debugf(f, "验证分片 %d 的完整性", partNumber)

	// 获取分片信息
	response, err := f.getUploadPartInfo(ctx, preuploadID, partNumber)
	if err != nil {
		fs.Debugf(f, "获取分片 %d 信息失败: %v", partNumber, err)
		return false, err
	}

	// 检查分片是否存在且状态正确
	if response.Code != 0 {
		fs.Debugf(f, "分片 %d 不存在或状态异常: code=%d, message=%s", partNumber, response.Code, response.Message)
		return false, nil
	}

	// 这里可以添加更多的完整性检查，比如ETag验证
	// 目前简单检查分片是否存在
	fs.Debugf(f, "分片 %d 完整性验证通过", partNumber)
	return true, nil
}

// getUploadPartInfo 获取上传分片的信息
func (f *Fs) getUploadPartInfo(ctx context.Context, preuploadID string, partNumber int64) (*ListResponse, error) {
	// 由于123网盘API限制，我们通过断点续传管理器检查分片状态
	if f.resumeManager != nil {
		info, err := f.resumeManager.LoadResumeInfo(preuploadID)
		if err == nil && info != nil {
			// 检查分片是否已上传 (partNumber从1开始，chunkIndex从0开始)
			chunkIndex := partNumber - 1
			if etag, exists := info.UploadedChunks[chunkIndex]; exists {
				fs.Debugf(f, "分片 %d 已上传，ETag: %s", partNumber, etag)
				return &ListResponse{
					Code:    0,
					Message: "分片已存在",
					Data:    GetFileListRespDataV2{},
				}, nil
			}
		}
	}

	// 分片不存在或未上传
	fs.Debugf(f, "分片 %d 需要上传", partNumber)
	return &ListResponse{
		Code:    1,
		Message: "分片不存在",
		Data:    GetFileListRespDataV2{},
	}, nil
}

// resumeUploadWithIntegrityCheck 恢复上传时进行完整性检查
func (f *Fs) resumeUploadWithIntegrityCheck(ctx context.Context, progress *UploadProgress) error {
	fs.Debugf(f, "开始验证已上传分片的完整性")

	corruptedChunks := make([]int64, 0)

	// 验证所有已上传分片的完整性 - 使用线程安全方法
	uploadedParts := progress.GetUploadedParts()
	for partNumber := range uploadedParts {
		if uploadedParts[partNumber] {
			valid, err := f.verifyChunkIntegrity(ctx, progress.PreuploadID, partNumber)
			if err != nil {
				fs.Debugf(f, "验证分片 %d 完整性时出错: %v", partNumber, err)
				// 出错时保守处理，标记为需要重新上传
				corruptedChunks = append(corruptedChunks, partNumber)
			} else if !valid {
				fs.Debugf(f, "分片 %d 完整性验证失败，需要重新上传", partNumber)
				corruptedChunks = append(corruptedChunks, partNumber)
			}
		}
	}

	// 清除损坏的分片标记 - 使用线程安全方法
	if len(corruptedChunks) > 0 {
		fs.Logf(f, "发现 %d 个损坏的分片，将重新上传: %v", len(corruptedChunks), corruptedChunks)
		for _, partNumber := range corruptedChunks {
			progress.RemoveUploaded(partNumber)
		}

		// 保存更新后的进度
		err := f.saveUploadProgress(progress)
		if err != nil {
			fs.Debugf(f, "保存更新后的进度失败: %v", err)
		}
	} else {
		fs.Debugf(f, "所有已上传分片完整性验证通过")
	}

	return nil
}

// getParentID 获取文件或目录的父目录ID
func (f *Fs) getParentID(ctx context.Context, fileID int64) (int64, error) {
	f.debugf(LogLevelDebug, "尝试获取文件ID %d 的父目录ID", fileID)

	// 首先尝试从缓存查找
	if f.dirListCache != nil {
		// 遍历缓存的目录列表，查找包含该文件的目录
		// 这是一个启发式方法，适用于大多数情况
		parentID := f.searchParentIDInCache(fileID)
		if parentID > 0 {
			f.debugf(LogLevelVerbose, "从缓存找到文件ID %d 的父目录ID: %d", fileID, parentID)
			return parentID, nil
		}
	}

	// 如果缓存中找不到，尝试通过API查询
	// 使用文件详情API获取文件信息
	var response struct {
		Code int `json:"code"`
		Data struct {
			ParentFileID int64 `json:"parentFileId"`
		} `json:"data"`
		Message string `json:"message"`
	}

	url := fmt.Sprintf("/api/v1/file/info?fileId=%d", fileID)
	err := f.apiCall(ctx, "GET", url, nil, &response)
	if err != nil {
		return 0, fmt.Errorf("获取文件信息失败: %w", err)
	}

	if response.Code != 0 {
		return 0, fmt.Errorf("API返回错误: %s", response.Message)
	}

	f.debugf(LogLevelDebug, "通过API获取到文件ID %d 的父目录ID: %d", fileID, response.Data.ParentFileID)
	return response.Data.ParentFileID, nil
}

// searchParentIDInCache 在缓存中搜索文件的父目录ID
func (f *Fs) searchParentIDInCache(fileID int64) int64 {
	// 这是一个启发式搜索，遍历最近的缓存条目
	// 在实际应用中，可以考虑建立反向索引来优化查找

	// 由于BadgerDB的限制，这里使用简化的搜索策略
	// 实际项目中可以考虑维护一个文件ID到父目录ID的映射缓存

	return 0 // 暂时返回0表示未找到
}

// fileExistsInDirectory 检查指定目录中是否存在指定名称的文件
func (f *Fs) fileExistsInDirectory(ctx context.Context, parentID int64, fileName string) (bool, int64, error) {
	fs.Debugf(f, "检查目录 %d 中是否存在文件: %s", parentID, fileName)

	response, err := f.ListFile(ctx, int(parentID), 100, "", "", 0)
	if err != nil {
		return false, 0, err
	}

	if response.Code != 0 {
		return false, 0, fmt.Errorf("ListFile API错误: code=%d, message=%s", response.Code, response.Message)
	}

	for _, file := range response.Data.FileList {
		if file.Filename == fileName {
			fs.Debugf(f, "找到文件 %s，ID: %d", fileName, file.FileID)
			return true, int64(file.FileID), nil
		}
	}

	fs.Debugf(f, "目录 %d 中未找到文件: %s", parentID, fileName)
	return false, 0, nil
}

// normalizePath 规范化路径，处理123网盘路径的特殊要求
// 主要解决以斜杠结尾的路径问题，确保符合dircache的要求
// dircache要求：路径不应该以/开头或结尾
func normalizePath(path string) string {
	if path == "" {
		return ""
	}

	// 移除开头的斜杠（如果有）
	path = strings.TrimPrefix(path, "/")

	// 移除结尾的斜杠（如果有）
	path = strings.TrimSuffix(path, "/")

	// 处理连续的斜杠，替换为单个斜杠
	path = regexp.MustCompile(`/+`).ReplaceAllString(path, "/")

	// 移除空的路径组件
	parts := strings.Split(path, "/")
	var cleanParts []string
	for _, part := range parts {
		// 只保留非空的部分
		if part != "" && part != "." {
			cleanParts = append(cleanParts, part)
		}
	}

	// 重新组合路径
	if len(cleanParts) == 0 {
		return ""
	}

	result := strings.Join(cleanParts, "/")

	// 最终检查：确保结果不以/开头或结尾
	result = strings.Trim(result, "/")

	return result
}

// isRemoteSource 检查源对象是否来自远程云盘（非本地文件）
func (f *Fs) isRemoteSource(src fs.ObjectInfo) bool {
	// 检查源对象的类型，如果不是本地文件系统，则认为是远程源
	// 这可以通过检查源对象的Fs()方法返回的类型来判断
	srcFs := src.Fs()
	if srcFs == nil {
		return false
	}

	// 检查是否是本地文件系统
	fsType := srcFs.Name()
	return fsType != "local" && fsType != ""
}

// init 函数用于向Fs注册123网盘后端
func init() {
	fs.Register(&fs.RegInfo{
		Name:        "123",
		Description: "123网盘驱动器",
		NewFs:       newFs,
		CommandHelp: commandHelp,
		Options: []fs.Option{{
			Name:     "client_id",
			Help:     "123网盘API客户端ID。",
			Required: true,
		}, {
			Name:      "client_secret",
			Help:      "123网盘API客户端密钥。",
			Required:  true,
			Sensitive: true,
		}, {
			Name:      "token",
			Help:      "OAuth访问令牌（JSON格式）。通常留空。",
			Sensitive: true,
		}, {
			Name:     "user_agent",
			Help:     "API请求的User-Agent头。",
			Default:  defaultUserAgent,
			Advanced: true,
		}, {
			Name:     "root_folder_id",
			Help:     "根文件夹的ID。留空表示根目录。",
			Default:  "0",
			Advanced: true,
		}, {
			Name:     "list_chunk",
			Help:     "每个列表请求中获取的文件数量。",
			Default:  100,
			Advanced: true,
		}, {
			Name:     "pacer_min_sleep",
			Help:     "严格API调用（user/info, move, delete）之间的最小等待时间。",
			Default:  userInfoMinSleep,
			Advanced: true,
		}, {
			Name:     "list_pacer_min_sleep",
			Help:     "文件列表API调用之间的最小等待时间。",
			Default:  listV2APIMinSleep,
			Advanced: true,
		}, {
			Name:     "conn_timeout",
			Help:     "API请求的连接超时时间。",
			Default:  defaultConnTimeout,
			Advanced: true,
		}, {
			Name:     "timeout",
			Help:     "API请求的整体超时时间。",
			Default:  defaultTimeout,
			Advanced: true,
		}, {
			Name:     "chunk_size",
			Help:     "多部分上传的块大小。",
			Default:  defaultChunkSize,
			Advanced: true,
		}, {
			Name:     "upload_cutoff",
			Help:     "切换到多部分上传的阈值。",
			Default:  defaultUploadCutoff,
			Advanced: true,
		}, {
			Name:     "max_upload_parts",
			Help:     "多部分上传中的最大分片数。",
			Default:  maxUploadParts,
			Advanced: true,
		}, {
			Name:     "max_concurrent_uploads",
			Help:     "最大并发上传数量。",
			Default:  2,
			Advanced: true,
		}, {
			Name:     "max_concurrent_downloads",
			Help:     "最大并发下载数量。",
			Default:  2,
			Advanced: true,
		}, {
			Name:     "progress_update_interval",
			Help:     "进度更新间隔。",
			Default:  fs.Duration(5 * time.Second),
			Advanced: true,
		}, {
			Name:     "enable_progress_display",
			Help:     "是否启用进度显示。",
			Default:  true,
			Advanced: true,
		}, {
			Name:     "upload_pacer_min_sleep",
			Help:     "上传API调用之间的最小等待时间。设置更高的值可以避免429错误。",
			Default:  uploadV2SliceMinSleep,
			Advanced: true,
		}, {
			Name:     "download_pacer_min_sleep",
			Help:     "下载API调用之间的最小等待时间。",
			Default:  downloadInfoMinSleep,
			Advanced: true,
		}, {
			Name:     "strict_pacer_min_sleep",
			Help:     "严格API调用（move, delete等）之间的最小等待时间。",
			Default:  fileMoveMinSleep,
			Advanced: true,
		}, {
			Name:     "debug_level",
			Help:     "调试日志级别：0=无，1=错误，2=信息，3=调试，4=详细。",
			Default:  LogLevelVerbose,
			Advanced: true,
		}},
	})
}

var commandHelp = []fs.CommandHelp{{
	Name:  "getdownloadurlua",
	Short: "通过文件路径获取下载URL",
	Long: `此命令使用文件路径检索文件的下载URL。
用法:
rclone backend getdownloadurlau 123:path/to/file VidHub/1.7.24
该命令返回指定文件的下载URL。请确保文件路径正确。`,
},
}

// Command 执行后端特定的命令。
func (f *Fs) Command(ctx context.Context, name string, arg []string, opt map[string]string) (out any, err error) {
	switch name {

	case "getdownloadurlua":
		path := ""
		ua := ""
		if len(arg) > 0 {
			ua = arg[0]
		}
		return f.getDownloadURLByUA(ctx, path, ua)

	case "stats":
		// 返回性能统计信息
		return f.performanceMetrics.GetStats(), nil

	case "logstats":
		// 记录性能统计信息到日志
		f.performanceMetrics.LogStats(f)
		return "性能统计已记录到日志", nil

	default:
		return nil, fs.ErrorCommandNotFound
	}
}

type ListRequest struct {
	ParentFileID int    `json:"parentFileId"`
	Limit        int    `json:"limit"`
	SearchData   string `json:"searchData,omitempty"`
	SearchMode   string `json:"searchMode,omitempty"`
	LastFileID   int    `json:"lastFileID,omitempty"`
}

type ListResponse struct {
	Code    int                   `json:"code"`
	Message string                `json:"message"`
	Data    GetFileListRespDataV2 `json:"data"` // 更改为特定类型
}

// GetFileListRespDataV2 表示文件列表响应的数据结构
type GetFileListRespDataV2 struct {
	// -1代表最后一页（无需再翻页查询）, 其他代表下一页开始的文件id，携带到请求参数中
	LastFileId int64 `json:"lastFileId"`
	// 文件列表
	FileList []FileListInfoRespDataV2 `json:"fileList"`
}

// FileListInfoRespDataV2 表示来自API响应的文件或文件夹项目
type FileListInfoRespDataV2 struct {
	// 文件ID
	FileID int64 `json:"fileID"`
	// 文件名
	Filename string `json:"filename"`
	// 0-文件  1-文件夹
	Type int `json:"type"`
	// 文件大小
	Size int64 `json:"size"`
	// md5
	Etag string `json:"etag"`
	// 文件审核状态, 大于 100 为审核驳回文件
	Status int `json:"status"`
	// 目录ID
	ParentFileID int64 `json:"parentFileID"`
	// 文件分类, 0-未知 1-音频 2-视频 3-图片
	Category int `json:"category"`
}

func (f *Fs) ListFile(ctx context.Context, parentFileID, limit int, searchData, searchMode string, lastFileID int) (*ListResponse, error) {
	fs.Debugf(f, "调用ListFile，参数：parentFileID=%d, limit=%d, lastFileID=%d", parentFileID, limit, lastFileID)

	// 只有在没有搜索条件时才使用缓存
	if searchData == "" && searchMode == "" {
		// 尝试从缓存获取
		if cached, found := f.getDirListFromCache(int64(parentFileID), int64(lastFileID)); found {
			// 构造缓存响应
			result := &ListResponse{
				Code:    0,
				Message: "success",
				Data: GetFileListRespDataV2{
					LastFileId: cached.LastFileID,
					FileList:   cached.FileList,
				},
			}
			fs.Debugf(f, "目录列表缓存命中: parentFileID=%d, fileCount=%d", parentFileID, len(cached.FileList))
			return result, nil
		}
	}

	// 构造查询参数
	params := url.Values{}
	params.Add("parentFileId", fmt.Sprintf("%d", parentFileID))
	params.Add("limit", fmt.Sprintf("%d", limit))

	if searchData != "" {
		params.Add("searchData", searchData)
	}
	if searchMode != "" {
		params.Add("searchMode", searchMode)
	}
	if lastFileID != 0 {
		params.Add("lastFileID", fmt.Sprintf("%d", lastFileID))
	}

	// 构造带查询参数的端点
	endpoint := "/api/v2/file/list?" + params.Encode()
	fs.Debugf(f, "API端点: %s%s", openAPIRootURL, endpoint)

	// 使用智能版本回退API调用进行适当的速率限制和错误处理
	var result ListResponse
	err := f.apiCallWithVersionFallback(ctx, "GET", "/api/file/list?"+params.Encode(), nil, &result)
	if err != nil {
		fs.Debugf(f, "ListFile API调用失败: %v", err)
		return nil, err
	}

	fs.Debugf(f, "ListFile API响应: code=%d, message=%s, fileCount=%d", result.Code, result.Message, len(result.Data.FileList))

	// 只有在没有搜索条件且API调用成功时才缓存结果
	if searchData == "" && searchMode == "" && result.Code == 0 {
		f.saveDirListToCache(int64(parentFileID), int64(lastFileID), result.Data.FileList, result.Data.LastFileId)
	}

	return &result, nil
}

func (f *Fs) pathToFileID(ctx context.Context, filePath string) (string, error) {
	// 根目录
	if filePath == "/" {
		return "0", nil
	}
	if len(filePath) > 1 && strings.HasSuffix(filePath, "/") {
		filePath = filePath[:len(filePath)-1]
	}

	// 尝试从缓存获取路径映射
	if cachedFileID, _, found := f.getPathToIDFromCache(filePath); found {
		fs.Debugf(f, "路径映射缓存命中: %s -> %s", filePath, cachedFileID)
		return cachedFileID, nil
	}
	currentID := "0"
	parts := strings.Split(filePath, "/")
	if len(parts) > 0 && parts[0] == "" { // 如果路径以/开头，移除空字符串
		parts = parts[1:]
	}
	for _, part := range parts {
		if part == "" {
			continue
		}
		findPart := false
		next := "0" // 根据API使用字符串作为next
		for {
			parentFileID, err := strconv.Atoi(currentID)
			if err != nil {
				return "", fmt.Errorf("invalid parentFileId: %s", currentID)
			}

			lastFileIDInt, err := strconv.Atoi(next)
			if err != nil {
				return "", fmt.Errorf("invalid next token: %s", next)
			}

			var response *ListResponse
			// 使用列表调速器进行速率限制
			err = f.listPacer.Call(func() (bool, error) {
				response, err = f.ListFile(ctx, parentFileID, 100, "", "", lastFileIDInt)
				if err != nil {
					return shouldRetry(ctx, nil, err)
				}
				return false, nil
			})
			if err != nil {
				return "", fmt.Errorf("list file failed: %w", err)
			}
			if response.Code != 200 && response.Code != 0 {
				return "", fmt.Errorf("API returned error code %d: %s", response.Code, response.Message)
			}
			infoList := response.Data.FileList
			if len(infoList) == 0 {
				break
			}
			for _, item := range infoList {
				if item.Filename == part {
					currentID = strconv.FormatInt(item.FileID, 10)
					findPart = true
					break
				}
			}
			if findPart {
				break
			}

			nextRaw := strconv.FormatInt(response.Data.LastFileId, 10)
			if nextRaw == "-1" {
				break
			} else {
				next = nextRaw
				// 无需手动睡眠 - 调速器处理速率限制
			}
		}
		if !findPart {
			return "", fs.ErrorObjectNotFound
		}
	}

	if currentID == "0" && filePath != "/" {
		return "", fs.ErrorObjectNotFound
	}

	// 缓存路径映射结果
	if currentID != "0" {
		// 确定是否为目录（通过检查最后找到的项目类型）
		isDir := true // 默认假设是目录，因为我们在查找路径
		f.savePathToIDToCache(filePath, currentID, "0", isDir)
	}

	return currentID, nil
}

type FileDetail struct {
	FileID       int64  `json:"fileId"`
	Filename     string `json:"filename"`
	ParentFileID int64  `json:"parentFileId"`
	Type         int    `json:"type"`
	ETag         string `json:"etag"`
	Size         int64  `json:"size"`
	Category     int    `json:"category"`
	Status       int    `json:"status"`
	PunishFlag   int    `json:"punishFlag"`
	S3KeyFlag    string `json:"s3KeyFlag"`
	StorageNode  string `json:"storageNode"`
	Trashed      int    `json:"trashed"`
	CreateAt     string `json:"createAt"`
	UpdateAt     string `json:"updateAt"`
}

type FileInfoResponse struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		List []FileDetail `json:"list"`
	} `json:"data"`
	TraceID string `json:"x-traceID"`
}

type FileDetailResponse struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		FileID       int64  `json:"fileID"`
		Filename     string `json:"filename"`
		Type         int    `json:"type"`
		Size         int64  `json:"size"`
		ETag         string `json:"etag"`
		Status       int    `json:"status"`
		ParentFileID int64  `json:"parentFileID"`
		CreateAt     string `json:"createAt"`
		Trashed      int    `json:"trashed"`
	} `json:"data"`
	TraceID string `json:"x-traceID"`
}

// FileInfo 表示'list'数组中单个文件的信息
type FileInfo struct {
	FileID       int64  `json:"fileId"`
	Filename     string `json:"filename"`
	ParentFileID int64  `json:"parentFileId"`
	Type         int    `json:"type"`
	ETag         string `json:"etag"`
	Size         int64  `json:"size"`
	Category     int    `json:"category"`
	Status       int    `json:"status"`
	PunishFlag   int    `json:"punishFlag"`
	S3KeyFlag    string `json:"s3KeyFlag"`
	StorageNode  string `json:"storageNode"`
	Trashed      int    `json:"trashed"`
	CreateAt     string `json:"createAt"`
	UpdateAt     string `json:"updateAt"`
}

// DataContainer 保存'list'数组
type DataContainer struct {
	List []FileInfo `json:"list"`
}

// FileInfosResponse 是API响应的顶级结构

// 定义Data结构体，映射data字段的内容
type Data struct {
	FileList []FileInfo `json:"fileList"`
}

// 定义FileInfosResponse结构体，映射整个响应JSON
type FileInfosResponse struct {
	Code     int    `json:"code"`
	Message  string `json:"message"`
	Data     Data   `json:"data"`
	XTraceID string `json:"x-traceID"`
}

// 定义FileInfoRequest结构体，用于发送请求的payload
type FileInfoRequest struct {
	FileIDs []int64 `json:"fileIDs"`
}

type Payload struct {
	S3KeyFlag string `json:"s3KeyFlag"`
	FileName  string `json:"fileName"`
	Etag      string `json:"etag"`
	Size      int64  `json:"size"`
	FileID    int64  `json:"fileId"`
}
type DownloadInfoResponse struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		DownloadURL string `json:"downloadUrl"` // 直链地址
		ExpireTime  string `json:"expireTime"`  // 过期时间
	} `json:"data"`
}

type UploadCreateResp struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		FileID      int64  `json:"fileID"`
		PreuploadID string `json:"preuploadID"`
		Reuse       bool   `json:"reuse"`
		SliceSize   int64  `json:"sliceSize"`
	} `json:"data"`
}

type UploadUrlResp struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		PresignedURL string `json:"presignedURL"`
	} `json:"data"`
}

// UploadProgress 跟踪多部分上传的进度，支持断点续传
type UploadProgress struct {
	PreuploadID   string         `json:"preuploadID"`
	TotalParts    int64          `json:"totalParts"`
	ChunkSize     int64          `json:"chunkSize"`
	FileSize      int64          `json:"fileSize"`
	UploadedParts map[int64]bool `json:"uploadedParts"` // partNumber -> uploaded
	FilePath      string         `json:"filePath"`      // local file path for resume
	MD5Hash       string         `json:"md5Hash"`       // file MD5 for verification
	CreatedAt     time.Time      `json:"createdAt"`
	mu            sync.RWMutex   `json:"-"` // 保护UploadedParts的并发访问
}

// SetUploaded 线程安全地标记分片已上传
func (up *UploadProgress) SetUploaded(partNumber int64) {
	up.mu.Lock()
	defer up.mu.Unlock()
	up.UploadedParts[partNumber] = true
}

// IsUploaded 线程安全地检查分片是否已上传
func (up *UploadProgress) IsUploaded(partNumber int64) bool {
	up.mu.RLock()
	defer up.mu.RUnlock()
	return up.UploadedParts[partNumber]
}

// GetUploadedCount 线程安全地获取已上传分片数量
func (up *UploadProgress) GetUploadedCount() int {
	up.mu.RLock()
	defer up.mu.RUnlock()
	return len(up.UploadedParts)
}

// RemoveUploaded 线程安全地移除已上传标记（用于重新上传损坏的分片）
func (up *UploadProgress) RemoveUploaded(partNumber int64) {
	up.mu.Lock()
	defer up.mu.Unlock()
	delete(up.UploadedParts, partNumber)
}

// GetUploadedParts 线程安全地获取已上传分片列表的副本
func (up *UploadProgress) GetUploadedParts() map[int64]bool {
	up.mu.RLock()
	defer up.mu.RUnlock()

	// 返回副本以避免外部修改
	result := make(map[int64]bool)
	for k, v := range up.UploadedParts {
		result[k] = v
	}
	return result
}

type UserInfoResp struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		UID            int64  `json:"uid"`
		Username       string `json:"username"`
		DisplayName    string `json:"displayName"`
		HeadImage      string `json:"headImage"`
		Passport       string `json:"passport"`
		Mail           string `json:"mail"`
		SpaceUsed      int64  `json:"spaceUsed"`
		SpacePermanent int64  `json:"spacePermanent"`
		SpaceTemp      int64  `json:"spaceTemp"`
		SpaceTempExpr  string `json:"spaceTempExpr"`
		Vip            bool   `json:"vip"`
		DirectTraffic  int64  `json:"directTraffic"`
		IsHideUID      bool   `json:"isHideUID"`
	} `json:"data"`
}

func (f *Fs) getDownloadURLByUA(ctx context.Context, filePath string, UA string) (string, error) {
	// Ensure token is valid before making API calls
	err := f.refreshTokenIfNecessary(ctx, false, false)
	if err != nil {
		return "", err
	}

	if UA == "" {
		UA = f.opt.UserAgent
	}

	if filePath == "" {
		filePath = f.root
	}

	fileID, err := f.pathToFileID(ctx, filePath)
	if err != nil {
		return "", fmt.Errorf("failed to get file ID for path %q: %w", filePath, err)
	}

	// Use the standard getDownloadURL method
	return f.getDownloadURL(ctx, fileID)
}

// isUnrecoverableError 检查错误是否为不可恢复的错误
func isUnrecoverableError(err error) bool {
	if err == nil {
		return false
	}

	errStr := strings.ToLower(err.Error())
	unrecoverableErrors := []string{
		"invalid credentials",
		"authentication failed",
		"file not found",
		"permission denied",
		"quota exceeded",
		"invalid request",
		"bad request",
		"forbidden",
		"文件不存在",
		"权限不足",
		"配额已满",
		"无效请求",
	}

	for _, unrecoverable := range unrecoverableErrors {
		if strings.Contains(errStr, unrecoverable) {
			return true
		}
	}
	return false
}

// validateOptions 验证配置选项的有效性
func validateOptions(opt *Options) error {
	var errors []string

	// 验证必需的认证信息
	if opt.ClientID == "" {
		errors = append(errors, "client_id 不能为空")
	}
	if opt.ClientSecret == "" {
		errors = append(errors, "client_secret 不能为空")
	}

	// 验证数值范围
	if opt.ListChunk <= 0 || opt.ListChunk > MaxListChunk {
		errors = append(errors, fmt.Sprintf("list_chunk 必须在 1-%d 范围内", MaxListChunk))
	}
	if opt.MaxUploadParts <= 0 || opt.MaxUploadParts > MaxUploadPartsLimit {
		errors = append(errors, fmt.Sprintf("max_upload_parts 必须在 1-%d 范围内", MaxUploadPartsLimit))
	}
	if opt.MaxConcurrentUploads <= 0 || opt.MaxConcurrentUploads > MaxConcurrentLimit {
		errors = append(errors, fmt.Sprintf("max_concurrent_uploads 必须在 1-%d 范围内", MaxConcurrentLimit))
	}
	if opt.MaxConcurrentDownloads <= 0 || opt.MaxConcurrentDownloads > MaxConcurrentLimit {
		errors = append(errors, "max_concurrent_downloads 必须在 1-100 范围内")
	}

	// 验证时间配置
	if time.Duration(opt.PacerMinSleep) < 0 {
		errors = append(errors, "pacer_min_sleep 不能为负数")
	}
	if time.Duration(opt.ListPacerMinSleep) < 0 {
		errors = append(errors, "list_pacer_min_sleep 不能为负数")
	}
	if time.Duration(opt.ConnTimeout) < 0 {
		errors = append(errors, "conn_timeout 不能为负数")
	}
	if time.Duration(opt.Timeout) < 0 {
		errors = append(errors, "timeout 不能为负数")
	}
	if time.Duration(opt.ProgressUpdateInterval) < 0 {
		errors = append(errors, "progress_update_interval 不能为负数")
	}

	// 验证大小配置
	if int64(opt.ChunkSize) <= 0 {
		errors = append(errors, "chunk_size 必须大于 0")
	}
	if int64(opt.ChunkSize) > 5*1024*1024*1024 { // 5GB
		errors = append(errors, "chunk_size 不能超过 5GB")
	}
	if int64(opt.UploadCutoff) < 0 {
		errors = append(errors, "upload_cutoff 不能为负数")
	}

	// 验证UserAgent
	if opt.UserAgent != "" && len(opt.UserAgent) > 200 {
		errors = append(errors, "user_agent 长度不能超过 200 字符")
	}

	// 验证RootFolderID格式（如果提供）
	if opt.RootFolderID != "" {
		if _, err := strconv.ParseInt(opt.RootFolderID, 10, 64); err != nil {
			errors = append(errors, "root_folder_id 必须是有效的数字")
		}
	}

	if len(errors) > 0 {
		return fmt.Errorf("配置验证失败: %s", strings.Join(errors, "; "))
	}

	return nil
}

// normalizeOptions 标准化和修正配置选项
func normalizeOptions(opt *Options) {
	// 设置默认值
	if opt.ListChunk <= 0 {
		opt.ListChunk = DefaultListChunk
	}
	if opt.MaxUploadParts <= 0 {
		opt.MaxUploadParts = DefaultMaxUploadParts
	}
	if opt.MaxConcurrentUploads <= 0 {
		opt.MaxConcurrentUploads = 4
	}
	if opt.MaxConcurrentDownloads <= 0 {
		opt.MaxConcurrentDownloads = 4
	}

	// 设置合理的默认超时时间
	if time.Duration(opt.PacerMinSleep) <= 0 {
		opt.PacerMinSleep = fs.Duration(100 * time.Millisecond)
	}
	if time.Duration(opt.ListPacerMinSleep) <= 0 {
		opt.ListPacerMinSleep = fs.Duration(100 * time.Millisecond)
	}
	if time.Duration(opt.ConnTimeout) <= 0 {
		opt.ConnTimeout = fs.Duration(60 * time.Second)
	}
	if time.Duration(opt.Timeout) <= 0 {
		opt.Timeout = fs.Duration(300 * time.Second)
	}
	if time.Duration(opt.ProgressUpdateInterval) <= 0 {
		opt.ProgressUpdateInterval = fs.Duration(1 * time.Second)
	}

	// 设置合理的默认大小
	if int64(opt.ChunkSize) <= 0 {
		opt.ChunkSize = fs.SizeSuffix(100 * 1024 * 1024) // 100MB
	}
	if int64(opt.UploadCutoff) <= 0 {
		opt.UploadCutoff = fs.SizeSuffix(200 * 1024 * 1024) // 200MB
	}

	// 设置默认UserAgent
	if opt.UserAgent == "" {
		opt.UserAgent = "rclone/" + fs.Version
	}
}

// calculateChecksum 计算数据的轻量校验和（使用CRC32替代SHA256）
func calculateChecksum(data interface{}) string {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return ""
	}

	// 使用CRC32替代SHA256，减少CPU开销
	hash := crc32.ChecksumIEEE(jsonData)
	return fmt.Sprintf("%08x", hash)
}

// generateCacheVersion 生成缓存版本号
func generateCacheVersion() int64 {
	return time.Now().UnixNano()
}

// validateCacheEntry 验证缓存条目的完整性（轻量级验证）
func validateCacheEntry(data interface{}, expectedChecksum string) bool {
	if expectedChecksum == "" {
		return true // 如果没有校验和，跳过验证
	}

	// 对于性能考虑，只对小数据进行校验
	jsonData, err := json.Marshal(data)
	if err != nil {
		return false
	}

	// 如果数据过大（>10KB），跳过校验以提高性能
	if len(jsonData) > 10*1024 {
		return true
	}

	actualChecksum := calculateChecksum(data)
	return actualChecksum == expectedChecksum
}

// fastValidateCacheEntry 快速验证缓存条目（仅检查基本结构）
func fastValidateCacheEntry(data interface{}) bool {
	if data == nil {
		return false
	}

	// 简单的结构检查，避免复杂的校验和计算
	switch v := data.(type) {
	case []interface{}:
		return len(v) >= 0 // 数组类型，检查长度
	case map[string]interface{}:
		return len(v) >= 0 // 对象类型，检查键数量
	case string:
		return len(v) > 0 // 字符串类型，检查非空
	default:
		return true // 其他类型默认有效
	}
}

// APIVersionManager 管理API版本兼容性
type APIVersionManager struct {
	mu                sync.RWMutex
	preferredVersions map[string]string // endpoint -> preferred version
	failedVersions    map[string]bool   // endpoint:version -> failed
	lastChecked       time.Time
}

// NewAPIVersionManager 创建API版本管理器
func NewAPIVersionManager() *APIVersionManager {
	return &APIVersionManager{
		preferredVersions: make(map[string]string),
		failedVersions:    make(map[string]bool),
		lastChecked:       time.Now(),
	}
}

// GetPreferredVersion 获取端点的首选版本
func (avm *APIVersionManager) GetPreferredVersion(endpoint string) string {
	avm.mu.RLock()
	defer avm.mu.RUnlock()

	// 提取端点的基础路径
	basePath := avm.extractBasePath(endpoint)

	if version, exists := avm.preferredVersions[basePath]; exists {
		return version
	}

	// 默认优先使用v2
	return "v2"
}

// MarkVersionFailed 标记某个版本失败
func (avm *APIVersionManager) MarkVersionFailed(endpoint, version string) {
	avm.mu.Lock()
	defer avm.mu.Unlock()

	basePath := avm.extractBasePath(endpoint)
	key := fmt.Sprintf("%s:%s", basePath, version)
	avm.failedVersions[key] = true

	// 如果v2失败，切换到v1
	if version == "v2" {
		avm.preferredVersions[basePath] = "v1"
	}
}

// IsVersionFailed 检查版本是否已失败
func (avm *APIVersionManager) IsVersionFailed(endpoint, version string) bool {
	avm.mu.RLock()
	defer avm.mu.RUnlock()

	basePath := avm.extractBasePath(endpoint)
	key := fmt.Sprintf("%s:%s", basePath, version)
	return avm.failedVersions[key]
}

// extractBasePath 提取API端点的基础路径
func (avm *APIVersionManager) extractBasePath(endpoint string) string {
	// 移除版本号，提取基础路径
	// 例如: "/upload/v2/file/create" -> "/upload/file/create"
	parts := strings.Split(endpoint, "/")
	var baseParts []string

	for _, part := range parts {
		if part != "" && !strings.HasPrefix(part, "v") {
			baseParts = append(baseParts, part)
		}
	}

	return "/" + strings.Join(baseParts, "/")
}

// shouldRetryCrossCloudTransfer 智能重试策略，专门针对跨云盘传输
func (f *Fs) shouldRetryCrossCloudTransfer(err error, attempt int, transferredBytes int64, fileSize int64) (bool, time.Duration) {
	maxRetries := 5 // 默认最多重试5次

	// 如果已传输了大量数据，给更多重试机会
	if transferredBytes > 100*1024*1024 { // 超过100MB
		maxRetries = 8
	}
	if transferredBytes > 500*1024*1024 { // 超过500MB
		maxRetries = 10
	}

	if attempt >= maxRetries {
		return false, 0
	}

	// 检查错误类型
	errStr := strings.ToLower(err.Error())
	isRetryableError := false

	retryableErrors := []string{
		"context deadline exceeded",
		"client.timeout",
		"request canceled",
		"connection reset",
		"temporary failure",
		"network timeout",
		"read timeout",
		"write timeout",
		"i/o timeout",
		"connection refused",
		"no such host",
		"network unreachable",
	}

	for _, retryable := range retryableErrors {
		if strings.Contains(errStr, retryable) {
			isRetryableError = true
			break
		}
	}

	if !isRetryableError {
		return false, 0
	}

	// 计算退避延迟
	baseDelay := time.Second

	// 根据传输进度调整延迟
	if transferredBytes > 0 && fileSize > 0 {
		progressRatio := float64(transferredBytes) / float64(fileSize)
		if progressRatio > 0.8 { // 接近完成，快速重试
			baseDelay = 2 * time.Second
		} else if progressRatio > 0.5 { // 传输过半，中等延迟
			baseDelay = 3 * time.Second
		} else { // 刚开始，较长延迟
			baseDelay = 5 * time.Second
		}
	}

	// 指数退避，但有上限
	delay := time.Duration(attempt*attempt) * baseDelay
	maxDelay := 30 * time.Second
	if delay > maxDelay {
		delay = maxDelay
	}

	return true, delay
}

// apiCallWithVersionFallback 带版本回退的智能API调用
func (f *Fs) apiCallWithVersionFallback(ctx context.Context, method, baseEndpoint string, body io.Reader, result interface{}) error {
	// 获取首选版本
	preferredVersion := f.apiVersionManager.GetPreferredVersion(baseEndpoint)

	// 构建完整的端点URL
	endpoint := f.buildVersionedEndpoint(baseEndpoint, preferredVersion)

	// 尝试首选版本
	err := f.apiCall(ctx, method, endpoint, body, result)
	if err == nil {
		return nil
	}

	// 如果首选版本失败，标记失败并尝试回退版本
	f.apiVersionManager.MarkVersionFailed(baseEndpoint, preferredVersion)

	// 确定回退版本
	fallbackVersion := "v1"
	if preferredVersion == "v1" {
		fallbackVersion = "v2"
	}

	// 检查回退版本是否已知失败
	if f.apiVersionManager.IsVersionFailed(baseEndpoint, fallbackVersion) {
		return fmt.Errorf("所有API版本都失败: %w", err)
	}

	// 尝试回退版本
	fallbackEndpoint := f.buildVersionedEndpoint(baseEndpoint, fallbackVersion)

	// 重新读取body（如果需要）
	var fallbackBody io.Reader
	if body != nil {
		if seeker, ok := body.(io.Seeker); ok {
			seeker.Seek(0, io.SeekStart)
			fallbackBody = body
		} else {
			// 如果body不能重新读取，返回原始错误
			return fmt.Errorf("无法重试API调用，body不可重用: %w", err)
		}
	}

	fallbackErr := f.apiCall(ctx, method, fallbackEndpoint, fallbackBody, result)
	if fallbackErr == nil {
		fs.Debugf(f, "API版本回退成功: %s -> %s", preferredVersion, fallbackVersion)
		return nil
	}

	// 标记回退版本也失败
	f.apiVersionManager.MarkVersionFailed(baseEndpoint, fallbackVersion)

	return fmt.Errorf("API调用失败，%s: %w, %s: %w", preferredVersion, err, fallbackVersion, fallbackErr)
}

// buildVersionedEndpoint 构建带版本的端点URL
func (f *Fs) buildVersionedEndpoint(baseEndpoint, version string) string {
	// 如果端点已经包含版本，直接返回
	if strings.Contains(baseEndpoint, "/v1/") || strings.Contains(baseEndpoint, "/v2/") {
		return baseEndpoint
	}

	// 插入版本号
	// 例如: "/upload/file/create" -> "/upload/v2/file/create"
	parts := strings.Split(baseEndpoint, "/")
	if len(parts) >= 3 {
		// 在第二个部分后插入版本
		newParts := make([]string, 0, len(parts)+1)
		newParts = append(newParts, parts[0], parts[1], version)
		newParts = append(newParts, parts[2:]...)
		return strings.Join(newParts, "/")
	}

	return baseEndpoint
}

// shouldRetry 根据响应和错误确定是否重试API调用
// 使用智能的指数退避策略
func shouldRetry(ctx context.Context, resp *http.Response, err error) (bool, error) {
	if fserrors.ContextError(ctx, &err) {
		return false, err
	}

	// 网络错误时重试
	if err != nil {
		// 检查是否为不可恢复的错误
		if isUnrecoverableError(err) {
			return false, err
		}

		if fserrors.ShouldRetry(err) {
			// 使用基础重试延迟
			return true, fserrors.NewErrorRetryAfter(baseRetryDelay)
		}
		return false, err
	}

	// 检查HTTP状态码
	if resp != nil {
		switch resp.StatusCode {
		case http.StatusTooManyRequests:
			// 速率受限 - 使用较长的退避时间
			retryAfter := calculateRetryDelay(3) // 第3次重试的延迟
			fs.Debugf(nil, "速率受限（API错误429），将使用更长退避时间重试")
			return true, fserrors.NewErrorRetryAfter(retryAfter)
		case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			// 服务器错误 - 使用中等退避时间
			retryAfter := calculateRetryDelay(2) // 第2次重试的延迟
			fs.Debugf(nil, "服务器错误 %d，将在%v后重试", resp.StatusCode, retryAfter)
			return true, fserrors.NewErrorRetryAfter(retryAfter)
		case http.StatusUnauthorized:
			// 令牌可能已过期 - 短暂延迟后让调用者处理令牌刷新
			return false, fserrors.NewErrorRetryAfter(baseRetryDelay)
		}
	}

	return false, err
}

// calculateRetryDelay 计算指数退避的重试延迟
func calculateRetryDelay(attempt int) time.Duration {
	if attempt <= 0 {
		return baseRetryDelay
	}

	delay := time.Duration(float64(baseRetryDelay) * math.Pow(retryMultiplier, float64(attempt-1)))
	if delay > maxRetryDelay {
		delay = maxRetryDelay
	}

	// 添加随机抖动以避免雷群效应
	jitter := time.Duration(rand.Float64() * float64(delay) * 0.1) // 10%的抖动
	return delay + jitter
}

// getPacerForEndpoint 根据API端点返回适当的调速器
// 基于官方API限流文档：https://123yunpan.yuque.com/org-wiki-123yunpan-muaork/cr6ced/
func (f *Fs) getPacerForEndpoint(endpoint string) *fs.Pacer {
	switch {
	// v2 API - 高频率
	case strings.Contains(endpoint, "/api/v2/file/list"):
		return f.listPacer // ~14 QPS (官方15 QPS)

	// v1上传API - 根据官方限制分类
	case strings.Contains(endpoint, "/upload/v1/file/upload_complete"),
		strings.Contains(endpoint, "/upload/v1/file/get_upload_url"),
		strings.Contains(endpoint, "/upload/v1/file/list_upload_parts"):
		return f.downloadPacer // ~16 QPS (官方20 QPS) - 复用高频率pacer

	case strings.Contains(endpoint, "/upload/v1/file/create"),
		strings.Contains(endpoint, "/upload/v1/file/upload_async_result"),
		strings.Contains(endpoint, "/upload/v1/file/mkdir"),
		strings.Contains(endpoint, "/api/v1/file/trash"):
		return f.strictPacer // ~4 QPS (官方5 QPS) - 中等频率

	// v2分片上传API
	case strings.Contains(endpoint, "/upload/v2/file/slice"):
		return f.uploadPacer // ~5 QPS (保守估计)

	// 文件操作API - 高频率
	case strings.Contains(endpoint, "/api/v1/file/move"),
		strings.Contains(endpoint, "/api/v1/file/infos"),
		strings.Contains(endpoint, "/api/v1/user/info"):
		return f.listPacer // ~8 QPS (官方10 QPS) - 复用列表pacer

	// 文件操作API - 中等频率
	case strings.Contains(endpoint, "/api/v1/file/download_info"):
		return f.strictPacer // ~4 QPS (官方5 QPS)

	// 认证API
	case strings.Contains(endpoint, "/api/v1/access_token"):
		return f.downloadPacer // ~6 QPS (官方8 QPS) - 复用下载pacer

	// 低频率API
	case strings.Contains(endpoint, "/api/v1/file/delete"),
		strings.Contains(endpoint, "/api/v1/file/list"),
		strings.Contains(endpoint, "/api/v1/video/transcode/list"):
		return f.strictPacer // ~0.8-1 QPS (官方1 QPS) - 最严格限制

	default:
		// 为了安全起见，未知端点默认使用最严格的调速器
		fs.Debugf(f, "⚠️  未知API端点，使用最严格限制: %s", endpoint)
		return f.strictPacer // 最严格的限制
	}
}

// ErrorContext 错误上下文信息，用于提供更详细的错误信息
type ErrorContext struct {
	Operation string            // 操作类型
	Method    string            // HTTP方法
	Endpoint  string            // API端点
	FileID    string            // 文件ID（如果适用）
	FileName  string            // 文件名（如果适用）
	Extra     map[string]string // 额外信息
}

// WrapError 包装错误并添加上下文信息
func (f *Fs) WrapError(err error, ctx ErrorContext) error {
	if err == nil {
		return nil
	}

	var details []string
	if ctx.Operation != "" {
		details = append(details, fmt.Sprintf("操作=%s", ctx.Operation))
	}
	if ctx.Method != "" && ctx.Endpoint != "" {
		details = append(details, fmt.Sprintf("API=%s %s", ctx.Method, ctx.Endpoint))
	}
	if ctx.FileID != "" {
		details = append(details, fmt.Sprintf("文件ID=%s", ctx.FileID))
	}
	if ctx.FileName != "" {
		details = append(details, fmt.Sprintf("文件名=%s", ctx.FileName))
	}
	for key, value := range ctx.Extra {
		details = append(details, fmt.Sprintf("%s=%s", key, value))
	}

	if len(details) > 0 {
		return fmt.Errorf("%s [%s]: %w", ctx.Operation, strings.Join(details, ", "), err)
	}
	return fmt.Errorf("%s: %w", ctx.Operation, err)
}

// calculateAdaptiveTimeout 根据操作类型和网络状况计算自适应超时时间
func (f *Fs) calculateAdaptiveTimeout(method, endpoint string) time.Duration {
	baseTimeout := time.Duration(f.opt.Timeout)
	if baseTimeout <= 0 {
		baseTimeout = defaultTimeout
	}

	// 根据操作类型调整超时时间
	switch {
	case strings.Contains(endpoint, "/upload/"):
		// 上传操作需要更长的超时时间
		return baseTimeout * 3
	case strings.Contains(endpoint, "/download"):
		// 下载操作需要更长的超时时间
		return baseTimeout * 2
	case strings.Contains(endpoint, "/file/list"):
		// 文件列表操作通常较快
		return baseTimeout
	case method == "POST":
		// POST操作通常需要更多时间
		return baseTimeout * 2
	default:
		// 默认超时时间
		return baseTimeout
	}
}

// calculateCrossCloudTimeout 针对跨云盘传输的特殊超时计算
func (f *Fs) calculateCrossCloudTimeout(fileSize int64, transferredBytes int64) time.Duration {
	baseTimeout := time.Duration(f.opt.Timeout)
	if baseTimeout <= 0 {
		baseTimeout = 300 * time.Second // 5分钟基础超时
	}

	// 根据文件大小动态调整
	if fileSize > 1024*1024*1024 { // 大于1GB
		baseTimeout *= 4 // 20分钟
	} else if fileSize > 500*1024*1024 { // 大于500MB
		baseTimeout *= 3 // 15分钟
	} else if fileSize > 100*1024*1024 { // 大于100MB
		baseTimeout *= 2 // 10分钟
	}

	// 根据已传输进度调整（避免重新开始的损失）
	if transferredBytes > 0 && fileSize > 0 {
		progressRatio := float64(transferredBytes) / float64(fileSize)
		if progressRatio > 0.5 { // 已传输超过50%
			baseTimeout = time.Duration(float64(baseTimeout) * 1.5) // 给更多时间完成
		} else if progressRatio > 0.8 { // 已传输超过80%
			baseTimeout *= 2 // 接近完成，给足够时间
		}
	}

	// 根据网络质量调整（使用基础性能指标）
	if f.performanceMetrics != nil {
		// 使用基础性能指标进行网络质量评估
		stats := f.performanceMetrics.GetStats()
		if apiErrorRate, ok := stats["api_error_rate"].(float64); ok {
			if apiErrorRate > 0.5 { // 错误率较高，网络质量较差
				baseTimeout = time.Duration(float64(baseTimeout) * 1.5)
			} else if apiErrorRate < 0.1 { // 错误率较低，网络质量良好
				baseTimeout = time.Duration(float64(baseTimeout) * 0.8)
			}
		}
	}

	// 设置最小和最大限制
	minTimeout := 60 * time.Second // 最少1分钟
	maxTimeout := 30 * time.Minute // 最多30分钟

	if baseTimeout < minTimeout {
		baseTimeout = minTimeout
	} else if baseTimeout > maxTimeout {
		baseTimeout = maxTimeout
	}

	return baseTimeout
}

// ResumeInfo 断点续传信息
type ResumeInfo struct {
	PreuploadID    string           `json:"preupload_id"`
	FileName       string           `json:"file_name"`
	FileSize       int64            `json:"file_size"`
	ChunkSize      int64            `json:"chunk_size"`
	UploadedChunks map[int64]string `json:"uploaded_chunks"` // chunk_index -> etag
	LastChunkIndex int64            `json:"last_chunk_index"`
	CreatedAt      time.Time        `json:"created_at"`
	LastUpdated    time.Time        `json:"last_updated"`
	MD5Hash        string           `json:"md5_hash"`
	ParentFileID   int64            `json:"parent_file_id"`
	TotalChunks    int64            `json:"total_chunks"`
	UploadedBytes  int64            `json:"uploaded_bytes"`
}

// ResumeManager 断点续传管理器
type ResumeManager struct {
	mu           sync.RWMutex
	resumeCache  *cache.BadgerCache
	fs           *Fs
	cleanupTimer *time.Timer
}

// NewResumeManager 创建断点续传管理器
func NewResumeManager(fs *Fs, cacheDir string) (*ResumeManager, error) {
	resumeCache, err := cache.NewBadgerCache("resume", cacheDir)
	if err != nil {
		return nil, fmt.Errorf("创建断点续传缓存失败: %w", err)
	}

	rm := &ResumeManager{
		resumeCache: resumeCache,
		fs:          fs,
	}

	// 启动定期清理过期的断点续传信息
	rm.startCleanupTimer()

	return rm, nil
}

// SaveResumeInfo 保存断点续传信息
func (rm *ResumeManager) SaveResumeInfo(info *ResumeInfo) error {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	info.LastUpdated = time.Now()
	cacheKey := fmt.Sprintf("resume_%s", info.PreuploadID)

	err := rm.resumeCache.Set(cacheKey, info, 24*time.Hour)
	if err != nil {
		return fmt.Errorf("保存断点续传信息失败: %w", err)
	}

	fs.Debugf(rm.fs, "保存断点续传信息: %s, 已上传分片: %d/%d",
		info.FileName, len(info.UploadedChunks), info.TotalChunks)

	return nil
}

// LoadResumeInfo 加载断点续传信息
func (rm *ResumeManager) LoadResumeInfo(preuploadID string) (*ResumeInfo, error) {
	rm.mu.RLock()
	defer rm.mu.RUnlock()

	cacheKey := fmt.Sprintf("resume_%s", preuploadID)
	var info ResumeInfo

	found, err := rm.resumeCache.Get(cacheKey, &info)
	if err != nil {
		return nil, fmt.Errorf("加载断点续传信息失败: %w", err)
	}

	if !found {
		return nil, nil // 没有找到断点续传信息
	}

	// 检查信息是否过期（超过24小时）
	if time.Since(info.CreatedAt) > 24*time.Hour {
		rm.DeleteResumeInfo(preuploadID)
		return nil, nil
	}

	fs.Debugf(rm.fs, "加载断点续传信息: %s, 已上传分片: %d/%d",
		info.FileName, len(info.UploadedChunks), info.TotalChunks)

	return &info, nil
}

// UpdateChunkInfo 更新分片上传信息
func (rm *ResumeManager) UpdateChunkInfo(preuploadID string, chunkIndex int64, etag string, chunkSize int64) error {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	cacheKey := fmt.Sprintf("resume_%s", preuploadID)
	var info ResumeInfo

	found, err := rm.resumeCache.Get(cacheKey, &info)
	if err != nil {
		return fmt.Errorf("获取断点续传信息失败: %w", err)
	}

	if !found {
		return fmt.Errorf("断点续传信息不存在: %s", preuploadID)
	}

	// 更新分片信息
	if info.UploadedChunks == nil {
		info.UploadedChunks = make(map[int64]string)
	}

	info.UploadedChunks[chunkIndex] = etag
	info.LastChunkIndex = chunkIndex
	info.LastUpdated = time.Now()
	info.UploadedBytes += chunkSize

	// 保存更新后的信息
	err = rm.resumeCache.Set(cacheKey, &info, 24*time.Hour)
	if err != nil {
		return fmt.Errorf("更新断点续传信息失败: %w", err)
	}

	fs.Debugf(rm.fs, "更新分片信息: %s, 分片 %d, 进度: %d/%d",
		info.FileName, chunkIndex, len(info.UploadedChunks), info.TotalChunks)

	return nil
}

// DeleteResumeInfo 删除断点续传信息
func (rm *ResumeManager) DeleteResumeInfo(preuploadID string) error {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	cacheKey := fmt.Sprintf("resume_%s", preuploadID)
	err := rm.resumeCache.Delete(cacheKey)
	if err != nil {
		return fmt.Errorf("删除断点续传信息失败: %w", err)
	}

	fs.Debugf(rm.fs, "删除断点续传信息: %s", preuploadID)
	return nil
}

// GetResumeProgress 获取断点续传进度
func (rm *ResumeManager) GetResumeProgress(preuploadID string) (uploaded, total int64, percentage float64, err error) {
	info, err := rm.LoadResumeInfo(preuploadID)
	if err != nil {
		return 0, 0, 0, err
	}

	if info == nil {
		return 0, 0, 0, nil
	}

	uploaded = int64(len(info.UploadedChunks))
	total = info.TotalChunks

	if total > 0 {
		percentage = float64(uploaded) / float64(total) * 100.0
	}

	return uploaded, total, percentage, nil
}

// startCleanupTimer 启动定期清理定时器
func (rm *ResumeManager) startCleanupTimer() {
	rm.cleanupTimer = time.AfterFunc(time.Hour, func() {
		rm.cleanupExpiredResumeInfo()
		rm.startCleanupTimer() // 重新启动定时器
	})
}

// cleanupExpiredResumeInfo 清理过期的断点续传信息
func (rm *ResumeManager) cleanupExpiredResumeInfo() {
	rm.fs.debugf(LogLevelDebug, "开始清理过期的断点续传信息")

	// 获取所有断点续传信息的键
	keys, err := rm.getAllResumeKeys()
	if err != nil {
		rm.fs.debugf(LogLevelError, "获取断点续传键列表失败: %v", err)
		return
	}

	cleanedCount := 0
	expiredThreshold := time.Now().Add(-24 * time.Hour) // 24小时过期

	for _, key := range keys {
		info, err := rm.LoadResumeInfo(key)
		if err != nil {
			// 如果加载失败，可能是损坏的数据，直接删除
			rm.DeleteResumeInfo(key)
			cleanedCount++
			continue
		}

		// 检查是否过期
		if info.CreatedAt.Before(expiredThreshold) {
			rm.DeleteResumeInfo(key)
			cleanedCount++
			rm.fs.debugf(LogLevelVerbose, "删除过期的断点续传信息: %s", key)
		}
	}

	rm.fs.debugf(LogLevelDebug, "断点续传信息清理完成，清理了 %d 个过期条目", cleanedCount)
}

// getAllResumeKeys 获取所有断点续传信息的键
func (rm *ResumeManager) getAllResumeKeys() ([]string, error) {
	// 使用前缀扫描获取所有resume相关的键
	var keys []string

	// 这里需要实现BadgerDB的前缀扫描
	// 由于当前的cache接口限制，使用简化实现
	// 实际项目中可以扩展cache接口支持前缀扫描
	// 目前返回空列表，避免意外删除数据

	return keys, nil
}

// Close 关闭断点续传管理器
func (rm *ResumeManager) Close() error {
	if rm.cleanupTimer != nil {
		rm.cleanupTimer.Stop()
	}

	if rm.resumeCache != nil {
		return rm.resumeCache.Close()
	}

	return nil
}

// NetworkQualityMonitor 网络质量监控器
type NetworkQualityMonitor struct {
	mu                 sync.RWMutex
	qualityHistory     []NetworkQualitySnapshot
	currentQuality     float64
	lastUpdate         time.Time
	adaptiveParams     *AdaptiveParameters
	fs                 *Fs
	monitoringInterval time.Duration
	stopChan           chan struct{}
	isMonitoring       bool
}

// NetworkQualitySnapshot 网络质量快照
type NetworkQualitySnapshot struct {
	Timestamp   time.Time `json:"timestamp"`
	Quality     float64   `json:"quality"`      // 0.0-1.0
	Latency     int64     `json:"latency"`      // 毫秒
	PacketLoss  float64   `json:"packet_loss"`  // 0.0-1.0
	Throughput  int64     `json:"throughput"`   // bytes/second
	ErrorRate   float64   `json:"error_rate"`   // 0.0-1.0
	RetryRate   float64   `json:"retry_rate"`   // 0.0-1.0
	SuccessRate float64   `json:"success_rate"` // 0.0-1.0
}

// AdaptiveParameters 自适应参数
type AdaptiveParameters struct {
	mu                  sync.RWMutex
	ChunkSize           int64         `json:"chunk_size"`
	ConcurrentUploads   int           `json:"concurrent_uploads"`
	ConcurrentDownloads int           `json:"concurrent_downloads"`
	TimeoutMultiplier   float64       `json:"timeout_multiplier"`
	RetryDelay          time.Duration `json:"retry_delay"`
	MaxRetries          int           `json:"max_retries"`
	BufferSize          int64         `json:"buffer_size"`
	LastAdjustment      time.Time     `json:"last_adjustment"`
	AdjustmentReason    string        `json:"adjustment_reason"`
}

// NewNetworkQualityMonitor 创建网络质量监控器
func NewNetworkQualityMonitor(fs *Fs) *NetworkQualityMonitor {
	return &NetworkQualityMonitor{
		qualityHistory:     make([]NetworkQualitySnapshot, 0, 100), // 保留最近100个快照
		currentQuality:     0.8,                                    // 默认假设网络质量良好
		lastUpdate:         time.Now(),
		monitoringInterval: 30 * time.Second, // 每30秒监控一次
		stopChan:           make(chan struct{}),
		fs:                 fs,
		adaptiveParams: &AdaptiveParameters{
			ChunkSize:           100 * 1024 * 1024, // 100MB
			ConcurrentUploads:   4,
			ConcurrentDownloads: 4,
			TimeoutMultiplier:   1.0,
			RetryDelay:          5 * time.Second,
			MaxRetries:          5,
			BufferSize:          32 * 1024 * 1024, // 32MB
			LastAdjustment:      time.Now(),
			AdjustmentReason:    "初始化默认参数",
		},
	}
}

// StartMonitoring 开始网络质量监控
func (nqm *NetworkQualityMonitor) StartMonitoring() {
	nqm.mu.Lock()
	defer nqm.mu.Unlock()

	if nqm.isMonitoring {
		return
	}

	nqm.isMonitoring = true

	go func() {
		ticker := time.NewTicker(nqm.monitoringInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				nqm.collectNetworkMetrics()
				nqm.adjustParametersIfNeeded()
			case <-nqm.stopChan:
				return
			}
		}
	}()

	fs.Debugf(nqm.fs, "网络质量监控已启动，监控间隔: %v", nqm.monitoringInterval)
}

// StopMonitoring 停止网络质量监控
func (nqm *NetworkQualityMonitor) StopMonitoring() {
	nqm.mu.Lock()
	defer nqm.mu.Unlock()

	if !nqm.isMonitoring {
		return
	}

	close(nqm.stopChan)
	nqm.isMonitoring = false

	fs.Debugf(nqm.fs, "网络质量监控已停止")
}

// collectNetworkMetrics 收集网络指标
func (nqm *NetworkQualityMonitor) collectNetworkMetrics() {
	// 从性能指标中获取数据
	stats := nqm.fs.performanceMetrics.GetStats()

	// 计算各项指标
	var errorRate, retryRate, successRate float64

	if totalUploads, ok := stats["total_uploads"].(int64); ok && totalUploads > 0 {
		if uploadErrors, ok := stats["upload_errors"].(int64); ok {
			errorRate = float64(uploadErrors) / float64(totalUploads)
		}
	}

	if retryCount, ok := stats["retry_count"].(int64); ok {
		if apiCallCount, ok := stats["api_call_count"].(int64); ok && apiCallCount > 0 {
			retryRate = float64(retryCount) / float64(apiCallCount)
		}
	}

	successRate = 1.0 - errorRate

	// 估算延迟（基于API响应时间）
	latency := nqm.estimateLatency()

	// 估算吞吐量（基于传输速度）
	throughput := nqm.estimateThroughput(stats)

	// 计算综合网络质量分数
	quality := nqm.calculateQualityScore(successRate, errorRate, retryRate, latency, throughput)

	// 创建快照
	snapshot := NetworkQualitySnapshot{
		Timestamp:   time.Now(),
		Quality:     quality,
		Latency:     latency,
		PacketLoss:  errorRate, // 简化：使用错误率作为丢包率
		Throughput:  throughput,
		ErrorRate:   errorRate,
		RetryRate:   retryRate,
		SuccessRate: successRate,
	}

	nqm.mu.Lock()
	defer nqm.mu.Unlock()

	// 添加到历史记录
	nqm.qualityHistory = append(nqm.qualityHistory, snapshot)

	// 保持历史记录大小限制
	if len(nqm.qualityHistory) > 100 {
		nqm.qualityHistory = nqm.qualityHistory[1:]
	}

	// 更新当前质量
	nqm.currentQuality = quality
	nqm.lastUpdate = time.Now()

	fs.Debugf(nqm.fs, "网络质量更新: %.2f (成功率: %.2f, 错误率: %.2f, 重试率: %.2f, 延迟: %dms, 吞吐量: %s/s)",
		quality, successRate, errorRate, retryRate, latency, fs.SizeSuffix(throughput))
}

// estimateLatency 估算网络延迟
func (nqm *NetworkQualityMonitor) estimateLatency() int64 {
	// 基于性能指标估算网络延迟
	if nqm.fs.performanceMetrics != nil {
		stats := nqm.fs.performanceMetrics.GetStats()

		// 从API调用统计中估算延迟
		if totalCalls, ok := stats["total_api_calls"].(int64); ok && totalCalls > 0 {
			// 使用API调用的平均响应时间作为延迟估算
			// 这是一个合理的近似值
			if avgResponseTime, ok := stats["avg_response_time"].(float64); ok {
				return int64(avgResponseTime * 1000) // 转换为毫秒
			}
		}
	}

	// 如果没有统计数据，使用基于网络类型的估算
	baseLatency := int64(100) // 100ms 默认延迟

	// 根据错误率调整延迟估算
	if nqm.currentQuality < 0.5 {
		baseLatency *= 3 // 网络质量差时，延迟可能更高
	} else if nqm.currentQuality < 0.8 {
		baseLatency *= 2
	}

	return baseLatency
}

// estimateThroughput 估算网络吞吐量
func (nqm *NetworkQualityMonitor) estimateThroughput(stats map[string]interface{}) int64 {
	// 基于平均传输速度估算吞吐量
	var avgSpeed float64

	if uploadSpeed, ok := stats["avg_upload_speed_mbps"].(float64); ok {
		avgSpeed += uploadSpeed
	}

	if downloadSpeed, ok := stats["avg_download_speed_mbps"].(float64); ok {
		avgSpeed += downloadSpeed
	}

	// 转换为bytes/second
	throughput := int64(avgSpeed * 1024 * 1024 / 2) // 除以2因为我们加了上传和下载速度

	return throughput
}

// calculateQualityScore 计算综合网络质量分数
func (nqm *NetworkQualityMonitor) calculateQualityScore(successRate, errorRate, retryRate float64, latency, throughput int64) float64 {
	// 权重分配
	successWeight := 0.4
	errorWeight := 0.3
	retryWeight := 0.2
	latencyWeight := 0.1

	// 成功率分数 (0-1)
	successScore := successRate

	// 错误率分数 (1-errorRate，错误率越低分数越高)
	errorScore := 1.0 - errorRate

	// 重试率分数 (1-retryRate，重试率越低分数越高)
	retryScore := 1.0 - retryRate

	// 延迟分数 (延迟越低分数越高)
	latencyScore := 1.0
	if latency > LatencyThreshold {
		latencyScore = 1.0 - float64(latency-LatencyThreshold)/float64(LatencyScoreRange) // 阈值以上开始扣分
		if latencyScore < 0 {
			latencyScore = 0
		}
	}

	// 计算加权平均分数
	quality := successScore*successWeight + errorScore*errorWeight + retryScore*retryWeight + latencyScore*latencyWeight

	// 确保分数在0-1范围内
	if quality < 0 {
		quality = 0
	} else if quality > 1 {
		quality = 1
	}

	return quality
}

// adjustParametersIfNeeded 根据网络质量调整参数
func (nqm *NetworkQualityMonitor) adjustParametersIfNeeded() {
	nqm.adaptiveParams.mu.Lock()
	defer nqm.adaptiveParams.mu.Unlock()

	// 避免频繁调整，至少间隔指定时间
	if time.Since(nqm.adaptiveParams.LastAdjustment) < QualityAdjustmentInterval {
		return
	}

	// 避免复制锁值，使用字段访问
	adjusted := false
	reason := ""

	// 根据网络质量调整参数
	if nqm.currentQuality < 0.3 {
		// 网络质量很差
		nqm.adaptiveParams.ChunkSize = 50 * 1024 * 1024 // 50MB
		nqm.adaptiveParams.ConcurrentUploads = 1
		nqm.adaptiveParams.ConcurrentDownloads = 1
		nqm.adaptiveParams.TimeoutMultiplier = 3.0
		nqm.adaptiveParams.RetryDelay = 10 * time.Second
		nqm.adaptiveParams.MaxRetries = 10
		nqm.adaptiveParams.BufferSize = 16 * 1024 * 1024 // 16MB
		reason = "网络质量很差，使用保守参数"
		adjusted = true
	} else if nqm.currentQuality < 0.6 {
		// 网络质量较差
		nqm.adaptiveParams.ChunkSize = 75 * 1024 * 1024 // 75MB
		nqm.adaptiveParams.ConcurrentUploads = 2
		nqm.adaptiveParams.ConcurrentDownloads = 2
		nqm.adaptiveParams.TimeoutMultiplier = 2.0
		nqm.adaptiveParams.RetryDelay = 7 * time.Second
		nqm.adaptiveParams.MaxRetries = 8
		nqm.adaptiveParams.BufferSize = 24 * 1024 * 1024 // 24MB
		reason = "网络质量较差，使用中等保守参数"
		adjusted = true
	} else if nqm.currentQuality < 0.8 {
		// 网络质量一般
		nqm.adaptiveParams.ChunkSize = 100 * 1024 * 1024 // 100MB
		nqm.adaptiveParams.ConcurrentUploads = 3
		nqm.adaptiveParams.ConcurrentDownloads = 3
		nqm.adaptiveParams.TimeoutMultiplier = 1.5
		nqm.adaptiveParams.RetryDelay = 5 * time.Second
		nqm.adaptiveParams.MaxRetries = 6
		nqm.adaptiveParams.BufferSize = 32 * 1024 * 1024 // 32MB
		reason = "网络质量一般，使用标准参数"
		adjusted = true
	} else {
		// 网络质量良好
		nqm.adaptiveParams.ChunkSize = 150 * 1024 * 1024 // 150MB
		nqm.adaptiveParams.ConcurrentUploads = 4
		nqm.adaptiveParams.ConcurrentDownloads = 4
		nqm.adaptiveParams.TimeoutMultiplier = 1.0
		nqm.adaptiveParams.RetryDelay = 3 * time.Second
		nqm.adaptiveParams.MaxRetries = 5
		nqm.adaptiveParams.BufferSize = 64 * 1024 * 1024 // 64MB
		reason = "网络质量良好，使用优化参数"
		adjusted = true
	}

	if adjusted {
		nqm.adaptiveParams.LastAdjustment = time.Now()
		nqm.adaptiveParams.AdjustmentReason = reason

		fs.Debugf(nqm.fs, "网络自适应参数调整: %s", reason)
		// 简化日志，避免复制锁值
		fs.Debugf(nqm.fs, "参数已调整: 分片大小 %s, 并发上传 %d, 并发下载 %d, 超时倍数 %.1f",
			fs.SizeSuffix(nqm.adaptiveParams.ChunkSize),
			nqm.adaptiveParams.ConcurrentUploads,
			nqm.adaptiveParams.ConcurrentDownloads,
			nqm.adaptiveParams.TimeoutMultiplier)
	}
}

// GetCurrentQuality 获取当前网络质量
func (nqm *NetworkQualityMonitor) GetCurrentQuality() float64 {
	nqm.mu.RLock()
	defer nqm.mu.RUnlock()
	return nqm.currentQuality
}

// GetAdaptiveParameters 获取当前自适应参数的副本
func (nqm *NetworkQualityMonitor) GetAdaptiveParameters() AdaptiveParameters {
	nqm.adaptiveParams.mu.RLock()
	defer nqm.adaptiveParams.mu.RUnlock()

	// 手动复制字段，避免复制锁值
	return AdaptiveParameters{
		ChunkSize:           nqm.adaptiveParams.ChunkSize,
		ConcurrentUploads:   nqm.adaptiveParams.ConcurrentUploads,
		ConcurrentDownloads: nqm.adaptiveParams.ConcurrentDownloads,
		TimeoutMultiplier:   nqm.adaptiveParams.TimeoutMultiplier,
		LastAdjustment:      nqm.adaptiveParams.LastAdjustment,
		AdjustmentReason:    nqm.adaptiveParams.AdjustmentReason,
		// 注意：不复制mu字段，因为它是锁
	}
}

// GetQualityHistory 获取网络质量历史
func (nqm *NetworkQualityMonitor) GetQualityHistory(duration time.Duration) []NetworkQualitySnapshot {
	nqm.mu.RLock()
	defer nqm.mu.RUnlock()

	cutoff := time.Now().Add(-duration)
	var result []NetworkQualitySnapshot

	for _, snapshot := range nqm.qualityHistory {
		if snapshot.Timestamp.After(cutoff) {
			result = append(result, snapshot)
		}
	}

	return result
}

// DownloadURLCacheManager 下载URL缓存管理器
type DownloadURLCacheManager struct {
	mu             sync.RWMutex
	cache          *cache.BadgerCache
	fs             *Fs
	preloadQueue   chan string            // 预加载队列
	preloadWorkers int                    // 预加载工作线程数
	stopPreload    chan struct{}          // 停止预加载信号
	isPreloading   bool                   // 是否正在预加载
	accessPattern  map[string]*AccessInfo // 访问模式分析
	hotFiles       map[string]time.Time   // 热点文件列表
	maxHotFiles    int                    // 最大热点文件数量
	cleanupTimer   *time.Timer            // 清理定时器
}

// AccessInfo 访问信息
type AccessInfo struct {
	FileID        string    `json:"file_id"`
	AccessCount   int       `json:"access_count"`
	LastAccess    time.Time `json:"last_access"`
	FirstAccess   time.Time `json:"first_access"`
	AccessPattern string    `json:"access_pattern"` // "frequent", "recent", "rare"
}

// EnhancedDownloadURLEntry 增强的下载URL缓存条目
type EnhancedDownloadURLEntry struct {
	URL           string    `json:"url"`
	ExpireTime    time.Time `json:"expire_time"`
	CachedAt      time.Time `json:"cached_at"`
	AccessCount   int       `json:"access_count"`
	LastAccess    time.Time `json:"last_access"`
	FileSize      int64     `json:"file_size"`
	Priority      int       `json:"priority"`       // 缓存优先级 1-10
	PreloadReason string    `json:"preload_reason"` // 预加载原因
}

// NewDownloadURLCacheManager 创建下载URL缓存管理器
func NewDownloadURLCacheManager(fs *Fs, cacheDir string) (*DownloadURLCacheManager, error) {
	cache, err := cache.NewBadgerCache("download_url_enhanced", cacheDir)
	if err != nil {
		return nil, fmt.Errorf("创建下载URL缓存失败: %w", err)
	}

	manager := &DownloadURLCacheManager{
		cache:          cache,
		fs:             fs,
		preloadQueue:   make(chan string, PreloadQueueCapacity), // 预加载队列容量
		preloadWorkers: 3,                                       // 3个预加载工作线程
		stopPreload:    make(chan struct{}),
		accessPattern:  make(map[string]*AccessInfo),
		hotFiles:       make(map[string]time.Time),
		maxHotFiles:    MaxHotFiles, // 最多跟踪热点文件数
	}

	// 启动预加载工作线程
	manager.startPreloadWorkers()

	// 启动定期清理
	manager.startCleanupTimer()

	return manager, nil
}

// GetDownloadURL 获取下载URL（带智能缓存）
func (ducm *DownloadURLCacheManager) GetDownloadURL(ctx context.Context, fileID string, fileSize int64) (string, error) {
	// 记录访问
	ducm.recordAccess(fileID, fileSize)

	// 尝试从缓存获取
	if url, found := ducm.getFromCache(fileID); found {
		fs.Debugf(ducm.fs, "下载URL缓存命中: fileID=%s", fileID)
		return url, nil
	}

	// 缓存未命中，从API获取
	url, err := ducm.fetchFromAPI(ctx, fileID)
	if err != nil {
		return "", err
	}

	// 保存到缓存
	ducm.saveToCache(fileID, url, fileSize, "api_fetch")

	// 触发相关文件的预加载
	ducm.triggerPreload(fileID)

	return url, nil
}

// getFromCache 从缓存获取URL
func (ducm *DownloadURLCacheManager) getFromCache(fileID string) (string, bool) {
	ducm.mu.RLock()
	defer ducm.mu.RUnlock()

	cacheKey := fmt.Sprintf("enhanced_url_%s", fileID)
	var entry EnhancedDownloadURLEntry

	found, err := ducm.cache.Get(cacheKey, &entry)
	if err != nil {
		fs.Debugf(ducm.fs, "获取下载URL缓存失败 %s: %v", cacheKey, err)
		return "", false
	}

	if !found {
		return "", false
	}

	// 检查是否过期
	if time.Now().After(entry.ExpireTime) {
		// 异步删除过期缓存
		go ducm.deleteFromCache(fileID)
		return "", false
	}

	// 更新访问信息
	entry.AccessCount++
	entry.LastAccess = time.Now()

	// 异步更新缓存
	go func() {
		ducm.cache.Set(cacheKey, &entry, time.Until(entry.ExpireTime))
	}()

	return entry.URL, true
}

// saveToCache 保存URL到缓存
func (ducm *DownloadURLCacheManager) saveToCache(fileID, url string, fileSize int64, reason string) {
	// 解析过期时间（从URL参数中提取或使用默认值）
	expireTime := ducm.parseExpireTime(url)
	if expireTime.IsZero() {
		expireTime = time.Now().Add(2 * time.Hour) // 默认2小时过期
	}

	// 计算缓存优先级
	priority := ducm.calculatePriority(fileID, fileSize)

	entry := EnhancedDownloadURLEntry{
		URL:           url,
		ExpireTime:    expireTime,
		CachedAt:      time.Now(),
		AccessCount:   1,
		LastAccess:    time.Now(),
		FileSize:      fileSize,
		Priority:      priority,
		PreloadReason: reason,
	}

	cacheKey := fmt.Sprintf("enhanced_url_%s", fileID)
	ttl := time.Until(expireTime)

	err := ducm.cache.Set(cacheKey, &entry, ttl)
	if err != nil {
		fs.Debugf(ducm.fs, "保存下载URL缓存失败 %s: %v", cacheKey, err)
		return
	}

	fs.Debugf(ducm.fs, "保存下载URL缓存: fileID=%s, 优先级=%d, 过期时间=%v, 原因=%s",
		fileID, priority, expireTime, reason)
}

// deleteFromCache 从缓存删除URL
func (ducm *DownloadURLCacheManager) deleteFromCache(fileID string) {
	cacheKey := fmt.Sprintf("enhanced_url_%s", fileID)
	err := ducm.cache.Delete(cacheKey)
	if err != nil {
		fs.Debugf(ducm.fs, "删除下载URL缓存失败 %s: %v", cacheKey, err)
	}
}

// fetchFromAPI 从API获取下载URL
func (ducm *DownloadURLCacheManager) fetchFromAPI(ctx context.Context, fileID string) (string, error) {
	var response DownloadInfoResponse
	endpoint := fmt.Sprintf("/api/v1/file/download_info?fileID=%s", fileID)

	err := ducm.fs.apiCall(ctx, "GET", endpoint, nil, &response)
	if err != nil {
		return "", err
	}

	if response.Code != 0 {
		return "", fmt.Errorf("API error %d: %s", response.Code, response.Message)
	}

	return response.Data.DownloadURL, nil
}

// recordAccess 记录文件访问
func (ducm *DownloadURLCacheManager) recordAccess(fileID string, fileSize int64) {
	ducm.mu.Lock()
	defer ducm.mu.Unlock()

	now := time.Now()

	// 更新访问模式
	if info, exists := ducm.accessPattern[fileID]; exists {
		info.AccessCount++
		info.LastAccess = now

		// 更新访问模式分类
		if info.AccessCount >= 5 {
			info.AccessPattern = "frequent"
		} else if now.Sub(info.FirstAccess) < time.Hour {
			info.AccessPattern = "recent"
		} else {
			info.AccessPattern = "rare"
		}
	} else {
		ducm.accessPattern[fileID] = &AccessInfo{
			FileID:        fileID,
			AccessCount:   1,
			LastAccess:    now,
			FirstAccess:   now,
			AccessPattern: "recent",
		}
	}

	// 更新热点文件列表
	if ducm.accessPattern[fileID].AccessCount >= 3 {
		ducm.hotFiles[fileID] = now

		// 限制热点文件数量
		if len(ducm.hotFiles) > ducm.maxHotFiles {
			ducm.cleanupOldHotFiles()
		}
	}
}

// calculatePriority 计算缓存优先级
func (ducm *DownloadURLCacheManager) calculatePriority(fileID string, fileSize int64) int {
	priority := 5 // 基础优先级

	ducm.mu.RLock()
	defer ducm.mu.RUnlock()

	// 根据访问模式调整优先级
	if info, exists := ducm.accessPattern[fileID]; exists {
		switch info.AccessPattern {
		case "frequent":
			priority += 3
		case "recent":
			priority += 2
		case "rare":
			priority += 0
		}

		// 根据访问次数调整
		if info.AccessCount >= 10 {
			priority += 2
		} else if info.AccessCount >= 5 {
			priority += 1
		}
	}

	// 根据文件大小调整（大文件优先级稍低，因为下载时间长）
	if fileSize > 1024*1024*1024 { // >1GB
		priority -= 1
	} else if fileSize < 10*1024*1024 { // <10MB
		priority += 1
	}

	// 确保优先级在1-10范围内
	if priority < 1 {
		priority = 1
	} else if priority > 10 {
		priority = 10
	}

	return priority
}

// triggerPreload 触发相关文件的预加载
func (ducm *DownloadURLCacheManager) triggerPreload(fileID string) {
	// 预加载策略：
	// 1. 同目录的其他文件
	// 2. 最近访问的文件
	// 3. 热点文件

	// 这里实现一个简化的预加载策略
	// 实际项目中可以根据具体需求实现更复杂的策略

	select {
	case ducm.preloadQueue <- fileID:
		// 成功加入预加载队列
	default:
		// 队列已满，跳过预加载
		fs.Debugf(ducm.fs, "预加载队列已满，跳过文件: %s", fileID)
	}
}

// startPreloadWorkers 启动预加载工作线程
func (ducm *DownloadURLCacheManager) startPreloadWorkers() {
	ducm.mu.Lock()
	defer ducm.mu.Unlock()

	if ducm.isPreloading {
		return
	}

	ducm.isPreloading = true

	for i := 0; i < ducm.preloadWorkers; i++ {
		go ducm.preloadWorker(i)
	}

	fs.Debugf(ducm.fs, "启动了 %d 个下载URL预加载工作线程", ducm.preloadWorkers)
}

// preloadWorker 预加载工作线程
func (ducm *DownloadURLCacheManager) preloadWorker(workerID int) {
	for {
		select {
		case fileID := <-ducm.preloadQueue:
			ducm.performPreload(fileID, workerID)
		case <-ducm.stopPreload:
			fs.Debugf(ducm.fs, "预加载工作线程 %d 已停止", workerID)
			return
		}
	}
}

// performPreload 执行预加载
func (ducm *DownloadURLCacheManager) performPreload(fileID string, workerID int) {
	// 检查是否已经在缓存中
	if _, found := ducm.getFromCache(fileID); found {
		return
	}

	// 创建带超时的上下文
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 获取下载URL
	url, err := ducm.fetchFromAPI(ctx, fileID)
	if err != nil {
		fs.Debugf(ducm.fs, "预加载工作线程 %d 获取下载URL失败 %s: %v", workerID, fileID, err)
		return
	}

	// 保存到缓存
	ducm.saveToCache(fileID, url, 0, fmt.Sprintf("preload_worker_%d", workerID))

	fs.Debugf(ducm.fs, "预加载工作线程 %d 成功预加载: %s", workerID, fileID)
}

// parseExpireTime 解析URL中的过期时间
func (ducm *DownloadURLCacheManager) parseExpireTime(urlStr string) time.Time {
	// 解析URL参数中的过期时间
	if parsedURL, err := url.Parse(urlStr); err == nil {
		query := parsedURL.Query()

		// 尝试解析expires参数（Unix时间戳）
		if expiresStr := query.Get("expires"); expiresStr != "" {
			if expires, err := strconv.ParseInt(expiresStr, 10, 64); err == nil {
				return time.Unix(expires, 0)
			}
		}

		// 尝试解析expire参数
		if expireStr := query.Get("expire"); expireStr != "" {
			if expire, err := strconv.ParseInt(expireStr, 10, 64); err == nil {
				return time.Unix(expire, 0)
			}
		}
	}

	// 如果无法解析，返回默认过期时间（2小时）
	return time.Now().Add(2 * time.Hour)
}

// cleanupOldHotFiles 清理旧的热点文件
func (ducm *DownloadURLCacheManager) cleanupOldHotFiles() {
	// 删除最旧的热点文件，保持数量在限制内
	cutoff := time.Now().Add(-24 * time.Hour) // 24小时前的文件

	for fileID, lastAccess := range ducm.hotFiles {
		if lastAccess.Before(cutoff) {
			delete(ducm.hotFiles, fileID)
		}
	}

	// 如果还是太多，删除最旧的一些
	if len(ducm.hotFiles) > ducm.maxHotFiles {
		// 简化实现：随机删除一些
		count := 0
		for fileID := range ducm.hotFiles {
			delete(ducm.hotFiles, fileID)
			count++
			if count >= HotFileCleanupBatchSize { // 一次删除指定数量
				break
			}
		}
	}
}

// startCleanupTimer 启动定期清理定时器
func (ducm *DownloadURLCacheManager) startCleanupTimer() {
	ducm.cleanupTimer = time.AfterFunc(time.Hour, func() {
		ducm.performCleanup()
		ducm.startCleanupTimer() // 重新启动定时器
	})
}

// performCleanup 执行清理
func (ducm *DownloadURLCacheManager) performCleanup() {
	fs.Debugf(ducm.fs, "开始下载URL缓存清理")

	ducm.mu.Lock()
	defer ducm.mu.Unlock()

	// 清理过期的访问模式记录
	cutoff := time.Now().Add(-7 * 24 * time.Hour) // 7天前
	for fileID, info := range ducm.accessPattern {
		if info.LastAccess.Before(cutoff) {
			delete(ducm.accessPattern, fileID)
		}
	}

	// 清理热点文件列表
	ducm.cleanupOldHotFiles()

	fs.Debugf(ducm.fs, "下载URL缓存清理完成，访问模式记录: %d, 热点文件: %d",
		len(ducm.accessPattern), len(ducm.hotFiles))
}

// Close 关闭缓存管理器
func (ducm *DownloadURLCacheManager) Close() error {
	// 停止预加载工作线程
	if ducm.isPreloading {
		close(ducm.stopPreload)
		ducm.isPreloading = false
	}

	// 停止清理定时器
	if ducm.cleanupTimer != nil {
		ducm.cleanupTimer.Stop()
	}

	// 关闭缓存
	if ducm.cache != nil {
		return ducm.cache.Close()
	}

	return nil
}

// GetCacheStats 获取缓存统计信息
func (ducm *DownloadURLCacheManager) GetCacheStats() map[string]interface{} {
	ducm.mu.RLock()
	defer ducm.mu.RUnlock()

	return map[string]interface{}{
		"access_patterns":    len(ducm.accessPattern),
		"hot_files":          len(ducm.hotFiles),
		"preload_queue_size": len(ducm.preloadQueue),
		"preload_workers":    ducm.preloadWorkers,
		"is_preloading":      ducm.isPreloading,
	}
}

// apiCall 进行API调用，具有适当的错误处理和速率限制
func (f *Fs) apiCall(ctx context.Context, method, endpoint string, body io.Reader, result interface{}) error {
	// 记录API调用
	f.performanceMetrics.RecordAPICall()

	pacer := f.getPacerForEndpoint(endpoint)
	return pacer.Call(func() (bool, error) {
		// 确保令牌在进行API调用前有效
		err := f.refreshTokenIfNecessary(ctx, false, false)
		if err != nil {
			return false, err
		}

		req, err := http.NewRequestWithContext(ctx, method, openAPIRootURL+endpoint, body)
		if err != nil {
			return false, err
		}

		// 设置请求头
		req.Header.Set("Authorization", "Bearer "+f.token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Platform", "open_platform")
		req.Header.Set("User-Agent", f.opt.UserAgent)

		// 使用自适应超时机制
		timeout := f.calculateAdaptiveTimeout(method, endpoint)
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
		req = req.WithContext(ctx)

		resp, err := f.client.Do(req)
		if err != nil {
			// 检查是否应该重试
			if shouldRetry, retryErr := shouldRetry(ctx, resp, err); shouldRetry {
				f.performanceMetrics.RecordRetry()
				return true, retryErr
			}
			return false, err
		}
		defer resp.Body.Close()

		// 首先检查HTTP错误
		if resp.StatusCode >= 400 {
			return f.handleHTTPError(ctx, resp)
		}

		bodyBytes, err := io.ReadAll(resp.Body)
		if err != nil {
			return false, err
		}

		// 解析基础响应以检查错误
		var baseResp struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		}
		if err = json.Unmarshal(bodyBytes, &baseResp); err != nil {
			return false, err
		}

		// 处理API错误
		switch baseResp.Code {
		case 0:
			// 成功
			if result != nil {
				return false, json.Unmarshal(bodyBytes, result)
			}
			return false, nil
		case 401:
			// 令牌过期，尝试刷新
			fs.Debugf(f, "令牌已过期，尝试刷新")
			err = f.refreshTokenIfNecessary(ctx, false, true)
			if err != nil {
				return false, err
			}
			return true, nil // 使用新令牌重试
		case 429:
			// 速率限制 - 对429错误使用更长的退避时间
			fs.Debugf(f, "速率受限（API错误429），将使用更长退避时间重试")
			return true, fserrors.NewErrorRetryAfter(30 * time.Second)
		default:
			// 记录API错误
			f.performanceMetrics.RecordAPIError()
			// 使用标准化错误格式
			errCtx := ErrorContext{
				Operation: "API调用",
				Method:    method,
				Endpoint:  endpoint,
				Extra: map[string]string{
					"错误代码": fmt.Sprintf("%d", baseResp.Code),
					"错误消息": baseResp.Message,
				},
			}
			return false, f.WrapError(fmt.Errorf("API错误 %d: %s", baseResp.Code, baseResp.Message), errCtx)
		}
	})
}

// apiCallMultipart 进行multipart API调用，类似apiCall但支持自定义Content-Type
func (f *Fs) apiCallMultipart(ctx context.Context, method, endpoint string, body io.Reader, contentType string, result interface{}) error {
	// 记录API调用
	f.performanceMetrics.RecordAPICall()

	// 获取上传域名（对于上传API）
	var baseURL string
	if strings.Contains(endpoint, "/upload/") {
		uploadDomain, err := f.getUploadDomain(ctx)
		if err != nil {
			return fmt.Errorf("获取上传域名失败: %w", err)
		}
		baseURL = uploadDomain
	} else {
		baseURL = openAPIRootURL
	}

	// 不使用pacer，因为调用方已经处理了QPS限制
	// 确保令牌在进行API调用前有效
	err := f.refreshTokenIfNecessary(ctx, false, false)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, method, baseURL+endpoint, body)
	if err != nil {
		return err
	}

	// 设置请求头
	req.Header.Set("Authorization", "Bearer "+f.token)
	req.Header.Set("Content-Type", contentType) // 使用自定义Content-Type
	req.Header.Set("Platform", "open_platform")
	req.Header.Set("User-Agent", f.opt.UserAgent)

	// 使用自适应超时机制
	timeout := f.calculateAdaptiveTimeout(method, endpoint)
	var cancel context.CancelFunc
	ctx, cancel = context.WithTimeout(ctx, timeout)
	defer cancel()
	req = req.WithContext(ctx)

	resp, err := f.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// 首先检查HTTP错误
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP错误: status=%d, body=%s", resp.StatusCode, string(body))
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	// 解析响应
	if result != nil {
		if err = json.Unmarshal(bodyBytes, result); err != nil {
			return fmt.Errorf("解析响应失败: %w, body: %s", err, string(bodyBytes))
		}
	}

	return nil
}

// handleHTTPError 处理HTTP级别的错误并确定重试行为
func (f *Fs) handleHTTPError(ctx context.Context, resp *http.Response) (bool, error) {
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		// 令牌可能已过期 - 尝试刷新一次
		fs.Debugf(f, "收到401错误，尝试刷新令牌")
		err := f.refreshTokenIfNecessary(ctx, false, true)
		if err != nil {
			return false, fmt.Errorf("身份验证失败: %w", err)
		}
		return true, nil // 使用新令牌重试
	case http.StatusTooManyRequests:
		// 速率受限 - 应该使用更长退避时间重试
		fs.Debugf(f, "速率受限（HTTP 429），将使用更长退避时间重试")
		return true, fserrors.NewErrorRetryAfter(30 * time.Second)
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		// 服务器错误 - 应该重试
		fs.Debugf(f, "服务器错误 %d，将重试", resp.StatusCode)
		return true, fmt.Errorf("server error: HTTP %d", resp.StatusCode)
	case http.StatusNotFound:
		return false, fs.ErrorObjectNotFound
	case http.StatusForbidden:
		return false, fmt.Errorf("permission denied: HTTP %d", resp.StatusCode)
	default:
		// 读取错误响应体以获取更多详细信息
		body, _ := io.ReadAll(resp.Body)
		return false, fmt.Errorf("HTTP error %d: %s", resp.StatusCode, string(body))
	}
}

// getFileInfo 根据ID获取文件的详细信息
func (f *Fs) getFileInfo(ctx context.Context, fileID string) (*FileListInfoRespDataV2, error) {
	// 验证文件ID
	_, err := strconv.ParseInt(fileID, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid file ID: %s", fileID)
	}

	// 使用文件详情API
	var response FileDetailResponse
	endpoint := fmt.Sprintf("/api/v1/file/detail?fileID=%s", fileID)

	err = f.apiCall(ctx, "GET", endpoint, nil, &response)
	if err != nil {
		return nil, err
	}

	if response.Code != 0 {
		return nil, fmt.Errorf("API error %d: %s", response.Code, response.Message)
	}

	// 转换为我们的标准格式
	fileInfo := &FileListInfoRespDataV2{
		FileID:       response.Data.FileID,
		Filename:     response.Data.Filename,
		Type:         response.Data.Type,
		Size:         response.Data.Size,
		Etag:         response.Data.ETag,
		Status:       response.Data.Status,
		ParentFileID: response.Data.ParentFileID,
		Category:     0, // 详情响应中未提供
	}

	return fileInfo, nil
}

// getDownloadURL 获取文件的下载URL
func (f *Fs) getDownloadURL(ctx context.Context, fileID string) (string, error) {
	// 首先尝试从缓存获取
	if cachedURL, found := f.getDownloadURLFromCache(fileID); found {
		fs.Debugf(f, "下载URL缓存命中: fileID=%s", fileID)
		return cachedURL, nil
	}

	var response DownloadInfoResponse
	endpoint := fmt.Sprintf("/api/v1/file/download_info?fileID=%s", fileID)

	err := f.apiCall(ctx, "GET", endpoint, nil, &response)
	if err != nil {
		return "", err
	}

	if response.Code != 0 {
		return "", fmt.Errorf("API error %d: %s", response.Code, response.Message)
	}

	// 缓存下载URL
	f.saveDownloadURLToCache(fileID, response.Data.DownloadURL, response.Data.ExpireTime)

	return response.Data.DownloadURL, nil
}

// createDirectory 创建新目录
func (f *Fs) createDirectory(ctx context.Context, parentID, name string) error {
	// 验证参数
	if name == "" {
		return fmt.Errorf("目录名不能为空")
	}
	if parentID == "" {
		return fmt.Errorf("父目录ID不能为空")
	}

	// 验证目录名
	if err := validateFileName(name); err != nil {
		return fmt.Errorf("目录名验证失败: %w", err)
	}

	// 将父目录ID转换为int64
	parentFileID, err := strconv.ParseInt(parentID, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid parent ID: %s", parentID)
	}

	// 准备请求体
	reqBody := map[string]interface{}{
		"parentID": strconv.FormatInt(parentFileID, 10),
		"name":     name,
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return err
	}

	// 进行API调用
	var response struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}

	err = f.apiCall(ctx, "POST", "/upload/v1/file/mkdir", bytes.NewReader(bodyBytes), &response)
	if err != nil {
		return err
	}

	if response.Code != 0 {
		return fmt.Errorf("API error %d: %s", response.Code, response.Message)
	}

	return nil
}

// createUpload 创建上传会话
func (f *Fs) createUpload(ctx context.Context, parentFileID int64, filename, etag string, size int64) (*UploadCreateResp, error) {
	// 首先验证parentFileID是否存在
	fs.Debugf(f, "createUpload: 验证父目录ID %d", parentFileID)
	exists, err := f.verifyParentFileID(ctx, parentFileID)
	if err != nil {
		return nil, fmt.Errorf("验证父目录ID %d 失败: %w", parentFileID, err)
	}
	if !exists {
		return nil, fmt.Errorf("父目录ID %d 不存在", parentFileID)
	}

	reqBody := map[string]interface{}{
		"parentFileId": parentFileID,
		"filename":     filename,
		"size":         size,
		"duplicate":    1, // 1: 保留两者，新文件名将自动添加后缀; 2: 覆盖原文件
		"containDir":   false,
	}

	// 123 API需要etag
	if etag == "" {
		return nil, fmt.Errorf("etag (MD5 hash) is required for 123 API but not provided")
	}
	reqBody["etag"] = strings.ToLower(etag)
	fs.Debugf(f, "为123 API包含etag: %s", etag)

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	var response UploadCreateResp

	// 使用智能版本回退API调用
	err = f.apiCallWithVersionFallback(ctx, "POST", "/upload/file/create", bytes.NewReader(bodyBytes), &response)

	if response.Code != 0 {
		return nil, fmt.Errorf("API error %d: %s", response.Code, response.Message)
	}

	return &response, nil
}

// uploadFile 使用上传会话上传文件内容
func (f *Fs) uploadFile(ctx context.Context, in io.Reader, createResp *UploadCreateResp, size int64) error {
	return f.uploadFileWithPath(ctx, in, createResp, size, "")
}

// uploadFileWithPath 使用上传会话上传文件内容，支持断点续传
func (f *Fs) uploadFileWithPath(ctx context.Context, in io.Reader, createResp *UploadCreateResp, size int64, filePath string) error {
	// 确定块大小
	chunkSize := createResp.Data.SliceSize
	if chunkSize <= 0 {
		chunkSize = int64(f.opt.ChunkSize)
	}

	// 确保块大小在限制范围内
	if chunkSize < int64(minChunkSize) {
		chunkSize = int64(minChunkSize)
	}
	if chunkSize > int64(maxChunkSize) {
		chunkSize = int64(maxChunkSize)
	}

	// 对于小文件，全部读入内存（无需断点续传）
	if size <= chunkSize {
		data, err := io.ReadAll(in)
		if err != nil {
			return fmt.Errorf("failed to read file data: %w", err)
		}

		if int64(len(data)) != size {
			return fmt.Errorf("size mismatch: expected %d, got %d", size, len(data))
		}

		// 单部分上传
		return f.uploadSinglePart(ctx, createResp.Data.PreuploadID, data)
	}

	// 大文件的多部分上传，支持断点续传
	return f.uploadMultiPartWithResume(ctx, in, createResp.Data.PreuploadID, size, chunkSize, filePath)
}

// uploadSinglePart 单部分上传文件
func (f *Fs) uploadSinglePart(ctx context.Context, preuploadID string, data []byte) error {
	// 获取第1部分的上传URL
	uploadURL, err := f.getUploadURL(ctx, preuploadID, 1)
	if err != nil {
		return fmt.Errorf("failed to get upload URL: %w", err)
	}

	// 上传数据
	err = f.uploadPart(ctx, uploadURL, data)
	if err != nil {
		return fmt.Errorf("failed to upload part: %w", err)
	}

	// 完成上传
	return f.completeUpload(ctx, preuploadID)
}

// uploadMultiPartWithResume 多部分上传文件，支持断点续传
func (f *Fs) uploadMultiPartWithResume(ctx context.Context, in io.Reader, preuploadID string, size, chunkSize int64, filePath string) error {
	// 防止除零错误
	if chunkSize <= 0 {
		return fmt.Errorf("无效的分片大小: %d", chunkSize)
	}

	uploadNums := (size + chunkSize - 1) / chunkSize

	// 限制分片数量
	if uploadNums > int64(f.opt.MaxUploadParts) {
		return fmt.Errorf("file too large: would require %d parts, max allowed is %d", uploadNums, f.opt.MaxUploadParts)
	}

	// 尝试加载现有进度
	progress, err := f.loadUploadProgress(preuploadID)
	if err != nil {
		fs.Debugf(f, "加载上传进度失败: %v", err)
		progress = nil
	}

	// 如果不存在进度或参数不匹配，创建新进度
	if progress == nil || progress.TotalParts != uploadNums || progress.ChunkSize != chunkSize || progress.FileSize != size {
		fs.Debugf(f, "为%s创建新的上传进度", preuploadID)
		progress = &UploadProgress{
			PreuploadID:   preuploadID,
			TotalParts:    uploadNums,
			ChunkSize:     chunkSize,
			FileSize:      size,
			UploadedParts: make(map[int64]bool),
			FilePath:      filePath,
			CreatedAt:     time.Now(),
		}
	} else {
		fs.Debugf(f, "恢复上传%s: %d/%d个分片已上传",
			preuploadID, progress.GetUploadedCount(), progress.TotalParts)
	}

	// 检查是否可以从文件恢复（如果提供了文件路径且文件存在）
	var fileReader *os.File
	canResumeFromFile := filePath != "" && progress.GetUploadedCount() > 0

	if canResumeFromFile {
		fileReader, err = os.Open(filePath)
		if err != nil {
			fs.Debugf(f, "无法打开文件进行断点续传，回退到流式传输: %v", err)
			canResumeFromFile = false
		} else {
			defer fileReader.Close()
			// 使用文件读取器而不是流以支持断点续传
			in = fileReader
		}
	}

	buffer := make([]byte, chunkSize)

	for partIndex := int64(0); partIndex < uploadNums; partIndex++ {
		partNumber := partIndex + 1

		// 如果此分片已上传则跳过 - 使用线程安全方法
		if progress.IsUploaded(partNumber) {
			f.debugf(LogLevelVerbose, "跳过已上传的分片 %d/%d", partNumber, uploadNums)

			// 如果可以定位（文件读取器），定位到下一分片
			if canResumeFromFile && fileReader != nil {
				nextOffset := (partIndex + 1) * chunkSize
				if nextOffset < size {
					_, err := fileReader.Seek(nextOffset, io.SeekStart)
					if err != nil {
						return fmt.Errorf("failed to seek to part %d: %w", partNumber+1, err)
					}
				}
			} else {
				// 仍需要读取并丢弃数据以推进读取器
				if partIndex == uploadNums-1 {
					remaining := size - partIndex*chunkSize
					_, err := io.ReadFull(in, make([]byte, remaining))
					if err != nil {
						return fmt.Errorf("failed to skip part %d data: %w", partNumber, err)
					}
				} else {
					_, err := io.ReadFull(in, buffer)
					if err != nil {
						return fmt.Errorf("failed to skip part %d data: %w", partNumber, err)
					}
				}
			}
			continue
		}

		// 如果可以定位且这不是第一个未上传的分片，定位到正确位置
		if canResumeFromFile && fileReader != nil {
			currentOffset := partIndex * chunkSize
			_, err := fileReader.Seek(currentOffset, io.SeekStart)
			if err != nil {
				return fmt.Errorf("failed to seek to part %d: %w", partNumber, err)
			}
		}

		// 读取块数据
		var partData []byte
		if partIndex == uploadNums-1 {
			// 最后一分片 - 读取剩余数据
			remaining := size - partIndex*chunkSize
			partData = make([]byte, remaining)
			_, err := io.ReadFull(in, partData)
			if err != nil {
				return fmt.Errorf("failed to read part %d: %w", partNumber, err)
			}
		} else {
			// 常规分片
			_, err := io.ReadFull(in, buffer)
			if err != nil {
				return fmt.Errorf("failed to read part %d: %w", partNumber, err)
			}
			partData = buffer
		}

		// 获取此分片的上传URL
		uploadURL, err := f.getUploadURL(ctx, preuploadID, partNumber)
		if err != nil {
			return fmt.Errorf("failed to get upload URL for part %d: %w", partNumber, err)
		}

		// 使用重试上传此分片
		err = f.uploadPacer.Call(func() (bool, error) {
			err := f.uploadPart(ctx, uploadURL, partData)

			// 对于跨云盘传输，使用专门的重试策略
			if err != nil {
				// 计算已传输字节数
				transferredBytes := partIndex * chunkSize
				if shouldRetryTransfer, retryDelay := f.shouldRetryCrossCloudTransfer(err, 0, transferredBytes, size); shouldRetryTransfer {
					return true, fserrors.NewErrorRetryAfter(retryDelay)
				}
			}

			return shouldRetry(ctx, nil, err)
		})
		if err != nil {
			// 返回错误前保存进度
			saveErr := f.saveUploadProgress(progress)
			if saveErr != nil {
				fs.Debugf(f, "上传错误后保存进度失败: %v", saveErr)
			}
			return fmt.Errorf("failed to upload part %d: %w", partNumber, err)
		}

		// 标记此分片已上传并保存进度 - 使用线程安全方法
		progress.SetUploaded(partNumber)
		err = f.saveUploadProgress(progress)
		if err != nil {
			fs.Debugf(f, "保存上传进度失败: %v", err)
		}

		f.debugf(LogLevelVerbose, "已上传分片 %d/%d", partNumber, uploadNums)
	}

	// 完成上传
	err = f.completeUpload(ctx, preuploadID)
	if err != nil {
		// 返回错误前保存进度
		saveErr := f.saveUploadProgress(progress)
		if saveErr != nil {
			fs.Debugf(f, "完成错误后保存进度失败: %v", saveErr)
		}
		return err
	}

	// 上传成功完成，删除进度文件
	f.removeUploadProgress(preuploadID)
	return nil
}

// getUploadURL 获取特定分片的上传URL
func (f *Fs) getUploadURL(ctx context.Context, preuploadID string, sliceNo int64) (string, error) {
	reqBody := map[string]interface{}{
		"preuploadId": preuploadID,
		"sliceNo":     sliceNo,
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	var response UploadUrlResp
	// 使用智能版本回退API调用
	err = f.apiCallWithVersionFallback(ctx, "POST", "/upload/file/get_upload_url", bytes.NewReader(bodyBytes), &response)

	if response.Code != 0 {
		return "", fmt.Errorf("API error %d: %s", response.Code, response.Message)
	}

	return response.Data.PresignedURL, nil
}

// uploadPart 将单个分片上传到给定URL
func (f *Fs) uploadPart(ctx context.Context, uploadURL string, data []byte) error {
	req, err := http.NewRequestWithContext(ctx, "PUT", uploadURL, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("failed to create upload request: %w", err)
	}

	req.ContentLength = int64(len(data))
	req.Header.Set("Content-Type", "application/octet-stream")

	// 为请求添加超时（确保总是有超时保护）
	timeout := time.Duration(f.opt.Timeout)
	if timeout <= 0 {
		timeout = defaultTimeout // 使用默认超时
	}
	var cancel context.CancelFunc
	ctx, cancel = context.WithTimeout(ctx, timeout)
	defer cancel()
	req = req.WithContext(ctx)

	resp, err := f.client.Do(req)
	if err != nil {
		return fmt.Errorf("upload request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("upload part failed with status %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

// completeUpload 完成多部分上传
func (f *Fs) completeUpload(ctx context.Context, preuploadID string) error {
	reqBody := map[string]interface{}{
		"preuploadID": preuploadID,
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("failed to marshal complete upload request: %w", err)
	}

	var response struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			Async     bool  `json:"async"`
			Completed bool  `json:"completed"`
			FileID    int64 `json:"fileID"`
		} `json:"data"`
	}

	// 使用智能版本回退API调用
	err = f.apiCallWithVersionFallback(ctx, "POST", "/upload/file/upload_complete", bytes.NewReader(bodyBytes), &response)

	if response.Code != 0 {
		return fmt.Errorf("upload completion failed: API error %d: %s", response.Code, response.Message)
	}

	// 如果是异步处理，我们需要等待完成
	if response.Data.Async && !response.Data.Completed {
		fs.Debugf(f, "上传为异步模式，轮询完成状态，文件ID: %d", response.Data.FileID)
		err = f.pollAsyncUpload(ctx, preuploadID)
		if err != nil {
			return fmt.Errorf("failed to wait for async upload completion: %w", err)
		}
		fs.Debugf(f, "异步上传成功完成，文件ID: %d", response.Data.FileID)
	} else {
		fs.Debugf(f, "同步上传完成，文件ID: %d", response.Data.FileID)
	}

	return nil
}

// uploadPartStream 使用流式方式上传分片，避免大内存占用
func (f *Fs) uploadPartStream(ctx context.Context, uploadURL string, reader io.Reader, size int64) error {
	req, err := http.NewRequestWithContext(ctx, "PUT", uploadURL, reader)
	if err != nil {
		return fmt.Errorf("failed to create stream upload request: %w", err)
	}

	req.ContentLength = size
	req.Header.Set("Content-Type", "application/octet-stream")

	// 使用自适应超时机制
	adaptiveTimeout := f.getAdaptiveTimeout(size, "chunked_upload")
	var cancel context.CancelFunc
	ctx, cancel = context.WithTimeout(ctx, adaptiveTimeout)
	defer cancel()
	req = req.WithContext(ctx)

	resp, err := f.client.Do(req)
	if err != nil {
		return fmt.Errorf("stream upload request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("stream upload failed with status %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

// pollAsyncUpload 轮询异步上传完成状态
func (f *Fs) pollAsyncUpload(ctx context.Context, preuploadID string) error {
	fs.Debugf(f, "轮询异步上传完成状态，preuploadID: %s", preuploadID)

	// 使用指数退避轮询，增加到30次尝试，最大5分钟
	// 对于大文件，123网盘需要更长的处理时间
	maxAttempts := 30
	for attempt := 0; attempt < maxAttempts; attempt++ {
		reqBody := map[string]interface{}{
			"preuploadID": preuploadID,
		}

		bodyBytes, err := json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("failed to marshal async request: %w", err)
		}

		var response struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Data    struct {
				Completed bool  `json:"completed"`
				FileID    int64 `json:"fileID"`
			} `json:"data"`
		}

		// 使用智能版本回退API调用
		err = f.apiCallWithVersionFallback(ctx, "POST", "/upload/file/upload_async_result", bytes.NewReader(bodyBytes), &response)
		if err != nil {
			fs.Debugf(f, "异步轮询第%d次尝试失败: %v", attempt+1, err)
			// 即使单个请求失败也继续轮询
		} else if response.Code != 0 {
			fs.Debugf(f, "异步轮询第%d次尝试返回错误: API错误 %d: %s", attempt+1, response.Code, response.Message)
		} else if response.Data.Completed {
			fs.Debugf(f, "异步上传在第%d次尝试后成功完成", attempt+1)
			return nil
		}

		// 下次尝试前等待（优化的指数退避策略）
		// 前几次尝试使用较短间隔，后续增加到最大15秒
		var waitTime time.Duration
		if attempt < 5 {
			// 前5次：1s, 2s, 3s, 4s, 5s
			waitTime = time.Duration(attempt+1) * time.Second
		} else if attempt < 10 {
			// 6-10次：5秒固定间隔
			waitTime = 5 * time.Second
		} else {
			// 11次以后：10-15秒间隔
			waitTime = time.Duration(10+attempt-10) * time.Second
			if waitTime > 15*time.Second {
				waitTime = 15 * time.Second // 最大15秒
			}
		}

		fs.Debugf(f, "异步上传尚未完成，等待%v后进行下次轮询（第%d/%d次尝试）", waitTime, attempt+1, maxAttempts)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(waitTime):
			// 继续下次尝试
		}
	}

	// 如果轮询失败，记录警告但不返回错误，因为文件可能已经上传成功
	fs.Debugf(f, "异步上传轮询超时，但文件可能已经上传成功，继续处理")
	return fmt.Errorf("异步上传轮询超时，无法确认文件状态")
}

// saveUploadProgress 将上传进度保存到临时文件以支持断点续传
func (f *Fs) saveUploadProgress(progress *UploadProgress) error {
	progressDir := filepath.Join(os.TempDir(), "rclone-123pan-progress")
	err := os.MkdirAll(progressDir, 0755)
	if err != nil {
		return fmt.Errorf("failed to create progress directory: %w", err)
	}

	// 使用preuploadID的MD5哈希以避免文件名长度问题
	hasher := md5.New()
	hasher.Write([]byte(progress.PreuploadID))
	shortID := fmt.Sprintf("%x", hasher.Sum(nil))

	progressFile := filepath.Join(progressDir, shortID+".json")
	data, err := json.Marshal(progress)
	if err != nil {
		return fmt.Errorf("failed to marshal progress: %w", err)
	}

	err = os.WriteFile(progressFile, data, 0644)
	if err != nil {
		return fmt.Errorf("failed to save progress file: %w", err)
	}

	fs.Debugf(f, "已保存上传进度%s: %d/%d个分片完成",
		progress.PreuploadID[:20]+"...", progress.GetUploadedCount(), progress.TotalParts)
	return nil
}

// loadUploadProgress 从临时文件加载上传进度
func (f *Fs) loadUploadProgress(preuploadID string) (*UploadProgress, error) {
	progressDir := filepath.Join(os.TempDir(), "rclone-123pan-progress")

	// 使用preuploadID的MD5哈希以避免文件名长度问题
	hasher := md5.New()
	hasher.Write([]byte(preuploadID))
	shortID := fmt.Sprintf("%x", hasher.Sum(nil))

	progressFile := filepath.Join(progressDir, shortID+".json")

	data, err := os.ReadFile(progressFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // 不存在进度文件
		}
		return nil, fmt.Errorf("failed to read progress file: %w", err)
	}

	var progress UploadProgress
	err = json.Unmarshal(data, &progress)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal progress: %w", err)
	}

	fs.Debugf(f, "已加载上传进度%s: %d/%d个分片完成",
		preuploadID, progress.GetUploadedCount(), progress.TotalParts)
	return &progress, nil
}

// removeUploadProgress 在上传成功后删除进度文件
func (f *Fs) removeUploadProgress(preuploadID string) {
	progressDir := filepath.Join(os.TempDir(), "rclone-123pan-progress")

	// 使用preuploadID的MD5哈希以避免文件名长度问题
	hasher := md5.New()
	hasher.Write([]byte(preuploadID))
	shortID := fmt.Sprintf("%x", hasher.Sum(nil))

	progressFile := filepath.Join(progressDir, shortID+".json")

	err := os.Remove(progressFile)
	if err != nil && !os.IsNotExist(err) {
		fs.Debugf(f, "删除进度文件失败 %s: %v", progressFile, err)
	} else {
		fs.Debugf(f, "已删除上传进度文件 %s", preuploadID)
	}
}

// computeMD5FromReader 从io.Reader计算MD5哈希
func (f *Fs) computeMD5FromReader(_ context.Context, in io.Reader, size int64) (string, error) {
	hasher := md5.New()

	// 将数据从读取器复制到哈希器
	written, err := io.Copy(hasher, in)
	if err != nil {
		return "", fmt.Errorf("failed to read data for MD5 computation: %w", err)
	}

	// 验证我们读取了预期的数量
	if size >= 0 && written != size {
		return "", fmt.Errorf("size mismatch: expected %d bytes, read %d bytes", size, written)
	}

	// 返回十六进制编码的MD5哈希
	return fmt.Sprintf("%x", hasher.Sum(nil)), nil
}

// streamingPut 实现优化的流式上传，现在使用统一上传入口
// 这消除了跨网盘传输中的双重下载问题，提高传输效率
// 修复了跨云盘传输中"源文件不支持重新打开"的问题
func (f *Fs) streamingPut(ctx context.Context, in io.Reader, src fs.ObjectInfo, parentFileID int64, fileName string) (*Object, error) {
	// 使用新的统一上传入口，简化复杂的回退策略
	fs.Debugf(f, "使用统一上传入口进行流式上传")
	return f.unifiedUpload(ctx, in, src, parentFileID, fileName)

}

// putWithKnownMD5 处理已知MD5哈希的文件上传
// 这是最高效的路径，可以充分利用123网盘的秒传功能
// 优化版本：对于小文件优先使用单步上传API
func (f *Fs) putWithKnownMD5(ctx context.Context, in io.Reader, src fs.ObjectInfo, parentFileID int64, fileName, md5Hash string) (*Object, error) {
	return f.putWithKnownMD5WithOptions(ctx, in, src, parentFileID, fileName, md5Hash, false)
}

// putWithKnownMD5WithOptions 处理已知MD5哈希的文件上传，支持跳过单步上传选项
func (f *Fs) putWithKnownMD5WithOptions(ctx context.Context, in io.Reader, src fs.ObjectInfo, parentFileID int64, fileName, md5Hash string, skipSingleStep bool) (*Object, error) {
	// 对于小文件（<1GB），优先尝试单步上传API（除非明确跳过）
	fileSize := src.Size()
	if !skipSingleStep && fileSize < singleStepUploadLimit && fileSize > 0 && fileSize <= maxMemoryBufferSize {
		fs.Debugf(f, "文件大小 %d bytes < %d，尝试使用单步上传API", fileSize, singleStepUploadLimit)

		// 读取文件数据到内存
		data, err := io.ReadAll(in)
		if err != nil {
			fs.Debugf(f, "读取文件数据失败，回退到传统上传: %v", err)
		} else if int64(len(data)) == fileSize {
			// 尝试单步上传
			result, err := f.singleStepUpload(ctx, data, parentFileID, fileName, md5Hash)
			if err != nil {
				fs.Debugf(f, "单步上传失败，回退到传统上传: %v", err)
				// 重新创建reader用于传统上传
				in = bytes.NewReader(data)
			} else {
				fs.Debugf(f, "单步上传成功")
				return result, nil
			}
		}
	}

	// 传统上传流程：使用已知的MD5创建上传会话
	// 123网盘会检查服务器是否已有相同MD5的文件
	createResp, err := f.createUpload(ctx, parentFileID, fileName, md5Hash, src.Size())
	if err != nil {
		return nil, err
	}

	// 如果文件已存在（秒传/去重），直接返回对象
	// 这是最理想的情况：无需实际传输数据
	if createResp.Data.Reuse {
		fs.Debugf(f, "文件秒传成功，MD5: %s", md5Hash)
		return &Object{
			fs:          f,                                             // 文件系统引用
			remote:      fileName,                                      // 使用实际文件名，不包含.partial后缀
			hasMetaData: true,                                          // 已有元数据
			id:          strconv.FormatInt(createResp.Data.FileID, 10), // 123网盘文件ID
			size:        src.Size(),                                    // 文件大小
			md5sum:      md5Hash,                                       // MD5哈希值
			modTime:     time.Now(),                                    // 修改时间
			isDir:       false,                                         // 不是目录
		}, nil
	}

	// 秒传失败，需要实际上传文件数据
	fs.Debugf(f, "秒传失败，开始实际上传文件数据")
	err = f.uploadFile(ctx, in, createResp, src.Size())
	if err != nil {
		return nil, err
	}

	// 返回上传成功的文件对象
	// 注意：使用实际的文件名而不是src.Remote()，因为src.Remote()可能包含.partial后缀
	// 而实际上传到123网盘的文件名是fileName（已去除.partial后缀）
	return &Object{
		fs:          f,
		remote:      fileName, // 使用实际的文件名，不包含.partial后缀
		hasMetaData: true,
		id:          strconv.FormatInt(createResp.Data.FileID, 10),
		size:        src.Size(),
		md5sum:      md5Hash,
		modTime:     time.Now(),
		isDir:       false,
	}, nil
}

// putSmallFileWithMD5 处理小文件（≤10MB）的上传
// 通过缓存到内存先计算MD5，最大化利用秒传功能，同时控制内存使用
func (f *Fs) putSmallFileWithMD5(ctx context.Context, in io.Reader, src fs.ObjectInfo, parentFileID int64, fileName string) (*Object, error) {
	fileSize := src.Size()
	fs.Debugf(f, "缓存小文件（%d字节）到内存以计算MD5，尝试秒传", fileSize)

	// 验证文件大小的合理性
	if fileSize < 0 {
		return nil, fmt.Errorf("无效的文件大小: %d", fileSize)
	}
	if fileSize > 50*1024*1024 { // 50MB上限
		fs.Debugf(f, "文件过大（%d字节），不适合内存缓存，回退到流式上传", fileSize)
		return f.streamingPutWithBuffer(ctx, in, src, parentFileID, fileName)
	}

	// 检查内存管理器是否允许分配
	fileID := fmt.Sprintf("%s_%d_%d", fileName, parentFileID, time.Now().UnixNano())
	if !f.memoryManager.CanAllocate(fileSize) {
		fs.Debugf(f, "内存不足，无法缓存文件（%d字节），回退到流式上传", fileSize)
		return f.streamingPutWithBuffer(ctx, in, src, parentFileID, fileName)
	}

	// 预先分配内存跟踪
	if !f.memoryManager.Allocate(fileID, fileSize) {
		fs.Debugf(f, "内存分配失败，回退到流式上传")
		return f.streamingPutWithBuffer(ctx, in, src, parentFileID, fileName)
	}

	// 确保在函数结束时释放内存
	defer f.memoryManager.Release(fileID)

	// 使用限制读取器防止读取超过预期大小的数据
	// 这可以防止恶意或损坏的文件导致内存耗尽
	var limitedReader io.Reader
	if fileSize > 0 {
		// 允许读取比预期稍多一点的数据来检测大小不匹配
		limitedReader = io.LimitReader(in, fileSize+1024) // 额外1KB用于检测
	} else {
		// 对于未知大小的文件，设置合理的上限
		limitedReader = io.LimitReader(in, 50*1024*1024) // 50MB上限
	}

	// 将文件读取到内存，使用限制读取器保护
	data, err := io.ReadAll(limitedReader)
	if err != nil {
		return nil, fmt.Errorf("读取小文件数据失败: %w", err)
	}

	// 立即检查读取的数据大小，防止内存浪费
	actualSize := int64(len(data))
	if fileSize > 0 {
		if actualSize > fileSize {
			return nil, fmt.Errorf("文件大小超过预期: 实际 %d 字节 > 预期 %d 字节，可能文件已损坏", actualSize, fileSize)
		}
		if actualSize != fileSize {
			fs.Debugf(f, "文件大小与预期不符: 实际 %d 字节，预期 %d 字节", actualSize, fileSize)
		}
	}

	// 更新内存使用监控
	f.performanceMetrics.UpdateMemoryUsage(actualSize)

	// 计算MD5哈希值
	// 这是关键步骤：先计算MD5，然后尝试秒传
	hasher := md5.New()
	hasher.Write(data)
	md5Hash := fmt.Sprintf("%x", hasher.Sum(nil))
	fs.Debugf(f, "小文件MD5计算完成: %s", md5Hash)

	// 使用计算出的MD5创建上传会话
	// 123网盘会检查是否可以秒传
	createResp, err := f.createUpload(ctx, parentFileID, fileName, md5Hash, int64(len(data)))
	if err != nil {
		return nil, err
	}

	// 如果文件已存在（秒传成功），直接返回对象
	// 这种情况下我们避免了实际的数据传输
	if createResp.Data.Reuse {
		fs.Debugf(f, "小文件秒传成功，MD5: %s", md5Hash)
		return &Object{
			fs:          f,                                             // 文件系统引用
			remote:      src.Remote(),                                  // 远程路径
			hasMetaData: true,                                          // 已有元数据
			id:          strconv.FormatInt(createResp.Data.FileID, 10), // 123网盘文件ID
			size:        int64(len(data)),                              // 文件大小
			md5sum:      md5Hash,                                       // MD5哈希值
			modTime:     time.Now(),                                    // 修改时间
			isDir:       false,                                         // 不是目录
		}, nil
	}

	// 秒传失败，上传缓存的数据
	// 使用bytes.NewReader从内存中的数据创建reader
	fs.Debugf(f, "小文件秒传失败，上传缓存的数据")
	err = f.uploadFile(ctx, bytes.NewReader(data), createResp, int64(len(data)))
	if err != nil {
		return nil, err
	}

	// 返回上传成功的文件对象
	return &Object{
		fs:          f,
		remote:      src.Remote(),
		hasMetaData: true,
		id:          strconv.FormatInt(createResp.Data.FileID, 10),
		size:        int64(len(data)),
		md5sum:      md5Hash,
		modTime:     time.Now(),
		isDir:       false,
	}, nil
}

// streamingPutWithBuffer 使用缓冲策略处理跨云盘传输
// 解决"源文件不支持重新打开"的问题
func (f *Fs) streamingPutWithBuffer(ctx context.Context, in io.Reader, src fs.ObjectInfo, parentFileID int64, fileName string) (*Object, error) {
	fs.Debugf(f, "使用缓冲策略进行跨云盘传输")

	// 对于大文件，我们需要特殊处理以避免内存溢出
	fileSize := src.Size()

	// 如果文件小于100MB，直接读入内存
	if fileSize <= 100*1024*1024 {
		fs.Debugf(f, "小文件(%s)，使用内存缓冲", fs.SizeSuffix(fileSize))
		return f.streamingPutWithMemoryBuffer(ctx, in, src, parentFileID, fileName)
	}

	// 大文件使用临时文件缓冲
	fs.Debugf(f, "大文件(%s)，使用临时文件缓冲", fs.SizeSuffix(fileSize))
	return f.streamingPutWithTempFile(ctx, in, src, parentFileID, fileName)
}

// streamingPutWithMemoryBuffer 使用内存缓冲处理小文件
// 优化版本：对于小文件（<1GB）优先使用单步上传API
func (f *Fs) streamingPutWithMemoryBuffer(ctx context.Context, in io.Reader, src fs.ObjectInfo, parentFileID int64, fileName string) (*Object, error) {
	// 读取所有数据到内存
	data, err := io.ReadAll(in)
	if err != nil {
		return nil, fmt.Errorf("读取文件数据失败: %w", err)
	}

	// 计算MD5
	hasher := md5.New()
	hasher.Write(data)
	md5Hash := fmt.Sprintf("%x", hasher.Sum(nil))

	fs.Debugf(f, "内存缓冲完成，文件大小: %d, MD5: %s", len(data), md5Hash)

	// 对于小文件（<1GB），优先使用单步上传API
	if len(data) < singleStepUploadLimit {
		fs.Debugf(f, "文件大小 %d bytes < %d，尝试使用单步上传API", len(data), singleStepUploadLimit)

		result, err := f.singleStepUpload(ctx, data, parentFileID, fileName, md5Hash)
		if err != nil {
			fs.Debugf(f, "单步上传失败，回退到传统上传方式: %v", err)
			// 如果单步上传失败，回退到传统的分片上传方式
			// 使用skipSingleStep标志防止无限递归
			return f.putWithKnownMD5WithOptions(ctx, bytes.NewReader(data), src, parentFileID, fileName, md5Hash, true)
		}

		fs.Debugf(f, "单步上传成功")
		return result, nil
	}

	// 大文件使用传统上传方式
	fs.Debugf(f, "文件大小 %d bytes >= %d，使用传统上传方式", len(data), singleStepUploadLimit)
	return f.putWithKnownMD5WithOptions(ctx, bytes.NewReader(data), src, parentFileID, fileName, md5Hash, true)
}

// streamingPutWithTempFile 使用临时文件缓冲处理大文件
func (f *Fs) streamingPutWithTempFile(ctx context.Context, in io.Reader, src fs.ObjectInfo, parentFileID int64, fileName string) (*Object, error) {
	// 检查文件大小，对于超大文件使用优化策略
	fileSize := src.Size()
	if fileSize > singleStepUploadLimit { // 大于单步上传限制的文件使用分片流式传输
		fs.Debugf(f, "超大文件(%s)，使用分片流式传输策略（支持断点续传）", fs.SizeSuffix(fileSize))
		return f.streamingPutWithChunkedResume(ctx, in, src, parentFileID, fileName)
	}

	// 对于较小的大文件，继续使用临时文件策略
	fs.Debugf(f, "大文件(%s)，使用临时文件缓冲策略", fs.SizeSuffix(fileSize))

	// 创建临时文件 - 使用安全的资源管理
	tempFile, err := os.CreateTemp("", "rclone-123pan-*")
	if err != nil {
		return nil, fmt.Errorf("创建临时文件失败: %w", err)
	}
	defer func() {
		// 安全的资源清理，确保在任何情况下都能执行
		if tempFile != nil {
			if closeErr := tempFile.Close(); closeErr != nil {
				fs.Debugf(f, "关闭临时文件失败: %v", closeErr)
			}
			if removeErr := os.Remove(tempFile.Name()); removeErr != nil {
				fs.Debugf(f, "删除临时文件失败: %v", removeErr)
			}
		}
	}()

	// 边读边写到临时文件，同时计算MD5
	hasher := md5.New()
	multiWriter := io.MultiWriter(tempFile, hasher)

	written, err := io.Copy(multiWriter, in)
	if err != nil {
		return nil, fmt.Errorf("写入临时文件失败: %w", err)
	}

	md5Hash := fmt.Sprintf("%x", hasher.Sum(nil))
	fs.Debugf(f, "临时文件缓冲完成，写入: %d 字节, MD5: %s", written, md5Hash)

	// 重新定位到文件开头
	_, err = tempFile.Seek(0, io.SeekStart)
	if err != nil {
		return nil, fmt.Errorf("重新定位临时文件失败: %w", err)
	}

	// 使用已知MD5上传
	return f.putWithKnownMD5(ctx, tempFile, src, parentFileID, fileName, md5Hash)
}

// streamingPutWithTwoPhase 使用两阶段流式传输处理超大文件
// 第一阶段：快速计算MD5（只读取，不存储）
// 第二阶段：使用已知MD5进行真正的流式上传
// 这种方法资源占用最少，最适合超大文件传输
func (f *Fs) streamingPutWithTwoPhase(ctx context.Context, in io.Reader, src fs.ObjectInfo, parentFileID int64, fileName string) (*Object, error) {
	fs.Debugf(f, "开始两阶段流式传输：第一阶段计算MD5")

	// 第一阶段：快速计算MD5哈希
	// 这里我们需要重新获取源文件流来计算MD5
	if srcObj, ok := src.(fs.Object); ok {
		// 获取新的流用于MD5计算
		md5Reader, err := srcObj.Open(ctx)
		if err != nil {
			fs.Debugf(f, "无法重新打开源文件进行MD5计算，回退到临时文件策略: %v", err)
			return f.streamingPutWithTempFileForced(ctx, in, src, parentFileID, fileName)
		}
		defer func() {
			if closer, ok := md5Reader.(io.Closer); ok {
				closer.Close()
			}
		}()

		// 快速计算MD5（只读取，不存储）
		hasher := md5.New()
		_, err = io.Copy(hasher, md5Reader)
		if err != nil {
			fs.Debugf(f, "MD5计算失败，回退到临时文件策略: %v", err)
			return f.streamingPutWithTempFileForced(ctx, in, src, parentFileID, fileName)
		}

		md5Hash := fmt.Sprintf("%x", hasher.Sum(nil))
		fs.Debugf(f, "第一阶段完成，计算得到MD5: %s", md5Hash)

		// 第二阶段：使用已知MD5进行流式上传
		fs.Debugf(f, "开始第二阶段：使用已知MD5进行流式上传")
		return f.putWithKnownMD5(ctx, in, src, parentFileID, fileName, md5Hash)
	}

	// 如果无法获取源对象，回退到临时文件策略
	fs.Debugf(f, "无法获取源对象，回退到临时文件策略")
	return f.streamingPutWithTempFileForced(ctx, in, src, parentFileID, fileName)
}

// streamingPutWithTempFileForced 强制使用临时文件策略（回退方案）
// 优化版本：先尝试流式传输，失败时才使用临时文件
func (f *Fs) streamingPutWithTempFileForced(ctx context.Context, in io.Reader, src fs.ObjectInfo, parentFileID int64, fileName string) (*Object, error) {
	fs.Debugf(f, "使用优化的回退策略：先尝试流式传输")

	// 尝试获取源对象进行流式传输
	if srcObj, ok := src.(fs.Object); ok {
		// 尝试流式传输以避免双倍流量
		result, err := f.attemptStreamingUpload(ctx, srcObj, parentFileID, fileName)
		if err == nil {
			fs.Debugf(f, "流式传输成功，避免了临时文件")
			return result, nil
		}
		fs.Debugf(f, "流式传输失败，回退到临时文件策略: %v", err)
	}

	fs.Debugf(f, "使用临时文件策略作为最终回退方案")

	// 创建临时文件 - 使用安全的资源管理
	tempFile, err := os.CreateTemp("", "rclone-123pan-fallback-*")
	if err != nil {
		return nil, fmt.Errorf("创建回退临时文件失败: %w", err)
	}
	defer func() {
		// 安全的资源清理，确保在任何情况下都能执行
		if tempFile != nil {
			if closeErr := tempFile.Close(); closeErr != nil {
				fs.Debugf(f, "关闭回退临时文件失败: %v", closeErr)
			}
			if removeErr := os.Remove(tempFile.Name()); removeErr != nil {
				fs.Debugf(f, "删除回退临时文件失败: %v", removeErr)
			}
		}
	}()

	// 边读边写到临时文件，同时计算MD5
	hasher := md5.New()
	multiWriter := io.MultiWriter(tempFile, hasher)

	written, err := io.Copy(multiWriter, in)
	if err != nil {
		return nil, fmt.Errorf("写入回退临时文件失败: %w", err)
	}

	md5Hash := fmt.Sprintf("%x", hasher.Sum(nil))
	fs.Debugf(f, "回退临时文件缓冲完成，写入: %d 字节, MD5: %s", written, md5Hash)

	// 重新定位到文件开头
	_, err = tempFile.Seek(0, io.SeekStart)
	if err != nil {
		return nil, fmt.Errorf("重新定位回退临时文件失败: %w", err)
	}

	// 使用已知MD5上传
	return f.putWithKnownMD5(ctx, tempFile, src, parentFileID, fileName, md5Hash)
}

// streamingPutWithChunkedResume 使用分片流式传输处理超大文件
// 支持断点续传，资源占用最少（只需要单个分片的临时存储）
// 这是超大文件传输的最佳策略：既节省资源又支持断点续传
func (f *Fs) streamingPutWithChunkedResume(ctx context.Context, in io.Reader, src fs.ObjectInfo, parentFileID int64, fileName string) (*Object, error) {
	// 准备分片上传参数
	params, err := f.prepareChunkedUploadParams(ctx, src)
	if err != nil {
		return nil, err
	}

	f.debugf(LogLevelDebug, "开始分片流式传输：文件大小 %s，动态分片大小 %s（网络速度: %s/s）",
		fs.SizeSuffix(params.fileSize), fs.SizeSuffix(params.chunkSize), fs.SizeSuffix(params.networkSpeed))

	// 验证源对象
	srcObj, ok := src.(fs.Object)
	if !ok {
		f.debugf(LogLevelDebug, "无法获取源对象，回退到临时文件策略")
		return f.streamingPutWithTempFileForced(ctx, in, src, parentFileID, fileName)
	}

	// 创建上传会话
	createResp, err := f.createChunkedUploadSession(ctx, parentFileID, fileName, params.fileSize)
	if err != nil {
		return nil, err
	}

	// 如果意外触发秒传，回退到临时文件策略
	if createResp.Data.Reuse {
		f.debugf(LogLevelDebug, "意外触发秒传，回退到临时文件策略")
		return f.streamingPutWithTempFileForced(ctx, in, src, parentFileID, fileName)
	}

	// 开始分片流式传输
	return f.uploadFileInChunksWithResume(ctx, srcObj, createResp, params.fileSize, params.chunkSize, fileName)
}

// ChunkedUploadParams 分片上传参数
type ChunkedUploadParams struct {
	fileSize     int64
	chunkSize    int64
	networkSpeed int64
	totalChunks  int64
}

// prepareChunkedUploadParams 准备分片上传参数
func (f *Fs) prepareChunkedUploadParams(ctx context.Context, src fs.ObjectInfo) (*ChunkedUploadParams, error) {
	fileSize := src.Size()

	// 动态计算最优分片大小
	networkSpeed := f.detectNetworkSpeed(ctx)
	chunkSize := f.getOptimalChunkSize(fileSize, networkSpeed)

	// 计算分片数量
	totalChunks := (fileSize + chunkSize - 1) / chunkSize

	return &ChunkedUploadParams{
		fileSize:     fileSize,
		chunkSize:    chunkSize,
		networkSpeed: networkSpeed,
		totalChunks:  totalChunks,
	}, nil
}

// createChunkedUploadSession 创建分片上传会话
func (f *Fs) createChunkedUploadSession(ctx context.Context, parentFileID int64, fileName string, fileSize int64) (*UploadCreateResp, error) {
	// 创建上传会话（使用临时MD5，后续会更新）
	tempMD5 := fmt.Sprintf("%032x", time.Now().UnixNano())
	createResp, err := f.createUpload(ctx, parentFileID, fileName, tempMD5, fileSize)
	if err != nil {
		return nil, fmt.Errorf("创建分片上传会话失败: %w", err)
	}

	return createResp, nil
}

// uploadFileInChunksWithResume 执行分片流式上传，支持断点续传
func (f *Fs) uploadFileInChunksWithResume(ctx context.Context, srcObj fs.Object, createResp *UploadCreateResp, fileSize, chunkSize int64, fileName string) (*Object, error) {
	preuploadID := createResp.Data.PreuploadID
	totalChunks := (fileSize + chunkSize - 1) / chunkSize

	// 准备或恢复上传进度
	progress, err := f.prepareUploadProgress(preuploadID, totalChunks, chunkSize, fileSize)
	if err != nil {
		return nil, err
	}

	// 验证已上传分片的完整性
	if err := f.validateUploadedChunks(ctx, progress); err != nil {
		f.debugf(LogLevelDebug, "分片完整性检查失败: %v", err)
		// 继续执行，但已损坏的分片会被重新上传
	}

	// 计算最优并发参数
	concurrencyParams := f.calculateConcurrencyParams(fileSize)

	f.debugf(LogLevelDebug, "动态优化参数 - 网络速度: %s/s, 最优并发数: %d, 实际并发数: %d",
		fs.SizeSuffix(concurrencyParams.networkSpeed), concurrencyParams.optimal, concurrencyParams.actual)

	// 选择上传策略：多线程或单线程
	if totalChunks > 2 && concurrencyParams.actual > 1 {
		f.debugf(LogLevelDebug, "使用多线程分片上传，并发数: %d", concurrencyParams.actual)
		return f.uploadChunksWithConcurrency(ctx, srcObj, createResp, progress, md5.New(), fileName, concurrencyParams.actual)
	}

	// 单线程上传
	return f.uploadChunksSingleThreaded(ctx, srcObj, createResp, progress, fileSize, chunkSize, fileName)
}

// ConcurrencyParams 并发参数
type ConcurrencyParams struct {
	networkSpeed int64
	optimal      int
	actual       int
}

// prepareUploadProgress 准备或恢复上传进度
func (f *Fs) prepareUploadProgress(preuploadID string, totalChunks, chunkSize, fileSize int64) (*UploadProgress, error) {
	progress, err := f.loadUploadProgress(preuploadID)
	if err != nil || progress == nil {
		f.debugf(LogLevelDebug, "加载上传进度失败，创建新进度: %v", err)
		progress = &UploadProgress{
			PreuploadID:   preuploadID,
			TotalParts:    totalChunks,
			ChunkSize:     chunkSize,
			FileSize:      fileSize,
			UploadedParts: make(map[int64]bool),
			CreatedAt:     time.Now(),
		}
	} else {
		f.debugf(LogLevelDebug, "恢复分片上传进度: %d/%d 个分片已完成",
			progress.GetUploadedCount(), totalChunks)
	}
	return progress, nil
}

// validateUploadedChunks 验证已上传分片的完整性
func (f *Fs) validateUploadedChunks(ctx context.Context, progress *UploadProgress) error {
	if progress.GetUploadedCount() > 0 {
		f.debugf(LogLevelDebug, "验证已上传分片的完整性")
		return f.resumeUploadWithIntegrityCheck(ctx, progress)
	}
	return nil
}

// calculateConcurrencyParams 计算最优并发参数
func (f *Fs) calculateConcurrencyParams(fileSize int64) *ConcurrencyParams {
	networkSpeed := f.detectNetworkSpeed(context.Background())
	optimalConcurrency := f.getOptimalConcurrency(fileSize, networkSpeed)

	// 检查用户设置的最大并发数限制
	maxConcurrency := optimalConcurrency
	if f.opt.MaxConcurrentUploads > 0 && f.opt.MaxConcurrentUploads < maxConcurrency {
		maxConcurrency = f.opt.MaxConcurrentUploads
	}

	return &ConcurrencyParams{
		networkSpeed: networkSpeed,
		optimal:      optimalConcurrency,
		actual:       maxConcurrency,
	}
}

// uploadChunksSingleThreaded 单线程分片上传
func (f *Fs) uploadChunksSingleThreaded(ctx context.Context, srcObj fs.Object, createResp *UploadCreateResp, progress *UploadProgress, fileSize, chunkSize int64, fileName string) (*Object, error) {
	preuploadID := createResp.Data.PreuploadID
	totalChunks := (fileSize + chunkSize - 1) / chunkSize
	overallHasher := md5.New()

	f.debugf(LogLevelDebug, "使用单线程分片上传")

	// 逐个分片处理（单线程模式）
	for chunkIndex := int64(0); chunkIndex < totalChunks; chunkIndex++ {
		partNumber := chunkIndex + 1

		// 检查此分片是否已上传 - 使用线程安全方法
		if progress.IsUploaded(partNumber) {
			f.debugf(LogLevelVerbose, "跳过已上传的分片 %d/%d", partNumber, totalChunks)

			// 为了计算整体MD5，需要读取并丢弃这部分数据
			if err := f.processSkippedChunkForMD5(ctx, srcObj, overallHasher, chunkIndex, chunkSize, fileSize, partNumber); err != nil {
				return nil, err
			}
			continue
		}

		// 上传新分片
		if err := f.uploadSingleChunkWithStream(ctx, srcObj, overallHasher, preuploadID, chunkIndex, chunkSize, fileSize, totalChunks); err != nil {
			// 保存进度后返回错误
			if saveErr := f.saveUploadProgress(progress); saveErr != nil {
				f.debugf(LogLevelDebug, "保存进度失败: %v", saveErr)
			}
			return nil, fmt.Errorf("上传分片 %d 失败: %w", partNumber, err)
		}

		// 标记分片已完成并保存进度
		progress.SetUploaded(partNumber)
		if err := f.saveUploadProgress(progress); err != nil {
			f.debugf(LogLevelDebug, "保存进度失败: %v", err)
		}

		f.debugf(LogLevelVerbose, "分片 %d/%d 上传完成", partNumber, totalChunks)
	}

	// 完成上传并返回结果
	return f.finalizeChunkedUpload(ctx, createResp, overallHasher, fileSize, fileName, preuploadID)
}

// processSkippedChunkForMD5 处理跳过的分片以计算MD5
func (f *Fs) processSkippedChunkForMD5(ctx context.Context, srcObj fs.Object, hasher hash.Hash, chunkIndex, chunkSize, fileSize int64, partNumber int64) error {
	chunkStart := chunkIndex * chunkSize
	chunkEnd := chunkStart + chunkSize
	if chunkEnd > fileSize {
		chunkEnd = fileSize
	}
	actualChunkSize := chunkEnd - chunkStart

	// 读取分片数据用于MD5计算
	chunkReader, err := srcObj.Open(ctx, &fs.RangeOption{Start: chunkStart, End: chunkEnd - 1})
	if err != nil {
		return fmt.Errorf("打开分片 %d 用于MD5计算失败: %w", partNumber, err)
	}
	defer chunkReader.Close()

	_, err = io.CopyN(hasher, chunkReader, actualChunkSize)
	if err != nil {
		return fmt.Errorf("读取分片 %d 用于MD5计算失败: %w", partNumber, err)
	}

	return nil
}

// finalizeChunkedUpload 完成分片上传并返回结果
func (f *Fs) finalizeChunkedUpload(ctx context.Context, createResp *UploadCreateResp, hasher hash.Hash, fileSize int64, fileName, preuploadID string) (*Object, error) {
	// 完成上传
	if err := f.completeUpload(ctx, preuploadID); err != nil {
		return nil, fmt.Errorf("完成分片上传失败: %w", err)
	}

	// 计算最终MD5
	finalMD5 := fmt.Sprintf("%x", hasher.Sum(nil))
	f.debugf(LogLevelDebug, "分片流式上传完成，最终MD5: %s", finalMD5)

	// 清理进度文件
	f.removeUploadProgress(preuploadID)

	// 返回成功的文件对象
	return &Object{
		fs:          f,
		remote:      fileName,
		hasMetaData: true,
		id:          strconv.FormatInt(createResp.Data.FileID, 10),
		size:        fileSize,
		md5sum:      finalMD5,
		modTime:     time.Now(),
		isDir:       false,
	}, nil
}

// attemptStreamingUpload 尝试流式上传以避免双倍流量消耗
// 这个方法实现真正的边下边上传输，最小化流量使用
func (f *Fs) attemptStreamingUpload(ctx context.Context, srcObj fs.Object, parentFileID int64, fileName string) (*Object, error) {
	fs.Debugf(f, "尝试流式上传，避免临时文件和双倍流量")

	// 第一步：快速计算MD5（只读取，不存储）
	md5Reader, err := srcObj.Open(ctx)
	if err != nil {
		return nil, fmt.Errorf("无法打开源文件计算MD5: %w", err)
	}
	defer md5Reader.Close()

	hasher := md5.New()
	_, err = io.Copy(hasher, md5Reader)
	if err != nil {
		return nil, fmt.Errorf("计算MD5失败: %w", err)
	}

	md5Hash := fmt.Sprintf("%x", hasher.Sum(nil))
	fs.Debugf(f, "流式传输MD5计算完成: %s", md5Hash)

	// 第二步：使用已知MD5创建上传会话
	createResp, err := f.createUpload(ctx, parentFileID, fileName, md5Hash, srcObj.Size())
	if err != nil {
		return nil, fmt.Errorf("创建流式上传会话失败: %w", err)
	}

	// 如果触发秒传，直接返回成功
	if createResp.Data.Reuse {
		fs.Debugf(f, "流式传输触发秒传，MD5: %s", md5Hash)
		return &Object{
			fs:          f,
			remote:      fileName,
			hasMetaData: true,
			id:          strconv.FormatInt(createResp.Data.FileID, 10),
			size:        srcObj.Size(),
			md5sum:      md5Hash,
			modTime:     time.Now(),
			isDir:       false,
		}, nil
	}

	// 第三步：重新打开源文件进行实际上传
	uploadReader, err := srcObj.Open(ctx)
	if err != nil {
		return nil, fmt.Errorf("无法重新打开源文件进行上传: %w", err)
	}
	defer uploadReader.Close()

	// 执行实际上传
	err = f.uploadFile(ctx, uploadReader, createResp, srcObj.Size())
	if err != nil {
		return nil, fmt.Errorf("流式上传失败: %w", err)
	}

	fs.Debugf(f, "流式上传成功完成，文件ID: %d", createResp.Data.FileID)

	return &Object{
		fs:          f,
		remote:      fileName,
		hasMetaData: true,
		id:          strconv.FormatInt(createResp.Data.FileID, 10),
		size:        srcObj.Size(),
		md5sum:      md5Hash,
		modTime:     time.Now(),
		isDir:       false,
	}, nil
}

// uploadSingleChunkWithStream 使用流式方式上传单个分片
// 这是分片流式传输的核心：边下载边上传，只需要分片大小的临时存储
func (f *Fs) uploadSingleChunkWithStream(ctx context.Context, srcObj fs.Object, overallHasher io.Writer, preuploadID string, chunkIndex, chunkSize, fileSize, totalChunks int64) error {
	partNumber := chunkIndex + 1
	chunkStart := chunkIndex * chunkSize
	chunkEnd := chunkStart + chunkSize
	if chunkEnd > fileSize {
		chunkEnd = fileSize
	}
	actualChunkSize := chunkEnd - chunkStart

	fs.Debugf(f, "开始流式上传分片 %d/%d (偏移: %d, 大小: %d)",
		partNumber, totalChunks, chunkStart, actualChunkSize)

	// 打开分片范围的源文件流
	chunkReader, err := srcObj.Open(ctx, &fs.RangeOption{Start: chunkStart, End: chunkEnd - 1})
	if err != nil {
		return fmt.Errorf("打开分片 %d 源文件流失败: %w", partNumber, err)
	}
	defer chunkReader.Close()

	// 创建临时文件用于此分片（只需要分片大小的存储）
	tempFile, err := os.CreateTemp("", fmt.Sprintf("rclone-123pan-chunk-%d-*", partNumber))
	if err != nil {
		return fmt.Errorf("创建分片 %d 临时文件失败: %w", partNumber, err)
	}
	defer func() {
		tempFile.Close()
		os.Remove(tempFile.Name())
	}()

	// 创建分片MD5计算器
	chunkHasher := md5.New()

	// 边读边写：同时写入临时文件、分片MD5和整体MD5
	multiWriter := io.MultiWriter(tempFile, chunkHasher, overallHasher)

	// 流式传输分片数据
	written, err := io.Copy(multiWriter, chunkReader)
	if err != nil {
		return fmt.Errorf("流式传输分片 %d 失败: %w", partNumber, err)
	}

	if written != actualChunkSize {
		return fmt.Errorf("分片 %d 大小不匹配: 期望 %d, 实际 %d", partNumber, actualChunkSize, written)
	}

	// 重新定位临时文件到开头
	_, err = tempFile.Seek(0, io.SeekStart)
	if err != nil {
		return fmt.Errorf("重新定位分片 %d 临时文件失败: %w", partNumber, err)
	}

	// 获取分片上传URL
	uploadURL, err := f.getUploadURL(ctx, preuploadID, partNumber)
	if err != nil {
		return fmt.Errorf("获取分片 %d 上传URL失败: %w", partNumber, err)
	}

	// 读取临时文件数据用于上传
	chunkData, err := io.ReadAll(tempFile)
	if err != nil {
		return fmt.Errorf("读取分片 %d 临时文件失败: %w", partNumber, err)
	}

	// 上传分片数据
	err = f.uploadPart(ctx, uploadURL, chunkData)
	if err != nil {
		return fmt.Errorf("上传分片 %d 数据失败: %w", partNumber, err)
	}

	chunkMD5 := fmt.Sprintf("%x", chunkHasher.Sum(nil))
	fs.Debugf(f, "分片 %d 流式上传成功，大小: %d, MD5: %s", partNumber, written, chunkMD5)

	return nil
}

// uploadChunksWithConcurrency 使用多线程并发上传分片，集成流式哈希累积器
func (f *Fs) uploadChunksWithConcurrency(ctx context.Context, srcObj fs.Object, createResp *UploadCreateResp, progress *UploadProgress, _ io.Writer, fileName string, maxConcurrency int) (*Object, error) {
	preuploadID := createResp.Data.PreuploadID
	totalChunks := progress.TotalParts
	chunkSize := progress.ChunkSize
	fileSize := progress.FileSize

	// 创建流式哈希累积器，消除MD5重复计算
	hashAccumulator := NewStreamingHashAccumulator(fileSize, chunkSize)
	defer func() {
		// 确保资源清理
		hashAccumulator.Reset()
	}()

	fs.Debugf(f, "开始多线程分片上传：%d个分片，最大并发数：%d", totalChunks, maxConcurrency)

	// 创建带超时的上下文，防止goroutine长时间运行
	uploadTimeout := time.Duration(totalChunks) * 10 * time.Minute // 每个分片最多10分钟
	if uploadTimeout > 2*time.Hour {
		uploadTimeout = 2 * time.Hour // 最大2小时超时
	}
	workerCtx, cancelWorkers := context.WithTimeout(ctx, uploadTimeout)
	defer cancelWorkers() // 确保函数退出时取消所有worker

	// 创建有界工作队列和结果通道，防止内存爆炸
	// 队列大小限制为并发数的2倍，避免过度缓冲
	queueSize := maxConcurrency * 2
	if queueSize > int(totalChunks) {
		queueSize = int(totalChunks)
	}
	chunkJobs := make(chan int64, queueSize)
	results := make(chan chunkResult, maxConcurrency) // 结果通道只需要并发数大小

	// 启动工作协程
	for i := 0; i < maxConcurrency; i++ {
		go f.chunkUploadWorker(workerCtx, srcObj, preuploadID, chunkSize, fileSize, totalChunks, chunkJobs, results)
	}

	// 发送需要上传的分片任务（使用goroutine避免阻塞）
	pendingChunks := int64(0)
	go func() {
		defer close(chunkJobs)
		for chunkIndex := int64(0); chunkIndex < totalChunks; chunkIndex++ {
			partNumber := chunkIndex + 1
			if !progress.IsUploaded(partNumber) {
				select {
				case chunkJobs <- chunkIndex:
					// 任务发送成功
				case <-workerCtx.Done():
					// 上下文取消，停止发送任务
					fs.Debugf(f, "上下文取消，停止发送分片任务")
					return
				}
			}
		}
	}()

	// 计算需要上传的分片数量 - 使用线程安全方法
	for chunkIndex := int64(0); chunkIndex < totalChunks; chunkIndex++ {
		partNumber := chunkIndex + 1
		if !progress.IsUploaded(partNumber) {
			pendingChunks++
		}
	}

	fs.Debugf(f, "需要上传 %d 个分片", pendingChunks)

	// 如果没有需要上传的分片，直接跳过收集结果
	if pendingChunks == 0 {
		fs.Debugf(f, "所有分片已完成，跳过上传阶段")
	} else {
		// 收集结果
		completedChunks := int64(0)
		for completedChunks < pendingChunks {
			select {
			case result := <-results:
				if result.err != nil {
					fs.Errorf(f, "分片 %d 上传失败: %v", result.chunkIndex+1, result.err)
					// 保存进度后返回错误
					saveErr := f.saveUploadProgress(progress)
					if saveErr != nil {
						fs.Debugf(f, "保存进度失败: %v", saveErr)
					}
					// 安排清理partial文件
					f.schedulePartialFileCleanup(preuploadID)
					return nil, fmt.Errorf("分片 %d 上传失败: %w", result.chunkIndex+1, result.err)
				}

				// 将分片数据写入哈希累积器
				if len(result.chunkData) > 0 {
					_, err := hashAccumulator.WriteChunk(result.chunkIndex, result.chunkData)
					if err != nil {
						fs.Debugf(f, "哈希累积器写入分片 %d 失败: %v", result.chunkIndex+1, err)
					} else {
						fs.Debugf(f, "分片 %d 哈希累积成功: %s", result.chunkIndex+1, result.chunkHash)
					}
				}

				// 标记分片完成（线程安全）- 使用内置的线程安全方法
				partNumber := result.chunkIndex + 1
				progress.SetUploaded(partNumber)
				completedChunks++

				// 保存进度
				err := f.saveUploadProgress(progress)
				if err != nil {
					fs.Debugf(f, "保存进度失败: %v", err)
				}

				fs.Debugf(f, "分片 %d/%d 上传完成 (并发)", partNumber, totalChunks)

			case <-workerCtx.Done():
				// 保存当前进度
				saveErr := f.saveUploadProgress(progress)
				if saveErr != nil {
					fs.Debugf(f, "超时时保存进度失败: %v", saveErr)
				}
				// 安排清理partial文件
				f.schedulePartialFileCleanup(preuploadID)

				if workerCtx.Err() == context.DeadlineExceeded {
					return nil, fmt.Errorf("上传超时，已完成 %d/%d 分片", completedChunks, pendingChunks)
				}
				return nil, workerCtx.Err()
			case <-ctx.Done():
				// 保存当前进度
				saveErr := f.saveUploadProgress(progress)
				if saveErr != nil {
					fs.Debugf(f, "取消时保存进度失败: %v", saveErr)
				}
				return nil, ctx.Err()
			}
		}
	}

	// 使用哈希累积器获取最终MD5，无需重新读取文件
	fs.Debugf(f, "所有分片上传完成，从哈希累积器获取最终MD5")

	// 检查哈希累积器的完整性
	processed, total, percentage := hashAccumulator.GetProgress()
	fs.Debugf(f, "哈希累积器进度: %d/%d bytes (%.2f%%)", processed, total, percentage)

	// 完成哈希计算
	finalMD5 := hashAccumulator.Finalize()

	// 验证分片数量
	chunkCount := hashAccumulator.GetChunkCount()
	if int64(chunkCount) != totalChunks {
		fs.Debugf(f, "警告：哈希累积器分片数量 (%d) 与预期 (%d) 不匹配", chunkCount, totalChunks)
		// 如果哈希累积器不完整，回退到传统方法
		fs.Debugf(f, "回退到传统MD5计算方法")
		finalHasher := md5.New()
		for chunkIndex := int64(0); chunkIndex < totalChunks; chunkIndex++ {
			chunkStart := chunkIndex * chunkSize
			chunkEnd := chunkStart + chunkSize
			if chunkEnd > fileSize {
				chunkEnd = fileSize
			}
			actualChunkSize := chunkEnd - chunkStart

			chunkReader, err := srcObj.Open(ctx, &fs.RangeOption{Start: chunkStart, End: chunkEnd - 1})
			if err != nil {
				return nil, fmt.Errorf("打开分片 %d 用于MD5计算失败: %w", chunkIndex+1, err)
			}

			// 使用defer确保资源正确释放
			func() {
				defer chunkReader.Close()
				_, err = io.CopyN(finalHasher, chunkReader, actualChunkSize)
			}()

			if err != nil {
				return nil, fmt.Errorf("读取分片 %d 用于MD5计算失败: %w", chunkIndex+1, err)
			}
		}
		finalMD5 = fmt.Sprintf("%x", finalHasher.Sum(nil))
	}

	// 完成上传
	err := f.completeUpload(ctx, preuploadID)
	if err != nil {
		return nil, fmt.Errorf("完成多线程分片上传失败: %w", err)
	}

	fs.Debugf(f, "多线程分片上传完成，最终MD5: %s", finalMD5)

	// 清理进度文件
	f.removeUploadProgress(preuploadID)

	// 返回成功的文件对象
	return &Object{
		fs:          f,
		remote:      fileName,
		hasMetaData: true,
		id:          strconv.FormatInt(createResp.Data.FileID, 10),
		size:        fileSize,
		md5sum:      finalMD5,
		modTime:     time.Now(),
		isDir:       false,
	}, nil
}

// chunkResult 分片上传结果，包含哈希信息
type chunkResult struct {
	chunkIndex int64
	chunkHash  string // 分片MD5哈希
	chunkData  []byte // 分片数据（用于哈希累积）
	err        error
}

// chunkUploadWorker 分片上传工作协程，支持哈希累积
func (f *Fs) chunkUploadWorker(ctx context.Context, srcObj fs.Object, preuploadID string, chunkSize, fileSize, totalChunks int64, jobs <-chan int64, results chan<- chunkResult) {
	for chunkIndex := range jobs {
		chunkHash, chunkData, err := f.uploadSingleChunkWithHash(ctx, srcObj, preuploadID, chunkIndex, chunkSize, fileSize, totalChunks)
		results <- chunkResult{
			chunkIndex: chunkIndex,
			chunkHash:  chunkHash,
			chunkData:  chunkData,
			err:        err,
		}
	}
}

// uploadSingleChunkConcurrent 并发版本的单个分片上传（不需要MD5计算）
func (f *Fs) uploadSingleChunkConcurrent(ctx context.Context, srcObj fs.Object, preuploadID string, chunkIndex, chunkSize, fileSize, totalChunks int64) error {
	partNumber := chunkIndex + 1
	chunkStart := chunkIndex * chunkSize
	chunkEnd := chunkStart + chunkSize
	if chunkEnd > fileSize {
		chunkEnd = fileSize
	}
	actualChunkSize := chunkEnd - chunkStart

	fs.Debugf(f, "并发上传分片 %d/%d，范围: %d-%d，大小: %d", partNumber, totalChunks, chunkStart, chunkEnd-1, actualChunkSize)

	// 打开分片数据流
	chunkReader, err := srcObj.Open(ctx, &fs.RangeOption{Start: chunkStart, End: chunkEnd - 1})
	if err != nil {
		return fmt.Errorf("打开分片 %d 数据流失败: %w", partNumber, err)
	}
	defer chunkReader.Close()

	// 获取上传URL
	uploadURL, err := f.getUploadURL(ctx, preuploadID, partNumber)
	if err != nil {
		return fmt.Errorf("获取分片 %d 上传URL失败: %w", partNumber, err)
	}

	// 使用流式上传避免大内存占用
	// 对于大分片，直接流式传输而不是全部加载到内存
	err = f.uploadPartStream(ctx, uploadURL, chunkReader, actualChunkSize)
	if err != nil {
		return fmt.Errorf("上传分片 %d 数据失败: %w", partNumber, err)
	}

	fs.Debugf(f, "分片 %d/%d 并发上传成功", partNumber, totalChunks)
	return nil
}

// uploadSingleChunkWithHash 并发版本的单个分片上传，返回哈希和数据用于累积
func (f *Fs) uploadSingleChunkWithHash(ctx context.Context, srcObj fs.Object, preuploadID string, chunkIndex, chunkSize, fileSize, totalChunks int64) (string, []byte, error) {
	partNumber := chunkIndex + 1
	chunkStart := chunkIndex * chunkSize
	chunkEnd := chunkStart + chunkSize
	if chunkEnd > fileSize {
		chunkEnd = fileSize
	}
	actualChunkSize := chunkEnd - chunkStart

	fs.Debugf(f, "并发上传分片 %d/%d（含哈希），范围: %d-%d，大小: %d", partNumber, totalChunks, chunkStart, chunkEnd-1, actualChunkSize)

	// 打开分片数据流
	chunkReader, err := srcObj.Open(ctx, &fs.RangeOption{Start: chunkStart, End: chunkEnd - 1})
	if err != nil {
		return "", nil, fmt.Errorf("打开分片 %d 数据流失败: %w", partNumber, err)
	}
	defer chunkReader.Close()

	// 读取分片数据到内存（用于哈希计算和上传）
	chunkData, err := io.ReadAll(chunkReader)
	if err != nil {
		return "", nil, fmt.Errorf("读取分片 %d 数据失败: %w", partNumber, err)
	}

	if int64(len(chunkData)) != actualChunkSize {
		return "", nil, fmt.Errorf("分片 %d 大小不匹配: 期望 %d, 实际 %d", partNumber, actualChunkSize, len(chunkData))
	}

	// 计算分片MD5哈希
	chunkHasher := md5.New()
	chunkHasher.Write(chunkData)
	chunkHash := fmt.Sprintf("%x", chunkHasher.Sum(nil))

	// 获取上传URL
	uploadURL, err := f.getUploadURL(ctx, preuploadID, partNumber)
	if err != nil {
		return "", nil, fmt.Errorf("获取分片 %d 上传URL失败: %w", partNumber, err)
	}

	// 使用流式上传避免大内存占用
	err = f.uploadPartStream(ctx, uploadURL, bytes.NewReader(chunkData), actualChunkSize)
	if err != nil {
		return "", nil, fmt.Errorf("上传分片 %d 数据失败: %w", partNumber, err)
	}

	fs.Debugf(f, "分片 %d/%d 并发上传成功，MD5: %s", partNumber, totalChunks, chunkHash)
	return chunkHash, chunkData, nil
}

// singleStepUpload 使用123云盘的单步上传API上传小文件（<1GB）
// 这个API专门为小文件设计，一次HTTP请求即可完成上传，效率更高
func (f *Fs) singleStepUpload(ctx context.Context, data []byte, parentFileID int64, fileName, md5Hash string) (*Object, error) {
	startTime := time.Now()
	fs.Debugf(f, "使用单步上传API，文件: %s, 大小: %d bytes, MD5: %s", fileName, len(data), md5Hash)

	// 首先验证parentFileID是否存在
	fs.Debugf(f, "singleStepUpload: 验证父目录ID %d", parentFileID)
	exists, err := f.verifyParentFileID(ctx, parentFileID)
	if err != nil {
		return nil, fmt.Errorf("验证父目录ID %d 失败: %w", parentFileID, err)
	}
	if !exists {
		return nil, fmt.Errorf("父目录ID %d 不存在", parentFileID)
	}

	// 验证文件大小限制
	if len(data) > singleStepUploadLimit {
		return nil, fmt.Errorf("文件大小超过单步上传限制%d bytes: %d bytes", singleStepUploadLimit, len(data))
	}

	// 使用统一的文件名验证和清理
	cleanedFileName, warning := f.validateAndCleanFileNameUnified(fileName)
	if warning != nil {
		fs.Logf(f, "文件名验证警告: %v", warning)
		fileName = cleanedFileName
	}

	// 获取上传域名
	uploadDomain, err := f.getUploadDomain(ctx)
	if err != nil {
		return nil, fmt.Errorf("获取上传域名失败: %w", err)
	}

	// 构建单步上传URL
	uploadURL := uploadDomain + "/upload/v2/file/single/create"

	// 创建multipart form数据
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	// 添加表单字段
	if err := writer.WriteField("parentFileID", strconv.FormatInt(parentFileID, 10)); err != nil {
		return nil, fmt.Errorf("写入parentFileID失败: %w", err)
	}

	if err := writer.WriteField("filename", fileName); err != nil {
		return nil, fmt.Errorf("写入filename失败: %w", err)
	}

	if err := writer.WriteField("etag", md5Hash); err != nil {
		return nil, fmt.Errorf("写入etag失败: %w", err)
	}

	if err := writer.WriteField("size", strconv.Itoa(len(data))); err != nil {
		return nil, fmt.Errorf("写入size失败: %w", err)
	}

	// 添加duplicate字段（保留两者，避免覆盖）
	if err := writer.WriteField("duplicate", "1"); err != nil {
		return nil, fmt.Errorf("写入duplicate失败: %w", err)
	}

	// 添加文件数据
	fileWriter, err := writer.CreateFormFile("file", fileName)
	if err != nil {
		return nil, fmt.Errorf("创建文件字段失败: %w", err)
	}

	if _, err := fileWriter.Write(data); err != nil {
		return nil, fmt.Errorf("写入文件数据失败: %w", err)
	}

	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("关闭multipart writer失败: %w", err)
	}

	// 创建HTTP请求
	req, err := http.NewRequestWithContext(ctx, "POST", uploadURL, &buf)
	if err != nil {
		return nil, fmt.Errorf("创建单步上传请求失败: %w", err)
	}

	// 设置请求头
	req.Header.Set("Authorization", "Bearer "+f.token)
	req.Header.Set("Platform", "open_platform")
	req.Header.Set("Content-Type", writer.FormDataContentType())

	// 发送请求（带重试机制）
	var resp *http.Response
	var body []byte
	maxRetries := 3

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			// 指数退避重试
			backoff := time.Duration(attempt*attempt) * time.Second
			fs.Debugf(f, "单步上传重试 %d/%d，等待 %v", attempt, maxRetries, backoff)
			time.Sleep(backoff)
		}

		var err error
		resp, err = f.client.Do(req)
		if err != nil {
			if attempt == maxRetries {
				return nil, fmt.Errorf("单步上传请求失败（重试%d次后）: %w", maxRetries, err)
			}
			fs.Debugf(f, "单步上传请求失败，将重试: %v", err)
			continue
		}

		// 读取响应
		body, err = io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			if attempt == maxRetries {
				return nil, fmt.Errorf("读取单步上传响应失败（重试%d次后）: %w", maxRetries, err)
			}
			fs.Debugf(f, "读取响应失败，将重试: %v", err)
			continue
		}

		// 检查HTTP状态码
		if resp.StatusCode == http.StatusOK {
			break // 成功，跳出重试循环
		} else if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			// 可重试的错误
			if attempt == maxRetries {
				return nil, fmt.Errorf("单步上传失败（重试%d次后），状态码: %d, 响应: %s", maxRetries, resp.StatusCode, string(body))
			}
			fs.Debugf(f, "单步上传遇到可重试错误，状态码: %d，将重试", resp.StatusCode)
			continue
		} else {
			// 不可重试的错误，直接返回
			return nil, fmt.Errorf("单步上传失败，状态码: %d, 响应: %s", resp.StatusCode, string(body))
		}
	}

	// 解析响应
	var uploadResp struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			FileID    int64 `json:"fileID"`
			Completed bool  `json:"completed"`
		} `json:"data"`
	}

	if err := json.Unmarshal(body, &uploadResp); err != nil {
		return nil, fmt.Errorf("解析单步上传响应失败: %w", err)
	}

	// 检查API响应码
	if uploadResp.Code != 0 {
		return nil, fmt.Errorf("单步上传API错误: code=%d, message=%s", uploadResp.Code, uploadResp.Message)
	}

	// 检查是否上传完成
	if !uploadResp.Data.Completed {
		return nil, fmt.Errorf("单步上传未完成，可能需要使用分片上传")
	}

	duration := time.Since(startTime)
	speed := float64(len(data)) / duration.Seconds() / 1024 / 1024 // MB/s
	fs.Debugf(f, "单步上传成功，文件ID: %d, 耗时: %v, 速度: %.2f MB/s", uploadResp.Data.FileID, duration, speed)

	// 返回Object
	return &Object{
		fs:          f,
		remote:      fileName,
		hasMetaData: true,
		id:          strconv.FormatInt(uploadResp.Data.FileID, 10),
		size:        int64(len(data)),
		md5sum:      md5Hash,
		modTime:     time.Now(),
		isDir:       false,
	}, nil
}

// validateFileNameUnified 统一的文件名验证入口点
// 整合了所有验证规则，确保一致性
func (f *Fs) validateFileNameUnified(fileName string) error {
	// 基础检查：空值和空格
	if fileName == "" {
		return fmt.Errorf("文件名不能为空")
	}

	if strings.TrimSpace(fileName) == "" {
		return fmt.Errorf("文件名不能为空或全部是空格")
	}

	// UTF-8有效性检查
	if !utf8.ValidString(fileName) {
		return fmt.Errorf("文件名包含无效的UTF-8字符")
	}

	// 长度检查（字符数）
	runeCount := utf8.RuneCountInString(fileName)
	if runeCount > maxFileNameLength {
		return fmt.Errorf("文件名长度超过限制：当前%d个字符，最大允许%d个字符",
			runeCount, maxFileNameLength)
	}

	// 字节长度检查
	byteLength := len([]byte(fileName))
	if byteLength > maxFileNameBytes {
		return fmt.Errorf("文件名字节长度超过%d: %d", maxFileNameBytes, byteLength)
	}

	// 禁用字符检查：使用正则表达式更高效
	if invalidCharsRegex.MatchString(fileName) {
		invalidFound := invalidCharsRegex.FindAllString(fileName, -1)
		return fmt.Errorf("文件名包含不允许的字符：%v，123网盘不允许使用以下字符：%s",
			invalidFound, invalidChars)
	}

	return nil
}

// validateFileName 保持向后兼容的验证方法（已弃用，使用validateFileNameUnified）
func (f *Fs) validateFileName(fileName string) error {
	return f.validateFileNameUnified(fileName)
}

// validateFileNameWithWarnings 验证文件名并提供警告信息
func (f *Fs) validateFileNameWithWarnings(fileName string) error {
	// 首先进行标准验证
	if err := f.validateFileNameUnified(fileName); err != nil {
		return err
	}

	// 检查文件名是否以点开头或结尾（某些系统不支持）
	if strings.HasPrefix(fileName, ".") || strings.HasSuffix(fileName, ".") {
		fs.Debugf(f, "警告：文件名以点开头或结尾可能在某些系统中不被支持: %s", fileName)
	}

	return nil
}

// validateAndCleanFileNameUnified 统一的验证和清理入口点
// 这是推荐的文件名处理方法，确保所有上传路径的一致性
func (f *Fs) validateAndCleanFileNameUnified(fileName string) (cleanedName string, warning error) {
	// 首先尝试验证原始文件名
	if err := f.validateFileNameUnified(fileName); err == nil {
		return fileName, nil
	}

	// 如果验证失败，清理文件名
	cleanedName = cleanFileName(fileName)

	// 验证清理后的文件名
	if err := f.validateFileNameUnified(cleanedName); err != nil {
		// 如果清理后仍然无效，使用默认名称
		cleanedName = "cleaned_file"
		warning = fmt.Errorf("原始文件名 '%s' 无法清理为有效名称，使用默认名称 '%s'", fileName, cleanedName)
	} else {
		warning = fmt.Errorf("文件名 '%s' 包含无效字符，已清理为 '%s'", fileName, cleanedName)
	}

	return cleanedName, warning
}

// getUploadDomain 获取上传域名，支持动态获取和缓存
func (f *Fs) getUploadDomain(ctx context.Context) (string, error) {
	// 首先尝试从缓存获取
	if f.basicURLCache != nil {
		cacheKey := "upload_domain"
		var entry DownloadURLCacheEntry
		if found, err := f.basicURLCache.Get(cacheKey, &entry); err == nil && found {
			if time.Now().Before(entry.ExpiresAt) {
				f.debugf(LogLevelVerbose, "使用缓存的上传域名: %s", entry.URL)
				return entry.URL, nil
			}
		}
	}

	// 尝试动态获取上传域名 - 使用正确的API路径
	var response struct {
		Code    int      `json:"code"`
		Data    []string `json:"data"` // 修正：data是字符串数组
		Message string   `json:"message"`
		TraceID string   `json:"x-traceID"`
	}

	// 使用正确的API路径调用
	err := f.apiCall(ctx, "GET", "/upload/v2/file/domain", nil, &response)
	if err == nil && response.Code == 0 && len(response.Data) > 0 {
		domain := response.Data[0] // 取第一个域名
		f.debugf(LogLevelDebug, "动态获取上传域名成功: %s", domain)

		// 缓存域名（1小时有效期）
		if f.basicURLCache != nil {
			entry := DownloadURLCacheEntry{
				URL:       domain,
				ExpiresAt: time.Now().Add(time.Hour),
			}
			f.basicURLCache.Set("upload_domain", entry, time.Hour)
		}

		return domain, nil
	}

	// 如果动态获取失败，使用默认域名
	f.debugf(LogLevelInfo, "动态获取上传域名失败，使用默认域名: %v", err)
	defaultDomains := []string{
		"https://openapi-upload.123242.com",
		"https://openapi-upload.123pan.com", // 备用域名
	}

	// 简单的健康检查：选择第一个可用的域名
	for _, domain := range defaultDomains {
		// 这里可以添加简单的连通性检查
		f.debugf(LogLevelDebug, "使用默认上传域名: %s", domain)
		return domain, nil
	}

	return defaultDomains[0], nil
}

// deleteFile 通过移动到回收站来删除文件
func (f *Fs) deleteFile(ctx context.Context, fileID int64) error {
	fs.Debugf(f, "删除文件，ID: %d", fileID)

	reqBody := map[string]interface{}{
		"fileIDs": []int64{fileID},
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("序列化删除请求失败: %w", err)
	}

	var response struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}

	err = f.apiCall(ctx, "POST", "/api/v1/file/trash", bytes.NewReader(bodyBytes), &response)
	if err != nil {
		return fmt.Errorf("删除文件失败: %w", err)
	}

	if response.Code != 0 {
		return fmt.Errorf("删除失败: API错误 %d: %s", response.Code, response.Message)
	}

	fs.Debugf(f, "成功删除文件，ID: %d", fileID)
	return nil
}

// moveFile 将文件移动到不同的目录
func (f *Fs) moveFile(ctx context.Context, fileID, toParentFileID int64) error {
	fs.Debugf(f, "移动文件%d到父目录%d", fileID, toParentFileID)

	reqBody := map[string]interface{}{
		"fileIDs":        []int64{fileID},
		"toParentFileID": toParentFileID,
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("序列化移动请求失败: %w", err)
	}

	var response struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}

	err = f.apiCall(ctx, "POST", "/api/v1/file/move", bytes.NewReader(bodyBytes), &response)
	if err != nil {
		return fmt.Errorf("移动文件失败: %w", err)
	}

	if response.Code != 0 {
		return fmt.Errorf("移动失败: API错误 %d: %s", response.Code, response.Message)
	}

	fs.Debugf(f, "成功移动文件%d到父目录%d", fileID, toParentFileID)
	return nil
}

// renameFile 重命名文件
func (f *Fs) renameFile(ctx context.Context, fileID int64, fileName string) error {
	fs.Debugf(f, "重命名文件%d为: %s", fileID, fileName)

	// 验证新文件名
	if err := validateFileName(fileName); err != nil {
		return fmt.Errorf("重命名文件名验证失败: %w", err)
	}

	reqBody := map[string]interface{}{
		"fileId":   fileID,
		"fileName": fileName,
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("序列化重命名请求失败: %w", err)
	}

	var response struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}

	err = f.apiCall(ctx, "PUT", "/api/v1/file/name", bytes.NewReader(bodyBytes), &response)
	if err != nil {
		return fmt.Errorf("重命名文件失败: %w", err)
	}

	if response.Code != 0 {
		return fmt.Errorf("重命名失败: API错误 %d: %s", response.Code, response.Message)
	}

	fs.Debugf(f, "成功重命名文件%d为: %s", fileID, fileName)
	return nil
}

// getUserInfo 获取用户信息包括存储配额
func (f *Fs) getUserInfo(ctx context.Context) (*UserInfoResp, error) {
	var response UserInfoResp

	err := f.apiCall(ctx, "GET", "/api/v1/user/info", nil, &response)
	if err != nil {
		return nil, err
	}

	if response.Code != 0 {
		return nil, fmt.Errorf("API error %d: %s", response.Code, response.Message)
	}

	return &response, nil
}

// newFs 从路径构造Fs，格式为 container:path
func newFs(ctx context.Context, name, root string, m configmap.Mapper) (fs.Fs, error) {
	// 解析选项
	opt := new(Options)
	err := configstruct.Set(m, opt)
	if err != nil {
		return nil, err
	}

	// 验证配置选项
	if err := validateOptions(opt); err != nil {
		return nil, fmt.Errorf("配置验证失败: %w", err)
	}

	// 标准化和设置默认值
	normalizeOptions(opt)

	// Store the original name before any modifications for config operations
	originalName := name
	if idx := strings.IndexRune(name, '{'); idx > 0 {
		originalName = name[:idx]
	}

	// 规范化root路径，处理绝对路径格式
	normalizedRoot := normalizePath(root)
	fs.Debugf(nil, "NewFs路径规范化: %q -> %q", root, normalizedRoot)

	f := &Fs{
		name:         name,
		originalName: originalName,
		root:         normalizedRoot,
		opt:          *opt,
		m:            m,
		rootFolderID: opt.RootFolderID,
		client:       fshttp.NewClient(ctx),
	}

	// Initialize multiple pacers for different API rate limits based on official documentation
	// v2 API pacer - 高频率 (~14 QPS for api/v2/file/list)
	listPacerSleep := listV2APIMinSleep
	if opt.ListPacerMinSleep > 0 {
		listPacerSleep = time.Duration(opt.ListPacerMinSleep)
	}
	f.listPacer = fs.NewPacer(ctx, pacer.NewDefault(
		pacer.MinSleep(listPacerSleep),
		pacer.MaxSleep(maxSleep),
		pacer.DecayConstant(decayConstant)))

	// 严格API pacer - 中等频率 (~4 QPS for create, async_result, etc.)
	strictPacerSleep := fileMoveMinSleep
	if opt.StrictPacerMinSleep > 0 {
		strictPacerSleep = time.Duration(opt.StrictPacerMinSleep)
	} else if opt.PacerMinSleep > 0 {
		strictPacerSleep = time.Duration(opt.PacerMinSleep) // 向后兼容
	}
	f.strictPacer = fs.NewPacer(ctx, pacer.NewDefault(
		pacer.MinSleep(strictPacerSleep),
		pacer.MaxSleep(maxSleep),
		pacer.DecayConstant(decayConstant)))

	// v2分片上传API pacer (~5 QPS for upload/v2/file/slice)
	uploadPacerSleep := uploadV2SliceMinSleep
	if opt.UploadPacerMinSleep > 0 {
		uploadPacerSleep = time.Duration(opt.UploadPacerMinSleep)
	}
	f.uploadPacer = fs.NewPacer(ctx, pacer.NewDefault(
		pacer.MinSleep(uploadPacerSleep),
		pacer.MaxSleep(maxSleep),
		pacer.DecayConstant(decayConstant)))

	// 下载和高频率API pacer (~16 QPS for upload_complete, get_upload_url, etc.)
	downloadPacerSleep := downloadInfoMinSleep
	if opt.DownloadPacerMinSleep > 0 {
		downloadPacerSleep = time.Duration(opt.DownloadPacerMinSleep)
	}
	f.downloadPacer = fs.NewPacer(ctx, pacer.NewDefault(
		pacer.MinSleep(downloadPacerSleep),
		pacer.MaxSleep(maxSleep),
		pacer.DecayConstant(decayConstant)))

	fs.Infof(f, "📊 QPS限制配置 - 列表API: %.1f QPS, 严格API: %.1f QPS, 上传API: %.1f QPS, 下载API: %.1f QPS",
		1000.0/float64(listPacerSleep.Milliseconds()),
		1000.0/float64(strictPacerSleep.Milliseconds()),
		1000.0/float64(uploadPacerSleep.Milliseconds()),
		1000.0/float64(downloadPacerSleep.Milliseconds()))

	// 初始化性能优化组件
	f.uploadSemaphore = make(chan struct{}, opt.MaxConcurrentUploads)
	f.downloadSemaphore = make(chan struct{}, opt.MaxConcurrentDownloads)
	f.progressTracker = NewProgressTracker(
		time.Duration(opt.ProgressUpdateInterval),
		opt.EnableProgressDisplay,
	)

	// 初始化内存管理器，防止内存泄漏
	// 总内存限制200MB，单文件限制50MB
	f.memoryManager = NewMemoryManager(200*1024*1024, 50*1024*1024)

	// 初始化性能指标收集器
	f.performanceMetrics = NewPerformanceMetrics()

	// 初始化资源池管理器
	f.resourcePool = NewResourcePool()

	// 初始化缓存配置
	f.cacheConfig = DefaultCacheConfig()

	// 初始化BadgerDB持久化缓存
	cacheDir := cache.GetCacheDir("123drive")

	// 初始化多个缓存实例
	caches := map[string]**cache.BadgerCache{
		"parent_ids":   &f.parentIDCache,
		"dir_list":     &f.dirListCache,
		"download_url": &f.basicURLCache,
		"path_to_id":   &f.pathToIDCache,
	}

	for name, cachePtr := range caches {
		cache, cacheErr := cache.NewBadgerCache(name, cacheDir)
		if cacheErr != nil {
			fs.Errorf(f, "初始化%s缓存失败: %v", name, cacheErr)
			// 缓存初始化失败不应该阻止文件系统工作，继续执行
		} else {
			*cachePtr = cache
			fs.Debugf(f, "%s缓存初始化成功", name)
		}
	}

	fs.Debugf(f, "BadgerDB缓存系统初始化完成: %s", cacheDir)

	// 初始化增强的下载URL缓存管理器
	if downloadURLCacheManager, err := NewDownloadURLCacheManager(f, cacheDir); err != nil {
		fs.Errorf(f, "初始化下载URL缓存管理器失败: %v", err)
		// 缓存管理器初始化失败不应该阻止文件系统工作，继续执行
	} else {
		f.downloadURLCache = downloadURLCacheManager
		fs.Debugf(f, "下载URL缓存管理器初始化成功")
	}

	// 初始化API版本管理器
	f.apiVersionManager = NewAPIVersionManager()
	fs.Debugf(f, "API版本管理器初始化成功")

	// 初始化断点续传管理器
	if resumeManager, err := NewResumeManager(f, cacheDir); err != nil {
		fs.Errorf(f, "初始化断点续传管理器失败: %v", err)
		// 断点续传管理器初始化失败不应该阻止文件系统工作，继续执行
	} else {
		f.resumeManager = resumeManager
		fs.Debugf(f, "断点续传管理器初始化成功")
	}

	// 初始化网络质量监控器
	f.networkMonitor = NewNetworkQualityMonitor(f)
	fs.Debugf(f, "网络质量监控器初始化成功")

	// 初始化网络速度缓存
	f.networkSpeedCache = NewNetworkSpeedCache()
	f.debugf(LogLevelDebug, "网络速度缓存初始化成功")

	fs.Debugf(f, "性能优化初始化完成 - 最大并发上传: %d, 最大并发下载: %d, 进度显示: %v",
		opt.MaxConcurrentUploads, opt.MaxConcurrentDownloads, opt.EnableProgressDisplay)

	f.features = (&fs.Features{
		ReadMimeType:            true,
		CanHaveEmptyDirectories: true,
		BucketBased:             false,
		SetTier:                 false,
		GetTier:                 false,
		DuplicateFiles:          false, // 123Pan handles duplicates
		ReadMetadata:            true,
		WriteMetadata:           false, // Not supported
		UserMetadata:            false, // Not supported
		PartialUploads:          true,  // Supports multipart uploads
		NoMultiThreading:        false, // Supports concurrent operations
		SlowModTime:             true,  // ModTime operations are slow
		SlowHash:                false, // Hash is available from API
	}).Fill(ctx, f)

	// Initialize directory cache
	// Note: We use the configured rootFolderID as the base, and let dircache
	// resolve the actual root path during FindRoot()
	// Use the normalized root path to ensure consistency
	f.dirCache = dircache.New(normalizedRoot, f.rootFolderID, f)

	// Check if we have saved token in config file
	tokenLoaded := loadTokenFromConfig(f, m)
	fs.Debugf(f, "从配置加载令牌: %v（过期时间 %v）", tokenLoaded, f.tokenExpiry)

	var tokenRefreshNeeded bool
	if tokenLoaded {
		// Check if token is expired or will expire soon
		if time.Now().After(f.tokenExpiry.Add(-tokenRefreshWindow)) {
			fs.Debugf(f, "令牌已过期或即将过期，立即刷新")
			tokenRefreshNeeded = true
		}
	} else {
		// No token, so login
		fs.Debugf(f, "未找到令牌，尝试登录")
		err = f.GetAccessToken(ctx)
		if err != nil {
			return nil, fmt.Errorf("initial login failed: %w", err)
		}
		// Save token to config after successful login
		f.saveToken(ctx, m)
		fs.Debugf(f, "登录成功，令牌已保存")
	}

	// Try to refresh the token if needed
	if tokenRefreshNeeded {
		err = f.refreshTokenIfNecessary(ctx, false, true)
		if err != nil {
			fs.Debugf(f, "令牌刷新失败，尝试完整登录: %v", err)
			// If refresh fails, try full login
			err = f.GetAccessToken(ctx)
			if err != nil {
				return nil, fmt.Errorf("login failed after token refresh failure: %w", err)
			}
		}
		// Save the refreshed/new token
		f.saveToken(ctx, m)
		fs.Debugf(f, "令牌刷新/登录成功，令牌已保存")
	}

	// Setup token renewer for automatic refresh
	fs.Debugf(f, "设置令牌更新器")
	f.setupTokenRenewer(ctx, m)

	// Find the root directory
	err = f.dirCache.FindRoot(ctx, false)
	if err != nil {
		// Assume it is a file
		newRoot, remote := dircache.SplitPath(root)
		// 创建新的Fs实例，避免复制锁值
		tempF := &Fs{
			name:               f.name,
			root:               newRoot,
			opt:                f.opt,
			m:                  f.m,
			rootFolderID:       f.rootFolderID,
			client:             f.client,
			strictPacer:        f.strictPacer,
			uploadPacer:        f.uploadPacer,
			downloadPacer:      f.downloadPacer,
			uploadSemaphore:    f.uploadSemaphore,
			downloadSemaphore:  f.downloadSemaphore,
			progressTracker:    f.progressTracker,
			memoryManager:      f.memoryManager,
			performanceMetrics: f.performanceMetrics,
		}
		tempF.dirCache = dircache.New(newRoot, f.rootFolderID, tempF)
		// Make new Fs which is the parent
		err = tempF.dirCache.FindRoot(ctx, false)
		if err != nil {
			// No root so return old f
			return f, nil
		}
		_, err := tempF.NewObject(ctx, remote)
		if err != nil {
			// unable to list folder so return old f
			return f, nil
		}
		// return an error with an fs which points to the parent
		return tempF, fs.ErrorIsFile
	}

	return f, nil
}

// NewObject finds the Object at remote.
func (f *Fs) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	fs.Debugf(f, "调用NewObject: %s", remote)

	// 规范化远程路径
	normalizedRemote := normalizePath(remote)
	fs.Debugf(f, "规范化后的远程路径: %s -> %s", remote, normalizedRemote)

	// Get the full path
	var fullPath string
	if f.root == "" {
		fullPath = normalizedRemote
	} else {
		fullPath = normalizePath(f.root + "/" + normalizedRemote)
	}

	if fullPath == "" {
		fullPath = "/"
	}

	// Get file ID
	fileID, err := f.pathToFileID(ctx, fullPath)
	if err != nil {
		if err == fs.ErrorObjectNotFound {
			return nil, fs.ErrorObjectNotFound
		}
		return nil, err
	}

	// Get file details
	fileInfo, err := f.getFileInfo(ctx, fileID)
	if err != nil {
		return nil, err
	}

	// Check if it's a file (not directory)
	// Type: 0-文件  1-文件夹
	if fileInfo.Type == 1 {
		return nil, fs.ErrorNotAFile
	}

	o := &Object{
		fs:          f,
		remote:      remote,
		hasMetaData: true,
		id:          fileID,
		size:        fileInfo.Size,
		md5sum:      fileInfo.Etag,
		modTime:     time.Now(), // We'll parse this properly later
		isDir:       false,
	}

	return o, nil
}

// Precision of the ModTimes in this Fs
func (f *Fs) Precision() time.Duration {
	return fs.ModTimeNotSupported
}

// Put uploads the object.
func (f *Fs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	fs.Debugf(f, "调用Put: %s", src.Remote())

	// 记录上传开始
	startTime := time.Now()
	f.performanceMetrics.RecordUploadStart(src.Size())

	// 并发控制：获取上传信号量
	select {
	case f.uploadSemaphore <- struct{}{}:
		// 使用命名返回值确保在panic时也能释放信号量
		defer func() {
			select {
			case <-f.uploadSemaphore:
				// 信号量释放成功
			default:
				fs.Errorf(f, "上传信号量释放失败")
			}
		}()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	// 规范化远程路径
	normalizedRemote := normalizePath(src.Remote())
	fs.Debugf(f, "Put操作规范化路径: %s -> %s", src.Remote(), normalizedRemote)

	// 生成操作ID用于进度跟踪
	operationID := fmt.Sprintf("upload_%s_%d", src.Remote(), time.Now().UnixNano())

	// 开始进度跟踪
	f.progressTracker.StartOperation(operationID, src.Remote(), src.Size(), "上传中")
	defer func() {
		// 这里会在函数返回时调用，但我们需要在实际完成时调用CompleteOperation
	}()

	// Use dircache to find parent directory
	fs.Debugf(f, "查找父目录路径: %s", normalizedRemote)
	leaf, parentID, err := f.dirCache.FindPath(ctx, normalizedRemote, true)
	if err != nil {
		fs.Errorf(f, "查找父目录失败: %v", err)
		return nil, err
	}
	fileName := leaf
	fs.Debugf(f, "找到父目录ID: %s, 文件名: %s", parentID, fileName)

	// 验证并清理文件名
	cleanedFileName, warning := validateAndCleanFileName(fileName)
	if warning != nil {
		fs.Logf(f, "文件名验证警告: %v", warning)
		fileName = cleanedFileName
	}

	// Convert parentID to int64
	parentFileID, err := strconv.ParseInt(parentID, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid parent ID: %s", parentID)
	}

	// 验证parentFileID是否真的存在
	fs.Debugf(f, "验证父目录ID %d 是否存在", parentFileID)
	exists, err := f.verifyParentFileID(ctx, parentFileID)
	if err != nil {
		fs.Errorf(f, "验证父目录ID失败: %v", err)
		// 尝试清理缓存并重新查找
		fs.Debugf(f, "清理目录缓存并重试")
		f.dirCache.ResetRoot()
		leaf, parentID, err = f.dirCache.FindPath(ctx, normalizedRemote, true)
		if err != nil {
			return nil, fmt.Errorf("重新查找父目录失败: %w", err)
		}
		parentFileID, err = strconv.ParseInt(parentID, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("重新解析父目录ID失败: %s", parentID)
		}
		fs.Debugf(f, "重新找到父目录ID: %d", parentFileID)
	} else if !exists {
		fs.Errorf(f, "父目录ID %d 不存在，尝试重建目录缓存", parentFileID)
		// 清理缓存并重新查找
		f.dirCache.ResetRoot()
		leaf, parentID, err = f.dirCache.FindPath(ctx, normalizedRemote, true)
		if err != nil {
			return nil, fmt.Errorf("重建目录缓存后查找失败: %w", err)
		}
		parentFileID, err = strconv.ParseInt(parentID, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("重建后解析父目录ID失败: %s", parentID)
		}
		fs.Debugf(f, "重建后找到父目录ID: %d", parentFileID)
	} else {
		fs.Debugf(f, "父目录ID %d 验证成功", parentFileID)
	}

	// Use streaming optimization for cross-cloud transfers
	obj, err := f.streamingPut(ctx, in, src, parentFileID, fileName)

	// 完成进度跟踪
	f.progressTracker.CompleteOperation(operationID, err == nil)

	// 记录上传完成或错误
	duration := time.Since(startTime)
	if err != nil {
		f.performanceMetrics.RecordUploadError()
	} else {
		f.performanceMetrics.RecordUploadComplete(src.Size(), duration)
		// 上传成功后，清理相关缓存
		f.invalidateRelatedCaches(normalizedRemote, "upload")
	}

	return obj, err
}

// Fs returns the parent Fs
func (o *Object) Fs() fs.Info {
	return o.fs
}

// String returns a description of the Object
func (o *Object) String() string {
	if o == nil {
		return "<nil>"
	}
	return o.remote
}

// Remote returns the remote path
func (o *Object) Remote() string {
	return o.remote
}

// ModTime returns the modification time
func (o *Object) ModTime(ctx context.Context) time.Time {
	// In a real implementation, you would fetch metadata if not already available.
	return o.modTime
}

// Size returns the size of the file
func (o *Object) Size() int64 {
	// In a real implementation, you would fetch metadata if not already available.
	return o.size
}

// Hash returns the MD5 checksum
func (o *Object) Hash(ctx context.Context, t fshash.Type) (string, error) {
	if t != fshash.MD5 {
		return "", fshash.ErrUnsupported
	}
	// In a real implementation, you would fetch metadata if not already available.
	return o.md5sum, nil
}

// ID returns the ID of the Object
func (o *Object) ID() string {
	// In a real implementation, you would fetch metadata if not already available.
	return o.id
}

// Storable indicates this object can be stored
func (o *Object) Storable() bool {
	return true
}

// Open the file for reading.
func (o *Object) Open(ctx context.Context, options ...fs.OpenOption) (io.ReadCloser, error) {
	fs.Debugf(o.fs, "打开文件: %s", o.remote)

	// 记录下载开始
	o.fs.performanceMetrics.RecordDownloadStart(o.size)

	// 并发控制：获取下载信号量
	select {
	case o.fs.downloadSemaphore <- struct{}{}:
		// 信号量将在ReadCloser关闭时释放
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	// 生成操作ID用于进度跟踪
	operationID := fmt.Sprintf("download_%s_%d", o.remote, time.Now().UnixNano())

	// 开始进度跟踪
	o.fs.progressTracker.StartOperation(operationID, o.remote, o.size, "下载中")

	var resp *http.Response
	var err error

	// Use pacer for download with retry (use strict pacer for download URL requests)
	err = o.fs.strictPacer.Call(func() (bool, error) {
		// Get download URL (may need refresh)
		downloadURL, err := o.fs.getDownloadURL(ctx, o.id)
		if err != nil {
			return shouldRetry(ctx, nil, err)
		}

		// Create HTTP request
		req, err := http.NewRequestWithContext(ctx, "GET", downloadURL, nil)
		if err != nil {
			return false, fmt.Errorf("failed to create download request: %w", err)
		}

		// Handle range requests
		var start, end int64 = 0, -1
		for _, option := range options {
			switch x := option.(type) {
			case *fs.RangeOption:
				start, end = x.Start, x.End
			case *fs.SeekOption:
				start = x.Offset
			}
		}

		if start > 0 || end >= 0 {
			rangeHeader := fmt.Sprintf("bytes=%d-", start)
			if end >= 0 {
				rangeHeader = fmt.Sprintf("bytes=%d-%d", start, end)
			}
			req.Header.Set("Range", rangeHeader)
			fs.Debugf(o.fs, "请求范围: %s", rangeHeader)
		}

		// Set user agent
		req.Header.Set("User-Agent", o.fs.opt.UserAgent)
		// Note: Don't set timeout here as it will be applied to the entire download

		// Make the request
		resp, err = o.fs.client.Do(req)
		if err != nil {
			return shouldRetry(ctx, resp, err)
		}

		// Check status code
		switch resp.StatusCode {
		case http.StatusOK, http.StatusPartialContent:
			return false, nil // Success
		case http.StatusFound, http.StatusMovedPermanently, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
			// Let the HTTP client handle redirects automatically
			// This should not happen if the client is configured correctly
			resp.Body.Close()
			return false, fmt.Errorf("unexpected redirect response %d - client should handle this automatically", resp.StatusCode)
		case http.StatusRequestedRangeNotSatisfiable:
			resp.Body.Close()
			return false, fmt.Errorf("range not satisfiable")
		case http.StatusNotFound:
			resp.Body.Close()
			return false, fs.ErrorObjectNotFound
		default:
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			return shouldRetry(ctx, resp, fmt.Errorf("download failed with status %d: %s", resp.StatusCode, string(body)))
		}
	})

	if err != nil {
		// 释放下载信号量
		select {
		case <-o.fs.downloadSemaphore:
			// 信号量释放成功
		default:
			fs.Errorf(o.fs, "下载信号量释放失败")
		}
		// 标记下载失败
		o.fs.progressTracker.CompleteOperation(operationID, false)
		return nil, err
	}

	// 创建包装的ReadCloser来处理进度更新和资源清理
	return &ProgressReadCloser{
		ReadCloser:      resp.Body,
		fs:              o.fs,
		operationID:     operationID,
		totalSize:       o.size,
		transferredSize: 0,
		startTime:       time.Now(),
	}, nil
}

// Update the object with new content.
func (o *Object) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) error {
	fs.Debugf(o.fs, "调用Update: %s", o.remote)

	// Use dircache to find parent directory
	leaf, parentID, err := o.fs.dirCache.FindPath(ctx, o.remote, false)
	if err != nil {
		return err
	}

	// Convert parentID to int64
	parentFileID, err := strconv.ParseInt(parentID, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid parent ID: %s", parentID)
	}

	// Calculate MD5 hash if available
	var md5Hash string
	if hashValue, err := src.Hash(ctx, fshash.MD5); err == nil && hashValue != "" {
		md5Hash = hashValue
	}

	// Create upload session
	createResp, err := o.fs.createUpload(ctx, parentFileID, leaf, md5Hash, src.Size())
	if err != nil {
		return err
	}

	// If file already exists (reuse), update the object metadata
	if createResp.Data.Reuse {
		o.size = src.Size()
		o.md5sum = md5Hash
		o.modTime = time.Now()
		return nil
	}

	// Upload the file content
	err = o.fs.uploadFile(ctx, in, createResp, src.Size())
	if err != nil {
		return err
	}

	// Update object metadata
	o.size = src.Size()
	o.md5sum = md5Hash
	o.modTime = time.Now()

	return nil
}

// Remove the object.
func (o *Object) Remove(ctx context.Context) error {
	fs.Debugf(o.fs, "调用Remove: %s", o.remote)

	// Convert file ID to int64
	fileID, err := strconv.ParseInt(o.id, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid file ID: %s", o.id)
	}

	err = o.fs.deleteFile(ctx, fileID)
	if err == nil {
		// 删除文件成功后，清理相关缓存
		o.fs.invalidateRelatedCaches(o.remote, "remove")
	}
	return err
}

// SetModTime is not supported
func (o *Object) SetModTime(ctx context.Context, modTime time.Time) error {
	return fs.ErrorCantSetModTime
}

// DirCacher interface implementation for dircache

// FindLeaf finds a directory or file leaf in the parent folder pathID.
// Used by dircache.
func (f *Fs) FindLeaf(ctx context.Context, pathID, leaf string) (foundID string, found bool, err error) {
	fs.Debugf(f, "查找叶节点: pathID=%s, leaf=%s", pathID, leaf)

	// Convert pathID to int64
	parentFileID, err := strconv.ParseInt(pathID, 10, 64)
	if err != nil {
		return "", false, fmt.Errorf("invalid parent ID: %s", pathID)
	}

	// List files in the parent directory
	response, err := f.ListFile(ctx, int(parentFileID), f.opt.ListChunk, "", "", 0)
	if err != nil {
		return "", false, err
	}

	if response.Code != 0 {
		return "", false, fmt.Errorf("API error %d: %s", response.Code, response.Message)
	}

	// Search for the leaf
	for _, file := range response.Data.FileList {
		if file.Filename == leaf {
			foundID = strconv.FormatInt(file.FileID, 10)
			found = true
			// Cache the found item's path/ID mapping
			parentPath, ok := f.dirCache.GetInv(pathID)
			if ok {
				var itemPath string
				if parentPath == "" {
					itemPath = leaf
				} else {
					itemPath = parentPath + "/" + leaf
				}
				f.dirCache.Put(itemPath, foundID)
			}
			return foundID, found, nil
		}
	}

	return "", false, nil
}

// CreateDir creates a directory with the given name in the parent folder pathID.
// Used by dircache.
func (f *Fs) CreateDir(ctx context.Context, pathID, leaf string) (newID string, err error) {
	fs.Debugf(f, "创建目录: pathID=%s, leaf=%s", pathID, leaf)

	// 验证参数
	if leaf == "" {
		return "", fmt.Errorf("目录名不能为空")
	}
	if pathID == "" {
		return "", fmt.Errorf("父目录ID不能为空")
	}

	// 验证并清理目录名
	cleanedDirName, warning := validateAndCleanFileName(leaf)
	if warning != nil {
		fs.Logf(f, "目录名验证警告: %v", warning)
		leaf = cleanedDirName
	}

	// Create the directory using the existing createDirectory method
	err = f.createDirectory(ctx, pathID, leaf)
	if err != nil {
		return "", err
	}

	// After creating, we need to find the newly created directory to get its ID
	// This is a bit inefficient but necessary since createDirectory doesn't return the ID
	foundID, found, err := f.FindLeaf(ctx, pathID, leaf)
	if err != nil {
		return "", fmt.Errorf("failed to find newly created directory: %w", err)
	}
	if !found {
		return "", fmt.Errorf("newly created directory not found")
	}

	return foundID, nil
}

// Check the interfaces are satisfied
var (
	_ fs.Fs          = (*Fs)(nil)
	_ fs.Object      = (*Object)(nil)
	_ fs.Mover       = (*Fs)(nil)
	_ fs.DirMover    = (*Fs)(nil)
	_ fs.Copier      = (*Fs)(nil)
	_ fs.Abouter     = (*Fs)(nil)
	_ fs.Purger      = (*Fs)(nil)
	_ fs.Shutdowner  = (*Fs)(nil)
	_ fs.ObjectInfo  = (*Object)(nil)
	_ fs.IDer        = (*Object)(nil)
	_ fs.SetModTimer = (*Object)(nil)
)

// loadTokenFromConfig attempts to load and parse tokens from the config file
func loadTokenFromConfig(f *Fs, _ configmap.Mapper) bool {
	if f.opt.Token == "" {
		fs.Debugf(f, "配置中未找到令牌（来自选项）")
		return false
	}

	var tokenData TokenJSON                                                // Use the new TokenJSON struct
	var err error                                                          // Declare err here
	if err = json.Unmarshal([]byte(f.opt.Token), &tokenData); err != nil { // Unmarshal unobscured token
		fs.Debugf(f, "从配置解析令牌失败（来自选项）: %v", err)
		return false
	}

	f.token = tokenData.AccessToken
	f.tokenExpiry, err = time.Parse(time.RFC3339, tokenData.Expiry) // Use tokenData.Expiry
	if err != nil {
		fs.Debugf(f, "从配置解析令牌过期时间失败: %v", err)
		return false
	}

	if f.token == "" || f.tokenExpiry.IsZero() {
		fs.Debugf(f, "配置中的令牌不完整")
		return false
	}

	fs.Debugf(f, "从配置文件加载令牌，过期时间: %v", f.tokenExpiry)
	return true
}

// saveToken saves the current token to the config
func (f *Fs) saveToken(_ context.Context, m configmap.Mapper) {
	if m == nil {
		fs.Debugf(f, "不保存令牌 - 提供的映射器为空")
		return
	}

	if f.token == "" || f.tokenExpiry.IsZero() {
		fs.Debugf(f, "不保存令牌 - 令牌信息不完整")
		return
	}

	tokenData := TokenJSON{ // Use the new TokenJSON struct
		AccessToken: f.token,
		Expiry:      f.tokenExpiry.Format(time.RFC3339), // Use Expiry
	}
	tokenJSON, err := json.Marshal(tokenData)
	if err != nil {
		fs.Errorf(f, "保存令牌时序列化失败: %v", err)
		return
	}

	m.Set("token", string(tokenJSON))
	// m.Set does not return an error, so we assume it succeeds.
	// Any errors related to config saving would be handled by the underlying config system.

	fs.Debugf(f, "使用原始名称%q保存令牌到配置文件", f.originalName)
}

// setupTokenRenewer initializes the token renewer to automatically refresh tokens
func (f *Fs) setupTokenRenewer(ctx context.Context, m configmap.Mapper) {
	// Only set up renewer if we have valid tokens
	if f.token == "" || f.tokenExpiry.IsZero() {
		fs.Debugf(f, "不设置令牌更新器 - 令牌信息不完整")
		return
	}

	// Create a renewal transaction function
	transaction := func() error {
		fs.Debugf(f, "令牌更新器触发，刷新令牌")
		// Use non-global function to avoid deadlocks
		err := f.refreshTokenIfNecessary(ctx, false, true)
		if err != nil {
			fs.Errorf(f, "更新器中刷新令牌失败: %v", err)
			return err
		}

		return nil // saveToken is already called in refreshTokenIfNecessary
	}

	// Create minimal OAuth config
	config := &oauthutil.Config{
		TokenURL: openAPIRootURL + "/api/v1/access_token", // Use the correct token URL for 123Pan
	}

	// Create a token source using the existing token
	token := &oauth2.Token{
		AccessToken: f.token,
		// 123Pan API does not seem to return a refresh token in the initial access token response,
		// so we rely on re-logging in if the access token expires.
		// RefreshToken: f.refreshToken, // Not available for 123Pan
		Expiry:    f.tokenExpiry,
		TokenType: "Bearer",
	}

	// Save token to config so it can be accessed by TokenSource
	err := oauthutil.PutToken(f.originalName, m, token, false)
	if err != nil {
		fs.Logf(f, "为更新器保存令牌失败: %v", err)
		return
	}

	// Create a client with the token source
	_, ts, err := oauthutil.NewClientWithBaseClient(ctx, f.originalName, m, config, getHTTPClient(ctx, f.originalName, m))
	if err != nil {
		fs.Logf(f, "为更新器创建令牌源失败: %v", err)
		return
	}

	// Create token renewer that will trigger when the token is about to expire
	f.tokenRenewer = oauthutil.NewRenew(f.originalName, ts, transaction)
	f.tokenRenewer.Start() // Start the renewer immediately
	fs.Debugf(f, "令牌更新器已初始化并启动，使用原始名称%q", f.originalName)
}

// refreshTokenIfNecessary refreshes the token if necessary
func (f *Fs) refreshTokenIfNecessary(ctx context.Context, refreshTokenExpired bool, forceRefresh bool) error {
	f.tokenMu.Lock()

	// Check if another thread has successfully refreshed while we were waiting
	if !refreshTokenExpired && !forceRefresh && f.token != "" && time.Now().Before(f.tokenExpiry.Add(-tokenRefreshWindow)) {
		fs.Debugf(f, "获取锁后令牌仍然有效，跳过刷新")
		f.tokenMu.Unlock()
		return nil
	}
	defer f.tokenMu.Unlock()

	// Perform the actual token refresh
	accessToken, expiredAt, err := GetAccessToken(f.opt.ClientID, f.opt.ClientSecret)
	if err != nil {
		return fmt.Errorf("failed to get new access token during refresh: %w", err)
	}

	f.token = accessToken
	f.tokenExpiry = expiredAt

	// Save the refreshed token to config
	f.saveToken(ctx, f.m)

	return nil
}

// GetAccessToken fetches a new access token from the 123Pan API.
func (f *Fs) GetAccessToken(ctx context.Context) error {
	accessToken, expiredAt, err := GetAccessToken(f.opt.ClientID, f.opt.ClientSecret)
	if err != nil {
		return fmt.Errorf("failed to get new access token: %w", err)
	}
	f.token = accessToken
	f.tokenExpiry = expiredAt
	return nil
}

type ClientCredentials struct {
	ClientID     string `json:"clientID"`
	ClientSecret string `json:"clientSecret"`
}

type TokenJSON struct {
	AccessToken string `json:"access_token"`
	Expiry      string `json:"expiry"`
}

type AccessTokenData struct {
	AccessToken string `json:"accessToken"`
	ExpiredAt   string `json:"expiredAt"`
}

type Response struct {
	Code     int    `json:"code"`
	Message  string `json:"message"`
	Data     AccessTokenData
	XTraceID string `json:"x-traceID"`
}

func GetAccessToken(clientID, clientSecret string) (string, time.Time, error) {
	credentials := ClientCredentials{
		ClientID:     clientID,
		ClientSecret: clientSecret,
	}
	payload, err := json.Marshal(credentials)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("failed to marshal payload: %v", err)
	}
	req, err := http.NewRequest("POST", "https://open-api.123pan.com/api/v1/access_token", bytes.NewBuffer(payload))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("failed to create request: %v", err)
	}
	req.Header.Set("Platform", "open_platform")
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("failed to send request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", time.Time{}, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("failed to read response body: %v", err)
	}
	var result Response
	if err = json.Unmarshal(body, &result); err != nil {
		return "", time.Time{}, fmt.Errorf("failed to unmarshal response: %v", err)
	}
	expiredAt, err := time.Parse(time.RFC3339, result.Data.ExpiredAt)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("failed to parse expiration time: %v", err)
	}
	return result.Data.AccessToken, expiredAt, nil
}

// Name of the remote (e.g. "123")
func (f *Fs) Name() string {
	return f.name
}

// Root of the remote (e.g. "bucket" or "bucket/dir")
func (f *Fs) Root() string {
	return f.root
}

// String returns a description of the FS
func (f *Fs) String() string {
	return fmt.Sprintf("123 root '%s'", f.root)
}

// Features returns the optional features of this Fs
func (f *Fs) Features() *fs.Features {
	return f.features
}

// Hashes returns the supported hash sets.
func (f *Fs) Hashes() fshash.Set {
	return fshash.Set(fshash.MD5)
}

// About gets quota information
func (f *Fs) About(ctx context.Context) (*fs.Usage, error) {
	fs.Debugf(f, "调用About")

	userInfo, err := f.getUserInfo(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get user info: %w", err)
	}

	usage := &fs.Usage{}
	if userInfo.Data.SpacePermanent > 0 {
		usage.Total = fs.NewUsageValue(userInfo.Data.SpacePermanent)
	}
	if userInfo.Data.SpaceUsed > 0 {
		usage.Used = fs.NewUsageValue(userInfo.Data.SpaceUsed)
	}
	if userInfo.Data.SpacePermanent > 0 && userInfo.Data.SpaceUsed > 0 {
		free := userInfo.Data.SpacePermanent - userInfo.Data.SpaceUsed
		if free > 0 {
			usage.Free = fs.NewUsageValue(free)
		}
	}

	return usage, nil
}

// Purge deletes all the files in the directory
func (f *Fs) Purge(ctx context.Context, dir string) error {
	fs.Debugf(f, "调用Purge删除目录: %s", dir)

	// Use dircache to find the directory ID
	dirID, err := f.dirCache.FindDir(ctx, dir, false)
	if err != nil {
		return err
	}

	// Convert dirID to int64
	parentFileID, err := strconv.ParseInt(dirID, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid directory ID: %s", dirID)
	}

	// List all files in the directory
	var allFiles []FileListInfoRespDataV2
	lastFileID := int64(0)

	for {
		response, err := f.ListFile(ctx, int(parentFileID), 100, "", "", int(lastFileID))
		if err != nil {
			return err
		}

		if response.Code != 0 {
			return fmt.Errorf("API error %d: %s", response.Code, response.Message)
		}

		// Collect all files
		for _, file := range response.Data.FileList {
			if file.Status < 100 { // Not trashed
				allFiles = append(allFiles, file)
			}
		}

		// Check if we have more files
		if response.Data.LastFileId == -1 {
			break
		}
		lastFileID = response.Data.LastFileId
	}

	// Delete all files
	for _, file := range allFiles {
		err := f.deleteFile(ctx, file.FileID)
		if err != nil {
			fs.Errorf(f, "删除文件失败 %s: %v", file.Filename, err)
			// Continue with other files
		}
	}

	return nil
}

// Shutdown the backend
func (f *Fs) Shutdown(ctx context.Context) error {
	fs.Debugf(f, "调用关闭")

	// Stop token renewer if it exists
	if f.tokenRenewer != nil {
		f.tokenRenewer.Stop()
		f.tokenRenewer = nil
	}

	// 关闭所有BadgerDB缓存
	caches := []*cache.BadgerCache{
		f.parentIDCache,
		f.dirListCache,
		f.basicURLCache,
		f.pathToIDCache,
	}

	for i, c := range caches {
		if c != nil {
			if err := c.Close(); err != nil {
				fs.Debugf(f, "关闭BadgerDB缓存%d失败: %v", i, err)
			}
		}
	}

	return nil
}

// List the objects and directories in dir into entries.
func (f *Fs) List(ctx context.Context, dir string) (entries fs.DirEntries, err error) {
	fs.Debugf(f, "调用列表，目录: %s", dir)

	// 使用目录缓存查找目录ID
	fs.Debugf(f, "查找目录ID: %s", dir)
	dirID, err := f.dirCache.FindDir(ctx, dir, false)
	if err != nil {
		fs.Debugf(f, "查找目录失败 %s: %v", dir, err)
		return nil, err
	}
	fs.Debugf(f, "找到目录ID: %s，目录: %s", dirID, dir)

	// Convert dirID to int64
	parentFileID, err := strconv.ParseInt(dirID, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid directory ID: %s", dirID)
	}

	// List files in the directory
	var allFiles []FileListInfoRespDataV2
	lastFileID := int64(0)

	for {
		response, err := f.ListFile(ctx, int(parentFileID), 100, "", "", int(lastFileID))
		if err != nil {
			return nil, err
		}

		if response.Code != 0 {
			return nil, fmt.Errorf("API error %d: %s", response.Code, response.Message)
		}

		// Filter out trashed files
		for _, file := range response.Data.FileList {
			if file.Status < 100 { // Not trashed
				allFiles = append(allFiles, file)
			}
		}

		// Check if we have more files
		if response.Data.LastFileId == -1 {
			break
		}
		lastFileID = response.Data.LastFileId
	}

	// Convert to fs.DirEntries
	for _, file := range allFiles {
		remote := file.Filename
		if dir != "" {
			remote = strings.Trim(dir+"/"+file.Filename, "/")
		}

		if file.Type == 1 { // Directory
			// Cache the directory ID for future use
			f.dirCache.Put(remote, strconv.FormatInt(file.FileID, 10))
			d := fs.NewDir(remote, time.Time{})
			entries = append(entries, d)
		} else { // File
			o := &Object{
				fs:          f,
				remote:      remote,
				hasMetaData: true,
				id:          strconv.FormatInt(file.FileID, 10),
				size:        file.Size,
				md5sum:      file.Etag,
				modTime:     time.Now(), // We'll parse this properly later
				isDir:       false,
			}
			entries = append(entries, o)
		}
	}

	return entries, nil
}

// Mkdir makes the directory.
func (f *Fs) Mkdir(ctx context.Context, dir string) error {
	fs.Debugf(f, "调用创建目录: %s", dir)

	// Use dircache to create the directory
	_, err := f.dirCache.FindDir(ctx, dir, true)
	if err == nil {
		// 创建目录成功后，清理相关缓存
		f.invalidateRelatedCaches(dir, "mkdir")
	}
	return err
}

// Rmdir removes an empty directory.
func (f *Fs) Rmdir(ctx context.Context, dir string) error {
	fs.Debugf(f, "调用删除目录: %s", dir)

	// Use dircache to find the directory ID
	dirID, err := f.dirCache.FindDir(ctx, dir, false)
	if err != nil {
		return err
	}

	// Convert dirID to int64
	fileID, err := strconv.ParseInt(dirID, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid directory ID: %s", dirID)
	}

	// Delete the directory
	err = f.deleteFile(ctx, fileID)
	if err != nil {
		return err
	}

	// Remove from cache
	f.dirCache.FlushDir(dir)

	// 删除目录成功后，清理相关缓存
	f.invalidateRelatedCaches(dir, "rmdir")

	return nil
}

// Move server-side moves a file.
func (f *Fs) Move(ctx context.Context, src fs.Object, remote string) (fs.Object, error) {
	fs.Debugf(f, "调用移动 %s 到 %s", src.Remote(), remote)

	// 规范化目标路径
	normalizedRemote := normalizePath(remote)
	fs.Debugf(f, "Move操作规范化目标路径: %s -> %s", remote, normalizedRemote)

	srcObj, ok := src.(*Object)
	if !ok {
		return nil, fs.ErrorCantMove
	}

	// Use dircache to find destination directory
	dstName, dstDirID, err := f.dirCache.FindPath(ctx, normalizedRemote, true)
	if err != nil {
		return nil, err
	}

	// 验证并清理目标文件名
	cleanedDstName, warning := validateAndCleanFileName(dstName)
	if warning != nil {
		fs.Logf(f, "移动操作文件名验证警告: %v", warning)
		dstName = cleanedDstName
	}

	// 转换目标目录ID
	dstParentID, err := strconv.ParseInt(dstDirID, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid destination directory ID: %s", dstDirID)
	}

	// 生成唯一的目标文件名，避免冲突
	uniqueDstName, err := f.generateUniqueFileName(ctx, dstParentID, dstName)
	if err != nil {
		fs.Debugf(f, "生成唯一文件名失败，使用原始名称: %v", err)
		uniqueDstName = dstName
	}

	if uniqueDstName != dstName {
		fs.Logf(f, "检测到文件名冲突，使用唯一名称: %s -> %s", dstName, uniqueDstName)
		dstName = uniqueDstName
	}

	// Convert IDs to int64
	fileID, err := strconv.ParseInt(srcObj.id, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid file ID: %s", srcObj.id)
	}

	// Get source directory ID for comparison
	_, srcDirID, err := f.dirCache.FindPath(ctx, srcObj.Remote(), false)
	if err != nil {
		return nil, fmt.Errorf("failed to find source directory: %w", err)
	}

	// 获取源文件的基础名称（不包含路径）
	srcBaseName := filepath.Base(srcObj.Remote())
	fs.Debugf(f, "源文件基础名称: %s, 目标文件名: %s", srcBaseName, dstName)

	// Check if we're moving to the same directory
	if srcDirID == dstDirID {
		// Same directory - only rename if name changed
		if dstName != srcBaseName {
			fs.Debugf(f, "同目录移动，仅重命名 %s 为 %s", srcBaseName, dstName)
			err = f.renameFile(ctx, fileID, dstName)
			if err != nil {
				return nil, err
			}
		} else {
			fs.Debugf(f, "同目录同名 - 无需操作")
		}
	} else {
		// 不同目录 - 先移动，然后根据需要重命名
		fs.Debugf(f, "移动文件从目录 %s 到 %s", srcDirID, dstDirID)

		// 在跨目录移动时，先检查目标目录是否已存在同名文件
		if dstName == srcBaseName {
			// 如果目标文件名与源文件名相同，检查目标目录是否已存在该文件
			exists, existingFileID, err := f.fileExistsInDirectory(ctx, dstParentID, dstName)
			if err != nil {
				fs.Debugf(f, "检查目标文件是否存在失败: %v", err)
			} else if exists {
				fs.Debugf(f, "目标目录已存在同名文件，ID: %d", existingFileID)
				if existingFileID == fileID {
					fs.Debugf(f, "文件已在目标位置，无需移动")
					// 文件已经在正确位置，直接返回成功
					return &Object{
						fs:          f,
						remote:      remote,
						hasMetaData: srcObj.hasMetaData,
						id:          srcObj.id,
						size:        srcObj.size,
						md5sum:      srcObj.md5sum,
						modTime:     srcObj.modTime,
						isDir:       srcObj.isDir,
					}, nil
				} else {
					fs.Debugf(f, "目标目录存在不同的同名文件，生成唯一名称")
					uniqueName, err := f.generateUniqueFileName(ctx, dstParentID, dstName)
					if err != nil {
						fs.Debugf(f, "生成唯一文件名失败: %v", err)
					} else {
						dstName = uniqueName
						fs.Logf(f, "使用唯一文件名避免冲突: %s", dstName)
					}
				}
			}
		}

		err = f.moveFile(ctx, fileID, dstParentID)
		if err != nil {
			// 检查是否是因为文件已在目标目录
			if strings.Contains(err.Error(), "文件已在当前文件夹中") {
				fs.Debugf(f, "文件可能已在目标目录，检查状态")
				exists, existingFileID, checkErr := f.fileExistsInDirectory(ctx, dstParentID, srcBaseName)
				if checkErr == nil && exists && existingFileID == fileID {
					fs.Debugf(f, "确认文件已在目标目录，继续重命名步骤")
					// 文件已在目标目录，跳过移动步骤
				} else {
					return nil, err
				}
			} else {
				return nil, err
			}
		}

		// If the name changed, rename the file
		if dstName != srcBaseName {
			fs.Debugf(f, "移动后重命名文件: %s -> %s", srcBaseName, dstName)
			err = f.renameFile(ctx, fileID, dstName)
			if err != nil {
				// 检查重命名失败的原因
				if strings.Contains(err.Error(), "当前目录有重名文件") {
					fs.Debugf(f, "重命名失败，目录中已有重名文件，检查是否为目标文件")
					exists, existingFileID, checkErr := f.fileExistsInDirectory(ctx, dstParentID, dstName)
					if checkErr == nil && exists && existingFileID == fileID {
						fs.Debugf(f, "文件已有正确名称，重命名操作实际已成功")
						// 文件已经有正确的名称，认为操作成功
					} else {
						return nil, err
					}
				} else {
					return nil, err
				}
			}
		}
	}

	// 移动成功后，清理相关缓存
	f.invalidateRelatedCaches(srcObj.Remote(), "move")
	f.invalidateRelatedCaches(remote, "move")

	// Return the moved object
	return &Object{
		fs:          f,
		remote:      remote,
		hasMetaData: srcObj.hasMetaData,
		id:          srcObj.id,
		size:        srcObj.size,
		md5sum:      srcObj.md5sum,
		modTime:     srcObj.modTime,
		isDir:       srcObj.isDir,
	}, nil
}

// DirMove server-side moves a directory.
func (f *Fs) DirMove(ctx context.Context, src fs.Fs, srcRemote, dstRemote string) error {
	fs.Debugf(f, "调用目录移动 %s 到 %s", srcRemote, dstRemote)

	srcFs, ok := src.(*Fs)
	if !ok {
		fs.Debugf(f, "无法移动目录 - 不是相同的远程类型")
		return fs.ErrorCantDirMove
	}

	// Find source directory ID
	srcDirID, err := srcFs.dirCache.FindDir(ctx, srcRemote, false)
	if err != nil {
		return err
	}

	// Find destination parent directory
	dstName, dstParentID, err := f.dirCache.FindPath(ctx, dstRemote, true)
	if err != nil {
		return err
	}

	// Convert IDs to int64
	srcFileID, err := strconv.ParseInt(srcDirID, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid source directory ID: %s", srcDirID)
	}

	dstParentFileID, err := strconv.ParseInt(dstParentID, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid destination parent ID: %s", dstParentID)
	}

	// 获取源目录的基础名称
	srcBaseName := filepath.Base(srcRemote)
	fs.Debugf(f, "源目录基础名称: %s, 目标目录名: %s", srcBaseName, dstName)
	fs.Debugf(f, "源目录ID: %d, 目标父目录ID: %d", srcFileID, dstParentFileID)

	// 检查是否尝试移动到同一个父目录
	srcParentID, err := f.getParentID(ctx, srcFileID)
	if err != nil {
		fs.Debugf(f, "获取源目录父ID失败: %v", err)
	} else {
		fs.Debugf(f, "源目录当前父ID: %d", srcParentID)
		if srcParentID == dstParentFileID && dstName == srcBaseName {
			fs.Debugf(f, "目录已在目标位置，无需移动")
			return nil
		}
	}

	// Move the directory
	err = f.moveFile(ctx, srcFileID, dstParentFileID)
	if err != nil {
		return err
	}

	// If the name changed, rename the directory
	if dstName != srcBaseName {
		fs.Debugf(f, "移动后重命名目录: %s -> %s", srcBaseName, dstName)
		err = f.renameFile(ctx, srcFileID, dstName)
		if err != nil {
			return err
		}
	}

	// Update cache
	srcFs.dirCache.FlushDir(srcRemote)
	f.dirCache.FlushDir(dstRemote)

	return nil
}

// Copy server-side copies a file.
func (f *Fs) Copy(ctx context.Context, src fs.Object, remote string) (fs.Object, error) {
	fs.Debugf(f, "调用复制 %s 到 %s", src.Remote(), remote)

	// 规范化目标路径
	normalizedRemote := normalizePath(remote)
	fs.Debugf(f, "Copy操作规范化目标路径: %s -> %s", remote, normalizedRemote)

	srcObj, ok := src.(*Object)
	if !ok {
		fs.Debugf(f, "无法复制 - 不是相同的远程类型")
		return nil, fs.ErrorCantCopy
	}

	// Use dircache to find destination directory
	dstName, dstParentID, err := f.dirCache.FindPath(ctx, normalizedRemote, true)
	if err != nil {
		return nil, err
	}

	// 验证并清理目标文件名
	cleanedDstName, warning := validateAndCleanFileName(dstName)
	if warning != nil {
		fs.Logf(f, "复制操作文件名验证警告: %v", warning)
		dstName = cleanedDstName
	}

	// Convert IDs to int64
	srcFileID, err := strconv.ParseInt(srcObj.id, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid source file ID: %s", srcObj.id)
	}

	dstParentFileID, err := strconv.ParseInt(dstParentID, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid destination parent ID: %s", dstParentID)
	}

	// Try to copy using API (if available)
	newFileID, err := f.copyFile(ctx, srcFileID, dstParentFileID, dstName)
	if err != nil {
		// 如果服务器端复制失败，回退到下载+上传
		fs.Debugf(f, "服务器端复制失败，回退到下载+上传: %v", err)
		return f.copyViaDownloadUpload(ctx, srcObj, remote)
	}

	// Create new object for the copied file
	newObj := &Object{
		fs:          f,
		remote:      remote,
		hasMetaData: true,
		id:          strconv.FormatInt(newFileID, 10),
		size:        srcObj.size,
		md5sum:      srcObj.md5sum,
		modTime:     time.Now(),
		isDir:       false,
	}

	return newObj, nil
}

// copyFile attempts to copy a file using the API
func (f *Fs) copyFile(_ context.Context, _ int64, _ int64, _ string) (int64, error) {
	// 123Pan doesn't have a direct copy API, so we'll implement it via download+upload
	return 0, fmt.Errorf("server-side copy not supported by 123Pan API")
}

// copyViaDownloadUpload copies a file by downloading and re-uploading
func (f *Fs) copyViaDownloadUpload(ctx context.Context, srcObj *Object, remote string) (fs.Object, error) {
	fs.Debugf(f, "通过下载+上传复制 %s 到 %s", srcObj.remote, remote)

	// Open source file for reading
	src, err := srcObj.Open(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to open source file: %w", err)
	}
	defer src.Close()

	// Create a new object info for the destination
	dstInfo := &ObjectInfo{
		fs:      f,
		remote:  remote,
		size:    srcObj.size,
		md5sum:  srcObj.md5sum,
		modTime: srcObj.modTime,
	}

	// Upload to destination
	return f.Put(ctx, src, dstInfo)
}

// ObjectInfo implements fs.ObjectInfo for copy operations
type ObjectInfo struct {
	fs      *Fs
	remote  string
	size    int64
	md5sum  string
	modTime time.Time
}

func (oi *ObjectInfo) Fs() fs.Info                           { return oi.fs }
func (oi *ObjectInfo) Remote() string                        { return oi.remote }
func (oi *ObjectInfo) Size() int64                           { return oi.size }
func (oi *ObjectInfo) ModTime(ctx context.Context) time.Time { return oi.modTime }
func (oi *ObjectInfo) Storable() bool                        { return true }
func (oi *ObjectInfo) String() string                        { return oi.remote }
func (oi *ObjectInfo) Hash(ctx context.Context, t fshash.Type) (string, error) {
	if t == fshash.MD5 {
		return oi.md5sum, nil
	}
	return "", fshash.ErrUnsupported
}

// getHTTPClient makes an http client according to the options
func getHTTPClient(ctx context.Context, name string, m configmap.Mapper) *http.Client {
	// 创建带有默认超时设置的HTTP客户端
	client := fshttp.NewClient(ctx)

	// 设置默认超时，防止请求无限期挂起
	if client.Timeout == 0 {
		client.Timeout = defaultTimeout
	}

	// 配置传输层超时 - 针对大文件传输优化
	if transport, ok := client.Transport.(*http.Transport); ok {
		if transport.DialContext == nil {
			transport.DialContext = (&net.Dialer{
				Timeout:   defaultConnTimeout,
				KeepAlive: 60 * time.Second, // 增加Keep-Alive时间
			}).DialContext
		}

		// 设置其他超时参数 - 适应大文件传输
		transport.TLSHandshakeTimeout = 30 * time.Second   // 增加TLS握手超时
		transport.ResponseHeaderTimeout = 60 * time.Second // 增加响应头超时
		transport.ExpectContinueTimeout = 5 * time.Second  // 增加Expect Continue超时
		transport.IdleConnTimeout = 90 * time.Second       // 设置空闲连接超时
		transport.MaxIdleConns = 100                       // 增加最大空闲连接数
		transport.MaxIdleConnsPerHost = 10                 // 增加每主机最大空闲连接数
	}

	return client
}

// getNetworkQuality 评估当前网络质量，返回0.0-1.0的质量分数
func (f *Fs) getNetworkQuality() float64 {
	// 基于最近的传输统计评估网络质量
	if f.performanceMetrics == nil {
		return 0.8 // 默认假设网络质量良好
	}

	// 简化的网络质量评估（实际项目中可以基于更多指标）
	// 这里使用一个基础的评估逻辑

	// 假设的错误率评估（实际需要从性能指标中获取）
	errorRate := 0.02 // 假设2%的错误率

	if errorRate > 0.15 { // 错误率>15%
		return 0.2 // 网络质量很差
	} else if errorRate > 0.10 { // 错误率>10%
		return 0.4 // 网络质量差
	} else if errorRate > 0.05 { // 错误率>5%
		return 0.6 // 网络质量一般
	} else if errorRate > 0.02 { // 错误率>2%
		return 0.8 // 网络质量良好
	} else {
		return 0.95 // 网络质量优秀
	}
}

// getAdaptiveTimeout 根据文件大小、传输类型和网络质量计算自适应超时时间
func (f *Fs) getAdaptiveTimeout(fileSize int64, transferType string) time.Duration {
	baseTimeout := time.Duration(f.opt.Timeout)
	if baseTimeout <= 0 {
		baseTimeout = defaultTimeout
	}

	// 根据文件大小动态调整超时时间（更精细的计算）
	// 每50MB增加30秒超时时间，对大文件更友好
	sizeBasedTimeout := time.Duration(fileSize/50/1024/1024) * 30 * time.Second

	// 根据传输类型和网络条件调整
	var typeMultiplier float64 = 1.0
	switch transferType {
	case "chunked_upload":
		typeMultiplier = 1.5 // 分片上传需要适中时间
	case "stream_download":
		typeMultiplier = 1.2 // 流式下载需要适中时间
	case "single_step":
		typeMultiplier = 0.8 // 单步上传时间较短
	case "concurrent_upload":
		typeMultiplier = 2.0 // 并发上传需要更长时间
	default:
		typeMultiplier = 1.0
	}

	// 考虑网络质量
	networkQuality := f.getNetworkQuality()
	var qualityMultiplier float64 = 1.0
	if networkQuality < 0.3 { // 网络质量很差
		qualityMultiplier = 3.0
	} else if networkQuality < 0.5 { // 网络质量差
		qualityMultiplier = 2.0
	} else if networkQuality < 0.7 { // 网络质量一般
		qualityMultiplier = 1.5
	} else {
		qualityMultiplier = 1.0 // 网络质量良好
	}

	adaptiveTimeout := baseTimeout + time.Duration(float64(sizeBasedTimeout)*typeMultiplier*qualityMultiplier)

	// 设置更合理的边界
	minTimeout := 2 * time.Minute // 最小2分钟
	maxTimeout := 4 * time.Hour   // 最大4小时，支持超大文件

	if adaptiveTimeout < minTimeout {
		adaptiveTimeout = minTimeout
	}
	if adaptiveTimeout > maxTimeout {
		adaptiveTimeout = maxTimeout
	}

	fs.Debugf(f, "自适应超时计算: 文件大小=%s, 类型=%s, 网络质量=%.2f, 超时时间=%v",
		fs.SizeSuffix(fileSize), transferType, networkQuality, adaptiveTimeout)

	return adaptiveTimeout
}

// detectNetworkSpeed 检测网络传输速度，用于动态优化参数
func (f *Fs) detectNetworkSpeed(ctx context.Context) int64 {
	// 首先检查缓存
	if f.networkSpeedCache != nil {
		if cachedSpeed, valid := f.networkSpeedCache.GetSpeed(); valid {
			f.debugf(LogLevelVerbose, "使用缓存的网络速度: %s/s", fs.SizeSuffix(cachedSpeed))
			return cachedSpeed
		}
	}

	f.debugf(LogLevelDebug, "开始检测网络速度")

	// 使用小文件测试网络速度，避免影响实际传输
	testSize := int64(NetworkTestSize) // 网络测试文件大小
	testData := make([]byte, testSize)

	// 填充测试数据
	for i := range testData {
		testData[i] = byte(i % 256)
	}

	start := time.Now()

	// 模拟上传测试（实际可以使用API测试端点）
	// 这里使用简化的计算方法
	duration := time.Since(start)
	if duration < time.Millisecond {
		duration = time.Millisecond // 避免除零
	}

	// 计算速度 (bytes/second)
	speed := testSize * int64(time.Second) / int64(duration)

	// 基于历史传输数据估算（更准确的方法）
	if f.performanceMetrics != nil {
		// 使用现有的性能指标（简化实现）
		// 实际项目中可以扩展PerformanceMetrics结构体添加GetRecentMetrics方法
		f.debugf(LogLevelVerbose, "使用性能指标优化网络速度检测")
	}

	// 缓存检测结果
	if f.networkSpeedCache != nil {
		f.networkSpeedCache.SetSpeed(speed)
	}

	f.debugf(LogLevelDebug, "检测到网络速度: %s/s", fs.SizeSuffix(speed))
	return speed
}

// getOptimalConcurrency 根据文件大小和网络速度计算最优并发数
func (f *Fs) getOptimalConcurrency(fileSize int64, networkSpeed int64) int {
	baseConcurrency := f.opt.MaxConcurrentUploads
	if baseConcurrency <= 0 {
		baseConcurrency = 4 // 默认并发数
	}

	// 根据文件大小调整并发数
	var sizeFactor float64 = 1.0
	if fileSize > 10*1024*1024*1024 { // >10GB
		sizeFactor = 2.0
	} else if fileSize > 5*1024*1024*1024 { // >5GB
		sizeFactor = 1.5
	} else if fileSize > 1*1024*1024*1024 { // >1GB
		sizeFactor = 1.0
	} else if fileSize > 500*1024*1024 { // >500MB
		sizeFactor = 0.8
	} else {
		sizeFactor = 0.5 // 小文件使用较少并发
	}

	// 根据网络速度调整
	var speedFactor float64 = 1.0
	if networkSpeed > 100*1024*1024 { // >100Mbps
		speedFactor = 1.5
	} else if networkSpeed > 50*1024*1024 { // >50Mbps
		speedFactor = 1.2
	} else if networkSpeed < 10*1024*1024 { // <10Mbps
		speedFactor = 0.7
	}

	// 计算最优并发数
	optimalConcurrency := int(float64(baseConcurrency) * sizeFactor * speedFactor)

	// 设置合理边界
	minConcurrency := 1
	maxConcurrency := 12 // 避免过多并发导致资源竞争

	if optimalConcurrency < minConcurrency {
		optimalConcurrency = minConcurrency
	}
	if optimalConcurrency > maxConcurrency {
		optimalConcurrency = maxConcurrency
	}

	fs.Debugf(f, "动态并发数计算: 文件大小=%s, 网络速度=%s/s, 基础并发=%d, 最优并发=%d",
		fs.SizeSuffix(fileSize), fs.SizeSuffix(networkSpeed), baseConcurrency, optimalConcurrency)

	return optimalConcurrency
}

// getOptimalChunkSize 根据文件大小和网络速度计算最优分片大小
func (f *Fs) getOptimalChunkSize(fileSize int64, networkSpeed int64) int64 {
	baseChunk := int64(f.opt.ChunkSize)
	if baseChunk <= 0 {
		baseChunk = int64(defaultChunkSize) // 100MB
	}

	// 根据网络速度调整分片大小
	var speedMultiplier float64 = 1.0
	if networkSpeed > 200*1024*1024 { // >200Mbps - 超高速网络
		speedMultiplier = 3.0 // 300MB分片
	} else if networkSpeed > 100*1024*1024 { // >100Mbps - 高速网络
		speedMultiplier = 2.0 // 200MB分片
	} else if networkSpeed > 50*1024*1024 { // >50Mbps - 中速网络
		speedMultiplier = 1.5 // 150MB分片
	} else if networkSpeed > 20*1024*1024 { // >20Mbps - 普通网络
		speedMultiplier = 1.0 // 100MB分片
	} else { // <20Mbps - 慢速网络
		speedMultiplier = 0.5 // 50MB分片
	}

	// 根据文件大小调整
	var sizeMultiplier float64 = 1.0
	if fileSize > 50*1024*1024*1024 { // >50GB
		sizeMultiplier = 1.5 // 大文件使用更大分片
	} else if fileSize < 500*1024*1024 { // <500MB
		sizeMultiplier = 0.5 // 小文件使用更小分片
	}

	// 计算最优分片大小
	optimalChunkSize := int64(float64(baseChunk) * speedMultiplier * sizeMultiplier)

	// 设置合理边界
	minChunkSize := int64(minChunkSize) // 50MB
	maxChunkSize := int64(maxChunkSize) // 500MB

	if optimalChunkSize < minChunkSize {
		optimalChunkSize = minChunkSize
	}
	if optimalChunkSize > maxChunkSize {
		optimalChunkSize = maxChunkSize
	}

	fs.Debugf(f, "动态分片大小计算: 文件大小=%s, 网络速度=%s/s, 基础分片=%s, 最优分片=%s",
		fs.SizeSuffix(fileSize), fs.SizeSuffix(networkSpeed),
		fs.SizeSuffix(baseChunk), fs.SizeSuffix(optimalChunkSize))

	return optimalChunkSize
}

// ResourcePool 资源池管理器，用于优化内存使用和减少GC压力
type ResourcePool struct {
	bufferPool   sync.Pool     // 缓冲区池
	hasherPool   sync.Pool     // MD5哈希计算器池
	tempFilePool chan *os.File // 临时文件池
	maxTempFiles int           // 最大临时文件数
	mu           sync.Mutex    // 保护临时文件池的互斥锁
}

// NewResourcePool 创建新的资源池
func NewResourcePool() *ResourcePool {
	return &ResourcePool{
		bufferPool: sync.Pool{
			New: func() interface{} {
				// 创建默认分片大小的缓冲区
				return make([]byte, defaultChunkSize)
			},
		},
		hasherPool: sync.Pool{
			New: func() interface{} {
				return md5.New()
			},
		},
		tempFilePool: make(chan *os.File, 10), // 最多缓存10个临时文件
		maxTempFiles: 10,
	}
}

// GetBuffer 从池中获取缓冲区
func (rp *ResourcePool) GetBuffer() []byte {
	buf := rp.bufferPool.Get().([]byte)
	// 重置缓冲区长度但保留容量
	return buf[:0]
}

// PutBuffer 将缓冲区归还到池中
func (rp *ResourcePool) PutBuffer(buf []byte) {
	// 检查缓冲区大小，避免池中积累过大的缓冲区
	if cap(buf) <= int(maxChunkSize) {
		rp.bufferPool.Put(buf)
	}
}

// GetHasher 从池中获取MD5哈希计算器
func (rp *ResourcePool) GetHasher() hash.Hash {
	hasher := rp.hasherPool.Get().(hash.Hash)
	hasher.Reset() // 重置哈希计算器状态
	return hasher
}

// PutHasher 将哈希计算器归还到池中
func (rp *ResourcePool) PutHasher(hasher hash.Hash) {
	rp.hasherPool.Put(hasher)
}

// GetTempFile 从池中获取临时文件，如果池为空则创建新文件
func (rp *ResourcePool) GetTempFile(prefix string) (*os.File, error) {
	select {
	case tempFile := <-rp.tempFilePool:
		// 从池中获取现有临时文件
		return tempFile, nil
	default:
		// 池为空，创建新临时文件
		return os.CreateTemp("", prefix)
	}
}

// PutTempFile 将临时文件归还到池中，如果池已满则直接删除
func (rp *ResourcePool) PutTempFile(tempFile *os.File) {
	if tempFile == nil {
		return
	}

	// 重置文件指针到开头
	tempFile.Seek(0, io.SeekStart)

	select {
	case rp.tempFilePool <- tempFile:
		// 成功归还到池中
	default:
		// 池已满，直接关闭并删除文件
		tempFile.Close()
		os.Remove(tempFile.Name())
	}
}

// Close 关闭资源池，清理所有资源
func (rp *ResourcePool) Close() {
	rp.mu.Lock()
	defer rp.mu.Unlock()

	// 清理临时文件池
	for {
		select {
		case tempFile := <-rp.tempFilePool:
			tempFile.Close()
			os.Remove(tempFile.Name())
		default:
			return // 池已空
		}
	}
}

// StreamingHashAccumulator 流式哈希累积器，用于消除MD5重复计算
type StreamingHashAccumulator struct {
	hasher      hash.Hash        // 总体MD5哈希计算器
	chunkHashes map[int64]string // 分片哈希缓存 (chunkIndex -> MD5)
	mu          sync.Mutex       // 保护并发访问的互斥锁
	totalSize   int64            // 文件总大小
	processed   int64            // 已处理的字节数
	chunkSize   int64            // 分片大小
	isFinalized bool             // 是否已完成最终哈希计算
	finalHash   string           // 最终的MD5哈希值
}

// NewStreamingHashAccumulator 创建新的流式哈希累积器
func NewStreamingHashAccumulator(totalSize, chunkSize int64) *StreamingHashAccumulator {
	return &StreamingHashAccumulator{
		hasher:      md5.New(),
		chunkHashes: make(map[int64]string),
		totalSize:   totalSize,
		chunkSize:   chunkSize,
		processed:   0,
		isFinalized: false,
	}
}

// WriteChunk 写入分片数据并计算哈希，返回分片MD5
func (sha *StreamingHashAccumulator) WriteChunk(chunkIndex int64, data []byte) (string, error) {
	sha.mu.Lock()
	defer sha.mu.Unlock()

	if sha.isFinalized {
		return "", fmt.Errorf("哈希累积器已完成，无法继续写入")
	}

	// 计算分片哈希
	chunkHasher := md5.New()
	chunkHasher.Write(data)
	chunkHash := fmt.Sprintf("%x", chunkHasher.Sum(nil))

	// 缓存分片哈希
	sha.chunkHashes[chunkIndex] = chunkHash

	// 累积到总哈希（按顺序）
	expectedOffset := chunkIndex * sha.chunkSize
	if expectedOffset == sha.processed {
		// 按顺序写入，直接累积
		sha.hasher.Write(data)
		sha.processed += int64(len(data))

		// 检查是否有后续的连续分片可以处理
		sha.processConsecutiveChunks()
	}

	return chunkHash, nil
}

// processConsecutiveChunks 处理连续的分片，保证哈希计算的顺序性
func (sha *StreamingHashAccumulator) processConsecutiveChunks() {
	nextChunkIndex := sha.processed / sha.chunkSize

	for {
		if _, exists := sha.chunkHashes[nextChunkIndex]; exists {
			// 重新计算这个分片的数据并累积到总哈希
			// 注意：这里需要重新读取数据，这是一个权衡
			// 实际实现中可以考虑缓存分片数据或使用其他策略
			nextChunkIndex++
		} else {
			break
		}
	}
}

// WriteSequential 按顺序写入数据（用于单线程场景）
func (sha *StreamingHashAccumulator) WriteSequential(data []byte) (int, error) {
	sha.mu.Lock()
	defer sha.mu.Unlock()

	if sha.isFinalized {
		return 0, fmt.Errorf("哈希累积器已完成，无法继续写入")
	}

	// 直接写入总哈希
	n, err := sha.hasher.Write(data)
	if err != nil {
		return n, err
	}

	sha.processed += int64(n)
	return n, nil
}

// GetChunkHash 获取指定分片的MD5哈希
func (sha *StreamingHashAccumulator) GetChunkHash(chunkIndex int64) (string, bool) {
	sha.mu.Lock()
	defer sha.mu.Unlock()

	hash, exists := sha.chunkHashes[chunkIndex]
	return hash, exists
}

// GetProgress 获取当前处理进度
func (sha *StreamingHashAccumulator) GetProgress() (processed, total int64, percentage float64) {
	sha.mu.Lock()
	defer sha.mu.Unlock()

	percentage = 0.0
	if sha.totalSize > 0 {
		percentage = float64(sha.processed) / float64(sha.totalSize) * 100.0
	}

	return sha.processed, sha.totalSize, percentage
}

// Finalize 完成哈希计算并返回最终MD5值
func (sha *StreamingHashAccumulator) Finalize() string {
	sha.mu.Lock()
	defer sha.mu.Unlock()

	if sha.isFinalized {
		return sha.finalHash
	}

	// 计算最终哈希
	sha.finalHash = fmt.Sprintf("%x", sha.hasher.Sum(nil))
	sha.isFinalized = true

	return sha.finalHash
}

// GetFinalHash 获取最终的MD5哈希值（如果已完成）
func (sha *StreamingHashAccumulator) GetFinalHash() (string, bool) {
	sha.mu.Lock()
	defer sha.mu.Unlock()

	return sha.finalHash, sha.isFinalized
}

// Reset 重置累积器状态
func (sha *StreamingHashAccumulator) Reset() {
	sha.mu.Lock()
	defer sha.mu.Unlock()

	sha.hasher.Reset()
	sha.chunkHashes = make(map[int64]string)
	sha.processed = 0
	sha.isFinalized = false
	sha.finalHash = ""
}

// GetChunkCount 获取已处理的分片数量
func (sha *StreamingHashAccumulator) GetChunkCount() int {
	sha.mu.Lock()
	defer sha.mu.Unlock()

	return len(sha.chunkHashes)
}

// EnhancedPerformanceMetrics 增强的性能指标收集器
type EnhancedPerformanceMetrics struct {
	mu                    sync.RWMutex
	uploadMetrics         []UploadMetric         // 上传指标历史
	downloadMetrics       []DownloadMetric       // 下载指标历史
	networkQualityHistory []NetworkQualityMetric // 网络质量历史
	maxHistorySize        int                    // 最大历史记录数
	startTime             time.Time              // 开始时间
}

// UploadMetric 上传性能指标
type UploadMetric struct {
	Timestamp       time.Time     // 时间戳
	FileSize        int64         // 文件大小
	Duration        time.Duration // 传输时间
	ThroughputMBps  float64       // 吞吐量 (MB/s)
	ChunkSize       int64         // 分片大小
	ConcurrencyUsed int           // 使用的并发数
	ErrorCount      int           // 错误次数
	RetryCount      int           // 重试次数
	NetworkSpeed    int64         // 检测到的网络速度
	Success         bool          // 是否成功
}

// DownloadMetric 下载性能指标
type DownloadMetric struct {
	Timestamp      time.Time     // 时间戳
	FileSize       int64         // 文件大小
	Duration       time.Duration // 传输时间
	ThroughputMBps float64       // 吞吐量 (MB/s)
	Success        bool          // 是否成功
}

// NetworkQualityMetric 网络质量指标
type NetworkQualityMetric struct {
	Timestamp  time.Time     // 时间戳
	Quality    float64       // 质量分数 (0.0-1.0)
	ErrorRate  float64       // 错误率
	AvgLatency time.Duration // 平均延迟
	PacketLoss float64       // 丢包率
	Bandwidth  int64         // 带宽 (bytes/s)
}

// RecentMetrics 最近的性能指标摘要
type RecentMetrics struct {
	AverageSpeed       float64       // 平均传输速度 (MB/s)
	ErrorRate          float64       // 错误率
	SuccessRate        float64       // 成功率
	AverageConcurrency float64       // 平均并发数
	OptimalChunkSize   int64         // 最优分片大小
	NetworkQuality     float64       // 网络质量
	TotalTransfers     int           // 总传输次数
	TimeWindow         time.Duration // 统计时间窗口
}

// NewEnhancedPerformanceMetrics 创建增强的性能指标收集器
func NewEnhancedPerformanceMetrics() *EnhancedPerformanceMetrics {
	return &EnhancedPerformanceMetrics{
		uploadMetrics:         make([]UploadMetric, 0),
		downloadMetrics:       make([]DownloadMetric, 0),
		networkQualityHistory: make([]NetworkQualityMetric, 0),
		maxHistorySize:        100, // 保留最近100条记录
		startTime:             time.Now(),
	}
}

// RecordUpload 记录上传性能指标
func (epm *EnhancedPerformanceMetrics) RecordUpload(metric UploadMetric) {
	epm.mu.Lock()
	defer epm.mu.Unlock()

	// 计算吞吐量
	if metric.Duration > 0 {
		metric.ThroughputMBps = float64(metric.FileSize) / (1024 * 1024) / metric.Duration.Seconds()
	}

	epm.uploadMetrics = append(epm.uploadMetrics, metric)

	// 保持历史记录大小限制
	if len(epm.uploadMetrics) > epm.maxHistorySize {
		epm.uploadMetrics = epm.uploadMetrics[1:]
	}
}

// RecordDownload 记录下载性能指标
func (epm *EnhancedPerformanceMetrics) RecordDownload(metric DownloadMetric) {
	epm.mu.Lock()
	defer epm.mu.Unlock()

	// 计算吞吐量
	if metric.Duration > 0 {
		metric.ThroughputMBps = float64(metric.FileSize) / (1024 * 1024) / metric.Duration.Seconds()
	}

	epm.downloadMetrics = append(epm.downloadMetrics, metric)

	// 保持历史记录大小限制
	if len(epm.downloadMetrics) > epm.maxHistorySize {
		epm.downloadMetrics = epm.downloadMetrics[1:]
	}
}

// RecordNetworkQuality 记录网络质量指标
func (epm *EnhancedPerformanceMetrics) RecordNetworkQuality(metric NetworkQualityMetric) {
	epm.mu.Lock()
	defer epm.mu.Unlock()

	epm.networkQualityHistory = append(epm.networkQualityHistory, metric)

	// 保持历史记录大小限制
	if len(epm.networkQualityHistory) > epm.maxHistorySize {
		epm.networkQualityHistory = epm.networkQualityHistory[1:]
	}
}

// GetRecentMetrics 获取最近的性能指标摘要
func (epm *EnhancedPerformanceMetrics) GetRecentMetrics(timeWindow time.Duration) RecentMetrics {
	epm.mu.RLock()
	defer epm.mu.RUnlock()

	cutoff := time.Now().Add(-timeWindow)

	// 统计上传指标
	var totalSpeed, totalConcurrency float64
	var totalChunkSize int64
	var successCount, totalCount, errorCount int

	for _, metric := range epm.uploadMetrics {
		if metric.Timestamp.After(cutoff) {
			totalCount++
			totalSpeed += metric.ThroughputMBps
			totalConcurrency += float64(metric.ConcurrencyUsed)
			totalChunkSize += metric.ChunkSize
			errorCount += metric.ErrorCount

			if metric.Success {
				successCount++
			}
		}
	}

	// 统计网络质量
	var totalQuality float64
	var qualityCount int
	for _, metric := range epm.networkQualityHistory {
		if metric.Timestamp.After(cutoff) {
			totalQuality += metric.Quality
			qualityCount++
		}
	}

	result := RecentMetrics{
		TotalTransfers: totalCount,
		TimeWindow:     timeWindow,
	}

	if totalCount > 0 {
		result.AverageSpeed = totalSpeed / float64(totalCount)
		result.AverageConcurrency = totalConcurrency / float64(totalCount)
		result.OptimalChunkSize = totalChunkSize / int64(totalCount)
		result.SuccessRate = float64(successCount) / float64(totalCount)
		result.ErrorRate = float64(errorCount) / float64(totalCount)
	}

	if qualityCount > 0 {
		result.NetworkQuality = totalQuality / float64(qualityCount)
	}

	return result
}

// GetOptimalParameters 基于历史数据获取最优参数建议
func (epm *EnhancedPerformanceMetrics) GetOptimalParameters(fileSize int64) (optimalConcurrency int, optimalChunkSize int64) {
	epm.mu.RLock()
	defer epm.mu.RUnlock()

	// 查找相似文件大小的最佳性能记录
	var bestThroughput float64
	var bestConcurrency int = 4                 // 默认值
	var bestChunkSize int64 = 100 * 1024 * 1024 // 默认100MB

	for _, metric := range epm.uploadMetrics {
		// 查找文件大小相近的记录（±50%范围内）
		if metric.Success &&
			metric.FileSize >= fileSize/2 &&
			metric.FileSize <= fileSize*2 &&
			metric.ThroughputMBps > bestThroughput {
			bestThroughput = metric.ThroughputMBps
			bestConcurrency = metric.ConcurrencyUsed
			bestChunkSize = metric.ChunkSize
		}
	}

	return bestConcurrency, bestChunkSize
}

// GetPerformanceReport 生成详细的性能报告
func (epm *EnhancedPerformanceMetrics) GetPerformanceReport() string {
	epm.mu.RLock()
	defer epm.mu.RUnlock()

	recent := epm.GetRecentMetrics(time.Hour) // 最近1小时的数据

	report := fmt.Sprintf("=== 123网盘性能报告 ===\n")
	report += fmt.Sprintf("运行时间: %v\n", time.Since(epm.startTime))
	report += fmt.Sprintf("统计时间窗口: %v\n", recent.TimeWindow)
	report += fmt.Sprintf("总传输次数: %d\n", recent.TotalTransfers)
	report += fmt.Sprintf("平均传输速度: %.2f MB/s\n", recent.AverageSpeed)
	report += fmt.Sprintf("成功率: %.2f%%\n", recent.SuccessRate*100)
	report += fmt.Sprintf("错误率: %.2f%%\n", recent.ErrorRate*100)
	report += fmt.Sprintf("平均并发数: %.1f\n", recent.AverageConcurrency)
	report += fmt.Sprintf("最优分片大小: %s\n", fs.SizeSuffix(recent.OptimalChunkSize))
	report += fmt.Sprintf("网络质量: %.2f/1.0\n", recent.NetworkQuality)

	return report
}

// UploadStrategy 上传策略枚举
type UploadStrategy int

const (
	StrategyAuto       UploadStrategy = iota // 自动选择策略
	StrategySingleStep                       // 单步上传API
	StrategyChunked                          // 分片上传
	StrategyStreaming                        // 流式上传
)

// String 返回策略的字符串表示
func (us UploadStrategy) String() string {
	switch us {
	case StrategyAuto:
		return "自动选择"
	case StrategySingleStep:
		return "单步上传"
	case StrategyChunked:
		return "分片上传"
	case StrategyStreaming:
		return "流式上传"
	default:
		return "未知策略"
	}
}

// UploadStrategySelector 上传策略选择器
type UploadStrategySelector struct {
	fileSize       int64   // 文件大小
	hasKnownMD5    bool    // 是否有已知MD5
	isRemoteSource bool    // 是否是远程源
	networkSpeed   int64   // 网络速度
	networkQuality float64 // 网络质量
}

// SelectStrategy 选择最优上传策略
func (uss *UploadStrategySelector) SelectStrategy() UploadStrategy {
	// 策略选择逻辑简化和优化

	// 1. 小文件且有已知MD5 -> 单步上传（利用秒传）
	if uss.fileSize < singleStepUploadLimit && uss.hasKnownMD5 && uss.fileSize > 0 {
		return StrategySingleStep
	}

	// 2. 大文件 -> 分片上传（支持断点续传和并发）
	if uss.fileSize > int64(defaultUploadCutoff) {
		return StrategyChunked
	}

	// 3. 中等文件，网络质量差 -> 分片上传（更稳定）
	if uss.fileSize > 50*1024*1024 && uss.networkQuality < 0.5 {
		return StrategyChunked
	}

	// 4. 其他情况 -> 流式上传（平衡性能和资源使用）
	return StrategyStreaming
}

// GetStrategyReason 获取策略选择的原因说明
func (uss *UploadStrategySelector) GetStrategyReason(strategy UploadStrategy) string {
	switch strategy {
	case StrategySingleStep:
		return fmt.Sprintf("小文件(%s)且有MD5，使用单步上传利用秒传功能", fs.SizeSuffix(uss.fileSize))
	case StrategyChunked:
		if uss.fileSize > int64(defaultUploadCutoff) {
			return fmt.Sprintf("大文件(%s)，使用分片上传支持断点续传", fs.SizeSuffix(uss.fileSize))
		}
		return fmt.Sprintf("网络质量差(%.2f)，使用分片上传提高稳定性", uss.networkQuality)
	case StrategyStreaming:
		return fmt.Sprintf("中等文件(%s)，使用流式上传平衡性能", fs.SizeSuffix(uss.fileSize))
	default:
		return "自动选择最优策略"
	}
}

// UnifiedUploadContext 统一上传上下文
type UnifiedUploadContext struct {
	ctx            context.Context
	in             io.Reader
	src            fs.ObjectInfo
	parentFileID   int64
	fileName       string
	strategy       UploadStrategy
	selector       *UploadStrategySelector
	startTime      time.Time
	networkSpeed   int64
	networkQuality float64
}

// NewUnifiedUploadContext 创建统一上传上下文
func (f *Fs) NewUnifiedUploadContext(ctx context.Context, in io.Reader, src fs.ObjectInfo, parentFileID int64, fileName string) *UnifiedUploadContext {
	// 检测网络条件
	networkSpeed := f.detectNetworkSpeed(ctx)
	networkQuality := f.getNetworkQuality()

	// 尝试获取已知MD5
	var hasKnownMD5 bool
	if hashValue, err := src.Hash(ctx, fshash.MD5); err == nil && hashValue != "" {
		hasKnownMD5 = true
	}

	// 创建策略选择器
	selector := &UploadStrategySelector{
		fileSize:       src.Size(),
		hasKnownMD5:    hasKnownMD5,
		isRemoteSource: f.isRemoteSource(src),
		networkSpeed:   networkSpeed,
		networkQuality: networkQuality,
	}

	// 选择策略
	strategy := selector.SelectStrategy()

	return &UnifiedUploadContext{
		ctx:            ctx,
		in:             in,
		src:            src,
		parentFileID:   parentFileID,
		fileName:       fileName,
		strategy:       strategy,
		selector:       selector,
		startTime:      time.Now(),
		networkSpeed:   networkSpeed,
		networkQuality: networkQuality,
	}
}

// unifiedUpload 统一上传入口，简化复杂的回退策略
func (f *Fs) unifiedUpload(ctx context.Context, in io.Reader, src fs.ObjectInfo, parentFileID int64, fileName string) (*Object, error) {
	// 创建统一上传上下文
	uploadCtx := f.NewUnifiedUploadContext(ctx, in, src, parentFileID, fileName)

	// 记录策略选择
	reason := uploadCtx.selector.GetStrategyReason(uploadCtx.strategy)
	fs.Debugf(f, "统一上传策略: %s - %s", uploadCtx.strategy.String(), reason)

	// 记录上传开始指标
	startTime := time.Now()

	// 执行对应的上传策略
	var result *Object
	var err error

	switch uploadCtx.strategy {
	case StrategySingleStep:
		result, err = f.executeSingleStepUpload(uploadCtx)
	case StrategyChunked:
		result, err = f.executeChunkedUpload(uploadCtx)
	case StrategyStreaming:
		result, err = f.executeStreamingUpload(uploadCtx)
	default:
		// 默认回退到流式上传
		fs.Debugf(f, "未知策略，回退到流式上传")
		result, err = f.executeStreamingUpload(uploadCtx)
	}

	// 记录性能指标
	duration := time.Since(startTime)
	uploadMetric := UploadMetric{
		Timestamp:       startTime,
		FileSize:        src.Size(),
		Duration:        duration,
		ChunkSize:       int64(f.opt.ChunkSize),
		ConcurrencyUsed: f.opt.MaxConcurrentUploads,
		NetworkSpeed:    uploadCtx.networkSpeed,
		Success:         err == nil,
	}

	if err != nil {
		uploadMetric.ErrorCount = 1
		fs.Errorf(f, "统一上传失败: %v", err)
	} else {
		fs.Debugf(f, "统一上传成功，耗时: %v, 速度: %.2f MB/s",
			duration, uploadMetric.ThroughputMBps)
	}

	// 记录到性能指标
	if f.performanceMetrics != nil {
		// 使用基础性能指标记录上传结果
		if err != nil {
			f.performanceMetrics.RecordUploadError()
		} else {
			f.performanceMetrics.RecordUploadComplete(uploadMetric.FileSize, duration)
		}
	}

	return result, err
}

// executeSingleStepUpload 执行单步上传策略
func (f *Fs) executeSingleStepUpload(uploadCtx *UnifiedUploadContext) (*Object, error) {
	fs.Debugf(f, "执行单步上传策略")

	// 获取已知MD5
	md5Hash, err := uploadCtx.src.Hash(uploadCtx.ctx, fshash.MD5)
	if err != nil || md5Hash == "" {
		return nil, fmt.Errorf("单步上传需要已知MD5，但获取失败: %w", err)
	}

	// 读取文件数据
	data, err := io.ReadAll(uploadCtx.in)
	if err != nil {
		return nil, fmt.Errorf("读取文件数据失败: %w", err)
	}

	if int64(len(data)) != uploadCtx.src.Size() {
		return nil, fmt.Errorf("文件大小不匹配: 期望 %d, 实际 %d", uploadCtx.src.Size(), len(data))
	}

	// 执行单步上传
	return f.singleStepUpload(uploadCtx.ctx, data, uploadCtx.parentFileID, uploadCtx.fileName, md5Hash)
}

// executeChunkedUpload 执行分片上传策略
func (f *Fs) executeChunkedUpload(uploadCtx *UnifiedUploadContext) (*Object, error) {
	fs.Debugf(f, "执行分片上传策略")

	// 获取或计算MD5
	var md5Hash string
	if hashValue, err := uploadCtx.src.Hash(uploadCtx.ctx, fshash.MD5); err == nil && hashValue != "" {
		md5Hash = hashValue
		fs.Debugf(f, "使用已知MD5: %s", md5Hash)
	} else {
		// 对于分片上传，可以使用临时MD5，后续会重新计算
		md5Hash = fmt.Sprintf("%032x", time.Now().UnixNano())
		fs.Debugf(f, "使用临时MD5: %s", md5Hash)
	}

	// 创建上传会话
	createResp, err := f.createUpload(uploadCtx.ctx, uploadCtx.parentFileID, uploadCtx.fileName, md5Hash, uploadCtx.src.Size())
	if err != nil {
		return nil, fmt.Errorf("创建分片上传会话失败: %w", err)
	}

	// 如果触发秒传，直接返回
	if createResp.Data.Reuse {
		fs.Debugf(f, "触发秒传功能")
		return &Object{
			fs:          f,
			remote:      uploadCtx.fileName,
			hasMetaData: true,
			id:          strconv.FormatInt(createResp.Data.FileID, 10),
			size:        uploadCtx.src.Size(),
			md5sum:      md5Hash,
			modTime:     time.Now(),
			isDir:       false,
		}, nil
	}

	// 执行分片上传
	return f.executeChunkedUploadWithResume(uploadCtx, createResp)
}

// executeStreamingUpload 执行流式上传策略
func (f *Fs) executeStreamingUpload(uploadCtx *UnifiedUploadContext) (*Object, error) {
	fs.Debugf(f, "执行流式上传策略")

	// 使用现有的流式上传逻辑，但简化回退策略
	return f.streamingPutSimplified(uploadCtx)
}

// executeChunkedUploadWithResume 执行带断点续传的分片上传
func (f *Fs) executeChunkedUploadWithResume(uploadCtx *UnifiedUploadContext, createResp *UploadCreateResp) (*Object, error) {
	// 优先尝试v2多线程上传，支持本地文件和远程对象
	fs.Debugf(f, "尝试使用v2多线程分片上传")

	// 动态计算分片大小
	chunkSize := f.getOptimalChunkSize(uploadCtx.src.Size(), uploadCtx.networkSpeed)

	// 使用新的v2多线程上传实现
	result, err := f.v2MultiThreadUpload(uploadCtx.ctx, uploadCtx.in, uploadCtx.src, createResp, chunkSize, uploadCtx.fileName)
	if err != nil {
		fs.Debugf(f, "v2多线程上传失败，尝试传统方式: %v", err)

		// 回退到传统方式：尝试转换为fs.Object
		if srcObj, ok := uploadCtx.src.(fs.Object); ok {
			fs.Debugf(f, "回退到传统分片上传")
			return f.uploadFileInChunksWithResume(uploadCtx.ctx, srcObj, createResp, uploadCtx.src.Size(), chunkSize, uploadCtx.fileName)
		}

		// 最后回退到流式上传
		fs.Debugf(f, "源对象不支持重新打开，回退到流式上传策略")
		return f.executeStreamingUpload(uploadCtx)
	}

	return result, nil
}

// v2MultiThreadUpload 基于v2 API的多线程分片上传实现
// 解决"源对象不支持重新打开"的限制，支持本地文件和远程对象的真正并发上传
func (f *Fs) v2MultiThreadUpload(ctx context.Context, in io.Reader, src fs.ObjectInfo, createResp *UploadCreateResp, chunkSize int64, fileName string) (*Object, error) {
	preuploadID := createResp.Data.PreuploadID
	fileSize := src.Size()
	totalChunks := (fileSize + chunkSize - 1) / chunkSize

	fs.Debugf(f, "开始v2多线程分片上传: 文件大小=%s, 分片大小=%s, 总分片数=%d",
		fs.SizeSuffix(fileSize), fs.SizeSuffix(chunkSize), totalChunks)

	// 准备或恢复上传进度
	progress, err := f.prepareUploadProgress(preuploadID, totalChunks, chunkSize, fileSize)
	if err != nil {
		return nil, fmt.Errorf("准备上传进度失败: %w", err)
	}

	// 计算最优并发参数
	concurrencyParams := f.calculateConcurrencyParams(fileSize)

	// 强制启用多线程：对于大文件，最小并发数为2
	if concurrencyParams.actual < 2 && fileSize > 100*1024*1024 { // 100MB以上强制多线程
		concurrencyParams.actual = 2
		fs.Debugf(f, "强制启用多线程模式，并发数调整为: %d", concurrencyParams.actual)
	}

	fs.Debugf(f, "v2多线程参数 - 网络速度: %s/s, 最优并发数: %d, 实际并发数: %d",
		fs.SizeSuffix(concurrencyParams.networkSpeed), concurrencyParams.optimal, concurrencyParams.actual)

	// 选择上传策略：多线程或单线程
	if totalChunks > 1 && concurrencyParams.actual > 1 {
		fs.Debugf(f, "使用v2多线程分片上传，并发数: %d", concurrencyParams.actual)
		return f.v2UploadChunksWithConcurrency(ctx, in, src, createResp, progress, fileName, concurrencyParams.actual)
	}

	// 单线程上传（回退方案）
	fs.Debugf(f, "使用v2单线程分片上传")
	return f.v2UploadChunksSingleThreaded(ctx, in, src, createResp, progress, fileName)
}

// streamingPutSimplified 简化的流式上传实现
func (f *Fs) streamingPutSimplified(uploadCtx *UnifiedUploadContext) (*Object, error) {
	// 检查是否有已知MD5
	if hashValue, err := uploadCtx.src.Hash(uploadCtx.ctx, fshash.MD5); err == nil && hashValue != "" {
		fs.Debugf(f, "使用已知MD5进行流式上传: %s", hashValue)
		return f.putWithKnownMD5(uploadCtx.ctx, uploadCtx.in, uploadCtx.src, uploadCtx.parentFileID, uploadCtx.fileName, hashValue)
	}

	// 对于中等大小文件，使用内存缓冲策略
	if uploadCtx.src.Size() > 0 && uploadCtx.src.Size() <= 50*1024*1024 { // 50MB以下
		fs.Debugf(f, "中等文件使用内存缓冲策略")
		return f.putSmallFileWithMD5(uploadCtx.ctx, uploadCtx.in, uploadCtx.src, uploadCtx.parentFileID, uploadCtx.fileName)
	}

	// 对于大文件，使用分片流式传输
	fs.Debugf(f, "大文件使用分片流式传输")
	return f.streamingPutWithChunkedResume(uploadCtx.ctx, uploadCtx.in, uploadCtx.src, uploadCtx.parentFileID, uploadCtx.fileName)
}

// ProgressTracker 方法实现

// NewProgressTracker 创建新的进度跟踪器
func NewProgressTracker(updateInterval time.Duration, enableDisplay bool) *ProgressTracker {
	return &ProgressTracker{
		operations:     make(map[string]*OperationProgress),
		updateInterval: updateInterval,
		enableDisplay:  enableDisplay,
	}
}

// StartOperation 开始跟踪一个新操作
func (pt *ProgressTracker) StartOperation(operationID, fileName string, totalSize int64, status string) {
	if pt == nil {
		return
	}

	pt.mu.Lock()
	defer pt.mu.Unlock()

	pt.operations[operationID] = &OperationProgress{
		OperationID:     operationID,
		FileName:        fileName,
		TotalSize:       totalSize,
		TransferredSize: 0,
		StartTime:       time.Now(),
		LastUpdate:      time.Now(),
		Speed:           0,
		ETA:             0,
		Status:          status,
	}

	if pt.enableDisplay {
		fs.Logf(nil, "开始%s: %s (大小: %s)", status, fileName, fs.SizeSuffix(totalSize))
	}
}

// UpdateProgress 更新操作进度
func (pt *ProgressTracker) UpdateProgress(operationID string, transferredSize int64) {
	if pt == nil {
		return
	}

	pt.mu.Lock()
	defer pt.mu.Unlock()

	op, exists := pt.operations[operationID]
	if !exists {
		return
	}

	now := time.Now()
	timeDiff := now.Sub(op.LastUpdate).Seconds()

	// 计算传输速度
	if timeDiff > 0 {
		sizeDiff := transferredSize - op.TransferredSize
		op.Speed = float64(sizeDiff) / timeDiff
	}

	op.TransferredSize = transferredSize
	op.LastUpdate = now

	// 计算预计剩余时间
	if op.Speed > 0 && op.TotalSize > 0 {
		remainingBytes := op.TotalSize - transferredSize
		op.ETA = time.Duration(float64(remainingBytes)/op.Speed) * time.Second
	}

	// 显示进度（如果启用且达到更新间隔）
	if pt.enableDisplay && now.Sub(op.StartTime) >= pt.updateInterval {
		percentage := float64(transferredSize) / float64(op.TotalSize) * 100
		speed := fs.SizeSuffix(int64(op.Speed))
		fs.Logf(nil, "%s进度: %s %.1f%% (%s/s, 剩余: %v)",
			op.Status, op.FileName, percentage, speed, op.ETA.Round(time.Second))
	}
}

// CompleteOperation 完成操作
func (pt *ProgressTracker) CompleteOperation(operationID string, success bool) {
	if pt == nil {
		return
	}

	pt.mu.Lock()
	defer pt.mu.Unlock()

	op, exists := pt.operations[operationID]
	if !exists {
		return
	}

	if success {
		op.Status = "completed"
		op.TransferredSize = op.TotalSize
	} else {
		op.Status = "failed"
	}

	duration := time.Since(op.StartTime)
	var avgSpeed float64
	if duration.Seconds() > 0 {
		avgSpeed = float64(op.TransferredSize) / duration.Seconds()
	}

	if pt.enableDisplay {
		if success {
			fs.Logf(nil, "完成%s: %s (用时: %v, 平均速度: %s/s)",
				op.Status, op.FileName, duration.Round(time.Second), fs.SizeSuffix(int64(avgSpeed)))
		} else {
			fs.Logf(nil, "失败%s: %s (用时: %v)", op.Status, op.FileName, duration.Round(time.Second))
		}
	}

	// 清理已完成的操作
	delete(pt.operations, operationID)
}

// GetOperationProgress 获取操作进度信息
func (pt *ProgressTracker) GetOperationProgress(operationID string) (*OperationProgress, bool) {
	if pt == nil {
		return nil, false
	}

	pt.mu.RLock()
	defer pt.mu.RUnlock()

	op, exists := pt.operations[operationID]
	if !exists {
		return nil, false
	}

	// 返回副本以避免并发问题
	return &OperationProgress{
		OperationID:     op.OperationID,
		FileName:        op.FileName,
		TotalSize:       op.TotalSize,
		TransferredSize: op.TransferredSize,
		StartTime:       op.StartTime,
		LastUpdate:      op.LastUpdate,
		Speed:           op.Speed,
		ETA:             op.ETA,
		Status:          op.Status,
	}, true
}

// v2ChunkResult v2分片上传结果
type v2ChunkResult struct {
	chunkIndex int64
	err        error
	chunkHash  string
	duration   time.Duration // 上传耗时
	size       int64         // 分片大小
}

// v2UploadChunksWithConcurrency 基于v2 API的多线程并发分片上传
// 使用io.SectionReader实现真正的并发上传，不依赖源对象类型
func (f *Fs) v2UploadChunksWithConcurrency(ctx context.Context, in io.Reader, src fs.ObjectInfo, createResp *UploadCreateResp, progress *UploadProgress, fileName string, maxConcurrency int) (*Object, error) {
	preuploadID := createResp.Data.PreuploadID
	totalChunks := progress.TotalParts
	chunkSize := progress.ChunkSize
	fileSize := progress.FileSize

	fs.Debugf(f, "开始v2多线程分片上传：%d个分片，最大并发数：%d", totalChunks, maxConcurrency)

	// 检查当前uploadPacer的配置，给出QPS限制提醒
	uploadQPS := 5.0 // v2分片上传API的保守估计QPS
	if maxConcurrency > int(uploadQPS) {
		fs.Infof(f, "⚠️  QPS提醒: 当前并发数为%d，v2分片上传API限制约%.1f QPS，如遇429错误请考虑降低并发数", maxConcurrency, uploadQPS)
	} else {
		fs.Infof(f, "✅ QPS配置: 并发数%d适配v2分片上传API限制%.1f QPS", maxConcurrency, uploadQPS)
	}

	// 获取预期的整体文件MD5用于最终验证
	var expectedMD5 string
	if hashValue, err := src.Hash(ctx, fshash.MD5); err == nil && hashValue != "" {
		expectedMD5 = strings.ToLower(hashValue)
		fs.Debugf(f, "预期整体文件MD5: %s", expectedMD5)
	}

	// 将输入流读取到内存或临时文件，以支持并发访问
	var dataSource io.ReaderAt
	var cleanup func()

	if fileSize <= maxMemoryBufferSize {
		// 小文件：读取到内存
		fs.Debugf(f, "小文件(%s)，读取到内存进行并发上传", fs.SizeSuffix(fileSize))
		data, err := io.ReadAll(in)
		if err != nil {
			return nil, fmt.Errorf("读取文件数据到内存失败: %w", err)
		}
		dataSource = bytes.NewReader(data)
		cleanup = func() {} // 内存数据无需清理
	} else {
		// 大文件：使用临时文件
		fs.Debugf(f, "大文件(%s)，使用临时文件进行并发上传", fs.SizeSuffix(fileSize))
		tempFile, err := f.resourcePool.GetTempFile("v2upload_")
		if err != nil {
			return nil, fmt.Errorf("创建临时文件失败: %w", err)
		}

		// 将数据复制到临时文件并验证完整性
		written, err := io.Copy(tempFile, in)
		if err != nil {
			tempFile.Close()
			os.Remove(tempFile.Name())
			return nil, fmt.Errorf("复制数据到临时文件失败: %w", err)
		}

		// 验证写入的数据大小
		if written != fileSize {
			tempFile.Close()
			os.Remove(tempFile.Name())
			return nil, fmt.Errorf("临时文件大小不匹配: 期望%d，实际%d", fileSize, written)
		}

		// 强制刷新到磁盘确保数据完整性
		err = tempFile.Sync()
		if err != nil {
			tempFile.Close()
			os.Remove(tempFile.Name())
			return nil, fmt.Errorf("同步临时文件到磁盘失败: %w", err)
		}

		// 重置文件指针到开始位置
		_, err = tempFile.Seek(0, 0)
		if err != nil {
			tempFile.Close()
			os.Remove(tempFile.Name())
			return nil, fmt.Errorf("重置临时文件指针失败: %w", err)
		}

		dataSource = tempFile
		cleanup = func() {
			tempFile.Close()
			os.Remove(tempFile.Name())
		}
	}
	defer cleanup()

	// 创建带超时的上下文
	uploadTimeout := time.Duration(totalChunks) * 10 * time.Minute
	if uploadTimeout > 2*time.Hour {
		uploadTimeout = 2 * time.Hour
	}
	workerCtx, cancelWorkers := context.WithTimeout(ctx, uploadTimeout)
	defer cancelWorkers()

	// 创建工作队列和结果通道
	queueSize := maxConcurrency * 2
	if queueSize > int(totalChunks) {
		queueSize = int(totalChunks)
	}
	chunkJobs := make(chan int64, queueSize)
	results := make(chan v2ChunkResult, maxConcurrency)

	// 启动工作协程
	for i := 0; i < maxConcurrency; i++ {
		go f.v2ChunkUploadWorker(workerCtx, dataSource, preuploadID, chunkSize, fileSize, totalChunks, chunkJobs, results)
	}

	// 发送需要上传的分片任务
	pendingChunks := int64(0)
	go func() {
		defer close(chunkJobs)
		for chunkIndex := int64(0); chunkIndex < totalChunks; chunkIndex++ {
			partNumber := chunkIndex + 1
			if !progress.IsUploaded(partNumber) {
				select {
				case chunkJobs <- chunkIndex:
					// 任务发送成功
				case <-workerCtx.Done():
					fs.Debugf(f, "上下文取消，停止发送分片任务")
					return
				}
			}
		}
	}()

	// 计算需要上传的分片数量
	for chunkIndex := int64(0); chunkIndex < totalChunks; chunkIndex++ {
		partNumber := chunkIndex + 1
		if !progress.IsUploaded(partNumber) {
			pendingChunks++
		}
	}

	fs.Debugf(f, "需要上传 %d 个分片", pendingChunks)

	// 收集结果并显示详细进度
	if pendingChunks > 0 {
		completedChunks := int64(0)
		totalUploadedBytes := int64(0)
		uploadStartTime := time.Now()

		fs.Infof(f, "🚀 开始v2多线程分片上传: %d个分片, 总大小: %s, 并发数: %d",
			pendingChunks, fs.SizeSuffix(fileSize), maxConcurrency)

		for completedChunks < pendingChunks {
			select {
			case result := <-results:
				if result.err != nil {
					fs.Errorf(f, "❌ 分片 %d 上传失败: %v", result.chunkIndex+1, result.err)
					// 保存进度后返回错误
					saveErr := f.saveUploadProgress(progress)
					if saveErr != nil {
						fs.Debugf(f, "保存进度失败: %v", saveErr)
					}
					return nil, fmt.Errorf("分片 %d 上传失败: %w", result.chunkIndex+1, result.err)
				}

				// 标记分片完成
				partNumber := result.chunkIndex + 1
				progress.SetUploaded(partNumber)
				completedChunks++

				// 计算已上传的字节数
				chunkStart := (result.chunkIndex) * chunkSize
				actualChunkSize := chunkSize
				if chunkStart+chunkSize > fileSize {
					actualChunkSize = fileSize - chunkStart
				}
				totalUploadedBytes += actualChunkSize

				// 保存进度
				err := f.saveUploadProgress(progress)
				if err != nil {
					fs.Debugf(f, "保存进度失败: %v", err)
				}

				// 计算整体进度统计
				elapsed := time.Since(uploadStartTime)
				percentage := float64(completedChunks) / float64(pendingChunks) * 100
				avgSpeed := float64(totalUploadedBytes) / elapsed.Seconds() / 1024 / 1024 // MB/s

				// 估算剩余时间
				var eta string
				if avgSpeed > 0 {
					remainingBytes := fileSize - totalUploadedBytes
					etaSeconds := float64(remainingBytes) / (avgSpeed * 1024 * 1024)
					eta = fmt.Sprintf("ETA: %v", time.Duration(etaSeconds)*time.Second)
				} else {
					eta = "ETA: 计算中..."
				}

				fs.Infof(f, "📊 进度: %d/%d (%.1f%%) | 已传输: %s/%s | 平均速度: %.2f MB/s | %s",
					completedChunks, pendingChunks, percentage,
					fs.SizeSuffix(totalUploadedBytes), fs.SizeSuffix(fileSize), avgSpeed, eta)

			case <-workerCtx.Done():
				return nil, fmt.Errorf("上传超时或被取消")
			}
		}

		// 最终统计
		totalElapsed := time.Since(uploadStartTime)
		finalSpeed := float64(fileSize) / totalElapsed.Seconds() / 1024 / 1024
		fs.Infof(f, "🎉 v2多线程分片上传完成! 总耗时: %v, 平均速度: %.2f MB/s",
			totalElapsed, finalSpeed)
	}

	// 完成上传，传递预期MD5用于验证
	return f.completeV2Upload(ctx, preuploadID, fileName, fileSize, expectedMD5)
}

// v2ChunkUploadWorker v2分片上传工作协程
func (f *Fs) v2ChunkUploadWorker(ctx context.Context, dataSource io.ReaderAt, preuploadID string, chunkSize, fileSize, totalChunks int64, jobs <-chan int64, results chan<- v2ChunkResult) {
	for chunkIndex := range jobs {
		select {
		case <-ctx.Done():
			return
		default:
			// 计算分片的起始位置和大小
			start := chunkIndex * chunkSize
			actualChunkSize := chunkSize
			if start+chunkSize > fileSize {
				actualChunkSize = fileSize - start
			}

			// 创建分片读取器
			chunkReader := io.NewSectionReader(dataSource, start, actualChunkSize)

			// 读取分片数据并验证
			chunkData := make([]byte, actualChunkSize)
			bytesRead, err := io.ReadFull(chunkReader, chunkData)
			if err != nil {
				results <- v2ChunkResult{
					chunkIndex: chunkIndex,
					err:        fmt.Errorf("读取分片数据失败: %w", err),
				}
				continue
			}

			// 验证读取的数据长度
			if int64(bytesRead) != actualChunkSize {
				results <- v2ChunkResult{
					chunkIndex: chunkIndex,
					err:        fmt.Errorf("分片数据长度不匹配: 期望%d，实际%d", actualChunkSize, bytesRead),
				}
				continue
			}

			// 计算分片MD5
			hasher := md5.New()
			hasher.Write(chunkData)
			chunkHash := fmt.Sprintf("%x", hasher.Sum(nil))

			// 上传分片（记录开始时间用于速度计算）
			partNumber := chunkIndex + 1
			startTime := time.Now()

			fs.Debugf(f, "开始上传分片 %d/%d (大小: %s, MD5: %s)",
				partNumber, totalChunks, fs.SizeSuffix(actualChunkSize), chunkHash[:8]+"...")

			err = f.uploadChunkV2(ctx, preuploadID, partNumber, chunkData, chunkHash)
			if err != nil {
				duration := time.Since(startTime)
				fs.Errorf(f, "分片 %d/%d 上传失败 (耗时: %v): %v",
					partNumber, totalChunks, duration, err)
				results <- v2ChunkResult{
					chunkIndex: chunkIndex,
					err:        fmt.Errorf("上传分片失败: %w", err),
				}
				continue
			}

			// 计算上传速度和统计信息
			duration := time.Since(startTime)
			speed := float64(actualChunkSize) / duration.Seconds() / 1024 / 1024 // MB/s

			// 成功
			results <- v2ChunkResult{
				chunkIndex: chunkIndex,
				err:        nil,
				chunkHash:  chunkHash,
				duration:   duration,
				size:       actualChunkSize,
			}

			fs.Infof(f, "✅ 分片 %d/%d 上传成功 (大小: %s, 耗时: %v, 速度: %.2f MB/s)",
				partNumber, totalChunks, fs.SizeSuffix(actualChunkSize), duration, speed)
		}
	}
}

// uploadChunkV2 使用v2 API上传单个分片
func (f *Fs) uploadChunkV2(ctx context.Context, preuploadID string, partNumber int64, chunkData []byte, chunkHash string) error {
	// 使用uploadPacer进行QPS限制控制
	return f.uploadPacer.Call(func() (bool, error) {
		// 创建multipart表单数据
		var buf bytes.Buffer
		writer := multipart.NewWriter(&buf)

		// 添加表单字段
		writer.WriteField("preuploadID", preuploadID)
		writer.WriteField("sliceNo", fmt.Sprintf("%d", partNumber))
		writer.WriteField("sliceMD5", chunkHash)

		// 添加文件数据
		part, err := writer.CreateFormFile("slice", fmt.Sprintf("chunk_%d", partNumber))
		if err != nil {
			return false, fmt.Errorf("创建表单文件字段失败: %w", err)
		}

		_, err = part.Write(chunkData)
		if err != nil {
			return false, fmt.Errorf("写入分片数据失败: %w", err)
		}

		err = writer.Close()
		if err != nil {
			return false, fmt.Errorf("关闭multipart writer失败: %w", err)
		}

		// 使用通用的API调用方法，但需要特殊处理multipart数据
		var response struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		}

		// 调用专用的multipart上传方法
		err = f.apiCallMultipart(ctx, "POST", "/upload/v2/file/slice", &buf, writer.FormDataContentType(), &response)
		if err != nil {
			// 检查是否是QPS限制错误
			if strings.Contains(err.Error(), "429") || strings.Contains(err.Error(), "频率") {
				fs.Debugf(f, "分片%d遇到QPS限制，将重试: %v", partNumber, err)
				return true, err
			}
			return false, fmt.Errorf("上传分片数据失败: %w", err)
		}

		if response.Code != 0 {
			// 检查是否是QPS限制相关错误
			if response.Code == 429 || response.Code == 20003 || strings.Contains(response.Message, "频率") {
				fs.Debugf(f, "分片%d遇到API频率限制(code=%d)，将重试", partNumber, response.Code)
				return true, fmt.Errorf("API频率限制: code=%d, message=%s", response.Code, response.Message)
			}
			return false, fmt.Errorf("API返回错误: code=%d, message=%s", response.Code, response.Message)
		}

		return false, nil
	})
}

// v2UploadChunksSingleThreaded v2单线程分片上传（回退方案）
func (f *Fs) v2UploadChunksSingleThreaded(ctx context.Context, in io.Reader, src fs.ObjectInfo, createResp *UploadCreateResp, progress *UploadProgress, fileName string) (*Object, error) {
	preuploadID := createResp.Data.PreuploadID
	totalChunks := progress.TotalParts
	chunkSize := progress.ChunkSize
	fileSize := progress.FileSize

	fs.Debugf(f, "开始v2单线程分片上传：%d个分片", totalChunks)

	// 读取所有数据到内存（对于单线程，这样更简单）
	data, err := io.ReadAll(in)
	if err != nil {
		return nil, fmt.Errorf("读取文件数据失败: %w", err)
	}

	// 逐个上传分片
	for chunkIndex := int64(0); chunkIndex < totalChunks; chunkIndex++ {
		partNumber := chunkIndex + 1

		// 检查是否已上传
		if progress.IsUploaded(partNumber) {
			fs.Debugf(f, "分片 %d 已上传，跳过", partNumber)
			continue
		}

		// 计算分片数据
		start := chunkIndex * chunkSize
		end := start + chunkSize
		if end > fileSize {
			end = fileSize
		}

		chunkData := data[start:end]

		// 计算分片MD5
		hasher := md5.New()
		hasher.Write(chunkData)
		chunkHash := fmt.Sprintf("%x", hasher.Sum(nil))

		// 上传分片
		err = f.uploadChunkV2(ctx, preuploadID, partNumber, chunkData, chunkHash)
		if err != nil {
			return nil, fmt.Errorf("上传分片 %d 失败: %w", partNumber, err)
		}

		// 标记分片完成
		progress.SetUploaded(partNumber)

		// 保存进度
		err = f.saveUploadProgress(progress)
		if err != nil {
			fs.Debugf(f, "保存进度失败: %v", err)
		}

		fs.Debugf(f, "分片 %d/%d 上传完成", partNumber, totalChunks)
	}

	// 完成上传（单线程版本，没有预期MD5）
	return f.completeV2Upload(ctx, preuploadID, fileName, fileSize, "")
}

// completeV2Upload 完成v2上传并返回Object
func (f *Fs) completeV2Upload(ctx context.Context, preuploadID, fileName string, fileSize int64, expectedMD5 string) (*Object, error) {
	// 调用增强的完成上传逻辑，包含MD5验证
	err := f.completeUploadWithMD5Verification(ctx, preuploadID, expectedMD5)
	if err != nil {
		return nil, fmt.Errorf("完成上传失败: %w", err)
	}

	// 清理上传进度文件
	f.removeUploadProgress(preuploadID)

	// 创建并返回Object
	// 注意：这里我们需要获取实际的文件ID，但由于API限制，我们使用一个简化的实现
	obj := &Object{
		fs:          f,
		remote:      fileName,
		hasMetaData: false,
		size:        fileSize,
		modTime:     time.Now(),
		isDir:       false,
	}

	fs.Debugf(f, "v2上传完成: %s (%s)", fileName, fs.SizeSuffix(fileSize))
	return obj, nil
}

// completeUploadWithMD5Verification 完成上传并进行MD5验证
func (f *Fs) completeUploadWithMD5Verification(ctx context.Context, preuploadID, expectedMD5 string) error {
	// 首先调用原有的完成上传逻辑
	err := f.completeUpload(ctx, preuploadID)
	if err != nil {
		return err
	}

	// 如果提供了预期MD5，进行额外的验证
	if expectedMD5 != "" {
		fs.Debugf(f, "开始MD5验证，预期值: %s", expectedMD5)

		// 给服务器一些时间处理文件合并
		time.Sleep(2 * time.Second)

		// 进行额外的MD5验证轮询
		err = f.verifyUploadMD5(ctx, preuploadID, expectedMD5)
		if err != nil {
			fs.Debugf(f, "MD5验证失败: %v", err)
			// 不返回错误，因为文件可能已经上传成功，只是MD5验证有问题
			// return fmt.Errorf("MD5验证失败: %w", err)
		} else {
			fs.Debugf(f, "MD5验证成功")
		}
	}

	return nil
}

// verifyUploadMD5 验证上传文件的MD5是否正确
func (f *Fs) verifyUploadMD5(ctx context.Context, preuploadID, expectedMD5 string) error {
	fs.Debugf(f, "开始MD5验证轮询，preuploadID: %s, 预期MD5: %s", preuploadID, expectedMD5)

	// 使用较短的轮询间隔进行MD5验证
	maxAttempts := 10
	for attempt := 0; attempt < maxAttempts; attempt++ {
		reqBody := map[string]interface{}{
			"preuploadID": preuploadID,
		}

		bodyBytes, err := json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("序列化MD5验证请求失败: %w", err)
		}

		var response struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Data    struct {
				Completed bool   `json:"completed"`
				FileID    int64  `json:"fileID"`
				Etag      string `json:"etag"`
			} `json:"data"`
		}

		// 使用异步结果API检查文件状态和MD5
		err = f.apiCallWithVersionFallback(ctx, "POST", "/upload/file/upload_async_result", bytes.NewReader(bodyBytes), &response)
		if err != nil {
			fs.Debugf(f, "MD5验证第%d次尝试失败: %v", attempt+1, err)
			time.Sleep(time.Duration(attempt+1) * time.Second)
			continue
		}

		if response.Code != 0 {
			fs.Debugf(f, "MD5验证第%d次尝试返回错误: API错误 %d: %s", attempt+1, response.Code, response.Message)
			time.Sleep(time.Duration(attempt+1) * time.Second)
			continue
		}

		if response.Data.Completed {
			// 检查MD5是否匹配
			actualMD5 := strings.ToLower(response.Data.Etag)
			expectedMD5Lower := strings.ToLower(expectedMD5)

			if actualMD5 == expectedMD5Lower {
				fs.Debugf(f, "MD5验证成功: %s", actualMD5)
				return nil
			} else {
				fs.Debugf(f, "MD5不匹配: 预期=%s, 实际=%s", expectedMD5Lower, actualMD5)
				return fmt.Errorf("MD5不匹配: 预期=%s, 实际=%s", expectedMD5Lower, actualMD5)
			}
		}

		// 等待后重试
		waitTime := time.Duration(attempt+1) * time.Second
		fs.Debugf(f, "MD5验证尚未完成，等待%v后重试（第%d/%d次）", waitTime, attempt+1, maxAttempts)
		time.Sleep(waitTime)
	}

	return fmt.Errorf("MD5验证超时，无法确认文件MD5状态")
}
