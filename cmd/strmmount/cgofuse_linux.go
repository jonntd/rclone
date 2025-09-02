//go:build linux && cgo

package strmmount

/*
#include <errno.h>
#include <fcntl.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <unistd.h>

// Define errno constants as functions to avoid cross-compilation issues
static int get_E2BIG() { return E2BIG; }
static int get_EACCES() { return EACCES; }
static int get_EAGAIN() { return EAGAIN; }
static int get_EBADF() { return EBADF; }
static int get_EBUSY() { return EBUSY; }
static int get_ECHILD() { return ECHILD; }
static int get_EDEADLK() { return EDEADLK; }
static int get_EDOM() { return EDOM; }
static int get_EEXIST() { return EEXIST; }
static int get_EFAULT() { return EFAULT; }
static int get_EFBIG() { return EFBIG; }
static int get_EINTR() { return EINTR; }
static int get_EINVAL() { return EINVAL; }
static int get_EIO() { return EIO; }
static int get_EISDIR() { return EISDIR; }
static int get_EMFILE() { return EMFILE; }
static int get_EMLINK() { return EMLINK; }
static int get_ENAMETOOLONG() { return ENAMETOOLONG; }
static int get_ENFILE() { return ENFILE; }
static int get_ENODEV() { return ENODEV; }
static int get_ENOENT() { return ENOENT; }
static int get_ENOEXEC() { return ENOEXEC; }
static int get_ENOLCK() { return ENOLCK; }
static int get_ENOMEM() { return ENOMEM; }
static int get_ENOSPC() { return ENOSPC; }
static int get_ENOSYS() { return ENOSYS; }
static int get_ENOTDIR() { return ENOTDIR; }
static int get_ENOTEMPTY() { return ENOTEMPTY; }
static int get_ENOTTY() { return ENOTTY; }
static int get_ENXIO() { return ENXIO; }
static int get_EPERM() { return EPERM; }
static int get_EPIPE() { return EPIPE; }
static int get_ERANGE() { return ERANGE; }
static int get_EROFS() { return EROFS; }
static int get_ESPIPE() { return ESPIPE; }
static int get_ESRCH() { return ESRCH; }
static int get_EXDEV() { return EXDEV; }
*/
import "C"

// This file provides errno constants for cgofuse cross-compilation compatibility
// It's only compiled on Linux with CGO enabled

func init() {
    // Initialize errno constants that cgofuse needs
    // This ensures they're available during cross-compilation
    _ = int(C.get_E2BIG())
    _ = int(C.get_EACCES())
    _ = int(C.get_EAGAIN())
    _ = int(C.get_EBADF())
    _ = int(C.get_EBUSY())
    _ = int(C.get_ECHILD())
    _ = int(C.get_EDEADLK())
    _ = int(C.get_EDOM())
    _ = int(C.get_EEXIST())
    _ = int(C.get_EFAULT())
    _ = int(C.get_EFBIG())
    _ = int(C.get_EINTR())
    _ = int(C.get_EINVAL())
    _ = int(C.get_EIO())
    _ = int(C.get_EISDIR())
    _ = int(C.get_EMFILE())
    _ = int(C.get_EMLINK())
    _ = int(C.get_ENAMETOOLONG())
    _ = int(C.get_ENFILE())
    _ = int(C.get_ENODEV())
    _ = int(C.get_ENOENT())
    _ = int(C.get_ENOEXEC())
    _ = int(C.get_ENOLCK())
    _ = int(C.get_ENOMEM())
    _ = int(C.get_ENOSPC())
    _ = int(C.get_ENOSYS())
    _ = int(C.get_ENOTDIR())
    _ = int(C.get_ENOTEMPTY())
    _ = int(C.get_ENOTTY())
    _ = int(C.get_ENXIO())
    _ = int(C.get_EPERM())
    _ = int(C.get_EPIPE())
    _ = int(C.get_ERANGE())
    _ = int(C.get_EROFS())
    _ = int(C.get_ESPIPE())
    _ = int(C.get_ESRCH())
    _ = int(C.get_EXDEV())
}
