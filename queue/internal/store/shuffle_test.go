package store

import (
	"strconv"
	"testing"
)

func TestShuffleIsPermutationAndNotIdentity(t *testing.T) {
	n := 200
	ids := make([]string, n)
	for i := range ids {
		ids[i] = strconv.Itoa(i)
	}
	Shuffle(ids)
	seen := map[string]bool{}
	same := 0
	for i, v := range ids {
		if seen[v] {
			t.Fatalf("doublon %s", v)
		}
		seen[v] = true
		if v == strconv.Itoa(i) {
			same++
		}
	}
	if len(seen) != n {
		t.Fatalf("%d éléments au lieu de %d", len(seen), n)
	}
	if same > n/4 {
		t.Fatalf("%d éléments à leur place initiale sur %d : mélange suspect", same, n)
	}
}
