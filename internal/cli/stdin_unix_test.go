//go:build darwin || linux

package cli

import (
	"errors"
	"io"
	"os"
	"syscall"
	"testing"
	"time"
)

func TestPrepareStdinMakesInheritedPipeInterruptible(t *testing.T) {
	for _, nonblocking := range []bool{false, true} {
		name := "blocking"
		if nonblocking {
			name = "nonblocking"
		}
		t.Run(name, func(t *testing.T) {
			reader, writer, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			defer writer.Close()
			// Fd puts an os.Pipe descriptor in blocking mode, as inherited stdin
			// normally is. NewFile must see that mode when wrapping our duplicate.
			fd, err := syscall.Dup(int(reader.Fd()))
			if err != nil {
				t.Fatal(err)
			}
			if err := syscall.SetNonblock(fd, nonblocking); err != nil {
				_ = syscall.Close(fd)
				t.Fatal(err)
			}
			input := os.NewFile(uintptr(fd), "stdin")
			defer input.Close()
			src, cleanup, err := prepareStdin(input)
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			prepared := src.(*os.File)
			if err := prepared.SetReadDeadline(time.Now().Add(-time.Second)); err != nil {
				t.Fatalf("stdin is not pollable: %v", err)
			}
			if _, err := prepared.Read(make([]byte, 1)); !errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatalf("idle pipe read was not interruptible: %v", err)
			}
			if err := prepared.SetReadDeadline(time.Time{}); err != nil {
				t.Fatal(err)
			}
			if _, err := writer.Write([]byte("stdin data")); err != nil {
				t.Fatal(err)
			}
			_ = writer.Close()
			data, err := io.ReadAll(prepared)
			if err != nil || string(data) != "stdin data" {
				t.Fatalf("stdin data/EOF: %q %v", data, err)
			}
			cleanup()
			flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_GETFL, 0)
			if errno != 0 || (flags&syscall.O_NONBLOCK != 0) != nonblocking {
				t.Fatalf("original descriptor/mode not preserved: flags=%x errno=%v", flags, errno)
			}
			if _, err := input.Read(make([]byte, 1)); err != io.EOF {
				t.Fatalf("original stdin was closed: %v", err)
			}
		})
	}
}
