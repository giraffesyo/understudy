package template

import (
	"crypto/md5"
	"crypto/rand"
	"fmt"
	"math/big"
	"strings"

	"github.com/giraffesyo/understudy/internal/yaml"
)

// password_hash is ansible-core's get_encrypted_password(password,
// hashtype='sha512', salt=None, salt_size=None, rounds=None, ident=None).
// Which library hashes depends on the controller (cryptGensalt): passlib
// (where libxcrypt is not found) or libxcrypt's crypt_gensalt and crypt.
// Each validates its arguments with its own errors. A salt that is not
// given is random, as is the hash then.

type hashAlgo struct {
	ident         string
	saltSize      int
	implicitRound int // 0: none
	maxSalt       int
}

var hashAlgos = map[string]hashAlgo{
	"md5_crypt":    {ident: "1", saltSize: 8, maxSalt: 8},
	"bcrypt":       {ident: "2b", saltSize: 22, implicitRound: 12, maxSalt: 22},
	"sha256_crypt": {ident: "5", saltSize: 16, implicitRound: 535000, maxSalt: 16},
	"sha512_crypt": {ident: "6", saltSize: 16, implicitRound: 656000, maxSalt: 16},
}

var passlibMapping = [][2]string{{"md5", "md5_crypt"}, {"blowfish", "bcrypt"}, {"sha256", "sha256_crypt"}, {"sha512", "sha512_crypt"}}

func filterPasswordHash(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
	arg := func(i int, name string, def any) any {
		if i < len(args) {
			return Undeprecate(args[i])
		}
		if v, ok := kwargs[name]; ok {
			return Undeprecate(v)
		}
		return def
	}
	hashtypeV := arg(0, "hashtype", "sha512")
	salt, saltSize, rounds, ident := arg(1, "salt", nil), arg(2, "salt_size", nil), arg(3, "rounds", nil), arg(4, "ident", nil)
	hashtype := toStr(hashtypeV)
	known := false
	for _, m := range passlibMapping {
		if hashtype == m[0] {
			hashtype = m[1]
		}
	}
	for _, m := range passlibMapping {
		known = known || hashtype == m[1]
	}
	usePasslib := !cryptGensalt()
	if usePasslib && !known {
		return nil, fmt.Errorf("%s is not in the list of supported passlib algorithms: md5, blowfish, sha256, sha512", hashtype)
	}
	if !usePasslib && !known {
		return nil, fmt.Errorf("crypt does not support %s algorithm", pyStrRepr(hashtype))
	}
	algo := hashAlgos[hashtype]
	if ec.engine.Verbose != nil {
		// BaseHash announces the backend do_encrypt picked.
		backend := "PasslibHash"
		if !usePasslib {
			backend = "CryptHash"
		}
		ec.engine.Verbose(2, fmt.Sprintf("Using %s to hash input with '%s'", backend, hashtype))
	}
	if usePasslib {
		return passlibHash(hashtype, algo, Undeprecate(in), salt, saltSize, rounds, ident, ec.fromVar(-1))
	}
	return libxcryptHash(hashtype, algo, Undeprecate(in), salt, saltSize, rounds, ident)
}

// passlibHash is PasslibHash.hash: passlib's CryptContext handler
// .using(salt=, salt_size=, rounds=).hash(secret).
func passlibHash(name string, algo hashAlgo, secret, salt, saltSize, rounds, ident any, secretFromVar bool) (any, error) {
	var saltStr string
	hasSalt := truthy(salt)
	if hasSalt {
		saltStr = toStr(salt)
	}
	if !truthy(rounds) {
		rounds = nil
		if algo.implicitRound != 0 {
			rounds = int64(algo.implicitRound)
		}
	}
	if saltSize != nil {
		if _, ok := saltSize.(bool); ok {
		} else if _, ok := asInt(saltSize); !ok {
			return nil, fmt.Errorf("salt_size must be an integer")
		}
	}
	could := func(format string, a ...any) error {
		return fmt.Errorf("Could not hash the secret: "+format, a...)
	}
	if name == "bcrypt" {
		return passlibBcrypt(secret, salt, saltSize, rounds, ident, secretFromVar)
	}
	// HasSalt.using: the salt size, then the salt.
	size := algo.saltSize
	if n, ok := asInt(saltSize); ok && truthy(saltSize) {
		size = min(int(n), algo.maxSalt)
	}
	if hasSalt {
		for _, c := range saltStr {
			if !strings.ContainsRune(cryptAlphabet, c) {
				return nil, could("invalid characters in %s salt", name)
			}
		}
		if len(saltStr) > algo.maxSalt {
			return nil, could("salt too large (%s requires <= %d chars)", name, algo.maxSalt)
		}
	} else {
		saltStr = randomSalt(size)
	}
	// HasRounds.using.
	nRounds := 0
	if rounds != nil && name != "md5_crypt" {
		switch r := rounds.(type) {
		case string, yaml.UnsafeString:
			s, _ := asString(r)
			v, ok := pyParseInt(s, 10)
			if !ok {
				return nil, could("invalid literal for int() with base 10: %s", pyStrRepr(s))
			}
			rounds = v
		case float64:
			return nil, fmt.Errorf("min_desired_rounds must be integer, not float")
		}
		n, ok := asInt(rounds)
		if !ok {
			return nil, fmt.Errorf("min_desired_rounds must be integer, not %s", pyClassName(rounds, false))
		}
		if n < 1000 {
			return nil, could("%s: min_desired_rounds (%d) is too low, must be at least 1000", name, n)
		}
		if n > 999999999 {
			return nil, could("%s: min_desired_rounds (%d) is too large, cannot be more than 999999999", name, n)
		}
		nRounds = int(n)
	}
	// .hash(secret)
	pw, ok := asString(secret)
	if !ok {
		return nil, fmt.Errorf("secret must be unicode or bytes, not %s", passlibTypeName(secret, secretFromVar))
	}
	if name == "md5_crypt" {
		return md5Crypt(pw, saltStr), nil
	}
	return shaCrypt(pw, saltStr, nRounds, name == "sha512_crypt"), nil
}

// passlibTypeName is passlib's type_name: a builtin's name, else the
// class with its module.
func passlibTypeName(v any, fromVar bool) string {
	if v == nil {
		return "None"
	}
	name := pyOperandClass(v, fromVar)
	switch {
	case strings.HasPrefix(name, "_AnsibleLazy"):
		return "ansible._internal._templating._lazy_containers." + name
	case strings.HasPrefix(name, "_AnsibleTagged"):
		return "ansible.module_utils._internal._datatag." + name
	}
	return name
}

// libxcryptHash is CryptHash.hash with crypt_gensalt: a given salt is
// checked and becomes the random bytes gensalt encodes.
func libxcryptHash(name string, algo hashAlgo, secret, salt, saltSize, rounds, ident any) (any, error) {
	if name == "bcrypt" {
		return libxcryptBcrypt(secret, salt, saltSize, rounds, ident)
	}
	size := algo.saltSize
	if saltSize != nil {
		n, ok := asInt(saltSize)
		if _, isBool := saltSize.(bool); !ok || isBool {
			return nil, fmt.Errorf("salt_size must be an integer")
		}
		if n != 0 {
			size = int(n)
		}
		if size > algo.saltSize {
			return nil, fmt.Errorf("invalid salt size supplied (%d), expected at most %d", size, algo.saltSize)
		}
	}
	var saltStr string
	if salt != nil {
		saltStr = toStr(salt)
		if saltStr == "" {
			saltStr = randomSalt(size)
		}
		for _, c := range saltStr {
			if !strings.ContainsRune(cryptAlphabet, c) {
				return nil, fmt.Errorf("invalid characters in salt")
			}
		}
		if len(saltStr) > algo.saltSize {
			return nil, fmt.Errorf("invalid salt size supplied (%d), expected at most %d", len(saltStr), algo.saltSize)
		}
		var err error
		if saltStr, err = cryptSalt(saltStr); err != nil {
			return nil, fmt.Errorf("Failed to generate salt for %s algorithm", pyStrRepr(name))
		}
	} else {
		saltStr = randomSalt(size)
	}
	pw := toStr(secret)
	if name == "md5_crypt" {
		return md5Crypt(pw, saltStr), nil
	}
	n := algo.implicitRound
	if truthy(rounds) {
		r, ok := asInt(rounds)
		if !ok {
			return nil, fmt.Errorf("Failed to generate salt for %s algorithm", pyStrRepr(name))
		}
		n = int(r)
	}
	return shaCrypt(pw, saltStr, n, name == "sha512_crypt"), nil
}

// randomSalt is n characters of the crypt alphabet.
func randomSalt(n int) string {
	b := make([]byte, n)
	for i := range b {
		v, _ := rand.Int(rand.Reader, big.NewInt(int64(len(cryptAlphabet))))
		b[i] = cryptAlphabet[v.Int64()]
	}
	return string(b)
}

// md5Crypt is the FreeBSD MD5-crypt scheme ($1$).
func md5Crypt(password, salt string) string {
	if len(salt) > 8 {
		salt = salt[:8]
	}
	pw := []byte(password)
	ctx := md5.New()
	ctx.Write(pw)
	ctx.Write([]byte("$1$"))
	ctx.Write([]byte(salt))
	alt := md5.New()
	alt.Write(pw)
	alt.Write([]byte(salt))
	alt.Write(pw)
	altSum := alt.Sum(nil)
	for i := len(pw); i > 0; i -= 16 {
		ctx.Write(altSum[:min(16, i)])
	}
	for i := len(pw); i > 0; i >>= 1 {
		if i&1 != 0 {
			ctx.Write([]byte{0})
		} else {
			ctx.Write(pw[:1])
		}
	}
	final := ctx.Sum(nil)
	for i := range 1000 {
		c := md5.New()
		if i&1 != 0 {
			c.Write(pw)
		} else {
			c.Write(final)
		}
		if i%3 != 0 {
			c.Write([]byte(salt))
		}
		if i%7 != 0 {
			c.Write(pw)
		}
		if i&1 != 0 {
			c.Write(final)
		} else {
			c.Write(pw)
		}
		final = c.Sum(nil)
	}
	var b strings.Builder
	b.WriteString("$1$" + salt + "$")
	to64 := func(v uint32, n int) {
		for ; n > 0; n-- {
			b.WriteByte(cryptAlphabet[v&63])
			v >>= 6
		}
	}
	for _, g := range [][3]int{{0, 6, 12}, {1, 7, 13}, {2, 8, 14}, {3, 9, 15}, {4, 10, 5}} {
		to64(uint32(final[g[0]])<<16|uint32(final[g[1]])<<8|uint32(final[g[2]]), 4)
	}
	to64(uint32(final[11]), 2)
	return b.String()
}

// passlibBcrypt is PasslibHash.hash for bcrypt: ansible-core repairs a
// salt's unused bits and defaults the cost (12) and ident ("2b"); then
// passlib's bcrypt.using(salt_size=, salt=, rounds=, ident=) validates
// each in that order, and .hash(secret) the secret.
func passlibBcrypt(secret, salt, saltSize, rounds, ident any, secretFromVar bool) (any, error) {
	could := func(format string, a ...any) error {
		return fmt.Errorf("Could not hash the secret: "+format, a...)
	}
	var saltStr string
	hasSalt := truthy(salt)
	if hasSalt {
		var err error
		if saltStr, err = bcryptRepairUnused(toStr(salt)); err != nil {
			return nil, err
		}
	}
	if !truthy(rounds) {
		rounds = int64(12)
	}
	identStr := "2b"
	if truthy(ident) {
		identStr = toStr(ident)
		if _, isStr := asString(ident); !isStr {
			identStr = "\x00" + pyRepr(ident)
		}
	}
	if saltSize != nil {
		if _, ok := asInt(saltSize); !ok {
			return nil, fmt.Errorf("salt_size must be an integer")
		}
		if n, _ := asInt(saltSize); truthy(saltSize) && n != 22 {
			return nil, could("bcrypt: salt_size (%d) must be exactly 22", n)
		}
	}
	if hasSalt {
		for _, c := range saltStr {
			if c > 0x7f || bcryptIndex(byte(c)) < 0 {
				return nil, could("invalid characters in bcrypt salt")
			}
		}
		switch n := len([]rune(saltStr)); {
		case n < 22:
			return nil, could("salt too small (bcrypt requires exactly 22 chars)")
		case n > 22:
			return nil, could("salt too large (bcrypt requires exactly 22 chars)")
		}
	} else {
		saltStr = randomSalt(21) + string(".Oeu"[randomIndex(4)])
	}
	switch r := rounds.(type) {
	case string, yaml.UnsafeString:
		rs, _ := asString(r)
		v, ok := pyParseInt(rs, 10)
		if !ok {
			return nil, could("invalid literal for int() with base 10: %s", pyStrRepr(rs))
		}
		rounds = v
	case float64:
		return nil, fmt.Errorf("min_desired_rounds must be integer, not float")
	}
	cost, ok := asInt(rounds)
	if !ok {
		return nil, fmt.Errorf("min_desired_rounds must be integer, not %s", pyClassName(rounds, false))
	}
	if cost < 4 {
		return nil, could("bcrypt: min_desired_rounds (%d) is too low, must be at least 4", cost)
	}
	if cost > 31 {
		return nil, could("bcrypt: min_desired_rounds (%d) is too large, cannot be more than 31", cost)
	}
	switch identStr {
	case "2", "2a", "2y", "2b":
	case "$2$", "$2a$", "$2y$", "$2b$":
		identStr = identStr[1 : len(identStr)-1]
	default:
		if strings.HasPrefix(identStr, "\x00") {
			return nil, could("invalid ident: %s", identStr[1:])
		}
		return nil, could("invalid ident: %s", pyStrRepr(identStr))
	}
	pw, ok := asString(secret)
	if !ok {
		return nil, fmt.Errorf("secret must be unicode or bytes, not %s", passlibTypeName(secret, secretFromVar))
	}
	b := []byte(pw)
	if strings.IndexByte(pw, 0) >= 0 {
		return nil, could("bcrypt does not allow NULL bytes in password")
	}
	if identStr == "2" && len(b) > 0 {
		// The legacy $2$ hash, which the backend lacks: the password
		// repeated to 72 bytes (ending on a UTF-8 boundary) hashed as $2b$.
		for len(b) < 72 {
			b = append(b, pw...)
		}
		end := 72
		for end < len(b) && b[end]&0xc0 == 0x80 {
			end++
		}
		b = b[:end]
	}
	return bcryptCrypt(b, identStr, saltStr, int(cost)), nil
}

// bcryptRepairUnused is passlib's bcrypt64.repair_unused: the unused low
// bits of a salt's last character cleared.
func bcryptRepairUnused(s string) (string, error) {
	r := []rune(s)
	var mask int
	switch len(r) & 3 {
	case 0:
		return s, nil
	case 1:
		return "", fmt.Errorf("source length must != 1 mod 4")
	case 2:
		mask = 0x30
	case 3:
		mask = 0x3c
	}
	last := r[len(r)-1]
	idx := -1
	if last < 0x80 {
		idx = bcryptIndex(byte(last))
	}
	if idx < 0 {
		return "", fmt.Errorf("substring not found")
	}
	r[len(r)-1] = rune(bcryptAlphabet[idx&mask])
	return string(r), nil
}

// libxcryptBcrypt is CryptHash.hash for bcrypt with crypt_gensalt: a
// given salt (exactly 22 salt characters) is the random bytes gensalt
// encodes, of which bcrypt uses 16.
func libxcryptBcrypt(secret, salt, saltSize, rounds, ident any) (any, error) {
	failed := fmt.Errorf("Failed to generate salt for 'bcrypt' algorithm")
	cost := int64(12)
	if truthy(rounds) {
		n, ok := asInt(rounds)
		if !ok {
			return nil, failed
		}
		cost = n
	}
	identStr := "2b"
	if truthy(ident) {
		identStr = toStr(ident)
	}
	size := 22
	if saltSize != nil {
		n, ok := asInt(saltSize)
		if _, isBool := saltSize.(bool); !ok || isBool {
			return nil, fmt.Errorf("salt_size must be an integer")
		}
		if n != 0 {
			size = int(n)
		}
	}
	var raw []byte
	if salt != nil {
		if size != 22 {
			return nil, fmt.Errorf("invalid salt size supplied (%d), expected 22", size)
		}
		s := toStr(salt)
		if s == "" {
			s = randomSalt(22)
		}
		for _, c := range s {
			if !strings.ContainsRune(cryptAlphabet, c) {
				return nil, fmt.Errorf("invalid characters in salt")
			}
		}
		if len(s) != 22 {
			return nil, fmt.Errorf("invalid salt size supplied (%d), expected 22", len(s))
		}
		raw = []byte(s)
	} else {
		if size != 22 {
			return nil, fmt.Errorf("invalid salt size supplied (%d), expected 22", size)
		}
		raw = make([]byte, 22)
		_, _ = rand.Read(raw)
	}
	switch identStr {
	case "2a", "2b", "2y":
	default:
		return nil, failed
	}
	if cost < 4 || cost > 31 {
		return nil, failed
	}
	salt22 := bcrypt64.EncodeToString(raw[:16])
	return bcryptCrypt([]byte(toStr(secret)), identStr, salt22, int(cost)), nil
}

// randomIndex is a random int in [0, n).
func randomIndex(n int) int {
	v, _ := rand.Int(rand.Reader, big.NewInt(int64(n)))
	return int(v.Int64())
}
