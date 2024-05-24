/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package compression

import (
	"bufio"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"

	"github.com/containerd/log"
	"github.com/klauspost/compress/zstd"
)

type (
	// Compression is the state represents if compressed or not.
	Compression int
)

const (
	// Uncompressed represents the uncompressed.
	Uncompressed Compression = iota
	// Gzip is gzip compression algorithm.
	Gzip
	// Zstd is zstd compression algorithm.
	Zstd
	// Bzip2 is bzip2 compression algorithm
	Bzip2
	// Xz is Xz compression algorithm
	Xz
)

const (
	disablePigzEnv  = "CONTAINERD_DISABLE_PIGZ"
	disableIgzipEnv = "CONTAINERD_DISABLE_IGZIP"
)

var (
	initGzip sync.Once
	gzipPath string
)

var (
	bufioReader32KPool = &sync.Pool{
		New: func() interface{} { return bufio.NewReaderSize(nil, 32*1024) },
	}
)

// DecompressReadCloser include the stream after decompress and the compress method detected.
type DecompressReadCloser interface {
	io.ReadCloser
	// GetCompression returns the compress method which is used before decompressing
	GetCompression() Compression
}

type readCloserWrapper struct {
	io.Reader
	compression Compression
	closer      func() error
}

func (r *readCloserWrapper) Close() error {
	if r.closer != nil {
		return r.closer()
	}
	return nil
}

func (r *readCloserWrapper) GetCompression() Compression {
	return r.compression
}

type writeCloserWrapper struct {
	io.Writer
	closer func() error
}

func (w *writeCloserWrapper) Close() error {
	if w.closer != nil {
		w.closer()
	}
	return nil
}

type bufferedReader struct {
	buf *bufio.Reader
}

func newBufferedReader(r io.Reader) *bufferedReader {
	buf := bufioReader32KPool.Get().(*bufio.Reader)
	buf.Reset(r)
	return &bufferedReader{buf}
}

func (r *bufferedReader) Read(p []byte) (n int, err error) {
	if r.buf == nil {
		return 0, io.EOF
	}
	n, err = r.buf.Read(p)
	if err == io.EOF {
		r.buf.Reset(nil)
		bufioReader32KPool.Put(r.buf)
		r.buf = nil
	}
	return
}

func (r *bufferedReader) Peek(n int) ([]byte, error) {
	if r.buf == nil {
		return nil, io.EOF
	}
	return r.buf.Peek(n)
}

const (
	zstdMagicSkippableStart = 0x184D2A50
	zstdMagicSkippableMask  = 0xFFFFFFF0
)

var (
	gzipMagic  = []byte{0x1F, 0x8B, 0x08}
	zstdMagic  = []byte{0x28, 0xb5, 0x2f, 0xfd}
	bzip2Magic = []byte{0x42, 0x5A, 0x68}
	xzMagic    = []byte{0xFD, 0x37, 0x7A, 0x58, 0x5A, 0x00}
)

type matcher = func([]byte) bool

func magicNumberMatcher(m []byte) matcher {
	return func(source []byte) bool {
		return bytes.HasPrefix(source, m)
	}
}

// zstdMatcher detects zstd compression algorithm.
// There are two frame formats defined by Zstandard: Zstandard frames and Skippable frames.
// See https://datatracker.ietf.org/doc/html/rfc8878#section-3 for more details.
func zstdMatcher() matcher {
	return func(source []byte) bool {
		if bytes.HasPrefix(source, zstdMagic) {
			// Zstandard frame
			return true
		}
		// skippable frame
		if len(source) < 8 {
			return false
		}
		// magic number from 0x184D2A50 to 0x184D2A5F.
		if binary.LittleEndian.Uint32(source[:4])&zstdMagicSkippableMask == zstdMagicSkippableStart {
			return true
		}
		return false
	}
}

// DetectCompression detects the compression algorithm of the source.
func DetectCompression(source []byte) Compression {
	for compression, fn := range map[Compression]matcher{
		Gzip:  magicNumberMatcher(gzipMagic),
		Zstd:  zstdMatcher(),
		Bzip2: magicNumberMatcher(bzip2Magic),
		Xz:    magicNumberMatcher(xzMagic),
	} {
		if fn(source) {
			return compression
		}
	}
	return Uncompressed
}

// DecompressStream decompresses the archive and returns a ReaderCloser with the decompressed archive.
func DecompressStream(archive io.Reader) (DecompressReadCloser, error) {
	buf := newBufferedReader(archive)
	bs, err := buf.Peek(10)
	if err != nil && err != io.EOF {
		// Note: we'll ignore any io.EOF error because there are some odd
		// cases where the layer.tar file will be empty (zero bytes) and
		// that results in an io.EOF from the Peek() call. So, in those
		// cases we'll just treat it as a non-compressed stream and
		// that means just create an empty layer.
		// See Issue docker/docker#18170
		return nil, err
	}

	switch compression := DetectCompression(bs); compression {
	case Uncompressed:
		return &readCloserWrapper{
			Reader:      buf,
			compression: compression,
		}, nil
	case Gzip:
		ctx, cancel := context.WithCancel(context.Background())
		gzReader, err := gzipDecompress(ctx, buf)
		if err != nil {
			cancel()
			return nil, err
		}

		return &readCloserWrapper{
			Reader:      gzReader,
			compression: compression,
			closer: func() error {
				cancel()
				return gzReader.Close()
			},
		}, nil
	case Zstd:
		zstdReader, err := zstd.NewReader(buf)
		if err != nil {
			return nil, err
		}
		return &readCloserWrapper{
			Reader:      zstdReader,
			compression: compression,
			closer: func() error {
				zstdReader.Close()
				return nil
			},
		}, nil
	case Xz:
		ctx, cancel := context.WithCancel(context.Background())
		xzReader, err := xzDecompress(ctx, buf)
		if err != nil {
			cancel()
			return nil, err
		}
		return &readCloserWrapper{
			Reader:      xzReader,
			compression: compression,
			closer: func() error {
				cancel()
				return xzReader.Close()
			},
		}, nil
	case Bzip2:
		bzip2Reader := bzip2.NewReader(buf)
		if err != nil {
			return nil, err
		}
		return &readCloserWrapper{
			Reader:      bzip2Reader,
			compression: compression,
			closer: func() error {
				return nil
			},
		}, nil

	default:
		return nil, fmt.Errorf("unsupported compression format %s", (&compression).Extension())
	}
}

// CompressStream compresses the dest with specified compression algorithm.
func CompressStream(dest io.Writer, compression Compression) (io.WriteCloser, error) {
	switch compression {
	case Uncompressed:
		return &writeCloserWrapper{dest, nil}, nil
	case Gzip:
		return gzip.NewWriter(dest), nil
	case Zstd:
		return zstd.NewWriter(dest)
	default:
		return nil, fmt.Errorf("unsupported compression format %s", (&compression).Extension())
	}
}

// Extension returns the extension of a file that uses the specified compression algorithm.
func (compression *Compression) Extension() string {
	switch *compression {
	case Gzip:
		return "gz"
	case Zstd:
		return "zst"
	}
	return ""
}

func xzDecompress(ctx context.Context, archive io.Reader) (io.ReadCloser, error) {
	args := []string{"xz", "-d", "-c", "-q"}

	return cmdStream(exec.CommandContext(ctx, args[0], args[1:]...), archive)
}

func gzipDecompress(ctx context.Context, buf io.Reader) (io.ReadCloser, error) {
	initGzip.Do(func() {
		if gzipPath = detectCommand("igzip", disableIgzipEnv); gzipPath != "" {
			log.L.Debug("using igzip for decompression")
			return
		}
		if gzipPath = detectCommand("unpigz", disablePigzEnv); gzipPath != "" {
			log.L.Debug("using unpigz for decompression")
		}
	})

	if gzipPath == "" {
		return gzip.NewReader(buf)
	}
	return cmdStream(exec.CommandContext(ctx, gzipPath, "-d", "-c"), buf)
}

func cmdStream(cmd *exec.Cmd, in io.Reader) (io.ReadCloser, error) {
	reader, writer := io.Pipe()

	cmd.Stdin = in
	cmd.Stdout = writer

	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	go func() {
		if err := cmd.Wait(); err != nil {
			writer.CloseWithError(fmt.Errorf("%s: %s", err, errBuf.String()))
		} else {
			writer.Close()
		}
	}()

	return reader, nil
}

func detectCommand(path, disableEnvName string) string {
	// Check if this command is disabled via the env variable
	value := os.Getenv(disableEnvName)
	if value != "" {
		disable, err := strconv.ParseBool(value)
		if err != nil {
			log.L.WithError(err).Warnf("could not parse %s: %s", disableEnvName, value)
		}

		if disable {
			return ""
		}
	}

	path, err := exec.LookPath(path)
	if err != nil {
		log.L.WithError(err).Debugf("%s not found", path)
		return ""
	}

	return path
}
