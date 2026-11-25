// Package browsertest is a stand-in for idp-browser's HTTP API, for tests: it verifies the caller assertion exactly as
// the renderer does and answers what the test scripts (ADR-0001 D33).
//
// verify.go is a COPY of the verifier of idp-browser's internal/caller/caller.go (Digest, keyID, claims, Verifier and
// its checks, unchanged except for the package name and the omission of the signer and the key generator, which idp-web
// has of its own in internal/render), so that a signature that this double accepts is one that the renderer accepts.
// testdata/caller-assertion-v1.json is idp-browser's vector file, copied: TestVectors in internal/render runs it
// against this verifier and against idp-web's own digest and layout.
package browsertest

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"
)

var maxSeen = 65536 // replay table size; a variable for the test

const (
	assertionType = "idp-caller+jwt"
	// Skew is how far the two clocks may differ.
	Skew = 30 * time.Second
	// MaxTTL is the longest life an assertion may claim; idp-web signs for 30 seconds.
	MaxTTL       = 2 * time.Minute
	maxAssertion = 4096
)

// ErrAssertion: the request carries no assertion this server accepts. The reason goes to the log, not to the caller.
var ErrAssertion = errors.New("the caller assertion is missing or invalid")

// Digest is what the signer signs about a request.
func Digest(method, path string, body []byte) string {
	h := sha256.New()
	h.Write([]byte(method + "\n" + path + "\n"))
	h.Write(body)
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

func keyID(pub *ecdsa.PublicKey) (string, error) {
	raw, err := pub.Bytes() // 0x04 || X || Y
	if err != nil || len(raw) != 65 {
		return "", errors.New("not an uncompressed P-256 point")
	}
	enc := base64.RawURLEncoding.EncodeToString
	sum := sha256.Sum256([]byte(`{"crv":"P-256","kty":"EC","x":"` + enc(raw[1:33]) + `","y":"` + enc(raw[33:]) + `"}`))
	return enc(sum[:]), nil
}

type claims struct {
	Audience string `json:"aud"`
	Issued   int64  `json:"iat"`
	Expires  int64  `json:"exp"`
	ID       string `json:"jti"`
	Subject  string `json:"sub"`
	Request  string `json:"req"`
}

// Verifier checks assertions.
type Verifier struct {
	keys     map[string]*ecdsa.PublicKey
	audience string
	subject  string
	now      func() time.Time

	mu   sync.Mutex
	seen map[string]int64
}

// NewVerifier reads one or more PEM public keys (PKIX, P-256). audience is this endpoint, the URL idp-web calls
// (for example http://idp-browser.idp-browser.svc.cluster.local:8590/render); subject is the one caller accepted.
func NewVerifier(pemBytes []byte, audience, subject string) (*Verifier, error) {
	if audience == "" || subject == "" {
		return nil, errors.New("no audience or no subject")
	}
	v := &Verifier{keys: map[string]*ecdsa.PublicKey{}, audience: audience, subject: subject, now: time.Now, seen: map[string]int64{}}
	for rest := pemBytes; ; {
		var block *pem.Block
		if block, rest = pem.Decode(rest); block == nil {
			break
		}
		k, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("not a PKIX public key: %w", err)
		}
		pub, ok := k.(*ecdsa.PublicKey)
		if !ok || pub.Curve != elliptic.P256() {
			return nil, errors.New("the key is not an EC key on P-256")
		}
		kid, err := keyID(pub)
		if err != nil {
			return nil, err
		}
		v.keys[kid] = pub
	}
	if len(v.keys) == 0 {
		return nil, errors.New("no public key found")
	}
	return v, nil
}

// SetClock replaces the clock (tests and vectors).
func (v *Verifier) SetClock(now func() time.Time) { v.now = now }

// KeyIDs of the accepted keys, for the operator's log.
func (v *Verifier) KeyIDs() []string {
	out := make([]string, 0, len(v.keys))
	for k := range v.keys {
		out = append(out, k)
	}
	return out
}

// Verify checks that idp-web signed assertion for this endpoint and for exactly this request, that it has not
// expired, and that it was not presented before. The error says why not (for the log).
func (v *Verifier) Verify(assertion, method, path string, body []byte) error {
	if assertion == "" {
		return errors.New("no assertion")
	}
	parts := strings.Split(assertion, ".")
	if len(parts) != 3 || len(assertion) > maxAssertion {
		return errors.New("not a compact JWS")
	}
	dec := base64.RawURLEncoding.DecodeString
	hraw, err1 := dec(parts[0])
	craw, err2 := dec(parts[1])
	sig, err3 := dec(parts[2])
	if err1 != nil || err2 != nil || err3 != nil {
		return errors.New("not base64url")
	}
	var h struct{ Alg, Typ, Kid string }
	var hh map[string]string
	if json.Unmarshal(hraw, &hh) != nil {
		return errors.New("header is not an object")
	}
	h.Alg, h.Typ, h.Kid = hh["alg"], hh["typ"], hh["kid"]
	if h.Alg != "ES256" || h.Typ != assertionType {
		return fmt.Errorf("header alg %q typ %q", h.Alg, h.Typ)
	}
	key := v.keys[h.Kid]
	if key == nil {
		return fmt.Errorf("signed with a key this server does not hold (kid %q)", h.Kid)
	}
	if len(sig) != 64 {
		return errors.New("signature length")
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(key, sum[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		return errors.New("signature does not verify")
	}
	var c claims
	if err := json.NewDecoder(bytes.NewReader(craw)).Decode(&c); err != nil {
		return errors.New("claims are not an object")
	}
	now := v.now()
	switch {
	case c.Audience != v.audience:
		return fmt.Errorf("made for %q, not for %s", c.Audience, v.audience)
	case c.Expires == 0 || now.After(time.Unix(c.Expires, 0).Add(Skew)):
		return errors.New("expired")
	case c.Issued == 0 || time.Unix(c.Issued, 0).After(now.Add(Skew)):
		return errors.New("issued in the future")
	case c.Expires < c.Issued || c.Expires-c.Issued > int64(MaxTTL/time.Second):
		return errors.New("valid for too long")
	case c.ID == "":
		return errors.New("no id")
	case c.Subject != v.subject:
		return fmt.Errorf("the caller is %q, only %q is accepted", c.Subject, v.subject)
	case c.Request != Digest(method, path, body):
		return errors.New("made for another request")
	}
	if !v.first(c.ID, c.Expires+int64(Skew/time.Second), now.Unix()) {
		return errors.New("presented before")
	}
	return nil
}

// first records id until forget and reports whether it was new. A full table of unexpired ids refuses (fails closed).
func (v *Verifier) first(id string, forget, now int64) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	if _, dup := v.seen[id]; dup {
		return false
	}
	if len(v.seen) >= maxSeen/2 {
		for k, t := range v.seen {
			if t < now {
				delete(v.seen, k)
			}
		}
		if len(v.seen) >= maxSeen {
			return false
		}
	}
	v.seen[id] = forget
	return true
}
