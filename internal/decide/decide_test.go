package decide

import (
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func k(s string) Key { return KeyFor(netip.MustParseAddrPort(s)) }

func noExplore(e *Engine) { e.rnd = func() float64 { return 1 } }

func TestDefaultAndLearning(t *testing.T) {
	e := New(Config{Default: Relay})
	noExplore(e)
	key := k("5.6.7.8:443")
	if c := e.Choose(key); c.Path != Relay {
		t.Fatalf("default: %+v", c)
	}
	// Small connection: ignored.
	if e.Observe(Sample{key, Direct, 1000, time.Second}) {
		t.Fatal("small sample counted")
	}
	for range 3 {
		e.Observe(Sample{key, Direct, 50 << 20, time.Second}) // 50 MiB/s
		e.Observe(Sample{key, Relay, 10 << 20, time.Second})  // 10 MiB/s
	}
	if c := e.Choose(key); c.Path != Direct {
		t.Fatalf("should learn direct is faster: %+v", c)
	}
	// Other key with no samples follows global evidence (direct wins overall).
	if c := e.Choose(k("9.9.9.9:443")); c.Path != Direct {
		t.Fatalf("global fallback: %+v", c)
	}
	// Same /24 different host shares the key.
	if KeyFor(netip.MustParseAddrPort("5.6.7.200:443")) != key {
		t.Fatal("keys should group by /24")
	}
}

func TestMarginKeepsDefault(t *testing.T) {
	e := New(Config{Default: Relay})
	noExplore(e)
	key := k("5.6.7.8:443")
	for range 3 {
		e.Observe(Sample{key, Direct, 105 << 20, time.Second})
		e.Observe(Sample{key, Relay, 100 << 20, time.Second})
	}
	if c := e.Choose(key); c.Path != Relay {
		t.Fatalf("5%% difference should not flip: %+v", c)
	}
}

func TestExploration(t *testing.T) {
	e := New(Config{Default: Relay})
	e.rnd = func() float64 { return 0 }
	if c := e.Choose(k("5.6.7.8:443")); c.Path != Direct || !c.Explore {
		t.Fatalf("expected exploration of direct: %+v", c)
	}
	if c := e.Predict(k("5.6.7.8:443")); c.Explore {
		t.Fatalf("Predict must not explore: %+v", c)
	}
}

func TestPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.json")
	e := New(Config{})
	key := k("5.6.7.8:443")
	e.Observe(Sample{key, Relay, 10 << 20, time.Second})
	if err := e.Save(path); err != nil {
		t.Fatal(err)
	}
	f := New(Config{})
	if err := f.Load(path); err != nil {
		t.Fatal(err)
	}
	if _, r := f.Lookup(key); r.N != 1 {
		t.Fatalf("not restored: %+v", r)
	}
	if err := New(Config{}).Load(filepath.Join(t.TempDir(), "missing")); err != nil {
		t.Fatal(err)
	}
}

func TestNewerFormatNotOverwritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.json")
	newer := []byte(`{"version": 2, "whatever": true}`)
	if err := os.WriteFile(path, newer, 0o644); err != nil {
		t.Fatal(err)
	}
	e := New(Config{})
	if err := e.Load(path); err == nil {
		t.Fatal("expected a version error")
	}
	e.Observe(Sample{k("5.6.7.8:443"), Relay, 10 << 20, time.Second})
	if err := e.Save(path); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != string(newer) {
		t.Fatalf("newer state was overwritten: %s", b)
	}
}
