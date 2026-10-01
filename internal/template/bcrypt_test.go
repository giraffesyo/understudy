package template

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestPiWords(t *testing.T) {
	c := blowfishInit()
	for _, tc := range []struct {
		got, want uint32
	}{{c.p[0], 0x243f6a88}, {c.p[17], 0x8979fb1b}, {c.s[0][0], 0xd1310ba6}, {c.s[3][255], 0x3ac372e6}} {
		if tc.got != tc.want {
			t.Errorf("got %08x, want %08x", tc.got, tc.want)
		}
	}
}

// TestBcryptMatchesPasslib holds password_hash('blowfish') to what
// ansible-core returns through passlib (testdata/bcrypt.json, from
// testdata/bcrypt_oracle.py): the hash, or the error.
func TestBcryptMatchesPasslib(t *testing.T) {
	data, err := os.ReadFile("testdata/bcrypt.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Password string `json:"password"`
		Salt     any    `json:"salt"`
		Rounds   any    `json:"rounds"`
		Ident    any    `json:"ident"`
		SaltSize any    `json:"salt_size"`
		Hash     string `json:"hash"`
		Error    string `json:"error"`
		Cause    string `json:"cause"`
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	num := func(v any) any {
		if f, ok := v.(float64); ok {
			return int64(f)
		}
		return v
	}
	for _, c := range cases {
		got, err := passlibBcrypt(c.Password, c.Salt, num(c.SaltSize), num(c.Rounds), c.Ident, false)
		name := strings.Join([]string{c.Password, toStr(c.Salt), toStr(c.Rounds), toStr(c.Ident), toStr(c.SaltSize)}, "|")
		switch {
		case c.Error != "":
			want := c.Error
			if c.Cause != "" && !strings.HasSuffix(want, c.Cause) {
				want = strings.TrimRight(want, ". ") + ": " + c.Cause
			}
			if err == nil || err.Error() != want {
				t.Errorf("%q: got %v, %v; want error %q", name, got, err, want)
			}
		case !truthy(c.Salt):
			// A random salt: the hash's shape.
			s, _ := got.(string)
			if err != nil || len(s) != len(c.Hash) || s[:7] != c.Hash[:7] {
				t.Errorf("%q: got %v, %v; want the shape of %q", name, got, err, c.Hash)
			}
		case err != nil || got != c.Hash:
			t.Errorf("%q: got %v, %v; want %q", name, got, err, c.Hash)
		}
	}
}

// TestBcryptLibxcrypt holds the crypt_gensalt path to libxcrypt's own
// crypt_gensalt and crypt (Rocky Linux 9): the salt's first 16 bytes are
// encoded as the bcrypt salt.
func TestBcryptLibxcrypt(t *testing.T) {
	for _, tc := range []struct {
		ident, salt, pw string
		cost            int64
		want            string
	}{
		{"2b", "abcdefghijklmnopqrstuv", "pw", 12, "$2b$12$WUHhXETkX0fnYkrqZU3ta.KUVbS1KaZP4pGl5OPt83CTcxtOvBMI."},
		{"2b", "abcdefghijklmnopqrstuv", "mypassword", 4, "$2b$04$WUHhXETkX0fnYkrqZU3ta.oYxLIhD8Yw8wfJWckInAFbkax2tMzP2"},
		{"2a", "ABCDEFGHIJKLMNOPQRSTU.", strings.Repeat("x", 80), 5, "$2a$05$OSHBPCTEPyfHQirKRS3NS.SLiaSRPzPvTMLyRB/uJxrPw0GO.0y0a"},
		{"2y", "./0123456789ABCDEFGHIJ", "", 4, "$2y$04$Jg6uKRGxLBS0Lxe3OSHBP.shMw8YDlrXEaP4ordtCFuds3uJ.B3Jq"},
	} {
		got, err := libxcryptBcrypt(tc.pw, tc.salt, nil, tc.cost, tc.ident)
		if err != nil || got != tc.want {
			t.Errorf("%s %s %q: got %v, %v; want %s", tc.ident, tc.salt, tc.pw, got, err, tc.want)
		}
	}
	for _, ident := range []string{"2", "2x"} {
		if _, err := libxcryptBcrypt("pw", "abcdefghijklmnopqrstuv", nil, int64(4), ident); err == nil {
			t.Errorf("ident %s: want an error", ident)
		}
	}
}
