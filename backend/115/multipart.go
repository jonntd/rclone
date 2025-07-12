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
	"github.com/rclone/rclone/fs/chunksize"
	"github.com/rclone/rclone/fs/fserrors"
	"github.com/rclone/rclone/lib/atexit"
	"github.com/rclone/rclone/lib/pool"
)

const (
	bufferSize           = 1024 * 1024     // default size of the pages used in the reader
	bufferCacheSize      = 64              // max number of buffers to keep in cache
	bufferCacheFlushTime = 5 * time.Second // flush the cached buffers after this long
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
// 采用顺序上传模式，确保上传稳定性和SHA1验证通过
func (w *ossChunkWriter) Upload(ctx context.Context) (err error) {
	uploadCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer atexit.OnError(&err, func() {
		cancel()
		fs.Debugf(w.o, "分片上传取消中...")
		errCancel := w.Abort(ctx)
		if errCancel != nil {
			fs.Debugf(w.o, "取消分片上传失败: %v", errCancel)
		}
	})()

	var (
		finished   = false
		off        int64
		size       = w.size
		chunkSize  = w.chunkSize
		partNum    int64
		totalParts int64
	)

	// 计算总分片数用于进度显示
	if size > 0 {
		totalParts = (size + chunkSize - 1) / chunkSize
	}

	// 手动处理accounting
	in, acc := accounting.UnWrapAccounting(w.in)

	// 🔧 用户要求：115网盘单线程多部分上传文件≥100MB，100MB块，中文代码注释，详细进度显示
	fs.Infof(w.o, "🚀 开始115网盘单线程分片上传: 文件大小 %v, 分片大小 %v, 预计分片数 %d",
		fs.SizeSuffix(size), fs.SizeSuffix(chunkSize), totalParts)

	for partNum = 0; !finished; partNum++ {
		// 获取内存缓冲区
		rw := NewRW()
		if acc != nil {
			rw.SetAccounting(acc.AccountRead)
		}

		// 检查上下文是否已取消
		if uploadCtx.Err() != nil {
			_ = rw.Close()
			break
		}

		// 读取分片数据
		var n int64
		n, err = io.CopyN(rw, in, chunkSize)
		if err == io.EOF {
			if n == 0 && partNum != 0 { // 如果不是第一个分片且没有数据，则结束
				_ = rw.Close()
				break
			}
			finished = true
		} else if err != nil {
			_ = rw.Close()
			return fmt.Errorf("读取分片数据失败: %w", err)
		}

		// 🔧 用户要求：显示"分片5/15"格式的详细进度，提升到Info级别
		currentPart := partNum + 1
		if totalParts > 0 {
			fs.Infof(w.o, "📤 115网盘单线程上传: 分片%d/%d (%.1f%%) | %v | 偏移: %v",
				currentPart, totalParts,
				float64(currentPart)/float64(totalParts)*100,
				fs.SizeSuffix(n), fs.SizeSuffix(off))
		} else {
			fs.Infof(w.o, "📤 115网盘单线程上传: 分片%d | %v | 偏移: %v",
				currentPart, fs.SizeSuffix(n), fs.SizeSuffix(off))
		}

		// 上传当前分片
		_, err = w.WriteChunk(uploadCtx, int32(partNum), rw)
		_ = rw.Close() // 释放内存缓冲区

		if err != nil {
			return fmt.Errorf("上传分片 %d 失败: %w", currentPart, err)
		}

		off += n
	}

	// 完成分片上传
	err = w.Close(ctx)
	if err != nil {
		return fmt.Errorf("完成分片上传失败: %w", err)
	}

	return nil
}

var warnStreamUpload sync.Once

// ossChunkWriter 115网盘分片上传写入器
// 采用单线程顺序上传模式
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
}

func (f *Fs) newChunkWriter(ctx context.Context, src fs.ObjectInfo, ui *api.UploadInitInfo, in io.Reader, o *Object, options ...fs.OpenOption) (w *ossChunkWriter, err error) {
	uploadParts := min(max(1, f.opt.MaxUploadParts), maxUploadParts)
	size := src.Size()

	// 🔧 用户要求：115网盘使用100MB分片大小
	// chunkSize := fs.SizeSuffix(100 * 1024 * 1024) // 100MB分片
	chunkSize := f.opt.ChunkSize

	// 处理未知文件大小的情况（流式上传）
	if size == -1 {
		warnStreamUpload.Do(func() {
			fs.Logf(f, "流式上传使用分片大小 %v，最大文件大小限制为 %v",
				f.opt.ChunkSize, fs.SizeSuffix(int64(chunkSize)*int64(uploadParts)))
		})
	} else {
		chunkSize = chunksize.Calculator(src, size, uploadParts, chunkSize)
	}

	// 115网盘采用单线程分片上传模式，确保稳定性
	fs.Debugf(f, "115网盘分片上传: 文件大小 %v, 分片大小 %v", fs.SizeSuffix(size), fs.SizeSuffix(int64(chunkSize)))

	w = &ossChunkWriter{
		chunkSize: int64(chunkSize),
		size:      size,
		f:         f,
		o:         o,
		in:        in,
	}

	var client *oss.Client
	client, err = f.newOSSClient()
	if err != nil {
		return nil, fmt.Errorf("failed to create OSS client: %w", err)
	}
	w.client = client

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
func (w *ossChunkWriter) WriteChunk(ctx context.Context, chunkNumber int32, reader io.ReadSeeker) (currentChunkSize int64, err error) {
	if chunkNumber < 0 {
		err := fmt.Errorf("invalid chunk number provided: %v", chunkNumber)
		return -1, err
	}

	ossPartNumber := chunkNumber + 1
	var res *oss.UploadPartResult
	// 🔧 使用专用的上传调速器，优化分片上传API调用频率
	err = w.f.uploadPacer.Call(func() (bool, error) {
		// Discover the size by seeking to the end
		currentChunkSize, err = reader.Seek(0, io.SeekEnd)
		if err != nil {
			return false, err
		}
		// rewind the reader on retry and after reading md5
		_, err := reader.Seek(0, io.SeekStart)
		if err != nil {
			return false, err
		}
		res, err = w.client.UploadPart(ctx, &oss.UploadPartRequest{
			Bucket:     oss.Ptr(*w.imur.Bucket),
			Key:        oss.Ptr(*w.imur.Key),
			UploadId:   w.imur.UploadId,
			PartNumber: ossPartNumber,
			Body:       reader,
		})
		if err != nil {
			if chunkNumber <= 8 {
				return w.shouldRetry(ctx, err)
			}
			// retry all chunks once have done the first few
			return true, err
		}
		return false, nil
	})
	if err != nil {
		return -1, fmt.Errorf("failed to upload chunk %d with %v bytes: %w", ossPartNumber, currentChunkSize, err)
	}

	part := oss.UploadPart{
		PartNumber: ossPartNumber,
		ETag:       res.ETag,
	}
	w.addCompletedPart(part)

	fs.Debugf(w.o, "分片 %d 上传完成: 大小 %v", ossPartNumber, fs.SizeSuffix(currentChunkSize))
	return currentChunkSize, err
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
