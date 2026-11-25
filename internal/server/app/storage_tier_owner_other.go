//go:build !unix

package app

import "os"

func likeRecord(stage *os.File, record os.FileInfo) error {
	return stage.Chmod(record.Mode().Perm())
}
