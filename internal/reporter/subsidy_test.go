package reporter

import "testing"

func TestSubsidy(t *testing.T) {
	rvn := Subsidy{Start: 5000, Halving: 2100000}
	btc := Subsidy{Start: 50, Halving: 210000}
	for _, c := range []struct {
		s    Subsidy
		h    int64
		want int64
	}{
		{rvn, 0, 5000e8}, {rvn, 2099999, 5000e8}, {rvn, 2100000, 2500e8}, {rvn, 4626747, 1250e8},
		{btc, 840000, 3.125e8}, {btc, 210000 * 64, 0}, {Subsidy{}, 5, 0},
	} {
		if got := c.s.at(c.h); got != c.want {
			t.Errorf("%+v at %d = %d, want %d", c.s, c.h, got, c.want)
		}
	}
}
