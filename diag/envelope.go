package diag

import (
	"bytes"
	"encoding/json"
)

// FromEnvelope interpreta el sobre de error estandar del kernel,
// {"success":false,"error":{"code":"...","message":"..."}}, y devuelve un
// Diagnostic parcial (kind=envelope, code, props sacadas del mensaje). ok es
// false si raw no es un sobre de fallo (incluida una salida cualquiera de un
// addon). No toma ni conserva el mensaje: solo el codigo y rutas de campo.
//
// Un dato nuevo del sobre debe ir DENTRO de "error" (los consumidores leen un
// sobre con claves extra en el nivel superior como una salida normal).
func FromEnvelope(raw []byte) (Diagnostic, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) < 2 || raw[0] != '{' || len(raw) > 4<<20 {
		return Diagnostic{}, false
	}
	// Prefiltro barato: una salida exitosa (la inmensa mayoria) no menciona
	// "success" junto a false; no vale la pena parsearla. La clave se busca sin
	// distinguir mayusculas porque encoding/json tambien las ignora al decodificar
	// ("Success":false es un sobre de fallo); "false" es literal JSON y va en minusculas.
	if !containsFoldKey(raw, `"success"`) || !bytes.Contains(raw, []byte("false")) {
		return Diagnostic{}, false
	}
	var env struct {
		Success *bool           `json:"success"`
		Error   json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || env.Success == nil || *env.Success || len(env.Error) == 0 {
		return Diagnostic{}, false
	}
	d := Diagnostic{Kind: KindEnvelope, Severity: SeverityError, Code: "unknown_error"}
	var obj struct {
		Code    json.RawMessage `json:"code"`
		Message string          `json:"message"`
	}
	if err := json.Unmarshal(env.Error, &obj); err != nil {
		// "error":"texto": una frase no es un codigo; solo rutas.
		var s string
		if json.Unmarshal(env.Error, &s) == nil {
			d.Props = ExtractPaths(s)
		}
		return d, true
	}
	var code string
	if len(obj.Code) > 0 && obj.Code[0] == '"' {
		_ = json.Unmarshal(obj.Code, &code)
	}
	if c := cleanCode(code, nil); c != "" {
		d.Code = c
	}
	d.Props = ExtractPaths(obj.Message)
	return d, true
}

// containsFoldKey busca key (ASCII, en minusculas) en raw sin distinguir
// mayusculas, en una sola pasada y sin copiar.
func containsFoldKey(raw []byte, key string) bool {
	first := key[0] // '"': no tiene mayuscula, ancla la busqueda
	for i := 0; i+len(key) <= len(raw); {
		j := bytes.IndexByte(raw[i:], first)
		if j < 0 || i+j+len(key) > len(raw) {
			return false
		}
		i += j
		if bytes.EqualFold(raw[i:i+len(key)], []byte(key)) {
			return true
		}
		i++
	}
	return false
}
