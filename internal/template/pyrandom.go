package template

import (
	"crypto/rand"
	"crypto/sha512"
	"encoding/binary"
	"fmt"
	"math/big"

	"github.com/giraffesyo/understudy/internal/yaml"
)

// pyRandom is CPython's random.Random: the Mersenne Twister seeded as
// random.seed seeds it, so a seeded random or shuffle filter picks what
// ansible-core's does (`59 | random(seed=inventory_hostname)`).
type pyRandom struct {
	mt  [624]uint32
	idx int
}

// newPyRandom is random.Random(seed); a nil seed draws one from the OS,
// as SystemRandom would.
func newPyRandom(seed any) (*pyRandom, error) {
	r := &pyRandom{}
	var n *big.Int
	switch t := Undeprecate(seed).(type) {
	case nil:
		var b [32]byte
		_, _ = rand.Read(b[:])
		n = new(big.Int).SetBytes(b[:])
	case bool:
		n = big.NewInt(0)
		if t {
			n.SetInt64(1)
		}
	case int64:
		n = new(big.Int).Abs(big.NewInt(t))
	case int:
		n = new(big.Int).Abs(big.NewInt(int64(t)))
	case *big.Int:
		n = new(big.Int).Abs(t)
	case float64:
		h, ok := pyHash(t)
		if !ok {
			h = 0
		}
		n = new(big.Int).SetUint64(uint64(h))
	case string, yaml.UnsafeString:
		s, _ := asString(t)
		sum := sha512.Sum512([]byte(s))
		n = new(big.Int).SetBytes(append([]byte(s), sum[:]...))
	default:
		return nil, fmt.Errorf("The only supported seed types are:\nNone, int, float, str, bytes, and bytearray.")
	}
	// The seed's 32-bit words, least significant first.
	var key []uint32
	b := n.Bytes() // big-endian
	for len(b)%4 != 0 {
		b = append([]byte{0}, b...)
	}
	for i := len(b); i > 0; i -= 4 {
		key = append(key, binary.BigEndian.Uint32(b[i-4:i]))
	}
	if len(key) == 0 {
		key = []uint32{0}
	}
	r.initByArray(key)
	return r, nil
}

func (r *pyRandom) initGenrand(s uint32) {
	r.mt[0] = s
	for i := 1; i < 624; i++ {
		r.mt[i] = 1812433253*(r.mt[i-1]^(r.mt[i-1]>>30)) + uint32(i)
	}
	r.idx = 624
}

func (r *pyRandom) initByArray(key []uint32) {
	r.initGenrand(19650218)
	i, j := 1, 0
	for k := max(624, len(key)); k > 0; k-- {
		r.mt[i] = (r.mt[i] ^ ((r.mt[i-1] ^ (r.mt[i-1] >> 30)) * 1664525)) + key[j] + uint32(j)
		i++
		j++
		if i >= 624 {
			r.mt[0] = r.mt[623]
			i = 1
		}
		if j >= len(key) {
			j = 0
		}
	}
	for k := 623; k > 0; k-- {
		r.mt[i] = (r.mt[i] ^ ((r.mt[i-1] ^ (r.mt[i-1] >> 30)) * 1566083941)) - uint32(i)
		i++
		if i >= 624 {
			r.mt[0] = r.mt[623]
			i = 1
		}
	}
	r.mt[0] = 0x80000000
}

func (r *pyRandom) uint32() uint32 {
	const upper, lower, matrixA = 0x80000000, 0x7fffffff, 0x9908b0df
	if r.idx >= 624 {
		for k := 0; k < 624; k++ {
			y := (r.mt[k] & upper) | (r.mt[(k+1)%624] & lower)
			v := r.mt[(k+397)%624] ^ (y >> 1)
			if y&1 != 0 {
				v ^= matrixA
			}
			r.mt[k] = v
		}
		r.idx = 0
	}
	y := r.mt[r.idx]
	r.idx++
	y ^= y >> 11
	y ^= (y << 7) & 0x9d2c5680
	y ^= (y << 15) & 0xefc60000
	y ^= y >> 18
	return y
}

// getrandbits is random.getrandbits(k).
func (r *pyRandom) getrandbits(k int) *big.Int {
	if k == 0 {
		return new(big.Int)
	}
	if k <= 32 {
		return new(big.Int).SetUint64(uint64(r.uint32() >> (32 - k)))
	}
	words := (k-1)/32 + 1
	buf := make([]byte, words*4)
	for i := 0; i < words; i, k = i+1, k-32 {
		w := r.uint32()
		if k < 32 {
			w >>= 32 - k
		}
		binary.BigEndian.PutUint32(buf[(words-1-i)*4:], w)
	}
	return new(big.Int).SetBytes(buf)
}

// randbelow is Random._randbelow(n), n > 0.
func (r *pyRandom) randbelow(n *big.Int) *big.Int {
	k := n.BitLen()
	v := r.getrandbits(k)
	for v.Cmp(n) >= 0 {
		v = r.getrandbits(k)
	}
	return v
}

func (r *pyRandom) randbelowInt(n int) int {
	return int(r.randbelow(big.NewInt(int64(n))).Int64())
}

// shuffle is Random.shuffle.
func (r *pyRandom) shuffle(x []any) {
	for i := len(x) - 1; i > 0; i-- {
		j := r.randbelowInt(i + 1)
		x[i], x[j] = x[j], x[i]
	}
}

// pyIndex is operator.index(v).
func pyIndex(v any) (*big.Int, error) {
	switch t := Undeprecate(v).(type) {
	case bool:
		if t {
			return big.NewInt(1), nil
		}
		return big.NewInt(0), nil
	case int64:
		return big.NewInt(t), nil
	case int:
		return big.NewInt(int64(t)), nil
	case *big.Int:
		return t, nil
	}
	return nil, fmt.Errorf("'%s' object cannot be interpreted as an integer", pyClassName(v, false))
}

// pyRandrange is Random.randrange(start, stop, step).
func pyRandrange(r *pyRandom, start, stop, step any) (any, error) {
	istart, err := pyIndex(start)
	if err != nil {
		return nil, err
	}
	istop, err := pyIndex(stop)
	if err != nil {
		return nil, err
	}
	width := new(big.Int).Sub(istop, istart)
	istep, err := pyIndex(step)
	if err != nil {
		return nil, err
	}
	if istep.Cmp(big.NewInt(1)) == 0 {
		if width.Sign() > 0 {
			return normInt(new(big.Int).Add(istart, r.randbelow(width))), nil
		}
		return nil, fmt.Errorf("empty range in randrange(%s, %s)", toStr(start), toStr(stop))
	}
	var n *big.Int
	switch istep.Sign() {
	case 1:
		n = new(big.Int).Add(width, istep)
		n.Sub(n, big.NewInt(1))
	case -1:
		n = new(big.Int).Add(width, istep)
		n.Add(n, big.NewInt(1))
	default:
		return nil, fmt.Errorf("zero step for randrange()")
	}
	n = pyFloorDiv(n, istep)
	if n.Sign() <= 0 {
		return nil, fmt.Errorf("empty range in randrange(%s, %s, %s)", toStr(start), toStr(stop), toStr(step))
	}
	off := new(big.Int).Mul(istep, r.randbelow(n))
	return normInt(off.Add(off, istart)), nil
}

func pyFloorDiv(a, b *big.Int) *big.Int {
	q, m := new(big.Int).QuoRem(a, b, new(big.Int))
	if m.Sign() != 0 && (m.Sign() < 0) != (b.Sign() < 0) {
		q.Sub(q, big.NewInt(1))
	}
	return q
}
