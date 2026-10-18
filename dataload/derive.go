package dataload

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
	"unicode"
)

// Derive calcula un campo a partir de otras columnas de la misma fila. Es
// declarativo y genérico: no sabe de ningún origen concreto.
//
//	first    primer valor no vacío de Cols (en orden)
//	extract  grupo 1 de Pattern sobre el primer valor no vacío de Cols
//	gtin     primer valor de Cols que sea un GTIN-8/12/13/14 con dígito verificador válido
//
// When, si se indica, es una columna que debe traer dato para que el campo se
// calcule (p. ej. "precio antes" solo si hay "precio rebajado"). El resultado
// vacío deja el campo ausente.
type Derive struct {
	Op      string   `json:"op"`
	Cols    []string `json:"cols"`
	Pattern string   `json:"pattern,omitempty"`
	When    string   `json:"when,omitempty"`
}

const (
	DeriveFirst   = "first"
	DeriveExtract = "extract"
	DeriveGTIN    = "gtin"
)

func (d Derive) validate() error {
	if len(d.Cols) == 0 {
		return fmt.Errorf("derive sin cols")
	}
	switch d.Op {
	case DeriveFirst, DeriveGTIN:
	case DeriveExtract:
		re, err := regexp.Compile(d.Pattern)
		if err != nil {
			return fmt.Errorf("derive.pattern inválido: %v", err)
		}
		if re.NumSubexp() < 1 {
			return fmt.Errorf("derive.pattern necesita un grupo de captura")
		}
	default:
		return fmt.Errorf("derive.op desconocido %q (first, extract, gtin)", d.Op)
	}
	return nil
}

// headers son los encabezados con que el campo entra al importador: los From,
// o el seudo-encabezado que alimenta Derive.
func (f Field) headers() []string {
	if f.Derive != nil {
		return []string{derivedHeader(f.Target)}
	}
	return f.From
}

func derivedHeader(target string) string { return "derived:" + target }

// withDerived devuelve la fila con los valores calculados agregados bajo sus
// seudo-encabezados. No muta la fila original.
func (p Profile) withDerived(raw map[string]any) map[string]any {
	has := false
	for _, f := range p.Fields {
		if f.Derive != nil {
			has = true
			break
		}
	}
	if !has {
		return raw
	}
	byHeader := make(map[string]string, len(raw))
	for h, v := range raw {
		byHeader[normHeader(h)] = applyClean(p.Clean, stringifyCell(v))
	}
	out := make(map[string]any, len(raw)+2)
	for k, v := range raw {
		out[k] = v
	}
	for _, f := range p.Fields {
		if f.Derive == nil {
			continue
		}
		if v := f.Derive.eval(byHeader); v != "" {
			out[derivedHeader(f.Target)] = v
		}
	}
	return out
}

func (d Derive) eval(cells map[string]string) string {
	if d.When != "" && strings.TrimSpace(cells[normHeader(d.When)]) == "" {
		return ""
	}
	for _, c := range d.Cols {
		v := strings.TrimSpace(cells[normHeader(c)])
		if v == "" {
			continue
		}
		switch d.Op {
		case DeriveFirst:
			return v
		case DeriveExtract:
			if m := compileCached(d.Pattern).FindStringSubmatch(v); len(m) > 1 {
				return strings.TrimSpace(m[1])
			}
			return ""
		case DeriveGTIN:
			if IsGTIN(v) {
				return v
			}
		}
	}
	return ""
}

var reCache sync.Map

func compileCached(pat string) *regexp.Regexp {
	if re, ok := reCache.Load(pat); ok {
		return re.(*regexp.Regexp)
	}
	re := regexp.MustCompile(pat)
	reCache.Store(pat, re)
	return re
}

// IsGTIN dice si s es un GTIN-8/12/13/14 (solo dígitos) con dígito verificador GS1 válido.
func IsGTIN(s string) bool {
	switch len(s) {
	case 8, 12, 13, 14:
	default:
		return false
	}
	sum := 0
	for i := len(s) - 2; i >= 0; i-- {
		r := rune(s[i])
		if !unicode.IsDigit(r) || r > '9' {
			return false
		}
		w := 1
		if (len(s)-2-i)%2 == 0 {
			w = 3
		}
		sum += int(r-'0') * w
	}
	last := rune(s[len(s)-1])
	if last < '0' || last > '9' {
		return false
	}
	return (10-sum%10)%10 == int(last-'0')
}

// applySplits convierte en lista los campos con Split.
func (p Profile) applySplits(rec map[string]any) {
	for _, f := range p.Fields {
		if f.Split == "" {
			continue
		}
		v, ok := getPath(rec, f.Target)
		if !ok {
			continue
		}
		s, _ := v.(string)
		var parts []any
		for _, part := range strings.Split(s, f.Split) {
			if part = strings.TrimSpace(part); part != "" {
				parts = append(parts, part)
			}
		}
		if len(parts) == 0 {
			deletePath(rec, f.Target)
			continue
		}
		setRecordPath(rec, f.Target, parts)
	}
}

func setRecordPath(rec map[string]any, path string, v any) {
	parts := strings.Split(path, ".")
	cur := rec
	for _, part := range parts[:len(parts)-1] {
		next, ok := cur[part].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[part] = next
		}
		cur = next
	}
	cur[parts[len(parts)-1]] = v
}

func deletePath(rec map[string]any, path string) {
	parts := strings.Split(path, ".")
	cur := rec
	for _, part := range parts[:len(parts)-1] {
		next, ok := cur[part].(map[string]any)
		if !ok {
			return
		}
		cur = next
	}
	delete(cur, parts[len(parts)-1])
}
