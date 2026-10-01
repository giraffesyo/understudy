package sshkey

import (
	"crypto/aes"
	"crypto/cipher"
	//lint:ignore SA1019 DSA keys are still an openssh_keypair type
	"crypto/dsa"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"errors"
	"hash"
	"math/big"
	"strings"
)

// Private key formats, as openssh_keypair names them.
const (
	FormatSSH   = "SSH"
	FormatPKCS8 = "PKCS8"
	FormatPKCS1 = "PKCS1"
)

// ErrPKCS1Ed25519 is cryptography's refusal to write an Ed25519 key in
// the traditional format.
var ErrPKCS1Ed25519 = errors.New("ed25519 keys cannot be represented in PKCS1 format")

// MarshalPrivate encodes k as PEM in format (SSH, PKCS8, PKCS1),
// encrypted with passphrase when it is non-nil (cryptography's
// BestAvailableEncryption).
func MarshalPrivate(k any, format string, passphrase []byte) ([]byte, error) {
	switch format {
	case FormatSSH:
		return marshalOpenSSH(k, passphrase)
	case FormatPKCS8:
		der, err := marshalPKCS8(k)
		if err != nil {
			return nil, err
		}
		if passphrase == nil {
			return pemEncode("PRIVATE KEY", nil, der), nil
		}
		enc, err := encryptPKCS8(der, passphrase)
		if err != nil {
			return nil, err
		}
		return pemEncode("ENCRYPTED PRIVATE KEY", nil, enc), nil
	case FormatPKCS1:
		typ, der, err := marshalTraditional(k)
		if err != nil {
			return nil, err
		}
		if passphrase == nil {
			return pemEncode(typ, nil, der), nil
		}
		//lint:ignore SA1019 OpenSSL's traditional encryption is what cryptography writes
		blk, err := x509.EncryptPEMBlock(rand.Reader, typ, der, passphrase, x509.PEMCipherAES256) //nolint:staticcheck
		if err != nil {
			return nil, err
		}
		return pemEncode(typ, [][2]string{{"Proc-Type", blk.Headers["Proc-Type"]},
			{"DEK-Info", blk.Headers["DEK-Info"]}}, blk.Bytes), nil
	}
	return nil, errors.New("The accepted private key formats are SSH, PKCS8, and PKCS1")
}

var (
	oidDSA         = asn1.ObjectIdentifier{1, 2, 840, 10040, 4, 1}
	oidPBES2       = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 5, 13}
	oidPBKDF2      = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 5, 12}
	oidHMACSHA1    = asn1.ObjectIdentifier{1, 2, 840, 113549, 2, 7}
	oidHMACSHA256  = asn1.ObjectIdentifier{1, 2, 840, 113549, 2, 9}
	oidHMACSHA512  = asn1.ObjectIdentifier{1, 2, 840, 113549, 2, 11}
	oidAES128CBC   = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 1, 2}
	oidAES192CBC   = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 1, 22}
	oidAES256CBC   = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 1, 42}
	pbkdf2Iter     = 2048
	pbkdf2SaltSize = 16
)

type pkcs8Info struct {
	Version    int
	Algo       pkix.AlgorithmIdentifier
	PrivateKey []byte
}

type dsaParams struct{ P, Q, G *big.Int }

type dsaTraditional struct {
	Version       int
	P, Q, G, Y, X *big.Int
}

func marshalPKCS8(k any) ([]byte, error) {
	if d, ok := k.(*dsa.PrivateKey); ok {
		params, err := asn1.Marshal(dsaParams{d.P, d.Q, d.G})
		if err != nil {
			return nil, err
		}
		x, err := asn1.Marshal(d.X)
		if err != nil {
			return nil, err
		}
		return asn1.Marshal(pkcs8Info{Algo: pkix.AlgorithmIdentifier{Algorithm: oidDSA,
			Parameters: asn1.RawValue{FullBytes: params}}, PrivateKey: x})
	}
	return x509.MarshalPKCS8PrivateKey(k)
}

func marshalTraditional(k any) (string, []byte, error) {
	switch k := k.(type) {
	case *rsa.PrivateKey:
		return "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(k), nil
	case *ecdsa.PrivateKey:
		der, err := x509.MarshalECPrivateKey(k)
		return "EC PRIVATE KEY", der, err
	case *dsa.PrivateKey:
		der, err := asn1.Marshal(dsaTraditional{0, k.P, k.Q, k.G, k.Y, k.X})
		return "DSA PRIVATE KEY", der, err
	}
	return "", nil, ErrPKCS1Ed25519
}

type pbkdf2Params struct {
	Salt       []byte
	Iterations int
	KeyLength  int                      `asn1:"optional"`
	PRF        pkix.AlgorithmIdentifier `asn1:"optional"`
}

type pbes2Params struct {
	KDF    pkix.AlgorithmIdentifier
	Scheme pkix.AlgorithmIdentifier
}

type encryptedPKCS8 struct {
	Algo pkix.AlgorithmIdentifier
	Data []byte
}

func encryptPKCS8(der, passphrase []byte) ([]byte, error) {
	salt := make([]byte, pbkdf2SaltSize)
	iv := make([]byte, aes.BlockSize)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	if _, err := rand.Read(iv); err != nil {
		return nil, err
	}
	key, err := pbkdf2.Key(sha256.New, string(passphrase), salt, pbkdf2Iter, 32)
	if err != nil {
		return nil, err
	}
	c, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	pad := aes.BlockSize - len(der)%aes.BlockSize
	data := append(append([]byte(nil), der...), make([]byte, pad)...)
	for i := len(der); i < len(data); i++ {
		data[i] = byte(pad)
	}
	cipher.NewCBCEncrypter(c, iv).CryptBlocks(data, data)
	kdf, err := asn1.Marshal(pbkdf2Params{Salt: salt, Iterations: pbkdf2Iter,
		PRF: pkix.AlgorithmIdentifier{Algorithm: oidHMACSHA256, Parameters: asn1.NullRawValue}})
	if err != nil {
		return nil, err
	}
	ivDER, err := asn1.Marshal(iv)
	if err != nil {
		return nil, err
	}
	params, err := asn1.Marshal(pbes2Params{
		KDF:    pkix.AlgorithmIdentifier{Algorithm: oidPBKDF2, Parameters: asn1.RawValue{FullBytes: kdf}},
		Scheme: pkix.AlgorithmIdentifier{Algorithm: oidAES256CBC, Parameters: asn1.RawValue{FullBytes: ivDER}},
	})
	if err != nil {
		return nil, err
	}
	return asn1.Marshal(encryptedPKCS8{Algo: pkix.AlgorithmIdentifier{Algorithm: oidPBES2,
		Parameters: asn1.RawValue{FullBytes: params}}, Data: data})
}

func decryptPKCS8(der, passphrase []byte) ([]byte, error) {
	var e encryptedPKCS8
	if _, err := asn1.Unmarshal(der, &e); err != nil || !e.Algo.Algorithm.Equal(oidPBES2) {
		return nil, invalid("Could not deserialize key data. The data may be in an incorrect format or it may be encrypted with an unsupported algorithm.")
	}
	var p pbes2Params
	var kp pbkdf2Params
	if _, err := asn1.Unmarshal(e.Algo.Parameters.FullBytes, &p); err != nil || !p.KDF.Algorithm.Equal(oidPBKDF2) {
		return nil, invalid("Unsupported key encryption")
	}
	if _, err := asn1.Unmarshal(p.KDF.Parameters.FullBytes, &kp); err != nil {
		return nil, invalid("Unsupported key encryption")
	}
	var h func() hash.Hash
	switch {
	case kp.PRF.Algorithm == nil || kp.PRF.Algorithm.Equal(oidHMACSHA1):
		h = sha1.New
	case kp.PRF.Algorithm.Equal(oidHMACSHA256):
		h = sha256.New
	case kp.PRF.Algorithm.Equal(oidHMACSHA512):
		h = sha512.New
	default:
		return nil, invalid("Unsupported key encryption")
	}
	keyLen := 0
	switch {
	case p.Scheme.Algorithm.Equal(oidAES128CBC):
		keyLen = 16
	case p.Scheme.Algorithm.Equal(oidAES192CBC):
		keyLen = 24
	case p.Scheme.Algorithm.Equal(oidAES256CBC):
		keyLen = 32
	default:
		return nil, invalid("Unsupported key encryption")
	}
	var iv []byte
	if _, err := asn1.Unmarshal(p.Scheme.Parameters.FullBytes, &iv); err != nil || len(iv) != aes.BlockSize {
		return nil, invalid("Unsupported key encryption")
	}
	key, err := pbkdf2.Key(h, string(passphrase), kp.Salt, kp.Iterations, keyLen)
	if err != nil {
		return nil, invalid("Incorrect password, could not decrypt key")
	}
	c, _ := aes.NewCipher(key)
	if len(e.Data) == 0 || len(e.Data)%aes.BlockSize != 0 {
		return nil, invalid("Incorrect password, could not decrypt key")
	}
	out := append([]byte(nil), e.Data...)
	cipher.NewCBCDecrypter(c, iv).CryptBlocks(out, out)
	pad := int(out[len(out)-1])
	if pad == 0 || pad > aes.BlockSize {
		return nil, invalid("Incorrect password, could not decrypt key")
	}
	for _, b := range out[len(out)-pad:] {
		if int(b) != pad {
			return nil, invalid("Incorrect password, could not decrypt key")
		}
	}
	return out[:len(out)-pad], nil
}

func parsePKCS8(der []byte) (any, error) {
	var info pkcs8Info
	if _, err := asn1.Unmarshal(der, &info); err == nil && info.Algo.Algorithm.Equal(oidDSA) {
		var params dsaParams
		var x *big.Int
		if _, err := asn1.Unmarshal(info.Algo.Parameters.FullBytes, &params); err != nil {
			return nil, invalid("Could not deserialize key data.")
		}
		if _, err := asn1.Unmarshal(info.PrivateKey, &x); err != nil {
			return nil, invalid("Could not deserialize key data.")
		}
		k := &dsa.PrivateKey{X: x}
		k.P, k.Q, k.G = params.P, params.Q, params.G
		k.Y = new(big.Int).Exp(k.G, x, k.P)
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, invalid("Could not deserialize key data.")
	}
	switch k.(type) {
	case *rsa.PrivateKey, *ecdsa.PrivateKey, ed25519.PrivateKey:
		return k, nil
	}
	return nil, invalid("Unsupported key type")
}

func parseTraditional(typ string, der []byte) (any, error) {
	switch typ {
	case "RSA PRIVATE KEY":
		if k, err := x509.ParsePKCS1PrivateKey(der); err == nil {
			return k, nil
		}
	case "EC PRIVATE KEY":
		if k, err := x509.ParseECPrivateKey(der); err == nil {
			return k, nil
		}
	case "DSA PRIVATE KEY":
		var d dsaTraditional
		if _, err := asn1.Unmarshal(der, &d); err == nil {
			k := &dsa.PrivateKey{X: d.X}
			k.P, k.Q, k.G, k.Y = d.P, d.Q, d.G, d.Y
			return k, nil
		}
	}
	return nil, invalid("Could not deserialize key data.")
}

// ParsePrivate loads a private key the way openssh_keypair's cryptography
// backend does: load_ssh_private_key, falling back to
// load_pem_private_key. A nil passphrase means none was given.
func ParsePrivate(data, passphrase []byte) (any, error) {
	if len(passphrase) == 0 {
		passphrase = nil
	}
	blk, _ := pem.Decode(data)
	if blk == nil {
		return nil, invalid("Could not deserialize key data.")
	}
	switch blk.Type {
	case "OPENSSH PRIVATE KEY":
		return parseOpenSSH(blk.Bytes, passphrase)
	case "PRIVATE KEY":
		if passphrase != nil {
			return nil, ErrPassphrase
		}
		return parsePKCS8(blk.Bytes)
	case "ENCRYPTED PRIVATE KEY":
		if passphrase == nil {
			return nil, ErrPassphrase
		}
		der, err := decryptPKCS8(blk.Bytes, passphrase)
		if err != nil {
			return nil, err
		}
		return parsePKCS8(der)
	case "RSA PRIVATE KEY", "EC PRIVATE KEY", "DSA PRIVATE KEY":
		//lint:ignore SA1019 reading OpenSSL's traditional encryption
		encrypted := x509.IsEncryptedPEMBlock(blk) //nolint:staticcheck
		if encrypted != (passphrase != nil) {
			return nil, ErrPassphrase
		}
		der := blk.Bytes
		if encrypted {
			var err error
			//lint:ignore SA1019 reading OpenSSL's traditional encryption
			if der, err = x509.DecryptPEMBlock(blk, passphrase); err != nil { //nolint:staticcheck
				return nil, invalid("Incorrect password, could not decrypt key")
			}
		}
		return parseTraditional(blk.Type, der)
	}
	if strings.HasSuffix(blk.Type, "PRIVATE KEY") {
		return nil, invalid("Could not deserialize key data.")
	}
	return nil, invalid("Valid PEM but no BEGIN/END delimiters for a private key found. Are you sure this is a private key?")
}
