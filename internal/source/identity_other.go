//go:build !unix

package source

import "io/fs"

// identity has no device and inode where the system does not give them.
func identity(info fs.FileInfo) Identity {
	return Identity{Size: info.Size(), ModTime: info.ModTime()}
}
