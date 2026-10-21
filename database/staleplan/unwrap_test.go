package staleplan

import "testing"

func TestPoolUnwrapReturnsInner(t *testing.T) {
	p := &pool{}
	if p.UnwrapConnPool() != p.inner {
		t.Fatal("pool.UnwrapConnPool debe devolver inner")
	}
}

// Lo que corre por el pool desenvuelto es invisible al txPool: la transacción
// deja de ser reiniciable para que un 0A000 posterior no la reinicie y pierda
// ese trabajo.
func TestTxPoolUnwrapStopsReplay(t *testing.T) {
	tp := &txPool{replayable: true}
	if got := tp.UnwrapConnPool(); got != tp.inner {
		t.Fatal("txPool.UnwrapConnPool debe devolver inner")
	}
	if tp.replayable {
		t.Fatal("tras desenvolver, la transacción no debe ser reiniciable")
	}
	if _, restartable := tp.begin("SELECT 1", nil, false); restartable {
		t.Fatal("begin no debe permitir reinicio tras desenvolver")
	}
}
