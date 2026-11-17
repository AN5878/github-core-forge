// The scenario's deterministic identity, ported from the kit's wire.ts (FNV-1a +
// splitmix64 + xorshift64*). The entry cannot learn the viewer login from the opaque
// bearer <redacted> so it recomputes it from the fixed SCENARIO_SEED, exactly as the
// suite's buildScenario() does. viewer = users[0], teammate = users[1], stranger = users[2].

package main

import (
	"strings"
	"time"
)

const scenarioSeed uint64 = 0x6a09e667f3bcc909

// The scenario's day one, 2025-01-01T00:00:00Z.
const scenarioEpochSec = 1735689600

func fnv1a64(text string) uint64 {
	hash := uint64(0xcbf29ce484222325)
	for i := 0; i < len(text); i++ {
		hash ^= uint64(text[i])
		hash *= 0x100000001b3
	}
	return hash
}

func mix64(value uint64) uint64 {
	z := value + 0x9e3779b97f4a7c15
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

func scenarioSeedFor(label string) uint64 {
	return mix64(scenarioSeed ^ fnv1a64(label))
}

// Rng is a xorshift64* generator with rejection sampling, matching the kit.
type Rng struct {
	state uint64
}

func newRng(seed uint64) *Rng {
	if seed == 0 {
		seed = scenarioSeed
	}
	return &Rng{state: seed}
}

func (r *Rng) nextU64() uint64 {
	x := r.state
	x ^= x >> 12
	x ^= x << 25
	x ^= x >> 27
	r.state = x
	return x * 0x2545f4914f6cdd1d
}

func (r *Rng) nextU32() uint32 {
	return uint32(r.nextU64() >> 32)
}

func (r *Rng) nextInt(maxExclusive int) int {
	if maxExclusive == 1 {
		return 0
	}
	const rangeSize uint64 = 4294967296
	limit := rangeSize - (rangeSize % uint64(maxExclusive))
	for {
		v := r.nextU32()
		if uint64(v) < limit {
			return int(v % uint32(maxExclusive))
		}
	}
}

func (r *Rng) pick(items []string) string {
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

// ScenarioUser is one deterministic scenario identity.
type ScenarioUser struct {
	ID    int
	Login string
	Name  string
	Email string
}

func generateUsers(count int) []ScenarioUser {
	rng := newRng(scenarioSeedFor("users"))
	used := map[string]bool{}
	users := []ScenarioUser{}
	for i := 0; i < count; i++ {
		first := rng.pick(firstNames)
		last := rng.pick(lastNames)
		base := strings.Map(func(r rune) rune {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
				return r
			}
			return -1
		}, first+last)
		login := base
		suffix := 2
		for used[login] {
			login = base + "-" + itoa(suffix)
			suffix++
		}
		used[login] = true
		users = append(users, ScenarioUser{ID: 1000 + i, Login: login, Name: first + " " + last, Email: login + "@example.invalid"})
	}
	return users
}

var users = generateUsers(12)

// VIEWER is the token owner's login (users[0]).
var VIEWER = users[0]

// TEAMMATE is users[1].
var TEAMMATE = users[1]

// STRANGER is users[2]: same repo names under this login are 404, never 403.
var STRANGER = users[2]

// DEFAULT_BRANCH is the branch every repository is born with.
const DEFAULT_BRANCH = "main"

// DetClock is a deterministic clock: the scenario epoch plus a sequence number.
// Every visible timestamp derives from this counter, never from the wall clock,
// so two runs on the same instance leave the same state.
type DetClock struct {
	seq int
}

// Next returns the next second offset from the epoch and its ISO string.
func (c *DetClock) Next() (epochSec int, iso string) {
	c.seq++
	epochSec = scenarioEpochSec + c.seq
	iso = time.Unix(int64(epochSec), 0).UTC().Format("2006-01-02T15:04:05.000Z07:00")
	return epochSec, iso
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
