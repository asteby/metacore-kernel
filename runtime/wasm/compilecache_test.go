package wasm

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/asteby/metacore-kernel/manifest"
	kmetrics "github.com/asteby/metacore-kernel/metrics"
	"github.com/google/uuid"
	dto "github.com/prometheus/client_model/go"
)

// bigWasm is echoWasm plus n filler functions, so compilation has real work
// to do (cold vs cached is measurable). Filler: (i32)->i32 { x*3 + x/7 ... }.
func bigWasm(n int) []byte {
	buf := []byte{0x00, 0x61, 0x73, 0x6D, 0x01, 0x00, 0x00, 0x00}
	buf = append(buf, section(0x01, []byte{
		0x02,
		0x60, 0x01, 0x7F, 0x01, 0x7F,
		0x60, 0x02, 0x7F, 0x7F, 0x01, 0x7E,
	})...)
	funcs := encodeULEB128(uint32(n + 2))
	funcs = append(funcs, 0x00, 0x01)
	for i := 0; i < n; i++ {
		funcs = append(funcs, 0x00)
	}
	buf = append(buf, section(0x03, funcs)...)
	buf = append(buf, section(0x05, []byte{0x01, 0x00, 0x01})...)
	globals := []byte{0x01, 0x7F, 0x01, 0x41}
	globals = append(globals, encodeSLEB128(1024)...)
	globals = append(globals, 0x0B)
	buf = append(buf, section(0x06, globals)...)
	var exports []byte
	exports = append(exports, 0x03)
	exports = append(exports, encodeName("memory")...)
	exports = append(exports, 0x02, 0x00)
	exports = append(exports, encodeName("alloc")...)
	exports = append(exports, 0x00, 0x00)
	exports = append(exports, encodeName("echo")...)
	exports = append(exports, 0x00, 0x01)
	buf = append(buf, section(0x07, exports)...)
	alloc := withSize([]byte{0x01, 0x01, 0x7F, 0x23, 0x00, 0x22, 0x01, 0x20, 0x00, 0x6A, 0x24, 0x00, 0x20, 0x01, 0x0B})
	echo := withSize([]byte{0x00, 0x20, 0x00, 0xAD, 0x42, 0x20, 0x86, 0x20, 0x01, 0xAD, 0x84, 0x0B})
	code := encodeULEB128(uint32(n + 2))
	code = append(code, alloc...)
	code = append(code, echo...)
	for i := 0; i < n; i++ {
		var b []byte
		b = append(b, 0x00)
		for k := 0; k < 12; k++ { // x = x*K + (x>>3) ^ K
			b = append(b, 0x20, 0x00, 0x41)
			b = append(b, encodeSLEB128(int32(3+k+i%11))...)
			b = append(b, 0x6C, 0x20, 0x00, 0x41, 0x03, 0x76, 0x6A, 0x21, 0x00)
		}
		b = append(b, 0x20, 0x00, 0x0B)
		code = append(code, withSize(b)...)
	}
	buf = append(buf, section(0x0A, code)...)
	return buf
}

func dirBytes(t testing.TB, dir string) (files int, total int64) {
	t.Helper()
	_ = filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			files++
			total += info.Size()
		}
		return nil
	})
	return
}

func TestCompileCache_PersistsAcrossHostsAndProcesses(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "wasm-cache") // does not exist yet
	ctx := context.Background()
	wasmBytes := bigWasm(300)
	spec := echoSpec()

	cold := newPoolHost(t, WithCompileCacheDir(dir))
	t0 := time.Now()
	if err := cold.Load(ctx, "testaddon", wasmBytes, spec); err != nil {
		t.Fatalf("cold Load: %v", err)
	}
	coldDur := time.Since(t0)
	files, size := dirBytes(t, dir)
	if files == 0 || size == 0 {
		t.Fatalf("compiled code not persisted in %s (files=%d)", dir, files)
	}
	if st, err := os.Stat(dir); err != nil || st.Mode().Perm()&0o077 != 0 {
		t.Fatalf("cache dir must be created private (0700): %v %v", st, err)
	}
	if err := cold.Close(ctx); err != nil {
		t.Fatal(err)
	}

	// "Another replica / restarted process": new Host, same volume.
	warm := newPoolHost(t, WithCompileCacheDir(dir))
	t1 := time.Now()
	if err := warm.Load(ctx, "testaddon", wasmBytes, spec); err != nil {
		t.Fatalf("warm Load: %v", err)
	}
	warmDur := time.Since(t1)
	after, _ := dirBytes(t, dir)
	if after != files {
		t.Fatalf("warm Load wrote new cache files (%d -> %d): it recompiled", files, after)
	}
	out, err := warm.Invoke(ctx, uuid.New(), "testaddon", "echo", []byte("cached"), nil)
	if err != nil || string(out) != "cached" {
		t.Fatalf("invoke on cache-loaded module: %q %v", out, err)
	}
	t.Logf("cold compile %s, warm (persistent cache) %s", coldDur, warmDur)
}

func TestCompileCache_BadDirDegradesToMemory(t *testing.T) {
	file := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newPoolHost(t, WithCompileCacheDir(filepath.Join(file, "sub"))) // parent is a file
	if err := h.Load(context.Background(), "testaddon", echoWasm(), echoSpec()); err != nil {
		t.Fatalf("Load must still work with an unusable cache dir: %v", err)
	}
}

func TestCompileCache_SharedAcrossMemoryLimitRuntimes(t *testing.T) {
	dir := t.TempDir()
	h := newPoolHost(t, WithCompileCacheDir(dir))
	ctx := context.Background()
	for i, mb := range []int{1, 2, 8} {
		spec := &manifest.BackendSpec{Runtime: "wasm", Entry: "b.wasm", Exports: []string{"echo"}, MemoryLimitMB: mb, TimeoutMs: 2000}
		if err := h.Load(ctx, fmt.Sprintf("a%d", i), bigWasm(50), spec); err != nil {
			t.Fatal(err)
		}
	}
	// Same bytes under 3 different runtimes => one cache entry set, not three.
	files, _ := dirBytes(t, dir)
	single := t.TempDir()
	h2 := newPoolHost(t, WithCompileCacheDir(single))
	spec := &manifest.BackendSpec{Runtime: "wasm", Entry: "b.wasm", Exports: []string{"echo"}, MemoryLimitMB: 1, TimeoutMs: 2000}
	if err := h2.Load(ctx, "a", bigWasm(50), spec); err != nil {
		t.Fatal(err)
	}
	one, _ := dirBytes(t, single)
	if files > one+1 { // tolerate a sibling file, not 3x
		t.Fatalf("limit runtimes did not share the cache: %d files vs %d for a single runtime", files, one)
	}
}

func counterValue(t *testing.T, reg *kmetrics.Registry, name string, labels map[string]string) float64 {
	t.Helper()
	mfs, err := reg.Prometheus.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
	next:
		for _, m := range mf.Metric {
			for k, v := range labels {
				found := false
				for _, lp := range m.Label {
					if lp.GetName() == k && lp.GetValue() == v {
						found = true
					}
				}
				if !found {
					continue next
				}
			}
			return metricNumber(m)
		}
	}
	return 0
}

func metricNumber(m *dto.Metric) float64 {
	switch {
	case m.Counter != nil:
		return m.Counter.GetValue()
	case m.Gauge != nil:
		return m.Gauge.GetValue()
	case m.Histogram != nil:
		return float64(m.Histogram.GetSampleCount())
	}
	return 0
}

func TestMetrics_InvocationsEvictionsCompilations(t *testing.T) {
	reg := kmetrics.NewRegistry()
	h := newPoolHost(t, WithMetrics(reg), WithPoolSize(2))
	ctx := context.Background()
	if err := h.Load(ctx, "testaddon", echoWasm(), echoSpec()); err != nil {
		t.Fatal(err)
	}
	// Re-loading identical bytes must not count as a compilation.
	if err := h.Load(ctx, "testaddon", echoWasm(), echoSpec()); err != nil {
		t.Fatal(err)
	}
	inst := uuid.New()
	for i := 0; i < 3; i++ {
		if _, err := h.Invoke(ctx, inst, "testaddon", "echo", []byte("m"), nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.Invoke(ctx, inst, "testaddon", "nope", nil, nil); err == nil {
		t.Fatal("non-exported function must fail")
	}
	a := map[string]string{"addon": "testaddon"}
	if v := counterValue(t, reg, "wasm_compilations_total", a); v != 1 {
		t.Errorf("compilations = %v, want 1", v)
	}
	if v := counterValue(t, reg, "wasm_invocations_total", map[string]string{"addon": "testaddon", "result": "ok"}); v != 3 {
		t.Errorf("ok invocations = %v, want 3", v)
	}
	if v := counterValue(t, reg, "wasm_invocations_total", map[string]string{"addon": "testaddon", "result": "error"}); v != 1 {
		t.Errorf("error invocations = %v, want 1", v)
	}
	if v := counterValue(t, reg, "wasm_invocation_duration_seconds", a); v != 4 {
		t.Errorf("latency samples = %v, want 4", v)
	}
	if v := counterValue(t, reg, "wasm_instantiations_total", a); v != 1 {
		t.Errorf("sequential calls reuse one instance, instantiations = %v", v)
	}
	if v := counterValue(t, reg, "wasm_instances", a); v != 1 {
		t.Errorf("live instances = %v, want 1", v)
	}
	_ = h.Unload(ctx, "testaddon", inst)
	if v := counterValue(t, reg, "wasm_instance_evictions_total", map[string]string{"addon": "testaddon", "reason": "unload"}); v != 1 {
		t.Errorf("unload evictions = %v, want 1", v)
	}
	if v := counterValue(t, reg, "wasm_instances", a); v != 0 {
		t.Errorf("live instances after unload = %v, want 0", v)
	}
	// Second Host on the same registry shares collectors instead of panicking.
	h2 := newPoolHost(t, WithMetrics(reg))
	if h2.metrics == nil {
		t.Fatal("second host lost metrics")
	}
}

func TestMetrics_TrapEviction(t *testing.T) {
	reg := kmetrics.NewRegistry()
	h := newPoolHost(t, WithMetrics(reg))
	ctx := context.Background()
	spec := &manifest.BackendSpec{Runtime: "wasm", Entry: "b.wasm", Exports: []string{"boom"}, MemoryLimitMB: 4, TimeoutMs: 2000}
	if err := h.Load(ctx, "testaddon", trapWasm(), spec); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Invoke(ctx, uuid.New(), "testaddon", "boom", []byte("x"), nil); err == nil {
		t.Fatal("expected trap")
	}
	if v := counterValue(t, reg, "wasm_instance_evictions_total", map[string]string{"addon": "testaddon", "reason": "trap"}); v != 1 {
		t.Errorf("trap evictions = %v, want 1", v)
	}
}

// ---- benchmarks ------------------------------------------------------------

// BenchmarkLoad compares compiling from scratch (no cache) with loading from a
// populated persistent cache — the restart / scale-out path.
func BenchmarkLoad(b *testing.B) {
	wasmBytes := bigWasm(2000)
	spec := echoSpec()
	ctx := context.Background()
	run := func(b *testing.B, dir string) {
		for i := 0; i < b.N; i++ {
			b.StopTimer()
			h, err := NewHost(ctx, nil, nil, WithCompileCacheDir(dir))
			if err != nil {
				b.Fatal(err)
			}
			b.StartTimer()
			if err := h.Load(ctx, "bench", wasmBytes, spec); err != nil {
				b.Fatal(err)
			}
			b.StopTimer()
			_ = h.Close(ctx)
			b.StartTimer()
		}
	}
	b.Run("cold_no_cache", func(b *testing.B) {
		run(b, "") // memory-only cache, fresh per iteration: always compiles
	})
	b.Run("warm_persistent_cache", func(b *testing.B) {
		dir := b.TempDir()
		h, _ := NewHost(ctx, nil, nil, WithCompileCacheDir(dir))
		if err := h.Load(ctx, "bench", wasmBytes, spec); err != nil { // populate
			b.Fatal(err)
		}
		_ = h.Close(ctx)
		b.ResetTimer()
		run(b, dir)
	})
}

// BenchmarkInvoke compares the first call on an installation (instantiation)
// with a warm pooled call.
func BenchmarkInvoke(b *testing.B) {
	ctx := context.Background()
	h, err := NewHost(ctx, nil, nil, WithCompileCacheDir(b.TempDir()))
	if err != nil {
		b.Fatal(err)
	}
	defer h.Close(ctx)
	if err := h.Load(ctx, "bench", bigWasm(200), echoSpec()); err != nil {
		b.Fatal(err)
	}
	b.Run("cold_instance", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := h.Invoke(ctx, uuid.New(), "bench", "echo", []byte("x"), nil); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("warm_pooled", func(b *testing.B) {
		inst := uuid.New()
		for i := 0; i < b.N; i++ {
			// Recycle before the fixture's bump arena (64 KiB) fills.
			if i%1000 == 0 {
				_ = h.Unload(ctx, "bench", inst)
			}
			if _, err := h.Invoke(ctx, inst, "bench", "echo", []byte("x"), nil); err != nil {
				b.Fatal(err)
			}
		}
	})
}
