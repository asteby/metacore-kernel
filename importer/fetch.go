package importer

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/asteby/metacore-kernel/netguard"
)

// FetchOptions personaliza una descarga.
type FetchOptions struct {
	// IfNoneMatch / IfModifiedSince hacen la petición condicional: si el servidor
	// responde 304, el resultado trae NotModified y ningún byte. Se mandan solo en
	// la primera petición de una descarga (no con una reanudación).
	IfNoneMatch     string
	IfModifiedSince string
	// Resume continúa una descarga cortada en un intento ANTERIOR (otra llamada,
	// otro proceso): se piden solo los bytes que faltan con Range + If-Range. Solo
	// se usa con validador fuerte; si el servidor no confirma la misma versión
	// (200 completo, 416, rango que no empieza donde se esperaba) se descarta y se
	// baja de nuevo, nunca se mezclan versiones.
	Resume *Partial
	// OnPartial, si no es nil, recibe lo bajado cuando la llamada termina en error
	// con un cuerpo incompleto que se puede continuar (validador fuerte). Quien
	// llama lo guarda y lo pasa como Resume en el siguiente intento.
	OnPartial func(Partial)
}

// Partial es una descarga incompleta que se puede continuar con Range.
type Partial struct {
	Data         []byte `json:"data"`
	ETag         string `json:"etag,omitempty"`
	LastModified string `json:"lastModified,omitempty"`
}

// Resumable dice si p se puede continuar: trae bytes y un validador fuerte.
func (p Partial) Resumable() bool {
	d := download{data: p.Data, etag: p.ETag, lastMod: p.LastModified}
	return len(p.Data) > 0 && d.ifRange() != ""
}

// FetchMedia descarga rawURL aplicando MediaPolicy (allowlist, tope de bytes
// sin truncar, tipo por magic bytes, reintentos con backoff y Retry-After) y la
// guarda de salida de netguard. No escribe en ningún almacén: quien llama decide
// qué hacer con los bytes (media_url los guarda; dataload respalda el original
// por sha256). TransformDeps.Store no se usa.
//
// Si la conexión se corta a mitad del cuerpo y el servidor dio un validador
// fuerte (ETag que no sea W/ o Last-Modified), el siguiente intento pide solo lo
// que falta con Range + If-Range; si el servidor contesta otra cosa (200 completo,
// 416, un rango que no empieza donde se esperaba) se descarta lo parcial y se
// baja de nuevo, nunca se mezclan versiones.
func FetchMedia(deps *TransformDeps, rawURL string) (*FetchedMedia, error) {
	return FetchMediaWith(deps, rawURL, FetchOptions{})
}

// FetchMediaWith es FetchMedia con opciones (petición condicional).
func FetchMediaWith(deps *TransformDeps, rawURL string, opt FetchOptions) (*FetchedMedia, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil, fmt.Errorf("URL vacía")
	}
	if !strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://") {
		return nil, fmt.Errorf("URL debe empezar con http:// o https:// (recibido: %q)", rawURL)
	}
	if deps == nil {
		deps = &TransformDeps{}
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("URL inválida: %q", rawURL)
	}
	pol := deps.Policy
	if len(pol.AllowHosts) > 0 && !netguard.HostAllowed(pol.AllowHosts, strings.ToLower(u.Host)) {
		return nil, fmt.Errorf("%w: %s", ErrMediaHost, u.Host)
	}

	ctx := transformDepsContext(deps)
	client := httpClient(deps)
	if deps.HTTPClient == nil {
		defer client.CloseIdleConnections()
	}
	sleep := sleepFn(deps)
	attempts := pol.maxAttempts()

	st := &download{}
	if r := opt.Resume; r != nil && r.Resumable() && int64(len(r.Data)) < pol.maxBytes() {
		st.data, st.etag, st.lastMod, st.partial = append([]byte(nil), r.Data...), r.ETag, r.LastModified, true
	}
	// Al salir con error, lo que se pueda continuar se entrega a quien llama.
	keep := func(err error) error {
		if opt.OnPartial != nil && st.partial && len(st.data) > 0 && st.ifRange() != "" {
			opt.OnPartial(Partial{Data: append([]byte(nil), st.data...), ETag: st.etag, LastModified: st.lastMod})
		}
		return err
	}
	var lastErr error
	for n := 1; n <= attempts; n++ {
		if err := ctx.Err(); err != nil {
			return nil, keep(err)
		}
		retryAfter, err := st.attempt(ctx, client, rawURL, pol, opt)
		if err == nil {
			if st.notModified {
				return &FetchedMedia{URL: u, NotModified: true, ETag: firstNonEmpty(st.etag, opt.IfNoneMatch), LastModified: firstNonEmpty(st.lastMod, opt.IfModifiedSince)}, nil
			}
			detected := detectImageType(st.data)
			if !pol.typeAllowed(detected) {
				return nil, fmt.Errorf("%w: %s", ErrMediaType, detected)
			}
			return &FetchedMedia{URL: u, Data: st.data, Type: detected, SuggestedName: suggestName(u, detected), ETag: st.etag, LastModified: st.lastMod, ResumedBytes: st.resumedBytes}, nil
		}
		if !retryable(ctx, err) {
			return nil, keep(err)
		}
		lastErr = err
		if n == attempts {
			break
		}
		wait := backoff(pol.baseBackoff(), n)
		if retryAfter > 0 {
			wait = retryAfter
		}
		if serr := sleep(ctx, wait); serr != nil {
			return nil, keep(serr)
		}
	}
	return nil, keep(fmt.Errorf("tras %d intentos: %w", attempts, lastErr))
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// download es el estado de una descarga que puede reanudarse entre intentos.
type download struct {
	data         []byte
	etag         string
	lastMod      string
	notModified  bool
	resumedBytes int64
	partial      bool // data es un cuerpo incompleto que se puede continuar
}

// ifRange devuelve el validador fuerte para If-Range ("" si no hay: no se puede reanudar).
func (d *download) ifRange() string {
	if d.etag != "" && !strings.HasPrefix(d.etag, "W/") {
		return d.etag
	}
	return d.lastMod
}

func (d *download) reset() { d.data, d.partial = nil, false }

// attempt hace una petición. Con un cuerpo parcial y validador fuerte pide el resto.
func (d *download) attempt(ctx context.Context, client *http.Client, rawURL string, pol MediaPolicy, opt FetchOptions) (retryAfter time.Duration, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, fmt.Errorf("URL inválida: %w", err)
	}
	// Sin compresión de transporte: un Range sobre bytes ya descomprimidos no coincidiría con el servidor.
	req.Header.Set("Accept-Encoding", "identity")
	resuming := d.partial && len(d.data) > 0 && d.ifRange() != ""
	if d.partial && !resuming {
		d.reset()
	}
	switch {
	case resuming:
		req.Header.Set("Range", "bytes="+strconv.Itoa(len(d.data))+"-")
		req.Header.Set("If-Range", d.ifRange())
	case opt.IfNoneMatch != "" || opt.IfModifiedSince != "":
		if opt.IfNoneMatch != "" {
			req.Header.Set("If-None-Match", opt.IfNoneMatch)
		} else {
			req.Header.Set("If-Modified-Since", opt.IfModifiedSince)
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("no se pudo descargar: %w", err)
	}
	defer resp.Body.Close()

	max := pol.maxBytes()
	switch {
	case resp.StatusCode == http.StatusNotModified && !resuming:
		d.notModified = true
		d.etag, d.lastMod = resp.Header.Get("ETag"), resp.Header.Get("Last-Modified")
		return 0, nil
	case resp.StatusCode == http.StatusRequestedRangeNotSatisfiable:
		d.reset() // se reintenta desde cero: no es un error definitivo
		return 0, fmt.Errorf("no se pudo descargar: el servidor rechazó el rango pedido")
	case resp.StatusCode == http.StatusPartialContent && resuming:
		start, ok := contentRangeStart(resp.Header.Get("Content-Range"))
		if !ok || start != int64(len(d.data)) || resp.Header.Get("Content-Encoding") != "" {
			d.reset() // el servidor no continuó donde se esperaba: nunca se mezclan bytes de dos respuestas
			return 0, fmt.Errorf("no se pudo descargar: rango inesperado %q", resp.Header.Get("Content-Range"))
		}
		if int64(len(d.data))+resp.ContentLength > max && resp.ContentLength > 0 {
			return 0, fmt.Errorf("%w (máximo %d)", ErrMediaTooLarge, max)
		}
		before := int64(len(d.data))
		if err := d.read(resp.Body, max); err != nil {
			return 0, err
		}
		d.resumedBytes += before
		return 0, nil
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return parseRetryAfter(resp.Header.Get("Retry-After")), ErrMediaHTTP{Status: resp.StatusCode}
	}

	// 200: cuerpo completo (también cuando se pidió un Range y el servidor no lo respetó).
	d.reset()
	d.etag, d.lastMod = resp.Header.Get("ETag"), resp.Header.Get("Last-Modified")
	if resp.ContentLength > max {
		return 0, fmt.Errorf("%w (%d bytes, máximo %d)", ErrMediaTooLarge, resp.ContentLength, max)
	}
	return 0, d.read(resp.Body, max)
}

// read agrega el cuerpo a data sin pasar de max. Si la lectura se corta, lo leído
// se conserva como parcial (si hay validador para continuarlo).
func (d *download) read(body io.Reader, max int64) error {
	room := max - int64(len(d.data))
	chunk, err := io.ReadAll(io.LimitReader(body, room+1))
	d.data = append(d.data, chunk...)
	if int64(len(d.data)) > max {
		d.reset()
		return fmt.Errorf("%w (máximo %d bytes)", ErrMediaTooLarge, max)
	}
	if err != nil {
		d.partial = len(d.data) > 0 && d.ifRange() != ""
		if !d.partial {
			d.reset()
		}
		return fmt.Errorf("no se pudo descargar: %w", err)
	}
	d.partial = false
	return nil
}

// contentRangeStart lee el inicio de "bytes 100-199/200".
func contentRangeStart(v string) (int64, bool) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(v), "bytes ")
	if !ok {
		return 0, false
	}
	dash := strings.IndexByte(rest, '-')
	if dash <= 0 {
		return 0, false
	}
	n, err := strconv.ParseInt(rest[:dash], 10, 64)
	return n, err == nil
}
