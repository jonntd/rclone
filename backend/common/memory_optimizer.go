// Package common provides memory optimization components for cloud storage backends
package common

import (
	"crypto/md5"
	"fmt"
	"hash"
	"io"
	"os"
	"sync"
	"time"
)

// MemoryOptimizer 内存优化器，提供流式处理和内存管理功能
type MemoryOptimizer struct {
	maxMemoryBuffer    int64 // 最大内存缓冲区大小
	currentMemoryUsage int64 // 当前内存使用量
	bufferPool         sync.Pool
	hasherPool         sync.Pool
	tempFilePool       sync.Pool
	streamPool         sync.Pool    // 流式处理器池
	mu                 sync.RWMutex // 保护内存使用统计
}

// NewMemoryOptimizer 创建内存优化器
func NewMemoryOptimizer(maxMemoryBuffer int64) *MemoryOptimizer {
	if maxMemoryBuffer <= 0 {
		maxMemoryBuffer = 50 * 1024 * 1024 // 默认50MB
	}

	return &MemoryOptimizer{
		maxMemoryBuffer:    maxMemoryBuffer,
		currentMemoryUsage: 0,
		bufferPool: sync.Pool{
			New: func() any {
				// 创建64KB缓冲区用于流式处理
				return make([]byte, 64*1024)
			},
		},
		hasherPool: sync.Pool{
			New: func() any {
				return md5.New()
			},
		},
		tempFilePool: sync.Pool{
			New: func() any {
				return &TempFileWrapper{}
			},
		},
		streamPool: sync.Pool{
			New: func() any {
				return &StreamProcessor{}
			},
		},
	}
}

// TempFileWrapper 临时文件包装器
type TempFileWrapper struct {
	file     *os.File
	path     string
	size     int64
	inUse    bool
	created  time.Time
	accessed time.Time
}

// StreamingHashCalculator 流式哈希计算器
type StreamingHashCalculator struct {
	hasher hash.Hash
	buffer []byte
	mo     *MemoryOptimizer
}

// NewStreamingHashCalculator 创建流式哈希计算器
func (mo *MemoryOptimizer) NewStreamingHashCalculator() *StreamingHashCalculator {
	hasher := mo.hasherPool.Get().(hash.Hash)
	hasher.Reset()
	buffer := mo.bufferPool.Get().([]byte)

	return &StreamingHashCalculator{
		hasher: hasher,
		buffer: buffer,
		mo:     mo,
	}
}

// CalculateHash 流式计算文件哈希，避免将整个文件加载到内存
func (shc *StreamingHashCalculator) CalculateHash(reader io.Reader) (string, error) {
	defer shc.Release()

	for {
		n, err := reader.Read(shc.buffer)
		if n > 0 {
			shc.hasher.Write(shc.buffer[:n])
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("读取数据失败: %w", err)
		}
	}

	return fmt.Sprintf("%x", shc.hasher.Sum(nil)), nil
}

// Release 释放资源
func (shc *StreamingHashCalculator) Release() {
	if shc.hasher != nil {
		shc.mo.hasherPool.Put(shc.hasher)
		shc.hasher = nil
	}
	if shc.buffer != nil {
		shc.mo.bufferPool.Put(shc.buffer)
		shc.buffer = nil
	}
}

// StreamingCopier 流式复制器，支持进度跟踪和哈希计算
type StreamingCopier struct {
	buffer   []byte
	hasher   hash.Hash
	mo       *MemoryOptimizer
	progress func(written int64)
}

// NewStreamingCopier 创建流式复制器
func (mo *MemoryOptimizer) NewStreamingCopier(calculateHash bool, progressCallback func(int64)) *StreamingCopier {
	buffer := mo.bufferPool.Get().([]byte)

	var hasher hash.Hash
	if calculateHash {
		hasher = mo.hasherPool.Get().(hash.Hash)
		hasher.Reset()
	}

	return &StreamingCopier{
		buffer:   buffer,
		hasher:   hasher,
		mo:       mo,
		progress: progressCallback,
	}
}

// CopyWithHash 流式复制数据并可选计算哈希
func (sc *StreamingCopier) CopyWithHash(dst io.Writer, src io.Reader) (written int64, hash string, err error) {
	defer sc.Release()

	for {
		nr, er := src.Read(sc.buffer)
		if nr > 0 {
			data := sc.buffer[:nr]

			// 写入目标
			nw, ew := dst.Write(data)
			if nw < 0 || nr < nw {
				nw = 0
				if ew == nil {
					ew = fmt.Errorf("invalid write result")
				}
			}
			written += int64(nw)

			// 更新哈希
			if sc.hasher != nil {
				sc.hasher.Write(data[:nw])
			}

			// 更新进度
			if sc.progress != nil {
				sc.progress(written)
			}

			if ew != nil {
				err = ew
				break
			}
			if nr != nw {
				err = io.ErrShortWrite
				break
			}
		}
		if er != nil {
			if er != io.EOF {
				err = er
			}
			break
		}
	}

	if sc.hasher != nil {
		hash = fmt.Sprintf("%x", sc.hasher.Sum(nil))
	}

	return written, hash, err
}

// Release 释放资源
func (sc *StreamingCopier) Release() {
	if sc.buffer != nil {
		sc.mo.bufferPool.Put(sc.buffer)
		sc.buffer = nil
	}
	if sc.hasher != nil {
		sc.mo.hasherPool.Put(sc.hasher)
		sc.hasher = nil
	}
}

// OptimizedTempFile 优化的临时文件管理
func (mo *MemoryOptimizer) OptimizedTempFile(prefix string, expectedSize int64) (*TempFileWrapper, error) {
	wrapper := mo.tempFilePool.Get().(*TempFileWrapper)

	// 创建临时文件
	file, err := os.CreateTemp("", prefix+"*")
	if err != nil {
		mo.tempFilePool.Put(wrapper)
		return nil, fmt.Errorf("创建临时文件失败: %w", err)
	}

	wrapper.file = file
	wrapper.path = file.Name()
	wrapper.size = 0
	wrapper.inUse = true
	wrapper.created = time.Now()
	wrapper.accessed = time.Now()

	// 对于大文件，预分配磁盘空间
	if expectedSize > 100*1024*1024 {
		if err := file.Truncate(expectedSize); err != nil {
			file.Close()
			os.Remove(file.Name())
			mo.tempFilePool.Put(wrapper)
			return nil, fmt.Errorf("预分配磁盘空间失败: %w", err)
		}
		// 重置文件指针
		file.Seek(0, 0)
	}

	return wrapper, nil
}

// Close 关闭临时文件
func (tfw *TempFileWrapper) Close() error {
	if tfw.file != nil {
		err := tfw.file.Close()
		tfw.file = nil
		return err
	}
	return nil
}

// Remove 删除临时文件
func (tfw *TempFileWrapper) Remove() error {
	tfw.Close()
	if tfw.path != "" {
		err := os.Remove(tfw.path)
		tfw.path = ""
		return err
	}
	return nil
}

// File 获取底层文件对象
func (tfw *TempFileWrapper) File() *os.File {
	tfw.accessed = time.Now()
	return tfw.file
}

// ReleaseTempFile 释放临时文件资源
func (mo *MemoryOptimizer) ReleaseTempFile(wrapper *TempFileWrapper) {
	if wrapper != nil {
		wrapper.Remove()
		wrapper.inUse = false
		mo.tempFilePool.Put(wrapper)
	}
}

// ShouldUseMemoryBuffer 判断是否应该使用内存缓冲
func (mo *MemoryOptimizer) ShouldUseMemoryBuffer(size int64) bool {
	mo.mu.RLock()
	defer mo.mu.RUnlock()

	// 检查当前内存使用量和请求大小
	if mo.currentMemoryUsage+size > mo.maxMemoryBuffer {
		return false
	}

	return size <= mo.maxMemoryBuffer
}

// AllocateMemory 分配内存并跟踪使用量
func (mo *MemoryOptimizer) AllocateMemory(size int64) bool {
	mo.mu.Lock()
	defer mo.mu.Unlock()

	if mo.currentMemoryUsage+size > mo.maxMemoryBuffer {
		return false
	}

	mo.currentMemoryUsage += size
	return true
}

// ReleaseMemory 释放内存并更新使用量
func (mo *MemoryOptimizer) ReleaseMemory(size int64) {
	mo.mu.Lock()
	defer mo.mu.Unlock()

	mo.currentMemoryUsage -= size
	if mo.currentMemoryUsage < 0 {
		mo.currentMemoryUsage = 0
	}
}

// GetMemoryUsage 获取当前内存使用情况
func (mo *MemoryOptimizer) GetMemoryUsage() (current, max int64) {
	mo.mu.RLock()
	defer mo.mu.RUnlock()
	return mo.currentMemoryUsage, mo.maxMemoryBuffer
}

// StreamProcessor 流式处理器，用于大文件的流式处理
type StreamProcessor struct {
	buffer     []byte
	hasher     hash.Hash
	totalRead  int64
	totalWrite int64
	inUse      bool
}

// NewStreamProcessor 创建流式处理器
func (mo *MemoryOptimizer) NewStreamProcessor() *StreamProcessor {
	sp := mo.streamPool.Get().(*StreamProcessor)
	sp.inUse = true
	sp.totalRead = 0
	sp.totalWrite = 0

	if sp.buffer == nil {
		sp.buffer = mo.bufferPool.Get().([]byte)
	}
	if sp.hasher == nil {
		sp.hasher = mo.hasherPool.Get().(hash.Hash)
	}
	sp.hasher.Reset()

	return sp
}

// ReleaseStreamProcessor 释放流式处理器
func (mo *MemoryOptimizer) ReleaseStreamProcessor(sp *StreamProcessor) {
	if sp != nil && sp.inUse {
		if sp.buffer != nil {
			mo.bufferPool.Put(sp.buffer)
			sp.buffer = nil
		}
		if sp.hasher != nil {
			mo.hasherPool.Put(sp.hasher)
			sp.hasher = nil
		}
		sp.inUse = false
		mo.streamPool.Put(sp)
	}
}

// StreamCopy 流式复制数据，同时计算哈希
func (sp *StreamProcessor) StreamCopy(dst io.Writer, src io.Reader) (int64, string, error) {
	if !sp.inUse {
		return 0, "", fmt.Errorf("stream processor not in use")
	}

	var totalCopied int64

	for {
		n, err := src.Read(sp.buffer)
		if n > 0 {
			// 写入目标
			written, writeErr := dst.Write(sp.buffer[:n])
			if writeErr != nil {
				return totalCopied, "", writeErr
			}

			// 更新哈希
			sp.hasher.Write(sp.buffer[:n])

			totalCopied += int64(written)
			sp.totalRead += int64(n)
			sp.totalWrite += int64(written)
		}

		if err == io.EOF {
			break
		}
		if err != nil {
			return totalCopied, "", err
		}
	}

	// 获取最终哈希值
	hashSum := fmt.Sprintf("%x", sp.hasher.Sum(nil))
	return totalCopied, hashSum, nil
}

// OptimizedReader 优化的读取器，自动选择内存或文件缓冲
type OptimizedReader struct {
	reader   io.Reader
	size     int64
	isMemory bool
	tempFile *TempFileWrapper
	mo       *MemoryOptimizer
}

// NewOptimizedReader 创建优化的读取器
func (mo *MemoryOptimizer) NewOptimizedReader(src io.Reader, size int64) (*OptimizedReader, error) {
	or := &OptimizedReader{
		size: size,
		mo:   mo,
	}

	if mo.ShouldUseMemoryBuffer(size) {
		// 小文件使用内存缓冲
		data, err := io.ReadAll(src)
		if err != nil {
			return nil, fmt.Errorf("读取到内存失败: %w", err)
		}
		or.reader = io.NopCloser(io.NewSectionReader(
			&readerAt{data: data}, 0, int64(len(data))))
		or.isMemory = true
	} else {
		// 大文件使用临时文件
		tempFile, err := mo.OptimizedTempFile("optimized_reader_", size)
		if err != nil {
			return nil, fmt.Errorf("创建临时文件失败: %w", err)
		}

		// 复制数据到临时文件
		_, err = io.Copy(tempFile.File(), src)
		if err != nil {
			mo.ReleaseTempFile(tempFile)
			return nil, fmt.Errorf("写入临时文件失败: %w", err)
		}

		// 重置文件指针
		tempFile.File().Seek(0, 0)
		or.reader = tempFile.File()
		or.tempFile = tempFile
		or.isMemory = false
	}

	return or, nil
}

// Read 读取数据
func (or *OptimizedReader) Read(p []byte) (n int, err error) {
	return or.reader.Read(p)
}

// Close 关闭读取器
func (or *OptimizedReader) Close() error {
	if or.tempFile != nil {
		or.mo.ReleaseTempFile(or.tempFile)
		or.tempFile = nil
	}
	if closer, ok := or.reader.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

// readerAt 实现 io.ReaderAt 接口
type readerAt struct {
	data []byte
}

func (r *readerAt) ReadAt(p []byte, off int64) (n int, err error) {
	if off >= int64(len(r.data)) {
		return 0, io.EOF
	}
	n = copy(p, r.data[off:])
	if n < len(p) {
		err = io.EOF
	}
	return n, err
}
