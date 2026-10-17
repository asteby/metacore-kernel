package wasm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/asteby/metacore-kernel/events"
	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/security"
	"github.com/google/uuid"
)

func echoSpec() *manifest.BackendSpec {
	return &manifest.BackendSpec{Runtime: "wasm", Entry: "b.wasm", Exports: []string{"echo"}, MemoryLimitMB: 4, TimeoutMs: 2000}
}

func newPoolHost(t *testing.T, opts ...Option) *Host {
	t.Helper()
	h, err := NewHost(context.Background(), security.Compile("testaddon", nil), nil, opts...)
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	t.Cleanup(func() { _ = h.Close(context.Background()) })
	return h
}

func TestPool_DefaultSizeIsOneAndSerialises(t *testing.T) {
	h := newPoolHost(t)
	if h.sizeFor("x") != 1 {
		t.Fatalf("default pool size = %d, want 1", h.sizeFor("x"))
	}
	ctx := context.Background()
	if err := h.Load(ctx, "testaddon", echoWasm(), echoSpec()); err != nil {
		t.Fatal(err)
	}
	inst := uuid.New()
	entry := mustEntry(t, h, "testaddon")
	p := h.poolFor("testaddon", inst, entry)
	m1, err := p.acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Second acquire must block while the only slot is busy...
	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, err := p.acquire(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("size-1 pool must block the second caller, got %v", err)
	}
	// ...and an Invoke through the public API sheds load with a clear error.
	spec := echoSpec()
	spec.TimeoutMs = 80
	if err := h.Load(ctx, "testaddon", echoWasm(), spec); err != nil {
		t.Fatal(err)
	}
	// Same bytes: pool survives, spec refreshed. Busy slot still held.
	_, err = h.Invoke(ctx, inst, "testaddon", "echo", []byte("x"), nil)
	if err == nil || !strings.Contains(err.Error(), "pooled instances busy") {
		t.Fatalf("want pool-exhausted error, got %v", err)
	}
	p.release(m1)
	out, err := h.Invoke(ctx, inst, "testaddon", "echo", []byte("x"), nil)
	if err != nil || string(out) != "x" {
		t.Fatalf("after release: %q %v", out, err)
	}
}

func TestPool_SizeNUsesDistinctInstancesUpToBound(t *testing.T) {
	h := newPoolHost(t, WithPoolSize(3))
	ctx := context.Background()
	if err := h.Load(ctx, "testaddon", echoWasm(), echoSpec()); err != nil {
		t.Fatal(err)
	}
	p := h.poolFor("testaddon", uuid.New(), mustEntry(t, h, "testaddon"))
	var mods []*Module
	for i := 0; i < 3; i++ {
		m, err := p.acquire(ctx)
		if err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
		mods = append(mods, m)
	}
	seen := map[*Module]bool{}
	for _, m := range mods {
		if seen[m] {
			t.Fatal("pool handed the same instance to two holders")
		}
		seen[m] = true
	}
	// Distinct linear memories: writing to one is invisible in another.
	mods[0].inst.Memory().WriteByte(2000, 0xAA)
	if b, _ := mods[1].inst.Memory().ReadByte(2000); b == 0xAA {
		t.Fatal("guest memory leaked between pool instances")
	}
	short, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	if _, err := p.acquire(short); err == nil {
		t.Fatal("4th acquire on a size-3 pool must block")
	}
	for _, m := range mods {
		p.release(m)
	}
	if idle, busy := p.stats(); idle != 3 || busy != 0 {
		t.Fatalf("stats idle=%d busy=%d, want 3/0", idle, busy)
	}
}

func TestPool_PerAddonSizeOverride(t *testing.T) {
	h := newPoolHost(t, WithPoolSize(2), WithAddonPoolSize(func(k string) int {
		if k == "big" {
			return 500 // clamped
		}
		return 0
	}))
	if h.sizeFor("big") != maxPoolSize || h.sizeFor("other") != 2 {
		t.Fatalf("sizes: big=%d other=%d", h.sizeFor("big"), h.sizeFor("other"))
	}
}

func TestPool_EnvConfig(t *testing.T) {
	t.Setenv(EnvPoolSize, "4")
	t.Setenv(EnvCompileCacheDir, t.TempDir())
	h := newPoolHost(t)
	if h.poolSize != 4 {
		t.Fatalf("env pool size = %d", h.poolSize)
	}
	t.Setenv(EnvPoolSize, "garbage")
	h2 := newPoolHost(t)
	if h2.poolSize != 1 {
		t.Fatalf("invalid env must fall back to 1, got %d", h2.poolSize)
	}
	// Explicit option beats env.
	h3 := newPoolHost(t, WithPoolSize(2))
	if h3.poolSize != 2 {
		t.Fatalf("option must beat env, got %d", h3.poolSize)
	}
}

// Many goroutines over several installations and pool sizes: every caller must
// get ITS payload back (no cross-talk) and the race detector must stay quiet.
func TestPool_ConcurrentInvokeNoCrosstalk(t *testing.T) {
	for _, size := range []int{1, 4} {
		t.Run(fmt.Sprintf("pool%d", size), func(t *testing.T) {
			h := newPoolHost(t, WithPoolSize(size))
			ctx := context.Background()
			spec := echoSpec()
			spec.TimeoutMs = 10000
			if err := h.Load(ctx, "testaddon", echoWasm(), spec); err != nil {
				t.Fatal(err)
			}
			installs := []uuid.UUID{uuid.New(), uuid.New()}
			var wg sync.WaitGroup
			var bad atomic.Int32
			for g := 0; g < 16; g++ {
				wg.Add(1)
				go func(g int) {
					defer wg.Done()
					for i := 0; i < 40; i++ {
						want := fmt.Sprintf("g%d-i%d", g, i)
						// echoWasm is a bump allocator that never frees: stay
						// well inside one 64 KiB page per instance by letting
						// traffic spread (size>1) or recycle (evict) — see below.
						out, err := h.Invoke(ctx, installs[g%2], "testaddon", "echo", []byte(want), nil)
						if err != nil {
							// Bump arena exhaustion is a fixture limit, not a bug.
							if strings.Contains(err.Error(), "out of range") {
								continue
							}
							bad.Add(1)
							t.Errorf("invoke: %v", err)
							return
						}
						if string(out) != want {
							bad.Add(1)
							t.Errorf("crosstalk: got %q want %q", out, want)
						}
					}
				}(g)
			}
			wg.Wait()
			for _, in := range installs {
				idle, busy := h.poolStats("testaddon", in)
				if busy != 0 || idle > size {
					t.Errorf("pool bound violated: idle=%d busy=%d size=%d", idle, busy, size)
				}
			}
		})
	}
}

// Unload racing with in-flight calls must neither panic nor leak: afterwards
// the installation has no pool, and a fresh Invoke rebuilds one.
func TestUnload_Installation_ClosesInstancesKeepsCompiled(t *testing.T) {
	h := newPoolHost(t, WithPoolSize(2))
	ctx := context.Background()
	if err := h.Load(ctx, "testaddon", echoWasm(), echoSpec()); err != nil {
		t.Fatal(err)
	}
	a, b := uuid.New(), uuid.New()
	for _, in := range []uuid.UUID{a, b} {
		if _, err := h.Invoke(ctx, in, "testaddon", "echo", []byte("hi"), nil); err != nil {
			t.Fatal(err)
		}
	}
	mod := h.anyModule(t, "testaddon", a)
	if err := h.Unload(ctx, "testaddon", a); err != nil {
		t.Fatal(err)
	}
	if !mod.inst.IsClosed() {
		t.Fatal("Unload must close the installation's instances")
	}
	if _, ok := h.modules.Load(instanceKey("testaddon", a)); ok {
		t.Fatal("pool of unloaded installation still mapped")
	}
	if _, ok := h.modules.Load(instanceKey("testaddon", b)); !ok {
		t.Fatal("other installation must be untouched")
	}
	if !h.IsCompiled("testaddon") {
		t.Fatal("per-installation Unload must keep the shared compiled module")
	}
	if out, err := h.Invoke(ctx, a, "testaddon", "echo", []byte("again"), nil); err != nil || string(out) != "again" {
		t.Fatalf("reinstall after unload: %q %v", out, err)
	}
	// Idempotent.
	if err := h.Unload(ctx, "nope", uuid.New()); err != nil {
		t.Fatal(err)
	}
}

func TestUnload_Addon_RemovesCompiledAndFailsClosed(t *testing.T) {
	h := newPoolHost(t)
	ctx := context.Background()
	if err := h.Load(ctx, "testaddon", echoWasm(), echoSpec()); err != nil {
		t.Fatal(err)
	}
	inst := uuid.New()
	if _, err := h.Invoke(ctx, inst, "testaddon", "echo", []byte("hi"), nil); err != nil {
		t.Fatal(err)
	}
	mod := h.anyModule(t, "testaddon", inst)
	if err := h.UnloadAddon(ctx, "testaddon"); err != nil {
		t.Fatal(err)
	}
	if h.IsCompiled("testaddon") || len(h.CompiledKeys()) != 0 {
		t.Fatal("compiled map must be emptied")
	}
	if !mod.inst.IsClosed() {
		t.Fatal("instance leaked after Unload")
	}
	_, err := h.Invoke(ctx, inst, "testaddon", "echo", []byte("x"), nil)
	var nl *AddonNotLoadedError
	if !errors.As(err, &nl) {
		t.Fatalf("want AddonNotLoadedError, got %v", err)
	}
}

func TestUnload_ConcurrentWithInvokes(t *testing.T) {
	h := newPoolHost(t, WithPoolSize(2))
	ctx := context.Background()
	if err := h.Load(ctx, "testaddon", echoWasm(), echoSpec()); err != nil {
		t.Fatal(err)
	}
	inst := uuid.New()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for g := 0; g < 6; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_, err := h.Invoke(ctx, inst, "testaddon", "echo", []byte("p"), nil)
				var nl *AddonNotLoadedError
				if err != nil && !errors.As(err, &nl) && !errors.Is(err, errPoolClosed) &&
					!strings.Contains(err.Error(), "out of range") && !isClosedModuleErr(err) {
					t.Errorf("unexpected error: %v", err)
					return
				}
			}
		}()
	}
	for i := 0; i < 20; i++ {
		_ = h.Unload(ctx, "testaddon", inst)
		time.Sleep(time.Millisecond)
	}
	_ = h.UnloadAddon(ctx, "testaddon")
	close(stop)
	wg.Wait()
	if idle, busy := h.poolStats("testaddon", inst); busy != 0 {
		t.Fatalf("leaked busy slots: idle=%d busy=%d", idle, busy)
	}
}

// Replacing the binary while calls are in flight must retire the old pool and
// never serve an instance of the superseded compile afterwards.
func TestLoad_ReplaceWhileInvoking(t *testing.T) {
	h := newPoolHost(t, WithPoolSize(2))
	ctx := context.Background()
	if err := h.Load(ctx, "testaddon", echoWasm(), echoSpec()); err != nil {
		t.Fatal(err)
	}
	inst := uuid.New()
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_, _ = h.Invoke(ctx, inst, "testaddon", "echo", []byte("p"), nil)
			}
		}()
	}
	for i := 0; i < 8; i++ {
		// A different valid binary each time (patched memory minimum).
		if err := h.Load(ctx, "testaddon", echoWasmMinPages(t, byte(1+i%3)), echoSpec()); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
	out, err := h.Invoke(ctx, inst, "testaddon", "echo", []byte("final"), nil)
	if err != nil || string(out) != "final" {
		t.Fatalf("after replace: %q %v", out, err)
	}
	v, _ := h.modules.Load(instanceKey("testaddon", inst))
	if v.(*instPool).entry != mustEntry(t, h, "testaddon") {
		t.Fatal("pool bound to a superseded compile")
	}
}

func mustEntry(t *testing.T, h *Host, addon string) *compiledEntry {
	t.Helper()
	v, ok := h.compiled.Load(addon)
	if !ok {
		t.Fatalf("%s not compiled", addon)
	}
	return v.(*compiledEntry)
}

func poolSizeOf(t *testing.T, h *Host, key string) int {
	t.Helper()
	return h.poolFor(key, uuid.New(), mustEntry(t, h, key)).size
}

func TestPool_ManifestConcurrencyAndPrecedence(t *testing.T) {
	ctx := context.Background()
	spec := echoSpec()

	// No manifest value, no host option: the historical single instance.
	h := newPoolHost(t)
	if err := h.Load(ctx, "testaddon", echoWasm(), spec); err != nil {
		t.Fatal(err)
	}
	if got := poolSizeOf(t, h, "testaddon"); got != 1 {
		t.Fatalf("default = %d, want 1", got)
	}

	// The manifest raises it for that addon alone.
	spec.Concurrency = 6
	if err := h.Load(ctx, "testaddon", echoWasm(), spec); err != nil {
		t.Fatal(err)
	}
	if got := poolSizeOf(t, h, "testaddon"); got != 6 {
		t.Fatalf("manifest concurrency = %d, want 6", got)
	}

	// An operator override beats the manifest (up and down).
	for _, tc := range []struct{ override, want int }{{2, 2}, {20, 20}, {0, 6}} {
		ho := newPoolHost(t, WithPoolSize(3), WithAddonPoolSize(func(string) int { return tc.override }))
		if err := ho.Load(ctx, "testaddon", echoWasm(), spec); err != nil {
			t.Fatal(err)
		}
		if got := poolSizeOf(t, ho, "testaddon"); got != tc.want {
			t.Fatalf("override %d: size %d, want %d", tc.override, got, tc.want)
		}
	}

	// Host default applies when the manifest is silent.
	hd := newPoolHost(t, WithPoolSize(3))
	spec.Concurrency = 0
	if err := hd.Load(ctx, "testaddon", echoWasm(), spec); err != nil {
		t.Fatal(err)
	}
	if got := poolSizeOf(t, hd, "testaddon"); got != 3 {
		t.Fatalf("host default = %d, want 3", got)
	}
}

func TestPool_MemoryBudgetCapsSize(t *testing.T) {
	ctx := context.Background()
	spec := echoSpec() // 4 MiB ceiling
	spec.Concurrency = 40
	h := newPoolHost(t, WithPoolMemoryBudget(64)) // 64 / 4 = 16 instances at most
	if err := h.Load(ctx, "testaddon", echoWasm(), spec); err != nil {
		t.Fatal(err)
	}
	if got := poolSizeOf(t, h, "testaddon"); got != 16 {
		t.Fatalf("size = %d, want 16 (budget 64 MiB / 4 MiB)", got)
	}
	// An operator override is capped by memory too: it cannot commit more than the budget.
	ho := newPoolHost(t, WithPoolMemoryBudget(8), WithAddonPoolSize(func(string) int { return 30 }))
	if err := ho.Load(ctx, "testaddon", echoWasm(), spec); err != nil {
		t.Fatal(err)
	}
	if got := poolSizeOf(t, ho, "testaddon"); got != 2 {
		t.Fatalf("override size = %d, want 2 (budget 8 MiB / 4 MiB)", got)
	}
	// A budget smaller than one instance still leaves one.
	hs := newPoolHost(t, WithPoolMemoryBudget(1))
	if err := hs.Load(ctx, "testaddon", echoWasm(), spec); err != nil {
		t.Fatal(err)
	}
	if got := poolSizeOf(t, hs, "testaddon"); got != 1 {
		t.Fatalf("size = %d, want 1", got)
	}
	// No declared memory_limit_mb: the 256 MiB global ceiling counts, default budget 1024 -> 4.
	spec.MemoryLimitMB = 0
	hg := newPoolHost(t)
	if err := hg.Load(ctx, "testaddon", echoWasm(), spec); err != nil {
		t.Fatal(err)
	}
	if got := poolSizeOf(t, hg, "testaddon"); got != 4 {
		t.Fatalf("undeclared ceiling: size = %d, want 4", got)
	}
}

func TestPool_MemoryBudgetEnv(t *testing.T) {
	t.Setenv(EnvPoolMemoryMB, "32")
	h := newPoolHost(t)
	if h.poolMemMB != 32 {
		t.Fatalf("env budget = %d", h.poolMemMB)
	}
	t.Setenv(EnvPoolMemoryMB, "nope")
	if h2 := newPoolHost(t); h2.poolMemMB != defaultPoolMemoryMB {
		t.Fatalf("invalid env must fall back, got %d", h2.poolMemMB)
	}
}

// Pools are per (addon, installation): two organizations never share an
// instance, whatever the size.
func TestPool_ConcurrencyKeepsTenantIsolation(t *testing.T) {
	ctx := context.Background()
	spec := echoSpec()
	spec.Concurrency = 4
	h := newPoolHost(t)
	if err := h.Load(ctx, "testaddon", echoWasm(), spec); err != nil {
		t.Fatal(err)
	}
	entry := mustEntry(t, h, "testaddon")
	a, b := uuid.New(), uuid.New()
	pa, pb := h.poolFor("testaddon", a, entry), h.poolFor("testaddon", b, entry)
	if pa == pb {
		t.Fatal("two installations share a pool")
	}
	ma, err := pa.acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	mb, err := pb.acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ma == mb {
		t.Fatal("two installations share an instance")
	}
	pa.release(ma)
	pb.release(mb)
}

// Changing backend.concurrency on a same-bytes reload rebuilds the pools.
func TestPool_ConcurrencyChangeRebuildsPools(t *testing.T) {
	ctx := context.Background()
	h := newPoolHost(t)
	spec := echoSpec()
	if err := h.Load(ctx, "testaddon", echoWasm(), spec); err != nil {
		t.Fatal(err)
	}
	inst := uuid.New()
	if got := h.poolFor("testaddon", inst, mustEntry(t, h, "testaddon")).size; got != 1 {
		t.Fatalf("size = %d", got)
	}
	spec2 := echoSpec()
	spec2.Concurrency = 5
	if err := h.Load(ctx, "testaddon", echoWasm(), spec2); err != nil {
		t.Fatal(err)
	}
	if got := h.poolFor("testaddon", inst, mustEntry(t, h, "testaddon")).size; got != 5 {
		t.Fatalf("size after reload = %d, want 5", got)
	}
	if out, err := h.Invoke(ctx, inst, "testaddon", "echo", []byte("ok"), nil); err != nil || string(out) != "ok" {
		t.Fatalf("invoke after resize: %q %v", out, err)
	}
}

// BenchmarkPoolBlockingHostImport models what serialises a store today: each
// call spends most of its time inside a host import (here an event_emit whose
// subscriber takes 2 ms, standing in for a db_query / http_fetch / the ~270 ms
// catalog read). One installation, 16 concurrent callers; compare pool sizes:
//
//	go test ./runtime/wasm -run '^$' -bench PoolBlockingHostImport -benchtime=2s
//
// ns/op is wall time per call across all callers (lower = more throughput).
func BenchmarkPoolBlockingHostImport(b *testing.B) {
	log.SetOutput(io.Discard) // the event bus logs every publish
	defer log.SetOutput(os.Stderr)
	for _, size := range []int{1, 2, 4, 8} {
		b.Run(fmt.Sprintf("pool%d", size), func(b *testing.B) {
			ctx := context.Background()
			bus := events.NewBus(nil)
			if err := bus.Subscribe("testaddon", "ev.fired", func(context.Context, uuid.UUID, any) error {
				time.Sleep(2 * time.Millisecond)
				return nil
			}); err != nil {
				b.Fatal(err)
			}
			h, err := NewHost(ctx, security.Compile("testaddon", nil), nil, WithPoolSize(size))
			if err != nil {
				b.Fatal(err)
			}
			defer h.Close(ctx)
			h.WithBus(bus)
			spec := &manifest.BackendSpec{Runtime: "wasm", Entry: "b.wasm", Exports: []string{"emit_test"}, MemoryLimitMB: 4, TimeoutMs: 60000}
			if err := h.Load(ctx, "testaddon", eventEmitWasm(), spec); err != nil {
				b.Fatal(err)
			}
			org, inst := uuid.New(), uuid.New()
			b.SetParallelism(16)
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					if _, err := h.InvokeFor(ctx, org, inst, "testaddon", "emit_test", nil, nil); err != nil {
						b.Error(err)
						return
					}
				}
			})
		})
	}
}
