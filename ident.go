// Scenario identity: the deterministic user stream the suite derives from
// SCENARIO_SEED. Reimplemented from the competition wire spec so the entry
// knows the viewer's login without any network access.
package main

type user struct {
	id    int
	login string
	name  string
	email string
}

const scenarioSeed = uint64(0x6a09e667f3bcc909)

func fnv1a64(text string) uint64 {
	h := uint64(0xcbf29ce484222325)
	for i := 0; i < len(text); i++ {
		h ^= uint64(text[i])
		h *= 0x100000001b3
	}
	return h
}

func mix64(v uint64) uint64 {
	z := v + 0x9e3779b97f4a7c15
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

func scenarioSeedFor(label string) uint64 {
	return mix64(scenarioSeed ^ fnv1a64(label))
}

// rng is xorshift64*, exactly as specified: three shifts, one multiply,
// nextU32 from the top half of nextU64.
type rng struct{ s uint64 }

func newRng(label string) *rng {
	s := scenarioSeedFor(label)
	if s == 0 {
		s = scenarioSeed
	}
	return &rng{s: s}
}

func (r *rng) nextU64() uint64 {
	x := r.s
	x ^= x >> 12
	x ^= x << 25
	x ^= x >> 27
	r.s = x
	return x * 0x2545f4914f6cdd1d
}

func (r *rng) nextU32() uint32 {
	return uint32(r.nextU64() >> 32)
}

// nextInt returns a value in [0, max) with unbiased rejection sampling.
func (r *rng) nextInt(max int) int {
	if max <= 0 {
		return 0
	}
	if max == 1 {
		return 0
	}
	limit := uint32(uint64(1<<32) - (uint64(1<<32) % uint64(max)))
	for {
		v := r.nextU32()
		if v < limit {
			return int(v % uint32(max))
		}
	}
}

func (r *rng) pick(items []string) string {
	return items[r.nextInt(len(items))]
}

var firstNames = []string{
	"ada", "bram", "cora", "dmitri", "elena", "felix", "greta", "hugo", "iris", "jonas", "kira", "lars",
	"mira", "nico", "olga", "pavel", "quinn", "rosa", "sven", "tess", "ulla", "vito", "wren", "yara", "zane",
}

var lastNames = []string{
	"anders", "bauer", "costa", "dahl", "eriksen", "fischer", "garcia", "holm", "iversen", "jansen", "keller",
	"lind", "moreau", "novak", "olsen", "peters", "quist", "rossi", "sato", "turi", "ulrich", "vega", "weber",
	"xu", "yilmaz", "zimmer",
}

// generateUsers mirrors the suite's generator: first+last logins, deduped
// with a -2 suffix, ids starting at 1000.
func generateUsers(count int) []user {
	r := newRng("users")
	used := make(map[string]bool, count)
	out := make([]user, 0, count)
	for i := 0; i < count; i++ {
		first := r.pick(firstNames)
		last := r.pick(lastNames)
		base := first + last
		login := base
		suffix := 2
		for used[login] {
			login = base + "-" + itoa(suffix)
			suffix++
		}
		used[login] = true
		out = append(out, user{id: 1000 + i, login: login, name: first + " " + last, email: login + "@example.invalid"})
	}
	return out
}

// itoa renders a small non-negative integer without fmt.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
