package scram

import (
	"encoding/base64"
	"strings"
	"testing"
	"unicode"
)

// The vectors are computed independently (Python's hashlib.pbkdf2_hmac and
// hmac over RFC 7677's salt), not by this package.
func TestVerifierVectors(t *testing.T) {
	salt, _ := base64.StdEncoding.DecodeString("W22ZaJ0SNY7soEsUEjb6gQ==")
	for _, c := range []struct {
		password   string
		iterations int
		want       string
	}{
		{"pencil", 4096, "SCRAM-SHA-256$4096:W22ZaJ0SNY7soEsUEjb6gQ==$WG5d8oPm3OtcPnkdi4Uo7BkeZkBFzpcXkuLmtbsT4qY=:wfPLwcE6nTWhTAmQ7tl2KeoiWGPlZqQxSrmfPwDl2dU="},
		{"pencil", 1, "SCRAM-SHA-256$1:W22ZaJ0SNY7soEsUEjb6gQ==$bzcn5wYzlcMpEXczzDM1iuyLhni5BVbqsm82vjMHWXI=:fg/vS0Y425LcbLGWSqdzrFlRn9451QblzgpwLQYoXCI="},
	} {
		if got := Verifier(c.password, salt, c.iterations); got != c.want {
			t.Errorf("Verifier(%q, %d) = %s; want %s", c.password, c.iterations, got, c.want)
		}
	}
}

// normalizeCases walks each step of pg_saslprep (src/common/saslprep.c),
// with its answer. pgmo's TestLiveRolePasswordMatchesServer holds a server's
// own hashing to the same inputs.
var normalizeCases = []struct{ name, in, want string }{
	{"ascii", "pencil", "pencil"},
	{"ascii control kept", "a\x01b\x7f", "a\x01b\x7f"},
	{"non-ascii space mapped", "a\u00a0b\u3000c", "a b c"},
	{"mapped to nothing", "pa\u00adss\u200dword", "password"},
	{"u+200b is a space before it is nothing", "a\u200bb", "a b"},
	{"nfkc", "\uff21\ufb01\u00b2", "Afi2"},
	{"nfkc composes", "e\u0301", "\u00e9"},
	{"latin-1 letter", "p\u00e4ss", "p\u00e4ss"},
	{"only mapped to nothing: as is", "\u00ad\ufeff", "\u00ad\ufeff"},
	{"prohibited: as is", "\u00e4\ue000", "\u00e4\ue000"},
	{"non-ascii control: as is", "\u00e4\u0085", "\u00e4\u0085"},
	{"unassigned in 3.2: as is", "\u00e4\u0221", "\u00e4\u0221"},
	{"checks the mapped form: \u2474 is not prohibited, its nfkc '(1)' is fine", "\u2474", "(1)"},
	{"rtl", "\u05d0\u05d1", "\u05d0\u05d1"},
	{"rtl with digits inside", "\u05d01\u05d1", "\u05d01\u05d1"},
	{"rtl mixed with ltr: as is", "\u05d0a\u05d1", "\u05d0a\u05d1"},
	{"rtl not last: as is", "\u05d0\u05d11", "\u05d0\u05d11"},
	{"not utf-8: as is", "\xff\u00e4", "\xff\u00e4"},
	// Each refusal where normalizing would have changed the password, so
	// the fallback shows.
	{"unassigned in 3.2, nfkc would change: as is", "\u00b2\u0221", "\u00b2\u0221"},
	{"rtl with ltr, nfkc would change: as is", "\u05d0\uff21\u05d1", "\u05d0\uff21\u05d1"},
	{"rtl not first, nfkc would change: as is", "1\u05d0\u00b2\u05d1", "1\u05d0\u00b2\u05d1"},
	{"rtl not last, nfkc would change: as is", "\u05d0\u00b2\u05d11", "\u05d0\u00b2\u05d11"},
	{"rtl, nfkc applied", "\u05d0\u00b2\u05d1", "\u05d02\u05d1"},
}

func TestNormalize(t *testing.T) {
	for _, c := range normalizeCases {
		if got := Normalize(c.in); got != c.want {
			t.Errorf("%s: Normalize(%+q) = %+q; want %+q", c.name, c.in, got, c.want)
		}
	}
}

// One code point inside and one outside each table, at its edges.
func TestSASLprepTableEdges(t *testing.T) {
	for _, c := range []struct {
		name  string
		table *unicode.RangeTable
		in    []rune
		out   []rune
	}{
		{"C.1.2", nonASCIISpace, []rune{0x00a0, 0x2000, 0x200b, 0x3000}, []rune{' ', 0x200c, 0x2fff}},
		{"B.1", mappedToNothing, []rune{0x00ad, 0x034f, 0x200b, 0x200d, 0xfeff, 0xfe00, 0xfe0f}, []rune{0x200e, 0xfe10}},
		{"prohibited", prohibited, []rune{0x0000, 0x001f, 0x007f, 0x00a0, 0xe000, 0xf8ff, 0xfffe, 0xd800, 0xe0001, 0xe007f, 0x10fffd}, []rune{' ', '~', 0x00a1, 0xe0080}},
		{"A.1", unassigned, []rune{0x0221, 0x0234, 0x024f, 0xe0080}, []rune{0x0220, 0x0250, 'a'}},
		{"D.1", randALCat, []rune{0x05be, 0x05d0, 0x0627, 0xfefc}, []rune{'a', 0x05bf, '1'}},
		{"D.2", lCat, []rune{'A', 'z', 0x00aa, 0x10ffff - 2}, []rune{'1', ' ', 0x05d0}},
	} {
		for _, r := range c.in {
			if !unicode.Is(c.table, r) {
				t.Errorf("%s lacks U+%04X", c.name, r)
			}
		}
		for _, r := range c.out {
			if unicode.Is(c.table, r) {
				t.Errorf("%s holds U+%04X", c.name, r)
			}
		}
	}
}

func TestNewVerifierSaltsAndVerifies(t *testing.T) {
	a, b := NewVerifier("pw", 2), NewVerifier("pw", 2)
	if a == b {
		t.Errorf("two verifiers share a salt: %s", a)
	}
	iter, salt, ok := Parse(a)
	if !ok || iter != 2 || len(salt) != SaltLen {
		t.Errorf("Parse(%s) = %d, %x, %v", a, iter, salt, ok)
	}
	if !Verifies(a, "pw") || Verifies(a, "pW") || Verifies("pw", "pw") {
		t.Error("Verifies does not tell the password from another")
	}
	if !Verifies(NewVerifier("\uff21", 1), "A") || Verifier("\uff21", []byte("s"), 1) != Verifier("A", []byte("s"), 1) {
		t.Error("a verifier is not of the normalized password")
	}
}

func TestParseRefusesOtherForms(t *testing.T) {
	good := NewVerifier("x", 1)
	for _, s := range []string{
		"",
		"md5" + strings.Repeat("0", 32),
		"x",
		strings.Replace(good, "$1:", "$0:", 1),
		strings.Replace(good, "$1:", "$a:", 1),
		strings.Replace(good, "SCRAM", "scram", 1),
		good[:len(good)-4],
		strings.Replace(good, ":", ";", 1),
	} {
		if _, _, ok := Parse(s); ok {
			t.Errorf("Parse(%q) accepted", s)
		}
	}
}
