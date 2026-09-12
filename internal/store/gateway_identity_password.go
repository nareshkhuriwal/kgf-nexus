package store

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"runtime"
	"strings"

	"golang.org/x/crypto/argon2"
)

// ErrPasswordMismatch means the presented password did not verify.
//
// Callers must map this onto the SAME opaque failure as "no such user", for the
// reason gateway_apikeys.go gives about ErrAPIKeyUnknown: distinguishing the two
// turns login into an oracle that confirms which accounts exist.
var ErrPasswordMismatch = errors.New("password does not match")

// ErrPasswordHashMalformed means a stored verifier could not be parsed. It is a
// data-integrity fault, not an authentication outcome, so it is distinct: a
// caller must not report a corrupt row as "wrong password" and send an operator
// hunting for a typo that is not there.
var ErrPasswordHashMalformed = errors.New("stored password hash is malformed")

// Argon2 parameters.
//
// WHY ARGON2ID AND NOT SHA-256. gateway_apikeys.go explains that plain SHA-256 is
// correct for an API key: the key is generated with full machine entropy, so
// there is no dictionary to attack, and a KDF on a path hit by every worker RPC
// would be self-inflicted denial of service. A PASSWORD is the exact opposite
// credential. It is chosen by a human, so it IS drawn from a dictionary, and a
// SHA-256 of it falls to an offline attack at billions of guesses per second if
// this table ever leaks. The KDF's cost is the entire defence, and it is
// affordable here precisely because logins are rare -- the property that makes a
// KDF wrong for API keys is the property passwords do not have.
//
// argon2id rather than bcrypt: it resists GPU and ASIC attack through memory
// hardness, which bcrypt's 4KB working set does not, and it is the current
// consensus choice. It is also already in the module graph via x/crypto, so this
// adds no dependency.
const (
	// argonTime is the number of passes. With 64MB of memory, 1 pass is the
	// RFC 9106 second-recommended profile and the one tuned for memory-hard
	// resistance rather than iteration count.
	argonTime = 1
	// argonMemory is 64 MiB per hash. This is the parameter that actually costs an
	// attacker: it bounds how many guesses fit on a GPU at once. It also bounds
	// OUR concurrency, which is why login must never be on a hot path -- a burst
	// of logins is a memory spike, and that is a deliberate, bounded trade.
	argonMemory = 64 * 1024
	// argonKeyLen and argonSaltLen are the sizes RFC 9106 recommends; 16 bytes of
	// salt is well beyond the point where collisions matter.
	argonKeyLen  = 32
	argonSaltLen = 16
)

// argonParallelism is derived from the machine rather than fixed, because the
// right value is "how many cores can work on one hash" and that is a property of
// where this runs. Capped at 4: beyond that the gain is marginal and the memory
// pressure from concurrent logins is not.
func argonParallelism() uint8 {
	n := runtime.NumCPU()
	if n > 4 {
		n = 4
	}
	if n < 1 {
		n = 1
	}
	return uint8(n)
}

// HashPassword derives the at-rest verifier for a human password.
//
// The result is a PHC string ($argon2id$v=19$m=...,t=...,p=...$salt$hash). That
// format is self-describing, which is the property that matters operationally:
// the parameters a hash was made with travel WITH the hash, so raising the cost
// later does not invalidate existing passwords -- old rows verify under their own
// recorded parameters and are re-hashed on next successful login.
//
// A fresh random salt per password means two users with the same password have
// different verifiers, so cracking one does not crack the other and a precomputed
// table is useless.
func HashPassword(plaintext string) (string, error) {
	if plaintext == "" {
		// An empty password must never produce a usable verifier: it would make an
		// account that anyone can log into look identical to a real one.
		return "", fmt.Errorf("hash password: %w", errors.New("password must not be empty"))
	}
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate password salt: %w", err)
	}
	p := argonParallelism()
	sum := argon2.IDKey([]byte(plaintext), salt, argonTime, argonMemory, p, argonKeyLen)

	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, p,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(sum),
	), nil
}

// VerifyPassword checks a presented password against a stored PHC verifier.
//
// The comparison is constant-time. A byte-by-byte compare leaks, through timing,
// how many leading bytes of the derived key were correct, which is enough to
// recover the key one byte at a time given enough attempts.
func VerifyPassword(encoded, presented string) error {
	params, salt, want, err := parsePHC(encoded)
	if err != nil {
		return err
	}
	got := argon2.IDKey([]byte(presented), salt, params.time, params.memory, params.parallelism, uint32(len(want)))
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return ErrPasswordMismatch
	}
	return nil
}

// PasswordNeedsRehash reports whether a stored verifier was made with weaker
// parameters than the current policy.
//
// Exists so raising the cost is actually effective: without it, a password hashed
// years ago under old parameters stays at that strength forever, and the upgrade
// protects only accounts created after it. The caller re-hashes on the next
// SUCCESSFUL login, which is the only moment the plaintext is available.
//
// A malformed hash reports true: it cannot be verified as adequate, and failing
// toward "replace it" is the safe direction.
func PasswordNeedsRehash(encoded string) bool {
	params, _, _, err := parsePHC(encoded)
	if err != nil {
		return true
	}
	return params.memory < argonMemory || params.time < argonTime
}

// argonParams are the cost parameters recovered from a stored hash.
type argonParams struct {
	memory      uint32
	time        uint32
	parallelism uint8
}

// parsePHC decodes the PHC string format.
//
// Written by hand rather than pulled from a library: the format is six
// fields, and the alternative is a dependency in the authentication path, which
// is the last place to take one for the sake of thirty lines.
func parsePHC(encoded string) (argonParams, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	// "", "argon2id", "v=19", "m=...,t=...,p=...", salt, hash
	if len(parts) != 6 || parts[0] != "" {
		return argonParams{}, nil, nil, fmt.Errorf("%w: expected 6 PHC fields, got %d", ErrPasswordHashMalformed, len(parts))
	}
	if parts[1] != "argon2id" {
		// Only argon2id is accepted. Silently verifying a weaker algorithm found in
		// the column would let a downgrade attack survive a migration.
		return argonParams{}, nil, nil, fmt.Errorf("%w: unsupported algorithm %q", ErrPasswordHashMalformed, parts[1])
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return argonParams{}, nil, nil, fmt.Errorf("%w: bad version field %q", ErrPasswordHashMalformed, parts[2])
	}
	if version != argon2.Version {
		return argonParams{}, nil, nil, fmt.Errorf("%w: argon2 version %d is not %d", ErrPasswordHashMalformed, version, argon2.Version)
	}

	var p argonParams
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.memory, &p.time, &p.parallelism); err != nil {
		return argonParams{}, nil, nil, fmt.Errorf("%w: bad parameter field %q", ErrPasswordHashMalformed, parts[3])
	}
	if p.memory == 0 || p.time == 0 || p.parallelism == 0 {
		// Zero cost would make argon2.IDKey panic or return instantly; either way a
		// row claiming zero work is corrupt, not a valid cheap hash.
		return argonParams{}, nil, nil, fmt.Errorf("%w: zero cost parameter in %q", ErrPasswordHashMalformed, parts[3])
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return argonParams{}, nil, nil, fmt.Errorf("%w: salt is not base64", ErrPasswordHashMalformed)
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return argonParams{}, nil, nil, fmt.Errorf("%w: hash is not base64", ErrPasswordHashMalformed)
	}
	if len(salt) == 0 || len(want) == 0 {
		return argonParams{}, nil, nil, fmt.Errorf("%w: empty salt or hash", ErrPasswordHashMalformed)
	}
	return p, salt, want, nil
}
