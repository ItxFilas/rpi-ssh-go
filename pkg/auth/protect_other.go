//go:build !windows

package auth

import "fmt"

// protectForUser leaves the data as it is: on these systems the cookie file
// is protected by its owner-only permissions.
func protectForUser(plain []byte) (sealed []byte, scheme string, err error) {
	return plain, "", nil
}

func unprotectForUser(sealed []byte, scheme string) ([]byte, error) {
	return nil, fmt.Errorf("cookie file uses protection %q, which is not available on this system", scheme)
}
