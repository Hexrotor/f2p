package utils

import (
	"bytes"
	"encoding/binary"
	"io"
	"log/slog"
	"sync"

	ddzstd "github.com/DataDog/zstd"
)

type ZstdDuplex struct {
	base io.ReadWriteCloser

	readBuf bytes.Buffer

	wrFrames      int
	wrCompressed  int
	wrOrigBytes   int64
	wrStoredBytes int64
	rdFrames      int
	rdCompressed  int
	rdOrigBytes   int64
	rdStoredBytes int64

	closeOnce sync.Once
	closeErr  error

	metaSide     string
	metaService  string
	metaPeer     string
	metaProtocol string
}

const (
	flagCompressed byte    = 0x01
	compressRatio  float64 = 1.0
)

var (
	zstdLevel      = 20
	zstdMinSizeB   = 256
	zstdChunkSizeB = 32 * 1024
)

func ConfigureZstdParams(level, minSizeB, chunkSizeB int) {
	zstdLevel = level
	curMin := minSizeB
	curChunk := chunkSizeB
	if curChunk <= 0 {
		curChunk = 32 * 1024
		slog.Debug("zstd chunk size missing; using default", "chunkB", curChunk)
	}
	if curMin <= 0 {
		curMin = 256
		slog.Debug("zstd min size missing; using default", "minB", curMin)
	}
	if curMin >= curChunk {
		curMin = curChunk - 1
		if curMin < 32 {
			curMin = 32
		}
		slog.Debug("zstd min adjusted to be smaller than chunk", "minB", curMin, "chunkB", curChunk)
	}
	zstdMinSizeB = curMin
	zstdChunkSizeB = curChunk
}

func writeUvarint(w io.Writer, v uint64) error {
	var buf [10]byte
	n := binary.PutUvarint(buf[:], v)
	_, err := w.Write(buf[:n])
	return err
}

type byteReader struct{ r io.Reader }

func (b *byteReader) ReadByte() (byte, error) {
	var one [1]byte
	_, err := io.ReadFull(b.r, one[:])
	return one[0], err
}

func readUvarint(r io.Reader) (uint64, error) {
	br := &byteReader{r: r}
	return binary.ReadUvarint(br)
}

func NewZstdDuplex(base io.ReadWriteCloser) (*ZstdDuplex, error) {
	return &ZstdDuplex{base: base}, nil
}

func (z *ZstdDuplex) Read(p []byte) (int, error) {
	if z.readBuf.Len() > 0 {
		return z.readBuf.Read(p)
	}
	var flag [1]byte
	if _, err := io.ReadFull(z.base, flag[:]); err != nil {
		return 0, err
	}
	storedLen, err := readUvarint(z.base)
	if err != nil {
		return 0, err
	}
	if storedLen > 1<<30 {
		return 0, io.ErrUnexpectedEOF
	}
	payload := make([]byte, int(storedLen))
	if _, err := io.ReadFull(z.base, payload); err != nil {
		return 0, err
	}

	var out []byte
	var origLen uint64
	if (flag[0] & flagCompressed) != 0 {
		out, err = ddzstd.Decompress(nil, payload)
		if err != nil {
			return 0, err
		}
		origLen = uint64(len(out))
		z.rdCompressed++
	} else {
		out = payload
		origLen = uint64(len(out))
	}

	z.rdFrames++
	z.rdOrigBytes += int64(origLen)
	z.rdStoredBytes += int64(storedLen)

	z.readBuf.Write(out)
	return z.readBuf.Read(p)
}

func uvarintLen(v uint64) int {
	l := 1
	for v >= 0x80 {
		v >>= 7
		l++
	}
	return l
}

func (z *ZstdDuplex) Write(p []byte) (int, error) {
	writtenIn := 0
	for off := 0; off < len(p); {
		end := off + zstdChunkSizeB
		if end > len(p) {
			end = len(p)
		}
		chunk := p[off:end]

		var (
			useCompressed bool
			compressed    bytes.Buffer
		)

		if (len(chunk) >= zstdMinSizeB) && (len(chunk) < ddzstd.CompressBound(len(chunk))) {
			compBytes, err := ddzstd.CompressLevel(nil, chunk, zstdLevel)
			if err == nil {
				compHead := 1 + uvarintLen(uint64(len(compBytes)))
				useCompressed = float64(len(compBytes)+compHead) < float64(len(chunk))*compressRatio
				// If size gets bigger than before, we don't send compressed data
				if useCompressed {
					compressed.Write(compBytes)
				}
			}
		}

		var flag byte
		var payload []byte
		if useCompressed {
			flag = flagCompressed
			payload = compressed.Bytes()
			z.wrCompressed++
		} else {
			flag = 0
			payload = chunk
		}

		if _, err := z.base.Write([]byte{flag}); err != nil {
			return writtenIn, err
		}
		if err := writeUvarint(z.base, uint64(len(payload))); err != nil {
			return writtenIn, err
		}
		if _, err := z.base.Write(payload); err != nil {
			return writtenIn, err
		}

		z.wrFrames++
		z.wrOrigBytes += int64(len(chunk))
		z.wrStoredBytes += int64(len(payload))

		writtenIn += len(chunk)
		off = end
	}
	return writtenIn, nil
}

func (z *ZstdDuplex) Close() error {
	z.closeOnce.Do(func() {
		if z.wrOrigBytes > 0 || z.rdOrigBytes > 0 {
			wrRatio := 1.0
			if z.wrOrigBytes > 0 {
				wrRatio = float64(z.wrStoredBytes) / float64(z.wrOrigBytes)
			}
			rdRatio := 1.0
			if z.rdOrigBytes > 0 {
				rdRatio = float64(z.rdStoredBytes) / float64(z.rdOrigBytes)
			}
			// log summary for further zstd performance tests
			slog.Debug("zstd summary",
				"side", z.metaSide,
				"service", z.metaService,
				"protocol", z.metaProtocol,
				"peer", z.metaPeer,
				"wr_frames", z.wrFrames,
				"wr_comp_frames", z.wrCompressed,
				"wr_orig_bytes", z.wrOrigBytes,
				"wr_stored_bytes", z.wrStoredBytes,
				"wr_ratio", wrRatio,
				"rd_frames", z.rdFrames,
				"rd_comp_frames", z.rdCompressed,
				"rd_orig_bytes", z.rdOrigBytes,
				"rd_stored_bytes", z.rdStoredBytes,
				"rd_ratio", rdRatio,
			)
		}
		z.closeErr = z.base.Close()
	})
	return z.closeErr
}

func (z *ZstdDuplex) SetInfo(side, service, peer, protocol string) *ZstdDuplex {
	if side != "" {
		z.metaSide = side
	}
	if service != "" {
		z.metaService = service
	}
	if peer != "" {
		z.metaPeer = peer
	}
	if protocol != "" {
		z.metaProtocol = protocol
	}
	return z
}
