//go:build !darwin && !linux

package cli

import "io"

func prepareStdin(src io.Reader) (io.Reader, func(), error) {
	return src, func() {}, nil
}
