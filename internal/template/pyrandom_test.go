package template

import (
	"fmt"
	"math/big"
	"strings"
	"testing"
)

// TestPyRandom checks seeded draws against CPython 3.14's random.Random.
func TestPyRandom(t *testing.T) {
	big70, _ := new(big.Int).SetString("1180591620717411303429", 10)
	cases := []struct {
		seed    any
		first3  string // randrange(0, 60, 1) three times
		huge    string // randrange(0, 10**30, 1)
		shuffle string // shuffle(range(10))
		choice  string // choice('abcdefghij')
		stepped string // randrange(5, 100, 7)
	}{
		{int64(0), "[54 24 48]", "198441737587687207744109680147", "[7 8 1 5 3 4 2 0 9 6]", "g", "96"},
		{int64(1), "[8 36 54]", "99436813185968347955962756036", "[6 8 9 7 5 3 0 4 1 2]", "c", "19"},
		{int64(42), "[40 7 1]", "873491343714207852616756591005", "[7 3 2 8 5 6 9 4 0 1]", "b", "75"},
		{"web01", "[22 24 0]", "159254664959708666288228836429", "[3 2 8 4 7 9 1 0 6 5]", "f", "40"},
		{"localhost", "[42 29 25]", "505748472312277583765479424758", "[8 0 2 1 9 3 4 5 6 7]", "h", "75"},
		{big70, "[29 24 30]", "829904723954819379215411981483", "[3 1 0 4 8 2 5 9 6 7]", "h", "54"},
		{int64(-7), "[20 9 25]", "487320478161116480663150048312", "[8 3 1 4 7 0 9 6 2 5]", "f", "40"},
		{1.5, "[35 52 16]", "178746110028709186310342992684", "[7 5 0 1 6 3 9 2 4 8]", "i", "61"},
		{true, "[8 36 54]", "99436813185968347955962756036", "[6 8 9 7 5 3 0 4 1 2]", "c", "19"},
		{strings.Repeat("x", 300), "[14 56 1]", "161244527300817996729202918866", "[4 2 9 6 5 1 7 8 0 3]", "d", "26"},
	}
	huge, _ := new(big.Int).SetString("1000000000000000000000000000000", 10)
	for _, c := range cases {
		r, _ := newPyRandom(c.seed)
		var first3 []any
		for range 3 {
			v, err := pyRandrange(r, int64(0), int64(60), int64(1))
			if err != nil {
				t.Fatal(err)
			}
			first3 = append(first3, v)
		}
		if got := fmt.Sprint(first3); got != c.first3 {
			t.Errorf("seed %v: randrange %s, want %s", c.seed, got, c.first3)
		}
		r, _ = newPyRandom(c.seed)
		if v, _ := pyRandrange(r, int64(0), huge, int64(1)); fmt.Sprint(v) != c.huge {
			t.Errorf("seed %v: huge randrange %v, want %s", c.seed, v, c.huge)
		}
		r, _ = newPyRandom(c.seed)
		list := []any{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}
		r.shuffle(list)
		if got := fmt.Sprint(list); got != c.shuffle {
			t.Errorf("seed %v: shuffle %s, want %s", c.seed, got, c.shuffle)
		}
		r, _ = newPyRandom(c.seed)
		if got := string("abcdefghij"[r.randbelowInt(10)]); got != c.choice {
			t.Errorf("seed %v: choice %s, want %s", c.seed, got, c.choice)
		}
		r, _ = newPyRandom(c.seed)
		if v, _ := pyRandrange(r, int64(5), int64(100), int64(7)); fmt.Sprint(v) != c.stepped {
			t.Errorf("seed %v: stepped randrange %v, want %s", c.seed, v, c.stepped)
		}
	}
}
