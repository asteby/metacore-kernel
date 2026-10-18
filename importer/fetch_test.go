package importer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// rangeServer sirve un JPEG de ~6 KiB con validadores y soporte opcional de Range. Cortar>0 hace que
// la primera respuesta completa se interrumpa tras esa cantidad de bytes.
type rangeServer struct {
	*httptest.Server
	mu       sync.Mutex
	body     []byte
	etag     string
	lastMod  string
	honor    bool // atiende Range / If-Range
	cutAt    int  // cortar la primera respuesta con 200 después de estos bytes
	requests []http.Header
	status   []int
	cut      bool
}

func newRangeServer(t *testing.T, honor bool, cutAt int, etag, lastMod string) *rangeServer {
	t.Helper()
	body := append([]byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00}, bytes.Repeat([]byte("0123456789abcdef"), 400)...)
	s := &rangeServer{body: body, etag: etag, lastMod: lastMod, honor: honor, cutAt: cutAt}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

func (s *rangeServer) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.requests = append(s.requests, r.Header.Clone())
	s.mu.Unlock()
	h := w.Header()
	h.Set("Content-Type", "image/jpeg")
	if s.etag != "" {
		h.Set("ETag", s.etag)
	}
	if s.lastMod != "" {
		h.Set("Last-Modified", s.lastMod)
	}
	if (s.etag != "" && r.Header.Get("If-None-Match") == s.etag) || (s.lastMod != "" && r.Header.Get("If-Modified-Since") == s.lastMod) {
		s.record(304)
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if rg := r.Header.Get("Range"); rg != "" && s.honor {
		if ir := r.Header.Get("If-Range"); ir == "" || ir == s.etag || ir == s.lastMod {
			var start int
			if _, err := fmt.Sscanf(rg, "bytes=%d-", &start); err != nil || start >= len(s.body) {
				s.record(416)
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(s.body)-1, len(s.body)))
			h.Set("Content-Length", strconv.Itoa(len(s.body)-start))
			s.record(206)
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(s.body[start:])
			return
		}
	}
	h.Set("Content-Length", strconv.Itoa(len(s.body)))
	s.record(200)
	if s.cutAt > 0 && !s.cut {
		s.cut = true
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(s.body[:s.cutAt])
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler) // corta la conexión a mitad del cuerpo
	}
	_, _ = w.Write(s.body)
}

func (s *rangeServer) record(code int) {
	s.mu.Lock()
	s.status = append(s.status, code)
	s.mu.Unlock()
}

func fetchDeps(attempts int) *TransformDeps {
	return &TransformDeps{
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
		Sleep:      func(context.Context, time.Duration) error { return nil },
		Policy:     MediaPolicy{MaxAttempts: attempts},
	}
}

func TestFetchMedia_ResumesACutDownloadWithRangeAndIfRange(t *testing.T) {
	s := newRangeServer(t, true, 2000, `"v1"`, "")
	f, err := FetchMedia(fetchDeps(3), s.URL+"/a.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(f.Data, s.body) || f.Type != "image/jpeg" || f.ETag != `"v1"` {
		t.Fatalf("el archivo ensamblado debe ser idéntico al original (%d vs %d bytes)", len(f.Data), len(s.body))
	}
	if len(s.requests) != 2 || strings.Join(strInts(s.status), ",") != "200,206" {
		t.Fatalf("estados = %v", s.status)
	}
	if got := s.requests[1].Get("Range"); got != "bytes=2000-" {
		t.Fatalf("Range = %q, quería bytes=2000-", got)
	}
	if s.requests[1].Get("If-Range") != `"v1"` {
		t.Fatalf("If-Range = %q", s.requests[1].Get("If-Range"))
	}
	if f.ResumedBytes != 2000 {
		t.Fatalf("ResumedBytes = %d", f.ResumedBytes)
	}
}

func strInts(in []int) []string {
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = strconv.Itoa(v)
	}
	return out
}

func TestFetchMedia_ResumesWithLastModifiedWhenThereIsNoStrongETag(t *testing.T) {
	lm := "Mon, 02 Jan 2006 15:04:05 GMT"
	s := newRangeServer(t, true, 1500, `W/"debil"`, lm)
	f, err := FetchMedia(fetchDeps(3), s.URL+"/a.jpg")
	if err != nil || !bytes.Equal(f.Data, s.body) {
		t.Fatalf("%v", err)
	}
	if s.requests[1].Get("If-Range") != lm {
		t.Fatalf("un ETag débil no sirve para If-Range; debe usar Last-Modified: %q", s.requests[1].Get("If-Range"))
	}
}

func TestFetchMedia_WithoutAValidatorItRestartsFromScratchNeverGuessing(t *testing.T) {
	s := newRangeServer(t, true, 2000, "", "")
	f, err := FetchMedia(fetchDeps(3), s.URL+"/a.jpg")
	if err != nil || !bytes.Equal(f.Data, s.body) || f.ResumedBytes != 0 {
		t.Fatalf("%v", err)
	}
	if s.requests[1].Get("Range") != "" {
		t.Fatal("sin ETag ni Last-Modified no se puede garantizar que sea el mismo archivo: no se pide Range")
	}
}

func TestFetchMedia_ServerIgnoringRangeFallsBackToTheFullBody(t *testing.T) {
	s := newRangeServer(t, false, 2000, `"v1"`, "")
	f, err := FetchMedia(fetchDeps(3), s.URL+"/a.jpg")
	if err != nil || !bytes.Equal(f.Data, s.body) || f.ResumedBytes != 0 {
		t.Fatalf("%v", err)
	}
	if strings.Join(strInts(s.status), ",") != "200,200" || s.requests[1].Get("Range") == "" {
		t.Fatalf("estados = %v", s.status)
	}
}

func TestFetchMedia_ChangedFileMidDownloadIsNotStitchedTogether(t *testing.T) {
	// Entre los dos intentos el archivo cambia: If-Range ya no coincide y el servidor manda el nuevo completo.
	s := newRangeServer(t, true, 2000, `"v1"`, "")
	deps := fetchDeps(3)
	deps.Sleep = func(context.Context, time.Duration) error {
		s.mu.Lock()
		s.etag = `"v2"`
		s.body = append([]byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00}, bytes.Repeat([]byte("ZYXWVUTS"), 700)...)
		s.mu.Unlock()
		return nil
	}
	f, err := FetchMedia(deps, s.URL+"/a.jpg")
	if err != nil || !bytes.Equal(f.Data, s.body) || f.ETag != `"v2"` {
		t.Fatalf("debe quedar el archivo nuevo íntegro: %v", err)
	}
	if strings.Join(strInts(s.status), ",") != "200,200" {
		t.Fatalf("estados = %v", s.status)
	}
}

func TestFetchMedia_ConditionalRequestReturnsNotModified(t *testing.T) {
	s := newRangeServer(t, true, 0, `"v1"`, "")
	f, err := FetchMediaWith(fetchDeps(1), s.URL+"/a.jpg", FetchOptions{IfNoneMatch: `"v1"`})
	if err != nil || !f.NotModified || len(f.Data) != 0 || f.ETag != `"v1"` {
		t.Fatalf("%v %+v", err, f)
	}
	// Con otro validador el servidor manda el cuerpo.
	f, err = FetchMediaWith(fetchDeps(1), s.URL+"/a.jpg", FetchOptions{IfNoneMatch: `"viejo"`})
	if err != nil || f.NotModified || !bytes.Equal(f.Data, s.body) {
		t.Fatalf("%v", err)
	}
	lm := "Mon, 02 Jan 2006 15:04:05 GMT"
	s2 := newRangeServer(t, true, 0, "", lm)
	f, err = FetchMediaWith(fetchDeps(1), s2.URL+"/a.jpg", FetchOptions{IfModifiedSince: lm})
	if err != nil || !f.NotModified || f.LastModified != lm {
		t.Fatalf("%v %+v", err, f)
	}
}

func TestFetchMedia_RangeNeverExceedsTheSizeLimitAndTypeStillChecked(t *testing.T) {
	s := newRangeServer(t, true, 2000, `"v1"`, "")
	deps := fetchDeps(3)
	deps.Policy.MaxBytes = 3000 // el archivo mide ~6.4 KiB
	if _, err := FetchMedia(deps, s.URL+"/a.jpg"); !errors.Is(err, ErrMediaTooLarge) {
		t.Fatalf("err = %v", err)
	}
}

func TestFetchMedia_RejectedRangeRestartsInsteadOfFailing(t *testing.T) {
	// El servidor devuelve 416 al pedir el resto (p. ej. el archivo se acortó): se baja de nuevo completo.
	s := newRangeServer(t, true, 2000, `"v1"`, "")
	deps := fetchDeps(3)
	first := true
	deps.Sleep = func(context.Context, time.Duration) error {
		if first {
			first = false
			s.mu.Lock()
			s.body = s.body[:1500] // ahora el archivo es más corto que lo ya descargado
			s.mu.Unlock()
		}
		return nil
	}
	f, err := FetchMedia(deps, s.URL+"/a.jpg")
	if err != nil || !bytes.Equal(f.Data, s.body) {
		t.Fatalf("%v", err)
	}
}
