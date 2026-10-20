package legacybridge

import (
	"context"
	"hash/fnv"
	"sync"
)

// keyLocks serializa dentro del proceso el trabajo sobre una misma clave (una
// fila de origen o un cursor). Son franjas fijas: dos claves pueden compartir
// franja y esperarse, pero la memoria no crece con las claves.
type keyLocks struct{ stripes [64]sync.Mutex }

func (l *keyLocks) lock(key string) func() {
	h := fnv.New32a()
	h.Write([]byte(key))
	m := &l.stripes[h.Sum32()%uint32(len(l.stripes))]
	m.Lock()
	return m.Unlock
}

// withKey corre fn en una transacción (atomic) con la clave tomada en el
// proceso y, si store implementa Locker, también entre procesos.
func withKey(ctx context.Context, locks *keyLocks, store any, atomic func(context.Context, func(context.Context) error) error, key string, fn func(context.Context) error) error {
	unlock := locks.lock(key)
	defer unlock()
	return atomic(ctx, func(ctx context.Context) error {
		if lk, ok := store.(Locker); ok {
			if err := lk.Lock(ctx, key); err != nil {
				return err
			}
		}
		return fn(ctx)
	})
}

func rowKey(org, system, entity, externalID string) string {
	return "legacybridge.row\x00" + org + "\x00" + system + "\x00" + entity + "\x00" + externalID
}

func cursorKey(org, system, stream string) string {
	return "legacybridge.cursor\x00" + org + "\x00" + system + "\x00" + stream
}
