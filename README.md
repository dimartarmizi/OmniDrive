# OmniDrive

OmniDrive is a Go prototype for a unified virtual Windows drive backed by multiple Google Drive accounts.

## Prerequisites

1. Install Go 1.21 or newer.
2. Install WinFsp for mounting the virtual drive on Windows.
3. Create a Google Cloud OAuth desktop/web client and download credentials as `credentials.json`.

## Quick start

From this folder:

```powershell
go mod tidy
go run omnidrive init
go run omnidrive add-account
go run omnidrive accounts
go run omnidrive set-priority --email primary@gmail.com --position 1
go run omnidrive sync
go run omnidrive list
```

Mounting requires WinFsp:

```powershell
go run omnidrive mount
```

On first `init`, OmniDrive creates `config.json` in the project folder with default values such as `credentials.json`, `Z:`, a 10-second refresh interval, automatic cache/staging cleanup on mount and unmount, automatic sync on mount, 5-second directory cache age, overwrite conflict policy, `info` log level, and `OmniDrive` as the mount label. After that, `add-account`, `sync`, and `mount` reuse those values from that file.

Available account management commands:

```powershell
go run omnidrive accounts
go run omnidrive set-priority --email primary@gmail.com --position 1
go run omnidrive remove-account --email old@gmail.com
go run omnidrive config --credentials .\credentials.json --mountpoint Y: --refresh-interval 30 --clean-on-mount=true --clean-on-unmount=false --auto-sync-on-mount=true --dir-cache-max-age 15 --conflict-policy overwrite --log-level info --mount-label OmniDrive
```

Example `config.json`:

```json
{
	"credentials_path": "credentials.json",
	"mount_point": "Z:",
	"refresh_interval_seconds": 10,
	"clean_cache_on_mount": true,
	"clean_cache_on_unmount": true,
	"auto_sync_on_mount": true,
	"dir_cache_max_age_seconds": 5,
	"conflict_policy": "overwrite",
	"log_level": "info",
	"mount_label": "OmniDrive"
}
```

Config tambahan:

- `auto_sync_on_mount`: jalankan refresh metadata otomatis saat mount.
- `dir_cache_max_age_seconds`: batas umur cache listing folder sebelum di-refresh.
- `conflict_policy`: `overwrite`, `deny`, atau `rename`.
- `log_level`: `debug`, `info`, atau `error`.
- `mount_label`: nama volume yang tampil di Explorer.

## Current filesystem behavior

- Directory listings and metadata come from the local SQLite cache populated by `sync`.
- File content is downloaded into `%LOCALAPPDATA%\OmniDrive\cache` when a file is opened.
- New or modified files are staged in `%LOCALAPPDATA%\OmniDrive\staging` and uploaded back to Google Drive when the file handle is released.
- Creating folders/files, renaming, and deleting from Explorer now forward to Google Drive and update the local cache.
- Reported filesystem capacity is aggregated from active Google Drive account quotas.

## Recommended usage flow

1. Run `add-account` for each Google account you want to merge.
2. Use `accounts` and `set-priority` to put the preferred upload account at the top.
3. Run `sync` before mounting so the virtual tree is populated from the metadata cache.
4. Mount the drive.
5. Open files to trigger downloads, then edit/save them normally from Explorer or desktop apps.
6. If you add files directly in Google Drive outside OmniDrive, run `sync` again to refresh the cache.

## Limitations

- Google-native document types are exported on download to common Office-compatible formats where possible.
- Upload currently writes the saved file bytes back as regular Drive files; native Google Docs round-trip editing is not preserved.
- Large-file resume/retry and background incremental sync are not implemented yet.

## Runtime data

By default data is stored in `%LOCALAPPDATA%\\OmniDrive\\omnidrive.db`.
Use `--db` to override the database path temporarily when needed.

Default command settings are stored in `config.json`.
