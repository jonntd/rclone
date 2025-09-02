#!/bin/bash

# Fix cgofuse v1.6.0 compatibility with Ubuntu 24.04 FUSE3
set -e

echo "🔧 Fixing cgofuse v1.6.0 compatibility with Ubuntu 24.04..."

GOARCH=$(go env GOARCH)
GOOS=$(go env GOOS)
echo "🏗️  Target: $GOOS/$GOARCH"

GOMODCACHE=$(go env GOMODCACHE)
CGOFUSE_DIR="$GOMODCACHE/github.com/winfsp/cgofuse@v1.6.0"

if [ ! -d "$CGOFUSE_DIR" ]; then
    echo "❌ cgofuse not found: $CGOFUSE_DIR"
    exit 1
fi

echo "📁 Found cgofuse at: $CGOFUSE_DIR"

# Create Ubuntu 24.04 compatible FUSE wrapper
echo "🔧 Creating Ubuntu 24.04 FUSE3 compatibility layer..."

cat > "$CGOFUSE_DIR/fuse/host_ubuntu24.go" << 'EOF'
//go:build linux && cgo

package fuse

/*
#cgo pkg-config: fuse3
#include <errno.h>
#include <fuse.h>

// Ubuntu 24.04 FUSE3 compatibility wrapper
struct fuse *get_fuse_context_fuse() {
    struct fuse_context *ctx = fuse_get_context();
    return ctx ? ctx->fuse : NULL;
}

// Define errno constants for cgofuse compatibility
int get_E2BIG() { return E2BIG; }
int get_EACCES() { return EACCES; }
int get_EADDRINUSE() { return EADDRINUSE; }
int get_EADDRNOTAVAIL() { return EADDRNOTAVAIL; }
int get_EAFNOSUPPORT() { return EAFNOSUPPORT; }
int get_EAGAIN() { return EAGAIN; }
int get_EALREADY() { return EALREADY; }
int get_EBADF() { return EBADF; }
int get_EBADMSG() { return EBADMSG; }
int get_EBUSY() { return EBUSY; }
int get_ECHILD() { return ECHILD; }
int get_EDEADLK() { return EDEADLK; }
int get_EDOM() { return EDOM; }
int get_EEXIST() { return EEXIST; }
int get_EFAULT() { return EFAULT; }
int get_EFBIG() { return EFBIG; }
int get_EINTR() { return EINTR; }
int get_EINVAL() { return EINVAL; }
int get_EIO() { return EIO; }
int get_EISDIR() { return EISDIR; }
int get_EMFILE() { return EMFILE; }
int get_EMLINK() { return EMLINK; }
int get_ENAMETOOLONG() { return ENAMETOOLONG; }
int get_ENFILE() { return ENFILE; }
int get_ENODEV() { return ENODEV; }
int get_ENOENT() { return ENOENT; }
int get_ENOEXEC() { return ENOEXEC; }
int get_ENOLCK() { return ENOLCK; }
int get_ENOMEM() { return ENOMEM; }
int get_ENOSPC() { return ENOSPC; }
int get_ENOSYS() { return ENOSYS; }
int get_ENOTDIR() { return ENOTDIR; }
int get_ENOTEMPTY() { return ENOTEMPTY; }
int get_ENOTTY() { return ENOTTY; }
int get_ENXIO() { return ENXIO; }
int get_EPERM() { return EPERM; }
int get_EPIPE() { return EPIPE; }
int get_ERANGE() { return ERANGE; }
int get_EROFS() { return EROFS; }
int get_ESPIPE() { return ESPIPE; }
int get_ESRCH() { return ESRCH; }
int get_EXDEV() { return EXDEV; }
*/
import "C"
import "unsafe"

// Define the missing types for cgofuse compatibility
type c_struct_fuse = C.struct_fuse

// Export errno constants
var (
    E2BIG         = int(C.get_E2BIG())
    EACCES        = int(C.get_EACCES())
    EADDRINUSE    = int(C.get_EADDRINUSE())
    EADDRNOTAVAIL = int(C.get_EADDRNOTAVAIL())
    EAFNOSUPPORT  = int(C.get_EAFNOSUPPORT())
    EAGAIN        = int(C.get_EAGAIN())
    EALREADY      = int(C.get_EALREADY())
    EBADF         = int(C.get_EBADF())
    EBADMSG       = int(C.get_EBADMSG())
    EBUSY         = int(C.get_EBUSY())
    ECHILD        = int(C.get_ECHILD())
    EDEADLK       = int(C.get_EDEADLK())
    EDOM          = int(C.get_EDOM())
    EEXIST        = int(C.get_EEXIST())
    EFAULT        = int(C.get_EFAULT())
    EFBIG         = int(C.get_EFBIG())
    EINTR         = int(C.get_EINTR())
    EINVAL        = int(C.get_EINVAL())
    EIO           = int(C.get_EIO())
    EISDIR        = int(C.get_EISDIR())
    EMFILE        = int(C.get_EMFILE())
    EMLINK        = int(C.get_EMLINK())
    ENAMETOOLONG  = int(C.get_ENAMETOOLONG())
    ENFILE        = int(C.get_ENFILE())
    ENODEV        = int(C.get_ENODEV())
    ENOENT        = int(C.get_ENOENT())
    ENOEXEC       = int(C.get_ENOEXEC())
    ENOLCK        = int(C.get_ENOLCK())
    ENOMEM        = int(C.get_ENOMEM())
    ENOSPC        = int(C.get_ENOSPC())
    ENOSYS        = int(C.get_ENOSYS())
    ENOTDIR       = int(C.get_ENOTDIR())
    ENOTEMPTY     = int(C.get_ENOTEMPTY())
    ENOTTY        = int(C.get_ENOTTY())
    ENXIO         = int(C.get_ENXIO())
    EPERM         = int(C.get_EPERM())
    EPIPE         = int(C.get_EPIPE())
    ERANGE        = int(C.get_ERANGE())
    EROFS         = int(C.get_EROFS())
    ESPIPE        = int(C.get_ESPIPE())
    ESRCH         = int(C.get_ESRCH())
    EXDEV         = int(C.get_EXDEV())
)

func hostGetfuse(host unsafe.Pointer) unsafe.Pointer {
    return unsafe.Pointer(C.get_fuse_context_fuse())
}
EOF

# Add build constraints to exclude original problematic files
if ! grep -q "//go:build !linux" "$CGOFUSE_DIR/fuse/host.go"; then
    sed -i '1i//go:build !linux\n' "$CGOFUSE_DIR/fuse/host.go"
    echo "✅ Added build constraint to host.go"
fi

if ! grep -q "//go:build !linux" "$CGOFUSE_DIR/fuse/errstr.go"; then
    sed -i '1i//go:build !linux\n' "$CGOFUSE_DIR/fuse/errstr.go"
    echo "✅ Added build constraint to errstr.go"
fi

echo "🎉 cgofuse Ubuntu 24.04 compatibility fix completed!"
