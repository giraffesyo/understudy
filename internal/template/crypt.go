package template

import (
	"crypto/sha256"
	"crypto/sha512"
	"fmt"
	"hash"
	"strconv"
)

// cryptAlphabet is the base64 variant used by the crypt(3) SHA schemes.
const cryptAlphabet = "./0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// shaCryptRounds is the default round count for the $5$/$6$ schemes; it is
// omitted from the output only when equal to this value (matching glibc).
const shaCryptRounds = 5000

// shaCrypt implements the glibc SHA-256/SHA-512 crypt(3) scheme (Drepper's
// spec) used by Ansible's password_hash filter. salt is truncated to 16
// characters; rounds<1000 is clamped up as glibc does.
func shaCrypt(password, salt string, rounds int, use512 bool) string {
	var newHash func() hash.Hash
	var digestLen int
	var prefix string
	if use512 {
		newHash, digestLen, prefix = sha512.New, 64, "$6$"
	} else {
		newHash, digestLen, prefix = sha256.New, 32, "$5$"
	}
	explicitRounds := rounds != shaCryptRounds
	if rounds < 1000 {
		rounds = 1000
	}
	if rounds > 999999999 {
		rounds = 999999999
	}

	pw := []byte(password)
	if len(salt) > 16 {
		salt = salt[:16]
	}
	saltB := []byte(salt)

	// Digest B = H(pw + salt + pw).
	b := newHash()
	b.Write(pw)
	b.Write(saltB)
	b.Write(pw)
	sumB := b.Sum(nil)

	// Digest A.
	a := newHash()
	a.Write(pw)
	a.Write(saltB)
	writeCycled(a, sumB, len(pw), digestLen)
	for cnt := len(pw); cnt > 0; cnt >>= 1 {
		if cnt&1 != 0 {
			a.Write(sumB)
		} else {
			a.Write(pw)
		}
	}
	sumA := a.Sum(nil)

	// P: len(pw) bytes derived from H(pw * len(pw)).
	dp := newHash()
	for range pw {
		dp.Write(pw)
	}
	p := cycle(dp.Sum(nil), len(pw), digestLen)

	// S: len(salt) bytes derived from H(salt * (16 + sumA[0])).
	ds := newHash()
	for i := 0; i < 16+int(sumA[0]); i++ {
		ds.Write(saltB)
	}
	s := cycle(ds.Sum(nil), len(saltB), digestLen)

	// Rounds loop.
	c := sumA
	for i := 0; i < rounds; i++ {
		ctx := newHash()
		if i&1 != 0 {
			ctx.Write(p)
		} else {
			ctx.Write(c)
		}
		if i%3 != 0 {
			ctx.Write(s)
		}
		if i%7 != 0 {
			ctx.Write(p)
		}
		if i&1 != 0 {
			ctx.Write(c)
		} else {
			ctx.Write(p)
		}
		c = ctx.Sum(nil)
	}

	out := prefix
	if explicitRounds {
		out += "rounds=" + strconv.Itoa(rounds) + "$"
	}
	out += salt + "$" + cryptEncode(c, use512)
	return out
}

// writeCycled writes n bytes of src (repeating from the start) into h.
func writeCycled(h hash.Hash, src []byte, n, block int) {
	for n > 0 {
		take := block
		if take > n {
			take = n
		}
		h.Write(src[:take])
		n -= take
	}
}

// cycle returns n bytes formed by repeating src.
func cycle(src []byte, n, block int) []byte {
	out := make([]byte, 0, n)
	for len(out) < n {
		take := block
		if take > n-len(out) {
			take = n - len(out)
		}
		out = append(out, src[:take]...)
	}
	return out
}

// cryptEncode applies the scheme's byte permutation and crypt-base64 encoding.
func cryptEncode(c []byte, use512 bool) string {
	var b []byte
	emit := func(b2, b1, b0 byte, n int) {
		w := uint32(b2)<<16 | uint32(b1)<<8 | uint32(b0)
		for i := 0; i < n; i++ {
			b = append(b, cryptAlphabet[w&0x3f])
			w >>= 6
		}
	}
	if use512 {
		order := [][3]int{
			{0, 21, 42}, {22, 43, 1}, {44, 2, 23}, {3, 24, 45}, {25, 46, 4},
			{47, 5, 26}, {6, 27, 48}, {28, 49, 7}, {50, 8, 29}, {9, 30, 51},
			{31, 52, 10}, {53, 11, 32}, {12, 33, 54}, {34, 55, 13}, {56, 14, 35},
			{15, 36, 57}, {37, 58, 16}, {59, 17, 38}, {18, 39, 60}, {40, 61, 19},
			{62, 20, 41},
		}
		for _, g := range order {
			emit(c[g[0]], c[g[1]], c[g[2]], 4)
		}
		emit(0, 0, c[63], 2)
	} else {
		order := [][3]int{
			{0, 10, 20}, {21, 1, 11}, {12, 22, 2}, {3, 13, 23}, {24, 4, 14},
			{15, 25, 5}, {6, 16, 26}, {27, 7, 17}, {18, 28, 8}, {9, 19, 29},
		}
		for _, g := range order {
			emit(c[g[0]], c[g[1]], c[g[2]], 4)
		}
		emit(0, c[31], c[30], 3)
	}
	return string(b)
}

// CryptHash hashes a password with the glibc crypt scheme Ansible's
// vars_prompt encrypt: and password_hash use ("sha512_crypt" or
// "sha256_crypt").
func CryptHash(scheme, password, salt string, rounds int) (string, error) {
	if rounds == 0 {
		rounds = shaCryptRounds
	}
	switch scheme {
	case "sha512_crypt":
		return shaCrypt(password, salt, rounds, true), nil
	case "sha256_crypt":
		return shaCrypt(password, salt, rounds, false), nil
	}
	return "", fmt.Errorf("unsupported encrypt scheme %q (sha512_crypt or sha256_crypt)", scheme)
}
