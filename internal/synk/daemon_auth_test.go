package synk

import (
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/kolesnikav/icloud-synk/internal/config"
	"github.com/kolesnikav/icloud-synk/internal/icloud"
)

func TestCheckAuthError(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		initial   bool
		wantLogin bool
	}{
		{
			name:      "login required sets state",
			err:       icloud.ErrLoginRequired,
			wantLogin: true,
		},
		{
			name:      "wrapped login required sets state",
			err:       errors.Join(errors.New("request failed"), icloud.ErrLoginRequired),
			wantLogin: true,
		},
		{
			name:      "repeated login error keeps state",
			err:       icloud.ErrLoginRequired,
			initial:   true,
			wantLogin: true,
		},
		{
			name:      "other error leaves state unchanged",
			err:       errors.New("network unavailable"),
			wantLogin: false,
		},
		{
			name:      "other error preserves existing login state",
			err:       errors.New("network unavailable"),
			initial:   true,
			wantLogin: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Defaults()
			cfg.Notify = false
			eng := &Engine{
				cfg: cfg,
				log: slog.New(slog.NewTextHandler(io.Discard, nil)),
			}
			d := &Daemon{
				eng: eng,
				log: eng.log,
			}
			d.needsLogin.Store(tt.initial)

			d.checkAuthError(tt.err)

			if got := d.needsLogin.Load(); got != tt.wantLogin {
				t.Errorf("needsLogin = %v, want %v", got, tt.wantLogin)
			}
		})
	}
}
