package dataload

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"
)

// plainJobs esconde la extensión LimitedJobStore: un almacén que solo sabe Claim.
type plainJobs struct{ MediaJobStore }

// secondQueue es otro "proceso" que comparte almacén, manifiesto y origen con el rig.
func secondQueue(r *rig, jobs MediaJobStore) *MediaQueue {
	return &MediaQueue{Jobs: jobs, Backup: r.queue.Backup, Sink: r.sink, Now: r.clk.Now, Workers: r.queue.Workers, Limits: r.queue.Limits}
}

// Punto 2: el tope global de una organización se cumple entre procesos que
// comparten la cola, y no frena a las demás organizaciones.
func TestMediaGlobal_CapHoldsAcrossProcessesSharingTheStore(t *testing.T) {
	r := newRig(t)
	o := newVOrigin(t)
	o.delay = 30 * time.Millisecond
	var urlsA, urlsB []string
	for i := 0; i < 8; i++ {
		pa, pb := fmt.Sprintf("/a/%d.jpg", i), fmt.Sprintf("/b/%d.jpg", i)
		o.set(pa, jpegN("ga"+strconv.Itoa(i), 64), "")
		o.set(pb, jpegN("gb"+strconv.Itoa(i), 64), "")
		urlsA, urlsB = append(urlsA, o.URL(pa)), append(urlsB, o.URL(pb))
	}
	r.queue.Workers = 4
	r.queue.Limits = StaticOrgLimits(OrgMediaLimits{}, map[string]OrgMediaLimits{"org-a": {MaxConcurrentGlobal: 2}})
	enqueueURLs(t, r, "org-a", "images", urlsA...)
	enqueueURLs(t, r, "org-b", "images", urlsB...)
	queues := []*MediaQueue{r.queue, secondQueue(r, r.jobs), secondQueue(r, r.jobs)}

	ctx := context.Background()
	for round := 0; round < 30; round++ {
		var wg sync.WaitGroup
		for _, q := range queues {
			wg.Add(1)
			go func(q *MediaQueue) { defer wg.Done(); _, _ = q.ProcessOnce(ctx, 10) }(q)
		}
		wg.Wait()
		a, _ := r.queue.Stats(ctx, "org-a", "")
		b, _ := r.queue.Stats(ctx, "org-b", "")
		if a.Done == 8 && b.Done == 8 {
			break
		}
	}
	if got := o.maxConcurrent("a"); got > 2 || got < 1 {
		t.Fatalf("org-a tiene tope global 2 entre tres procesos y llegó a %d", got)
	}
	if got := o.maxConcurrent("b"); got <= 2 {
		t.Fatalf("org-b no tiene tope y debería pasar de 2 simultáneos con tres procesos (máx %d)", got)
	}
	for _, org := range []string{"org-a", "org-b"} {
		if st, _ := r.queue.Stats(ctx, org, ""); st.Done != 8 {
			t.Fatalf("%s: %+v", org, st)
		}
	}
}

// Punto 2: el almacén en memoria nunca reclama de más aunque el tope ya esté
// ocupado por trabajos en curso de otro proceso, y libera lo vencido.
func TestMediaGlobal_MemoryStoreCountsRunningJobsWithLiveLeases(t *testing.T) {
	r := newRig(t)
	enqueueURLs(t, r, "org-a", "images", "http://x/1", "http://x/2", "http://x/3")
	now := r.clk.Now()
	cap2 := func(string) int { return 2 }
	first, _ := r.jobs.ClaimLimited(context.Background(), now, 10, time.Minute, cap2)
	if len(first) != 2 {
		t.Fatalf("primer proceso: %d", len(first))
	}
	if again, _ := r.jobs.ClaimLimited(context.Background(), now, 10, time.Minute, cap2); len(again) != 0 {
		t.Fatalf("con el tope ocupado no se reclama nada: %d", len(again))
	}
	// Vencido el arrendamiento (el proceso murió), esos lugares vuelven a contar libres.
	later := now.Add(2 * time.Minute)
	if got, _ := r.jobs.ClaimLimited(context.Background(), later, 10, time.Minute, cap2); len(got) != 2 {
		t.Fatalf("tras vencer: %d", len(got))
	}
}

// Punto 2: sin la extensión, el tope global se aplica como tope por proceso.
func TestMediaGlobal_WithoutTheExtensionTheGlobalCapActsPerProcess(t *testing.T) {
	r := newRig(t)
	o := newVOrigin(t)
	o.delay = 30 * time.Millisecond
	var urls []string
	for i := 0; i < 6; i++ {
		p := fmt.Sprintf("/a/%d.jpg", i)
		o.set(p, jpegN("gp"+strconv.Itoa(i), 64), "")
		urls = append(urls, o.URL(p))
	}
	r.queue.Jobs, r.queue.Workers = plainJobs{r.jobs}, 8
	r.queue.Limits = StaticOrgLimits(OrgMediaLimits{MaxConcurrentGlobal: 2}, nil)
	enqueueURLs(t, r, "org-a", "images", urls...)
	if n, err := r.queue.ProcessOnce(context.Background(), 100); err != nil || n != 6 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if got := o.maxConcurrent("a"); got != 2 {
		t.Fatalf("tope por proceso = %d", got)
	}
}
