//go:build unix

package config

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A FIFO without a writer would block a read for ever: Load refuses it at
// once.
func TestLoadRefusesAFIFO(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := Load(path, nil)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || err.Error() != "configuration file: not a regular file" {
			t.Fatalf("err=%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Load blocked on a FIFO")
	}
}

func TestLoadPermissionDenied(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads any file")
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("policy: {default_action: deny}\n"), 0o000); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path, nil); err == nil || err.Error() != "configuration file: permission denied" {
		t.Fatalf("err=%v", err)
	}
}
