// Package common provides unified components for cloud storage backends
package common

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/accounting"
)

// ConcurrentDownloader 统一的并发下载器接口
// 🚀 设计目标：消除123网盘和115网盘85%的重复代码
type ConcurrentDownloader interface {
	// 下载文件到临时文件
	DownloadToFile(ctx context.Context, obj fs.Object, tempFile *os.File, options ...fs.OpenOption) (int64, error)

	// 检查是否应该使用并发下载
	ShouldUseConcurrentDownload(ctx context.Context, obj fs.Object, options []fs.OpenOption) bool

	// 计算最优分片参数
	CalculateChunkParams(fileSize int64) ChunkParams

	// 获取下载URL（后端特定实现）
	GetDownloadURL(ctx context.Context, obj fs.Object, start, end int64) (string, error)

	// 下载单个分片（后端特定实现）
	DownloadChunk(ctx context.Context, url string, tempFile *os.File, start, end int64, account *accounting.Account) error
}

// ChunkParams 分片参数
type ChunkParams struct {
	ChunkSize      int64 // 分片大小
	NumChunks      int64 // 分片数量
	MaxConcurrency int64 // 最大并发数
}

// DownloadProgress 下载进度跟踪器
type DownloadProgress struct {
	mu              sync.RWMutex
	totalChunks     int64
	completedChunks int64
	totalBytes      int64
	downloadedBytes int64
	startTime       time.Time
	chunkProgress   map[int64]ChunkProgress
}

// ChunkProgress 分片进度
type ChunkProgress struct {
	Index       int64
	Size        int64
	Downloaded  int64
	StartTime   time.Time
	CompletedAt time.Time
	Speed       int64 // bytes/second
}

// UnifiedConcurrentDownloader 统一的并发下载器实现
type UnifiedConcurrentDownloader struct {
	fs            fs.Fs
	backendType   string // "123" 或 "115"
	adapter       DownloadAdapter
	resumeManager UnifiedResumeManager

	// 配置参数
	minFileSize      int64 // 启用并发下载的最小文件大小
	maxConcurrency   int64 // 最大并发数
	defaultChunkSize int64 // 默认分片大小
}

// DownloadAdapter 后端特定的下载适配器接口
type DownloadAdapter interface {
	// 获取下载URL
	GetDownloadURL(ctx context.Context, obj fs.Object, start, end int64) (string, error)

	// 下载单个分片
	DownloadChunk(ctx context.Context, url string, tempFile *os.File, start, end int64, account *accounting.Account) error

	// 验证下载完整性
	VerifyDownload(ctx context.Context, obj fs.Object, tempFile *os.File) error

	// 获取后端特定的配置
	GetConfig() DownloadConfig
}

// DownloadConfig 下载配置
type DownloadConfig struct {
	MinFileSize      int64         // 启用并发下载的最小文件大小
	MaxConcurrency   int64         // 最大并发数
	DefaultChunkSize int64         // 默认分片大小
	TimeoutPerChunk  time.Duration // 每个分片的超时时间
}

// NewUnifiedConcurrentDownloader 创建统一的并发下载器
func NewUnifiedConcurrentDownloader(filesystem fs.Fs, backendType string, adapter DownloadAdapter, resumeManager UnifiedResumeManager) *UnifiedConcurrentDownloader {
	config := adapter.GetConfig()

	return &UnifiedConcurrentDownloader{
		fs:               filesystem,
		backendType:      backendType,
		adapter:          adapter,
		resumeManager:    resumeManager,
		minFileSize:      config.MinFileSize,
		maxConcurrency:   config.MaxConcurrency,
		defaultChunkSize: config.DefaultChunkSize,
	}
}

// ShouldUseConcurrentDownload 判断是否应该使用并发下载
func (d *UnifiedConcurrentDownloader) ShouldUseConcurrentDownload(ctx context.Context, obj fs.Object, options []fs.OpenOption) bool {
	// 🔍 调试：添加详细的判断日志
	fs.Debugf(d.fs, "🔍 统一并发下载器判断开始: 后端=%s, 文件=%s, 大小=%s",
		d.backendType, obj.Remote(), fs.SizeSuffix(obj.Size()))

	// 检查是否有禁用并发下载的选项
	for _, option := range options {
		if option.String() == "DisableConcurrentDownload" {
			fs.Debugf(d.fs, "🔧 检测到禁用并发下载选项，跳过并发下载")
			return false
		}
	}

	// 检查文件大小
	fs.Debugf(d.fs, "🔍 文件大小检查: %s >= %s ?", fs.SizeSuffix(obj.Size()), fs.SizeSuffix(d.minFileSize))
	if obj.Size() < d.minFileSize {
		fs.Debugf(d.fs, "🔍 文件太小，跳过并发下载")
		return false
	}

	// 检查Range选项，避免多重并发下载冲突
	for _, option := range options {
		if rangeOpt, ok := option.(*fs.RangeOption); ok {
			// 如果Range覆盖了整个文件，则可以使用并发下载
			if rangeOpt.Start == 0 && (rangeOpt.End == -1 || rangeOpt.End >= obj.Size()-1) {
				fs.Debugf(d.fs, "检测到全文件Range选项，允许并发下载")
				break
			} else {
				// 计算Range大小
				rangeSize := rangeOpt.End - rangeOpt.Start + 1

				// 🚀 优化：123网盘特殊处理 - 与rclone多线程复制协同工作
				if d.backendType == "123" {
					// 🎯 关键优化：检测rclone多线程复制场景
					// rclone多线程复制通常使用64MB分片，我们应该配合而不是冲突
					if rangeSize >= 32*1024*1024 && rangeSize <= 128*1024*1024 {
						// 这很可能是rclone多线程复制的分片，不需要额外的并发下载
						fs.Debugf(d.fs, "🎯 123网盘检测到rclone多线程复制分片 (%d-%d, %s)，使用单线程下载配合rclone并发",
							rangeOpt.Start, rangeOpt.End, fs.SizeSuffix(rangeSize))
						return false
					} else if rangeSize >= 200*1024*1024 {
						// 超大Range，可能是跨云传输，禁用并发避免资源竞争
						fs.Infof(d.fs, "🌐 123网盘检测到超大Range选项 (%d-%d, %s)，为避免资源竞争禁用并发下载",
							rangeOpt.Start, rangeOpt.End, fs.SizeSuffix(rangeSize))
						return false
					} else {
						// 小Range或其他情况，跳过并发下载
						fs.Debugf(d.fs, "123网盘检测到Range选项 (%d-%d, %s)，跳过并发下载",
							rangeOpt.Start, rangeOpt.End, fs.SizeSuffix(rangeSize))
						return false
					}
				} else {
					// 🔧 修复115网盘Range选项处理：Range选项不应该禁用并发下载
					// Range选项用于rclone多线程复制的分片下载，与我们的并发下载是不同层面的优化
					// 115网盘应该支持Range请求的并发下载，除非文件太小
					if rangeSize < 10*1024*1024 { // 小于10MB的Range，跳过并发下载
						fs.Debugf(d.fs, "115网盘检测到小Range选项 (%d-%d, %s)，跳过并发下载",
							rangeOpt.Start, rangeOpt.End, fs.SizeSuffix(rangeSize))
						return false
					} else {
						// 大Range请求，允许并发下载以提高效率
						fs.Debugf(d.fs, "115网盘检测到大Range选项 (%d-%d, %s)，允许并发下载",
							rangeOpt.Start, rangeOpt.End, fs.SizeSuffix(rangeSize))
						// 继续检查其他条件，不直接返回false
					}
				}
			}
		}
	}

	return true
}

// CalculateChunkParams 计算最优分片参数
func (d *UnifiedConcurrentDownloader) CalculateChunkParams(fileSize int64) ChunkParams {
	// 动态计算分片大小
	chunkSize := d.calculateOptimalChunkSize(fileSize)

	// 计算分片数量
	numChunks := (fileSize + chunkSize - 1) / chunkSize

	// 计算最优并发数
	maxConcurrency := d.calculateOptimalConcurrency(fileSize)
	maxConcurrency = min(numChunks, maxConcurrency)

	return ChunkParams{
		ChunkSize:      chunkSize,
		NumChunks:      numChunks,
		MaxConcurrency: maxConcurrency,
	}
}

// DownloadToFile 下载文件到临时文件
func (d *UnifiedConcurrentDownloader) DownloadToFile(ctx context.Context, obj fs.Object, tempFile *os.File, options ...fs.OpenOption) (int64, error) {
	fileSize := obj.Size()

	// 计算分片参数
	params := d.CalculateChunkParams(fileSize)

	fs.Infof(d.fs, "📊 %s网盘并发下载参数: 分片大小=%s, 分片数=%d, 并发数=%d",
		d.backendType, fs.SizeSuffix(params.ChunkSize), params.NumChunks, params.MaxConcurrency)

	fs.Infof(d.fs, "🚀 %s网盘开始并发下载: %s (%s)",
		d.backendType, obj.Remote(), fs.SizeSuffix(fileSize))

	// 生成任务ID用于断点续传
	taskID := d.generateTaskID(obj)

	// 尝试加载断点续传信息
	resumeInfo, err := d.resumeManager.LoadResumeInfo(taskID)
	if err != nil {
		fs.Debugf(d.fs, "📊 加载断点续传信息失败: %v (TaskID: %s)", err, taskID)
	} else if resumeInfo != nil {
		fs.Infof(d.fs, "📊 发现断点续传信息: 已完成 %d/%d 分片 (TaskID: %s)",
			resumeInfo.GetCompletedChunkCount(), resumeInfo.GetTotalChunks(), taskID)
	} else {
		fs.Debugf(d.fs, "📊 未找到断点续传信息，将创建新的 (TaskID: %s)", taskID)
	}

	// 🔧 优化1：断点续传信息预验证
	if resumeInfo != nil {
		if !d.validateResumeInfo(resumeInfo, tempFile, params, fileSize) {
			fs.Debugf(d.fs, "🔧 %s网盘断点续传信息验证失败，清理并重新开始", d.backendType)
			d.resumeManager.DeleteResumeInfo(taskID)
			resumeInfo = nil
		} else {
			fs.Debugf(d.fs, "✅ %s网盘断点续传信息验证通过", d.backendType)
		}
	}

	// 如果没有有效的断点信息，创建新的
	if resumeInfo == nil {
		resumeInfo = d.createResumeInfo(taskID, obj, params, tempFile)
	}

	// 创建进度跟踪器
	progress := NewDownloadProgress(params.NumChunks, fileSize)

	// 🔧 修复进度初始化：如果有已完成的分片，使用正确的分片大小更新进度
	if resumeInfo != nil {
		for chunkIndex := range resumeInfo.GetCompletedChunks() {
			// 🔧 关键修复：计算实际的分片大小，而不是使用固定的params.ChunkSize
			chunkStart := chunkIndex * params.ChunkSize
			chunkEnd := chunkStart + params.ChunkSize - 1
			if chunkEnd >= fileSize {
				chunkEnd = fileSize - 1
			}
			actualChunkSize := chunkEnd - chunkStart + 1
			progress.UpdateChunkProgress(chunkIndex, actualChunkSize, 0)
		}
	}

	// 执行并发下载
	startTime := time.Now()
	err = d.downloadChunksConcurrently(ctx, obj, tempFile, params, resumeInfo, progress)
	downloadDuration := time.Since(startTime)

	if err != nil {
		return 0, fmt.Errorf("%s网盘并发下载失败: %w", d.backendType, err)
	}

	// 🔧 修复：增强下载完整性验证
	stat, err := tempFile.Stat()
	if err != nil {
		return 0, fmt.Errorf("获取下载文件信息失败: %w", err)
	}

	actualSize := stat.Size()
	if actualSize != fileSize {
		// 🔧 文件大小不匹配时，清理断点续传信息并提供详细错误信息
		if d.resumeManager != nil {
			d.resumeManager.DeleteResumeInfo(taskID)
			fs.Debugf(d.fs, "文件大小不匹配，已清理断点续传信息: %s", taskID)
		}

		// 提供更详细的错误信息，帮助诊断问题
		completedChunks := 0
		if resumeInfo != nil {
			completedChunks = len(resumeInfo.GetCompletedChunks())
		}

		return 0, fmt.Errorf("下载文件大小不匹配: 期望%s，实际%s (已完成分片: %d/%d)",
			fs.SizeSuffix(fileSize), fs.SizeSuffix(actualSize), completedChunks, params.NumChunks)
	}

	// 使用后端特定的验证
	if err := d.adapter.VerifyDownload(ctx, obj, tempFile); err != nil {
		return 0, fmt.Errorf("下载完整性验证失败: %w", err)
	}

	fs.Infof(d.fs, "✅ %s网盘并发下载完成: %s, 用时: %v, 速度: %s/s",
		d.backendType, fs.SizeSuffix(fileSize), downloadDuration,
		fs.SizeSuffix(int64(float64(fileSize)/downloadDuration.Seconds())))

	// 🔧 修复：延迟清理断点续传信息，支持跨云传输场景
	if d.resumeManager != nil {
		// 添加延迟清理，给跨云传输等后续操作留出时间
		go func() {
			time.Sleep(30 * time.Second) // 延迟30秒清理
			err := d.resumeManager.DeleteResumeInfo(taskID)
			if err != nil {
				fs.Debugf(d.fs, "延迟清理断点续传信息失败: %v (TaskID: %s)", err, taskID)
			} else {
				fs.Debugf(d.fs, "✅ 延迟清理断点续传信息成功 (TaskID: %s)", taskID)
			}
		}()
		fs.Debugf(d.fs, "🔧 已安排延迟清理断点续传信息: %s", taskID)
	}

	return actualSize, nil
}

// DownloadToFileWithAccount 支持Account对象的并发下载（参考115网盘实现）
func (d *UnifiedConcurrentDownloader) DownloadToFileWithAccount(ctx context.Context, obj fs.Object, tempFile *os.File, transfer *accounting.Transfer, options ...fs.OpenOption) (int64, error) {
	fileSize := obj.Size()
	if fileSize <= 0 {
		return 0, fmt.Errorf("文件大小无效: %d", fileSize)
	}

	// 计算分片参数
	params := d.CalculateChunkParams(fileSize)

	fs.Infof(d.fs, "📊 %s网盘并发下载参数: 分片大小=%s, 分片数=%d, 并发数=%d",
		d.backendType, fs.SizeSuffix(params.ChunkSize), params.NumChunks, params.MaxConcurrency)

	fs.Infof(d.fs, "🚀 %s网盘开始并发下载: %s (%s)",
		d.backendType, obj.Remote(), fs.SizeSuffix(fileSize))

	// 生成任务ID用于断点续传
	taskID := d.generateTaskID(obj)

	// 尝试加载断点续传信息
	resumeInfo, err := d.resumeManager.LoadResumeInfo(taskID)
	if err != nil {
		fs.Debugf(d.fs, "加载断点续传信息失败: %v", err)
	}

	// 🔧 优化1：断点续传信息预验证（与DownloadToFile保持一致）
	if resumeInfo != nil {
		if !d.validateResumeInfo(resumeInfo, tempFile, params, fileSize) {
			fs.Debugf(d.fs, "🔧 %s网盘断点续传信息验证失败，清理并重新开始", d.backendType)
			d.resumeManager.DeleteResumeInfo(taskID)
			resumeInfo = nil
		} else {
			fs.Debugf(d.fs, "✅ %s网盘断点续传信息验证通过", d.backendType)
		}
	}

	// 如果没有有效的断点信息，创建新的
	if resumeInfo == nil {
		resumeInfo = d.createResumeInfo(taskID, obj, params, tempFile)
	}

	// 🔧 完全参考115网盘：设置进度跟踪
	progress, currentAccount := d.setupProgressTracking(ctx, obj, resumeInfo, params.ChunkSize, transfer)

	// 🔧 完全参考115网盘：启动进度更新协程
	progressCtx, cancelProgress := context.WithCancel(ctx)
	defer cancelProgress()

	go d.updateAccountExtraInfo(progressCtx, progress, obj.Remote(), currentAccount)

	// 执行并发下载
	startTime := time.Now()
	err = d.downloadChunksConcurrentlyWithAccount(ctx, obj, tempFile, params, resumeInfo, progress, currentAccount)
	downloadDuration := time.Since(startTime)

	if err != nil {
		return 0, fmt.Errorf("%s网盘并发下载失败: %w", d.backendType, err)
	}

	// 🔧 修复：增强下载完整性验证
	stat, err := tempFile.Stat()
	if err != nil {
		return 0, fmt.Errorf("获取下载文件信息失败: %w", err)
	}

	actualSize := stat.Size()
	if actualSize != fileSize {
		// 🔧 文件大小不匹配时，清理断点续传信息并提供详细错误信息
		if d.resumeManager != nil {
			taskID := d.generateTaskID(obj)
			d.resumeManager.DeleteResumeInfo(taskID)
			fs.Debugf(d.fs, "文件大小不匹配，已清理断点续传信息: %s", taskID)
		}

		// 提供更详细的错误信息，帮助诊断问题
		completedChunks := 0
		if resumeInfo != nil {
			completedChunks = len(resumeInfo.GetCompletedChunks())
		}

		return 0, fmt.Errorf("下载文件大小不匹配: 期望%s，实际%s (已完成分片: %d/%d)",
			fs.SizeSuffix(fileSize), fs.SizeSuffix(actualSize), completedChunks, params.NumChunks)
	}

	// 使用后端特定的验证
	if err := d.adapter.VerifyDownload(ctx, obj, tempFile); err != nil {
		return 0, fmt.Errorf("下载完整性验证失败: %w", err)
	}

	fs.Infof(d.fs, "✅ %s网盘并发下载完成: %s, 用时: %v, 速度: %s/s",
		d.backendType, fs.SizeSuffix(fileSize), downloadDuration,
		fs.SizeSuffix(int64(float64(fileSize)/downloadDuration.Seconds())))

	// 🔧 修复：延迟清理断点续传信息，支持跨云传输场景
	if d.resumeManager != nil {
		// 添加延迟清理，给跨云传输等后续操作留出时间
		go func() {
			time.Sleep(30 * time.Second) // 延迟30秒清理
			err := d.resumeManager.DeleteResumeInfo(taskID)
			if err != nil {
				fs.Debugf(d.fs, "延迟清理断点续传信息失败: %v (TaskID: %s)", err, taskID)
			} else {
				fs.Debugf(d.fs, "✅ 延迟清理断点续传信息成功 (TaskID: %s)", taskID)
			}
		}()
		fs.Debugf(d.fs, "🔧 已安排延迟清理断点续传信息: %s", taskID)
	}

	return actualSize, nil
}

// setupProgressTracking 设置进度跟踪（完全参考115网盘实现）
func (d *UnifiedConcurrentDownloader) setupProgressTracking(ctx context.Context, obj fs.Object, resumeInfo UnifiedResumeInfo, chunkSize int64, transfer *accounting.Transfer) (*DownloadProgress, *accounting.Account) {
	// 创建进度跟踪器，考虑已完成的分片
	progress := NewDownloadProgress(resumeInfo.GetTotalChunks(), obj.Size())

	// 🔧 修复进度初始化：如果有已完成的分片，使用正确的分片大小更新进度
	if resumeInfo != nil {
		fileSize := obj.Size()
		for chunkIndex := range resumeInfo.GetCompletedChunks() {
			// 🔧 关键修复：计算实际的分片大小，而不是使用固定的chunkSize
			chunkStart := chunkIndex * chunkSize
			chunkEnd := chunkStart + chunkSize - 1
			if chunkEnd >= fileSize {
				chunkEnd = fileSize - 1
			}
			actualChunkSize := chunkEnd - chunkStart + 1
			progress.UpdateChunkProgress(chunkIndex, actualChunkSize, 0) // 已完成的分片，耗时为0
		}
	}

	// 设置Account对象
	var currentAccount *accounting.Account
	remoteName := obj.Remote()

	if transfer != nil {
		// 使用传入的Transfer对象创建Account
		dummyReader := io.NopCloser(strings.NewReader(""))
		currentAccount = transfer.Account(ctx, dummyReader)
		dummyReader.Close()
		fs.Debugf(d.fs, "🔧 使用传入的Transfer对象创建Account: %s", remoteName)
	} else {
		// 回退：尝试从全局统计查找现有Account
		currentAccount = d.findExistingAccount(remoteName)
	}

	return progress, currentAccount
}

// findExistingAccount 查找现有的Account对象（完全参考115网盘实现）
func (d *UnifiedConcurrentDownloader) findExistingAccount(remoteName string) *accounting.Account {
	if stats := accounting.GlobalStats(); stats != nil {
		currentAccount := stats.GetInProgressAccount(remoteName)
		if currentAccount == nil {
			// 尝试模糊匹配：查找包含文件名的Account
			allAccounts := stats.ListInProgressAccounts()
			for _, accountName := range allAccounts {
				if strings.Contains(accountName, remoteName) || strings.Contains(remoteName, accountName) {
					currentAccount = stats.GetInProgressAccount(accountName)
					if currentAccount != nil {
						fs.Debugf(d.fs, "%s网盘找到匹配的Account: %s -> %s", d.backendType, remoteName, accountName)
						return currentAccount
					}
				}
			}
		} else {
			fs.Debugf(d.fs, "%s网盘找到精确匹配的Account: %s", d.backendType, remoteName)
			return currentAccount
		}
	}

	fs.Debugf(d.fs, "%s网盘未找到Account对象，进度显示可能不准确: %s", d.backendType, remoteName)
	return nil
}

// updateAccountExtraInfo 更新Account的额外进度信息（完全参考115网盘实现）
func (d *UnifiedConcurrentDownloader) updateAccountExtraInfo(ctx context.Context, progress *DownloadProgress, remoteName string, account *accounting.Account) {
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

			// 🔧 完全参考115网盘：构建简洁的额外信息字符串，显示分片进度
			if completed > 0 && total > 0 {
				extraInfo := fmt.Sprintf("[%d/%d分片]", completed, total)
				account.SetExtraInfo(extraInfo)
				fs.Debugf(d.fs, "🔧 %s网盘设置分片进度信息: %s", d.backendType, extraInfo)
			} else {
				fs.Debugf(d.fs, "🔧 %s网盘分片进度信息为空: completed=%d, total=%d", d.backendType, completed, total)
			}
		}
	}
}

// GetDownloadURL 获取下载URL（委托给适配器）
func (d *UnifiedConcurrentDownloader) GetDownloadURL(ctx context.Context, obj fs.Object, start, end int64) (string, error) {
	return d.adapter.GetDownloadURL(ctx, obj, start, end)
}

// DownloadChunk 下载单个分片（委托给适配器）
func (d *UnifiedConcurrentDownloader) DownloadChunk(ctx context.Context, url string, tempFile *os.File, start, end int64) error {
	return d.adapter.DownloadChunk(ctx, url, tempFile, start, end, nil)
}

// 私有辅助方法

func (d *UnifiedConcurrentDownloader) calculateOptimalChunkSize(fileSize int64) int64 {
	// 🔧 优先使用后端特定的配置（如115网盘的10MB配置）
	if d.defaultChunkSize > 0 {
		fs.Debugf(d.fs, "🔧 使用后端特定分片大小: %s", fs.SizeSuffix(d.defaultChunkSize))
		return d.defaultChunkSize
	}

	// 基于文件大小动态计算分片大小（后备方案）
	switch {
	case fileSize < 100*1024*1024: // <100MB
		return 10 * 1024 * 1024 // 10MB
	case fileSize < 1*1024*1024*1024: // <1GB
		return 32 * 1024 * 1024 // 32MB
	case fileSize < 10*1024*1024*1024: // <10GB
		return 100 * 1024 * 1024 // 100MB
	default:
		return 200 * 1024 * 1024 // 200MB
	}
}

func (d *UnifiedConcurrentDownloader) calculateOptimalConcurrency(fileSize int64) int64 {
	// 基于文件大小和后端类型计算最优并发数
	baseConcurrency := int64(2)

	if fileSize > 1*1024*1024*1024 { // >1GB
		baseConcurrency = 4
	}

	// 不超过配置的最大并发数
	if baseConcurrency > d.maxConcurrency {
		baseConcurrency = d.maxConcurrency
	}

	return baseConcurrency
}

func (d *UnifiedConcurrentDownloader) generateTaskID(obj fs.Object) string {
	var taskID string
	switch d.backendType {
	case "123":
		taskID = GenerateTaskID123(obj.Remote(), obj.Size())
		fs.Debugf(d.fs, "🔧 生成123网盘TaskID: %s (路径: %s, 大小: %d)", taskID, obj.Remote(), obj.Size())
	case "115":
		taskID = GenerateTaskID115(obj.Remote(), obj.Size())
		fs.Debugf(d.fs, "🔧 生成115网盘TaskID: %s (路径: %s, 大小: %d)", taskID, obj.Remote(), obj.Size())
	default:
		taskID = fmt.Sprintf("%s_%s_%d_%d", d.backendType, obj.Remote(), obj.Size(), time.Now().Unix())
		fs.Debugf(d.fs, "🔧 生成默认TaskID: %s", taskID)
	}
	return taskID
}

func (d *UnifiedConcurrentDownloader) createResumeInfo(taskID string, obj fs.Object, params ChunkParams, tempFile *os.File) UnifiedResumeInfo {
	// 🔧 关键修复：为所有后端保存临时文件路径
	tempFilePath := ""
	if tempFile != nil {
		tempFilePath = tempFile.Name()
	}

	switch d.backendType {
	case "123":
		return &ResumeInfo123{
			TaskID:              taskID,
			FileName:            obj.Remote(),
			FileSize:            obj.Size(),
			FilePath:            obj.Remote(),
			ChunkSize:           params.ChunkSize,
			TotalChunks:         params.NumChunks,
			CompletedChunks:     make(map[int64]bool),
			CreatedAt:           time.Now(),
			TempFilePath:        tempFilePath, // 🔧 123网盘也保存临时文件路径
			BackendSpecificData: make(map[string]interface{}),
		}
	case "115":
		return &ResumeInfo115{
			TaskID:              taskID,
			FileName:            obj.Remote(),
			FileSize:            obj.Size(),
			FilePath:            obj.Remote(),
			ChunkSize:           params.ChunkSize,
			TotalChunks:         params.NumChunks,
			CompletedChunks:     make(map[int64]bool),
			CreatedAt:           time.Now(),
			TempFilePath:        tempFilePath, // 🔧 115网盘保存临时文件路径
			BackendSpecificData: make(map[string]interface{}),
		}
	default:
		return nil
	}
}

// downloadChunksConcurrently 并发下载文件分片
func (d *UnifiedConcurrentDownloader) downloadChunksConcurrently(ctx context.Context, obj fs.Object, tempFile *os.File, params ChunkParams, resumeInfo UnifiedResumeInfo, progress *DownloadProgress) error {
	// 创建信号量控制并发数
	semaphore := make(chan struct{}, params.MaxConcurrency)

	// 创建错误通道
	errChan := make(chan error, params.NumChunks)

	// 创建等待组
	var wg sync.WaitGroup

	// 启动分片下载
	for i := int64(0); i < params.NumChunks; i++ {
		wg.Add(1)
		go func(chunkIndex int64) {
			defer wg.Done()

			// 添加panic恢复机制
			defer func() {
				if r := recover(); r != nil {
					fs.Errorf(d.fs, "%s网盘下载分片 %d 发生panic: %v", d.backendType, chunkIndex, r)
					errChan <- fmt.Errorf("下载分片 %d panic: %v", chunkIndex, r)
				}
			}()

			// 检查分片是否已完成
			if resumeInfo != nil && resumeInfo.IsChunkCompleted(chunkIndex) {
				fs.Debugf(d.fs, "跳过已完成的分片: %d", chunkIndex)
				return
			}

			// 获取信号量，带超时控制
			chunkTimeout := 10 * time.Minute
			chunkCtx, cancelChunk := context.WithTimeout(ctx, chunkTimeout)
			defer cancelChunk()

			select {
			case semaphore <- struct{}{}:
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
			start := chunkIndex * params.ChunkSize
			end := start + params.ChunkSize - 1
			if end >= obj.Size() {
				end = obj.Size() - 1
			}

			// 下载分片
			chunkStartTime := time.Now()
			err := d.downloadSingleChunk(chunkCtx, obj, tempFile, start, end, chunkIndex, nil)
			chunkDuration := time.Since(chunkStartTime)

			if err != nil {
				if chunkCtx.Err() != nil {
					errChan <- fmt.Errorf("下载分片 %d 超时: %w", chunkIndex, chunkCtx.Err())
				} else {
					errChan <- fmt.Errorf("下载分片 %d 失败: %w", chunkIndex, err)
				}
				return
			}

			// 标记分片为已完成
			if resumeInfo != nil {
				resumeInfo.MarkChunkCompleted(chunkIndex)
				if d.resumeManager != nil {
					if saveErr := d.resumeManager.SaveResumeInfo(resumeInfo); saveErr != nil {
						fs.Debugf(d.fs, "保存断点续传信息失败: %v", saveErr)
					}
				}
			}

			// 更新进度
			actualChunkSize := end - start + 1
			progress.UpdateChunkProgress(chunkIndex, actualChunkSize, chunkDuration.Nanoseconds())

			// 🔧 修复分片索引显示：显示实际的分片索引和字节范围，避免用户困惑
			fs.Debugf(d.fs, "✅ %s网盘分片 %d/%d 下载完成, 大小: %s, 用时: %v (bytes=%d-%d)",
				d.backendType, chunkIndex, params.NumChunks-1, fs.SizeSuffix(actualChunkSize), chunkDuration, start, end)

		}(i)
	}

	// 等待所有分片完成
	wg.Wait()
	close(errChan)

	// 检查是否有错误
	var errors []error
	for err := range errChan {
		errors = append(errors, err)
	}

	if len(errors) > 0 {
		return fmt.Errorf("下载失败，错误数量: %d, 第一个错误: %w", len(errors), errors[0])
	}

	return nil
}

// downloadChunksConcurrentlyWithAccount 支持Account对象的并发下载（完全参考115网盘实现）
func (d *UnifiedConcurrentDownloader) downloadChunksConcurrentlyWithAccount(ctx context.Context, obj fs.Object, tempFile *os.File, params ChunkParams, resumeInfo UnifiedResumeInfo, progress *DownloadProgress, currentAccount *accounting.Account) error {
	// 创建信号量控制并发数
	semaphore := make(chan struct{}, params.MaxConcurrency)

	// 创建错误通道
	errChan := make(chan error, params.NumChunks)

	// 创建等待组
	var wg sync.WaitGroup

	// 启动分片下载
	for i := int64(0); i < params.NumChunks; i++ {
		wg.Add(1)
		go func(chunkIndex int64) {
			defer wg.Done()

			// 添加panic恢复机制
			defer func() {
				if r := recover(); r != nil {
					fs.Errorf(d.fs, "%s网盘下载分片 %d 发生panic: %v", d.backendType, chunkIndex, r)
					errChan <- fmt.Errorf("下载分片 %d panic: %v", chunkIndex, r)
				}
			}()

			// 检查分片是否已完成
			if resumeInfo != nil && resumeInfo.IsChunkCompleted(chunkIndex) {
				fs.Debugf(d.fs, "✅ 跳过已完成的分片: %d/%d", chunkIndex, params.NumChunks-1)
				return
			} else if resumeInfo != nil {
				fs.Debugf(d.fs, "🔄 分片 %d/%d 需要下载（未完成）", chunkIndex, params.NumChunks-1)
			} else {
				fs.Debugf(d.fs, "🔄 分片 %d/%d 需要下载（无断点续传信息）", chunkIndex, params.NumChunks-1)
			}

			// 获取信号量，带超时控制
			chunkTimeout := 10 * time.Minute
			chunkCtx, cancelChunk := context.WithTimeout(ctx, chunkTimeout)
			defer cancelChunk()

			select {
			case semaphore <- struct{}{}:
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
			start := chunkIndex * params.ChunkSize
			end := start + params.ChunkSize - 1
			if end >= obj.Size() {
				end = obj.Size() - 1
			}

			// 下载分片
			chunkStartTime := time.Now()
			err := d.downloadSingleChunk(chunkCtx, obj, tempFile, start, end, chunkIndex, currentAccount)
			chunkDuration := time.Since(chunkStartTime)

			if err != nil {
				if chunkCtx.Err() != nil {
					errChan <- fmt.Errorf("下载分片 %d 超时: %w", chunkIndex, chunkCtx.Err())
				} else {
					errChan <- fmt.Errorf("下载分片 %d 失败: %w", chunkIndex, err)
				}
				return
			}

			// 标记分片为已完成
			if resumeInfo != nil {
				resumeInfo.MarkChunkCompleted(chunkIndex)
				if d.resumeManager != nil {
					if saveErr := d.resumeManager.SaveResumeInfo(resumeInfo); saveErr != nil {
						fs.Debugf(d.fs, "保存断点续传信息失败: %v", saveErr)
					}
				}
			}

			// 更新进度
			actualChunkSize := end - start + 1
			progress.UpdateChunkProgress(chunkIndex, actualChunkSize, chunkDuration.Nanoseconds())

			// 🔧 关键修复：进度报告由DownloadChunk方法负责，这里不重复报告
			// downloadSingleChunk会将currentAccount传递给DownloadChunk方法，由DownloadChunk负责进度报告

			// 🔧 修复分片索引显示：显示实际的分片索引和字节范围，避免用户困惑
			fs.Debugf(d.fs, "✅ %s网盘分片 %d/%d 下载完成, 大小: %s, 用时: %v (bytes=%d-%d)",
				d.backendType, chunkIndex, params.NumChunks-1, fs.SizeSuffix(actualChunkSize), chunkDuration, start, end)

		}(i)
	}

	// 等待所有分片完成
	wg.Wait()
	close(errChan)

	// 检查是否有错误
	var errors []error
	for err := range errChan {
		errors = append(errors, err)
	}

	if len(errors) > 0 {
		return fmt.Errorf("下载失败，错误数量: %d, 第一个错误: %w", len(errors), errors[0])
	}

	return nil
}

// downloadSingleChunk 下载单个分片
func (d *UnifiedConcurrentDownloader) downloadSingleChunk(ctx context.Context, obj fs.Object, tempFile *os.File, start, end, chunkIndex int64, account *accounting.Account) error {
	maxRetries := 2 // 最多重试2次

	for retry := 0; retry <= maxRetries; retry++ {
		// 获取下载URL
		url, err := d.adapter.GetDownloadURL(ctx, obj, start, end)
		if err != nil {
			if retry < maxRetries {
				fs.Debugf(d.fs, "🔄 分片 %d 获取URL失败，重试 %d/%d: %v", chunkIndex, retry+1, maxRetries, err)
				time.Sleep(time.Duration(retry+1) * 500 * time.Millisecond)
				continue
			}
			return fmt.Errorf("获取下载URL失败: %w", err)
		}

		// 下载分片数据
		err = d.adapter.DownloadChunk(ctx, url, tempFile, start, end, account)
		if err != nil {
			// 🔧 检查是否为403 URL过期错误
			if strings.Contains(err.Error(), "403") && strings.Contains(err.Error(), "invalid signature") {
				if retry < maxRetries {
					fs.Debugf(d.fs, "🔄 分片 %d URL过期(403)，重试 %d/%d: %v", chunkIndex, retry+1, maxRetries, err)
					time.Sleep(time.Duration(retry+1) * 1000 * time.Millisecond) // 稍长的延迟
					continue
				}
			}

			if retry < maxRetries {
				fs.Debugf(d.fs, "🔄 分片 %d 下载失败，重试 %d/%d: %v", chunkIndex, retry+1, maxRetries, err)
				time.Sleep(time.Duration(retry+1) * 500 * time.Millisecond)
				continue
			}
			return fmt.Errorf("下载分片数据失败: %w", err)
		}

		// 成功下载
		return nil
	}

	return fmt.Errorf("分片 %d 下载失败，已达到最大重试次数", chunkIndex)
}

// NewDownloadProgress 创建下载进度跟踪器
func NewDownloadProgress(totalChunks, totalBytes int64) *DownloadProgress {
	return &DownloadProgress{
		totalChunks:   totalChunks,
		totalBytes:    totalBytes,
		startTime:     time.Now(),
		chunkProgress: make(map[int64]ChunkProgress),
	}
}

// UpdateChunkProgress 更新分片进度
func (dp *DownloadProgress) UpdateChunkProgress(chunkIndex, chunkSize, durationNanos int64) {
	dp.mu.Lock()
	defer dp.mu.Unlock()

	// 🔧 关键修复：检查分片是否已经被计算过，避免重复计算
	if _, exists := dp.chunkProgress[chunkIndex]; exists {
		// 分片已经存在，不重复计算
		return
	}

	dp.completedChunks++
	dp.downloadedBytes += chunkSize

	speed := int64(0)
	if durationNanos > 0 {
		speed = int64(float64(chunkSize) / (float64(durationNanos) / 1e9))
	}

	dp.chunkProgress[chunkIndex] = ChunkProgress{
		Index:       chunkIndex,
		Size:        chunkSize,
		Downloaded:  chunkSize,
		StartTime:   time.Now().Add(-time.Duration(durationNanos)),
		CompletedAt: time.Now(),
		Speed:       speed,
	}
}

// GetProgress 获取当前进度
func (dp *DownloadProgress) GetProgress() (completed, total int64, percentage float64, avgSpeed int64) {
	dp.mu.RLock()
	defer dp.mu.RUnlock()

	completed = dp.completedChunks
	total = dp.totalChunks

	if total > 0 {
		percentage = float64(completed) / float64(total) * 100.0
	}

	elapsed := time.Since(dp.startTime)
	if elapsed.Seconds() > 0 {
		avgSpeed = int64(float64(dp.downloadedBytes) / elapsed.Seconds())
	}

	return completed, total, percentage, avgSpeed
}

// GetProgressInfo 获取当前进度信息（完全参考115网盘实现）
func (dp *DownloadProgress) GetProgressInfo() (percentage float64, avgSpeed float64, peakSpeed float64, eta time.Duration, completed, total int64, downloadedBytes, totalBytes int64) {
	dp.mu.RLock()
	defer dp.mu.RUnlock()

	elapsed := time.Since(dp.startTime)
	percentage = float64(dp.completedChunks) / float64(dp.totalChunks) * 100

	if elapsed.Seconds() > 0 {
		avgSpeed = float64(dp.downloadedBytes) / elapsed.Seconds() / 1024 / 1024 // MB/s
	}

	// 计算峰值速度（简化版本，使用平均速度）
	peakSpeed = avgSpeed

	if avgSpeed > 0 && dp.downloadedBytes > 0 {
		remainingBytes := dp.totalBytes - dp.downloadedBytes
		etaSeconds := float64(remainingBytes) / (avgSpeed * 1024 * 1024)
		eta = time.Duration(etaSeconds) * time.Second
	}

	completed = dp.completedChunks
	total = dp.totalChunks
	downloadedBytes = dp.downloadedBytes
	totalBytes = dp.totalBytes

	return
}

// validateResumeInfo 验证断点续传信息的有效性
// 🔧 优化改进：智能验证断点续传信息，尝试恢复之前的临时文件
func (d *UnifiedConcurrentDownloader) validateResumeInfo(resumeInfo UnifiedResumeInfo, tempFile *os.File, params ChunkParams, fileSize int64) bool {
	if resumeInfo == nil {
		return false
	}

	completedChunks := resumeInfo.GetCompletedChunks()

	// 验证分片索引的合理性
	for chunkIndex := range completedChunks {
		if chunkIndex < 0 || chunkIndex >= params.NumChunks {
			fs.Debugf(d.fs, "🔧 %s网盘断点续传信息包含无效分片索引: %d (总分片数: %d)",
				d.backendType, chunkIndex, params.NumChunks)
			return false
		}
	}

	// 验证断点续传信息的基本完整性
	if resumeInfo.GetTotalChunks() != params.NumChunks {
		fs.Debugf(d.fs, "🔧 %s网盘断点续传信息分片数不匹配: 记录=%d, 计算=%d",
			d.backendType, resumeInfo.GetTotalChunks(), params.NumChunks)
		return false
	}

	// 验证文件大小是否匹配
	if resumeInfo.GetFileSize() != fileSize {
		fs.Debugf(d.fs, "🔧 %s网盘断点续传信息文件大小不匹配: 记录=%s, 实际=%s",
			d.backendType, fs.SizeSuffix(resumeInfo.GetFileSize()), fs.SizeSuffix(fileSize))
		return false
	}

	// 🔧 关键修复：尝试恢复之前的临时文件（支持115和123网盘）
	var previousTempFilePath string
	if r115, ok := resumeInfo.(*ResumeInfo115); ok {
		previousTempFilePath = r115.TempFilePath
	} else if r123, ok := resumeInfo.(*ResumeInfo123); ok {
		previousTempFilePath = r123.TempFilePath
	}

	if previousTempFilePath != "" {
		// 检查之前的临时文件是否存在
		if stat, err := os.Stat(previousTempFilePath); err == nil {
			// 计算预期大小
			var expectedSize int64
			for chunkIndex := range completedChunks {
				chunkStart := chunkIndex * params.ChunkSize
				chunkEnd := chunkStart + params.ChunkSize - 1
				if chunkEnd >= fileSize {
					chunkEnd = fileSize - 1
				}
				chunkSize := chunkEnd - chunkStart + 1
				expectedSize += chunkSize
			}

			if stat.Size() == expectedSize {
				fs.Debugf(d.fs, "🎯 %s网盘发现匹配的临时文件: %s, 大小=%s",
					d.backendType, previousTempFilePath, fs.SizeSuffix(stat.Size()))

				// 尝试复制之前的临时文件内容到当前临时文件
				if err := d.copyTempFileContent(previousTempFilePath, tempFile); err != nil {
					fs.Debugf(d.fs, "⚠️ %s网盘复制临时文件内容失败: %v", d.backendType, err)
				} else {
					fs.Debugf(d.fs, "✅ %s网盘成功恢复临时文件内容: %s", d.backendType, fs.SizeSuffix(expectedSize))
					return true
				}
			} else {
				fs.Debugf(d.fs, "⚠️ %s网盘临时文件大小不匹配: 期望=%s, 实际=%s",
					d.backendType, fs.SizeSuffix(expectedSize), fs.SizeSuffix(stat.Size()))
			}
		} else {
			fs.Debugf(d.fs, "⚠️ %s网盘之前的临时文件不存在: %s", d.backendType, previousTempFilePath)
		}
	}

	// 🔧 智能验证：检查当前临时文件状态
	stat, err := tempFile.Stat()
	if err != nil {
		fs.Debugf(d.fs, "🔧 获取当前临时文件信息失败: %v", err)
		return true // 允许使用断点续传信息，让下载过程处理
	}

	actualFileSize := stat.Size()
	// 计算预期大小
	var expectedSize int64
	for chunkIndex := range completedChunks {
		chunkStart := chunkIndex * params.ChunkSize
		chunkEnd := chunkStart + params.ChunkSize - 1
		if chunkEnd >= fileSize {
			chunkEnd = fileSize - 1
		}
		chunkSize := chunkEnd - chunkStart + 1
		expectedSize += chunkSize
	}

	if actualFileSize == expectedSize {
		fs.Debugf(d.fs, "✅ %s网盘断点续传信息与临时文件完全匹配: 已完成分片=%d/%d, 文件大小=%s",
			d.backendType, len(completedChunks), params.NumChunks, fs.SizeSuffix(actualFileSize))
	} else {
		fs.Debugf(d.fs, "⚠️ %s网盘临时文件大小不匹配，但断点续传信息仍然有效: 已完成分片=%d/%d, 预期大小=%s, 实际大小=%s",
			d.backendType, len(completedChunks), params.NumChunks,
			fs.SizeSuffix(expectedSize), fs.SizeSuffix(actualFileSize))
	}

	fs.Debugf(d.fs, "✅ %s网盘断点续传信息验证通过: 已完成分片=%d/%d",
		d.backendType, len(completedChunks), params.NumChunks)
	return true
}

// copyTempFileContent 复制临时文件内容
func (d *UnifiedConcurrentDownloader) copyTempFileContent(srcPath string, dstFile *os.File) error {
	// 打开源文件
	srcFile, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("打开源临时文件失败: %w", err)
	}
	defer srcFile.Close()

	// 重置目标文件指针到开始位置
	if _, err := dstFile.Seek(0, 0); err != nil {
		return fmt.Errorf("重置目标文件指针失败: %w", err)
	}

	// 复制文件内容
	copied, err := io.Copy(dstFile, srcFile)
	if err != nil {
		return fmt.Errorf("复制文件内容失败: %w", err)
	}

	// 确保数据写入磁盘
	if err := dstFile.Sync(); err != nil {
		return fmt.Errorf("同步文件数据失败: %w", err)
	}

	fs.Debugf(d.fs, "成功复制临时文件内容: %s", fs.SizeSuffix(copied))
	return nil
}
