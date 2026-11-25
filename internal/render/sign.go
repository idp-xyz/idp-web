package render

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"time"
)

// The caller assertion idp-web signs for every render request (ADR-0001 D33; idp-browser ADR-0001 D8). It is the form of
// idp-browser's internal/caller, whose verifier is the other half: a compact JWS, ES256, header {alg, kid, typ
// "idp-caller+jwt"}, claims aud (the render endpoint's address, exactly as configured), iat, exp (30 seconds later), jti
// (random), sub ("idp-web") and req, the digest of the request:
//
//	"sha256:" + hex(sha256(METHOD "\n" PATH "\n" BODY))
//
// with the body exactly as sent. It travels as "Authorization: Bearer <jws>". The renderer's vectors
// (testdata/caller-assertion-v1.json, copied from idp-browser) pin the digest and the layout, and the test double in
// internal/browsertest verifies with a copy of the renderer's verifier.
const (
	assertionType = "idp-caller+jwt"
	// Subject is the one caller the renderer accepts.
	Subject = "idp-web"
	// TTL is how long an assertion lives; the renderer accepts at most two minutes.
	TTL = 30 * time.Second
)

// Digest is what the signer signs about a request.
func Digest(method, path string, body []byte) string {
	h := sha256.New()
	h.Write([]byte(method + "\n" + path + "\n"))
	h.Write(body)
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// KeyID is the RFC 7638 thumbprint of a P-256 public key, which is what the renderer looks the key up by.
func KeyID(pub *ecdsa.PublicKey) (string, error) {
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

// Signer makes assertions with idp-web's private key.
type Signer struct {
	key *ecdsa.PrivateKey
	kid string
	// Now is the clock; time.Now unless a test sets it.
	Now func() time.Time
}

// NewSigner reads a P-256 private key (PEM, PKCS#8 or SEC1).
func NewSigner(pemBytes []byte) (*Signer, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("no PEM block")
	}
	var key *ecdsa.PrivateKey
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		var ok bool
		if key, ok = k.(*ecdsa.PrivateKey); !ok {
			return nil, errors.New("the key is not an EC key")
		}
	} else if k, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		key = k
	} else {
		return nil, errors.New("not a PKCS#8 or SEC1 private key")
	}
	if key.Curve != elliptic.P256() {
		return nil, errors.New("the key is not on P-256")
	}
	kid, err := KeyID(&key.PublicKey)
	if err != nil {
		return nil, err
	}
	return &Signer{key: key, kid: kid, Now: time.Now}, nil
}

// KeyID of the signing key, for the start-up line (the renderer's log names the keys it holds by the same value).
func (s *Signer) KeyID() string { return s.kid }

// Sign returns the assertion for one request to audience.
func (s *Signer) Sign(audience, method, path string, body []byte) (string, error) {
	now := s.Now()
	id := make([]byte, 12)
	if _, err := rand.Read(id); err != nil {
		return "", err
	}
	return s.sign(claims{Audience: audience, Issued: now.Unix(), Expires: now.Add(TTL).Unix(), ID: base64.RawURLEncoding.EncodeToString(id),
		Subject: Subject, Request: Digest(method, path, body)})
}

func (s *Signer) sign(c claims) (string, error) {
	enc := base64.RawURLEncoding.EncodeToString
	h, _ := json.Marshal(map[string]string{"alg": "ES256", "kid": s.kid, "typ": assertionType})
	p, _ := json.Marshal(c)
	signing := enc(h) + "." + enc(p)
	sum := sha256.Sum256([]byte(signing))
	r, ss, err := ecdsa.Sign(rand.Reader, s.key, sum[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	ss.FillBytes(sig[32:])
	return signing + "." + enc(sig), nil
}
