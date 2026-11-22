//go:build unix

package source

import (
	"errors"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/gtriggiano/tessera/internal/sets"
)

func mkfifo(path string) error { return syscall.Mkfifo(path, 0o600) }

// A FIFO without a writer would block a read for ever: ReadFile refuses it
// at once.
func TestReadFileRefusesAFIFO(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fifo")
	if err := mkfifo(path); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := ReadFile(path, sets.String, 10)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrNotRegular) {
			t.Errorf("got %v, want %v", err, ErrNotRegular)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ReadFile blocks on a FIFO")
	}
}
