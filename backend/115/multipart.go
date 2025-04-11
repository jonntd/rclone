// Implements multipart uploading for 115 using OSS SDK v2.
package _115

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss"
	"github.com/cenkalti/backoff/v4"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/accounting"
	"github.com/rclone/rclone/lib/atexit"
	"github.com/rclone/rclone/lib/pool"
	"golang.org/x/sync/errgroup"
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

// getPool gets a buffer pool, initializing it on first use.
func getPool() *pool.Pool {
	bufferPoolOnce.Do(func() {
		ci := fs.GetConfig(context.Background())
		// Initialise the buffer pool when used
		bufferPool = pool.New(bufferCacheFlushTime, bufferSize, bufferCacheSize, ci.UseMmap)
	})
	return bufferPool
}

// NewRW gets a pool.RW using the multipart pool.
func NewRW() *pool.RW {
	return pool.NewRW(getPool())
}

// ossChunkWriter handles the state for OSS multipart uploads.
type ossChunkWriter struct {
	chunkSize     int64
	size          int64
	con           int // Concurrency (hardcoded to 1)
	f             *Fs
	o             *Object
	in            io.Reader // Potentially buffered input
	mu            sync.Mutex
	uploadedParts []oss.UploadPart // Store completed parts
	client        *oss.Client      // OSS SDK v2 client
	callback      string           // Base64 encoded callback info
	callbackVar   string           // Base64 encoded callback vars
	callbackRes   map[string]any   // Result from CompleteMultipartUpload callback
	imur          *oss.InitiateMultipartUploadResult
	cleanup       func() // Cleanup function for temp file or buffer
}

// Upload performs the multipart upload.
func (w *ossChunkWriter) Upload(ctx context.Context) (err error) {
	uploadCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Ensure cleanup runs if buffering was used
	if w.cleanup != nil {
		defer w.cleanup()
	}

	// Abort upload on error
	defer atexit.OnError(&err, func() {
		cancel() // Cancel any ongoing part uploads
		fs.Debugf(w.o, "OSS multipart upload error: %v. Aborting upload ID %s...", err, *w.imur.UploadId)
		abortCtx, abortCancel := context.WithTimeout(context.Background(), 30*time.Second) // Use background context for abort
		defer abortCancel()
		abortErr := w.Abort(abortCtx)
		if abortErr != nil {
			fs.Errorf(w.o, "Failed to abort OSS multipart upload %s: %v", *w.imur.UploadId, abortErr)
		} else {
			fs.Debugf(w.o, "Successfully aborted OSS multipart upload %s", *w.imur.UploadId)
		}
	})()

	var (
		g, gCtx   = errgroup.WithContext(uploadCtx)
		finished  = false
		off       int64
		size      = w.size
		chunkSize = w.chunkSize
	)

	// Unwrap accounting from the input reader
	in, acc := accounting.UnWrapAccounting(w.in)

	// Since concurrency is 1, we don't need the token dispenser.
	// Upload parts sequentially.
	for partNum := int64(0); !finished; partNum++ {
		// Check for cancellation before reading next chunk
		if gCtx.Err() != nil {
			fs.Debugf(w.o, "Context cancelled before reading chunk %d", partNum)
			break
		}

		// Get a buffer from the pool
		rw := NewRW()
		if acc != nil {
			rw.SetAccounting(acc.AccountRead) // Apply accounting to buffer reads
		}

		// Read the chunk into the buffer
		n, readErr := io.CopyN(rw, in, chunkSize)
		if readErr == io.EOF {
			if n == 0 && partNum != 0 { // End if no data read and not the first chunk
				_ = rw.Close() // Close the empty buffer
				break
			}
			finished = true // Mark as finished after reading the last chunk
		} else if readErr != nil {
			_ = rw.Close()
			return fmt.Errorf("multipart upload: failed to read source for chunk %d: %w", partNum, readErr)
		}

		// Prepare part upload in the sequential loop (concurrency=1)
		partNumber := partNum // Capture loop variable
		partOffset := off
		off += n

		fs.Debugf(w.o, "Uploading chunk %d (size %v, offset %v/%v)", partNumber, fs.SizeSuffix(n), fs.SizeSuffix(partOffset), fs.SizeSuffix(size))
		_, err = w.WriteChunk(gCtx, int32(partNumber), rw) // Use gCtx for cancellation
		_ = rw.Close()                                     // Close buffer after WriteChunk finishes or errors
		if err != nil {
			// Error occurred during WriteChunk, errgroup context will be cancelled
			fs.Errorf(w.o, "Failed to upload chunk %d: %v", partNumber, err)
			return err // Return error immediately
		}
	}

	// Wait for any potential lingering goroutines (shouldn't be any with concurrency=1)
	err = g.Wait()
	if err != nil {
		return err // Return error from chunk uploads
	}

	// Complete the multipart upload
	err = w.Close(ctx)
	if err != nil {
		return fmt.Errorf("multipart upload: failed to finalize: %w", err)
	}

	fs.Debugf(w.o, "OSS multipart upload completed successfully.")
	return nil
}

// addCompletedPart adds metadata about a successfully uploaded part.
func (w *ossChunkWriter) addCompletedPart(part oss.UploadPart) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.uploadedParts = append(w.uploadedParts, part)
}

// WriteChunk uploads a single chunk to OSS.
func (w *ossChunkWriter) WriteChunk(ctx context.Context, chunkNumber int32, reader io.ReadSeeker) (currentChunkSize int64, err error) {
	if chunkNumber < 0 {
		return -1, fmt.Errorf("invalid chunk number provided: %v", chunkNumber)
	}

	ossPartNumber := chunkNumber + 1 // OSS part numbers are 1-based

	// Determine chunk size
	currentChunkSize, err = reader.Seek(0, io.SeekEnd)
	if err != nil {
		return -1, fmt.Errorf("failed to seek to end of chunk %d reader: %w", ossPartNumber, err)
	}
	if currentChunkSize == 0 {
		fs.Debugf(w.o, "Skipping empty chunk %d", ossPartNumber)
		return 0, nil // Nothing to upload
	}
	_, err = reader.Seek(0, io.SeekStart) // Rewind reader
	if err != nil {
		return -1, fmt.Errorf("failed to seek to start of chunk %d reader: %w", ossPartNumber, err)
	}

	// Prepare UploadPart request
	req := &oss.UploadPartRequest{
		Bucket:     w.imur.Bucket,
		Key:        w.imur.Key,
		UploadId:   w.imur.UploadId,
		PartNumber: ossPartNumber,
		Body:       reader,
		// ContentLength is inferred by the SDK from the seeker
	}

	var res *oss.UploadPartResult
	// Retry logic handled by the global pacer wrapping the Upload function's loop
	err = w.f.globalPacer.Call(func() (bool, error) { // Pace each part upload
		var uploadErr error
		// Rewind reader before each attempt
		if _, seekErr := reader.Seek(0, io.SeekStart); seekErr != nil {
			return false, backoff.Permanent(fmt.Errorf("failed to rewind chunk %d reader for retry: %w", ossPartNumber, seekErr))
		}
		res, uploadErr = w.client.UploadPart(ctx, req)
		retry, retryErr := shouldRetry(ctx, nil, uploadErr) // Check if error is retryable
		if retry {
			fs.Debugf(w.o, "Retrying upload for chunk %d: %v", ossPartNumber, retryErr)
			return true, retryErr
		}
		if uploadErr != nil {
			return false, backoff.Permanent(uploadErr) // Non-retryable error
		}
		return false, nil // Success
	})

	if err != nil {
		return -1, fmt.Errorf("failed to upload chunk %d (size %v): %w", ossPartNumber, currentChunkSize, err)
	}

	if res == nil || res.ETag == nil {
		return -1, fmt.Errorf("upload chunk %d succeeded but response or ETag is nil", ossPartNumber)
	}

	// Add part to the list for completion
	w.addCompletedPart(oss.UploadPart{
		PartNumber: ossPartNumber,
		ETag:       res.ETag,
	})

	fs.Debugf(w.o, "Successfully uploaded chunk %d (ETag: %s)", ossPartNumber, *res.ETag)
	return currentChunkSize, nil
}

// Abort the multipart upload.
func (w *ossChunkWriter) Abort(ctx context.Context) (err error) {
	if w.imur == nil || w.imur.UploadId == nil {
		fs.Debugf(w.o, "Abort called but no valid upload ID found.")
		return nil // Nothing to abort
	}
	fs.Debugf(w.o, "Aborting OSS multipart upload %s", *w.imur.UploadId)
	req := &oss.AbortMultipartUploadRequest{
		Bucket:   w.imur.Bucket,
		Key:      w.imur.Key,
		UploadId: w.imur.UploadId,
	}
	// Retry abort on failure? Usually not critical but good practice.
	err = w.f.globalPacer.Call(func() (bool, error) {
		_, abortErr := w.client.AbortMultipartUpload(ctx, req)
		retry, retryErr := shouldRetry(ctx, nil, abortErr)
		if retry {
			return true, retryErr
		}
		if abortErr != nil {
			return false, backoff.Permanent(abortErr)
		}
		return false, nil
	})

	if err != nil {
		// Log error but don't necessarily fail the calling operation
		fs.Errorf(w.o, "Failed to abort multipart upload %q: %v", *w.imur.UploadId, err)
		return err // Return the error
	}
	fs.Debugf(w.o, "Successfully aborted multipart upload %q", *w.imur.UploadId)
	return nil
}

// Close finalizes the multipart upload by sending the CompleteMultipartUpload request.
func (w *ossChunkWriter) Close(ctx context.Context) (err error) {
	if w.imur == nil || w.imur.UploadId == nil {
		return errors.New("cannot complete multipart upload without a valid upload ID")
	}
	fs.Debugf(w.o, "Completing OSS multipart upload %s with %d parts", *w.imur.UploadId, len(w.uploadedParts))

	// Sort parts by PartNumber as required by OSS
	sort.Slice(w.uploadedParts, func(i, j int) bool {
		return w.uploadedParts[i].PartNumber < w.uploadedParts[j].PartNumber
	})

	req := &oss.CompleteMultipartUploadRequest{
		Bucket:   w.imur.Bucket,
		Key:      w.imur.Key,
		UploadId: w.imur.UploadId,
		CompleteMultipartUpload: &oss.CompleteMultipartUpload{
			Parts: w.uploadedParts,
		},
		Callback:    oss.Ptr(w.callback),    // Pass base64 encoded callback
		CallbackVar: oss.Ptr(w.callbackVar), // Pass base64 encoded callback vars
	}

	var res *oss.CompleteMultipartUploadResult
	err = w.f.globalPacer.Call(func() (bool, error) { // Pace the completion call
		var completeErr error
		res, completeErr = w.client.CompleteMultipartUpload(ctx, req)
		retry, retryErr := shouldRetry(ctx, nil, completeErr)
		if retry {
			return true, retryErr
		}
		if completeErr != nil {
			// Check for specific OSS error "CallbackFailed"
			var ossErr *oss.ServiceError
			if errors.As(completeErr, &ossErr) && ossErr.Code == "CallbackFailed" {
				// Log the callback failure details but treat upload as potentially successful
				fs.Errorf(w.o, "OSS CompleteMultipartUpload reported CallbackFailed: %v. Upload might be complete but callback processing failed.", ossErr)
				// Don't retry, but maybe don't return error? Depends on desired behavior.
				// Let's return the error for now.
				return false, backoff.Permanent(completeErr)
			}
			return false, backoff.Permanent(completeErr) // Non-retryable error
		}
		return false, nil // Success
	})

	if err != nil {
		return fmt.Errorf("failed to complete multipart upload %q: %w", *w.imur.UploadId, err)
	}

	if res == nil {
		return fmt.Errorf("complete multipart upload %q succeeded but response is nil", *w.imur.UploadId)
	}

	// Store the callback result for postUpload processing
	w.callbackRes = res.CallbackResult
	fs.Debugf(w.o, "Successfully completed multipart upload %q", *w.imur.UploadId)
	return nil
}
