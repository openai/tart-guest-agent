package vd

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
)

// File transfer status codes
const (
	VD_AGENT_FILE_XFER_STATUS_CAN_SEND_DATA = iota
	VD_AGENT_FILE_XFER_STATUS_CANCELLED
	VD_AGENT_FILE_XFER_STATUS_ERROR
	VD_AGENT_FILE_XFER_STATUS_SUCCESS
	VD_AGENT_FILE_XFER_STATUS_NOT_ENOUGH_SPACE
	VD_AGENT_FILE_XFER_STATUS_SESSION_LOCKED
	VD_AGENT_FILE_XFER_STATUS_VDAGENT_NOT_CONNECTED
	VD_AGENT_FILE_XFER_STATUS_DISABLED
)

// VDAgentFileXferStart initiates a file transfer task.
type VDAgentFileXferStart struct {
	ID       uint32
	FileSize uint64 // Advertised total file size (parsed from binary size field or 0 if omitted)
	Data     []byte // Variable length metadata (key-value or raw filename)
}

// isTextIni checks whether the payload represents an unambiguous SPICE text-based INI metadata
// envelope (such as "[vdagent-file-xfer]\nname=..." or "name=..."), rather than a standard binary
// payload starting with an 8-byte uint64 file size.
func isTextIni(data []byte) bool {
	trimmed := bytes.TrimLeft(data, " \t\r\n")
	if len(trimmed) == 0 {
		return false
	}
	// Canonical SPICE group header
	if bytes.HasPrefix(trimmed, []byte("[vdagent-file-xfer]")) {
		return true
	}
	// Key-value metadata lines. The leading bytes of a binary size field can coincidentally
	// spell "name=" or "size=" (e.g. a file size whose low 5 bytes happen to match, with the
	// zero padding above it), so also require the candidate line to end in a newline with no
	// embedded NUL byte before it: a real size field's zero padding always produces a NUL
	// before any newline, while every bare key-value sender this format supports terminates
	// its line with '\n'.
	if bytes.HasPrefix(trimmed, []byte("name=")) || bytes.HasPrefix(trimmed, []byte("size=")) {
		newline := bytes.IndexAny(trimmed, "\r\n")
		if newline >= 0 && bytes.IndexByte(trimmed[:newline], 0x00) == -1 {
			return true
		}
	}
	// General INI envelope: must start with "[", have a closing "]", a newline,
	// contain "name=", and contain no NUL bytes in the section header line.
	if trimmed[0] == '[' {
		closeBracket := bytes.IndexByte(trimmed, ']')
		newline := bytes.IndexAny(trimmed, "\r\n")
		if closeBracket > 1 && newline > closeBracket && bytes.Contains(trimmed, []byte("name=")) {
			if bytes.IndexByte(trimmed[:newline], 0x00) == -1 {
				return true
			}
		}
	}
	return false
}

func DecodeVDAgentFileXferStart(buf []byte) (*VDAgentFileXferStart, error) {
	if len(buf) < 4 {
		return nil, io.ErrUnexpectedEOF
	}

	id := binary.LittleEndian.Uint32(buf[:4])
	rem := buf[4:]

	var fileSize uint64
	data := rem

	// A standard SPICE START message places an 8-byte little-endian uint64 size before the filename.
	// We parse the binary size header when rem contains both the 8-byte size and a filename (len(rem) > 8),
	// unless the payload explicitly begins with an unambiguous INI metadata envelope.
	if !isTextIni(rem) && len(rem) > 8 {
		fileSize = binary.LittleEndian.Uint64(rem[:8])
		data = rem[8:]
	}

	return &VDAgentFileXferStart{
		ID:       id,
		FileSize: fileSize,
		Data:     data,
	}, nil
}

func (msg VDAgentFileXferStart) Encode() ([]byte, error) {
	buffer := &bytes.Buffer{}

	if err := binary.Write(buffer, binary.LittleEndian, msg.ID); err != nil {
		return nil, err
	}

	if msg.FileSize > 0 && !isTextIni(msg.Data) {
		if err := binary.Write(buffer, binary.LittleEndian, msg.FileSize); err != nil {
			return nil, err
		}
	}

	if _, err := buffer.Write(msg.Data); err != nil {
		return nil, err
	}

	return buffer.Bytes(), nil
}

func (msg VDAgentFileXferStart) String() string {
	return fmt.Sprintf("VDAgentFileXferStart(id=%d, metadata=%d bytes)", msg.ID, len(msg.Data))
}

// VDAgentFileXferStatus sends status or progress for a transfer task.
type VDAgentFileXferStatus struct {
	ID     uint32
	Result uint32
	Data   []byte // Optional detailed error payload
}

func DecodeVDAgentFileXferStatus(buf []byte) (*VDAgentFileXferStatus, error) {
	if len(buf) < 8 {
		return nil, io.ErrUnexpectedEOF
	}

	r := bufio.NewReader(bytes.NewReader(buf))
	var header struct {
		ID     uint32
		Result uint32
	}

	if err := binary.Read(r, binary.LittleEndian, &header); err != nil {
		return nil, err
	}

	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}

	return &VDAgentFileXferStatus{
		ID:     header.ID,
		Result: header.Result,
		Data:   data,
	}, nil
}

func (msg VDAgentFileXferStatus) Encode() ([]byte, error) {
	buffer := &bytes.Buffer{}

	header := struct {
		ID     uint32
		Result uint32
	}{
		ID:     msg.ID,
		Result: msg.Result,
	}

	if err := binary.Write(buffer, binary.LittleEndian, header); err != nil {
		return nil, err
	}

	if len(msg.Data) > 0 {
		if _, err := buffer.Write(msg.Data); err != nil {
			return nil, err
		}
	}

	return buffer.Bytes(), nil
}

func (msg VDAgentFileXferStatus) String() string {
	return fmt.Sprintf("VDAgentFileXferStatus(id=%d, result=%d)", msg.ID, msg.Result)
}

// VDAgentFileXferData streams a binary chunk for an ongoing file transfer task.
type VDAgentFileXferData struct {
	ID   uint32
	Size uint64
	Data []byte
}

func DecodeVDAgentFileXferData(buf []byte) (*VDAgentFileXferData, error) {
	if len(buf) < 12 {
		return nil, io.ErrUnexpectedEOF
	}

	r := bufio.NewReader(bytes.NewReader(buf))
	var header struct {
		ID   uint32
		Size uint64
	}

	if err := binary.Read(r, binary.LittleEndian, &header); err != nil {
		return nil, err
	}

	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}

	if uint64(len(data)) < header.Size {
		return nil, fmt.Errorf(
			"filexfer data truncated: header declared %d bytes, payload has %d bytes: %w",
			header.Size, len(data), io.ErrUnexpectedEOF,
		)
	}
	if uint64(len(data)) > header.Size {
		return nil, fmt.Errorf(
			"filexfer data overflow: header declared %d bytes, payload has %d bytes",
			header.Size, len(data),
		)
	}

	return &VDAgentFileXferData{
		ID:   header.ID,
		Size: header.Size,
		Data: data,
	}, nil
}

func (msg VDAgentFileXferData) Encode() ([]byte, error) {
	buffer := &bytes.Buffer{}

	header := struct {
		ID   uint32
		Size uint64
	}{
		ID:   msg.ID,
		Size: msg.Size,
	}

	if err := binary.Write(buffer, binary.LittleEndian, header); err != nil {
		return nil, err
	}

	if _, err := buffer.Write(msg.Data); err != nil {
		return nil, err
	}

	return buffer.Bytes(), nil
}

func (msg VDAgentFileXferData) String() string {
	return fmt.Sprintf("VDAgentFileXferData(id=%d, chunk_size=%d, data_len=%d)",
		msg.ID, msg.Size, len(msg.Data))
}
