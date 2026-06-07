//go:build !windows

package config

import "fmt"

func ProtectString(plain string) (string, error) {
	return "", fmt.Errorf("DPAPI token protection is only available on Windows")
}

func UnprotectString(cipherText string) (string, error) {
	return "", fmt.Errorf("DPAPI token unprotection is only available on Windows")
}
