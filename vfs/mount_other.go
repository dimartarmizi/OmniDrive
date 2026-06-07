//go:build !windows
// +build !windows

package vfs

import (
	"fmt"

	"golang.org/x/oauth2"

	"omnidrive/database"
)

func Mount(store *database.Store, oauthConfig *oauth2.Config, mountPoint string) int {
	_ = store
	_ = oauthConfig
	_ = mountPoint
	fmt.Println("mount is only supported on Windows (WinFsp)")
	return 1
}
