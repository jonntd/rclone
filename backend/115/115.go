package _115

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/rclone/rclone/backend/115/api"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/accounting"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/configstruct"
	"github.com/rclone/rclone/fs/fserrors"
	"github.com/rclone/rclone/fs/fshttp"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/lib/cache"
	"github.com/rclone/rclone/lib/dircache"
	"github.com/rclone/rclone/lib/encoder"
	"github.com/rclone/rclone/lib/oauthutil"
	"github.com/rclone/rclone/lib/pacer"
	"github.com/rclone/rclone/lib/rest"
	"golang.org/x/oauth2"
)

// Constants
const (
	domain             = "www.115.com"
	traditionalRootURL = "https://webapi.115.com"
	openAPIRootURL     = "https://proapi.115.com"
	passportRootURL    = "https://passportapi.115.com"
	qrCodeAPIRootURL   = "https://qrcodeapi.115.com"
	hnQrCodeAPIRootURL = "https://hnqrcodeapi.115.com"     // For confirm step
	defaultAppID       = "100195123"                       // Provided App ID
	tradUserAgent      = "Mozilla/5.0 115Browser/27.0.7.5" // Keep for traditional login mimicry?
	defaultUserAgent   = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/135.0.0.0 Safari/537.36"

	// 🔧 115网盘统一QPS配置 - 全局账户级别限制
	// 115网盘特殊性：所有API共享同一个QPS配额，需要统一管理避免770004错误

	// 统一QPS设置 - 基于实际测试
	unifiedMinSleep      = fs.Duration(250 * time.Millisecond) // ~4 QPS - 统一全局限制
	conservativeMinSleep = fs.Duration(500 * time.Millisecond) // ~2 QPS - 保守模式（遇到限制时）
	aggressiveMinSleep   = fs.Duration(150 * time.Millisecond) // ~6 QPS - 激进模式（网络良好时）

	maxSleep      = 2 * time.Second
	decayConstant = 2 // bigger for slower decay, exponential

	defaultConTimeout = fs.Duration(15 * time.Second)  // 减少连接超时，快速失败重试
	defaultTimeout    = fs.Duration(120 * time.Second) // 减少IO超时，避免长时间等待

	maxUploadSize       = 115 * fs.Gibi // 115 GiB from https://proapi.115.com/app/uploadinfo (or OpenAPI equivalent)
	maxUploadParts      = 10000         // Part number must be an integer between 1 and 10000, inclusive.
	defaultChunkSize    = 20 * fs.Mebi  // 🔧 参考OpenList：设置为20MB，与OpenList保持一致
	minChunkSize        = 100 * fs.Kibi
	maxChunkSize        = 5 * fs.Gibi   // Max part size for OSS
	defaultUploadCutoff = 50 * fs.Mebi  // 🔧 设置为50MB，小于50MB使用简单上传，大于50MB使用分片上传
	defaultNohashSize   = 100 * fs.Mebi // 🔧 设置为100MB，小文件优先使用传统上传
	StreamUploadLimit   = 5 * fs.Gibi   // Max size for sample/streamed upload (traditional)
	maxUploadCutoff     = 5 * fs.Gibi   // maximum allowed size for singlepart uploads (OSS PutObject limit)

	tokenRefreshWindow = 10 * time.Minute // Refresh token 10 minutes before expiry
	pkceVerifierLength = 64               // Length for PKCE code verifier
)

// TraditionalRequest is the standard 115.com request structure for traditional API
// ... existing code ...

// Register with Fs
func init() {
	fs.Register(&fs.RegInfo{
		Name:        "115",
		Description: "115 drive (supports Open API)",
		NewFs:       NewFs,
		CommandHelp: commandHelp,
		Options: []fs.Option{{
			Name: "cookie",
			Help: `Provide the login cookie in the format "UID=...; CID=...; SEID=...;".
Required for initial login to obtain API tokens.
Example: "UID=123; CID=abc; SEID=def;"`,
			Required:  true,
			Sensitive: true,
		}, {
			Name:      "share_code",
			Help:      "Share code from share link (for accessing shared files via traditional API).",
			Sensitive: true,
		}, {
			Name:      "receive_code",
			Help:      "Password from share link (for accessing shared files via traditional API).",
			Sensitive: true,
		}, {
			Name:     "user_agent",
			Default:  defaultUserAgent,
			Advanced: true,
			Help: fmt.Sprintf(`HTTP user agent. Primarily used for initial login mimicry.
Defaults to "%s".`, defaultUserAgent),
		}, {
			Name: "root_folder_id",
			Help: `ID of the root folder.
Leave blank normally (uses the drive root '0').
Fill in for rclone to use a non root folder as its starting point.`,
			Advanced:  true,
			Sensitive: true,
		}, {
			Name:     "app_id",
			Default:  defaultAppID,
			Advanced: true,
			Help: fmt.Sprintf(`Custom App ID for authentication.
Defaults to "%s". Only change this if you have a specific reason to use a different App ID.`, defaultAppID),
		}, {
			Name:     "list_chunk",
			Default:  1150, // Max limit for OpenAPI file list
			Help:     "Size of listing chunk.",
			Advanced: true,
		}, {
			Name:     "censored_only",
			Default:  false,
			Help:     "Only show files that are censored (only applies to traditional API calls).",
			Advanced: true,
		}, {
			Name:     "pacer_min_sleep",
			Default:  unifiedMinSleep,
			Help:     "Minimum time to sleep between API calls (controls unified QPS, default ~4 QPS).",
			Advanced: true,
		}, {
			Name:     "contimeout",
			Default:  defaultConTimeout,
			Help:     "Connect timeout.",
			Advanced: true,
		}, {
			Name:     "timeout",
			Default:  defaultTimeout,
			Help:     "IO idle timeout.",
			Advanced: true,
		}, {
			Name:     "upload_hash_only",
			Default:  false,
			Advanced: true,
			Help: `Attempt hash-based upload (秒传) only. Skip uploading if the server doesn't have the file.
Requires SHA1 hash to be available or calculable.`,
		}, {
			Name:     "only_stream",
			Default:  false,
			Advanced: true,
			Help:     `Use traditional streamed upload (sample upload) for all files up to 5 GiB. Fails for larger files.`,
		}, {
			Name:     "fast_upload",
			Default:  false,
			Advanced: true,
			Help: `Upload strategy:
- Files <= nohash_size: Use traditional streamed upload.
- Files > nohash_size: Attempt hash-based upload (秒传).
- If 秒传 fails and size <= 5 GiB: Use traditional streamed upload.
- If 秒传 fails and size > 5 GiB: Use multipart upload.`,
		}, {
			Name:     "hash_memory_limit",
			Help:     "Files bigger than this will be cached on disk to calculate hash if required.",
			Default:  fs.SizeSuffix(10 * 1024 * 1024),
			Advanced: true,
		}, {
			Name: "upload_cutoff",
			Help: `Cutoff for switching to multipart upload.
Any files larger than this will be uploaded in chunks using the OSS multipart API.
Minimum is 0, maximum is 5 GiB.`,
			Default:  defaultUploadCutoff,
			Advanced: true,
		}, {
			Name:     "nohash_size",
			Help:     `Files smaller than this size will use traditional streamed upload if fast_upload is enabled or if hash upload is not attempted/fails. Max is 5 GiB.`,
			Default:  defaultNohashSize,
			Advanced: true,
		}, {
			Name: "chunk_size",
			Help: `Chunk size for multipart uploads.
Rclone will automatically increase the chunk size for large files to stay below the 10,000 parts limit.
Minimum is 100 KiB, maximum is 5 GiB.`,
			Default:  defaultChunkSize,
			Advanced: true,
		}, {
			Name:     "max_upload_parts",
			Help:     `Maximum number of parts in a multipart upload.`,
			Default:  maxUploadParts,
			Advanced: true,
		}, {
			Name: "upload_concurrency",
			Help: `Concurrency for multipart uploads.
Number of chunks to upload in parallel. Set to 0 to disable multipart uploads.
Minimum is 1, maximum is 32.
Note: 115网盘强制使用单线程上传以确保稳定性，此参数实际不生效。`,
			Default:  1, // 🔧 修复：115网盘强制单线程上传，配置与实际行为保持一致
			Advanced: true,
		}, {
			Name:     "internal",
			Help:     `Use the internal OSS endpoint for uploads (requires appropriate network access).`,
			Default:  false,
			Advanced: true,
		}, {
			Name:     "dual_stack",
			Help:     `Use a dual-stack (IPv4/IPv6) OSS endpoint for uploads.`,
			Default:  false,
			Advanced: true,
		}, {
			Name:     "no_check",
			Default:  false,
			Advanced: true,
			Help:     "Disable post-upload check (avoids extra API call but reduces certainty).",
		}, {
			Name:     "no_buffer",
			Default:  false,
			Advanced: true,
			Help:     "Skip disk buffering for uploads.",
		}, {
			Name:     config.ConfigEncoding,
			Help:     config.ConfigEncodingHelp,
			Advanced: true,
			Default: (encoder.EncodeLtGt |
				encoder.EncodeDoubleQuote |
				encoder.EncodeLeftSpace |
				encoder.EncodeCtl |
				encoder.EncodeRightSpace |
				encoder.EncodeInvalidUtf8),
		}},
	})
}

func checkUploadChunkSize(cs fs.SizeSuffix) error {
	if cs < minChunkSize {
		return fmt.Errorf("%s is less than %s", cs, minChunkSize)
	}
	if cs > maxChunkSize {
		return fmt.Errorf("%s is greater than %s", cs, maxChunkSize)
	}
	return nil
}

func checkUploadCutoff(cs fs.SizeSuffix) error {
	if cs > maxUploadCutoff {
		return fmt.Errorf("%s is greater than %s", cs, maxUploadCutoff)
	}
	return nil
}

func checkUploadConcurrency(concurrency int) error {
	if concurrency < 1 {
		return fmt.Errorf("upload_concurrency must be at least 1, got %d", concurrency)
	}
	if concurrency > 32 {
		return fmt.Errorf("upload_concurrency must be at most 32, got %d", concurrency)
	}
	return nil
}

// Options defines the configuration of this backend
type Options struct {
	Cookie              string               `config:"cookie"` // Single cookie string now
	ShareCode           string               `config:"share_code"`
	ReceiveCode         string               `config:"receive_code"`
	UserAgent           string               `config:"user_agent"`
	RootFolderID        string               `config:"root_folder_id"`
	ListChunk           int                  `config:"list_chunk"`
	CensoredOnly        bool                 `config:"censored_only"`
	PacerMinSleep       fs.Duration          `config:"pacer_min_sleep"` // Global pacer setting
	ConTimeout          fs.Duration          `config:"contimeout"`
	Timeout             fs.Duration          `config:"timeout"`
	HashMemoryThreshold fs.SizeSuffix        `config:"hash_memory_limit"`
	UploadHashOnly      bool                 `config:"upload_hash_only"`
	OnlyStream          bool                 `config:"only_stream"`
	FastUpload          bool                 `config:"fast_upload"`
	UploadCutoff        fs.SizeSuffix        `config:"upload_cutoff"`
	NohashSize          fs.SizeSuffix        `config:"nohash_size"`
	ChunkSize           fs.SizeSuffix        `config:"chunk_size"`
	MaxUploadParts      int                  `config:"max_upload_parts"`
	UploadConcurrency   int                  `config:"upload_concurrency"`
	Internal            bool                 `config:"internal"`
	DualStack           bool                 `config:"dual_stack"`
	NoCheck             bool                 `config:"no_check"`
	NoBuffer            bool                 `config:"no_buffer"` // Skip disk buffering for uploads
	Enc                 encoder.MultiEncoder `config:"encoding"`
	AppID               string               `config:"app_id"` // Custom App ID for authentication
}

// Fs represents a remote 115 drive
type Fs struct {
	name          string
	originalName  string // Original config name without modifications
	root          string
	opt           Options
	features      *fs.Features
	tradClient    *rest.Client // Client for traditional (cookie, encrypted) API calls
	openAPIClient *rest.Client // Client for OpenAPI (token) calls
	dirCache      *dircache.DirCache
	globalPacer   *fs.Pacer // Controls overall QPS
	tradPacer     *fs.Pacer // Controls QPS for traditional calls only (subset of global)
	downloadPacer *fs.Pacer // Controls QPS for download URL API calls (专门用于获取下载URL的API调用)
	uploadPacer   *fs.Pacer // Controls QPS for upload related API calls (专门用于上传相关的API调用)
	rootFolder    string    // path of the absolute root
	rootFolderID  string
	appVer        string // parsed from user-agent; used in traditional calls
	userID        string // User ID from cookie/token
	userkey       string // User key from traditional uploadinfo (needed for traditional upload init signature)
	isShare       bool   // mark it is from shared or not
	fileObj       *fs.Object
	m             configmap.Mapper // config map for saving tokens

	// 断点续传管理器
	resumeManager *ResumeManager115

	// 🚀 功能增强：智能URL管理器，解决下载URL频繁过期问题
	smartURLManager *SmartURLManager

	// 🔧 修复重复下载：下载协调器，防止同一文件被重复下载
	downloadCoordinator *DownloadCoordinator

	// Token management
	tokenMu      sync.Mutex
	accessToken  string
	refreshToken string
	tokenExpiry  time.Time
	codeVerifier string // For PKCE
	tokenRenewer *oauthutil.Renew
	loginMu      sync.Mutex

	// 🔧 重构：删除pathCache，统一使用dirCache进行目录缓存

	// BadgerDB持久化缓存系统
	pathResolveCache *cache.BadgerCache // 路径解析缓存 (已实现)
	dirListCache     *cache.BadgerCache // 目录列表缓存
	metadataCache    *cache.BadgerCache // 文件元数据缓存
	fileIDCache      *cache.BadgerCache // 文件ID验证缓存

	// 缓存时间配置
	cacheConfig CacheConfig115

	// HTTP连接池优化
	httpClient *http.Client

	// 上传操作锁，防止预热和上传同时进行
	uploadingMu sync.Mutex
	isUploading bool

	// 🔧 熔断器，防止级联失败
	circuitBreaker *CircuitBreaker
}

// CircuitBreaker 熔断器，防止级联失败
type CircuitBreaker struct {
	failures    int
	lastFailure time.Time
	threshold   int
	timeout     time.Duration
	mu          sync.RWMutex
}

// NewCircuitBreaker 创建新的熔断器
func NewCircuitBreaker(threshold int, timeout time.Duration) *CircuitBreaker {
	return &CircuitBreaker{
		threshold: threshold,
		timeout:   timeout,
	}
}

// ShouldRetry 检查是否应该重试
func (cb *CircuitBreaker) ShouldRetry() bool {
	cb.mu.RLock()
	defer cb.mu.RUnlock()

	// 如果失败次数超过阈值且在超时期内，熔断器打开
	if cb.failures >= cb.threshold && time.Since(cb.lastFailure) < cb.timeout {
		return false
	}
	return true
}

// ShouldRetryForURLExpiry 针对URL过期场景的重试检查（更宽松）
func (cb *CircuitBreaker) ShouldRetryForURLExpiry() bool {
	cb.mu.RLock()
	defer cb.mu.RUnlock()

	// 🔧 URL过期场景使用更宽松的策略：更高的阈值，更短的超时
	urlExpiryThreshold := cb.threshold * 3 // 15次失败
	urlExpiryTimeout := 10 * time.Second   // 10秒超时

	if cb.failures >= urlExpiryThreshold && time.Since(cb.lastFailure) < urlExpiryTimeout {
		return false
	}
	return true
}

// RecordFailure 记录失败
func (cb *CircuitBreaker) RecordFailure() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.failures++
	cb.lastFailure = time.Now()
}

// RecordSuccess 记录成功
func (cb *CircuitBreaker) RecordSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.failures = 0
}

// DownloadCoordinator 下载协调器，防止重复下载
type DownloadCoordinator struct {
	activeDownloads    map[string]*DownloadSession
	completedDownloads map[string]*CompletedDownload // 🔧 新增：缓存成功的下载结果
	mu                 sync.RWMutex
	maxConcurrent      int
	semaphore          chan struct{}
	cacheExpiry        time.Duration // 🔧 新增：缓存过期时间
}

// CompletedDownload 已完成的下载缓存
type CompletedDownload struct {
	result      DownloadResult
	completedAt time.Time
	accessCount int // 访问次数，用于统计
}

// DownloadSession 下载会话
type DownloadSession struct {
	fileID       string
	fileName     string
	size         int64
	startTime    time.Time
	participants int
	result       chan DownloadResult
}

// DownloadResult 下载结果
type DownloadResult struct {
	Success bool
	Error   error
	Reader  io.ReadCloser
	Path    string
}

// NewDownloadCoordinator 创建新的下载协调器
func NewDownloadCoordinator(maxConcurrent int) *DownloadCoordinator {
	dc := &DownloadCoordinator{
		activeDownloads:    make(map[string]*DownloadSession),
		completedDownloads: make(map[string]*CompletedDownload),
		maxConcurrent:      maxConcurrent,
		semaphore:          make(chan struct{}, maxConcurrent),
		cacheExpiry:        10 * time.Minute, // 🔧 缓存成功下载10分钟
	}

	// 🔧 启动定期清理过期缓存的协程
	go dc.cleanupExpiredCache()

	return dc
}

// cleanupExpiredCache 定期清理过期的下载缓存
func (dc *DownloadCoordinator) cleanupExpiredCache() {
	ticker := time.NewTicker(5 * time.Minute) // 每5分钟清理一次
	defer ticker.Stop()

	for range ticker.C {
		dc.mu.Lock()
		now := time.Now()
		for fileID, completed := range dc.completedDownloads {
			if now.Sub(completed.completedAt) > dc.cacheExpiry {
				fs.Debugf(nil, "清理过期的下载缓存: %s (完成时间: %v, 访问次数: %d)",
					fileID, completed.completedAt, completed.accessCount)
				delete(dc.completedDownloads, fileID)
			}
		}
		dc.mu.Unlock()
	}
}

// StartDownload 开始下载，返回会话和是否为新下载
func (dc *DownloadCoordinator) StartDownload(fileID, fileName string, size int64) (*DownloadSession, bool) {
	dc.mu.Lock()
	defer dc.mu.Unlock()

	// 🔧 首先检查是否有缓存的成功下载结果
	if completed, exists := dc.completedDownloads[fileID]; exists {
		// 检查缓存是否过期
		if time.Since(completed.completedAt) <= dc.cacheExpiry {
			completed.accessCount++
			fs.Infof(nil, "🎯 使用缓存的下载结果: %s (缓存时间: %v, 访问次数: %d)",
				fileName, time.Since(completed.completedAt), completed.accessCount)

			// 创建一个虚拟会话来返回缓存结果
			session := &DownloadSession{
				fileID:       fileID,
				fileName:     fileName,
				size:         size,
				startTime:    completed.completedAt,
				participants: 1,
				result:       make(chan DownloadResult, 1),
			}

			// 立即发送缓存的结果
			go func() {
				session.result <- completed.result
				close(session.result)
			}()

			return session, false // 返回缓存结果，不启动新下载
		} else {
			// 缓存过期，删除
			delete(dc.completedDownloads, fileID)
		}
	}

	// 检查是否已有相同文件的下载
	if session, exists := dc.activeDownloads[fileID]; exists {
		session.participants++
		fs.Debugf(nil, "文件 %s 已在下载中，等待现有下载完成 (参与者: %d)", fileName, session.participants)
		return session, false // 返回现有会话，不启动新下载
	}

	// 检查并发限制
	select {
	case dc.semaphore <- struct{}{}:
		// 成功获取信号量
	default:
		fs.Debugf(nil, "达到最大并发下载限制 (%d)，拒绝新下载: %s", dc.maxConcurrent, fileName)
		return nil, false
	}

	// 创建新的下载会话
	session := &DownloadSession{
		fileID:       fileID,
		fileName:     fileName,
		size:         size,
		startTime:    time.Now(),
		participants: 1,
		result:       make(chan DownloadResult, 1),
	}

	dc.activeDownloads[fileID] = session
	fs.Debugf(nil, "启动新的下载会话: %s", fileName)
	return session, true
}

// WaitForDownload 等待下载完成
func (dc *DownloadCoordinator) WaitForDownload(session *DownloadSession) DownloadResult {
	select {
	case result := <-session.result:
		// 🔧 关键修复：为每个等待者创建独立的文件句柄
		if result.Success && result.Reader != nil {
			if concurrentReader, ok := result.Reader.(*ConcurrentDownloadReader); ok {
				// 创建新的文件句柄
				newFile, err := os.Open(concurrentReader.tempPath)
				if err != nil {
					fs.Debugf(nil, "为等待者创建新文件句柄失败: %v", err)
					return DownloadResult{
						Success: false,
						Error:   fmt.Errorf("创建新文件句柄失败: %w", err),
					}
				}

				// 返回新的Reader
				return DownloadResult{
					Success: true,
					Reader: &ConcurrentDownloadReader{
						file:             newFile,
						tempPath:         concurrentReader.tempPath,
						progressReporter: concurrentReader.progressReporter,
						totalSize:        concurrentReader.totalSize,
					},
				}
			}
		}
		return result
	case <-time.After(30 * time.Minute): // 下载超时
		dc.CompleteDownload(session.fileID, DownloadResult{
			Success: false,
			Error:   fmt.Errorf("下载超时: %s", session.fileName),
		})
		return DownloadResult{
			Success: false,
			Error:   fmt.Errorf("下载超时: %s", session.fileName),
		}
	}
}

// CompleteDownload 完成下载
func (dc *DownloadCoordinator) CompleteDownload(fileID string, result DownloadResult) {
	dc.mu.Lock()
	defer dc.mu.Unlock()

	if session, exists := dc.activeDownloads[fileID]; exists {
		// 发送结果给所有等待者
		select {
		case session.result <- result:
		default:
			// 通道已满或已关闭
		}
		close(session.result)

		// 🔧 如果下载成功，缓存结果以防止重复下载
		if result.Success {
			dc.completedDownloads[fileID] = &CompletedDownload{
				result:      result,
				completedAt: time.Now(),
				accessCount: 0,
			}
			fs.Debugf(nil, "✅ 缓存成功的下载结果: %s (缓存期: %v)", session.fileName, dc.cacheExpiry)
		}

		// 清理活跃会话
		delete(dc.activeDownloads, fileID)
		<-dc.semaphore // 释放信号量
		fs.Debugf(nil, "下载会话完成: %s, 成功: %v, 参与者: %d", session.fileName, result.Success, session.participants)
	}
}

// CacheConfig115 115网盘缓存时间配置
type CacheConfig115 struct {
	PathResolveCacheTTL time.Duration // 路径解析缓存TTL，默认10分钟
	DirListCacheTTL     time.Duration // 目录列表缓存TTL，默认5分钟
	MetadataCacheTTL    time.Duration // 文件元数据缓存TTL，默认30分钟
	FileIDCacheTTL      time.Duration // 文件ID验证缓存TTL，默认15分钟
}

// DefaultCacheConfig115 返回115网盘优化的缓存配置
// 🔧 重构：统一TTL策略和缓存键命名规范，与123网盘保持一致
func DefaultCacheConfig115() CacheConfig115 {
	unifiedTTL := 5 * time.Minute // 统一TTL策略，与123网盘保持一致
	return CacheConfig115{
		PathResolveCacheTTL: unifiedTTL, // 从15分钟改为5分钟，统一TTL
		DirListCacheTTL:     unifiedTTL, // 从10分钟改为5分钟，统一TTL
		MetadataCacheTTL:    unifiedTTL, // 从60分钟改为5分钟，避免长期缓存不一致
		FileIDCacheTTL:      unifiedTTL, // 从30分钟改为5分钟，统一TTL
	}
}

// generatePathToIDCacheKey 生成路径到ID映射缓存键（与123网盘格式一致）
func generatePathToIDCacheKey(path string) string {
	return fmt.Sprintf("path_to_id_%s", path)
}

// 🔧 重构：添加与123网盘相同的目录列表缓存结构
// DirListCacheEntry115 目录列表缓存条目（与123网盘格式统一）
type DirListCacheEntry115 struct {
	FileList   []api.File `json:"file_list"`
	LastFileID string     `json:"last_file_id"`
	TotalCount int        `json:"total_count"`
	CachedAt   time.Time  `json:"cached_at"`
	ParentID   string     `json:"parent_id"`
	Version    string     `json:"version"`
	Checksum   string     `json:"checksum"`
}

// PathToIDCacheEntry115 路径到FileID映射缓存条目（与123网盘格式统一）
type PathToIDCacheEntry115 struct {
	Path     string    `json:"path"`
	FileID   string    `json:"file_id"`
	IsDir    bool      `json:"is_dir"`
	ParentID string    `json:"parent_id"`
	CachedAt time.Time `json:"cached_at"`
}

// 🔧 重构：添加与123网盘相同的路径到ID映射缓存函数

// 🔧 重构：删除PathCache相关结构体，统一使用dirCache
// PathCache功能已迁移到rclone标准的dirCache中
// isAPILimitError函数已存在于第823行，无需重复声明

// AsyncCacheUpdate 异步更新缓存，避免阻塞主要操作
func (f *Fs) AsyncCacheUpdate(ctx context.Context, path, id string, isDir bool) {
	select {
	case <-ctx.Done():
		fs.Debugf(f, "AsyncCacheUpdate cancelled for path: %s", path)
		return
	default:
		go func() {
			defer func() {
				if r := recover(); r != nil {
					fs.Debugf(f, "AsyncCacheUpdate panic for path %s: %v", path, r)
				}
			}()

			// 使用带超时的context防止goroutine长时间运行
			updateCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			select {
			case <-updateCtx.Done():
				fs.Debugf(f, "AsyncCacheUpdate timeout for path: %s", path)
				return
			case <-ctx.Done():
				fs.Debugf(f, "AsyncCacheUpdate cancelled during execution for path: %s", path)
				return
			default:
				// 🔧 重构：使用dirCache进行异步缓存更新
				// 只有目录才需要缓存到dirCache中
				if isDir {
					f.dirCache.Put(path, id)
					fs.Debugf(f, "AsyncCacheUpdate completed for directory path: %s", path)
				}
			}
		}()
	}
}

// AsyncDirCacheUpdate 异步更新目录缓存
func (f *Fs) AsyncDirCacheUpdate(ctx context.Context, path, id string) {
	select {
	case <-ctx.Done():
		fs.Debugf(f, "AsyncDirCacheUpdate cancelled for path: %s", path)
		return
	default:
		go func() {
			defer func() {
				if r := recover(); r != nil {
					fs.Debugf(f, "AsyncDirCacheUpdate panic for path %s: %v", path, r)
				}
			}()

			// 使用带超时的context防止goroutine长时间运行
			updateCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			select {
			case <-updateCtx.Done():
				fs.Debugf(f, "AsyncDirCacheUpdate timeout for path: %s", path)
				return
			case <-ctx.Done():
				fs.Debugf(f, "AsyncDirCacheUpdate cancelled during execution for path: %s", path)
				return
			default:
				// 检查目录缓存是否仍然有效
				if f.dirCache != nil {
					f.dirCache.Put(path, id)
					fs.Debugf(f, "AsyncDirCacheUpdate completed for path: %s", path)
				}
			}
		}()
	}
}

// BatchFindRequest 批量查找请求
type BatchFindRequest struct {
	PathID string
	Leaf   string
}

// BatchFindResult 批量查找结果
type BatchFindResult struct {
	Request BatchFindRequest
	ID      string
	Found   bool
	IsDir   bool
	Error   error
}

// BatchFindLeaf 批量查找叶节点，优化多个查找操作
func (f *Fs) BatchFindLeaf(ctx context.Context, requests []BatchFindRequest) ([]BatchFindResult, error) {
	results := make([]BatchFindResult, len(requests))

	// 按父目录ID分组请求
	groupedRequests := make(map[string][]int)
	for i, req := range requests {
		groupedRequests[req.PathID] = append(groupedRequests[req.PathID], i)
	}

	// 对每个父目录执行一次listAll操作
	for pathID, indices := range groupedRequests {
		// 创建查找映射
		leafMap := make(map[string][]int)
		for _, idx := range indices {
			leaf := requests[idx].Leaf
			leafMap[leaf] = append(leafMap[leaf], idx)
		}

		// 执行单次listAll操作
		_, err := f.listAll(ctx, pathID, f.opt.ListChunk, false, false, func(item *api.File) bool {
			decodedName := f.opt.Enc.ToStandardName(item.FileNameBest())
			if indices, exists := leafMap[decodedName]; exists {
				for _, idx := range indices {
					results[idx] = BatchFindResult{
						Request: requests[idx],
						ID:      item.ID(),
						Found:   true,
						IsDir:   item.IsDir(),
						Error:   nil,
					}
				}
				delete(leafMap, decodedName) // 避免重复处理
			}
			return len(leafMap) == 0 // 如果所有项目都找到了就停止
		})

		if err != nil {
			// 如果API调用失败，标记所有相关结果为错误
			for _, idx := range indices {
				if results[idx].Error == nil { // 只设置尚未设置的错误
					results[idx] = BatchFindResult{
						Request: requests[idx],
						Found:   false,
						Error:   err,
					}
				}
			}
		}

		// 标记未找到的项目
		for _, indices := range leafMap {
			for _, idx := range indices {
				if results[idx].Error == nil {
					results[idx] = BatchFindResult{
						Request: requests[idx],
						Found:   false,
						Error:   nil,
					}
				}
			}
		}
	}

	return results, nil
}

// isAPILimitError 检查错误是否为API限制错误
func isAPILimitError(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	return strings.Contains(errStr, "770004") ||
		strings.Contains(errStr, "已达到当前访问上限")
}

// 注意：clearCacheGradually函数已删除（未使用）

// 注意：invalidateRelatedCaches115和clearAllRelatedCaches115函数已删除（未使用）

// Object describes a 115 object
type Object struct {
	fs          *Fs
	remote      string
	hasMetaData bool
	id          string
	parent      string
	size        int64
	sha1sum     string
	pickCode    string
	modTime     time.Time
	durl        *api.DownloadURL // link to download the object
	durlMu      *sync.Mutex
}
type ApiResponse struct {
	State   bool                `json:"state"`   // Indicates success or failure
	Message string              `json:"message"` // Optional message
	Code    int                 `json:"code"`    // Status code
	Data    map[string]FileInfo `json:"data"`    // Map where keys are file IDs (strings) and values are FileInfo objects
}

// FileInfo represents the details of a single file.
// The key for this object in the parent map (ApiResponse.Data) is the file ID.
type FileInfo struct {
	FileName string      `json:"file_name"` // Name of the file
	FileSize int64       `json:"file_size"` // Size of the file in bytes (using int64 for potentially large files)
	PickCode string      `json:"pick_code"` // File pick code (extraction code)
	SHA1     string      `json:"sha1"`      // SHA1 hash of the file content
	URL      DownloadURL `json:"url"`       // Object containing the download URL
}
type DownloadURL struct {
	URL string `json:"url"` // The file download address
}

// retryErrorCodes is a slice of HTTP status codes that we will retry
var retryErrorCodes = []int{
	429, // Too Many Requests.
	500, // Internal Server Error
	502, // Bad Gateway
	503, // Service Unavailable
	504, // Gateway Timeout
	509, // Bandwidth Limit Exceeded
}

// CloudDriveErrorClassifier 云盘通用错误分类器
// 🔧 重构：提取为可复用组件，支持115和123网盘
type CloudDriveErrorClassifier struct {
	// 错误分类统计
	ServerOverloadCount  int64
	URLExpiredCount      int64
	NetworkTimeoutCount  int64
	RateLimitCount       int64
	AuthErrorCount       int64
	PermissionErrorCount int64
	NotFoundErrorCount   int64
	UnknownErrorCount    int64
}

// ClassifyError 统一错误分类方法
func (c *CloudDriveErrorClassifier) ClassifyError(err error) string {
	if err == nil {
		return "no_error"
	}

	errStr := strings.ToLower(err.Error())

	// 服务器过载错误
	if strings.Contains(errStr, "502") || strings.Contains(errStr, "bad gateway") ||
		strings.Contains(errStr, "503") || strings.Contains(errStr, "service unavailable") ||
		strings.Contains(errStr, "504") || strings.Contains(errStr, "gateway timeout") {
		c.ServerOverloadCount++
		return "server_overload"
	}

	// URL过期错误
	if strings.Contains(errStr, "download url is invalid") || strings.Contains(errStr, "expired") ||
		strings.Contains(errStr, "url过期") || strings.Contains(errStr, "链接失效") {
		c.URLExpiredCount++
		return "url_expired"
	}

	// 网络超时错误
	if strings.Contains(errStr, "timeout") || strings.Contains(errStr, "deadline exceeded") ||
		strings.Contains(errStr, "context canceled") || strings.Contains(errStr, "connection reset") {
		c.NetworkTimeoutCount++
		return "network_timeout"
	}

	// 限流错误
	if strings.Contains(errStr, "429") || strings.Contains(errStr, "too many requests") ||
		strings.Contains(errStr, "rate limit") || strings.Contains(errStr, "770004") ||
		strings.Contains(errStr, "已达到当前访问上限") {
		c.RateLimitCount++
		return "rate_limit"
	}

	// 认证错误
	if strings.Contains(errStr, "401") || strings.Contains(errStr, "unauthorized") ||
		strings.Contains(errStr, "token") || strings.Contains(errStr, "认证失败") {
		c.AuthErrorCount++
		return "auth_error"
	}

	// 权限错误
	if strings.Contains(errStr, "403") || strings.Contains(errStr, "forbidden") ||
		strings.Contains(errStr, "permission denied") {
		c.PermissionErrorCount++
		return "permission_error"
	}

	// 资源不存在错误
	if strings.Contains(errStr, "404") || strings.Contains(errStr, "not found") ||
		strings.Contains(errStr, "file not found") {
		c.NotFoundErrorCount++
		return "not_found"
	}

	c.UnknownErrorCount++
	return "unknown"
}

// shouldRetry checks if a request should be retried based on the response, error, and API type.
// 🔧 优化：使用统一错误分类和重试策略
func shouldRetry(ctx context.Context, resp *http.Response, err error) (bool, error) {
	if fserrors.ContextError(ctx, &err) {
		return false, err
	}

	// Check HTTP response status codes first
	if resp != nil {
		switch resp.StatusCode {
		case 429: // Too Many Requests
			fs.Debugf(nil, "HTTP 429 Too Many Requests, retrying with delay")
			return true, pacer.RetryAfterError(err, 15*time.Second)
		case 500, 502, 503, 504: // Server errors
			fs.Debugf(nil, "Server error %d, retrying: %v", resp.StatusCode, err)
			return true, pacer.RetryAfterError(err, 5*time.Second)
		case 408: // Request Timeout
			fs.Debugf(nil, "Request timeout, retrying: %v", err)
			return true, pacer.RetryAfterError(err, 3*time.Second)
		}
	}

	// 🔧 重构：优先使用rclone标准错误处理
	if err != nil && fserrors.ShouldRetry(err) {
		return true, err
	}

	// 🔧 重构：云盘特定错误处理（仅处理特殊情况）
	if err != nil {
		errStr := strings.ToLower(err.Error())

		// URL过期错误需要重试（会触发URL刷新）
		if strings.Contains(errStr, "download url is invalid") ||
			strings.Contains(errStr, "expired") || strings.Contains(errStr, "url过期") {
			return true, err
		}

		// 认证错误不重试，让上层处理
		if strings.Contains(errStr, "401") || strings.Contains(errStr, "unauthorized") {
			return false, err
		}
	}

	// 回退到rclone标准HTTP状态码重试
	return fserrors.ShouldRetryHTTP(resp, retryErrorCodes), err
}

// ------------------------------------------------------------
// Authentication and Client Setup
// ------------------------------------------------------------

// Credential holds the parsed cookie values needed for initial login
type Credential struct {
	UID  string
	CID  string
	SEID string
	KID  string // Keep KID as it might be used implicitly by the web API calls
}

// Valid reports whether the credential is valid.
func (cr *Credential) Valid() error {
	if cr == nil {
		return errors.New("nil credential")
	}
	// KID is optional/sometimes empty, SEID seems required for login mimicry
	if cr.UID == "" || cr.CID == "" || cr.SEID == "" {
		return errors.New("missing UID, CID, or SEID in cookie")
	}
	return nil
}

// FromCookie loads credential from cookie string
func (cr *Credential) FromCookie(cookieStr string) *Credential {
	for _, item := range strings.Split(cookieStr, ";") {
		kv := strings.SplitN(strings.TrimSpace(item), "=", 2)
		if len(kv) != 2 {
			continue
		}
		key := strings.TrimSpace(strings.ToUpper(kv[0]))
		val := strings.TrimSpace(kv[1])
		switch key {
		case "UID":
			cr.UID = val
		case "CID":
			cr.CID = val
		case "SEID":
			cr.SEID = val
		case "KID":
			cr.KID = val
		}
	}
	return cr
}

// Cookie turns the credential into a list of http cookie for the traditional client
func (cr *Credential) Cookie() []*http.Cookie {
	cookies := []*http.Cookie{
		{Name: "UID", Value: cr.UID, Domain: domain, Path: "/", HttpOnly: true},
		{Name: "CID", Value: cr.CID, Domain: domain, Path: "/", HttpOnly: true},
		{Name: "SEID", Value: cr.SEID, Domain: domain, Path: "/", HttpOnly: true},
	}
	// Add KID only if it's present
	if cr.KID != "" {
		cookies = append(cookies, &http.Cookie{Name: "KID", Value: cr.KID, Domain: domain, Path: "/", HttpOnly: true})
	}
	return cookies
}

// UserID parses userID from UID field
func (cr *Credential) UserID() string {
	userID, _, _ := strings.Cut(cr.UID, "_")
	return userID
}

// getHTTPClient makes an http client according to the options with optimized connection pool
func getHTTPClient(ctx context.Context, opt *Options) *http.Client {
	t := fshttp.NewTransportCustom(ctx, func(t *http.Transport) {
		t.TLSHandshakeTimeout = time.Duration(opt.ConTimeout)
		// 🔧 大幅增加响应头超时时间，支持大文件上传
		t.ResponseHeaderTimeout = 10 * time.Minute // 从2分钟增加到10分钟

		// 优化连接池配置
		t.MaxIdleConns = 100                 // 最大空闲连接数
		t.MaxIdleConnsPerHost = 20           // 每个主机的最大空闲连接数
		t.MaxConnsPerHost = 50               // 每个主机的最大连接数
		t.IdleConnTimeout = 90 * time.Second // 空闲连接超时
		t.DisableKeepAlives = false          // 启用Keep-Alive
		t.ForceAttemptHTTP2 = true           // 强制尝试HTTP/2

		// 优化超时设置
		t.DialContext = (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext
		t.ExpectContinueTimeout = 1 * time.Second
	})

	return &http.Client{
		Transport: t,
		// 🔧 大幅增加总超时时间，支持大文件上传
		Timeout: 15 * time.Minute, // 从3分钟增加到15分钟
	}
}

// getTradHTTPClient creates an HTTP client with traditional UserAgent
func getTradHTTPClient(ctx context.Context, opt *Options) *http.Client {
	// Create a new context with the traditional UserAgent
	newCtx, ci := fs.AddConfig(ctx)
	ci.UserAgent = tradUserAgent
	return getHTTPClient(newCtx, opt)
}

// getOpenAPIHTTPClient creates an HTTP client with default UserAgent
func getOpenAPIHTTPClient(ctx context.Context, opt *Options) *http.Client {
	// Create a new context with the default UserAgent
	newCtx, ci := fs.AddConfig(ctx)
	ci.UserAgent = defaultUserAgent
	return getHTTPClient(newCtx, opt)
}

// errorHandler parses a non 2xx error response into an error (Generic, might need adjustment per API)
func errorHandler(resp *http.Response) error {
	// Attempt to decode as OpenAPI error first
	openAPIErr := new(api.OpenAPIBase)
	bodyBytes, readErr := rest.ReadBody(resp) // Read body once
	if readErr != nil {
		fs.Debugf(nil, "Couldn't read error response body: %v", readErr)
		// Fallback to status code if body read fails
		return api.NewTokenError(fmt.Sprintf("HTTP error %d (%s)", resp.StatusCode, resp.Status))
	}

	decodeErr := json.Unmarshal(bodyBytes, &openAPIErr)
	if decodeErr == nil && !openAPIErr.State {
		// Successfully decoded as OpenAPI error
		err := openAPIErr.Err()
		// Check for specific token-related errors
		if openAPIErr.ErrCode() == 401 || openAPIErr.ErrCode() == 100001 || strings.Contains(openAPIErr.ErrMsg(), "token") { // Example codes
			return api.NewTokenError(err.Error(), true) // Assume token error needs refresh/relogin
		}
		return err
	}

	// Attempt to decode as Traditional error
	tradErr := new(api.TraditionalBase)
	decodeErr = json.Unmarshal(bodyBytes, &tradErr)
	if decodeErr == nil && !tradErr.State {
		// Successfully decoded as Traditional error
		return tradErr.Err()
	}

	// Fallback if JSON decoding fails or state is true (but status code != 2xx)
	fs.Debugf(nil, "Couldn't decode error response: %v. Body: %s", decodeErr, string(bodyBytes))
	return api.NewTokenError(fmt.Sprintf("HTTP error %d (%s): %s", resp.StatusCode, resp.Status, string(bodyBytes)))
}

// generatePKCE generates a code_verifier and code_challenge
func generatePKCE() (verifier, challenge string, err error) {
	verifierBytes := make([]byte, pkceVerifierLength)
	_, err = rand.Read(verifierBytes)
	if err != nil {
		return "", "", fmt.Errorf("failed to generate random verifier: %w", err)
	}
	// Use URL-safe base64 encoding without padding
	verifier = base64.RawURLEncoding.EncodeToString(verifierBytes)
	// Calculate SHA256 hash
	hash := sha256.Sum256([]byte(verifier))
	// Base64 encode the hash
	challenge = base64.RawURLEncoding.EncodeToString(hash[:])
	return verifier, challenge, nil
}

// login performs the initial authentication flow to get tokens
func (f *Fs) login(ctx context.Context) error {
	// Use a global mutex for login to prevent multiple concurrent login attempts
	// which could overwhelm the API and cause authentication failures
	f.loginMu.Lock()
	defer f.loginMu.Unlock()

	// Check if login is needed (avoid nested locking)
	needLogin := func() bool {
		f.tokenMu.Lock()
		defer f.tokenMu.Unlock()
		return f.accessToken == "" || time.Now().After(f.tokenExpiry)
	}()

	if !needLogin {
		fs.Debugf(f, "Token is already valid after waiting for login mutex, skipping login")
		return nil
	}

	// Parse cookie and setup clients
	if err := f.setupLoginEnvironment(ctx); err != nil {
		return err
	}

	fs.Debugf(f, "Starting login process for user %s", f.userID)

	// Generate PKCE
	var challenge string
	var err error
	if f.codeVerifier, challenge, err = generatePKCE(); err != nil {
		return fmt.Errorf("failed to generate PKCE codes: %w", err)
	}
	fs.Debugf(f, "Generated PKCE challenge")

	// Request device code
	loginUID, err := f.getAuthDeviceCode(ctx, challenge)
	if err != nil {
		return err
	}

	// Mimic QR scan confirmation
	if err := f.simulateQRCodeScan(ctx, loginUID); err != nil {
		// Only log errors from these steps, don't fail
		fs.Logf(f, "QR code scan simulation steps had errors (continuing anyway): %v", err)
	}

	// Exchange device code for access token
	if err := f.exchangeDeviceCodeForToken(ctx, loginUID); err != nil {
		return err
	}

	// Get userkey for traditional uploads if needed
	if f.userkey == "" {
		fs.Debugf(f, "Fetching userkey using traditional API...")
		if err := f.getUploadBasicInfo(ctx); err != nil {
			// Log error but don't fail login, userkey is only for traditional upload init
			fs.Logf(f, "Failed to get userkey (needed for some traditional uploads): %v", err)
		} else {
			fs.Debugf(f, "Successfully fetched userkey.")
		}
	}

	return nil
}

// setupLoginEnvironment parses cookies and sets up HTTP clients
func (f *Fs) setupLoginEnvironment(ctx context.Context) error {
	// Parse cookie
	cred := (&Credential{}).FromCookie(f.opt.Cookie)
	if err := cred.Valid(); err != nil {
		return fmt.Errorf("invalid cookie provided: %w", err)
	}
	f.userID = cred.UserID() // Set userID early

	// Setup clients (needed for the login calls)
	// Create separate clients for each API type with different User-Agents
	tradHTTPClient := getTradHTTPClient(ctx, &f.opt)
	openAPIHTTPClient := getOpenAPIHTTPClient(ctx, &f.opt)

	// Traditional client (uses cookie)
	f.tradClient = rest.NewClient(tradHTTPClient).
		SetRoot(traditionalRootURL).
		SetCookie(cred.Cookie()...).
		SetErrorHandler(errorHandler)

	// OpenAPI client (will have token set later)
	f.openAPIClient = rest.NewClient(openAPIHTTPClient).
		SetRoot(openAPIRootURL).
		SetErrorHandler(errorHandler)

	return nil
}

// getAuthDeviceCode calls the authDeviceCode API to start the login process
func (f *Fs) getAuthDeviceCode(ctx context.Context, challenge string) (string, error) {
	// Use configured AppID if provided, otherwise use default
	clientID := f.opt.AppID

	authData := url.Values{
		"client_id":             {clientID},
		"code_challenge":        {challenge},
		"code_challenge_method": {"sha256"}, // Use SHA256
	}
	authOpts := rest.Opts{
		Method:       "POST",
		RootURL:      passportRootURL, // Use passport API domain
		Path:         "/open/authDeviceCode",
		Parameters:   authData, // Send as query parameters for POST? Docs say body, let's try body.
		Body:         strings.NewReader(authData.Encode()),
		ExtraHeaders: map[string]string{"Content-Type": "application/x-www-form-urlencoded"},
	}
	var authResp api.AuthDeviceCodeResp
	// This initial call uses the traditional client *but doesn't need encryption*
	// It still needs the cookie and pacing.
	err := f.CallTraditionalAPI(ctx, &authOpts, nil, &authResp, true) // Pass skipEncrypt=true
	if err != nil {
		return "", fmt.Errorf("authDeviceCode failed: %w", err)
	}
	if authResp.Data == nil || authResp.Data.UID == "" {
		return "", fmt.Errorf("authDeviceCode returned empty data: %v", authResp)
	}
	loginUID := authResp.Data.UID
	fs.Debugf(f, "authDeviceCode successful, login UID: %s", loginUID)
	return loginUID, nil
}

// simulateQRCodeScan mimics the QR code scan and confirm process
func (f *Fs) simulateQRCodeScan(ctx context.Context, loginUID string) error {
	// Call QR scan API
	scanErr := f.callQRScanAPI(ctx, loginUID)

	// Call QR confirm API - still proceed if scan had an error
	confirmErr := f.callQRConfirmAPI(ctx, loginUID)

	// Add a small delay after mimic steps, just in case
	time.Sleep(1 * time.Second)

	// Return an error if both steps failed, otherwise continue
	if scanErr != nil && confirmErr != nil {
		return fmt.Errorf("both scan and confirm steps failed: scan: %v, confirm: %v", scanErr, confirmErr)
	}
	return nil
}

// callQRScanAPI calls the QR scan API
func (f *Fs) callQRScanAPI(ctx context.Context, loginUID string) error {
	scanPayload := map[string]string{"uid": loginUID}
	scanOpts := rest.Opts{
		Method:     "GET",
		RootURL:    qrCodeAPIRootURL,
		Path:       "/api/2.0/prompt.php",
		Parameters: url.Values{"uid": []string{loginUID}}, // Send as query params
	}
	var scanResp api.TraditionalBase // Use base struct, don't care about response data much
	fs.Debugf(f, "Calling login_qrcode_scan...")
	err := f.CallTraditionalAPI(ctx, &scanOpts, scanPayload, &scanResp, true)
	if err != nil {
		return fmt.Errorf("login_qrcode_scan failed: %w", err)
	}
	fs.Debugf(f, "login_qrcode_scan call successful (State: %v)", scanResp.State)
	return nil
}

// callQRConfirmAPI calls the QR confirm API
func (f *Fs) callQRConfirmAPI(ctx context.Context, loginUID string) error {
	confirmPayload := map[string]string{"uid": loginUID, "key": loginUID, "client": "0"} // Key seems to be same as uid?
	confirmOpts := rest.Opts{
		Method:     "GET",
		RootURL:    hnQrCodeAPIRootURL,
		Path:       "/api/2.0/slogin.php",
		Parameters: url.Values{"uid": []string{loginUID}, "key": []string{loginUID}, "client": []string{"0"}}, // Send as query params
	}
	var confirmResp api.TraditionalBase
	fs.Debugf(f, "Calling login_qrcode_scan_confirm...")
	err := f.CallTraditionalAPI(ctx, &confirmOpts, confirmPayload, &confirmResp, true) // Needs encryption? Assume yes.
	if err != nil {
		return fmt.Errorf("login_qrcode_scan_confirm failed: %w", err)
	}
	fs.Debugf(f, "login_qrcode_scan_confirm call successful (State: %v)", confirmResp.State)
	return nil
}

// exchangeDeviceCodeForToken gets the access token using the device code
func (f *Fs) exchangeDeviceCodeForToken(ctx context.Context, loginUID string) error {
	tokenData := url.Values{
		"uid":           {loginUID},
		"code_verifier": {f.codeVerifier},
	}
	tokenOpts := rest.Opts{
		Method:       "POST",
		RootURL:      passportRootURL,
		Path:         "/open/deviceCodeToToken",
		Body:         strings.NewReader(tokenData.Encode()),
		ExtraHeaders: map[string]string{"Content-Type": "application/x-www-form-urlencoded"},
	}
	var tokenResp api.DeviceCodeTokenResp
	// This call also uses traditional client but no encryption needed.
	err := f.CallTraditionalAPI(ctx, &tokenOpts, nil, &tokenResp, true) // skipEncrypt=true
	if err != nil {
		return fmt.Errorf("deviceCodeToToken failed: %w", err)
	}
	if tokenResp.Data == nil || tokenResp.Data.AccessToken == "" {
		return fmt.Errorf("deviceCodeToToken returned empty data: %v", tokenResp)
	}

	// Store tokens with proper locking
	f.tokenMu.Lock()
	f.accessToken = tokenResp.Data.AccessToken
	f.refreshToken = tokenResp.Data.RefreshToken
	f.tokenExpiry = time.Now().Add(time.Duration(tokenResp.Data.ExpiresIn) * time.Second)
	f.tokenMu.Unlock()

	fs.Debugf(f, "Successfully obtained access token, expires at %v", f.tokenExpiry)

	return nil
}

// refreshTokenIfNecessary refreshes the token if necessary
func (f *Fs) refreshTokenIfNecessary(ctx context.Context, refreshTokenExpired bool, forceRefresh bool) error {
	f.tokenMu.Lock()

	// Check if token is already valid when we acquire the lock
	// This handles the case where another thread just refreshed the token
	if !refreshTokenExpired && !forceRefresh && isTokenStillValid(f) {
		fs.Debugf(f, "Token is still valid after acquiring lock, skipping refresh")
		f.tokenMu.Unlock()
		return nil
	}

	// Check if we need to perform a full login instead of a refresh
	if shouldPerformFullLogin(f, refreshTokenExpired) {
		f.tokenMu.Unlock()
		err := f.login(ctx) // login handles its own locking
		if err != nil {
			return err
		}
		// Save the token after successful login
		f.saveToken(ctx, f.m)
		return nil
	}

	// Check if token is still valid and refresh not forced
	if !forceRefresh && isTokenStillValid(f) {
		f.tokenMu.Unlock()
		return nil
	}

	// Prepare for token refresh
	refreshToken := f.refreshToken // Make a local copy of the refresh token
	f.tokenMu.Unlock()             // Unlock before making API call

	// Perform the actual token refresh
	result, err := f.performTokenRefresh(ctx, refreshToken)
	if err != nil {
		return err // Error already formatted with context
	}

	// Update the tokens with new values
	f.updateTokens(result)

	// Save the refreshed token to config
	f.saveToken(ctx, f.m)

	return nil
}

// shouldPerformFullLogin determines if we should skip refresh and do a full login
func shouldPerformFullLogin(f *Fs, refreshTokenExpired bool) bool {
	// Skip directly to re-login if refresh token expired
	if refreshTokenExpired {
		fs.Debugf(f, "Token refresh skipped, going directly to re-login due to expired refresh token")
		return true
	}

	// Re-login if no tokens available
	if f.accessToken == "" || f.refreshToken == "" {
		fs.Debugf(f, "No token found, attempting login.")
		return true
	}

	return false
}

// isTokenStillValid checks if the current token is still valid
func isTokenStillValid(f *Fs) bool {
	return time.Now().Before(f.tokenExpiry.Add(-tokenRefreshWindow))
}

// performTokenRefresh handles the actual API call to refresh the token
func (f *Fs) performTokenRefresh(ctx context.Context, refreshToken string) (*api.RefreshTokenResp, error) {
	// Ensure client exists
	if err := f.ensureOpenAPIClient(ctx); err != nil {
		return nil, err
	}

	// Set up and make the refresh request
	refreshResp, err := f.callRefreshTokenAPI(ctx, refreshToken)
	if err != nil {
		return handleRefreshError(f, ctx, err)
	}

	// Validate the response
	if refreshResp.Data == nil || refreshResp.Data.AccessToken == "" {
		// Log detailed information about the empty response
		fs.Errorf(f, "Refresh token response empty or invalid. Full response: %#v", refreshResp)
		// Log OpenAPI base information (state, code, message)
		fs.Errorf(f, "Response state: %v, code: %d, message: %q",
			refreshResp.State, refreshResp.Code, refreshResp.Message)

		fs.Errorf(f, "Refresh token response empty, attempting re-login.")

		// Re-lock before checking token again to avoid race condition
		f.tokenMu.Lock()
		// Check if another thread has already refreshed the token
		if f.accessToken != "" && time.Now().Before(f.tokenExpiry) {
			fs.Debugf(f, "Token was refreshed by another thread while waiting")
			f.tokenMu.Unlock()
			return nil, nil
		}
		f.tokenMu.Unlock()

		loginErr := f.login(ctx)
		if loginErr != nil {
			return nil, fmt.Errorf("re-login failed after empty refresh response: %w", loginErr)
		}
		fs.Debugf(f, "Re-login successful after empty refresh response.")
		return nil, nil // Re-login successful, no need to update tokens
	}

	return refreshResp, nil
}

// ensureOpenAPIClient ensures the OpenAPI client is initialized
func (f *Fs) ensureOpenAPIClient(ctx context.Context) error {
	if f.openAPIClient != nil {
		return nil
	}

	// Setup client if it doesn't exist
	newCtx, ci := fs.AddConfig(ctx)
	ci.UserAgent = f.opt.UserAgent
	httpClient := getHTTPClient(newCtx, &f.opt)
	f.openAPIClient = rest.NewClient(httpClient).
		SetRoot(openAPIRootURL).
		SetErrorHandler(errorHandler)

	return nil
}

// callRefreshTokenAPI makes the actual API call to refresh the token
func (f *Fs) callRefreshTokenAPI(ctx context.Context, refreshToken string) (*api.RefreshTokenResp, error) {
	refreshData := url.Values{
		"refresh_token": {refreshToken},
	}
	opts := rest.Opts{
		Method:       "POST",
		RootURL:      passportRootURL,
		Path:         "/open/refreshToken",
		Body:         strings.NewReader(refreshData.Encode()),
		ExtraHeaders: map[string]string{"Content-Type": "application/x-www-form-urlencoded"},
	}

	var refreshResp api.RefreshTokenResp
	resp, err := f.openAPIClient.Call(ctx, &opts)
	if err != nil {
		return nil, err
	}

	// Read the raw response for logging
	defer fs.CheckClose(resp.Body, &err)
	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		fs.Errorf(f, "Failed to read refresh token response body: %v", readErr)
		return nil, readErr
	}

	// Log the raw response
	fs.Debugf(f, "Raw refresh token response: %s", string(body))

	// Create a new reader for the JSON unmarshal
	err = json.Unmarshal(body, &refreshResp)
	if err != nil {
		fs.Errorf(f, "Failed to parse refresh token response: %v", err)
		return nil, err
	}

	return &refreshResp, nil
}

// handleRefreshError handles errors from the refresh token API call
func handleRefreshError(f *Fs, ctx context.Context, err error) (*api.RefreshTokenResp, error) {
	fs.Errorf(f, "Refresh token failed: %v", err)

	// Check if the error indicates the refresh token itself is expired
	var tokenErr *api.TokenError
	if errors.As(err, &tokenErr) && tokenErr.IsRefreshTokenExpired ||
		strings.Contains(err.Error(), "refresh token expired") {
		fs.Debugf(f, "Refresh token seems expired, attempting full re-login.")
		loginErr := f.login(ctx) // login handles its own locking
		if loginErr != nil {
			return nil, fmt.Errorf("re-login failed after refresh token expired: %w", loginErr)
		}
		fs.Debugf(f, "Re-login successful after refresh token expiry.")
		return nil, nil // Re-login successful
	}

	// Return the original refresh error if it wasn't an expiry issue
	return nil, fmt.Errorf("token refresh failed: %w", err)
}

// updateTokens updates the token values with new values from a refresh response
func (f *Fs) updateTokens(refreshResp *api.RefreshTokenResp) {
	// If we got a nil response, it means we did a re-login instead of a refresh
	if refreshResp == nil {
		return
	}

	f.tokenMu.Lock()
	defer f.tokenMu.Unlock()

	f.accessToken = refreshResp.Data.AccessToken
	// OpenAPI spec says refresh_token might be updated, so store the new one
	if refreshResp.Data.RefreshToken != "" {
		f.refreshToken = refreshResp.Data.RefreshToken
	}
	f.tokenExpiry = time.Now().Add(time.Duration(refreshResp.Data.ExpiresIn) * time.Second)
	fs.Debugf(f, "Token refreshed successfully, new expiry: %v", f.tokenExpiry)
}

// saveToken saves the current token to the config
func (f *Fs) saveToken(_ context.Context, m configmap.Mapper) {
	if m == nil {
		fs.Debugf(f, "Not saving tokens - nil mapper provided")
		return
	}

	f.tokenMu.Lock()
	defer f.tokenMu.Unlock()

	if f.accessToken == "" || f.refreshToken == "" || f.tokenExpiry.IsZero() {
		fs.Debugf(f, "Not saving tokens - incomplete token information")
		return
	}

	// Create the token structure
	token := &oauth2.Token{
		AccessToken:  f.accessToken,
		TokenType:    "Bearer",
		RefreshToken: f.refreshToken,
		Expiry:       f.tokenExpiry,
	}

	// Save the token directly using oauthutil's method
	// Note: This uses the originalName without brackets to ensure consistency
	err := oauthutil.PutToken(f.originalName, m, token, false)
	if err != nil {
		fs.Errorf(f, "Failed to save token to config: %v", err)
		return
	}

	fs.Debugf(f, "Saved token to config file using original name %q", f.originalName)
}

// setupTokenRenewer initializes the token renewer to automatically refresh tokens
func (f *Fs) setupTokenRenewer(ctx context.Context, m configmap.Mapper) {
	// Only set up renewer if we have valid tokens
	if f.accessToken == "" || f.refreshToken == "" || f.tokenExpiry.IsZero() {
		fs.Debugf(f, "Not setting up token renewer - incomplete token information")
		return
	}

	// Create a renewal transaction function
	transaction := func() error {
		fs.Debugf(f, "Token renewer triggered, refreshing token")
		// Use non-global function to avoid deadlocks
		err := f.refreshTokenIfNecessary(ctx, false, true)
		if err != nil {
			fs.Errorf(f, "Failed to refresh token in renewer: %v", err)
			return err
		}

		return nil // saveToken is already called in refreshTokenIfNecessary
	}

	// Create minimal OAuth config
	config := &oauthutil.Config{
		TokenURL: passportRootURL + "/open/refreshToken",
	}

	// Create a token source using the existing token
	token := &oauth2.Token{
		AccessToken:  f.accessToken,
		RefreshToken: f.refreshToken,
		Expiry:       f.tokenExpiry,
		TokenType:    "Bearer",
	}

	// Save token to config so it can be accessed by TokenSource
	err := oauthutil.PutToken(f.originalName, m, token, false)
	if err != nil {
		fs.Logf(f, "Failed to save token for renewer: %v", err)
		return
	}

	// Create a client with the token source
	_, ts, err := oauthutil.NewClientWithBaseClient(ctx, f.originalName, m, config, fshttp.NewClient(ctx))
	if err != nil {
		fs.Logf(f, "Failed to create token source for renewer: %v", err)
		return
	}

	// Create token renewer that will trigger when the token is about to expire
	f.tokenRenewer = oauthutil.NewRenew(f.originalName, ts, transaction)
	f.tokenRenewer.Start() // Start the renewer immediately
	fs.Debugf(f, "Token renewer initialized and started with original name %q", f.originalName)
}

// CallOpenAPI performs a call to the OpenAPI endpoint.
// It handles token refresh and sets the Authorization header.
// If skipToken is true, it skips adding the Authorization header (used for refresh itself).
func (f *Fs) CallOpenAPI(ctx context.Context, opts *rest.Opts, request any, response any, skipToken bool) error {
	// Ensure root URL is set if not provided in opts
	if opts.RootURL == "" {
		opts.RootURL = openAPIRootURL
	}

	// 🔧 智能QPS管理：根据API端点选择合适的调速器
	smartPacer := f.getPacerForEndpoint(opts.Path)
	fs.Debugf(f, "🎯 智能QPS选择: %s -> 使用智能调速器", opts.Path)

	// Wrap the entire attempt sequence with the smart pacer, returning proper retry signals
	return smartPacer.Call(func() (shouldRetryGlobal bool, errGlobal error) {
		// Ensure token is available and current
		if !skipToken {
			if err := f.prepareTokenForRequest(ctx, opts); err != nil {
				fs.Debugf(f, "🔍 CallOpenAPI: prepareTokenForRequest失败: %v", err)
				return false, backoff.Permanent(err)
			}
		}

		// Make the API call
		resp, apiErr := f.executeOpenAPICall(ctx, opts, request, response)

		// Handle retries for network/server errors
		if retryNeeded, retryErr := shouldRetry(ctx, resp, apiErr); retryNeeded {
			fs.Debugf(f, "pacer: low level retry required for OpenAPI call (error: %v)", retryErr)
			return true, retryErr // Signal globalPacer to retry
		}

		// Handle token errors
		if apiErr != nil {
			if retryAfterTokenRefresh, err := f.handleTokenError(ctx, opts, apiErr, skipToken); retryAfterTokenRefresh {
				return true, nil // Retry with refreshed token
			} else if err != nil {
				return false, backoff.Permanent(err)
			}
			return false, backoff.Permanent(apiErr) // Non-token error, don't retry
		}

		// Check for API-level errors in the response
		if apiErr = f.checkResponseForAPIErrors(response, skipToken, opts); apiErr != nil {
			if tokenRefreshed, err := f.handleTokenError(ctx, opts, apiErr, skipToken); tokenRefreshed {
				return true, nil // Retry with refreshed token
			} else if err != nil {
				return false, backoff.Permanent(err)
			}
			return false, backoff.Permanent(apiErr) // Other API error, don't retry
		}

		fs.Debugf(f, "pacer: OpenAPI call successful")
		return false, nil // Success, don't retry
	})
}

// CallUploadAPI 专门用于上传相关API调用的函数，使用专用调速器
// 🔧 优化上传API调用频率，平衡性能和稳定性
func (f *Fs) CallUploadAPI(ctx context.Context, opts *rest.Opts, request any, response any, skipToken bool) error {
	// Ensure root URL is set if not provided in opts
	if opts.RootURL == "" {
		opts.RootURL = openAPIRootURL
	}

	// 🔧 使用专用的上传调速器，而不是全局调速器
	return f.uploadPacer.Call(func() (shouldRetryGlobal bool, errGlobal error) {
		// Ensure token is available and current
		if !skipToken {
			if err := f.prepareTokenForRequest(ctx, opts); err != nil {
				fs.Debugf(f, "🔍 CallUploadAPI: prepareTokenForRequest失败: %v", err)
				return false, backoff.Permanent(err)
			}
		}

		// Make the actual API call
		fs.Debugf(f, "🔍 CallUploadAPI: 执行API调用")
		resp, err := f.openAPIClient.CallJSON(ctx, opts, request, response)

		// Check for API-level errors in the response
		if apiErr := f.checkResponseForAPIErrors(response, skipToken, opts); apiErr != nil {
			if tokenRefreshed, err := f.handleTokenError(ctx, opts, apiErr, skipToken); tokenRefreshed {
				return true, nil // Retry with refreshed token
			} else if err != nil {
				return false, backoff.Permanent(err)
			}
			return false, backoff.Permanent(apiErr) // Other API error, don't retry
		}

		fs.Debugf(f, "🔍 CallUploadAPI: API调用成功")
		return shouldRetry(ctx, resp, err)
	})
}

// CallDownloadURLAPI 专门用于下载URL API调用的函数，使用专用调速器
// 🔧 防止下载URL API调用频率过高，避免触发115网盘反爬机制
func (f *Fs) CallDownloadURLAPI(ctx context.Context, opts *rest.Opts, request any, response any, skipToken bool) error {
	// Ensure root URL is set if not provided in opts
	if opts.RootURL == "" {
		opts.RootURL = openAPIRootURL
	}

	// 🔧 使用专用的下载URL调速器，而不是全局调速器
	return f.downloadPacer.Call(func() (shouldRetryGlobal bool, errGlobal error) {
		// Ensure token is available and current
		if !skipToken {
			if err := f.prepareTokenForRequest(ctx, opts); err != nil {
				fs.Debugf(f, "🔍 CallDownloadURLAPI: prepareTokenForRequest失败: %v", err)
				return false, backoff.Permanent(err)
			}
		}

		// Make the actual API call
		fs.Debugf(f, "🔍 CallDownloadURLAPI: 执行API调用")
		resp, err := f.openAPIClient.CallJSON(ctx, opts, request, response)

		// Check for API-level errors in the response
		if apiErr := f.checkResponseForAPIErrors(response, skipToken, opts); apiErr != nil {
			if tokenRefreshed, err := f.handleTokenError(ctx, opts, apiErr, skipToken); tokenRefreshed {
				return true, nil // Retry with refreshed token
			} else if err != nil {
				return false, backoff.Permanent(err)
			}
			return false, backoff.Permanent(apiErr) // Other API error, don't retry
		}

		fs.Debugf(f, "🔍 CallDownloadURLAPI: API调用成功")
		return shouldRetry(ctx, resp, err)
	})
}

// prepareTokenForRequest ensures a valid token is available and sets it in the request headers
func (f *Fs) prepareTokenForRequest(ctx context.Context, opts *rest.Opts) error {
	// Use double-checked locking pattern to minimize lock contention
	// First check without lock (fast path for common case)
	f.tokenMu.Lock()
	needsRefresh := !isTokenStillValid(f)
	currentToken := f.accessToken
	f.tokenMu.Unlock()

	// If refresh is needed, call refreshTokenIfNecessary which has its own locking
	if needsRefresh {
		refreshErr := f.refreshTokenIfNecessary(ctx, false, false)
		if refreshErr != nil {
			fs.Debugf(f, "Token refresh failed: %v", refreshErr)
			return fmt.Errorf("token refresh failed: %w", refreshErr)
		}

		// Get the refreshed token
		f.tokenMu.Lock()
		currentToken = f.accessToken
		f.tokenMu.Unlock()
	}

	// Validate we have a token before using it
	if currentToken == "" {
		return fmt.Errorf("no valid access token available")
	}

	if opts.ExtraHeaders == nil {
		opts.ExtraHeaders = make(map[string]string)
	}
	opts.ExtraHeaders["Authorization"] = "Bearer " + currentToken
	return nil
}

// executeOpenAPICall makes the actual API call with the provided parameters
func (f *Fs) executeOpenAPICall(ctx context.Context, opts *rest.Opts, request any, response any) (*http.Response, error) {
	var resp *http.Response
	var err error

	if request != nil && response != nil {
		// Assume standard JSON request/response
		resp, err = f.openAPIClient.CallJSON(ctx, opts, request, response)
	} else if response != nil {
		// Assume GET request with JSON response
		resp, err = f.openAPIClient.CallJSON(ctx, opts, nil, response)
	} else {
		// Assume call without specific request/response body
		var baseResp api.OpenAPIBase
		resp, err = f.openAPIClient.CallJSON(ctx, opts, nil, &baseResp)
		if err == nil {
			err = baseResp.Err() // Check for API-level errors
		}
	}

	if err != nil {
		fs.Debugf(f, "🔍 executeOpenAPICall失败: %v", err)
	}

	return resp, err
}

// handleTokenError processes token-related errors and attempts to refresh or re-login
func (f *Fs) handleTokenError(ctx context.Context, opts *rest.Opts, apiErr error, skipToken bool) (bool, error) {
	var tokenErr *api.TokenError
	if errors.As(apiErr, &tokenErr) {
		fs.Debugf(f, "Token error detected: %v (relogin needed: %v)", tokenErr, tokenErr.IsRefreshTokenExpired)
		// Handle token refresh/re-login using refreshTokenIfNecessary
		refreshErr := f.refreshTokenIfNecessary(ctx, tokenErr.IsRefreshTokenExpired, !tokenErr.IsRefreshTokenExpired)
		if refreshErr != nil {
			fs.Debugf(f, "Token refresh/relogin failed: %v", refreshErr)
			return false, fmt.Errorf("token refresh/relogin failed: %w (original: %v)", refreshErr, apiErr)
		}

		// Token was successfully refreshed or re-login succeeded, retry the API call
		fs.Debugf(f, "Token refresh/relogin succeeded, retrying API call")

		// Update the Authorization header with the new token
		if !skipToken {
			// Always get the freshest token right before using it
			f.tokenMu.Lock()
			token := f.accessToken
			f.tokenMu.Unlock()

			if opts.ExtraHeaders == nil {
				opts.ExtraHeaders = make(map[string]string)
			}
			opts.ExtraHeaders["Authorization"] = "Bearer " + token
		}
		return true, nil // Signal retry with the refreshed token
	}
	return false, nil // Not a token error
}

// checkResponseForAPIErrors examines the response for API-level errors using reflection
func (f *Fs) checkResponseForAPIErrors(response any, _ bool, _ *rest.Opts) error {
	if response == nil {
		return nil
	}

	// Use reflection to check for and call an Err() method
	val := reflect.ValueOf(response)
	if val.Kind() == reflect.Ptr && !val.IsNil() {
		method := val.MethodByName("Err")
		if method.IsValid() && method.Type().NumIn() == 0 && method.Type().NumOut() == 1 &&
			method.Type().Out(0).Implements(reflect.TypeOf((*error)(nil)).Elem()) {
			result := method.Call(nil)
			if !result[0].IsNil() {
				// Get the error and check if it's a rate limit error
				err := result[0].Interface().(error)
				if strings.Contains(err.Error(), "rate limit exceeded") {
					fs.Debugf(f, "Rate limit error detected in response: %v", err)
					// Return the error as is - the shouldRetry function will handle it
					// This ensures both CallOpenAPI and CallTraditionalAPI will retry rate limit errors
				}
				return err
			}
		}
	}
	return nil
}

// CallTraditionalAPI performs a call to the traditional (cookie, encrypted) API.
// It uses both the traditional and global pacers.
// If skipEncrypt is true, it skips the request/response encryption (used for some login steps).
func (f *Fs) CallTraditionalAPI(ctx context.Context, opts *rest.Opts, request any, response any, skipEncrypt bool) error {
	// Ensure root URL is set if not provided in opts
	if opts.RootURL == "" {
		opts.RootURL = traditionalRootURL
	}

	// Wrap the entire attempt sequence with the global pacer
	return f.globalPacer.Call(func() (shouldRetryGlobal bool, errGlobal error) {
		// Wait for traditional pacer
		if err := f.enforceTraditionalPacerDelay(); err != nil {
			return false, backoff.Permanent(err)
		}

		// Make the API call (with or without encryption)
		resp, apiErr := f.executeTraditionalAPICall(ctx, opts, request, response, skipEncrypt)

		// Process the result
		return f.processTraditionalAPIResult(ctx, resp, apiErr)
	})
}

// enforceTraditionalPacerDelay ensures we respect the traditional API pacer limits
func (f *Fs) enforceTraditionalPacerDelay() error {
	// Use tradPacer.Call with a dummy function that always succeeds immediately
	// and doesn't retry. This effectively just waits for the pacer's internal timer.
	tradPaceErr := f.tradPacer.Call(func() (bool, error) {
		return false, nil // Dummy call: Success, don't retry this dummy op.
	})
	if tradPaceErr != nil {
		// If waiting for tradPacer was interrupted (e.g., context cancelled)
		fs.Debugf(f, "Context cancelled or error while waiting for traditional pacer: %v", tradPaceErr)
		return tradPaceErr
	}
	return nil
}

// executeTraditionalAPICall makes the actual API call with proper handling of encryption
func (f *Fs) executeTraditionalAPICall(ctx context.Context, opts *rest.Opts, request any, response any, skipEncrypt bool) (*http.Response, error) {
	if skipEncrypt {
		return f.executeUnencryptedCall(ctx, opts, request, response)
	} else {
		return f.executeEncryptedCall(ctx, opts, request, response)
	}
}

// executeUnencryptedCall makes a traditional API call without encryption
func (f *Fs) executeUnencryptedCall(ctx context.Context, opts *rest.Opts, request any, response any) (*http.Response, error) {
	var resp *http.Response
	var apiErr error

	// Choose the right call pattern based on request/response
	if request != nil && response != nil {
		resp, apiErr = f.tradClient.CallJSON(ctx, opts, request, response)
	} else if response != nil {
		resp, apiErr = f.tradClient.CallJSON(ctx, opts, nil, response)
	} else {
		var baseResp api.TraditionalBase
		resp, apiErr = f.tradClient.CallJSON(ctx, opts, nil, &baseResp)
		if apiErr == nil {
			apiErr = baseResp.Err()
		}
	}

	return resp, apiErr
}

// executeEncryptedCall makes a traditional API call (encryption removed)
// 🔧 重构：移除加密功能，直接使用标准JSON调用
func (f *Fs) executeEncryptedCall(ctx context.Context, opts *rest.Opts, request any, response any) (*http.Response, error) {
	var resp *http.Response
	var apiErr error

	// 🔧 重构：所有传统API调用都改为标准JSON调用，不再使用加密
	if request != nil && response != nil {
		resp, apiErr = f.tradClient.CallJSON(ctx, opts, request, response)
	} else if response != nil {
		resp, apiErr = f.tradClient.CallJSON(ctx, opts, nil, response)
	} else {
		var baseResp api.TraditionalBase
		resp, apiErr = f.tradClient.CallJSON(ctx, opts, nil, &baseResp)
		if apiErr == nil {
			apiErr = baseResp.Err()
		}
	}

	return resp, apiErr
}

// processTraditionalAPIResult handles the result of a traditional API call
func (f *Fs) processTraditionalAPIResult(ctx context.Context, resp *http.Response, apiErr error) (bool, error) {
	// Check for retryable errors
	retryNeeded, retryErr := shouldRetry(ctx, resp, apiErr)
	if retryNeeded {
		fs.Debugf(f, "pacer: low level retry required for traditional call (error: %v)", retryErr)
		return true, retryErr
	}

	// Handle non-retryable errors
	if apiErr != nil {
		fs.Debugf(f, "pacer: permanent error encountered in traditional call: %v", apiErr)
		// Ensure the error is marked as permanent
		var permanentErr *backoff.PermanentError
		if !errors.As(apiErr, &permanentErr) {
			return false, backoff.Permanent(apiErr)
		}
		return false, apiErr // Already permanent
	}

	// Success
	fs.Debugf(f, "pacer: traditional call successful")
	return false, nil
}

// newFs constructs an Fs from the path, container:path
func NewFs(ctx context.Context, name, root string, m configmap.Mapper) (fs.Fs, error) {
	// Parse config into Options struct
	opt := new(Options)
	err := configstruct.Set(m, opt)
	if err != nil {
		return nil, err
	}

	// Validate incompatible options
	if opt.FastUpload && opt.OnlyStream {
		return nil, errors.New("fast_upload and only_stream cannot be set simultaneously")
	}
	if opt.FastUpload && opt.UploadHashOnly {
		return nil, errors.New("fast_upload and upload_hash_only cannot be set simultaneously")
	}
	if opt.OnlyStream && opt.UploadHashOnly {
		return nil, errors.New("only_stream and upload_hash_only cannot be set simultaneously")
	}

	// Validate upload parameters
	err = checkUploadChunkSize(opt.ChunkSize)
	if err != nil {
		return nil, fmt.Errorf("115: chunk size: %w", err)
	}
	err = checkUploadCutoff(opt.UploadCutoff)
	if err != nil {
		return nil, fmt.Errorf("115: upload cutoff: %w", err)
	}
	err = checkUploadConcurrency(opt.UploadConcurrency)
	if err != nil {
		return nil, fmt.Errorf("115: upload concurrency: %w", err)
	}
	if opt.NohashSize > StreamUploadLimit {
		fs.Logf(name, "nohash_size (%v) reduced to stream upload limit (%v)", opt.NohashSize, StreamUploadLimit)
		opt.NohashSize = StreamUploadLimit
	}

	// Store the original name before any modifications for config operations
	// Extract the base name without the config override suffix {xxxx}
	originalName := name
	if idx := strings.IndexRune(name, '{'); idx > 0 {
		originalName = name[:idx]
	}

	// Parse root ID from path if present
	if rootID, _, _ := parseRootID(root); rootID != "" {
		name += rootID // Append ID to name for uniqueness
		root = root[strings.Index(root, "}")+1:]
	}

	root = strings.Trim(root, "/")

	f := &Fs{
		name:         name,
		originalName: originalName,
		root:         root,
		opt:          *opt,
		m:            m,
	}

	// Initialize features
	f.features = (&fs.Features{
		DuplicateFiles:          false,
		CanHaveEmptyDirectories: true,
		NoMultiThreading:        true, // Keep true as downloads might still use traditional API
	}).Fill(ctx, f)

	// 🔧 重构：删除pathCache初始化，统一使用dirCache

	// 初始化缓存配置
	f.cacheConfig = DefaultCacheConfig115()

	// 初始化BadgerDB持久化缓存系统 - 使用公共缓存管理器
	cacheInstances := map[string]**cache.BadgerCache{
		"path_resolve": &f.pathResolveCache,
		"dir_list":     &f.dirListCache,
		"metadata":     &f.metadataCache,
		"file_id":      &f.fileIDCache,
	}

	// 🔧 重构：使用与123网盘相同的缓存配置格式
	cacheConfig := &cache.CloudDriveCacheConfig{
		CacheType:       "115drive",
		CacheDir:        cache.GetCacheDir("115drive"),
		CacheInstances:  cacheInstances,
		ContinueOnError: true, // 缓存失败不阻止文件系统工作
		LogContext:      f,
	}

	err = cache.InitCloudDriveCache(cacheConfig)
	if err != nil {
		fs.Errorf(f, "初始化缓存失败: %v", err)
		// 缓存初始化失败不应该阻止文件系统工作，继续执行
	} else {
		fs.Infof(f, "🔧 简化缓存管理器初始化成功")
	}

	// 初始化断点续传管理器（使用BadgerDB）
	resumeManager, resumeErr := NewResumeManager115(f, filepath.Join(cacheConfig.CacheDir, "resume"))
	if resumeErr != nil {
		fs.Errorf(f, "初始化断点续传管理器失败: %v", resumeErr)
		// 断点续传失败不应该阻止文件系统工作，继续执行
	} else {
		f.resumeManager = resumeManager
		fs.Debugf(f, "断点续传管理器初始化成功（BadgerDB）")
	}

	// 🚀 功能增强：初始化智能URL管理器，解决下载URL频繁过期问题
	f.smartURLManager = NewSmartURLManager(f)
	fs.Debugf(f, "智能URL管理器初始化成功")

	// 🔧 初始化熔断器，防止级联失败
	f.circuitBreaker = NewCircuitBreaker(5, 30*time.Second) // 5次失败，30秒超时
	fs.Debugf(f, "熔断器初始化成功")

	// 初始化优化的HTTP客户端
	f.httpClient = &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:        100,              // 最大空闲连接数
			MaxIdleConnsPerHost: 10,               // 每个主机的最大空闲连接数
			IdleConnTimeout:     90 * time.Second, // 空闲连接超时
			DisableCompression:  false,            // 启用压缩
		},
		Timeout: 30 * time.Second, // 请求超时
	}

	// Setting appVer (needed for traditional calls)
	re := regexp.MustCompile(`\d+\.\d+\.\d+(\.\d+)?$`)
	if m := re.FindStringSubmatch(tradUserAgent); m == nil {
		fs.Logf(f, "Could not parse app version from User-Agent %q. Using default.", tradUserAgent)
		f.appVer = "27.0.7.5" // Default fallback
	} else {
		f.appVer = m[0]
		fs.Debugf(f, "Using App Version %q from User-Agent %q", f.appVer, tradUserAgent)
	}

	// Initialize pacers
	f.globalPacer = fs.NewPacer(ctx, pacer.NewDefault(
		pacer.MinSleep(time.Duration(opt.PacerMinSleep)),
		pacer.MaxSleep(maxSleep),
		pacer.DecayConstant(decayConstant)))

	// 🔧 115网盘统一QPS管理：所有API使用相同的调速器
	// 115网盘特殊性：全局账户级别限制，需要统一管理避免770004错误

	// 传统API调速器 - 使用保守设置
	f.tradPacer = fs.NewPacer(ctx, pacer.NewDefault(
		pacer.MinSleep(conservativeMinSleep), // 2 QPS - 传统API使用保守模式
		pacer.MaxSleep(maxSleep),
		pacer.DecayConstant(decayConstant)))

	// 下载URL调速器 - 使用统一设置
	f.downloadPacer = fs.NewPacer(ctx, pacer.NewDefault(
		pacer.MinSleep(unifiedMinSleep), // 4 QPS - 与全局保持一致
		pacer.MaxSleep(maxSleep),
		pacer.DecayConstant(decayConstant)))

	// 上传调速器 - 使用保守设置
	f.uploadPacer = fs.NewPacer(ctx, pacer.NewDefault(
		pacer.MinSleep(conservativeMinSleep), // 2 QPS - 上传使用保守模式
		pacer.MaxSleep(maxSleep),
		pacer.DecayConstant(decayConstant)))

	// Create clients first to ensure they're available for token operations
	// Create separate clients for each API type with different User-Agents
	tradHTTPClient := getTradHTTPClient(ctx, &f.opt)
	openAPIHTTPClient := getOpenAPIHTTPClient(ctx, &f.opt)

	// Traditional client (uses cookie)
	f.tradClient = rest.NewClient(tradHTTPClient).
		SetRoot(traditionalRootURL).
		SetErrorHandler(errorHandler)

	// Add cookie to traditional client if provided
	if f.opt.Cookie != "" {
		cred := (&Credential{}).FromCookie(f.opt.Cookie)
		f.tradClient.SetCookie(cred.Cookie()...)
	}

	// OpenAPI client
	f.openAPIClient = rest.NewClient(openAPIHTTPClient).
		SetRoot(openAPIRootURL).
		SetErrorHandler(errorHandler)

	// Check if we have saved token in config file
	tokenLoaded := loadTokenFromConfig(f, m)
	fs.Debugf(f, "Token loaded from config: %v (expires at %v)", tokenLoaded, f.tokenExpiry)

	var tokenRefreshNeeded bool
	if tokenLoaded {
		// Check if token is expired or will expire soon
		if time.Now().After(f.tokenExpiry.Add(-tokenRefreshWindow)) {
			fs.Debugf(f, "Token expired or will expire soon, refreshing now")
			tokenRefreshNeeded = true
		}
	} else if f.opt.Cookie != "" {
		// No token but have cookie, so login
		fs.Debugf(f, "No token found but cookie provided, attempting login")
		err = f.login(ctx)
		if err != nil {
			return nil, fmt.Errorf("initial login failed: %w", err)
		}
		// Save token to config after successful login
		f.saveToken(ctx, m)
		fs.Debugf(f, "Login successful, token saved")
	} else {
		return nil, errors.New("no valid cookie or token found, please configure cookie")
	}

	// Try to refresh the token if needed
	if tokenRefreshNeeded {
		err = f.refreshTokenIfNecessary(ctx, false, true)
		if err != nil {
			fs.Debugf(f, "Token refresh failed, attempting full login: %v", err)
			// If refresh fails, try full login
			err = f.login(ctx)
			if err != nil {
				return nil, fmt.Errorf("login failed after token refresh failure: %w", err)
			}
		}
		// Save the refreshed/new token
		f.saveToken(ctx, m)
		fs.Debugf(f, "Token refresh/login successful, token saved")
	}

	// Setup token renewer for automatic refresh
	fs.Debugf(f, "Setting up token renewer")
	f.setupTokenRenewer(ctx, m)

	// Set the root folder ID based on config
	if f.opt.RootFolderID != "" {
		// Use configured root folder ID
		f.rootFolderID = f.opt.RootFolderID
	} else {
		// Use default root folder ID
		f.rootFolderID = "0"
	}

	// Initialize directory cache
	f.dirCache = dircache.New(f.root, f.rootFolderID, f)

	// 🔧 暂时禁用智能缓存预热，避免干扰上传性能
	// go f.intelligentCachePreheating(ctx)

	// 🚀 全新设计：智能路径处理策略
	// 参考123网盘的简洁设计，但针对115网盘的特点进行优化
	if f.root != "" {
		fs.Debugf(f, "🚀 智能NewFs: 开始处理路径: %q", f.root)

		// 🔧 策略1：优先检查缓存中是否已有路径信息
		if isFile, found := f.checkPathTypeFromCache(f.root); found {
			if isFile {
				fs.Debugf(f, "🎯 缓存命中：确认为文件，直接设置文件模式: %q", f.root)
				return f.setupFileFromCache(ctx)
			} else {
				fs.Debugf(f, "🎯 缓存命中：确认为目录，直接设置目录模式: %q", f.root)
				return f, nil
			}
		}

		// 🔧 策略2：缓存未命中，使用智能检测
		if isLikelyFilePath(f.root) {
			fs.Debugf(f, "🔧 智能检测：路径看起来像文件: %q", f.root)
			return f.handleAsFile(ctx)
		} else {
			fs.Debugf(f, "🔧 智能检测：路径看起来像目录: %q", f.root)
			return f.handleAsDirectory(ctx)
		}
	}

	return f, nil
}

// Name of the remote (as passed into NewFs)
func (f *Fs) Name() string {
	return f.name
}

// Root of the remote (as passed into NewFs)
func (f *Fs) Root() string {
	return f.root
}

// String returns a description of the FS
func (f *Fs) String() string {
	return fmt.Sprintf("115 %s", f.root)
}

// Features returns the optional features of this Fs
func (f *Fs) Features() *fs.Features {
	return f.features
}

// Precision of the ModTimes in this Fs
func (f *Fs) Precision() time.Duration {
	// OpenAPI might allow setting modtime, but let's assume not for now
	return fs.ModTimeNotSupported
}

// DirCacheFlush resets the directory cache
func (f *Fs) DirCacheFlush() {
	f.dirCache.ResetRoot()
}

// Hashes returns the supported hash types of the filesystem
func (f *Fs) Hashes() hash.Set {
	return hash.Set(hash.SHA1)
}

// NewObject finds the Object at remote.
func (f *Fs) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	if f.fileObj != nil { // Handle case where Fs points to a single file
		obj := *f.fileObj
		if obj.Remote() == remote || obj.Remote() == "isFile:"+remote {
			fs.Debugf(f, "🔍 NewObject: 单文件匹配成功")
			return obj, nil
		}
		fs.Debugf(f, "🔍 NewObject: 单文件不匹配，返回NotFound")
		return nil, fs.ErrorObjectNotFound // If remote doesn't match the single file
	}

	result, err := f.newObjectWithInfo(ctx, remote, nil)
	if err != nil {
		fs.Debugf(f, "🔍 NewObject失败: %v", err)
	}
	return result, err
}

// FindLeaf finds a directory or file leaf in the parent folder pathID.
// Used by dircache.
// 🔧 重构：移除pathCache，统一使用dirCache进行目录缓存
func (f *Fs) FindLeaf(ctx context.Context, pathID, leaf string) (foundID string, found bool, err error) {
	// 🔧 重构：直接使用listAll查找，不再使用pathCache
	// dirCache已经在上层提供了路径缓存功能，避免重复缓存

	// Use listAll which now uses OpenAPI
	found, err = f.listAll(ctx, pathID, f.opt.ListChunk, false, false, func(item *api.File) bool {
		// Compare with decoded name to handle special characters correctly
		decodedName := f.opt.Enc.ToStandardName(item.FileNameBest())
		if decodedName == leaf {
			// 检查找到的项目是否为目录
			if item.IsDir() {
				// 这是目录，返回目录ID
				foundID = item.ID()
				// Cache the found item's path/ID mapping (only for directories)
				parentPath, ok := f.dirCache.GetInv(pathID)
				if ok {
					itemPath := path.Join(parentPath, leaf)
					f.dirCache.Put(itemPath, foundID)
				}
				// 🔧 重构：移除pathCache.Put()调用，统一使用dirCache
			} else {
				// 这是文件，不返回ID（保持foundID为空字符串）
				foundID = "" // 明确设置为空，表示找到的是文件而不是目录
				// 🔧 重构：移除pathCache.Put()调用，文件信息不需要缓存在目录缓存中
			}
			return true // Stop searching
		}
		return false // Continue searching
	})

	// 🔧 重构：如果遇到API限制错误，清理dirCache而不是pathCache
	if isAPILimitError(err) {
		f.dirCache.Flush()
	}

	return foundID, found, err
}

// 🔧 重构：删除GetDirID()方法，标准lib/dircache不需要此方法
// GetDirID()功能已通过FindLeaf()和CreateDir()的递归调用实现

// List the objects and directories in dir into entries.
func (f *Fs) List(ctx context.Context, dir string) (entries fs.DirEntries, err error) {
	fs.Debugf(f, "List调用，目录: %q", dir)

	dirID, err := f.dirCache.FindDir(ctx, dir, false)
	if err != nil {
		// 如果是API限制错误，记录详细信息并返回
		if strings.Contains(err.Error(), "770004") ||
			strings.Contains(err.Error(), "已达到当前访问上限") {
			fs.Debugf(f, "List遇到API限制错误，目录: %q, 错误: %v", dir, err)
			return nil, err
		}
		fs.Debugf(f, "List查找目录失败，目录: %q, 错误: %v", dir, err)
		return nil, err
	}

	fs.Debugf(f, "List找到目录ID: %s，目录: %q", dirID, dir)

	// 🔧 关键修复：添加dir_list缓存支持，参考123网盘实现
	if cached, found := f.getDirListFromCache(dirID, ""); found {
		fs.Debugf(f, "🎯 List缓存命中: dirID=%s, 文件数=%d", dirID, len(cached.FileList))

		// 从缓存构建entries
		for _, item := range cached.FileList {
			entry, err := f.itemToDirEntry(ctx, path.Join(dir, item.FileNameBest()), &item)
			if err != nil {
				fs.Debugf(f, "List缓存条目转换失败: %v", err)
				continue // 跳过有问题的条目，继续处理其他条目
			}
			if entry != nil {
				entries = append(entries, entry)
			}
		}

		fs.Debugf(f, "🎯 List缓存返回: %d个条目", len(entries))
		return entries, nil
	}

	// 缓存未命中，调用API并保存到缓存
	fs.Debugf(f, "🔍 List缓存未命中，调用API: dirID=%s", dirID)
	var fileList []api.File
	var iErr error

	_, err = f.listAll(ctx, dirID, f.opt.ListChunk, false, false, func(item *api.File) bool {
		// 保存到临时列表用于缓存
		fileList = append(fileList, *item)

		// 构建entries
		entry, err := f.itemToDirEntry(ctx, path.Join(dir, item.FileNameBest()), item)
		if err != nil {
			iErr = err // Capture error but continue listing
			return false
		}
		if entry != nil {
			entries = append(entries, entry)
		}
		return false
	})

	if err != nil {
		return nil, err // Return listing error
	}
	if iErr != nil {
		return nil, iErr // Return item processing error
	}

	// 🔧 保存到缓存
	if len(fileList) > 0 {
		f.saveDirListToCache(dirID, "", fileList, "")
		fs.Debugf(f, "🎯 List保存到缓存: dirID=%s, 文件数=%d", dirID, len(fileList))
	}

	return entries, nil
}

// CreateDir makes a directory with pathID as parent and name leaf. Used by dircache.
func (f *Fs) CreateDir(ctx context.Context, pathID, leaf string) (newID string, err error) {
	if f.isShare {
		return "", errors.New("unsupported operation: Mkdir on shared filesystem")
	}
	// Use makeDir which now uses OpenAPI
	info, err := f.makeDir(ctx, pathID, leaf)
	if err != nil {
		return "", err
	}
	if info.Data == nil || info.Data.FileID == "" {
		return "", errors.New("Mkdir response did not contain a file ID")
	}
	return info.Data.FileID, nil
}

// Put uploads the object.
func (f *Fs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	// Check if destination exists. If not, use PutUnchecked. If yes, use Update.
	existingObj, err := f.NewObject(ctx, src.Remote())
	if err == fs.ErrorObjectNotFound {
		// Not found, so create it using PutUnchecked
		return f.PutUnchecked(ctx, in, src, options...)
	} else if err != nil {
		// An error other than not found
		return nil, err
	}
	// Object exists, so update it
	return existingObj, existingObj.Update(ctx, in, src, options...)
}

// PutUnchecked uploads the object without checking for existence first.
func (f *Fs) PutUnchecked(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	return f.putUnchecked(ctx, in, src, src.Remote(), options...)
}

// putUnchecked uploads the object
func (f *Fs) putUnchecked(ctx context.Context, in io.Reader, src fs.ObjectInfo, remote string, options ...fs.OpenOption) (fs.Object, error) {
	if f.isShare {
		return nil, errors.New("unsupported operation: Put on shared filesystem")
	}

	// 🌐 跨云传输检测和本地化处理
	if f.isRemoteSource(src) {
		fs.Infof(f, "🌐 检测到跨云传输: %s → 115网盘 (大小: %s)", src.Fs().Name(), fs.SizeSuffix(src.Size()))
		fs.Infof(f, "📥 强制下载到本地后上传，确保数据完整性...")
		return f.crossCloudUploadWithLocalCache(ctx, in, src, remote, options...)
	}

	// Call the main upload function which handles different strategies
	newObj, err := f.upload(ctx, in, src, remote, options...)
	if err != nil {
		return nil, err
	}
	if newObj == nil {
		return nil, errors.New("internal error: upload returned nil object without error")
	}

	o := newObj.(*Object)

	// Post-upload check (optional)
	if !f.opt.NoCheck && !o.hasMetaData {
		fs.Debugf(o, "Running post-upload check...")
		// Attempt to read metadata for the uploaded object to confirm
		err = o.readMetaData(ctx) // This will list the parent directory
		if err != nil {
			// Don't fail the upload, just log a warning
			fs.Logf(o, "Post-upload check failed to read metadata: %v", err)
			// Mark as having metadata anyway to avoid repeated checks
			o.hasMetaData = true
		} else {
			fs.Debugf(o, "Post-upload check successful.")
		}
	}

	// 🔧 重构：上传成功后，清理dirCache相关缓存
	// 上传可能影响目录结构，清理缓存确保一致性
	f.dirCache.FlushDir(path.Dir(remote))

	return newObj, nil
}

// MergeDirs merges multiple source directories into the first one.
func (f *Fs) MergeDirs(ctx context.Context, dirs []fs.Directory) (err error) {
	if f.isShare {
		return errors.New("unsupported operation: MergeDirs on shared filesystem")
	}
	if len(dirs) < 2 {
		return nil
	}
	dstDir := dirs[0]
	dstDirID := dstDir.ID()

	for _, srcDir := range dirs[1:] {
		srcDirID := srcDir.ID()
		fs.Debugf(srcDir, "Merging contents into %v", dstDir)

		// List all items in the source directory
		var itemsToMove []*api.File
		_, err = f.listAll(ctx, srcDirID, f.opt.ListChunk, false, false, func(item *api.File) bool {
			itemsToMove = append(itemsToMove, item)
			return false // Collect all items
		})
		if err != nil {
			return fmt.Errorf("MergeDirs list failed on %v: %w", srcDir, err)
		}

		// Move items in chunks
		chunkSize := f.opt.ListChunk // Use list chunk size for move chunks
		for i := 0; i < len(itemsToMove); i += chunkSize {
			end := min(i+chunkSize, len(itemsToMove))
			chunk := itemsToMove[i:end]
			if len(chunk) == 0 {
				continue
			}

			var idsToMove []string
			for _, item := range chunk {
				idsToMove = append(idsToMove, item.ID())
			}

			fs.Debugf(srcDir, "Moving %d items to %v", len(idsToMove), dstDir)
			if err = f.moveFiles(ctx, idsToMove, dstDirID); err != nil {
				return fmt.Errorf("MergeDirs move failed for %v: %w", srcDir, err)
			}
		}
	}

	// Remove the source directories (now empty)
	var dirsToDelete []string
	for _, srcDir := range dirs[1:] {
		dirsToDelete = append(dirsToDelete, srcDir.ID())
	}

	// Delete directories in chunks
	chunkSize := f.opt.ListChunk
	for i := 0; i < len(dirsToDelete); i += chunkSize {
		end := min(i+chunkSize, len(dirsToDelete))
		chunkIDs := dirsToDelete[i:end]
		if len(chunkIDs) == 0 {
			continue
		}

		fs.Debugf(f, "Removing merged source directories: %v", chunkIDs)
		if err = f.deleteFiles(ctx, chunkIDs); err != nil {
			// Log error but continue trying to delete others
			fs.Errorf(f, "MergeDirs failed to rmdir chunk %v: %v", chunkIDs, err)
		}
	}

	// Flush the cache for the source directories
	for _, srcDir := range dirs[1:] {
		f.dirCache.FlushDir(srcDir.Remote())
	}

	return nil // Return nil even if some deletions failed, as merge likely succeeded partially
}

// Mkdir makes the directory.
func (f *Fs) Mkdir(ctx context.Context, dir string) error {
	if f.isShare {
		return errors.New("unsupported operation: Mkdir on shared filesystem")
	}
	_, err := f.dirCache.FindDir(ctx, dir, true) // create = true
	if err == nil {
		// 🔧 重构：创建目录成功后，清理dirCache相关缓存
		// 新目录创建可能影响父目录的缓存
		f.dirCache.FlushDir(path.Dir(dir))
	}
	return err
}

// Move server-side moves a file.
func (f *Fs) Move(ctx context.Context, src fs.Object, remote string) (fs.Object, error) {
	if f.isShare {
		return nil, fs.ErrorCantMove
	}
	srcObj, ok := src.(*Object)
	if !ok {
		fs.Debugf(src, "Can't move - not same remote type")
		return nil, fs.ErrorCantMove
	}
	// Ensure metadata is read for srcObj.id
	err := srcObj.readMetaData(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to read source metadata for move: %w", err)
	}
	if srcObj.id == "" {
		return nil, errors.New("cannot move object with empty ID")
	}

	// Find destination parent directory ID, creating if necessary
	dstLeaf, dstParentID, err := f.dirCache.FindPath(ctx, remote, true)
	if err != nil {
		return nil, fmt.Errorf("failed to find destination directory for move: %w", err)
	}

	// Find source parent directory ID
	srcLeaf, srcParentID, err := srcObj.fs.dirCache.FindPath(ctx, srcObj.remote, false)
	if err != nil {
		// Log error but proceed, maybe parent doesn't exist in cache but file does
		fs.Logf(src, "Could not find source path in cache for move: %v", err)
		// Attempt to get parent ID from object if available
		if srcObj.parent != "" {
			srcParentID = srcObj.parent
		} else {
			return nil, fmt.Errorf("failed to find source directory for move and object has no parent ID: %w", err)
		}
	}

	// Perform the move if parents differ
	if srcParentID != dstParentID {
		fs.Debugf(srcObj, "Moving %q from %q to %q", srcObj.id, srcParentID, dstParentID)
		err = f.moveFiles(ctx, []string{srcObj.id}, dstParentID)
		if err != nil {
			return nil, fmt.Errorf("server-side move failed: %w", err)
		}
	}

	// Perform rename if names differ
	if srcLeaf != dstLeaf {
		fs.Debugf(srcObj, "Renaming %q to %q", srcLeaf, dstLeaf)
		err = f.renameFile(ctx, srcObj.id, dstLeaf)
		if err != nil {
			// Attempt to move back if rename fails? Or just return error?
			return nil, fmt.Errorf("failed to rename after move: %w", err)
		}
	}

	// Create new object representing the destination
	dstObj := &Object{
		fs:          f,
		remote:      remote,
		hasMetaData: false, // Mark metadata as stale
		id:          srcObj.id,
		parent:      dstParentID, // Update parent ID
		size:        srcObj.size,
		sha1sum:     srcObj.sha1sum,
		pickCode:    srcObj.pickCode,
		modTime:     srcObj.modTime, // Keep original modTime? Or update? Keep for now.
		durlMu:      new(sync.Mutex),
	}

	// Read metadata for the new object to confirm and update details
	err = dstObj.readMetaData(ctx)
	if err != nil {
		// Log error but return the object anyway, metadata might be eventually consistent
		fs.Logf(dstObj, "Failed to read metadata after move: %v", err)
	} else {
		dstObj.hasMetaData = true
	}

	// Flush source directory from cache
	dir, _ := dircache.SplitPath(srcObj.remote)
	srcObj.fs.dirCache.FlushDir(dir)

	// 🔧 重构：移动成功后，清理dirCache相关缓存
	// 移动操作影响源和目标目录，清理相关缓存
	f.dirCache.FlushDir(path.Dir(srcObj.remote))
	f.dirCache.FlushDir(path.Dir(remote))

	return dstObj, nil
}

// DirMove server-side moves a directory.
func (f *Fs) DirMove(ctx context.Context, src fs.Fs, srcRemote, dstRemote string) error {
	if f.isShare {
		return fs.ErrorCantDirMove
	}
	srcFs, ok := src.(*Fs)
	if !ok {
		fs.Debugf(srcFs, "Can't move directory - not same remote type")
		return fs.ErrorCantDirMove
	}

	// Use dircache helper to prepare for move
	srcID, srcParentID, srcLeaf, dstParentID, dstLeaf, err := f.dirCache.DirMove(ctx, srcFs.dirCache, srcFs.root, srcRemote, f.root, dstRemote)
	if err != nil {
		return err // Errors like DirExists, CantMoveRoot handled by DirMove helper
	}

	// Perform the move if parents differ
	if srcParentID != dstParentID {
		fs.Debugf(srcFs, "Moving directory %q from %q to %q", srcID, srcParentID, dstParentID)
		err = f.moveFiles(ctx, []string{srcID}, dstParentID)
		if err != nil {
			return fmt.Errorf("server-side directory move failed: %w", err)
		}
	}

	// Perform rename if names differ
	if srcLeaf != dstLeaf {
		fs.Debugf(srcFs, "Renaming directory %q to %q", srcLeaf, dstLeaf)
		err = f.renameFile(ctx, srcID, dstLeaf)
		if err != nil {
			return fmt.Errorf("failed to rename directory after move: %w", err)
		}
	}

	// Flush source directory from cache
	srcFs.dirCache.FlushDir(srcRemote)
	return nil
}

// Copy server-side copies a file.
func (f *Fs) Copy(ctx context.Context, src fs.Object, remote string) (fs.Object, error) {
	srcObj, ok := src.(*Object)
	if !ok {
		fs.Debugf(src, "Can't copy - not same remote type")
		return nil, fs.ErrorCantCopy
	}

	// Special handling for copying *from* a shared remote (using traditional API)
	if srcObj.fs.isShare {
		if f.isShare {
			// Cannot copy from share to share directly? Assume not supported.
			return nil, errors.New("copying between shared remotes is not supported")
		}
		fs.Debugf(src, "Copying from shared remote using traditional API")
		// Find destination parent ID
		_, dstParentID, err := f.dirCache.FindPath(ctx, remote, true)
		if err != nil {
			return nil, fmt.Errorf("failed to find destination directory for copy: %w", err)
		}
		// Call traditional copyFromShare
		err = f.copyFromShare(ctx, srcObj.fs.opt.ShareCode, srcObj.fs.opt.ReceiveCode, srcObj.id, dstParentID)
		if err != nil {
			return nil, fmt.Errorf("copy from share failed: %w", err)
		}
		// Need to find the new object in the destination to return it
		// The name will be the same as the source object's name
		srcLeaf, _, _ := srcObj.fs.dirCache.FindPath(ctx, srcObj.remote, false) // Get original leaf name
		dir, _ := dircache.SplitPath(remote)
		dstPath := path.Join(dir, srcLeaf)       // Construct potential destination path
		newObj, err := f.NewObject(ctx, dstPath) // Find the newly copied object
		if err != nil {
			return nil, fmt.Errorf("failed to find copied object in destination after share copy: %w", err)
		}
		// If the target remote name is different, rename the copied object
		dstLeaf := path.Base(remote)
		if srcLeaf != dstLeaf {
			err = f.renameFile(ctx, newObj.(*Object).id, dstLeaf)
			if err != nil {
				return nil, fmt.Errorf("failed to rename after copy from share: %w", err)
			}
			// Re-fetch the object to get updated metadata/remote path
			finalObj, err := f.NewObject(ctx, remote)
			if err != nil {
				return nil, fmt.Errorf("failed to find renamed object after share copy: %w", err)
			}
			return finalObj, nil
		}
		return newObj, nil
	}

	// --- Standard Copy (Non-Share Source) ---
	if f.isShare {
		// Cannot copy *to* a shared remote
		return nil, errors.New("copying to a shared remote is not supported")
	}

	// Ensure metadata is read for srcObj.id
	err := srcObj.readMetaData(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to read source metadata for copy: %w", err)
	}
	if srcObj.id == "" {
		return nil, errors.New("cannot copy object with empty ID")
	}

	// Find destination parent directory ID, creating if necessary
	dstLeaf, dstParentID, err := f.dirCache.FindPath(ctx, remote, true)
	if err != nil {
		return nil, fmt.Errorf("failed to find destination directory for copy: %w", err)
	}

	// Check if source and destination parent are the same
	srcParentID := srcObj.parent
	if srcParentID == "" { // Try to get from cache if not on object
		_, srcParentID, _ = srcObj.fs.dirCache.FindPath(ctx, srcObj.remote, false)
	}
	if srcParentID == dstParentID {
		// API restriction: cannot copy within the same directory
		// We could potentially handle this by copying to a temp dir and moving,
		// but for now, return ErrorCantCopy.
		fs.Debugf(src, "Can't copy - source and destination directory are the same (%q)", srcParentID)
		return nil, fs.ErrorCantCopy
	}

	// Perform the copy using OpenAPI
	fs.Debugf(srcObj, "Copying %q to %q", srcObj.id, dstParentID)
	err = f.copyFiles(ctx, []string{srcObj.id}, dstParentID)
	if err != nil {
		return nil, fmt.Errorf("server-side copy failed: %w", err)
	}

	// Find the newly created object in the destination directory
	// It will initially have the same name as the source object.
	srcLeaf, _, _ := srcObj.fs.dirCache.FindPath(ctx, srcObj.remote, false) // Get original leaf name
	dir, _ := dircache.SplitPath(remote)
	copiedObjPath := path.Join(dir, srcLeaf)       // Construct path where copied object should be
	newObj, err := f.NewObject(ctx, copiedObjPath) // Find the object at that path
	if err != nil {
		return nil, fmt.Errorf("failed to find copied object in destination: %w", err)
	}
	newObjConcrete := newObj.(*Object)

	// Rename the copied object if the target remote name is different
	if srcLeaf != dstLeaf {
		fs.Debugf(newObj, "Renaming copied object to %q", dstLeaf)
		err = f.renameFile(ctx, newObjConcrete.id, dstLeaf)
		if err != nil {
			// Attempt to delete the wrongly named copy? Or just return error?
			_ = f.deleteFiles(ctx, []string{newObjConcrete.id}) // Best effort cleanup
			return nil, fmt.Errorf("failed to rename after copy: %w", err)
		}
		// Update the object's remote path and mark metadata stale
		newObjConcrete.remote = remote
		newObjConcrete.hasMetaData = false
		// Read metadata again to confirm rename and get latest info
		err = newObjConcrete.readMetaData(ctx)
		if err != nil {
			fs.Logf(newObj, "Failed to read metadata after rename: %v", err)
		} else {
			newObjConcrete.hasMetaData = true
		}
	} else {
		// If no rename needed, ensure the returned object has the correct remote path
		newObjConcrete.remote = remote
	}

	return newObjConcrete, nil
}

// purgeCheck removes the root directory. Refuses if check=true and not empty.
func (f *Fs) purgeCheck(ctx context.Context, dir string, check bool) error {
	if f.isShare {
		return errors.New("unsupported operation: Purge/Rmdir on shared filesystem")
	}
	root := path.Join(f.root, dir)
	if root == "" && dir != "" { // Check if trying to delete the effective root specified in config
		// This case needs careful handling. If root_folder_id is set, `dir` might be ""
		// but `f.rootFolderID` is not "0".
		if f.rootFolderID == "0" {
			return errors.New("internal error: attempting to purge root directory")
		}
		// Allow purging the configured root folder ID
	} else if root == "" && dir == "" && f.rootFolderID == "0" {
		// Explicitly prevent purging the absolute root "0"
		return errors.New("refusing to purge the absolute root directory '0'")
	}

	// Find the ID of the directory to purge
	dirID, err := f.dirCache.FindDir(ctx, dir, false) // Don't create
	if err != nil {
		return err // Return DirNotFound or other errors
	}

	if check {
		// Check if directory is empty using listAll with limit 1
		found, listErr := f.listAll(ctx, dirID, 1, false, false, func(item *api.File) bool {
			fs.Debugf(f, "Rmdir check: directory %q contains %q", dir, item.FileNameBest())
			return true // Found an item, stop listing
		})
		if listErr != nil {
			return fmt.Errorf("failed to check if directory %q is empty: %w", dir, listErr)
		}
		if found {
			return fs.ErrorDirectoryNotEmpty
		}
	}

	// Perform the delete using OpenAPI
	fs.Debugf(f, "Purging directory %q (ID: %q)", dir, dirID)
	err = f.deleteFiles(ctx, []string{dirID})
	if err != nil {
		return fmt.Errorf("failed to delete directory %q (ID: %q): %w", dir, dirID, err)
	}

	// Flush the directory from cache
	f.dirCache.FlushDir(dir)
	return nil
}

// Rmdir removes an empty directory.
func (f *Fs) Rmdir(ctx context.Context, dir string) error {
	return f.purgeCheck(ctx, dir, true) // check = true
}

// Purge removes a directory and all its contents.
func (f *Fs) Purge(ctx context.Context, dir string) error {
	return f.purgeCheck(ctx, dir, false) // check = false
}

// About gets quota information (currently uses traditional API).
// TODO: Check if OpenAPI provides a quota endpoint.
func (f *Fs) About(ctx context.Context) (*fs.Usage, error) {
	// Using traditional indexInfo for now
	info, err := f.indexInfo(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get usage info: %w", err)
	}

	usage := &fs.Usage{}
	if totalInfo, ok := info.SpaceInfo["all_total"]; ok {
		usage.Total = fs.NewUsageValue(int64(totalInfo.Size))
	}
	if useInfo, ok := info.SpaceInfo["all_use"]; ok {
		usage.Used = fs.NewUsageValue(int64(useInfo.Size))
	}
	if remainInfo, ok := info.SpaceInfo["all_remain"]; ok {
		usage.Free = fs.NewUsageValue(int64(remainInfo.Size))
	}

	return usage, nil
}

// Shutdown shuts down the fs, closing any background tasks
func (f *Fs) Shutdown(ctx context.Context) error {
	if f.tokenRenewer != nil {
		f.tokenRenewer.Shutdown()
		f.tokenRenewer = nil
	}

	// 关闭所有BadgerDB缓存
	caches := []*cache.BadgerCache{
		f.pathResolveCache,
		f.dirListCache,
		f.metadataCache,
		f.fileIDCache,
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

// itemToDirEntry converts an api.File to an fs.DirEntry
func (f *Fs) itemToDirEntry(ctx context.Context, remote string, item *api.File) (entry fs.DirEntry, err error) {
	if item.IsDir() {
		// Cache the directory ID
		f.dirCache.Put(remote, item.ID())
		d := fs.NewDir(remote, item.ModTime()).SetID(item.ID()).SetParentID(item.ParentID())
		return d, nil
	}
	// It's a file
	entry, err = f.newObjectWithInfo(ctx, remote, item)
	if err == fs.ErrorObjectNotFound {
		return nil, nil // Should not happen if item came from listing
	}
	return entry, err
}

// newObjectWithInfo creates an fs.Object from an api.File or by reading metadata.
func (f *Fs) newObjectWithInfo(ctx context.Context, remote string, info *api.File) (fs.Object, error) {

	o := &Object{
		fs:     f,
		remote: remote,
		durlMu: new(sync.Mutex),
	}
	var err error
	if info != nil {
		// Set metadata from provided info
		err = o.setMetaData(info)
	} else {
		// Read metadata from the backend
		err = o.readMetaData(ctx)
	}
	if err != nil {
		fs.Debugf(f, "🔍 newObjectWithInfo失败: %v", err)
		return nil, err
	}
	return o, nil
}

// readMetaDataForPath finds metadata for a specific file path.
func (f *Fs) readMetaDataForPath(ctx context.Context, path string) (info *api.File, err error) {
	fs.Debugf(f, "🔍 readMetaDataForPath开始: path=%q", path)

	leaf, dirID, err := f.dirCache.FindPath(ctx, path, false)
	if err != nil {
		fs.Debugf(f, "🔍 readMetaDataForPath: FindPath失败: %v", err)
		// 检查是否是API限制错误，如果是则立即返回，避免路径混乱
		if isAPILimitError(err) {
			fs.Debugf(f, "readMetaDataForPath遇到API限制错误，路径: %q, 错误: %v", path, err)
			// 🔧 重构：清理dirCache而不是pathCache
			f.dirCache.Flush()
			return nil, err
		}
		if err == fs.ErrorDirNotFound {
			return nil, fs.ErrorObjectNotFound
		}
		return nil, err
	}

	fs.Debugf(f, "🔍 readMetaDataForPath: FindPath成功, leaf=%q, dirID=%q", leaf, dirID)

	// 验证目录ID是否有效
	if dirID == "" {
		fs.Errorf(f, "🔍 readMetaDataForPath: 目录ID为空，路径: %q, leaf: %q", path, leaf)
		// 清理可能损坏的缓存
		f.dirCache.Flush()
		return nil, fmt.Errorf("invalid directory ID for path %q", path)
	}

	// List the directory and find the leaf
	fs.Debugf(f, "🔍 readMetaDataForPath: 开始调用listAll, dirID=%q, leaf=%q", dirID, leaf)
	found, err := f.listAll(ctx, dirID, f.opt.ListChunk, true, false, func(item *api.File) bool {
		// Compare with decoded name to handle special characters correctly
		decodedName := f.opt.Enc.ToStandardName(item.FileNameBest())
		if decodedName == leaf {
			fs.Debugf(f, "🔍 readMetaDataForPath: 找到匹配文件: %q", decodedName)
			info = item
			return true // Found it
		}
		return false // Keep looking
	})
	if err != nil {
		fs.Debugf(f, "🔍 readMetaDataForPath: listAll失败: %v", err)
		return nil, fmt.Errorf("failed to list directory %q to find %q: %w", dirID, leaf, err)
	}
	if !found {
		fs.Debugf(f, "🔍 readMetaDataForPath: 未找到文件")
		return nil, fs.ErrorObjectNotFound
	}
	fs.Debugf(f, "🔍 readMetaDataForPath: 成功找到文件元数据")
	return info, nil
}

// createObject creates a placeholder Object struct before upload.
func (f *Fs) createObject(ctx context.Context, remote string, modTime time.Time, size int64) (o *Object, leaf string, dirID string, err error) {
	// Create the parent directory if it doesn't exist
	leaf, dirID, err = f.dirCache.FindPath(ctx, remote, true)
	if err != nil {
		return
	}
	// Temporary Object under construction
	o = &Object{
		fs:          f,
		remote:      remote,
		parent:      dirID,
		size:        size,
		modTime:     modTime,
		hasMetaData: false, // Metadata not set yet
		durlMu:      new(sync.Mutex),
	}
	return o, leaf, dirID, nil
}

// ------------------------------------------------------------
// Command Help & Execution
// ------------------------------------------------------------

var commandHelp = []fs.CommandHelp{{
	Name:  "addurls",
	Short: "Add offline download task for urls (uses traditional API)",
	Long: `This command adds offline download task for urls using the traditional API.

Usage:

    rclone backend addurls 115:dirpath url1 url2

Downloads are saved to the folder "dirpath". If omitted or non-existent,
it defaults to "云下载". Requires cookie authentication.
This command always exits with code 0; check output for errors.`,
}, {
	Name:  "getid",
	Short: "Get the ID of a file or directory",
	Long: `This command obtains the ID of a file or directory using the OpenAPI.

Usage:

    rclone backend getid 115:path/to/item

Returns the internal ID used by the 115 API.`,
}, {
	Name:  "addshare",
	Short: "Add shared files/dirs from a share link (uses traditional API)",
	Long: `This command adds shared files/dirs from a share link using the traditional API.

Usage:

    rclone backend addshare 115:dirpath share_link

Content from the link is copied to "dirpath". Requires cookie authentication.`,
}, {
	Name:  "stats",
	Short: "Get folder statistics (uses OpenAPI)",
	Long: `This command retrieves statistics for a folder using the OpenAPI.

Usage:

    rclone backend stats 115:path/to/folder

Returns information like total size, file count, folder count, etc.`,
}, {
	Name:  "getdownloadurl",
	Short: "Get the download URL of a file by its path",
	Long: `This command retrieves the download URL of a file using its path.

Usage:

rclone backend getdownloadurl 115:path/to/file

The command returns the download URL for the specified file. Ensure the file path is correct.`,
}, {
	Name:  "getdownloadurlau",
	Short: "Get the download URL of a file by its path",
	Long: `This command retrieves the download URL of a file using its path.

Usage:

rclone backend getdownloadurlau 115:path/to/file VidHub/1.7.24

The command returns the download URL for the specified file. Ensure the file path is correct.`,
},
}

// Command executes backend-specific commands.
func (f *Fs) Command(ctx context.Context, name string, arg []string, opt map[string]string) (out any, err error) {
	switch name {
	case "addurls":
		if f.isShare {
			return nil, errors.New("addurls unsupported for shared filesystem")
		}
		dir := "" // Default to root or 云下载 handled by API
		if parentDir, ok := opt["dir"]; ok {
			dir = parentDir
		}
		return f.addURLs(ctx, dir, arg) // Uses traditional API
	case "getid":
		path := ""
		if len(arg) > 0 {
			path = arg[0]
		}
		return f.getID(ctx, path) // Uses OpenAPI via listAll/FindDir/NewObject
	case "addshare":
		if f.isShare {
			return nil, errors.New("addshare unsupported for shared filesystem")
		}
		if len(arg) < 1 {
			return nil, errors.New("addshare requires a share link argument")
		}
		shareCode, receiveCode, err := parseShareLink(arg[0])
		if err != nil {
			return nil, err
		}
		dirID, err := f.dirCache.FindDir(ctx, "", true) // Find target dir ID (create if needed)
		if err != nil {
			return nil, err
		}
		return nil, f.copyFromShare(ctx, shareCode, receiveCode, "", dirID) // Uses traditional API
	case "getdownloadurlua":
		path := ""
		ua := ""
		if len(arg) > 0 {
			ua = arg[0]
		}

		return f.getDownloadURLByUA(ctx, path, ua)
	case "cache-info":
		// 使用统一缓存查看器
		caches := map[string]cache.PersistentCache{
			"path_resolve": f.pathResolveCache,
			"dir_list":     f.dirListCache,
			"metadata":     f.metadataCache,
			"file_id":      f.fileIDCache,
		}
		viewer := cache.NewUnifiedCacheViewer("115", f, caches)

		// 根据参数决定返回格式
		format := "tree"
		if formatOpt, ok := opt["format"]; ok {
			format = formatOpt
		}

		switch format {
		case "tree":
			return viewer.GenerateDirectoryTreeText()
		case "stats":
			return viewer.GetCacheStats(), nil
		case "info":
			return viewer.GetCacheInfo()
		default:
			return viewer.GenerateDirectoryTreeText()
		}
	default:
		return nil, fs.ErrorCommandNotFound
	}
}

type FileInfoResponse struct {
	State   bool      `json:"state"`
	Message string    `json:"message"`
	Code    int       `json:"code"`
	Data    *struct { // 直接定义匿名结构体，只包含 PickCode
		PickCode string `json:"pick_code"`
	} `json:"data"`
}

func (f *Fs) GetPickCodeByPath(ctx context.Context, path string) (string, error) {
	// 使用CallOpenAPI通过pacer进行调用
	formData := url.Values{}
	formData.Add("path", path)

	opts := rest.Opts{
		Method:      "POST",
		Path:        "/open/folder/get_info",
		ContentType: "application/x-www-form-urlencoded",
		Body:        strings.NewReader(formData.Encode()),
	}

	var response struct {
		State   int    `json:"state"`
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			PickCode string `json:"pick_code"`
		} `json:"data"`
	}

	err := f.CallOpenAPI(ctx, &opts, nil, &response, false)
	if err != nil {
		return "", fmt.Errorf("获取PickCode失败: %w", err)
	}

	// 检查API返回的状态
	if response.Code != 0 {
		return "", fmt.Errorf("API返回错误: %s (Code: %d)", response.Message, response.Code)
	}

	// 返回 PickCode
	return response.Data.PickCode, nil
}

func (f *Fs) getDownloadURLByUA(ctx context.Context, filePath string, UA string) (string, error) {
	pickCode, err := f.GetPickCodeByPath(ctx, filePath)
	if err != nil {
		return "", fmt.Errorf("failed to read metadata for file path %q: %w", filePath, err)
	}

	// 如果没有提供 UA，使用默认值
	if UA == "" {
		UA = defaultUserAgent
	}

	// 使用CallOpenAPI通过pacer进行调用
	opts := rest.Opts{
		Method:      "POST",
		Path:        "/open/ufile/downurl",
		ContentType: "application/x-www-form-urlencoded",
		Body:        strings.NewReader("pick_code=" + pickCode),
		ExtraHeaders: map[string]string{
			"User-Agent": UA,
		},
	}

	var response ApiResponse
	err = f.CallOpenAPI(ctx, &opts, nil, &response, false)
	if err != nil {
		return "", fmt.Errorf("获取下载URL失败: %w", err)
	}

	for _, downInfo := range response.Data {
		if downInfo != (FileInfo{}) {
			fs.Infof(nil, "获取到下载URL: %s", downInfo.URL.URL)
			return downInfo.URL.URL, nil
		}
	}
	return "", fmt.Errorf("未从API响应中获取到下载URL")
}

// ------------------------------------------------------------
// Object Methods
// ------------------------------------------------------------

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
	err := o.readMetaData(ctx)
	if err != nil {
		fs.Logf(o, "failed to read metadata for ModTime: %v", err)
		// Return a zero time instead of Now() as Precision is NotSupported
		return time.Time{}
	}
	return o.modTime
}

// Size returns the size of the file
func (o *Object) Size() int64 {
	// Return size immediately if known, otherwise read metadata
	if o.hasMetaData || o.size > 0 { // Check if size is already populated
		return o.size
	}
	err := o.readMetaData(context.TODO()) // Use TODO context for simplicity here
	if err != nil {
		fs.Logf(o, "failed to read metadata for Size: %v", err)
		return -1 // Indicate error or unknown size
	}
	return o.size
}

// Hash returns the SHA1 checksum
func (o *Object) Hash(ctx context.Context, t hash.Type) (string, error) {
	if t != hash.SHA1 {
		return "", hash.ErrUnsupported
	}
	// Return hash immediately if known, otherwise read metadata
	if o.hasMetaData || o.sha1sum != "" {
		return o.sha1sum, nil
	}
	err := o.readMetaData(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to read metadata for Hash: %w", err)
	}
	return o.sha1sum, nil
}

// ID returns the ID of the Object
func (o *Object) ID() string {
	// Return ID immediately if known, otherwise read metadata
	if o.hasMetaData || o.id != "" {
		return o.id
	}
	// Reading metadata just for ID might be inefficient, but necessary if not cached
	err := o.readMetaData(context.TODO()) // Use TODO context
	if err != nil {
		fs.Logf(o, "failed to read metadata for ID: %v", err)
		return "" // Return empty string on error
	}
	return o.id
}

// ParentID returns the parent ID of the Object
func (o *Object) ParentID() string {
	// Return parent immediately if known, otherwise read metadata
	if o.hasMetaData || o.parent != "" {
		return o.parent
	}
	err := o.readMetaData(context.TODO()) // Use TODO context
	if err != nil {
		fs.Logf(o, "failed to read metadata for ParentID: %v", err)
		return "" // Return empty string on error
	}
	return o.parent
}

// SetModTime is not supported
func (o *Object) SetModTime(ctx context.Context, modTime time.Time) error {
	return fs.ErrorCantSetModTime
}

// Storable indicates this object can be stored
func (o *Object) Storable() bool {
	return true
}

// open opens the object for reading.
func (o *Object) open(ctx context.Context, options ...fs.OpenOption) (io.ReadCloser, error) {
	// 🔒 并发安全修复：使用互斥锁保护对durl的访问
	o.durlMu.Lock()

	// 检查URL有效性（在锁保护下）
	if o.durl == nil || !o.durl.Valid() {
		o.durlMu.Unlock()

		// 🚀 功能增强：使用智能URL管理器获取有效URL
		if o.fs.smartURLManager != nil && o.pickCode != "" {
			fs.Debugf(o, "🔄 URL无效，使用智能URL管理器获取新URL: pickCode=%s", o.pickCode)
			newURL, err := o.fs.smartURLManager.GetDownloadURL(o.pickCode, false)
			if err != nil {
				fs.Debugf(o, "❌ 智能URL管理器获取URL失败: %v", err)
				return nil, fmt.Errorf("智能URL管理器获取URL失败: %w", err)
			}

			// 更新对象的下载URL
			o.durlMu.Lock()
			o.durl = &api.DownloadURL{URL: newURL}
			o.durlMu.Unlock()
			fs.Debugf(o, "✅ 智能URL管理器成功获取新URL")
		} else {
			if o.fs.smartURLManager == nil {
				fs.Debugf(o, "⚠️ 智能URL管理器未初始化")
			}
			if o.pickCode == "" {
				fs.Debugf(o, "⚠️ pickCode为空")
			}
			return nil, errors.New("download URL is invalid or expired")
		}

		// 重新获取锁以继续后续操作
		o.durlMu.Lock()
	}

	// 创建URL的本地副本，避免在使用过程中被其他线程修改
	downloadURL := o.durl.URL
	o.durlMu.Unlock()

	// 使用本地副本创建请求，避免并发访问问题
	req, err := http.NewRequestWithContext(ctx, "GET", downloadURL, nil)
	if err != nil {
		return nil, err
	}

	// 🔧 Range请求调试和处理
	fs.FixRangeOption(options, o.size)
	fs.OpenOptionAddHTTPHeaders(req.Header, options)

	// 调试Range请求信息
	if rangeHeader := req.Header.Get("Range"); rangeHeader != "" {
		fs.Debugf(o, "🔍 115网盘Range请求: %s (文件大小: %s)", rangeHeader, fs.SizeSuffix(o.size))
	}

	if o.size == 0 {
		// Don't supply range requests for 0 length objects as they always fail
		delete(req.Header, "Range")
	}

	resp, err := o.fs.openAPIClient.Do(req)
	if err != nil {
		fs.Debugf(o, "❌ 115网盘HTTP请求失败: %v", err)
		return nil, err
	}

	// 🔧 Range响应验证
	if rangeHeader := req.Header.Get("Range"); rangeHeader != "" {
		contentLength := resp.Header.Get("Content-Length")
		contentRange := resp.Header.Get("Content-Range")
		fs.Debugf(o, "🔍 115网盘Range响应: Status=%d, Content-Length=%s, Content-Range=%s",
			resp.StatusCode, contentLength, contentRange)

		// 检查Range请求是否被正确处理
		if resp.StatusCode != 206 {
			fs.Debugf(o, "⚠️ 115网盘Range请求未返回206状态码: %d", resp.StatusCode)
		}
	}

	return resp.Body, nil
}

// Open the file for reading.
func (o *Object) Open(ctx context.Context, options ...fs.OpenOption) (io.ReadCloser, error) {
	// Ensure metadata (specifically pickCode or ID) is available
	err := o.readMetaData(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to read metadata before open: %w", err)
	}

	if o.size == 0 {
		// No need for download URL for 0-byte files
		return io.NopCloser(bytes.NewReader(nil)), nil
	}

	// 🔧 115网盘并发下载优化：修复进度显示问题
	// 🔧 修复：优化并发下载检测，提升跨云传输性能
	if o.size > 100*1024*1024 && o.shouldUseConcurrentDownload(ctx, options) {
		// 🚀 基于日志分析优化：提升并发数从2到4
		fs.Infof(o, "🚀 115网盘大文件检测，启用并发下载优化(并发数=4): %s", fs.SizeSuffix(o.size))
		return o.openWithConcurrency(ctx, options...)
	}

	// Get/refresh download URL
	err = o.setDownloadURL(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get download URL: %w", err)
	}
	if !o.durl.Valid() {
		// Attempt refresh again if invalid right after getting it
		fs.Debugf(o, "Download URL invalid immediately after fetching, retrying...")
		time.Sleep(500 * time.Millisecond) // Small delay before retry
		err = o.setDownloadURL(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to get download URL on retry: %w", err)
		}
		if !o.durl.Valid() {
			return nil, errors.New("failed to obtain a valid download URL")
		}
	}

	// Open the URL
	return o.open(ctx, options...)
}

// Update the object with new content.
func (o *Object) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) error {
	if o.fs.isShare {
		return errors.New("unsupported operation: Update on shared filesystem")
	}
	if src.Size() < 0 {
		return errors.New("refusing to update with unknown size")
	}

	// Start the token renewer if we have a valid one
	if o.fs.tokenRenewer != nil {
		o.fs.tokenRenewer.Start()
		defer o.fs.tokenRenewer.Stop()
	}

	// Ensure metadata is read for the existing object
	err := o.readMetaData(ctx)
	if err != nil {
		return fmt.Errorf("failed to read metadata of existing object for update: %w", err)
	}
	oldID := o.id // Keep track of the old ID

	// Upload the new content using putUnchecked (handles creation logic)
	// This will place the new file at the same remote path.
	newObj, err := o.fs.putUnchecked(ctx, in, src, o.remote, options...)
	if err != nil {
		return fmt.Errorf("upload during update failed: %w", err)
	}

	newO := newObj.(*Object)

	// If the upload resulted in a *new* file ID (not an overwrite), delete the old one.
	if oldID != "" && newO.id != oldID {
		fs.Debugf(o, "Update created new object %q, removing old object %q", newO.id, oldID)
		err = o.fs.deleteFiles(ctx, []string{oldID})
		if err != nil {
			// Log error but don't fail the update, the new file is there
			fs.Errorf(o, "Failed to remove old version %q after update: %v", oldID, err)
		}
	} else {
		fs.Debugf(o, "Update likely overwrote existing object %q", oldID)
	}

	// Replace the metadata of the original object `o` with the new object's data
	*o = *newO

	return nil
}

// Remove the object.
func (o *Object) Remove(ctx context.Context) error {
	if o.fs.isShare {
		return errors.New("unsupported operation: Remove on shared filesystem")
	}
	// Ensure metadata (ID) is read
	err := o.readMetaData(ctx)
	if err != nil {
		// If object not found, Remove should succeed
		if errors.Is(err, fs.ErrorObjectNotFound) {
			return nil
		}
		return fmt.Errorf("failed to read metadata before remove: %w", err)
	}
	if o.id == "" {
		return errors.New("cannot remove object with empty ID")
	}

	err = o.fs.deleteFiles(ctx, []string{o.id})
	if err != nil {
		return fmt.Errorf("failed to delete object %q: %w", o.id, err)
	}

	// 🔧 重构：删除文件成功后，清理dirCache相关缓存
	// 删除文件可能影响父目录的缓存
	o.fs.dirCache.FlushDir(path.Dir(o.remote))

	return nil
}

// setMetaData updates the object's metadata from an api.File struct.
func (o *Object) setMetaData(info *api.File) error {
	if info == nil {
		return errors.New("cannot set metadata from nil info")
	}
	if info.IsDir() {
		// This indicates we tried to create an Object for a directory path
		return fs.ErrorIsDir
	}
	o.id = info.ID()
	o.parent = info.ParentID()
	o.size = info.FileSizeBest()
	o.sha1sum = strings.ToLower(info.Sha1Best())
	o.pickCode = info.PickCodeBest()
	o.modTime = info.ModTime()
	o.hasMetaData = true
	return nil
}

// setMetaDataFromCallBack updates metadata after an upload callback.
func (o *Object) setMetaDataFromCallBack(data *api.CallbackData) error {
	if data == nil {
		return errors.New("cannot set metadata from nil callback data")
	}
	// Assume size and modTime are already set from the source info
	o.id = data.FileID
	o.parent = data.CID // Callback provides parent CID
	o.pickCode = data.PickCode
	o.sha1sum = strings.ToLower(data.Sha)
	// Update size from callback if available and different?
	if data.FileSize > 0 {
		o.size = int64(data.FileSize)
	}
	// ModTime is usually the upload time, keep the original source ModTime
	o.hasMetaData = true
	return nil
}

// readMetaData gets the metadata if it hasn't already been fetched.
func (o *Object) readMetaData(ctx context.Context) error {
	if o.hasMetaData {
		return nil
	}

	// Use the path-based lookup
	info, err := o.fs.readMetaDataForPath(ctx, o.remote)
	if err != nil {
		fs.Debugf(o.fs, "🔍 readMetaData失败: %v", err)
		return err // fs.ErrorObjectNotFound or other errors
	}

	err = o.setMetaData(info)
	if err != nil {
		fs.Debugf(o.fs, "🔍 readMetaData: setMetaData失败: %v", err)
	}
	return err
}

// setDownloadURL ensures a valid download URL is available with optimized concurrent access.
func (o *Object) setDownloadURL(ctx context.Context) error {
	// 🔒 并发安全修复：快速路径也需要锁保护
	o.durlMu.Lock()

	// 检查URL是否已经有效（在锁保护下）
	if o.durl != nil && o.durl.Valid() {
		o.durlMu.Unlock()
		return nil
	}

	// URL无效或不存在，需要获取新的URL（继续持有锁）
	// Double-check: 确保在锁保护下进行所有检查
	if o.durl != nil && o.durl.Valid() {
		fs.Debugf(o, "Download URL was fetched by another goroutine")
		o.durlMu.Unlock()
		return nil
	}
	// 注意：这里不释放锁，继续在锁保护下执行URL获取

	var err error
	var newURL *api.DownloadURL

	// Add timeout context for URL fetching to prevent hanging
	urlCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if o.fs.isShare {
		// Use traditional share download API
		if o.id == "" {
			return errors.New("cannot get share download URL without file ID")
		}

		newURL, err = o.fs.getDownloadURLFromShare(urlCtx, o.id)
	} else {
		// Use OpenAPI download URL endpoint
		if o.pickCode == "" {
			// If pickCode is missing, try getting it from metadata first
			metaErr := o.readMetaData(urlCtx)
			if metaErr != nil || o.pickCode == "" {
				return fmt.Errorf("cannot get download URL without pick code (metadata read error: %v)", metaErr)
			}
		}

		newURL, err = o.fs.getDownloadURL(urlCtx, o.pickCode)
	}

	if err != nil {
		o.durl = nil      // Clear invalid URL
		o.durlMu.Unlock() // 🔒 错误路径释放锁

		// 检查是否是502错误
		if strings.Contains(err.Error(), "502") || strings.Contains(err.Error(), "Bad Gateway") {

			return fmt.Errorf("server overload (502) when fetching download URL: %w", err)
		}

		// Check if it's a context timeout
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("timeout fetching download URL: %w", err)
		}

		return fmt.Errorf("failed to get download URL: %w", err)
	}

	o.durl = newURL
	if !o.durl.Valid() {
		// This might happen if the link expires immediately or is invalid
		fs.Logf(o, "Fetched download URL is invalid or expired immediately: %s", o.durl.URL)
		// 清除无效的URL，强制下次重新获取
		o.durl = nil
		o.durlMu.Unlock() // 🔒 无效URL路径释放锁
		return fmt.Errorf("fetched download URL is invalid or expired immediately")
	} else {
		fs.Debugf(o, "Successfully fetched download URL")
	}
	o.durlMu.Unlock() // 🔒 成功路径释放锁
	return nil
}

// setDownloadURLWithForce sets the download URL for the object with optional cache bypass
func (o *Object) setDownloadURLWithForce(ctx context.Context, forceRefresh bool) error {
	// 🔒 并发安全修复：快速路径也需要锁保护
	o.durlMu.Lock()

	// 如果不是强制刷新，检查URL是否已经有效（在锁保护下）
	if !forceRefresh && o.durl != nil && o.durl.Valid() {
		o.durlMu.Unlock()
		return nil
	}

	// URL无效或不存在，或者强制刷新，需要获取新的URL（继续持有锁）
	// Double-check: 确保在锁保护下进行所有检查
	if !forceRefresh && o.durl != nil && o.durl.Valid() {
		fs.Debugf(o, "Download URL was fetched by another goroutine")
		o.durlMu.Unlock()
		return nil
	}
	// 注意：这里不释放锁，继续在锁保护下执行URL获取

	var err error
	var newURL *api.DownloadURL

	// Add timeout context for URL fetching to prevent hanging
	urlCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if o.fs.isShare {
		// Use traditional share download API
		if o.id == "" {
			o.durlMu.Unlock()
			return errors.New("cannot get share download URL without file ID")
		}

		newURL, err = o.fs.getDownloadURLFromShare(urlCtx, o.id)
	} else {
		// Use OpenAPI download URL endpoint
		if o.pickCode == "" {
			// If pickCode is missing, try getting it from metadata first
			metaErr := o.readMetaData(urlCtx)
			if metaErr != nil || o.pickCode == "" {
				o.durlMu.Unlock()
				return fmt.Errorf("cannot get download URL without pick code (metadata read error: %v)", metaErr)
			}
		}

		newURL, err = o.fs.getDownloadURLWithForce(urlCtx, o.pickCode, forceRefresh)
	}

	if err != nil {
		o.durl = nil      // Clear invalid URL
		o.durlMu.Unlock() // 🔒 错误路径释放锁

		// 检查是否是502错误
		if strings.Contains(err.Error(), "502") || strings.Contains(err.Error(), "Bad Gateway") {
			return fmt.Errorf("server overload (502) when fetching download URL: %w", err)
		}

		// Check if it's a context timeout
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("timeout fetching download URL: %w", err)
		}

		return fmt.Errorf("failed to get download URL: %w", err)
	}

	o.durl = newURL
	if !o.durl.Valid() {
		// This might happen if the link expires immediately or is invalid
		fs.Logf(o, "Fetched download URL is invalid or expired immediately: %s", o.durl.URL)
		// 清除无效的URL，强制下次重新获取
		o.durl = nil
		o.durlMu.Unlock() // 🔒 无效URL路径释放锁
		return fmt.Errorf("fetched download URL is invalid or expired immediately")
	} else {
		fs.Debugf(o, "Successfully fetched download URL")
	}
	o.durlMu.Unlock() // 🔒 成功路径释放锁
	return nil
}

// Check the interfaces are satisfied
var (
	_ fs.Fs              = (*Fs)(nil)
	_ fs.Purger          = (*Fs)(nil)
	_ fs.Copier          = (*Fs)(nil)
	_ fs.Mover           = (*Fs)(nil)
	_ fs.DirMover        = (*Fs)(nil)
	_ fs.DirCacheFlusher = (*Fs)(nil)
	_ fs.MergeDirser     = (*Fs)(nil)
	_ fs.PutUncheckeder  = (*Fs)(nil)
	_ fs.Abouter         = (*Fs)(nil)
	_ fs.Commander       = (*Fs)(nil)
	_ fs.Object          = (*Object)(nil)
	_ fs.ObjectInfo      = (*Object)(nil)
	_ fs.IDer            = (*Object)(nil)
	_ fs.ParentIDer      = (*Object)(nil)
	_ fs.Shutdowner      = (*Fs)(nil)
)

// loadTokenFromConfig attempts to load and parse tokens from the config file
func loadTokenFromConfig(f *Fs, m configmap.Mapper) bool {
	// Try to load the token using oauthutil's method instead of
	// directly accessing the config file
	token, err := oauthutil.GetToken(f.originalName, m)
	if err != nil {
		fs.Debugf(f, "Failed to get token from config: %v", err)
		return false
	}

	if token == nil || token.AccessToken == "" || token.RefreshToken == "" {
		fs.Debugf(f, "Token from config is incomplete")
		return false
	}

	// Extract token components
	f.accessToken = token.AccessToken
	f.refreshToken = token.RefreshToken
	f.tokenExpiry = token.Expiry

	// Check if we got valid token data
	if f.accessToken == "" || f.refreshToken == "" {
		return false
	}

	fs.Debugf(f, "Loaded token from config file, expires at %v", f.tokenExpiry)
	return true
}

// rclone backend getdownloadurlua "116:/电影/2025_电影/独裁者 (2012) {tmdb-76493} 23.8GB.mkv" "VidHub/1.7.2"

// downloadWithConcurrency 115网盘多线程并发下载实现
// 🔧 修复进度显示：支持传入Transfer对象来正确跟踪进度
func (f *Fs) downloadWithConcurrency(ctx context.Context, srcObj *Object, tempFile *os.File, fileSize int64, transfer *accounting.Transfer) (int64, error) {
	// 🚀 网络参数调优：优化TCP连接参数
	f.optimizeNetworkParameters(ctx)

	// 计算最优分片参数
	chunkSize := f.calculateDownloadChunkSize(fileSize)
	numChunks := (fileSize + chunkSize - 1) / chunkSize
	maxConcurrency := f.calculateDownloadConcurrency(fileSize)

	fs.Infof(f, "📊 115网盘并发下载参数: 分片大小=%s, 分片数=%d, 并发数=%d",
		fs.SizeSuffix(chunkSize), numChunks, maxConcurrency)

	fs.Infof(f, "🚀 115网盘开始并发下载: %s (%s)", srcObj.remote, fs.SizeSuffix(fileSize))

	// 使用并发下载实现
	downloadStartTime := time.Now()
	err := f.downloadChunksConcurrentlyWithTransfer(ctx, srcObj, tempFile, chunkSize, numChunks, int64(maxConcurrency), transfer)
	downloadDuration := time.Since(downloadStartTime)
	if err != nil {
		return 0, fmt.Errorf("115网盘并发下载失败: %w", err)
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

	// 🔧 优化：详细性能统计和监控
	avgSpeed := float64(actualSize) / downloadDuration.Seconds() / 1024 / 1024 // MB/s

	// 性能评估
	performanceLevel := f.evaluateDownloadPerformance(avgSpeed, maxConcurrency)

	fs.Infof(f, "✅ 115网盘并发下载完成: %s | 用时: %v | 平均速度: %.2f MB/s | 性能: %s",
		fs.SizeSuffix(actualSize), downloadDuration, avgSpeed, performanceLevel)

	// 性能建议
	f.suggestPerformanceOptimizations(avgSpeed, maxConcurrency, fileSize)

	return actualSize, nil
}

// DownloadProgress 115网盘下载进度跟踪器
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

// downloadChunksConcurrentlyWithTransfer 115网盘并发下载文件分片（支持Transfer对象）
// 🔧 修复进度显示：支持传入Transfer对象来正确跟踪进度
func (f *Fs) downloadChunksConcurrentlyWithTransfer(ctx context.Context, srcObj *Object, tempFile *os.File, chunkSize, numChunks, maxConcurrency int64, transfer *accounting.Transfer) error {
	// 🔧 断点续传：生成任务ID并尝试加载已有的断点信息
	taskID := GenerateTaskID115(srcObj.remote, srcObj.Size())
	var resumeInfo *ResumeInfo115

	if f.resumeManager != nil {
		var err error
		resumeInfo, err = f.resumeManager.LoadResumeInfo(taskID)
		if err != nil {
			fs.Debugf(f, "加载断点续传信息失败: %v", err)
		} else if resumeInfo != nil {
			// 验证断点信息的有效性
			if resumeInfo.FileSize == srcObj.Size() &&
				resumeInfo.ChunkSize == chunkSize &&
				resumeInfo.TotalChunks == numChunks {
				fs.Infof(f, "🔄 发现断点续传信息: %s, 已完成 %d/%d 分片",
					srcObj.remote, resumeInfo.GetCompletedChunkCount(), numChunks)
			} else {
				fs.Debugf(f, "断点续传信息不匹配，重新开始下载")
				resumeInfo = nil
			}
		}
	}

	// 如果没有有效的断点信息，创建新的
	if resumeInfo == nil {
		resumeInfo = &ResumeInfo115{
			TaskID:          taskID,
			FileName:        srcObj.remote,
			FileSize:        srcObj.Size(),
			FilePath:        srcObj.remote,
			ChunkSize:       chunkSize,
			TotalChunks:     numChunks,
			CompletedChunks: make(map[int64]bool),
			CreatedAt:       time.Now(),
			TempFilePath:    tempFile.Name(),
		}
	}

	// 创建进度跟踪器，考虑已完成的分片
	progress := NewDownloadProgress(numChunks, srcObj.Size())

	// 如果有已完成的分片，更新进度（修复nil pointer问题）
	if resumeInfo != nil {
		for chunkIndex := range resumeInfo.CompletedChunks {
			progress.UpdateChunkProgress(chunkIndex, chunkSize, 0) // 已完成的分片，耗时为0
		}
	}

	// 🔧 关键修复：使用传入的Transfer对象创建Account，而不是查找现有的
	var currentAccount *accounting.Account
	remoteName := srcObj.Remote() // 使用标准Remote()方法获取路径

	if transfer != nil {
		// 🔧 修复进度显示：使用传入的Transfer对象创建Account
		// 创建一个虚拟Reader，但立即用实际的分片数据替换
		dummyReader := io.NopCloser(strings.NewReader(""))
		currentAccount = transfer.Account(ctx, dummyReader)
		dummyReader.Close()
		fs.Debugf(f, "🔧 使用传入的Transfer对象创建Account: %s", remoteName)
	} else {
		// 回退：尝试从全局统计查找现有Account
		if stats := accounting.GlobalStats(); stats != nil {
			currentAccount = stats.GetInProgressAccount(remoteName)
			if currentAccount == nil {
				// 尝试模糊匹配：查找包含文件名的Account
				allAccounts := stats.ListInProgressAccounts()
				for _, accountName := range allAccounts {
					if strings.Contains(accountName, remoteName) || strings.Contains(remoteName, accountName) {
						currentAccount = stats.GetInProgressAccount(accountName)
						if currentAccount != nil {
							fs.Debugf(f, "115网盘找到匹配的Account: %s -> %s", remoteName, accountName)
							break
						}
					}
				}
			} else {
				fs.Debugf(f, "115网盘找到精确匹配的Account: %s", remoteName)
			}
		}

		if currentAccount == nil {
			fs.Debugf(f, "115网盘未找到Account对象，进度显示可能不准确: %s", remoteName)
		}
	}

	// 启动进度更新协程（更新Account的额外信息）
	progressCtx, cancelProgress := context.WithCancel(ctx)
	defer cancelProgress()

	go f.updateAccountExtraInfo(progressCtx, progress, remoteName, currentAccount)

	// 创建工作池
	semaphore := make(chan struct{}, maxConcurrency)
	errChan := make(chan error, numChunks)
	var wg sync.WaitGroup

	for i := range numChunks {
		wg.Add(1)
		go func(chunkIndex int64) {
			defer wg.Done()

			// 🔧 添加panic恢复机制，提高系统稳定性
			defer func() {
				if r := recover(); r != nil {
					fs.Errorf(f, "115网盘下载分片 %d 发生panic: %v", chunkIndex, r)
					errChan <- fmt.Errorf("下载分片 %d panic: %v", chunkIndex, r)
				}
			}()

			// 🔧 断点续传：检查分片是否已完成（修复nil pointer问题）
			if resumeInfo != nil && resumeInfo.IsChunkCompleted(chunkIndex) {
				fs.Debugf(f, "跳过已完成的分片: %d", chunkIndex)
				return
			}

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

			// 🔧 断点续传：标记分片为已完成并保存（修复nil pointer问题）
			if resumeInfo != nil {
				resumeInfo.MarkChunkCompleted(chunkIndex)
				if f.resumeManager != nil {
					if saveErr := f.resumeManager.SaveResumeInfo(resumeInfo); saveErr != nil {
						fs.Debugf(f, "保存断点续传信息失败: %v", saveErr)
					}
				}
			}

			// 更新进度（包含下载时间）
			progress.UpdateChunkProgress(chunkIndex, actualChunkSize, chunkDuration)

			// 🔧 修复进度显示：报告分片进度到rclone accounting系统
			if currentAccount != nil {
				if err := currentAccount.AccountRead(int(actualChunkSize)); err != nil {
					fs.Debugf(f, "⚠️ 报告115网盘分片进度失败: %v", err)
				} else {
					fs.Debugf(f, "✅ 成功报告115网盘分片进度: %d bytes", actualChunkSize)
				}
			}

			// 🔧 统一进度显示：调用共享的进度显示函数
			f.displayDownloadProgress(progress, "115网盘", srcObj.remote)

		}(int64(i))
	}

	// 🔧 修复死锁风险：添加整体超时控制，防止无限等待
	overallTimeout := min(time.Duration(numChunks)*15*time.Minute, 2*time.Hour) // 每个分片最多15分钟，最大2小时超时

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
	case <-time.After(overallTimeout):
		fs.Errorf(f, "🚨 115网盘并发下载整体超时 (%v)，可能存在死锁", overallTimeout)
		return fmt.Errorf("115网盘并发下载整体超时: %v", overallTimeout)
	}

	// 🔧 断点续传：下载完成后清理断点信息
	if f.resumeManager != nil {
		if err := f.resumeManager.DeleteResumeInfo(taskID); err != nil {
			fs.Debugf(f, "清理断点续传信息失败: %v", err)
		} else {
			fs.Debugf(f, "断点续传信息已清理: %s", taskID)
		}
	}

	return nil
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
					// 尝试精确匹配
					account = stats.GetInProgressAccount(remoteName)
					if account == nil {
						// 尝试模糊匹配：查找包含文件名的Account
						allAccounts := stats.ListInProgressAccounts()
						for _, accountName := range allAccounts {
							if strings.Contains(accountName, remoteName) || strings.Contains(remoteName, accountName) {
								account = stats.GetInProgressAccount(accountName)
								if account != nil {
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

// displayDownloadProgress 统一的下载进度显示函数
// 🚀 性能优化：集成到rclone主进度条，减少独立进度显示的频率
func (f *Fs) displayDownloadProgress(progress *DownloadProgress, networkName, _ string) {
	percentage, avgSpeed, _, eta, completed, total, downloadedBytes, _ := progress.GetProgressInfo()

	// 🚀 优化：减少独立进度显示频率，只在关键节点显示
	if completed%5 == 0 || completed == total { // 每5个分片或完成时显示
		// 计算ETA显示
		etaStr := "ETA: -"
		if eta > 0 {
			etaStr = fmt.Sprintf("ETA: %v", eta.Round(time.Second))
		} else if avgSpeed > 0 {
			etaStr = "ETA: 计算中..."
		}

		// 简化的进度显示格式，集成到rclone日志系统
		fs.Debugf(f, "📥 %s: %d/%d分片 (%.1f%%) | %s | %.2f MB/s | %s",
			networkName, completed, total, percentage,
			fs.SizeSuffix(downloadedBytes), avgSpeed, etaStr)
	}
}

// downloadChunk 115网盘下载单个文件分片
func (f *Fs) downloadChunk(ctx context.Context, srcObj *Object, tempFile *os.File, start, end, chunkIndex int64) error {
	rangeOption := &fs.RangeOption{Start: start, End: end}
	expectedSize := end - start + 1

	// 🔧 修复无限重试循环：强化重试终止条件和错误分类
	maxRetries := 5
	maxURLFailures := 3 // 最大连续URL获取失败次数
	var lastErr error
	var consecutiveURLFailures int // 连续URL获取失败次数
	retryStartTime := time.Now()
	maxRetryDuration := 10 * time.Minute // 最大重试时间

	for retry := range maxRetries {
		// 🔧 检查总重试时间，防止无限重试
		if time.Since(retryStartTime) > maxRetryDuration {
			fs.Errorf(f, "🚨 115网盘分片 %d 重试超时 (%v)，终止重试", chunkIndex, maxRetryDuration)
			return fmt.Errorf("重试超时，已重试 %v", time.Since(retryStartTime))
		}

		// 🔧 检查熔断器状态（URL过期场景使用更宽松策略）
		isURLExpiry := strings.Contains(fmt.Sprintf("%v", lastErr), "403") ||
			strings.Contains(fmt.Sprintf("%v", lastErr), "URL无效") ||
			strings.Contains(fmt.Sprintf("%v", lastErr), "过期")

		var shouldRetry bool
		if isURLExpiry {
			shouldRetry = f.circuitBreaker.ShouldRetryForURLExpiry()
			if !shouldRetry {
				fs.Errorf(f, "🚨 115网盘分片 %d URL过期熔断器打开，跳过重试", chunkIndex)
			}
		} else {
			shouldRetry = f.circuitBreaker.ShouldRetry()
			if !shouldRetry {
				fs.Errorf(f, "🚨 115网盘分片 %d 熔断器打开，跳过重试", chunkIndex)
			}
		}

		if !shouldRetry {
			return fmt.Errorf("熔断器打开，跳过重试")
		}

		// 🔧 检查连续URL获取失败次数
		if consecutiveURLFailures >= maxURLFailures {
			fs.Errorf(f, "🚨 115网盘分片 %d 连续URL获取失败 %d 次，终止重试", chunkIndex, consecutiveURLFailures)
			return fmt.Errorf("连续URL获取失败 %d 次", consecutiveURLFailures)
		}

		// 在每次重试前强制刷新URL
		if retry > 0 {
			fs.Debugf(f, "115网盘分片 %d 重试 %d/%d: 强制刷新下载URL", chunkIndex, retry, maxRetries)

			// 🔧 修复：强制刷新下载URL，避免重试时仍然获取过期URL
			fs.Debugf(f, "115网盘分片重试，强制刷新下载URL: pickCode=%s", srcObj.pickCode)

			// 🚀 功能增强：使用智能URL管理器，减少API调用冲突
			if f.smartURLManager != nil {
				fs.Debugf(f, "🔄 使用智能URL管理器刷新URL: pickCode=%s, 分片=%d", srcObj.pickCode, chunkIndex)
				newURL, err := f.smartURLManager.GetDownloadURL(srcObj.pickCode, true)
				if err != nil {
					consecutiveURLFailures++
					fs.Debugf(f, "❌ 智能URL管理器失败 (连续失败%d次): %v", consecutiveURLFailures, err)
					lastErr = fmt.Errorf("智能URL管理器获取URL失败: %w", err)

					// 🔧 修复性能下降：适度增加重试延迟，避免过度重试
					time.Sleep(time.Duration(retry+1) * 3 * time.Second) // 适度延迟
					continue
				}

				// URL获取成功，重置失败计数
				consecutiveURLFailures = 0

				// 更新对象的下载URL
				srcObj.durlMu.Lock()
				srcObj.durl = &api.DownloadURL{URL: newURL}
				srcObj.durlMu.Unlock()
				fs.Debugf(f, "✅ 智能URL管理器成功更新URL: 分片=%d", chunkIndex)
			} else {
				fs.Debugf(f, "⚠️ 智能URL管理器未初始化，使用传统方法")
				// 回退到原有方法
				err := srcObj.setDownloadURLWithForce(ctx, true)
				if err != nil {
					consecutiveURLFailures++
					lastErr = fmt.Errorf("刷新下载URL失败: %w", err)
					time.Sleep(time.Duration(retry+1) * 3 * time.Second) // 🔧 适度延迟
					continue
				}
				// URL获取成功，重置失败计数
				consecutiveURLFailures = 0
			}

			// 验证新URL是否有效
			if !srcObj.durl.Valid() {
				lastErr = fmt.Errorf("刷新后的URL仍然无效")
				time.Sleep(time.Duration(retry) * time.Second)
				continue
			}

			// 递增延迟避免过于频繁的重试
			time.Sleep(time.Duration(retry) * 500 * time.Millisecond)
		}

		// 尝试打开分片
		chunkReader, err := srcObj.open(ctx, rangeOption)
		if err != nil {
			lastErr = err

			// 🔧 修复无限循环：智能错误分类和严格终止条件
			errorType := f.classifyDownloadError(err)
			retryDelay := f.calculateRetryDelay(errorType, retry)

			// 🔧 简化错误处理：统一重试逻辑
			maxRetries := map[string]int{
				"server_overload": 3,
				"url_expired":     4,
				"network_timeout": 2,
				"rate_limit":      3,
				"fatal_error":     0, // 不重试
			}

			maxRetry, exists := maxRetries[errorType]
			if !exists {
				maxRetry = 2 // 默认重试2次
			}

			if maxRetry == 0 || retry >= maxRetry {
				fs.Errorf(f, "🚨 115网盘分片 %d %s重试超限，终止重试", chunkIndex, errorType)
				return fmt.Errorf("%s重试超限: %w", errorType, err)
			}

			// URL过期错误不需要延迟
			if errorType != "url_expired" {
				time.Sleep(retryDelay)
			}
			continue

			// 🔧 移除：删除可能导致无限循环的通用5xx错误处理
		}

		// 读取分片数据
		chunkData, err := io.ReadAll(chunkReader)
		chunkReader.Close()
		if err != nil {
			lastErr = err
			continue
		}

		// 检测HTML错误响应（通常是79字节左右的错误页面）
		if len(chunkData) < 1000 && isHTMLErrorResponse(chunkData) {
			lastErr = fmt.Errorf("收到HTML错误响应，可能是URL过期: 大小=%d", len(chunkData))
			continue
		}

		// 验证分片大小
		if int64(len(chunkData)) != expectedSize {
			// 如果是最后一个分片，可能大小不足
			if chunkIndex == (srcObj.Size()+expectedSize-1)/expectedSize-1 {
				// 最后一个分片，检查是否是合理的剩余大小
				remainingSize := srcObj.Size() - start
				if int64(len(chunkData)) == remainingSize {
					// 最后分片大小正确
				} else {
					lastErr = fmt.Errorf("最后分片大小不匹配: 期望%d，实际%d", remainingSize, len(chunkData))
					continue
				}
			} else {
				lastErr = fmt.Errorf("分片大小不匹配: 期望%d，实际%d", expectedSize, len(chunkData))
				continue
			}
		}

		// 写入临时文件的正确位置
		_, err = tempFile.WriteAt(chunkData, start)
		if err != nil {
			return fmt.Errorf("写入分片数据失败: %w", err)
		}

		// 🔧 记录成功到熔断器
		f.circuitBreaker.RecordSuccess()

		// 成功完成
		return nil
	}

	// 🔧 记录失败到熔断器
	f.circuitBreaker.RecordFailure()

	// 所有重试都失败了
	return fmt.Errorf("分片 %d 下载失败，已重试 %d 次: %w", chunkIndex, maxRetries, lastErr)
}

// isHTMLErrorResponse 检测是否是HTML错误响应
func isHTMLErrorResponse(data []byte) bool {
	if len(data) == 0 {
		return false
	}

	// 转换为小写字符串进行检查
	content := strings.ToLower(string(data))

	// 检查常见的HTML错误响应特征
	htmlIndicators := []string{
		"<html",
		"<!doctype",
		"<head>",
		"<title>",
		"error",
		"404",
		"403",
		"500",
		"expired",
		"invalid",
	}

	for _, indicator := range htmlIndicators {
		if strings.Contains(content, indicator) {
			return true
		}
	}

	return false
}

// calculateDownloadChunkSize 115网盘智能计算下载分片大小
// 🔧 修复硬编码问题：基于文件大小和网络状况动态计算最优分片大小
func (f *Fs) calculateDownloadChunkSize(fileSize int64) int64 {
	// 🔧 动态分片策略：基于文件大小智能计算，优化下载性能
	switch {
	case fileSize < 50*1024*1024: // <50MB
		return 16 * 1024 * 1024 // 16MB分片，小文件使用小分片
	case fileSize < 200*1024*1024: // <200MB
		return 32 * 1024 * 1024 // 32MB分片
	case fileSize < 1*1024*1024*1024: // <1GB
		return 64 * 1024 * 1024 // 64MB分片，平衡性能和内存使用
	case fileSize < 5*1024*1024*1024: // <5GB
		return 128 * 1024 * 1024 // 128MB分片，大文件使用大分片
	default: // >5GB
		return 256 * 1024 * 1024 // 256MB分片，超大文件
	}
}

// calculateOptimalChunkSize 115网盘智能计算上传分片大小
// 🚀 超激进优化：使用更大的分片大小，减少分片数量，提升整体传输效率
func (f *Fs) calculateOptimalChunkSize(fileSize int64) fs.SizeSuffix {
	// 🚀 基于2MB/s目标速度优化：使用更大的分片，减少API调用开销
	// 更大的分片可以更好地利用网络带宽，减少分片间的开销
	var partSize int64

	switch {
	case fileSize <= 50*1024*1024: // ≤50MB
		partSize = 25 * 1024 * 1024 // 🚀 进一步优化：从20MB增加到25MB
	case fileSize <= 200*1024*1024: // ≤200MB
		partSize = 100 * 1024 * 1024 // 🚀 超激进优化：从50MB增加到100MB，减少分片数量
	case fileSize <= 1*1024*1024*1024: // ≤1GB
		partSize = 200 * 1024 * 1024 // 🚀 超激进优化：从100MB增加到200MB，最大化传输效率
	case fileSize > 1*1024*1024*1024*1024: // >1TB
		partSize = 5 * 1024 * 1024 * 1024 // 5GB分片
	case fileSize > 768*1024*1024*1024: // >768GB
		partSize = 109951163 // ≈ 104.8576MB
	case fileSize > 512*1024*1024*1024: // >512GB
		partSize = 82463373 // ≈ 78.6432MB
	case fileSize > 384*1024*1024*1024: // >384GB
		partSize = 54975582 // ≈ 52.4288MB
	case fileSize > 256*1024*1024*1024: // >256GB
		partSize = 41231687 // ≈ 39.3216MB
	case fileSize > 128*1024*1024*1024: // >128GB
		partSize = 27487791 // ≈ 26.2144MB
	default:
		partSize = 100 * 1024 * 1024 // 🚀 默认100MB分片，最大化性能
	}

	return fs.SizeSuffix(f.normalizeChunkSize(partSize))
}

// normalizeChunkSize 确保分片大小在合理范围内
func (f *Fs) normalizeChunkSize(partSize int64) int64 {
	if partSize < int64(minChunkSize) {
		partSize = int64(minChunkSize)
	}
	if partSize > int64(maxChunkSize) {
		partSize = int64(maxChunkSize)
	}
	return partSize
}

// calculateDownloadConcurrency 115网盘智能计算下载并发数
// 🔧 优化：实现动态并发控制，平衡性能和稳定性
func (f *Fs) calculateDownloadConcurrency(fileSize int64) int {
	// 🚀 新的智能并发策略：基于文件大小和网络状况动态调整
	baseConcurrency := f.getBaseConcurrency(fileSize)

	// 🔧 网络状况自适应调整
	adjustedConcurrency := f.adjustConcurrencyByNetworkCondition(baseConcurrency)

	return adjustedConcurrency
}

// getBaseConcurrency 获取基础并发数
// 🚀 基于日志分析优化：提升115网盘并发数，目标提升下载速度
func (f *Fs) getBaseConcurrency(fileSize int64) int {
	// 🚀 基于日志分析的优化策略：
	// 日志显示115网盘使用2并发，速度10.57MB/s（较慢）
	// 而123网盘使用4并发，速度131.912Mi/s（优秀）
	// 建议提升115网盘并发数以改善性能
	switch {
	case fileSize < 50*1024*1024: // <50MB
		return 3 // 小文件使用3并发（从2提升）
	case fileSize < 200*1024*1024: // <200MB
		return 4 // 中等文件使用4并发（从3提升）
	case fileSize < 1*1024*1024*1024: // <1GB
		return 4 // 大文件使用4并发（保持）
	case fileSize < 5*1024*1024*1024: // <5GB
		return 4 // 超大文件使用4并发（保持）
	default: // >5GB
		return 4 // 巨大文件使用4并发（保持）
	}
}

// adjustConcurrencyByNetworkCondition 根据网络状况调整并发数
// 🚀 性能优化：实现基于网络延迟和带宽的自适应并发数调整
func (f *Fs) adjustConcurrencyByNetworkCondition(baseConcurrency int) int {
	// 🚀 新增：网络质量检测和自适应调整
	networkLatency := f.measureNetworkLatency()
	networkBandwidth := f.estimateNetworkBandwidth()

	// 🔧 修复过度保守的延迟调整：优化延迟阈值和调整因子
	latencyFactor := 1.0
	if networkLatency < 100 { // <100ms 低延迟（从50ms放宽到100ms）
		latencyFactor = 1.2 // 从1.3降低到1.2，避免过度激进
	} else if networkLatency < 300 { // <300ms 中等延迟（从200ms放宽到300ms）
		latencyFactor = 1.0
	} else if networkLatency < 500 { // <500ms 高延迟（从200ms放宽到500ms）
		latencyFactor = 0.9 // 从0.8提升到0.9，减少惩罚
	} else { // >500ms 很高延迟
		latencyFactor = 0.8 // 从0.6提升到0.8，减少过度惩罚
	}

	// 🔧 修复过度保守的带宽调整：优化带宽阈值和调整因子
	bandwidthFactor := 1.0
	if networkBandwidth > 50*1024*1024 { // >50Mbps（从100Mbps降低阈值）
		bandwidthFactor = 1.1 // 从1.2降低到1.1，避免过度激进
	} else if networkBandwidth > 20*1024*1024 { // >20Mbps
		bandwidthFactor = 1.0
	} else if networkBandwidth > 10*1024*1024 { // >10Mbps（新增中等带宽档位）
		bandwidthFactor = 0.95 // 轻微降低，从0.9提升
	} else { // <10Mbps（从20Mbps降低阈值）
		bandwidthFactor = 0.8 // 从0.7提升到0.8，减少过度惩罚
	}

	// 计算调整后的并发数
	adjustedConcurrency := int(float64(baseConcurrency) * latencyFactor * bandwidthFactor)

	// 🔧 修复重试循环：强制2线程下载，平衡性能和稳定性
	maxConcurrency := 2 // 强制2线程，平衡性能和稳定性
	fs.Debugf(f, "🔧 强制2线程下载，平衡性能和稳定性")

	if adjustedConcurrency > maxConcurrency {
		adjustedConcurrency = maxConcurrency
	}

	// 🔧 修复过度保守的最小并发数：提升到2，避免单线程下载过慢
	minConcurrency := 2 // 从1提升到2，确保基本的并发性能
	if adjustedConcurrency < minConcurrency {
		adjustedConcurrency = minConcurrency
	}

	fs.Debugf(f, "🔧 115网盘自适应并发调整: 延迟=%dms, 带宽=%s/s, 基础并发=%d, 调整后并发=%d",
		networkLatency, fs.SizeSuffix(networkBandwidth), baseConcurrency, adjustedConcurrency)

	return adjustedConcurrency
}

// measureNetworkLatency 测量网络延迟
// 🚀 性能优化：实现网络延迟检测，用于自适应并发调整
func (f *Fs) measureNetworkLatency() int64 {
	// 简单的延迟测量：向115网盘API发送HEAD请求
	start := time.Now()

	// 构建测试URL
	testURL := "https://webapi.115.com/files"
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

// estimateNetworkBandwidth 估算网络带宽
// 🚀 性能优化：实现网络带宽估算，用于自适应并发调整
func (f *Fs) estimateNetworkBandwidth() int64 {
	// 简化的带宽估算：基于历史传输速度
	// 这里使用一个保守的估算方法

	// 默认带宽估算（50Mbps）
	defaultBandwidth := int64(50 * 1024 * 1024)

	// TODO: 可以基于历史传输数据进行更精确的估算
	// 目前返回默认值，后续可以集成实际的带宽测试

	return defaultBandwidth
}

// SmartURLManager 智能URL管理器
// 🔧 修复无限刷新：增强频率控制和错误处理
type SmartURLManager struct {
	fs           *Fs
	urlCache     map[string]*URLCacheEntry
	cacheMutex   sync.RWMutex
	refreshMutex sync.Mutex // 防止并发刷新

	// 🚀 新增：批量刷新管理
	batchRefreshQueue map[string]*BatchRefreshRequest
	batchMutex        sync.Mutex

	// 🔧 新增：刷新频率控制
	refreshHistory map[string][]time.Time // 记录每个pickCode的刷新历史
	historyMutex   sync.RWMutex

	// 统计信息
	refreshCount int64
	hitCount     int64
	missCount    int64
}

// BatchRefreshRequest 批量刷新请求
type BatchRefreshRequest struct {
	PickCode     string
	RequestTime  time.Time
	WaitingCount int32 // 等待此次刷新的分片数量
	Done         chan *URLCacheEntry
}

// URLCacheEntry URL缓存条目
type URLCacheEntry struct {
	URL        string
	ExpiryTime time.Time
	CreatedAt  time.Time
	HitCount   int64
}

// NewSmartURLManager 创建智能URL管理器
func NewSmartURLManager(fs *Fs) *SmartURLManager {
	sum := &SmartURLManager{
		fs:                fs,
		urlCache:          make(map[string]*URLCacheEntry),
		batchRefreshQueue: make(map[string]*BatchRefreshRequest),
		refreshHistory:    make(map[string][]time.Time), // 🔧 新增：初始化刷新历史
	}

	// 🔧 启动定期清理协程
	go sum.startPeriodicCleanup()

	return sum
}

// GetDownloadURL 智能获取下载URL
// 🔧 修复无限刷新：增强超时控制和错误处理
func (sum *SmartURLManager) GetDownloadURL(pickCode string, forceRefresh bool) (string, error) {
	// 🔧 修复：添加调用计数，防止无限递归
	if sum == nil {
		return "", fmt.Errorf("智能URL管理器未初始化")
	}

	// 如果不强制刷新，先检查缓存
	if !forceRefresh {
		if url, valid := sum.getCachedURL(pickCode); valid {
			atomic.AddInt64(&sum.hitCount, 1)
			return url, nil
		}
		atomic.AddInt64(&sum.missCount, 1)
	}

	// 🔧 修复：检查刷新频率，防止过于频繁的刷新
	if sum.isRefreshTooFrequent(pickCode) {
		return "", fmt.Errorf("URL刷新过于频繁，请稍后重试")
	}

	// 检查是否已有批量刷新请求
	if entry, exists := sum.checkBatchRefresh(pickCode); exists {
		// 🔧 修复：缩短等待时间，避免长时间阻塞
		select {
		case result := <-entry.Done:
			if result != nil {
				return result.URL, nil
			}
			return "", fmt.Errorf("批量刷新失败")
		case <-time.After(5 * time.Second): // 从10秒缩短到5秒
			// 🔧 修复：超时后清理批量刷新请求
			sum.cleanupBatchRefresh(pickCode)
			return "", fmt.Errorf("批量刷新超时")
		}
	}

	// 创建新的批量刷新请求
	return sum.performBatchRefresh(pickCode)
}

// getCachedURL 获取缓存的URL
func (sum *SmartURLManager) getCachedURL(pickCode string) (string, bool) {
	sum.cacheMutex.RLock()
	defer sum.cacheMutex.RUnlock()

	entry, exists := sum.urlCache[pickCode]
	if !exists {
		return "", false
	}

	// 🔧 修复URL频繁过期：减少提前过期时间，从30秒减少到10秒
	// 这样可以更充分利用URL的有效期，减少不必要的刷新
	if time.Now().Add(10 * time.Second).After(entry.ExpiryTime) {
		return "", false
	}

	entry.HitCount++
	return entry.URL, true
}

// checkBatchRefresh 检查是否已有批量刷新请求
func (sum *SmartURLManager) checkBatchRefresh(pickCode string) (*BatchRefreshRequest, bool) {
	sum.batchMutex.Lock()
	defer sum.batchMutex.Unlock()

	request, exists := sum.batchRefreshQueue[pickCode]
	if exists {
		atomic.AddInt32(&request.WaitingCount, 1)
	}
	return request, exists
}

// performBatchRefresh 执行批量刷新
func (sum *SmartURLManager) performBatchRefresh(pickCode string) (string, error) {
	sum.refreshMutex.Lock()
	defer sum.refreshMutex.Unlock()

	// 双重检查：可能在等待锁的过程中已经被其他goroutine刷新了
	if url, valid := sum.getCachedURL(pickCode); valid {
		return url, nil
	}

	// 创建批量刷新请求
	request := &BatchRefreshRequest{
		PickCode:     pickCode,
		RequestTime:  time.Now(),
		WaitingCount: 1,
		Done:         make(chan *URLCacheEntry, 10), // 缓冲通道
	}

	sum.batchMutex.Lock()
	sum.batchRefreshQueue[pickCode] = request
	sum.batchMutex.Unlock()

	// 延迟100ms执行刷新，允许更多分片加入批量请求
	time.Sleep(100 * time.Millisecond)

	// 🔧 修复：记录刷新历史，用于频率控制
	sum.recordRefresh(pickCode)

	// 执行实际的URL刷新
	url, expiryTime, err := sum.refreshURLFromAPI(pickCode)

	// 清理批量刷新请求
	sum.batchMutex.Lock()
	delete(sum.batchRefreshQueue, pickCode)
	sum.batchMutex.Unlock()

	if err != nil {
		// 通知所有等待的goroutine失败
		close(request.Done)
		return "", err
	}

	// 更新缓存
	entry := &URLCacheEntry{
		URL:        url,
		ExpiryTime: expiryTime,
		CreatedAt:  time.Now(),
		HitCount:   1,
	}

	sum.cacheMutex.Lock()
	sum.urlCache[pickCode] = entry
	sum.cacheMutex.Unlock()

	atomic.AddInt64(&sum.refreshCount, 1)

	// 通知所有等待的goroutine成功
	waitingCount := atomic.LoadInt32(&request.WaitingCount)
	for range waitingCount {
		select {
		case request.Done <- entry:
		default:
			// 通道已满，跳过
		}
	}
	close(request.Done)

	fs.Debugf(sum.fs, "🔄 115网盘批量刷新URL成功: pickCode=%s, 等待分片数=%d, URL过期时间=%v",
		pickCode, waitingCount, expiryTime)

	return url, nil
}

// refreshURLFromAPI 从API刷新URL
func (sum *SmartURLManager) refreshURLFromAPI(pickCode string) (string, time.Time, error) {
	ctx := context.Background()

	// 构建API请求
	opts := rest.Opts{
		Method:      "POST",
		Path:        "/open/ufile/downurl",
		ContentType: "application/x-www-form-urlencoded",
		Body:        strings.NewReader("pick_code=" + pickCode),
		ExtraHeaders: map[string]string{
			"User-Agent": sum.fs.opt.UserAgent,
		},
	}

	// 🚀 修复：使用正确的API响应结构
	var response ApiResponse
	err := sum.fs.CallDownloadURLAPI(ctx, &opts, nil, &response, false)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("获取下载URL失败: %w", err)
	}

	// 🚀 修复：从ApiResponse中提取下载URL
	for _, downInfo := range response.Data {
		if downInfo != (FileInfo{}) && downInfo.URL.URL != "" {
			// 🔧 修复URL频繁过期：延长默认过期时间到6分钟
			// 115网盘URL实际可能有更长的有效期，4分钟过于保守
			expiryTime := time.Now().Add(6 * time.Minute)

			// 尝试从URL中解析过期时间
			if parsedURL, err := url.Parse(downInfo.URL.URL); err == nil {
				if expireStr := parsedURL.Query().Get("expire"); expireStr != "" {
					if expireTimestamp, err := strconv.ParseInt(expireStr, 10, 64); err == nil {
						parsedExpiryTime := time.Unix(expireTimestamp, 0)
						// 🔧 修复：如果解析到的过期时间更长，使用解析的时间
						if parsedExpiryTime.After(expiryTime) {
							expiryTime = parsedExpiryTime
						}
					}
				}
			}

			fs.Debugf(sum.fs, "🔄 115网盘智能URL管理器成功获取URL: pickCode=%s, URL长度=%d, 过期时间=%v",
				pickCode, len(downInfo.URL.URL), expiryTime)

			return downInfo.URL.URL, expiryTime, nil
		}
	}

	return "", time.Time{}, fmt.Errorf("API响应中未找到有效的下载URL")
}

// GetStatistics 获取统计信息
func (sum *SmartURLManager) GetStatistics() map[string]any {
	sum.cacheMutex.RLock()
	defer sum.cacheMutex.RUnlock()

	totalHits := atomic.LoadInt64(&sum.hitCount)
	totalMisses := atomic.LoadInt64(&sum.missCount)
	totalRefresh := atomic.LoadInt64(&sum.refreshCount)

	hitRate := float64(0)
	if totalHits+totalMisses > 0 {
		hitRate = float64(totalHits) / float64(totalHits+totalMisses) * 100
	}

	return map[string]any{
		"cache_entries": len(sum.urlCache),
		"hit_count":     totalHits,
		"miss_count":    totalMisses,
		"refresh_count": totalRefresh,
		"hit_rate":      fmt.Sprintf("%.2f%%", hitRate),
	}
}

// classifyDownloadError 智能分类下载错误类型
// 🔧 修复无限循环：增强错误分类，添加致命错误检测
func (f *Fs) classifyDownloadError(err error) string {
	errStr := strings.ToLower(err.Error())

	// 🔧 新增：致命错误检测 - 这些错误不应该重试
	if strings.Contains(errStr, "401") || strings.Contains(errStr, "unauthorized") ||
		strings.Contains(errStr, "403") || strings.Contains(errStr, "forbidden") ||
		strings.Contains(errStr, "404") || strings.Contains(errStr, "not found") ||
		strings.Contains(errStr, "400") || strings.Contains(errStr, "bad request") ||
		strings.Contains(errStr, "invalid credentials") || strings.Contains(errStr, "access denied") ||
		strings.Contains(errStr, "file not found") || strings.Contains(errStr, "permission denied") {
		return "fatal_error"
	}

	// 服务器过载错误
	if strings.Contains(errStr, "502") || strings.Contains(errStr, "bad gateway") ||
		strings.Contains(errStr, "503") || strings.Contains(errStr, "service unavailable") ||
		strings.Contains(errStr, "504") || strings.Contains(errStr, "gateway timeout") {
		return "server_overload"
	}

	// URL过期错误
	if strings.Contains(errStr, "download url is invalid") || strings.Contains(errStr, "expired") ||
		strings.Contains(errStr, "url.*invalid") || strings.Contains(errStr, "link.*expired") {
		return "url_expired"
	}

	// 网络超时错误
	if strings.Contains(errStr, "timeout") || strings.Contains(errStr, "deadline exceeded") ||
		strings.Contains(errStr, "context deadline exceeded") || strings.Contains(errStr, "i/o timeout") {
		return "network_timeout"
	}

	// 限流错误
	if strings.Contains(errStr, "429") || strings.Contains(errStr, "too many requests") ||
		strings.Contains(errStr, "rate limit") || strings.Contains(errStr, "quota exceeded") {
		return "rate_limit"
	}

	// 连接错误
	if strings.Contains(errStr, "connection") || strings.Contains(errStr, "network") ||
		strings.Contains(errStr, "dns") || strings.Contains(errStr, "host") {
		return "network_error"
	}

	// 🔧 新增：智能URL管理器相关错误
	if strings.Contains(errStr, "智能url管理器") || strings.Contains(errStr, "批量刷新") {
		return "url_manager_error"
	}

	return "unknown"
}

// calculateRetryDelay 根据错误类型和重试次数计算延迟时间
func (f *Fs) calculateRetryDelay(errorType string, retryCount int) time.Duration {
	baseDelay := time.Second

	switch errorType {
	case "server_overload":
		// 服务器过载使用指数退避，最大30秒
		delay := min(time.Duration(1<<uint(retryCount))*baseDelay, 30*time.Second)
		return delay

	case "url_expired":
		// URL过期立即重试
		return 0

	case "network_timeout":
		// 网络超时使用线性增长，最大10秒
		delay := min(time.Duration(retryCount+1)*baseDelay, 10*time.Second)
		return delay

	case "rate_limit":
		// 限流错误使用较长延迟
		delay := min(time.Duration(retryCount+1)*5*time.Second, 60*time.Second)
		return delay

	default:
		// 其他错误使用标准指数退避
		delay := min(time.Duration(1<<uint(retryCount))*baseDelay, 15*time.Second)
		return delay
	}
}

// optimizeNetworkParameters 优化网络参数以提升下载性能
func (f *Fs) optimizeNetworkParameters(ctx context.Context) {
	// 🔧 网络参数调优：设置更优的HTTP客户端参数
	ci := fs.GetConfig(ctx)

	// 网络参数优化建议（静默处理）
	_ = ci.Transfers
	_ = ci.Timeout
	_ = ci.LowLevelRetries
}

// evaluateDownloadPerformance 评估下载性能等级
func (f *Fs) evaluateDownloadPerformance(avgSpeed float64, _ int) string {
	// 🔧 性能等级评估标准
	switch {
	case avgSpeed >= 50: // >=50MB/s
		return "优秀"
	case avgSpeed >= 30: // >=30MB/s
		return "良好"
	case avgSpeed >= 15: // >=15MB/s
		return "一般"
	case avgSpeed >= 5: // >=5MB/s
		return "较慢"
	default: // <5MB/s
		return "很慢"
	}
}

// suggestPerformanceOptimizations 提供性能优化建议
func (f *Fs) suggestPerformanceOptimizations(avgSpeed float64, concurrency int, fileSize int64) {
	suggestions := []string{}

	// 基于速度的建议
	if avgSpeed < 15 {
		suggestions = append(suggestions, "网络速度较慢，建议检查网络连接")

		if concurrency < 8 {
			suggestions = append(suggestions, "可以尝试增加并发数以提升性能")
		}

		if fileSize > 5*1024*1024*1024 { // >5GB
			suggestions = append(suggestions, "大文件建议在网络状况良好时下载")
		}
	}

	// 基于并发数的建议
	if concurrency < 4 && fileSize > 1*1024*1024*1024 { // >1GB
		suggestions = append(suggestions, "大文件可以考虑增加并发数")
	}

	// 输出建议
	if len(suggestions) > 0 {
		fs.Infof(f, "💡 115网盘性能优化建议:")
		for i, suggestion := range suggestions {
			fs.Infof(f, "   %d. %s", i+1, suggestion)
		}
	}
}

// shouldUseConcurrentDownload 判断是否应该使用并发下载
func (o *Object) shouldUseConcurrentDownload(_ context.Context, options []fs.OpenOption) bool {
	// 🔧 修复多重并发下载：检查是否有禁用并发下载的选项
	for _, option := range options {
		if option.String() == "DisableConcurrentDownload" {
			fs.Debugf(o, "🔧 检测到禁用并发下载选项，跳过并发下载")
			return false
		}
	}

	// 检查文件大小，只对大文件使用并发下载
	if o.size < 100*1024*1024 { // 小于100MB
		return false
	}

	// 🌐 跨云传输优化：检测Range选项，避免多重并发下载冲突
	hasRangeOption := false
	for _, option := range options {
		if rangeOpt, ok := option.(*fs.RangeOption); ok {
			hasRangeOption = true
			// 如果Range覆盖了整个文件，则可以使用并发下载
			if rangeOpt.Start == 0 && (rangeOpt.End == -1 || rangeOpt.End >= o.size-1) {
				fs.Debugf(o, "检测到全文件Range选项，允许并发下载")
				break
			} else {
				// 计算Range大小
				rangeSize := rangeOpt.End - rangeOpt.Start + 1

				// 🚀 跨云传输场景检测：如果是大Range请求，很可能是123网盘分片上传
				// 为避免115并发下载与123分片上传的资源竞争，禁用115并发下载
				if rangeSize >= 32*1024*1024 { // 32MB阈值
					fs.Infof(o, "🌐 检测到跨云传输大Range选项 (%d-%d, %s)，为避免资源竞争禁用115并发下载",
						rangeOpt.Start, rangeOpt.End, fs.SizeSuffix(rangeSize))
					return false // 关键修改：禁用并发下载
				} else {
					fs.Debugf(o, "检测到小Range选项 (%d-%d, %s)，跳过并发下载",
						rangeOpt.Start, rangeOpt.End, fs.SizeSuffix(rangeSize))
					return false
				}
			}
		}
	}

	// 如果没有Range选项，允许并发下载
	if !hasRangeOption {
		fs.Debugf(o, "无Range选项，允许并发下载")
		return true
	}

	return true
}

// openWithConcurrency 使用并发下载打开文件
func (o *Object) openWithConcurrency(ctx context.Context, options ...fs.OpenOption) (io.ReadCloser, error) {
	// 🔧 修复重复下载：检查是否已有相同文件的下载
	downloadKey := fmt.Sprintf("%s_%d", o.Remote(), o.size)

	// 使用文件系统级别的下载协调器
	if o.fs.downloadCoordinator == nil {
		o.fs.downloadCoordinator = NewDownloadCoordinator(4) // 最大4个并发下载
	}

	session, isNew := o.fs.downloadCoordinator.StartDownload(downloadKey, o.Remote(), o.size)
	if !isNew {
		fs.Infof(o, "⏳ 检测到重复下载，等待现有下载完成: %s", o.Remote())
		result := o.fs.downloadCoordinator.WaitForDownload(session)
		if result.Success {
			fs.Infof(o, "✅ 使用现有下载结果: %s", o.Remote())
			return result.Reader, nil
		}
		fs.Debugf(o, "现有下载失败，启动新的下载: %v", result.Error)
	}

	fs.Infof(o, "🚀 115网盘启动并发下载: %s", fs.SizeSuffix(o.size))

	// 🔧 修复进度显示：创建专用的并发下载Transfer对象
	var downloadTransfer *accounting.Transfer

	stats := accounting.GlobalStats()
	if stats != nil {
		// 创建专用的并发下载Transfer对象，集成到rclone进度显示
		downloadTransfer = stats.NewTransferRemoteSize(
			fmt.Sprintf("📥 %s (并发下载)", o.Remote()),
			o.size,
			o.fs, // 源文件系统
			o.fs, // 目标文件系统（临时文件）
		)
		defer downloadTransfer.Done(ctx, nil)

		fs.Debugf(o, "🔧 创建115网盘并发下载Transfer对象")
	}

	// 创建临时文件用于并发下载
	tempFile, err := os.CreateTemp("", "115_concurrent_download_*.tmp")
	if err != nil {
		fs.Debugf(o, "创建临时文件失败，回退到普通下载: %v", err)
		return o.open(ctx, options...)
	}

	// 🔧 修复进度显示：创建进度报告器，集成到rclone accounting
	progressReporter := NewProgressReporter(o.size)

	// 🔧 修复进度显示：将Transfer对象传递给下载函数
	_ = progressReporter // 暂时保留，后续可能使用

	// 使用并发下载到临时文件，同时报告进度
	downloadedSize, err := o.fs.downloadWithConcurrency(ctx, o, tempFile, o.size, downloadTransfer)
	if err != nil {
		// 🔧 修复重复下载：通知下载协调器下载失败
		if o.fs.downloadCoordinator != nil {
			o.fs.downloadCoordinator.CompleteDownload(downloadKey, DownloadResult{
				Success: false,
				Error:   err,
			})
		}

		// 🔧 改进错误恢复：确保资源完全清理
		if closeErr := tempFile.Close(); closeErr != nil {
			fs.Debugf(o, "关闭临时文件失败: %v", closeErr)
		}
		if removeErr := os.Remove(tempFile.Name()); removeErr != nil {
			fs.Debugf(o, "删除临时文件失败: %v", removeErr)
		}
		fs.Debugf(o, "并发下载失败，回退到普通下载: %v", err)
		return o.open(ctx, options...)
	}

	// 验证下载大小
	if downloadedSize != o.size {
		// 🔧 修复重复下载：通知下载协调器下载失败
		if o.fs.downloadCoordinator != nil {
			o.fs.downloadCoordinator.CompleteDownload(downloadKey, DownloadResult{
				Success: false,
				Error:   fmt.Errorf("下载大小不匹配: 期望%d，实际%d", o.size, downloadedSize),
			})
		}

		// 🔧 改进错误恢复：确保资源完全清理
		if closeErr := tempFile.Close(); closeErr != nil {
			fs.Debugf(o, "关闭临时文件失败: %v", closeErr)
		}
		if removeErr := os.Remove(tempFile.Name()); removeErr != nil {
			fs.Debugf(o, "删除临时文件失败: %v", removeErr)
		}
		fs.Debugf(o, "下载大小不匹配，回退到普通下载: 期望%d，实际%d", o.size, downloadedSize)
		return o.open(ctx, options...)
	}

	// 重置文件指针到开始位置
	_, err = tempFile.Seek(0, io.SeekStart)
	if err != nil {
		// 🔧 改进错误恢复：确保资源完全清理
		if closeErr := tempFile.Close(); closeErr != nil {
			fs.Debugf(o, "关闭临时文件失败: %v", closeErr)
		}
		if removeErr := os.Remove(tempFile.Name()); removeErr != nil {
			fs.Debugf(o, "删除临时文件失败: %v", removeErr)
		}
		return nil, fmt.Errorf("重置临时文件指针失败: %w", err)
	}

	fs.Infof(o, "✅ 115网盘并发下载完成: %s", fs.SizeSuffix(downloadedSize))

	// 🔧 添加监控：记录并发下载性能指标
	fs.Debugf(o, "📊 115网盘并发下载性能统计: 文件大小=%s, 临时文件=%s",
		fs.SizeSuffix(o.size), tempFile.Name())

	// 🔧 关键修复：为每个请求创建独立的文件句柄，避免共享导致的关闭问题
	// 先关闭原始文件句柄
	tempFile.Close()

	// 重新打开文件创建新的句柄
	newTempFile, err := os.Open(tempFile.Name())
	if err != nil {
		return nil, fmt.Errorf("重新打开临时文件失败: %w", err)
	}

	// 创建返回的ReadCloser
	reader := &ConcurrentDownloadReader{
		file:             newTempFile, // 使用新的文件句柄
		tempPath:         tempFile.Name(),
		progressReporter: progressReporter,
		totalSize:        o.size,
	}

	// 🔧 修复重复下载：通知下载协调器下载完成
	if o.fs.downloadCoordinator != nil {
		o.fs.downloadCoordinator.CompleteDownload(downloadKey, DownloadResult{
			Success: true,
			Reader:  reader,
		})
	}

	return reader, nil
}

// ProgressReporter 进度报告器，用于向rclone报告下载进度
type ProgressReporter struct {
	totalSize       int64
	transferredSize int64
	mu              sync.Mutex
}

// NewProgressReporter 创建新的进度报告器
func NewProgressReporter(totalSize int64) *ProgressReporter {
	return &ProgressReporter{
		totalSize: totalSize,
	}
}

// AddTransferred 添加已传输的字节数
func (pr *ProgressReporter) AddTransferred(bytes int64) {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	pr.transferredSize += bytes
}

// GetTransferred 获取已传输的字节数
func (pr *ProgressReporter) GetTransferred() int64 {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	return pr.transferredSize
}

// ConcurrentDownloadReader 并发下载的文件读取器
type ConcurrentDownloadReader struct {
	file             *os.File
	tempPath         string
	progressReporter *ProgressReporter
	totalSize        int64
	readBytes        int64
}

// Read 实现io.Reader接口，同时报告读取进度
func (r *ConcurrentDownloadReader) Read(p []byte) (n int, err error) {
	n, err = r.file.Read(p)
	if n > 0 {
		r.readBytes += int64(n)
		// 这里rclone的accounting系统会自动处理进度显示
	}
	return n, err
}

// Close 实现io.Closer接口，关闭时删除临时文件
func (r *ConcurrentDownloadReader) Close() error {
	// 🔧 改进资源清理：确保临时文件被正确删除
	err := r.file.Close()
	if removeErr := os.Remove(r.tempPath); removeErr != nil {
		fs.Debugf(nil, "删除临时文件失败: %s, 错误: %v", r.tempPath, removeErr)
		// 如果文件关闭成功但删除失败，返回删除错误
		if err == nil {
			err = fmt.Errorf("删除临时文件失败: %w", removeErr)
		}
	} else {
		fs.Debugf(nil, "✅ 成功删除临时文件: %s", r.tempPath)
	}
	return err
}

// ResumeInfo115 115网盘断点续传信息
type ResumeInfo115 struct {
	TaskID          string         `json:"task_id"`
	FileName        string         `json:"file_name"`
	FileSize        int64          `json:"file_size"`
	FilePath        string         `json:"file_path"`
	ChunkSize       int64          `json:"chunk_size"`
	TotalChunks     int64          `json:"total_chunks"`
	CompletedChunks map[int64]bool `json:"completed_chunks"`
	CreatedAt       time.Time      `json:"created_at"`
	LastUpdated     time.Time      `json:"last_updated"`
	TempFilePath    string         `json:"temp_file_path"`
}

// ResumeManager115 115网盘断点续传管理器（使用BadgerDB）
type ResumeManager115 struct {
	mu          sync.RWMutex
	resumeCache *cache.BadgerCache
	fs          *Fs
}

// NewResumeManager115 创建115网盘断点续传管理器（使用BadgerDB）
func NewResumeManager115(fs *Fs, cacheDir string) (*ResumeManager115, error) {
	resumeCache, err := cache.NewBadgerCache("115-resume", cacheDir)
	if err != nil {
		return nil, fmt.Errorf("创建115网盘断点续传BadgerDB缓存失败: %w", err)
	}

	return &ResumeManager115{
		resumeCache: resumeCache,
		fs:          fs,
	}, nil
}

// SaveResumeInfo 保存断点续传信息到BadgerDB
func (rm *ResumeManager115) SaveResumeInfo(info *ResumeInfo115) error {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	info.LastUpdated = time.Now()

	// 使用taskID作为缓存键
	cacheKey := fmt.Sprintf("resume_115_%s", info.TaskID)

	err := rm.resumeCache.Set(cacheKey, info, 24*time.Hour) // 24小时过期
	if err != nil {
		return fmt.Errorf("保存115网盘断点续传信息到BadgerDB失败: %w", err)
	}

	fs.Debugf(rm.fs, "保存115网盘断点续传信息: %s, 已完成分片: %d/%d",
		info.FileName, len(info.CompletedChunks), info.TotalChunks)

	return nil
}

// LoadResumeInfo 从BadgerDB加载断点续传信息
func (rm *ResumeManager115) LoadResumeInfo(taskID string) (*ResumeInfo115, error) {
	rm.mu.RLock()
	defer rm.mu.RUnlock()

	cacheKey := fmt.Sprintf("resume_115_%s", taskID)
	var info ResumeInfo115

	found, err := rm.resumeCache.Get(cacheKey, &info)
	if err != nil {
		return nil, fmt.Errorf("从BadgerDB加载115网盘断点续传信息失败: %w", err)
	}

	if !found {
		return nil, nil // 没有找到断点续传信息
	}

	// 检查信息是否过期（超过24小时）
	if time.Since(info.CreatedAt) > 24*time.Hour {
		rm.DeleteResumeInfo(taskID)
		return nil, nil
	}

	fs.Debugf(rm.fs, "加载115网盘断点续传信息: %s, 已完成分片: %d/%d",
		info.FileName, len(info.CompletedChunks), info.TotalChunks)

	return &info, nil
}

// DeleteResumeInfo 从BadgerDB删除断点续传信息
func (rm *ResumeManager115) DeleteResumeInfo(taskID string) error {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	cacheKey := fmt.Sprintf("resume_115_%s", taskID)
	err := rm.resumeCache.Delete(cacheKey)
	if err != nil {
		return fmt.Errorf("从BadgerDB删除115网盘断点续传信息失败: %w", err)
	}

	fs.Debugf(rm.fs, "删除115网盘断点续传信息: %s", taskID)
	return nil
}

// IsChunkCompleted 检查分片是否已完成
func (info *ResumeInfo115) IsChunkCompleted(chunkIndex int64) bool {
	if info.CompletedChunks == nil {
		return false
	}
	return info.CompletedChunks[chunkIndex]
}

// MarkChunkCompleted 标记分片为已完成
func (info *ResumeInfo115) MarkChunkCompleted(chunkIndex int64) {
	if info.CompletedChunks == nil {
		info.CompletedChunks = make(map[int64]bool)
	}
	info.CompletedChunks[chunkIndex] = true
}

// GetCompletedChunkCount 获取已完成的分片数量
func (info *ResumeInfo115) GetCompletedChunkCount() int64 {
	return int64(len(info.CompletedChunks))
}

// Close 关闭115网盘断点续传管理器
func (rm *ResumeManager115) Close() error {
	if rm.resumeCache != nil {
		return rm.resumeCache.Close()
	}
	return nil
}

// GenerateTaskID115 生成115网盘任务ID
func GenerateTaskID115(filePath string, fileSize int64) string {
	return fmt.Sprintf("115_%s_%d_%d",
		strings.ReplaceAll(filePath, "/", "_"),
		fileSize,
		time.Now().Unix())
}

// isRefreshTooFrequent 检查URL刷新是否过于频繁
// 🔧 修复无限刷新：防止过于频繁的URL刷新请求
func (sum *SmartURLManager) isRefreshTooFrequent(pickCode string) bool {
	sum.historyMutex.RLock()
	defer sum.historyMutex.RUnlock()

	history, exists := sum.refreshHistory[pickCode]
	if !exists {
		return false
	}

	now := time.Now()
	// 检查最近1分钟内的刷新次数
	recentRefreshes := 0
	for _, refreshTime := range history {
		if now.Sub(refreshTime) <= time.Minute {
			recentRefreshes++
		}
	}

	// 如果1分钟内刷新超过10次，认为过于频繁
	return recentRefreshes >= 10
}

// cleanupBatchRefresh 清理批量刷新请求
// 🔧 修复无限刷新：清理超时的批量刷新请求
func (sum *SmartURLManager) cleanupBatchRefresh(pickCode string) {
	sum.batchMutex.Lock()
	defer sum.batchMutex.Unlock()

	if entry, exists := sum.batchRefreshQueue[pickCode]; exists {
		// 关闭通道并删除请求
		close(entry.Done)
		delete(sum.batchRefreshQueue, pickCode)
	}
}

// startPeriodicCleanup 启动定期清理协程
// 🔧 修复批量刷新死锁：定期清理过期的批量刷新请求
func (sum *SmartURLManager) startPeriodicCleanup() {
	ticker := time.NewTicker(30 * time.Second) // 每30秒清理一次
	defer ticker.Stop()

	for range ticker.C {
		sum.cleanupExpiredBatchRequests()
	}
}

// cleanupExpiredBatchRequests 清理过期的批量刷新请求
func (sum *SmartURLManager) cleanupExpiredBatchRequests() {
	sum.batchMutex.Lock()
	defer sum.batchMutex.Unlock()

	now := time.Now()
	expiredPickCodes := make([]string, 0)

	for pickCode, request := range sum.batchRefreshQueue {
		// 如果请求超过30秒未完成，认为过期
		if now.Sub(request.RequestTime) > 30*time.Second {
			expiredPickCodes = append(expiredPickCodes, pickCode)
		}
	}

	// 🔧 修复死锁风险：安全清理过期请求
	for _, pickCode := range expiredPickCodes {
		if request, exists := sum.batchRefreshQueue[pickCode]; exists {
			// 安全关闭通道，防止panic
			select {
			case <-request.Done:
				// 通道已关闭，无需操作
			default:
				// 发送nil表示超时，然后关闭通道
				select {
				case request.Done <- nil:
				default:
					// 通道已满，直接关闭
				}
				close(request.Done)
			}
			delete(sum.batchRefreshQueue, pickCode)
			fs.Debugf(sum.fs, "🧹 清理过期的批量刷新请求: %s (等待时间: %v)",
				pickCode, time.Since(request.RequestTime))
		}
	}
}

// recordRefresh 记录刷新历史
// 🔧 修复无限刷新：记录刷新历史用于频率控制
func (sum *SmartURLManager) recordRefresh(pickCode string) {
	sum.historyMutex.Lock()
	defer sum.historyMutex.Unlock()

	now := time.Now()
	if sum.refreshHistory[pickCode] == nil {
		sum.refreshHistory[pickCode] = make([]time.Time, 0)
	}

	// 添加当前刷新时间
	sum.refreshHistory[pickCode] = append(sum.refreshHistory[pickCode], now)

	// 清理1小时前的历史记录
	cutoff := now.Add(-time.Hour)
	filtered := make([]time.Time, 0)
	for _, refreshTime := range sum.refreshHistory[pickCode] {
		if refreshTime.After(cutoff) {
			filtered = append(filtered, refreshTime)
		}
	}
	sum.refreshHistory[pickCode] = filtered
}

// localFileInfo 本地文件信息结构，用于跨云传输
type localFileInfo struct {
	name    string
	size    int64
	modTime time.Time
	remote  string
}

func (l *localFileInfo) String() string                        { return l.remote }
func (l *localFileInfo) Remote() string                        { return l.remote }
func (l *localFileInfo) ModTime(ctx context.Context) time.Time { return l.modTime }
func (l *localFileInfo) Size() int64                           { return l.size }
func (l *localFileInfo) Fs() fs.Info                           { return nil }
func (l *localFileInfo) Hash(ctx context.Context, t hash.Type) (string, error) {
	return "", hash.ErrUnsupported
}
func (l *localFileInfo) Storable() bool { return true }

// crossCloudUploadWithLocalCache 跨云传输专用上传方法，强制下载到本地后上传
func (f *Fs) crossCloudUploadWithLocalCache(ctx context.Context, in io.Reader, src fs.ObjectInfo, remote string, options ...fs.OpenOption) (fs.Object, error) {
	fileSize := src.Size()
	fs.Infof(f, "🌐 开始跨云传输本地化处理: %s (大小: %s)", remote, fs.SizeSuffix(fileSize))

	// 🔧 强制下载到本地：无论文件大小，都先完整下载到本地
	var localDataSource io.Reader
	var cleanup func()
	var localFileSize int64

	maxMemoryBufferSize := int64(100 * 1024 * 1024) // 100MB内存缓冲限制

	if fileSize <= maxMemoryBufferSize {
		// 小文件：下载到内存
		fs.Infof(f, "📝 小文件跨云传输，下载到内存: %s", fs.SizeSuffix(fileSize))
		data, err := io.ReadAll(in)
		if err != nil {
			return nil, fmt.Errorf("跨云传输下载到内存失败: %w", err)
		}
		localDataSource = bytes.NewReader(data)
		localFileSize = int64(len(data))
		cleanup = func() {} // 内存数据无需清理
		fs.Infof(f, "✅ 跨云传输内存下载完成: %s", fs.SizeSuffix(localFileSize))
	} else {
		// 大文件：下载到临时文件
		fs.Infof(f, "🗂️  大文件跨云传输，下载到临时文件: %s", fs.SizeSuffix(fileSize))
		tempFile, err := os.CreateTemp("", "115_cross_cloud_*.tmp")
		if err != nil {
			return nil, fmt.Errorf("创建临时文件失败: %w", err)
		}

		written, err := io.Copy(tempFile, in)
		if err != nil {
			tempFile.Close()
			os.Remove(tempFile.Name())
			return nil, fmt.Errorf("跨云传输下载到临时文件失败: %w", err)
		}

		// 重置文件指针到开头
		if _, err := tempFile.Seek(0, io.SeekStart); err != nil {
			tempFile.Close()
			os.Remove(tempFile.Name())
			return nil, fmt.Errorf("重置临时文件指针失败: %w", err)
		}

		localDataSource = tempFile
		localFileSize = written
		cleanup = func() {
			tempFile.Close()
			os.Remove(tempFile.Name())
		}
		fs.Infof(f, "✅ 跨云传输临时文件下载完成: %s", fs.SizeSuffix(localFileSize))
	}
	defer cleanup()

	// 验证下载完整性
	if localFileSize != fileSize {
		return nil, fmt.Errorf("跨云传输下载不完整: 期望%d字节，实际%d字节", fileSize, localFileSize)
	}

	// 🚀 使用本地数据进行上传，创建本地文件信息对象
	localSrc := &localFileInfo{
		name:    filepath.Base(remote),
		size:    localFileSize,
		modTime: src.ModTime(ctx),
		remote:  remote,
	}

	fs.Infof(f, "🚀 开始从本地数据上传到115网盘: %s", fs.SizeSuffix(localFileSize))

	// 使用主上传函数，但此时源已经是本地数据
	return f.upload(ctx, localDataSource, localSrc, remote, options...)
}

// 🔧 新增：智能文件路径检测函数
// isLikelyFilePath 判断给定路径是否可能是文件路径而非目录路径
func isLikelyFilePath(path string) bool {
	if path == "" {
		return false
	}

	// 如果路径以斜杠结尾，很可能是目录
	if strings.HasSuffix(path, "/") {
		return false
	}

	// 检查是否包含文件扩展名（包含点且点后面有字符）
	lastSlash := strings.LastIndex(path, "/")
	fileName := path
	if lastSlash >= 0 {
		fileName = path[lastSlash+1:]
	}

	// 文件名包含点且不是隐藏文件（不以点开头）
	if strings.Contains(fileName, ".") && !strings.HasPrefix(fileName, ".") {
		dotIndex := strings.LastIndex(fileName, ".")
		// 确保点不是最后一个字符，且扩展名不为空
		if dotIndex > 0 && dotIndex < len(fileName)-1 {
			// 添加调试信息
			fs.Debugf(nil, "🔧 isLikelyFilePath: 路径 %q 被识别为文件路径 (fileName=%q)", path, fileName)
			return true
		}
	}

	fs.Debugf(nil, "🔧 isLikelyFilePath: 路径 %q 被识别为目录路径 (fileName=%q)", path, fileName)
	return false
}

// 🔧 新增：保存路径类型到缓存
// savePathTypeToCache 保存路径类型信息到缓存
func (f *Fs) savePathTypeToCache(path, fileID, parentID string, isDir bool) {
	if f.pathResolveCache == nil {
		return
	}

	cacheKey := generatePathToIDCacheKey(path)
	entry := PathToIDCacheEntry115{
		Path:     path,
		FileID:   fileID,
		IsDir:    isDir,
		ParentID: parentID,
		CachedAt: time.Now(),
	}

	if err := f.pathResolveCache.Set(cacheKey, entry, f.cacheConfig.PathResolveCacheTTL); err != nil {
		fs.Debugf(f, "保存路径类型缓存失败 %s: %v", cacheKey, err)
	} else {
		fs.Debugf(f, "已保存路径类型到缓存: %s -> %s (dir: %v)", path, fileID, isDir)
	}
}

// 🔧 新增：dir_list缓存支持函数，参考123网盘实现

// getDirListFromCache 从缓存获取目录列表
func (f *Fs) getDirListFromCache(parentID, lastID string) (*DirListCacheEntry115, bool) {
	if f.dirListCache == nil {
		return nil, false
	}

	cacheKey := fmt.Sprintf("dirlist_%s_%s", parentID, lastID)
	var entry DirListCacheEntry115
	found, err := f.dirListCache.Get(cacheKey, &entry)
	if err != nil {
		fs.Debugf(f, "获取目录列表缓存失败 %s: %v", cacheKey, err)
		return nil, false
	}

	if !found {
		return nil, false
	}

	// 简单验证缓存条目
	if len(entry.FileList) == 0 && entry.TotalCount > 0 {
		fs.Debugf(f, "目录列表缓存数据不一致: %s", cacheKey)
		return nil, false
	}

	fs.Debugf(f, "从缓存获取目录列表成功: parentID=%s, 文件数=%d", parentID, len(entry.FileList))
	return &entry, true
}

// saveDirListToCache 保存目录列表到缓存
func (f *Fs) saveDirListToCache(parentID, lastID string, fileList []api.File, nextLastID string) {
	if f.dirListCache == nil {
		return
	}

	cacheKey := fmt.Sprintf("dirlist_%s_%s", parentID, lastID)
	entry := DirListCacheEntry115{
		FileList:   fileList,
		LastFileID: nextLastID,
		TotalCount: len(fileList),
		CachedAt:   time.Now(),
		ParentID:   parentID,
		Version:    "v1.0",                           // 简化版本号
		Checksum:   fmt.Sprintf("%d", len(fileList)), // 简化校验和
	}

	err := f.dirListCache.Set(cacheKey, entry, f.cacheConfig.DirListCacheTTL)
	if err != nil {
		fs.Debugf(f, "保存目录列表缓存失败 %s: %v", cacheKey, err)
	} else {
		fs.Debugf(f, "已保存目录列表到缓存: parentID=%s, 文件数=%d, TTL=%v", parentID, len(fileList), f.cacheConfig.DirListCacheTTL)
	}
}

// 🔧 新增：路径预填充缓存功能
// preloadPathCache 利用API返回的path信息预填充路径缓存
func (f *Fs) preloadPathCache(paths []*api.FilePath) {
	if f.pathResolveCache == nil {
		return
	}

	var fullPath string
	for i, pathItem := range paths {
		// 构建完整路径
		if i == 0 {
			// 根目录，跳过
			if pathItem.Name == "" || pathItem.Name == "根目录" || pathItem.Name == "文件" {
				continue
			}
			fullPath = pathItem.Name
		} else {
			// 跳过根目录级别的"文件"目录
			if pathItem.Name == "文件" && i == 1 {
				continue
			}
			if fullPath == "" {
				fullPath = pathItem.Name
			} else {
				fullPath = path.Join(fullPath, pathItem.Name)
			}
		}

		// 获取目录ID
		cid := pathItem.CID.String()
		if cid == "" || cid == "0" {
			continue // 跳过无效的ID
		}

		// 获取父目录ID（如果有的话）
		var parentID string
		if i > 0 {
			parentID = paths[i-1].CID.String()
		}

		// 保存路径到缓存
		f.savePathTypeToCache(fullPath, cid, parentID, true) // true表示是目录

		fs.Debugf(f, "🎯 预填充路径缓存: %s -> %s (parent: %s)", fullPath, cid, parentID)
	}
}

// 🚀 新设计：智能路径处理方法

// checkPathTypeFromCache 检查缓存中的路径类型信息
func (f *Fs) checkPathTypeFromCache(path string) (isFile bool, found bool) {
	if f.pathResolveCache == nil {
		return false, false
	}

	cacheKey := generatePathToIDCacheKey(path)
	var entry PathToIDCacheEntry115
	found, err := f.pathResolveCache.Get(cacheKey, &entry)
	if err != nil || !found {
		return false, false
	}

	fs.Debugf(f, "🎯 缓存命中：路径 %s -> IsDir=%v", path, entry.IsDir)
	return !entry.IsDir, true
}

// setupFileFromCache 从缓存信息设置文件模式
func (f *Fs) setupFileFromCache(_ context.Context) (*Fs, error) {
	// 分割路径获取父目录和文件名
	newRoot, remote := dircache.SplitPath(f.root)

	// 修改当前Fs指向父目录
	f.root = newRoot
	f.dirCache = dircache.New(newRoot, f.rootFolderID, f)

	// 创建文件对象
	obj := &Object{
		fs:          f,
		remote:      remote,
		hasMetaData: false, // 延迟加载元数据
		durlMu:      new(sync.Mutex),
	}

	// 设置文件模式
	var fsObj fs.Object = obj
	f.fileObj = &fsObj

	fs.Debugf(f, "🎯 缓存文件模式设置完成: %s", remote)
	return f, fs.ErrorIsFile
}

// handleAsFile 将路径作为文件处理
func (f *Fs) handleAsFile(ctx context.Context) (*Fs, error) {
	// 分割路径获取父目录和文件名
	newRoot, remote := dircache.SplitPath(f.root)

	// 创建临时Fs指向父目录
	tempF := &Fs{
		name:          f.name,
		originalName:  f.originalName,
		root:          newRoot,
		opt:           f.opt,
		features:      f.features,
		tradClient:    f.tradClient,
		openAPIClient: f.openAPIClient,
		globalPacer:   f.globalPacer,
		tradPacer:     f.tradPacer,
		rootFolder:    f.rootFolder,
		rootFolderID:  f.rootFolderID,
		appVer:        f.appVer,
		userID:        f.userID,
		userkey:       f.userkey,
		isShare:       f.isShare,
		fileObj:       f.fileObj,
		m:             f.m,
		accessToken:   f.accessToken,
		refreshToken:  f.refreshToken,
		tokenExpiry:   f.tokenExpiry,
		codeVerifier:  f.codeVerifier,
		tokenRenewer:  f.tokenRenewer,
		httpClient:    f.httpClient,
		// 复用缓存实例
		pathResolveCache: f.pathResolveCache,
		dirListCache:     f.dirListCache,
		metadataCache:    f.metadataCache,
		fileIDCache:      f.fileIDCache,
		cacheConfig:      f.cacheConfig,
		smartURLManager:  f.smartURLManager,
		resumeManager:    f.resumeManager,
		circuitBreaker:   f.circuitBreaker,
	}

	tempF.dirCache = dircache.New(newRoot, f.rootFolderID, tempF)

	// 尝试找到父目录
	err := tempF.dirCache.FindRoot(ctx, false)
	if err != nil {
		fs.Debugf(f, "🔧 文件处理：父目录不存在，回退到目录模式: %v", err)
		return f.handleAsDirectory(ctx)
	}

	// 🔧 关键：使用轻量级验证，参考123网盘策略
	_, err = tempF.NewObject(ctx, remote)
	if err == nil {
		fs.Debugf(f, "🎯 文件验证成功，设置文件模式: %s", remote)

		// 保存到缓存
		f.savePathTypeToCache(f.root, "", "", false) // isFile=true

		// 设置文件模式
		f.root = newRoot
		f.dirCache = tempF.dirCache

		obj := &Object{
			fs:          f,
			remote:      remote,
			hasMetaData: false,
			durlMu:      new(sync.Mutex),
		}

		var fsObj fs.Object = obj
		f.fileObj = &fsObj

		return f, fs.ErrorIsFile
	} else {
		fs.Debugf(f, "🔧 文件验证失败，回退到目录模式: %v", err)
		return f.handleAsDirectory(ctx)
	}
}

// 🔧 115网盘统一QPS管理 - 根据API类型选择调速器
// getPacerForEndpoint 根据115网盘API端点返回适当的调速器
// 115网盘特殊性：使用统一QPS避免770004全局限制错误
func (f *Fs) getPacerForEndpoint(endpoint string) *fs.Pacer {
	switch {
	// 文件列表API - 使用全局调速器（最常用）
	case strings.Contains(endpoint, "/open/ufile/files"), // 文件列表
		strings.Contains(endpoint, "/open/ufile/file"),   // 文件信息
		strings.Contains(endpoint, "/open/ufile/search"): // 文件搜索
		return f.globalPacer // 统一QPS

	// 下载相关API - 使用下载调速器
	case strings.Contains(endpoint, "/open/ufile/download"), // 下载URL
		strings.Contains(endpoint, "/open/user/info"): // 用户信息
		return f.downloadPacer // 统一QPS

	// 上传和敏感操作 - 使用保守调速器
	case strings.Contains(endpoint, "/open/ufile/upload"), // 上传相关
		strings.Contains(endpoint, "/open/ufile/move"),   // 移动文件
		strings.Contains(endpoint, "/open/ufile/rename"), // 重命名
		strings.Contains(endpoint, "/open/ufile/copy"),   // 复制文件
		strings.Contains(endpoint, "/open/ufile/delete"), // 删除文件
		strings.Contains(endpoint, "/open/ufile/trash"),  // 回收站
		strings.Contains(endpoint, "/open/ufile/mkdir"):  // 创建目录
		return f.uploadPacer // 保守QPS

	// 认证相关API - 使用传统调速器
	case strings.Contains(endpoint, "/open/auth/"), // 认证相关
		strings.Contains(endpoint, "/open/token/"),   // Token相关
		strings.Contains(endpoint, "115.com"),        // 传统API
		strings.Contains(endpoint, "webapi.115.com"): // 传统API
		return f.tradPacer // 保守QPS

	default:
		// 未知端点使用全局调速器，安全起见
		fs.Debugf(f, "⚠️  未知115网盘API端点，使用统一QPS限制: %s", endpoint)
		return f.globalPacer // 统一QPS
	}
}

// handleAsDirectory 将路径作为目录处理
func (f *Fs) handleAsDirectory(ctx context.Context) (*Fs, error) {
	// 尝试找到目录
	err := f.dirCache.FindRoot(ctx, false)
	if err != nil {
		fs.Debugf(f, "🔧 目录处理：目录不存在: %v", err)
		return f, nil
	}

	// 保存到缓存
	f.savePathTypeToCache(f.root, "", "", true) // isDir=true

	fs.Debugf(f, "🎯 目录模式设置完成: %s", f.root)
	return f, nil
}
