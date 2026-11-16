package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TEMPORARY: prints token estimates in CI, removed before merge.
func TestZZMeasureCatalogSizes(t *testing.T) {
	crud, org := newCatalogCRUD(t, nil)
	id := &AgentIdentity{OrganizationID: org, UserID: uuid.New()}
	ix := NewCatalogIndex(crud.cfg.Catalog, CatalogOptions{})
	var b strings.Builder
	fmt.Fprintf(&b, "legend=%dtok clause=%dtok\n", EstimateTokens(ERPCardLegend), EstimateTokens(CatalogPromptClause))
	for _, m := range ix.Models() {
		c := RenderCard(m, CardOptions{})
		fmt.Fprintf(&b, "card %s=%dtok/%dB\n", m.Name, EstimateTokens(c), len(c))
	}
	legacy, _ := crud.ListModels(id)
	lj, _ := json.Marshal(map[string]any{"total": len(legacy), "models": legacy})
	list := crud.ListModelsCompact(id, ix)
	fmt.Fprintf(&b, "list_models %dtok/%dB -> %dtok/%dB\n", EstimateTokens(string(lj)), len(lj), EstimateTokens(list), len(list))
	for _, name := range []string{"sales_orders", "customers", "invoices", "stock"} {
		d, _ := crud.DescribeModel(id, name)
		dj, _ := json.Marshal(d)
		card, _ := crud.DescribeModelCard(id, ix, name)
		fmt.Fprintf(&b, "describe %s %dtok/%dB -> %dtok/%dB\n", name, EstimateTokens(string(dj)), len(dj), EstimateTokens(card), len(card))
	}
	for _, q := range []string{"¿cuántos pedidos de venta confirmados hay?", "existencias por sucursal", "pedidos de venta, clientes y facturas"} {
		blk := BuildPrefetchBlock(context.Background(), crud, id, ix, q, 3, 800)
		fmt.Fprintf(&b, "prefetch %q=%dtok\n", q, EstimateTokens(blk))
	}
	qr, _ := crud.Query(id, QueryRequest{Model: "sales_orders", Limit: 20})
	qj, _ := json.Marshal(qr)
	res, _ := crud.QueryTree(context.Background(), id, QueryTree{Model: "sales_orders"}, QueryTreeOptions{Catalog: ix})
	txt := res.(*QueryResult).Text
	fmt.Fprintf(&b, "query 3 rows JSON %dtok/%dB -> table %dtok/%dB\n%s\n", EstimateTokens(string(qj)), len(qj), EstimateTokens(txt), len(txt), txt)
	t.Errorf("MEASURE\n%s", b.String())
}
