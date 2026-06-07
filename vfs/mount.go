//go:build windows
// +build windows

package vfs

import (
	"github.com/winfsp/cgofuse/fuse"

	"golang.org/x/oauth2"

	"omnidrive/database"
)

func Mount(store *database.Store, oauthConfig *oauth2.Config, mountPoint string) int {
	runtime := NewRuntime(store, oauthConfig)
	host := fuse.NewFileSystemHost(New(runtime))
	runtime.attachHost(host)
	if host.Mount(mountPoint, []string{"-o", "volname=OmniDrive"}) {
		return 0
	}
	return -1
}
