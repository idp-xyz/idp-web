// Package refs makes and checks the references that tie a page address to the search that listed it (ADR-0001 D16,
// platform ADR-0003 D2 and 7.3 T1, T2). A ref is "<expiry, 8 hex digits>.<16 base32 characters of
// HMAC-SHA-256(key, address, caller, expiry)>", 25 characters, the form that was measured with a real model in phase 0.
//
// web_fetch reads an address only when its ref was made for that address and that caller and has not expired, so an
// address the model composed itself (with a secret in its path, say) has no valid ref and cannot be read. The ref
// is stateless: any replica that holds the key verifies what another made.
package refs

import (
	"bufio"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Lifetime of a ref: the longest a run lasts (platform ADR-0003 D2).
const Lifetime = time.Hour

// MinKeyBytes is the shortest key accepted: the size of the HMAC's output.
const MinKeyBytes = 32

const macChars = 16 // 80 bits

// Why a ref was refused. Each is a sentence the model can act on; the check never says more than which of the three it is.
var (
	ErrMalformed = errors.New("the ref is not a ref this service made")
	ErrExpired   = errors.New("the ref has expired")
	ErrNotForIt  = errors.New("the ref was not made for this address and caller")
)

// Keys signs with the first key and verifies with all of them, so that a key can be rotated without refusing the refs
// that are still alive: add the new key in front, remove the old one an hour later.
type Keys struct {
	keys [][]byte
	now  func() time.Time
}

// New returns Keys from the given keys, the first of which signs. Every key must hold at least MinKeyBytes.
func New(keys ...[]byte) (*Keys, error) {
	if len(keys) == 0 {
		return nil, errors.New("no ref key")
	}
	for i, k := range keys {
		if len(k) < MinKeyBytes {
			return nil, fmt.Errorf("ref key %d is %d bytes: at least %d are needed", i+1, len(k), MinKeyBytes)
		}
	}
	return &Keys{keys: keys, now: time.Now}, nil
}

// WithClock returns Keys that read the time from now, for tests that need a ref to expire.
func (k *Keys) WithClock(now func() time.Time) *Keys { return &Keys{keys: k.keys, now: now} }

// Load reads the keys from a file: one key a line, the first signs and all verify (rotation). Blank lines are
// skipped, and so is trailing white space, which an editor adds without meaning it.
func Load(path string) (*Keys, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var keys [][]byte
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 4096), 1<<16)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			keys = append(keys, []byte(line))
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return New(keys...)
}

// mac is the tag over the three parts. They are joined unambiguously (ADR-0001 D16): a fixed label, then each of the
// address and the caller with a 4-byte length in front, then the expiry as 4 bytes. No two different triples give the
// same bytes, so "a" + "bc" can never collide with "ab" + "c" (the reference's NUL separator would let an address
// that contains one do so).
func mac(key []byte, address, caller string, expiry uint32) string {
	h := hmac.New(sha256.New, key)
	h.Write([]byte("idp-web ref v1\x00"))
	var n [4]byte
	for _, part := range []string{address, caller} {
		binary.BigEndian.PutUint32(n[:], uint32(len(part)))
		h.Write(n[:])
		h.Write([]byte(part))
	}
	binary.BigEndian.PutUint32(n[:], expiry)
	h.Write(n[:])
	return strings.ToLower(base32.StdEncoding.EncodeToString(h.Sum(nil)))[:macChars]
}

// Make returns the ref of address for caller (tenant "/" subject).
func (k *Keys) Make(caller, address string) string {
	expiry := uint32(k.now().Add(Lifetime).Unix())
	return fmt.Sprintf("%08x.%s", expiry, mac(k.keys[0], address, caller, expiry))
}

func validMAC(s string) bool {
	if len(s) != macChars {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= '2' && c <= '7') {
			return false
		}
	}
	return true
}

// Check returns nil when ref was made by one of the keys for this address and caller and has not expired. Otherwise it
// returns ErrMalformed, ErrNotForIt or ErrExpired. A ref that is both wrong and old is "not for it": the expiry is
// only reported for a ref that is otherwise genuine, so the answer never confirms anything about a forged one.
// The tags are compared in constant time.
func (k *Keys) Check(caller, address, ref string) error {
	ref = strings.TrimSpace(ref)
	if len(ref) != 8+1+macChars || ref[8] != '.' || !validMAC(ref[9:]) {
		return ErrMalformed
	}
	exp, err := strconv.ParseUint(ref[:8], 16, 32)
	if err != nil {
		return ErrMalformed
	}
	good := 0
	for _, key := range k.keys {
		good |= subtle.ConstantTimeCompare([]byte(ref[9:]), []byte(mac(key, address, caller, uint32(exp))))
	}
	if good != 1 {
		return ErrNotForIt
	}
	if k.now().Unix() > int64(exp) {
		return ErrExpired
	}
	return nil
}
