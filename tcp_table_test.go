package caddyconsul

import (
	"sort"
	"sync"
	"testing"
)

func up(addr string) Upstream { return Upstream{Address: addr, Healthy: true, Weight: 1} }

func TestTCPTable_UpdateLookupPorts(t *testing.T) {
	tbl := NewTCPTable()
	tbl.Update([]CompiledTCPRoute{
		{Port: 5432, Upstreams: []Upstream{up("db:5432")}, ServiceName: "pg"},
		{Port: 6379, Upstreams: []Upstream{up("cache:6379")}, ServiceName: "redis"},
	})

	if tbl.Len() != 2 {
		t.Fatalf("Len = %d, want 2", tbl.Len())
	}
	ports := tbl.Ports()
	sort.Ints(ports)
	if len(ports) != 2 || ports[0] != 5432 || ports[1] != 6379 {
		t.Fatalf("Ports = %v, want [5432 6379]", ports)
	}
	if _, ok := tbl.Lookup(5432); !ok {
		t.Fatal("Lookup(5432) missing")
	}
	if _, ok := tbl.Lookup(9999); ok {
		t.Fatal("Lookup(9999) should be absent")
	}
}

func TestTCPTable_PlainTCP_NoSNINeeded(t *testing.T) {
	tbl := NewTCPTable()
	tbl.Update([]CompiledTCPRoute{
		{Port: 5432, Upstreams: []Upstream{up("db:5432")}, ServiceName: "pg"},
	})
	pr, ok := tbl.Lookup(5432)
	if !ok {
		t.Fatal("missing port")
	}
	if pr.anySNI {
		t.Fatal("anySNI should be false for a plain TCP route")
	}
	// match returns the single route regardless of the sni argument.
	if r := pr.match(""); r == nil || r.ServiceName != "pg" {
		t.Fatalf("match(\"\") = %v, want pg", r)
	}
	if r := pr.match("anything.example.com"); r == nil || r.ServiceName != "pg" {
		t.Fatalf("match(sni) for plain TCP should still return the route, got %v", r)
	}
}

func TestTCPTable_SNI_Precedence(t *testing.T) {
	tbl := NewTCPTable()
	tbl.Update([]CompiledTCPRoute{
		{Port: 443, SNI: "exact.klmh.co", Passthrough: true, Upstreams: []Upstream{up("a:443")}, ServiceName: "exact"},
		{Port: 443, SNI: "*.klmh.co", Passthrough: true, Upstreams: []Upstream{up("b:443")}, ServiceName: "wild"},
		{Port: 443, SNI: "", Passthrough: true, Upstreams: []Upstream{up("c:443")}, ServiceName: "default"},
	})
	pr, _ := tbl.Lookup(443)
	if !pr.anySNI {
		t.Fatal("anySNI should be true")
	}

	cases := []struct {
		sni  string
		want string
	}{
		{"exact.klmh.co", "exact"},     // exact beats wildcard
		{"other.klmh.co", "wild"},      // wildcard match
		{"deep.sub.klmh.co", "wild"},   // wildcard is single-label? matchHost suffix => matches
		{"klmh.co", "default"},         // apex doesn't match *.klmh.co => default
		{"nomatch.example.org", "default"}, // unrelated => default
		{"", "default"},                // no SNI => default
	}
	for _, c := range cases {
		r := pr.match(c.sni)
		if r == nil || r.ServiceName != c.want {
			got := "nil"
			if r != nil {
				got = r.ServiceName
			}
			t.Errorf("match(%q) = %s, want %s", c.sni, got, c.want)
		}
	}
}

func TestTCPTable_SNI_NoDefault_NoMatch(t *testing.T) {
	tbl := NewTCPTable()
	tbl.Update([]CompiledTCPRoute{
		{Port: 443, SNI: "only.klmh.co", Passthrough: true, Upstreams: []Upstream{up("a:443")}, ServiceName: "only"},
	})
	pr, _ := tbl.Lookup(443)
	if r := pr.match("only.klmh.co"); r == nil || r.ServiceName != "only" {
		t.Fatalf("exact match failed: %v", r)
	}
	if r := pr.match("nope.example.org"); r != nil {
		t.Fatalf("expected nil for no match + no default, got %s", r.ServiceName)
	}
	if r := pr.match(""); r != nil {
		t.Fatalf("expected nil for empty SNI + no default, got %s", r.ServiceName)
	}
}

func TestTCPTable_UpdateReplacesAtomically(t *testing.T) {
	tbl := NewTCPTable()
	tbl.Update([]CompiledTCPRoute{{Port: 1111, Upstreams: []Upstream{up("a:1")}, ServiceName: "old"}})
	tbl.Update([]CompiledTCPRoute{{Port: 2222, Upstreams: []Upstream{up("b:2")}, ServiceName: "new"}})

	if _, ok := tbl.Lookup(1111); ok {
		t.Fatal("port 1111 should be gone after replacement")
	}
	if _, ok := tbl.Lookup(2222); !ok {
		t.Fatal("port 2222 should be present")
	}
}

// Run with -race to verify readers never race with Update.
func TestTCPTable_ConcurrentReadWrite(t *testing.T) {
	tbl := NewTCPTable()
	tbl.Update([]CompiledTCPRoute{{Port: 443, SNI: "x.klmh.co", Upstreams: []Upstream{up("a:1")}}})

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// writers
	for w := 0; w < 2; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					tbl.Update([]CompiledTCPRoute{
						{Port: 443, SNI: "x.klmh.co", Upstreams: []Upstream{up("a:1")}},
						{Port: 5432, Upstreams: []Upstream{up("db:5432")}},
					})
				}
			}
		}()
	}
	// readers
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = tbl.Ports()
					if pr, ok := tbl.Lookup(443); ok {
						_ = pr.match("x.klmh.co")
					}
				}
			}
		}()
	}

	// let them hammer briefly
	for i := 0; i < 1000; i++ {
		tbl.Lookup(5432)
	}
	close(stop)
	wg.Wait()
}
