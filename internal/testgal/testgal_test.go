package testgal

import (
	"strings"
	"testing"
)

func TestGenerateIsDeterministicAndShaped(t *testing.T) {
	a := Generate(3000, 1, []string{"corp.example", "sub.example"}, "outside.example")
	b := Generate(3000, 1, []string{"corp.example", "sub.example"}, "outside.example")
	if len(a) != 3000 {
		t.Fatal(len(a))
	}
	seen := map[string]bool{}
	ext, ar := 0, 0
	for i := range a {
		if a[i] != b[i] {
			t.Fatal("not deterministic")
		}
		if seen[a[i].Email] || seen[a[i].Alias] {
			t.Fatalf("duplicate %s", a[i].Email)
		}
		seen[a[i].Email], seen[a[i].Alias] = true, true
		for _, r := range a[i].Alias {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.') {
				t.Fatalf("alias %q is not in the alphabet", a[i].Alias)
			}
		}
		if a[i].External {
			ext++
			if !strings.HasSuffix(a[i].Email, "@outside.example") {
				t.Fatal("external")
			}
		}
		if a[i].Arabic {
			ar++
		}
	}
	if ext < 100 || ext > 400 || ar < 200 || ar > 600 {
		t.Errorf("external %d, arabic %d", ext, ar)
	}
	// Short prefixes saturate: some two-letter prefix matches more than 100 entries.
	big := 0
	for _, p := range []string{"mo", "ah", "ab", "kh"} {
		n := 0
		for _, e := range a {
			if e.MatchesANR(p) {
				n++
			}
		}
		if n > big {
			big = n
		}
	}
	if big <= 100 {
		t.Errorf("no prefix saturates: %d", big)
	}
}
