//go:build !unix

package source

import "errors"

func mkfifo(string) error { return errors.New("no FIFO") }
