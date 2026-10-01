package imapdrv

import (
	"errors"

	"github.com/CaffeinatedTech/go-imap"
)

// IsAuthError reports whether err is the server refusing our credentials:
// the RFC 5530 AUTHENTICATIONFAILED / AUTHORIZATIONFAILED response codes
// (Dovecot, Gmail and cPanel all send them, including on a rejected
// XOAUTH2 token). The library's error type stays inside this package
// (D-7): callers see only a bool.
//
// Servers that answer a failed LOGIN with a bare "NO" and no response
// code cannot be classified here; such a server will leave an account
// unready rather than auth-failed, which is the safe direction.
func IsAuthError(err error) bool {
	var imapErr *imap.Error
	if !errors.As(err, &imapErr) {
		return false
	}
	switch imapErr.Code {
	case imap.CodeAuthenticationFailed, imap.CodeAuthorizationFailed:
		return true
	default:
		return false
	}
}
