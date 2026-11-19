package tools

import (
	"crypto/sha256"

	"idp-web/internal/addr"
)

func sha256Sum(s string) [32]byte { return sha256.Sum256([]byte(s)) }

func errAddrRefused() error { return addr.ErrRefused }
