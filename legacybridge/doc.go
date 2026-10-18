// Package legacybridge es el motor genérico de coexistencia entre un sistema
// legado (el ERP que la empresa ya tiene) y el host (ops). Mientras dura una
// migración por fases, los dos sistemas operan a la vez y cada entidad tiene
// UN solo dueño por alcance; el motor replica hacia el host lo que es del
// legado y manda comandos al legado cuando el usuario actúa en el host sobre
// algo que el legado sigue siendo dueño.
//
// El paquete no sabe nada de ningún legado concreto: lo propio de cada empresa
// es dato (el mapa de propiedad y la traducción de eventos que configura el
// host) más el adaptador que vive en el repo del legado. Sirve igual para un
// Laravel a la medida, CONTPAQi, Aspel o SAP B1.
//
// (El nombre bridge ya lo usa el paquete que monta el kernel dentro de un host;
// por eso este es legacybridge.)
//
// # Entrada: legado → host
//
// El legado empuja lotes de Envelope (un agregado completo por envelope: la
// raíz, sus hijos y sus facetas). Applier.Ingest aplica cada uno así:
//
//  1. Idempotencia: si la versión o la huella ya están en bridge_refs, no hace
//     nada (y si la fila nació en el host y esperaba el eco del legado, solo
//     confirma el mapeo).
//  2. Orden por fila: SourceVersion menor que la guardada se descarta (stale).
//     No hay orden global entre agregados.
//  3. Dependencias: si falta un padre (o la sucursal del envelope no está
//     mapeada) el envelope queda en espera con reintento exponencial; pasado
//     Config.MaxWait va a revisión. Nunca se crean filas fantasma.
//  4. Propiedad: si el dueño del alcance (entidad, sucursal, fase) es el host,
//     es un conflicto y va a revisión (dataload.ReviewStore).
//  5. Escritura: Sink.Upsert recibe el agregado y los ids locales ya resueltos
//     (bridge_refs primero; si no hay, Sink.DeriveID) dentro de la misma
//     transacción que actualiza bridge_refs (Config.Atomic).
//
// El cursor del legado lo guarda el host (CursorStore) y se devuelve en cada
// respuesta: el adaptador de origen no tiene estado.
//
// # Salida: host → legado
//
// Router traduce eventos del host (CanonicalEvent) a Command según el mapa de
// propiedad: un comando hacia el dueño cuando el dueño es el legado, o una
// réplica cuando el dueño es el host y la ruta lo pide. Outbound guarda cada
// comando en bridge_commands con su clave de idempotencia y lo entrega con el
// outbox del kernel:
//
//   - Orden por agregado: un comando solo sale cuando todos los anteriores del
//     mismo agregado se entregaron. Un job de outbox por agregado ("bomba")
//     entrega en orden de Seq.
//   - Reintentos con backoff exponencial propios del comando; un error
//     permanente (4xx de validación) o agotar Config.MaxAttempts / MaxAge lo
//     manda a la cola de muertos (status dead) y bloquea su agregado hasta que
//     alguien lo reintente (RetryDead) o lo descarte (Discard).
//   - Connector entrega el comando; HTTPConnector lo hace por HTTPS con
//     X-Idempotency-Key y firma HMAC-SHA256 (Sign / Verify).
//
// # Propiedad
//
// OwnershipRule asigna dueño (legacy o local) a (entidad, sucursal, fase); "*"
// o vacío es comodín. Gana la regla más específica (entidad > sucursal > fase).
// Sin regla, el dueño es Config.DefaultOwner (legacy: en F0 todo es del legado).
// La fase activa por sistema la guarda OwnershipStore.
//
// # Almacenes
//
// GormStore implementa todos los almacenes sobre las tablas bridge_* (Postgres
// en producción, SQLite en pruebas) y MemoryStore los implementa en memoria.
// Migrate crea las tablas de forma idempotente.
package legacybridge
