//go:build windows

package config

import (
	"encoding/base64"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

func ProtectString(plain string) (string, error) {
	in := []byte(plain)
	var dataIn windows.DataBlob
	if len(in) > 0 {
		dataIn.Size = uint32(len(in))
		dataIn.Data = &in[0]
	}
	var dataOut windows.DataBlob
	if err := windows.CryptProtectData(&dataIn, nil, nil, 0, nil, 0, &dataOut); err != nil {
		return "", fmt.Errorf("protect token with DPAPI: %w", err)
	}
	defer windows.LocalFree(windows.Handle(uintptr(unsafe.Pointer(dataOut.Data))))
	protected := unsafeSlice(dataOut.Data, dataOut.Size)
	return base64.StdEncoding.EncodeToString(protected), nil
}

func UnprotectString(cipherText string) (string, error) {
	decoded, err := base64.StdEncoding.DecodeString(cipherText)
	if err != nil {
		return "", fmt.Errorf("decode protected token: %w", err)
	}
	var dataIn windows.DataBlob
	if len(decoded) > 0 {
		dataIn.Size = uint32(len(decoded))
		dataIn.Data = &decoded[0]
	}
	var dataOut windows.DataBlob
	if err := windows.CryptUnprotectData(&dataIn, nil, nil, 0, nil, 0, &dataOut); err != nil {
		return "", fmt.Errorf("unprotect token with DPAPI: %w", err)
	}
	defer windows.LocalFree(windows.Handle(uintptr(unsafe.Pointer(dataOut.Data))))
	plain := unsafeSlice(dataOut.Data, dataOut.Size)
	return string(plain), nil
}

func unsafeSlice(ptr *byte, size uint32) []byte {
	if ptr == nil || size == 0 {
		return nil
	}
	return unsafe.Slice(ptr, int(size))
}
