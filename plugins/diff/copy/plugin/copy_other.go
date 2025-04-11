//go:build !darwin

package plugin

import (
	"io"
	"os"
)

func copyToFile(dst string, src io.Reader) error {
	f, err := os.Create(dst)
	if err != nil {
		return err
	}

	// Most efficient form of copy will be chosen by ReadFrom
	_, err = io.Copy(f, src)
	return err
}
