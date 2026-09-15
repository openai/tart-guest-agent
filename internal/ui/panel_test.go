package ui

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cirruslabs/tart-guest-agent/internal/activity"
	"github.com/cirruslabs/tart-guest-agent/internal/settings"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFormatRecentNotificationsEmpty(t *testing.T) {
	text := FormatRecentNotifications(nil)
	assert.Contains(t, text, "No recent activity")
}

func TestFormatRecentNotificationsWithEvents(t *testing.T) {
	events := []activity.Event{
		{
			ID:        "test1",
			Timestamp: time.Now(),
			Category:  activity.CategoryClipboardText,
			Title:     "Copied 42 chars",
			Detail:    "Sample Text",
			Status:    "success",
		},
		{
			ID:        "test2",
			Timestamp: time.Now(),
			Category:  activity.CategoryFileTransfer,
			Title:     "Received screenshot.png",
			Detail:    "1.2MB",
			Status:    "success",
		},
	}

	text := FormatRecentNotifications(events)
	assert.Contains(t, text, "Recent Guest Agent Activity")
	assert.Contains(t, text, "Copied 42 chars")
	assert.Contains(t, text, "Received screenshot.png")
}

func TestCategoryIcons(t *testing.T) {
	assert.Equal(t, "📋", categoryIcon(activity.CategoryClipboardText, "success"))
	assert.Equal(t, "🖼️", categoryIcon(activity.CategoryClipboardImage, "success"))
	assert.Equal(t, "📁", categoryIcon(activity.CategoryFileTransfer, "success"))
	assert.Equal(t, "❌", categoryIcon(activity.CategorySystem, "error"))
	assert.Equal(t, "⚠️", categoryIcon(activity.CategorySystem, "warning"))
}

func TestEscapeAppleScriptString(t *testing.T) {
	// A hostile filename containing backslash before quote must escape backslash first
	input := `malicious\" filename \ with "quotes" and \n newlines`
	escaped := EscapeAppleScriptString(input)

	// \" in original must become \\\" so AppleScript parses it as literal backslash followed by escaped quote
	assert.Contains(t, escaped, `malicious\\\"`)
	assert.Contains(t, escaped, `\"quotes\"`)
	assert.Contains(t, escaped, `\\ with`)
}

func TestResolveDownloadsFolder_ValidCustom(t *testing.T) {
	tempCustom := t.TempDir()

	tempCfg := filepath.Join(tempCustom, "settings.json")
	t.Setenv("TART_GUEST_CONFIG", tempCfg)
	defer settings.Reset()

	s := settings.DefaultSettings()
	s.DownloadDir = tempCustom
	require.NoError(t, settings.Save(s))

	resolved := ResolveDownloadsFolder()
	assert.Equal(t, tempCustom, resolved)
	assert.DirExists(t, resolved)
}

func TestResolveDownloadsFolder_FallbackOnInaccessiblePath(t *testing.T) {
	tempBase := t.TempDir()

	// Create a regular file so trying to create a directory beneath it fails
	blockingFile := filepath.Join(tempBase, "blocker")
	require.NoError(t, os.WriteFile(blockingFile, []byte("data"), 0600))
	inaccessibleDir := filepath.Join(blockingFile, "cannot_create_dir")

	tempCfg := filepath.Join(tempBase, "settings.json")
	t.Setenv("TART_GUEST_CONFIG", tempCfg)
	defer settings.Reset()

	s := settings.DefaultSettings()
	s.DownloadDir = inaccessibleDir
	require.NoError(t, settings.Save(s))

	resolved := ResolveDownloadsFolder()
	// Must not return the inaccessible directory
	assert.NotEqual(t, inaccessibleDir, resolved)
	// Must return a directory that actually exists
	assert.DirExists(t, resolved)
}

