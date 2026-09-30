package sshkey

import (
	"crypto/sha512"
	"encoding/binary"
)

// blowfish is the Eksblowfish state bcrypt_pbkdf drives: the P-array and
// the four S-boxes, initialized from the digits of pi.
type blowfish struct {
	p [18]uint32
	s [4][256]uint32
}

func newBlowfish() *blowfish {
	return &blowfish{p: bfP, s: [4][256]uint32{bfS0, bfS1, bfS2, bfS3}}
}

func (c *blowfish) f(x uint32) uint32 {
	return ((c.s[0][x>>24] + c.s[1][x>>16&0xff]) ^ c.s[2][x>>8&0xff]) + c.s[3][x&0xff]
}

func (c *blowfish) encipher(l, r uint32) (uint32, uint32) {
	l ^= c.p[0]
	for i := 1; i <= 16; i += 2 {
		r ^= c.f(l) ^ c.p[i]
		l ^= c.f(r) ^ c.p[i+1]
	}
	r ^= c.p[17]
	return r, l
}

// stream2word reads the next big-endian word of data, cycling through it.
func stream2word(data []byte, j *int) uint32 {
	var w uint32
	for i := 0; i < 4; i++ {
		if *j >= len(data) {
			*j = 0
		}
		w = w<<8 | uint32(data[*j])
		*j++
	}
	return w
}

// expand is Blowfish_expandstate (data != nil) or Blowfish_expand0state.
func (c *blowfish) expand(data, key []byte) {
	j := 0
	for i := range c.p {
		c.p[i] ^= stream2word(key, &j)
	}
	j = 0
	var l, r uint32
	next := func() {
		if data != nil {
			l ^= stream2word(data, &j)
			r ^= stream2word(data, &j)
		}
		l, r = c.encipher(l, r)
	}
	for i := 0; i < 18; i += 2 {
		next()
		c.p[i], c.p[i+1] = l, r
	}
	for s := range c.s {
		for k := 0; k < 256; k += 2 {
			next()
			c.s[s][k], c.s[s][k+1] = l, r
		}
	}
}

// bcryptHash is OpenBSD's bcrypt_hash: one 32-byte block of bcrypt_pbkdf.
func bcryptHash(sha2pass, sha2salt []byte) [32]byte {
	c := newBlowfish()
	c.expand(sha2salt, sha2pass)
	for i := 0; i < 64; i++ {
		c.expand(nil, sha2salt)
		c.expand(nil, sha2pass)
	}
	magic := []byte("OxychromaticBlowfishSwatDynamite")
	var cdata [8]uint32
	j := 0
	for i := range cdata {
		cdata[i] = stream2word(magic, &j)
	}
	for i := 0; i < 64; i++ {
		for k := 0; k < 8; k += 2 {
			cdata[k], cdata[k+1] = c.encipher(cdata[k], cdata[k+1])
		}
	}
	var out [32]byte
	for i, w := range cdata {
		binary.LittleEndian.PutUint32(out[4*i:], w)
	}
	return out
}

// bcryptPBKDF is OpenBSD's bcrypt_pbkdf, the KDF of OpenSSH's
// openssh-key-v1 private key format.
func bcryptPBKDF(pass, salt []byte, rounds, keyLen int) []byte {
	key := make([]byte, keyLen)
	stride := (keyLen + 31) / 32
	amt := (keyLen + stride - 1) / stride
	h := sha512.Sum512(pass)
	sha2pass := h[:]
	remaining := keyLen
	for count := uint32(1); remaining > 0; count++ {
		countSalt := binary.BigEndian.AppendUint32(append([]byte(nil), salt...), count)
		s := sha512.Sum512(countSalt)
		tmp := bcryptHash(sha2pass, s[:])
		out := tmp
		for i := 1; i < rounds; i++ {
			s = sha512.Sum512(tmp[:])
			tmp = bcryptHash(sha2pass, s[:])
			for k := range out {
				out[k] ^= tmp[k]
			}
		}
		if amt > remaining {
			amt = remaining
		}
		i := 0
		for ; i < amt; i++ {
			dest := i*stride + int(count-1)
			if dest >= keyLen {
				break
			}
			key[dest] = out[i]
		}
		remaining -= i
	}
	return key
}
