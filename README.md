# OmniDrive

OmniDrive is a Go prototype for a unified virtual Windows drive backed by multiple Google Drive accounts.

## Current scope

This repository implements the first practical milestone from `blueprint.md`:

- OAuth2 add-account flow with a local callback server.
- DPAPI encryption for refresh tokens on Windows.
- Local SQLite cache for accounts and virtual file metadata.
- Multi-account Google Drive listing and cache merge with name-conflict display names.
- Smart account selection by largest free quota for future uploads.
- WinFsp/cgofuse VFS adapter with cache-backed directory reads, on-demand downloads, staged writes, folder creation, rename, delete, and quota reporting.

## Prerequisites

1. Install Go 1.21 or newer.
2. Install WinFsp for mounting the virtual drive on Windows.
3. Create a Google Cloud OAuth desktop/web client and download credentials as `credentials.json`.

## Quick start

From this folder:

```powershell
go mod tidy
go run .\cmd\omnidrive init
go run .\cmd\omnidrive add-account --credentials .\credentials.json
go run .\cmd\omnidrive sync --credentials .\credentials.json
go run .\cmd\omnidrive list
```

Mounting requires WinFsp:

```powershell
go run .\cmd\omnidrive mount --mountpoint Z:
```

## Current filesystem behavior

- Directory listings and metadata come from the local SQLite cache populated by `sync`.
- File content is downloaded into `%LOCALAPPDATA%\OmniDrive\cache` when a file is opened.
- New or modified files are staged in `%LOCALAPPDATA%\OmniDrive\staging` and uploaded back to Google Drive when the file handle is released.
- Creating folders/files, renaming, and deleting from Explorer now forward to Google Drive and update the local cache.
- Reported filesystem capacity is aggregated from active Google Drive account quotas.

## Recommended usage flow

1. Run `add-account` for each Google account you want to merge.
2. Run `sync` before mounting so the virtual tree is populated from the metadata cache.
3. Mount the drive.
4. Open files to trigger downloads, then edit/save them normally from Explorer or desktop apps.
5. If you add files directly in Google Drive outside OmniDrive, run `sync` again to refresh the cache.

## Limitations

- Google-native document types are exported on download to common Office-compatible formats where possible.
- Upload currently writes the saved file bytes back as regular Drive files; native Google Docs round-trip editing is not preserved.
- Large-file resume/retry and background incremental sync are not implemented yet.

## Runtime data

By default data is stored in `%LOCALAPPDATA%\\OmniDrive\\omnidrive.db`.
Use `--db` to override the database path.
