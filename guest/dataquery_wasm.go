//go:build wasm || wasip1

package guest

// hostDataQuery es el import crudo (docs/wasm-abi.md § 15).
//
//go:wasmimport metacore_host data_query
func hostDataQuery(reqPtr, reqLen uint32) uint64

func callDataQuery(req []byte) []byte {
	ptr, n := bytesRef(req)
	return readPacked(hostDataQuery(ptr, n))
}

// Query lee TODAS las filas de la tabla lógica `table` que cumplen q.Where,
// paginando por cursor (order_by id + id > último, 200 por página) hasta agotar
// o hasta q.MaxRows. Reemplaza el truncado silencioso a 50 filas del limit por
// omisión. Requiere la capability db:read de la tabla.
//
// Es la lectura OBLIGATORIA: si la fuente no existe devuelve *DataQueryError con
// Absent != nil (errors.As); cualquier otro fallo, *DataQueryError con Code del
// host. Si hay más filas que q.MaxRows devuelve las MaxRows primeras y
// ErrTruncated.
func Query(table string, q Q) ([]Row, error) {
	rows, absent, err := queryPages(callDataQuery, table, q, "")
	if err != nil {
		return rows, err
	}
	if absent != nil {
		return nil, &DataQueryError{Code: CodeSourceUnavailable, Message: absent.Message, Absent: absent}
	}
	return rows, nil
}

// QueryOptional es la lectura OPCIONAL: una fuente ausente (addon no instalado
// o desactivado, tabla o columna inexistente) NO es error, sale en
// Optional.Absent con Rows vacío. El error es solo para lo que no es ausencia
// (forbidden, invalid_request, db_error transitorio...), que el autor debe
// tratar como fallo (cerrado en guardias de dinero o stock).
//
// Una ausencia que el host marca degradable:false se devuelve como error
// (*DataQueryError con Absent != nil), nunca como Optional.Absent.
//
// Manda on_absent:"empty"; un host que aún no lo implementa lo ignora y responde
// con el error de siempre (source_unavailable o db_error + SQLSTATE 42P01 /
// 42703), que se convierte en la misma Absence. Un host viejo no distingue
// «addon no instalado»: la razón será table_missing / schema_mismatch.
func QueryOptional(table string, q Q) (Optional, error) {
	return optionalResult(queryPages(callDataQuery, table, q, "empty"))
}
