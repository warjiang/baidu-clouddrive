//go:build darwin || linux

package cli

import (
	"io"
	"os"
	"syscall"
)

func prepareStdin(src io.Reader) (io.Reader, func(), error) {
	f, ok := src.(*os.File)
	if !ok {
		return src, func() {}, nil
	}
	info, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	if info.Mode().IsRegular() {
		return src, func() {}, nil
	}
	raw, err := f.SyscallConn()
	if err != nil {
		return nil, nil, err
	}
	var input *os.File
	var flags uintptr
	controlErr := raw.Control(func(fd uintptr) {
		var errno syscall.Errno
		flags, _, errno = syscall.Syscall(syscall.SYS_FCNTL, fd, syscall.F_GETFL, 0)
		if errno != 0 {
			err = errno
			return
		}
		var duplicate int
		duplicate, err = syscall.Dup(int(fd))
		if err != nil {
			return
		}
		syscall.CloseOnExec(duplicate)
		// Inherited stdin is normally blocking. NewFile registers nonblocking
		// descriptors with Go's poller so Close can interrupt a pending Read.
		if err = syscall.SetNonblock(duplicate, true); err != nil {
			_ = syscall.Close(duplicate)
			return
		}
		input = os.NewFile(uintptr(duplicate), f.Name())
	})
	if controlErr != nil {
		return nil, nil, controlErr
	}
	if err != nil {
		return nil, nil, err
	}
	return input, func() {
		_ = input.Close()
		// Duplicates share status flags. Leave the caller's descriptor open
		// and restore its blocking mode after the reader and canceler exit.
		if flags&syscall.O_NONBLOCK == 0 {
			_ = raw.Control(func(fd uintptr) { _ = syscall.SetNonblock(int(fd), false) })
		}
	}, nil
}
