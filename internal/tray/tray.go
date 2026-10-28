package tray

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/cirruslabs/tart-guest-agent/internal/activity"
	"github.com/cirruslabs/tart-guest-agent/internal/doctor"
	"github.com/cirruslabs/tart-guest-agent/internal/notify"
	"github.com/cirruslabs/tart-guest-agent/internal/settings"
	"github.com/cirruslabs/tart-guest-agent/internal/ui"
	"github.com/cirruslabs/tart-guest-agent/internal/version"
	"go.uber.org/zap"
)

// ErrQuit signals that the user requested to quit the tray service from the menu.
var ErrQuit = errors.New("tray: quit requested")

// defaultMenuPresenter is the default menu rendering function used by New().
var defaultMenuPresenter = ui.ShowTrayMenu

// Status represents the operational state of the guest agent.
type Status string

const (
	StatusActive    Status = "Active"
	StatusConnected Status = "Connected"
	StatusStandby   Status = "Standby"
	StatusError     Status = "Error"
)

// Tray manages the system tray / menu bar item and interactive actions.
type Tray struct {
	mu            sync.RWMutex
	status        Status
	menuPresenter func(context.Context, []string) (string, error)
}

// New creates a new Tray controller.
func New() *Tray {
	return &Tray{
		status:        StatusActive,
		menuPresenter: defaultMenuPresenter,
	}
}

// SetStatus updates the status label shown in the tray menu.
func (t *Tray) SetStatus(s Status) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.status = s
	activity.Record(activity.CategorySystem, fmt.Sprintf("Status changed to %s", s), "", "info")
}

// Status returns the current operational status.
func (t *Tray) Status() Status {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.status
}

// MenuItems returns the list of formatted menu items for display.
func (t *Tray) MenuItems() []string {
	t.mu.RLock()
	defer t.mu.RUnlock()

	statusIcon := "🟢"
	if t.status == StatusError {
		statusIcon = "🔴"
	} else if t.status == StatusStandby {
		statusIcon = "🟡"
	}

	return []string{
		fmt.Sprintf("⚡ Tart Guest Agent (%s)", version.Version),
		fmt.Sprintf("%s Status: %s", statusIcon, t.status),
		"---",
		"🔔 Recent Activity & Notifications...",
		"⚙️ Settings & Preferences...",
		"🩺 Run Diagnostics (Doctor)...",
		"📂 Open Downloads Folder",
		"---",
		"❌ Quit",
	}
}

// ActionForMenuItem maps a rendered MenuItems() label or action name to its canonical action key.
func ActionForMenuItem(choice string) string {
	lower := strings.ToLower(strings.TrimSpace(choice))
	switch {
	case lower == "":
		return ""
	case lower == "notifications" || lower == "activity" || strings.Contains(lower, "notification") || strings.Contains(lower, "recent activity"):
		return "notifications"
	case lower == "settings" || strings.Contains(lower, "setting") || strings.Contains(lower, "preference"):
		return "settings"
	case lower == "doctor" || strings.Contains(lower, "diagnostic") || strings.Contains(lower, "doctor"):
		return "doctor"
	case lower == "downloads" || strings.Contains(lower, "download"):
		return "downloads"
	case lower == "quit" || lower == "exit" || strings.Contains(lower, "quit"):
		return "quit"
	default:
		return ""
	}
}

// HandleAction executes the requested action triggered from the tray menu.
func (t *Tray) HandleAction(action string) error {
	zap.S().Debugf("Tray menu action triggered: %s", action)

	canonical := ActionForMenuItem(action)
	if canonical == "" {
		canonical = action
	}

	switch canonical {
	case "notifications", "activity":
		return ui.ShowNotificationsPanel()
	case "settings":
		return ui.ShowSettingsDialog()
	case "doctor":
		report := doctor.RunDiagnosticsWithSelfTest(true)
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("=== Diagnostic Report (%s %s) ===\n\n", report.OS, report.Arch))
		for _, check := range report.Checks {
			sb.WriteString(fmt.Sprintf("%s %s: %s\n", check.Status, check.Name, check.Summary))
			if check.Remediation != "" {
				sb.WriteString(fmt.Sprintf("   ↳ Action: %s\n", check.Remediation))
			}
		}
		return ui.ShowDoctorDialog(sb.String(), report.Overall.String())
	case "downloads":
		return ui.OpenDownloadsFolder()
	case "quit", "exit":
		return ErrQuit
	default:
		return fmt.Errorf("unknown tray action: %s", action)
	}
}

// actionCh provides an action-dispatch queue to the active tray controller.
var actionCh = make(chan string, 16)

// Dispatch sends an action to the running tray controller event loop.
func Dispatch(action string) {
	select {
	case actionCh <- action:
	default:
		zap.S().Warnf("tray: action queue full, dropped action: %s", action)
	}
}

// Run starts the tray controller, renders the interactive menu loop when a GUI session is present,
// dispatches selected menu actions via Dispatch, and listens for context cancellation.
func (t *Tray) Run(ctx context.Context) error {
	zap.S().Infof("Starting Tart Guest Agent tray service (version %s)", version.Version)

	// Emit startup notification if enabled
	s := settings.Get()
	if s.StartupNotification && s.NotificationsEnabled {
		_ = notify.Send("Tart Guest Agent", "Agent active — Multi-format clipboard and SPICE file transfer ready.", notify.UrgencyLow)
	}

	activity.Record(activity.CategorySystem, "Tart Guest Agent started", fmt.Sprintf("Version %s", version.Version), "success")

	loopCtx, cancelLoop := context.WithCancel(ctx)
	defer cancelLoop()

	menuReadyCh := make(chan struct{}, 1)
	menuReadyCh <- struct{}{}

	t.mu.RLock()
	presenter := t.menuPresenter
	t.mu.RUnlock()
	if presenter == nil {
		presenter = defaultMenuPresenter
	}

	go func() {
		for {
			select {
			case <-loopCtx.Done():
				return
			case <-menuReadyCh:
			}

			choice, err := presenter(loopCtx, t.MenuItems())
			if err != nil {
				if loopCtx.Err() == nil && !errors.Is(err, ui.ErrMenuDismissed) {
					zap.S().Debugf("tray: menu presenter exited: %v", err)
				}
				return
			}

			action := ActionForMenuItem(choice)
			if action == "" {
				select {
				case menuReadyCh <- struct{}{}:
				default:
				}
				continue
			}

			Dispatch(action)
		}
	}()

	// Run tray action-dispatch and lifecycle event loop
	for {
		select {
		case <-ctx.Done():
			zap.S().Infof("Stopping Tart Guest Agent tray service")
			return ctx.Err()
		case act := <-actionCh:
			if err := t.HandleAction(act); err != nil {
				if errors.Is(err, ErrQuit) {
					zap.S().Infof("Stopping Tart Guest Agent tray service (quit requested)")
					return nil
				}
				zap.S().Warnf("tray: error handling action %q: %v", act, err)
			}
			select {
			case menuReadyCh <- struct{}{}:
			default:
			}
		}
	}
}

// EmitStartupToast emits a desktop notification on agent startup.
func EmitStartupToast() {
	notify.StartupToast()
}

// EmitFileTransferNotification emits a notification when a file transfer completes.
func EmitFileTransferNotification(fileName string, fileSize int64) {
	notify.FileTransferCompleted(fileName, fileSize)
}

// EmitClipboardSyncNotification records a clipboard sync event.
func EmitClipboardSyncNotification(format string, size int) {
	notify.ClipboardSync(format, size)
}
