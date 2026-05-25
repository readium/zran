package zran

import (
	"encoding/gob"
	"io"
	"sort"
)

// Index collects decompressor state at offset Points.
// gzseek.Reader adds points to the index on the fly as decompression proceeds.
type Index []Point

// LoadIndex deserializes an Index from the given io.Reader.
func LoadIndex(r io.Reader) (Index, error) {
	dec := gob.NewDecoder(r)
	idx := make(Index, 0)
	err := dec.Decode(&idx)
	return idx, err
}

// WriteTo serializes the index to the given io.Writer.
// It can be deserialized with LoadIndex.
func (idx Index) WriteTo(w io.Writer) error {
	enc := gob.NewEncoder(w)
	return enc.Encode(idx)
}

func (idx Index) lastUncompressedOffset() int64 {
	if len(idx) == 0 {
		return 0
	}

	return idx[len(idx)-1].UncompressedOffset
}

func (idx Index) closestPointBefore(offset int64) Point {
	j := sort.Search(len(idx), func(j int) bool {
		return idx[j].UncompressedOffset > offset
	})

	if j == 0 {
		return Point{}
	}

	return idx[j-1]
}

// Point holds the decompressor state at a given offset within the uncompressed data.
type Point struct {
	CompressedOffset   int64
	UncompressedOffset int64
	DecompressorState  []byte
}
