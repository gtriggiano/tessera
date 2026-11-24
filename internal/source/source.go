// Package source reads the content of a set or of a database from a file,
// and keeps the copies read from files up to date. It reads a file through
// the open descriptor, and reports the identity of what it read, so that a
// later check can tell a changed file from the same one.
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

	"github.com/gtriggiano/tessera/internal/enrichment"
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

// Parse builds a copy from the content of a file, read through r, whose
// size, as fstat described the open descriptor, is size: a hint, since the
// content can change while it is read. Its error is invalid content, or a
// ceiling exceeded. A failure of r makes the read fail with ErrRead, whatever
// Parse returns.
type Parse[T any] func(r io.Reader, size int64) (T, error)

// Invalid is the error of content that Parse rejected: invalid, or over a
// ceiling. Err is the error of Parse.
type Invalid struct{ Err error }

func (e *Invalid) Error() string { return e.Err.Error() }
func (e *Invalid) Unwrap() error { return e.Err }

// Read reads the file at path and builds a copy with parse. It opens the
// file without blocking, refuses anything but a regular file, such as a FIFO
// whose reads could wait for ever, and reads the content through the open
// descriptor, whose identity it returns.
//
// Content that parse rejects is an *Invalid, with the identity of the file
// but without Sum; a file that does not exist or cannot be read is one of
// the errors above.
func Read[T any](path string, parse Parse[T]) (T, Identity, error) {
	return read(context.Background(), path, parse)
}

// ReadFile reads the set of type t, of at most maxEntries entries, from the
// file at path, as Read does: a text file, or a GeoJSON document for an area
// set. Invalid content, or a ceiling exceeded, is a *sets.Error.
func ReadFile(path string, t sets.Type, maxEntries int) (*sets.Set, Identity, error) {
	return unwrapInvalid(Read(path, SetParse(t, maxEntries)))
}

// ReadDatabase reads the database of kind k from the file at path, as Read
// does. Invalid content, or a ceiling exceeded, is an error of
// enrichment.Read.
func ReadDatabase(path string, k enrichment.Kind) (*enrichment.Database, Identity, error) {
	return unwrapInvalid(Read(path, DatabaseParse(k)))
}

func unwrapInvalid[T any](c T, id Identity, err error) (T, Identity, error) {
	var invalid *Invalid
	if errors.As(err, &invalid) {
		err = invalid.Err
	}
	return c, id, err
}

// SetParse reads a set of type t, of at most maxEntries entries.
func SetParse(t sets.Type, maxEntries int) Parse[*sets.Set] {
	return func(r io.Reader, _ int64) (*sets.Set, error) { return sets.Read(t, r, maxEntries) }
}

// DatabaseParse reads a database of kind k.
func DatabaseParse(k enrichment.Kind) Parse[*enrichment.Database] {
	return func(r io.Reader, size int64) (*enrichment.Database, error) {
		return enrichment.ReadAtMost(k, r, size, databaseMaxBytes)
	}
}

// databaseMaxBytes is the ceiling of a database: enrichment.MaxFileBytes,
// which the tests lower.
var databaseMaxBytes int64 = enrichment.MaxFileBytes

// read is Read, stopped when ctx ends: the read then fails with ErrRead.
func read[T any](ctx context.Context, path string, parse Parse[T]) (T, Identity, error) {
	var zero T
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return zero, Identity{}, openError(err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return zero, Identity{}, ErrRead
	}
	if !info.Mode().IsRegular() {
		return zero, Identity{}, ErrNotRegular
	}
	id := identity(info)
	sum := sha256.New()
	content := &contentReader{ctx: ctx, r: f}
	c, err := parse(io.TeeReader(content, sum), info.Size())
	if content.err != nil {
		return zero, Identity{}, ErrRead
	}
	if err != nil {
		return zero, id, &Invalid{Err: err}
	}
	sum.Sum(id.Sum[:0])
	return c, id, nil
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

// maxChunk bounds what one Read of the content asks the file for, so that
// the end of ctx stops a large read, from a slow volume too, between chunks.
const maxChunk = 1 << 20

// contentReader reads from r, at most maxChunk bytes at a time, until ctx
// ends, and records the first failure of r, or the end of ctx, in err.
type contentReader struct {
	ctx context.Context
	r   io.Reader
	err error
}

func (c *contentReader) Read(p []byte) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	if err := c.ctx.Err(); err != nil {
		c.err = err
		return 0, err
	}
	if len(p) > maxChunk {
		p = p[:maxChunk]
	}
	n, err := c.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		c.err = err
	}
	return n, err
}

// Problem describes err, an error of a read, without the content of the
// file or its path: for invalid content of a set, the line of the file and
// the problem, with the column and the place in the structure of a GeoJSON
// document; the problem alone otherwise.
func Problem(err error) string {
	var invalid *sets.Error
	switch {
	case errors.As(err, &invalid) && invalid.Line > 0 && invalid.Column > 0:
		return fmt.Sprintf("line %d, column %d of the file: %s", invalid.Line, invalid.Column, invalid.Where())
	case errors.As(err, &invalid) && invalid.Line > 0:
		return fmt.Sprintf("line %d of the file: %s", invalid.Line, invalid.Problem)
	}
	return err.Error()
}
