// Package mdns is a minimal mDNS / DNS-SD browser (RFC 6762, RFC 6763) that discovers cast
// devices on the local network. It only ever sends queries and never answers any: it is not a
// responder and announces nothing about this host.
//
// Everything received is untrusted LAN input. Packets are size-capped and rate-limited and are
// accepted only from on-link senders (and, on the shared port 5353, only from source port
// 5353); names are decoded with bounded compression-pointer chasing; TXT data is capped and
// sanitised; and the instance, host and address tables have fixed bounds, so a hostile host on
// the LAN can neither exhaust memory nor keep the browser busy.
package mdns

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Defaults for Options.
const (
	DefaultInterval      = 60 * time.Second
	DefaultRefreshWindow = 2500 * time.Millisecond
)

// Table and rate bounds against hostile or misbehaving hosts on the LAN.
const (
	mdnsPort               = 5353
	maxInstancesPerService = 256
	maxHosts               = 1024 // host address entries (SRV targets and not-yet-referenced hosts)
	maxAddrsPerHost        = 16
	maxPacketsPerSecond    = 1000
	// Address queries for SRV targets whose A/AAAA records did not come along.
	maxAddrQueryHosts = 16              // hosts per query message
	addrQueryEvery    = 5 * time.Second // per host
	maxAddrQueryState = 1024
	// minBrowseGap keeps back-to-back Refresh calls from flooding the network.
	minBrowseGap = 500 * time.Millisecond
	// changeCoalesce bounds OnChange calls to one per this period.
	changeCoalesce = 250 * time.Millisecond
	// flushGrace: a cache-flush record replaces only records older than this (RFC 6762 §10.2).
	flushGrace = time.Second
	sweepEvery = 5 * time.Second
)

// Instance is one discovered DNS-SD service instance that has an SRV record and at least one
// address.
type Instance struct {
	Service string            // service type without domain, e.g. "_googlecast._tcp" (as passed in Options.Services)
	Name    string            // instance label, unescaped (e.g. "Living Room TV"; RAOP: "AABBCCDDEEFF@Living Room")
	Host    string            // SRV target, lower-cased, without trailing dot (e.g. "chromecast-abc.local")
	Port    int               //
	Addrs   []netip.Addr      // IPv4 first, then IPv6 (no zones); never empty
	TXT     map[string]string // keys lower-cased; "key" without "=" → ""; first occurrence of a key wins
	Seen    time.Time         // last time a record of this instance arrived
}

// Options configures a Browser.
type Options struct {
	Services      []string      // service types to browse, e.g. "_googlecast._tcp" (".local." is appended)
	Interval      time.Duration // periodic browse interval while running; default 60 s
	RefreshWindow time.Duration // how long Refresh collects answers; default 2.5 s
	Log           *slog.Logger  // nil → discard
	// OnChange is called (from a browser goroutine; it must not block) after the visible
	// instance set changed: an instance appeared or disappeared, or a visible instance's name,
	// host, port, addresses or TXT changed. Bursts are coalesced to one call per 250 ms.
	OnChange func()
}

// service is a configured service type.
type service struct {
	name   string   // as configured
	labels []string // its labels plus "local"
}

// instance is the browser's state for one service instance.
type instance struct {
	key  string // canonical full name
	svc  int    // index into Browser.services
	name string // sanitised instance label
	srv  *srvRecord
	txt  map[string]string
	seen time.Time // last PTR/SRV/TXT record of the instance
	ttl  uint32    // TTL of that record (the entry is dropped when it lapses)
	fp   string    // fingerprint of the last visible state ("" = not visible)
}

// srvRecord is an instance's SRV record. The instance is visible while it is live.
type srvRecord struct {
	hostKey string
	host    string   // presentation form
	labels  []string // for address queries
	port    uint16
	seen    time.Time
	ttl     uint32
}

// hostEntry holds the addresses of one host name.
type hostEntry struct {
	key    string
	labels []string
	addrs  map[netip.Addr]*addrRecord
	refs   map[string]struct{} // keys of the instances whose SRV targets this host
	orphan *list.Element       // position in Browser.orphans while refs is empty
}

type addrRecord struct {
	seen time.Time
	ttl  uint32
}

// packetConn is one UDP socket of the browser.
type packetConn interface {
	ReadFrom(p []byte) (int, netip.AddrPort, error)
	WriteTo(p []byte) error // to the mDNS group of the socket's family (and interface)
	Close() error
	// Legacy reports an ephemeral-port socket: its queries are "legacy unicast" queries
	// (RFC 6762 §6.7) and responders answer it by unicast from any source port.
	Legacy() bool
}

// Browser browses DNS-SD services with multicast DNS. It is safe for concurrent use.
type Browser struct {
	opts     Options
	log      *slog.Logger
	services []service

	// Test seams.
	now      func() time.Time
	openFn   func(log *slog.Logger) ([]packetConn, func(netip.Addr) bool, error)
	sendFn   func(pkt []byte) // nil: send on every socket
	coalesce time.Duration

	mu         sync.Mutex
	instances  map[string]*instance
	perService []int
	hosts      map[string]*hostEntry
	orphans    *list.List // hosts no instance references, least recently refreshed at the back
	addrAsked  map[string]time.Time
	rateStart  time.Time
	rateCount  int
	dropped    int
	lastBrowse time.Time
	conns      []packetConn
	onLink     func(netip.Addr) bool
	started    bool
	closed     bool
	cancel     context.CancelFunc

	done     chan struct{} // closed once the browser stopped running
	doneOnce sync.Once
	changed  chan struct{} // an OnChange call is pending (buffered 1)
	wg       sync.WaitGroup
}

// New returns a browser for o.Services. Nothing is sent or opened until Start.
func New(o Options) *Browser {
	if o.Interval <= 0 {
		o.Interval = DefaultInterval
	}
	if o.RefreshWindow <= 0 {
		o.RefreshWindow = DefaultRefreshWindow
	}
	log := o.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	b := &Browser{
		opts: o, log: log, now: time.Now, openFn: openSockets, coalesce: changeCoalesce,
		instances: map[string]*instance{}, hosts: map[string]*hostEntry{}, orphans: list.New(),
		addrAsked: map[string]time.Time{}, done: make(chan struct{}), changed: make(chan struct{}, 1),
	}
	for _, s := range o.Services {
		labels := splitName(strings.TrimSuffix(strings.TrimSuffix(s, "."), ".local"))
		if labels == nil {
			log.Warn("mdns: ignoring invalid service type", "service", s)
			continue
		}
		labels = append(labels, "local")
		if slices.ContainsFunc(b.services, func(x service) bool { return labelsEqual(x.labels, labels) }) {
			continue
		}
		b.services = append(b.services, service{name: s, labels: labels})
	}
	b.perService = make([]int, len(b.services))
	return b
}

// Start opens the sockets and starts the background goroutines (receive loops, periodic
// browsing, expiry). It returns an error only when no socket at all could be opened. The
// browser runs until ctx is done or Close is called. A browser is started at most once.
func (b *Browser) Start(ctx context.Context) error {
	b.mu.Lock()
	if b.started || b.closed {
		b.mu.Unlock()
		return errors.New("mdns: browser already started or closed")
	}
	b.started = true
	b.mu.Unlock()

	conns, onLink, err := b.openFn(b.log)
	if err == nil && len(conns) == 0 {
		err = errors.New("mdns: no usable network interface")
	}
	if err != nil {
		b.stopNow()
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	b.mu.Lock()
	if b.closed { // Close raced with Start
		b.mu.Unlock()
		cancel()
		for _, c := range conns {
			c.Close()
		}
		return errors.New("mdns: browser closed")
	}
	b.conns, b.onLink, b.cancel = conns, onLink, cancel
	b.wg.Add(len(conns) + 2)
	for _, c := range conns {
		go b.readLoop(c)
	}
	go b.loop(runCtx)
	go b.notifier(runCtx)
	b.mu.Unlock()
	return nil
}

// Close closes the sockets and stops every goroutine, waiting for them. It is idempotent.
func (b *Browser) Close() error {
	b.mu.Lock()
	b.closed = true
	cancel := b.cancel
	b.mu.Unlock()
	if cancel != nil {
		cancel() // the loop closes the sockets, which ends the read loops
	} else {
		b.stopNow()
	}
	b.wg.Wait()
	return nil
}

// stopNow marks the browser stopped without goroutines to wait for.
func (b *Browser) stopNow() {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
	b.doneOnce.Do(func() { close(b.done) })
}

// running reports whether the browser has open sockets.
func (b *Browser) running() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.started && !b.closed && b.conns != nil
}

// Refresh sends the PTR queries now (and once more about a second later) and returns when ctx
// is done or RefreshWindow has passed. It is a no-op when the browser is not running.
func (b *Browser) Refresh(ctx context.Context) {
	if !b.running() {
		return
	}
	window := time.NewTimer(b.opts.RefreshWindow)
	defer window.Stop()
	again := time.NewTimer(time.Second)
	defer again.Stop()
	b.browse(true)
	for {
		select {
		case <-ctx.Done():
			return
		case <-b.done:
			return
		case <-window.C:
			return
		case <-again.C:
			b.browse(false)
		}
	}
}

// Instances returns the current complete, unexpired instances, sorted by service and name.
func (b *Browser) Instances() []Instance {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	out := make([]Instance, 0, len(b.instances))
	for _, in := range b.instances {
		if s, ok := b.snapshot(in, now); ok {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Service != out[j].Service {
			return out[i].Service < out[j].Service
		}
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Host < out[j].Host
	})
	return out
}

/* ------------------------------------------------------------------ goroutines */

// loop browses periodically and sweeps expired records until ctx is done, then closes the
// sockets.
func (b *Browser) loop(ctx context.Context) {
	defer b.wg.Done()
	defer b.shutdown()
	b.browse(true)
	next := time.NewTimer(time.Second) // RFC 6762 §5.2: repeat the first query after a second
	defer next.Stop()
	sweep := time.NewTicker(sweepEvery)
	defer sweep.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-next.C:
			b.browse(false)
			next.Reset(b.opts.Interval)
		case <-sweep.C:
			b.sweep(b.now())
		}
	}
}

// shutdown closes the sockets (ending the read loops) and marks the browser stopped.
func (b *Browser) shutdown() {
	b.mu.Lock()
	conns := b.conns
	b.conns = nil
	b.closed = true
	b.mu.Unlock()
	for _, c := range conns {
		c.Close()
	}
	b.doneOnce.Do(func() { close(b.done) })
}

// readLoop receives packets on one socket until it is closed.
func (b *Browser) readLoop(c packetConn) {
	defer b.wg.Done()
	// Room for any UDP datagram, so an oversized one is read whole and dropped by handle
	// rather than truncated (which Windows reports as a read error).
	buf := make([]byte, 64<<10)
	failures := 0
	for {
		n, src, err := c.ReadFrom(buf)
		if err == nil {
			failures = 0
			b.handle(buf[:n], src, c.Legacy(), b.now())
			continue
		}
		select {
		case <-b.done:
			return
		default:
		}
		if errors.Is(err, net.ErrClosed) {
			return
		}
		// Transient errors (e.g. an ICMP error reported on Windows) are skipped; a run of them
		// pauses the loop briefly so a persistent failure cannot spin.
		if failures++; failures >= 10 {
			failures = 0
			select {
			case <-b.done:
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
}

// notifier calls OnChange for pending changes, at most once per coalesce period.
func (b *Browser) notifier(ctx context.Context) {
	defer b.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case <-b.changed:
		}
		b.callOnChange()
		t := time.NewTimer(b.coalesce)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

func (b *Browser) callOnChange() {
	if b.opts.OnChange == nil {
		return
	}
	defer func() {
		if p := recover(); p != nil {
			b.log.Error("mdns: OnChange panicked", "panic", p)
		}
	}()
	b.opts.OnChange()
}

// signal records a pending OnChange call.
func (b *Browser) signal() {
	select {
	case b.changed <- struct{}{}:
	default:
	}
}

/* ---------------------------------------------------------------------- sending */

// browse sends one PTR query for every configured service (qu: request unicast responses).
func (b *Browser) browse(qu bool) {
	if len(b.services) == 0 {
		return
	}
	b.mu.Lock()
	now := b.now()
	if !b.lastBrowse.IsZero() && now.After(b.lastBrowse) && now.Sub(b.lastBrowse) < minBrowseGap {
		b.mu.Unlock()
		return
	}
	b.lastBrowse = now
	b.mu.Unlock()
	qs := make([]question, 0, len(b.services))
	for _, s := range b.services {
		qs = append(qs, question{name: s.labels, qtype: typePTR, qclass: classIN, qu: qu})
	}
	b.send(buildQuery(qs))
}

// queryAddrs asks for the A and AAAA records of hosts.
func (b *Browser) queryAddrs(hosts [][]string) {
	qs := make([]question, 0, 2*len(hosts))
	for _, h := range hosts {
		qs = append(qs, question{name: h, qtype: typeA, qclass: classIN}, question{name: h, qtype: typeAAAA, qclass: classIN})
	}
	b.send(buildQuery(qs))
}

func (b *Browser) send(pkt []byte) {
	if b.sendFn != nil {
		b.sendFn(pkt)
		return
	}
	b.mu.Lock()
	conns := slices.Clone(b.conns)
	b.mu.Unlock()
	for _, c := range conns {
		if err := c.WriteTo(pkt); err != nil && !errors.Is(err, net.ErrClosed) {
			b.log.Debug("mdns: sending a query failed", "err", err)
		}
	}
}

/* --------------------------------------------------------------------- receiving */

// handle processes one received packet from src. legacy marks packets that arrived on an
// ephemeral-port socket (answers to legacy unicast queries).
func (b *Browser) handle(pkt []byte, src netip.AddrPort, legacy bool, now time.Time) {
	if len(pkt) > maxPacket {
		return
	}
	// RFC 6762 §6: responses come from port 5353; legacy unicast answers reach only our own
	// ephemeral socket.
	if !legacy && src.Port() != mdnsPort {
		return
	}
	b.mu.Lock()
	onLink := b.onLink
	b.mu.Unlock()
	// RFC 6762 §11: only answers that originate from the local link count.
	if onLink != nil && !onLink(src.Addr().Unmap().WithZone("")) {
		return
	}
	b.mu.Lock()
	ok := b.allowPacket(now)
	b.mu.Unlock()
	if !ok {
		return
	}
	m, _ := parseMessage(pkt) // a broken tail still leaves the records decoded before it
	if m == nil || m.flags&flagQR == 0 || m.flags&opcodeMask != 0 || m.flags&rcodeMask != 0 {
		return
	}
	b.apply(m.records, now)
}

// allowPacket is the packet rate limit (b.mu held).
func (b *Browser) allowPacket(now time.Time) bool {
	if now.Before(b.rateStart) || now.Sub(b.rateStart) >= time.Second {
		if b.dropped > 0 {
			b.log.Debug("mdns: dropped packets over the rate limit", "count", b.dropped)
		}
		b.rateStart, b.rateCount, b.dropped = now, 0, 0
	}
	if b.rateCount >= maxPacketsPerSecond {
		b.dropped++
		return false
	}
	b.rateCount++
	return true
}

// apply updates the tables with the records of one message.
func (b *Browser) apply(rrs []record, now time.Time) {
	b.mu.Lock()
	touched := map[string]*instance{}
	// Instance records first, so addresses in the same message find the hosts they reference.
	for i := range rrs {
		r := &rrs[i]
		switch r.rtype {
		case typePTR:
			b.applyPTR(r, now, touched)
		case typeSRV:
			b.applySRV(r, now, touched)
		case typeTXT:
			b.applyTXT(r, now, touched)
		}
	}
	for i := range rrs {
		if r := &rrs[i]; r.rtype == typeA || r.rtype == typeAAAA {
			b.applyAddr(r, now, touched)
		}
	}
	changed := b.refresh(touched, now)
	need := b.hostsNeedingAddrs(touched, now)
	b.mu.Unlock()
	if changed {
		b.signal()
	}
	if len(need) > 0 {
		b.queryAddrs(need)
	}
}

// serviceOf returns the index of the configured service named labels, or -1.
func (b *Browser) serviceOf(labels []string) int {
	for i, s := range b.services {
		if labelsEqual(labels, s.labels) {
			return i
		}
	}
	return -1
}

// instanceService returns the service index when labels name an instance of a configured
// service (one instance label followed by the service name), else -1.
func (b *Browser) instanceService(labels []string) int {
	if len(labels) < 2 {
		return -1
	}
	return b.serviceOf(labels[1:])
}

// instanceFor returns the instance named labels, creating it when the service's table has
// room (nil otherwise).
func (b *Browser) instanceFor(labels []string, svc int, key string) *instance {
	if in := b.instances[key]; in != nil {
		return in
	}
	if b.perService[svc] >= maxInstancesPerService {
		return nil
	}
	in := &instance{key: key, svc: svc, name: sanitize(labels[0], 255)}
	b.instances[key] = in
	b.perService[svc]++
	return in
}

// removeInstance deletes an instance and its host reference (b.mu held).
func (b *Browser) removeInstance(in *instance) {
	if b.instances[in.key] != in {
		return
	}
	b.setSRV(in, nil)
	delete(b.instances, in.key)
	b.perService[in.svc]--
}

func (b *Browser) applyPTR(r *record, now time.Time, touched map[string]*instance) {
	svc := b.serviceOf(r.name)
	if svc < 0 || len(r.target) != len(r.name)+1 || !labelsEqual(r.target[1:], r.name) {
		return
	}
	key := presentation(r.target)
	if r.ttl == 0 { // goodbye: the instance is withdrawn
		if in := b.instances[key]; in != nil {
			b.removeInstance(in)
			touched[key] = in
		}
		return
	}
	if in := b.instanceFor(r.target, svc, key); in != nil {
		in.seen, in.ttl = now, r.ttl
		touched[key] = in
	}
}

func (b *Browser) applySRV(r *record, now time.Time, touched map[string]*instance) {
	svc := b.instanceService(r.name)
	if svc < 0 {
		return
	}
	key := presentation(r.name)
	if r.ttl == 0 {
		if in := b.instances[key]; in != nil && in.srv != nil {
			b.setSRV(in, nil)
			touched[key] = in
		}
		return
	}
	if !isLocal(r.target) {
		return
	}
	in := b.instanceFor(r.name, svc, key)
	if in == nil {
		return
	}
	hk := presentation(r.target)
	if in.srv == nil || in.srv.hostKey != hk || in.srv.port != r.port {
		b.setSRV(in, &srvRecord{hostKey: hk, host: hk, labels: r.target, port: r.port})
	}
	if in.srv != nil {
		in.srv.seen, in.srv.ttl = now, r.ttl
	}
	in.seen, in.ttl = now, r.ttl
	touched[key] = in
}

func (b *Browser) applyTXT(r *record, now time.Time, touched map[string]*instance) {
	svc := b.instanceService(r.name)
	if svc < 0 {
		return
	}
	key := presentation(r.name)
	if r.ttl == 0 {
		if in := b.instances[key]; in != nil && in.txt != nil {
			in.txt = nil
			touched[key] = in
		}
		return
	}
	in := b.instanceFor(r.name, svc, key)
	if in == nil {
		return
	}
	in.txt = txtMap(r.txt)
	in.seen, in.ttl = now, r.ttl
	touched[key] = in
}

func (b *Browser) applyAddr(r *record, now time.Time, touched map[string]*instance) {
	if !isLocal(r.name) || !usableAddr(r.addr) {
		return
	}
	key := presentation(r.name)
	h := b.hosts[key]
	if r.ttl == 0 {
		if h != nil {
			if _, ok := h.addrs[r.addr]; ok {
				delete(h.addrs, r.addr)
				b.touchRefs(h, touched)
				b.dropHostIfUnused(h)
			}
		}
		return
	}
	if h == nil {
		if h = b.newHost(r.name, key); h == nil {
			return
		}
	}
	if r.flush { // RFC 6762 §10.2: this record set replaces what was received before
		for a, rec := range h.addrs {
			if a.Is4() == r.addr.Is4() && now.Sub(rec.seen) > flushGrace {
				delete(h.addrs, a)
			}
		}
	}
	if rec := h.addrs[r.addr]; rec != nil {
		rec.seen, rec.ttl = now, r.ttl
	} else if len(h.addrs) < maxAddrsPerHost {
		h.addrs[r.addr] = &addrRecord{seen: now, ttl: r.ttl}
	}
	if h.orphan != nil {
		b.orphans.MoveToFront(h.orphan)
	}
	b.touchRefs(h, touched)
}

// usableAddr filters addresses a device could not really have (or that would point the
// server at itself).
func usableAddr(a netip.Addr) bool {
	return a.IsValid() && !a.IsUnspecified() && !a.IsLoopback() && !a.IsMulticast() && !a.Is4In6() &&
		a != netip.AddrFrom4([4]byte{255, 255, 255, 255})
}

// newHost adds a host entry, evicting the least recently refreshed unreferenced host when the
// table is full; nil when nothing can be evicted (b.mu held).
func (b *Browser) newHost(labels []string, key string) *hostEntry {
	if len(b.hosts) >= maxHosts {
		e := b.orphans.Back()
		if e == nil {
			return nil
		}
		old := e.Value.(*hostEntry)
		b.orphans.Remove(e)
		delete(b.hosts, old.key)
	}
	h := &hostEntry{key: key, labels: labels, addrs: map[netip.Addr]*addrRecord{}, refs: map[string]struct{}{}}
	h.orphan = b.orphans.PushFront(h)
	b.hosts[key] = h
	return h
}

// setSRV replaces an instance's SRV record, keeping the host references in sync (b.mu held).
func (b *Browser) setSRV(in *instance, srv *srvRecord) {
	if in.srv != nil {
		if h := b.hosts[in.srv.hostKey]; h != nil {
			delete(h.refs, in.key)
			if len(h.refs) == 0 && h.orphan == nil {
				h.orphan = b.orphans.PushFront(h)
			}
			b.dropHostIfUnused(h)
		}
	}
	in.srv = srv
	if srv == nil {
		return
	}
	h := b.hosts[srv.hostKey]
	if h == nil {
		if h = b.newHost(srv.labels, srv.hostKey); h == nil {
			return // cannot happen: referenced hosts are far fewer than maxHosts
		}
	}
	h.refs[in.key] = struct{}{}
	if h.orphan != nil {
		b.orphans.Remove(h.orphan)
		h.orphan = nil
	}
}

// dropHostIfUnused deletes a host without addresses and references (b.mu held).
func (b *Browser) dropHostIfUnused(h *hostEntry) {
	if len(h.addrs) > 0 || len(h.refs) > 0 || b.hosts[h.key] != h {
		return
	}
	if h.orphan != nil {
		b.orphans.Remove(h.orphan)
		h.orphan = nil
	}
	delete(b.hosts, h.key)
}

func (b *Browser) touchRefs(h *hostEntry, touched map[string]*instance) {
	for k := range h.refs {
		if in := b.instances[k]; in != nil {
			touched[k] = in
		}
	}
}

// refresh recomputes the fingerprints of touched instances and reports whether the visible
// set changed (b.mu held).
func (b *Browser) refresh(touched map[string]*instance, now time.Time) bool {
	changed := false
	for k, in := range touched {
		if b.instances[k] != in { // removed
			if in.fp != "" {
				in.fp = ""
				changed = true
			}
			continue
		}
		if fp := b.fingerprint(in, now); fp != in.fp {
			in.fp = fp
			changed = true
		}
	}
	return changed
}

// hostsNeedingAddrs returns the SRV targets of touched instances that have no address yet
// and were not asked about recently (b.mu held).
func (b *Browser) hostsNeedingAddrs(touched map[string]*instance, now time.Time) [][]string {
	var out [][]string
	for _, in := range touched {
		if len(out) >= maxAddrQueryHosts {
			break
		}
		if in.srv == nil || b.instances[in.key] != in {
			continue
		}
		if h := b.hosts[in.srv.hostKey]; h != nil && len(b.liveAddrs(h, now)) > 0 {
			continue
		}
		if t, ok := b.addrAsked[in.srv.hostKey]; ok && now.Sub(t) < addrQueryEvery && !now.Before(t) {
			continue
		}
		if len(b.addrAsked) >= maxAddrQueryState {
			b.pruneAsked(now)
			if len(b.addrAsked) >= maxAddrQueryState {
				break
			}
		}
		b.addrAsked[in.srv.hostKey] = now
		out = append(out, in.srv.labels)
	}
	return out
}

func (b *Browser) pruneAsked(now time.Time) {
	for k, t := range b.addrAsked {
		if now.Sub(t) >= addrQueryEvery || now.Before(t) {
			delete(b.addrAsked, k)
		}
	}
}

/* ------------------------------------------------------------------ expiry, views */

// life is how long a record stays valid after it was last received: its TTL clamped to
// [Interval+30s, 3×Interval] (the floor wins when they cross). Legacy unicast answers carry
// TTLs of at most 10 s, so the floor keeps them alive until the next periodic browse.
func (b *Browser) life(ttl uint32) time.Duration {
	floor := b.opts.Interval + 30*time.Second
	d := min(time.Duration(ttl)*time.Second, 3*b.opts.Interval)
	return max(d, floor)
}

// liveAddrs returns h's unexpired addresses, IPv4 first, then global IPv6, then link-local
// IPv6, each in address order (b.mu held).
func (b *Browser) liveAddrs(h *hostEntry, now time.Time) []netip.Addr {
	out := make([]netip.Addr, 0, len(h.addrs))
	for a, rec := range h.addrs {
		if now.Before(rec.seen.Add(b.life(rec.ttl))) {
			out = append(out, a)
		}
	}
	rank := func(a netip.Addr) int {
		switch {
		case a.Is4():
			return 0
		case a.IsLinkLocalUnicast():
			return 2
		}
		return 1
	}
	sort.Slice(out, func(i, j int) bool {
		if ri, rj := rank(out[i]), rank(out[j]); ri != rj {
			return ri < rj
		}
		return out[i].Less(out[j])
	})
	return out
}

// snapshot renders a visible instance (b.mu held).
func (b *Browser) snapshot(in *instance, now time.Time) (Instance, bool) {
	if in.srv == nil || !now.Before(in.srv.seen.Add(b.life(in.srv.ttl))) {
		return Instance{}, false
	}
	h := b.hosts[in.srv.hostKey]
	if h == nil {
		return Instance{}, false
	}
	addrs := b.liveAddrs(h, now)
	if len(addrs) == 0 {
		return Instance{}, false
	}
	txt := make(map[string]string, len(in.txt))
	for k, v := range in.txt {
		txt[k] = v
	}
	return Instance{
		Service: b.services[in.svc].name, Name: in.name, Host: in.srv.host, Port: int(in.srv.port),
		Addrs: addrs, TXT: txt, Seen: in.seen,
	}, true
}

// fingerprint summarises what OnChange watches of a visible instance; "" when not visible.
func (b *Browser) fingerprint(in *instance, now time.Time) string {
	s, ok := b.snapshot(in, now)
	if !ok {
		return ""
	}
	var sb strings.Builder
	sb.WriteString(s.Name)
	sb.WriteByte(0)
	sb.WriteString(s.Host)
	sb.WriteByte(0)
	sb.WriteString(strconv.Itoa(s.Port))
	for _, a := range s.Addrs {
		sb.WriteByte(0)
		sb.WriteString(a.String())
	}
	keys := make([]string, 0, len(s.TXT))
	for k := range s.TXT {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&sb, "\x01%s=%s", k, s.TXT[k])
	}
	return sb.String()
}

// sweep drops expired records and reports visibility changes.
func (b *Browser) sweep(now time.Time) {
	b.mu.Lock()
	changed := false
	for _, in := range b.instances {
		switch {
		case !now.Before(in.seen.Add(b.life(in.ttl))):
			b.removeInstance(in)
			if in.fp != "" {
				in.fp = ""
				changed = true
			}
		case in.srv != nil && !now.Before(in.srv.seen.Add(b.life(in.srv.ttl))):
			b.setSRV(in, nil) // releases the host
		}
	}
	for _, h := range b.hosts {
		for a, rec := range h.addrs {
			if !now.Before(rec.seen.Add(b.life(rec.ttl))) {
				delete(h.addrs, a)
			}
		}
		b.dropHostIfUnused(h)
	}
	for _, in := range b.instances {
		if fp := b.fingerprint(in, now); fp != in.fp {
			in.fp = fp
			changed = true
		}
	}
	b.pruneAsked(now)
	b.mu.Unlock()
	if changed {
		b.signal()
	}
}
