package main

import (
	"os"
	"time"
)

type config struct {
	ListenAddr       string
	Upstream         string
	DBPath           string
	OwnershipMapPath string
	DashboardAddr    string
	RawRetention     time.Duration
	RollUpInterval   time.Duration
}

func loadConfig() config {
	return config{
		ListenAddr:       getEnv("LISTEN_ADDR", ":53"),
		Upstream:         getEnv("UPSTREAM_DNS", "1.1.1.1:53"),
		DBPath:           getEnv("DB_PATH", "/data/independence-score.sqlite"),
		OwnershipMapPath: getEnv("OWNERSHIP_MAP_PATH", "/etc/independence-score/ownership-map.yaml"),
		DashboardAddr:    getEnv("DASHBOARD_ADDR", ":8080"),
		RawRetention:     getEnvDuration("RAW_RETENTION", 48*time.Hour),
		RollUpInterval:   getEnvDuration("ROLLUP_INTERVAL", 10*time.Second),
	}
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getEnvDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}
