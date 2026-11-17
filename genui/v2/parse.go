package genuiv2

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"strconv"
)

// Parse validates untrusted input (JSON text as []byte or string, or an
// already decoded value) as a genui/v2 document and returns a normalised COPY.
// It never returns a document that shares memory with the caller's value.
func Parse(v any, opts Options) (Doc, error) {
	d, err := prepare(v)
	if err != nil {
		return nil, err
	}
	if err := validatePrepared(d, opts); err != nil {
		return nil, err
	}
	return d, nil
}

// Validate reports whether the already-decoded document is a valid genui/v2 spec.
func Validate(v any, opts Options) error {
	_, err := Parse(v, opts)
	return err
}

func validatePrepared(d obj, opts Options) (err error) {
	loadSchema()
	defer func() {
		if r := recover(); r != nil {
			if pe, ok := r.(*Error); ok {
				err = pe
				return
			}
			panic(r)
		}
	}()
	if d["version"] != Version {
		panic(fail(CodeUnsupportedVersion, "version", "version debe ser %s", Version))
	}
	c := &ctx{doc: d, opts: opts, blockIDs: map[string]string{}}
	c.check(d, rootSchema, "$", 1)
	for _, s := range c.submits {
		if t := c.blockIDs[s.form]; t != "form" && t != "wizard" {
			panic(fail(CodeUnknownTarget, s.path, "%s: submit apunta a %s, que no es un bloque form/wizard del documento", s.path, s.form))
		}
	}
	return nil
}

// ---- untrusted input -> safe object (phases documented in testdata/conformance/README.md) ----

func decodeText(b []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fail(CodeInvalidJSON, "", "JSON invalido")
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fail(CodeInvalidJSON, "", "JSON invalido")
	}
	return v, nil
}

func prepare(in any) (obj, error) {
	var v any
	switch t := in.(type) {
	case []byte:
		x, err := fromText(t)
		if err != nil {
			return nil, err
		}
		v = x
	case string:
		x, err := fromText([]byte(t))
		if err != nil {
			return nil, err
		}
		v = x
	default:
		if !isGeneric(in) {
			// A Go struct or typed map: round-trip first (no pre-scan possible).
			raw, err := marshal(in)
			if err != nil {
				return nil, fail(CodeInvalidDocument, "", "el documento no es serializable")
			}
			if len(raw) > MaxBytes {
				return nil, fail(CodeLimitBytes, "", "documento mayor a %d bytes", MaxBytes)
			}
			x, err := decodeText(raw)
			if err != nil {
				return nil, err
			}
			in = x
		}
		if err := scan(in); err != nil {
			return nil, err
		}
		raw, err := marshal(in)
		if err != nil {
			return nil, fail(CodeInvalidDocument, "", "el documento no es serializable")
		}
		if len(raw) > MaxBytes {
			return nil, fail(CodeLimitBytes, "", "documento mayor a %d bytes", MaxBytes)
		}
		x, err := decodeText(raw)
		if err != nil {
			return nil, err
		}
		if err := scan(x); err != nil {
			return nil, err
		}
		v = x
	}
	v = normalize(v)
	o, ok := v.(obj)
	if !ok {
		return nil, fail(CodeInvalidDocument, "", "el documento debe ser un objeto")
	}
	return o, nil
}

func fromText(b []byte) (any, error) {
	if len(b) > MaxBytes {
		return nil, fail(CodeLimitBytes, "", "documento mayor a %d bytes", MaxBytes)
	}
	if err := scanText(b); err != nil {
		return nil, err
	}
	v, err := decodeText(b)
	if err != nil {
		return nil, err
	}
	if err := scan(v); err != nil {
		return nil, err
	}
	return v, nil
}

func isGeneric(v any) bool {
	switch v.(type) {
	case nil, bool, string, float64, json.Number, obj, []any:
		return true
	}
	return false
}

func marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// normalize turns json.Number into float64 (after scan proved it finite).
func normalize(v any) any {
	switch t := v.(type) {
	case json.Number:
		f, _ := strconv.ParseFloat(string(t), 64)
		return f
	case []any:
		for i := range t {
			t[i] = normalize(t[i])
		}
		return t
	case obj:
		for k := range t {
			t[k] = normalize(t[k])
		}
		return t
	}
	return v
}

// scan is the iterative pre-pass: nesting, non-finite numbers, prototype keys.
func scan(root any) error {
	type item struct {
		v any
		d int
	}
	stack := []item{{root, 1}}
	nodes := 0
	for len(stack) > 0 {
		it := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		switch t := it.v.(type) {
		case json.Number:
			f, err := strconv.ParseFloat(string(t), 64)
			if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
				return fail(CodeInvalidJSON, "", "numero fuera de rango")
			}
		case float64:
			if math.IsInf(t, 0) || math.IsNaN(t) {
				return fail(CodeInvalidJSON, "", "numero fuera de rango")
			}
		case []any:
			if it.d > MaxJSONDepth {
				return fail(CodeLimitDepth, "", "anidamiento JSON mayor a %d", MaxJSONDepth)
			}
			if nodes++; nodes > 1_000_000 {
				return fail(CodeLimitBytes, "", "documento demasiado grande")
			}
			for _, x := range t {
				stack = append(stack, item{x, it.d + 1})
			}
		case obj:
			if it.d > MaxJSONDepth {
				return fail(CodeLimitDepth, "", "anidamiento JSON mayor a %d", MaxJSONDepth)
			}
			if nodes++; nodes > 1_000_000 {
				return fail(CodeLimitBytes, "", "documento demasiado grande")
			}
			for k, x := range t {
				if forbidden(k) {
					return fail(CodeForbiddenKey, k, "clave prohibida %s", k)
				}
				stack = append(stack, item{x, it.d + 1})
			}
		}
	}
	return nil
}

// scanText measures JSON nesting without parsing (ignores string contents).
func scanText(b []byte) error {
	depth := 0
	inStr := false
	for i := 0; i < len(b); i++ {
		c := b[i]
		switch {
		case inStr:
			if c == '\\' {
				i++
			} else if c == '"' {
				inStr = false
			}
		case c == '"':
			inStr = true
		case c == '[' || c == '{':
			if depth++; depth > MaxJSONDepth {
				return fail(CodeLimitDepth, "", "anidamiento JSON mayor a %d", MaxJSONDepth)
			}
		case c == ']' || c == '}':
			depth--
		}
	}
	return nil
}
