// Package scram computes PostgreSQL SCRAM-SHA-256 password verifiers on the
// client, as libpq's PQencryptPasswordConn (psql's \password) does, so a
// password write sends the verifier and never the password: a statement the
// server logs (log_statement, log_min_error_statement, pg_stat_statements,
// pg_stat_activity) then holds nothing a login can use as is.
//
// A verifier is what PostgreSQL stores in pg_authid.rolpassword:
//
//	SCRAM-SHA-256$<iterations>:<salt>$<StoredKey>:<ServerKey>
//
// in standard base64, from RFC 5802 over the password prepared by SASLprep as
// PostgreSQL prepares it (Normalize). The server stores a string of this form
// as it is, whatever password_encryption says.
package scram

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// DefaultIterations is PostgreSQL's scram_iterations default, and its fixed
// count before PostgreSQL 16 made it a setting.
const DefaultIterations = 4096

// SaltLen is the salt length PostgreSQL generates (SCRAM_DEFAULT_SALT_LEN).
const SaltLen = 16

// Prefix opens every SCRAM-SHA-256 verifier.
const Prefix = "SCRAM-SHA-256$"

// Normalize is password prepared as PostgreSQL prepares it before hashing —
// the server for a password sent in the clear, libpq for a verifier or a
// login — so a verifier made here is the one the server would have stored
// for the same password. It is src/common/saslprep.c's pg_saslprep, with its
// fallback: a password SASLprep prohibits, or that is not UTF-8, is used as
// it is.
//
//   - ASCII is returned as it is (control characters included).
//   - Map: a non-ASCII space (C.1.2) becomes U+0020, a B.1 character is
//     dropped. A password nothing is left of is used as it is.
//   - Normalize: NFKC.
//   - Prohibit: a C.1.2–C.9 character or one unassigned in Unicode 3.2
//     (A.1); bidi: with an R/AL character (D.1), no L character (D.2), and
//     R/AL first and last. As PostgreSQL does, these check the mapped
//     password, not its normalized form.
//
// pgx's own SCRAM login prepares a password with PRECIS OpaqueString (NFC),
// which differs from this for a password NFKC changes (full-width letters,
// ligatures, superscripts): such a password set through pgmo, like one set
// in the clear, logs in with psql and not with pgx.
func Normalize(password string) string {
	ascii := true
	for i := 0; i < len(password); i++ {
		if password[i] >= utf8.RuneSelf {
			ascii = false
			break
		}
	}
	// Not UTF-8 needs no check of its own (an invalid byte reads as U+FFFD,
	// which C.6 prohibits), but pg_saslprep makes one, and so does this.
	if ascii || !utf8.ValidString(password) {
		return password
	}
	mapped := make([]rune, 0, len(password))
	for _, r := range password {
		switch {
		case unicode.Is(nonASCIISpace, r):
			mapped = append(mapped, ' ')
		case unicode.Is(mappedToNothing, r):
		default:
			mapped = append(mapped, r)
		}
	}
	if len(mapped) == 0 {
		return password
	}
	randAL := false
	for _, r := range mapped {
		if unicode.Is(prohibited, r) || unicode.Is(unassigned, r) {
			return password
		}
		randAL = randAL || unicode.Is(randALCat, r)
	}
	if randAL {
		if slices.ContainsFunc(mapped, func(r rune) bool { return unicode.Is(lCat, r) }) ||
			!unicode.Is(randALCat, mapped[0]) || !unicode.Is(randALCat, mapped[len(mapped)-1]) {
			return password
		}
	}
	return norm.NFKC.String(string(mapped))
}

// Verifier is the SCRAM-SHA-256 verifier of password with salt and
// iterations. It panics on iterations < 1: callers pass DefaultIterations or
// a server's scram_iterations, which is never below 1.
func Verifier(password string, salt []byte, iterations int) string {
	salted, err := pbkdf2.Key(sha256.New, Normalize(password), salt, iterations, sha256.Size)
	if err != nil {
		panic(fmt.Sprintf("scram: pbkdf2: %v", err))
	}
	clientKey := hmacSHA256(salted, "Client Key")
	storedKey := sha256.Sum256(clientKey)
	serverKey := hmacSHA256(salted, "Server Key")
	b64 := base64.StdEncoding.EncodeToString
	return Prefix + strconv.Itoa(iterations) + ":" + b64(salt) + "$" + b64(storedKey[:]) + ":" + b64(serverKey)
}

// NewVerifier is Verifier with a fresh random salt of SaltLen bytes.
func NewVerifier(password string, iterations int) string {
	salt := make([]byte, SaltLen)
	_, _ = rand.Read(salt) // crypto/rand.Read never fails (Go 1.24+)
	return Verifier(password, salt, iterations)
}

// Parse splits a verifier into its iterations and salt, as the server's
// parse_scram_secret reads them; ok is false for a string of any other form.
func Parse(verifier string) (iterations int, salt []byte, ok bool) {
	rest, found := strings.CutPrefix(verifier, Prefix)
	if !found {
		return 0, nil, false
	}
	params, keys, found := strings.Cut(rest, "$")
	if !found {
		return 0, nil, false
	}
	iter, saltText, found := strings.Cut(params, ":")
	if !found {
		return 0, nil, false
	}
	iterations, err := strconv.Atoi(iter)
	if err != nil || iterations < 1 {
		return 0, nil, false
	}
	salt, err = base64.StdEncoding.DecodeString(saltText)
	if err != nil {
		return 0, nil, false
	}
	stored, server, found := strings.Cut(keys, ":")
	if !found || !isKey(stored) || !isKey(server) {
		return 0, nil, false
	}
	return iterations, salt, true
}

func isKey(s string) bool {
	b, err := base64.StdEncoding.DecodeString(s)
	return err == nil && len(b) == sha256.Size
}

// Verifies reports whether verifier is password's: computed again with the
// verifier's own salt and iterations, it reads the same.
func Verifies(verifier, password string) bool {
	iterations, salt, ok := Parse(verifier)
	return ok && hmac.Equal([]byte(Verifier(password, salt, iterations)), []byte(verifier))
}

func hmacSHA256(key []byte, msg string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(msg))
	return m.Sum(nil)
}
