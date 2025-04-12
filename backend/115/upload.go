package _115

import (
	"bytes"
	"context" // Keep for traditional token generation
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss"
	"github.com/cenkalti/backoff/v4"

	// "github.com/pierrec/lz4/v4" // Keep commented out unless lz4 errors reappear
	"github.com/rclone/rclone/backend/115/api" // Keep for traditional initUpload
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/accounting"
	"github.com/rclone/rclone/fs/fserrors"
	"github.com/rclone/rclone/fs/hash"
)

// Globals
const (
	cachePrefix  = "rclone-115-sha1sum-"
	md5Salt      = "Qclm8MGWUv59TnrR0XPg"     // Salt for traditional token generation
	OSSRegion    = "cn-shenzhen"              // Default OSS region
	OSSUserAgent = "aliyun-sdk-android/2.9.1" // Keep or update as needed
)

// RereadableSource represents a source that can be read multiple times
type RereadableSource struct {
	src     fs.ObjectInfo
	ctx     context.Context
	options []fs.OpenOption
	current io.ReadCloser
}

// NewRereadableSource creates a source that can be read multiple times
func NewRereadableSource(ctx context.Context, src fs.ObjectInfo, options ...fs.OpenOption) *RereadableSource {
	rs := &RereadableSource{
		src:     src,
		ctx:     ctx,
		options: options,
	}

	return rs
}

// Read implements io.Reader
func (r *RereadableSource) Read(p []byte) (int, error) {
	// Open on first read if not already open
	if r.current == nil {
		err := r.Reopen()
		if err != nil {
			return 0, err
		}
	}
	return r.current.Read(p)
}

// Reopen closes any existing reader and opens a new one
func (r *RereadableSource) Reopen() error {
	// Close existing reader if any
	if r.current != nil {
		_ = r.current.Close() // Ignore close errors
		r.current = nil
	}

	// Get real object if possible
	obj := unWrapObjectInfo(r.src)
	if obj == nil {
		return errors.New("source doesn't support reopening (not an fs.Object)")
	}

	// Open new reader
	reader, err := obj.Open(r.ctx, r.options...)
	if err != nil {
		return fmt.Errorf("failed to reopen source object: %w", err)
	}
	r.current = reader
	return nil
}

// Close implements io.Closer
func (r *RereadableSource) Close() error {
	if r.current != nil {
		err := r.current.Close()
		r.current = nil
		return err
	}
	return nil
}

// RangeHash calculates SHA1 hash for a byte range
func (r *RereadableSource) RangeHash(start, end int64) (string, error) {
	// Close existing reader
	if r.current != nil {
		_ = r.current.Close()
		r.current = nil
	}

	// Get real object
	obj := unWrapObjectInfo(r.src)
	if obj == nil {
		return "", errors.New("source doesn't support range hashing")
	}

	// Open with range option
	rc, err := obj.Open(r.ctx, &fs.RangeOption{Start: start, End: end})
	if err != nil {
		return "", fmt.Errorf("failed to open range: %w", err)
	}
	defer rc.Close()

	// Calculate hash
	hashVal := sha1.New()
	if _, err := io.Copy(hashVal, rc); err != nil {
		return "", fmt.Errorf("failed to read range data: %w", err)
	}

	return strings.ToUpper(hex.EncodeToString(hashVal.Sum(nil))), nil
}

// calculateSHA1 calculates SHA1 hash using either buffering or direct reads
func calculateSHA1(f *Fs, in io.Reader, src fs.ObjectInfo, size int64, ctx context.Context, options ...fs.OpenOption) (sha1sum string, out io.Reader, cleanup func(), err error) {
	cleanup = func() {} // Default no-op cleanup

	// First check if source already has the hash
	if obj, ok := src.(fs.Object); ok {
		hash, hashErr := obj.Hash(ctx, hash.SHA1)
		if hashErr == nil && hash != "" {
			fs.Debugf(f, "Using source-provided SHA1: %s", hash)
			return hash, in, cleanup, nil
		}
	}

	// Handle no-buffer mode
	if f.opt.NoBuffer {
		fs.Debugf(f, "Computing SHA1 without buffering (no_buffer mode)")

		// Create a rereadable source if needed
		var rs *RereadableSource
		if source, ok := in.(*RereadableSource); ok {
			rs = source
			// Reset to beginning
			if err := rs.Reopen(); err != nil {
				return "", in, cleanup, fmt.Errorf("failed to reopen source: %w", err)
			}
		} else {
			rs = NewRereadableSource(ctx, src, options...)
		}

		// Calculate hash
		hashVal := sha1.New()
		_, hashErr := io.Copy(hashVal, rs)
		if hashErr != nil {
			return "", in, cleanup, fmt.Errorf("hash calculation failed: %w", hashErr)
		}

		// Reset for actual upload
		if err := rs.Reopen(); err != nil {
			return "", in, cleanup, fmt.Errorf("failed to reopen source after hash: %w", err)
		}

		return hex.EncodeToString(hashVal.Sum(nil)), rs, func() { rs.Close() }, nil
	}

	// Standard buffering approach
	threshold := int64(f.opt.HashMemoryThreshold)

	// Buffer in memory if small enough
	if size >= 0 && size <= threshold {
		fs.Debugf(f, "Buffering input in memory for SHA1 calculation (size: %v)", fs.SizeSuffix(size))
		buf, err := io.ReadAll(in)
		if err != nil {
			return "", nil, cleanup, fmt.Errorf("failed to read input to memory: %w", err)
		}

		// Calculate hash
		hashVal := sha1.New()
		hashVal.Write(buf)
		return hex.EncodeToString(hashVal.Sum(nil)), bytes.NewReader(buf), cleanup, nil
	}

	// Buffer to disk for larger files
	fs.Debugf(f, "Buffering input to disk for SHA1 calculation (size: %v)", fs.SizeSuffix(size))
	tempFile, err := os.CreateTemp("", cachePrefix)
	if err != nil {
		return "", nil, cleanup, fmt.Errorf("failed to create temp file: %w", err)
	}

	// Set up cleanup function
	cleanup = func() {
		_ = tempFile.Close()
		_ = os.Remove(tempFile.Name())
		fs.Debugf(f, "Cleaned up temp file: %s", tempFile.Name())
	}

	// Calculate hash while copying to temp file
	hashVal := sha1.New()
	writer := io.MultiWriter(tempFile, hashVal)

	_, err = io.Copy(writer, in)
	if err != nil {
		cleanup()
		return "", nil, func() {}, fmt.Errorf("failed to copy to temp file: %w", err)
	}

	// Reset file position
	_, err = tempFile.Seek(0, io.SeekStart)
	if err != nil {
		cleanup()
		return "", nil, func() {}, fmt.Errorf("failed to seek temp file: %w", err)
	}

	return hex.EncodeToString(hashVal.Sum(nil)), tempFile, cleanup, nil
}

// tryHashUpload attempts quick upload (秒传) using the file's hash
func (f *Fs) tryHashUpload(
	ctx context.Context,
	in io.Reader,
	src fs.ObjectInfo,
	o *Object,
	leaf, dirID string,
	size int64,
	options ...fs.OpenOption,
) (found bool, ui *api.UploadInitInfo, newIn io.Reader, cleanup func(), err error) {
	fs.Debugf(o, "Attempting hash upload (秒传)")
	cleanup = func() {}
	newIn = in

	// Calculate SHA1 hash (or get it from source)
	sha1sum, bufferedIn, localCleanup, err := calculateSHA1(f, in, src, size, ctx, options...)
	if err != nil {
		return false, nil, in, cleanup, fmt.Errorf("hash calculation failed: %w", err)
	}

	cleanup = localCleanup
	newIn = bufferedIn
	o.sha1sum = strings.ToLower(sha1sum)

	// Try quick upload using the hash
	ui, err = f.initUploadOpenAPI(ctx, size, leaf, dirID, sha1sum, "", "", "", "")
	if err != nil {
		return false, nil, newIn, cleanup, fmt.Errorf("hash upload check failed: %w", err)
	}

	// Process response status
	for {
		status := ui.GetStatus()
		switch status {
		case 2: // Hash upload successful
			fs.Debugf(o, "Hash upload successful")
			o.id = ui.GetFileID()
			o.pickCode = ui.GetPickCode()
			o.hasMetaData = true

			// Mark as server side copy for accounting
			reader, _ := accounting.UnWrap(newIn)
			if acc, ok := reader.(*accounting.Account); ok && acc != nil {
				acc.ServerSideTransferStart()
				acc.ServerSideCopyEnd(size)
			}

			return true, ui, newIn, cleanup, nil

		case 1: // Normal upload needed
			fs.Debugf(o, "Hash upload not available, proceeding with normal upload")
			return false, ui, newIn, cleanup, nil

		case 7: // Secondary hash check required
			fs.Debugf(o, "Hash upload requires secondary auth")
			signKey := ui.GetSignKey()
			signCheckRange := ui.GetSignCheck()

			if signKey == "" || signCheckRange == "" {
				return false, nil, newIn, cleanup, errors.New("missing sign_key or sign_check")
			}

			// Parse range
			var start, end int64
			if n, scanErr := fmt.Sscanf(signCheckRange, "%d-%d", &start, &end); scanErr != nil || n != 2 {
				return false, nil, newIn, cleanup, fmt.Errorf("invalid range format: %q", signCheckRange)
			}

			// Calculate range hash
			var signVal string
			if rs, ok := newIn.(*RereadableSource); ok && f.opt.NoBuffer {
				// Use range hash from RereadableSource
				signVal, err = rs.RangeHash(start, end)
			} else if seeker, ok := newIn.(io.Seeker); ok {
				// Use seeking for buffered input
				origPos, _ := seeker.Seek(0, io.SeekCurrent)
				_, err = seeker.Seek(start, io.SeekStart)
				if err != nil {
					return false, nil, newIn, cleanup, fmt.Errorf("seek failed: %w", err)
				}

				// Calculate hash
				hashVal := sha1.New()
				length := end - start + 1
				_, err = io.Copy(hashVal, io.LimitReader(newIn, length))

				// Restore position
				_, _ = seeker.Seek(origPos, io.SeekStart)
				if err != nil {
					return false, nil, newIn, cleanup, fmt.Errorf("range hash calculation failed: %w", err)
				}

				signVal = strings.ToUpper(hex.EncodeToString(hashVal.Sum(nil)))
			} else {
				return false, nil, newIn, cleanup, errors.New("input doesn't support seeking for range hash")
			}

			fs.Debugf(o, "Calculated range SHA1: %s for range %s", signVal, signCheckRange)

			// Retry with range hash
			ui, err = f.initUploadOpenAPI(ctx, size, leaf, dirID, sha1sum, "", "", signKey, signVal)
			if err != nil {
				return false, nil, newIn, cleanup, fmt.Errorf("secondary hash check failed: %w", err)
			}
			continue // Check new status

		default:
			return false, nil, newIn, cleanup, fmt.Errorf("unexpected status: %d, message: %s", status, ui.ErrMsg())
		}
	}
}

// upload is the main entry point for uploading files
func (f *Fs) upload(ctx context.Context, in io.Reader, src fs.ObjectInfo, remote string, options ...fs.OpenOption) (fs.Object, error) {
	if f.isShare {
		return nil, errors.New("upload not supported for shared filesystem")
	}

	// Start token renewer if available
	if f.tokenRenewer != nil {
		f.tokenRenewer.Start()
		defer f.tokenRenewer.Stop()
	}

	size := src.Size()

	// Convert to RereadableSource if no_buffer is enabled
	if f.opt.NoBuffer && size >= 0 {
		fs.Logf(src, "Using no_buffer mode: file will be read multiple times instead of buffered")
		if _, ok := in.(*RereadableSource); !ok {
			rereadable := NewRereadableSource(ctx, src, options...)
			in = rereadable
		}
	}

	// Ensure we have required user credentials
	if f.userID == "" {
		if err := f.getUploadBasicInfo(ctx); err != nil {
			return nil, fmt.Errorf("failed to get userID: %w", err)
		}
	}

	// Check size limits
	if size > int64(maxUploadSize) {
		return nil, fmt.Errorf("file size %v exceeds upload limit %v", fs.SizeSuffix(size), fs.SizeSuffix(maxUploadSize))
	}

	// Create placeholder object and ensure parent directory exists
	o, leaf, dirID, err := f.createObject(ctx, remote, src.ModTime(ctx), size)
	if err != nil {
		return nil, fmt.Errorf("failed to create object placeholder: %w", err)
	}

	// Resource cleanup
	var cleanup func()
	defer func() {
		if cleanup != nil {
			cleanup()
		}
	}()

	// === Upload Strategy Selection ===

	// 1. For OnlyStream: Use traditional upload for small files
	if f.opt.OnlyStream {
		if size < 0 || size <= int64(StreamUploadLimit) {
			return f.doSampleUpload(ctx, in, o, leaf, dirID, size, options...)
		}
		return nil, fserrors.NoRetryError(fmt.Errorf("only_stream enabled but file exceeds limit (%v > %v)",
			fs.SizeSuffix(size), fs.SizeSuffix(StreamUploadLimit)))
	}

	// 2. For FastUpload: Try hash upload, then sample or OSS based on size
	if f.opt.FastUpload {
		noHashSize := int64(f.opt.NohashSize)
		streamLimit := int64(StreamUploadLimit)

		// Small files go directly to sample upload
		if size >= 0 && size <= noHashSize {
			return f.doSampleUpload(ctx, in, o, leaf, dirID, size, options...)
		}

		// Try hash upload for known size files
		if size >= 0 {
			found, ui, newIn, localCleanup, err := f.tryHashUpload(ctx, in, src, o, leaf, dirID, size, options...)
			cleanup = localCleanup

			if err != nil {
				fs.Logf(o, "Hash upload failed, falling back: %v", err)
				if !f.opt.NoBuffer {
					// Revert to original input
					newIn = in
					if cleanup != nil {
						cleanup()
						cleanup = nil
					}
				}
			} else if found {
				return o, nil // Hash upload succeeded
			}

			// Fallback based on size
			if size <= streamLimit {
				return f.doSampleUpload(ctx, newIn, o, leaf, dirID, size, options...)
			}
			return f.uploadToOSS(ctx, newIn, src, o, leaf, dirID, size, ui, options...)
		} else {
			// Unknown size, try sample upload
			return f.doSampleUpload(ctx, in, o, leaf, dirID, size, options...)
		}
	}

	// 3. For UploadHashOnly: Only try hash upload and fail if not found
	if f.opt.UploadHashOnly {
		if size < 0 {
			return nil, fserrors.NoRetryError(errors.New("upload_hash_only requires known file size"))
		}

		found, _, _, localCleanup, err := f.tryHashUpload(ctx, in, src, o, leaf, dirID, size, options...)
		if localCleanup != nil {
			defer localCleanup()
		}

		if err != nil {
			return nil, fmt.Errorf("hash upload failed: %w", err)
		}
		if found {
			return o, nil
		}
		return nil, fserrors.NoRetryError(errors.New("file not found on server via hash check"))
	}

	// 4. Default strategy: Try hash upload if large enough, then OSS
	noHashSize := int64(f.opt.NohashSize)
	uploadCutoff := int64(f.opt.UploadCutoff)

	// Small files go directly to sample upload
	if size >= 0 && size < noHashSize {
		return f.doSampleUpload(ctx, in, o, leaf, dirID, size, options...)
	}

	// Try hash upload for known size files
	var ui *api.UploadInitInfo
	var newIn = in

	if size >= 0 {
		found, ui, newIn, localCleanup, err := f.tryHashUpload(ctx, in, src, o, leaf, dirID, size, options...)
		cleanup = localCleanup

		if err != nil {
			fs.Logf(o, "Hash upload failed, falling back: %v", err)
			if !f.opt.NoBuffer {
				// Revert to original input
				newIn = in
				if cleanup != nil {
					cleanup()
					cleanup = nil
				}
			}
		} else if found {
			return o, nil // Hash upload succeeded
		}
	}

	// Complete with OSS upload (single or multipart)
	if size < 0 || size >= uploadCutoff {
		return f.uploadToOSS(ctx, newIn, src, o, leaf, dirID, size, ui, options...)
	}

	// Use OSS PutObject for mid-sized files
	fs.Debugf(o, "Using OSS single-part upload for size %v", fs.SizeSuffix(size))

	// 1. Get UploadInitInfo if not already available (from failed hash check)
	if ui == nil {
		var initErr error
		ui, initErr = f.initUploadOpenAPI(ctx, size, leaf, dirID, o.sha1sum, "", "", "", "") // Provide hash if known
		if initErr != nil {
			return nil, fmt.Errorf("failed to initialize PutObject upload: %w", initErr)
		}
		if ui.GetStatus() != 1 { // Should be 1 if hash upload failed/skipped
			if ui.GetStatus() == 2 { // Unexpected 秒传
				fs.Logf(o, "Warning: initUpload for PutObject resulted in 秒传 (status 2), handling...")
				o.id = ui.GetFileID()
				o.pickCode = ui.GetPickCode()
				o.hasMetaData = true
				return o, nil
			}
			return nil, fmt.Errorf("expected status 1 from initUpload for PutObject, got %d: %s", ui.GetStatus(), ui.ErrMsg())
		}
	}

	// 2. Create OSS client
	ossClient, err := f.newOSSClient()
	if err != nil {
		return nil, fmt.Errorf("failed to create OSS client for PutObject: %w", err)
	}

	// 3. Prepare PutObject request
	req := &oss.PutObjectRequest{
		Bucket:      oss.Ptr(ui.GetBucket()),
		Key:         oss.Ptr(ui.GetObject()),
		Body:        newIn, // Use potentially buffered reader
		Callback:    oss.Ptr(ui.GetCallback()),
		CallbackVar: oss.Ptr(ui.GetCallbackVar()),
	}
	// Apply headers from options
	for _, option := range options {
		key, value := option.Header()
		lowerKey := strings.ToLower(key)
		switch lowerKey {
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

	// 4. Execute PutObject with retry via pacer
	var putRes *oss.PutObjectResult
	err = f.globalPacer.Call(func() (bool, error) {
		var putErr error
		putRes, putErr = ossClient.PutObject(ctx, req)
		retry, retryErr := shouldRetry(ctx, nil, putErr)
		if retry {
			// Rewind body if possible before retry
			if seeker, ok := newIn.(io.Seeker); ok {
				_, _ = seeker.Seek(0, io.SeekStart)
			} else {
				// Cannot retry non-seekable stream after partial read
				return false, backoff.Permanent(fmt.Errorf("cannot retry PutObject with non-seekable stream: %w", putErr))
			}
			return true, retryErr
		}
		if putErr != nil {
			return false, backoff.Permanent(putErr)
		}
		return false, nil // Success
	})
	if err != nil {
		return nil, fmt.Errorf("OSS PutObject failed: %w", err)
	}

	// 5. Process callback
	callbackData, err := f.postUpload(putRes.CallbackResult)
	if err != nil {
		return nil, fmt.Errorf("failed to process PutObject callback: %w", err)
	}

	// 6. Update metadata
	err = o.setMetaDataFromCallBack(callbackData)
	if err != nil {
		return nil, fmt.Errorf("failed to set metadata from PutObject callback: %w", err)
	}

	fs.Debugf(o, "OSS PutObject successful.")
	return o, nil
}
