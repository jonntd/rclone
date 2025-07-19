// 115网盘分片上传实现
// 采用单线程顺序上传模式，确保上传稳定性和SHA1验证通过
package _115

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss"
	"github.com/rclone/rclone/backend/115/api"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/accounting"
	"github.com/rclone/rclone/fs/fserrors"
	"github.com/rclone/rclone/lib/atexit"
	"github.com/rclone/rclone/lib/pool"
)

const (
	// 🚀 超激进优化：进一步增加缓冲区大小，最大化I/O性能
	bufferSize           = 16 * 1024 * 1024 // 从8MB增加到16MB，进一步提升I/O效率
	bufferCacheSize      = 16               // 进一步减少缓存数量，但单个更大
	bufferCacheFlushTime = 5 * time.Second  // flush the cached buffers after this long
)

// bufferPool is a global pool of buffers
var (
	bufferPool     *pool.Pool
	bufferPoolOnce sync.Once
)

// get a buffer pool
func getPool() *pool.Pool {
	bufferPoolOnce.Do(func() {
		ci := fs.GetConfig(context.Background())
		// Initialise the buffer pool when used
		bufferPool = pool.New(bufferCacheFlushTime, bufferSize, bufferCacheSize, ci.UseMmap)
	})
	return bufferPool
}

// NewRW gets a pool.RW using the multipart pool
func NewRW() *pool.RW {
	return pool.NewRW(getPool())
}

// Upload 执行115网盘单线程分片上传
// 🔧 参考阿里云OSS示例优化：采用顺序上传模式，确保上传稳定性和SHA1验证通过
func (w *ossChunkWriter) Upload(ctx context.Context) (err error) {
	uploadCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer atexit.OnError(&err, func() {
		cancel()
		fs.Debugf(w.o, "🔧 分片上传取消中...")
		errCancel := w.Abort(ctx)
		if errCancel != nil {
			fs.Debugf(w.o, "❌ 取消分片上传失败: %v", errCancel)
		}
	})()

	var (
		finished   = false
		off        int64
		size       = w.size
		chunkSize  = w.chunkSize
		partNum    int64
		totalParts int64
		startTime  = time.Now() // 🔧 添加计时，参考阿里云OSS示例
	)

	// 🔧 参考阿里云OSS示例：计算总分片数用于进度显示
	if size > 0 {
		totalParts = (size + chunkSize - 1) / chunkSize
	} else {
		// 🔧 处理未知大小的情况
		fs.Debugf(w.o, "⚠️ 文件大小未知，将动态计算分片数")
	}

	// 手动处理accounting
	in, acc := accounting.UnWrapAccounting(w.in)

	// 🔧 参考阿里云OSS示例：详细的上传开始信息
	fs.Infof(w.o, "🚀 开始115网盘单线程分片上传")
	fs.Infof(w.o, "📊 上传参数: 文件大小=%v, 分片大小=%v, 预计分片数=%d",
		fs.SizeSuffix(size), fs.SizeSuffix(chunkSize), totalParts)
	fs.Debugf(w.o, "🔧 OSS配置: Bucket=%s, Key=%s, UploadId=%s",
		*w.imur.Bucket, *w.imur.Key, *w.imur.UploadId)

	for partNum = 0; !finished; partNum++ {
		// 🔧 参考阿里云OSS示例：检查断点续传，跳过已完成的分片
		if w.resumeInfo != nil && w.resumeInfo.CompletedChunks[partNum] {
			fs.Debugf(w.o, "⏭️ 跳过已完成的分片%d", partNum+1)
			off += chunkSize
			if size > 0 && off >= size {
				finished = true
			}
			continue
		}

		// 🔧 参考阿里云OSS示例：获取内存缓冲区
		rw := NewRW()
		if acc != nil {
			rw.SetAccounting(acc.AccountRead)
		}

		// 🔧 检查上下文是否已取消
		if uploadCtx.Err() != nil {
			_ = rw.Close()
			fs.Debugf(w.o, "🔧 上传被取消，停止分片上传")
			break
		}

		// 🔧 参考阿里云OSS示例：读取分片数据
		var n int64
		chunkStartTime := time.Now() // 记录分片开始时间
		n, err = io.CopyN(rw, in, chunkSize)
		if err == io.EOF {
			if n == 0 && partNum != 0 { // 如果不是第一个分片且没有数据，则结束
				_ = rw.Close()
				fs.Debugf(w.o, "✅ 所有分片读取完成")
				break
			}
			finished = true
		} else if err != nil {
			_ = rw.Close()
			return fmt.Errorf("读取分片数据失败: %w", err)
		}

		// 🚀 激进优化：极简分片进度显示，减少日志开销
		currentPart := partNum + 1
		elapsed := time.Since(startTime)
		if totalParts > 0 {
			percentage := float64(currentPart) / float64(totalParts) * 100

			// 🚀 超激进优化：只在关键节点输出日志（每25%或最后一个分片）
			// 🚀 实时进度优化：更频繁的进度显示，提升用户体验
			shouldLog := (currentPart == 1) || // 第一个分片
				(currentPart%2 == 0) || // 每2个分片显示一次
				(currentPart == totalParts) // 最后一个分片

			if shouldLog {
				// 估算剩余时间
				avgTimePerPart := elapsed / time.Duration(currentPart)
				remainingParts := totalParts - currentPart
				estimatedRemaining := avgTimePerPart * time.Duration(remainingParts)

				fs.Infof(w.o, "📤 115网盘单线程上传: 分片%d/%d (%.1f%%) | %v | 已用时:%v | 预计剩余:%v",
					currentPart, totalParts, percentage, fs.SizeSuffix(n),
					elapsed.Truncate(time.Second), estimatedRemaining.Truncate(time.Second))
			}
		} else {
			// 🚀 未知大小时只在第一个分片输出日志
			if currentPart == 1 {
				fs.Infof(w.o, "📤 115网盘单线程上传: 分片%d | %v | 偏移:%v | 已用时:%v",
					currentPart, fs.SizeSuffix(n), fs.SizeSuffix(off), elapsed.Truncate(time.Second))
			}
		}

		// 🔧 参考阿里云OSS示例：上传当前分片并记录详细信息
		fs.Debugf(w.o, "🔧 开始上传分片%d: 大小=%v, 偏移=%v", currentPart, fs.SizeSuffix(n), fs.SizeSuffix(off))
		chunkSize, err := w.WriteChunk(uploadCtx, int32(partNum), rw)
		_ = rw.Close() // 释放内存缓冲区

		if err != nil {
			// 🔧 参考阿里云OSS示例：详细的错误信息
			return fmt.Errorf("上传分片%d失败 (大小:%v, 偏移:%v): %w",
				currentPart, fs.SizeSuffix(n), fs.SizeSuffix(off), err)
		}

		// 🔧 记录分片上传成功信息
		chunkDuration := time.Since(chunkStartTime)
		if chunkSize > 0 {
			speed := float64(chunkSize) / chunkDuration.Seconds() / 1024 / 1024 // MB/s
			fs.Debugf(w.o, "✅ 分片%d上传成功: 实际大小=%v, 用时=%v, 速度=%.2fMB/s",
				currentPart, fs.SizeSuffix(chunkSize), chunkDuration.Truncate(time.Millisecond), speed)
		}

		// 🔧 参考阿里云OSS示例：保存断点续传信息
		if w.resumeInfo != nil {
			w.resumeInfo.MarkChunkCompleted(partNum)
			if w.f.resumeManager != nil {
				if saveErr := w.f.resumeManager.SaveResumeInfo(w.resumeInfo); saveErr != nil {
					fs.Debugf(w.o, "⚠️ 保存断点续传信息失败: %v", saveErr)
				} else {
					fs.Debugf(w.o, "💾 断点续传信息已保存: 分片%d/%d",
						w.resumeInfo.GetCompletedChunkCount(), w.resumeInfo.TotalChunks)
				}
			}
		}

		off += n
	}

	// 🔧 参考阿里云OSS示例：完成分片上传并显示总体统计
	totalDuration := time.Since(startTime)
	actualParts := partNum

	fs.Infof(w.o, "🔧 开始完成分片上传: 总分片数=%d, 总用时=%v", actualParts, totalDuration.Truncate(time.Second))

	err = w.Close(ctx)
	if err != nil {
		return fmt.Errorf("完成分片上传失败: %w", err)
	}

	// 🔧 参考阿里云OSS示例：显示上传完成统计信息
	if size > 0 && totalDuration.Seconds() > 0 {
		avgSpeed := float64(size) / totalDuration.Seconds() / 1024 / 1024 // MB/s
		fs.Infof(w.o, "✅ 115网盘分片上传完成!")
		fs.Infof(w.o, "📊 上传统计: 文件大小=%v, 分片数=%d, 总用时=%v, 平均速度=%.2fMB/s",
			fs.SizeSuffix(size), actualParts, totalDuration.Truncate(time.Second), avgSpeed)
	} else {
		fs.Infof(w.o, "✅ 115网盘分片上传完成: 分片数=%d, 总用时=%v",
			actualParts, totalDuration.Truncate(time.Second))
	}

	// 🔧 参考阿里云OSS示例：清理断点续传信息
	if w.resumeInfo != nil && w.f.resumeManager != nil {
		if cleanErr := w.f.resumeManager.DeleteResumeInfo(w.taskID); cleanErr != nil {
			fs.Debugf(w.o, "⚠️ 清理断点续传信息失败: %v", cleanErr)
		} else {
			fs.Debugf(w.o, "🧹 断点续传信息已清理")
		}
	}

	return nil
}

var warnStreamUpload sync.Once

// ossChunkWriter 115网盘分片上传写入器
// 🔧 参考阿里云OSS示例优化：采用单线程顺序上传模式，集成断点续传功能
type ossChunkWriter struct {
	chunkSize     int64                              // 分片大小
	size          int64                              // 文件总大小
	f             *Fs                                // 文件系统实例
	o             *Object                            // 对象实例
	in            io.Reader                          // 输入流
	uploadedParts []oss.UploadPart                   // 已上传的分片列表
	client        *oss.Client                        // OSS客户端
	callback      string                             // 115网盘回调URL
	callbackVar   string                             // 115网盘回调变量
	callbackRes   map[string]any                     // 回调结果
	imur          *oss.InitiateMultipartUploadResult // 分片上传初始化结果
	// 🔧 新增断点续传支持
	resumeInfo *ResumeInfo115 // 断点续传信息
	taskID     string         // 任务ID
}

// newChunkWriterWithClient 创建分片写入器，使用指定的优化OSS客户端
func (f *Fs) newChunkWriterWithClient(ctx context.Context, src fs.ObjectInfo, ui *api.UploadInitInfo, in io.Reader, o *Object, ossClient *oss.Client, options ...fs.OpenOption) (w *ossChunkWriter, err error) {
	uploadParts := min(max(1, f.opt.MaxUploadParts), maxUploadParts)
	size := src.Size()

	// 🔧 参考OpenList：使用简化的分片大小计算
	chunkSize := f.calculateOptimalChunkSize(size)

	// 处理未知文件大小的情况（流式上传）
	if size == -1 {
		warnStreamUpload.Do(func() {
			fs.Logf(f, "流式上传使用分片大小 %v，最大文件大小限制为 %v",
				chunkSize, fs.SizeSuffix(int64(chunkSize)*int64(uploadParts)))
		})
	} else {
		// 🔧 简化分片计算：直接使用OpenList风格的分片大小，减少复杂性
		// 保持OpenList的简单有效策略
		fs.Debugf(f, "115网盘分片上传: 使用OpenList风格分片大小 %v", chunkSize)
	}

	// 115网盘采用单线程分片上传模式，确保稳定性
	fs.Debugf(f, "115网盘分片上传: 文件大小 %v, 分片大小 %v", fs.SizeSuffix(size), fs.SizeSuffix(int64(chunkSize)))

	// 🔧 参考阿里云OSS示例：初始化断点续传功能
	taskID := GenerateTaskID115(o.remote, size)
	var resumeInfo *ResumeInfo115

	// 尝试加载现有的断点续传信息
	if f.resumeManager != nil {
		resumeInfo, err = f.resumeManager.LoadResumeInfo(taskID)
		if err != nil {
			fs.Debugf(f, "⚠️ 加载断点续传信息失败: %v", err)
		} else if resumeInfo != nil {
			fs.Infof(o, "🔄 发现断点续传信息: 已完成分片 %d/%d",
				resumeInfo.GetCompletedChunkCount(), resumeInfo.TotalChunks)
		}
	}

	// 如果没有断点续传信息，创建新的
	if resumeInfo == nil {
		totalParts := int64(1)
		if size > 0 {
			totalParts = (size + int64(chunkSize) - 1) / int64(chunkSize)
		}
		resumeInfo = &ResumeInfo115{
			TaskID:          taskID,
			FileName:        o.remote,
			FileSize:        size,
			FilePath:        o.remote,
			ChunkSize:       int64(chunkSize),
			TotalChunks:     totalParts,
			CompletedChunks: make(map[int64]bool),
			CreatedAt:       time.Now(),
		}
	}

	w = &ossChunkWriter{
		chunkSize:  int64(chunkSize),
		size:       size,
		f:          f,
		o:          o,
		in:         in,
		resumeInfo: resumeInfo,
		taskID:     taskID,
	}

	// 🚀 关键优化：使用传入的优化OSS客户端，而不是创建新的
	w.client = ossClient
	fs.Debugf(o, "🚀 分片上传使用优化OSS客户端")

	req := &oss.InitiateMultipartUploadRequest{
		Bucket: oss.Ptr(ui.GetBucket()),
		Key:    oss.Ptr(ui.GetObject()),
	}
	// 115网盘OSS使用Sequential模式，确保分片按顺序处理
	req.Parameters = map[string]string{
		"sequential": "",
	}
	// Apply upload options
	for _, option := range options {
		key, value := option.Header()
		lowerKey := strings.ToLower(key)
		switch lowerKey {
		case "":
			// ignore
		case "cache-control":
			req.CacheControl = oss.Ptr(value)
		case "content-disposition":
			req.ContentDisposition = oss.Ptr(value)
		case "content-encoding":
			req.ContentEncoding = oss.Ptr(value)
		case "content-type":
			req.ContentType = oss.Ptr(value)
		}
	}
	// 🔧 使用专用的上传调速器，优化分片上传初始化API调用频率
	err = w.f.uploadPacer.Call(func() (bool, error) {
		w.imur, err = w.client.InitiateMultipartUpload(ctx, req)
		return w.shouldRetry(ctx, err)
	})
	if err != nil {
		return nil, fmt.Errorf("create multipart upload failed: %w", err)
	}
	w.callback, w.callbackVar = ui.GetCallback(), ui.GetCallbackVar()
	fs.Debugf(w.o, "115网盘分片上传初始化成功: %q", *w.imur.UploadId)
	return
}

func (f *Fs) newChunkWriter(ctx context.Context, src fs.ObjectInfo, ui *api.UploadInitInfo, in io.Reader, o *Object, options ...fs.OpenOption) (w *ossChunkWriter, err error) {
	uploadParts := min(max(1, f.opt.MaxUploadParts), maxUploadParts)
	size := src.Size()

	// 🔧 参考OpenList：使用简化的分片大小计算
	chunkSize := f.calculateOptimalChunkSize(size)

	// 处理未知文件大小的情况（流式上传）
	if size == -1 {
		warnStreamUpload.Do(func() {
			fs.Logf(f, "流式上传使用分片大小 %v，最大文件大小限制为 %v",
				chunkSize, fs.SizeSuffix(int64(chunkSize)*int64(uploadParts)))
		})
	} else {
		// 🔧 简化分片计算：直接使用OpenList风格的分片大小，减少复杂性
		// 保持OpenList的简单有效策略
		fs.Debugf(f, "115网盘分片上传: 使用OpenList风格分片大小 %v", chunkSize)
	}

	// 115网盘采用单线程分片上传模式，确保稳定性
	fs.Debugf(f, "115网盘分片上传: 文件大小 %v, 分片大小 %v", fs.SizeSuffix(size), fs.SizeSuffix(int64(chunkSize)))

	// 🔧 参考阿里云OSS示例：初始化断点续传功能
	taskID := GenerateTaskID115(o.remote, size)
	var resumeInfo *ResumeInfo115

	// 尝试加载现有的断点续传信息
	if f.resumeManager != nil {
		resumeInfo, err = f.resumeManager.LoadResumeInfo(taskID)
		if err != nil {
			fs.Debugf(f, "⚠️ 加载断点续传信息失败: %v", err)
		} else if resumeInfo != nil {
			fs.Infof(o, "🔄 发现断点续传信息: 已完成分片 %d/%d",
				resumeInfo.GetCompletedChunkCount(), resumeInfo.TotalChunks)
		}
	}

	// 如果没有断点续传信息，创建新的
	if resumeInfo == nil {
		totalParts := int64(1)
		if size > 0 {
			totalParts = (size + int64(chunkSize) - 1) / int64(chunkSize)
		}
		resumeInfo = &ResumeInfo115{
			TaskID:          taskID,
			FileName:        o.remote,
			FileSize:        size,
			FilePath:        o.remote,
			ChunkSize:       int64(chunkSize),
			TotalChunks:     totalParts,
			CompletedChunks: make(map[int64]bool),
			CreatedAt:       time.Now(),
		}
	}

	w = &ossChunkWriter{
		chunkSize:  int64(chunkSize),
		size:       size,
		f:          f,
		o:          o,
		in:         in,
		resumeInfo: resumeInfo,
		taskID:     taskID,
	}

	// 🚀 关键修复：使用标准的OSS客户端创建函数
	var client *oss.Client
	client, err = f.newOSSClient()
	if err != nil {
		return nil, fmt.Errorf("failed to create OSS client: %w", err)
	}
	w.client = client
	fs.Debugf(w.o, "🚀 分片上传使用优化OSS客户端（备用方法）")

	req := &oss.InitiateMultipartUploadRequest{
		Bucket: oss.Ptr(ui.GetBucket()),
		Key:    oss.Ptr(ui.GetObject()),
	}
	// 115网盘OSS使用Sequential模式，确保分片按顺序处理
	req.Parameters = map[string]string{
		"sequential": "",
	}
	// Apply upload options
	for _, option := range options {
		key, value := option.Header()
		lowerKey := strings.ToLower(key)
		switch lowerKey {
		case "":
			// ignore
		case "cache-control":
			req.CacheControl = oss.Ptr(value)
		case "content-disposition":
			req.ContentDisposition = oss.Ptr(value)
		case "content-encoding":
			req.ContentEncoding = oss.Ptr(value)
		case "content-type":
			req.ContentType = oss.Ptr(value)
		}
	}
	// 🔧 使用专用的上传调速器，优化分片上传初始化API调用频率
	err = w.f.uploadPacer.Call(func() (bool, error) {
		w.imur, err = w.client.InitiateMultipartUpload(ctx, req)
		return w.shouldRetry(ctx, err)
	})
	if err != nil {
		return nil, fmt.Errorf("create multipart upload failed: %w", err)
	}
	w.callback, w.callbackVar = ui.GetCallback(), ui.GetCallbackVar()
	fs.Debugf(w.o, "115网盘分片上传初始化成功: %q", *w.imur.UploadId)
	return
}

// shouldRetry returns a boolean as to whether this err
// deserve to be retried. It returns the err as a convenience
func (w *ossChunkWriter) shouldRetry(ctx context.Context, err error) (bool, error) {
	if fserrors.ContextError(ctx, &err) {
		return false, err
	}
	if fserrors.ShouldRetry(err) {
		return true, err
	}

	// Since alibabacloud-oss-go-sdk-v2, oss.ServiceError is wrapped by oss.OperationError
	// so we need to unwrap
	if opErr, ok := err.(*oss.OperationError); ok {
		err = opErr.Unwrap()
	}

	switch ossErr := err.(type) {
	case *oss.ServiceError:
		if ossErr.StatusCode == 403 && (ossErr.Code == "InvalidAccessKeyId" || ossErr.Code == "SecurityTokenExpired") {
			// oss: service returned error: StatusCode=403, ErrorCode=InvalidAccessKeyId,
			// ErrorMessage="The OSS Access Key Id you provided does not exist in our records.",

			// oss: service returned error: StatusCode=403, ErrorCode=SecurityTokenExpired,
			// ErrorMessage="The security token you provided has expired."

			// These errors cannot be handled once token is expired. Should update token proactively.
			return false, fserrors.FatalError(err)
		}
	}
	return false, err
}

// addCompletedPart 添加已完成的分片到列表
// 单线程模式下按顺序添加，无需复杂的并发控制
func (w *ossChunkWriter) addCompletedPart(part oss.UploadPart) {
	w.uploadedParts = append(w.uploadedParts, part)
}

// WriteChunk will write chunk number with reader bytes, where chunk number >= 0
// 🔧 参考阿里云OSS示例优化分片上传逻辑
func (w *ossChunkWriter) WriteChunk(ctx context.Context, chunkNumber int32, reader io.ReadSeeker) (currentChunkSize int64, err error) {
	if chunkNumber < 0 {
		err := fmt.Errorf("无效的分片编号: %v", chunkNumber)
		return -1, err
	}

	ossPartNumber := chunkNumber + 1
	var res *oss.UploadPartResult
	chunkStartTime := time.Now() // 🔧 记录分片上传开始时间

	// 🔧 参考阿里云OSS示例：使用专用的上传调速器，优化分片上传API调用频率
	err = w.f.uploadPacer.Call(func() (bool, error) {
		// 🔧 获取分片大小
		currentChunkSize, err = reader.Seek(0, io.SeekEnd)
		if err != nil {
			return false, fmt.Errorf("获取分片%d大小失败: %w", ossPartNumber, err)
		}

		// 🔧 重置读取位置
		_, err := reader.Seek(0, io.SeekStart)
		if err != nil {
			return false, fmt.Errorf("重置分片%d读取位置失败: %w", ossPartNumber, err)
		}

		// 🔧 参考阿里云OSS示例：创建UploadPart请求
		fs.Debugf(w.o, "🔧 开始上传分片%d到OSS: 大小=%v", ossPartNumber, fs.SizeSuffix(currentChunkSize))

		res, err = w.client.UploadPart(ctx, &oss.UploadPartRequest{
			Bucket:     oss.Ptr(*w.imur.Bucket),
			Key:        oss.Ptr(*w.imur.Key),
			UploadId:   w.imur.UploadId,
			PartNumber: ossPartNumber,
			Body:       reader,
			// 🔧 参考阿里云OSS示例：添加进度回调（虽然在分片级别，但可以提供更细粒度的反馈）
		})

		if err != nil {
			// 🔧 参考阿里云OSS示例：改进错误处理逻辑
			fs.Debugf(w.o, "❌ 分片%d上传失败: %v", ossPartNumber, err)

			if chunkNumber <= 8 {
				// 前几个分片使用智能重试策略
				shouldRetry, retryErr := w.shouldRetry(ctx, err)
				if shouldRetry {
					fs.Debugf(w.o, "🔄 分片%d将重试上传", ossPartNumber)
				}
				return shouldRetry, retryErr
			}
			// 后续分片使用简单重试策略
			return true, err
		}

		// 🔧 记录成功信息
		chunkDuration := time.Since(chunkStartTime)
		if currentChunkSize > 0 && chunkDuration.Seconds() > 0 {
			speed := float64(currentChunkSize) / chunkDuration.Seconds() / 1024 / 1024 // MB/s
			fs.Debugf(w.o, "✅ 分片%d OSS上传成功: 大小=%v, 用时=%v, 速度=%.2fMB/s, ETag=%s",
				ossPartNumber, fs.SizeSuffix(currentChunkSize),
				chunkDuration.Truncate(time.Millisecond), speed, *res.ETag)
		}

		return false, nil
	})

	if err != nil {
		// 🔧 参考阿里云OSS示例：提供详细的错误信息
		return -1, fmt.Errorf("分片%d上传失败 (大小:%v): %w", ossPartNumber, fs.SizeSuffix(currentChunkSize), err)
	}

	// 🔧 记录已完成的分片
	part := oss.UploadPart{
		PartNumber: ossPartNumber,
		ETag:       res.ETag,
	}
	w.addCompletedPart(part)

	// 🔧 最终成功日志
	totalDuration := time.Since(chunkStartTime)
	fs.Debugf(w.o, "✅ 分片%d完成: 大小=%v, 总用时=%v",
		ossPartNumber, fs.SizeSuffix(currentChunkSize), totalDuration.Truncate(time.Millisecond))

	return currentChunkSize, nil
}

// Abort the multipart upload
func (w *ossChunkWriter) Abort(ctx context.Context) (err error) {
	// 🔧 使用专用的上传调速器，优化分片上传中止API调用频率
	err = w.f.uploadPacer.Call(func() (bool, error) {
		_, err = w.client.AbortMultipartUpload(ctx, &oss.AbortMultipartUploadRequest{
			Bucket:   oss.Ptr(*w.imur.Bucket),
			Key:      oss.Ptr(*w.imur.Key),
			UploadId: w.imur.UploadId,
		})
		return w.shouldRetry(ctx, err)
	})
	if err != nil {
		return fmt.Errorf("failed to abort multipart upload %q: %w", *w.imur.UploadId, err)
	}
	// w.shutdownRenew()
	fs.Debugf(w.o, "multipart upload: %q aborted", *w.imur.UploadId)
	return
}

// Close 完成并确认分片上传
func (w *ossChunkWriter) Close(ctx context.Context) (err error) {
	// 单线程模式下分片已按顺序添加，但为保险起见仍进行排序
	sort.Slice(w.uploadedParts, func(i, j int) bool {
		return w.uploadedParts[i].PartNumber < w.uploadedParts[j].PartNumber
	})

	fs.Infof(w.o, "准备完成分片上传: 共 %d 个分片", len(w.uploadedParts))

	// 完成分片上传
	var res *oss.CompleteMultipartUploadResult
	req := &oss.CompleteMultipartUploadRequest{
		Bucket:   oss.Ptr(*w.imur.Bucket),
		Key:      oss.Ptr(*w.imur.Key),
		UploadId: w.imur.UploadId,
		CompleteMultipartUpload: &oss.CompleteMultipartUpload{
			Parts: w.uploadedParts,
		},
		Callback:    oss.Ptr(w.callback),
		CallbackVar: oss.Ptr(w.callbackVar),
	}

	// 🔧 使用专用的上传调速器，优化分片上传完成API调用频率
	err = w.f.uploadPacer.Call(func() (bool, error) {
		res, err = w.client.CompleteMultipartUpload(ctx, req)
		return w.shouldRetry(ctx, err)
	})
	if err != nil {
		return fmt.Errorf("完成分片上传失败: %w", err)
	}

	w.callbackRes = res.CallbackResult
	fs.Infof(w.o, "115网盘分片上传完成: %q", *w.imur.UploadId)
	return
}
