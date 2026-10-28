package batch

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// JSStringifyHash reproduce el content_hash que calculan los productores
// JS/TS/Python de un lote: sha256(JSON.stringify(doc sin las llaves raíz
// dropKeys)), en hex. JSON.stringify
// conserva el orden de llaves del documento, no deja espacios, no escapa
// caracteres no ASCII y formatea los números como JavaScript (1.0 → "1",
// 1e-7 → "1e-7"). Aquí se re-serializa el archivo leído token por token, sin
// pasar por mapas, para conservar ese orden.
func JSStringifyHash(raw []byte, dropKeys ...string) (string, error) {
	drop := map[string]bool{}
	for _, k := range dropKeys {
		drop[k] = true
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var out bytes.Buffer
	if err := jsWriteValue(dec, &out, 0, drop); err != nil {
		return "", err
	}
	if _, err := dec.Token(); err != io.EOF {
		return "", errors.New("contenido sobrante después del documento JSON")
	}
	sum := sha256.Sum256(out.Bytes())
	return hex.EncodeToString(sum[:]), nil
}

func jsWriteValue(dec *json.Decoder, out *bytes.Buffer, depth int, drop map[string]bool) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	return jsWriteToken(dec, out, tok, depth, drop)
}

func jsWriteToken(dec *json.Decoder, out *bytes.Buffer, tok json.Token, depth int, drop map[string]bool) error {
	switch v := tok.(type) {
	case json.Delim:
		switch v {
		case '{':
			out.WriteByte('{')
			first := true
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return err
				}
				key, ok := kt.(string)
				if !ok {
					return errors.New("llave de objeto inválida")
				}
				if depth == 0 && drop[key] {
					// Consumir y descartar el valor.
					var skip json.RawMessage
					if err := dec.Decode(&skip); err != nil {
						return err
					}
					continue
				}
				if !first {
					out.WriteByte(',')
				}
				first = false
				jsWriteString(out, key)
				out.WriteByte(':')
				if err := jsWriteValue(dec, out, depth+1, drop); err != nil {
					return err
				}
			}
			if _, err := dec.Token(); err != nil { // '}'
				return err
			}
			out.WriteByte('}')
		case '[':
			out.WriteByte('[')
			first := true
			for dec.More() {
				if !first {
					out.WriteByte(',')
				}
				first = false
				if err := jsWriteValue(dec, out, depth+1, drop); err != nil {
					return err
				}
			}
			if _, err := dec.Token(); err != nil { // ']'
				return err
			}
			out.WriteByte(']')
		default:
			return fmt.Errorf("delimitador inesperado %q", v)
		}
	case string:
		jsWriteString(out, v)
	case json.Number:
		f, err := strconv.ParseFloat(string(v), 64)
		if err != nil {
			return fmt.Errorf("número inválido %q", v)
		}
		out.WriteString(jsNumber(f))
	case bool:
		if v {
			out.WriteString("true")
		} else {
			out.WriteString("false")
		}
	case nil:
		out.WriteString("null")
	default:
		return fmt.Errorf("token inesperado %T", tok)
	}
	return nil
}

// jsNumber formatea f como Number.prototype.toString de JavaScript.
func jsNumber(f float64) string {
	if f == 0 {
		return "0"
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "null"
	}
	abs := math.Abs(f)
	if abs >= 1e-6 && abs < 1e21 {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	s := strconv.FormatFloat(f, 'e', -1, 64) // 1e-07, 1.5e+21
	mant, exp, _ := strings.Cut(s, "e")
	sign := exp[0]
	digits := strings.TrimLeft(exp[1:], "0")
	if digits == "" {
		digits = "0"
	}
	return mant + "e" + string(sign) + digits
}

// jsWriteString escribe s como JSON.stringify: escapa comillas, barra
// invertida y caracteres de control; el resto va literal.
func jsWriteString(out *bytes.Buffer, s string) {
	out.WriteByte('"')
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == '"':
			out.WriteString(`\"`)
		case r == '\\':
			out.WriteString(`\\`)
		case r == '\b':
			out.WriteString(`\b`)
		case r == '\f':
			out.WriteString(`\f`)
		case r == '\n':
			out.WriteString(`\n`)
		case r == '\r':
			out.WriteString(`\r`)
		case r == '\t':
			out.WriteString(`\t`)
		case r < 0x20:
			fmt.Fprintf(out, `\u%04x`, r)
		default:
			out.WriteString(s[i : i+size])
		}
		i += size
	}
	out.WriteByte('"')
}
