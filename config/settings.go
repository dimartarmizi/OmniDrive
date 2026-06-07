package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

type Settings struct {
	CredentialsPath        string `json:"credentials_path"`
	MountPoint             string `json:"mount_point"`
	RefreshIntervalSeconds int    `json:"refresh_interval_seconds"`
	CleanCacheOnMount      bool   `json:"clean_cache_on_mount"`
	CleanCacheOnUnmount    bool   `json:"clean_cache_on_unmount"`
	AutoSyncOnMount        bool   `json:"auto_sync_on_mount"`
	DirCacheMaxAgeSeconds  int    `json:"dir_cache_max_age_seconds"`
	ConflictPolicy         string `json:"conflict_policy"`
	LogLevel               string `json:"log_level"`
	MountLabel             string `json:"mount_label"`
}

func DefaultConfigPath() string {
	return "config.json"
}

func DefaultSettings() Settings {
	return Settings{
		CredentialsPath:        "credentials.json",
		MountPoint:             "Z:",
		RefreshIntervalSeconds: 10,
		CleanCacheOnMount:      true,
		CleanCacheOnUnmount:    true,
		AutoSyncOnMount:        true,
		DirCacheMaxAgeSeconds:  5,
		ConflictPolicy:         "overwrite",
		LogLevel:               "info",
		MountLabel:             "OmniDrive",
	}
}

func NormalizeSettings(settings Settings) Settings {
	defaults := DefaultSettings()
	if settings.CredentialsPath == "" {
		settings.CredentialsPath = defaults.CredentialsPath
	}
	if settings.MountPoint == "" {
		settings.MountPoint = defaults.MountPoint
	}
	if settings.RefreshIntervalSeconds < 0 {
		settings.RefreshIntervalSeconds = defaults.RefreshIntervalSeconds
	}
	if settings.DirCacheMaxAgeSeconds < 0 {
		settings.DirCacheMaxAgeSeconds = defaults.DirCacheMaxAgeSeconds
	}
	switch settings.ConflictPolicy {
	case "", "overwrite":
		if settings.ConflictPolicy == "" {
			settings.ConflictPolicy = defaults.ConflictPolicy
		}
	case "deny", "rename":
	default:
		settings.ConflictPolicy = defaults.ConflictPolicy
	}
	switch settings.LogLevel {
	case "", "debug", "info", "error":
		if settings.LogLevel == "" {
			settings.LogLevel = defaults.LogLevel
		}
	default:
		settings.LogLevel = defaults.LogLevel
	}
	if settings.MountLabel == "" {
		settings.MountLabel = defaults.MountLabel
	}
	return settings
}

func LoadSettings() (Settings, error) {
	path := DefaultConfigPath()
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Settings{}, nil
		}
		return Settings{}, err
	}

	var settings Settings
	if err := json.Unmarshal(data, &settings); err != nil {
		return Settings{}, err
	}
	return NormalizeSettings(settings), nil
}

func SaveSettings(settings Settings) error {
	settings = NormalizeSettings(settings)
	path := DefaultConfigPath()
	dir := filepath.Dir(path)
	if dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}

	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}

func EnsureSettingsFile(existing Settings) (Settings, error) {
	settings := DefaultSettings()
	if existing.CredentialsPath != "" {
		settings.CredentialsPath = existing.CredentialsPath
	}
	if existing.MountPoint != "" {
		settings.MountPoint = existing.MountPoint
	}
	if existing.RefreshIntervalSeconds > 0 {
		settings.RefreshIntervalSeconds = existing.RefreshIntervalSeconds
	}
	if existing.CleanCacheOnMount != settings.CleanCacheOnMount {
		settings.CleanCacheOnMount = existing.CleanCacheOnMount
	}
	if existing.CleanCacheOnUnmount != settings.CleanCacheOnUnmount {
		settings.CleanCacheOnUnmount = existing.CleanCacheOnUnmount
	}
	if existing.AutoSyncOnMount != settings.AutoSyncOnMount {
		settings.AutoSyncOnMount = existing.AutoSyncOnMount
	}
	if existing.DirCacheMaxAgeSeconds > 0 {
		settings.DirCacheMaxAgeSeconds = existing.DirCacheMaxAgeSeconds
	}
	if existing.ConflictPolicy != "" {
		settings.ConflictPolicy = existing.ConflictPolicy
	}
	if existing.LogLevel != "" {
		settings.LogLevel = existing.LogLevel
	}
	if existing.MountLabel != "" {
		settings.MountLabel = existing.MountLabel
	}

	if _, err := os.Stat(DefaultConfigPath()); err == nil {
		return NormalizeSettings(settings), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return Settings{}, err
	}

	if err := SaveSettings(settings); err != nil {
		return Settings{}, err
	}
	return NormalizeSettings(settings), nil
}
