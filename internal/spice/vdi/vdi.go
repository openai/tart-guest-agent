package vdi

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/cirruslabs/tart-guest-agent/internal/spice/vd"
)

type VDI struct {
	inner     io.ReadWriter
	remaining uint64
}

type chunkHeader struct {
	Port uint32
	Size uint32
}

func New(inner io.ReadWriter) *VDI {
	return &VDI{
		inner: inner,
	}
}

func (vdi *VDI) Read(buf []byte) (int, error) {
	if len(buf) == 0 {
		return 0, nil
	}

	for {
		if vdi.remaining > 0 {
			toRead := min(len(buf), int(vdi.remaining))
			n, err := vdi.inner.Read(buf[:toRead])
			if err != nil {
				return 0, err
			}
			vdi.remaining -= uint64(n)
			return n, nil
		}

		// Read next chunk header with resync on misaligned bytes
		var headerBuf [8]byte
		if _, err := io.ReadFull(vdi.inner, headerBuf[:]); err != nil {
			return 0, err
		}

		port := binary.LittleEndian.Uint32(headerBuf[0:4])
		size := binary.LittleEndian.Uint32(headerBuf[4:8])

		// Resynchronize if Port does not match VDP_CLIENT_PORT
		for port != vd.VDP_CLIENT_PORT {
			copy(headerBuf[0:7], headerBuf[1:8])
			var b [1]byte
			if _, err := io.ReadFull(vdi.inner, b[:]); err != nil {
				return 0, err
			}
			headerBuf[7] = b[0]
			port = binary.LittleEndian.Uint32(headerBuf[0:4])
			size = binary.LittleEndian.Uint32(headerBuf[4:8])
		}

		if size > MaxChunkSize {
			return 0, fmt.Errorf("vdi: chunk size %d exceeds maximum allowable limit (%d bytes)", size, MaxChunkSize)
		}

		vdi.remaining = uint64(size)
	}
}

// MaxChunkSize defines the maximum payload per VDI chunk (VD_AGENT_MAX_DATA_SIZE)
const MaxChunkSize = 2048

func (vdi *VDI) Write(buf []byte) (int, error) {
	if len(buf) == 0 {
		return 0, nil
	}

	totalWritten := 0
	for offset := 0; offset < len(buf); offset += MaxChunkSize {
		chunkEnd := min(offset+MaxChunkSize, len(buf))
		chunk := buf[offset:chunkEnd]

		buffer := &bytes.Buffer{}
		vdiChunkHeader := chunkHeader{
			Port: vd.VDP_CLIENT_PORT,
			Size: uint32(len(chunk)),
		}
		if err := binary.Write(buffer, binary.LittleEndian, &vdiChunkHeader); err != nil {
			return totalWritten, err
		}

		if _, err := vdi.inner.Write(append(buffer.Bytes(), chunk...)); err != nil {
			return totalWritten, err
		}
		totalWritten += len(chunk)
	}

	return totalWritten, nil
}
