#!/bin/bash

# Fix cgofuse cross-compilation issues
# This script patches cgofuse to work correctly with cross-compilation

set -e

echo "🔧 Fixing cgofuse cross-compilation issues..."

# Get Go module cache directory
GOMODCACHE=$(go env GOMODCACHE)
CGOFUSE_DIR="$GOMODCACHE/github.com/winfsp/cgofuse@v1.6.0"

if [ ! -d "$CGOFUSE_DIR" ]; then
    echo "❌ cgofuse module not found in cache: $CGOFUSE_DIR"
    exit 1
fi

echo "📁 Found cgofuse at: $CGOFUSE_DIR"

# Backup original files if not already backed up
if [ ! -f "$CGOFUSE_DIR/fuse/host.go.orig" ]; then
    echo "💾 Backing up original files..."
    cp "$CGOFUSE_DIR/fuse/host.go" "$CGOFUSE_DIR/fuse/host.go.orig"
    cp "$CGOFUSE_DIR/fuse/errstr.go" "$CGOFUSE_DIR/fuse/errstr.go.orig"
fi

# Create a fixed version of host.go for cross-compilation
cat > "$CGOFUSE_DIR/fuse/host_linux.go" << 'EOF'
//go:build linux && cgo

package fuse

/*
#include <errno.h>
#include <fcntl.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <unistd.h>

#define FUSE_USE_VERSION 28
#include <fuse.h>

// Wrapper functions to avoid CGO issues with cross-compilation
static struct fuse *get_fuse_from_context() {
    return fuse_get_context()->fuse;
}

static int get_errno_E2BIG() { return E2BIG; }
static int get_errno_EACCES() { return EACCES; }
static int get_errno_EAGAIN() { return EAGAIN; }
static int get_errno_EBADF() { return EBADF; }
static int get_errno_EBUSY() { return EBUSY; }
static int get_errno_ECHILD() { return ECHILD; }
static int get_errno_EDEADLK() { return EDEADLK; }
static int get_errno_EDOM() { return EDOM; }
static int get_errno_EEXIST() { return EEXIST; }
static int get_errno_EFAULT() { return EFAULT; }
static int get_errno_EFBIG() { return EFBIG; }
static int get_errno_EINTR() { return EINTR; }
static int get_errno_EINVAL() { return EINVAL; }
static int get_errno_EIO() { return EIO; }
static int get_errno_EISDIR() { return EISDIR; }
static int get_errno_EMFILE() { return EMFILE; }
static int get_errno_EMLINK() { return EMLINK; }
static int get_errno_ENAMETOOLONG() { return ENAMETOOLONG; }
static int get_errno_ENFILE() { return ENFILE; }
static int get_errno_ENODEV() { return ENODEV; }
static int get_errno_ENOENT() { return ENOENT; }
static int get_errno_ENOEXEC() { return ENOEXEC; }
static int get_errno_ENOLCK() { return ENOLCK; }
static int get_errno_ENOMEM() { return ENOMEM; }
static int get_errno_ENOSPC() { return ENOSPC; }
static int get_errno_ENOSYS() { return ENOSYS; }
static int get_errno_ENOTDIR() { return ENOTDIR; }
static int get_errno_ENOTEMPTY() { return ENOTEMPTY; }
static int get_errno_ENOTTY() { return ENOTTY; }
static int get_errno_ENXIO() { return ENXIO; }
static int get_errno_EPERM() { return EPERM; }
static int get_errno_EPIPE() { return EPIPE; }
static int get_errno_ERANGE() { return ERANGE; }
static int get_errno_EROFS() { return EROFS; }
static int get_errno_ESPIPE() { return ESPIPE; }
static int get_errno_ESRCH() { return ESRCH; }
static int get_errno_EXDEV() { return EXDEV; }
*/
import "C"

// Define the missing types and constants
type c_struct_fuse = C.struct_fuse

// Export errno constants through wrapper functions
var (
    E2BIG         = int(C.get_errno_E2BIG())
    EACCES        = int(C.get_errno_EACCES())
    EAGAIN        = int(C.get_errno_EAGAIN())
    EBADF         = int(C.get_errno_EBADF())
    EBUSY         = int(C.get_errno_EBUSY())
    ECHILD        = int(C.get_errno_ECHILD())
    EDEADLK       = int(C.get_errno_EDEADLK())
    EDOM          = int(C.get_errno_EDOM())
    EEXIST        = int(C.get_errno_EEXIST())
    EFAULT        = int(C.get_errno_EFAULT())
    EFBIG         = int(C.get_errno_EFBIG())
    EINTR         = int(C.get_errno_EINTR())
    EINVAL        = int(C.get_errno_EINVAL())
    EIO           = int(C.get_errno_EIO())
    EISDIR        = int(C.get_errno_EISDIR())
    EMFILE        = int(C.get_errno_EMFILE())
    EMLINK        = int(C.get_errno_EMLINK())
    ENAMETOOLONG  = int(C.get_errno_ENAMETOOLONG())
    ENFILE        = int(C.get_errno_ENFILE())
    ENODEV        = int(C.get_errno_ENODEV())
    ENOENT        = int(C.get_errno_ENOENT())
    ENOEXEC       = int(C.get_errno_ENOEXEC())
    ENOLCK        = int(C.get_errno_ENOLCK())
    ENOMEM        = int(C.get_errno_ENOMEM())
    ENOSPC        = int(C.get_errno_ENOSPC())
    ENOSYS        = int(C.get_errno_ENOSYS())
    ENOTDIR       = int(C.get_errno_ENOTDIR())
    ENOTEMPTY     = int(C.get_errno_ENOTEMPTY())
    ENOTTY        = int(C.get_errno_ENOTTY())
    ENXIO         = int(C.get_errno_ENXIO())
    EPERM         = int(C.get_errno_EPERM())
    EPIPE         = int(C.get_errno_EPIPE())
    ERANGE        = int(C.get_errno_ERANGE())
    EROFS         = int(C.get_errno_EROFS())
    ESPIPE        = int(C.get_errno_ESPIPE())
    ESRCH         = int(C.get_errno_ESRCH())
    EXDEV         = int(C.get_errno_EXDEV())
)
EOF

echo "✅ Created fixed host_linux.go"

# Create a build constraint to exclude the original problematic files during cross-compilation
if ! grep -q "//go:build !linux" "$CGOFUSE_DIR/fuse/host.go"; then
    sed -i '1i//go:build !linux\n' "$CGOFUSE_DIR/fuse/host.go"
    echo "✅ Added build constraint to host.go"
fi

if ! grep -q "//go:build !linux" "$CGOFUSE_DIR/fuse/errstr.go"; then
    sed -i '1i//go:build !linux\n' "$CGOFUSE_DIR/fuse/errstr.go"
    echo "✅ Added build constraint to errstr.go"
fi

echo "🎉 cgofuse cross-compilation fix completed!"
