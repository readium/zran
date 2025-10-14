// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package gzran implements a seekable gzip.Reader that indexes offsets within
// the file as reading progresses, to make subsequent seeking more performant.
//
//	gzr, err := gzran.NewReader(r)
//	if err != nil {
//	  panic(err)
//	}
//	// Seek forward within the file, indexing as we go.
//	if _, err := gzr.Seek(n, io.SeekStart); err != nil {
//	  panic(err)
//	}
//	// Seek backward, using the on-the-fly index to do so efficiently.
//	if _, err := gzr.Seek(n - 128000, io.SeekStart); err != nil {
//	  panic(err)
//	}
//
// The Index can also be persisted and reused later:
//
//	// Read through entire file to index it, and then save the Index.
//	if _, err := io.Copy(ioutil.Discard, gzr); err != nil {
//	  panic(err)
//	}
//	if err := gzr.Index.WriteTo(f); err != nil {
//	  panic(err)
//	}
//
//	// Create a new gzip.Reader and load the Index to use it.
//	gzr, err := gzran.NewReader(r)
//	if err != nil {
//	  panic(err)
//	}
//	gzr.Index, err = gzran.LoadIndex(f)
//	if err != nil {
//	  panic(err)
//	}
//	// Seek and read as desired using the Index.
package gzran

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"time"

	"github.com/chocolatkey/gzran/internal/flate"
)

const (
	gzipID1     = 0x1f
	gzipID2     = 0x8b
	gzipDeflate = 8
	flagText    = 1 << 0
	flagHdrCrc  = 1 << 1
	flagExtra   = 1 << 2
	flagName    = 1 << 3
	flagComment = 1 << 4
)

// DefaultIndexInterval is how often the reader will save decompressor state by default.
const DefaultIndexInterval = 1024 * 1024 // 1 MB

var (
	// ErrChecksum is returned when reading GZIP data that has an invalid checksum.
	ErrChecksum = errors.New("gzip: invalid checksum")
	// ErrHeader is returned when reading GZIP data that has an invalid header.
	ErrHeader = errors.New("gzip: invalid header")
	// ErrInvalidSeek is returned if attempting to seek prior to beginning of the file.
	ErrInvalidSeek = errors.New("gzseek: attempting to seek before beginning of file")
	// ErrUnimplementedSeek is returned if attempting to seek from the end of the file.
	ErrUnimplementedSeek = errors.New("gzseek: seek from SeekEnd is not implemented")
)

var le = binary.LittleEndian

// noEOF converts io.EOF to io.ErrUnexpectedEOF.
func noEOF(err error) error {
	if err == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return err
}

// The gzip file stores a header giving metadata about the compressed file.
// That header is exposed as the fields of the Writer and Reader structs.
//
// Strings must be UTF-8 encoded and may only contain Unicode code points
// U+0001 through U+00FF, due to limitations of the GZIP file format.
type Header struct {
	Comment string    // comment
	Extra   []byte    // "extra data"
	ModTime time.Time // modification time
	Name    string    // file name
	OS      byte      // operating system type
}

// A Reader is an io.Reader that can be read to retrieve
// uncompressed data from a gzip-format compressed file.
//
// In general, a gzip file can be a concatenation of gzip files,
// each with its own header. Reads from the Reader
// return the concatenation of the uncompressed data of each.
// Only the first header is recorded in the Reader fields.
//
// Gzip files store a length and checksum of the uncompressed data.
// The Reader will return an ErrChecksum when Read
// reaches the end of the uncompressed data if it does not
// have the expected length or checksum. Clients should treat data
// returned by Read as tentative until they receive the io.EOF
// marking the end of the data.
type Reader struct {
	Header // valid after NewReader
	Index  // valid after NewReader

	r            io.ReadSeeker
	bufR         *tellReader
	decompressor io.ReadCloser
	digest       uint32 // CRC-32, IEEE polynomial (section 8)
	size         uint32 // Uncompressed size (section 2.3.1)
	buf          [512]byte
	err          error

	pos           int64 // Current offset of Read() within the uncompressed data.
	furthestRead  int64
	checkedDigest bool
	indexInterval int64
}

// NewReader creates a new Reader reading the given reader and default index interval.
// If r does not also implement io.ByteReader,
// the decompressor may read more data than necessary from r.
//
// It is the caller's responsibility to call Close on the Reader when done.
//
// The Reader.Header fields will be valid in the Reader returned.
func NewReader(r io.ReadSeeker) (*Reader, error) {
	return NewReaderInterval(r, DefaultIndexInterval)
}

// NewReaderInterval creates a new Reader consuming the given reader and
// checkpointing decompressor state at the given index interval.
// If r does not also implement io.ByteReader,
// the decompressor may read more data than necessary from r.
//
// It is the caller's responsibility to call Close on the Reader when done.
//
// The Reader.Header fields will be valid in the Reader returned.
func NewReaderInterval(r io.ReadSeeker, indexInterval int64) (*Reader, error) {
	bufR, err := newTellReader(r)
	if err != nil {
		return nil, err
	}

	z := &Reader{
		Index: Index{{
			CompressedOffset:   bufR.Offset(),
			UncompressedOffset: 0,
		}},
		r:             r,
		bufR:          bufR,
		indexInterval: indexInterval,
	}
	z.Header, z.err = z.readHeader()
	return z, z.err
}

// readString reads a NUL-terminated string from z.r.
// It treats the bytes read as being encoded as ISO 8859-1 (Latin-1) and
// will output a string encoded using UTF-8.
// This method always updates z.digest with the data read.
func (z *Reader) readString() (string, error) {
	var err error
	needConv := false
	for i := 0; ; i++ {
		if i >= len(z.buf) {
			return "", ErrHeader
		}
		z.buf[i], err = z.bufR.ReadByte()
		if err != nil {
			return "", err
		}
		if z.buf[i] > 0x7f {
			needConv = true
		}
		if z.buf[i] == 0 {
			// Digest covers the NUL terminator.
			z.digest = crc32.Update(z.digest, crc32.IEEETable, z.buf[:i+1])

			// Strings are ISO 8859-1, Latin-1 (RFC 1952, section 2.3.1).
			if needConv {
				s := make([]rune, 0, i)
				for _, v := range z.buf[:i] {
					s = append(s, rune(v))
				}
				return string(s), nil
			}
			return string(z.buf[:i]), nil
		}
	}
}

// readHeader reads the GZIP header according to section 2.3.1.
// This method does not set z.err.
func (z *Reader) readHeader() (hdr Header, err error) {
	if _, err = io.ReadFull(z.bufR, z.buf[:10]); err != nil {
		// RFC 1952, section 2.2, says the following:
		//	A gzip file consists of a series of "members" (compressed data sets).
		//
		// Other than this, the specification does not clarify whether a
		// "series" is defined as "one or more" or "zero or more". To err on the
		// side of caution, Go interprets this to mean "zero or more".
		// Thus, it is okay to return io.EOF here.
		return hdr, err
	}
	if z.buf[0] != gzipID1 || z.buf[1] != gzipID2 || z.buf[2] != gzipDeflate {
		return hdr, ErrHeader
	}
	flg := z.buf[3]
	if t := int64(le.Uint32(z.buf[4:8])); t > 0 {
		// Section 2.3.1, the zero value for MTIME means that the
		// modified time is not set.
		hdr.ModTime = time.Unix(t, 0)
	}
	// z.buf[8] is XFL and is currently ignored.
	hdr.OS = z.buf[9]
	prevDigest := z.digest
	z.digest = crc32.ChecksumIEEE(z.buf[:10])

	if flg&flagExtra != 0 {
		if _, err = io.ReadFull(z.bufR, z.buf[:2]); err != nil {
			return hdr, noEOF(err)
		}
		z.digest = crc32.Update(z.digest, crc32.IEEETable, z.buf[:2])
		data := make([]byte, le.Uint16(z.buf[:2]))
		if _, err = io.ReadFull(z.bufR, data); err != nil {
			return hdr, noEOF(err)
		}
		z.digest = crc32.Update(z.digest, crc32.IEEETable, data)
		hdr.Extra = data
	}

	var s string
	if flg&flagName != 0 {
		if s, err = z.readString(); err != nil {
			return hdr, err
		}
		hdr.Name = s
	}

	if flg&flagComment != 0 {
		if s, err = z.readString(); err != nil {
			return hdr, err
		}
		hdr.Comment = s
	}

	if flg&flagHdrCrc != 0 {
		if _, err = io.ReadFull(z.bufR, z.buf[:2]); err != nil {
			return hdr, noEOF(err)
		}
		digest := le.Uint16(z.buf[:2])
		if digest != uint16(z.digest) {
			return hdr, ErrHeader
		}
	}

	z.digest = prevDigest
	z.decompressor = flate.NewReader(z.bufR)
	return hdr, nil
}

// Read implements io.Reader, reading uncompressed bytes from its underlying Reader.
func (z *Reader) Read(p []byte) (n int, err error) {
	if z.err != nil {
		return 0, z.err
	}

	n, z.err = z.decompressor.Read(p)

	z.pos += int64(n)
	// Is this read past the furthest point we have read before?
	// If so then update size/digest with new data.
	if z.pos > z.furthestRead {
		startIdx := z.furthestRead - (z.pos - int64(n))
		newData := p[startIdx:n]
		z.digest = crc32.Update(z.digest, crc32.IEEETable, newData)
		z.size += uint32(len(newData))
		z.furthestRead = z.pos
	}
	if z.pos >= z.Index.lastUncompressedOffset()+z.indexInterval {
		z.addPointToIndex()
	}
	if z.err != io.EOF {
		// In the normal case we return here.
		return n, z.err
	}

	// Finished file; check checksum and size.
	if _, err := io.ReadFull(z.bufR, z.buf[:8]); err != nil {
		z.err = noEOF(err)
		return n, z.err
	}
	if z.checkedDigest {
		return n, io.EOF
	}
	digest := le.Uint32(z.buf[:4])
	size := le.Uint32(z.buf[4:8])
	if digest != z.digest || size != z.size {
		z.err = ErrChecksum
		return n, z.err
	}
	z.checkedDigest = true
	z.digest, z.size = 0, 0
	return n, io.EOF
}

func (z *Reader) addPointToIndex() {
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
func (z *Reader) Seek(offset int64, whence int) (position int64, err error) {
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

func (z *Reader) seekForward(offset int64) (position int64, err error) {
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

func (z *Reader) seekBackward(offset int64) (position int64, err error) {
	seekPoint := z.Index.closestPointBefore(offset)
	if pos, err := z.seekToPoint(seekPoint); err != nil {
		return pos, err
	}
	// We're now <= the desired offset, move forward as necessary to it.
	return z.Seek(offset, io.SeekStart)
}

func (z *Reader) seekToPoint(p Point) (position int64, err error) {
	_, z.err = z.r.Seek(p.CompressedOffset, io.SeekStart)
	if z.err != nil {
		return -1, z.err
	}
	z.bufR, z.err = newTellReader(z.r)
	if z.err != nil {
		return -1, z.err
	}
	if p.UncompressedOffset == 0 { // Beginning of file.
		z.Header, z.err = z.readHeader()
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
func (z *Reader) Close() error { return z.decompressor.Close() }
