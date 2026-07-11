package logging

import (
	"context"
	"log/slog"
	"testing"
)

func TestNewLevel(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		in           string
		enabledAt    slog.Level // must be enabled
		disabledAt   slog.Level // must be disabled (use same as enabled to skip)
		skipDisabled bool
	}{
		{in: "debug", enabledAt: slog.LevelDebug, skipDisabled: true}, // nothing below debug to assert
		{in: "info", enabledAt: slog.LevelInfo, disabledAt: slog.LevelDebug},
		{in: "warn", enabledAt: slog.LevelWarn, disabledAt: slog.LevelInfo},
		{in: "error", enabledAt: slog.LevelError, disabledAt: slog.LevelWarn},
		// Normalization: case/space-insensitive.
		{in: "  ERROR ", enabledAt: slog.LevelError, disabledAt: slog.LevelWarn},
		// Unrecognized falls back to info.
		{in: "bogus", enabledAt: slog.LevelInfo, disabledAt: slog.LevelDebug},
		{in: "", enabledAt: slog.LevelInfo, disabledAt: slog.LevelDebug},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			h := New(tc.in).Handler()
			if !h.Enabled(ctx, tc.enabledAt) {
				t.Errorf("New(%q): level %v should be enabled", tc.in, tc.enabledAt)
			}
			if !tc.skipDisabled && h.Enabled(ctx, tc.disabledAt) {
				t.Errorf("New(%q): level %v should be disabled", tc.in, tc.disabledAt)
			}
		})
	}
}
