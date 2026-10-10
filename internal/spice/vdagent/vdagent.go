package vdagent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/cirruslabs/tart-guest-agent/internal/spice/vd"
	"github.com/cirruslabs/tart-guest-agent/internal/spice/vdi"
	"go.uber.org/zap"
	"golang.design/x/clipboard"
)

const serialPortPath = "/dev/tty.com.redhat.spice.0"

type VDAgent struct {
	serialPort         *os.File
	vdi                *vdi.VDI
	lastClipboardState []byte
	writeClipboard     func(context.Context, []byte) error
	readClipboard      func(context.Context) ([]byte, error)
	watchClipboard     func(context.Context) <-chan clipboard.Data
}

func New() (*VDAgent, error) {
	sp, err := os.OpenFile(serialPortPath, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}

	if err := clipboard.Init(); err != nil {
		return nil, err
	}

	return &VDAgent{
		serialPort: sp,
		vdi:        vdi.New(sp),
		writeClipboard: func(ctx context.Context, data []byte) error {
			_, err := clipboard.Write(ctx, clipboard.FmtText, data)
			return err
		},
		readClipboard: func(ctx context.Context) ([]byte, error) {
			return clipboard.Read(ctx, clipboard.FmtText)
		},
		watchClipboard: func(ctx context.Context) <-chan clipboard.Data {
			return clipboard.Watch(ctx, clipboard.FmtText)
		},
	}, nil
}

func (agent *VDAgent) Run(ctx context.Context) error {
	subCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	clipboardCh := agent.watchClipboard(subCtx)

	for {
		// Check for cancellation and clipboard changes
		select {
		case <-ctx.Done():
			return ctx.Err()
		case _, ok := <-clipboardCh:
			if !ok {
				return errors.New("clipboard watch channel closed")
			}
			if err := agent.processClipboardChange(ctx); err != nil {
				return err
			}
		default:
			// Nothing, proceed
		}

		if err := agent.serialPort.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
			return err
		}

		vdiAgentMessage, err := vd.ReadVDAgentMessage(agent.vdi)
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				continue
			}

			return err
		}

		switch vdiAgentMessage.Type {
		case vd.VD_AGENT_ANNOUNCE_CAPABILITIES:
			vdAgentAnnounceCapabilities, err := vd.ReadVDAgentAnnounceCapabilities(vdiAgentMessage.Data)
			if err != nil {
				return err
			}

			zap.S().Debugf("I: VD_AGENT_ANNOUNCE_CAPABILITIES: %s", vdAgentAnnounceCapabilities)

			if vdAgentAnnounceCapabilities.Request == 0 {
				// No need to send our capabilities
				break
			}

			// Send our capabilities
			ourCapabilities := vd.VDAgentAnnounceCapabilities{
				Request: 0,
				Caps:    vd.VD_AGENT_CAP_CLIPBOARD_BY_DEMAND | vd.VD_AGENT_CAP_CLIPBOARD_SELECTION,
			}
			ourCapabilitiesBytes, err := ourCapabilities.Encode()
			if err != nil {
				return err
			}

			ourAgentMessage := vd.VDAgentMessage{
				VDAgentMessageInner: vd.VDAgentMessageInner{
					Protocol: vd.VD_AGENT_PROTOCOL,
					Type:     vd.VD_AGENT_ANNOUNCE_CAPABILITIES,
					Size:     uint32(len(ourCapabilitiesBytes)),
				},
				Data: ourCapabilitiesBytes,
			}
			ourAgentMessageBytes, err := ourAgentMessage.Encode()
			if err != nil {
				return err
			}

			if _, err := agent.vdi.Write(ourAgentMessageBytes); err != nil {
				return err
			}

			zap.S().Debugf("O: VD_AGENT_ANNOUNCE_CAPABILITIES")
		case vd.VD_AGENT_CLIPBOARD_GRAB:
			vdAgentClipboardGrab, err := vd.DecodeVDAgentClipboardGrab(bytes.NewReader(vdiAgentMessage.Data))
			if err != nil {
				return err
			}

			zap.S().Debugf("I: VD_AGENT_CLIPBOARD_GRAB (%d bytes): %s",
				len(vdiAgentMessage.Data), vdAgentClipboardGrab)

			ourClipboardRequest := vd.VDAgentClipboardRequest{
				Type: 1,
			}
			ourClipboardRequestBytes, err := ourClipboardRequest.Encode()
			if err != nil {
				return err
			}

			ourAgentMessage := vd.VDAgentMessage{
				VDAgentMessageInner: vd.VDAgentMessageInner{
					Protocol: vd.VD_AGENT_PROTOCOL,
					Type:     vd.VD_AGENT_CLIPBOARD_REQUEST,
					Size:     uint32(len(ourClipboardRequestBytes)),
				},
				Data: ourClipboardRequestBytes,
			}
			ourAgentMessageBytes, err := ourAgentMessage.Encode()
			if err != nil {
				return err
			}

			if _, err := agent.vdi.Write(ourAgentMessageBytes); err != nil {
				return err
			}

			zap.S().Debugf("O: VD_AGENT_CLIPBOARD_REQUEST")
		case vd.VD_AGENT_CLIPBOARD:
			// Receive clipboard
			vdAgentClipboard, err := vd.DecodeVDAgentClipboard(vdiAgentMessage.Data)
			if err != nil {
				return err
			}

			zap.S().Debugf("I: VD_AGENT_CLIPBOARD: %s", vdAgentClipboard)

			if err := agent.processHostClipboard(ctx, vdAgentClipboard.Data); err != nil {
				return err
			}
		case vd.VD_AGENT_CLIPBOARD_REQUEST:
			vdAgentClipboardRequest, err := vd.DecodeVDAgentClipboardRequest(bytes.NewReader(vdiAgentMessage.Data))
			if err != nil {
				return err
			}

			zap.S().Debugf("I: VD_AGENT_CLIPBOARD_REQUEST: %s", vdAgentClipboardRequest)

			// No text on the clipboard is a valid empty response.
			clipboardData, err := clipboard.Read(ctx, clipboard.FmtText)
			if err != nil && !errors.Is(err, clipboard.ErrNoData) {
				return fmt.Errorf("failed to read clipboard: %w", err)
			}
			ourAgentClipboard := vd.VDAgentClipboard{
				VDAgentClipboardInner: vd.VDAgentClipboardInner{
					Selection: vd.VD_AGENT_CLIPBOARD_SELECTION_CLIPBOARD,
					Type:      vd.VD_AGENT_CLIPBOARD_UTF8_TEXT,
				},
				Data: clipboardData,
			}
			ourAgentClipboardBytes, err := ourAgentClipboard.Encode()
			if err != nil {
				return err
			}

			ourAgentMessage := vd.VDAgentMessage{
				VDAgentMessageInner: vd.VDAgentMessageInner{
					Protocol: vd.VD_AGENT_PROTOCOL,
					Type:     vd.VD_AGENT_CLIPBOARD,
					Size:     uint32(len(ourAgentClipboardBytes)),
				},
				Data: ourAgentClipboardBytes,
			}
			ourAgentMessageBytes, err := ourAgentMessage.Encode()
			if err != nil {
				return err
			}

			if _, err := agent.vdi.Write(ourAgentMessageBytes); err != nil {
				return err
			}

			zap.S().Debugf("O: VD_AGENT_CLIPBOARD")
		default:
			zap.S().Debugf("I: unhandled message type: %d", vdiAgentMessage.Type)
		}
	}
}

func (agent *VDAgent) Close() error {
	return agent.serialPort.Close()
}

func (agent *VDAgent) processHostClipboard(ctx context.Context, data []byte) error {
	if err := agent.writeClipboard(ctx, data); err != nil {
		return fmt.Errorf("failed to write clipboard: %w", err)
	}

	// The watcher must not announce this host-originated write back to the host:
	// a plain-text grab would replace any rich or file data on its clipboard.
	agent.lastClipboardState = bytes.Clone(data)

	return nil
}

func (agent *VDAgent) processClipboardChange(ctx context.Context) error {
	// Watch notifications can already be queued while we receive a host write.
	// Use them only as wakeups: their bytes may predate the current clipboard.
	// Reading the current value keeps stale notifications from replacing the
	// remembered host write and causing a plain-text echo.
	newClipboardState, err := agent.readClipboard(ctx)
	if err != nil && !errors.Is(err, clipboard.ErrNoData) {
		return fmt.Errorf("failed to read clipboard: %w", err)
	}

	// SPICE text may be NUL-terminated while the clipboard watcher omits it.
	if bytes.Equal(bytes.TrimRight(agent.lastClipboardState, "\x00"), bytes.TrimRight(newClipboardState, "\x00")) {
		// This value was already written by the host or advertised by us.
		return nil
	}

	ourGrab := vd.VDAgentClipboardGrab{
		Selection: vd.VD_AGENT_CLIPBOARD_SELECTION_CLIPBOARD,
		Type:      vd.VD_AGENT_CLIPBOARD_UTF8_TEXT,
	}
	ourGrabBytes, err := ourGrab.Encode()
	if err != nil {
		return err
	}

	ourAgentMessage := vd.VDAgentMessage{
		VDAgentMessageInner: vd.VDAgentMessageInner{
			Protocol: vd.VD_AGENT_PROTOCOL,
			Type:     vd.VD_AGENT_CLIPBOARD_GRAB,
			Size:     uint32(len(ourGrabBytes)),
		},
		Data: ourGrabBytes,
	}
	ourAgentMessageBytes, err := ourAgentMessage.Encode()
	if err != nil {
		return err
	}

	if _, err := agent.vdi.Write(ourAgentMessageBytes); err != nil {
		return err
	}

	agent.lastClipboardState = bytes.Clone(newClipboardState)

	zap.S().Debugf("O: VD_AGENT_CLIPBOARD_GRAB")

	return nil
}
