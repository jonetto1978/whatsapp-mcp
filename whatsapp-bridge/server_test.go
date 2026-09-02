package main

import "testing"

func TestEscapeLikeNeutralisesWildcards(t *testing.T) {
	cases := map[string]string{
		`abc`:        `abc`,
		`100%`:       `100\%`,
		`a_b`:        `a\_b`,
		`back\slash`: `back\\slash`,
		`%_%`:        `\%\_\%`,
	}
	for in, want := range cases {
		if got := escapeLike(in); got != want {
			t.Errorf("escapeLike(%q) = %q, want %q", in, got, want)
		}
	}
}
