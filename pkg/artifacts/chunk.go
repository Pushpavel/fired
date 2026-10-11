package artifacts

import (
	"io"
	"sync"

	chunkers "github.com/PlakarKorp/go-cdc-chunkers"
	_ "github.com/PlakarKorp/go-cdc-chunkers/chunkers/fastcdc" // registers fastcdc-v1.0.0
)

// Large files are stored, and can be uploaded, as content-defined chunks:
// FastCDC cuts them where the content says, so an edit or an append changes
// one or two chunks and the rest are shared with earlier versions, and with
// any other file, artifact or user that has them. The algorithm and sizes
// are part of the storage format: changing them breaks nothing, but new
// chunks would stop matching stored ones.
const (
	// ChunkThreshold is the size from which a file is chunked.
	ChunkThreshold = 1 << 20

	chunkAlgorithm = "fastcdc-v1.0.0" // the spec-faithful, versioned variant
	chunkMin       = 256 << 10
	chunkAvg       = 1 << 20
	chunkMax       = 4 << 20
)

// ChunkRef is one chunk of a chunked file, in order.
type ChunkRef struct {
	Hash string `json:"hash"`
	Size int64  `json:"size"`
}

// ChunksRequest declares a file as a list of uploaded chunks
// (PUT /v1/blobs/{hash}/chunks).
type ChunksRequest struct {
	Chunks []ChunkRef `json:"chunks"`
}

// chunkBufs holds scan buffers: a chunker needs MaxSize bytes or more.
var chunkBufs = sync.Pool{New: func() any { b := make([]byte, 2*chunkMax); return &b }}

// SplitChunks cuts r into chunks and calls fn with each one's bytes, which
// are only valid during the call.
func SplitChunks(r io.Reader, fn func(chunk []byte) error) error {
	buf := chunkBufs.Get().(*[]byte)
	defer chunkBufs.Put(buf)
	c, err := chunkers.NewChunkerBuffer(chunkAlgorithm, r, &chunkers.ChunkerOpts{
		MinSize: chunkMin, NormalSize: chunkAvg, MaxSize: chunkMax}, *buf)
	if err != nil {
		return err
	}
	for {
		data, err := c.Next()
		if err != nil && err != io.EOF {
			return err
		}
		if len(data) > 0 {
			if ferr := fn(data); ferr != nil {
				return ferr
			}
		}
		if err == io.EOF {
			return nil
		}
	}
}
