package fsutil

import (
	"os"
	"syscall"
)

func sysStat(info os.FileInfo) (*syscall.Stat_t, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	return st, ok
}
