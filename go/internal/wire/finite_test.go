package wire

import (
	"errors"
	"math"
	mbig "math/big"
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"
)

// finiteAsFloat64 agrees with ParseFloat around the overflow threshold, 2^1024 - 2^970, and far from it.
func TestFiniteAsFloat64MatchesParseFloat(t *testing.T) {
	half := new(mbig.Int).Sub(new(mbig.Int).Lsh(mbig.NewInt(1), 1024), new(mbig.Int).Lsh(mbig.NewInt(1), 970)).String()
	if half != overflowDigits {
		t.Fatal("overflowDigits is not 2^1024 - 2^970")
	}
	below := half[:len(half)-1] + "1"
	cases := []string{
		"0", "-0", "1", "-1", "1.5", "1e308", "1e309", "-1e309", "1e-400", "0e999999999999", "0.0e5",
		"1.7976931348623157e308", "1.7976931348623158e308", "1.7976931348623159e308", "-1.7976931348623159e308",
		"1.797693134862315807937289714053e308", "1.797693134862315807937289714054e308", half, "-" + half, below,
		half + ".0", half + ".0000001", below + ".99999", "0." + half + "e309", "0." + below + "e309", "000" + half,
		strings.Repeat("9", 309), strings.Repeat("9", 310), "1" + strings.Repeat("0", 308),
		"17976931348623158079e289", "17976931348623158080e289", "1e99999999999999999999", "1E+308", "1E-308",
		"10e307", "100e306", "0.1e310",
		// Exponents whose accumulation overflows a 32-bit int.
		"1e2147483648", "1e4294967296", "1e2147483647", "1e-2147483648", "1e-4294967296", "1e-99999999999999999999",
	}
	for range 200 {
		var d strings.Builder
		for range 1 + rand.IntN(30) {
			d.WriteByte(byte('0' + rand.IntN(10)))
		}
		cases = append(cases, "1"+d.String()+"e"+strconv.Itoa(270+rand.IntN(51)))
	}
	for _, c := range cases {
		f, err := strconv.ParseFloat(c, 64)
		want := !(math.IsInf(f, 0) && (err == nil || errors.Is(err, strconv.ErrRange)))
		if got := finiteAsFloat64([]byte(c)); got != want {
			t.Errorf("%.40s: %v, want %v", c, got, want)
		}
	}
	for _, c := range []string{"-", "1.", ".5", "1e", "1e+", "--1", "1-2"} {
		if !finiteAsFloat64([]byte(c)) {
			t.Errorf("%s: not a number, and refused", c)
		}
	}
}
