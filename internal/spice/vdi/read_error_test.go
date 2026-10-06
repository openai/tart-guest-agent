package vdi_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"testing"

	"github.com/cirruslabs/tart-guest-agent/internal/spice/vd"
	"github.com/cirruslabs/tart-guest-agent/internal/spice/vdi"
	"github.com/stretchr/testify/require"
)

type readStep struct {
	data []byte
	err  error
}

type steppedTransport struct{ steps []readStep }

func (transport *steppedTransport) Read(buffer []byte) (int, error) {
	if len(transport.steps) == 0 {
		return 0, io.EOF
	}
	step := &transport.steps[0]
	bytesRead := copy(buffer, step.data)
	step.data = step.data[bytesRead:]
	if len(step.data) != 0 {
		return bytesRead, nil
	}
	err := step.err
	transport.steps = transport.steps[1:]
	return bytesRead, err
}

func (*steppedTransport) Write(buffer []byte) (int, error) { return len(buffer), nil }

func header() []byte {
	data := make([]byte, 8)
	binary.LittleEndian.PutUint32(data, vd.VDP_CLIENT_PORT)
	binary.LittleEndian.PutUint32(data[4:], 4)
	return data
}

func TestReadPreservesPayloadReturnedWithError(t *testing.T) {
	for _, terminal := range []error{io.EOF, os.ErrDeadlineExceeded, io.ErrUnexpectedEOF} {
		t.Run(terminal.Error(), func(t *testing.T) {
			transport := &steppedTransport{steps: []readStep{
				{data: header(), err: nil}, {data: []byte("data"), err: terminal},
			}}
			stream := vdi.New(transport)
			buffer := make([]byte, 20)
			n, err := stream.Read(buffer)
			require.Equal(t, 4, n)
			require.ErrorIs(t, err, terminal)
			require.Equal(t, "data", string(buffer[:n]))
		})
	}
}

func TestPayloadErrorAdvancesTheRemainingChunkLength(t *testing.T) {
	for _, split := range []int{0, 2, 4} {
		t.Run(string(rune('0'+split)), func(t *testing.T) {
			steps := []readStep{{data: header(), err: nil}, {data: []byte("data")[:split], err: os.ErrDeadlineExceeded}}
			if split < 4 {
				steps = append(steps, readStep{data: []byte("data")[split:], err: nil})
			}
			steps = append(steps, readStep{data: header(), err: nil}, readStep{data: []byte("next"), err: nil})
			stream := vdi.New(&steppedTransport{steps: steps})
			buffer := make([]byte, 20)
			n, err := stream.Read(buffer)
			require.Equal(t, split, n)
			require.ErrorIs(t, err, os.ErrDeadlineExceeded)
			all := append([]byte(nil), buffer[:n]...)
			rest, err := io.ReadAll(stream)
			require.NoError(t, err)
			require.Equal(t, "datanext", string(append(all, rest...)))
		})
	}
}

func TestFullReadAcceptsFinalBytesWithEOF(t *testing.T) {
	stream := vdi.New(&steppedTransport{steps: []readStep{
		{data: header(), err: nil},
		{data: []byte("data"), err: io.EOF},
	}})
	buffer := make([]byte, 4)
	n, err := io.ReadFull(stream, buffer)
	require.NoError(t, err)
	require.Equal(t, 4, n)
	require.Equal(t, "data", string(buffer))
}

func TestOrdinaryChunkedReadsKeepTheirContents(t *testing.T) {
	wire := append(header(), []byte("data")...)
	wire = append(wire, header()...)
	wire = append(wire, []byte("next")...)
	stream := vdi.New(bytes.NewBuffer(wire))
	var actual []byte
	for {
		buffer := make([]byte, 3)
		n, err := stream.Read(buffer)
		actual = append(actual, buffer[:n]...)
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
	}
	require.Equal(t, "datanext", string(actual))
}
