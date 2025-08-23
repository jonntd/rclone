//go:build !cmount

// Package strmmount implements a FUSE mounting system for rclone remotes
// that virtualizes video files as .strm files for media servers.
package strmmount

import (
	"fmt"

	"github.com/rclone/rclone/cmd/mountlib"
	"github.com/rclone/rclone/vfs"
)

func init() {
	// Register a stub command that shows an error
	cmd := mountlib.NewMountCommand("strm-mount", false, mountStub)
	cmd.Short = "Mount cloud storage with video files as .strm files (requires cmount build tag)"
	cmd.Long = `
Mount cloud storage with video files virtualized as .strm files.

This command requires rclone to be built with the 'cmount' build tag.
Please rebuild rclone with: go build -tags cmount

Or use a pre-built binary that includes cmount support.
`
}

// mountStub is a stub implementation that shows an error
func mountStub(VFS *vfs.VFS, mountpoint string, opt *mountlib.Options) (<-chan error, func() error, error) {
	return nil, nil, fmt.Errorf("strm-mount requires rclone to be built with 'cmount' build tag")
}
