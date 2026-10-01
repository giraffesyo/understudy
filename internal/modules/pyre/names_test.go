package pyre

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// TestNamesComplete checks the names \N{...} looks up against CPython's:
// every one (names.bin's, the Hangul syllables, the CJK unified and
// Tangut ideographs) looks up its code point, lower case too, and the
// set hashes to the digest gen_tables.py took of unicodedata's.
func TestNamesComplete(t *testing.T) {
	namesOnce.Do(loadNames)
	var every []string
	check := func(name string, cp rune) {
		every = append(every, fmt.Sprintf("%s\t%x", name, cp))
		if r, ok := lookupName(name); !ok || r != cp {
			t.Errorf("lookupName(%q) = %#x, %v; want %#x", name, r, ok, cp)
		}
	}
	for name, cp := range names {
		check(name, cp)
	}
	for l, ls := range jamoL {
		for v, vs := range jamoV {
			for tt, ts := range jamoT {
				check("HANGUL SYLLABLE "+ls+vs+ts, rune(0xAC00+(l*21+v)*28+tt))
			}
		}
	}
	for _, r := range nameRanges {
		for _, rg := range r.tab {
			for cp := rg.lo; cp <= rg.hi; cp++ {
				check(fmt.Sprintf("%s%04X", r.prefix, cp), cp)
			}
		}
	}
	sort.Strings(every)
	sum := sha256.Sum256([]byte(strings.Join(every, "\n") + "\n"))
	if got := hex.EncodeToString(sum[:]); got != namesDigest {
		t.Errorf("%d names hash to %s, want %s (regenerate with gen_tables.py)", len(every), got, namesDigest)
	}
	for _, name := range []string{"latin small letter a", "Hangul Syllable GAGG", "tangut ideograph-17000", "cjk unified ideograph-4e00", "line feed"} {
		if _, ok := lookupName(name); !ok {
			t.Errorf("lookupName(%q) failed", name)
		}
	}
	for _, name := range []string{"KEYCAP DIGIT ONE", "CJK UNIFIED IDEOGRAPH-04E00", "HANGUL SYLLABLE KKA", "LATIN SMALL LETTER À", "TANGUT IDEOGRAPH-18800", ""} {
		if r, ok := lookupName(name); ok {
			t.Errorf("lookupName(%q) = %#x, want none", name, r)
		}
	}
}
