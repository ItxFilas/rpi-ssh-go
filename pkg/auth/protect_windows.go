//go:build windows

package auth

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// dpapiScheme names data encrypted with the Windows Data Protection API for
// the current user. File permissions mean little on Windows, so the cookie
// file is encrypted instead: only this user account can read it back.
const dpapiScheme = "dpapi"

func dataBlob(data []byte) windows.DataBlob {
	if len(data) == 0 {
		return windows.DataBlob{}
	}
	return windows.DataBlob{Size: uint32(len(data)), Data: &data[0]}
}

// takeBlob copies a blob allocated by the system and frees it.
func takeBlob(blob windows.DataBlob) []byte {
	if blob.Data == nil {
		return nil
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(blob.Data)))
	return append([]byte(nil), unsafe.Slice(blob.Data, blob.Size)...)
}

func protectForUser(plain []byte) (sealed []byte, scheme string, err error) {
	in := dataBlob(plain)
	var out windows.DataBlob
	if err := windows.CryptProtectData(&in, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return nil, "", fmt.Errorf("encrypt cookie file: %w", err)
	}
	return takeBlob(out), dpapiScheme, nil
}

func unprotectForUser(sealed []byte, scheme string) ([]byte, error) {
	if scheme != dpapiScheme {
		return nil, fmt.Errorf("cookie file uses unknown protection %q", scheme)
	}
	in := dataBlob(sealed)
	var out windows.DataBlob
	if err := windows.CryptUnprotectData(&in, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return nil, fmt.Errorf("decrypt cookie file: %w", err)
	}
	return takeBlob(out), nil
}
