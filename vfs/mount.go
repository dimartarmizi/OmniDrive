//go:build windows
// +build windows

package vfs

import (
	"github.com/winfsp/cgofuse/fuse"

	"golang.org/x/oauth2"

	"omnidrive/config"
	"omnidrive/database"
)

func Mount(store *database.Store, oauthConfig *oauth2.Config, settings config.Settings) int {
	runtime := NewRuntime(store, oauthConfig, settings)
	host := fuse.NewFileSystemHost(New(runtime))
	runtime.attachHost(host)
	if host.Mount(settings.MountPoint, []string{"-o", "volname=" + settings.MountLabel}) {
		return 0
	}
	return -1
}
