// Package auth contains the security functions of Squared: password
// hashes, random tokens and token hashes.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters. They follow the OWASP password storage
// recommendation (19 MiB memory, 2 iterations, 1 thread).
const (
	argonTime    = 2
	argonMemory  = 19 * 1024 // KiB
	argonThreads = 1
	argonKeyLen  = 32
	saltLen      = 16
)

var errBadHash = errors.New("bad password hash format")

// hashSlots limits the number of Argon2id operations at the same time.
// Each operation uses 19 MiB of memory. Without a limit, many sign-in
// requests at the same time can use all memory of the process.
var hashSlots = make(chan struct{}, 4)

func argonKey(password, salt []byte, time, memory uint32, threads uint8, keyLen uint32) []byte {
	hashSlots <- struct{}{}
	defer func() { <-hashSlots }()
	return argon2.IDKey(password, salt, time, memory, threads, keyLen)
}

// HashPassword returns an Argon2id hash of password in the PHC string
// format, for example "$argon2id$v=19$m=19456,t=2,p=1$<salt>$<hash>".
func HashPassword(password string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("make salt: %w", err)
	}
	key := argonKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, argonMemory, argonTime,
		argonThreads, b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

var b64 = base64.RawStdEncoding

// CheckPassword tells if password agrees with hash. The comparison uses
// constant time.
func CheckPassword(hash, password string) (bool, error) {
	parts := strings.Split(hash, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, errBadHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, errBadHash
	}
	var memory, time uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &time, &threads); err != nil {
		return false, errBadHash
	}
	// Limit the parameters. A bad hash in the database must not make the
	// process use too much memory.
	if memory == 0 || memory > 256*1024 || time == 0 || time > 10 || threads == 0 || threads > 8 {
		return false, errBadHash
	}
	salt, err := b64.DecodeString(parts[4])
	if err != nil {
		return false, errBadHash
	}
	want, err := b64.DecodeString(parts[5])
	if err != nil || len(want) == 0 || len(want) > 64 {
		return false, errBadHash
	}
	got := argonKey([]byte(password), salt, time, memory, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// dummyHash is a valid hash of a random password. Sign-in uses it when the
// user does not exist, so that the response time does not tell if the
// user exists.
var dummyHash = func() string {
	h, err := HashPassword(rand.Text())
	if err != nil {
		panic(err) // Only possible if the system has no random source.
	}
	return h
}()

// CheckDummyPassword uses the same time as CheckPassword. The result is
// always false.
func CheckDummyPassword(password string) {
	_, _ = CheckPassword(dummyHash, password)
}

// NewToken returns a new random token with a prefix, for example
// "sqp_KX3...". The prefix tells the type of the token. The random part has
// 160 bits of entropy.
func NewToken(prefix string) string {
	return prefix + rand.Text() + rand.Text()[:6]
}

// HashToken returns the SHA-256 hash of a token. The database keeps only
// token hashes. A fast hash is correct here because tokens are random and
// long, unlike passwords.
func HashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}
