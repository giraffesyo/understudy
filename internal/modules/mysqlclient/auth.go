package mysqlclient

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"errors"
)

const scrambleLength = 20

// scrambleNative is mysql_native_password:
// SHA1(pw) XOR SHA1(salt + SHA1(SHA1(pw))).
func scrambleNative(password, salt []byte) []byte {
	if len(password) == 0 {
		return nil
	}
	if len(salt) > scrambleLength {
		salt = salt[:scrambleLength]
	}
	stage1 := sha1.Sum(password)
	stage2 := sha1.Sum(stage1[:])
	h := sha1.New()
	h.Write(salt)
	h.Write(stage2[:])
	out := h.Sum(nil)
	for i := range out {
		out[i] ^= stage1[i]
	}
	return out
}

// scrambleSHA256 is the caching_sha2_password fast path:
// XOR(SHA256(pw), SHA256(SHA256(SHA256(pw)) + nonce)).
func scrambleSHA256(password, nonce []byte) []byte {
	if len(password) == 0 {
		return nil
	}
	p1 := sha256.Sum256(password)
	p2 := sha256.Sum256(p1[:])
	h := sha256.New()
	h.Write(p2[:])
	h.Write(nonce)
	p3 := h.Sum(nil)
	out := p1[:]
	for i := range p3 {
		out[i] ^= p3[i]
	}
	return out
}

// rsaEncryptPassword encrypts (password + NUL) XOR salt with the server's
// RSA public key (OAEP, SHA-1), as sha256_password and
// caching_sha2_password full authentication require on plain connections.
func rsaEncryptPassword(password, salt, pubPEM []byte) ([]byte, error) {
	if len(password) == 0 {
		return nil, nil
	}
	block, _ := pem.Decode(pubPEM)
	if block == nil {
		return nil, errors.New("Couldn't receive server's public key")
	}
	var pub *rsa.PublicKey
	if k, err := x509.ParsePKIXPublicKey(block.Bytes); err == nil {
		rk, ok := k.(*rsa.PublicKey)
		if !ok {
			return nil, errors.New("server public key is not RSA")
		}
		pub = rk
	} else if rk, err2 := x509.ParsePKCS1PublicKey(block.Bytes); err2 == nil {
		pub = rk
	} else {
		return nil, err
	}
	if len(salt) > scrambleLength {
		salt = salt[:scrambleLength]
	}
	msg := append(append([]byte{}, password...), 0)
	if len(salt) > 0 {
		for i := range msg {
			msg[i] ^= salt[i%len(salt)]
		}
	}
	return rsa.EncryptOAEP(sha1.New(), rand.Reader, pub, msg, nil)
}
