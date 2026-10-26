package schedule

import (
	"fmt"
	"testing"
	"time"
)

// positionLoop is the former on-demand guide: one Position call (and so one engine) per slot.
func positionLoop(items []Item, ch Channel, now time.Time, seq int64, offset float64, from, to time.Time, max int) []Slot {
	cur, _ := Position(items, ch, now, seq, offset)
	if cur == nil {
		return nil
	}
	var out []Slot
	s := *cur
	for len(out) < max && s.Start.Before(to) {
		if s.End.After(from) {
			out = append(out, s)
		}
		nx, _ := Position(items, ch, s.End, s.Seq+1, 0)
		if nx == nil {
			break
		}
		s = *nx
	}
	return out
}

func sameSlots(a, b []Slot) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, y := a[i], b[i]
		if x.FileID != y.FileID || !x.Start.Equal(y.Start) || !x.End.Equal(y.End) || x.Seq != y.Seq ||
			x.Cycle != y.Cycle || x.Index != y.Index || x.Offset != y.Offset {
			return false
		}
	}
	return true
}

func TestPositionGuideMatchesPositionWalk(t *testing.T) {
	now := t0
	items := tenItems()
	channels := []Channel{{}, {Shuffle: true, Seed: "p"}, {Shuffle: true, ShuffleMode: ModeFixed, Seed: "f"}}
	grouped := append([]Item{}, items...)
	for i := range grouped[2:7] {
		grouped[2+i].Group = "g"
	}
	for ci, ch := range channels {
		for _, list := range [][]Item{items, grouped, abc()} {
			for _, pos := range []struct {
				seq int64
				off float64
			}{{0, 0}, {3, 12.5}, {9, 1e6}, {27, 4}, {1 << 40, 3}} {
				for _, win := range []struct{ from, to time.Duration }{
					{-time.Hour, 6 * time.Hour}, {0, time.Minute}, {2 * time.Hour, 5 * time.Hour},
					{30 * time.Hour, 72 * time.Hour}, {-2 * time.Hour, -time.Hour},
				} {
					for _, max := range []int{1, 3, 500} {
						from, to := now.Add(win.from), now.Add(win.to)
						got := PositionGuide(list, ch, now, pos.seq, pos.off, from, to, max)
						want := positionLoop(list, ch, now, pos.seq, pos.off, from, to, max)
						if !sameSlots(got, want) {
							t.Fatalf("channel %d seq %d off %g window %v max %d:\n got %v\nwant %v", ci, pos.seq, pos.off, win, max, ids(got), ids(want))
						}
					}
				}
			}
		}
	}
	if PositionGuide(nil, Channel{}, now, 0, 0, now, now.Add(time.Hour), 10) != nil {
		t.Fatal("empty lineup")
	}
}

// TestPositionGuideLargeLineupIsFast: an on-demand guide over a 50 000-item lineup of short
// items (20 000 slots in 72 h) used to rebuild the engine per slot — minutes of CPU.
func TestPositionGuideLargeLineupIsFast(t *testing.T) {
	items := make([]Item, 50000)
	for i := range items {
		items[i] = Item{FileID: fmt.Sprint(i % 10), Duration: 10 + float64(i%3)}
	}
	ch := Channel{Anchor: t0.AddDate(-1, 0, 0), Shuffle: true, Seed: "dos"}
	start := time.Now()
	slots := PositionGuide(items, ch, t0, 123456, 2, t0, t0.Add(72*time.Hour), 20000)
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("on-demand guide took %v", el)
	}
	if len(slots) != MaxGuideSlots {
		t.Fatalf("%d slots", len(slots))
	}
	for i := 1; i < len(slots); i++ {
		if !slots[i].Start.Equal(slots[i-1].End) || slots[i].Seq != slots[i-1].Seq+1 {
			t.Fatalf("slot %d not contiguous", i)
		}
	}
	// A window a year ahead is reached by arithmetic, not by walking a year of slots.
	start = time.Now()
	far := PositionGuide(items, ch, t0, 0, 0, t0.AddDate(1, 0, 0), t0.AddDate(1, 0, 0).Add(time.Hour), 20000)
	if el := time.Since(start); el > 2*time.Second || len(far) == 0 || far[0].End.Before(t0.AddDate(1, 0, 0)) {
		t.Fatalf("far window: %v, %d slots", el, len(far))
	}
}
