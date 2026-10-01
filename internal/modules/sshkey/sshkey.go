// Package sshkey generates, serializes and loads SSH keypairs the way
// python-cryptography does for community.crypto's openssh_keypair
// (cryptography backend): OpenSSH (openssh-key-v1, bcrypt/aes256-ctr),
// PKCS#8 (PBES2/PBKDF2-SHA256/AES-256-CBC) and traditional PKCS#1/SEC1
// (AES-256-CBC DEK-Info) private keys, and OpenSSH public keys. Standard
// library only: the agent may not import golang.org/x/crypto.
package sshkey

import (
	"crypto/aes"
	"crypto/cipher"
	//lint:ignore SA1019 DSA keys are still an openssh_keypair type
	"crypto/dsa"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"strings"
)

// ErrPassphrase is a key that needs a passphrase that was not given, or
// was given one it does not need (cryptography's TypeError).
var ErrPassphrase = errors.New("passphrase mismatch")

// ErrInvalid is a key that cannot be read (cryptography's ValueError).
type ErrInvalid struct{ Msg string }

func (e *ErrInvalid) Error() string { return e.Msg }

func invalid(format string, a ...any) error { return &ErrInvalid{fmt.Sprintf(format, a...)} }

// Generate creates a private key of the given openssh_keypair type
// (rsa, dsa, ecdsa, ed25519) and size in bits.
func Generate(keyType string, size int) (any, error) {
	switch keyType {
	case "rsa":
		return rsa.GenerateKey(rand.Reader, size)
	case "dsa":
		k := new(dsa.PrivateKey)
		if err := dsa.GenerateParameters(&k.Parameters, rand.Reader, dsa.L1024N160); err != nil {
			return nil, err
		}
		if err := dsa.GenerateKey(k, rand.Reader); err != nil {
			return nil, err
		}
		return k, nil
	case "ecdsa":
		var c elliptic.Curve
		switch size {
		case 256:
			c = elliptic.P256()
		case 384:
			c = elliptic.P384()
		case 521:
			c = elliptic.P521()
		default:
			return nil, fmt.Errorf("%d is not a valid key size for ecdsa keys", size)
		}
		return ecdsa.GenerateKey(c, rand.Reader)
	case "ed25519":
		_, k, err := ed25519.GenerateKey(rand.Reader)
		return k, err
	}
	return nil, fmt.Errorf("%s is not a valid keytype", keyType)
}

// TypeAndSize reports openssh_keypair's type name and the key size.
func TypeAndSize(k any) (string, int) {
	switch k := k.(type) {
	case *rsa.PrivateKey:
		return "rsa", k.N.BitLen()
	case *dsa.PrivateKey:
		return "dsa", k.P.BitLen()
	case *ecdsa.PrivateKey:
		return "ecdsa", k.Curve.Params().BitSize
	case ed25519.PrivateKey:
		return "ed25519", 256
	}
	return "", 0
}

// sshWriter builds SSH wire-format data.
type sshWriter struct{ b []byte }

func (w *sshWriter) u32(v uint32)     { w.b = binary.BigEndian.AppendUint32(w.b, v) }
func (w *sshWriter) str(s []byte)     { w.u32(uint32(len(s))); w.b = append(w.b, s...) }
func (w *sshWriter) text(s string)    { w.str([]byte(s)) }
func (w *sshWriter) mpint(n *big.Int) { w.str(mpintBytes(n)) }

func mpintBytes(n *big.Int) []byte {
	if n.Sign() == 0 {
		return nil
	}
	b := n.Bytes()
	if b[0]&0x80 != 0 {
		b = append([]byte{0}, b...)
	}
	return b
}

func curveName(c elliptic.Curve) string {
	return "nistp" + strings.TrimPrefix(c.Params().Name, "P-")
}

func ecPoint(k *ecdsa.PublicKey) []byte {
	e, err := k.ECDH()
	if err != nil {
		return nil
	}
	return e.Bytes()
}

// PublicBlob is the key's SSH public key blob and its algorithm name.
func PublicBlob(k any) ([]byte, string) {
	var w sshWriter
	var name string
	switch k := k.(type) {
	case *rsa.PrivateKey:
		name = "ssh-rsa"
		w.text(name)
		w.mpint(big.NewInt(int64(k.E)))
		w.mpint(k.N)
	case *dsa.PrivateKey:
		name = "ssh-dss"
		w.text(name)
		w.mpint(k.P)
		w.mpint(k.Q)
		w.mpint(k.G)
		w.mpint(k.Y)
	case *ecdsa.PrivateKey:
		name = "ecdsa-sha2-" + curveName(k.Curve)
		w.text(name)
		w.text(curveName(k.Curve))
		w.str(ecPoint(&k.PublicKey))
	case ed25519.PrivateKey:
		name = "ssh-ed25519"
		w.text(name)
		w.str(k[32:])
	}
	return w.b, name
}

// AuthorizedKey is the OpenSSH public key line ("type base64"), without
// a comment or newline.
func AuthorizedKey(k any) string {
	blob, name := PublicBlob(k)
	return name + " " + base64.StdEncoding.EncodeToString(blob)
}

// Fingerprint is the SHA256 fingerprint of an OpenSSH public key line.
func Fingerprint(authorizedKey string) string {
	fields := strings.Split(authorizedKey, " ")
	if len(fields) < 2 {
		return ""
	}
	blob, err := base64.StdEncoding.DecodeString(fields[1])
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(blob)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
}

// privateSection is the key-specific part of an openssh-key-v1 private
// section.
func privateSection(w *sshWriter, k any) {
	switch k := k.(type) {
	case *rsa.PrivateKey:
		w.text("ssh-rsa")
		w.mpint(k.N)
		w.mpint(big.NewInt(int64(k.E)))
		w.mpint(k.D)
		iqmp := k.Precomputed.Qinv
		if iqmp == nil {
			iqmp = new(big.Int).ModInverse(k.Primes[1], k.Primes[0])
		}
		w.mpint(iqmp)
		w.mpint(k.Primes[0])
		w.mpint(k.Primes[1])
	case *dsa.PrivateKey:
		w.text("ssh-dss")
		w.mpint(k.P)
		w.mpint(k.Q)
		w.mpint(k.G)
		w.mpint(k.Y)
		w.mpint(k.X)
	case *ecdsa.PrivateKey:
		w.text("ecdsa-sha2-" + curveName(k.Curve))
		w.text(curveName(k.Curve))
		w.str(ecPoint(&k.PublicKey))
		d, _ := k.Bytes()
		w.mpint(new(big.Int).SetBytes(d))
	case ed25519.PrivateKey:
		w.text("ssh-ed25519")
		w.str(k[32:])
		w.str(k)
	}
}

const (
	sshMagic       = "openssh-key-v1\x00"
	sshBcryptRound = 16
)

// marshalOpenSSH is cryptography's PrivateFormat.OpenSSH encoding (empty
// comment; aes256-ctr with 16 bcrypt rounds when encrypted).
func marshalOpenSSH(k any, passphrase []byte) ([]byte, error) {
	pub, _ := PublicBlob(k)
	var check [4]byte
	if _, err := rand.Read(check[:]); err != nil {
		return nil, err
	}
	var priv sshWriter
	priv.b = append(priv.b, check[:]...)
	priv.b = append(priv.b, check[:]...)
	privateSection(&priv, k)
	priv.text("")
	block := 8
	if passphrase != nil {
		block = aes.BlockSize
	}
	for i := 1; len(priv.b)%block != 0; i++ {
		priv.b = append(priv.b, byte(i))
	}

	var w sshWriter
	w.b = append(w.b, sshMagic...)
	if passphrase == nil {
		w.text("none")
		w.text("none")
		w.text("")
	} else {
		salt := make([]byte, 16)
		if _, err := rand.Read(salt); err != nil {
			return nil, err
		}
		keyIV := bcryptPBKDF(passphrase, salt, sshBcryptRound, 32+aes.BlockSize)
		c, err := aes.NewCipher(keyIV[:32])
		if err != nil {
			return nil, err
		}
		cipher.NewCTR(c, keyIV[32:]).XORKeyStream(priv.b, priv.b)
		w.text("aes256-ctr")
		w.text("bcrypt")
		var opts sshWriter
		opts.str(salt)
		opts.u32(sshBcryptRound)
		w.str(opts.b)
	}
	w.u32(1)
	w.str(pub)
	w.str(priv.b)
	return pemEncode("OPENSSH PRIVATE KEY", nil, w.b), nil
}

// pemEncode writes a PEM block with 64-column base64 (OpenSSL's layout).
func pemEncode(typ string, headers [][2]string, der []byte) []byte {
	var b strings.Builder
	b.WriteString("-----BEGIN " + typ + "-----\n")
	for _, h := range headers {
		b.WriteString(h[0] + ": " + h[1] + "\n")
	}
	if len(headers) > 0 {
		b.WriteString("\n")
	}
	enc := base64.StdEncoding.EncodeToString(der)
	for len(enc) > 64 {
		b.WriteString(enc[:64] + "\n")
		enc = enc[64:]
	}
	if enc != "" {
		b.WriteString(enc + "\n")
	}
	b.WriteString("-----END " + typ + "-----\n")
	return []byte(b.String())
}

// sshReader parses SSH wire-format data.
type sshReader struct {
	b   []byte
	bad bool
}

func (r *sshReader) u32() uint32 {
	if len(r.b) < 4 {
		r.bad = true
		return 0
	}
	v := binary.BigEndian.Uint32(r.b)
	r.b = r.b[4:]
	return v
}

func (r *sshReader) str() []byte {
	n := r.u32()
	if r.bad || uint64(n) > uint64(len(r.b)) {
		r.bad = true
		return nil
	}
	s := r.b[:n]
	r.b = r.b[n:]
	return s
}

func (r *sshReader) mpint() *big.Int { return new(big.Int).SetBytes(r.str()) }

// parseOpenSSH loads an openssh-key-v1 private key (cryptography's
// load_ssh_private_key).
func parseOpenSSH(der, passphrase []byte) (any, error) {
	if !strings.HasPrefix(string(der), sshMagic) {
		return nil, invalid("Not OpenSSH private key format")
	}
	r := &sshReader{b: der[len(sshMagic):]}
	cipherName, kdfName, kdfOpts := string(r.str()), string(r.str()), r.str()
	nkeys := r.u32()
	r.str() // public key
	priv := append([]byte(nil), r.str()...)
	if r.bad || nkeys != 1 {
		return nil, invalid("Only one key supported")
	}
	if cipherName != "none" || kdfName != "none" {
		if passphrase == nil {
			return nil, ErrPassphrase
		}
		if cipherName != "aes256-ctr" && cipherName != "aes256-cbc" && cipherName != "aes128-ctr" &&
			cipherName != "aes192-ctr" && cipherName != "aes128-cbc" && cipherName != "aes192-cbc" {
			return nil, invalid("Unsupported cipher: %s", cipherName)
		}
		if kdfName != "bcrypt" {
			return nil, invalid("Unsupported KDF: %s", kdfName)
		}
		o := &sshReader{b: kdfOpts}
		salt, rounds := o.str(), o.u32()
		if o.bad {
			return nil, invalid("Invalid data")
		}
		keyLen := 32
		switch {
		case strings.HasPrefix(cipherName, "aes128"):
			keyLen = 16
		case strings.HasPrefix(cipherName, "aes192"):
			keyLen = 24
		}
		keyIV := bcryptPBKDF(passphrase, salt, int(rounds), keyLen+aes.BlockSize)
		c, err := aes.NewCipher(keyIV[:keyLen])
		if err != nil || len(priv)%aes.BlockSize != 0 {
			return nil, invalid("Corrupt data: invalid ciphertext")
		}
		if strings.HasSuffix(cipherName, "ctr") {
			cipher.NewCTR(c, keyIV[keyLen:]).XORKeyStream(priv, priv)
		} else {
			cipher.NewCBCDecrypter(c, keyIV[keyLen:]).CryptBlocks(priv, priv)
		}
	} else if passphrase != nil {
		return nil, ErrPassphrase
	}
	p := &sshReader{b: priv}
	c1, c2 := p.u32(), p.u32()
	if p.bad || c1 != c2 {
		return nil, invalid("Corrupt data: broken checksum")
	}
	var key any
	switch typ := string(p.str()); typ {
	case "ssh-rsa":
		n, e, d, _, pp, q := p.mpint(), p.mpint(), p.mpint(), p.mpint(), p.mpint(), p.mpint()
		k := &rsa.PrivateKey{PublicKey: rsa.PublicKey{N: n, E: int(e.Int64())}, D: d, Primes: []*big.Int{pp, q}}
		if err := k.Validate(); err != nil {
			return nil, invalid("Invalid key")
		}
		k.Precompute()
		key = k
	case "ssh-dss":
		k := &dsa.PrivateKey{}
		k.P, k.Q, k.G, k.Y, k.X = p.mpint(), p.mpint(), p.mpint(), p.mpint(), p.mpint()
		key = k
	case "ecdsa-sha2-nistp256", "ecdsa-sha2-nistp384", "ecdsa-sha2-nistp521":
		p.str()
		p.str()
		d := p.mpint()
		c := map[string]elliptic.Curve{"ecdsa-sha2-nistp256": elliptic.P256(),
			"ecdsa-sha2-nistp384": elliptic.P384(), "ecdsa-sha2-nistp521": elliptic.P521()}[typ]
		k, err := ecdsa.ParseRawPrivateKey(c, d.FillBytes(make([]byte, (c.Params().BitSize+7)/8)))
		if err != nil {
			return nil, invalid("Invalid key")
		}
		key = k
	case "ssh-ed25519":
		p.str()
		k := p.str()
		if len(k) != ed25519.PrivateKeySize {
			return nil, invalid("Invalid key")
		}
		key = ed25519.PrivateKey(append([]byte(nil), k...))
	default:
		return nil, invalid("Unsupported key type: %q", typ)
	}
	if p.bad {
		return nil, invalid("Invalid data")
	}
	return key, nil
}
