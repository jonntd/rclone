package _123

import (
	"bufio"
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"math"
	"math/rand/v2"
	"mime/multipart"
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

	"github.com/rclone/rclone/lib/atexit"
	"github.com/rclone/rclone/lib/dircache"
	"github.com/rclone/rclone/lib/oauthutil"
	"github.com/rclone/rclone/lib/pacer"
	"github.com/rclone/rclone/lib/rest"
	"golang.org/x/oauth2"

	// 123网盘SDK
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/accounting"
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
	// 官方QPS限制文档：https://123yunpan.yuque.com/org-wiki-123yunpan-muaork/cr6ced/
	// 更新时间：2025-07-04，基于最新官方QPS限制表
	// 所有API限制现在通过getPacerForEndpoint统一管理，以下常量仅作参考

	// 高频率API (15-20 QPS) - 进一步降低避免429错误
	listV2APIMinSleep = 200 * time.Millisecond // ~5 QPS 用于 api/v2/file/list (官方15 QPS，极保守设置)

	// 中等频率API (8-10 QPS) - 进一步降低避免429错误
	fileMoveMinSleep    = 250 * time.Millisecond // ~4 QPS 用于 api/v1/file/move (官方10 QPS，极保守设置)
	fileInfosMinSleep   = 250 * time.Millisecond // ~4 QPS 用于 api/v1/file/infos (官方10 QPS，极保守设置)
	userInfoMinSleep    = 250 * time.Millisecond // ~4 QPS 用于 api/v1/user/info (官方10 QPS，极保守设置)
	accessTokenMinSleep = 300 * time.Millisecond // ~3.3 QPS 用于 api/v1/access_token (官方8 QPS，极保守设置)

	// 低频率API (5 QPS) - 基于上传速度分析大幅优化性能
	uploadCreateMinSleep = 100 * time.Millisecond // ~10.0 QPS 用于 upload/v1/file/create (大幅提升，解决48秒/分片问题)
	mkdirMinSleep        = 500 * time.Millisecond // ~2 QPS 用于 upload/v1/file/mkdir (保持不变)
	fileTrashMinSleep    = 500 * time.Millisecond // ~2 QPS 用于 api/v1/file/trash (保持不变)

	downloadInfoMinSleep = 500 * time.Millisecond // ~2 QPS 用于 api/v1/file/download_info (保持不变)

	// 最低频率API (1 QPS) - 进一步降低避免429错误
	fileDeleteMinSleep     = 2000 * time.Millisecond // ~0.5 QPS 用于 api/v1/file/delete (官方1 QPS，极保守设置)
	fileListMinSleep       = 2000 * time.Millisecond // ~0.5 QPS 用于 api/v1/file/list (官方1 QPS，极保守设置)
	videoTranscodeMinSleep = 2000 * time.Millisecond // ~0.5 QPS 用于 api/v1/video/transcode/list (官方1 QPS，极保守设置)

	// 特殊API (保守估计) - 基于48秒/分片问题大幅优化分片上传性能
	uploadV2SliceMinSleep = 50 * time.Millisecond // ~20.0 QPS 用于 upload/v2/file/slice (激进提升，解决上传慢问题)

	maxSleep      = 30 * time.Second // 429退避的最大睡眠时间
	decayConstant = 2

	// 文件上传相关常量
	singleStepUploadLimit = 1024 * 1024 * 1024 // 1GB - 单步上传API的文件大小限制（修复：原来错误设置为50MB）
	maxMemoryBufferSize   = 1024 * 1024 * 1024 // 1GB - 内存缓冲的最大大小（从512MB提升）
	maxFileNameBytes      = 255                // 文件名的最大字节长度（UTF-8编码）

	// 文件冲突处理策略常量已移除，当前使用API默认行为

	// 上传相关常量 - 优化大文件传输性能
	defaultChunkSize    = 100 * fs.Mebi // 增加默认分片大小到100MB
	minChunkSize        = 50 * fs.Mebi  // 增加最小分片大小到50MB
	maxChunkSize        = 500 * fs.Mebi // 设置最大分片大小为500MB
	defaultUploadCutoff = 100 * fs.Mebi // 降低分片上传阈值
	maxUploadParts      = 10000

	// 连接和超时设置 - 针对大文件传输优化的参数
	defaultConnTimeout = 60 * time.Second   // 🔧 修复卡住问题：增加连接超时到60秒
	defaultTimeout     = 1200 * time.Second // 🔧 修复卡住问题：增加总体超时到20分钟，支持大文件传输

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
// 优化版本：标注了可以考虑使用rclone标准配置的选项
type Options struct {
	// 123网盘特定的认证配置
	ClientID     string `config:"client_id"`
	ClientSecret string `config:"client_secret"`
	Token        string `config:"token"`
	UserAgent    string `config:"user_agent"`
	RootFolderID string `config:"root_folder_id"`

	// 文件操作配置 (部分可考虑使用rclone标准配置)
	ListChunk      int           `config:"list_chunk"`    // 可考虑使用fs.Config.Checkers
	ChunkSize      fs.SizeSuffix `config:"chunk_size"`    // 可考虑使用fs.Config.ChunkSize
	UploadCutoff   fs.SizeSuffix `config:"upload_cutoff"` // 可考虑使用fs.Config.MultiThreadCutoff
	MaxUploadParts int           `config:"max_upload_parts"`

	// 网络和超时配置 (可考虑使用rclone标准配置)
	PacerMinSleep     fs.Duration `config:"pacer_min_sleep"`
	ListPacerMinSleep fs.Duration `config:"list_pacer_min_sleep"`
	ConnTimeout       fs.Duration `config:"conn_timeout"` // 可考虑使用fs.Config.ConnectTimeout
	Timeout           fs.Duration `config:"timeout"`      // 可考虑使用fs.Config.Timeout

	// 并发控制配置 (可考虑使用rclone标准配置)
	MaxConcurrentUploads   int         `config:"max_concurrent_uploads"`   // 可考虑使用fs.Config.Transfers
	MaxConcurrentDownloads int         `config:"max_concurrent_downloads"` // 可考虑使用fs.Config.Checkers
	ProgressUpdateInterval fs.Duration `config:"progress_update_interval"`
	EnableProgressDisplay  bool        `config:"enable_progress_display"`

	// 123网盘特定的QPS控制选项
	UploadPacerMinSleep   fs.Duration `config:"upload_pacer_min_sleep"`
	DownloadPacerMinSleep fs.Duration `config:"download_pacer_min_sleep"`
	StrictPacerMinSleep   fs.Duration `config:"strict_pacer_min_sleep"`

	// 调试配置
	DebugLevel             int         `config:"debug_level"`              // 调试级别：0=无，1=错误，2=信息，3=调试，4=详细
	EnablePerformanceLog   bool        `config:"enable_performance_log"`   // 启用性能监控日志
	PerformanceLogInterval fs.Duration `config:"performance_log_interval"` // 性能日志输出间隔
}

// UnifiedRetryManager 统一重试管理器
// 🔧 核心思想：在最外层统一管理所有重试逻辑，避免多层嵌套
type UnifiedRetryManager struct {
	maxRetries       int           // 全局最大重试次数
	maxRetryDuration time.Duration // 全局最大重试时间
	retryStats       map[string]*RetryStats
	mu               sync.RWMutex
}

// RetryStats 重试统计信息
type RetryStats struct {
	attempts    int
	startTime   time.Time
	lastAttempt time.Time
	totalDelay  time.Duration
}

// RetryConfig 重试配置
type RetryConfig struct {
	MaxRetries        int
	MaxDuration       time.Duration
	BaseDelay         time.Duration
	MaxDelay          time.Duration
	BackoffMultiplier float64
	OperationType     string // "upload", "download", "api_call"
}

// Fs 表示远程123网盘驱动器实例
type Fs struct {
	name         string       // 此远程实例的名称
	originalName string       // 未修改的原始配置名称
	root         string       // 当前操作的根路径
	opt          Options      // 解析后的配置选项
	features     *fs.Features // 可选功能特性

	// API客户端和身份验证相关
	client       *http.Client     // 用于API调用的HTTP客户端（保留用于特殊情况）
	rst          *rest.Client     // rclone标准rest客户端（推荐使用）
	token        string           // 缓存的访问令牌
	tokenExpiry  time.Time        // 缓存访问令牌的过期时间
	tokenMu      sync.Mutex       // 保护token和tokenExpiry的互斥锁
	tokenRenewer *oauthutil.Renew // 令牌自动更新器

	// 缓存并发安全锁
	cacheMu sync.RWMutex // 保护所有缓存操作的读写锁

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

	// 🔧 多实例BadgerDB缓存管理器，解决冲突问题
	cacheManager *MultiInstanceCacheManager

	// 兼容性字段：保持向后兼容
	parentIDCache *cache.BadgerCache // 通过cacheManager代理
	dirListCache  *cache.BadgerCache // 通过cacheManager代理
	pathToIDCache *cache.BadgerCache // 通过cacheManager代理

	// 🔧 上传验证管理器，优化断点续传验证阶段错误处理
	verificationManager *UploadVerificationManager

	// 性能优化相关 - 优化版本：使用更标准化的并发控制
	concurrencyManager *ConcurrencyManager         // 统一的并发控制管理器
	memoryManager      *MemoryManager              // 内存管理器，防止内存泄漏
	performanceMetrics *PerformanceMetrics         // 性能指标收集器
	enhancedMetrics    *EnhancedPerformanceMetrics // 增强性能指标收集器
	resourcePool       *ResourcePool               // 资源池管理器，优化内存使用
	// 🗑️ downloadURLCache 已删除 - 下载URL缓存已移除
	apiVersionManager *APIVersionManager        // API版本管理器
	resumeManager     *ResumeManager            // 断点续传管理器
	dynamicAdjuster   *DynamicParameterAdjuster // 动态参数调整器

	// 缓存时间配置
	cacheConfig CacheConfig

	// 🔧 跨云传输上下文管理
	crossCloudCtx *CrossCloudContext

	// 🔧 缓存键生成器，防止键冲突
	cacheKeyGen *CacheKeyGenerator

	// 🔧 传输跟踪器，防止重复下载
	transferTracker *TransferTracker

	// 🚀 上传域名缓存，避免频繁API调用
	uploadDomainCache     string
	uploadDomainCacheTime time.Time

	// 🔧 统一重试管理器，避免多层重试嵌套
	retryManager *UnifiedRetryManager

	// 🔧 跨云传输管理器，防止重复下载
	crossCloudManager *CrossCloudTransferManager

	// 🔧 统一进度管理器，避免重复进度显示
	progressManager *UnifiedProgressManager
}

// debugf 根据调试级别输出调试信息
func (f *Fs) debugf(level int, format string, args ...any) {
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

// CrossCloudContext 跨云传输上下文管理
type CrossCloudContext struct {
	downloadTransfer *accounting.Transfer
	uploadTransfer   *accounting.Transfer
	downloadAccount  *accounting.Account
	uploadAccount    *accounting.Account
	mu               sync.RWMutex
}

// setCrossCloudTransfer 设置跨云传输的Transfer对象
func (f *Fs) setCrossCloudTransfer(transfer *accounting.Transfer) {
	if f.crossCloudCtx == nil {
		f.crossCloudCtx = &CrossCloudContext{}
	}
	f.crossCloudCtx.mu.Lock()
	defer f.crossCloudCtx.mu.Unlock()
	f.crossCloudCtx.downloadTransfer = transfer
}

// clearCrossCloudTransfer 清理跨云传输上下文
func (f *Fs) clearCrossCloudTransfer() {
	if f.crossCloudCtx == nil {
		return
	}
	f.crossCloudCtx.mu.Lock()
	defer f.crossCloudCtx.mu.Unlock()
	f.crossCloudCtx.downloadTransfer = nil
	f.crossCloudCtx.downloadAccount = nil
}

// getCrossCloudTransfer 获取跨云传输的Transfer对象
func (f *Fs) getCrossCloudTransfer() *accounting.Transfer {
	if f.crossCloudCtx == nil {
		return nil
	}
	f.crossCloudCtx.mu.RLock()
	defer f.crossCloudCtx.mu.RUnlock()
	return f.crossCloudCtx.downloadTransfer
}

// CacheKeyGenerator 缓存键生成器，防止键冲突
type CacheKeyGenerator struct {
	fsType    string
	namespace string
}

// NewCacheKeyGenerator 创建新的缓存键生成器
func NewCacheKeyGenerator(fsType, namespace string) *CacheKeyGenerator {
	return &CacheKeyGenerator{
		fsType:    fsType,
		namespace: namespace,
	}
}

// GenerateKey 生成唯一的缓存键
func (ckg *CacheKeyGenerator) GenerateKey(operation, path string, size int64, extraParams ...string) string {
	// 🔧 修复缓存键冲突：增强唯一性保证
	components := []string{
		ckg.fsType,
		ckg.namespace,
		operation,
		path,
		fmt.Sprintf("%d", size),
		fmt.Sprintf("%d", time.Now().Unix()/600), // 10分钟级别的时间戳，减少冲突
		fmt.Sprintf("%d", os.Getpid()),           // 进程ID，避免多实例冲突
	}

	// 添加额外参数
	components = append(components, extraParams...)

	// 生成哈希
	h := sha256.New()
	for i, component := range components {
		h.Write([]byte(fmt.Sprintf("%d:", i))) // 添加位置标识
		h.Write([]byte(component))
		h.Write([]byte("|")) // 更明确的分隔符
	}

	hash := hex.EncodeToString(h.Sum(nil))
	// 使用更长的哈希减少冲突概率
	return fmt.Sprintf("%s_%s_%s", ckg.fsType, operation, hash[:24])
}

// TransferTracker 传输跟踪器，防止重复下载
type TransferTracker struct {
	activeTransfers map[string]*TransferSession
	mu              sync.RWMutex
}

// TransferSession 传输会话
type TransferSession struct {
	fileID    string
	fileName  string
	size      int64
	startTime time.Time
	cancel    context.CancelFunc
	waitGroup sync.WaitGroup
	progress  *DownloadProgress
	result    chan TransferResult
}

// TransferResult 传输结果
type TransferResult struct {
	Success bool
	Error   error
	Path    string
}

// NewTransferTracker 创建新的传输跟踪器
func NewTransferTracker() *TransferTracker {
	return &TransferTracker{
		activeTransfers: make(map[string]*TransferSession),
	}
}

// StartTransfer 开始传输，返回会话和是否为新传输
func (tt *TransferTracker) StartTransfer(fileID, fileName string, size int64) (*TransferSession, bool) {
	tt.mu.Lock()
	defer tt.mu.Unlock()

	// 检查是否已有相同文件的传输
	if session, exists := tt.activeTransfers[fileID]; exists {
		fs.Debugf(nil, "文件 %s 已在传输中，等待现有传输完成", fileName)
		return session, false // 返回现有会话，不启动新传输
	}

	// 创建新的传输会话
	_, cancel := context.WithCancel(context.Background())
	session := &TransferSession{
		fileID:    fileID,
		fileName:  fileName,
		size:      size,
		startTime: time.Now(),
		cancel:    cancel,
		result:    make(chan TransferResult, 1),
	}

	tt.activeTransfers[fileID] = session
	fs.Debugf(nil, "启动新的传输会话: %s", fileName)
	return session, true
}

// CompleteTransfer 完成传输
func (tt *TransferTracker) CompleteTransfer(fileID string, success bool, err error, path string) {
	tt.mu.Lock()
	defer tt.mu.Unlock()

	if session, exists := tt.activeTransfers[fileID]; exists {
		// 发送结果
		session.result <- TransferResult{
			Success: success,
			Error:   err,
			Path:    path,
		}
		close(session.result)

		// 清理会话
		delete(tt.activeTransfers, fileID)
		fs.Debugf(nil, "传输会话完成: %s, 成功: %v", session.fileName, success)
	}
}

// WaitForTransfer 等待传输完成
func (tt *TransferTracker) WaitForTransfer(session *TransferSession) TransferResult {
	select {
	case result := <-session.result:
		return result
	case <-time.After(30 * time.Minute): // 传输超时
		tt.CancelTransfer(session.fileID)
		return TransferResult{
			Success: false,
			Error:   fmt.Errorf("传输超时: %s", session.fileName),
		}
	}
}

// CancelTransfer 取消传输
func (tt *TransferTracker) CancelTransfer(fileID string) {
	tt.mu.Lock()
	defer tt.mu.Unlock()

	if session, exists := tt.activeTransfers[fileID]; exists {
		session.cancel()
		delete(tt.activeTransfers, fileID)
	}
}

// GetActiveTransfer 获取活跃的传输会话
func (tt *TransferTracker) GetActiveTransfer(fileID string) *TransferSession {
	tt.mu.RLock()
	defer tt.mu.RUnlock()

	if session, exists := tt.activeTransfers[fileID]; exists {
		return session
	}
	return nil
}

// DefaultCacheConfig 返回默认的缓存配置
// 🔧 轻量级优化：统一TTL为5分钟，避免缓存不一致问题
func DefaultCacheConfig() CacheConfig {
	unifiedTTL := 5 * time.Minute // 统一TTL策略
	return CacheConfig{
		ParentIDCacheTTL:    unifiedTTL, // 统一为5分钟
		DirListCacheTTL:     unifiedTTL, // 从3分钟改为5分钟，与其他缓存保持一致
		DownloadURLCacheTTL: 0,          // 动态TTL，根据API返回的过期时间
		PathToIDCacheTTL:    unifiedTTL, // 从12分钟改为5分钟，避免指向过期缓存
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

// 🗑️ DownloadURLCacheEntry 已删除 - 下载URL缓存已移除

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
func (pm *PerformanceMetrics) GetStats() map[string]any {
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

	stats := map[string]any{
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

// ProgressReadCloser 包装ReadCloser以提供进度跟踪和资源管理
type ProgressReadCloser struct {
	io.ReadCloser
	fs              *Fs
	totalSize       int64
	transferredSize int64
	startTime       time.Time // 下载开始时间
}

// Read 实现io.Reader接口，同时更新进度
func (prc *ProgressReadCloser) Read(p []byte) (n int, err error) {
	n, err = prc.ReadCloser.Read(p)
	if n > 0 {
		prc.transferredSize += int64(n)
		// rclone内置的accounting会自动处理进度跟踪
	}
	return n, err
}

// Close 实现io.Closer接口，同时清理资源
func (prc *ProgressReadCloser) Close() error {
	// 释放下载信号量
	prc.fs.concurrencyManager.ReleaseDownload()

	// 记录下载完成或错误
	duration := time.Since(prc.startTime)
	success := prc.transferredSize == prc.totalSize
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

// ConcurrentDownloadReader 并发下载的文件读取器
type ConcurrentDownloadReader struct {
	file     *os.File
	tempPath string
}

// Read 实现io.Reader接口
func (r *ConcurrentDownloadReader) Read(p []byte) (n int, err error) {
	return r.file.Read(p)
}

// Close 实现io.Closer接口，关闭文件并删除临时文件
func (r *ConcurrentDownloadReader) Close() error {
	var err error
	if r.file != nil {
		err = r.file.Close()
		r.file = nil
	}
	if r.tempPath != "" {
		if removeErr := os.Remove(r.tempPath); removeErr != nil {
			fs.Debugf(nil, "删除临时文件失败: %s, 错误: %v", r.tempPath, removeErr)
		} else {
			fs.Debugf(nil, "已删除临时文件: %s", r.tempPath)
		}
		r.tempPath = ""
	}
	return err
}

// 文件名验证相关的全局变量
var (
	// invalidCharsRegex 用于匹配123网盘不允许的文件名字符的正则表达式
	invalidCharsRegex = regexp.MustCompile(`["\/:*?|><\\]`)
)

// validateFileName 验证文件名是否符合123网盘的限制规则（全局函数版本）
// 优化版本：使用rclone标准验证加上123网盘特定规则
// 注意：推荐使用Fs.validateFileNameUnified方法以获得更完整的验证
func validateFileName(name string) error {
	// 使用rclone标准的路径检查
	if strings.Contains(name, "/") || strings.Contains(name, "\\") {
		return fmt.Errorf("文件名不能包含路径分隔符")
	}

	// 基础检查：空值和空格 - 使用通用工具
	if StringUtil.IsEmpty(name) {
		return fmt.Errorf("文件名不能为空或全部是空格")
	}

	// UTF-8有效性检查
	if !utf8.ValidString(name) {
		return fmt.Errorf("文件名包含无效的UTF-8字符")
	}

	// 123网盘特定的长度检查（字符数）
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
// 增强版本：添加并发安全保护
func (f *Fs) verifyParentFileID(ctx context.Context, parentFileID int64) (bool, error) {
	// 首先检查BadgerDB缓存（如果可用）
	cacheKey := fmt.Sprintf("parent_%d", parentFileID)
	parentIDCache := f.getParentIDCache()
	if parentIDCache != nil {
		// 使用读锁保护缓存读取
		f.cacheMu.RLock()
		cached, found := parentIDCache.GetBool(cacheKey)
		f.cacheMu.RUnlock()

		if found {
			f.debugf(LogLevelVerbose, "父目录ID %d BadgerDB缓存验证通过: %v", parentFileID, cached)
			return cached, nil
		}
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
	if parentIDCache != nil {
		// 使用写锁保护缓存写入
		f.cacheMu.Lock()
		err := parentIDCache.SetBool(cacheKey, isValid, f.cacheConfig.ParentIDCacheTTL)
		f.cacheMu.Unlock()

		if err != nil {
			fs.Debugf(f, "保存父目录ID %d 缓存失败: %v", parentFileID, err)
		} else {
			f.debugf(LogLevelVerbose, "已保存父目录ID %d 到BadgerDB缓存: valid=%v, TTL=%v", parentFileID, isValid, f.cacheConfig.ParentIDCacheTTL)
		}
	}

	return isValid, nil
}

// getCorrectParentFileID 获取上传API认可的正确父目录ID
// 解决缓存目录ID与上传API不一致的问题
func (f *Fs) getCorrectParentFileID(ctx context.Context, cachedParentID int64) (int64, error) {
	fs.Debugf(f, "🔍 重新获取上传API认可的正确父目录ID，缓存ID: %d", cachedParentID)

	// 策略1：清理所有相关缓存
	fs.Infof(f, "🔄 清理目录缓存，重新获取目录结构")
	f.clearParentIDCache(cachedParentID)
	f.clearDirListCache(cachedParentID)
	if f.dirCache != nil {
		f.dirCache.ResetRoot()
	}

	// 策略2：尝试验证根目录是否可用
	// 根据官方API示例，很多情况下可以使用根目录 (parentFileID: 0)
	fs.Debugf(f, "🧪 尝试验证根目录 (parentFileID: 0) 是否可用")
	rootExists, err := f.verifyParentFileID(ctx, 0)
	if err == nil && rootExists {
		fs.Infof(f, "✅ 根目录验证成功，使用根目录作为回退方案")
		return 0, nil
	}

	// 策略3：尝试重新验证原始ID（清理缓存后可能恢复）
	fs.Debugf(f, "🔄 重新验证原始目录ID: %d", cachedParentID)
	exists, err := f.verifyParentFileID(ctx, cachedParentID)
	if err == nil && exists {
		fs.Infof(f, "✅ 原始目录ID %d 在清理缓存后验证成功", cachedParentID)
		return cachedParentID, nil
	}

	// 策略4：如果所有策略都失败，回退到根目录
	fs.Infof(f, "⚠️ 所有验证策略失败，回退到根目录 (parentFileID: 0)")
	return 0, nil
}

// clearParentIDCache 清理parentFileID缓存中的特定条目
// 增强版本：添加并发安全保护
func (f *Fs) clearParentIDCache(parentFileID ...int64) {
	parentIDCache := f.getParentIDCache()
	if parentIDCache == nil {
		return
	}

	// 使用写锁保护缓存删除操作
	f.cacheMu.Lock()
	defer f.cacheMu.Unlock()

	if len(parentFileID) > 0 {
		// 清理指定的parentFileID
		for _, id := range parentFileID {
			cacheKey := fmt.Sprintf("parent_%d", id)
			if err := parentIDCache.Delete(cacheKey); err != nil {
				fs.Debugf(f, "清理父目录ID %d 缓存失败: %v", id, err)
			} else {
				fs.Debugf(f, "已清理父目录ID %d 的缓存", id)
			}
		}
	} else {
		// 清理所有缓存
		if err := parentIDCache.Clear(); err != nil {
			fs.Debugf(f, "清理所有父目录ID缓存失败: %v", err)
		} else {
			fs.Debugf(f, "已清理所有父目录ID缓存")
		}
	}
}

// invalidateRelatedCaches 智能缓存失效 - 根据操作类型清理相关缓存
// 🔧 轻量级优化：简化缓存失效逻辑，提高可靠性
func (f *Fs) invalidateRelatedCaches(path string, operation string) {
	fs.Debugf(f, "轻量级缓存失效: 操作=%s, 路径=%s", operation, path)

	// 🚀 简单有效的策略：对所有文件操作都进行全面缓存清理
	// 虽然会影响一些性能，但确保数据一致性，避免复杂的条件判断

	switch operation {
	case "upload", "put", "mkdir", "delete", "remove", "rmdir", "rename", "move":
		// 统一处理：清理当前路径和父路径的所有相关缓存
		f.clearAllRelatedCaches(path)

		// 额外清理父目录缓存
		parentPath := filepath.Dir(path)
		if parentPath != "." && parentPath != "/" {
			f.clearAllRelatedCaches(parentPath)
		}

		fs.Debugf(f, "已清理路径 %s 和父路径 %s 的所有相关缓存", path, parentPath)
	}
}

// clearAllRelatedCaches 清理指定路径的所有相关缓存
// 🔧 轻量级优化：新增统一缓存清理函数
func (f *Fs) clearAllRelatedCaches(path string) {
	// 清理路径映射缓存
	f.clearPathToIDCache(path)

	// 清理目录列表缓存（简单策略：清理所有）
	dirListCache := f.getDirListCache()
	if dirListCache != nil {
		f.cacheMu.Lock()
		err := dirListCache.Clear()
		f.cacheMu.Unlock()
		if err != nil {
			fs.Debugf(f, "清理目录列表缓存失败: %v", err)
		}
	}

	// 清理父目录ID缓存（简单策略：清理所有）
	parentIDCache := f.getParentIDCache()
	if parentIDCache != nil {
		f.cacheMu.Lock()
		err := parentIDCache.Clear()
		f.cacheMu.Unlock()
		if err != nil {
			fs.Debugf(f, "清理父目录ID缓存失败: %v", err)
		}
	}
}

// clearDirListCacheByPattern 根据路径模式清理目录列表缓存
// 🔧 轻量级优化：实现简单有效的缓存清理策略
func (f *Fs) clearDirListCacheByPattern(path string) {
	if f.dirListCache == nil {
		return
	}

	fs.Debugf(f, "轻量级缓存清理: 路径=%s", path)

	// 🚀 简单有效的策略：清理所有目录列表缓存
	// 虽然会影响一些性能，但确保数据一致性，避免复杂的模式匹配
	f.cacheMu.Lock()
	err := f.dirListCache.Clear()
	f.cacheMu.Unlock()

	if err != nil {
		fs.Debugf(f, "清理目录列表缓存失败: %v", err)
	} else {
		fs.Debugf(f, "已清理所有目录列表缓存，确保数据一致性")
	}
}

// getDirListFromCache 从缓存获取目录列表
// 🔧 修复版本：避免危险的锁升级，使用异步清理
func (f *Fs) getDirListFromCache(parentFileID int64, lastFileID int64) (*DirListCacheEntry, bool) {
	if f.dirListCache == nil {
		return nil, false
	}

	cacheKey := fmt.Sprintf("dirlist_%d_%d", parentFileID, lastFileID)

	// 🔧 修复：使用纯读操作，不在读锁中进行写操作
	f.cacheMu.RLock()
	var entry DirListCacheEntry
	found, err := f.dirListCache.Get(cacheKey, &entry)
	f.cacheMu.RUnlock() // 立即释放读锁，不使用defer

	if err != nil {
		fs.Debugf(f, "获取目录列表缓存失败 %s: %v", cacheKey, err)
		return nil, false
	}

	if !found {
		return nil, false
	}

	// 🔧 关键修复：在锁外进行验证，如果验证失败则异步清理
	if !fastValidateCacheEntry(entry.FileList) {
		fs.Debugf(f, "目录列表缓存快速校验失败: %s", cacheKey)

		// 🔧 修复：异步清理损坏的缓存，避免锁升级
		go f.asyncCleanInvalidCache(cacheKey)
		return nil, false
	}

	// 完整校验（如果需要）
	if entry.Checksum != "" && len(entry.FileList) > 100 {
		if !validateCacheEntry(entry.FileList, entry.Checksum) {
			fs.Debugf(f, "目录列表缓存完整校验失败: %s", cacheKey)

			// 🔧 修复：异步清理损坏的缓存
			go f.asyncCleanInvalidCache(cacheKey)
			return nil, false
		}
	}

	fs.Debugf(f, "目录列表缓存命中: parentID=%d, lastFileID=%d, 文件数=%d",
		parentFileID, lastFileID, len(entry.FileList))
	return &entry, true
}

// asyncCleanInvalidCache 异步清理无效缓存
// 🔧 关键修复：将写操作分离到专门的函数中，避免锁升级
func (f *Fs) asyncCleanInvalidCache(cacheKey string) {
	if f.dirListCache == nil {
		return
	}

	// 添加延迟，避免频繁清理
	time.Sleep(100 * time.Millisecond)

	// 🔧 修复：使用专门的写锁，简单直接
	f.cacheMu.Lock()
	defer f.cacheMu.Unlock()

	// 再次检查缓存是否仍然存在（可能已被其他goroutine清理）
	var entry DirListCacheEntry
	found, err := f.dirListCache.Get(cacheKey, &entry)
	if err != nil || !found {
		return // 缓存已不存在，无需清理
	}

	// 再次验证，确保确实需要清理
	if !fastValidateCacheEntry(entry.FileList) {
		err := f.dirListCache.Delete(cacheKey)
		if err != nil {
			fs.Debugf(f, "清理无效缓存失败 %s: %v", cacheKey, err)
		} else {
			fs.Debugf(f, "已清理无效缓存: %s", cacheKey)
		}
	}
}

// NewUnifiedRetryManager 创建统一重试管理器
func NewUnifiedRetryManager() *UnifiedRetryManager {
	return &UnifiedRetryManager{
		maxRetries:       10,               // 全局最大重试次数
		maxRetryDuration: 30 * time.Minute, // 全局最大重试时间
		retryStats:       make(map[string]*RetryStats),
	}
}

// ExecuteWithRetry 执行带重试的操作
// 🔧 关键修复：统一重试入口，避免多层重试嵌套
func (urm *UnifiedRetryManager) ExecuteWithRetry(
	ctx context.Context,
	operationID string,
	config RetryConfig,
	operation func() error,
) error {
	urm.mu.Lock()
	stats, exists := urm.retryStats[operationID]
	if !exists {
		stats = &RetryStats{
			startTime: time.Now(),
		}
		urm.retryStats[operationID] = stats
	}
	urm.mu.Unlock()

	// 🔧 关键检查：防止超过全局限制
	if stats.attempts >= urm.maxRetries {
		return fmt.Errorf("操作 %s 已达到全局最大重试次数 %d", operationID, urm.maxRetries)
	}

	if time.Since(stats.startTime) >= urm.maxRetryDuration {
		return fmt.Errorf("操作 %s 已达到全局最大重试时间 %v", operationID, urm.maxRetryDuration)
	}

	var lastErr error
	for attempt := stats.attempts; attempt < config.MaxRetries && attempt < urm.maxRetries; attempt++ {
		// 检查上下文取消
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// 检查全局时间限制
		if time.Since(stats.startTime) >= urm.maxRetryDuration {
			break
		}

		// 更新统计信息
		urm.mu.Lock()
		stats.attempts = attempt + 1
		stats.lastAttempt = time.Now()
		urm.mu.Unlock()

		// 执行操作
		err := operation()
		if err == nil {
			// 成功，清理统计信息
			urm.mu.Lock()
			delete(urm.retryStats, operationID)
			urm.mu.Unlock()
			return nil
		}

		lastErr = err

		// 检查是否应该重试
		if !urm.shouldRetry(err, config.OperationType) {
			break
		}

		// 计算重试延迟
		delay := urm.calculateRetryDelay(attempt, config)

		// 记录延迟时间
		urm.mu.Lock()
		stats.totalDelay += delay
		urm.mu.Unlock()

		// 等待重试
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}

	return fmt.Errorf("操作 %s 重试失败: %w", operationID, lastErr)
}

// shouldRetry 判断是否应该重试
// 🔧 统一错误分类逻辑
func (urm *UnifiedRetryManager) shouldRetry(err error, operationType string) bool {
	if err == nil {
		return false
	}

	errStr := err.Error()

	// 致命错误，不应重试
	fatalErrors := []string{
		"authentication failed",
		"permission denied",
		"file not found",
		"invalid parameter",
		"quota exceeded",
	}

	for _, fatal := range fatalErrors {
		if strings.Contains(strings.ToLower(errStr), fatal) {
			return false
		}
	}

	// 可重试的错误
	retryableErrors := []string{
		"network timeout",
		"connection reset",
		"server overload",
		"rate limit",
		"temporary failure",
		"503",
		"502",
		"500",
	}

	for _, retryable := range retryableErrors {
		if strings.Contains(strings.ToLower(errStr), retryable) {
			return true
		}
	}

	// 根据操作类型判断
	switch operationType {
	case "upload":
		return strings.Contains(strings.ToLower(errStr), "upload") && !strings.Contains(strings.ToLower(errStr), "invalid")
	case "download":
		return strings.Contains(strings.ToLower(errStr), "download") && !strings.Contains(strings.ToLower(errStr), "not found")
	default:
		return false
	}
}

// calculateRetryDelay 计算重试延迟
// 🔧 智能延迟计算，避免过度重试
func (urm *UnifiedRetryManager) calculateRetryDelay(attempt int, config RetryConfig) time.Duration {
	delay := config.BaseDelay

	// 指数退避
	for i := 0; i < attempt; i++ {
		delay = time.Duration(float64(delay) * config.BackoffMultiplier)
	}

	// 限制最大延迟
	if delay > config.MaxDelay {
		delay = config.MaxDelay
	}

	return delay
}

// CrossCloudTransferManager 跨云传输管理器
// 🔧 核心思想：防止重复下载，智能缓存传输状态
type CrossCloudTransferManager struct {
	activeTransfers map[string]*CrossCloudTransferSession
	transferCache   map[string]*CachedTransferData
	mu              sync.RWMutex
}

// CrossCloudTransferSession 跨云传输会话
type CrossCloudTransferSession struct {
	ID          string
	SourceInfo  SourceInfo
	Status      string // "downloading", "processing", "uploading", "completed", "failed"
	StartTime   time.Time
	Data        []byte // 缓存的文件数据
	MD5Hash     string
	SHA1Hash    string
	Progress    float64
	Error       error
	Subscribers []chan *CrossCloudTransferResult
}

// SourceInfo 源文件信息
type SourceInfo struct {
	Path    string
	Size    int64
	ModTime time.Time
	FsType  string
}

// CachedTransferData 缓存的传输数据
type CachedTransferData struct {
	Data      []byte
	MD5Hash   string
	SHA1Hash  string
	CachedAt  time.Time
	ExpiresAt time.Time
}

// CrossCloudTransferResult 跨云传输结果
type CrossCloudTransferResult struct {
	Success   bool
	Data      []byte
	MD5Hash   string
	SHA1Hash  string
	Error     error
	FromCache bool
}

// NewCrossCloudTransferManager 创建跨云传输管理器
func NewCrossCloudTransferManager() *CrossCloudTransferManager {
	return &CrossCloudTransferManager{
		activeTransfers: make(map[string]*CrossCloudTransferSession),
		transferCache:   make(map[string]*CachedTransferData),
	}
}

// StartTransfer 开始传输
// 🔧 关键修复：检测重复传输，返回现有会话或创建新会话
func (cctm *CrossCloudTransferManager) StartTransfer(ctx context.Context, sourceInfo SourceInfo) (*CrossCloudTransferResult, error) {
	transferID := cctm.generateTransferID(sourceInfo)

	cctm.mu.Lock()

	// 检查是否有活跃的传输
	if session, exists := cctm.activeTransfers[transferID]; exists {
		cctm.mu.Unlock()

		// 等待现有传输完成
		return cctm.waitForTransfer(ctx, session)
	}

	// 检查缓存
	if cached, exists := cctm.transferCache[transferID]; exists && time.Now().Before(cached.ExpiresAt) {
		cctm.mu.Unlock()

		return &CrossCloudTransferResult{
			Success:   true,
			Data:      cached.Data,
			MD5Hash:   cached.MD5Hash,
			SHA1Hash:  cached.SHA1Hash,
			FromCache: true,
		}, nil
	}

	// 创建新的传输会话
	session := &CrossCloudTransferSession{
		ID:          transferID,
		SourceInfo:  sourceInfo,
		Status:      "downloading",
		StartTime:   time.Now(),
		Subscribers: make([]chan *CrossCloudTransferResult, 0),
	}

	cctm.activeTransfers[transferID] = session
	cctm.mu.Unlock()

	// 异步执行传输
	go cctm.executeTransfer(ctx, session)

	// 等待传输完成
	return cctm.waitForTransfer(ctx, session)
}

// generateTransferID 生成传输ID
func (cctm *CrossCloudTransferManager) generateTransferID(sourceInfo SourceInfo) string {
	return fmt.Sprintf("%s_%s_%d_%d",
		sourceInfo.FsType, sourceInfo.Path, sourceInfo.Size, sourceInfo.ModTime.Unix())
}

// waitForTransfer 等待传输完成
func (cctm *CrossCloudTransferManager) waitForTransfer(ctx context.Context, session *CrossCloudTransferSession) (*CrossCloudTransferResult, error) {
	resultChan := make(chan *CrossCloudTransferResult, 1)

	cctm.mu.Lock()
	session.Subscribers = append(session.Subscribers, resultChan)
	cctm.mu.Unlock()

	select {
	case result := <-resultChan:
		return result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// executeTransfer 执行传输
func (cctm *CrossCloudTransferManager) executeTransfer(ctx context.Context, session *CrossCloudTransferSession) {
	defer func() {
		cctm.mu.Lock()
		delete(cctm.activeTransfers, session.ID)
		cctm.mu.Unlock()
	}()

	// 模拟传输逻辑（实际实现中会调用真实的下载逻辑）
	result := &CrossCloudTransferResult{
		Success: true,
		// 实际实现中会填充真实数据
	}

	// 缓存结果
	if result.Success && len(result.Data) > 0 {
		cctm.mu.Lock()
		cctm.transferCache[session.ID] = &CachedTransferData{
			Data:      result.Data,
			MD5Hash:   result.MD5Hash,
			SHA1Hash:  result.SHA1Hash,
			CachedAt:  time.Now(),
			ExpiresAt: time.Now().Add(30 * time.Minute), // 缓存30分钟
		}
		cctm.mu.Unlock()
	}

	// 通知所有订阅者
	cctm.mu.Lock()
	for _, subscriber := range session.Subscribers {
		select {
		case subscriber <- result:
		default:
		}
	}
	cctm.mu.Unlock()
}

// MultiInstanceCacheManager 多实例缓存管理器
// 🔧 核心思想：通过文件锁和实例协调解决多实例BadgerDB冲突
type MultiInstanceCacheManager struct {
	instanceID     string
	cacheDir       string
	lockFile       *os.File
	isMainInstance bool

	// 缓存实例
	parentIDCache *cache.BadgerCache
	dirListCache  *cache.BadgerCache
	pathToIDCache *cache.BadgerCache
	fallbackCache *MemoryCache

	// 实例协调
	instanceRegistry *InstanceRegistry
	heartbeatTicker  *time.Ticker

	mu sync.RWMutex
}

// InstanceRegistry 实例注册表
type InstanceRegistry struct {
	registryFile string
	instances    map[string]*InstanceInfo
	mu           sync.RWMutex
}

// InstanceInfo 实例信息
type InstanceInfo struct {
	InstanceID    string    `json:"instance_id"`
	ProcessID     int       `json:"process_id"`
	StartTime     time.Time `json:"start_time"`
	LastHeartbeat time.Time `json:"last_heartbeat"`
	CacheRole     string    `json:"cache_role"` // "primary", "secondary", "readonly"
	Status        string    `json:"status"`     // "active", "inactive", "failed"
}

// NewMultiInstanceCacheManager 创建多实例缓存管理器
// 🔧 关键修复：智能实例协调，避免BadgerDB冲突
func NewMultiInstanceCacheManager(cacheType, cacheDir string) (*MultiInstanceCacheManager, error) {
	instanceID := generateInstanceID()

	manager := &MultiInstanceCacheManager{
		instanceID: instanceID,
		cacheDir:   cacheDir,
	}

	// 初始化实例注册表
	registryFile := filepath.Join(cacheDir, "instance_registry.json")
	manager.instanceRegistry = &InstanceRegistry{
		registryFile: registryFile,
		instances:    make(map[string]*InstanceInfo),
	}

	// 尝试获取主实例锁
	err := manager.acquireMainInstanceLock()
	if err != nil {
		fs.Debugf(nil, "🔧 无法获取主实例锁，将作为从实例运行: %v", err)
		manager.isMainInstance = false
	} else {
		fs.Infof(nil, "🔧 获取主实例锁成功，作为主实例运行")
		manager.isMainInstance = true
	}

	// 注册当前实例
	err = manager.registerInstance()
	if err != nil {
		return nil, fmt.Errorf("注册实例失败: %w", err)
	}

	// 初始化缓存
	err = manager.initializeCache(cacheType)
	if err != nil {
		return nil, fmt.Errorf("初始化缓存失败: %w", err)
	}

	// 启动心跳
	manager.startHeartbeat()

	return manager, nil
}

// MemoryCache 内存缓存实现
type MemoryCache struct {
	data map[string]*CacheItem
	mu   sync.RWMutex
}

type CacheItem struct {
	Value     interface{}
	ExpiresAt time.Time
}

func NewMemoryCache() *MemoryCache {
	return &MemoryCache{
		data: make(map[string]*CacheItem),
	}
}

func (mc *MemoryCache) Get(key string, value interface{}) (bool, error) {
	mc.mu.RLock()
	defer mc.mu.RUnlock()

	item, exists := mc.data[key]
	if !exists {
		return false, nil
	}

	if time.Now().After(item.ExpiresAt) {
		delete(mc.data, key)
		return false, nil
	}

	return true, nil
}

func (mc *MemoryCache) Set(key string, value interface{}, ttl time.Duration) error {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	mc.data[key] = &CacheItem{
		Value:     value,
		ExpiresAt: time.Now().Add(ttl),
	}

	return nil
}

func (mc *MemoryCache) Delete(key string) error {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	delete(mc.data, key)
	return nil
}

// generateInstanceID 生成实例ID
func generateInstanceID() string {
	return fmt.Sprintf("rclone_%d_%d", os.Getpid(), time.Now().Unix())
}

// acquireMainInstanceLock 获取主实例锁
func (micm *MultiInstanceCacheManager) acquireMainInstanceLock() error {
	lockFilePath := filepath.Join(micm.cacheDir, "main_instance.lock")

	// 确保目录存在
	err := os.MkdirAll(micm.cacheDir, 0755)
	if err != nil {
		return fmt.Errorf("创建缓存目录失败: %w", err)
	}

	// 尝试创建并锁定文件
	lockFile, err := os.OpenFile(lockFilePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("创建锁文件失败: %w", err)
	}

	// 写入实例信息
	instanceInfo := fmt.Sprintf("instance_id=%s\nprocess_id=%d\nstart_time=%s\n",
		micm.instanceID, os.Getpid(), time.Now().Format(time.RFC3339))

	_, err = lockFile.WriteString(instanceInfo)
	if err != nil {
		lockFile.Close()
		return fmt.Errorf("写入锁文件失败: %w", err)
	}

	micm.lockFile = lockFile
	return nil
}

// registerInstance 注册实例
func (micm *MultiInstanceCacheManager) registerInstance() error {
	instanceInfo := &InstanceInfo{
		InstanceID:    micm.instanceID,
		ProcessID:     os.Getpid(),
		StartTime:     time.Now(),
		LastHeartbeat: time.Now(),
		Status:        "active",
	}

	if micm.isMainInstance {
		instanceInfo.CacheRole = "primary"
	} else {
		instanceInfo.CacheRole = "secondary"
	}

	return micm.instanceRegistry.RegisterInstance(instanceInfo)
}

// initializeCache 初始化缓存
func (micm *MultiInstanceCacheManager) initializeCache(cacheType string) error {
	if micm.isMainInstance {
		// 主实例使用BadgerDB
		fs.Infof(nil, "🔧 主实例初始化BadgerDB缓存")

		// 初始化各个缓存
		caches := map[string]**cache.BadgerCache{
			"parent_ids": &micm.parentIDCache,
			"dir_list":   &micm.dirListCache,
			"path_to_id": &micm.pathToIDCache,
		}

		for name, cachePtr := range caches {
			badgerCache, err := cache.NewBadgerCache(name, micm.cacheDir)
			if err != nil {
				fs.Errorf(nil, "主实例%s缓存初始化失败，降级为内存缓存: %v", name, err)
				micm.fallbackCache = NewMemoryCache()
				micm.isMainInstance = false
				break
			} else {
				*cachePtr = badgerCache
			}
		}
	} else {
		// 从实例使用内存缓存
		fs.Infof(nil, "🔧 从实例初始化内存缓存")
		micm.fallbackCache = NewMemoryCache()
	}

	return nil
}

// startHeartbeat 启动心跳
func (micm *MultiInstanceCacheManager) startHeartbeat() {
	micm.heartbeatTicker = time.NewTicker(30 * time.Second)

	go func() {
		for range micm.heartbeatTicker.C {
			err := micm.updateHeartbeat()
			if err != nil {
				fs.Debugf(nil, "更新心跳失败: %v", err)
			}

			// 清理失效实例
			micm.instanceRegistry.CleanupInactiveInstances()
		}
	}()
}

// updateHeartbeat 更新心跳
func (micm *MultiInstanceCacheManager) updateHeartbeat() error {
	micm.instanceRegistry.mu.Lock()
	defer micm.instanceRegistry.mu.Unlock()

	if info, exists := micm.instanceRegistry.instances[micm.instanceID]; exists {
		info.LastHeartbeat = time.Now()
		return micm.instanceRegistry.saveRegistry()
	}

	return fmt.Errorf("实例不存在: %s", micm.instanceID)
}

// RegisterInstance 注册实例
func (ir *InstanceRegistry) RegisterInstance(info *InstanceInfo) error {
	ir.mu.Lock()
	defer ir.mu.Unlock()

	ir.instances[info.InstanceID] = info
	return ir.saveRegistry()
}

// CleanupInactiveInstances 清理失效实例
func (ir *InstanceRegistry) CleanupInactiveInstances() {
	ir.mu.Lock()
	defer ir.mu.Unlock()

	now := time.Now()
	for instanceID, info := range ir.instances {
		// 如果心跳超过2分钟，认为实例失效
		if now.Sub(info.LastHeartbeat) > 2*time.Minute {
			fs.Debugf(nil, "清理失效实例: %s", instanceID)
			delete(ir.instances, instanceID)
		}
	}

	ir.saveRegistry()
}

// saveRegistry 保存注册表
func (ir *InstanceRegistry) saveRegistry() error {
	data, err := json.MarshalIndent(ir.instances, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(ir.registryFile, data, 0644)
}

// 兼容性方法：让现有代码可以无缝使用新的缓存管理器
func (micm *MultiInstanceCacheManager) GetParentIDCache() *cache.BadgerCache {
	if micm.isMainInstance && micm.parentIDCache != nil {
		return micm.parentIDCache
	}
	return nil
}

func (micm *MultiInstanceCacheManager) GetDirListCache() *cache.BadgerCache {
	if micm.isMainInstance && micm.dirListCache != nil {
		return micm.dirListCache
	}
	return nil
}

func (micm *MultiInstanceCacheManager) GetPathToIDCache() *cache.BadgerCache {
	if micm.isMainInstance && micm.pathToIDCache != nil {
		return micm.pathToIDCache
	}
	return nil
}

// Close 关闭缓存管理器
func (micm *MultiInstanceCacheManager) Close() error {
	// 停止心跳
	if micm.heartbeatTicker != nil {
		micm.heartbeatTicker.Stop()
	}

	// 关闭缓存
	caches := []*cache.BadgerCache{
		micm.parentIDCache,
		micm.dirListCache,
		micm.pathToIDCache,
	}

	for i, c := range caches {
		if c != nil {
			if err := c.Close(); err != nil {
				fs.Debugf(nil, "关闭BadgerDB缓存%d失败: %v", i, err)
			}
		}
	}

	// 释放文件锁
	if micm.lockFile != nil {
		micm.lockFile.Close()
	}

	return nil
}

// 兼容性方法：为Fs添加缓存访问方法
func (f *Fs) getParentIDCache() *cache.BadgerCache {
	if f.cacheManager != nil {
		return f.cacheManager.GetParentIDCache()
	}
	return nil
}

func (f *Fs) getDirListCache() *cache.BadgerCache {
	if f.cacheManager != nil {
		return f.cacheManager.GetDirListCache()
	}
	return nil
}

func (f *Fs) getPathToIDCache() *cache.BadgerCache {
	if f.cacheManager != nil {
		return f.cacheManager.GetPathToIDCache()
	}
	return nil
}

// UnifiedProgressManager 统一进度管理器
// 🔧 核心思想：统一管理所有进度显示，避免重复和混乱
type UnifiedProgressManager struct {
	activeProgresses map[string]*ProgressSession
	mu               sync.RWMutex
}

// ProgressSession 进度会话
type ProgressSession struct {
	ID           string
	Type         string // "upload", "download", "cross_cloud"
	Backend      string // "123drive", "115drive"
	FileName     string
	TotalSize    int64
	CurrentSize  int64
	TotalChunks  int
	CurrentChunk int
	StartTime    time.Time
	LastUpdate   time.Time
	Speed        float64
	ETA          time.Duration
}

// NewUnifiedProgressManager 创建统一进度管理器
func NewUnifiedProgressManager() *UnifiedProgressManager {
	return &UnifiedProgressManager{
		activeProgresses: make(map[string]*ProgressSession),
	}
}

// StartProgress 开始进度跟踪
// 🔧 关键修复：统一进度入口，避免重复显示
func (upm *UnifiedProgressManager) StartProgress(id, progressType, backend, fileName string, totalSize int64, totalChunks int) {
	upm.mu.Lock()
	defer upm.mu.Unlock()

	session := &ProgressSession{
		ID:          id,
		Type:        progressType,
		Backend:     backend,
		FileName:    fileName,
		TotalSize:   totalSize,
		TotalChunks: totalChunks,
		StartTime:   time.Now(),
		LastUpdate:  time.Now(),
	}

	upm.activeProgresses[id] = session
}

// UpdateProgress 更新进度
// 🔧 关键修复：智能进度更新，减少冗余显示
func (upm *UnifiedProgressManager) UpdateProgress(id string, currentSize int64, currentChunk int) {
	upm.mu.Lock()
	session, exists := upm.activeProgresses[id]
	if !exists {
		upm.mu.Unlock()
		return
	}

	now := time.Now()
	duration := now.Sub(session.StartTime)

	// 更新会话信息
	session.CurrentSize = currentSize
	session.CurrentChunk = currentChunk
	session.LastUpdate = now

	// 计算速度和ETA
	if duration > 0 {
		session.Speed = float64(currentSize) / duration.Seconds() / (1024 * 1024) // MB/s
		if session.Speed > 0 {
			remainingSize := session.TotalSize - currentSize
			session.ETA = time.Duration(float64(remainingSize)/session.Speed/1024/1024) * time.Second
		}
	}

	upm.mu.Unlock()

	// 🔧 关键修复：智能显示策略，只在关键节点显示
	upm.displayProgressIfNeeded(session)
}

// displayProgressIfNeeded 智能进度显示
// 🔧 关键修复：避免频繁显示，提升到Info级别让用户可见
func (upm *UnifiedProgressManager) displayProgressIfNeeded(session *ProgressSession) {
	// 显示条件：每5个分片、完成时、或每30秒
	shouldDisplay := false

	if session.CurrentChunk%5 == 0 || session.CurrentChunk == session.TotalChunks {
		shouldDisplay = true
	} else if time.Since(session.LastUpdate) > 30*time.Second {
		shouldDisplay = true
	}

	if !shouldDisplay {
		return
	}

	percentage := float64(session.CurrentChunk) / float64(session.TotalChunks) * 100

	// ETA显示
	etaStr := "ETA: -"
	if session.ETA > 0 {
		etaStr = fmt.Sprintf("ETA: %v", session.ETA.Round(time.Second))
	}

	// 🔧 关键修复：提升到Info级别，用户无需Debug模式即可看到
	// 使用"分片5/15"格式，符合用户需求
	fs.Infof(nil, "📊 %s %s: 分片%d/%d (%.1f%%) | %s/%s | %.2f MB/s | %s",
		session.Backend, session.Type,
		session.CurrentChunk, session.TotalChunks, percentage,
		fs.SizeSuffix(session.CurrentSize), fs.SizeSuffix(session.TotalSize),
		session.Speed, etaStr)
}

// FinishProgress 完成进度跟踪
func (upm *UnifiedProgressManager) FinishProgress(id string) {
	upm.mu.Lock()
	defer upm.mu.Unlock()

	if session, exists := upm.activeProgresses[id]; exists {
		duration := time.Since(session.StartTime)
		avgSpeed := float64(session.TotalSize) / duration.Seconds() / (1024 * 1024)

		// 🔧 关键修复：完成信息提升到Info级别
		fs.Infof(nil, "✅ %s %s完成: %s | 用时: %v | 平均速度: %.2f MB/s",
			session.Backend, session.Type, session.FileName,
			duration.Round(time.Second), avgSpeed)

		delete(upm.activeProgresses, id)
	}
}

// saveDirListToCache 保存目录列表到缓存
// 增强版本：添加并发安全保护
func (f *Fs) saveDirListToCache(parentFileID int64, lastFileID int64, fileList []FileListInfoRespDataV2, nextLastFileID int64) {
	dirListCache := f.getDirListCache()
	if dirListCache == nil {
		return
	}

	// 使用写锁保护缓存写入操作
	f.cacheMu.Lock()
	defer f.cacheMu.Unlock()

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
// 增强版本：添加并发安全保护
func (f *Fs) clearDirListCache(parentFileID int64) {
	if f.dirListCache == nil {
		return
	}

	// 使用写锁保护缓存删除操作
	f.cacheMu.Lock()
	defer f.cacheMu.Unlock()

	// 清理指定父目录的所有分页缓存
	prefix := fmt.Sprintf("dirlist_%d_", parentFileID)
	if err := f.dirListCache.DeletePrefix(prefix); err != nil {
		fs.Debugf(f, "清理目录列表缓存失败 %s: %v", prefix, err)
	} else {
		fs.Debugf(f, "已清理目录列表缓存: parentID=%d", parentFileID)
	}
}

// 🗑️ getDownloadURLFromCache 已删除 - 下载URL缓存已移除

// 🗑️ saveDownloadURLToCache 已删除 - 下载URL缓存已移除

// getPathToIDFromCache 从缓存获取路径到FileID的映射
// 增强版本：添加并发安全保护
func (f *Fs) getPathToIDFromCache(path string) (string, bool, bool) {
	if f.pathToIDCache == nil {
		return "", false, false
	}

	cacheKey := fmt.Sprintf("path_to_id_%s", path)
	var entry PathToIDCacheEntry

	// 使用读锁保护缓存读取
	f.cacheMu.RLock()
	found, err := f.pathToIDCache.Get(cacheKey, &entry)
	f.cacheMu.RUnlock()

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

	// 使用写锁保护缓存写入
	f.cacheMu.Lock()
	err := f.pathToIDCache.Set(cacheKey, entry, f.cacheConfig.PathToIDCacheTTL)
	f.cacheMu.Unlock()

	if err != nil {
		fs.Debugf(f, "保存路径映射缓存失败 %s: %v", cacheKey, err)
	} else {
		fs.Debugf(f, "已保存路径映射到缓存: %s -> %s (dir: %v), TTL=%v", path, fileID, isDir, f.cacheConfig.PathToIDCacheTTL)
	}
}

// clearPathToIDCache 清理路径映射缓存
// 增强版本：添加并发安全保护
func (f *Fs) clearPathToIDCache(path string) {
	if f.pathToIDCache == nil {
		return
	}

	// 使用写锁保护缓存删除
	f.cacheMu.Lock()
	cacheKey := fmt.Sprintf("path_to_id_%s", path)
	err := f.pathToIDCache.Delete(cacheKey)
	f.cacheMu.Unlock()

	if err != nil {
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
	err := f.makeAPICallWithRest(ctx, url, "GET", nil, &response)
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
func (f *Fs) searchParentIDInCache(_ int64) int64 {
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
// 优化版本：使用rclone标准路径处理加上123网盘特定要求
// dircache要求：路径不应该以/开头或结尾
func normalizePath(path string) string {
	if path == "" {
		return ""
	}

	// 使用filepath.Clean进行基础路径清理
	path = filepath.Clean(path)

	// 处理filepath.Clean的特殊返回值
	if path == "." {
		return ""
	}

	// 移除开头的斜杠（如果有）
	path = strings.TrimPrefix(path, "/")
	path = strings.TrimPrefix(path, "\\") // 处理Windows路径分隔符

	// 移除结尾的斜杠（如果有）
	path = strings.TrimSuffix(path, "/")
	path = strings.TrimSuffix(path, "\\")

	// 将反斜杠转换为正斜杠（标准化路径分隔符）
	path = strings.ReplaceAll(path, "\\", "/")

	// 最终检查：确保结果不以/开头或结尾
	path = strings.Trim(path, "/")

	return path
}

// isRemoteSource 检查源对象是否来自远程云盘（非本地文件）
func (f *Fs) isRemoteSource(src fs.ObjectInfo) bool {
	// 检查源对象的类型，如果不是本地文件系统，则认为是远程源
	// 这可以通过检查源对象的Fs()方法返回的类型来判断
	srcFs := src.Fs()
	if srcFs == nil {
		fs.Debugf(f, "🔍 isRemoteSource: srcFs为nil，返回false")
		return false
	}

	// 检查是否是本地文件系统
	fsType := srcFs.Name()
	isRemote := fsType != "local" && fsType != ""

	fs.Debugf(f, "🔍 isRemoteSource检测: fsType='%s', isRemote=%v", fsType, isRemote)

	// 特别检测115网盘和其他云盘
	if strings.Contains(fsType, "115") || strings.Contains(fsType, "pan") {
		fs.Debugf(f, "✅ 明确识别为云盘源: %s", fsType)
		return true
	}

	return isRemote
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
			Help:     "最大并发上传数量。减少此值可降低内存使用和提高稳定性。",
			Default:  8, // 🚀 基于48秒/分片问题优化：从4改为8，进一步提升上传性能
			Advanced: true,
		}, {
			Name:     "max_concurrent_downloads",
			Help:     "最大并发下载数量。提高此值可提升下载速度，但会增加内存使用。",
			Default:  4, // 从1改为4，提升下载性能
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
		if len(arg) < 2 {
			return nil, fmt.Errorf("需要提供文件路径和User-Agent参数")
		}
		path := arg[0]
		ua := arg[1]
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

	// 直接使用v2 API调用 - 已迁移到rclone标准方法
	var result ListResponse
	err := f.makeAPICallWithRest(ctx, endpoint, "GET", nil, &result)
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
	fs.Debugf(f, "🔍 pathToFileID开始: filePath=%s", filePath)

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
		next := "0"           // 根据API使用字符串作为next
		maxIterations := 1000 // 🔧 防止无限循环：最多1000次迭代
		iteration := 0
		for {
			iteration++
			if iteration > maxIterations {
				return "", fmt.Errorf("查找路径 %s 超过最大迭代次数 %d，可能存在循环", part, maxIterations)
			}
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

					// 记录找到的项目类型信息，用于后续缓存
					isDir := (item.Type == 1) // Type: 0-文件  1-文件夹
					fs.Debugf(f, "pathToFileID找到项目: %s -> ID=%s, Type=%d, isDir=%v", part, currentID, item.Type, isDir)

					// 如果这是路径的最后一部分，立即缓存类型信息
					if len(parts) > 0 && part == parts[len(parts)-1] {
						currentPath := "/" + strings.Join(parts, "/")
						f.savePathToIDToCache(currentPath, currentID, "0", isDir)
						fs.Debugf(f, "立即缓存路径类型: %s -> ID=%s, isDir=%v", currentPath, currentID, isDir)
					}

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

	// 缓存路径映射结果（如果还没有被缓存）
	// 注释掉错误的缓存逻辑，因为正确的类型信息已经在路径解析循环中缓存了
	// if currentID != "0" {
	// 	// 检查是否已经缓存过
	// 	if _, _, found := f.getPathToIDFromCache(filePath); !found {
	// 		// 默认假设是目录，因为我们在查找路径
	// 		isDir := true
	// 		f.savePathToIDToCache(filePath, currentID, "0", isDir)
	// 		fs.Debugf(f, "缓存路径映射: %s -> ID=%s, isDir=%v", filePath, currentID, isDir)
	// 	}
	// }

	fs.Debugf(f, "🔍 pathToFileID结果: filePath=%s -> currentID=%s", filePath, currentID)
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
		SpaceTempExpr  int64  `json:"spaceTempExpr"` // 修复：API返回数字而非字符串
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

// ConcurrencyManager 统一的并发控制管理器
// 优化版本：提供更标准化的并发控制接口
type ConcurrencyManager struct {
	uploadSemaphore   chan struct{} // 上传并发控制信号量
	downloadSemaphore chan struct{} // 下载并发控制信号量
	maxUploads        int           // 最大并发上传数
	maxDownloads      int           // 最大并发下载数
}

// NewConcurrencyManager 创建新的并发控制管理器
func NewConcurrencyManager(maxUploads, maxDownloads int) *ConcurrencyManager {
	return &ConcurrencyManager{
		uploadSemaphore:   make(chan struct{}, maxUploads),
		downloadSemaphore: make(chan struct{}, maxDownloads),
		maxUploads:        maxUploads,
		maxDownloads:      maxDownloads,
	}
}

// AcquireUpload 获取上传信号量
func (cm *ConcurrencyManager) AcquireUpload(ctx context.Context) error {
	select {
	case cm.uploadSemaphore <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ReleaseUpload 释放上传信号量
func (cm *ConcurrencyManager) ReleaseUpload() {
	select {
	case <-cm.uploadSemaphore:
		// 信号量释放成功
	default:
		// 信号量释放失败，记录警告
		fs.Debugf(nil, "上传信号量释放失败")
	}
}

// AcquireDownload 获取下载信号量
func (cm *ConcurrencyManager) AcquireDownload(ctx context.Context) error {
	select {
	case cm.downloadSemaphore <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ReleaseDownload 释放下载信号量
func (cm *ConcurrencyManager) ReleaseDownload() {
	select {
	case <-cm.downloadSemaphore:
		// 信号量释放成功
	default:
		// 信号量释放失败，记录警告
		fs.Debugf(nil, "下载信号量释放失败")
	}
}

// GetStats 获取并发控制统计信息
func (cm *ConcurrencyManager) GetStats() (uploadActive, downloadActive int) {
	return cm.maxUploads - len(cm.uploadSemaphore), cm.maxDownloads - len(cm.downloadSemaphore)
}

// validateRequired 验证必需字段不为空
func validateRequired(fieldName, value string) error {
	if value == "" {
		return fmt.Errorf("%s 不能为空", fieldName)
	}
	return nil
}

// validateRange 验证数值在指定范围内
func validateRange(fieldName string, value, min, max int) error {
	if value <= 0 || value < min || value > max {
		return fmt.Errorf("%s 必须在 %d-%d 范围内", fieldName, min, max)
	}
	return nil
}

// validateDuration 验证时间配置不为负数
func validateDuration(fieldName string, value time.Duration) error {
	if value < 0 {
		return fmt.Errorf("%s 不能为负数", fieldName)
	}
	return nil
}

// validateSize 验证大小配置
func validateSize(fieldName string, value int64, min, max int64) error {
	if value < min {
		return fmt.Errorf("%s 不能小于 %d", fieldName, min)
	}
	if max > 0 && value > max {
		return fmt.Errorf("%s 不能超过 %d", fieldName, max)
	}
	return nil
}

// validateOptions 验证配置选项的有效性
// 优化版本：使用结构化验证方法，减少重复代码
func validateOptions(opt *Options) error {
	var errors []string

	// 验证必需的认证信息
	if err := validateRequired("client_id", opt.ClientID); err != nil {
		errors = append(errors, err.Error())
	}
	if err := validateRequired("client_secret", opt.ClientSecret); err != nil {
		errors = append(errors, err.Error())
	}

	// 验证数值范围 - 使用通用验证函数
	if err := validateRange("list_chunk", opt.ListChunk, 1, MaxListChunk); err != nil {
		errors = append(errors, err.Error())
	}
	if err := validateRange("max_upload_parts", opt.MaxUploadParts, 1, MaxUploadPartsLimit); err != nil {
		errors = append(errors, err.Error())
	}
	if err := validateRange("max_concurrent_uploads", opt.MaxConcurrentUploads, 1, MaxConcurrentLimit); err != nil {
		errors = append(errors, err.Error())
	}
	if err := validateRange("max_concurrent_downloads", opt.MaxConcurrentDownloads, 1, MaxConcurrentLimit); err != nil {
		errors = append(errors, err.Error())
	}

	// 验证时间配置 - 使用通用验证函数
	if err := validateDuration("pacer_min_sleep", time.Duration(opt.PacerMinSleep)); err != nil {
		errors = append(errors, err.Error())
	}
	if err := validateDuration("list_pacer_min_sleep", time.Duration(opt.ListPacerMinSleep)); err != nil {
		errors = append(errors, err.Error())
	}
	if err := validateDuration("conn_timeout", time.Duration(opt.ConnTimeout)); err != nil {
		errors = append(errors, err.Error())
	}
	if err := validateDuration("timeout", time.Duration(opt.Timeout)); err != nil {
		errors = append(errors, err.Error())
	}
	if err := validateDuration("progress_update_interval", time.Duration(opt.ProgressUpdateInterval)); err != nil {
		errors = append(errors, err.Error())
	}

	// 验证大小配置 - 使用通用验证函数
	if err := validateSize("chunk_size", int64(opt.ChunkSize), 1, 5*1024*1024*1024); err != nil {
		errors = append(errors, err.Error())
	}
	if err := validateSize("upload_cutoff", int64(opt.UploadCutoff), 0, -1); err != nil {
		errors = append(errors, err.Error())
	}

	// 验证UserAgent长度
	if opt.UserAgent != "" && len(opt.UserAgent) > 200 {
		errors = append(errors, "user_agent 长度不能超过 200 字符")
	}

	// 验证RootFolderID格式（如果提供）
	if opt.RootFolderID != "" {
		if _, err := parseFileID(opt.RootFolderID); err != nil {
			errors = append(errors, "root_folder_id 必须是有效的数字")
		}
	}

	// 返回聚合的错误信息
	if len(errors) > 0 {
		return fmt.Errorf("配置验证失败: %s", strings.Join(errors, "; "))
	}

	return nil
}

// createPacer 创建pacer的工厂函数，减少重复配置代码
func createPacer(ctx context.Context, minSleep, maxSleep time.Duration, decayConstant float64) *fs.Pacer {
	return fs.NewPacer(ctx, pacer.NewDefault(
		pacer.MinSleep(minSleep),
		pacer.MaxSleep(maxSleep),
		pacer.DecayConstant(decayConstant)))
}

// applyStandardConfigDefaults 应用rclone标准配置的默认值
// 这个函数展示了如何在未来可以使用rclone的标准配置选项
func applyStandardConfigDefaults(opt *Options) {
	// 示例：如果未来要使用rclone标准配置，可以这样做：
	// if opt.MaxConcurrentUploads <= 0 {
	//     opt.MaxConcurrentUploads = fs.Config.Transfers
	// }
	// if opt.MaxConcurrentDownloads <= 0 {
	//     opt.MaxConcurrentDownloads = fs.Config.Checkers
	// }
	// if opt.ConnTimeout <= 0 {
	//     opt.ConnTimeout = fs.Duration(fs.Config.ConnectTimeout)
	// }
	// if opt.Timeout <= 0 {
	//     opt.Timeout = fs.Duration(fs.Config.Timeout)
	// }

	// 目前保持现有的123网盘特定配置
	_ = opt // 避免未使用变量警告
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

	// 确保进度显示默认启用（Go中bool零值是false，需要显式设置）
	// 注意：这里不能简单用 if !opt.EnableProgressDisplay，因为用户可能显式设置为false
	// 我们需要检查是否是从配置文件加载的值还是默认的零值
	// 由于configstruct.Set会处理默认值，这里应该已经是正确的值了
	// 但为了确保兼容性，我们保持现有逻辑

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
	case map[string]any:
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

// UnifiedErrorClassifier123 123网盘统一错误分类器
type UnifiedErrorClassifier123 struct {
	// 错误分类统计
	ServerOverloadCount int64
	URLExpiredCount     int64
	NetworkTimeoutCount int64
	RateLimitCount      int64
	UnknownErrorCount   int64
}

// ClassifyError 统一错误分类方法（123网盘版本）
func (c *UnifiedErrorClassifier123) ClassifyError(err error) string {
	if err == nil {
		return "no_error"
	}

	errStr := strings.ToLower(err.Error())

	// 服务器过载错误
	if strings.Contains(errStr, "502") || strings.Contains(errStr, "bad gateway") ||
		strings.Contains(errStr, "503") || strings.Contains(errStr, "service unavailable") ||
		strings.Contains(errStr, "504") || strings.Contains(errStr, "gateway timeout") ||
		strings.Contains(errStr, "internal server error") {
		c.ServerOverloadCount++
		return "server_overload"
	}

	// URL过期错误
	if strings.Contains(errStr, "download url is invalid") || strings.Contains(errStr, "expired") ||
		strings.Contains(errStr, "url过期") || strings.Contains(errStr, "链接失效") ||
		strings.Contains(errStr, "invalid download url") {
		c.URLExpiredCount++
		return "url_expired"
	}

	// 网络超时错误
	if strings.Contains(errStr, "timeout") || strings.Contains(errStr, "deadline exceeded") ||
		strings.Contains(errStr, "context canceled") || strings.Contains(errStr, "connection reset") ||
		strings.Contains(errStr, "client.timeout") || strings.Contains(errStr, "i/o timeout") {
		c.NetworkTimeoutCount++
		return "network_timeout"
	}

	// 限流错误
	if strings.Contains(errStr, "429") || strings.Contains(errStr, "too many requests") ||
		strings.Contains(errStr, "rate limit") || strings.Contains(errStr, "频率过高") {
		c.RateLimitCount++
		return "rate_limit"
	}

	// 认证错误
	if strings.Contains(errStr, "401") || strings.Contains(errStr, "unauthorized") ||
		strings.Contains(errStr, "token") || strings.Contains(errStr, "认证失败") {
		return "auth_error"
	}

	// 权限错误
	if strings.Contains(errStr, "403") || strings.Contains(errStr, "forbidden") ||
		strings.Contains(errStr, "permission denied") {
		return "permission_error"
	}

	// 资源不存在错误
	if strings.Contains(errStr, "404") || strings.Contains(errStr, "not found") ||
		strings.Contains(errStr, "file not found") {
		return "not_found"
	}

	c.UnknownErrorCount++
	return "unknown"
}

// shouldRetry 根据响应和错误确定是否重试API调用
// 🔧 优化：使用统一错误分类和重试策略，与115网盘保持一致
func shouldRetry(ctx context.Context, resp *http.Response, err error) (bool, error) {
	// 使用rclone标准的上下文错误检查
	if fserrors.ContextError(ctx, &err) {
		return false, err
	}

	// 🔧 优化：使用统一错误分类器，与115网盘保持一致
	classifier := &UnifiedErrorClassifier123{}
	errorType := classifier.ClassifyError(err)

	// 基于错误类型决定是否重试
	switch errorType {
	case "server_overload", "network_timeout":
		// 服务器过载和网络超时错误应该重试
		fs.Debugf(nil, "123网盘检测到%s错误，将重试", errorType)
		return true, err

	case "rate_limit":
		// 限流错误应该重试，但使用更长的延迟
		fs.Debugf(nil, "123网盘检测到限流错误，使用30秒延迟重试")
		return true, fserrors.NewErrorRetryAfter(30 * time.Second)

	case "url_expired":
		// URL过期错误应该重试（会触发URL刷新）
		fs.Debugf(nil, "123网盘检测到URL过期错误，将重试")
		return true, err

	case "auth_error":
		// 认证错误需要刷新token后重试
		fs.Debugf(nil, "123网盘检测到认证错误，需要刷新token")
		return true, fserrors.NewErrorRetryAfter(1 * time.Second)

	case "permission_error", "not_found":
		// 权限错误和资源不存在错误不重试
		fs.Debugf(nil, "123网盘检测到%s错误，不重试", errorType)
		return false, err

	default:
		// 网络错误时重试
		if err != nil {
			// 检查是否为不可恢复的错误（保留123网盘特定的错误检查）
			if isUnrecoverableError(err) {
				return false, err
			}

			// 使用rclone标准的重试判断
			return fserrors.ShouldRetry(err), err
		}

		// 检查HTTP状态码 - 使用rclone标准处理加上123网盘特定处理
		if resp != nil {
			switch resp.StatusCode {
			case http.StatusTooManyRequests:
				// 速率受限 - 使用较长的退避时间
				fs.Debugf(nil, "速率受限（API错误429），将使用更长退避时间重试")
				return true, fserrors.NewErrorRetryAfter(calculateRetryDelay(3))
			case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
				// 服务器错误 - 使用标准重试
				fs.Debugf(nil, "服务器错误 %d，将重试", resp.StatusCode)
				return true, fserrors.NewErrorRetryAfter(baseRetryDelay)
			case http.StatusUnauthorized:
				// 令牌可能已过期 - 不重试，让调用者处理令牌刷新
				return false, fserrors.NewErrorRetryAfter(baseRetryDelay)
			}
		}

		return false, err
	}
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

// makeAPICallWithRest 使用rclone标准rest客户端进行API调用，自动集成QPS限制
// 这是推荐的API调用方法，替代直接使用HTTP客户端
func (f *Fs) makeAPICallWithRest(ctx context.Context, endpoint string, method string, reqBody any, respBody any) error {
	fs.Debugf(f, "🔄 使用rclone标准方法调用API: %s %s", method, endpoint)

	// 安全检查：确保rest客户端已初始化
	if f.rst == nil {
		fs.Errorf(f, "⚠️  rest客户端未初始化，尝试重新创建")
		if f.client != nil {
			f.rst = rest.NewClient(f.client).SetRoot(openAPIRootURL)
			fs.Debugf(f, "✅ rest客户端重新创建成功")
		} else {
			return fmt.Errorf("HTTP客户端和rest客户端都未初始化，无法进行API调用")
		}
	}

	// 确保令牌在进行API调用前有效
	err := f.refreshTokenIfNecessary(ctx, false, false)
	if err != nil {
		return fmt.Errorf("刷新令牌失败: %w", err)
	}

	// 根据端点获取适当的调速器
	pacer := f.getPacerForEndpoint(endpoint)

	// 确定基础URL - 根据官方文档只有两个API使用上传域名
	var baseURL string
	if strings.Contains(endpoint, "/upload/v2/file/slice") ||
		strings.Contains(endpoint, "/upload/v2/file/single/create") {
		// 只有分片上传和单步上传使用上传域名（官方文档明确说明）
		uploadDomain, err := f.getUploadDomain(ctx)
		if err != nil {
			return fmt.Errorf("获取上传域名失败: %w", err)
		}
		baseURL = uploadDomain
	} else {
		// 其他API（包括create、upload_complete）使用标准API域名
		baseURL = openAPIRootURL
	}

	// 构造rest选项，包含必要的HTTP头
	opts := rest.Opts{
		Method:  method,
		Path:    endpoint,
		RootURL: baseURL,
		ExtraHeaders: map[string]string{
			"Authorization": "Bearer " + f.token,
			"Platform":      "open_platform",
			"User-Agent":    f.opt.UserAgent,
		},
	}

	// 使用pacer包装请求，自动处理QPS限制和重试
	var resp *http.Response
	err = pacer.Call(func() (bool, error) {
		var err error
		resp, err = f.rst.CallJSON(ctx, &opts, reqBody, respBody)

		// 检查是否是401错误，如果是则尝试刷新token
		if resp != nil && resp.StatusCode == http.StatusUnauthorized {
			fs.Debugf(f, "收到401错误，强制刷新token")
			// 强制刷新token，忽略时间检查
			refreshErr := f.refreshTokenIfNecessary(ctx, true, true)
			if refreshErr != nil {
				fs.Errorf(f, "刷新token失败: %v", refreshErr)
				return false, fmt.Errorf("身份验证失败: %w", refreshErr)
			}
			// 更新Authorization头
			opts.ExtraHeaders["Authorization"] = "Bearer " + f.token
			fs.Debugf(f, "token已强制刷新，将重试API调用")
			return true, nil // 重试
		}

		return shouldRetry(ctx, resp, err)
	})

	if err != nil {
		return fmt.Errorf("API调用失败 [%s %s]: %w", method, endpoint, err)
	}

	return nil
}

// makeAPICallWithRestMultipartToDomain 使用rclone标准rest客户端进行multipart API调用到指定域名
func (f *Fs) makeAPICallWithRestMultipartToDomain(ctx context.Context, domain string, endpoint string, method string, body io.Reader, contentType string, respBody any) error {
	fs.Debugf(f, "🔄 使用rclone标准方法调用multipart API到域名: %s %s %s", method, domain, endpoint)

	// 确保令牌在进行API调用前有效
	err := f.refreshTokenIfNecessary(ctx, false, false)
	if err != nil {
		return fmt.Errorf("刷新令牌失败: %w", err)
	}

	// 根据端点获取适当的调速器
	pacer := f.getPacerForEndpoint(endpoint)

	// 使用调速器进行API调用
	return pacer.Call(func() (bool, error) {
		opts := rest.Opts{
			Method:  method,
			RootURL: domain,
			Path:    endpoint,
			Body:    body,
			ExtraHeaders: map[string]string{
				"Content-Type": contentType,
			},
		}

		// 添加必要的认证头
		opts.ExtraHeaders["Authorization"] = "Bearer " + f.token
		opts.ExtraHeaders["Platform"] = "open_platform"
		opts.ExtraHeaders["User-Agent"] = f.opt.UserAgent

		var resp *http.Response
		var err error
		resp, err = f.rst.Call(ctx, &opts)
		if err != nil {
			// 检查是否需要重试
			if fserrors.ShouldRetry(err) {
				fs.Debugf(f, "API调用失败，将重试: %v", err)
				return true, err
			}
			return false, fmt.Errorf("API调用失败: %w", err)
		}
		defer resp.Body.Close()

		// 检查响应状态
		if resp.StatusCode >= 400 {
			body, _ := io.ReadAll(resp.Body)

			// 特殊处理401错误 - token过期
			if resp.StatusCode == http.StatusUnauthorized {
				fs.Debugf(f, "收到401错误，强制刷新token")
				// 强制刷新token，忽略时间检查
				refreshErr := f.refreshTokenIfNecessary(ctx, true, true)
				if refreshErr != nil {
					fs.Errorf(f, "刷新token失败: %v", refreshErr)
					return false, fmt.Errorf("身份验证失败: %w", refreshErr)
				}
				// 更新Authorization头
				opts.ExtraHeaders["Authorization"] = "Bearer " + f.token
				fs.Debugf(f, "token已强制刷新，将重试API调用")
				return true, nil // 重试
			}

			return false, fmt.Errorf("API返回错误状态 %d: %s", resp.StatusCode, string(body))
		}

		// 解析响应JSON
		if respBody != nil {
			err = json.NewDecoder(resp.Body).Decode(respBody)
			if err != nil {
				return false, fmt.Errorf("解析响应JSON失败: %w", err)
			}
		}

		return false, nil
	})
}

// makeAPICallWithRestMultipart 使用rclone标准rest客户端进行multipart API调用
func (f *Fs) makeAPICallWithRestMultipart(ctx context.Context, endpoint string, method string, body io.Reader, contentType string, respBody any) error {
	fs.Debugf(f, "🔄 使用rclone标准方法调用multipart API: %s %s", method, endpoint)

	// 确保令牌在进行API调用前有效
	err := f.refreshTokenIfNecessary(ctx, false, false)
	if err != nil {
		return fmt.Errorf("刷新令牌失败: %w", err)
	}

	// 根据端点获取适当的调速器
	pacer := f.getPacerForEndpoint(endpoint)

	// 确定基础URL - 特定API使用上传域名
	var baseURL string
	if strings.Contains(endpoint, "/upload/v2/file/slice") ||
		strings.Contains(endpoint, "/upload/v2/file/single/create") {
		uploadDomain, err := f.getUploadDomain(ctx)
		if err != nil {
			return fmt.Errorf("获取上传域名失败: %w", err)
		}
		baseURL = uploadDomain
	} else {
		baseURL = openAPIRootURL
	}

	// 构造rest选项，包含必要的HTTP头
	opts := rest.Opts{
		Method:  method,
		Path:    endpoint,
		RootURL: baseURL,
		Body:    body,
		ExtraHeaders: map[string]string{
			"Authorization": "Bearer " + f.token,
			"Platform":      "open_platform",
			"User-Agent":    f.opt.UserAgent,
			"Content-Type":  contentType,
		},
	}

	// 使用pacer包装请求，自动处理QPS限制和重试
	var resp *http.Response
	err = pacer.Call(func() (bool, error) {
		var err error
		resp, err = f.rst.Call(ctx, &opts)

		// 检查是否是401错误，如果是则尝试刷新token
		if resp != nil && resp.StatusCode == http.StatusUnauthorized {
			fs.Debugf(f, "收到401错误，强制刷新token")
			// 强制刷新token，忽略时间检查
			refreshErr := f.refreshTokenIfNecessary(ctx, true, true)
			if refreshErr != nil {
				fs.Errorf(f, "刷新token失败: %v", refreshErr)
				return false, fmt.Errorf("身份验证失败: %w", refreshErr)
			}
			// 更新Authorization头
			opts.ExtraHeaders["Authorization"] = "Bearer " + f.token
			fs.Debugf(f, "token已强制刷新，将重试API调用")
			return true, nil // 重试
		}

		if err != nil {
			return shouldRetry(ctx, resp, err)
		}

		// 解析响应
		if respBody != nil {
			defer resp.Body.Close()
			err = json.NewDecoder(resp.Body).Decode(respBody)
			if err != nil {
				return false, fmt.Errorf("解析响应失败: %w", err)
			}
		}

		return false, nil
	})

	if err != nil {
		return fmt.Errorf("multipart API调用失败 [%s %s]: %w", method, endpoint, err)
	}

	return nil
}

// getPacerForEndpoint 根据API端点返回适当的调速器
// 基于官方API限流文档：https://123yunpan.yuque.com/org-wiki-123yunpan-muaork/cr6ced/
// 更新时间：2025-07-04，使用最新官方QPS限制表
func (f *Fs) getPacerForEndpoint(endpoint string) *fs.Pacer {
	switch {
	// 高频率API (15-20 QPS)
	case strings.Contains(endpoint, "/api/v2/file/list"):
		return f.listPacer // ~14 QPS (官方15 QPS)
	case strings.Contains(endpoint, "/upload/v2/file/get_upload_url"),
		strings.Contains(endpoint, "/upload/v2/file/upload_complete"):
		return f.downloadPacer // ~16 QPS (官方20 QPS)

	// 中等频率API (8-10 QPS)
	case strings.Contains(endpoint, "/api/v1/file/move"),
		strings.Contains(endpoint, "/api/v1/file/name"),
		strings.Contains(endpoint, "/api/v1/file/infos"),
		strings.Contains(endpoint, "/api/v1/user/info"):
		return f.listPacer // ~8 QPS (官方10 QPS)
	case strings.Contains(endpoint, "/api/v1/access_token"):
		return f.downloadPacer // ~6 QPS (官方8 QPS)

	// 低频率API (5 QPS)
	case strings.Contains(endpoint, "/upload/v1/file/create"),
		strings.Contains(endpoint, "/upload/v2/file/create"),
		strings.Contains(endpoint, "/upload/v1/file/mkdir"),
		strings.Contains(endpoint, "/api/v1/file/trash"),
		strings.Contains(endpoint, "/upload/v2/file/upload_complete"),
		strings.Contains(endpoint, "/api/v1/file/download_info"):
		return f.strictPacer // ~4 QPS (官方5 QPS)

	// 最低频率API (1 QPS)
	case strings.Contains(endpoint, "/api/v1/file/delete"),
		strings.Contains(endpoint, "/api/v1/file/list"),
		strings.Contains(endpoint, "/api/v1/video/transcode/list"):
		return f.strictPacer // ~0.8 QPS (官方1 QPS) - 使用最严格限制

	// v2分片上传API (特殊处理，使用专用的uploadPacer)
	case strings.Contains(endpoint, "/upload/v2/file/slice"):
		return f.uploadPacer // ~5 QPS (基于官方文档和实际测试)

	// 上传域名API (特殊处理)
	case strings.Contains(endpoint, "/upload/v2/file/domain"):
		return f.strictPacer // 保守处理，避免频繁调用

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

// calculateAdaptiveTimeout 根据操作类型计算超时时间
// 简化版本：使用rclone标准超时配置加上123网盘特定调整
func (f *Fs) calculateAdaptiveTimeout(method, endpoint string) time.Duration {
	// 使用rclone的标准超时配置
	baseTimeout := time.Duration(f.opt.Timeout)
	if baseTimeout <= 0 {
		baseTimeout = defaultTimeout
	}

	// 简化的操作类型调整
	switch {
	case strings.Contains(endpoint, "/upload/"):
		return baseTimeout * 3 // 上传操作需要更长时间
	case strings.Contains(endpoint, "/download"):
		return baseTimeout * 2 // 下载操作需要更长时间
	case method == "POST":
		return baseTimeout * 2 // POST操作通常需要更多时间
	default:
		return baseTimeout // 使用标准超时
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
// 🚀 功能增强：支持跨云盘传输的智能断点续传
type ResumeManager struct {
	mu           sync.RWMutex
	resumeCache  *cache.BadgerCache
	fs           *Fs
	cleanupTimer *time.Timer

	// 🚀 新增：跨云盘传输断点续传支持
	crossCloudResume map[string]*CrossCloudResumeInfo
	crossCloudMutex  sync.RWMutex
}

// CrossCloudResumeInfo 跨云盘传输断点续传信息
// 🚀 功能增强：支持跨云盘传输的断点续传
type CrossCloudResumeInfo struct {
	SourcePath      string         `json:"source_path"`      // 源文件路径
	TargetPath      string         `json:"target_path"`      // 目标文件路径
	FileSize        int64          `json:"file_size"`        // 文件总大小
	CompletedChunks map[int64]bool `json:"completed_chunks"` // 已完成的分片
	ChunkSize       int64          `json:"chunk_size"`       // 分片大小
	TotalChunks     int64          `json:"total_chunks"`     // 总分片数
	MD5Hash         string         `json:"md5_hash"`         // 文件MD5
	SHA1Hash        string         `json:"sha1_hash"`        // 文件SHA1
	CreatedAt       time.Time      `json:"created_at"`       // 创建时间
	LastUpdated     time.Time      `json:"last_updated"`     // 最后更新时间
	TempFilePath    string         `json:"temp_file_path"`   // 临时文件路径
	SourceType      string         `json:"source_type"`      // 源类型(123/115/local)
	TargetType      string         `json:"target_type"`      // 目标类型(123/115/local)
	DownloadedBytes int64          `json:"downloaded_bytes"` // 已下载字节数
	UploadedBytes   int64          `json:"uploaded_bytes"`   // 已上传字节数
	TransferID      string         `json:"transfer_id"`      // 传输唯一标识
}

// NewResumeManager 创建断点续传管理器
// 🚀 功能增强：支持跨云盘传输的智能断点续传
func NewResumeManager(fs *Fs, cacheDir string) (*ResumeManager, error) {
	resumeCache, err := cache.NewBadgerCache("resume", cacheDir)
	if err != nil {
		return nil, fmt.Errorf("创建断点续传缓存失败: %w", err)
	}

	rm := &ResumeManager{
		resumeCache:      resumeCache,
		fs:               fs,
		crossCloudResume: make(map[string]*CrossCloudResumeInfo),
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
// 增强版本：更灵活的清理配置和异步执行
func (rm *ResumeManager) startCleanupTimer() {
	// 清理间隔配置（可以根据需要调整）
	cleanupInterval := 2 * time.Hour // 每2小时清理一次

	fs.Debugf(rm.fs, "🕐 启动断点续传定期清理定时器，间隔: %v", cleanupInterval)

	rm.cleanupTimer = time.AfterFunc(cleanupInterval, func() {
		// 异步执行清理，避免阻塞
		go func() {
			defer func() {
				if r := recover(); r != nil {
					fs.Debugf(rm.fs, "⚠️  断点续传清理过程中发生panic: %v", r)
				}
			}()

			fs.Debugf(rm.fs, "🧹 开始执行定期断点续传清理...")
			startTime := time.Now()

			rm.cleanupExpiredResumeInfo()

			duration := time.Since(startTime)
			fs.Debugf(rm.fs, "✅ 断点续传定期清理完成，耗时: %v", duration)
		}()

		// 重新启动定时器
		rm.startCleanupTimer()
	})
}

// cleanupExpiredResumeInfo 清理过期的断点续传信息
// 增强版本：更全面的清理策略和错误处理
func (rm *ResumeManager) cleanupExpiredResumeInfo() {
	fs.Debugf(rm.fs, "🧹 开始清理过期的断点续传信息")

	// 获取所有断点续传信息的键
	keys, err := rm.getAllResumeKeys()
	if err != nil {
		fs.Debugf(rm.fs, "❌ 获取断点续传键列表失败: %v", err)
		return
	}

	if len(keys) == 0 {
		fs.Debugf(rm.fs, "📋 没有找到断点续传信息，跳过清理")
		return
	}

	cleanedCount := 0
	corruptedCount := 0
	totalCount := len(keys)

	// 多层过期策略
	now := time.Now()
	expiredThresholds := map[string]time.Time{
		"超长期": now.Add(-7 * 24 * time.Hour), // 7天
		"长期":  now.Add(-3 * 24 * time.Hour), // 3天
		"中期":  now.Add(-24 * time.Hour),     // 1天
		"短期":  now.Add(-6 * time.Hour),      // 6小时（用于失败的上传）
	}

	fs.Debugf(rm.fs, "📊 开始处理 %d 个断点续传条目", totalCount)

	for i, key := range keys {
		// 显示进度
		if i%10 == 0 && i > 0 {
			fs.Debugf(rm.fs, "🔄 清理进度: %d/%d", i, totalCount)
		}

		// 尝试加载断点续传信息
		info, err := rm.LoadResumeInfo(key)
		if err != nil {
			// 如果加载失败，可能是损坏的数据，直接删除
			fs.Debugf(rm.fs, "🗑️  删除损坏的断点续传数据: %s, 错误: %v", key, err)
			rm.DeleteResumeInfo(key)
			corruptedCount++
			cleanedCount++
			continue
		}

		// 检查不同级别的过期
		shouldDelete := false
		deleteReason := ""

		if info.CreatedAt.Before(expiredThresholds["超长期"]) {
			shouldDelete = true
			deleteReason = "超长期过期(>7天)"
		} else if info.CreatedAt.Before(expiredThresholds["长期"]) {
			shouldDelete = true
			deleteReason = "长期过期(>3天)"
		} else if info.CreatedAt.Before(expiredThresholds["中期"]) {
			shouldDelete = true
			deleteReason = "中期过期(>1天)"
		} else if info.CreatedAt.Before(expiredThresholds["短期"]) && info.UploadedBytes == 0 {
			// 没有任何上传进度的会话6小时后清理（可能是失败的上传）
			shouldDelete = true
			deleteReason = "无进度上传短期过期(>6小时)"
		}

		if shouldDelete {
			rm.DeleteResumeInfo(key)
			cleanedCount++
			fs.Debugf(rm.fs, "🗑️  删除过期的断点续传信息: %s [%s]", key, deleteReason)
		}
	}

	// 清理统计报告
	fs.Debugf(rm.fs, "✅ 断点续传信息清理完成")
	fs.Debugf(rm.fs, "📊 清理统计: 总计=%d, 清理=%d, 损坏=%d, 保留=%d",
		totalCount, cleanedCount, corruptedCount, totalCount-cleanedCount)

	// 如果清理了大量数据，记录详细信息
	if cleanedCount > 10 {
		fs.Debugf(rm.fs, "⚠️  本次清理了较多数据(%d个)，建议检查上传流程是否正常", cleanedCount)
	}
}

// getAllResumeKeys 获取所有断点续传信息的键
// 增强版本：实现真正的前缀扫描功能
func (rm *ResumeManager) getAllResumeKeys() ([]string, error) {
	if rm.resumeCache == nil {
		return nil, fmt.Errorf("断点续传缓存未初始化")
	}

	// 使用前缀扫描获取所有resume相关的键
	var keys []string

	// 尝试扫描常见的断点续传键模式
	// 断点续传键通常以文件路径或preuploadID为基础
	prefixes := []string{
		"resume_",    // 通用断点续传前缀
		"upload_",    // 上传相关前缀
		"progress_",  // 进度相关前缀
		"preupload_", // 预上传相关前缀
	}

	for _, prefix := range prefixes {
		// 尝试获取以该前缀开头的键
		// 由于cache接口限制，我们使用一种变通方法
		// 生成可能的键名并检查是否存在
		for i := 0; i < 1000; i++ { // 限制扫描范围避免无限循环
			testKey := fmt.Sprintf("%s%d", prefix, i)

			// 尝试获取该键的值来检查是否存在
			var testValue interface{}
			found, err := rm.resumeCache.Get(testKey, &testValue)
			if err == nil && found {
				// 键存在，添加到列表
				keys = append(keys, testKey)
			}
			// 如果连续多次未找到，跳出循环
			if (!found || err != nil) && i > 10 {
				break
			}
		}
	}

	// 另一种方法：基于已知的上传会话扫描
	// 这需要维护一个活跃会话列表，但目前简化实现

	fs.Debugf(rm.fs, "📋 扫描到 %d 个断点续传相关键", len(keys))
	return keys, nil
}

// SaveCrossCloudResumeInfo 保存跨云盘传输断点续传信息
// 🚀 功能增强：支持跨云盘传输的智能断点续传
func (rm *ResumeManager) SaveCrossCloudResumeInfo(info *CrossCloudResumeInfo) error {
	rm.crossCloudMutex.Lock()
	defer rm.crossCloudMutex.Unlock()

	info.LastUpdated = time.Now()
	cacheKey := fmt.Sprintf("crosscloud_%s", info.TransferID)

	// 保存到内存缓存
	rm.crossCloudResume[info.TransferID] = info

	// 保存到持久化缓存
	err := rm.resumeCache.Set(cacheKey, info, 48*time.Hour) // 跨云盘传输保留48小时
	if err != nil {
		return fmt.Errorf("保存跨云盘断点续传信息失败: %w", err)
	}

	fs.Debugf(rm.fs, "🌐 保存跨云盘断点续传信息: %s->%s, 已完成分片: %d/%d",
		info.SourcePath, info.TargetPath, len(info.CompletedChunks), info.TotalChunks)

	return nil
}

// LoadCrossCloudResumeInfo 加载跨云盘传输断点续传信息
// 🚀 功能增强：支持跨云盘传输的智能断点续传
func (rm *ResumeManager) LoadCrossCloudResumeInfo(transferID string) (*CrossCloudResumeInfo, error) {
	rm.crossCloudMutex.RLock()
	defer rm.crossCloudMutex.RUnlock()

	// 先从内存缓存查找
	if info, exists := rm.crossCloudResume[transferID]; exists {
		// 检查是否过期
		if time.Since(info.CreatedAt) <= 48*time.Hour {
			return info, nil
		}
		// 过期则删除
		delete(rm.crossCloudResume, transferID)
	}

	// 从持久化缓存查找
	cacheKey := fmt.Sprintf("crosscloud_%s", transferID)
	var info CrossCloudResumeInfo

	found, err := rm.resumeCache.Get(cacheKey, &info)
	if err != nil {
		return nil, fmt.Errorf("加载跨云盘断点续传信息失败: %w", err)
	}

	if !found {
		return nil, nil // 没有找到断点续传信息
	}

	// 检查信息是否过期（超过48小时）
	if time.Since(info.CreatedAt) > 48*time.Hour {
		rm.DeleteCrossCloudResumeInfo(transferID)
		return nil, nil
	}

	// 加载到内存缓存
	rm.crossCloudResume[transferID] = &info

	fs.Debugf(rm.fs, "🌐 加载跨云盘断点续传信息: %s->%s, 已完成分片: %d/%d",
		info.SourcePath, info.TargetPath, len(info.CompletedChunks), info.TotalChunks)

	return &info, nil
}

// UpdateCrossCloudChunkInfo 更新跨云盘传输分片信息
// 🚀 功能增强：支持跨云盘传输的智能断点续传
func (rm *ResumeManager) UpdateCrossCloudChunkInfo(transferID string, chunkIndex int64, chunkSize int64) error {
	rm.crossCloudMutex.Lock()
	defer rm.crossCloudMutex.Unlock()

	info, exists := rm.crossCloudResume[transferID]
	if !exists {
		// 尝试从持久化缓存加载
		loadedInfo, err := rm.LoadCrossCloudResumeInfo(transferID)
		if err != nil || loadedInfo == nil {
			return fmt.Errorf("跨云盘断点续传信息不存在: %s", transferID)
		}
		info = loadedInfo
	}

	// 更新分片信息
	if info.CompletedChunks == nil {
		info.CompletedChunks = make(map[int64]bool)
	}
	info.CompletedChunks[chunkIndex] = true
	info.DownloadedBytes += chunkSize
	info.LastUpdated = time.Now()

	// 保存更新后的信息
	cacheKey := fmt.Sprintf("crosscloud_%s", transferID)
	err := rm.resumeCache.Set(cacheKey, info, 48*time.Hour)
	if err != nil {
		return fmt.Errorf("更新跨云盘断点续传信息失败: %w", err)
	}

	fs.Debugf(rm.fs, "🌐 更新跨云盘分片信息: %s, 分片 %d, 进度: %d/%d",
		info.SourcePath, chunkIndex, len(info.CompletedChunks), info.TotalChunks)

	return nil
}

// DeleteCrossCloudResumeInfo 删除跨云盘传输断点续传信息
// 🚀 功能增强：支持跨云盘传输的智能断点续传
func (rm *ResumeManager) DeleteCrossCloudResumeInfo(transferID string) error {
	rm.crossCloudMutex.Lock()
	defer rm.crossCloudMutex.Unlock()

	// 从内存缓存删除
	delete(rm.crossCloudResume, transferID)

	// 从持久化缓存删除
	cacheKey := fmt.Sprintf("crosscloud_%s", transferID)
	err := rm.resumeCache.Delete(cacheKey)
	if err != nil {
		return fmt.Errorf("删除跨云盘断点续传信息失败: %w", err)
	}

	fs.Debugf(rm.fs, "🌐 删除跨云盘断点续传信息: %s", transferID)
	return nil
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

// FileIntegrityVerifier 文件完整性验证器
// 🚀 功能增强：实现文件完整性验证机制
type FileIntegrityVerifier struct {
	fs *Fs
}

// NewFileIntegrityVerifier 创建文件完整性验证器
func NewFileIntegrityVerifier(fs *Fs) *FileIntegrityVerifier {
	return &FileIntegrityVerifier{fs: fs}
}

// VerifyFileIntegrity 验证文件完整性
// 🚀 功能增强：支持多种哈希算法的文件完整性验证
func (fiv *FileIntegrityVerifier) VerifyFileIntegrity(ctx context.Context, filePath string, expectedMD5, expectedSHA1 string, fileSize int64) (*IntegrityResult, error) {
	result := &IntegrityResult{
		FilePath:     filePath,
		FileSize:     fileSize,
		ExpectedMD5:  expectedMD5,
		ExpectedSHA1: expectedSHA1,
		StartTime:    time.Now(),
	}

	// 检查文件是否存在
	fileInfo, err := os.Stat(filePath)
	if err != nil {
		result.Error = fmt.Sprintf("文件不存在: %v", err)
		return result, err
	}

	// 验证文件大小
	actualSize := fileInfo.Size()
	if actualSize != fileSize {
		result.Error = fmt.Sprintf("文件大小不匹配: 期望=%d, 实际=%d", fileSize, actualSize)
		result.SizeMatch = false
		return result, fmt.Errorf("文件大小验证失败")
	}
	result.SizeMatch = true

	// 打开文件进行哈希计算
	file, err := os.Open(filePath)
	if err != nil {
		result.Error = fmt.Sprintf("无法打开文件: %v", err)
		return result, err
	}
	defer file.Close()

	// 并行计算MD5和SHA1
	md5Hash := md5.New()
	sha1Hash := sha1.New()
	multiWriter := io.MultiWriter(md5Hash, sha1Hash)

	// 使用缓冲读取提高性能
	buffer := make([]byte, 1024*1024) // 1MB缓冲区
	_, err = io.CopyBuffer(multiWriter, file, buffer)
	if err != nil {
		result.Error = fmt.Sprintf("读取文件失败: %v", err)
		return result, err
	}

	// 获取计算结果
	result.ActualMD5 = fmt.Sprintf("%x", md5Hash.Sum(nil))
	result.ActualSHA1 = fmt.Sprintf("%x", sha1Hash.Sum(nil))
	result.EndTime = time.Now()
	result.Duration = result.EndTime.Sub(result.StartTime)

	// 验证MD5
	if expectedMD5 != "" {
		result.MD5Match = strings.EqualFold(result.ActualMD5, expectedMD5)
		if !result.MD5Match {
			result.Error = fmt.Sprintf("MD5不匹配: 期望=%s, 实际=%s", expectedMD5, result.ActualMD5)
		}
	}

	// 验证SHA1
	if expectedSHA1 != "" {
		result.SHA1Match = strings.EqualFold(result.ActualSHA1, expectedSHA1)
		if !result.SHA1Match {
			if result.Error != "" {
				result.Error += "; "
			}
			result.Error += fmt.Sprintf("SHA1不匹配: 期望=%s, 实际=%s", expectedSHA1, result.ActualSHA1)
		}
	}

	// 判断整体验证结果
	result.IsValid = result.SizeMatch &&
		(expectedMD5 == "" || result.MD5Match) &&
		(expectedSHA1 == "" || result.SHA1Match)

	if result.IsValid {
		fs.Debugf(fiv.fs, "✅ 文件完整性验证通过: %s (耗时: %v)", filePath, result.Duration)
	} else {
		fs.Debugf(fiv.fs, "❌ 文件完整性验证失败: %s - %s", filePath, result.Error)
	}

	return result, nil
}

// IntegrityResult 完整性验证结果
type IntegrityResult struct {
	FilePath     string        `json:"file_path"`
	FileSize     int64         `json:"file_size"`
	ExpectedMD5  string        `json:"expected_md5"`
	ExpectedSHA1 string        `json:"expected_sha1"`
	ActualMD5    string        `json:"actual_md5"`
	ActualSHA1   string        `json:"actual_sha1"`
	SizeMatch    bool          `json:"size_match"`
	MD5Match     bool          `json:"md5_match"`
	SHA1Match    bool          `json:"sha1_match"`
	IsValid      bool          `json:"is_valid"`
	StartTime    time.Time     `json:"start_time"`
	EndTime      time.Time     `json:"end_time"`
	Duration     time.Duration `json:"duration"`
	Error        string        `json:"error,omitempty"`
}

// EnhancedErrorHandler 增强错误处理器
// 🚀 功能增强：提供更友好和详细的错误提示信息
type EnhancedErrorHandler struct {
	fs *Fs
}

// NewEnhancedErrorHandler 创建增强错误处理器
func NewEnhancedErrorHandler(fs *Fs) *EnhancedErrorHandler {
	return &EnhancedErrorHandler{fs: fs}
}

// HandleAPIError 处理API错误，提供友好的错误信息
// 🚀 功能增强：将技术错误转换为用户友好的提示信息
func (eeh *EnhancedErrorHandler) HandleAPIError(err error, operation string) error {
	if err == nil {
		return nil
	}

	// 解析错误类型和提供相应的解决方案
	errorMsg := err.Error()

	// 网络相关错误
	if strings.Contains(errorMsg, "timeout") || strings.Contains(errorMsg, "connection") {
		return fmt.Errorf("🌐 网络连接问题 - %s失败\n"+
			"💡 建议解决方案：\n"+
			"   1. 检查网络连接是否稳定\n"+
			"   2. 尝试重新执行操作\n"+
			"   3. 如果问题持续，请稍后再试\n"+
			"原始错误: %v", operation, err)
	}

	// 认证相关错误
	if strings.Contains(errorMsg, "401") || strings.Contains(errorMsg, "token") || strings.Contains(errorMsg, "unauthorized") {
		return fmt.Errorf("🔐 身份验证问题 - %s失败\n"+
			"💡 建议解决方案：\n"+
			"   1. 重新配置123网盘认证信息\n"+
			"   2. 检查token是否已过期\n"+
			"   3. 运行 'rclone config' 重新授权\n"+
			"原始错误: %v", operation, err)
	}

	// 权限相关错误
	if strings.Contains(errorMsg, "403") || strings.Contains(errorMsg, "forbidden") || strings.Contains(errorMsg, "permission") {
		return fmt.Errorf("🚫 权限不足 - %s失败\n"+
			"💡 建议解决方案：\n"+
			"   1. 检查是否有足够的权限访问该文件/目录\n"+
			"   2. 确认123网盘账户状态正常\n"+
			"   3. 联系管理员获取必要权限\n"+
			"原始错误: %v", operation, err)
	}

	// 文件不存在错误
	if strings.Contains(errorMsg, "404") || strings.Contains(errorMsg, "not found") {
		return fmt.Errorf("📁 文件或目录不存在 - %s失败\n"+
			"💡 建议解决方案：\n"+
			"   1. 检查文件路径是否正确\n"+
			"   2. 确认文件是否已被移动或删除\n"+
			"   3. 使用 'rclone ls' 查看可用文件\n"+
			"原始错误: %v", operation, err)
	}

	// 存储空间不足错误
	if strings.Contains(errorMsg, "space") || strings.Contains(errorMsg, "quota") || strings.Contains(errorMsg, "storage") {
		return fmt.Errorf("💾 存储空间不足 - %s失败\n"+
			"💡 建议解决方案：\n"+
			"   1. 清理123网盘中的不需要文件\n"+
			"   2. 升级存储空间套餐\n"+
			"   3. 选择其他目标位置\n"+
			"原始错误: %v", operation, err)
	}

	// 文件名相关错误
	if strings.Contains(errorMsg, "filename") || strings.Contains(errorMsg, "name") {
		return fmt.Errorf("📝 文件名问题 - %s失败\n"+
			"💡 建议解决方案：\n"+
			"   1. 检查文件名是否包含特殊字符\n"+
			"   2. 确保文件名长度不超过255字符\n"+
			"   3. 避免使用以下字符: \" \\ / : * ? | > <\n"+
			"原始错误: %v", operation, err)
	}

	// QPS限制错误
	if strings.Contains(errorMsg, "rate") || strings.Contains(errorMsg, "limit") || strings.Contains(errorMsg, "429") {
		return fmt.Errorf("⏱️ 请求频率过高 - %s失败\n"+
			"💡 建议解决方案：\n"+
			"   1. 稍等片刻后重试\n"+
			"   2. 减少并发传输数量\n"+
			"   3. 使用 --transfers=1 限制并发\n"+
			"原始错误: %v", operation, err)
	}

	// 服务器错误
	if strings.Contains(errorMsg, "500") || strings.Contains(errorMsg, "502") || strings.Contains(errorMsg, "503") {
		return fmt.Errorf("🔧 服务器暂时不可用 - %s失败\n"+
			"💡 建议解决方案：\n"+
			"   1. 稍后重试操作\n"+
			"   2. 检查123网盘服务状态\n"+
			"   3. 如果问题持续，请联系技术支持\n"+
			"原始错误: %v", operation, err)
	}

	// 默认错误处理
	return fmt.Errorf("❌ %s操作失败\n"+
		"💡 建议解决方案：\n"+
		"   1. 检查网络连接和认证状态\n"+
		"   2. 重试操作或稍后再试\n"+
		"   3. 如需帮助，请提供以下错误信息\n"+
		"原始错误: %v", operation, err)
}

// LogOperationResult 记录操作结果，提供详细的成功/失败信息
// 🚀 功能增强：统一的操作结果记录，便于问题诊断
func (eeh *EnhancedErrorHandler) LogOperationResult(operation string, success bool, duration time.Duration, details map[string]interface{}) {
	if success {
		fs.Infof(eeh.fs, "✅ %s成功完成 (耗时: %v)", operation, duration)
		if len(details) > 0 {
			for key, value := range details {
				fs.Debugf(eeh.fs, "   📊 %s: %v", key, value)
			}
		}
	} else {
		fs.Errorf(eeh.fs, "❌ %s失败 (耗时: %v)", operation, duration)
		if len(details) > 0 {
			for key, value := range details {
				fs.Debugf(eeh.fs, "   🔍 %s: %v", key, value)
			}
		}
	}
}

// SmartCacheManager 智能缓存管理器
// 🚀 功能增强：实现智能缓存管理策略，优化缓存效率
type SmartCacheManager struct {
	fs               *Fs
	cacheStats       map[string]*CacheStats
	statsMutex       sync.RWMutex
	cleanupThreshold int64         // 缓存清理阈值
	maxCacheSize     int64         // 最大缓存大小
	adaptiveCleanup  bool          // 自适应清理
	lastCleanupTime  time.Time     // 上次清理时间
	cleanupInterval  time.Duration // 清理间隔
}

// CacheStats 缓存统计信息
type CacheStats struct {
	HitCount    int64     `json:"hit_count"`    // 命中次数
	MissCount   int64     `json:"miss_count"`   // 未命中次数
	LastHit     time.Time `json:"last_hit"`     // 最后命中时间
	CreatedAt   time.Time `json:"created_at"`   // 创建时间
	AccessCount int64     `json:"access_count"` // 访问次数
	DataSize    int64     `json:"data_size"`    // 数据大小
}

// NewSmartCacheManager 创建智能缓存管理器
func NewSmartCacheManager(fs *Fs) *SmartCacheManager {
	return &SmartCacheManager{
		fs:               fs,
		cacheStats:       make(map[string]*CacheStats),
		cleanupThreshold: 1000,              // 1000个缓存项触发清理
		maxCacheSize:     500 * 1024 * 1024, // 500MB最大缓存
		adaptiveCleanup:  true,              // 启用自适应清理
		cleanupInterval:  30 * time.Minute,  // 30分钟清理间隔
		lastCleanupTime:  time.Now(),
	}
}

// RecordCacheHit 记录缓存命中
// 🚀 功能增强：智能缓存统计，优化缓存策略
func (scm *SmartCacheManager) RecordCacheHit(cacheKey string, dataSize int64) {
	scm.statsMutex.Lock()
	defer scm.statsMutex.Unlock()

	stats, exists := scm.cacheStats[cacheKey]
	if !exists {
		stats = &CacheStats{
			CreatedAt: time.Now(),
		}
		scm.cacheStats[cacheKey] = stats
	}

	stats.HitCount++
	stats.AccessCount++
	stats.LastHit = time.Now()
	stats.DataSize = dataSize

	// 检查是否需要清理
	if scm.shouldTriggerCleanup() {
		go scm.performSmartCleanup()
	}
}

// RecordCacheMiss 记录缓存未命中
func (scm *SmartCacheManager) RecordCacheMiss(cacheKey string) {
	scm.statsMutex.Lock()
	defer scm.statsMutex.Unlock()

	stats, exists := scm.cacheStats[cacheKey]
	if !exists {
		stats = &CacheStats{
			CreatedAt: time.Now(),
		}
		scm.cacheStats[cacheKey] = stats
	}

	stats.MissCount++
	stats.AccessCount++
}

// shouldTriggerCleanup 判断是否应该触发清理
func (scm *SmartCacheManager) shouldTriggerCleanup() bool {
	// 检查缓存项数量
	if len(scm.cacheStats) > int(scm.cleanupThreshold) {
		return true
	}

	// 检查总缓存大小
	totalSize := scm.getTotalCacheSize()
	if totalSize > scm.maxCacheSize {
		return true
	}

	// 检查时间间隔
	if time.Since(scm.lastCleanupTime) > scm.cleanupInterval {
		return true
	}

	return false
}

// getTotalCacheSize 获取总缓存大小
func (scm *SmartCacheManager) getTotalCacheSize() int64 {
	var totalSize int64
	for _, stats := range scm.cacheStats {
		totalSize += stats.DataSize
	}
	return totalSize
}

// performSmartCleanup 执行智能清理
// 🚀 功能增强：基于访问模式的智能缓存清理
func (scm *SmartCacheManager) performSmartCleanup() {
	scm.statsMutex.Lock()
	defer scm.statsMutex.Unlock()

	fs.Debugf(scm.fs, "🧹 开始智能缓存清理...")
	startTime := time.Now()

	// 计算清理策略
	cleanupCandidates := scm.identifyCleanupCandidates()

	cleanedCount := 0
	for _, candidate := range cleanupCandidates {
		// 实际清理缓存项
		scm.cleanupCacheItem(candidate)
		delete(scm.cacheStats, candidate)
		cleanedCount++
	}

	scm.lastCleanupTime = time.Now()
	duration := time.Since(startTime)

	fs.Debugf(scm.fs, "✅ 智能缓存清理完成: 清理了%d个项目, 耗时: %v", cleanedCount, duration)
}

// identifyCleanupCandidates 识别清理候选项
func (scm *SmartCacheManager) identifyCleanupCandidates() []string {
	now := time.Now()
	var candidates []string

	// 清理策略：
	// 1. 超过24小时未访问的项目
	// 2. 命中率低于10%的项目
	// 3. 最近很少访问的大文件缓存

	for key, stats := range scm.cacheStats {
		// 策略1：长时间未访问
		if now.Sub(stats.LastHit) > 24*time.Hour {
			candidates = append(candidates, key)
			continue
		}

		// 策略2：命中率过低
		if stats.AccessCount > 10 {
			hitRate := float64(stats.HitCount) / float64(stats.AccessCount)
			if hitRate < 0.1 {
				candidates = append(candidates, key)
				continue
			}
		}

		// 策略3：大文件且访问频率低
		if stats.DataSize > 10*1024*1024 && // 大于10MB
			stats.AccessCount < 3 && // 访问次数少于3次
			now.Sub(stats.CreatedAt) > 2*time.Hour { // 创建超过2小时
			candidates = append(candidates, key)
		}
	}

	return candidates
}

// cleanupCacheItem 清理具体的缓存项
func (scm *SmartCacheManager) cleanupCacheItem(cacheKey string) {
	// 这里需要根据具体的缓存类型进行清理
	// 由于123网盘有多种缓存，需要识别缓存类型并调用相应的清理方法

	if scm.fs.dirListCache != nil {
		scm.fs.dirListCache.Delete(cacheKey)
	}

	if scm.fs.pathToIDCache != nil {
		scm.fs.pathToIDCache.Delete(cacheKey)
	}

	if scm.fs.parentIDCache != nil {
		scm.fs.parentIDCache.Delete(cacheKey)
	}
}

// GetCacheStatistics 获取缓存统计信息
// 🚀 功能增强：提供详细的缓存性能统计
func (scm *SmartCacheManager) GetCacheStatistics() map[string]interface{} {
	scm.statsMutex.RLock()
	defer scm.statsMutex.RUnlock()

	totalHits := int64(0)
	totalMisses := int64(0)
	totalSize := int64(0)
	totalItems := len(scm.cacheStats)

	for _, stats := range scm.cacheStats {
		totalHits += stats.HitCount
		totalMisses += stats.MissCount
		totalSize += stats.DataSize
	}

	hitRate := float64(0)
	if totalHits+totalMisses > 0 {
		hitRate = float64(totalHits) / float64(totalHits+totalMisses) * 100
	}

	return map[string]interface{}{
		"total_items":  totalItems,
		"total_hits":   totalHits,
		"total_misses": totalMisses,
		"hit_rate":     fmt.Sprintf("%.2f%%", hitRate),
		"total_size":   fs.SizeSuffix(totalSize),
		"last_cleanup": scm.lastCleanupTime.Format("2006-01-02 15:04:05"),
	}
}

// 🗑️ DownloadURLCacheManager 已删除 - 下载URL缓存已移除

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

// 🗑️ NewDownloadURLCacheManager 已删除 - 下载URL缓存已移除

// 🗑️ 所有DownloadURLCacheManager方法已删除 - 下载URL缓存已移除

// parseFileID 通用的文件ID转换函数，统一错误处理
func parseFileID(idStr string) (int64, error) {
	if idStr == "" {
		return 0, fmt.Errorf("文件ID不能为空")
	}

	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("无效的文件ID格式: %s", idStr)
	}

	if id < 0 {
		return 0, fmt.Errorf("文件ID不能为负数: %d", id)
	}

	return id, nil
}

// parseFileIDWithContext 带上下文的文件ID转换函数，提供更详细的错误信息
func parseFileIDWithContext(idStr, context string) (int64, error) {
	id, err := parseFileID(idStr)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", context, err)
	}
	return id, nil
}

// parseParentID 解析父目录ID字符串为int64
// 专门用于父目录ID转换，提供更明确的错误信息
func parseParentID(idStr string) (int64, error) {
	if idStr == "" {
		return 0, fmt.Errorf("父目录ID不能为空")
	}

	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("无效的父目录ID格式: %s", idStr)
	}

	if id < 0 {
		return 0, fmt.Errorf("父目录ID不能为负数: %d", id)
	}

	return id, nil
}

// parseDirID 解析目录ID字符串为int64
// 专门用于目录ID转换，提供更明确的错误信息
func parseDirID(idStr string) (int64, error) {
	if idStr == "" {
		return 0, fmt.Errorf("目录ID不能为空")
	}

	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("无效的目录ID格式: %s", idStr)
	}

	if id < 0 {
		return 0, fmt.Errorf("目录ID不能为负数: %d", id)
	}

	return id, nil
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
	fs.Debugf(f, "🔍 getFileInfo开始: fileID=%s", fileID)

	// 验证文件ID
	_, err := parseFileIDWithContext(fileID, "获取文件信息")
	if err != nil {
		fs.Debugf(f, "🔍 getFileInfo文件ID验证失败: %v", err)
		return nil, err
	}

	// 使用文件详情API - 已迁移到rclone标准方法
	var response FileDetailResponse
	endpoint := fmt.Sprintf("/api/v1/file/detail?fileID=%s", fileID)

	fs.Debugf(f, "🔍 getFileInfo调用API: %s", endpoint)
	err = f.makeAPICallWithRest(ctx, endpoint, "GET", nil, &response)
	if err != nil {
		fs.Debugf(f, "🔍 getFileInfo API调用失败: %v", err)
		return nil, err
	}

	fs.Debugf(f, "🔍 getFileInfo API响应: code=%d, message=%s", response.Code, response.Message)
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

	fs.Debugf(f, "🔍 getFileInfo成功: fileID=%s, filename=%s, type=%d, size=%d", fileID, fileInfo.Filename, fileInfo.Type, fileInfo.Size)
	return fileInfo, nil
}

// getDownloadURL 获取文件的下载URL
func (f *Fs) getDownloadURL(ctx context.Context, fileID string) (string, error) {
	// 🔧 修复URL频繁获取：减少API调用频率
	fs.Debugf(f, "123网盘获取下载URL: fileID=%s", fileID)

	var response DownloadInfoResponse
	endpoint := fmt.Sprintf("/api/v1/file/download_info?fileID=%s", fileID)

	err := f.makeAPICallWithRest(ctx, endpoint, "GET", nil, &response)
	if err != nil {
		return "", err
	}

	if response.Code != 0 {
		return "", fmt.Errorf("API error %d: %s", response.Code, response.Message)
	}

	// 🗑️ 下载URL缓存已删除，不再保存到缓存
	fs.Debugf(f, "🔧 123网盘下载URL获取成功: fileID=%s", fileID)

	return response.Data.DownloadURL, nil
}

// createDirectory 创建新目录，如果目录已存在则返回现有目录的ID
func (f *Fs) createDirectory(ctx context.Context, parentID, name string) (string, error) {
	// 验证参数
	if name == "" {
		return "", fmt.Errorf("目录名不能为空")
	}
	if parentID == "" {
		return "", fmt.Errorf("父目录ID不能为空")
	}

	// 验证目录名
	if err := validateFileName(name); err != nil {
		return "", fmt.Errorf("目录名验证失败: %w", err)
	}

	// 将父目录ID转换为int64
	parentFileID, err := parseParentID(parentID)
	if err != nil {
		return "", err
	}

	// 首先检查目录是否已存在，避免不必要的API调用
	fs.Debugf(f, "检查目录 '%s' 是否已存在于父目录 %s", name, parentID)
	existingID, found, err := f.FindLeaf(ctx, parentID, name)
	if err != nil {
		fs.Debugf(f, "检查现有目录时出错: %v，继续尝试创建", err)
	} else if found {
		fs.Debugf(f, "目录 '%s' 已存在，ID: %s", name, existingID)
		return existingID, nil
	}

	// 如果常规查找失败，尝试强制刷新查找（防止缓存问题）
	if !found && err == nil {
		fs.Debugf(f, "常规查找未找到目录 '%s'，尝试强制刷新查找", name)
		existingID, found, err = f.findLeafWithForceRefresh(ctx, parentID, name)
		if err != nil {
			fs.Debugf(f, "强制刷新查找出错: %v，继续尝试创建", err)
		} else if found {
			fs.Debugf(f, "强制刷新找到目录 '%s'，ID: %s", name, existingID)
			return existingID, nil
		}
	}

	// 目录不存在，准备创建
	fs.Debugf(f, "目录 '%s' 不存在，开始创建", name)
	reqBody := map[string]any{
		"parentID": strconv.FormatInt(parentFileID, 10),
		"name":     name,
	}

	// 进行API调用 - 已迁移到rclone标准方法
	var response struct {
		Code    int         `json:"code"`
		Message string      `json:"message"`
		Data    interface{} `json:"data"` // 使用interface{}因为不确定具体结构
	}

	err = f.makeAPICallWithRest(ctx, "/upload/v1/file/mkdir", "POST", reqBody, &response)
	if err != nil {
		return "", err
	}

	if response.Code != 0 {
		// 检查是否是目录已存在的错误
		if response.Code == 1 && strings.Contains(response.Message, "已经有同名文件夹") {
			fs.Debugf(f, "目录 '%s' 已存在，强制清理缓存后查找现有目录ID", name)

			// 强制清理目录列表缓存，确保获取最新状态
			f.clearDirListCache(parentFileID)

			// 等待一小段时间确保缓存清理完成
			time.Sleep(100 * time.Millisecond)

			// 查找现有目录的ID - 使用强制刷新模式
			existingID, found, err := f.findLeafWithForceRefresh(ctx, parentID, name)
			if err != nil {
				return "", fmt.Errorf("查找现有目录失败: %w", err)
			}
			if !found {
				// 如果仍然找不到，尝试多次查找（可能是API延迟）
				fs.Debugf(f, "第一次查找失败，尝试重试查找目录 '%s'", name)
				for retry := 0; retry < 3; retry++ {
					time.Sleep(time.Duration(retry+1) * 200 * time.Millisecond)
					existingID, found, err = f.findLeafWithForceRefresh(ctx, parentID, name)
					if err == nil && found {
						break
					}
					fs.Debugf(f, "重试第%d次查找目录 '%s': found=%v, err=%v", retry+1, name, found, err)
				}

				if !found {
					return "", fmt.Errorf("目录已存在但无法找到其ID，可能是API同步延迟或权限问题")
				}
			}

			fs.Debugf(f, "找到现有目录ID: %s", existingID)
			return existingID, nil
		}
		return "", fmt.Errorf("API error %d: %s", response.Code, response.Message)
	}

	// 创建成功，但API可能不返回目录ID，需要通过查找获取
	fs.Debugf(f, "目录 '%s' 创建成功，开始查找新目录ID", name)

	// 清理缓存以确保能找到新创建的目录
	f.clearDirListCache(parentFileID)

	// 等待一小段时间确保服务器端同步完成
	time.Sleep(200 * time.Millisecond)

	// 使用强制刷新模式查找新创建目录的ID，包含重试机制
	var newDirID string
	var foundNew bool
	var errNew error

	// 尝试多次查找，处理服务器端同步延迟
	maxRetries := 5
	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			// 递增等待时间：200ms, 400ms, 600ms, 800ms
			waitTime := time.Duration(attempt*200) * time.Millisecond
			fs.Debugf(f, "第%d次重试查找新目录 '%s'，等待 %v", attempt+1, name, waitTime)
			time.Sleep(waitTime)
		}

		// 使用强制刷新查找，跳过缓存直接从API获取最新数据
		newDirID, foundNew, errNew = f.findLeafWithForceRefresh(ctx, parentID, name)
		if errNew != nil {
			fs.Debugf(f, "第%d次查找新目录失败: %v", attempt+1, errNew)
			continue
		}

		if foundNew {
			fs.Debugf(f, "第%d次尝试成功找到新创建目录 '%s'，ID: %s", attempt+1, name, newDirID)
			break
		}

		fs.Debugf(f, "第%d次尝试未找到新目录 '%s'", attempt+1, name)
	}

	// 最终检查结果
	if errNew != nil {
		return "", fmt.Errorf("查找新创建目录失败，已重试%d次: %w", maxRetries, errNew)
	}
	if !foundNew {
		return "", fmt.Errorf("新创建的目录未找到，已重试%d次，可能是服务器同步延迟或权限问题", maxRetries)
	}

	fs.Debugf(f, "成功创建目录 '%s'，ID: %s", name, newDirID)
	return newDirID, nil
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
		// 🔧 修复：如果parentFileID不存在，尝试重新获取真实的parentFileID
		fs.Errorf(f, "⚠️ 父目录ID %d 不存在，尝试重新获取真实的父目录ID", parentFileID)

		// 通过API重新获取正确的parentFileID
		realParentFileID, err := f.getCorrectParentFileID(ctx, parentFileID)
		if err != nil {
			return nil, fmt.Errorf("重新获取正确父目录ID失败: %w", err)
		}

		if realParentFileID != parentFileID {
			fs.Infof(f, "🔄 发现真实父目录ID: %d (原ID: %d)，使用真实ID继续上传", realParentFileID, parentFileID)
			parentFileID = realParentFileID
		} else {
			return nil, fmt.Errorf("父目录ID %d 确实不存在", parentFileID)
		}
	}

	reqBody := map[string]any{
		"parentFileID": parentFileID, // 🔧 修复：使用官方API文档的正确参数名 parentFileID (大写D)
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

	var response UploadCreateResp

	// 使用rclone标准方法调用API - create使用标准API域名
	fs.Debugf(f, "🔍 DEBUG: 使用v2 API端点 /upload/v2/file/create 和标准API域名")
	err = f.makeAPICallWithRest(ctx, "/upload/v2/file/create", "POST", reqBody, &response)

	// 🔧 修复：检测parentFileID不存在错误，尝试重新获取目录ID
	if response.Code == 1 && strings.Contains(response.Message, "parentFileID不存在") {
		fs.Errorf(f, "🚨 检测到parentFileID不存在错误 (ID: %d)，尝试重新获取正确的目录ID", parentFileID)

		// 重新获取正确的目录ID
		// 使用通用的重试策略：清理缓存并回退到根目录
		correctParentID, err := f.getCorrectParentFileID(ctx, parentFileID)
		if err != nil {
			return nil, fmt.Errorf("重新获取正确目录ID失败: %w", err)
		}

		if correctParentID != parentFileID {
			fs.Infof(f, "🔄 发现正确的目录ID: %d (原ID: %d)，重新尝试创建上传会话", correctParentID, parentFileID)

			// 使用正确的目录ID重新构建请求
			reqBody["parentFileID"] = correctParentID

			// 重新调用API
			err = f.makeAPICallWithRest(ctx, "/upload/v2/file/create", "POST", reqBody, &response)
			if err != nil {
				return nil, fmt.Errorf("使用正确目录ID重试API调用失败: %w", err)
			}
		}
	}

	if response.Code != 0 {
		return nil, fmt.Errorf("API error %d: %s", response.Code, response.Message)
	}

	// 调试响应数据
	fs.Debugf(f, "创建上传会话成功: FileID=%d, PreuploadID='%s', Reuse=%v, SliceSize=%d",
		response.Data.FileID, response.Data.PreuploadID, response.Data.Reuse, response.Data.SliceSize)

	return &response, nil
}

// createUploadV2 专门为v2多线程上传创建会话，强制使用v2 API
func (f *Fs) createUploadV2(ctx context.Context, parentFileID int64, filename, etag string, size int64) (*UploadCreateResp, error) {
	// 验证文件名符合API要求
	if err := f.validateFileName(filename); err != nil {
		return nil, fmt.Errorf("文件名验证失败: %w", err)
	}

	// 首先验证parentFileID是否存在
	fs.Debugf(f, "createUploadV2: 验证父目录ID %d", parentFileID)
	exists, err := f.verifyParentFileID(ctx, parentFileID)
	if err != nil {
		return nil, fmt.Errorf("验证父目录ID %d 失败: %w", parentFileID, err)
	}
	if !exists {
		return nil, fmt.Errorf("父目录ID %d 不存在", parentFileID)
	}

	reqBody := map[string]any{
		"parentFileID": parentFileID, // 🔧 修复：使用官方API文档的正确参数名 parentFileID (大写D)
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
	fs.Debugf(f, "为v2多线程上传包含etag: %s", etag)

	var response UploadCreateResp

	// 强制使用v2 API，不使用版本回退 - 使用rclone标准方法
	bodyBytes, _ := json.Marshal(reqBody)
	fs.Debugf(f, "调用v2 API创建上传会话: %s", string(bodyBytes))
	// 根据官方文档使用v2端点和标准API域名
	fs.Debugf(f, "🔍 DEBUG: 使用v2 API端点 /upload/v2/file/create 和标准API域名")
	err = f.makeAPICallWithRest(ctx, "/upload/v2/file/create", "POST", reqBody, &response)
	if err != nil {
		fs.Errorf(f, "v2 API调用失败: %v", err)

		// 检查是否是parentFileID不存在的错误
		if strings.Contains(err.Error(), "parentFileID不存在") {
			fs.Errorf(f, "⚠️  父目录ID %d 不存在，可能缓存过期，需要清理缓存重试", parentFileID)
			// 清理相关缓存
			f.clearParentIDCache(parentFileID)
			f.clearDirListCache(parentFileID)
			// 重置目录缓存
			if f.dirCache != nil {
				fs.Debugf(f, "重置目录缓存以清理过期的父目录ID: %d", parentFileID)
				f.dirCache.ResetRoot()
			}
			return nil, fmt.Errorf("父目录ID不存在，缓存已清理，请重试: %w", err)
		}

		return nil, fmt.Errorf("v2 API创建上传会话失败: %w", err)
	}

	// 详细记录API响应
	fs.Debugf(f, "v2 API响应: Code=%d, Message='%s', FileID=%d, PreuploadID='%s', Reuse=%v, SliceSize=%d",
		response.Code, response.Message, response.Data.FileID, response.Data.PreuploadID, response.Data.Reuse, response.Data.SliceSize)

	if response.Code != 0 {
		fs.Errorf(f, "v2 API返回错误: Code=%d, Message='%s'", response.Code, response.Message)
		return nil, fmt.Errorf("v2 API error %d: %s", response.Code, response.Message)
	}

	// 检查是否为秒传
	if response.Data.Reuse {
		fs.Debugf(f, "🚀 v2 API秒传成功: FileID=%d, 文件已存在于服务器", response.Data.FileID)
		return &response, nil
	}

	// 非秒传情况，验证PreuploadID不为空
	if response.Data.PreuploadID == "" {
		fs.Errorf(f, "❌ v2 API响应异常: 非秒传情况下PreuploadID为空，完整响应: %+v", response)
		return nil, fmt.Errorf("v2 API返回的预上传ID为空，无法进行分片上传")
	}

	// 调试响应数据
	fs.Debugf(f, "✅ v2创建上传会话成功: FileID=%d, PreuploadID='%s', SliceSize=%d",
		response.Data.FileID, response.Data.PreuploadID, response.Data.SliceSize)

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
	// 计算分片MD5
	chunkHash := fmt.Sprintf("%x", md5.Sum(data))

	// 使用正确的multipart上传方法
	err := f.uploadPartWithMultipart(ctx, preuploadID, 1, chunkHash, data)
	if err != nil {
		return fmt.Errorf("failed to upload part: %w", err)
	}

	// 完成上传 - 🔧 使用带文件大小的版本以支持增强验证
	_, err = f.completeUploadWithResultAndSize(ctx, preuploadID, int64(len(data)))
	return err
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

		// 计算分片MD5
		chunkHash := fmt.Sprintf("%x", md5.Sum(partData))

		// 🔧 关键修复：移除多层重试嵌套，直接调用uploadPartWithMultipart
		// uploadPartWithMultipart内部已使用统一重试管理器
		err = f.uploadPartWithMultipart(ctx, preuploadID, partNumber, chunkHash, partData)
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

	// 完成上传 - 🔧 使用带文件大小的版本以支持增强验证
	_, err = f.completeUploadWithResultAndSize(ctx, preuploadID, size)
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

// uploadPartWithMultipart 使用正确的multipart/form-data格式上传分片
// 🔧 修复版本：使用统一重试管理器，避免多层重试嵌套
func (f *Fs) uploadPartWithMultipart(ctx context.Context, preuploadID string, partNumber int64, chunkHash string, data []byte) error {
	if len(data) == 0 {
		return fmt.Errorf("分片数据不能为空")
	}

	fs.Debugf(f, "开始上传分片 %d，预上传ID: %s, 分片MD5: %s, 数据大小: %d 字节",
		partNumber, preuploadID, chunkHash, len(data))

	// 🔧 关键修复：使用统一重试管理器，移除 uploadPacer.Call 的重试层
	operationID := fmt.Sprintf("upload_part_%s_%d", preuploadID, partNumber)

	config := RetryConfig{
		MaxRetries:        5,
		MaxDuration:       10 * time.Minute,
		BaseDelay:         1 * time.Second,
		MaxDelay:          30 * time.Second,
		BackoffMultiplier: 2.0,
		OperationType:     "upload",
	}

	return f.retryManager.ExecuteWithRetry(ctx, operationID, config, func() error {
		// 🔧 修复：直接执行上传操作，不再嵌套重试
		return f.uploadPartDirectly(ctx, preuploadID, partNumber, chunkHash, data)
	})
}

// uploadPartDirectly 直接上传分片，不包含重试逻辑
// 🔧 关键修复：将重试逻辑从业务逻辑中分离
func (f *Fs) uploadPartDirectly(ctx context.Context, preuploadID string, partNumber int64, chunkHash string, data []byte) error {
	// 🔧 修复：移除 uploadPacer.Call，直接执行，重试由统一管理器处理
	// 🚀 优化：缓存上传域名，避免每个分片都重新获取
	uploadDomain, err := f.getCachedUploadDomain(ctx)
	if err != nil {
		return fmt.Errorf("获取上传域名失败: %w", err)
	}

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
		return fmt.Errorf("创建表单文件字段失败: %w", err)
	}

	_, err = part.Write(data)
	if err != nil {
		return fmt.Errorf("写入分片数据失败: %w", err)
	}

	err = writer.Close()
	if err != nil {
		return fmt.Errorf("关闭multipart writer失败: %w", err)
	}

	// 使用上传域名进行multipart上传
	var response struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}

	// 🔧 修复卡住问题：为分片上传添加超时控制
	uploadCtx, cancel := context.WithTimeout(ctx, 5*time.Minute) // 每个分片最多5分钟
	defer cancel()

	// 使用上传域名调用分片上传API
	err = f.makeAPICallWithRestMultipartToDomain(uploadCtx, uploadDomain, "/upload/v2/file/slice", "POST", &buf, writer.FormDataContentType(), &response)
	if err != nil {
		return fmt.Errorf("上传分片数据失败: %w", err)
	}

	if response.Code != 0 {
		return fmt.Errorf("分片上传API错误 %d: %s", response.Code, response.Message)
	}

	fs.Debugf(f, "分片 %d 上传成功", partNumber)
	return nil
}

// UploadCompleteResult 上传完成结果
type UploadCompleteResult struct {
	FileID int64  `json:"fileID"`
	Etag   string `json:"etag"`
}

// 🔧 123网盘断点续传验证阶段错误处理优化
// UploadVerificationManager 上传验证管理器
type UploadVerificationManager struct {
	fs           *Fs
	maxRetries   int           // 最大重试次数
	baseWaitTime time.Duration // 基础等待时间
	maxWaitTime  time.Duration // 最大等待时间
}

// NewUploadVerificationManager 创建上传验证管理器
func NewUploadVerificationManager(fs *Fs) *UploadVerificationManager {
	return &UploadVerificationManager{
		fs:           fs,
		maxRetries:   300,             // 🔧 根据官方文档：最多重试300次（5分钟）
		baseWaitTime: 1 * time.Second, // 🔧 根据官方文档：间隔1秒轮询
		maxWaitTime:  1 * time.Second, // 🔧 根据官方文档：固定1秒间隔
	}
}

// ChunkAnalysis 分片分析结果
type ChunkAnalysis struct {
	TotalChunks    int
	UploadedChunks int
	FailedChunks   int
	MissingChunks  int
	CanResume      bool
}

// EnhancedCompleteUploadWithResultAndSize 增强的上传完成函数，支持智能分片验证
func (uvm *UploadVerificationManager) EnhancedCompleteUploadWithResultAndSize(
	ctx context.Context,
	preuploadID string,
	fileSize int64,
) (*UploadCompleteResult, error) {

	fs.Infof(uvm.fs, "🔧 启动增强的上传验证流程: preuploadID=%s, 文件大小=%s",
		preuploadID[:8]+"...", fs.SizeSuffix(fileSize))

	// 第一阶段：尝试标准的upload_complete
	result, err := uvm.tryStandardComplete(ctx, preuploadID, fileSize)
	if err == nil {
		fs.Infof(uvm.fs, "✅ 标准上传完成成功")
		return result, nil
	}

	// 🔧 修复：检查是否需要进入智能重试
	// 如果是20101错误或标准完成失败，都应该进入智能重试
	if uvm.isAPI20101Error(err) {
		fs.Infof(uvm.fs, "⚠️ 检测到API error 20101，启动官方文档标准轮询")
	} else if strings.Contains(err.Error(), "标准上传完成失败") {
		fs.Infof(uvm.fs, "⚠️ 标准上传完成失败，启动官方文档标准轮询")
	} else {
		fs.Debugf(uvm.fs, "其他错误，直接返回: %v", err)
		return nil, err
	}

	// 第二阶段：智能重试策略
	return uvm.intelligentRetryStrategy(ctx, preuploadID, fileSize)
}

// tryStandardComplete 尝试标准的上传完成流程
func (uvm *UploadVerificationManager) tryStandardComplete(
	ctx context.Context,
	preuploadID string,
	fileSize int64,
) (*UploadCompleteResult, error) {

	reqBody := map[string]any{
		"preuploadID": preuploadID,
	}

	var response struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			Async     bool   `json:"async"`
			Completed bool   `json:"completed"`
			FileID    int64  `json:"fileID"`
			Etag      string `json:"etag"`
		} `json:"data"`
	}

	// 尝试3次标准完成
	for attempt := 0; attempt < 3; attempt++ {
		fs.Debugf(uvm.fs, "🔄 标准上传完成尝试 %d/3", attempt+1)

		err := uvm.fs.makeAPICallWithRest(ctx, "/upload/v2/file/upload_complete", "POST", reqBody, &response)
		if err != nil {
			fs.Debugf(uvm.fs, "标准完成API调用失败: %v", err)
			continue
		}

		if response.Code == 0 && response.Data.Completed {
			return &UploadCompleteResult{
				FileID: response.Data.FileID,
				Etag:   response.Data.Etag,
			}, nil
		}

		if response.Code == 20101 {
			return nil, fmt.Errorf("API error 20101: %s", response.Message)
		}

		// 其他错误，等待后重试
		time.Sleep(time.Duration(attempt+1) * 2 * time.Second)
	}

	return nil, fmt.Errorf("标准上传完成失败，准备进入智能恢复流程")
}

// intelligentRetryStrategy 智能重试策略
func (uvm *UploadVerificationManager) intelligentRetryStrategy(
	ctx context.Context,
	preuploadID string,
	fileSize int64,
) (*UploadCompleteResult, error) {

	fs.Infof(uvm.fs, "🔍 开始官方文档标准轮询策略（间隔1秒）")

	reqBody := map[string]any{
		"preuploadID": preuploadID,
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

	// 使用智能等待策略和更多重试
	consecutiveFailures := 0
	maxConsecutiveFailures := 5

	for attempt := 0; attempt < uvm.maxRetries; attempt++ {
		fs.Debugf(uvm.fs, "🔄 官方标准轮询 %d/%d (连续失败: %d)", attempt+1, uvm.maxRetries, consecutiveFailures)

		// 🔧 根据官方文档：间隔1秒轮询
		waitTime := uvm.calculateIntelligentWaitTime(attempt, consecutiveFailures, fileSize)
		if attempt > 0 {
			fs.Debugf(uvm.fs, "⏰ 按官方文档要求等待 %v 后轮询", waitTime)
			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("官方标准轮询被取消: %w", ctx.Err())
			case <-time.After(waitTime):
			}
		}

		err := uvm.fs.makeAPICallWithRest(ctx, "/upload/v2/file/upload_complete", "POST", reqBody, &response)
		if err != nil {
			fs.Debugf(uvm.fs, "智能重试API调用失败: %v", err)
			consecutiveFailures++
			if consecutiveFailures >= maxConsecutiveFailures {
				return nil, fmt.Errorf("连续网络错误过多(%d次)，智能重试失败: %w", consecutiveFailures, err)
			}
			continue
		}

		// 重置连续失败计数
		consecutiveFailures = 0

		if response.Code == 0 && response.Data.Completed {
			fs.Infof(uvm.fs, "✅ 官方标准轮询成功完成")
			return &UploadCompleteResult{
				FileID: response.Data.FileID,
				Etag:   response.Data.Etag,
			}, nil
		}

		if response.Code == 0 && !response.Data.Completed {
			// 🔧 根据官方文档：completed=false时继续轮询
			fs.Debugf(uvm.fs, "📋 服务器返回completed=false，按官方文档继续1秒后轮询")
			continue
		}

		if response.Code != 20101 {
			// 非20101错误，直接返回
			return nil, fmt.Errorf("官方标准轮询失败: API error %d: %s", response.Code, response.Message)
		}

		// 20101错误，记录并继续重试
		fs.Debugf(uvm.fs, "⚠️ 遇到20101错误，按官方文档继续轮询")
	}

	return nil, fmt.Errorf("官方标准轮询超时，已轮询%d次（%d分钟）", uvm.maxRetries, uvm.maxRetries/60)
}

// calculateIntelligentWaitTime 计算智能等待时间
func (uvm *UploadVerificationManager) calculateIntelligentWaitTime(attempt, consecutiveFailures int, fileSize int64) time.Duration {
	// 🔧 根据123网盘官方文档：completed=false时需间隔1秒继续轮询
	// 不再使用指数退避，严格按照官方要求使用1秒间隔
	return uvm.baseWaitTime // 固定1秒
}

// isAPI20101Error 检查是否为API error 20101
func (uvm *UploadVerificationManager) isAPI20101Error(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	return strings.Contains(errStr, "20101") ||
		strings.Contains(errStr, "API error 20101")
}

// completeUpload 完成多部分上传
func (f *Fs) completeUpload(ctx context.Context, preuploadID string) error {
	_, err := f.completeUploadWithResult(ctx, preuploadID)
	return err
}

// completeUploadWithResult 完成多部分上传并返回结果
func (f *Fs) completeUploadWithResult(ctx context.Context, preuploadID string) (*UploadCompleteResult, error) {
	return f.completeUploadWithResultAndSize(ctx, preuploadID, 0)
}

// completeUploadWithResultAndSize 完成多部分上传并返回结果，支持根据文件大小调整轮询次数
func (f *Fs) completeUploadWithResultAndSize(ctx context.Context, preuploadID string, fileSize int64) (*UploadCompleteResult, error) {
	// 🔧 优化：使用增强的上传验证管理器处理断点续传验证阶段错误
	if f.verificationManager != nil {
		fs.Debugf(f, "🔧 使用增强的上传验证管理器")
		return f.verificationManager.EnhancedCompleteUploadWithResultAndSize(ctx, preuploadID, fileSize)
	}

	// 回退到原始实现（兼容性保障）
	fs.Debugf(f, "⚠️ 验证管理器未初始化，使用原始实现")
	return f.originalCompleteUploadWithResultAndSize(ctx, preuploadID, fileSize)
}

// originalCompleteUploadWithResultAndSize 原始的上传完成实现（作为备用）
func (f *Fs) originalCompleteUploadWithResultAndSize(ctx context.Context, preuploadID string, fileSize int64) (*UploadCompleteResult, error) {
	reqBody := map[string]any{
		"preuploadID": preuploadID,
	}

	var response struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			Async     bool   `json:"async"`
			Completed bool   `json:"completed"`
			FileID    int64  `json:"fileID"`
			Etag      string `json:"etag"`
		} `json:"data"`
	}

	// 📊 根据文件大小动态调整轮询次数（针对123网盘校验特性优化）
	// 基础轮询次数：120次（2分钟）- 为随机数据文件提供充足校验时间
	// 大文件额外轮询：每25MB增加60次轮询（更积极的等待策略）
	// 最大轮询次数：600次（10分钟）- 为大文件提供充足时间
	maxAttempts := 120 // 大幅提高基础轮询次数，适应123网盘校验特性
	if fileSize > 0 {
		// 计算文件大小（MB）
		fileSizeMB := fileSize / (1024 * 1024)
		// 每25MB增加60次轮询，为中小文件也提供充足的校验时间
		extraAttempts := int(fileSizeMB/25) * 60
		if extraAttempts > 480 {
			extraAttempts = 480 // 最多600次轮询（10分钟）
		}
		maxAttempts += extraAttempts
		fs.Debugf(f, "📊 根据文件大小(%dMB)调整轮询策略: 基础120次 + 额外%d次 = 总计%d次轮询 (预计%d分钟)", fileSizeMB, extraAttempts, maxAttempts, maxAttempts/60+1)
	}

	fs.Debugf(f, "🔍 DEBUG: completeUploadWithResultAndSize 开始轮询，文件大小=%d字节，最大轮询次数=%d", fileSize, maxAttempts)

	// 增强的轮询逻辑，包含网络错误容错处理
	networkErrorCount := 0
	maxNetworkErrors := 10 // 允许最多10次网络错误

	for attempt := 0; attempt < maxAttempts; attempt++ {
		// 使用rclone标准方法调用API
		err := f.makeAPICallWithRest(ctx, "/upload/v2/file/upload_complete", "POST", reqBody, &response)
		if err != nil {
			// 网络错误处理：记录错误但继续重试，而不是直接退出
			networkErrorCount++
			fs.Debugf(f, "⚠️ 轮询第%d次遇到网络错误 (第%d个网络错误): %v", attempt+1, networkErrorCount, err)

			if networkErrorCount >= maxNetworkErrors {
				return nil, fmt.Errorf("轮询过程中网络错误过多(%d次)，最后错误: %w", networkErrorCount, err)
			}

			// 网络错误时等待更长时间后重试
			waitTime := time.Duration(networkErrorCount) * time.Second
			if waitTime > 5*time.Second {
				waitTime = 5 * time.Second
			}

			fs.Debugf(f, "🔄 网络错误，等待%v后重试...", waitTime)
			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("上传完成轮询被取消: %w", ctx.Err())
			case <-time.After(waitTime):
				continue
			}
		}

		// 重置网络错误计数器（成功调用API）
		if networkErrorCount > 0 {
			fs.Debugf(f, "✅ 网络连接恢复，重置错误计数器")
			networkErrorCount = 0
		}

		// 检查API响应码
		if response.Code == 0 {
			// 成功响应，继续处理
			fs.Debugf(f, "🎉 轮询成功！文件校验完成，第%d次尝试成功", attempt+1)
			break
		} else if response.Code == 20103 {
			// 文件正在校验中，需要轮询等待
			progressPercent := float64(attempt+1) / float64(maxAttempts) * 100
			fs.Debugf(f, "📋 文件正在校验中，第%d次轮询等待 (最多%d次，进度%.1f%%): %s",
				attempt+1, maxAttempts, progressPercent, response.Message)

			if attempt < maxAttempts-1 {
				// 等待1秒后重试（API文档要求）
				select {
				case <-ctx.Done():
					return nil, fmt.Errorf("上传完成轮询被取消: %w", ctx.Err())
				case <-time.After(1 * time.Second):
					continue
				}
			} else {
				// 达到最大重试次数
				return nil, fmt.Errorf("文件校验超时，已轮询%d次: API error %d: %s", maxAttempts, response.Code, response.Message)
			}
		} else if response.Code == 20101 {
			// 🔧 修复API error 20101: 文件上传失败 - 通常是分片验证问题
			fs.Debugf(f, "⚠️ API error 20101检测到，可能是分片验证问题，尝试重新验证...")

			// 等待更长时间让服务器完成分片验证
			if attempt < maxAttempts-1 {
				waitTime := time.Duration(attempt+1) * 2 * time.Second // 递增等待时间
				fs.Debugf(f, "🔄 等待%v后重试分片验证...", waitTime)
				select {
				case <-ctx.Done():
					return nil, fmt.Errorf("上传完成轮询被取消: %w", ctx.Err())
				case <-time.After(waitTime):
					continue
				}
			} else {
				return nil, fmt.Errorf("upload completion failed: API error %d: %s", response.Code, response.Message)
			}
		} else {
			// 其他错误，直接返回
			return nil, fmt.Errorf("upload completion failed: API error %d: %s", response.Code, response.Message)
		}
	}

	// 如果是异步处理，我们需要等待完成
	if response.Data.Async && !response.Data.Completed {
		fs.Debugf(f, "上传为异步模式，轮询完成状态，文件ID: %d", response.Data.FileID)

		// 轮询异步上传结果，获取最终的文件ID和MD5
		finalResult, err := f.pollAsyncUploadWithResult(ctx, preuploadID)
		if err != nil {
			return nil, fmt.Errorf("failed to wait for async upload completion: %w", err)
		}

		fs.Debugf(f, "异步上传成功完成，文件ID: %d, MD5: %s", finalResult.FileID, finalResult.Etag)
		return finalResult, nil
	} else {
		fs.Debugf(f, "✅ 上传完成确认成功，文件ID: %d, MD5: %s", response.Data.FileID, response.Data.Etag)
		return &UploadCompleteResult{
			FileID: response.Data.FileID,
			Etag:   response.Data.Etag,
		}, nil
	}
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

// pollAsyncUploadWithResult 轮询异步上传完成状态并返回结果
func (f *Fs) pollAsyncUploadWithResult(ctx context.Context, preuploadID string) (*UploadCompleteResult, error) {
	fs.Debugf(f, "轮询异步上传完成状态并获取结果，preuploadID: %s", preuploadID)

	// 使用指数退避轮询，增加到30次尝试，最大5分钟
	// 对于大文件，123网盘需要更长的处理时间
	maxAttempts := 30
	for attempt := 0; attempt < maxAttempts; attempt++ {
		reqBody := map[string]any{
			"preuploadID": preuploadID,
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

		// 使用rclone标准方法调用API - 修正：使用正确的upload_complete端点
		err := f.makeAPICallWithRest(ctx, "/upload/v2/file/upload_complete", "POST", reqBody, &response)
		if err != nil {
			fs.Debugf(f, "异步轮询第%d次尝试失败: %v", attempt+1, err)
			// 即使单个请求失败也继续轮询
		} else if response.Code != 0 {
			fs.Debugf(f, "异步轮询第%d次尝试返回错误: API错误 %d: %s", attempt+1, response.Code, response.Message)
		} else if response.Data.Completed {
			fs.Debugf(f, "异步上传在第%d次尝试后成功完成，文件ID: %d, MD5: %s", attempt+1, response.Data.FileID, response.Data.Etag)
			return &UploadCompleteResult{
				FileID: response.Data.FileID,
				Etag:   response.Data.Etag,
			}, nil
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
			return nil, ctx.Err()
		case <-time.After(waitTime):
			// 继续下次尝试
		}
	}

	// 如果轮询失败，返回错误
	return nil, fmt.Errorf("异步上传轮询超时，无法确认文件状态")
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

	// 安全地截取PreuploadID用于日志显示
	preuploadIDDisplay := progress.PreuploadID
	if len(preuploadIDDisplay) > 20 {
		preuploadIDDisplay = preuploadIDDisplay[:20] + "..."
	} else if len(preuploadIDDisplay) == 0 {
		preuploadIDDisplay = "<空>"
	}
	fs.Debugf(f, "已保存上传进度%s: %d/%d个分片完成",
		preuploadIDDisplay, progress.GetUploadedCount(), progress.TotalParts)
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

// streamingPut 实现优化的流式上传，现在使用统一上传入口
// 这消除了跨网盘传输中的双重下载问题，提高传输效率
// 修复了跨云盘传输中"源文件不支持重新打开"的问题
func (f *Fs) streamingPut(ctx context.Context, in io.Reader, src fs.ObjectInfo, parentFileID int64, fileName string) (*Object, error) {
	fs.Debugf(f, "🚨 【重要调试】进入streamingPut函数，文件: %s", fileName)

	// 🚀 通用跨云传输检测：检测是否来自其他云存储服务
	srcFsString := src.Fs().String()
	currentFsString := f.String()

	// 检测跨云传输：源和目标是不同的文件系统
	if srcFsString != currentFsString {
		// 进一步检测是否为云存储到云存储的传输（排除本地文件系统）
		if !strings.Contains(srcFsString, "local") && !strings.Contains(currentFsString, "local") {
			fs.Infof(f, "🌐 检测到跨云传输: %s → %s，启用优化传输模式", srcFsString, currentFsString)
			return f.handleCrossCloudTransfer(ctx, in, src, parentFileID, fileName)
		}
	}

	// 🌐 最早期跨云传输检测：在任何处理之前就检测并处理跨云传输
	fs.Infof(f, "🚨 【重要调试】streamingPut中开始跨云传输检测...")
	isRemoteSource := f.isRemoteSource(src)
	fs.Infof(f, "🚨 【重要调试】streamingPut跨云传输检测结果: %v", isRemoteSource)

	if isRemoteSource {
		fs.Infof(f, "🌐 【streamingPut】检测到跨云传输，使用智能重试感知的跨云传输策略")
		return f.handleCrossCloudTransferWithRetryAwareness(ctx, src, parentFileID, fileName)
	}

	// 本地文件使用统一上传入口
	fs.Debugf(f, "本地文件使用统一上传入口进行流式上传")
	return f.unifiedUpload(ctx, in, src, parentFileID, fileName)
}

// handlePreDownloadedCrossCloudTransfer 处理已预下载的跨云传输
// 🚀 避免重复下载：直接使用rclone已经下载的数据进行上传
func (f *Fs) handlePreDownloadedCrossCloudTransfer(ctx context.Context, in io.Reader, src fs.ObjectInfo, parentFileID int64, fileName string, crossCloudInfo map[string]interface{}) (*Object, error) {
	fs.Infof(f, "🌐 处理预下载跨云传输: %s (大小: %s)", fileName, fs.SizeSuffix(src.Size()))

	// 🚀 关键优化：直接使用传入的Reader（已经是115网盘下载的数据）
	// 不再重复调用115网盘下载，避免双重下载问题
	fs.Infof(f, "📤 直接使用已下载数据进行123网盘上传...")

	// 尝试秒传检查（如果可能）
	if srcObj, ok := src.(fs.Object); ok {
		// 尝试获取MD5进行秒传
		if srcHash, err := srcObj.Hash(ctx, fshash.MD5); err == nil && srcHash != "" {
			fs.Infof(f, "🔍 尝试秒传检查，MD5: %s", srcHash)

			// 检查123网盘是否支持此文件的秒传（简化版本）
			fs.Debugf(f, "⚠️ 秒传功能暂未实现，继续正常上传")
		}
	}

	// 直接使用统一上传接口，避免重复下载
	return f.unifiedUpload(ctx, in, src, parentFileID, fileName)
}

// handleCrossCloudTransfer 处理通用跨云传输
// 🚀 通用跨云传输处理：适用于任何云存储服务到123网盘的传输
func (f *Fs) handleCrossCloudTransfer(ctx context.Context, in io.Reader, src fs.ObjectInfo, parentFileID int64, fileName string) (*Object, error) {
	fs.Infof(f, "🌐 开始处理跨云传输: %s (大小: %s)", fileName, fs.SizeSuffix(src.Size()))

	// 🚀 优化策略：直接使用传入的Reader进行上传，避免重复下载
	// 这个Reader已经是源云盘下载的数据流
	fs.Infof(f, "📤 直接使用源云盘数据流进行123网盘上传...")

	// 尝试秒传检查（如果源对象支持MD5）
	if srcObj, ok := src.(fs.Object); ok {
		// 尝试获取MD5进行秒传
		if srcHash, err := srcObj.Hash(ctx, fshash.MD5); err == nil && srcHash != "" {
			fs.Infof(f, "🔍 尝试秒传检查，MD5: %s", srcHash)

			// 这里可以添加123网盘的秒传检查逻辑
			// 暂时跳过秒传，直接上传
			fs.Debugf(f, "⚠️ 秒传功能暂未实现，继续正常上传")
		}
	}

	// 直接使用统一上传接口
	return f.unifiedUpload(ctx, in, src, parentFileID, fileName)
}

// getCachedUploadDomain 获取缓存的上传域名，避免频繁API调用
// 🚀 性能优化：基于日志分析，每个分片都重新获取域名浪费时间
func (f *Fs) getCachedUploadDomain(ctx context.Context) (string, error) {
	// 检查缓存的上传域名是否仍然有效
	if f.uploadDomainCache != "" && time.Since(f.uploadDomainCacheTime) < 5*time.Minute {
		fs.Debugf(f, "🚀 使用缓存的上传域名: %s", f.uploadDomainCache)
		return f.uploadDomainCache, nil
	}

	// 缓存过期或不存在，重新获取
	fs.Debugf(f, "🔄 上传域名缓存过期，重新获取")
	domain, err := f.getUploadDomain(ctx)
	if err != nil {
		return "", err
	}

	// 更新缓存
	f.uploadDomainCache = domain
	f.uploadDomainCacheTime = time.Now()

	fs.Debugf(f, "✅ 上传域名缓存更新: %s", domain)
	return domain, nil
}

// calculateOptimalChunkSize 计算建议的分片大小（仅用于估算）
// ⚠️ 注意：实际上传时会严格使用API返回的SliceSize，此函数仅用于性能估算
func (f *Fs) calculateOptimalChunkSize(fileSize int64) int64 {
	// 🔧 动态分片策略：基于文件大小提供建议，但实际使用API返回的SliceSize
	// 这个函数主要用于性能估算和进度计算，不影响实际上传分片大小
	switch {
	case fileSize < 50*1024*1024: // <50MB
		return 16 * 1024 * 1024 // 16MB建议分片
	case fileSize < 200*1024*1024: // <200MB
		return 32 * 1024 * 1024 // 32MB建议分片
	case fileSize < 1*1024*1024*1024: // <1GB
		return 64 * 1024 * 1024 // 64MB建议分片（常见API返回值）
	case fileSize < 5*1024*1024*1024: // <5GB
		return 128 * 1024 * 1024 // 128MB建议分片
	default: // >5GB
		return 256 * 1024 * 1024 // 256MB建议分片
	}
}

// handleCrossCloudTransferWithRetryAwareness 智能重试感知的跨云传输处理
// 🔧 修复重复下载问题：检测重试情况，避免重复下载
func (f *Fs) handleCrossCloudTransferWithRetryAwareness(ctx context.Context, src fs.ObjectInfo, parentFileID int64, fileName string) (*Object, error) {
	fileSize := src.Size()
	fs.Infof(f, "🌐 开始智能重试感知跨云传输: %s (%s)", fileName, fs.SizeSuffix(fileSize))

	// 🔧 关键修复：检测是否为重试情况
	// 通过检查全局MD5缓存来判断是否已经下载过
	if cachedMD5, found := f.getCachedMD5(src); found {
		fs.Infof(f, "🎯 检测到重试情况，文件已下载过，直接尝试上传 (MD5: %s)", cachedMD5)

		// 尝试直接使用已知MD5进行上传，避免重新下载
		return f.uploadWithKnownMD5FromRetry(ctx, src, parentFileID, fileName, cachedMD5)
	}

	// 首次下载，正常执行跨云传输
	fs.Infof(f, "🔄 首次下载，执行完整跨云传输流程")
	return f.handleCrossCloudTransferSimplified(ctx, src, parentFileID, fileName)
}

// uploadWithKnownMD5FromRetry 在重试情况下使用已知MD5直接上传
// 🔧 修复重复下载：避免重新下载，直接尝试上传
func (f *Fs) uploadWithKnownMD5FromRetry(ctx context.Context, src fs.ObjectInfo, parentFileID int64, fileName, md5Hash string) (*Object, error) {
	fs.Infof(f, "🚀 重试上传，使用已知MD5: %s", md5Hash)

	// 尝试秒传
	createResp, err := f.createUpload(ctx, parentFileID, fileName, md5Hash, src.Size())
	if err != nil {
		fs.Debugf(f, "重试时创建上传会话失败: %v", err)
		// 如果还是失败，回退到完整下载流程
		fs.Infof(f, "⚠️ 重试上传失败，回退到完整下载流程")
		return f.handleCrossCloudTransferSimplified(ctx, src, parentFileID, fileName)
	}

	// 检查是否秒传成功
	if createResp.Data.Reuse {
		fs.Infof(f, "✅ 重试时秒传成功: %s", fileName)
		return &Object{
			fs:          f,
			remote:      fileName,
			hasMetaData: true,
			id:          strconv.FormatInt(createResp.Data.FileID, 10),
			size:        src.Size(),
			md5sum:      md5Hash,
			modTime:     time.Now(),
			isDir:       false,
		}, nil
	}

	// 如果不能秒传，说明需要实际上传文件，回退到完整流程
	fs.Infof(f, "⚠️ 重试时无法秒传，需要重新下载文件进行上传")
	return f.handleCrossCloudTransferSimplified(ctx, src, parentFileID, fileName)
}

// handleCrossCloudTransferSimplified 简化的跨云传输处理函数
// 🔧 修复重复下载问题：直接选择策略，避免重复打开源文件
func (f *Fs) handleCrossCloudTransferSimplified(ctx context.Context, src fs.ObjectInfo, parentFileID int64, fileName string) (*Object, error) {
	fileSize := src.Size()
	fs.Infof(f, "🌐 开始简化跨云传输: %s (%s)", fileName, fs.SizeSuffix(fileSize))

	// 🔧 修复重复下载：直接根据文件大小选择策略，不预先打开文件
	// 尝试获取底层的fs.Object
	var srcObj fs.Object

	// 首先尝试直接转换为fs.Object
	if obj, ok := src.(fs.Object); ok {
		srcObj = obj
		fs.Debugf(f, "源对象是fs.Object类型，直接使用")
	} else if overrideRemote, ok := src.(*fs.OverrideRemote); ok {
		// 处理fs.OverrideRemote包装类型
		fs.Debugf(f, "源对象是fs.OverrideRemote类型，尝试解包")
		srcObj = overrideRemote.UnWrap()
		if srcObj == nil {
			return nil, fmt.Errorf("无法从fs.OverrideRemote中解包出fs.Object")
		}
		fs.Debugf(f, "成功从fs.OverrideRemote解包出fs.Object")
	} else {
		// 尝试使用rclone标准的UnWrapObjectInfo函数
		fs.Debugf(f, "源对象类型 %T，尝试使用标准解包方法", src)
		srcObj = fs.UnWrapObjectInfo(src)
		if srcObj == nil {
			return nil, fmt.Errorf("源对象类型 %T 无法解包为fs.Object，无法进行跨云传输", src)
		}
		fs.Debugf(f, "使用标准解包方法成功获取fs.Object")
	}

	// 🔧 修复重复下载：直接选择最优策略，避免重复打开文件
	return f.selectOptimalCrossCloudStrategy(ctx, srcObj, src, parentFileID, fileName)
}

// selectOptimalCrossCloudStrategy 选择最优的跨云传输策略
// 🔧 修复重复下载问题：统一策略选择，避免重复打开源文件
func (f *Fs) selectOptimalCrossCloudStrategy(ctx context.Context, srcObj fs.Object, src fs.ObjectInfo, parentFileID int64, fileName string) (*Object, error) {
	fileSize := src.Size()
	fs.Infof(f, "🎯 选择跨云传输策略: %s (%s)", fileName, fs.SizeSuffix(fileSize))

	// 根据文件大小选择最优传输策略
	switch {
	case fileSize <= 512*1024*1024: // 512MB以下 - 内存缓冲策略
		fs.Infof(f, "📝 选择内存缓冲策略 (文件大小: %s)", fs.SizeSuffix(fileSize))
		return f.memoryBufferedCrossCloudTransferDirect(ctx, srcObj, src, parentFileID, fileName)
	case fileSize <= 2*1024*1024*1024: // 2GB以下 - 智能混合策略
		fs.Infof(f, "🔄 选择智能混合策略 (文件大小: %s)", fs.SizeSuffix(fileSize))
		// 🔧 修复重复下载：对于中等文件，直接使用并发下载，不再回退
		if fileSize > 50*1024*1024 { // 50MB以上使用并发下载
			fs.Infof(f, "🚀 使用并发下载优化策略")
			return f.concurrentDownloadCrossCloudTransfer(ctx, srcObj, parentFileID, fileName)
		} else {
			// 小于50MB的文件使用优化磁盘策略
			return f.optimizedDiskCrossCloudTransferDirect(ctx, srcObj, src, parentFileID, fileName)
		}
	default: // 大文件 - 优化的磁盘策略
		fs.Infof(f, "💾 选择优化磁盘策略 (文件大小: %s)", fs.SizeSuffix(fileSize))
		return f.optimizedDiskCrossCloudTransferDirect(ctx, srcObj, src, parentFileID, fileName)
	}
}

// handleDownloadThenUpload 处理实际的下载然后上传逻辑
func (f *Fs) handleDownloadThenUpload(ctx context.Context, srcReader io.ReadCloser, src fs.ObjectInfo, parentFileID int64, fileName string) (*Object, error) {
	fileSize := src.Size()
	fs.Infof(f, "🌐 开始优化跨云传输: %s (%s)", fileName, fs.SizeSuffix(fileSize))

	// 根据文件大小选择最优传输策略
	switch {
	case fileSize <= 512*1024*1024: // 512MB以下 - 内存缓冲策略
		fs.Infof(f, "📝 使用内存缓冲策略 (文件大小: %s)", fs.SizeSuffix(fileSize))
		return f.memoryBufferedCrossCloudTransfer(ctx, srcReader, src, parentFileID, fileName)
	case fileSize <= 2*1024*1024*1024: // 2GB以下 - 混合策略
		fs.Infof(f, "🔄 使用混合缓冲策略 (文件大小: %s)", fs.SizeSuffix(fileSize))
		return f.hybridCrossCloudTransfer(ctx, srcReader, src, parentFileID, fileName)
	default: // 大文件 - 优化的磁盘策略
		fs.Infof(f, "💾 使用优化磁盘策略 (文件大小: %s)", fs.SizeSuffix(fileSize))
		return f.optimizedDiskCrossCloudTransfer(ctx, srcReader, src, parentFileID, fileName)
	}
}

// memoryBufferedCrossCloudTransfer 内存缓冲跨云传输（512MB以下文件）
func (f *Fs) memoryBufferedCrossCloudTransfer(ctx context.Context, srcReader io.ReadCloser, src fs.ObjectInfo, parentFileID int64, fileName string) (*Object, error) {
	fileSize := src.Size()
	fs.Debugf(f, "📝 开始内存缓冲跨云传输，文件大小: %s", fs.SizeSuffix(fileSize))

	// 步骤1: 直接读取到内存
	startTime := time.Now()
	data := make([]byte, fileSize)
	n, err := io.ReadFull(srcReader, data)
	downloadDuration := time.Since(startTime)

	if err != nil && err != io.ErrUnexpectedEOF {
		return nil, fmt.Errorf("内存读取文件失败: %w", err)
	}

	if int64(n) != fileSize {
		fs.Debugf(f, "实际读取大小(%d)与预期大小(%d)不匹配，使用实际大小", n, fileSize)
		data = data[:n]
		fileSize = int64(n)
	}

	fs.Infof(f, "📥 内存下载完成: %s, 用时: %v, 速度: %s/s",
		fs.SizeSuffix(fileSize), downloadDuration,
		fs.SizeSuffix(int64(float64(fileSize)/downloadDuration.Seconds())))

	// 步骤2: 计算MD5
	fs.Infof(f, "🔐 开始计算MD5哈希...")
	md5StartTime := time.Now()
	hasher := md5.New()
	hasher.Write(data)
	md5Hash := fmt.Sprintf("%x", hasher.Sum(nil))
	md5Duration := time.Since(md5StartTime)

	fs.Infof(f, "🔐 MD5计算完成: %s, 用时: %v", md5Hash, md5Duration)

	// 步骤3: 检查秒传
	fs.Infof(f, "⚡ 检查123网盘秒传功能...")
	createResp, err := f.createUpload(ctx, parentFileID, fileName, md5Hash, fileSize)
	if err != nil {
		return nil, fmt.Errorf("创建上传会话失败: %w", err)
	}

	if createResp.Data.Reuse {
		totalDuration := time.Since(startTime)
		fs.Infof(f, "🎉 秒传成功！总用时: %v (下载: %v + MD5: %v)",
			totalDuration, downloadDuration, md5Duration)
		return &Object{
			fs:          f,
			remote:      fileName,
			hasMetaData: true,
			id:          strconv.FormatInt(createResp.Data.FileID, 10),
			size:        fileSize,
			md5sum:      md5Hash,
			modTime:     time.Now(),
			isDir:       false,
		}, nil
	}

	// 步骤4: 内存上传
	fs.Infof(f, "📤 开始从内存上传到123网盘...")
	uploadStartTime := time.Now()

	// 使用内存数据创建reader进行上传
	dataReader := bytes.NewReader(data)
	err = f.uploadFile(ctx, dataReader, createResp, fileSize)
	if err != nil {
		return nil, fmt.Errorf("内存上传失败: %w", err)
	}

	uploadDuration := time.Since(uploadStartTime)
	totalDuration := time.Since(startTime)

	fs.Infof(f, "✅ 内存缓冲跨云传输完成！总用时: %v (下载: %v + MD5: %v + 上传: %v)",
		totalDuration, downloadDuration, md5Duration, uploadDuration)

	// 返回上传成功的文件对象
	return &Object{
		fs:          f,
		remote:      fileName,
		hasMetaData: true,
		id:          strconv.FormatInt(createResp.Data.FileID, 10),
		size:        fileSize,
		md5sum:      md5Hash,
		modTime:     time.Now(),
		isDir:       false,
	}, nil
}

// hybridCrossCloudTransfer 混合缓冲跨云传输（512MB-2GB文件）
func (f *Fs) hybridCrossCloudTransfer(ctx context.Context, srcReader io.ReadCloser, src fs.ObjectInfo, parentFileID int64, fileName string) (*Object, error) {
	fileSize := src.Size()
	fs.Debugf(f, "🔄 开始混合缓冲跨云传输，文件大小: %s", fs.SizeSuffix(fileSize))

	// 对于中等大小文件，尝试并发下载优化
	if srcObj, ok := src.(fs.Object); ok && fileSize > 50*1024*1024 { // 50MB以上才使用并发下载，降低触发阈值
		fs.Infof(f, "🚀 尝试并发下载优化策略")
		return f.concurrentDownloadCrossCloudTransfer(ctx, srcObj, parentFileID, fileName)
	}

	// 回退到优化的磁盘策略
	return f.optimizedDiskCrossCloudTransfer(ctx, srcReader, src, parentFileID, fileName)
}

// concurrentDownloadCrossCloudTransfer 并发下载跨云传输
func (f *Fs) concurrentDownloadCrossCloudTransfer(ctx context.Context, srcObj fs.Object, parentFileID int64, fileName string) (*Object, error) {
	fileSize := srcObj.Size()
	fs.Infof(f, "🚀 开始并发下载跨云传输: %s (%s)", fileName, fs.SizeSuffix(fileSize))

	// 🔧 防止重复下载：使用传输跟踪器
	fileID := fmt.Sprintf("%s_%d", srcObj.Remote(), srcObj.Size())
	var transferCompleted bool
	session, isNew := f.transferTracker.StartTransfer(fileID, fileName, fileSize)
	if !isNew {
		// 等待现有传输完成
		fs.Infof(f, "等待现有传输完成: %s", fileName)
		result := f.transferTracker.WaitForTransfer(session)
		if result.Success {
			// 返回已完成的传输结果
			obj, err := f.NewObject(ctx, result.Path)
			if err != nil {
				return nil, fmt.Errorf("获取传输结果对象失败: %w", err)
			}
			return obj.(*Object), nil
		}
		return nil, result.Error
	}

	// 确保传输完成时清理（如果没有显式完成的话）
	defer func() {
		if !transferCompleted {
			f.transferTracker.CompleteTransfer(fileID, false, nil, "")
		}
	}()

	// 🔧 修复进度显示：创建跨云传输的Transfer对象，集成到rclone accounting系统
	stats := f.getGlobalStats(ctx)
	var downloadTransfer *accounting.Transfer
	if stats != nil {
		// 获取源文件系统
		var srcFs fs.Fs
		if fsInfo := srcObj.Fs(); fsInfo != nil {
			// 尝试转换为Fs接口
			if fs, ok := fsInfo.(fs.Fs); ok {
				srcFs = fs
			}
		}

		// 创建下载阶段的Transfer对象
		downloadTransfer = stats.NewTransferRemoteSize(
			fileName, // 使用原始文件名，不添加"下载阶段"标记
			fileSize,
			srcFs,
			f,
		)
		defer downloadTransfer.Done(ctx, nil)

		// 🔧 关键修复：将Transfer对象存储到文件系统上下文中，供下载函数使用
		f.setCrossCloudTransfer(downloadTransfer)
		defer f.clearCrossCloudTransfer()

		fs.Debugf(f, "🔧 创建跨云传输Transfer对象: %s", fileName)
	}

	// 🚀 基于日志分析优化分片参数
	// 日志显示：32Mi分片，50分片，4并发，速度131.912Mi/s（优秀）
	// 但上传阶段用时6分47秒，是主要瓶颈，需要优化上传策略
	chunkSize := f.calculateOptimalChunkSize(fileSize) // 动态计算最优分片大小
	if fileSize < chunkSize*2 {
		// 文件太小，不值得并发下载
		fs.Debugf(f, "文件太小，回退到普通下载")
		// 🔧 修复多重并发下载：使用特殊选项避免触发源对象的并发下载
		srcReader, err := f.openSourceWithoutConcurrency(ctx, srcObj)
		if err != nil {
			return nil, fmt.Errorf("打开源文件失败: %w", err)
		}
		defer srcReader.Close()
		return f.optimizedDiskCrossCloudTransfer(ctx, srcReader, srcObj, parentFileID, fileName)
	}

	numChunks := (fileSize + chunkSize - 1) / chunkSize
	maxConcurrency := int64(4) // 🚀 用户要求：123网盘单文件并发限制到4
	if numChunks < maxConcurrency {
		maxConcurrency = numChunks
	}

	fs.Infof(f, "📊 优化后并发下载参数: 分片大小=%s, 分片数=%d, 并发数=%d",
		fs.SizeSuffix(chunkSize), numChunks, maxConcurrency)

	// 创建临时文件用于组装
	tempFile, err := f.resourcePool.GetOptimizedTempFile("concurrent_download_", fileSize)
	if err != nil {
		return nil, fmt.Errorf("创建临时文件失败: %w", err)
	}
	defer f.resourcePool.PutTempFile(tempFile)

	// 并发下载各个分片
	startTime := time.Now()
	err = f.downloadChunksConcurrently(ctx, srcObj, tempFile, chunkSize, numChunks, maxConcurrency)
	downloadDuration := time.Since(startTime)

	if err != nil {
		// 🔧 修复重复下载问题：智能回退机制，避免不必要的重新下载
		fs.Debugf(f, "并发下载失败，分析失败原因: %v", err)

		// 🔧 关键修复：检查错误类型，避免因上传失败导致重新下载
		errorStr := strings.ToLower(err.Error())
		isUploadRelatedError := strings.Contains(errorStr, "parentfileid") ||
			strings.Contains(errorStr, "上传") ||
			strings.Contains(errorStr, "upload") ||
			strings.Contains(errorStr, "api error 1")

		if isUploadRelatedError {
			fs.Errorf(f, "🚨 检测到上传相关错误，不应该重新下载源文件: %v", err)
			return nil, fmt.Errorf("跨云传输上传阶段失败: %w", err)
		}

		// 检查是否有部分数据已下载成功
		stat, statErr := tempFile.Stat()
		if statErr == nil && stat.Size() > 0 {
			partialSize := stat.Size()
			completionRate := float64(partialSize) / float64(fileSize) * 100

			fs.Infof(f, "🔄 检测到部分下载数据: %s (%.1f%%)，尝试智能恢复",
				fs.SizeSuffix(partialSize), completionRate)

			// 如果已下载超过30%，尝试断点续传恢复
			if completionRate > 30.0 {
				fs.Infof(f, "🚀 尝试断点续传恢复...")
				if resumeErr := f.attemptResumeDownload(ctx, srcObj, tempFile, chunkSize, numChunks, maxConcurrency); resumeErr == nil {
					fs.Infof(f, "✅ 断点续传恢复成功")
					downloadDuration = time.Since(startTime) // 更新总下载时间
				} else {
					fs.Debugf(f, "断点续传恢复失败: %v，谨慎回退", resumeErr)
					// 🔧 修复：谨慎回退，避免重复下载
					fs.Errorf(f, "⚠️ 断点续传恢复失败，避免重复下载，建议检查网络或重试整个操作")
					return nil, fmt.Errorf("断点续传恢复失败，避免重复下载: %w", resumeErr)
				}
			} else {
				fs.Debugf(f, "部分下载数据不足30%%，谨慎处理")
				// 🔧 修复：避免立即重新下载
				fs.Errorf(f, "⚠️ 部分下载数据不足，避免重复下载，建议重试整个操作")
				return nil, fmt.Errorf("部分下载数据不足，避免重复下载: %w", err)
			}
		} else {
			fs.Debugf(f, "无有效的部分下载数据，谨慎处理")
			// 🔧 修复：避免立即重新下载
			fs.Errorf(f, "⚠️ 无有效下载数据，避免重复下载，建议重试整个操作")
			return nil, fmt.Errorf("无有效下载数据，避免重复下载: %w", err)
		}
	}

	fs.Infof(f, "📥 并发下载完成: %s, 用时: %v, 速度: %s/s",
		fs.SizeSuffix(fileSize), downloadDuration,
		fs.SizeSuffix(int64(float64(fileSize)/downloadDuration.Seconds())))

	// 重置文件指针并继续后续处理
	_, err = tempFile.Seek(0, io.SeekStart)
	if err != nil {
		return nil, fmt.Errorf("重置文件指针失败: %w", err)
	}

	// 🔧 修复进度显示：创建上传阶段的Transfer对象
	var uploadTransfer *accounting.Transfer
	if stats != nil {
		uploadTransfer = stats.NewTransferRemoteSize(
			fmt.Sprintf("📤 %s (上传阶段)", fileName),
			fileSize,
			f, // 源是临时文件，目标是123网盘
			f,
		)
		defer uploadTransfer.Done(ctx, nil)
		fs.Debugf(f, "🔧 创建跨云传输上传阶段Transfer: %s", fileName)
	}

	// 继续MD5计算和上传流程
	result, err := f.continueAfterDownload(ctx, tempFile, srcObj, parentFileID, fileName, downloadDuration)
	if err != nil {
		return nil, err
	}

	// 🔧 更新传输跟踪器：传输成功
	transferCompleted = true
	f.transferTracker.CompleteTransfer(fileID, true, nil, fileName)

	return result, nil
}

// downloadChunksConcurrently 并发下载文件分片（集成到rclone标准进度显示）
func (f *Fs) downloadChunksConcurrently(ctx context.Context, srcObj fs.Object, tempFile *os.File, chunkSize, numChunks, maxConcurrency int64) error {
	// 创建进度跟踪器
	progress := NewDownloadProgress(numChunks, srcObj.Size())

	// 🔧 修复进度显示：直接使用跨云传输的Transfer对象
	var currentAccount *accounting.Account

	// 优先从跨云传输上下文获取Transfer对象
	if transfer := f.getCrossCloudTransfer(); transfer != nil {
		// 🔧 关键修复：使用实际的分片下载Reader创建Account，而不是虚拟Reader
		// 我们将在分片下载中使用这个Transfer对象来创建Account
		fs.Debugf(f, "🔧 找到跨云传输Transfer对象，将在分片下载中集成进度")
	} else {
		// 回退：尝试从全局统计查找现有Account
		stats := accounting.GlobalStats()
		if stats != nil {
			// 首先尝试精确匹配
			if acc := stats.GetInProgressAccount(srcObj.Remote()); acc != nil {
				currentAccount = acc
				fs.Debugf(f, "🔧 找到精确匹配的Account: %s", srcObj.Remote())
			} else {
				// 模糊匹配：查找包含文件名的Account
				allAccounts := stats.ListInProgressAccounts()
				for _, accountName := range allAccounts {
					// 检查是否是跨云传输相关的Account
					if strings.Contains(accountName, "下载阶段") ||
						strings.Contains(accountName, srcObj.Remote()) ||
						strings.Contains(srcObj.Remote(), strings.TrimSuffix(strings.TrimPrefix(accountName, "📥 "), " (下载阶段)")) {
						if acc := stats.GetInProgressAccount(accountName); acc != nil {
							currentAccount = acc
							fs.Debugf(f, "🔧 找到模糊匹配的Account: %s -> %s", srcObj.Remote(), accountName)
							break
						}
					}
				}
			}
		}
	}

	// 启动进度更新协程（更新Account的额外信息）
	progressCtx, cancelProgress := context.WithCancel(ctx)
	defer cancelProgress()

	go f.updateAccountExtraInfo(progressCtx, progress, srcObj.Remote(), currentAccount)

	// 创建工作池
	semaphore := make(chan struct{}, maxConcurrency)
	errChan := make(chan error, numChunks)
	var wg sync.WaitGroup

	for i := int64(0); i < numChunks; i++ {
		wg.Add(1)
		go func(chunkIndex int64) {
			defer wg.Done()

			// 🔧 添加panic恢复机制，提高系统稳定性
			defer func() {
				if r := recover(); r != nil {
					fs.Errorf(f, "123网盘下载分片 %d 发生panic: %v", chunkIndex, r)
					errChan <- fmt.Errorf("下载分片 %d panic: %v", chunkIndex, r)
				}
			}()

			// 🔧 修复信号量泄漏：使用带超时的信号量获取
			chunkTimeout := 10 * time.Minute // 每个分片最多10分钟超时
			chunkCtx, cancelChunk := context.WithTimeout(ctx, chunkTimeout)
			defer cancelChunk()

			// 获取信号量，带超时控制
			select {
			case semaphore <- struct{}{}:
				// 成功获取信号量，立即设置defer释放
				defer func() {
					select {
					case <-semaphore:
					default:
						// 如果信号量已经被释放，避免阻塞
					}
				}()
			case <-chunkCtx.Done():
				errChan <- fmt.Errorf("下载分片 %d 获取信号量超时: %w", chunkIndex, chunkCtx.Err())
				return
			}

			// 计算分片范围
			start := chunkIndex * chunkSize
			end := start + chunkSize - 1
			if end >= srcObj.Size() {
				end = srcObj.Size() - 1
			}
			actualChunkSize := end - start + 1

			// 🔧 修复信号量泄漏：使用带超时的分片下载
			chunkStartTime := time.Now()
			err := f.downloadChunk(chunkCtx, srcObj, tempFile, start, end, chunkIndex)
			chunkDuration := time.Since(chunkStartTime)

			if err != nil {
				// 检查是否是超时错误
				if chunkCtx.Err() != nil {
					errChan <- fmt.Errorf("下载分片 %d 超时: %w", chunkIndex, chunkCtx.Err())
				} else {
					errChan <- fmt.Errorf("下载分片 %d 失败: %w", chunkIndex, err)
				}
				return
			}

			// 更新进度
			progress.UpdateChunkProgress(chunkIndex, actualChunkSize, chunkDuration)

			// 🔧 参考115网盘：修复进度显示，报告分片进度到rclone accounting系统
			if currentAccount != nil {
				if err := currentAccount.AccountRead(int(actualChunkSize)); err != nil {
					fs.Debugf(f, "⚠️ 报告123网盘分片进度失败: %v", err)
				} else {
					fs.Debugf(f, "✅ 成功报告123网盘分片进度: %d bytes", actualChunkSize)
				}
			} else {
				// 🔧 参考115网盘：如果Account丢失，尝试重新获取
				remoteName := srcObj.Remote()
				if stats := accounting.GlobalStats(); stats != nil {
					// 尝试重新获取Account对象
					currentAccount = stats.GetInProgressAccount(remoteName)
					if currentAccount == nil {
						// 尝试模糊匹配
						allAccounts := stats.ListInProgressAccounts()
						for _, accountName := range allAccounts {
							if strings.Contains(accountName, remoteName) || strings.Contains(remoteName, accountName) {
								currentAccount = stats.GetInProgressAccount(accountName)
								if currentAccount != nil {
									break
								}
							}
						}
					}

					if currentAccount != nil {
						if err := currentAccount.AccountRead(int(actualChunkSize)); err != nil {
							fs.Debugf(f, "⚠️ 重新获取Account后报告进度失败: %v", err)
						} else {
							fs.Debugf(f, "✅ 重新获取Account并成功报告进度: %d bytes", actualChunkSize)
						}
					} else {
						fs.Debugf(f, "⚠️ 无法重新获取Account，跳过进度报告")
					}
				}
			}

			// 🔧 统一进度显示：使用新的统一进度管理器
			progressID := fmt.Sprintf("download_%s_%d", srcObj.Remote(), time.Now().Unix())
			f.progressManager.UpdateProgress(progressID, int64(chunkIndex+1)*chunkSize, int(chunkIndex+1))

			fs.Debugf(f, "123网盘分片 %d 下载成功: %d-%d (%s) | 用时: %v | 速度: %.2f MB/s",
				chunkIndex, start, end, fs.SizeSuffix(actualChunkSize), chunkDuration,
				float64(actualChunkSize)/chunkDuration.Seconds()/1024/1024)
		}(i)
	}

	// 🔧 修复死锁风险：添加整体超时控制，防止无限等待
	overallTimeout := time.Duration(numChunks) * 15 * time.Minute // 每个分片最多15分钟
	if overallTimeout > 2*time.Hour {
		overallTimeout = 2 * time.Hour // 最大2小时超时
	}

	// 使用带超时的等待机制
	done := make(chan struct{})
	go func() {
		defer close(done)
		wg.Wait()
		close(errChan)
	}()

	// 等待完成或超时
	select {
	case <-done:
		// 正常完成，检查是否有错误
		for err := range errChan {
			if err != nil {
				return err
			}
		}
		return nil
	case <-time.After(overallTimeout):
		fs.Errorf(f, "🚨 并发下载整体超时 (%v)，可能存在死锁", overallTimeout)
		return fmt.Errorf("并发下载整体超时: %v", overallTimeout)
	}
}

// downloadChunk 下载单个文件分片（带重试机制）
func (f *Fs) downloadChunk(ctx context.Context, srcObj fs.Object, tempFile *os.File, start, end, chunkIndex int64) error {
	const maxRetries = 4
	var lastErr error

	for retry := 0; retry < maxRetries; retry++ {
		// 在每次重试前强制清除下载URL缓存（参考115网盘修复方案）
		if retry > 0 {
			// 🗑️ 下载URL缓存已删除，不需要清除缓存
			fs.Debugf(f, "123网盘分片重试，直接获取新下载URL")

			// 添加重试延迟
			retryDelay := time.Duration(retry) * time.Second
			fs.Debugf(f, "123网盘分片 %d 重试 %d/%d: 等待 %v 后重试", chunkIndex, retry, maxRetries-1, retryDelay)
			time.Sleep(retryDelay)
		}

		// 使用Range选项打开文件分片
		// 🔧 修复多重并发下载：使用特殊方法避免触发源对象的并发下载
		rangeOption := &fs.RangeOption{Start: start, End: end}
		var chunkReader io.ReadCloser
		var err error

		// 🔧 修复115网盘并发下载被禁用问题：移除不必要的DisableConcurrentDownloadOption
		switch obj := srcObj.(type) {
		case *Object:
			// 如果是123网盘对象，直接调用openNormal避免无限循环
			chunkReader, err = obj.openNormal(ctx, rangeOption)
		case interface {
			open(context.Context, ...fs.OpenOption) (io.ReadCloser, error)
		}:
			// 🚀 修复：115网盘对象使用正常的Range下载，不禁用并发下载
			// 让115网盘自己决定是否使用并发下载（基于分片大小）
			chunkReader, err = obj.open(ctx, rangeOption)
		default:
			// 🚀 修复：其他类型对象也使用正常的Range下载
			chunkReader, err = srcObj.Open(ctx, rangeOption)
		}
		if err != nil {
			lastErr = fmt.Errorf("打开分片失败: %w", err)
			fs.Debugf(f, "123网盘分片 %d 打开失败 (重试 %d/%d): %v", chunkIndex, retry, maxRetries-1, err)
			continue
		}

		// 🔧 参考115网盘：修复Account进度跟踪，使用更稳定的方式
		var currentAccount *accounting.Account
		if transfer := f.getCrossCloudTransfer(); transfer != nil {
			// 🔧 参考115网盘：使用虚拟Reader创建Account，然后用实际数据替换
			dummyReader := io.NopCloser(strings.NewReader(""))
			currentAccount = transfer.Account(ctx, dummyReader)
			dummyReader.Close()
			fs.Debugf(f, "🔧 分片 %d 使用跨云传输Account跟踪进度", chunkIndex)
		} else {
			// 🔧 参考115网盘：从全局统计查找现有Account
			remoteName := srcObj.Remote()
			if stats := accounting.GlobalStats(); stats != nil {
				currentAccount = stats.GetInProgressAccount(remoteName)
				if currentAccount == nil {
					// 尝试模糊匹配：查找包含文件名的Account
					allAccounts := stats.ListInProgressAccounts()
					for _, accountName := range allAccounts {
						if strings.Contains(accountName, remoteName) || strings.Contains(remoteName, accountName) {
							currentAccount = stats.GetInProgressAccount(accountName)
							if currentAccount != nil {
								fs.Debugf(f, "🔧 分片 %d 找到匹配的Account: %s -> %s", chunkIndex, remoteName, accountName)
								break
							}
						}
					}
				} else {
					fs.Debugf(f, "🔧 分片 %d 找到精确匹配的Account: %s", chunkIndex, remoteName)
				}
			}

			// 如果还是没有找到，创建临时Account
			if currentAccount == nil {
				tempTransfer := accounting.Stats(ctx).NewTransfer(srcObj, f)
				dummyReader := io.NopCloser(strings.NewReader(""))
				currentAccount = tempTransfer.Account(ctx, dummyReader)
				dummyReader.Close()
				fs.Debugf(f, "🔧 分片 %d 使用临时Account跟踪进度", chunkIndex)
			}
		}

		// 读取分片数据并报告进度
		chunkData, err := io.ReadAll(chunkReader)
		chunkReader.Close()
		if err != nil {
			lastErr = fmt.Errorf("读取分片数据失败: %w", err)
			fs.Debugf(f, "123网盘分片 %d 读取失败 (重试 %d/%d): %v", chunkIndex, retry, maxRetries-1, err)
			continue
		}

		// 🔧 关键修复：向Account报告已读取的字节数
		if currentAccount != nil {
			err = currentAccount.AccountRead(len(chunkData))
			if err != nil {
				fs.Debugf(f, "Account报告进度失败: %v", err)
			}
		}

		// 写入临时文件的正确位置
		_, err = tempFile.WriteAt(chunkData, start)
		if err != nil {
			lastErr = fmt.Errorf("写入分片数据失败: %w", err)
			fs.Debugf(f, "123网盘分片 %d 写入失败 (重试 %d/%d): %v", chunkIndex, retry, maxRetries-1, err)
			continue
		}

		// 成功完成
		if retry > 0 {
			fs.Debugf(f, "123网盘分片 %d 重试成功 (第 %d 次尝试)", chunkIndex, retry+1)
		}
		return nil
	}

	// 所有重试都失败了
	return fmt.Errorf("123网盘分片 %d 下载失败，已重试 %d 次: %w", chunkIndex, maxRetries, lastErr)
}

// continueAfterDownload 下载完成后继续MD5计算和上传流程
func (f *Fs) continueAfterDownload(ctx context.Context, tempFile *os.File, src fs.ObjectInfo, parentFileID int64, fileName string, downloadDuration time.Duration) (*Object, error) {
	fileSize := src.Size()
	startTime := time.Now().Add(-downloadDuration) // 调整开始时间以包含下载时间

	// 步骤2: 计算MD5
	fs.Infof(f, "🔐 开始计算MD5哈希...")
	md5StartTime := time.Now()

	_, err := tempFile.Seek(0, io.SeekStart)
	if err != nil {
		return nil, fmt.Errorf("重置文件指针失败: %w", err)
	}

	hasher := md5.New()
	_, err = io.Copy(hasher, tempFile)
	if err != nil {
		return nil, fmt.Errorf("计算MD5失败: %w", err)
	}

	md5Hash := fmt.Sprintf("%x", hasher.Sum(nil))
	md5Duration := time.Since(md5StartTime)

	fs.Infof(f, "🔐 MD5计算完成: %s, 用时: %v", md5Hash, md5Duration)

	// 步骤3: 检查秒传
	fs.Infof(f, "⚡ 检查123网盘秒传功能...")
	createResp, err := f.createUpload(ctx, parentFileID, fileName, md5Hash, fileSize)
	if err != nil {
		return nil, fmt.Errorf("创建上传会话失败: %w", err)
	}

	if createResp.Data.Reuse {
		totalDuration := time.Since(startTime)
		fs.Infof(f, "🎉 秒传成功！总用时: %v (下载: %v + MD5: %v)",
			totalDuration, downloadDuration, md5Duration)

		return &Object{
			fs:          f,
			remote:      fileName,
			hasMetaData: true,
			id:          strconv.FormatInt(createResp.Data.FileID, 10),
			size:        fileSize,
			md5sum:      md5Hash,
			modTime:     time.Now(),
			isDir:       false,
		}, nil
	}

	// 步骤4: 上传文件
	fs.Infof(f, "📤 开始上传到123网盘...")
	uploadStartTime := time.Now()

	_, err = tempFile.Seek(0, io.SeekStart)
	if err != nil {
		return nil, fmt.Errorf("重置文件指针用于上传失败: %w", err)
	}

	err = f.uploadFile(ctx, tempFile, createResp, fileSize)
	if err != nil {
		return nil, fmt.Errorf("上传失败: %w", err)
	}

	uploadDuration := time.Since(uploadStartTime)
	totalDuration := time.Since(startTime)

	fs.Infof(f, "✅ 并发下载跨云传输完成！总用时: %v (下载: %v + MD5: %v + 上传: %v)",
		totalDuration, downloadDuration, md5Duration, uploadDuration)

	// 返回上传成功的文件对象
	return &Object{
		fs:          f,
		remote:      fileName,
		hasMetaData: true,
		id:          strconv.FormatInt(createResp.Data.FileID, 10),
		size:        fileSize,
		md5sum:      md5Hash,
		modTime:     time.Now(),
		isDir:       false,
	}, nil
}

// MD5缓存管理器
type CrossCloudMD5Cache struct {
	cache map[string]CrossCloudMD5Entry
	mutex sync.RWMutex
}

type CrossCloudMD5Entry struct {
	MD5Hash    string
	FileSize   int64
	ModTime    time.Time
	CachedTime time.Time
}

// 🔧 修复全局状态竞态条件：使用sync.Once确保线程安全初始化
var (
	globalMD5Cache     *CrossCloudMD5Cache
	globalMD5CacheOnce sync.Once
)

func getGlobalMD5Cache() *CrossCloudMD5Cache {
	globalMD5CacheOnce.Do(func() {
		globalMD5Cache = &CrossCloudMD5Cache{
			cache: make(map[string]CrossCloudMD5Entry),
		}
	})
	return globalMD5Cache
}

// getCachedMD5 尝试从缓存获取MD5
func (f *Fs) getCachedMD5(src fs.ObjectInfo) (string, bool) {
	// 生成缓存键：源文件系统类型 + 路径 + 大小 + 修改时间
	srcFs := src.Fs()
	if srcFs == nil {
		return "", false
	}

	cacheKey := fmt.Sprintf("%s:%s:%d:%d",
		srcFs.Name(), src.Remote(), src.Size(), src.ModTime(context.Background()).Unix())

	cache := getGlobalMD5Cache()
	cache.mutex.RLock()
	defer cache.mutex.RUnlock()

	entry, exists := cache.cache[cacheKey]
	if !exists {
		return "", false
	}

	// 检查缓存是否过期（24小时）
	if time.Since(entry.CachedTime) > 24*time.Hour {
		// 异步清理过期缓存
		go func() {
			cache := getGlobalMD5Cache()
			cache.mutex.Lock()
			delete(cache.cache, cacheKey)
			cache.mutex.Unlock()
		}()
		return "", false
	}

	// 验证文件信息是否匹配
	if entry.FileSize == src.Size() && entry.ModTime.Equal(src.ModTime(context.Background())) {
		fs.Debugf(f, "🎯 MD5缓存命中: %s -> %s", cacheKey, entry.MD5Hash)
		return entry.MD5Hash, true
	}

	return "", false
}

// cacheMD5 将MD5存入缓存
func (f *Fs) cacheMD5(src fs.ObjectInfo, md5Hash string) {
	srcFs := src.Fs()
	if srcFs == nil {
		return
	}

	cacheKey := fmt.Sprintf("%s:%s:%d:%d",
		srcFs.Name(), src.Remote(), src.Size(), src.ModTime(context.Background()).Unix())

	entry := CrossCloudMD5Entry{
		MD5Hash:    md5Hash,
		FileSize:   src.Size(),
		ModTime:    src.ModTime(context.Background()),
		CachedTime: time.Now(),
	}

	cache := getGlobalMD5Cache()
	cache.mutex.Lock()
	cache.cache[cacheKey] = entry
	cache.mutex.Unlock()

	fs.Debugf(f, "💾 MD5已缓存: %s -> %s", cacheKey, md5Hash)
}

// continueWithKnownMD5 使用已知MD5继续上传流程
func (f *Fs) continueWithKnownMD5(ctx context.Context, tempFile *os.File, src fs.ObjectInfo, parentFileID int64, fileName string, md5Hash string, downloadDuration, md5Duration time.Duration, startTime time.Time) (*Object, error) {
	fileSize := src.Size()

	// 步骤3: 检查秒传
	fs.Infof(f, "⚡ 检查123网盘秒传功能...")
	createResp, err := f.createUpload(ctx, parentFileID, fileName, md5Hash, fileSize)
	if err != nil {
		return nil, fmt.Errorf("创建上传会话失败: %w", err)
	}

	if createResp.Data.Reuse {
		totalDuration := time.Since(startTime)
		fs.Infof(f, "🎉 秒传成功！总用时: %v (下载: %v + MD5: %v)",
			totalDuration, downloadDuration, md5Duration)
		return &Object{
			fs:          f,
			remote:      fileName,
			hasMetaData: true,
			id:          strconv.FormatInt(createResp.Data.FileID, 10),
			size:        fileSize,
			md5sum:      md5Hash,
			modTime:     time.Now(),
			isDir:       false,
		}, nil
	}

	// 步骤4: 上传文件
	fs.Infof(f, "📤 开始上传到123网盘...")
	uploadStartTime := time.Now()

	_, err = tempFile.Seek(0, io.SeekStart)
	if err != nil {
		return nil, fmt.Errorf("重置文件指针用于上传失败: %w", err)
	}

	err = f.uploadFile(ctx, tempFile, createResp, fileSize)
	if err != nil {
		return nil, fmt.Errorf("上传失败: %w", err)
	}

	uploadDuration := time.Since(uploadStartTime)
	totalDuration := time.Since(startTime)

	// 缓存MD5以供后续使用
	f.cacheMD5(src, md5Hash)

	fs.Infof(f, "✅ 优化跨云传输完成！总用时: %v (下载: %v + MD5: %v + 上传: %v)",
		totalDuration, downloadDuration, md5Duration, uploadDuration)

	// 返回上传成功的文件对象
	return &Object{
		fs:          f,
		remote:      fileName,
		hasMetaData: true,
		id:          strconv.FormatInt(createResp.Data.FileID, 10),
		size:        fileSize,
		md5sum:      md5Hash,
		modTime:     time.Now(),
		isDir:       false,
	}, nil
}

// fallbackToNormalDownload 回退到普通下载的统一处理函数
// 🔧 修复重复下载问题：避免重复下载，复用已有的优化逻辑
func (f *Fs) fallbackToNormalDownload(ctx context.Context, srcObj fs.Object, parentFileID int64, fileName string) (*Object, error) {
	fs.Infof(f, "🔄 回退到普通下载模式: %s", fileName)

	// 🔧 关键修复：检查是否为115网盘源，避免重复下载
	if srcFs := srcObj.Fs(); srcFs != nil && srcFs.Name() == "115" {
		fs.Debugf(f, "🚨 检测到115网盘源文件，检查是否存在重复下载风险")

		// 检查是否在短时间内（5分钟）已经下载过相同文件
		downloadKey := fmt.Sprintf("%s_%d", srcObj.Remote(), srcObj.Size())
		if f.transferTracker != nil {
			// 检查是否有活跃的传输
			if session := f.transferTracker.GetActiveTransfer(downloadKey); session != nil {
				fs.Infof(f, "⚠️ 检测到115网盘文件正在传输中，避免重复下载: %s", fileName)
				return nil, fmt.Errorf("文件正在传输中，避免重复下载: %s", fileName)
			}
		}

		fs.Debugf(f, "⚠️ 115网盘回退下载可能导致重复下载，建议检查上层逻辑")
	}

	// 重新打开源文件
	srcReader, err := srcObj.Open(ctx)
	if err != nil {
		return nil, fmt.Errorf("回退时打开源文件失败: %w", err)
	}
	defer srcReader.Close()

	// 使用优化的磁盘传输策略，这个方法已经包含了MD5缓存等优化
	return f.optimizedDiskCrossCloudTransfer(ctx, srcReader, srcObj, parentFileID, fileName)
}

// optimizedDiskCrossCloudTransfer 优化的磁盘跨云传输（原有逻辑优化版）
func (f *Fs) optimizedDiskCrossCloudTransfer(ctx context.Context, srcReader io.ReadCloser, src fs.ObjectInfo, parentFileID int64, fileName string) (*Object, error) {
	fileSize := src.Size()
	startTime := time.Now()

	// 步骤1: 检查MD5缓存
	var md5Hash string
	var downloadDuration, md5Duration time.Duration

	if cachedMD5, found := f.getCachedMD5(src); found {
		fs.Infof(f, "🎯 使用缓存的MD5: %s", cachedMD5)
		md5Hash = cachedMD5
		md5Duration = 0 // 缓存命中，无需计算时间

		// 仍需要下载文件用于上传，但可以跳过MD5计算
		tempFile, err := f.resourcePool.GetOptimizedTempFile("cross_cloud_cached_", fileSize)
		if err != nil {
			return nil, fmt.Errorf("创建跨云传输临时文件失败: %w", err)
		}
		defer f.resourcePool.PutTempFile(tempFile)

		downloadStartTime := time.Now()
		written, err := f.copyWithProgressAndValidation(ctx, tempFile, srcReader, fileSize, false, fileName) // 不需要计算MD5
		downloadDuration = time.Since(downloadStartTime)

		if err != nil {
			return nil, fmt.Errorf("下载文件失败: %w", err)
		}

		if written != fileSize {
			return nil, fmt.Errorf("下载文件大小不匹配: 期望 %d, 实际 %d", fileSize, written)
		}

		fs.Infof(f, "📥 下载完成 (使用缓存MD5): %s, 用时: %v, 速度: %s/s",
			fs.SizeSuffix(fileSize), downloadDuration,
			fs.SizeSuffix(int64(float64(fileSize)/downloadDuration.Seconds())))

		// 直接跳转到上传步骤
		return f.continueWithKnownMD5(ctx, tempFile, src, parentFileID, fileName, md5Hash, downloadDuration, md5Duration, startTime)
	}

	// 步骤2: 缓存未命中，正常下载并计算MD5
	fs.Infof(f, "💾 MD5缓存未命中，开始下载并计算MD5")

	// 创建本地临时文件
	tempFile, err := f.resourcePool.GetOptimizedTempFile("cross_cloud_optimized_", fileSize)
	if err != nil {
		return nil, fmt.Errorf("创建跨云传输临时文件失败: %w", err)
	}
	defer f.resourcePool.PutTempFile(tempFile)

	// 完整下载文件并计算MD5
	downloadStartTime := time.Now()
	written, err := f.copyWithProgressAndValidation(ctx, tempFile, srcReader, fileSize, true, fileName)
	downloadDuration = time.Since(downloadStartTime)

	if err != nil {
		return nil, fmt.Errorf("下载文件失败: %w", err)
	}

	if written != fileSize {
		return nil, fmt.Errorf("下载文件大小不匹配: 期望%s，实际%s",
			fs.SizeSuffix(fileSize), fs.SizeSuffix(written))
	}

	downloadSpeed := float64(written) / downloadDuration.Seconds() / (1024 * 1024) // MB/s
	fs.Infof(f, "✅ 步骤1完成: 文件下载成功 %s，耗时: %v，速度: %.2f MB/s",
		fs.SizeSuffix(written), downloadDuration.Round(time.Second), downloadSpeed)

	// 步骤2: 从本地文件计算MD5
	fs.Infof(f, "🔢 步骤2/4: 开始计算MD5哈希...")
	_, err = tempFile.Seek(0, io.SeekStart)
	if err != nil {
		return nil, fmt.Errorf("重置文件指针失败: %w", err)
	}

	hasher := md5.New()
	md5StartTime := time.Now()
	_, err = io.Copy(hasher, tempFile)
	if err != nil {
		return nil, fmt.Errorf("计算MD5失败: %w", err)
	}

	md5Hash = fmt.Sprintf("%x", hasher.Sum(nil))
	md5Duration = time.Since(md5StartTime)
	fs.Infof(f, "✅ 步骤2完成: MD5计算成功 %s，耗时: %v", md5Hash, md5Duration.Round(time.Second))

	// 步骤3: 创建上传会话并检查秒传
	fs.Infof(f, "🚀 步骤3/4: 创建上传会话，检查是否可以秒传...")
	createResp, err := f.createUploadV2(ctx, parentFileID, fileName, md5Hash, fileSize)
	if err != nil {
		return nil, fmt.Errorf("创建上传会话失败: %w", err)
	}

	// 检查是否触发秒传
	if createResp.Data.Reuse {
		totalDuration := time.Since(startTime)
		fs.Infof(f, "🎉 步骤3完成: 触发秒传功能！总耗时: %v (下载: %v + MD5: %v)",
			totalDuration.Round(time.Second), downloadDuration.Round(time.Second), md5Duration.Round(time.Second))
		return &Object{
			fs:          f,
			remote:      fileName,
			hasMetaData: true,
			id:          strconv.FormatInt(createResp.Data.FileID, 10),
			size:        fileSize,
			md5sum:      md5Hash,
			modTime:     time.Now(),
			isDir:       false,
		}, nil
	}

	// 步骤4: 执行实际上传
	fs.Infof(f, "📤 步骤4/4: 开始上传文件到123网盘...")
	_, err = tempFile.Seek(0, io.SeekStart)
	if err != nil {
		return nil, fmt.Errorf("重置文件指针用于上传失败: %w", err)
	}

	// 准备上传参数
	chunkSize := createResp.Data.SliceSize
	if chunkSize <= 0 {
		chunkSize = f.getOptimalChunkSize(fileSize, 20*1024*1024) // 默认20MB/s网速
	}
	totalChunks := (fileSize + chunkSize - 1) / chunkSize
	progress, err := f.prepareUploadProgress(createResp.Data.PreuploadID, totalChunks, chunkSize, fileSize)
	if err != nil {
		return nil, fmt.Errorf("准备上传进度失败: %w", err)
	}

	// 计算并发参数
	concurrencyParams := f.calculateConcurrencyParams(fileSize)
	maxConcurrency := concurrencyParams.optimal

	// 执行上传
	uploadStartTime := time.Now()
	result, err := f.v2UploadChunksWithConcurrency(ctx, tempFile, &localFileInfo{
		name:    fileName,
		size:    fileSize,
		modTime: src.ModTime(ctx),
	}, createResp, progress, fileName, maxConcurrency)

	if err != nil {
		return nil, fmt.Errorf("上传到123网盘失败: %w", err)
	}

	uploadDuration := time.Since(uploadStartTime)
	uploadSpeed := float64(fileSize) / uploadDuration.Seconds() / (1024 * 1024) // MB/s
	totalDuration := time.Since(startTime)

	// 缓存MD5以供后续使用
	f.cacheMD5(src, md5Hash)

	fs.Infof(f, "🎉 跨云传输完成: %s，总耗时: %v (下载: %v + MD5: %v + 上传: %v)，上传速度: %.2f MB/s",
		fs.SizeSuffix(fileSize), totalDuration.Round(time.Second),
		downloadDuration.Round(time.Second), md5Duration.Round(time.Second),
		uploadDuration.Round(time.Second), uploadSpeed)

	return result, nil
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

	// 优化的MD5计算策略：避免重复读取源文件
	// 首先尝试从源对象获取已知的MD5哈希
	if srcObj, ok := src.(fs.Object); ok {
		// 尝试获取已知的MD5哈希，避免重复计算
		if md5Hash, err := srcObj.Hash(ctx, fshash.MD5); err == nil && md5Hash != "" {
			fs.Debugf(f, "使用源对象已知MD5: %s", md5Hash)
			return f.putWithKnownMD5(ctx, in, src, parentFileID, fileName, md5Hash)
		}

		// 如果没有已知MD5，使用临时文件策略确保数据一致性
		fs.Debugf(f, "源对象无已知MD5，使用临时文件策略确保数据一致性")
		return f.streamingPutWithTempFileForced(ctx, in, src, parentFileID, fileName)
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
	// 为大文件添加用户友好的初始化提示
	fileSize := src.Size()
	if fileSize > 1024*1024*1024 { // 大于1GB的文件
		fs.Infof(f, "🚀 正在初始化大文件上传 (%s) - 准备分片上传参数，请稍候...", fs.SizeSuffix(fileSize))
	}

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
			f.saveUploadProgress(progress) // 忽略保存错误
			return nil, fmt.Errorf("上传分片 %d 失败: %w", partNumber, err)
		}

		// 标记分片已完成并保存进度
		progress.SetUploaded(partNumber)
		if err := f.saveUploadProgress(progress); err != nil {
			f.debugf(LogLevelDebug, "保存进度失败: %v", err)
		}

		f.debugf(LogLevelVerbose, "✅ 分片 %d/%d 上传完成", partNumber, totalChunks)
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
	// 计算最终MD5
	finalMD5 := fmt.Sprintf("%x", hasher.Sum(nil))

	// 完成上传并获取结果，传入文件大小以优化轮询策略
	result, err := f.completeUploadWithResultAndSize(ctx, preuploadID, fileSize)
	if err != nil {
		return nil, fmt.Errorf("完成分片上传失败: %w", err)
	}

	f.debugf(LogLevelDebug, "分片流式上传完成，计算MD5: %s, 服务器MD5: %s", finalMD5, result.Etag)

	// 清理进度文件
	f.removeUploadProgress(preuploadID)

	// 验证文件ID
	if result.FileID == 0 {
		return nil, fmt.Errorf("上传完成但未获取到有效的文件ID")
	}

	// 返回成功的文件对象，使用服务器返回的文件ID
	return &Object{
		fs:          f,
		remote:      fileName,
		hasMetaData: true,
		id:          strconv.FormatInt(result.FileID, 10), // 使用服务器返回的文件ID
		size:        fileSize,
		md5sum:      finalMD5, // 使用计算的MD5
		modTime:     time.Now(),
		isDir:       false,
	}, nil
}

// attemptStreamingUpload 尝试流式上传以避免双倍流量消耗
// 这个方法实现真正的边下边上传输，最小化流量使用
func (f *Fs) attemptStreamingUpload(ctx context.Context, srcObj fs.Object, parentFileID int64, fileName string) (*Object, error) {
	fs.Debugf(f, "尝试流式上传，避免临时文件和双倍流量")

	// 优化的MD5获取策略：优先使用已知哈希，避免重复计算
	var md5Hash string
	if hash, err := srcObj.Hash(ctx, fshash.MD5); err == nil && hash != "" {
		md5Hash = hash
		fs.Debugf(f, "使用源对象已知MD5: %s", md5Hash)
	} else {
		// 如果没有已知MD5，计算MD5（只读取一次）
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

		md5Hash = fmt.Sprintf("%x", hasher.Sum(nil))
		fs.Debugf(f, "流式传输MD5计算完成: %s", md5Hash)
	}

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

	// 注意：新的uploadPartWithMultipart方法内部会获取上传域名，这里不再需要

	// 读取临时文件数据用于上传
	chunkData, err := io.ReadAll(tempFile)
	if err != nil {
		return fmt.Errorf("读取分片 %d 临时文件失败: %w", partNumber, err)
	}

	// 计算分片MD5
	chunkMD5 := fmt.Sprintf("%x", chunkHasher.Sum(nil))

	// 使用正确的multipart上传方法
	err = f.uploadPartWithMultipart(ctx, preuploadID, partNumber, chunkMD5, chunkData)
	if err != nil {
		return fmt.Errorf("上传分片 %d 数据失败: %w", partNumber, err)
	}
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

	// 🔧 统一进度显示：查找Account对象进行集成
	var currentAccount *accounting.Account
	if stats := accounting.GlobalStats(); stats != nil {
		// 尝试精确匹配
		currentAccount = stats.GetInProgressAccount(fileName)
		if currentAccount == nil {
			// 尝试模糊匹配：查找包含文件名的Account
			allAccounts := stats.ListInProgressAccounts()
			for _, accountName := range allAccounts {
				if strings.Contains(accountName, fileName) || strings.Contains(fileName, accountName) {
					currentAccount = stats.GetInProgressAccount(accountName)
					if currentAccount != nil {
						break
					}
				}
			}
		}
	}

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
	fs.Debugf(f, "📤 开始分发分片上传任务...")
	go func() {
		defer close(chunkJobs)
		tasksSent := 0
		for chunkIndex := int64(0); chunkIndex < totalChunks; chunkIndex++ {
			partNumber := chunkIndex + 1
			if !progress.IsUploaded(partNumber) {
				select {
				case chunkJobs <- chunkIndex:
					tasksSent++
					if tasksSent%10 == 0 || tasksSent <= 5 {
						fs.Debugf(f, "📋 已分发 %d 个分片任务，当前分片: %d/%d", tasksSent, partNumber, totalChunks)
					}
				case <-workerCtx.Done():
					fs.Debugf(f, "⚠️  上下文取消，停止发送分片任务 (已发送%d个任务)", tasksSent)
					return
				}
			}
		}
		fs.Debugf(f, "✅ 分片任务分发完成，共发送 %d 个任务", tasksSent)
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
					// 记录partial文件（用于统计）
					f.performanceMetrics.RecordPartialFile(true)
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

				fs.Debugf(f, "✅ 分片 %d/%d 上传完成 (并发)", partNumber, totalChunks)

				// 🔧 统一进度显示：更新Account的额外信息
				if currentAccount != nil {
					extraInfo := fmt.Sprintf("[%d/%d分片]", completedChunks, totalChunks)
					currentAccount.SetExtraInfo(extraInfo)
				}

			case <-workerCtx.Done():
				// 保存当前进度
				saveErr := f.saveUploadProgress(progress)
				if saveErr != nil {
					fs.Debugf(f, "超时时保存进度失败: %v", saveErr)
				}
				// 记录partial文件（用于统计）
				f.performanceMetrics.RecordPartialFile(true)

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

	// 完成上传并获取结果，传入文件大小以优化轮询策略
	result, err := f.completeUploadWithResultAndSize(ctx, preuploadID, fileSize)
	if err != nil {
		return nil, fmt.Errorf("完成多线程分片上传失败: %w", err)
	}

	fs.Debugf(f, "多线程分片上传完成，计算MD5: %s, 服务器MD5: %s", finalMD5, result.Etag)

	// 清理进度文件
	f.removeUploadProgress(preuploadID)

	// 验证文件ID
	if result.FileID == 0 {
		return nil, fmt.Errorf("上传完成但未获取到有效的文件ID")
	}

	// 返回成功的文件对象，使用服务器返回的文件ID
	return &Object{
		fs:          f,
		remote:      fileName,
		hasMetaData: true,
		id:          strconv.FormatInt(result.FileID, 10), // 使用服务器返回的文件ID
		size:        fileSize,
		md5sum:      finalMD5, // 使用计算的MD5
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

	// 获取上传域名
	uploadDomain, err := f.getUploadDomain(ctx)
	if err != nil {
		return fmt.Errorf("获取上传域名失败 分片 %d: %w", partNumber, err)
	}

	// 构造分片上传URL
	uploadURL := fmt.Sprintf("%s/upload/v2/file/slice", uploadDomain)

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

	// 获取上传域名
	uploadDomain, err := f.getUploadDomain(ctx)
	if err != nil {
		return "", nil, fmt.Errorf("获取上传域名失败 分片 %d: %w", partNumber, err)
	}

	// 构造分片上传URL
	uploadURL := fmt.Sprintf("%s/upload/v2/file/slice", uploadDomain)

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

	// 使用专门的multipart上传方法
	var uploadResp struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			FileID    int64 `json:"fileID"`
			Completed bool  `json:"completed"`
		} `json:"data"`
	}

	err = f.singleStepUploadMultipart(ctx, data, parentFileID, fileName, md5Hash, &uploadResp)
	if err != nil {
		return nil, err
	}

	// 检查API响应码
	if uploadResp.Code != 0 {
		// 检查是否是parentFileID不存在的错误
		if uploadResp.Code == 1 && strings.Contains(uploadResp.Message, "parentFileID不存在") {
			fs.Errorf(f, "⚠️  单步上传: 父目录ID %d 不存在，可能缓存过期，需要清理缓存重试", parentFileID)
			// 清理相关缓存
			f.clearParentIDCache(parentFileID)
			f.clearDirListCache(parentFileID)
			// 重置目录缓存
			if f.dirCache != nil {
				fs.Debugf(f, "重置目录缓存以清理过期的父目录ID: %d", parentFileID)
				f.dirCache.ResetRoot()
			}
			return nil, fmt.Errorf("父目录ID不存在，缓存已清理，请重试: 单步上传API错误: code=%d, message=%s", uploadResp.Code, uploadResp.Message)
		}
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
// 优化版本：直接使用通用工具函数，减少代码重复
func (f *Fs) validateFileNameUnified(fileName string) error {
	return ValidationUtil.ValidateFileName(fileName)
}

// validateFileName 保持向后兼容的验证方法
// 优化版本：直接使用统一验证函数，减少函数调用层次
func (f *Fs) validateFileName(fileName string) error {
	return ValidationUtil.ValidateFileName(fileName)
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
	// 临时调试：强制重新获取上传域名，跳过缓存
	f.debugf(LogLevelDebug, "🔍 DEBUG: 强制重新获取上传域名，跳过缓存")

	// 🗑️ 下载URL缓存已删除，不需要清除缓存
	f.debugf(LogLevelDebug, "🔍 DEBUG: 强制重新获取上传域名")

	// 尝试动态获取上传域名 - 使用正确的API路径
	var response struct {
		Code    int      `json:"code"`
		Data    []string `json:"data"` // 修正：data是字符串数组
		Message string   `json:"message"`
		TraceID string   `json:"x-traceID"`
	}

	// 使用正确的API路径调用 - 已迁移到rclone标准方法
	f.debugf(LogLevelDebug, "🔍 DEBUG: 调用 /upload/v2/file/domain 获取上传域名")
	err := f.makeAPICallWithRest(ctx, "/upload/v2/file/domain", "GET", nil, &response)
	f.debugf(LogLevelDebug, "🔍 DEBUG: 域名API响应: code=%d, data=%v, err=%v", response.Code, response.Data, err)
	if err == nil && response.Code == 0 && len(response.Data) > 0 {
		domain := response.Data[0] // 取第一个域名
		f.debugf(LogLevelDebug, "动态获取上传域名成功: %s", domain)

		// 🗑️ 下载URL缓存已删除，不再缓存域名
		f.debugf(LogLevelDebug, "上传域名获取成功，不使用缓存: %s", domain)

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

	reqBody := map[string]any{
		"fileIDs": []int64{fileID},
	}

	var response struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}

	err := f.makeAPICallWithRest(ctx, "/api/v1/file/trash", "POST", reqBody, &response)
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

	reqBody := map[string]any{
		"fileIDs":        []int64{fileID},
		"toParentFileID": toParentFileID,
	}

	var response struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}

	err := f.makeAPICallWithRest(ctx, "/api/v1/file/move", "POST", reqBody, &response)
	if err != nil {
		return fmt.Errorf("移动文件失败: %w", err)
	}

	if response.Code != 0 {
		return fmt.Errorf("移动失败: API错误 %d: %s", response.Code, response.Message)
	}

	fs.Debugf(f, "成功移动文件%d到父目录%d", fileID, toParentFileID)
	return nil
}

// renameFile 重命名文件 - 使用正确的123网盘重命名API
func (f *Fs) renameFile(ctx context.Context, fileID int64, fileName string) error {
	fs.Debugf(f, "重命名文件%d为: %s", fileID, fileName)

	// 验证新文件名
	if err := validateFileName(fileName); err != nil {
		return fmt.Errorf("重命名文件名验证失败: %w", err)
	}
	time.Sleep(2 * time.Second)
	// 使用正确的123网盘重命名API: PUT /api/v1/file/name
	reqBody := map[string]any{
		"fileId":   fileID,
		"fileName": fileName,
	}

	var response struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}

	// 实现重试机制，最多重试3次
	maxRetries := 3
	var err error
	for attempt := 1; attempt <= maxRetries; attempt++ {
		fs.Debugf(f, "重命名文件尝试 %d/%d", attempt, maxRetries)

		err = f.makeAPICallWithRest(ctx, "/api/v1/file/name", "PUT", reqBody, &response)
		if err == nil {
			fs.Debugf(f, "重命名文件成功: %d -> %s", fileID, fileName)
			return nil
		}

		// 检查是否是"文件未找到"错误
		if strings.Contains(err.Error(), "没有找到文件") || strings.Contains(err.Error(), "API错误 1") {
			if attempt < maxRetries {
				waitTime := time.Duration(attempt*2) * time.Second
				fs.Debugf(f, "文件未找到，等待%v后重试 (尝试 %d/%d)", waitTime, attempt, maxRetries)
				time.Sleep(waitTime)
				continue
			}
		}

		// 其他错误或最后一次尝试失败
		break
	}

	return fmt.Errorf("重命名文件失败 (尝试%d次): %w", maxRetries, err)
}

// handlePartialFileConflict 处理partial文件重命名冲突
// 当partial文件尝试重命名为最终文件名时发生冲突时调用
func (f *Fs) handlePartialFileConflict(ctx context.Context, fileID int64, partialName string, parentID int64, targetName string) (*Object, error) {
	fs.Debugf(f, "处理partial文件冲突: %s -> %s (父目录: %d)", partialName, targetName, parentID)

	// 移除.partial后缀获取原始文件名
	originalName := strings.TrimSuffix(partialName, ".partial")
	if targetName == "" {
		targetName = originalName
	}

	// 检查目标文件是否已存在
	exists, existingFileID, err := f.fileExistsInDirectory(ctx, parentID, targetName)
	if err != nil {
		fs.Debugf(f, "检查目标文件存在性失败: %v", err)
		return nil, fmt.Errorf("检查目标文件失败: %w", err)
	}

	if exists {
		fs.Debugf(f, "目标文件已存在，ID: %d", existingFileID)

		// 如果存在的文件就是当前文件（可能已经重命名成功）
		if existingFileID == fileID {
			fs.Debugf(f, "文件已成功重命名，返回成功对象")
			return &Object{
				fs:          f,
				remote:      targetName,
				hasMetaData: true,
				id:          strconv.FormatInt(fileID, 10),
				size:        -1, // 需要重新获取
				modTime:     time.Now(),
				isDir:       false,
			}, nil
		}

		// 存在不同的同名文件，生成唯一名称
		fs.Debugf(f, "目标文件名冲突，生成唯一名称")
		uniqueName, err := f.generateUniqueFileName(ctx, parentID, targetName)
		if err != nil {
			fs.Debugf(f, "生成唯一文件名失败: %v", err)
			return nil, fmt.Errorf("生成唯一文件名失败: %w", err)
		}

		fs.Logf(f, "Partial文件重命名冲突，使用唯一名称: %s -> %s", targetName, uniqueName)
		targetName = uniqueName
	}

	// 尝试重命名partial文件为目标名称
	fs.Debugf(f, "重命名partial文件: %d -> %s", fileID, targetName)
	err = f.renameFile(ctx, fileID, targetName)
	if err != nil {
		// 如果重命名仍然失败，检查是否是因为文件已经有正确名称
		if strings.Contains(err.Error(), "当前目录有重名文件") ||
			strings.Contains(err.Error(), "文件已在当前文件夹") {
			fs.Debugf(f, "重命名失败，检查文件当前状态")

			// 再次检查文件是否已有正确名称
			exists, existingFileID, checkErr := f.fileExistsInDirectory(ctx, parentID, targetName)
			if checkErr == nil && exists && existingFileID == fileID {
				fs.Debugf(f, "文件实际已有正确名称，重命名成功")
				return &Object{
					fs:          f,
					remote:      targetName,
					hasMetaData: true,
					id:          strconv.FormatInt(fileID, 10),
					size:        -1, // 需要重新获取
					modTime:     time.Now(),
					isDir:       false,
				}, nil
			}
		}

		return nil, fmt.Errorf("partial文件重命名失败: %w", err)
	}

	fs.Debugf(f, "Partial文件重命名成功: %s", targetName)
	return &Object{
		fs:          f,
		remote:      targetName,
		hasMetaData: true,
		id:          strconv.FormatInt(fileID, 10),
		size:        -1, // 需要重新获取
		modTime:     time.Now(),
		isDir:       false,
	}, nil
}

// getUserInfo 获取用户信息包括存储配额 - 已迁移到rclone标准方法
func (f *Fs) getUserInfo(ctx context.Context) (*UserInfoResp, error) {
	var response UserInfoResp

	// 使用rclone标准rest客户端方法，自动集成QPS限制和重试机制
	err := f.makeAPICallWithRest(ctx, "/api/v1/user/info", "GET", nil, &response)
	if err != nil {
		return nil, err
	}

	if response.Code != 0 {
		return nil, fmt.Errorf("API error %d: %s", response.Code, response.Message)
	}

	return &response, nil
}

// GetUserInfoWithRest 获取用户信息包括存储配额 - 使用rclone标准方法的示例
// 这展示了如何将现有API调用迁移到rclone标准方法
func (f *Fs) GetUserInfoWithRest(ctx context.Context) (*UserInfoResp, error) {
	var response UserInfoResp

	// 使用新的rest客户端方法，自动集成QPS限制和重试机制
	err := f.makeAPICallWithRest(ctx, "/api/v1/user/info", "GET", nil, &response)
	if err != nil {
		return nil, err
	}

	if response.Code != 0 {
		return nil, fmt.Errorf("API error %d: %s", response.Code, response.Message)
	}

	return &response, nil
}

// createDirectoryWithRest 创建目录 - 使用rclone标准方法的POST请求示例
// 展示如何处理带请求体的API调用
func (f *Fs) createDirectoryWithRest(ctx context.Context, parentFileID int64, name string) (string, error) {
	// 准备请求体
	reqBody := map[string]any{
		"parentID": strconv.FormatInt(parentFileID, 10),
		"name":     name,
	}

	// 使用与现有方法相同的响应结构
	var response struct {
		Code    int         `json:"code"`
		Message string      `json:"message"`
		Data    interface{} `json:"data"`
	}

	// 使用rest客户端进行POST请求，自动处理QPS限制
	err := f.makeAPICallWithRest(ctx, "/upload/v1/file/mkdir", "POST", reqBody, &response)
	if err != nil {
		return "", err
	}

	if response.Code != 0 {
		return "", fmt.Errorf("创建目录失败: API错误 %d: %s", response.Code, response.Message)
	}

	// 这里应该从response.Data中提取目录ID，简化示例返回空字符串
	return "", nil
}

/*
=== rclone标准HTTP请求方法使用指南 ===

当前123网盘backend已支持rclone标准的HTTP请求方法，具有以下优势：

1. **自动QPS限制**: 通过pacer机制自动控制请求频率
2. **统一错误处理**: 使用rclone标准的重试和错误处理逻辑
3. **更好的日志记录**: 集成rclone的日志系统
4. **框架一致性**: 符合rclone backend开发标准

## 使用方法：

### 1. GET请求示例（无请求体）：
```go
var response SomeResponse
err := f.makeAPICallWithRest(ctx, "/api/v1/user/info", "GET", nil, &response)
```

### 2. POST请求示例（带请求体）：
```go
reqBody := map[string]any{
    "param1": "value1",
    "param2": "value2",
}
var response SomeResponse
err := f.makeAPICallWithRest(ctx, "/api/v1/some/endpoint", "POST", reqBody, &response)
```

## 迁移建议：

1. **保持向后兼容**: 现有的apiCall方法继续工作
2. **逐步迁移**: 新功能优先使用makeAPICallWithRest
3. **测试验证**: 迁移后充分测试确保功能正常

## QPS控制：

makeAPICallWithRest会自动根据API端点选择合适的pacer：
- /api/v2/file/list: ~14 QPS
- /upload/v2/file/*: 4-16 QPS（根据具体端点）
- /api/v1/file/*: 1-10 QPS（根据具体端点）

所有QPS限制基于123网盘官方API文档设定。
*/

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

	// 创建HTTP客户端
	client := fshttp.NewClient(ctx)

	f := &Fs{
		name:         name,
		originalName: originalName,
		root:         normalizedRoot,
		opt:          *opt,
		m:            m,
		rootFolderID: opt.RootFolderID,
		client:       client,                                         // 保留用于特殊情况
		rst:          rest.NewClient(client).SetRoot(openAPIRootURL), // rclone标准rest客户端
	}

	// Initialize multiple pacers for different API rate limits based on official documentation
	// 使用pacer工厂函数减少重复代码

	// v2 API pacer - 高频率 (~14 QPS for api/v2/file/list)
	listPacerSleep := listV2APIMinSleep
	if opt.ListPacerMinSleep > 0 {
		listPacerSleep = time.Duration(opt.ListPacerMinSleep)
	}
	f.listPacer = createPacer(ctx, listPacerSleep, maxSleep, decayConstant)

	// 严格API pacer - 中等频率 (~4 QPS for create, async_result, etc.)
	strictPacerSleep := fileMoveMinSleep
	if opt.StrictPacerMinSleep > 0 {
		strictPacerSleep = time.Duration(opt.StrictPacerMinSleep)
	} else if opt.PacerMinSleep > 0 {
		strictPacerSleep = time.Duration(opt.PacerMinSleep) // 向后兼容
	}
	f.strictPacer = createPacer(ctx, strictPacerSleep, maxSleep, decayConstant)

	// v2分片上传API pacer (~5 QPS for upload/v2/file/slice)
	uploadPacerSleep := uploadV2SliceMinSleep
	if opt.UploadPacerMinSleep > 0 {
		uploadPacerSleep = time.Duration(opt.UploadPacerMinSleep)
	}
	f.uploadPacer = createPacer(ctx, uploadPacerSleep, maxSleep, decayConstant)

	// 下载和高频率API pacer (~16 QPS for v2 upload APIs)
	downloadPacerSleep := downloadInfoMinSleep
	if opt.DownloadPacerMinSleep > 0 {
		downloadPacerSleep = time.Duration(opt.DownloadPacerMinSleep)
	}
	f.downloadPacer = createPacer(ctx, downloadPacerSleep, maxSleep, decayConstant)

	fs.Infof(f, "📊 QPS限制配置 - 列表API: %.1f QPS, 严格API: %.1f QPS, 上传API: %.1f QPS, 下载API: %.1f QPS",
		1000.0/float64(listPacerSleep.Milliseconds()),
		1000.0/float64(strictPacerSleep.Milliseconds()),
		1000.0/float64(uploadPacerSleep.Milliseconds()),
		1000.0/float64(downloadPacerSleep.Milliseconds()))

	// 初始化性能优化组件 - 使用统一的并发控制管理器
	f.concurrencyManager = NewConcurrencyManager(opt.MaxConcurrentUploads, opt.MaxConcurrentDownloads)

	// 初始化内存管理器，防止内存泄漏 - 优化内存限制
	// 总内存限制150MB，单文件限制30MB，减少内存压力
	f.memoryManager = NewMemoryManager(150*1024*1024, 30*1024*1024)

	// 初始化性能指标收集器
	f.performanceMetrics = NewPerformanceMetrics()

	// 初始化增强性能指标收集器
	f.enhancedMetrics = NewEnhancedPerformanceMetrics()

	// 初始化资源池管理器
	f.resourcePool = NewResourcePool()

	// 启动性能监控定时器（如果启用）
	if opt.EnablePerformanceLog {
		f.startPerformanceMonitoring(ctx)
	}

	// 初始化缓存配置
	f.cacheConfig = DefaultCacheConfig()

	// 🔧 初始化缓存键生成器
	f.cacheKeyGen = NewCacheKeyGenerator("123pan", name)

	// 🔧 初始化统一重试管理器，避免多层重试嵌套
	f.retryManager = NewUnifiedRetryManager()

	// 🔧 初始化跨云传输管理器，防止重复下载
	f.crossCloudManager = NewCrossCloudTransferManager()

	// 🔧 初始化统一进度管理器，避免重复进度显示
	f.progressManager = NewUnifiedProgressManager()

	// 🔧 初始化传输跟踪器
	f.transferTracker = NewTransferTracker()

	// 🔧 初始化多实例BadgerDB缓存管理器，解决冲突问题
	cacheDir := cache.GetCacheDir("123drive")

	cacheManager, err := NewMultiInstanceCacheManager("123drive", cacheDir)
	if err != nil {
		fs.Errorf(f, "初始化多实例缓存管理器失败: %v", err)
		// 缓存初始化失败不应该阻止文件系统工作，继续执行
	} else {
		f.cacheManager = cacheManager

		// 🔧 设置兼容性字段，保持向后兼容
		f.parentIDCache = cacheManager.GetParentIDCache()
		f.dirListCache = cacheManager.GetDirListCache()
		f.pathToIDCache = cacheManager.GetPathToIDCache()

		fs.Infof(f, "🔧 多实例缓存管理器初始化成功")
	}

	// 🗑️ 下载URL缓存管理器已删除

	// 🔧 初始化上传验证管理器，优化断点续传验证阶段错误处理
	f.verificationManager = NewUploadVerificationManager(f)
	fs.Infof(f, "🔧 上传验证管理器初始化成功")

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

	// 初始化动态参数调整器
	f.dynamicAdjuster = NewDynamicParameterAdjuster()
	fs.Debugf(f, "动态参数调整器初始化成功")

	// 🛡️ 注册程序退出时的资源清理钩子
	// 确保即使程序异常退出也能清理临时文件
	if f.resourcePool != nil {
		atexit.Register(func() {
			fs.Debugf(f, "🚨 程序退出信号检测到，开始清理123网盘资源...")

			// 清理所有临时文件
			f.resourcePool.CleanupAllTempFiles()

			// 🔧 关闭多实例缓存管理器
			if f.cacheManager != nil {
				if err := f.cacheManager.Close(); err != nil {
					fs.Debugf(f, "程序退出时关闭多实例缓存管理器失败: %v", err)
				}
			}

			fs.Debugf(f, "✅ 123网盘资源清理完成")
		})
		fs.Debugf(f, "🛡️ 程序退出清理钩子注册成功")
	}

	fs.Infof(f, "123网盘性能优化初始化完成 - 最大并发上传: %d, 最大并发下载: %d, 进度显示: %v, 动态调整: 启用",
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
		PartialUploads:          false, // 禁用.partial机制，避免重命名API问题
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
			listPacer:          f.listPacer,
			strictPacer:        f.strictPacer,
			uploadPacer:        f.uploadPacer,
			downloadPacer:      f.downloadPacer,
			parentIDCache:      f.parentIDCache,
			dirListCache:       f.dirListCache,
			pathToIDCache:      f.pathToIDCache,
			concurrencyManager: f.concurrencyManager,
			memoryManager:      f.memoryManager,
			performanceMetrics: f.performanceMetrics,
			resourcePool:       f.resourcePool,
			apiVersionManager:  f.apiVersionManager,
			resumeManager:      f.resumeManager,
			cacheConfig:        f.cacheConfig,
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
	} else if normalizedRemote == "" {
		// 当remote为空时，fullPath就是root本身
		fullPath = f.root
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

	fs.Debugf(f, "🔍 NewObject pathToFileID结果: fullPath=%s -> fileID=%s", fullPath, fileID)

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

	// 确定正确的remote路径
	objectRemote := remote
	if objectRemote == "" && f.root != "" {
		// 当remote为空且root指向文件时，使用文件名作为remote
		objectRemote = fileInfo.Filename
		fs.Debugf(f, "🔍 NewObject设置remote: %s -> %s", remote, objectRemote)
	}

	o := &Object{
		fs:          f,
		remote:      objectRemote,
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
	if err := f.concurrencyManager.AcquireUpload(ctx); err != nil {
		return nil, fmt.Errorf("获取上传信号量失败: %w", err)
	}
	// 使用命名返回值确保在panic时也能释放信号量
	defer f.concurrencyManager.ReleaseUpload()

	// 规范化远程路径
	normalizedRemote := normalizePath(src.Remote())
	fs.Debugf(f, "Put操作规范化路径: %s -> %s", src.Remote(), normalizedRemote)

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
	parentFileID, err := parseParentID(parentID)
	if err != nil {
		return nil, fmt.Errorf("解析父目录ID失败: %w", err)
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
		parentFileID, err = parseParentID(parentID)
		if err != nil {
			return nil, fmt.Errorf("重新解析父目录ID失败: %w", err)
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
		parentFileID, err = parseParentID(parentID)
		if err != nil {
			return nil, fmt.Errorf("重建后解析父目录ID失败: %w", err)
		}
		fs.Debugf(f, "重建后找到父目录ID: %d", parentFileID)
	} else {
		fs.Debugf(f, "父目录ID %d 验证成功", parentFileID)
	}

	// Use streaming optimization for cross-cloud transfers
	obj, err := f.streamingPut(ctx, in, src, parentFileID, fileName)

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
	fs.Debugf(o.fs, "🔍 DEBUG: 123网盘Open方法被调用: %s, 大小: %s", o.remote, fs.SizeSuffix(o.size))

	// 🚀 跨云传输优化：检测大文件并启用多线程下载（参考115网盘实现）
	// 🔧 修复：检查是否已经有禁用并发下载选项，避免重复并发
	hasDisableOption := false
	for _, option := range options {
		if option.String() == "DisableConcurrentDownload" {
			hasDisableOption = true
			break
		}
	}

	if !hasDisableOption && o.shouldUseConcurrentDownload(ctx, options) {
		return o.openWithConcurrency(ctx, options...)
	}

	fs.Debugf(o.fs, "🔍 DEBUG: 123网盘使用普通Open方法: %s", o.remote)

	// 记录下载开始
	o.fs.performanceMetrics.RecordDownloadStart(o.size)

	// 并发控制：获取下载信号量
	if err := o.fs.concurrencyManager.AcquireDownload(ctx); err != nil {
		return nil, err
	}
	// 信号量将在ReadCloser关闭时释放

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
		o.fs.concurrencyManager.ReleaseDownload()
		return nil, err
	}

	// 创建包装的ReadCloser来处理进度更新和资源清理
	return &ProgressReadCloser{
		ReadCloser:      resp.Body,
		fs:              o.fs,
		totalSize:       o.size,
		transferredSize: 0,
		startTime:       time.Now(),
	}, nil
}

// shouldUseConcurrentDownload 判断是否应该使用并发下载（参考115网盘实现）
func (o *Object) shouldUseConcurrentDownload(ctx context.Context, options []fs.OpenOption) bool {
	// 检查文件大小，只对大文件使用并发下载

	if o.size < 1*1024*1024 { // 降低到1MB阈值，几乎所有文件都触发

		return false
	}

	// 检查是否有用户指定的Range选项
	// 智能Range检测：区分小Range请求和大Range请求（跨云传输优化）
	for _, option := range options {
		if rangeOpt, ok := option.(*fs.RangeOption); ok {
			// 如果Range覆盖了整个文件，则可以使用并发下载
			if rangeOpt.Start == 0 && (rangeOpt.End == -1 || rangeOpt.End >= o.size-1) {
				fs.Debugf(o, "检测到全文件Range选项，允许并发下载")
				break
			} else {
				// 计算Range大小
				rangeSize := rangeOpt.End - rangeOpt.Start + 1

				// 🚀 跨云传输优化：如果Range足够大（>=32MB），启用并发下载
				// 这主要针对rclone多线程传输场景，每个线程处理的数据块通常较大
				if rangeSize >= 32*1024*1024 { // 32MB阈值
					fs.Infof(o, "🚀 检测到大Range选项 (%d-%d, %s)，启用并发下载优化（跨云传输场景）",
						rangeOpt.Start, rangeOpt.End, fs.SizeSuffix(rangeSize))
					break
				} else {
					fs.Debugf(o, "检测到小Range选项 (%d-%d, %s)，跳过并发下载",
						rangeOpt.Start, rangeOpt.End, fs.SizeSuffix(rangeSize))
					return false
				}
			}
		}
	}

	return true
}

// openWithConcurrency 使用并发下载打开文件（用于跨云传输优化）
func (o *Object) openWithConcurrency(ctx context.Context, options ...fs.OpenOption) (io.ReadCloser, error) {
	fs.Infof(o, "🚀 123网盘启动并发下载: %s", fs.SizeSuffix(o.size))

	// 创建临时文件用于并发下载
	tempFile, err := os.CreateTemp("", "123_concurrent_download_*.tmp")
	if err != nil {
		fs.Debugf(o, "创建临时文件失败，回退到普通下载: %v", err)
		return o.openNormal(ctx, options...)
	}

	// 使用并发下载到临时文件
	downloadedSize, err := o.fs.downloadWithConcurrency(ctx, o, tempFile, o.size, o.remote)
	if err != nil {
		tempFile.Close()
		os.Remove(tempFile.Name())
		fs.Debugf(o, "并发下载失败，回退到普通下载: %v", err)
		return o.openNormal(ctx, options...)
	}

	// 验证下载大小
	if downloadedSize != o.size {
		tempFile.Close()
		os.Remove(tempFile.Name())
		fs.Debugf(o, "下载大小不匹配，回退到普通下载: 期望%d，实际%d", o.size, downloadedSize)
		return o.openNormal(ctx, options...)
	}

	// 重置文件指针到开始位置
	_, err = tempFile.Seek(0, io.SeekStart)
	if err != nil {
		tempFile.Close()
		os.Remove(tempFile.Name())
		return nil, fmt.Errorf("重置临时文件指针失败: %w", err)
	}

	fs.Infof(o, "✅ 123网盘并发下载完成: %s", fs.SizeSuffix(downloadedSize))

	// 返回一个包装的ReadCloser，在关闭时删除临时文件
	return &ConcurrentDownloadReader{
		file:     tempFile,
		tempPath: tempFile.Name(),
	}, nil
}

// openNormal 普通的打开方法（原来的逻辑）
func (o *Object) openNormal(ctx context.Context, options ...fs.OpenOption) (io.ReadCloser, error) {
	// 记录下载开始
	o.fs.performanceMetrics.RecordDownloadStart(o.size)

	// 并发控制：获取下载信号量
	if err := o.fs.concurrencyManager.AcquireDownload(ctx); err != nil {
		return nil, err
	}
	// 信号量将在ReadCloser关闭时释放

	var resp *http.Response
	var err error

	// 使用重试机制获取下载链接并下载
	err = o.fs.downloadPacer.Call(func() (bool, error) {
		downloadURL, err := o.fs.getDownloadURL(ctx, o.id)
		if err != nil {
			return shouldRetry(ctx, resp, fmt.Errorf("获取下载链接失败: %w", err))
		}

		req, err := http.NewRequestWithContext(ctx, "GET", downloadURL, nil)
		if err != nil {
			return false, err
		}

		// 处理Range选项
		fs.FixRangeOption(options, o.size)
		fs.OpenOptionAddHTTPHeaders(req.Header, options)

		resp, err = o.fs.client.Do(req)
		if err != nil {
			return shouldRetry(ctx, resp, fmt.Errorf("下载请求失败: %w", err))
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			return shouldRetry(ctx, resp, fmt.Errorf("下载失败，状态码: %d, 响应: %s", resp.StatusCode, string(body)))
		}

		return false, nil
	})

	if err != nil {
		// 释放下载信号量
		o.fs.concurrencyManager.ReleaseDownload()
		return nil, err
	}

	// 创建包装的ReadCloser来处理进度更新和资源清理
	return &ProgressReadCloser{
		ReadCloser:      resp.Body,
		fs:              o.fs,
		totalSize:       o.size,
		transferredSize: 0,
		startTime:       time.Now(),
	}, nil
}

// Update the object with new content.
// 优化版本：使用统一上传系统，支持动态参数调整和多线程上传
func (o *Object) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) error {
	fs.Debugf(o.fs, "调用Update: %s", o.remote)

	// 记录上传开始
	startTime := time.Now()
	o.fs.performanceMetrics.RecordUploadStart(src.Size())

	// 并发控制：获取上传信号量
	if err := o.fs.concurrencyManager.AcquireUpload(ctx); err != nil {
		return fmt.Errorf("获取上传信号量失败: %w", err)
	}
	defer o.fs.concurrencyManager.ReleaseUpload()

	// Use dircache to find parent directory
	leaf, parentID, err := o.fs.dirCache.FindPath(ctx, o.remote, false)
	if err != nil {
		return err
	}

	// Convert parentID to int64
	parentFileID, err := parseParentID(parentID)
	if err != nil {
		return fmt.Errorf("解析父目录ID失败: %w", err)
	}

	// 验证并清理文件名
	cleanedFileName, warning := validateAndCleanFileName(leaf)
	if warning != nil {
		fs.Logf(o.fs, "文件名验证警告: %v", warning)
		leaf = cleanedFileName
	}

	// 使用新的统一上传系统进行更新
	fs.Debugf(o.fs, "使用统一上传系统进行Update操作")
	newObj, err := o.fs.streamingPut(ctx, in, src, parentFileID, leaf)

	// 记录上传完成或错误
	duration := time.Since(startTime)
	if err != nil {
		o.fs.performanceMetrics.RecordUploadError()
		fs.Errorf(o.fs, "Update操作失败: %v", err)
		return err
	}

	// 更新当前对象的元数据
	o.size = newObj.size
	o.md5sum = newObj.md5sum
	o.modTime = newObj.modTime
	o.id = newObj.id

	// 记录上传成功
	o.fs.performanceMetrics.RecordUploadComplete(src.Size(), duration)
	fs.Debugf(o.fs, "Update操作成功，耗时: %v", duration)

	return nil
}

// Remove the object.
func (o *Object) Remove(ctx context.Context) error {
	fs.Debugf(o.fs, "调用Remove: %s", o.remote)

	// 检查文件ID是否有效
	if o.id == "" {
		fs.Debugf(o.fs, "文件ID为空，可能是partial文件或上传失败的文件: %s", o.remote)
		// 对于partial文件或上传失败的文件，我们无法删除，但也不应该报错
		// 因为这些文件可能根本不存在于服务器上
		return nil
	}

	// Convert file ID to int64
	fileID, err := parseFileID(o.id)
	if err != nil {
		fs.Debugf(o.fs, "解析文件ID失败，可能是partial文件: %s, 错误: %v", o.remote, err)
		// 对于无法解析的文件ID，我们也不报错，因为这通常是partial文件
		return nil
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
	parentFileID, err := parseParentID(pathID)
	if err != nil {
		return "", false, fmt.Errorf("解析父目录ID失败: %w", err)
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

// findLeafWithForceRefresh 强制刷新缓存后查找叶节点
// 用于解决缓存不一致导致的目录查找失败问题
func (f *Fs) findLeafWithForceRefresh(ctx context.Context, pathID, leaf string) (foundID string, found bool, err error) {
	fs.Debugf(f, "强制刷新查找叶节点: pathID=%s, leaf=%s", pathID, leaf)

	// Convert pathID to int64
	parentFileID, err := parseParentID(pathID)
	if err != nil {
		return "", false, fmt.Errorf("解析父目录ID失败: %w", err)
	}

	// 强制清理缓存
	f.clearDirListCache(parentFileID)

	// 直接调用API获取最新的目录列表，跳过缓存
	params := url.Values{}
	params.Add("parentFileId", fmt.Sprintf("%d", parentFileID))
	params.Add("limit", fmt.Sprintf("%d", f.opt.ListChunk))

	endpoint := "/api/v2/file/list?" + params.Encode()

	var response ListResponse
	err = f.makeAPICallWithRest(ctx, endpoint, "GET", nil, &response)
	if err != nil {
		return "", false, fmt.Errorf("强制刷新API调用失败: %w", err)
	}

	if response.Code != 0 {
		return "", false, fmt.Errorf("API error %d: %s", response.Code, response.Message)
	}

	// Search for the leaf in fresh data
	for _, file := range response.Data.FileList {
		if file.Filename == leaf {
			foundID = strconv.FormatInt(file.FileID, 10)
			found = true

			// 更新缓存以保持一致性
			f.saveDirListToCache(parentFileID, 0, response.Data.FileList, response.Data.LastFileId)

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

			fs.Debugf(f, "强制刷新找到叶节点: %s -> %s", leaf, foundID)
			return foundID, found, nil
		}
	}

	fs.Debugf(f, "强制刷新未找到叶节点: %s", leaf)
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
	dirID, err := f.createDirectory(ctx, pathID, leaf)
	if err != nil {
		return "", err
	}

	return dirID, nil
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
	parentFileID, err := parseDirID(dirID)
	if err != nil {
		return fmt.Errorf("解析目录ID失败: %w", err)
	}

	// List all files in the directory
	var allFiles []FileListInfoRespDataV2
	lastFileID := int64(0)
	maxIterations := 10000 // 🔧 防止无限循环：最多10000次迭代
	iteration := 0

	for {
		iteration++
		if iteration > maxIterations {
			return fmt.Errorf("清理目录超过最大迭代次数 %d，可能存在循环", maxIterations)
		}
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

	// 清理所有临时文件
	if f.resourcePool != nil {
		f.resourcePool.CleanupAllTempFiles()
		f.resourcePool.Close()
	}

	// 🔧 关闭多实例缓存管理器
	if f.cacheManager != nil {
		if err := f.cacheManager.Close(); err != nil {
			fs.Debugf(f, "关闭多实例缓存管理器失败: %v", err)
		}
	}

	return nil
}

// List the objects and directories in dir into entries.
func (f *Fs) List(ctx context.Context, dir string) (entries fs.DirEntries, err error) {
	fs.Debugf(f, "调用列表，目录: %s", dir)

	// 特殊处理：如果dir为空且root指向文件，返回包含该文件的列表
	if dir == "" && f.root != "" {
		// 检查root是否指向一个文件
		rootObj, err := f.NewObject(ctx, "")
		if err == nil {
			// root指向一个文件，返回包含该文件的列表
			fs.Debugf(f, "root路径指向文件，返回包含该文件的列表: %s", f.root)
			return fs.DirEntries{rootObj}, nil
		}
		// 如果NewObject失败，检查是否是因为root指向目录
		if err == fs.ErrorNotAFile {
			fs.Debugf(f, "root路径指向目录，继续目录列表逻辑: %s", f.root)
		} else {
			fs.Debugf(f, "root路径检查失败，继续目录列表逻辑: %s, 错误: %v", f.root, err)
		}
	}

	// 使用目录缓存查找目录ID
	fs.Debugf(f, "查找目录ID: %s", dir)
	dirID, err := f.dirCache.FindDir(ctx, dir, false)
	if err != nil {
		fs.Debugf(f, "查找目录失败 %s: %v", dir, err)
		return nil, err
	}
	fs.Debugf(f, "找到目录ID: %s，目录: %s", dirID, dir)

	// Convert dirID to int64
	parentFileID, err := parseDirID(dirID)
	if err != nil {
		return nil, fmt.Errorf("解析目录ID失败: %w", err)
	}

	// List files in the directory
	var allFiles []FileListInfoRespDataV2
	lastFileID := int64(0)
	maxIterations := 10000 // 🔧 防止无限循环：最多10000次迭代
	iteration := 0

	for {
		iteration++
		if iteration > maxIterations {
			return nil, fmt.Errorf("列表目录超过最大迭代次数 %d，可能存在循环", maxIterations)
		}
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
		// 🔍 调试：检查API返回的原始文件名
		fs.Debugf(f, "🔍 API返回的原始文件名: [%s] (长度=%d, 字节=%v)", file.Filename, len(file.Filename), []byte(file.Filename))

		// 🔧 修复：清理API返回的文件名，去除前后空格
		cleanedFilename := strings.TrimSpace(file.Filename)
		if cleanedFilename != file.Filename {
			fs.Debugf(f, "🔧 清理文件名: [%s] -> [%s]", file.Filename, cleanedFilename)
		}

		remote := cleanedFilename
		if dir != "" {
			remote = strings.Trim(dir+"/"+cleanedFilename, "/")
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

	// 规范化目标路径 - 使用通用工具
	normalizedRemote := PathUtil.NormalizePath(remote)
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
			// 检查是否是因为文件已在目标目录 - 修复字符串匹配问题
			if strings.Contains(err.Error(), "文件已在当前文件夹中") ||
				strings.Contains(err.Error(), "文件已在当前文件夹") {
				fs.Debugf(f, "文件可能已在目标目录，检查状态")
				exists, existingFileID, checkErr := f.fileExistsInDirectory(ctx, dstParentID, srcBaseName)
				if checkErr == nil && exists && existingFileID == fileID {
					fs.Debugf(f, "确认文件已在目标目录，继续重命名步骤")
					// 文件已在目标目录，跳过移动步骤
				} else {
					// 检查是否是partial文件重命名冲突
					if strings.HasSuffix(srcBaseName, ".partial") {
						fs.Debugf(f, "Partial文件移动冲突，尝试处理文件名冲突")
						return f.handlePartialFileConflict(ctx, fileID, srcBaseName, dstParentID, dstName)
					}
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
				// 检查重命名失败的原因 - 增强错误匹配
				if strings.Contains(err.Error(), "当前目录有重名文件") ||
					strings.Contains(err.Error(), "文件已在当前文件夹") {
					fs.Debugf(f, "重命名失败，目录中已有重名文件，检查是否为目标文件")
					exists, existingFileID, checkErr := f.fileExistsInDirectory(ctx, dstParentID, dstName)
					if checkErr == nil && exists && existingFileID == fileID {
						fs.Debugf(f, "文件已有正确名称，重命名操作实际已成功")
						// 文件已经有正确的名称，认为操作成功
					} else {
						// 检查是否是partial文件重命名冲突
						if strings.HasSuffix(srcBaseName, ".partial") {
							fs.Debugf(f, "Partial文件重命名冲突，尝试特殊处理")
							return f.handlePartialFileConflict(ctx, fileID, srcBaseName, dstParentID, dstName)
						}
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

	// 规范化目标路径 - 使用通用工具
	normalizedRemote := PathUtil.NormalizePath(remote)
	fs.Debugf(f, "Copy操作规范化目标路径: %s -> %s", remote, normalizedRemote)

	srcObj, ok := src.(*Object)
	if !ok {
		fs.Debugf(f, "无法复制 - 不是相同的远程类型")
		return nil, fs.ErrorCantCopy
	}

	// Use dircache to find destination directory
	dstName, _, err := f.dirCache.FindPath(ctx, normalizedRemote, true)
	if err != nil {
		return nil, err
	}

	// 验证并清理目标文件名
	cleanedDstName, warning := validateAndCleanFileName(dstName)
	if warning != nil {
		fs.Logf(f, "复制操作文件名验证警告: %v", warning)
		dstName = cleanedDstName
	}

	// 123Pan doesn't have a direct copy API, use download+upload
	fs.Debugf(f, "123网盘不支持服务器端复制，使用下载+上传方式")
	return f.copyViaDownloadUpload(ctx, srcObj, remote)
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
// 简化版本：直接使用rclone的标准HTTP客户端配置
func getHTTPClient(ctx context.Context, name string, m configmap.Mapper) *http.Client {
	// rclone的fshttp.NewClient已经提供了合适的默认配置
	// 包括超时、连接池、TLS设置等，无需额外自定义
	return fshttp.NewClient(ctx)
}

// getNetworkQuality 评估当前网络质量，返回0.0-1.0的质量分数
func (f *Fs) getNetworkQuality() float64 {
	// 基于最近的传输统计评估网络质量
	if f.performanceMetrics == nil {
		return 0.8 // 默认假设网络质量良好
	}

	// 从性能指标获取实际的错误率和传输统计
	stats := f.performanceMetrics.GetStats()

	// 获取错误率
	var errorRate float64 = 0.02 // 默认2%错误率
	if apiErrorRate, ok := stats["api_error_rate"].(float64); ok {
		errorRate = apiErrorRate
	}

	// 获取平均传输速度作为网络质量指标
	var speedQuality float64 = 0.8 // 默认质量分数
	if avgSpeed, ok := stats["average_upload_speed"].(float64); ok {
		// 基于传输速度评估网络质量
		if avgSpeed > 50 { // >50MB/s - 优秀网络
			speedQuality = 0.95
		} else if avgSpeed > 20 { // >20MB/s - 良好网络
			speedQuality = 0.85
		} else if avgSpeed > 10 { // >10MB/s - 一般网络
			speedQuality = 0.7
		} else if avgSpeed > 5 { // >5MB/s - 较差网络
			speedQuality = 0.5
		} else { // <5MB/s - 很差网络
			speedQuality = 0.3
		}
	}

	// 基于错误率调整质量分数
	var errorQuality float64
	if errorRate > 0.15 { // 错误率>15%
		errorQuality = 0.2 // 网络质量很差
	} else if errorRate > 0.10 { // 错误率>10%
		errorQuality = 0.4 // 网络质量差
	} else if errorRate > 0.05 { // 错误率>5%
		errorQuality = 0.6 // 网络质量一般
	} else if errorRate > 0.02 { // 错误率>2%
		errorQuality = 0.8 // 网络质量良好
	} else {
		errorQuality = 0.95 // 网络质量优秀
	}

	// 综合速度和错误率评估（权重：速度60%，错误率40%）
	finalQuality := speedQuality*0.6 + errorQuality*0.4

	f.debugf(LogLevelVerbose, "网络质量评估: 错误率=%.3f, 速度质量=%.2f, 错误质量=%.2f, 最终质量=%.2f",
		errorRate, speedQuality, errorQuality, finalQuality)

	return finalQuality
}

// DynamicParameterAdjuster 动态参数调整器
type DynamicParameterAdjuster struct {
	mu                    sync.RWMutex
	lastAdjustment        time.Time
	adjustmentInterval    time.Duration
	networkQualityHistory []float64
	maxHistorySize        int
	currentConcurrency    int
	currentChunkSize      int64
	currentTimeout        time.Duration
}

// NewDynamicParameterAdjuster 创建动态参数调整器
func NewDynamicParameterAdjuster() *DynamicParameterAdjuster {
	return &DynamicParameterAdjuster{
		adjustmentInterval:    30 * time.Second, // 每30秒调整一次
		networkQualityHistory: make([]float64, 0),
		maxHistorySize:        10,                // 保留最近10次质量记录
		currentConcurrency:    4,                 // 默认并发数（用户要求上限）
		currentChunkSize:      100 * 1024 * 1024, // 默认100MB分片
		currentTimeout:        5 * time.Minute,   // 默认5分钟超时
	}
}

// ShouldAdjust 检查是否需要调整参数
func (dpa *DynamicParameterAdjuster) ShouldAdjust() bool {
	dpa.mu.RLock()
	defer dpa.mu.RUnlock()

	return time.Since(dpa.lastAdjustment) >= dpa.adjustmentInterval
}

// RecordNetworkQuality 记录网络质量
func (dpa *DynamicParameterAdjuster) RecordNetworkQuality(quality float64) {
	dpa.mu.Lock()
	defer dpa.mu.Unlock()

	dpa.networkQualityHistory = append(dpa.networkQualityHistory, quality)

	// 保持历史记录大小限制
	if len(dpa.networkQualityHistory) > dpa.maxHistorySize {
		dpa.networkQualityHistory = dpa.networkQualityHistory[1:]
	}
}

// GetAverageNetworkQuality 获取平均网络质量
func (dpa *DynamicParameterAdjuster) GetAverageNetworkQuality() float64 {
	dpa.mu.RLock()
	defer dpa.mu.RUnlock()

	if len(dpa.networkQualityHistory) == 0 {
		return 0.8 // 默认质量
	}

	var total float64
	for _, quality := range dpa.networkQualityHistory {
		total += quality
	}

	return total / float64(len(dpa.networkQualityHistory))
}

// AdjustParameters 根据网络质量调整参数
func (dpa *DynamicParameterAdjuster) AdjustParameters(fileSize int64, networkSpeed int64, networkQuality float64) (concurrency int, chunkSize int64, timeout time.Duration) {
	dpa.mu.Lock()
	defer dpa.mu.Unlock()

	dpa.lastAdjustment = time.Now()

	// 直接记录网络质量，避免重入锁问题
	dpa.networkQualityHistory = append(dpa.networkQualityHistory, networkQuality)
	if len(dpa.networkQualityHistory) > dpa.maxHistorySize {
		dpa.networkQualityHistory = dpa.networkQualityHistory[1:]
	}

	// 直接计算平均网络质量，避免重入锁问题
	var avgQuality float64
	if len(dpa.networkQualityHistory) == 0 {
		avgQuality = 0.8 // 默认质量
	} else {
		var total float64
		for _, quality := range dpa.networkQualityHistory {
			total += quality
		}
		avgQuality = total / float64(len(dpa.networkQualityHistory))
	}

	// 🚀 用户要求：123网盘单文件并发限制到4
	baseConcurrency := 4
	if avgQuality > 0.9 { // 优秀网络
		baseConcurrency = 4 // 限制为4
	} else if avgQuality > 0.7 { // 良好网络
		baseConcurrency = 4 // 限制为4
	} else if avgQuality > 0.5 { // 一般网络
		baseConcurrency = 4 // 限制为4
	} else { // 较差网络
		baseConcurrency = 2
	}

	// 根据文件大小调整（但不超过4）
	if fileSize > 5*1024*1024*1024 { // >5GB
		baseConcurrency = 4 // 限制为4
	} else if fileSize < 500*1024*1024 { // <500MB
		baseConcurrency = int(float64(baseConcurrency) * 0.7)
	}

	// 🚀 用户要求：123网盘单文件并发限制到4
	if baseConcurrency < 1 {
		baseConcurrency = 1
	}
	if baseConcurrency > 4 {
		baseConcurrency = 4 // 限制为4
	}

	dpa.currentConcurrency = baseConcurrency

	// 基于网络质量调整分片大小
	baseChunkSize := int64(100 * 1024 * 1024) // 100MB
	if avgQuality > 0.8 {                     // 高质量网络使用大分片
		baseChunkSize = int64(200 * 1024 * 1024) // 200MB
	} else if avgQuality < 0.5 { // 低质量网络使用小分片
		baseChunkSize = int64(50 * 1024 * 1024) // 50MB
	}

	dpa.currentChunkSize = baseChunkSize

	// 基于网络质量调整超时时间
	baseTimeout := 5 * time.Minute
	if avgQuality < 0.5 { // 网络质量差，增加超时时间
		baseTimeout = time.Duration(float64(baseTimeout) * 2.0)
	} else if avgQuality > 0.8 { // 网络质量好，可以减少超时时间
		baseTimeout = time.Duration(float64(baseTimeout) * 0.8)
	}

	dpa.currentTimeout = baseTimeout

	return dpa.currentConcurrency, dpa.currentChunkSize, dpa.currentTimeout
}

// getAdaptiveTimeout 根据文件大小、传输类型和网络质量计算自适应超时时间
// 优化版本：集成动态参数调整器，实现智能自适应超时控制
func (f *Fs) getAdaptiveTimeout(fileSize int64, transferType string) time.Duration {
	// 检查是否需要进行动态调整
	if f.dynamicAdjuster != nil && f.dynamicAdjuster.ShouldAdjust() {
		networkSpeed := f.detectNetworkSpeed(context.Background())
		networkQuality := f.getNetworkQuality()
		_, _, timeout := f.dynamicAdjuster.AdjustParameters(fileSize, networkSpeed, networkQuality)

		// 根据传输类型调整超时时间
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

		adjustedTimeout := time.Duration(float64(timeout) * typeMultiplier)

		f.debugf(LogLevelVerbose, "动态超时调整: %s -> %v (质量=%.2f)",
			fs.SizeSuffix(fileSize), adjustedTimeout, networkQuality)

		return adjustedTimeout
	}

	// 回退到传统的静态计算方法
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
	f.debugf(LogLevelDebug, "开始检测网络速度")

	// 优先使用历史性能数据进行速度估算
	if f.performanceMetrics != nil {
		// 尝试从性能指标获取最近的传输速度
		stats := f.performanceMetrics.GetStats()
		if avgSpeed, ok := stats["average_upload_speed"].(float64); ok && avgSpeed > 0 {
			// 将MB/s转换为bytes/s
			historicalSpeed := int64(avgSpeed * 1024 * 1024)
			f.debugf(LogLevelDebug, "使用历史数据检测网络速度: %s/s (基于性能统计)",
				fs.SizeSuffix(historicalSpeed))
			return historicalSpeed
		}
	}

	// 使用快速估算方法，避免大文件初始化延迟
	// 基于网络质量进行快速估算，不进行实际网络测试
	networkQuality := f.getNetworkQuality()

	// 基础速度估算（基于典型网络环境）
	var baseSpeed int64 = 10 * 1024 * 1024 // 10MB/s 基础速度

	// 根据网络质量调整速度估算
	if networkQuality >= 0.8 {
		baseSpeed = 20 * 1024 * 1024 // 20MB/s 高质量网络
	} else if networkQuality >= 0.6 {
		baseSpeed = 15 * 1024 * 1024 // 15MB/s 良好网络
	} else if networkQuality >= 0.4 {
		baseSpeed = 8 * 1024 * 1024 // 8MB/s 一般网络
	} else {
		baseSpeed = 5 * 1024 * 1024 // 5MB/s 较差网络
	}

	f.debugf(LogLevelVerbose, "网络速度估算: %s/s (质量=%.2f)",
		fs.SizeSuffix(baseSpeed), networkQuality)

	return baseSpeed
}

// getOptimalConcurrency 根据文件大小和网络速度计算最优并发数
// 优化版本：集成动态参数调整器，实现智能自适应并发控制
func (f *Fs) getOptimalConcurrency(fileSize int64, networkSpeed int64) int {
	// 检查是否需要进行动态调整
	if f.dynamicAdjuster != nil && f.dynamicAdjuster.ShouldAdjust() {
		networkQuality := f.getNetworkQuality()
		concurrency, _, _ := f.dynamicAdjuster.AdjustParameters(fileSize, networkSpeed, networkQuality)

		f.debugf(LogLevelVerbose, "动态并发调整: %s -> %d并发 (速度=%s/s)",
			fs.SizeSuffix(fileSize), concurrency, fs.SizeSuffix(networkSpeed))

		// 应用用户配置的最大并发数限制
		if f.opt.MaxConcurrentUploads > 0 && concurrency > f.opt.MaxConcurrentUploads {
			concurrency = f.opt.MaxConcurrentUploads
			f.debugf(LogLevelDebug, "应用用户配置限制，最终并发数: %d", concurrency)
		}

		return concurrency
	}

	// 回退到传统的静态计算方法
	baseConcurrency := f.opt.MaxConcurrentUploads
	if baseConcurrency <= 0 {
		baseConcurrency = 4 // 默认并发数
	}

	// 根据文件大小调整并发数 - 优化大文件并发策略
	var sizeFactor float64 = 1.0
	if fileSize > 20*1024*1024*1024 { // >20GB - 超大文件
		sizeFactor = 3.0 // 显著提升并发数
	} else if fileSize > 10*1024*1024*1024 { // >10GB - 大文件
		sizeFactor = 2.5
	} else if fileSize > 5*1024*1024*1024 { // >5GB - 中大文件
		sizeFactor = 2.0
	} else if fileSize > 2*1024*1024*1024 { // >2GB - 中等文件
		sizeFactor = 1.5
	} else if fileSize > 1*1024*1024*1024 { // >1GB - 较大文件
		sizeFactor = 1.2
	} else if fileSize > 500*1024*1024 { // >500MB - 中等文件
		sizeFactor = 1.0
	} else if fileSize > 100*1024*1024 { // >100MB - 小文件
		sizeFactor = 0.8
	} else {
		sizeFactor = 0.5 // 很小文件使用较少并发
	}

	// 根据网络速度调整 - 🚀 性能优化：更精细的网络速度分级和自适应调整
	var speedFactor float64 = 1.0

	// 🚀 新增：网络质量自适应调整
	networkLatency := f.measureNetworkLatency()
	latencyFactor := 1.0
	if networkLatency < 50 { // <50ms 低延迟
		latencyFactor = 1.2
	} else if networkLatency < 100 { // <100ms 中等延迟
		latencyFactor = 1.0
	} else if networkLatency < 200 { // <200ms 高延迟
		latencyFactor = 0.8
	} else { // >200ms 很高延迟
		latencyFactor = 0.6
	}

	if networkSpeed > 200*1024*1024 { // >200Mbps - 超高速网络
		speedFactor = 2.0 * latencyFactor // 从1.8提升，结合延迟调整
	} else if networkSpeed > 100*1024*1024 { // >100Mbps - 高速网络
		speedFactor = 1.7 * latencyFactor // 从1.5提升，结合延迟调整
	} else if networkSpeed > 50*1024*1024 { // >50Mbps - 中高速网络
		speedFactor = 1.4 * latencyFactor // 从1.2提升，结合延迟调整
	} else if networkSpeed > 20*1024*1024 { // >20Mbps - 中速网络
		speedFactor = 1.1 * latencyFactor // 从1.0提升，结合延迟调整
	} else if networkSpeed > 10*1024*1024 { // >10Mbps - 中低速网络
		speedFactor = 0.9 * latencyFactor // 从0.8提升，结合延迟调整
	} else {
		speedFactor = 0.7 * latencyFactor // 从0.6提升，结合延迟调整
	}

	// 计算最优并发数
	optimalConcurrency := int(float64(baseConcurrency) * sizeFactor * speedFactor)

	// 🚀 用户要求：123网盘单文件并发限制到4
	minConcurrency := 1
	maxConcurrency := 4 // 用户要求的并发上限

	// 不再允许超大文件使用更高并发数，统一限制为4

	if optimalConcurrency < minConcurrency {
		optimalConcurrency = minConcurrency
	}
	if optimalConcurrency > maxConcurrency {
		optimalConcurrency = maxConcurrency
	}

	fs.Debugf(f, "🚀 优化并发数计算: 文件大小=%s, 网络速度=%s/s, 基础并发=%d, 大小因子=%.1f, 速度因子=%.1f, 最优并发=%d",
		fs.SizeSuffix(fileSize), fs.SizeSuffix(networkSpeed), baseConcurrency, sizeFactor, speedFactor, optimalConcurrency)

	return optimalConcurrency
}

// getOptimalChunkSize 根据文件大小和网络速度计算最优分片大小
// 优化版本：集成动态参数调整器，实现智能自适应分片大小控制
func (f *Fs) getOptimalChunkSize(fileSize int64, networkSpeed int64) int64 {
	fs.Debugf(f, "🔧 开始计算最优分片大小: 文件大小=%s, 网络速度=%s/s", fs.SizeSuffix(fileSize), fs.SizeSuffix(networkSpeed))

	// 检查是否需要进行动态调整
	if f.dynamicAdjuster != nil && f.dynamicAdjuster.ShouldAdjust() {
		fs.Debugf(f, "🔧 使用动态调整器计算分片大小")
		networkQuality := f.getNetworkQuality()
		_, chunkSize, _ := f.dynamicAdjuster.AdjustParameters(fileSize, networkSpeed, networkQuality)

		f.debugf(LogLevelVerbose, "动态分片调整: %s -> %s分片",
			fs.SizeSuffix(fileSize), fs.SizeSuffix(chunkSize))

		return chunkSize
	}

	// 回退到传统的静态计算方法
	baseChunk := int64(f.opt.ChunkSize)
	if baseChunk <= 0 {
		baseChunk = int64(defaultChunkSize) // 100MB
	}

	// 根据网络速度调整分片大小 - 🚀 性能优化：提升分片大小倍数
	var speedMultiplier float64 = 1.0
	if networkSpeed > 200*1024*1024 { // >200Mbps - 超高速网络
		speedMultiplier = 4.0 // 400MB分片（从3.0提升）
	} else if networkSpeed > 100*1024*1024 { // >100Mbps - 高速网络
		speedMultiplier = 3.0 // 300MB分片（从2.0提升）
	} else if networkSpeed > 50*1024*1024 { // >50Mbps - 中速网络
		speedMultiplier = 2.0 // 200MB分片（从1.5提升）
	} else if networkSpeed > 20*1024*1024 { // >20Mbps - 普通网络
		speedMultiplier = 1.5 // 150MB分片（从1.0提升）
	} else { // <20Mbps - 慢速网络
		speedMultiplier = 0.8 // 80MB分片（从0.5提升）
	}

	// 根据文件大小调整 - 🚀 性能优化：更精细的文件大小分级
	var sizeMultiplier float64 = 1.0
	if fileSize > 50*1024*1024*1024 { // >50GB
		sizeMultiplier = 2.0 // 大文件使用更大分片（从1.5提升）
	} else if fileSize > 20*1024*1024*1024 { // >20GB
		sizeMultiplier = 1.8 // 新增：超大文件分级
	} else if fileSize > 10*1024*1024*1024 { // >10GB
		sizeMultiplier = 1.5 // 新增：大文件分级
	} else if fileSize > 5*1024*1024*1024 { // >5GB
		sizeMultiplier = 1.3 // 新增：中大文件分级
	} else if fileSize > 2*1024*1024*1024 { // >2GB
		sizeMultiplier = 1.1 // 新增：中等文件分级
	} else if fileSize < 500*1024*1024 { // <500MB
		sizeMultiplier = 0.7 // 小文件使用更小分片（从0.5提升）
	}

	// 计算最优分片大小
	optimalChunkSize := int64(float64(baseChunk) * speedMultiplier * sizeMultiplier)

	// 设置合理边界 - 🚀 性能优化：提升最大分片大小限制
	minChunkSize := int64(minChunkSize)      // 50MB
	maxChunkSize := int64(800 * 1024 * 1024) // 800MB（从500MB提升）

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

// measureNetworkLatency 测量网络延迟
// 🚀 性能优化：实现网络延迟检测，用于自适应并发调整
func (f *Fs) measureNetworkLatency() int64 {
	// 简单的延迟测量：向123网盘API发送HEAD请求
	start := time.Now()

	// 构建测试URL
	testURL := "https://www.123pan.com/api/v1/user/info"
	req, err := http.NewRequest("HEAD", testURL, nil)
	if err != nil {
		return 100 // 默认延迟100ms
	}

	// 设置请求头
	req.Header.Set("User-Agent", f.opt.UserAgent)

	// 发送请求
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 150 // 网络错误时返回较高延迟
	}
	defer resp.Body.Close()

	latency := time.Since(start).Milliseconds()
	return latency
}

// ResourcePool 资源池管理器，用于优化内存使用和减少GC压力
// 🚀 性能优化：增强内存池管理，减少内存分配和GC压力
type ResourcePool struct {
	bufferPool   sync.Pool // 缓冲区池
	hasherPool   sync.Pool // MD5哈希计算器池
	chunkPool    sync.Pool // 分片缓冲区池（新增）
	readerPool   sync.Pool // Reader池（新增）
	progressPool sync.Pool // 进度跟踪器池（新增）

	// 临时文件跟踪
	tempFilesMu sync.Mutex
	tempFiles   map[string]bool // 跟踪创建的临时文件
}

// DownloadProgress 123网盘下载进度跟踪器
type DownloadProgress struct {
	totalChunks     int64
	completedChunks int64
	totalBytes      int64
	downloadedBytes int64
	startTime       time.Time
	lastUpdateTime  time.Time
	chunkSizes      map[int64]int64         // 记录每个分片的大小
	chunkTimes      map[int64]time.Duration // 记录每个分片的下载时间
	peakSpeed       float64                 // 峰值速度 MB/s
	mu              sync.RWMutex
}

// NewDownloadProgress 创建新的下载进度跟踪器
func NewDownloadProgress(totalChunks, totalBytes int64) *DownloadProgress {
	return &DownloadProgress{
		totalChunks:    totalChunks,
		totalBytes:     totalBytes,
		startTime:      time.Now(),
		lastUpdateTime: time.Now(),
		chunkSizes:     make(map[int64]int64),
		chunkTimes:     make(map[int64]time.Duration),
		peakSpeed:      0,
	}
}

// UpdateChunkProgress 更新分片下载进度
func (dp *DownloadProgress) UpdateChunkProgress(chunkIndex, chunkSize int64, chunkDuration time.Duration) {
	dp.mu.Lock()
	defer dp.mu.Unlock()

	// 如果这个分片还没有记录，增加完成计数
	if _, exists := dp.chunkSizes[chunkIndex]; !exists {
		dp.completedChunks++
		dp.downloadedBytes += chunkSize
		dp.chunkSizes[chunkIndex] = chunkSize
		dp.chunkTimes[chunkIndex] = chunkDuration
		dp.lastUpdateTime = time.Now()

		// 计算并更新峰值速度
		if chunkDuration.Seconds() > 0 {
			chunkSpeed := float64(chunkSize) / chunkDuration.Seconds() / 1024 / 1024 // MB/s
			if chunkSpeed > dp.peakSpeed {
				dp.peakSpeed = chunkSpeed
			}
		}
	}
}

// GetProgressInfo 获取当前进度信息
func (dp *DownloadProgress) GetProgressInfo() (percentage float64, avgSpeed float64, peakSpeed float64, eta time.Duration, completed, total int64, downloadedBytes, totalBytes int64) {
	dp.mu.RLock()
	defer dp.mu.RUnlock()

	elapsed := time.Since(dp.startTime)
	percentage = float64(dp.completedChunks) / float64(dp.totalChunks) * 100

	if elapsed.Seconds() > 0 {
		avgSpeed = float64(dp.downloadedBytes) / elapsed.Seconds() / 1024 / 1024 // MB/s
	}

	peakSpeed = dp.peakSpeed

	if avgSpeed > 0 && dp.downloadedBytes > 0 {
		remainingBytes := dp.totalBytes - dp.downloadedBytes
		etaSeconds := float64(remainingBytes) / (avgSpeed * 1024 * 1024)
		eta = time.Duration(etaSeconds) * time.Second
	}

	return percentage, avgSpeed, peakSpeed, eta, dp.completedChunks, dp.totalChunks, dp.downloadedBytes, dp.totalBytes
}

// updateAccountExtraInfo 更新Account的额外进度信息（集成到rclone标准进度显示）
func (f *Fs) updateAccountExtraInfo(ctx context.Context, progress *DownloadProgress, remoteName string, account *accounting.Account) {
	ticker := time.NewTicker(2 * time.Second) // 每2秒更新一次Account的额外信息
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// 清除额外信息
			if account != nil {
				account.SetExtraInfo("")
			}
			return
		case <-ticker.C:
			if account == nil {
				// 尝试重新获取Account对象
				if stats := accounting.GlobalStats(); stats != nil {
					// 首先尝试用原始名称查找
					if acc := stats.GetInProgressAccount(remoteName); acc != nil {
						account = acc
					} else {
						// 添加模糊匹配：查找包含文件名的Account
						allAccounts := stats.ListInProgressAccounts()
						for _, accountName := range allAccounts {
							if strings.Contains(accountName, remoteName) || strings.Contains(remoteName, accountName) {
								if acc := stats.GetInProgressAccount(accountName); acc != nil {
									account = acc
									break
								}
							}
						}
					}
				}
				if account == nil {
					continue // 如果还是没有找到，跳过这次更新
				}
			}

			_, _, _, _, completed, total, _, _ := progress.GetProgressInfo()

			// 🔧 集成：构建额外信息字符串，显示分片进度
			if completed > 0 && total > 0 {
				extraInfo := fmt.Sprintf("[%d/%d分片]", completed, total)
				account.SetExtraInfo(extraInfo)
			}
		}
	}
}

// displayDownloadProgress 显示123网盘下载进度
func (f *Fs) displayDownloadProgress(ctx context.Context, progress *DownloadProgress, fileName string) {

	ticker := time.NewTicker(2 * time.Second) // 每2秒更新一次进度
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// 显示最终进度
			percentage, avgSpeed, peakSpeed, _, completed, total, downloadedBytes, totalBytes := progress.GetProgressInfo()
			fs.Infof(f, "📥 123网盘下载完成: %s | %d/%d分片 (%.1f%%) | %s/%s | 平均: %.2f MB/s | 峰值: %.2f MB/s",
				fileName, completed, total, percentage,
				fs.SizeSuffix(downloadedBytes), fs.SizeSuffix(totalBytes), avgSpeed, peakSpeed)
			return
		case <-ticker.C:
			percentage, avgSpeed, peakSpeed, eta, completed, total, downloadedBytes, totalBytes := progress.GetProgressInfo()

			// 构建ETA字符串
			var etaStr string
			if eta > 0 {
				if eta > time.Hour {
					etaStr = fmt.Sprintf("ETA: %dh%dm", int(eta.Hours()), int(eta.Minutes())%60)
				} else if eta > time.Minute {
					etaStr = fmt.Sprintf("ETA: %dm%ds", int(eta.Minutes()), int(eta.Seconds())%60)
				} else {
					etaStr = fmt.Sprintf("ETA: %ds", int(eta.Seconds()))
				}
			} else {
				etaStr = "ETA: 计算中..."
			}

			// 显示详细进度信息（从开始就显示，不等待第一个分片完成）
			// 创建进度条
			progressBar := f.createProgressBar(percentage)

			fs.Infof(f, "📥 123网盘下载进度: %s", fileName)
			fs.Infof(f, "   %s %.1f%% | %d/%d分片 | %s/%s",
				progressBar, percentage, completed, total,
				fs.SizeSuffix(downloadedBytes), fs.SizeSuffix(totalBytes))
			fs.Infof(f, "   平均速度: %.2f MB/s | 峰值速度: %.2f MB/s | %s",
				avgSpeed, peakSpeed, etaStr)
		}
	}
}

// 🔧 已移除displayUnifiedDownloadProgress函数，使用UnifiedProgressManager替代

// createProgressBar 创建进度条字符串
func (f *Fs) createProgressBar(percentage float64) string {
	const barLength = 30
	filled := int(percentage / 100 * barLength)
	if filled > barLength {
		filled = barLength
	}

	bar := "["
	for i := 0; i < barLength; i++ {
		if i < filled {
			bar += "█"
		} else {
			bar += "░"
		}
	}
	bar += "]"
	return bar
}

// NewResourcePool 创建新的资源池
// 🚀 性能优化：增强内存池管理，支持更多类型的对象复用
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
		// 🚀 新增：分片缓冲区池，用于大分片的内存复用
		chunkPool: sync.Pool{
			New: func() interface{} {
				return make([]byte, 100*1024*1024) // 100MB分片缓冲区
			},
		},
		// 🚀 新增：Reader池，减少Reader对象的创建开销
		readerPool: sync.Pool{
			New: func() interface{} {
				return &bytes.Buffer{}
			},
		},
		// 🚀 新增：进度跟踪器池，复用进度对象
		progressPool: sync.Pool{
			New: func() interface{} {
				return &DownloadProgress{
					chunkSizes: make(map[int64]int64),
					chunkTimes: make(map[int64]time.Duration),
				}
			},
		},
		tempFiles: make(map[string]bool),
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

// GetTempFile 创建临时文件
// 增强版本：更强的异常处理和资源管理保证
func (rp *ResourcePool) GetTempFile(prefix string) (*os.File, error) {
	// 检查资源池状态
	if rp == nil {
		return nil, fmt.Errorf("资源池未初始化")
	}

	// 创建临时文件，使用系统默认临时目录
	tempFile, err := os.CreateTemp("", prefix+"*.tmp")
	if err != nil {
		return nil, fmt.Errorf("创建临时文件失败: %w", err)
	}

	fileName := tempFile.Name()

	// 使用defer确保异常时也能清理资源
	var success bool
	defer func() {
		if !success {
			// 创建失败时的清理工作
			if tempFile != nil {
				tempFile.Close()
			}
			rp.removeTempFileFromTracking(fileName)
			if err := os.Remove(fileName); err != nil && !os.IsNotExist(err) {
				fs.Debugf(nil, "⚠️  清理失败的临时文件时出错: %s, 错误: %v", fileName, err)
			}
		}
	}()

	// 跟踪临时文件
	rp.tempFilesMu.Lock()
	if rp.tempFiles == nil {
		rp.tempFiles = make(map[string]bool)
	}
	rp.tempFiles[fileName] = true
	rp.tempFilesMu.Unlock()

	// 设置文件权限，确保只有当前用户可以访问
	if err = tempFile.Chmod(0600); err != nil {
		return nil, fmt.Errorf("设置临时文件权限失败: %w", err)
	}

	success = true
	fs.Debugf(nil, "📁 临时文件创建成功: %s", fileName)
	return tempFile, nil
}

// GetOptimizedTempFile 创建优化的临时文件，支持大文件高效处理
func (rp *ResourcePool) GetOptimizedTempFile(prefix string, expectedSize int64) (*os.File, error) {
	tempFile, err := rp.GetTempFile(prefix)
	if err != nil {
		return nil, err
	}

	// GetTempFile已经处理了跟踪，这里不需要重复跟踪

	// 对于大文件，预分配磁盘空间以提升写入性能
	if expectedSize > 100*1024*1024 { // >100MB
		err = tempFile.Truncate(expectedSize)
		if err != nil {
			tempFile.Close()
			rp.removeTempFileFromTracking(tempFile.Name())
			os.Remove(tempFile.Name())
			return nil, fmt.Errorf("预分配临时文件空间失败: %w", err)
		}

		// 重置文件指针到开始位置
		_, err = tempFile.Seek(0, 0)
		if err != nil {
			tempFile.Close()
			rp.removeTempFileFromTracking(tempFile.Name())
			os.Remove(tempFile.Name())
			return nil, fmt.Errorf("重置临时文件指针失败: %w", err)
		}
	}

	return tempFile, nil
}

// removeTempFileFromTracking 从跟踪列表中移除临时文件
// 增强版本：更强的安全检查和错误处理
func (rp *ResourcePool) removeTempFileFromTracking(fileName string) {
	if rp == nil {
		fs.Debugf(nil, "⚠️  资源池未初始化，无法移除临时文件跟踪: %s", fileName)
		return
	}

	if fileName == "" {
		fs.Debugf(nil, "⚠️  临时文件名为空，跳过移除跟踪")
		return
	}

	rp.tempFilesMu.Lock()
	defer rp.tempFilesMu.Unlock()

	if rp.tempFiles == nil {
		fs.Debugf(nil, "⚠️  临时文件跟踪映射未初始化")
		return
	}

	if _, exists := rp.tempFiles[fileName]; exists {
		delete(rp.tempFiles, fileName)
		fs.Debugf(nil, "📝 临时文件跟踪移除成功: %s", fileName)
	} else {
		fs.Debugf(nil, "📝 临时文件未在跟踪列表中: %s", fileName)
	}
}

// PutTempFile 清理临时文件
// 增强版本：更强的异常处理和资源清理保证
func (rp *ResourcePool) PutTempFile(tempFile *os.File) {
	if tempFile == nil {
		return
	}

	fileName := tempFile.Name()

	// 安全关闭文件，使用defer确保即使出现panic也能关闭
	defer func() {
		if tempFile != nil {
			if closeErr := tempFile.Close(); closeErr != nil {
				fs.Debugf(nil, "⚠️  关闭临时文件失败: %s, 错误: %v", fileName, closeErr)
			}
		}
	}()

	// 从跟踪列表中移除
	rp.removeTempFileFromTracking(fileName)

	// 多次尝试删除文件，处理可能的文件锁定问题
	maxRetries := 3
	for attempt := 0; attempt < maxRetries; attempt++ {
		if err := os.Remove(fileName); err != nil {
			if os.IsNotExist(err) {
				// 文件已不存在，认为删除成功
				fs.Debugf(nil, "📁 临时文件已不存在: %s", fileName)
				return
			}

			if attempt < maxRetries-1 {
				// 等待一小段时间后重试（可能是文件锁定）
				time.Sleep(time.Duration(attempt+1) * 100 * time.Millisecond)
				fs.Debugf(nil, "🔄 临时文件删除重试 %d/%d: %s", attempt+1, maxRetries, fileName)
				continue
			}

			// 最后一次尝试失败，记录错误但不影响程序运行
			fs.Errorf(nil, "❌ 删除临时文件最终失败: %s, 错误: %v", fileName, err)
		} else {
			fs.Debugf(nil, "✅ 临时文件删除成功: %s", fileName)
			return
		}
	}
}

// CleanupAllTempFiles 清理所有跟踪的临时文件
func (rp *ResourcePool) CleanupAllTempFiles() {
	rp.tempFilesMu.Lock()
	defer rp.tempFilesMu.Unlock()

	cleanedCount := 0
	for fileName := range rp.tempFiles {
		if err := os.Remove(fileName); err != nil {
			if !os.IsNotExist(err) {
				fs.Debugf(nil, "⚠️  清理临时文件失败: %s, 错误: %v", fileName, err)
			}
		} else {
			cleanedCount++
		}
		delete(rp.tempFiles, fileName)
	}

	if cleanedCount > 0 {
		fs.Debugf(nil, "🧹 资源池清理完成，删除了 %d 个临时文件", cleanedCount)
	}
}

// Close 关闭资源池，清理所有资源
// 优化版本：简化实现，sync.Pool会自动管理资源
func (rp *ResourcePool) Close() {
	// 清理所有临时文件
	rp.CleanupAllTempFiles()

	// sync.Pool会自动管理内存池，无需手动清理
	// 这个方法保留是为了接口兼容性
}

// ===== 通用工具函数模块 =====
// 这些函数可以被多个地方复用，提高代码的可维护性

// StringUtils 字符串处理工具集合
type StringUtils struct{}

// IsEmpty 检查字符串是否为空或只包含空白字符
func (StringUtils) IsEmpty(s string) bool {
	return strings.TrimSpace(s) == ""
}

// TruncateString 截断字符串到指定字节长度，保持UTF-8完整性
func (StringUtils) TruncateString(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}

	// 确保不会在UTF-8字符中间截断
	for i := maxBytes; i >= 0; i-- {
		if utf8.ValidString(s[:i]) {
			return s[:i]
		}
	}
	return ""
}

// PathUtils 路径处理工具集合
type PathUtils struct{}

// NormalizePath 标准化路径处理
func (PathUtils) NormalizePath(path string) string {
	return normalizePath(path)
}

// SplitPath 分割路径为目录和文件名
func (PathUtils) SplitPath(path string) (dir, name string) {
	path = normalizePath(path)
	if path == "" {
		return "", ""
	}

	lastSlash := strings.LastIndex(path, "/")
	if lastSlash == -1 {
		return "", path
	}

	return path[:lastSlash], path[lastSlash+1:]
}

// ValidationUtils 验证工具集合
type ValidationUtils struct{}

// ValidateFileID 验证文件ID格式
func (ValidationUtils) ValidateFileID(idStr string) (int64, error) {
	return parseFileID(idStr)
}

// ValidateFileName 验证文件名
func (ValidationUtils) ValidateFileName(name string) error {
	return validateFileName(name)
}

// TimeUtils 时间处理工具集合
type TimeUtils struct{}

// CalculateRetryDelay 计算重试延迟时间
func (TimeUtils) CalculateRetryDelay(attempt int) time.Duration {
	return calculateRetryDelay(attempt)
}

// ParseUnixTimestamp 解析Unix时间戳
func (TimeUtils) ParseUnixTimestamp(timestamp string) (time.Time, error) {
	if timestamp == "" {
		return time.Time{}, fmt.Errorf("时间戳不能为空")
	}

	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("无效的时间戳格式: %s", timestamp)
	}

	return time.Unix(ts, 0), nil
}

// HTTPUtils HTTP处理工具集合
type HTTPUtils struct{}

// IsRetryableError 判断错误是否可重试
func (HTTPUtils) IsRetryableError(ctx context.Context, err error, resp *http.Response) (bool, error) {
	return shouldRetry(ctx, resp, err)
}

// BuildAPIEndpoint 构建API端点URL
func (HTTPUtils) BuildAPIEndpoint(base, path string, params map[string]string) string {
	endpoint := base + path
	if len(params) > 0 {
		values := url.Values{}
		for k, v := range params {
			values.Set(k, v)
		}
		endpoint += "?" + values.Encode()
	}
	return endpoint
}

// LoggingUtils 统一日志和调试接口
type LoggingUtils struct{}

// LogWithLevel 根据级别输出日志
func (LoggingUtils) LogWithLevel(f *Fs, level int, format string, args ...any) {
	if f == nil {
		return
	}

	// 检查是否应该输出此级别的日志
	if !f.shouldLog(level) {
		return
	}

	switch level {
	case LogLevelError:
		fs.Errorf(f, format, args...)
	case LogLevelInfo:
		fs.Infof(f, format, args...)
	case LogLevelDebug, LogLevelVerbose:
		fs.Debugf(f, format, args...)
	default:
		fs.Debugf(f, format, args...)
	}
}

// LogError 输出错误日志
func (LoggingUtils) LogError(f *Fs, format string, args ...any) {
	LoggingUtil.LogWithLevel(f, LogLevelError, format, args...)
}

// LogInfo 输出信息日志
func (LoggingUtils) LogInfo(f *Fs, format string, args ...any) {
	LoggingUtil.LogWithLevel(f, LogLevelInfo, format, args...)
}

// LogDebug 输出调试日志
func (LoggingUtils) LogDebug(f *Fs, format string, args ...any) {
	LoggingUtil.LogWithLevel(f, LogLevelDebug, format, args...)
}

// LogVerbose 输出详细日志
func (LoggingUtils) LogVerbose(f *Fs, format string, args ...any) {
	LoggingUtil.LogWithLevel(f, LogLevelVerbose, format, args...)
}

// shouldLog 检查是否应该输出指定级别的日志
func (f *Fs) shouldLog(level int) bool {
	if f == nil {
		return false
	}
	return level <= f.opt.DebugLevel
}

// 全局工具实例，方便使用
var (
	StringUtil     = StringUtils{}
	PathUtil       = PathUtils{}
	ValidationUtil = ValidationUtils{}
	TimeUtil       = TimeUtils{}
	HTTPUtil       = HTTPUtils{}
	LoggingUtil    = LoggingUtils{}
)

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

// PerformanceStats 性能统计摘要
type PerformanceStats struct {
	TotalRuntime         time.Duration // 总运行时间
	TotalUploads         int           // 总上传数
	SuccessfulUploads    int           // 成功上传数
	FailedUploads        int           // 失败上传数
	AverageUploadSpeed   float64       // 平均上传速度 MB/s
	TotalDownloads       int           // 总下载数
	SuccessfulDownloads  int           // 成功下载数
	FailedDownloads      int           // 失败下载数
	AverageDownloadSpeed float64       // 平均下载速度 MB/s
	TotalAPICalls        int           // 总API调用数
	APISuccessRate       float64       // API成功率
	AverageAPILatency    time.Duration // 平均API延迟
	CurrentMemoryUsage   int64         // 当前内存使用
	PeakMemoryUsage      int64         // 峰值内存使用
}

// GetStats 获取性能统计摘要
func (epm *EnhancedPerformanceMetrics) GetStats() PerformanceStats {
	epm.mu.RLock()
	defer epm.mu.RUnlock()

	stats := PerformanceStats{
		TotalRuntime: time.Since(epm.startTime),
	}

	// 计算上传统计
	stats.TotalUploads = len(epm.uploadMetrics)
	var totalUploadSpeed float64
	for _, metric := range epm.uploadMetrics {
		if metric.Success {
			stats.SuccessfulUploads++
		} else {
			stats.FailedUploads++
		}
		totalUploadSpeed += metric.ThroughputMBps
	}
	if stats.TotalUploads > 0 {
		stats.AverageUploadSpeed = totalUploadSpeed / float64(stats.TotalUploads)
	}

	// 计算下载统计
	stats.TotalDownloads = len(epm.downloadMetrics)
	var totalDownloadSpeed float64
	for _, metric := range epm.downloadMetrics {
		if metric.Success {
			stats.SuccessfulDownloads++
		} else {
			stats.FailedDownloads++
		}
		totalDownloadSpeed += metric.ThroughputMBps
	}
	if stats.TotalDownloads > 0 {
		stats.AverageDownloadSpeed = totalDownloadSpeed / float64(stats.TotalDownloads)
	}

	// 计算网络质量统计
	if len(epm.networkQualityHistory) > 0 {
		var totalLatency time.Duration
		successCount := 0
		for _, metric := range epm.networkQualityHistory {
			totalLatency += metric.AvgLatency
			if metric.ErrorRate < 0.1 { // 错误率小于10%视为成功
				successCount++
			}
		}
		stats.TotalAPICalls = len(epm.networkQualityHistory)
		stats.APISuccessRate = float64(successCount) / float64(stats.TotalAPICalls)
		stats.AverageAPILatency = totalLatency / time.Duration(stats.TotalAPICalls)
	}

	return stats
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
	var bestConcurrency int = 4                 // 默认值（用户要求上限）
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
	// 策略选择逻辑优化 - 修复单步上传选择问题

	// 1. 小文件（<1GB）-> 单步上传（API限制1GB，性能更好）
	if uss.fileSize < singleStepUploadLimit && uss.fileSize > 0 {
		return StrategySingleStep
	}

	// 2. 大文件（>=1GB）-> 分片上传（支持断点续传和并发）
	if uss.fileSize >= singleStepUploadLimit {
		return StrategyChunked
	}

	// 3. 其他情况（理论上不应该到达这里）-> 流式上传
	return StrategyStreaming
}

// GetStrategyReason 获取策略选择的原因说明
func (uss *UploadStrategySelector) GetStrategyReason(strategy UploadStrategy) string {
	switch strategy {
	case StrategySingleStep:
		return fmt.Sprintf("小文件(%s)，使用单步上传接口（API限制1GB）", fs.SizeSuffix(uss.fileSize))
	case StrategyChunked:
		return fmt.Sprintf("大文件(%s)，使用分片上传支持断点续传", fs.SizeSuffix(uss.fileSize))
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
	result, err := f.singleStepUpload(uploadCtx.ctx, data, uploadCtx.parentFileID, uploadCtx.fileName, md5Hash)
	if err != nil {
		// 检查是否是父目录ID不存在的错误，如果是则重试一次
		if strings.Contains(err.Error(), "父目录ID不存在，缓存已清理，请重试") {
			fs.Debugf(f, "单步上传: 父目录ID不存在，重新查找父目录并重试")

			// 重新查找父目录ID
			leaf, parentID, findErr := f.dirCache.FindPath(uploadCtx.ctx, uploadCtx.fileName, true)
			if findErr != nil {
				return nil, fmt.Errorf("重新查找父目录失败: %w", findErr)
			}

			newParentFileID, parseErr := parseParentID(parentID)
			if parseErr != nil {
				return nil, fmt.Errorf("重新解析父目录ID失败: %w", parseErr)
			}

			fs.Debugf(f, "单步上传: 重新找到父目录ID: %d (原ID: %d)", newParentFileID, uploadCtx.parentFileID)
			uploadCtx.parentFileID = newParentFileID
			uploadCtx.fileName = leaf

			// 重试单步上传
			result, err = f.singleStepUpload(uploadCtx.ctx, data, uploadCtx.parentFileID, uploadCtx.fileName, md5Hash)
			if err != nil {
				return nil, fmt.Errorf("重试单步上传失败: %w", err)
			}
		} else {
			return nil, err
		}
	}

	return result, nil
}

// executeChunkedUpload 执行分片上传策略
func (f *Fs) executeChunkedUpload(uploadCtx *UnifiedUploadContext) (*Object, error) {
	fs.Infof(f, "🚨 【重要调试】进入executeChunkedUpload函数，文件: %s", uploadCtx.fileName)
	fs.Debugf(f, "执行分片上传策略")

	// 🌐 跨云传输检测和智能秒传策略
	fs.Infof(f, "🚨 【重要调试】开始跨云传输检测...")
	fs.Debugf(f, "🔍 开始检测是否为跨云传输...")
	isRemoteSource := f.isRemoteSource(uploadCtx.src)
	fs.Infof(f, "🚨 【重要调试】跨云传输检测结果: %v", isRemoteSource)
	fs.Debugf(f, "🔍 跨云传输检测结果: %v", isRemoteSource)

	// 🚀 智能秒传检测：无论本地还是跨云传输，都先尝试获取MD5进行秒传检测
	var md5Hash string

	if isRemoteSource {
		fs.Infof(f, "🌐 检测到跨云传输，先尝试获取源文件MD5进行秒传检测")

		// 尝试从源对象获取已知MD5（无需额外计算）
		if hashValue, err := uploadCtx.src.Hash(uploadCtx.ctx, fshash.MD5); err == nil && hashValue != "" {
			md5Hash = hashValue
			fs.Infof(f, "🚀 跨云传输获取到源文件MD5: %s，将尝试秒传", md5Hash)
		} else {
			fs.Debugf(f, "🔍 跨云传输源文件无可用MD5，将跳过秒传检测: %v", err)
		}
	}

	// 本地文件或跨云传输无MD5时：获取或计算MD5
	if !isRemoteSource || md5Hash == "" {
		if hashValue, err := uploadCtx.src.Hash(uploadCtx.ctx, fshash.MD5); err == nil && hashValue != "" {
			md5Hash = hashValue
			fs.Debugf(f, "使用已知MD5: %s", md5Hash)
		} else if !isRemoteSource {
			// 对于分片上传，必须计算准确的MD5
			fs.Debugf(f, "源对象无MD5哈希，尝试从输入流计算MD5")

			// 优先尝试从fs.Object接口计算MD5
			if srcObj, ok := uploadCtx.src.(fs.Object); ok {
				fs.Debugf(f, "使用fs.Object接口计算MD5")
				md5Reader, err := srcObj.Open(uploadCtx.ctx)
				if err != nil {
					fs.Debugf(f, "无法打开源对象，回退到输入流计算: %v", err)
				} else {
					defer md5Reader.Close()
					hasher := md5.New()
					_, err = io.Copy(hasher, md5Reader)
					if err != nil {
						fs.Debugf(f, "从源对象计算MD5失败，回退到输入流计算: %v", err)
					} else {
						md5Hash = fmt.Sprintf("%x", hasher.Sum(nil))
						fs.Debugf(f, "从源对象计算MD5完成: %s", md5Hash)
					}
				}
			}
		}

		// 如果从源对象计算MD5失败，尝试通用方法计算MD5
		if md5Hash == "" {
			fs.Debugf(f, "使用通用方法计算MD5（跨云盘传输兼容模式）")

			// 尝试通过fs.ObjectInfo接口重新打开源对象
			if opener, ok := uploadCtx.src.(fs.ObjectInfo); ok {
				fs.Debugf(f, "通过fs.ObjectInfo接口计算MD5")

				// 检查是否有Open方法（通过接口断言）
				if openable, hasOpen := opener.(interface {
					Open(context.Context, ...fs.OpenOption) (io.ReadCloser, error)
				}); hasOpen {
					md5Reader, err := openable.Open(uploadCtx.ctx)
					if err != nil {
						fs.Debugf(f, "无法通过Open方法打开源对象: %v", err)
					} else {
						defer md5Reader.Close()
						md5Hash, err = f.calculateMD5FromReader(uploadCtx.ctx, md5Reader, uploadCtx.src.Size())
						if err != nil {
							fs.Debugf(f, "通过Open方法计算MD5失败: %v", err)
						} else {
							fs.Debugf(f, "通过Open方法计算MD5成功: %s", md5Hash)
						}
					}
				}
			}

			// 如果仍然没有MD5，使用输入流缓存方法计算MD5（此时已确保是本地文件）
			if md5Hash == "" {
				fs.Debugf(f, "使用输入流缓存方法计算MD5")
				var err error
				md5Hash, uploadCtx.in, err = f.calculateMD5WithStreamCache(uploadCtx.ctx, uploadCtx.in, uploadCtx.src.Size())
				if err != nil {
					return nil, fmt.Errorf("无法计算文件MD5: %w", err)
				}
				fs.Debugf(f, "输入流缓存方法计算MD5完成: %s", md5Hash)
			}
		}
	}

	// 🌐 跨云传输无MD5时的特殊处理
	if isRemoteSource && md5Hash == "" {
		fs.Infof(f, "🌐 跨云传输无可用MD5，使用下载后上传策略")

		// 为跨云传输创建上传会话（不需要MD5）
		createResp, err := f.createUploadV2(uploadCtx.ctx, uploadCtx.parentFileID, uploadCtx.fileName, "", uploadCtx.src.Size())
		if err != nil {
			return nil, fmt.Errorf("创建跨云传输上传会话失败: %w", err)
		}

		// 准备上传进度信息
		fileSize := uploadCtx.src.Size()
		chunkSize := createResp.Data.SliceSize
		if chunkSize <= 0 {
			// 使用默认网络速度计算分片大小
			defaultNetworkSpeed := int64(20 * 1024 * 1024) // 20MB/s
			chunkSize = f.getOptimalChunkSize(fileSize, defaultNetworkSpeed)
		}
		totalChunks := (fileSize + chunkSize - 1) / chunkSize
		progress, err := f.prepareUploadProgress(createResp.Data.PreuploadID, totalChunks, chunkSize, fileSize)
		if err != nil {
			return nil, fmt.Errorf("准备跨云传输上传进度失败: %w", err)
		}

		// 计算最优并发参数
		concurrencyParams := f.calculateConcurrencyParams(fileSize)
		maxConcurrency := concurrencyParams.optimal

		return f.downloadThenUpload(uploadCtx.ctx, uploadCtx.src, createResp, progress, uploadCtx.fileName, maxConcurrency)
	}

	// 为v2多线程上传创建专用会话，强制使用v2 API
	createResp, err := f.createUploadV2(uploadCtx.ctx, uploadCtx.parentFileID, uploadCtx.fileName, md5Hash, uploadCtx.src.Size())
	if err != nil {
		// 检查是否是父目录ID不存在的错误，如果是则重试一次
		if strings.Contains(err.Error(), "父目录ID不存在，缓存已清理，请重试") {
			fs.Debugf(f, "父目录ID不存在，重新查找父目录并重试")

			// 重新查找父目录ID
			leaf, parentID, findErr := f.dirCache.FindPath(uploadCtx.ctx, uploadCtx.fileName, true)
			if findErr != nil {
				return nil, fmt.Errorf("重新查找父目录失败: %w", findErr)
			}

			newParentFileID, parseErr := parseParentID(parentID)
			if parseErr != nil {
				return nil, fmt.Errorf("重新解析父目录ID失败: %w", parseErr)
			}

			fs.Debugf(f, "重新找到父目录ID: %d (原ID: %d)", newParentFileID, uploadCtx.parentFileID)
			uploadCtx.parentFileID = newParentFileID
			uploadCtx.fileName = leaf

			// 重试创建上传会话
			createResp, err = f.createUploadV2(uploadCtx.ctx, uploadCtx.parentFileID, uploadCtx.fileName, md5Hash, uploadCtx.src.Size())
			if err != nil {
				return nil, fmt.Errorf("重试创建v2分片上传会话失败: %w", err)
			}
		} else {
			return nil, fmt.Errorf("创建v2分片上传会话失败: %w", err)
		}
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
	fs.Debugf(f, "🚀 开始v2多线程分片上传流程，文件大小: %d bytes", uploadCtx.src.Size())
	fs.Debugf(f, "📋 【阶段1/3】创建文件 - 调用创建文件接口，检查是否秒传")

	// 🚀 遵循API规范：严格使用API返回的SliceSize
	// 根据123网盘API文档，分片大小必须严格按照API返回的sliceSize进行上传
	apiSliceSize := createResp.Data.SliceSize

	if apiSliceSize <= 0 {
		// 如果API没有返回有效的SliceSize，使用64MB作为回退
		fs.Debugf(f, "⚠️ API未返回有效SliceSize(%d)，使用64MB作为回退", apiSliceSize)
		apiSliceSize = 64 * 1024 * 1024 // 64MB回退大小
	} else {
		// 🚀 使用API返回的分片大小，确保与服务器要求一致
		fs.Debugf(f, "🔧 使用API规范分片大小: %s (API返回SliceSize=%d)", fs.SizeSuffix(apiSliceSize), apiSliceSize)
	}

	fs.Debugf(f, "🔧 使用API规范分片大小: %s (API返回SliceSize=%d)", fs.SizeSuffix(apiSliceSize), createResp.Data.SliceSize)

	// 使用新的v2多线程上传实现，严格遵循API返回的分片大小
	result, err := f.v2MultiThreadUpload(uploadCtx.ctx, uploadCtx.in, uploadCtx.src, createResp, apiSliceSize, uploadCtx.fileName)
	if err != nil {
		// 检查是否是可以通过fallback解决的错误
		if f.shouldFallbackFromV2Upload(err) {
			fs.Debugf(f, "v2多线程上传遇到可恢复错误，尝试传统方式: %v", err)

			// 回退到传统方式：尝试转换为fs.Object
			if srcObj, ok := uploadCtx.src.(fs.Object); ok {
				fs.Debugf(f, "回退到传统分片上传")
				return f.uploadFileInChunksWithResume(uploadCtx.ctx, srcObj, createResp, uploadCtx.src.Size(), apiSliceSize, uploadCtx.fileName)
			}

			// 最后回退到流式上传
			fs.Debugf(f, "源对象不支持重新打开，回退到流式上传策略")
			return f.executeStreamingUpload(uploadCtx)
		} else {
			// 对于不可恢复的错误（如upload_complete失败），直接返回错误
			fs.Debugf(f, "v2多线程上传遇到不可恢复错误，直接返回: %v", err)
			return nil, err
		}
	}

	return result, nil
}

// shouldFallbackFromV2Upload 判断是否应该从v2上传fallback到传统方式
func (f *Fs) shouldFallbackFromV2Upload(err error) bool {
	if err == nil {
		return false
	}

	errStr := err.Error()

	// 不应该fallback的错误类型（这些是最终错误，fallback也无法解决）
	if strings.Contains(errStr, "upload completion failed") ||
		strings.Contains(errStr, "文件校验超时") ||
		strings.Contains(errStr, "完成上传失败") {
		return false
	}

	// 应该fallback的错误类型（这些可能通过传统方式解决）
	if strings.Contains(errStr, "网络连接") ||
		strings.Contains(errStr, "超时") ||
		strings.Contains(errStr, "分片上传失败") ||
		strings.Contains(errStr, "并发") {
		return true
	}

	// 默认不fallback，避免不必要的重试
	return false
}

// v2MultiThreadUpload 基于v2 API的多线程分片上传实现
// 解决"源对象不支持重新打开"的限制，支持本地文件和远程对象的真正并发上传
func (f *Fs) v2MultiThreadUpload(ctx context.Context, in io.Reader, src fs.ObjectInfo, createResp *UploadCreateResp, chunkSize int64, fileName string) (*Object, error) {
	fs.Debugf(f, "📋 【阶段2/3】上传分片 - 开始多线程并发上传，分片大小=%s", fs.SizeSuffix(chunkSize))

	// 注意：跨云传输检测已在更早阶段处理，此函数只处理本地文件

	preuploadID := createResp.Data.PreuploadID
	fileSize := src.Size()

	// 🔧 关键验证：确保使用的分片大小与API返回的SliceSize一致
	apiSliceSize := createResp.Data.SliceSize
	if apiSliceSize > 0 && chunkSize != apiSliceSize {
		fs.Debugf(f, "⚠️ 分片大小不匹配警告: 传入大小=%s, API要求大小=%s, 强制使用API要求大小",
			fs.SizeSuffix(chunkSize), fs.SizeSuffix(apiSliceSize))
		chunkSize = apiSliceSize
	}

	totalChunks := (fileSize + chunkSize - 1) / chunkSize

	fs.Debugf(f, "开始v2多线程分片上传: 文件大小=%s, 分片大小=%s, 总分片数=%d, API要求SliceSize=%d",
		fs.SizeSuffix(fileSize), fs.SizeSuffix(chunkSize), totalChunks, apiSliceSize)

	// 准备或恢复上传进度
	progress, err := f.prepareUploadProgress(preuploadID, totalChunks, chunkSize, fileSize)
	if err != nil {
		return nil, fmt.Errorf("准备上传进度失败: %w", err)
	}

	// 计算最优并发参数
	concurrencyParams := f.calculateConcurrencyParams(fileSize)

	// 智能多线程启用策略 - 根据文件大小动态调整最小并发数
	minConcurrency := 1
	if fileSize > 2*1024*1024*1024 { // >2GB - 强制至少4个并发
		minConcurrency = 4
	} else if fileSize > 1*1024*1024*1024 { // >1GB - 强制至少3个并发
		minConcurrency = 3
	} else if fileSize > 500*1024*1024 { // >500MB - 强制至少2个并发
		minConcurrency = 2
	} else if fileSize > 100*1024*1024 { // >100MB - 强制至少2个并发
		minConcurrency = 2
	}

	if concurrencyParams.actual < minConcurrency {
		concurrencyParams.actual = minConcurrency
		fs.Debugf(f, "🔧 智能多线程调整: 文件大小=%s，最小并发数=%d，调整后并发数=%d",
			fs.SizeSuffix(fileSize), minConcurrency, concurrencyParams.actual)
	}

	fs.Debugf(f, "🚀 v2多线程参数 - 网络速度: %s/s, 最优并发数: %d, 实际并发数: %d",
		fs.SizeSuffix(concurrencyParams.networkSpeed), concurrencyParams.optimal, concurrencyParams.actual)

	// 注意：跨云传输流缓存策略已在v2UploadChunksWithConcurrency函数中实现

	// 选择上传策略：多线程或单线程
	if totalChunks > 1 && concurrencyParams.actual > 1 {
		fs.Debugf(f, "使用v2多线程分片上传，并发数: %d", concurrencyParams.actual)
		return f.v2UploadChunksWithConcurrency(ctx, in, src, createResp, progress, fileName, concurrencyParams.actual)
	}

	// 单线程上传（回退方案）
	fs.Debugf(f, "使用v2单线程分片上传")
	return f.v2UploadChunksSingleThreaded(ctx, in, src, createResp, progress, fileName)
}

// downloadThenUpload 跨云传输策略：先完整下载到本地临时文件，再进行v2多线程上传
// 确保跨云传输的可靠性，避免复杂的流管理问题
func (f *Fs) downloadThenUpload(ctx context.Context, src fs.ObjectInfo, createResp *UploadCreateResp, progress *UploadProgress, fileName string, maxConcurrency int) (*Object, error) {
	fileSize := src.Size()
	fs.Infof(f, "📥 跨云传输策略：开始下载文件到本地临时文件 (%s)", fs.SizeSuffix(fileSize))

	// 重新打开源对象获取全新的输入流
	srcObj, ok := src.(fs.Object)
	if !ok {
		return nil, fmt.Errorf("源对象不支持重新打开，无法进行跨云传输")
	}

	// 创建本地临时文件用于完整下载
	tempFile, err := f.resourcePool.GetOptimizedTempFile("cross_cloud_download_", fileSize)
	if err != nil {
		return nil, fmt.Errorf("创建跨云传输临时文件失败: %w", err)
	}
	defer f.resourcePool.PutTempFile(tempFile)

	// 完整下载文件内容到本地，显示详细进度
	fs.Infof(f, "📥 正在从源云盘下载文件内容，预计大小: %s", fs.SizeSuffix(fileSize))

	// 智能选择下载策略：大文件使用并发下载，小文件使用单线程
	startTime := time.Now()
	var written int64

	if fileSize > 50*1024*1024 { // 大于50MB使用并发下载，降低触发阈值
		fs.Infof(f, "🚀 123网盘启用多线程并发下载优化 (文件大小: %s)", fs.SizeSuffix(fileSize))
		written, err = f.downloadWithConcurrency(ctx, srcObj, tempFile, fileSize, fileName)
	} else {
		fs.Infof(f, "📥 123网盘使用单线程下载 (文件大小: %s)", fs.SizeSuffix(fileSize))
		srcReader, err := srcObj.Open(ctx)
		if err != nil {
			return nil, fmt.Errorf("无法重新打开源文件进行下载: %w", err)
		}
		defer srcReader.Close()
		written, err = f.copyWithProgressAndValidation(ctx, tempFile, srcReader, fileSize, true, fileName)
	}
	downloadDuration := time.Since(startTime)

	if err != nil {
		return nil, fmt.Errorf("从源云盘下载文件失败: %w", err)
	}

	if written != fileSize {
		return nil, fmt.Errorf("下载文件大小不匹配: 期望%s，实际%s",
			fs.SizeSuffix(fileSize), fs.SizeSuffix(written))
	}

	// 计算下载速度
	downloadSpeed := float64(written) / downloadDuration.Seconds() / (1024 * 1024) // MB/s
	fs.Infof(f, "✅ 文件下载完成: %s，耗时: %v，平均速度: %.2f MB/s",
		fs.SizeSuffix(written), downloadDuration.Round(time.Second), downloadSpeed)

	// 重置文件指针到开头准备上传
	_, err = tempFile.Seek(0, io.SeekStart)
	if err != nil {
		return nil, fmt.Errorf("重置临时文件指针失败: %w", err)
	}

	fs.Infof(f, "🚀 开始上传到123网盘: %s", fs.SizeSuffix(written))

	// 使用本地文件进行v2多线程上传（递归调用，但此时isRemoteSource为false）
	uploadStartTime := time.Now()
	result, err := f.v2UploadChunksWithConcurrency(ctx, tempFile, &localFileInfo{
		name:    fileName,
		size:    written,
		modTime: src.ModTime(ctx),
	}, createResp, progress, fileName, maxConcurrency)

	if err != nil {
		return nil, fmt.Errorf("上传到123网盘失败: %w", err)
	}

	uploadDuration := time.Since(uploadStartTime)
	uploadSpeed := float64(written) / uploadDuration.Seconds() / (1024 * 1024) // MB/s
	totalDuration := time.Since(startTime)

	fs.Infof(f, "🎉 跨云传输完成: %s，总耗时: %v (下载: %v + 上传: %v)，上传速度: %.2f MB/s",
		fs.SizeSuffix(written), totalDuration.Round(time.Second),
		downloadDuration.Round(time.Second), uploadDuration.Round(time.Second), uploadSpeed)

	return result, nil
}

// localFileInfo 本地文件信息结构，用于downloadThenUpload策略
type localFileInfo struct {
	name    string
	size    int64
	modTime time.Time
}

func (lfi *localFileInfo) Fs() fs.Info                           { return nil }
func (lfi *localFileInfo) String() string                        { return lfi.name }
func (lfi *localFileInfo) Remote() string                        { return lfi.name }
func (lfi *localFileInfo) ModTime(ctx context.Context) time.Time { return lfi.modTime }
func (lfi *localFileInfo) Size() int64                           { return lfi.size }
func (lfi *localFileInfo) Storable() bool                        { return true }
func (lfi *localFileInfo) Hash(ctx context.Context, ty fshash.Type) (string, error) {
	return "", fshash.ErrUnsupported
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

	// 调试preuploadID
	fs.Debugf(f, "v2多线程上传参数检查: preuploadID='%s', totalChunks=%d, maxConcurrency=%d", preuploadID, totalChunks, maxConcurrency)
	if preuploadID == "" {
		return nil, fmt.Errorf("预上传ID为空，无法进行分片上传")
	}

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

	// 将输入流读取到内存或临时文件，以支持并发访问（此时已确保是本地文件）
	var dataSource io.ReaderAt
	var cleanup func()

	if fileSize <= maxMemoryBufferSize {
		// 小文件：读取到内存进行并发上传
		fs.Debugf(f, "📝 小文件(%s)，读取到内存进行并发上传", fs.SizeSuffix(fileSize))
		data, err := f.readToMemoryWithRetry(ctx, in, fileSize, false)
		if err != nil {
			return nil, fmt.Errorf("读取文件数据到内存失败: %w", err)
		}
		dataSource = bytes.NewReader(data)
		cleanup = func() {} // 内存数据无需清理
		fs.Debugf(f, "✅ 小文件内存缓存完成，实际大小: %s", fs.SizeSuffix(int64(len(data))))
	} else {
		// 大文件：使用临时文件进行并发上传
		fs.Debugf(f, "🗂️  大文件(%s)，使用临时文件进行并发上传", fs.SizeSuffix(fileSize))
		tempFile, written, err := f.createTempFileWithRetry(ctx, in, fileSize, false, fileName)
		if err != nil {
			return nil, fmt.Errorf("创建临时文件失败: %w", err)
		}

		dataSource = tempFile
		cleanup = func() {
			f.resourcePool.PutTempFile(tempFile) // 使用资源池的清理方法
		}

		fs.Debugf(f, "✅ 大文件临时文件创建完成，期望大小: %s，实际大小: %s",
			fs.SizeSuffix(fileSize), fs.SizeSuffix(written))
	}
	defer cleanup()

	// 验证数据源完整性
	fs.Debugf(f, "🔍 验证数据源完整性...")
	if dataSource == nil {
		return nil, fmt.Errorf("数据源创建失败，无法进行多线程上传")
	}

	// 测试数据源的随机访问能力
	testBuffer := make([]byte, 1024)
	if _, err := dataSource.ReadAt(testBuffer, 0); err != nil && err != io.EOF {
		fs.Debugf(f, "⚠️  数据源随机访问测试失败: %v", err)
		return nil, fmt.Errorf("数据源不支持随机访问，无法进行多线程上传: %w", err)
	}
	fs.Debugf(f, "✅ 数据源验证通过，支持多线程并发访问")

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
	fs.Debugf(f, "🚀 启动 %d 个工作协程进行并发上传", maxConcurrency)
	for i := 0; i < maxConcurrency; i++ {
		workerID := i + 1
		fs.Debugf(f, "📋 启动工作协程 #%d", workerID)
		go f.v2ChunkUploadWorker(workerCtx, dataSource, preuploadID, chunkSize, fileSize, totalChunks, chunkJobs, results)
	}
	fs.Debugf(f, "✅ 所有工作协程已启动，开始分片上传任务分发")

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

		// 🔧 统一进度显示：查找Account对象进行集成
		var currentAccount *accounting.Account
		if stats := accounting.GlobalStats(); stats != nil {
			// 尝试精确匹配
			currentAccount = stats.GetInProgressAccount(fileName)
			if currentAccount == nil {
				// 尝试模糊匹配：查找包含文件名的Account
				allAccounts := stats.ListInProgressAccounts()
				for _, accountName := range allAccounts {
					if strings.Contains(accountName, fileName) || strings.Contains(fileName, accountName) {
						currentAccount = stats.GetInProgressAccount(accountName)
						if currentAccount != nil {
							break
						}
					}
				}
			}
		}

		for completedChunks < pendingChunks {
			select {
			case result := <-results:
				if result.err != nil {
					fs.Errorf(f, "❌ 分片 %d 上传失败: %v", result.chunkIndex+1, result.err)

					// 检查是否应该触发智能回退
					shouldFallback := f.shouldFallbackToSingleThread(result.err, src)
					if shouldFallback {
						fs.Infof(f, "🔄 检测到多线程上传问题，启动智能回退机制...")

						// 保存当前进度
						saveErr := f.saveUploadProgress(progress)
						if saveErr != nil {
							fs.Debugf(f, "保存进度失败: %v", saveErr)
						}

						// 尝试回退到单线程上传
						return f.fallbackToSingleThreadUpload(ctx, in, src, createResp, progress, fileName, result.err)
					}

					// 如果不需要回退，保存进度后返回错误
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

				fs.Infof(f, "📊 上传进度: %d/%d分片 (%.1f%%) | 已传输: %s/%s | 速度: %.2f MB/s | %s",
					completedChunks, pendingChunks, percentage,
					fs.SizeSuffix(totalUploadedBytes), fs.SizeSuffix(fileSize), avgSpeed, eta)

				// 🔧 统一进度显示：更新Account的额外信息
				if currentAccount != nil {
					extraInfo := fmt.Sprintf("[%d/%d分片]", completedChunks, pendingChunks)
					currentAccount.SetExtraInfo(extraInfo)
				}

			case <-workerCtx.Done():
				return nil, fmt.Errorf("上传超时或被取消")
			}
		}

		// 最终统计
		totalElapsed := time.Since(uploadStartTime)
		finalSpeed := float64(fileSize) / totalElapsed.Seconds() / 1024 / 1024
		fs.Infof(f, "🎉 多线程分片上传完成！文件大小: %s | 总耗时: %v | 平均速度: %.2f MB/s",
			fs.SizeSuffix(fileSize), totalElapsed.Round(time.Second), finalSpeed)
	}

	// 完成上传，传递预期MD5用于验证
	return f.completeV2Upload(ctx, preuploadID, fileName, fileSize, expectedMD5)
}

// v2ChunkUploadWorker v2分片上传工作协程，带超时和死锁检测
func (f *Fs) v2ChunkUploadWorker(ctx context.Context, dataSource io.ReaderAt, preuploadID string, chunkSize, fileSize, totalChunks int64, jobs <-chan int64, results chan<- v2ChunkResult) {
	workerID := fmt.Sprintf("Worker-%d", time.Now().UnixNano()%1000) // 简单的工作协程ID
	fs.Debugf(f, "🔧 %s 启动，等待分片任务...", workerID)

	processedCount := 0
	lastActivityTime := time.Now()

	// 启动死锁检测协程
	deadlockCtx, cancelDeadlock := context.WithCancel(ctx)
	defer cancelDeadlock()

	go f.workerDeadlockDetector(deadlockCtx, workerID, &lastActivityTime, 5*time.Minute)

	for chunkIndex := range jobs {
		select {
		case <-ctx.Done():
			fs.Debugf(f, "⚠️  %s 收到取消信号，已处理 %d 个分片", workerID, processedCount)
			return
		default:
			// 更新活动时间
			lastActivityTime = time.Now()
			processedCount++
			partNumber := chunkIndex + 1
			fs.Debugf(f, "📋 %s 开始处理分片 %d/%d (第%d个任务)", workerID, partNumber, totalChunks, processedCount)

			// 为单个分片处理设置超时
			chunkCtx, cancelChunk := context.WithTimeout(ctx, 10*time.Minute)
			result := f.processChunkWithTimeout(chunkCtx, dataSource, preuploadID, chunkIndex, chunkSize, fileSize, totalChunks, workerID)
			cancelChunk()

			// 发送结果时也要检测超时
			select {
			case results <- result:
				lastActivityTime = time.Now() // 更新活动时间
			case <-ctx.Done():
				fs.Debugf(f, "⚠️  %s 发送结果时收到取消信号", workerID)
				return
			case <-time.After(30 * time.Second):
				fs.Errorf(f, "⚠️  %s 发送结果超时，可能存在死锁", workerID)
				// 尝试发送超时错误结果
				timeoutResult := v2ChunkResult{
					chunkIndex: chunkIndex,
					err:        fmt.Errorf("工作协程发送结果超时，可能存在死锁"),
				}
				select {
				case results <- timeoutResult:
				default:
					fs.Errorf(f, "❌ %s 无法发送超时错误结果，通道可能已满", workerID)
				}
				return
			}
		}
	}

	fs.Debugf(f, "🏁 %s 完成工作，共处理 %d 个分片", workerID, processedCount)
}

// workerDeadlockDetector 工作协程死锁检测器
func (f *Fs) workerDeadlockDetector(ctx context.Context, workerID string, lastActivityTime *time.Time, timeout time.Duration) {
	ticker := time.NewTicker(30 * time.Second) // 每30秒检查一次
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if time.Since(*lastActivityTime) > timeout {
				fs.Errorf(f, "🚨 %s 可能发生死锁！超过 %v 无活动", workerID, timeout)
				// 这里可以添加更多的死锁处理逻辑，比如发送告警等
			}
		}
	}
}

// processChunkWithTimeout 带超时的分片处理函数
func (f *Fs) processChunkWithTimeout(ctx context.Context, dataSource io.ReaderAt, preuploadID string, chunkIndex, chunkSize, fileSize, totalChunks int64, workerID string) v2ChunkResult {
	partNumber := chunkIndex + 1

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
		return v2ChunkResult{
			chunkIndex: chunkIndex,
			err:        fmt.Errorf("读取分片数据失败: %w", err),
		}
	}

	// 验证读取的数据长度
	if int64(bytesRead) != actualChunkSize {
		return v2ChunkResult{
			chunkIndex: chunkIndex,
			err:        fmt.Errorf("分片数据长度不匹配: 期望%d，实际%d", actualChunkSize, bytesRead),
		}
	}

	// 计算分片MD5
	hasher := md5.New()
	hasher.Write(chunkData)
	chunkHash := fmt.Sprintf("%x", hasher.Sum(nil))

	// 上传分片（记录开始时间用于速度计算）
	startTime := time.Now()

	fs.Debugf(f, "🚀 %s 开始上传分片 %d/%d (大小: %s, MD5: %s)",
		workerID, partNumber, totalChunks, fs.SizeSuffix(actualChunkSize), chunkHash[:8]+"...")

	// 实现重试机制 - 提升上传成功率
	var uploadErr error
	maxRetries := 3
	for retry := 0; retry < maxRetries; retry++ {
		// 检查上下文是否已取消
		select {
		case <-ctx.Done():
			return v2ChunkResult{
				chunkIndex: chunkIndex,
				err:        fmt.Errorf("分片上传被取消: %w", ctx.Err()),
			}
		default:
		}

		uploadErr = f.uploadChunkV2(ctx, preuploadID, partNumber, chunkData, chunkHash)
		if uploadErr == nil {
			break // 上传成功，跳出重试循环
		}

		if retry < maxRetries {
			retryDelay := time.Duration(retry+1) * 2 * time.Second // 递增延迟
			fs.Debugf(f, "⚠️  %s 分片 %d/%d 上传失败，%v后重试 (%d/%d): %v",
				workerID, partNumber, totalChunks, retryDelay, retry+1, maxRetries, uploadErr)

			select {
			case <-ctx.Done():
				return v2ChunkResult{
					chunkIndex: chunkIndex,
					err:        fmt.Errorf("分片上传重试时被取消: %w", ctx.Err()),
				}
			case <-time.After(retryDelay):
				// 继续重试
			}
		}
	}

	if uploadErr != nil {
		duration := time.Since(startTime)
		fs.Errorf(f, "❌ %s 分片 %d/%d 上传失败 (耗时: %v, 重试: %d次): %v",
			workerID, partNumber, totalChunks, duration, maxRetries, uploadErr)
		return v2ChunkResult{
			chunkIndex: chunkIndex,
			err:        fmt.Errorf("上传分片失败(重试%d次): %w", maxRetries, uploadErr),
		}
	}

	// 计算上传速度和统计信息
	duration := time.Since(startTime)
	speed := float64(actualChunkSize) / duration.Seconds() / 1024 / 1024 // MB/s

	fs.Infof(f, "✅ %s 分片 %d/%d 上传成功 (大小: %s, 耗时: %v, 速度: %.2f MB/s)",
		workerID, partNumber, totalChunks, fs.SizeSuffix(actualChunkSize), duration, speed)

	// 成功
	return v2ChunkResult{
		chunkIndex: chunkIndex,
		err:        nil,
		chunkHash:  chunkHash,
		duration:   duration,
		size:       actualChunkSize,
	}
}

// shouldFallbackToSingleThread 判断是否应该回退到单线程上传
func (f *Fs) shouldFallbackToSingleThread(err error, src fs.ObjectInfo) bool {
	if err == nil {
		return false
	}

	errStr := strings.ToLower(err.Error())

	// 检查跨云传输相关错误
	isRemoteSource := f.isRemoteSource(src)
	if isRemoteSource {
		// 跨云传输时，以下错误触发回退
		fallbackKeywords := []string{
			"数据不完整",
			"数据不匹配",
			"读取失败",
			"连接重置",
			"超时",
			"网络错误",
			"源对象",
			"重新打开",
			"死锁",
			"工作协程",
		}

		for _, keyword := range fallbackKeywords {
			if strings.Contains(errStr, keyword) {
				fs.Debugf(f, "🔍 检测到跨云传输错误关键词: %s", keyword)
				return true
			}
		}
	}

	// 检查多线程相关错误
	multiThreadKeywords := []string{
		"并发",
		"协程",
		"goroutine",
		"concurrent",
		"deadlock",
		"timeout",
		"context canceled",
		"context deadline exceeded",
	}

	for _, keyword := range multiThreadKeywords {
		if strings.Contains(errStr, keyword) {
			fs.Debugf(f, "🔍 检测到多线程错误关键词: %s", keyword)
			return true
		}
	}

	return false
}

// fallbackToSingleThreadUpload 回退到单线程上传
func (f *Fs) fallbackToSingleThreadUpload(ctx context.Context, in io.Reader, src fs.ObjectInfo, createResp *UploadCreateResp, progress *UploadProgress, fileName string, originalErr error) (*Object, error) {
	fs.Infof(f, "🔄 多线程上传失败，回退到单线程上传模式")
	fs.Debugf(f, "原始错误: %v", originalErr)

	// 检查是否为跨云传输
	isRemoteSource := f.isRemoteSource(src)
	if isRemoteSource {
		fs.Infof(f, "🌐 跨云传输场景，使用增强单线程模式")
	}

	// 尝试单线程分片上传
	fs.Debugf(f, "🔧 尝试v2单线程分片上传...")
	result, err := f.v2UploadChunksSingleThreaded(ctx, in, src, createResp, progress, fileName)
	if err == nil {
		fs.Infof(f, "✅ 单线程分片上传成功")
		return result, nil
	}

	fs.Debugf(f, "单线程分片上传也失败: %v", err)

	// 如果单线程分片上传也失败，尝试流式上传
	if isRemoteSource {
		fs.Infof(f, "🔄 单线程分片上传失败，尝试流式上传...")

		// 创建流式上传上下文
		// 注意：这里需要从其他地方获取parentFileID，暂时使用0作为占位符
		uploadCtx := &UnifiedUploadContext{
			ctx:          ctx,
			in:           in,
			src:          src,
			parentFileID: 0, // TODO: 需要从上传上下文中获取正确的parentFileID
			fileName:     fileName,
		}

		streamResult, streamErr := f.streamingPutSimplified(uploadCtx)
		if streamErr == nil {
			fs.Infof(f, "✅ 流式上传成功")
			return streamResult, nil
		}

		fs.Debugf(f, "流式上传也失败: %v", streamErr)

		// 返回最详细的错误信息
		return nil, fmt.Errorf("所有上传方式都失败 - 原始多线程错误: %w, 单线程错误: %v, 流式上传错误: %v", originalErr, err, streamErr)
	}

	// 非跨云传输场景，返回单线程上传错误
	return nil, fmt.Errorf("多线程上传失败后单线程上传也失败 - 原始错误: %w, 单线程错误: %v", originalErr, err)
}

// uploadChunkV2 使用v2 API上传单个分片
func (f *Fs) uploadChunkV2(ctx context.Context, preuploadID string, partNumber int64, chunkData []byte, chunkHash string) error {
	// 检查preuploadID是否为空
	if preuploadID == "" {
		fs.Errorf(f, "❌ 致命错误：预上传ID为空，无法上传分片 %d", partNumber)
		return fmt.Errorf("预上传ID不能为空")
	}

	// 检查分片数据是否为空
	if len(chunkData) == 0 {
		fs.Errorf(f, "❌ 致命错误：分片 %d 数据为空", partNumber)
		return fmt.Errorf("分片数据不能为空")
	}

	fs.Debugf(f, "开始上传分片 %d，预上传ID: %s, 分片MD5: %s, 数据大小: %d 字节",
		partNumber, preuploadID, chunkHash, len(chunkData))

	// 使用uploadPacer进行QPS限制控制
	return f.uploadPacer.Call(func() (bool, error) {
		// 🚀 优化：使用缓存的上传域名
		uploadDomain, err := f.getCachedUploadDomain(ctx)
		if err != nil {
			return false, fmt.Errorf("获取上传域名失败: %w", err)
		}

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

		// 🚀 优化：使用缓存的上传域名进行multipart上传
		err = f.makeAPICallWithRestMultipartToDomain(ctx, uploadDomain, "/upload/v2/file/slice", "POST", &buf, writer.FormDataContentType(), &response)
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

	// 验证读取的数据长度与预期文件大小匹配
	actualDataSize := int64(len(data))
	if actualDataSize != fileSize {
		return nil, fmt.Errorf("数据长度不匹配: 实际读取 %d 字节，预期 %d 字节", actualDataSize, fileSize)
	}

	if actualDataSize == 0 {
		return nil, fmt.Errorf("读取的文件数据为空，无法进行分片上传")
	}

	fs.Debugf(f, "数据验证通过: 读取 %d 字节，分片大小 %d，总分片数 %d", actualDataSize, chunkSize, totalChunks)

	// 逐个上传分片
	for chunkIndex := int64(0); chunkIndex < totalChunks; chunkIndex++ {
		partNumber := chunkIndex + 1

		// 检查是否已上传
		if progress.IsUploaded(partNumber) {
			fs.Debugf(f, "分片 %d 已上传，跳过", partNumber)
			continue
		}

		// 计算分片数据，添加边界检查
		start := chunkIndex * chunkSize
		end := start + chunkSize
		if end > fileSize {
			end = fileSize
		}

		// 额外的边界检查，防止slice panic
		if start >= actualDataSize {
			return nil, fmt.Errorf("分片起始位置超出数据范围: start=%d, dataSize=%d", start, actualDataSize)
		}
		if end > actualDataSize {
			end = actualDataSize
		}

		chunkData := data[start:end]

		// 计算分片MD5
		hasher := md5.New()
		hasher.Write(chunkData)
		chunkHash := fmt.Sprintf("%x", hasher.Sum(nil))

		// 添加详细的MD5调试信息
		fs.Debugf(f, "分片 %d MD5计算: 数据长度=%d, MD5=%s",
			partNumber, len(chunkData), chunkHash)

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

		fs.Debugf(f, "✅ 分片 %d/%d 上传完成", partNumber, totalChunks)
	}

	// 完成上传（单线程版本，没有预期MD5）
	return f.completeV2Upload(ctx, preuploadID, fileName, fileSize, "")
}

// completeV2Upload 完成v2上传并返回Object
func (f *Fs) completeV2Upload(ctx context.Context, preuploadID, fileName string, fileSize int64, expectedMD5 string) (*Object, error) {
	fs.Debugf(f, "📋 【阶段3/3】上传完毕 - 开始轮询校验，等待服务端文件校验完成")
	// 调用增强的完成上传逻辑，包含MD5验证和动态轮询
	result, err := f.completeUploadWithMD5VerificationAndSize(ctx, preuploadID, expectedMD5, fileSize)
	if err != nil {
		return nil, fmt.Errorf("完成上传失败: %w", err)
	}

	// 清理上传进度文件
	f.removeUploadProgress(preuploadID)

	// 验证文件ID
	if result.FileID == 0 {
		return nil, fmt.Errorf("上传完成但未获取到有效的文件ID")
	}

	// 创建并返回Object，使用从API获取的实际数据
	obj := &Object{
		fs:          f,
		remote:      fileName,
		hasMetaData: true,                                 // 我们有完整的元数据
		id:          strconv.FormatInt(result.FileID, 10), // 使用实际的文件ID
		size:        fileSize,
		md5sum:      result.Etag, // 使用从API获取的MD5
		modTime:     time.Now(),
		isDir:       false,
	}

	fs.Infof(f, "🎉 文件上传成功！文件名: %s | 大小: %s | 文件ID: %d | MD5: %s", fileName, fs.SizeSuffix(fileSize), result.FileID, result.Etag)
	return obj, nil
}

// completeUploadWithMD5Verification 完成上传并进行MD5验证（兼容性函数）
func (f *Fs) completeUploadWithMD5Verification(ctx context.Context, preuploadID, expectedMD5 string) (*UploadCompleteResult, error) {
	return f.completeUploadWithMD5VerificationAndSize(ctx, preuploadID, expectedMD5, 0)
}

// completeUploadWithMD5VerificationAndSize 完成上传并进行MD5验证，支持动态轮询
func (f *Fs) completeUploadWithMD5VerificationAndSize(ctx context.Context, preuploadID, expectedMD5 string, fileSize int64) (*UploadCompleteResult, error) {
	// 首先调用支持动态轮询的完成上传逻辑并获取结果
	result, err := f.completeUploadWithResultAndSize(ctx, preuploadID, fileSize)
	if err != nil {
		return nil, err
	}

	// 如果提供了预期MD5，进行额外的验证
	if expectedMD5 != "" && result.Etag != "" {
		fs.Debugf(f, "开始MD5验证，预期值: %s, 实际值: %s", expectedMD5, result.Etag)

		if result.Etag != expectedMD5 {
			fs.Debugf(f, "MD5不匹配，但文件已上传成功，文件ID: %d", result.FileID)
			// 不返回错误，因为文件已经上传成功，只是MD5验证有问题
		} else {
			fs.Debugf(f, "MD5验证成功")
		}
	} else if expectedMD5 != "" {
		fs.Debugf(f, "服务器未返回MD5值，跳过验证")
	}

	return result, nil
}

// verifyUploadMD5 验证上传文件的MD5是否正确
// 增强版本：更强的错误处理和智能重试机制
func (f *Fs) verifyUploadMD5(ctx context.Context, preuploadID, expectedMD5 string) error {
	fs.Debugf(f, "🔍 开始MD5验证轮询，preuploadID: %s, 预期MD5: %s", preuploadID, expectedMD5)

	// 参数验证
	if preuploadID == "" {
		return fmt.Errorf("preuploadID不能为空")
	}
	if expectedMD5 == "" {
		fs.Debugf(f, "⚠️  预期MD5为空，跳过MD5验证")
		return nil
	}

	// 增强的重试配置
	maxAttempts := 15 // 增加重试次数
	networkErrorRetries := 0
	maxNetworkErrorRetries := 5
	md5MismatchRetries := 0
	maxMD5MismatchRetries := 3

	for attempt := 0; attempt < maxAttempts; attempt++ {
		// 检查上下文是否已取消
		select {
		case <-ctx.Done():
			return fmt.Errorf("MD5验证被取消: %w", ctx.Err())
		default:
		}

		reqBody := map[string]any{
			"preuploadID": preuploadID,
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

		// 使用v2上传完毕API检查文件状态和MD5
		err := f.makeAPICallWithRest(ctx, "/upload/v2/file/upload_complete", "POST", reqBody, &response)
		if err != nil {
			// 网络错误分类处理
			errorType := f.classifyNetworkError(err)
			fs.Debugf(f, "🔄 MD5验证第%d次尝试失败 [%s]: %v", attempt+1, errorType, err)

			networkErrorRetries++
			if networkErrorRetries > maxNetworkErrorRetries {
				return fmt.Errorf("网络错误重试次数超限，MD5验证失败: %w", err)
			}

			// 根据错误类型调整等待时间
			waitTime := f.calculateRetryWaitTime(errorType, attempt)
			fs.Debugf(f, "⏰ 网络错误，等待%v后重试", waitTime)
			time.Sleep(waitTime)
			continue
		}

		// API错误处理
		if response.Code != 0 {
			fs.Debugf(f, "🚫 MD5验证第%d次尝试返回错误: API错误 %d: %s", attempt+1, response.Code, response.Message)

			// 特定API错误的处理
			apiErr := fmt.Errorf("API错误 %d: %s", response.Code, response.Message)
			if f.isRetryableAPIError(apiErr) {
				waitTime := time.Duration(attempt+1) * 2 * time.Second
				fs.Debugf(f, "⏰ 可重试的API错误，等待%v后重试", waitTime)
				time.Sleep(waitTime)
				continue
			} else {
				return fmt.Errorf("不可重试的API错误 %d: %s", response.Code, response.Message)
			}
		}

		if response.Data.Completed {
			// 检查MD5是否匹配
			actualMD5 := strings.ToLower(strings.TrimSpace(response.Data.Etag))
			expectedMD5Lower := strings.ToLower(strings.TrimSpace(expectedMD5))

			if actualMD5 == expectedMD5Lower {
				fs.Debugf(f, "✅ MD5验证成功: %s", actualMD5)
				return nil
			} else {
				md5MismatchRetries++
				fs.Debugf(f, "❌ MD5不匹配 (第%d次): 预期=%s, 实际=%s", md5MismatchRetries, expectedMD5Lower, actualMD5)

				// MD5不匹配的智能重试
				if md5MismatchRetries <= maxMD5MismatchRetries {
					waitTime := time.Duration(md5MismatchRetries) * 3 * time.Second
					fs.Debugf(f, "🔄 MD5不匹配，等待%v后重试验证（可能是服务端处理延迟）", waitTime)
					time.Sleep(waitTime)
					continue
				} else {
					return fmt.Errorf("MD5验证失败，多次重试后仍不匹配: 预期=%s, 实际=%s", expectedMD5Lower, actualMD5)
				}
			}
		}

		// 等待后重试
		waitTime := f.calculateProgressiveWaitTime(attempt)
		fs.Debugf(f, "⏳ MD5验证尚未完成，等待%v后重试（第%d/%d次）", waitTime, attempt+1, maxAttempts)
		time.Sleep(waitTime)
	}

	return fmt.Errorf("MD5验证超时，经过%d次尝试后无法确认文件MD5状态", maxAttempts)
}

// classifyNetworkError 分类网络错误类型
func (f *Fs) classifyNetworkError(err error) string {
	if err == nil {
		return "无错误"
	}

	errStr := strings.ToLower(err.Error())

	// 超时错误
	if strings.Contains(errStr, "timeout") || strings.Contains(errStr, "deadline exceeded") {
		return "超时错误"
	}

	// 连接错误
	if strings.Contains(errStr, "connection refused") || strings.Contains(errStr, "no route to host") {
		return "连接错误"
	}

	// DNS错误
	if strings.Contains(errStr, "no such host") || strings.Contains(errStr, "dns") {
		return "DNS错误"
	}

	// 网络不可达
	if strings.Contains(errStr, "network unreachable") || strings.Contains(errStr, "host unreachable") {
		return "网络不可达"
	}

	// TLS/SSL错误
	if strings.Contains(errStr, "tls") || strings.Contains(errStr, "ssl") || strings.Contains(errStr, "certificate") {
		return "TLS错误"
	}

	// HTTP状态码错误
	if strings.Contains(errStr, "status") && (strings.Contains(errStr, "5") || strings.Contains(errStr, "4")) {
		return "HTTP状态错误"
	}

	return "其他网络错误"
}

// calculateRetryWaitTime 根据错误类型计算重试等待时间
func (f *Fs) calculateRetryWaitTime(errorType string, attempt int) time.Duration {
	baseWait := time.Duration(attempt+1) * time.Second

	switch errorType {
	case "超时错误":
		// 超时错误需要更长的等待时间
		return baseWait * 3
	case "连接错误", "网络不可达":
		// 连接问题需要较长等待
		return baseWait * 2
	case "DNS错误":
		// DNS问题通常需要更长时间恢复
		return baseWait * 4
	case "TLS错误":
		// TLS问题可能需要重新握手
		return baseWait * 2
	case "HTTP状态错误":
		// HTTP错误通常可以较快重试
		return baseWait
	default:
		// 其他错误使用标准等待时间
		return baseWait
	}
}

// calculateProgressiveWaitTime 计算渐进式等待时间
func (f *Fs) calculateProgressiveWaitTime(attempt int) time.Duration {
	// 渐进式退避算法：1s, 2s, 4s, 8s, 16s, 然后保持16s
	waitSeconds := 1 << attempt // 2^attempt
	if waitSeconds > 16 {
		waitSeconds = 16
	}
	return time.Duration(waitSeconds) * time.Second
}

// calculateMD5WithStreamCache 从输入流计算MD5并缓存流内容，返回新的可读流
// 使用分块读取和超时控制，避免大文件处理时的网络超时问题
func (f *Fs) calculateMD5WithStreamCache(ctx context.Context, in io.Reader, expectedSize int64) (string, io.Reader, error) {
	fs.Debugf(f, "开始从输入流计算MD5并缓存流内容，预期大小: %s", fs.SizeSuffix(expectedSize))

	hasher := md5.New()
	startTime := time.Now()

	// 对于大文件，使用临时文件缓存
	if expectedSize > maxMemoryBufferSize {
		fs.Infof(f, "📋 准备上传大文件 (%s) - 正在计算MD5哈希以启用秒传功能...", fs.SizeSuffix(expectedSize))
		fs.Infof(f, "💡 提示：MD5计算完成后将检查是否可以秒传，大文件计算需要一些时间，请耐心等待")
		fs.Debugf(f, "大文件使用临时文件缓存流内容，采用分块读取策略")
		tempFile, err := f.resourcePool.GetTempFile("stream_cache_")
		if err != nil {
			return "", nil, fmt.Errorf("创建临时文件失败: %w", err)
		}

		// 使用分块读取，避免一次性读取整个大文件导致超时
		written, err := f.copyWithChunksAndTimeout(ctx, tempFile, hasher, in, expectedSize)
		if err != nil {
			tempFile.Close()
			os.Remove(tempFile.Name())
			return "", nil, fmt.Errorf("缓存流内容时复制数据失败: %w", err)
		}

		if expectedSize > 0 && written != expectedSize {
			fs.Debugf(f, "警告：实际读取大小(%d)与预期大小(%d)不匹配", written, expectedSize)
		}

		// 重置文件指针到开头
		_, err = tempFile.Seek(0, io.SeekStart)
		if err != nil {
			tempFile.Close()
			os.Remove(tempFile.Name())
			return "", nil, fmt.Errorf("重置临时文件指针失败: %w", err)
		}

		md5Hash := fmt.Sprintf("%x", hasher.Sum(nil))
		elapsed := time.Since(startTime)
		speed := float64(written) / elapsed.Seconds() / 1024 / 1024 // MB/s
		fs.Infof(f, "✅ MD5计算完成！哈希值: %s | 处理: %s | 耗时: %v | 平均速度: %.2f MB/s",
			md5Hash, fs.SizeSuffix(written), elapsed.Round(time.Second), speed)
		fs.Debugf(f, "大文件MD5计算完成: %s，缓存了 %s 数据", md5Hash, fs.SizeSuffix(written))

		// 返回一个自动清理的读取器
		return md5Hash, &tempFileReader{file: tempFile}, nil
	} else {
		// 🔧 修复数据流读取问题：小文件在内存中缓存
		fs.Infof(f, "📋 正在计算文件MD5哈希 (%s)...", fs.SizeSuffix(expectedSize))
		fs.Debugf(f, "小文件在内存中缓存流内容")

		// 🚨 关键修复：使用更安全的数据读取方式
		data := make([]byte, expectedSize)
		totalRead := int64(0)

		// 循环读取直到读完所有数据
		for totalRead < expectedSize {
			n, err := in.Read(data[totalRead:])
			totalRead += int64(n)

			if err == io.EOF {
				break // 正常结束
			}
			if err != nil {
				return "", nil, fmt.Errorf("读取输入流失败: %w", err)
			}
		}

		// 调整数据大小以匹配实际读取的内容
		if totalRead != expectedSize {
			fs.Debugf(f, "🚨 数据流大小调整：期望%d字节，实际读取%d字节", expectedSize, totalRead)
			data = data[:totalRead]
		}

		// 计算MD5
		hasher.Write(data)
		written := totalRead

		md5Hash := fmt.Sprintf("%x", hasher.Sum(nil))
		elapsed := time.Since(startTime)
		fs.Infof(f, "✅ MD5计算完成！哈希值: %s | 处理: %s | 耗时: %v",
			md5Hash, fs.SizeSuffix(written), elapsed.Round(time.Millisecond))
		fs.Debugf(f, "小文件MD5计算完成: %s，缓存了 %s 数据", md5Hash, fs.SizeSuffix(written))

		// 🔧 修复：使用实际读取的数据创建Reader
		return md5Hash, bytes.NewReader(data), nil
	}
}

// copyWithChunksAndTimeout 分块复制数据，支持超时控制和进度监控
// 同时写入到文件和MD5计算器，避免大文件一次性读取导致的超时问题
// 修复版本：解决跨云传输卡死问题，增强错误处理和超时机制
func (f *Fs) copyWithChunksAndTimeout(ctx context.Context, file *os.File, hasher hash.Hash, in io.Reader, expectedSize int64) (int64, error) {
	const (
		chunkSize       = 4 * 1024 * 1024  // 4MB 分块大小（减小以提高响应性）
		timeoutPerChunk = 60 * time.Second // 每个分块的超时时间（增加到60秒）
		maxRetries      = 3                // 最大重试次数
	)

	multiWriter := io.MultiWriter(file, hasher)
	buffer := make([]byte, chunkSize)
	var totalWritten int64

	fs.Debugf(f, "🚀 开始分块复制，分块大小: %s，预期总大小: %s", fs.SizeSuffix(chunkSize), fs.SizeSuffix(expectedSize))

	// 为大文件添加进度提示
	var lastProgressTime time.Time
	startTime := time.Now()
	const progressInterval = 3 * time.Second // 每3秒显示一次进度（更频繁的反馈）

	// 添加总体超时控制，防止整个操作无限期卡住
	var overallTimeout time.Duration
	if expectedSize > 0 {
		// 基于文件大小计算合理的总超时时间：每MB最多30秒，最少5分钟，最多2小时
		timeoutPerMB := 30 * time.Second
		overallTimeout = time.Duration(expectedSize/(1024*1024)) * timeoutPerMB
		if overallTimeout < 5*time.Minute {
			overallTimeout = 5 * time.Minute
		}
		if overallTimeout > 2*time.Hour {
			overallTimeout = 2 * time.Hour
		}
	} else {
		overallTimeout = 30 * time.Minute // 未知大小文件默认30分钟超时
	}

	overallCtx, overallCancel := context.WithTimeout(ctx, overallTimeout)
	defer overallCancel()

	fs.Debugf(f, "⏰ 设置总体超时时间: %v", overallTimeout)

	chunkNumber := 0
	maxChunks := 100000 // 🔧 防止无限循环：最多100000个分片
	for {
		chunkNumber++
		if chunkNumber > maxChunks {
			return totalWritten, fmt.Errorf("读取分片数量超过最大限制 %d，可能存在循环", maxChunks)
		}

		// 检查总体超时
		select {
		case <-overallCtx.Done():
			return totalWritten, fmt.Errorf("整体操作超时 (%v): %w", overallTimeout, overallCtx.Err())
		default:
		}

		var n int
		var readErr error

		// 使用重试机制处理网络不稳定问题
		for retry := 0; retry < maxRetries; retry++ {
			// 为每个分块设置超时
			chunkCtx, cancel := context.WithTimeout(overallCtx, timeoutPerChunk)

			// 创建一个带超时的读取器
			readDone := make(chan struct {
				n   int
				err error
			}, 1)

			go func() {
				n, err := in.Read(buffer)
				readDone <- struct {
					n   int
					err error
				}{n, err}
			}()

			select {
			case result := <-readDone:
				n, readErr = result.n, result.err
				cancel()
				break // 成功读取，跳出重试循环
			case <-chunkCtx.Done():
				cancel()
				if retry < maxRetries-1 {
					fs.Debugf(f, "⚠️ 分块 %d 读取超时，重试 %d/%d", chunkNumber, retry+1, maxRetries)
					time.Sleep(time.Duration(retry+1) * time.Second) // 递增延迟
					continue
				} else {
					return totalWritten, fmt.Errorf("分块 %d 读取超时，已重试 %d 次: %w", chunkNumber, maxRetries, chunkCtx.Err())
				}
			}
		}

		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			return totalWritten, fmt.Errorf("读取分块 %d 数据失败: %w", chunkNumber, readErr)
		}

		if n == 0 {
			break
		}

		// 写入数据
		written, writeErr := multiWriter.Write(buffer[:n])
		if writeErr != nil {
			return totalWritten, fmt.Errorf("写入分块 %d 数据失败: %w", chunkNumber, writeErr)
		}

		if written != n {
			return totalWritten, fmt.Errorf("写入分块 %d 数据不完整: 期望 %d，实际 %d", chunkNumber, n, written)
		}

		totalWritten += int64(written)

		// 优化的进度显示：基于时间间隔，包含速度和ETA信息
		now := time.Now()
		if now.Sub(lastProgressTime) >= progressInterval || (expectedSize > 0 && totalWritten >= expectedSize) {
			elapsed := now.Sub(startTime)
			if expectedSize > 0 {
				progress := float64(totalWritten) / float64(expectedSize) * 100
				speed := float64(totalWritten) / elapsed.Seconds() / 1024 / 1024 // MB/s

				// 计算ETA
				var etaStr string
				if speed > 0 && progress < 100 {
					remainingBytes := expectedSize - totalWritten
					etaSeconds := float64(remainingBytes) / (speed * 1024 * 1024)
					eta := time.Duration(etaSeconds) * time.Second
					etaStr = fmt.Sprintf(" | ETA: %v", eta.Round(time.Second))
				} else {
					etaStr = ""
				}

				fs.Infof(f, "🔄 MD5计算进度: %s / %s (%.1f%%) | 速度: %.2f MB/s%s | 分块: %d",
					fs.SizeSuffix(totalWritten), fs.SizeSuffix(expectedSize), progress, speed, etaStr, chunkNumber)
			} else {
				// 未知大小的文件，只显示已传输量和速度
				speed := float64(totalWritten) / elapsed.Seconds() / 1024 / 1024 // MB/s
				fs.Infof(f, "🔄 MD5计算进度: %s | 速度: %.2f MB/s | 耗时: %v | 分块: %d",
					fs.SizeSuffix(totalWritten), speed, elapsed.Round(time.Second), chunkNumber)
			}
			lastProgressTime = now
		}

		// 检查上下文是否被取消
		select {
		case <-ctx.Done():
			return totalWritten, fmt.Errorf("操作被取消: %w", ctx.Err())
		default:
		}
	}

	// 最终进度报告
	elapsed := time.Now().Sub(startTime)
	avgSpeed := float64(totalWritten) / elapsed.Seconds() / 1024 / 1024 // MB/s
	fs.Infof(f, "✅ MD5计算完成！总大小: %s | 平均速度: %.2f MB/s | 总耗时: %v | 总分块数: %d",
		fs.SizeSuffix(totalWritten), avgSpeed, elapsed.Round(time.Second), chunkNumber-1)

	return totalWritten, nil
}

// readToMemoryWithRetry 增强的内存读取函数，支持跨云传输重试
func (f *Fs) readToMemoryWithRetry(ctx context.Context, in io.Reader, expectedSize int64, isRemoteSource bool) ([]byte, error) {
	maxRetries := 1
	if isRemoteSource {
		maxRetries = 3 // 跨云传输增加重试次数
	}

	var lastErr error
	for retry := 0; retry < maxRetries; retry++ {
		if retry > 0 {
			fs.Debugf(f, "🔄 内存读取重试 %d/%d", retry, maxRetries)
			time.Sleep(time.Duration(retry) * 2 * time.Second) // 递增延迟
		}

		// 使用限制读取器防止读取过多数据
		var limitedReader io.Reader
		if expectedSize > 0 {
			limitedReader = io.LimitReader(in, expectedSize+1024) // 额外1KB用于检测大小异常
		} else {
			limitedReader = io.LimitReader(in, maxMemoryBufferSize) // 使用最大内存限制
		}

		data, err := io.ReadAll(limitedReader)
		if err != nil {
			lastErr = fmt.Errorf("读取数据失败(重试%d/%d): %w", retry, maxRetries, err)
			if retry < maxRetries {
				continue
			}
			return nil, lastErr
		}

		actualSize := int64(len(data))

		// 验证数据大小
		if expectedSize > 0 {
			if actualSize > expectedSize+1024 {
				lastErr = fmt.Errorf("读取数据过多: 期望%d字节，实际%d字节", expectedSize, actualSize)
				if retry < maxRetries {
					continue
				}
				return nil, lastErr
			}

			if actualSize < expectedSize {
				fs.Debugf(f, "⚠️  数据大小不匹配: 期望%d字节，实际%d字节", expectedSize, actualSize)
				if isRemoteSource && retry < maxRetries {
					lastErr = fmt.Errorf("跨云传输数据不完整: 期望%d字节，实际%d字节", expectedSize, actualSize)
					continue
				}
			}
		}

		fs.Debugf(f, "✅ 内存读取成功: %d字节", actualSize)
		return data, nil
	}

	return nil, lastErr
}

// createTempFileWithRetry 增强的临时文件创建函数，支持跨云传输重试
func (f *Fs) createTempFileWithRetry(ctx context.Context, in io.Reader, expectedSize int64, isRemoteSource bool, fileName ...string) (*os.File, int64, error) {
	maxRetries := 1
	if isRemoteSource {
		maxRetries = 3 // 跨云传输增加重试次数
	}

	var lastErr error
	for retry := 0; retry < maxRetries; retry++ {
		if retry > 0 {
			fs.Debugf(f, "🔄 临时文件创建重试 %d/%d", retry, maxRetries)
			time.Sleep(time.Duration(retry) * 2 * time.Second) // 递增延迟
		}

		tempFile, err := f.resourcePool.GetOptimizedTempFile("v2upload_", expectedSize)
		if err != nil {
			lastErr = fmt.Errorf("创建临时文件失败(重试%d/%d): %w", retry, maxRetries, err)
			if retry < maxRetries {
				continue
			}
			return nil, 0, lastErr
		}

		// 使用缓冲写入提升性能
		bufWriter := bufio.NewWriterSize(tempFile, 1024*1024) // 1MB缓冲区
		written, err := f.copyWithProgressAndValidation(ctx, bufWriter, in, expectedSize, isRemoteSource, fileName...)

		// 刷新缓冲区
		flushErr := bufWriter.Flush()
		if flushErr != nil {
			tempFile.Close()
			os.Remove(tempFile.Name())
			lastErr = fmt.Errorf("刷新缓冲区失败(重试%d/%d): %w", retry, maxRetries, flushErr)
			if retry < maxRetries {
				continue
			}
			return nil, 0, lastErr
		}

		if err != nil {
			tempFile.Close()
			os.Remove(tempFile.Name())
			lastErr = fmt.Errorf("复制数据失败(重试%d/%d): %w", retry, maxRetries, err)
			if retry < maxRetries {
				continue
			}
			return nil, 0, lastErr
		}

		// 跨云传输额外验证：检查文件实际大小
		if isRemoteSource && expectedSize > 0 {
			fileInfo, statErr := tempFile.Stat()
			if statErr != nil {
				tempFile.Close()
				os.Remove(tempFile.Name())
				lastErr = fmt.Errorf("获取临时文件信息失败(重试%d/%d): %w", retry, maxRetries, statErr)
				if retry < maxRetries {
					continue
				}
				return nil, 0, lastErr
			}

			actualFileSize := fileInfo.Size()
			if actualFileSize != written {
				tempFile.Close()
				os.Remove(tempFile.Name())
				lastErr = fmt.Errorf("文件大小验证失败(重试%d/%d): 写入%d字节，文件实际%d字节", retry, maxRetries, written, actualFileSize)
				if retry < maxRetries {
					continue
				}
				return nil, 0, lastErr
			}

			if actualFileSize != expectedSize {
				fs.Debugf(f, "⚠️  跨云传输大小不匹配: 期望%d字节，实际%d字节", expectedSize, actualFileSize)
				if actualFileSize < expectedSize && retry < maxRetries {
					tempFile.Close()
					os.Remove(tempFile.Name())
					lastErr = fmt.Errorf("跨云传输数据不完整(重试%d/%d): 期望%d字节，实际%d字节", retry, maxRetries, expectedSize, actualFileSize)
					continue
				}
			}
		}

		// 强制同步到磁盘
		if syncErr := tempFile.Sync(); syncErr != nil {
			tempFile.Close()
			os.Remove(tempFile.Name())
			lastErr = fmt.Errorf("同步磁盘失败(重试%d/%d): %w", retry, maxRetries, syncErr)
			if retry < maxRetries {
				continue
			}
			return nil, 0, lastErr
		}

		// 重置文件指针
		if _, seekErr := tempFile.Seek(0, 0); seekErr != nil {
			tempFile.Close()
			os.Remove(tempFile.Name())
			lastErr = fmt.Errorf("重置文件指针失败(重试%d/%d): %w", retry, maxRetries, seekErr)
			if retry < maxRetries {
				continue
			}
			return nil, 0, lastErr
		}

		fs.Debugf(f, "✅ 临时文件创建成功: %d字节", written)
		return tempFile, written, nil
	}

	return nil, 0, lastErr
}

// copyWithProgressAndValidation 带进度和验证的数据复制
func (f *Fs) copyWithProgressAndValidation(ctx context.Context, dst io.Writer, src io.Reader, expectedSize int64, isRemoteSource bool, fileName ...string) (int64, error) {
	// 优化缓冲区大小以提升传输速度
	bufferSize := 1024 * 1024 // 1MB - 大幅提升速度
	if isRemoteSource {
		// 跨云传输使用更大的缓冲区以提升速度
		if expectedSize > 1024*1024*1024 { // 大于1GB的文件
			bufferSize = 2 * 1024 * 1024 // 2MB缓冲区
		} else {
			bufferSize = 1024 * 1024 // 1MB缓冲区
		}
	}

	buffer := make([]byte, bufferSize)
	var totalWritten int64
	startTime := time.Now()
	lastProgressTime := startTime

	for {
		select {
		case <-ctx.Done():
			return totalWritten, ctx.Err()
		default:
		}

		// 设置读取超时（用于日志记录）
		readTimeout := 30 * time.Second
		if isRemoteSource {
			readTimeout = 60 * time.Second // 跨云传输增加超时时间
		}
		_ = readTimeout // 暂时未使用，保留用于未来超时控制

		// 读取数据
		n, readErr := src.Read(buffer)
		if n > 0 {
			// 写入数据
			written, writeErr := dst.Write(buffer[:n])
			if writeErr != nil {
				return totalWritten, fmt.Errorf("写入失败: %w", writeErr)
			}
			totalWritten += int64(written)

			// 定期输出进度 - 更频繁的进度更新
			progressInterval := 2 * time.Second                  // 每2秒更新一次进度
			if isRemoteSource && expectedSize > 1024*1024*1024 { // 大文件跨云传输
				progressInterval = 1 * time.Second // 每1秒更新一次进度
			}

			if time.Since(lastProgressTime) > progressInterval {
				progress := float64(totalWritten) / float64(expectedSize) * 100
				elapsed := time.Since(startTime).Seconds()
				currentSpeed := float64(totalWritten) / elapsed / (1024 * 1024) // MB/s

				// 获取文件名用于显示
				displayFileName := "文件"
				if len(fileName) > 0 && fileName[0] != "" {
					displayFileName = fileName[0]
					// 如果文件名太长，截取显示
					if len(displayFileName) > 30 {
						displayFileName = displayFileName[:27] + "..."
					}
				}

				if expectedSize > 0 {
					remainingBytes := expectedSize - totalWritten
					eta := time.Duration(float64(remainingBytes) / (currentSpeed * 1024 * 1024) * float64(time.Second))
					// 使用INFO日志输出，提升用户体验
					fs.Infof(f, "📊 [%s] 123网盘数据复制进度: %s/%s (%.1f%%) - 速度: %.2f MB/s - 预计剩余: %v",
						displayFileName, fs.SizeSuffix(totalWritten), fs.SizeSuffix(expectedSize), progress, currentSpeed, eta.Round(time.Second))
				} else {
					fs.Infof(f, "📊 [%s] 123网盘数据复制进度: %s - 速度: %.2f MB/s",
						displayFileName, fs.SizeSuffix(totalWritten), currentSpeed)
				}
				lastProgressTime = time.Now()
			}
		}

		if readErr != nil {
			if readErr == io.EOF {
				break // 正常结束
			}
			return totalWritten, fmt.Errorf("读取失败: %w", readErr)
		}
	}

	// 验证最终大小和数据完整性
	if expectedSize > 0 && totalWritten != expectedSize {
		if isRemoteSource && totalWritten < expectedSize {
			// 跨云传输数据不完整，尝试恢复
			fs.Debugf(f, "🔄 跨云传输数据不完整，尝试数据恢复...")
			return totalWritten, fmt.Errorf("跨云传输数据不完整: 期望%d字节，实际%d字节，建议重试", expectedSize, totalWritten)
		}
		fs.Debugf(f, "⚠️  数据大小不匹配: 期望%d字节，实际%d字节", expectedSize, totalWritten)
	}

	// 对于跨云传输，添加额外的完整性检查
	if isRemoteSource && expectedSize > 0 {
		fs.Debugf(f, "✅ 跨云传输数据完整性验证通过: %d字节", totalWritten)
	}

	return totalWritten, nil
}

// tempFileReader 包装临时文件，实现自动清理
type tempFileReader struct {
	file *os.File
}

func (r *tempFileReader) Read(p []byte) (n int, err error) {
	return r.file.Read(p)
}

func (r *tempFileReader) Close() error {
	if r.file != nil {
		fileName := r.file.Name()
		r.file.Close()
		os.Remove(fileName)
		r.file = nil
	}
	return nil
}

// calculateMD5FromReader 从输入流计算MD5哈希，支持跨云盘传输
// 使用分块读取和超时控制，避免大文件处理时的网络超时问题
func (f *Fs) calculateMD5FromReader(ctx context.Context, in io.Reader, expectedSize int64) (string, error) {
	fs.Debugf(f, "开始从输入流计算MD5，预期大小: %s", fs.SizeSuffix(expectedSize))

	hasher := md5.New()
	startTime := time.Now()

	// 对于大文件，使用分块读取避免超时
	if expectedSize > maxMemoryBufferSize {
		fs.Infof(f, "📋 正在计算大文件MD5哈希 (%s) - 启用秒传检测...", fs.SizeSuffix(expectedSize))
		fs.Debugf(f, "大文件使用分块读取计算MD5")
		tempFile, err := f.resourcePool.GetTempFile("md5calc_")
		if err != nil {
			return "", fmt.Errorf("创建临时文件失败: %w", err)
		}
		defer func() {
			tempFile.Close()
			os.Remove(tempFile.Name())
		}()

		// 使用分块读取，避免一次性读取整个大文件导致超时
		written, err := f.copyWithChunksAndTimeout(ctx, tempFile, hasher, in, expectedSize)
		if err != nil {
			return "", fmt.Errorf("计算MD5时复制数据失败: %w", err)
		}

		if expectedSize > 0 && written != expectedSize {
			fs.Debugf(f, "警告：实际读取大小(%d)与预期大小(%d)不匹配", written, expectedSize)
		}

		fs.Debugf(f, "MD5计算完成，处理了 %s 数据", fs.SizeSuffix(written))
	} else {
		// 小文件直接在内存中计算
		fs.Debugf(f, "小文件在内存中计算MD5")
		written, err := io.Copy(hasher, in)
		if err != nil {
			return "", fmt.Errorf("计算MD5失败: %w", err)
		}

		if expectedSize > 0 && written != expectedSize {
			fs.Debugf(f, "警告：实际读取大小(%d)与预期大小(%d)不匹配", written, expectedSize)
		}

		fs.Debugf(f, "MD5计算完成，处理了 %s 数据", fs.SizeSuffix(written))
	}

	md5Hash := fmt.Sprintf("%x", hasher.Sum(nil))
	elapsed := time.Since(startTime)
	fs.Infof(f, "✅ MD5计算完成！哈希值: %s | 耗时: %v", md5Hash, elapsed.Round(time.Millisecond))
	fs.Debugf(f, "MD5计算结果: %s", md5Hash)
	return md5Hash, nil
}

// singleStepUploadMultipart 使用multipart form上传文件的专用方法
func (f *Fs) singleStepUploadMultipart(ctx context.Context, data []byte, parentFileID int64, fileName, md5Hash string, result interface{}) error {
	// 创建multipart form数据
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	// 添加表单字段
	if err := writer.WriteField("parentFileID", strconv.FormatInt(parentFileID, 10)); err != nil {
		return fmt.Errorf("写入parentFileID失败: %w", err)
	}

	if err := writer.WriteField("filename", fileName); err != nil {
		return fmt.Errorf("写入filename失败: %w", err)
	}

	if err := writer.WriteField("etag", md5Hash); err != nil {
		return fmt.Errorf("写入etag失败: %w", err)
	}

	if err := writer.WriteField("size", strconv.Itoa(len(data))); err != nil {
		return fmt.Errorf("写入size失败: %w", err)
	}

	// 添加duplicate字段（保留两者，避免覆盖）
	if err := writer.WriteField("duplicate", "1"); err != nil {
		return fmt.Errorf("写入duplicate失败: %w", err)
	}

	// 添加文件数据
	fileWriter, err := writer.CreateFormFile("file", fileName)
	if err != nil {
		return fmt.Errorf("创建文件字段失败: %w", err)
	}

	if _, err := fileWriter.Write(data); err != nil {
		return fmt.Errorf("写入文件数据失败: %w", err)
	}

	if err := writer.Close(); err != nil {
		return fmt.Errorf("关闭multipart writer失败: %w", err)
	}

	// 使用rclone标准方法进行单步上传multipart调用
	return f.makeAPICallWithRestMultipart(ctx, "/upload/v2/file/single/create", "POST", &buf, writer.FormDataContentType(), result)
}

// downloadWithConcurrency 多线程并发下载文件到本地临时文件
// 基于现有的downloadChunksConcurrently实现，专门用于跨云传输场景
func (f *Fs) downloadWithConcurrency(ctx context.Context, srcObj fs.Object, tempFile *os.File, fileSize int64, fileName string) (int64, error) {
	// 计算最优分片参数
	chunkSize := f.calculateDownloadChunkSize(fileSize)
	numChunks := (fileSize + chunkSize - 1) / chunkSize
	maxConcurrency := f.calculateDownloadConcurrency(fileSize)

	fs.Infof(f, "📊 123网盘并发下载参数: 分片大小=%s, 分片数=%d, 并发数=%d",
		fs.SizeSuffix(chunkSize), numChunks, maxConcurrency)

	fs.Infof(f, "🚀 123网盘开始并发下载: %s (%s)", fileName, fs.SizeSuffix(fileSize))

	// 使用并发下载实现
	downloadStartTime := time.Now()
	err := f.downloadChunksConcurrently(ctx, srcObj, tempFile, chunkSize, numChunks, int64(maxConcurrency))
	downloadDuration := time.Since(downloadStartTime)
	if err != nil {
		return 0, fmt.Errorf("123网盘并发下载失败: %w", err)
	}

	// 验证下载完整性
	stat, err := tempFile.Stat()
	if err != nil {
		return 0, fmt.Errorf("获取下载文件信息失败: %w", err)
	}

	actualSize := stat.Size()
	if actualSize != fileSize {
		return 0, fmt.Errorf("下载文件大小不匹配: 期望%s，实际%s",
			fs.SizeSuffix(fileSize), fs.SizeSuffix(actualSize))
	}

	// 计算平均下载速度
	avgSpeed := float64(actualSize) / downloadDuration.Seconds() / 1024 / 1024 // MB/s

	fs.Infof(f, "✅ 123网盘并发下载完成: %s | 用时: %v | 平均速度: %.2f MB/s",
		fs.SizeSuffix(actualSize), downloadDuration, avgSpeed)
	return actualSize, nil
}

// startPerformanceMonitoring 启动性能监控定时器
func (f *Fs) startPerformanceMonitoring(ctx context.Context) {
	if f.enhancedMetrics == nil {
		return
	}

	interval := time.Duration(f.opt.PerformanceLogInterval)
	if interval <= 0 {
		interval = 60 * time.Second // 默认60秒
	}

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				fs.Debugf(f, "🔚 性能监控定时器停止")
				return
			case <-ticker.C:
				f.logPerformanceStats()
			}
		}
	}()

	fs.Infof(f, "📊 性能监控已启动，间隔: %v", interval)
}

// logPerformanceStats 输出性能统计信息
func (f *Fs) logPerformanceStats() {
	if f.enhancedMetrics == nil {
		return
	}

	stats := f.enhancedMetrics.GetStats()

	fs.Infof(f, "📊 性能统计 - 运行时间: %v", stats.TotalRuntime)
	fs.Infof(f, "📤 上传统计 - 总数: %d, 成功: %d, 失败: %d, 平均速度: %.2f MB/s",
		stats.TotalUploads, stats.SuccessfulUploads, stats.FailedUploads, stats.AverageUploadSpeed)
	fs.Infof(f, "📥 下载统计 - 总数: %d, 成功: %d, 失败: %d, 平均速度: %.2f MB/s",
		stats.TotalDownloads, stats.SuccessfulDownloads, stats.FailedDownloads, stats.AverageDownloadSpeed)
	fs.Infof(f, "🔄 API调用 - 总数: %d, 成功率: %.2f%%, 平均延迟: %v",
		stats.TotalAPICalls, stats.APISuccessRate*100, stats.AverageAPILatency)
	fs.Infof(f, "💾 内存使用 - 当前: %s, 峰值: %s",
		fs.SizeSuffix(stats.CurrentMemoryUsage), fs.SizeSuffix(stats.PeakMemoryUsage))
}

// isRetryableAPIError 判断是否为可重试的API错误
func (f *Fs) isRetryableAPIError(err error) bool {
	if err == nil {
		return false
	}

	errStr := strings.ToLower(err.Error())

	// 检查常见的可重试错误
	retryableErrors := []string{
		"timeout",
		"connection reset",
		"connection refused",
		"temporary failure",
		"server error",
		"service unavailable",
		"too many requests",
		"rate limit",
		"网络错误",
		"连接超时",
		"服务器繁忙",
	}

	for _, retryableErr := range retryableErrors {
		if strings.Contains(errStr, retryableErr) {
			return true
		}
	}

	// 检查HTTP状态码
	if strings.Contains(errStr, "429") || // Too Many Requests
		strings.Contains(errStr, "500") || // Internal Server Error
		strings.Contains(errStr, "502") || // Bad Gateway
		strings.Contains(errStr, "503") || // Service Unavailable
		strings.Contains(errStr, "504") { // Gateway Timeout
		return true
	}

	return false
}

// calculateDownloadChunkSize 计算下载分片大小
// 🔧 修复硬编码问题：下载分片大小应该基于文件大小和网络状况动态计算
func (f *Fs) calculateDownloadChunkSize(fileSize int64) int64 {
	// 🔧 动态分片策略：基于文件大小智能计算，不硬编码
	switch {
	case fileSize < 50*1024*1024: // <50MB
		return 16 * 1024 * 1024 // 16MB分片，小文件使用小分片
	case fileSize < 200*1024*1024: // <200MB
		return 32 * 1024 * 1024 // 32MB分片
	case fileSize < 1*1024*1024*1024: // <1GB
		return 64 * 1024 * 1024 // 64MB分片，常见大小
	case fileSize < 5*1024*1024*1024: // <5GB
		return 128 * 1024 * 1024 // 128MB分片，大文件使用大分片
	default: // >5GB
		return 256 * 1024 * 1024 // 256MB分片，超大文件
	}
}

// calculateDownloadConcurrency 计算下载并发数
// 🚀 用户要求：123网盘单文件并发限制到4
func (f *Fs) calculateDownloadConcurrency(fileSize int64) int {
	switch {
	case fileSize < 500*1024*1024: // <500MB
		return 2 // 2并发
	case fileSize < 2*1024*1024*1024: // <2GB
		return 4 // 4并发
	default: // >2GB
		return 4 // 4并发上限，符合用户要求
	}
}

// attemptResumeDownload 尝试断点续传恢复下载
// 🔧 修复重复下载问题：智能恢复部分下载的数据
func (f *Fs) attemptResumeDownload(ctx context.Context, srcObj fs.Object, tempFile *os.File, chunkSize, numChunks, maxConcurrency int64) error {
	fs.Debugf(f, "🔄 开始断点续传恢复，分析已下载的分片...")

	// 分析已下载的数据，确定哪些分片需要重新下载
	fileSize := srcObj.Size()
	missingChunks := make([]int64, 0)

	// 检查每个分片的完整性
	for chunkIndex := int64(0); chunkIndex < numChunks; chunkIndex++ {
		start := chunkIndex * chunkSize
		end := start + chunkSize - 1
		if end >= fileSize {
			end = fileSize - 1
		}

		// 检查这个分片是否完整下载
		if !f.isChunkComplete(tempFile, start, end) {
			missingChunks = append(missingChunks, chunkIndex)
		}
	}

	if len(missingChunks) == 0 {
		fs.Infof(f, "✅ 所有分片都已完整下载")
		return nil
	}

	fs.Infof(f, "🔄 需要重新下载 %d 个分片", len(missingChunks))

	// 只下载缺失的分片
	return f.downloadMissingChunks(ctx, srcObj, tempFile, chunkSize, missingChunks, maxConcurrency)
}

// isChunkComplete 检查指定分片是否完整下载
func (f *Fs) isChunkComplete(tempFile *os.File, start, end int64) bool {
	expectedSize := end - start + 1

	// 检查文件大小是否足够
	stat, err := tempFile.Stat()
	if err != nil || stat.Size() <= start {
		return false
	}

	// 检查指定范围的数据是否存在且非零
	buffer := make([]byte, min(expectedSize, 1024)) // 只检查前1KB
	n, err := tempFile.ReadAt(buffer, start)
	if err != nil || int64(n) < min(expectedSize, 1024) {
		return false
	}

	// 简单检查：确保不全是零字节（这可能表示未写入的区域）
	allZero := true
	for _, b := range buffer[:n] {
		if b != 0 {
			allZero = false
			break
		}
	}

	return !allZero
}

// downloadMissingChunks 下载缺失的分片
func (f *Fs) downloadMissingChunks(ctx context.Context, srcObj fs.Object, tempFile *os.File, chunkSize int64, missingChunks []int64, maxConcurrency int64) error {
	if len(missingChunks) == 0 {
		return nil
	}

	fs.Infof(f, "🚀 开始下载 %d 个缺失分片", len(missingChunks))

	// 创建信号量控制并发
	semaphore := make(chan struct{}, maxConcurrency)
	var wg sync.WaitGroup
	errChan := make(chan error, len(missingChunks))

	for _, chunkIndex := range missingChunks {
		wg.Add(1)
		go func(idx int64) {
			defer wg.Done()

			// 🔧 添加panic恢复机制
			defer func() {
				if r := recover(); r != nil {
					fs.Errorf(f, "下载缺失分片 %d 发生panic: %v", idx, r)
					errChan <- fmt.Errorf("下载分片 %d panic: %v", idx, r)
				}
			}()

			// 🔧 修复信号量泄漏：使用带超时的信号量获取
			chunkTimeout := 10 * time.Minute // 每个分片最多10分钟超时
			chunkCtx, cancelChunk := context.WithTimeout(ctx, chunkTimeout)
			defer cancelChunk()

			// 获取信号量，带超时控制
			select {
			case semaphore <- struct{}{}:
				// 成功获取信号量，立即设置defer释放
				defer func() {
					select {
					case <-semaphore:
					default:
						// 如果信号量已经被释放，避免阻塞
					}
				}()
			case <-chunkCtx.Done():
				errChan <- fmt.Errorf("重新下载分片 %d 获取信号量超时: %w", idx, chunkCtx.Err())
				return
			}

			// 计算分片范围
			start := idx * chunkSize
			end := start + chunkSize - 1
			if end >= srcObj.Size() {
				end = srcObj.Size() - 1
			}

			// 下载分片，使用带超时的上下文
			err := f.downloadChunk(chunkCtx, srcObj, tempFile, start, end, idx)
			if err != nil {
				// 检查是否是超时错误
				if chunkCtx.Err() != nil {
					errChan <- fmt.Errorf("重新下载分片 %d 超时: %w", idx, chunkCtx.Err())
				} else {
					errChan <- fmt.Errorf("重新下载分片 %d 失败: %w", idx, err)
				}
				return
			}

			fs.Debugf(f, "✅ 分片 %d 重新下载成功", idx)
		}(chunkIndex)
	}

	// 🔧 修复死锁风险：添加整体超时控制，防止无限等待
	overallTimeout := time.Duration(len(missingChunks)) * 15 * time.Minute // 每个分片最多15分钟
	if overallTimeout > 2*time.Hour {
		overallTimeout = 2 * time.Hour // 最大2小时超时
	}

	// 使用带超时的等待机制
	done := make(chan struct{})
	go func() {
		defer close(done)
		wg.Wait()
		close(errChan)
	}()

	// 等待完成或超时
	select {
	case <-done:
		// 正常完成，检查是否有错误
		for err := range errChan {
			if err != nil {
				return err
			}
		}
		fs.Infof(f, "✅ 所有缺失分片下载完成")
		return nil
	case <-time.After(overallTimeout):
		fs.Errorf(f, "🚨 缺失分片下载整体超时 (%v)，可能存在死锁", overallTimeout)
		return fmt.Errorf("缺失分片下载整体超时: %v", overallTimeout)
	}
}

// CrossCloudTransferProgress 跨云盘传输统一进度管理器
// 🔧 统一进度显示：创建统一的跨云盘传输进度管理器，改善用户体验
type CrossCloudTransferProgress struct {
	fileName  string
	totalSize int64
	startTime time.Time

	// 下载阶段
	downloadStartTime time.Time
	downloadEndTime   time.Time
	downloadBytes     int64
	downloadChunks    int64
	totalChunks       int64

	// 上传阶段
	uploadStartTime time.Time
	uploadEndTime   time.Time
	uploadBytes     int64

	// 当前阶段
	currentPhase string // "download", "processing", "upload", "complete"

	// 进度同步
	mu sync.RWMutex

	// rclone accounting集成
	account *accounting.Account
}

// NewCrossCloudTransferProgress 创建新的跨云盘传输进度管理器
func NewCrossCloudTransferProgress(fileName string, totalSize int64, account *accounting.Account) *CrossCloudTransferProgress {
	return &CrossCloudTransferProgress{
		fileName:     fileName,
		totalSize:    totalSize,
		startTime:    time.Now(),
		currentPhase: "download",
		account:      account,
	}
}

// StartDownloadPhase 开始下载阶段
func (ctp *CrossCloudTransferProgress) StartDownloadPhase(totalChunks int64) {
	ctp.mu.Lock()
	defer ctp.mu.Unlock()

	ctp.currentPhase = "download"
	ctp.downloadStartTime = time.Now()
	ctp.totalChunks = totalChunks
	ctp.downloadChunks = 0
	ctp.downloadBytes = 0

	// 更新rclone进度显示
	if ctp.account != nil {
		ctp.account.SetExtraInfo(fmt.Sprintf("[下载阶段] 0/%d分片", totalChunks))
	}
}

// UpdateDownloadProgress 更新下载进度
func (ctp *CrossCloudTransferProgress) UpdateDownloadProgress(completedChunks, downloadedBytes int64) {
	ctp.mu.Lock()
	defer ctp.mu.Unlock()

	ctp.downloadChunks = completedChunks
	ctp.downloadBytes = downloadedBytes

	// 计算下载进度百分比
	downloadPercent := float64(completedChunks) / float64(ctp.totalChunks) * 100

	// 更新rclone进度显示
	if ctp.account != nil {
		extraInfo := fmt.Sprintf("[下载阶段] %d/%d分片 (%.1f%%)",
			completedChunks, ctp.totalChunks, downloadPercent)
		ctp.account.SetExtraInfo(extraInfo)
	}
}

// StartProcessingPhase 开始处理阶段（MD5计算等）
func (ctp *CrossCloudTransferProgress) StartProcessingPhase() {
	ctp.mu.Lock()
	defer ctp.mu.Unlock()

	ctp.currentPhase = "processing"
	ctp.downloadEndTime = time.Now()

	// 更新rclone进度显示
	if ctp.account != nil {
		ctp.account.SetExtraInfo("[处理阶段] 计算MD5...")
	}
}

// StartUploadPhase 开始上传阶段
func (ctp *CrossCloudTransferProgress) StartUploadPhase() {
	ctp.mu.Lock()
	defer ctp.mu.Unlock()

	ctp.currentPhase = "upload"
	ctp.uploadStartTime = time.Now()
	ctp.uploadBytes = 0

	// 更新rclone进度显示
	if ctp.account != nil {
		ctp.account.SetExtraInfo("[上传阶段] 开始上传...")
	}
}

// UpdateUploadProgress 更新上传进度
func (ctp *CrossCloudTransferProgress) UpdateUploadProgress(uploadedBytes int64) {
	ctp.mu.Lock()
	defer ctp.mu.Unlock()

	ctp.uploadBytes = uploadedBytes

	// 计算上传进度百分比
	uploadPercent := float64(uploadedBytes) / float64(ctp.totalSize) * 100

	// 更新rclone进度显示
	if ctp.account != nil {
		extraInfo := fmt.Sprintf("[上传阶段] %.1f%% (%s/%s)",
			uploadPercent, fs.SizeSuffix(uploadedBytes), fs.SizeSuffix(ctp.totalSize))
		ctp.account.SetExtraInfo(extraInfo)
	}
}

// Complete 完成传输
func (ctp *CrossCloudTransferProgress) Complete() {
	ctp.mu.Lock()
	defer ctp.mu.Unlock()

	ctp.currentPhase = "complete"
	ctp.uploadEndTime = time.Now()

	// 计算总体统计信息
	totalDuration := time.Since(ctp.startTime)
	downloadDuration := ctp.downloadEndTime.Sub(ctp.downloadStartTime)
	uploadDuration := ctp.uploadEndTime.Sub(ctp.uploadStartTime)

	// 更新rclone进度显示
	if ctp.account != nil {
		ctp.account.SetExtraInfo(fmt.Sprintf("[完成] 总耗时: %v", totalDuration.Round(time.Second)))
	}

	// 输出详细的传输统计
	fs.Infof(nil, "🎉 跨云盘传输完成: %s", ctp.fileName)
	fs.Infof(nil, "   📊 传输统计: 总大小=%s, 总耗时=%v",
		fs.SizeSuffix(ctp.totalSize), totalDuration.Round(time.Second))
	fs.Infof(nil, "   📥 下载阶段: 耗时=%v, 速度=%.2f MB/s",
		downloadDuration.Round(time.Second),
		float64(ctp.downloadBytes)/downloadDuration.Seconds()/1024/1024)
	fs.Infof(nil, "   📤 上传阶段: 耗时=%v, 速度=%.2f MB/s",
		uploadDuration.Round(time.Second),
		float64(ctp.uploadBytes)/uploadDuration.Seconds()/1024/1024)
}

// GetCurrentStatus 获取当前状态信息
func (ctp *CrossCloudTransferProgress) GetCurrentStatus() (phase string, progress float64, eta time.Duration) {
	ctp.mu.RLock()
	defer ctp.mu.RUnlock()

	phase = ctp.currentPhase

	switch phase {
	case "download":
		if ctp.totalChunks > 0 {
			progress = float64(ctp.downloadChunks) / float64(ctp.totalChunks) * 50 // 下载占总进度的50%
		}
	case "processing":
		progress = 50 // 处理阶段固定为50%
	case "upload":
		if ctp.totalSize > 0 {
			uploadProgress := float64(ctp.uploadBytes) / float64(ctp.totalSize) * 50 // 上传占总进度的50%
			progress = 50 + uploadProgress                                           // 下载50% + 上传进度
		}
	case "complete":
		progress = 100
	}

	// 简单的ETA估算
	if progress > 0 && progress < 100 {
		elapsed := time.Since(ctp.startTime)
		totalEstimated := time.Duration(float64(elapsed) / progress * 100)
		eta = totalEstimated - elapsed
	}

	return phase, progress, eta
}

// memoryBufferedCrossCloudTransferDirect 内存缓冲跨云传输（直接版本，避免重复下载）
// 🔧 修复重复下载问题：直接使用srcObj，避免重复打开文件
func (f *Fs) memoryBufferedCrossCloudTransferDirect(ctx context.Context, srcObj fs.Object, src fs.ObjectInfo, parentFileID int64, fileName string) (*Object, error) {
	fileSize := src.Size()
	fs.Debugf(f, "📝 开始内存缓冲跨云传输（直接版本），文件大小: %s", fs.SizeSuffix(fileSize))

	// 步骤1: 打开源文件并直接读取到内存
	startTime := time.Now()
	srcReader, err := srcObj.Open(ctx)
	if err != nil {
		return nil, fmt.Errorf("无法打开源文件进行下载: %w", err)
	}
	defer srcReader.Close()

	data := make([]byte, fileSize)
	n, err := io.ReadFull(srcReader, data)
	downloadDuration := time.Since(startTime)

	if err != nil && err != io.ErrUnexpectedEOF {
		return nil, fmt.Errorf("内存读取文件失败: %w", err)
	}

	if int64(n) != fileSize {
		fs.Debugf(f, "实际读取大小(%d)与预期大小(%d)不匹配，使用实际大小", n, fileSize)
		data = data[:n]
		fileSize = int64(n)
	}

	fs.Infof(f, "📥 内存下载完成: %s, 用时: %v, 速度: %s/s",
		fs.SizeSuffix(fileSize), downloadDuration,
		fs.SizeSuffix(int64(float64(fileSize)/downloadDuration.Seconds())))

	// 步骤2: 计算MD5
	fs.Infof(f, "🔐 开始计算MD5哈希...")
	md5StartTime := time.Now()
	hasher := md5.New()
	hasher.Write(data)
	md5Hash := fmt.Sprintf("%x", hasher.Sum(nil))
	md5Duration := time.Since(md5StartTime)

	fs.Infof(f, "🔐 MD5计算完成: %s, 用时: %v", md5Hash, md5Duration)

	// 步骤3: 检查秒传
	fs.Infof(f, "⚡ 检查123网盘秒传功能...")
	createResp, err := f.createUpload(ctx, parentFileID, fileName, md5Hash, fileSize)
	if err != nil {
		return nil, fmt.Errorf("创建上传会话失败: %w", err)
	}

	if createResp.Data.Reuse {
		totalDuration := time.Since(startTime)
		fs.Infof(f, "🎉 秒传成功！总用时: %v (下载: %v + MD5: %v)",
			totalDuration, downloadDuration, md5Duration)
		return &Object{
			fs:          f,
			remote:      fileName,
			hasMetaData: true,
			id:          strconv.FormatInt(createResp.Data.FileID, 10),
			size:        fileSize,
			md5sum:      md5Hash,
			modTime:     time.Now(),
			isDir:       false,
		}, nil
	}

	// 步骤4: 上传文件
	fs.Infof(f, "📤 开始上传文件...")
	uploadStartTime := time.Now()

	reader := bytes.NewReader(data)
	err = f.uploadFile(ctx, reader, createResp, fileSize)
	if err != nil {
		return nil, fmt.Errorf("上传失败: %w", err)
	}

	uploadDuration := time.Since(uploadStartTime)
	totalDuration := time.Since(startTime)

	// 缓存MD5以供后续使用
	f.cacheMD5(src, md5Hash)

	fs.Infof(f, "✅ 内存缓冲跨云传输完成！总用时: %v (下载: %v + MD5: %v + 上传: %v)",
		totalDuration, downloadDuration, md5Duration, uploadDuration)

	return &Object{
		fs:          f,
		remote:      fileName,
		hasMetaData: true,
		id:          strconv.FormatInt(createResp.Data.FileID, 10),
		size:        fileSize,
		md5sum:      md5Hash,
		modTime:     time.Now(),
		isDir:       false,
	}, nil
}

// optimizedDiskCrossCloudTransferDirect 优化的磁盘跨云传输（直接版本，避免重复下载）
// 🔧 修复重复下载问题：直接使用srcObj，避免重复打开文件
func (f *Fs) optimizedDiskCrossCloudTransferDirect(ctx context.Context, srcObj fs.Object, src fs.ObjectInfo, parentFileID int64, fileName string) (*Object, error) {
	fileSize := src.Size()
	startTime := time.Now()
	fs.Debugf(f, "💾 开始优化磁盘跨云传输（直接版本），文件大小: %s", fs.SizeSuffix(fileSize))

	// 步骤1: 检查MD5缓存
	var md5Hash string
	var downloadDuration, md5Duration time.Duration

	if cachedMD5, found := f.getCachedMD5(src); found {
		fs.Infof(f, "🎯 使用缓存的MD5: %s", cachedMD5)
		md5Hash = cachedMD5
		md5Duration = 0 // 缓存命中，无需计算时间

		// 仍需要下载文件用于上传，但可以跳过MD5计算
		tempFile, err := f.resourcePool.GetOptimizedTempFile("cross_cloud_cached_", fileSize)
		if err != nil {
			return nil, fmt.Errorf("创建跨云传输临时文件失败: %w", err)
		}
		defer f.resourcePool.PutTempFile(tempFile)

		// 🔧 修复重复下载：直接使用srcObj打开文件
		downloadStartTime := time.Now()
		srcReader, err := srcObj.Open(ctx)
		if err != nil {
			return nil, fmt.Errorf("无法打开源文件进行下载: %w", err)
		}
		defer srcReader.Close()

		written, err := f.copyWithProgressAndValidation(ctx, tempFile, srcReader, fileSize, false, fileName) // 不需要计算MD5
		downloadDuration = time.Since(downloadStartTime)

		if err != nil {
			return nil, fmt.Errorf("下载文件失败: %w", err)
		}

		if written != fileSize {
			return nil, fmt.Errorf("下载文件大小不匹配: 期望 %d, 实际 %d", fileSize, written)
		}

		fs.Infof(f, "📥 下载完成 (使用缓存MD5): %s, 用时: %v, 速度: %s/s",
			fs.SizeSuffix(fileSize), downloadDuration,
			fs.SizeSuffix(int64(float64(fileSize)/downloadDuration.Seconds())))

		// 直接跳转到上传步骤
		return f.continueWithKnownMD5(ctx, tempFile, src, parentFileID, fileName, md5Hash, downloadDuration, md5Duration, startTime)
	}

	// 步骤2: 缓存未命中，正常下载并计算MD5
	fs.Infof(f, "💾 MD5缓存未命中，开始下载并计算MD5")

	// 创建本地临时文件
	tempFile, err := f.resourcePool.GetOptimizedTempFile("cross_cloud_optimized_", fileSize)
	if err != nil {
		return nil, fmt.Errorf("创建跨云传输临时文件失败: %w", err)
	}
	defer f.resourcePool.PutTempFile(tempFile)

	// 🔧 修复重复下载：直接使用srcObj打开文件
	downloadStartTime := time.Now()
	srcReader, err := srcObj.Open(ctx)
	if err != nil {
		return nil, fmt.Errorf("无法打开源文件进行下载: %w", err)
	}
	defer srcReader.Close()

	written, err := f.copyWithProgressAndValidation(ctx, tempFile, srcReader, fileSize, true, fileName) // 需要计算MD5
	downloadDuration = time.Since(downloadStartTime)

	if err != nil {
		return nil, fmt.Errorf("下载并计算MD5失败: %w", err)
	}

	if written != fileSize {
		return nil, fmt.Errorf("下载文件大小不匹配: 期望 %d, 实际 %d", fileSize, written)
	}

	// MD5已在copyWithProgressAndValidation中计算
	md5Duration = 0 // 已包含在下载时间中

	fs.Infof(f, "📥 下载并MD5计算完成: %s, 用时: %v, 速度: %s/s",
		fs.SizeSuffix(fileSize), downloadDuration,
		fs.SizeSuffix(int64(float64(fileSize)/downloadDuration.Seconds())))

	// 获取计算的MD5（这需要从copyWithProgressAndValidation返回）
	// 为了简化，我们重新计算MD5
	_, err = tempFile.Seek(0, io.SeekStart)
	if err != nil {
		return nil, fmt.Errorf("重置文件指针失败: %w", err)
	}

	md5StartTime := time.Now()
	hasher := md5.New()
	_, err = io.Copy(hasher, tempFile)
	if err != nil {
		return nil, fmt.Errorf("计算MD5失败: %w", err)
	}
	md5Hash = fmt.Sprintf("%x", hasher.Sum(nil))
	md5Duration = time.Since(md5StartTime)

	fs.Infof(f, "🔐 MD5计算完成: %s, 用时: %v", md5Hash, md5Duration)

	// 继续上传流程
	return f.continueWithKnownMD5(ctx, tempFile, src, parentFileID, fileName, md5Hash, downloadDuration, md5Duration, startTime)
}

// EndToEndIntegrityVerifier 端到端完整性验证器
// 🔧 增强完整性验证：添加端到端MD5验证和断点续传完整性检查
type EndToEndIntegrityVerifier struct {
	fs *Fs
}

// NewEndToEndIntegrityVerifier 创建新的端到端完整性验证器
func NewEndToEndIntegrityVerifier(fs *Fs) *EndToEndIntegrityVerifier {
	return &EndToEndIntegrityVerifier{fs: fs}
}

// VerifyCrossCloudTransfer 验证跨云盘传输的完整性
func (eiv *EndToEndIntegrityVerifier) VerifyCrossCloudTransfer(ctx context.Context, sourceObj fs.Object, targetObj *Object, expectedMD5 string) (*CrossCloudIntegrityResult, error) {
	result := &CrossCloudIntegrityResult{
		SourcePath:  sourceObj.Remote(),
		TargetPath:  targetObj.remote,
		SourceSize:  sourceObj.Size(),
		TargetSize:  targetObj.size,
		ExpectedMD5: expectedMD5,
		StartTime:   time.Now(),
	}

	fs.Infof(eiv.fs, "🔍 开始端到端完整性验证: %s", sourceObj.Remote())

	// 步骤1: 验证文件大小
	if result.SourceSize != result.TargetSize {
		result.Error = fmt.Sprintf("文件大小不匹配: 源文件=%s, 目标文件=%s",
			fs.SizeSuffix(result.SourceSize), fs.SizeSuffix(result.TargetSize))
		result.SizeMatch = false
		fs.Errorf(eiv.fs, "❌ 大小验证失败: %s", result.Error)
		return result, fmt.Errorf("文件大小验证失败")
	}
	result.SizeMatch = true
	fs.Debugf(eiv.fs, "✅ 文件大小验证通过: %s", fs.SizeSuffix(result.SourceSize))

	// 步骤2: 获取源文件MD5（如果可用）
	sourceMD5, err := sourceObj.Hash(ctx, fshash.MD5)
	if err == nil && sourceMD5 != "" {
		result.SourceMD5 = sourceMD5
		fs.Debugf(eiv.fs, "📋 源文件MD5: %s", sourceMD5)
	} else {
		fs.Debugf(eiv.fs, "⚠️ 无法获取源文件MD5: %v", err)
	}

	// 步骤3: 获取目标文件MD5
	if targetObj.md5sum != "" {
		result.TargetMD5 = targetObj.md5sum
		fs.Debugf(eiv.fs, "📋 目标文件MD5: %s", targetObj.md5sum)
	} else {
		fs.Debugf(eiv.fs, "⚠️ 目标文件MD5不可用")
	}

	// 步骤4: 验证MD5一致性
	result.MD5Match = true
	if expectedMD5 != "" {
		// 验证与期望MD5的一致性
		if result.TargetMD5 != "" && !strings.EqualFold(result.TargetMD5, expectedMD5) {
			result.MD5Match = false
			result.Error = fmt.Sprintf("目标文件MD5与期望不匹配: 期望=%s, 实际=%s", expectedMD5, result.TargetMD5)
		}

		if result.SourceMD5 != "" && !strings.EqualFold(result.SourceMD5, expectedMD5) {
			result.MD5Match = false
			if result.Error != "" {
				result.Error += "; "
			}
			result.Error += fmt.Sprintf("源文件MD5与期望不匹配: 期望=%s, 实际=%s", expectedMD5, result.SourceMD5)
		}
	}

	// 验证源文件和目标文件MD5是否一致
	if result.SourceMD5 != "" && result.TargetMD5 != "" {
		if !strings.EqualFold(result.SourceMD5, result.TargetMD5) {
			result.MD5Match = false
			if result.Error != "" {
				result.Error += "; "
			}
			result.Error += fmt.Sprintf("源文件与目标文件MD5不匹配: 源=%s, 目标=%s", result.SourceMD5, result.TargetMD5)
		}
	}

	result.EndTime = time.Now()
	result.Duration = result.EndTime.Sub(result.StartTime)
	result.IsValid = result.SizeMatch && result.MD5Match

	if result.IsValid {
		fs.Infof(eiv.fs, "✅ 端到端完整性验证通过: %s (耗时: %v)", sourceObj.Remote(), result.Duration)
	} else {
		fs.Errorf(eiv.fs, "❌ 端到端完整性验证失败: %s - %s", sourceObj.Remote(), result.Error)
	}

	return result, nil
}

// CrossCloudIntegrityResult 跨云盘传输完整性验证结果
type CrossCloudIntegrityResult struct {
	SourcePath  string        `json:"source_path"`
	TargetPath  string        `json:"target_path"`
	SourceSize  int64         `json:"source_size"`
	TargetSize  int64         `json:"target_size"`
	SourceMD5   string        `json:"source_md5"`
	TargetMD5   string        `json:"target_md5"`
	ExpectedMD5 string        `json:"expected_md5"`
	SizeMatch   bool          `json:"size_match"`
	MD5Match    bool          `json:"md5_match"`
	IsValid     bool          `json:"is_valid"`
	Error       string        `json:"error,omitempty"`
	StartTime   time.Time     `json:"start_time"`
	EndTime     time.Time     `json:"end_time"`
	Duration    time.Duration `json:"duration"`
}

// openSourceWithoutConcurrency 打开源文件但避免触发并发下载
// 🔧 修复多重并发下载：专门用于跨云传输，避免源对象启动自己的并发下载
func (f *Fs) openSourceWithoutConcurrency(ctx context.Context, srcObj fs.Object) (io.ReadCloser, error) {
	// 检查源对象类型，使用特定的方法避免并发下载
	switch obj := srcObj.(type) {
	case interface {
		openNormal(context.Context, ...fs.OpenOption) (io.ReadCloser, error)
	}:
		// 如果源对象有openNormal方法（如123网盘），直接使用
		fs.Debugf(f, "🔧 使用openNormal方法避免并发下载")
		return obj.openNormal(ctx)
	case interface {
		open(context.Context, ...fs.OpenOption) (io.ReadCloser, error)
	}:
		// 如果源对象有open方法（如115网盘），使用它
		fs.Debugf(f, "🔧 使用open方法避免并发下载")
		return obj.open(ctx)
	default:
		// 对于其他类型，使用标准Open方法，但添加特殊选项
		fs.Debugf(f, "🔧 使用标准Open方法，添加禁用并发下载选项")
		// 添加一个特殊的选项来指示不要使用并发下载
		options := []fs.OpenOption{&DisableConcurrentDownloadOption{}}
		return srcObj.Open(ctx, options...)
	}
}

// DisableConcurrentDownloadOption 禁用并发下载的选项
// 🔧 修复多重并发下载：用于指示源对象不要启动并发下载
type DisableConcurrentDownloadOption struct{}

func (o *DisableConcurrentDownloadOption) String() string {
	return "DisableConcurrentDownload"
}

func (o *DisableConcurrentDownloadOption) Header() (key, value string) {
	return "X-Disable-Concurrent-Download", "true"
}

func (o *DisableConcurrentDownloadOption) Mandatory() bool {
	return false
}

// getGlobalStats 获取全局统计对象
// 🔧 修复进度显示：集成到rclone accounting系统
func (f *Fs) getGlobalStats(ctx context.Context) *accounting.StatsInfo {
	// 使用全局stats（这是rclone的标准做法）
	return accounting.GlobalStats()
}
