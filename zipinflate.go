package gzran

import (
	"io"

	"github.com/chocolatkey/gzran/internal/flate"
)

// A Reader is an io.Reader that can be read to retrieve
// uncompressed data from a raw deflated (compressed) file, from a ZIP archive.
//
// Clients should treat data
// returned by Read as tentative until they receive the io.EOF
// marking the end of the data.
type DReader struct {
	Index // valid after NewReader

	r            io.ReadSeeker
	bufR         *tellReader
	decompressor io.ReadCloser
	size         uint32 // Uncompressed size (section 2.3.1)
	err          error

	pos           int64 // Current offset of Read() within the uncompressed data.
	furthestRead  int64
	indexInterval int64
}

func NewDReader(r io.ReadSeeker) (*DReader, error) {
	return NewDReaderInterval(r, DefaultIndexInterval)
}

func NewDReaderInterval(r io.ReadSeeker, indexInterval int64) (*DReader, error) {
	bufR, err := newTellReader(r)
	if err != nil {
		return nil, err
	}

	z := &DReader{
		Index: Index{{
			CompressedOffset:   bufR.Offset(),
			UncompressedOffset: 0,
		}},
		r:             r,
		bufR:          bufR,
		indexInterval: indexInterval,
	}

	z.decompressor = flate.NewReader(z.bufR)
	return z, z.err
}

// Read implements io.Reader, reading uncompressed bytes from its underlying Reader.
func (z *DReader) Read(p []byte) (n int, err error) {
	if z.err != nil {
		return 0, z.err
	}

	n, z.err = z.decompressor.Read(p)

	z.pos += int64(n)
	// Is this read past the furthest point we have read before?
	// If so then update size/digest with new data.
	if z.pos > z.furthestRead {
		z.furthestRead = z.pos
	}
	if z.pos >= z.Index.lastUncompressedOffset()+z.indexInterval {
		z.addPointToIndex()
	}
	if z.err != io.EOF {
		// In the normal case we return here.
		return n, z.err
	}

	return n, io.EOF
}

func (z *DReader) addPointToIndex() {
	state, err := flate.DecompressorState(z.decompressor)
	if err != nil {
		panic(err) // Error should be impossible since z is a flate.Reader.
	}

	p := Point{
		CompressedOffset:   z.bufR.Offset(),
		UncompressedOffset: z.pos,
		DecompressorState:  state,
	}

	z.Index = append(z.Index, p)
}

// Seek implements io.Seeker.
// The gzip file will be decompressed as needed to seek forward, building an index
// of offsets as it does so. Subsequent calls to seek will use the index to skip
// data more efficiently. Seeking from the end of the file is not implemented
// and will return ErrUnimplementedSeek.
func (z *DReader) Seek(offset int64, whence int) (position int64, err error) {
	switch whence {
	case io.SeekStart:
		if offset < 0 {
			return z.pos, ErrInvalidSeek
		} else if offset == z.pos {
			return z.pos, nil
		} else if offset > z.pos {
			return z.seekForward(offset)
		} else {
			return z.seekBackward(offset)
		}
	case io.SeekCurrent:
		return z.Seek(z.pos+offset, io.SeekStart)
	default:
		return z.pos, ErrUnimplementedSeek
	}
}

func (z *DReader) seekForward(offset int64) (position int64, err error) {
	seekPoint := z.Index.closestPointBefore(offset)
	if seekPoint.UncompressedOffset > z.pos+z.indexInterval {
		if pos, err := z.seekToPoint(seekPoint); err != nil {
			return pos, err
		}
	}
	nBytesToSkip := offset - z.pos
	_, z.err = io.CopyN(io.Discard, z, nBytesToSkip)
	return z.pos, z.err
}

func (z *DReader) seekBackward(offset int64) (position int64, err error) {
	seekPoint := z.Index.closestPointBefore(offset)
	if pos, err := z.seekToPoint(seekPoint); err != nil {
		return pos, err
	}
	// We're now <= the desired offset, move forward as necessary to it.
	return z.Seek(offset, io.SeekStart)
}

func (z *DReader) seekToPoint(p Point) (position int64, err error) {
	_, z.err = z.r.Seek(p.CompressedOffset, io.SeekStart)
	if z.err != nil {
		return -1, z.err
	}
	z.bufR, z.err = newTellReader(z.r)
	if z.err != nil {
		return -1, z.err
	}
	if p.UncompressedOffset == 0 { // Beginning of file.
		z.decompressor = flate.NewReader(z.bufR)
	} else {
		z.decompressor, z.err = flate.NewReaderState(z.bufR, p.DecompressorState)
	}
	z.pos = p.UncompressedOffset
	z.furthestRead = z.pos
	return z.pos, z.err
}

// Close closes the Reader. It does not close the underlying io.Reader.
// In order for the GZIP checksum to be verified, the reader must be
// fully consumed until the io.EOF.
func (z *DReader) Close() error { return z.decompressor.Close() }
