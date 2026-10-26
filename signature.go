package attos

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// Signed artifacts (docs: SIGNED_ARTIFACTS.md).
//
// The control plane can sign every dataset file with an Ed25519 key. The
// signature travels in the X-Attos-Signature response header, detached from the
// file, so the file format is unchanged and older SDKs ignore it:
//
//	X-Attos-Signature: alg=ed25519; kid=<key id>; sig=<base64 of 64 bytes>
//
// The signed message is "attos-artifact-v1\x00" + stashID + "\x00" + SHA-256(file),
// which binds the signature to one stash, so a valid file for another stash is
// rejected. Configure trusted public keys with WithSignatureKeys; from then on
// the SDK refuses any downloaded or cached file without a valid signature
// (fail closed), keeps serving the file it has, and counts the failure.

// SignatureHeader is the response header carrying the detached signature.
const SignatureHeader = "X-Attos-Signature"

var (
	// ErrSignatureMissing: signing is enforced but the response carried no signature.
	ErrSignatureMissing = errors.New("attos: dataset signature missing")
	// ErrSignatureInvalid: the signature is malformed, names an unknown key or does not verify.
	ErrSignatureInvalid = errors.New("attos: dataset signature invalid")
)

// WithSignatureKeys enforces signed artifacts: only files whose signature
// verifies against one of these public keys (by key id) are installed.
func WithSignatureKeys(keys map[string]ed25519.PublicKey) Option {
	return func(s *Synchronizer) {
		s.sigKeys = make(map[string]ed25519.PublicKey, len(keys))
		for k, v := range keys {
			s.sigKeys[k] = v
		}
	}
}

// ParsePublicKey decodes a base64 (standard or URL-safe, padded or not) Ed25519 public key.
func ParsePublicKey(b64 string) (ed25519.PublicKey, error) {
	b64 = strings.TrimRight(strings.TrimSpace(b64), "=")
	for _, enc := range []*base64.Encoding{base64.RawStdEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(b64); err == nil && len(b) == ed25519.PublicKeySize {
			return ed25519.PublicKey(b), nil
		}
	}
	return nil, errors.New("attos: public key must be 32 bytes, base64 encoded")
}

func signedMessage(stashID string, digest []byte) []byte {
	m := make([]byte, 0, 18+len(stashID)+1+len(digest))
	m = append(m, "attos-artifact-v1\x00"...)
	m = append(m, stashID...)
	m = append(m, 0)
	return append(m, digest...)
}

// SignArtifact signs a file digest the way the control plane does (for tests and tooling).
func SignArtifact(priv ed25519.PrivateKey, kid, stashID string, digest []byte) string {
	sig := ed25519.Sign(priv, signedMessage(stashID, digest))
	return fmt.Sprintf("alg=ed25519; kid=%s; sig=%s", kid, base64.StdEncoding.EncodeToString(sig))
}

func (s *Synchronizer) signingEnforced() bool { return len(s.sigKeys) > 0 }

// verifySignature checks header against digest for this stash.
func (s *Synchronizer) verifySignature(header string, digest []byte) error {
	if header == "" {
		return ErrSignatureMissing
	}
	var alg, kid, sig string
	for _, part := range strings.Split(header, ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch k {
		case "alg":
			alg = v
		case "kid":
			kid = v
		case "sig":
			sig = v
		}
	}
	if alg != "ed25519" {
		return fmt.Errorf("%w: unsupported algorithm %q", ErrSignatureInvalid, alg)
	}
	pub, ok := s.sigKeys[kid]
	if !ok {
		return fmt.Errorf("%w: unknown key id %q", ErrSignatureInvalid, kid)
	}
	raw, err := base64.StdEncoding.DecodeString(sig)
	if err != nil || len(raw) != ed25519.SignatureSize {
		return fmt.Errorf("%w: malformed signature", ErrSignatureInvalid)
	}
	if !ed25519.Verify(pub, signedMessage(s.datasetID, digest), raw) {
		return fmt.Errorf("%w: does not match the file", ErrSignatureInvalid)
	}
	return nil
}

func (s *Synchronizer) sigPath() string { return s.livePath() + ".sig" }

// verifyCached re-checks the cached live file against the signature saved with it.
func (s *Synchronizer) verifyCached(path string) error {
	hdr, err := os.ReadFile(s.sigPath())
	if err != nil {
		return ErrSignatureMissing
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	return s.verifySignature(strings.TrimSpace(string(hdr)), h.Sum(nil))
}
