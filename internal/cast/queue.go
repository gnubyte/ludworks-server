package cast

import (
	"errors"
	"math/rand/v2"
)

// Repeat modes (CastSession.repeat).
const (
	RepeatOff = "off"
	RepeatAll = "all"
	RepeatOne = "one"
)

// MaxQueue is the most items a session's queue holds (API.md §14).
const MaxQueue = 1000

// queue is a session's play queue: items in the order the client gave them, and the play
// order over them (the identity, or a permutation while shuffle is on). pos is the current
// position in order, so items[order[pos]] plays. It is pure data; the session serialises
// access to it.
type queue struct {
	items   []*castItem
	order   []int
	pos     int
	repeat  string
	shuffle bool
	rnd     func(n int) int // random source for shuffles (tests make it deterministic)
}

var errNoNext = errors.New("cast: no next item")

func newQueue(items []*castItem, index int, repeat string, shuffle bool, rnd func(int) int) *queue {
	if rnd == nil {
		rnd = rand.IntN
	}
	q := &queue{repeat: RepeatOff, rnd: rnd}
	if repeat != "" {
		q.repeat = repeat
	}
	q.replace(items, index)
	if shuffle {
		q.setShuffle(true)
	}
	return q
}

// current is the index (into items) of the item that plays.
func (q *queue) current() int { return q.order[q.pos] }

// item is the current item.
func (q *queue) item() *castItem { return q.items[q.current()] }

// replace swaps in a new item list with item index current (clamped); shuffle keeps its
// setting and reshuffles around the new current item.
func (q *queue) replace(items []*castItem, index int) {
	q.items = items
	index = max(0, min(index, len(items)-1))
	q.order = make([]int, len(items))
	for i := range q.order {
		q.order[i] = i
	}
	q.pos = index
	if q.shuffle {
		q.reshuffle()
	}
}

// insert adds items before position at (0..len(items); out of range appends) without
// changing what plays. In shuffle mode the new items are played next, in their order.
func (q *queue) insert(items []*castItem, at int) {
	if at < 0 || at > len(q.items) {
		at = len(q.items)
	}
	cur := q.current()
	n := len(items)
	merged := make([]*castItem, 0, len(q.items)+n)
	merged = append(merged, q.items[:at]...)
	merged = append(merged, items...)
	merged = append(merged, q.items[at:]...)
	q.items = merged
	shift := func(i int) int {
		if i >= at {
			return i + n
		}
		return i
	}
	for i, v := range q.order {
		q.order[i] = shift(v)
	}
	cur = shift(cur)
	if !q.shuffle {
		q.order = append(q.order, make([]int, n)...)
		for i := range q.order {
			q.order[i] = i
		}
		q.pos = cur
		return
	}
	order := make([]int, 0, len(q.order)+n)
	order = append(order, q.order[:q.pos+1]...)
	for i := range n {
		order = append(order, at+i)
	}
	q.order = append(order, q.order[q.pos+1:]...)
}

// setShuffle switches shuffle on (a fresh random order that starts with the current item)
// or off (the client's order, continuing from the current item).
func (q *queue) setShuffle(on bool) {
	cur := q.current()
	q.shuffle = on
	if !on {
		for i := range q.order {
			q.order[i] = i
		}
		q.pos = cur
		return
	}
	q.pos = cur
	for i := range q.order {
		q.order[i] = i
	}
	q.reshuffle()
}

// reshuffle randomises the order, keeping the current item first.
func (q *queue) reshuffle() {
	q.order[0], q.order[q.pos] = q.order[q.pos], q.order[0]
	q.pos = 0
	rest := q.order[1:]
	for i := len(rest) - 1; i > 0; i-- {
		j := q.rnd(i + 1)
		rest[i], rest[j] = rest[j], rest[i]
	}
}

// advance is the automatic step after an item ended on its own: repeat one replays it,
// repeat all wraps around (with a new shuffle), otherwise the queue is done (ok false).
func (q *queue) advance() bool {
	switch {
	case q.repeat == RepeatOne:
		return true
	case q.pos+1 < len(q.order):
		q.pos++
		return true
	case q.repeat == RepeatAll:
		q.pos = 0
		if q.shuffle && len(q.order) > 1 {
			last := q.order[len(q.order)-1]
			for range 3 { // avoid playing the last item twice in a row when possible
				for i := len(q.order) - 1; i > 0; i-- {
					j := q.rnd(i + 1)
					q.order[i], q.order[j] = q.order[j], q.order[i]
				}
				if q.order[0] != last {
					break
				}
			}
		}
		return true
	}
	return false
}

// next is the user's "next": the following item, wrapping around with repeat all (repeat
// one does not hold the user back). errNoNext at the end without repeat.
func (q *queue) next() error {
	switch {
	case q.pos+1 < len(q.order):
		q.pos++
	case q.repeat == RepeatAll:
		q.pos = 0
	default:
		return errNoNext
	}
	return nil
}

// previous is the user's "previous": the item before, wrapping with repeat all; at the
// start without repeat it stays (the caller restarts the item).
func (q *queue) previous() {
	switch {
	case q.pos > 0:
		q.pos--
	case q.repeat == RepeatAll:
		q.pos = len(q.order) - 1
	}
}

// jump makes items[index] the current item (index must be valid).
func (q *queue) jump(index int) {
	for i, v := range q.order {
		if v == index {
			q.pos = i
			return
		}
	}
}
