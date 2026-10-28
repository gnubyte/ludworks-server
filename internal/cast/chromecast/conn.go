package chromecast

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

// link is one TLS connection to a device: a reader goroutine, serialised writes and request /
// answer matching by requestId. A link that fails is never reused; the player dials a new one.
type link struct {
	conn      net.Conn
	sender    string        // our source id
	lostAfter time.Duration // the connection is dead after this long without any inbound message
	onMessage func(*Message)

	wmu sync.Mutex // serialises frame writes

	pmu     sync.Mutex
	pending map[int64]chan reply
	dead    bool

	done    chan struct{} // closed when the link failed or was closed
	errOnce sync.Once
	err     error
}

// reply is the answer to a request: its type and raw payload.
type reply struct {
	typ     string
	payload string
}

// errLinkClosed is the cause of a link closed on purpose.
var errLinkClosed = errors.New("chromecast: connection closed")

// writeTimeout bounds one frame write: a device that stops reading kills the link.
const writeTimeout = 5 * time.Second

func newLink(conn net.Conn, sender string, lostAfter time.Duration, onMessage func(*Message)) *link {
	return &link{conn: conn, sender: sender, lostAfter: lostAfter, onMessage: onMessage,
		pending: map[int64]chan reply{}, done: make(chan struct{})}
}

// fail closes the link with cause err (the first cause wins) and wakes every waiter.
func (l *link) fail(err error) {
	l.errOnce.Do(func() {
		l.err = err
		l.pmu.Lock()
		l.dead = true
		l.pmu.Unlock()
		_ = l.conn.Close()
		close(l.done)
	})
}

// cause returns why the link closed (nil while it is open).
func (l *link) cause() error {
	select {
	case <-l.done:
		return l.err
	default:
		return nil
	}
}

// alive reports whether the link is still open.
func (l *link) alive() bool { return l.cause() == nil }

// readLoop reads frames until the connection fails or stays silent for lostAfter. Heartbeat
// PINGs are answered here; everything else goes to onMessage and then, when it carries the
// requestId of a pending request, to that request's waiter.
func (l *link) readLoop() {
	for {
		_ = l.conn.SetReadDeadline(time.Now().Add(l.lostAfter))
		m, err := ReadMessage(l.conn)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				err = fmt.Errorf("chromecast: no message from the device for %s: %w", l.lostAfter, err)
			}
			l.fail(err)
			return
		}
		if m.PayloadType != PayloadString {
			continue // no namespace we use carries binary payloads
		}
		if m.Namespace == NamespaceHeartbeat {
			var t typeOnly
			if decode(m.PayloadUTF8, &t) && t.Type == "PING" && validID(m.SourceID) {
				if err := l.sendTo(NamespaceHeartbeat, m.SourceID, typeOnly{Type: "PONG"}); err != nil {
					l.fail(err)
					return
				}
			}
			continue
		}
		l.onMessage(m)
		if m.Namespace != NamespaceReceiver && m.Namespace != NamespaceMedia {
			continue
		}
		var env envelope
		if !decode(m.PayloadUTF8, &env) || env.RequestID <= 0 {
			continue
		}
		l.pmu.Lock()
		ch := l.pending[env.RequestID]
		delete(l.pending, env.RequestID)
		l.pmu.Unlock()
		if ch != nil {
			ch <- reply{typ: env.Type, payload: m.PayloadUTF8} // buffered: never blocks
		}
	}
}

// sendTo writes one JSON message to destination dest.
func (l *link) sendTo(ns, dest string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	m := &Message{SourceID: l.sender, DestinationID: dest, Namespace: ns, PayloadType: PayloadString, PayloadUTF8: string(b)}
	l.wmu.Lock()
	defer l.wmu.Unlock()
	if !l.alive() {
		return l.cause()
	}
	_ = l.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	if err := WriteMessage(l.conn, m); err != nil {
		l.fail(err)
		return err
	}
	return nil
}

// request sends payload (which carries requestId id) and waits for the message answering it.
func (l *link) request(ctx context.Context, ns, dest string, id int64, payload any, timeout time.Duration) (reply, error) {
	ch := make(chan reply, 1)
	l.pmu.Lock()
	if l.dead {
		l.pmu.Unlock()
		return reply{}, l.cause()
	}
	l.pending[id] = ch
	l.pmu.Unlock()
	defer func() {
		l.pmu.Lock()
		delete(l.pending, id)
		l.pmu.Unlock()
	}()
	if err := l.sendTo(ns, dest, payload); err != nil {
		return reply{}, err
	}
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case r := <-ch:
		return r, nil
	case <-ctx.Done():
		return reply{}, ctx.Err()
	case <-t.C:
		return reply{}, errTimeout
	case <-l.done:
		return reply{}, l.cause()
	}
}

// errTimeout: the device did not answer a request in time.
var errTimeout = errors.New("chromecast: the device did not answer in time")
