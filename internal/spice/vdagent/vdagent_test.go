//nolint:testpackage // Exercise protocol handlers without a real clipboard or serial device.
package vdagent

import (
	"bytes"
	"context"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/cirruslabs/tart-guest-agent/internal/spice/vd"
	"github.com/cirruslabs/tart-guest-agent/internal/spice/vdi"
	"github.com/stretchr/testify/require"
	"golang.design/x/clipboard"
)

const (
	hostClipboardText           = "host text"
	terminatedHostClipboardText = "host text\x00"
	guestClipboardText          = "new guest text"
)

type clipboardTestState struct {
	data      []byte
	readError error
}

type clipboardTestReader func([]byte) (int, error)

func (reader clipboardTestReader) Read(data []byte) (int, error) {
	return reader(data)
}

type clipboardTestSerial struct {
	io.Reader
	io.Writer
}

func newClipboardTestAgent() (*VDAgent, *bytes.Buffer, *clipboardTestState) {
	wire := &bytes.Buffer{}
	state := &clipboardTestState{}
	agent := &VDAgent{
		vdi: vdi.New(wire),
		writeClipboard: func(_ context.Context, data []byte) error {
			state.data = bytes.Clone(data)
			return nil
		},
		readClipboard: func(context.Context) ([]byte, error) {
			return bytes.Clone(state.data), state.readError
		},
	}

	return agent, wire, state
}

func requireClipboardGrab(t *testing.T, wire *bytes.Buffer) {
	t.Helper()
	message, err := vd.ReadVDAgentMessage(vdi.New(wire))
	require.NoError(t, err)
	require.Equal(t, uint32(vd.VD_AGENT_PROTOCOL), message.Protocol)
	require.Equal(t, uint32(vd.VD_AGENT_CLIPBOARD_GRAB), message.Type)
	grab, err := vd.DecodeVDAgentClipboardGrab(bytes.NewReader(message.Data))
	require.NoError(t, err)
	require.Equal(t, uint8(vd.VD_AGENT_CLIPBOARD_SELECTION_CLIPBOARD), grab.Selection)
	require.Equal(t, uint32(vd.VD_AGENT_CLIPBOARD_UTF8_TEXT), grab.Type)
	require.Zero(t, wire.Len(), "a new guest copy must send exactly one framed grab")
}

func TestHostClipboardDoesNotEcho(t *testing.T) {
	cases := []struct {
		name    string
		host    string
		watched string
	}{
		{name: "plain", host: hostClipboardText, watched: hostClipboardText},
		{name: "unicode", host: "\u4e2d\u6587\U0001f642", watched: "\u4e2d\u6587\U0001f642"},
		{name: "host terminator", host: terminatedHostClipboardText, watched: hostClipboardText},
		{name: "watcher terminator", host: hostClipboardText, watched: terminatedHostClipboardText},
		{name: "multiple terminators", host: "host text\x00\x00", watched: terminatedHostClipboardText},
		{name: "empty", host: "", watched: ""},
		{name: "empty terminated", host: "\x00", watched: ""},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			agent, wire, state := newClipboardTestAgent()
			agent.lastClipboardState = []byte("previous guest text")
			require.NoError(t, agent.processHostClipboard(t.Context(), []byte(testCase.host)))
			state.data = []byte(testCase.watched)
			require.NoError(t, agent.processClipboardChange(t.Context()))
			require.Zero(t, wire.Len(), "host-originated text must not produce a new clipboard grab")
		})
	}
}

func TestHostClipboardSnapshotOwnsItsData(t *testing.T) {
	agent, wire, _ := newClipboardTestAgent()
	data := []byte(hostClipboardText)
	require.NoError(t, agent.processHostClipboard(t.Context(), data))
	data[0] = 'X'

	require.NoError(t, agent.processClipboardChange(t.Context()))
	require.Zero(t, wire.Len(), "reusing the received buffer must not cause an echo")
}

func TestHostClipboardWritePreservesPayloadAndContext(t *testing.T) {
	agent, _, _ := newClipboardTestAgent()
	ctx := t.Context()
	var written []byte
	agent.writeClipboard = func(receivedContext context.Context, data []byte) error {
		require.Equal(t, ctx, receivedContext)
		written = bytes.Clone(data)
		return nil
	}

	require.NoError(t, agent.processHostClipboard(ctx, []byte("received text\x00")))
	require.Equal(t, []byte("received text\x00"), written,
		"normalizing comparisons must not alter the clipboard write payload")
}

func TestGuestCopyStillAdvertised(t *testing.T) {
	agent, wire, state := newClipboardTestAgent()
	require.NoError(t, agent.processHostClipboard(t.Context(), []byte(hostClipboardText)))
	require.NoError(t, agent.processClipboardChange(t.Context()))
	require.Zero(t, wire.Len())

	state.data = []byte(guestClipboardText)
	require.NoError(t, agent.processClipboardChange(t.Context()))
	requireClipboardGrab(t, wire)
	require.NoError(t, agent.processClipboardChange(t.Context()))
	require.Zero(t, wire.Len(), "duplicate notifications must not advertise an unchanged guest copy")
}

func TestClipboardWriteErrorPreservesState(t *testing.T) {
	agent, wire, state := newClipboardTestAgent()
	agent.lastClipboardState = []byte("previous text")
	state.data = []byte("previous text")
	agent.writeClipboard = func(context.Context, []byte) error {
		return os.ErrPermission
	}

	err := agent.processHostClipboard(t.Context(), []byte("failed host write"))
	require.ErrorIs(t, err, os.ErrPermission)
	require.NoError(t, agent.processClipboardChange(t.Context()))
	require.Zero(t, wire.Len(), "a failed write must leave the previous known state intact")

	state.data = []byte("failed host write")
	require.NoError(t, agent.processClipboardChange(t.Context()))
	requireClipboardGrab(t, wire)
}

func TestClipboardNormalizationPreservesTextChanges(t *testing.T) {
	cases := []struct {
		name  string
		host  string
		guest string
	}{
		{name: "different text", host: hostClipboardText, guest: "guest text"},
		{name: "embedded NUL", host: "a\x00b", guest: "ab"},
		{name: "leading NUL", host: "\x00a", guest: "a"},
		{name: "whitespace", host: "text", guest: "text "},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			agent, wire, state := newClipboardTestAgent()
			require.NoError(t, agent.processHostClipboard(t.Context(), []byte(testCase.host)))
			state.data = []byte(testCase.guest)
			require.NoError(t, agent.processClipboardChange(t.Context()))
			requireClipboardGrab(t, wire)
		})
	}
}

func TestQueuedGuestNotificationDoesNotRecreateHostEcho(t *testing.T) {
	agent, wire, _ := newClipboardTestAgent()
	notifications := make(chan clipboard.Data, 2)
	notifications <- clipboard.Data{Format: clipboard.FmtText, Bytes: []byte("queued guest text")}
	require.NoError(t, agent.processHostClipboard(t.Context(), []byte(hostClipboardText)))
	notifications <- clipboard.Data{Format: clipboard.FmtText, Bytes: []byte(hostClipboardText)}
	close(notifications)

	// Like Run, only use the notifications as wakeups. Both reads must see
	// the current host-owned clipboard, even though the first event predates it.
	for range notifications {
		require.NoError(t, agent.processClipboardChange(t.Context()))
		require.Zero(t, wire.Len(), "neither the queued guest event nor the host event may reclaim the host clipboard")
	}
}

func TestCoalescedHostWritesDoNotSuppressLaterGuestCopy(t *testing.T) {
	agent, wire, state := newClipboardTestAgent()
	require.NoError(t, agent.processHostClipboard(t.Context(), []byte(hostClipboardText)))
	require.NoError(t, agent.processHostClipboard(t.Context(), []byte("second host text")))

	// The watcher can coalesce writes, so the first host value is never observed.
	require.NoError(t, agent.processClipboardChange(t.Context()))
	require.Zero(t, wire.Len())

	state.data = []byte(hostClipboardText)
	require.NoError(t, agent.processClipboardChange(t.Context()))
	requireClipboardGrab(t, wire)
}

func TestDelayedHostNotificationDoesNotSuppressGuestCopy(t *testing.T) {
	agent, wire, state := newClipboardTestAgent()
	require.NoError(t, agent.processHostClipboard(t.Context(), []byte(hostClipboardText)))
	state.data = []byte(guestClipboardText)

	// Even if the first delivered notification belongs to the host write,
	// the guest's newer current value must be advertised immediately.
	require.NoError(t, agent.processClipboardChange(t.Context()))
	requireClipboardGrab(t, wire)
	require.NoError(t, agent.processClipboardChange(t.Context()))
	require.Zero(t, wire.Len())

	// An old host value is a legitimate new guest copy after another value.
	state.data = []byte(hostClipboardText)
	require.NoError(t, agent.processClipboardChange(t.Context()))
	requireClipboardGrab(t, wire)
}

func TestClipboardReadErrorPreservesState(t *testing.T) {
	agent, wire, state := newClipboardTestAgent()
	require.NoError(t, agent.processHostClipboard(t.Context(), []byte(hostClipboardText)))
	state.readError = os.ErrPermission

	err := agent.processClipboardChange(t.Context())
	require.ErrorIs(t, err, os.ErrPermission)
	require.Zero(t, wire.Len(), "a failed read must not advertise unknown clipboard data")

	state.readError = nil
	require.NoError(t, agent.processClipboardChange(t.Context()))
	require.Zero(t, wire.Len(), "a failed read must not discard the remembered host write")

	state.data = []byte(guestClipboardText)
	require.NoError(t, agent.processClipboardChange(t.Context()))
	requireClipboardGrab(t, wire)
}

func TestNoClipboardDataAdvertisesEmptyGuestCopy(t *testing.T) {
	agent, wire, state := newClipboardTestAgent()
	require.NoError(t, agent.processHostClipboard(t.Context(), []byte(hostClipboardText)))
	state.data = nil
	state.readError = clipboard.ErrNoData

	require.NoError(t, agent.processClipboardChange(t.Context()))
	requireClipboardGrab(t, wire)
	require.NoError(t, agent.processClipboardChange(t.Context()))
	require.Zero(t, wire.Len(), "an unchanged empty clipboard must not keep producing grabs")
}

func TestRunHandlesQueuedNotificationDuringHostRead(t *testing.T) {
	serial, hostSerial, err := os.Pipe()
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = serial.Close()
		_ = hostSerial.Close()
	})

	agent, wire, _ := newClipboardTestAgent()
	agent.serialPort = serial
	readStarted := make(chan struct{})
	var firstRead sync.Once
	agent.vdi = vdi.New(clipboardTestSerial{
		Reader: clipboardTestReader(func(data []byte) (int, error) {
			firstRead.Do(func() { close(readStarted) })
			return serial.Read(data)
		}),
		Writer: wire,
	})
	notifications := make(chan clipboard.Data, 2)
	agent.watchClipboard = func(context.Context) <-chan clipboard.Data {
		return notifications
	}
	writeClipboard := agent.writeClipboard
	agent.writeClipboard = func(ctx context.Context, data []byte) error {
		if err := writeClipboard(ctx, data); err != nil {
			return err
		}
		notifications <- clipboard.Data{Format: clipboard.FmtText, Bytes: bytes.Clone(data)}
		return nil
	}
	readClipboard := agent.readClipboard
	clipboardReads := make(chan struct{}, 2)
	agent.readClipboard = func(ctx context.Context) ([]byte, error) {
		data, err := readClipboard(ctx)
		clipboardReads <- struct{}{}
		return data, err
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	runResult := make(chan error, 1)
	go func() {
		runResult <- agent.Run(ctx)
	}()

	select {
	case <-readStarted:
	case <-ctx.Done():
		t.Fatal("agent never started its serial read")
	}
	notifications <- clipboard.Data{Format: clipboard.FmtText, Bytes: []byte("queued guest text")}
	hostClipboard := vd.VDAgentClipboard{
		VDAgentClipboardInner: vd.VDAgentClipboardInner{
			Selection: vd.VD_AGENT_CLIPBOARD_SELECTION_CLIPBOARD,
			Type:      vd.VD_AGENT_CLIPBOARD_UTF8_TEXT,
		},
		Data: []byte(hostClipboardText),
	}
	clipboardBytes, err := hostClipboard.Encode()
	require.NoError(t, err)
	message := vd.VDAgentMessage{
		VDAgentMessageInner: vd.VDAgentMessageInner{
			Protocol: vd.VD_AGENT_PROTOCOL,
			Type:     vd.VD_AGENT_CLIPBOARD,
			Size:     uint32(len(clipboardBytes)),
		},
		Data: clipboardBytes,
	}
	messageBytes, err := message.Encode()
	require.NoError(t, err)
	_, err = vdi.New(clipboardTestSerial{Writer: hostSerial}).Write(messageBytes)
	require.NoError(t, err)

	for range 2 {
		select {
		case <-clipboardReads:
		case runError := <-runResult:
			t.Fatalf("agent exited before consuming both notifications: %v", runError)
		case <-ctx.Done():
			t.Fatal("agent did not consume both queued notifications")
		}
	}
	cancel()
	require.ErrorIs(t, <-runResult, context.Canceled)
	require.Zero(t, wire.Len(), "the production loop must not echo either queued notification")
}
