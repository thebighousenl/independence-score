package main

import (
	"bytes"
	"log"
	"os"
	"strings"
	"testing"
	"time"
)

func TestGetEnvDuration(t *testing.T) {
	for _, tc := range []struct {
		name      string
		value     string
		want      time.Duration
		wantWarn  bool
		warnNeeds []string
	}{
		{name: "unset uses default", value: "", want: 48 * time.Hour},
		{name: "valid value is used", value: "6h30m", want: 6*time.Hour + 30*time.Minute},
		{
			// "7d" is the value a user is most likely to reach for when
			// asking for a week of retention, and Go does not accept it.
			name:      "day unit is rejected and reported",
			value:     "7d",
			want:      48 * time.Hour,
			wantWarn:  true,
			warnNeeds: []string{"RAW_RETENTION", "7d", "48h"},
		},
		{
			name:      "garbage is rejected and reported",
			value:     "forever",
			want:      48 * time.Hour,
			wantWarn:  true,
			warnNeeds: []string{"RAW_RETENTION", "forever", "48h"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("RAW_RETENTION", tc.value)

			var logged bytes.Buffer
			log.SetOutput(&logged)
			defer log.SetOutput(os.Stderr)

			got := getEnvDuration("RAW_RETENTION", 48*time.Hour)
			if got != tc.want {
				t.Errorf("getEnvDuration(%q) = %s, want %s", tc.value, got, tc.want)
			}

			out := logged.String()
			if tc.wantWarn {
				// A silent fallback is the failure this guards against:
				// the user would see a dashboard built on a retention
				// they did not ask for and nothing would say so.
				if out == "" {
					t.Fatalf("getEnvDuration(%q) fell back to the default without logging anything", tc.value)
				}
				for _, want := range tc.warnNeeds {
					if !strings.Contains(out, want) {
						t.Errorf("warning %q does not mention %q", strings.TrimSpace(out), want)
					}
				}
			} else if out != "" {
				t.Errorf("getEnvDuration(%q) logged %q, want no output", tc.value, strings.TrimSpace(out))
			}
		})
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	for _, key := range []string{
		"LISTEN_ADDR", "UPSTREAM_DNS", "DB_PATH", "OWNERSHIP_MAP_PATH",
		"DASHBOARD_ADDR", "RAW_RETENTION", "ROLLUP_INTERVAL",
	} {
		t.Setenv(key, "")
	}

	cfg := loadConfig()

	// These defaults are documented in the README's configuration table and
	// set again in the Dockerfile; a change here that is not made there
	// turns the table into a wrong answer for a first-time reader.
	for _, tc := range []struct{ name, got, want string }{
		{"LISTEN_ADDR", cfg.ListenAddr, ":53"},
		{"UPSTREAM_DNS", cfg.Upstream, "1.1.1.1:53"},
		{"DB_PATH", cfg.DBPath, "/data/independence-score.sqlite"},
		{"OWNERSHIP_MAP_PATH", cfg.OwnershipMapPath, "/etc/independence-score/ownership-map.yaml"},
		{"DASHBOARD_ADDR", cfg.DashboardAddr, ":8080"},
		{"RAW_RETENTION", cfg.RawRetention.String(), "48h0m0s"},
		{"ROLLUP_INTERVAL", cfg.RollUpInterval.String(), "10s"},
	} {
		if tc.got != tc.want {
			t.Errorf("default %s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}
