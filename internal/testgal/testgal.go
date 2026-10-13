// Package testgal generates a Global Address List for tests: a few thousand entries with
// common first and last names that are shared by many people (so that short prefixes
// match far more than a search window holds), Arabic display names with Latin aliases,
// and contacts of other domains. It is deterministic. Test support only; nothing in the
// production binary imports it.
package testgal

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
)

// Person is one entry.
type Person struct {
	Display, First, Last string
	Alias, Email         string
	Title, Company       string
	Office               string
	Phone, Mobile        string
	// External: the entry is a mail contact of another domain.
	External bool
	// Arabic: the display name, first and last name are Arabic; the alias is Latin.
	Arabic bool
}

var latinFirst = []string{"mohammed", "ahmed", "abdullah", "khalid", "fahad", "sara", "noura", "maha", "omar", "ali", "yousef", "hassan", "noorhan", "reem", "salman", "turki", "nasser", "huda", "ibrahim", "faisal"}
var latinLast = []string{"alsulami", "alotaibi", "alharbi", "alqahtani", "alzahrani", "alshehri", "aldosari", "almutairi", "alsubaie", "alanazi", "bakr", "hamdan", "saleh", "nasser"}

// syllable returns a made-up name of two or three syllables: the diverse part of the GAL.
func syllable(r *rand.Rand) string {
	const cons, vow = "bdfghjklmnprstwyz", "aeiou"
	n := 2 + r.Intn(2)
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteByte(cons[r.Intn(len(cons))])
		b.WriteByte(vow[r.Intn(len(vow))])
		if r.Intn(3) == 0 {
			b.WriteByte(cons[r.Intn(len(cons))])
		}
	}
	return b.String()
}

var arabicFirst = []string{"محمد", "أحمد", "عبدالله", "خالد", "فهد", "سارة", "نورة", "مها", "عمر", "علي", "يوسف", "حسن"}
var arabicLast = []string{"السلمي", "العتيبي", "الحربي", "القحطاني", "الزهراني", "الشهري", "الدوسري", "المطيري"}

// Generate returns n entries. Roughly 12 percent are Arabic, 8 percent external. The
// in-domain entries are spread over domains, round robin. Every alias is unique and
// ASCII, and no alias is a prefix of another that is shared by 100 or more entries
// (aliases end in a number), so a prefix walk can reach everyone.
func Generate(n int, seed int64, domains []string, external string) []Person {
	r := rand.New(rand.NewSource(seed))
	out := make([]Person, 0, n)
	used := map[string]bool{}
	for i := 0; i < n; i++ {
		var p Person
		// Three in ten names are common ones that many people share (their prefixes match far
		// more than a search returns); the rest are made up and spread over the alphabet.
		first, last := syllable(r), syllable(r)
		if r.Intn(100) < 30 {
			first = latinFirst[r.Intn(len(latinFirst))]
		}
		if r.Intn(100) < 30 {
			last = latinLast[r.Intn(len(latinLast))]
		}
		switch {
		case r.Intn(100) < 12:
			p.Arabic = true
			p.First, p.Last = arabicFirst[r.Intn(len(arabicFirst))], arabicLast[r.Intn(len(arabicLast))]
			p.Display = p.First + " " + p.Last
		default:
			p.First, p.Last = strings.Title(first), strings.Title(strings.TrimPrefix(last, "al")) //nolint
			p.Display = strings.Title(first) + " " + strings.Title(last)
		}
		base := first[:1] + last
		if r.Intn(2) == 0 {
			base = first + "." + last
		}
		alias := base
		for k := 1; used[alias]; k++ {
			alias = fmt.Sprintf("%s%d", base, k)
		}
		used[alias] = true
		p.Alias = alias
		p.External = r.Intn(100) < 8
		dom := domains[i%len(domains)]
		if p.External {
			dom = external
		}
		p.Email = alias + "@" + dom
		p.Title = []string{"Engineer", "Analyst", "Director", "Specialist"}[r.Intn(4)]
		p.Company = "Example Co"
		p.Office = fmt.Sprintf("B%d", 1+r.Intn(9))
		p.Phone = fmt.Sprintf("+1 555 11 555 %04d", i%10000)
		p.Mobile = fmt.Sprintf("+1 555 50 555 %04d", (i*7)%10000)
		out = append(out, p)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Email < out[j].Email })
	return out
}

// InDomain returns the entries whose address is in one of domains.
func InDomain(ps []Person, domains ...string) []Person {
	in := map[string]bool{}
	for _, d := range domains {
		in[strings.ToLower(d)] = true
	}
	var out []Person
	for _, p := range ps {
		if _, d, ok := strings.Cut(p.Email, "@"); ok && in[strings.ToLower(d)] {
			out = append(out, p)
		}
	}
	return out
}

// MatchesANR reports whether q (lower case) is the start of the display name, the first
// name, the last name or the alias, the way Exchange's ambiguous name resolution matches.
func (p Person) MatchesANR(q string) bool {
	for _, f := range []string{p.Display, p.First, p.Last, p.Alias} {
		if strings.HasPrefix(strings.ToLower(f), q) {
			return true
		}
	}
	return false
}
