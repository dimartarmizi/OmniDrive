package config

import (
	"os"
	"path/filepath"
)

func DefaultDataDir() string {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		base = "."
	}
	return filepath.Join(base, "OmniDrive")
}

func DefaultDBPath() string {
	return filepath.Join(DefaultDataDir(), "omnidrive.db")
}

func DefaultCacheDir() string {
	return filepath.Join(DefaultDataDir(), "cache")
}

func DefaultStagingDir() string {
	return filepath.Join(DefaultDataDir(), "staging")
}
