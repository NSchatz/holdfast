package node

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/NSchatz/holdfast/internal/config"
)

// DigestAlgorithm is the one digest algorithm the protocol speaks: sha-256, one of the two
// RFC 9530 lists as Active (https://www.rfc-editor.org/rfc/rfc9530.html, section 5; read
// 2026-10-03).
const DigestAlgorithm = "sha-256"

// ErrBadDigest is a Content-Digest value this build cannot read a sha-256 figure out of.
var ErrBadDigest = errors.New("node: not a readable sha-256 digest")

// FormatDigest renders a sha-256 sum as the RFC 9530 dictionary member a Content-Digest
// header carries: `sha-256=:<base64>:`. It is also the form a digest is recorded in.
func FormatDigest(sum []byte) string {
	return DigestAlgorithm + "=:" + base64.StdEncoding.EncodeToString(sum) + ":"
}

// ParseDigest reads the sha-256 member out of a Content-Digest (or Repr-Digest) field value
// and returns its 32 bytes. The field is a Structured Fields dictionary whose values are
// byte sequences, `:<base64>:` (RFC 9530 section 2 and RFC 8941 section 3.3.5); members for
// other algorithms are passed over. It refuses a value with no sha-256 member, with two,
// with one that is not a 32-byte byte sequence, and with parameters on it: a figure this
// build cannot read exactly is never guessed at.
func ParseDigest(value string) ([]byte, error) {
	var sum []byte
	for _, member := range strings.Split(value, ",") {
		key, val, ok := strings.Cut(strings.TrimSpace(member), "=")
		if !ok || key != DigestAlgorithm {
			continue
		}
		if sum != nil {
			return nil, fmt.Errorf("%w: it carries two %s members", ErrBadDigest, DigestAlgorithm)
		}
		if len(val) < 2 || val[0] != ':' || val[len(val)-1] != ':' {
			return nil, fmt.Errorf("%w: the %s member is not a byte sequence (:<base64>: between colons)", ErrBadDigest, DigestAlgorithm)
		}
		raw, err := base64.StdEncoding.DecodeString(val[1 : len(val)-1])
		if err != nil {
			return nil, fmt.Errorf("%w: the %s member is not base64", ErrBadDigest, DigestAlgorithm)
		}
		if len(raw) != sha256.Size {
			return nil, fmt.Errorf("%w: the %s member is %d bytes, not %d", ErrBadDigest, DigestAlgorithm, len(raw), sha256.Size)
		}
		sum = raw
	}
	if sum == nil {
		return nil, fmt.Errorf("%w: it carries no %s member", ErrBadDigest, DigestAlgorithm)
	}
	return sum, nil
}

// CanonicalDigest reads a digest value and returns it in the one form it is recorded and
// compared in.
func CanonicalDigest(value string) (string, error) {
	sum, err := ParseDigest(value)
	if err != nil {
		return "", err
	}
	return FormatDigest(sum), nil
}

// MaxOutputBytes is the largest output a lease on a source of that size admits: one byte
// less than the source, because the strictly-smaller gate accepts nothing else and bytes
// past that could only ever be discarded.
func MaxOutputBytes(sourceSize int64) int64 { return sourceSize - 1 }

// ArgsDigest is a digest of a leased argument list: the encoder, the options before the
// input and the options between the input and the output. A restarted server compares it
// with the list it re-derives, so a job whose plan moved is never taken for the leased one.
// Every string is written behind its length, so no two lists share an encoding.
func ArgsDigest(encoder string, pre, body []string) string {
	h := sha256.New()
	write := func(s string) {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(s)))
		h.Write(n[:])
		h.Write([]byte(s))
	}
	list := func(l []string) {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(l)))
		h.Write(n[:])
		for _, s := range l {
			write(s)
		}
	}
	write(encoder)
	list(pre)
	list(body)
	return FormatDigest(h.Sum(nil))
}

// ErrUnmapped is a source path no entry of the worker's path map covers.
var ErrUnmapped = errors.New("node: no worker_path_map entry covers the source path")

// MapSource translates a source path as the server names it into the path this worker
// reads it at, through the worker's path map. It REFUSES a path no entry covers: a path
// handed back unchanged because nothing matched would be read from wherever that name
// happens to lead on this host, and a wrong file encoded is a guess. A worker that mounts
// the library at the server's own paths says so with an entry mapping the directory to
// itself. A path that is not absolute and clean is refused for the reason the webhook
// intake refuses one: mapping a prefix is lexical, and resolving `..` is not.
func MapSource(m config.PathMap, serverPath string) (string, error) {
	if !strings.HasPrefix(serverPath, "/") || path.Clean(serverPath) != serverPath {
		return "", fmt.Errorf("%w: %q is not an absolute path in its clean form", ErrUnmapped, serverPath)
	}
	mapped, ok := m.MapMatched(serverPath)
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrUnmapped, serverPath)
	}
	return mapped, nil
}
