package schedule

import (
	"fmt"
	"math"
	"slices"
	"testing"
	"time"
)

func TestSplitMix64ReferenceVectors(t *testing.T) {
	r := NewSplitMix64(0)
	want := []uint64{0xe220a8397b1dcdaf, 0x6e789e6aa1b965f4, 0x06c45d188009454f}
	for i, w := range want {
		if got := r.Uint64(); got != w {
			t.Fatalf("output %d = %#x, want %#x", i, got, w)
		}
	}
}

func TestSeed64(t *testing.T) {
	if got := Seed64(""); got != 0xe3b0c44298fc1c14 {
		t.Fatalf("Seed64(\"\") = %#x", got)
	}
	if got := Seed64("abc"); got != 0xba7816bf8f01cfea {
		t.Fatalf("Seed64(abc) = %#x", got)
	}
}

func TestUint64nRangeAndUniformity(t *testing.T) {
	r := NewSplitMix64(42)
	if r.Uint64n(0) != 0 || r.Uint64n(1) != 0 {
		t.Fatal("degenerate bounds")
	}
	const n, draws = 7, 70000
	var counts [n]int
	for range draws {
		v := r.Uint64n(n)
		if v >= n {
			t.Fatalf("out of range: %d", v)
		}
		counts[v]++
	}
	for i, c := range counts {
		if math.Abs(float64(c)-draws/n) > 0.05*draws/n {
			t.Fatalf("bucket %d count %d deviates >5%% from %d", i, c, draws/n)
		}
	}
	// A bound just above 2^63 forces the rejection branch frequently.
	big := uint64(1)<<63 + 1
	for range 1000 {
		if v := r.Uint64n(big); v >= big {
			t.Fatalf("out of range: %d", v)
		}
	}
}

func TestShuffleDeterministicPermutation(t *testing.T) {
	base := make([]int, 50)
	for i := range base {
		base[i] = i
	}
	a, b := slices.Clone(base), slices.Clone(base)
	Shuffle(a, 99)
	Shuffle(b, 99)
	if !slices.Equal(a, b) {
		t.Fatal("same seed produced different orders")
	}
	if slices.Equal(a, base) {
		t.Fatal("shuffle left 50 items in place")
	}
	sorted := slices.Clone(a)
	slices.Sort(sorted)
	if !slices.Equal(sorted, base) {
		t.Fatal("shuffle is not a permutation")
	}
	c := slices.Clone(base)
	Shuffle(c, 100)
	if slices.Equal(a, c) {
		t.Fatal("different seeds produced identical orders")
	}
	Shuffle([]int{}, 1) // no panic on empty / single
	Shuffle([]int{1}, 1)
}

var t0 = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

func abc() []Item {
	return []Item{
		{FileID: "A", Duration: 10},
		{FileID: "B", Duration: 20},
		{FileID: "C", Duration: 30},
	}
}

func ids(slots []Slot) []string {
	out := make([]string, len(slots))
	for i, s := range slots {
		out[i] = s.FileID
	}
	return out
}

func TestNowBasicWalk(t *testing.T) {
	ch := Channel{Anchor: t0}
	cases := []struct {
		at        time.Duration
		id        string
		offset    float64
		cycle     int64
		start     time.Duration
		nextIDs   []string
		nextStart time.Duration
	}{
		{0, "A", 0, 0, 0, []string{"B", "C", "A"}, 10 * time.Second},
		{9999 * time.Millisecond, "A", 9.999, 0, 0, []string{"B", "C", "A"}, 10 * time.Second},
		{10 * time.Second, "B", 0, 0, 10 * time.Second, []string{"C", "A", "B"}, 30 * time.Second},
		{15 * time.Second, "B", 5, 0, 10 * time.Second, []string{"C", "A", "B"}, 30 * time.Second},
		{59999 * time.Millisecond, "C", 29.999, 0, 30 * time.Second, []string{"A", "B", "C"}, 60 * time.Second},
		{60 * time.Second, "A", 0, 1, 60 * time.Second, []string{"B", "C", "A"}, 70 * time.Second},
		{125 * time.Second, "A", 5, 2, 120 * time.Second, []string{"B", "C", "A"}, 130 * time.Second},
	}
	for _, c := range cases {
		cur, next := Now(abc(), ch, t0.Add(c.at))
		if cur == nil {
			t.Fatalf("at %v: nil slot", c.at)
		}
		if cur.FileID != c.id || cur.Offset != c.offset || cur.Cycle != c.cycle || !cur.Start.Equal(t0.Add(c.start)) {
			t.Errorf("at %v: got %s off=%v cycle=%d start=%v", c.at, cur.FileID, cur.Offset, cur.Cycle, cur.Start)
		}
		if !cur.End.Equal(cur.Start.Add(time.Duration(cur.Duration * float64(time.Second)))) {
			t.Errorf("at %v: end %v inconsistent", c.at, cur.End)
		}
		if !slices.Equal(ids(next), c.nextIDs) {
			t.Errorf("at %v: next = %v, want %v", c.at, ids(next), c.nextIDs)
		}
		if !next[0].Start.Equal(t0.Add(c.nextStart)) || !next[0].Start.Equal(cur.End) {
			t.Errorf("at %v: next starts %v", c.at, next[0].Start)
		}
		for i := 1; i < len(next); i++ {
			if !next[i].Start.Equal(next[i-1].End) {
				t.Errorf("at %v: gap between next[%d] and next[%d]", c.at, i-1, i)
			}
		}
		if cur.Start.Location() != time.UTC {
			t.Errorf("slot times must be UTC")
		}
	}
}

func TestSeqAndIndex(t *testing.T) {
	cur, next := Now(abc(), Channel{Anchor: t0}, t0.Add(2*time.Minute+45*time.Second))
	// e = 165s, T = 60s → cycle 2, pos 45 → C (index 2)
	if cur.FileID != "C" || cur.Index != 2 || cur.Cycle != 2 || cur.Seq != 8 {
		t.Fatalf("got %+v", *cur)
	}
	if next[0].Index != 0 || next[0].Cycle != 3 || next[0].Seq != 9 {
		t.Fatalf("next wrap: %+v", next[0])
	}
}

func TestSkipsUnplayableDurations(t *testing.T) {
	items := []Item{
		{FileID: "zero", Duration: 0},
		{FileID: "A", Duration: 10},
		{FileID: "neg", Duration: -5},
		{FileID: "nan", Duration: math.NaN()},
		{FileID: "inf", Duration: math.Inf(1)},
		{FileID: "tiny", Duration: 0.0004}, // rounds to 0 ms
		{FileID: "huge", Duration: MaxItemSeconds * 2},
		{FileID: "B", Duration: 5},
	}
	valid, skipped := Filter(items)
	if !slices.Equal(ids(toSlots(valid)), []string{"A", "B"}) || len(skipped) != 6 {
		t.Fatalf("valid=%v skipped=%d", ids(toSlots(valid)), len(skipped))
	}
	if TotalSeconds(items) != 15 || Count(items) != 2 {
		t.Fatalf("total=%v count=%d", TotalSeconds(items), Count(items))
	}
	cur, _ := Now(items, Channel{Anchor: t0}, t0.Add(12*time.Second))
	if cur.FileID != "B" || cur.Offset != 2 {
		t.Fatalf("got %s %v", cur.FileID, cur.Offset)
	}
}

func toSlots(items []Item) []Slot {
	out := make([]Slot, len(items))
	for i, it := range items {
		out[i].Item = it
	}
	return out
}

func TestEmptyLineup(t *testing.T) {
	for _, items := range [][]Item{nil, {}, {{FileID: "x", Duration: 0}}} {
		if cur, next := Now(items, Channel{Anchor: t0}, t0); cur != nil || next != nil {
			t.Fatal("expected nothing on air")
		}
		if g := Guide(items, Channel{Anchor: t0}, t0, t0.Add(time.Hour)); g != nil {
			t.Fatal("expected empty guide")
		}
		if cur, _ := Position(items, Channel{}, t0, 0, 0); cur != nil {
			t.Fatal("expected nil position")
		}
		if len(OrderForCycle(items, Channel{Shuffle: true}, 3)) != 0 {
			t.Fatal("expected empty order")
		}
	}
}

func TestAnchorInFuture(t *testing.T) {
	anchor := t0.Add(time.Hour)
	ch := Channel{Anchor: anchor}
	cur, next := Now(abc(), ch, t0)
	if cur.FileID != "A" || cur.Offset != 0 || cur.Cycle != 0 || !cur.Start.Equal(anchor) {
		t.Fatalf("got %+v", *cur)
	}
	if next[0].FileID != "B" || !next[0].Start.Equal(anchor.Add(10*time.Second)) {
		t.Fatalf("next %+v", next[0])
	}
	if g := Guide(abc(), ch, t0, t0.Add(30*time.Minute)); len(g) != 0 {
		t.Fatalf("guide before anchor should be empty, got %d", len(g))
	}
	g := Guide(abc(), ch, t0, anchor.Add(25*time.Second))
	if !slices.Equal(ids(g), []string{"A", "B"}) || !g[0].Start.Equal(anchor) {
		t.Fatalf("guide straddling anchor: %v", ids(g))
	}
}

func TestTenYearsElapsedExact(t *testing.T) {
	items := []Item{
		{FileID: "A", Duration: 1337.123},
		{FileID: "B", Duration: 2721.5},
		{FileID: "C", Duration: 1.001},
		{FileID: "D", Duration: 3600.0004}, // rounds to 3600000 ms
	}
	anchor := time.Date(2016, 10, 26, 3, 4, 5, 678_000_000, time.UTC)
	now := time.Date(2026, 10, 26, 21, 13, 7, 891_000_000, time.UTC)
	ch := Channel{Anchor: anchor}
	durs := []int64{1337123, 2721500, 1001, 3600000}
	var T int64
	for _, d := range durs {
		T += d
	}
	e := now.UnixMilli() - anchor.UnixMilli()
	cycle, pos := e/T, e%T
	var idx int
	var acc int64
	for idx = 0; acc+durs[idx] <= pos; idx++ {
		acc += durs[idx]
	}
	cur, _ := Now(items, ch, now)
	if cur.Cycle != cycle || cur.Index != idx {
		t.Fatalf("cycle/index = %d/%d want %d/%d", cur.Cycle, cur.Index, cycle, idx)
	}
	wantStart := anchor.UnixMilli() + cycle*T + acc
	if cur.Start.UnixMilli() != wantStart {
		t.Fatalf("start = %d want %d", cur.Start.UnixMilli(), wantStart)
	}
	if want := float64(pos-acc) / 1000; cur.Offset != want {
		t.Fatalf("offset = %v want %v", cur.Offset, want)
	}
	if cur.Start.After(now) || !cur.End.After(now) {
		t.Fatal("current slot does not contain now")
	}
}

func TestHugeCycleCountIsConstantTime(t *testing.T) {
	// One 1 ms item anchored 10 years ago → ~3e11 cycles. Iterating cycles
	// would never finish; jumping must be instant.
	items := []Item{{FileID: "x", Duration: 0.001}}
	ch := Channel{Anchor: t0.AddDate(-10, 0, 0), Shuffle: true}
	start := time.Now()
	cur, _ := Now(items, ch, t0)
	if cur.Cycle < 3e11 {
		t.Fatalf("cycle %d", cur.Cycle)
	}
	g := Guide(items, ch, t0, t0.Add(time.Hour))
	if len(g) != MaxGuideSlots {
		t.Fatalf("guide not capped: %d", len(g))
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("too slow: %v", time.Since(start))
	}
}

func tenItems() []Item {
	out := make([]Item, 10)
	for i := range out {
		out[i] = Item{FileID: fmt.Sprintf("f%d", i), Duration: float64(60 * (i + 1))}
	}
	return out
}

func TestShuffleModes(t *testing.T) {
	items := tenItems()
	plain := OrderForCycle(items, Channel{Seed: "s"}, 7)
	if !slices.Equal(ids(toSlots(plain)), ids(toSlots(items))) {
		t.Fatal("unshuffled order must be the expanded order")
	}

	fixed := Channel{Shuffle: true, ShuffleMode: ModeFixed, Seed: "seed-1"}
	f0 := ids(toSlots(OrderForCycle(items, fixed, 0)))
	f9 := ids(toSlots(OrderForCycle(items, fixed, 9)))
	if !slices.Equal(f0, f9) {
		t.Fatal("fixed shuffle must not change between cycles")
	}
	if slices.Equal(f0, ids(toSlots(items))) {
		t.Fatal("fixed shuffle left order unchanged")
	}
	// Exactly what the spec says: Fisher–Yates with SplitMix64(Seed64(seed)).
	ref := slices.Clone(items)
	Shuffle(ref, Seed64("seed-1"))
	if !slices.Equal(f0, ids(toSlots(ref))) {
		t.Fatal("fixed order differs from reference shuffle")
	}

	per := Channel{Shuffle: true, ShuffleMode: ModePerCycle, Seed: "seed-1"}
	distinct := map[string]bool{}
	for c := int64(0); c < 20; c++ {
		o := ids(toSlots(OrderForCycle(items, per, c)))
		ref := slices.Clone(items)
		Shuffle(ref, Seed64("seed-1")^uint64(c))
		if !slices.Equal(o, ids(toSlots(ref))) {
			t.Fatalf("cycle %d differs from reference", c)
		}
		distinct[fmt.Sprint(o)] = true
	}
	if len(distinct) < 15 {
		t.Fatalf("per_cycle produced only %d distinct slates in 20 cycles", len(distinct))
	}
	unknown := Channel{Shuffle: true, ShuffleMode: "bogus", Seed: "seed-1"}
	if !slices.Equal(ids(toSlots(OrderForCycle(items, unknown, 3))), ids(toSlots(OrderForCycle(items, per, 3)))) {
		t.Fatal("unknown mode should behave as per_cycle")
	}
	other := Channel{Shuffle: true, ShuffleMode: ModeFixed, Seed: "seed-2"}
	if slices.Equal(f0, ids(toSlots(OrderForCycle(items, other, 0)))) {
		t.Fatal("different seeds gave the same slate")
	}
	// Input must not be mutated.
	if items[0].FileID != "f0" || items[9].FileID != "f9" {
		t.Fatal("OrderForCycle mutated its input")
	}
}

func TestGroupShuffleStaysWithinGroup(t *testing.T) {
	items := []Item{{FileID: "intro", Duration: 5}}
	for i := range 8 {
		items = append(items, Item{FileID: fmt.Sprintf("g%d", i), Duration: 10, Group: "L1"})
	}
	items = append(items, Item{FileID: "mid", Duration: 5})
	for i := range 3 {
		items = append(items, Item{FileID: fmt.Sprintf("h%d", i), Duration: 10, Group: "L2"})
	}
	ch := Channel{Seed: "x"}
	seen := map[string]bool{}
	for c := int64(0); c < 10; c++ {
		o := ids(toSlots(OrderForCycle(items, ch, c)))
		if o[0] != "intro" || o[9] != "mid" {
			t.Fatalf("ungrouped items moved: %v", o)
		}
		g := slices.Clone(o[1:9])
		slices.Sort(g)
		if fmt.Sprint(g) != "[g0 g1 g2 g3 g4 g5 g6 g7]" {
			t.Fatalf("group L1 leaked: %v", o)
		}
		h := slices.Clone(o[10:])
		slices.Sort(h)
		if fmt.Sprint(h) != "[h0 h1 h2]" {
			t.Fatalf("group L2 leaked: %v", o)
		}
		seen[fmt.Sprint(o[1:9])] = true
		if !slices.Equal(o, ids(toSlots(OrderForCycle(items, ch, c)))) {
			t.Fatal("group order not deterministic")
		}
	}
	if len(seen) < 8 {
		t.Fatalf("group order should change per cycle; %d distinct in 10", len(seen))
	}
}

func checkContiguous(t *testing.T, g []Slot) {
	t.Helper()
	for i := 1; i < len(g); i++ {
		if !g[i].Start.Equal(g[i-1].End) {
			t.Fatalf("gap/overlap at %d: %v vs %v", i, g[i-1].End, g[i].Start)
		}
		if g[i].Seq != g[i-1].Seq+1 {
			t.Fatalf("seq jump at %d", i)
		}
	}
}

func TestGuideSpansCyclesAndMatchesNow(t *testing.T) {
	items := tenItems() // T = 55 min
	for _, ch := range []Channel{
		{Anchor: t0.Add(-1234567 * time.Second)},
		{Anchor: t0.Add(-1234567 * time.Second), Shuffle: true, ShuffleMode: ModePerCycle, Seed: "abc"},
		{Anchor: t0.Add(-1234567 * time.Second), Shuffle: true, ShuffleMode: ModeFixed, Seed: "abc"},
	} {
		from, to := t0, t0.Add(6*time.Hour)
		g := Guide(items, ch, from, to)
		if len(g) == 0 {
			t.Fatal("empty guide")
		}
		checkContiguous(t, g)
		if g[0].Start.After(from) || !g[0].End.After(from) {
			t.Fatalf("first slot %v–%v does not cover from", g[0].Start, g[0].End)
		}
		last := g[len(g)-1]
		if !last.Start.Before(to) || last.End.Before(to) {
			t.Fatalf("last slot %v–%v does not reach to", last.Start, last.End)
		}
		if g[len(g)-1].Cycle-g[0].Cycle < 5 {
			t.Fatalf("6h window over a 55min lineup should span ≥6 cycles")
		}
		for _, s := range g {
			cur, _ := Now(items, ch, s.Start.Add(time.Millisecond))
			if cur.FileID != s.FileID || !cur.Start.Equal(s.Start) || cur.Cycle != s.Cycle || cur.Index != s.Index {
				t.Fatalf("guide/now mismatch at %v: %s vs %s", s.Start, s.FileID, cur.FileID)
			}
		}
		// Every cycle in the guide is a complete permutation of the lineup.
		byCycle := map[int64][]string{}
		for _, s := range g {
			byCycle[s.Cycle] = append(byCycle[s.Cycle], s.FileID)
		}
		for c, l := range byCycle {
			if c == g[0].Cycle || c == last.Cycle {
				continue
			}
			if len(l) != 10 {
				t.Fatalf("cycle %d has %d items", c, len(l))
			}
		}
	}
	if Guide(items, Channel{Anchor: t0}, t0.Add(time.Hour), t0.Add(time.Hour)) != nil {
		t.Fatal("empty window should give nil")
	}
	if Guide(items, Channel{Anchor: t0}, t0.Add(2*time.Hour), t0.Add(time.Hour)) != nil {
		t.Fatal("inverted window should give nil")
	}
}

func TestDeterminismAcrossCalls(t *testing.T) {
	items := tenItems()
	ch := Channel{Anchor: t0.AddDate(-3, 0, 0), Shuffle: true, Seed: "viewer-independent"}
	at := t0.Add(17 * time.Minute)
	a, an := Now(slices.Clone(items), ch, at)
	b, bn := Now(slices.Clone(items), ch, at)
	if fmt.Sprint(*a, an) != fmt.Sprint(*b, bn) {
		t.Fatal("Now not deterministic")
	}
	g1 := Guide(items, ch, at, at.Add(10*time.Hour))
	g2 := Guide(items, ch, at, at.Add(10*time.Hour))
	if fmt.Sprint(g1) != fmt.Sprint(g2) {
		t.Fatal("Guide not deterministic")
	}
}

func TestPosition(t *testing.T) {
	ch := Channel{}
	now := t0
	cur, next := Position(abc(), ch, now, 1, 7.5)
	if cur.FileID != "B" || cur.Offset != 7.5 || cur.Seq != 1 || !cur.Start.Equal(now.Add(-7500*time.Millisecond)) {
		t.Fatalf("got %+v", *cur)
	}
	if !slices.Equal(ids(next), []string{"C", "A", "B"}) || next[1].Cycle != 1 || next[1].Seq != 3 {
		t.Fatalf("next %v", ids(next))
	}
	// Offset past the end → next programme at 0.
	cur, _ = Position(abc(), ch, now, 2, 30)
	if cur.FileID != "A" || cur.Cycle != 1 || cur.Seq != 3 || cur.Offset != 0 {
		t.Fatalf("carry: %+v", *cur)
	}
	// Large seq selects the right cycle; negative inputs clamp.
	cur, _ = Position(abc(), ch, now, 3*1000+2, -4)
	if cur.FileID != "C" || cur.Cycle != 1000 || cur.Offset != 0 {
		t.Fatalf("seq: %+v", *cur)
	}
	cur, _ = Position(abc(), ch, now, -9, math.NaN())
	if cur.Seq != 0 || cur.Offset != 0 {
		t.Fatalf("clamp: %+v", *cur)
	}
	// With per_cycle shuffle the on-demand order follows OrderForCycle.
	sh := Channel{Shuffle: true, Seed: "p"}
	items := tenItems()
	for seq := int64(0); seq < 40; seq++ {
		c, _ := Position(items, sh, now, seq, 0)
		want := OrderForCycle(items, sh, seq/10)[seq%10]
		if c.FileID != want.FileID {
			t.Fatalf("seq %d: %s want %s", seq, c.FileID, want.FileID)
		}
	}
}

func BenchmarkGuide72hLargeLineup(b *testing.B) {
	items := make([]Item, 5000)
	for i := range items {
		items[i] = Item{FileID: fmt.Sprint(i), Duration: 1320 + float64(i%7)*60}
	}
	ch := Channel{Anchor: t0.AddDate(-5, 0, 0), Shuffle: true, Seed: "bench"}
	for b.Loop() {
		Guide(items, ch, t0, t0.Add(72*time.Hour))
	}
}
