package plugin

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func copyToFile(dst string, src io.Reader) error {
	if lr, ok := src.(*io.LimitedReader); ok {
		if f, ok := lr.R.(*os.File); ok {
			var dstDir int
			if !filepath.IsAbs(dst) {
				dstDir = unix.AT_FDCWD
			}
			err := unix.Fclonefileat(int(f.Fd()), dstDir, dst, unix.CLONE_NOFOLLOW)
			if err == nil {
				return nil
			}
			if !errors.Is(err, unix.ENOTSUP) && !errors.Is(err, unix.EXDEV) {
				return fmt.Errorf("clonefile failed: %w", err)
			}
		}
	}

	f, err := os.Create(dst)
	if err != nil {
		return err
	}

	// Most efficient form of copy will be chosen by ReadFrom
	_, err = io.Copy(f, src)
	return err
}
