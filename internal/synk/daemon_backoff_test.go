package synk

import (
	"testing"
	"time"
)

func TestNextBackoff(t *testing.T) {
	max := 15 * time.Minute

	tests := []struct {
		name string
		cur  time.Duration
		max  time.Duration
		want time.Duration
	}{
		{name: "first failure", cur: 0, max: max, want: 15 * time.Second},
		{name: "negative current delay", cur: -time.Second, max: max, want: 15 * time.Second},
		{name: "doubles delay", cur: 15 * time.Second, max: max, want: 30 * time.Second},
		{name: "doubles again", cur: 30 * time.Second, max: max, want: time.Minute},
		{name: "caps delay at maximum", cur: 10 * time.Minute, max: max, want: max},
		{name: "already at maximum", cur: max, max: max, want: max},
		{name: "above maximum", cur: 20 * time.Minute, max: max, want: max},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := nextBackoff(tt.cur, tt.max); got != tt.want {
				t.Errorf("nextBackoff(%s, %s) = %s, want %s",
					tt.cur, tt.max, got, tt.want)
			}
		})
	}
}
