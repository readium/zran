package zran

import (
	"bufio"
	"fmt"
	"io"

	"github.com/readium/gzran/internal/flate"
)

// tellReader is a bufio.Reader that also tracks its offset
// (number of bytes read) within the underlying data.
// tellReader implements flate.Reader.
type tellReader struct {
	r      *bufio.Reader
	offset int64
}

var _ flate.Reader = &tellReader{}

func newTellReader(r io.ReadSeeker) (*tellReader, error) {
	initialOffset, err := r.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, fmt.Errorf("gzseek: unable to determinine initial reader offset: %v", err)
	}

	return &tellReader{
		r:      bufio.NewReader(r),
		offset: initialOffset,
	}, nil
}

func (r *tellReader) Read(p []byte) (n int, err error) {
	n, err = r.r.Read(p)
	r.offset += int64(n)
	return
}

func (r *tellReader) ReadByte() (byte, error) {
	b, err := r.r.ReadByte()
	if err == nil {
		r.offset++
	}
	return b, err
}

func (r *tellReader) Offset() int64 {
	return r.offset
}
