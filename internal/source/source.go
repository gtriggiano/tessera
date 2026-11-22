// Package source reads the content of a set from its source, and keeps the
// copy of each set read from a file up to date. It reads a file through the
// open descriptor, and reports the identity of what it read, so that a later
// check can tell a changed file from the same one.
package source

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"syscall"
	"time"

	"github.com/gtriggiano/tessera/internal/sets"
)

// Identity identifies the version of a file that a copy was read from: the
// file, as fstat described the open descriptor, and the SHA-256 of the
// content read. A file replaced, renamed over or rewritten has another
// device, inode, size or modification time; a check compares those first,
// and the content only when they differ.
type Identity struct {
	Device, Inode uint64
	Size          int64
	ModTime       time.Time
	// Sum is the SHA-256 of the content, zero when the content was not read
	// to its end.
	Sum [sha256.Size]byte
}

// sameFile reports whether a and b describe the same file, unchanged: the
// same device, inode, size and modification time. It ignores Sum.
func sameFile(a, b Identity) bool {
	return a.Device == b.Device && a.Inode == b.Inode && a.Size == b.Size && a.ModTime.Equal(b.ModTime)
}

// The errors of a file that cannot be read. They never repeat the path.
var (
	ErrNotFound   = errors.New("the file does not exist")
	ErrPermission = errors.New("the file cannot be opened: permission denied")
	ErrOpen       = errors.New("the file cannot be opened")
	ErrNotRegular = errors.New("not a regular file")
	ErrRead       = errors.New("the file cannot be read")
)

// ReadFile reads the set of type t, of at most maxEntries entries, from the
// file at path. It opens the file without blocking, refuses anything but a
// regular file, such as a FIFO whose reads could wait for ever, and reads
// the content through the open descriptor, whose identity it returns.
//
// Invalid content, or a ceiling exceeded, is a *sets.Error; a file that does
// not exist or cannot be read is one of the errors above.
func ReadFile(path string, t sets.Type, maxEntries int) (*sets.Set, Identity, error) {
	return readFile(context.Background(), path, t, maxEntries)
}

// readFile is ReadFile, stopped when ctx ends: the read then fails with
// ErrRead. On invalid content, the identity is still that of the file read,
// without Sum.
func readFile(ctx context.Context, path string, t sets.Type, maxEntries int) (*sets.Set, Identity, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, Identity{}, openError(err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, Identity{}, ErrRead
	}
	if !info.Mode().IsRegular() {
		return nil, Identity{}, ErrNotRegular
	}
	id := identity(info)
	sum := sha256.New()
	set, err := sets.Read(t, io.TeeReader(contextReader{ctx, f}, sum), maxEntries)
	var readErr *sets.ReadError
	if errors.As(err, &readErr) {
		return nil, Identity{}, ErrRead
	}
	if err != nil {
		return nil, id, err
	}
	sum.Sum(id.Sum[:0])
	return set, id, nil
}

// openError is the error of a file that cannot be opened, or described.
func openError(err error) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return ErrNotFound
	case errors.Is(err, fs.ErrPermission):
		return ErrPermission
	}
	return ErrOpen
}

// contextReader reads from r until ctx ends.
type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (c contextReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// Problem describes err, an error of ReadFile, without the content of the
// file or its path: the line of the file and the problem for invalid
// content, the problem alone otherwise.
func Problem(err error) string {
	var invalid *sets.Error
	if errors.As(err, &invalid) && invalid.Line > 0 {
		return fmt.Sprintf("line %d of the file: %s", invalid.Line, invalid.Problem)
	}
	return err.Error()
}
