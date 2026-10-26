// Package schedule implements the virtual-channel schedule engine described
// normatively in API.md §7.1.
//
// A channel plays an expanded lineup (a list of Items with durations) in a
// loop that started at Channel.Anchor. Everything here is a pure function of
// its inputs: the same items, channel settings and query time produce the
// same programme for every viewer, on every server restart, without storing
// any per-channel playback state. Concretely:
//
//   - Durations are converted once to integer milliseconds (rounded), and all
//     arithmetic (elapsed time, cycle number, position in cycle, slot start
//     and end times) is done in int64 milliseconds. Float seconds would lose
//     sub-second precision after a few years of elapsed time; int64 ms is
//     exact for ±292 million years.
//   - Items whose duration is not finite, ≤ 0 after rounding to ms, or larger
//     than MaxItemSeconds are skipped (they can never be "on air").
//   - T = sum of item durations. e = now − anchor (clamped to ≥ 0),
//     cycle = e / T, pos = e mod T (integer division).
//   - The order for a cycle is: the expanded order (with Item.Group runs
//     shuffled per cycle, see OrderForCycle) when shuffle is off; a
//     Fisher–Yates shuffle seeded with Seed64(Channel.Seed) when
//     shuffle_mode is "fixed"; seeded with Seed64(Channel.Seed) XOR cycle when
//     "per_cycle" (the default for unknown modes).
//   - The current programme is the first item in the cycle's order whose
//     running total of durations exceeds pos.
//
// Lookups jump straight to the right cycle with integer division, so a
// channel anchored ten years ago costs the same as one anchored a minute ago.
package schedule

import (
	"math"
	"slices"
	"time"
)

// Shuffle modes (Channel.ShuffleMode).
const (
	ModePerCycle = "per_cycle"
	ModeFixed    = "fixed"
)

const (
	// MaxItemSeconds is the longest single item the engine accepts (~115
	// days). Longer (or corrupt) durations are skipped; the bound also keeps
	// every int64 millisecond sum far from overflow.
	MaxItemSeconds = 1e7
	// MaxGuideSlots caps the number of slots one Guide call returns, so a
	// lineup of very short items cannot make a guide request unbounded.
	MaxGuideSlots = 20000
	// NextCount is how many upcoming programmes Now and Position return.
	NextCount = 3

	// maxTotalMs bounds a lineup's total length; items that would push the
	// total beyond it are skipped (unreachable for real libraries).
	maxTotalMs = int64(1) << 60
)

// Item is one playable entry of an expanded lineup.
type Item struct {
	FileID string
	// Duration in seconds.
	Duration float64
	// SourceItemID is the lineup item the file came from (reporting only).
	SourceItemID string
	// Group, when non-empty, marks the item as part of a lineup item with
	// order "random": each maximal run of consecutive items sharing the same
	// Group is shuffled within itself, with a new order every cycle. The tv
	// module sets it to the lineup item id. Ignored when the whole channel is
	// shuffled (the channel shuffle already randomises everything).
	Group string
}

// Channel holds the channel settings the schedule depends on.
type Channel struct {
	// Anchor is when cycle 0 started (channel anchor_at). Sub-millisecond
	// precision is discarded.
	Anchor  time.Time
	Shuffle bool
	// ShuffleMode is "per_cycle" or "fixed"; any other value is treated as
	// "per_cycle".
	ShuffleMode string
	// Seed is the channel's shuffle_seed string; see Seed64.
	Seed string
}

// Slot is one scheduled airing of an Item.
type Slot struct {
	Item
	Start, End time.Time // UTC, millisecond precision
	// Offset is how far (seconds) into the item playback is at the query
	// time. Only set on the "current" slot of Now and Position; 0 elsewhere.
	Offset float64
	// Cycle is the loop number (0 = first pass after the anchor).
	Cycle int64
	// Index is the slot's position within its cycle's order.
	Index int
	// Seq is the absolute programme counter Cycle*n + Index (n = number of
	// playable items). On-demand channels persist it as their position index.
	Seq int64
}

// durMs converts a duration in seconds to whole milliseconds, returning 0 for
// anything unplayable.
func durMs(sec float64) int64 {
	if math.IsNaN(sec) || math.IsInf(sec, 0) || sec <= 0 || sec > MaxItemSeconds {
		return 0
	}
	return int64(math.Round(sec * 1000))
}

// Playable reports whether the engine would schedule an item of this
// duration.
func Playable(durationSeconds float64) bool { return durMs(durationSeconds) > 0 }

// Filter splits items into those the engine schedules and those it skips
// (zero, negative, non-finite or absurd durations), preserving order.
func Filter(items []Item) (valid, skipped []Item) {
	var total int64
	for _, it := range items {
		d := durMs(it.Duration)
		if d <= 0 || total+d > maxTotalMs {
			skipped = append(skipped, it)
			continue
		}
		total += d
		valid = append(valid, it)
	}
	return valid, skipped
}

// TotalSeconds is T: the length of one cycle in seconds, computed exactly as
// the engine does (rounded milliseconds, skipped items excluded).
func TotalSeconds(items []Item) float64 {
	valid, _ := Filter(items)
	return float64(sumMs(valid)) / 1000
}

func sumMs(valid []Item) int64 {
	var t int64
	for _, it := range valid {
		t += durMs(it.Duration)
	}
	return t
}

// OrderForCycle returns the playable items in the order they air during the
// given cycle (a fresh slice; items is not modified). Cycle numbers < 0 are
// treated as 0.
//
//   - Shuffle off: expanded order, except that each run of consecutive items
//     with the same non-empty Group is Fisher–Yates shuffled with seed
//     Seed64(Seed) XOR cycle XOR Seed64("group:"+Group) — the "cycle seed"
//     of API.md §7.1 step 1, so random lineup items get a new slate per loop.
//   - Shuffle "fixed": Shuffle(items, Seed64(Seed)) — same order every cycle.
//   - Shuffle "per_cycle": Shuffle(items, Seed64(Seed) XOR cycle).
func OrderForCycle(items []Item, ch Channel, cycle int64) []Item {
	valid, _ := Filter(items)
	return orderValid(valid, ch, cycle)
}

func orderValid(valid []Item, ch Channel, cycle int64) []Item {
	if cycle < 0 {
		cycle = 0
	}
	out := slices.Clone(valid)
	base := Seed64(ch.Seed)
	if ch.Shuffle {
		seed := base
		if ch.ShuffleMode != ModeFixed {
			seed ^= uint64(cycle)
		}
		Shuffle(out, seed)
		return out
	}
	for i := 0; i < len(out); {
		g := out[i].Group
		j := i + 1
		for j < len(out) && out[j].Group == g {
			j++
		}
		if g != "" && j-i > 1 {
			Shuffle(out[i:j], base^uint64(cycle)^Seed64("group:"+g))
		}
		i = j
	}
	return out
}

// engine is a prepared lineup for one query.
type engine struct {
	valid    []Item
	ch       Channel
	total    int64 // T in ms
	anchorMs int64
	// cached order of the most recently requested cycle
	cycle int64
	order []Item
}

func newEngine(items []Item, ch Channel) *engine {
	valid, _ := Filter(items)
	return &engine{valid: valid, ch: ch, total: sumMs(valid), anchorMs: ch.Anchor.UnixMilli(), cycle: -1}
}

func (e *engine) orderFor(cycle int64) []Item {
	if cycle != e.cycle || e.order == nil {
		e.order = orderValid(e.valid, e.ch, cycle)
		e.cycle = cycle
	}
	return e.order
}

// locate returns the index of the item airing at pos (0 ≤ pos < T) within
// order, and the item's start offset within the cycle.
func locate(order []Item, pos int64) (int, int64) {
	var acc int64
	for i, it := range order {
		d := durMs(it.Duration)
		if acc+d > pos {
			return i, acc
		}
		acc += d
	}
	// Unreachable when pos < T; fall back to the last item defensively.
	last := len(order) - 1
	return last, acc - durMs(order[last].Duration)
}

func msTime(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

func (e *engine) slot(cycle int64, idx int, startMs int64) Slot {
	it := e.orderFor(cycle)[idx]
	return Slot{
		Item:  it,
		Start: msTime(startMs),
		End:   msTime(startMs + durMs(it.Duration)),
		Cycle: cycle,
		Index: idx,
		Seq:   cycle*int64(len(e.valid)) + int64(idx),
	}
}

// walk emits up to n consecutive slots starting at (cycle, idx) whose start
// time is startMs, crossing cycle boundaries as needed. It stops before the
// first slot starting at or after stopMs (pass math.MaxInt64 for no bound).
func (e *engine) walk(cycle int64, idx int, startMs int64, n int, stopMs int64) []Slot {
	out := make([]Slot, 0, min(n, 64))
	cnt := len(e.valid)
	for len(out) < n {
		if idx >= cnt {
			idx = 0
			cycle++
		}
		if startMs >= stopMs {
			break
		}
		s := e.slot(cycle, idx, startMs)
		out = append(out, s)
		startMs += durMs(s.Duration)
		idx++
	}
	return out
}

// position maps an absolute time (ms) to (cycle, index, slot start ms, offset ms).
func (e *engine) position(atMs int64) (cycle int64, idx int, startMs, offMs int64) {
	el := atMs - e.anchorMs
	if el < 0 {
		el = 0 // anchor in the future: cycle 0, position 0 (API.md §7.1 step 3)
	}
	cycle = el / e.total
	pos := el % e.total
	idx, cycStart := locate(e.orderFor(cycle), pos)
	startMs = e.anchorMs + cycle*e.total + cycStart
	return cycle, idx, startMs, pos - cycStart
}

// Now returns the programme on air at time now and up to NextCount upcoming
// programmes (crossing into following cycles). Both are nil when nothing is
// playable (T == 0). If now is before the anchor, the current slot is the
// first programme of cycle 0, starting at the anchor, with Offset 0.
func Now(items []Item, ch Channel, now time.Time) (*Slot, []Slot) {
	e := newEngine(items, ch)
	if e.total == 0 {
		return nil, nil
	}
	cycle, idx, startMs, offMs := e.position(now.UnixMilli())
	cur := e.slot(cycle, idx, startMs)
	cur.Offset = float64(offMs) / 1000
	next := e.walk(cycle, idx+1, cur.End.UnixMilli(), NextCount, math.MaxInt64)
	return &cur, next
}

// Guide returns the slots overlapping [from, to) in airing order: the first
// slot may have started before from. Nothing airs before the anchor, so a
// window entirely before it is empty. At most MaxGuideSlots slots are
// returned. Cost is O(len(items) × cycles touched + slots), independent of
// how long ago the anchor was.
func Guide(items []Item, ch Channel, from, to time.Time) []Slot {
	e := newEngine(items, ch)
	if e.total == 0 {
		return nil
	}
	fromMs, toMs := from.UnixMilli(), to.UnixMilli()
	if fromMs < e.anchorMs {
		fromMs = e.anchorMs
	}
	if toMs <= fromMs {
		return nil
	}
	cycle, idx, startMs, _ := e.position(fromMs)
	return e.walk(cycle, idx, startMs, MaxGuideSlots, toMs)
}

// Count is the number of playable items (n in Slot.Seq = Cycle*n + Index).
func Count(items []Item) int {
	valid, _ := Filter(items)
	return len(valid)
}

// Position resolves an on-demand playback position. seq is an absolute
// programme counter (Slot.Seq: cycle = seq / n, index = seq mod n, so an
// on-demand viewer who keeps watching moves through successive cycles and,
// with per_cycle shuffle, successive slates). offset is seconds into that
// programme; values < 0 or non-finite become 0, and an offset at or beyond
// the programme's end means it finished, so playback moves to the next
// programme at offset 0. Slot times are expressed relative to now (Start =
// now − offset). Returns nil, nil when nothing is playable.
func Position(items []Item, ch Channel, now time.Time, seq int64, offset float64) (*Slot, []Slot) {
	e := newEngine(items, ch)
	if len(e.valid) == 0 {
		return nil, nil
	}
	cur, cycle, idx := e.resolve(now, seq, offset)
	next := e.walk(cycle, idx+1, cur.End.UnixMilli(), NextCount, math.MaxInt64)
	return &cur, next
}

// resolve maps an on-demand position to the current slot (see Position) and its cycle and
// index. len(e.valid) must be > 0.
func (e *engine) resolve(now time.Time, seq int64, offset float64) (Slot, int64, int) {
	n := int64(len(e.valid))
	if seq < 0 {
		seq = 0
	}
	if math.IsNaN(offset) || math.IsInf(offset, 0) || offset < 0 {
		offset = 0
	}
	cycle, idx := seq/n, int(seq%n)
	d := durMs(e.orderFor(cycle)[idx].Duration)
	offMs := int64(0)
	if offset*1000 < float64(d) {
		offMs = int64(offset * 1000)
	} else {
		idx++
		if int64(idx) == n {
			idx, cycle = 0, cycle+1
		}
	}
	cur := e.slot(cycle, idx, now.UnixMilli()-offMs)
	cur.Offset = float64(offMs) / 1000
	return cur, cycle, idx
}

// PositionGuide is Guide for an on-demand viewer at position (seq, offset): the
// programmes they would watch from now on — the current one (as Position returns
// it, Start = now − offset) followed by the next ones back to back across cycle
// boundaries — that overlap [from, to), at most max (capped at MaxGuideSlots).
// Like Guide it uses one prepared engine, so the cost is O(len(items) × cycles
// touched + slots) instead of rebuilding the engine per slot; a window starting
// after the current programme is reached by cycle arithmetic, not by walking.
func PositionGuide(items []Item, ch Channel, now time.Time, seq int64, offset float64, from, to time.Time, max int) []Slot {
	max = min(max, MaxGuideSlots)
	e := newEngine(items, ch)
	if len(e.valid) == 0 || max <= 0 {
		return nil
	}
	fromMs, toMs := from.UnixMilli(), to.UnixMilli()
	cur, cycle, idx := e.resolve(now, seq, offset)
	startMs, endMs := cur.Start.UnixMilli(), cur.End.UnixMilli()
	if startMs >= toMs {
		return nil
	}
	if endMs > fromMs {
		out := append(make([]Slot, 0, min(max, 64)), cur)
		if max == 1 {
			return out
		}
		return append(out, e.walk(cycle, idx+1, endMs, max-1, toMs)...)
	}
	// The window starts after the current programme: find the programme airing at from on
	// the viewer's continuation (cycle arithmetic relative to the start of cur's cycle; all
	// values stay far below 2^62: cycStart < T ≤ 2^60 and from is within years of now).
	var cycStart int64
	for _, it := range e.orderFor(cycle)[:idx] {
		cycStart += durMs(it.Duration)
	}
	rel := cycStart + (fromMs - startMs)
	c2 := cycle + rel/e.total
	pos := rel % e.total
	i2, s2 := locate(e.orderFor(c2), pos)
	return e.walk(c2, i2, fromMs-(pos-s2), max, toMs)
}
