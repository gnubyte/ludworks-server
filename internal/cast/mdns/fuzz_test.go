package mdns

import (
	"testing"
	"time"
	"unicode/utf8"
)

// FuzzParse feeds arbitrary packets to the parser and through the browser's state handling:
// nothing may panic and every cap must hold.
func FuzzParse(f *testing.F) {
	f.Add(chromecastResponse(true))
	f.Add(chromecastResponse(false))
	f.Add(appleResponse(true))
	f.Add(buildQuery([]question{{name: svcCast, qtype: typePTR, qclass: classIN, qu: true}}))
	f.Add(newResponse(true).srv(secAnswer, ccInstance, ccHost, 8009, 0, true).addr(secAnswer, ccHost, "fe80::1", 0, true).bytes())
	f.Add([]byte{})
	f.Add(make([]byte, headerLen))
	f.Add(nameBuf(0xc0, 12))

	f.Fuzz(func(t *testing.T, data []byte) {
		m, _ := parseMessage(data)
		if m != nil {
			if len(m.records) > maxRecords || len(m.questions) > maxQuestions {
				t.Fatalf("%d records, %d questions", len(m.records), len(m.questions))
			}
			for _, q := range m.questions {
				checkName(t, q.name)
			}
			for _, r := range m.records {
				checkName(t, r.name)
				if r.target != nil {
					checkName(t, r.target)
				}
				if r.rtype == typeTXT {
					if len(r.txt) > maxTXTParts {
						t.Fatalf("%d TXT parts", len(r.txt))
					}
					checkTXT(t, txtMap(r.txt))
				}
			}
		}

		b := New(Options{Services: allSvcs, Interval: time.Minute})
		now := t0
		b.now = func() time.Time { return now }
		b.sendFn = func(p []byte) {
			if _, err := parseMessage(p); err != nil {
				t.Fatalf("browser sent an unparsable query: %v", err)
			}
		}
		b.handle(data, src5353, false, now)
		b.handle(data, src5353, true, now)
		for _, in := range b.Instances() {
			if len(in.Addrs) == 0 || len(in.Addrs) > maxAddrsPerHost || in.Port <= 0 || in.Port > 65535 || !utf8.ValidString(in.Name) {
				t.Fatalf("instance %+v", in)
			}
			checkTXT(t, in.TXT)
		}
		if len(b.instances) > maxInstancesPerService*len(b.services) || len(b.hosts) > maxHosts {
			t.Fatalf("%d instances, %d hosts", len(b.instances), len(b.hosts))
		}
		now = now.Add(time.Hour)
		b.sweep(now)
		if len(b.instances) != 0 || len(b.hosts) != 0 || b.orphans.Len() != 0 {
			t.Fatalf("sweep kept %d instances, %d hosts, %d orphans", len(b.instances), len(b.hosts), b.orphans.Len())
		}
	})
}

func checkName(t *testing.T, labels []string) {
	t.Helper()
	wire := 1
	for _, l := range labels {
		if l == "" || len(l) > maxLabel {
			t.Fatalf("label %q", l)
		}
		wire += 1 + len(l)
	}
	if wire > maxNameWire {
		t.Fatalf("name of %d bytes", wire)
	}
}

func checkTXT(t *testing.T, m map[string]string) {
	t.Helper()
	if len(m) > maxTXTPairs {
		t.Fatalf("%d TXT pairs", len(m))
	}
	total := 0
	for k, v := range m {
		total += len(k) + len(v)
		if !validTXTKey(k) || k != lowerASCII(k) || len(v) > maxTXTValue || !utf8.ValidString(v) {
			t.Fatalf("TXT %q=%q", k, v)
		}
	}
	if total > maxTXTBytes {
		t.Fatalf("TXT of %d bytes", total)
	}
}
