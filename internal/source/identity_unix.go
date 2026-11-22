//go:build unix

package source

import (
	"io/fs"
	"syscall"
)

func identity(info fs.FileInfo) Identity {
	id := Identity{Size: info.Size(), ModTime: info.ModTime()}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		id.Device, id.Inode = uint64(st.Dev), uint64(st.Ino)
	}
	return id
}
