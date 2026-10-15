package log

import (
	"errors"
	"io"
	"os"
	"sync"
	"syscall"
)

type syncWriter struct {
	io.Writer
	m sync.Mutex
}

func (w *syncWriter) Write(data []byte) (int, error) {
	w.m.Lock()
	defer w.m.Unlock()

	return w.Writer.Write(data)
}

func (w *syncWriter) Sync() error {
	w.m.Lock()
	defer w.m.Unlock()

	// If the underlying writer does not implement Sync()
	// just skip it.
	flusher, ok := w.Writer.(interface{ Sync() error })
	if !ok {
		return nil
	}

	// Only regular files can be fsync'd. Pipes (runit→svlogd), terminals and
	// other special files can't; attempting it returns EINVAL/ENOTSUP/EBADF.
	// One way of doing this would be to cast the `Writer` to an `*os.File` to
	// validate that the writer is a regular file. However, because of the way
	// Go types work, this would not work for an *os.File embedded in another
	// struct. So instead, we deduce the file indirectly by verifying if the type
	// implement `Stat()`.
	if st, ok := w.Writer.(interface{ Stat() (os.FileInfo, error) }); ok {
		info, err := st.Stat()
		if err != nil {
			return err
		}

		// Only regular files can be fsync'd.
		if !info.Mode().IsRegular() {
			return nil
		}
	}

	// Here, we may very well have a writer that implements `Sync` but not `Stat`.
	// Unlikely, but not impossible. So we validate the error and return nil
	// if the error is related to a file that does not support fsync.
	err := flusher.Sync()
	if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EBADF) {
		return nil
	}
	return err
}

// NewSyncWriter returns Writer wrapped with a mutex that is acquired
// before each Write call.
func NewSyncWriter(w io.Writer) io.Writer {
	return &syncWriter{Writer: w}
}
