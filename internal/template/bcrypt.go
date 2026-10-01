package template

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"math/big"
	"strings"
	"sync"
)

// bcrypt ($2b$ and its variants) on a Blowfish written here: the
// cipher's initial state is the hexadecimal digits of pi (its P-array
// the first 18 words of the fraction, the four S-boxes the next 1024),
// computed once with Machin's formula rather than tabulated.

const bcryptAlphabet = "./ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

var bcrypt64 = base64.NewEncoding(bcryptAlphabet).WithPadding(base64.NoPadding)

type blowfish struct {
	p [18]uint32
	s [4][256]uint32
}

// piWords are the first n 32-bit words of pi's fractional part.
func piWords(n int) []uint32 {
	bits := uint(32*n + 64)
	one := new(big.Int).Lsh(big.NewInt(1), bits)
	// atanInv is arctan(1/x) scaled by 2^bits.
	atanInv := func(x int64) *big.Int {
		sum := new(big.Int)
		term := new(big.Int).Div(one, big.NewInt(x))
		x2 := big.NewInt(x * x)
		t := new(big.Int)
		for k := int64(0); term.Sign() != 0; k++ {
			t.Div(term, big.NewInt(2*k+1))
			if k%2 == 0 {
				sum.Add(sum, t)
			} else {
				sum.Sub(sum, t)
			}
			term.Div(term, x2)
		}
		return sum
	}
	pi := new(big.Int).Mul(atanInv(5), big.NewInt(16))
	pi.Sub(pi, new(big.Int).Mul(atanInv(239), big.NewInt(4)))
	pi.Sub(pi, new(big.Int).Mul(big.NewInt(3), one)) // the fraction
	pi.Rsh(pi, 64)
	words := make([]uint32, n)
	mask := big.NewInt(0xffffffff)
	w := new(big.Int)
	for i := n - 1; i >= 0; i-- {
		words[i] = uint32(w.And(pi, mask).Uint64())
		pi.Rsh(pi, 32)
	}
	return words
}

var blowfishInit = sync.OnceValue(func() *blowfish {
	words := piWords(18 + 4*256)
	var c blowfish
	copy(c.p[:], words[:18])
	for i := range 4 {
		copy(c.s[i][:], words[18+256*i:18+256*(i+1)])
	}
	return &c
})

func (c *blowfish) f(x uint32) uint32 {
	return ((c.s[0][x>>24] + c.s[1][x>>16&0xff]) ^ c.s[2][x>>8&0xff]) + c.s[3][x&0xff]
}

func (c *blowfish) encrypt(l, r uint32) (uint32, uint32) {
	l ^= c.p[0]
	for i := 1; i <= 16; i += 2 {
		r ^= c.f(l) ^ c.p[i]
		l ^= c.f(r) ^ c.p[i+1]
	}
	r ^= c.p[17]
	return r, l
}

// nextWord is the next big-endian word of b, read cyclically from *pos.
func nextWord(b []byte, pos *int) uint32 {
	var w uint32
	for range 4 {
		w = w<<8 | uint32(b[*pos])
		*pos = (*pos + 1) % len(b)
	}
	return w
}

// expand is Blowfish_expandstate (salt) or Blowfish_expand0state (salt
// nil): the key into the P-array, then the state re-encrypted.
func (c *blowfish) expand(key, salt []byte) {
	j := 0
	for i := range c.p {
		c.p[i] ^= nextWord(key, &j)
	}
	j = 0
	var l, r uint32
	mix := func() {
		if salt != nil {
			l ^= nextWord(salt, &j)
			r ^= nextWord(salt, &j)
		}
		l, r = c.encrypt(l, r)
	}
	for i := 0; i < 18; i += 2 {
		mix()
		c.p[i], c.p[i+1] = l, r
	}
	for s := range c.s {
		for i := 0; i < 256; i += 2 {
			mix()
			c.s[s][i], c.s[s][i+1] = l, r
		}
	}
}

// bcryptHash is bcrypt's hash of key (the password with its NUL, at
// most 72 bytes) with a 16-byte salt at a cost: the 31 characters after
// the salt.
func bcryptHash(key, salt []byte, cost int) string {
	c := *blowfishInit()
	c.expand(key, salt)
	for range uint64(1) << cost {
		c.expand(key, nil)
		c.expand(salt, nil)
	}
	text := []byte("OrpheanBeholderScryDoubt")
	for i := 0; i < 24; i += 8 {
		l, r := binary.BigEndian.Uint32(text[i:]), binary.BigEndian.Uint32(text[i+4:])
		for range 64 {
			l, r = c.encrypt(l, r)
		}
		binary.BigEndian.PutUint32(text[i:], l)
		binary.BigEndian.PutUint32(text[i+4:], r)
	}
	return bcrypt64.EncodeToString(text[:23])
}

// bcryptKey is the key bcrypt hashes a password with: its bytes and a
// NUL, at most 72 bytes.
func bcryptKey(pw []byte) []byte {
	key := append(append([]byte(nil), pw...), 0)
	if len(key) > 72 {
		key = key[:72]
	}
	return key
}

// bcryptCrypt is "$<ident>$<cost>$<salt><hash>" for a 22-character
// bcrypt64 salt.
func bcryptCrypt(pw []byte, ident, salt22 string, cost int) string {
	// 22 characters are 132 bits: the 16 bytes, and 4 unused bits.
	salt, _ := bcrypt64.DecodeString(salt22)
	return fmt.Sprintf("$%s$%02d$%s%s", ident, cost, salt22, bcryptHash(bcryptKey(pw), salt, cost))
}

// bcryptIndex is c's value in the bcrypt64 alphabet, -1 when not in it.
func bcryptIndex(c byte) int {
	return strings.IndexByte(bcryptAlphabet, c)
}
