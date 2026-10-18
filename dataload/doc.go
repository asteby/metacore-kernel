// Package dataload es el motor genérico de carga masiva de datos: toma una
// fuente (CSV, JSON o un bundle), la lee con un Profile declarativo, calcula un
// plan (dry-run y diff), lo aplica de forma idempotente y asíncrona dejando un
// historial de corridas, y respalda los medios descargados como originales
// inmutables direccionados por sha256 con un manifiesto verificable.
//
// El paquete no conoce ningún giro, plataforma de origen ni esquema de
// persistencia. Lo específico de un giro o de una plataforma de origen vive en
// datos: un Profile JSON y los Overrides de cada organización. La persistencia la cablean ops/addons implementando las
// interfaces Target, Ledger, MediaJobStore, Manifest, BackupStore y MediaSink;
// las implementaciones en memoria (NewMemory*) sirven para pruebas y desarrollo.
//
// Piezas:
//
//	profile.go   Profile, Field, Overrides: mapeo, limpieza y transformaciones
//	source.go    Source y adaptadores CSV / JSON / bundle
//	plan.go      Plan, dry-run y diff contra el destino
//	ledger.go    Run, Ledger y la llave de idempotencia
//	runner.go    ejecución asíncrona con estado y progreso
//	media.go     cola de medios, respaldo por sha256, manifiesto y verificación
//	memory.go    implementaciones en memoria de las interfaces de persistencia
package dataload
