//go:build !windows
// +build !windows

package vfs

import (
	"fmt"

	"golang.org/x/oauth2"

	"omnidrive/config"
	"omnidrive/database"
)

func Mount(store *database.Store, oauthConfig *oauth2.Config, settings config.Settings) int {
	_ = store
	_ = oauthConfig
	_ = settings
	fmt.Println("mount is only supported on Windows (WinFsp)")
	return 1
}
