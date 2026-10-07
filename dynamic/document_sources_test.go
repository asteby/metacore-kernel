package dynamic

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/manifest"
)

// «Crear desde» con option_filter: el selector del origen esconde los
// documentos que ya no sirven (una cotización convertida en venta), pero el
// filtro corre en el cliente. El guardado re-evalúa el origen elegido con la
// misma regla, así que un payload editado no puede facturar dos veces la misma
// mercancía por dos vínculos distintos.

func TestDeriveDocumentLines_CarriesSourceOptionFilter(t *testing.T) {
	defs := dlDefs()
	sp := DeriveDocumentLines(defs["DlInvoice"], func(m string) (manifest.ModelDefinition, bool) { d, ok := defs[m]; return d, ok })
	f := sp.Types[0].Sources[0].OptionFilter
	if len(f) != 2 || f[1].Field != "converted_to" || len(f[1].NotIn) != 2 {
		t.Fatalf("option filter = %+v", f)
	}
}

func setOrder(t *testing.T, svc *Service, user *fakeUser, order string, data map[string]any) {
	t.Helper()
	if _, err := svc.Update(context.Background(), "DlOrder", user, uuid.MustParse(order), data); err != nil {
		t.Fatalf("update order: %v", err)
	}
}

func sourceFieldError(t *testing.T, err error) FieldError {
	t.Helper()
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want a 422 ValidationError", err)
	}
	fe := ve.Fields["order_id"]
	if len(fe) != 1 {
		t.Fatalf("field errors = %+v, want one on order_id", ve.Fields)
	}
	return fe[0]
}

func TestCreate_RejectsASourceTheOptionFilterHides(t *testing.T) {
	svc, db := newDocLinesService(t, true)
	user := newUser(uuid.New())
	ctx := context.Background()
	order, item := seedOrder(t, svc, user)

	// Elegible (sin estado ni conversión: las reglas negativas lo dejan pasar).
	if _, err := svc.Create(ctx, "DlInvoice", user, invoiceFrom(order, item, 1, "draft")); err != nil {
		t.Fatalf("eligible source rejected: %v", err)
	}

	// Ya convertida: el selector la esconde y el servidor también la rechaza.
	setOrder(t, svc, user, order, map[string]any{"converted_to": "order"})
	in := invoiceFrom(order, item, 1, "draft")
	_, err := svc.Create(ctx, "DlInvoice", user, in)
	fe := sourceFieldError(t, err)
	if fe.Code != "source_not_eligible" || !strings.Contains(fe.Message, "«Venta»") {
		t.Fatalf("field error = %+v", fe)
	}
	var n int64
	db.Table("dl_invoices").Where("id = ?", in["id"]).Count(&n)
	if n != 0 {
		t.Fatal("an invoice from a hidden source must not be inserted")
	}

	// Cancelada (mayúsculas/espacios: misma comparación que el SDK).
	setOrder(t, svc, user, order, map[string]any{"converted_to": "", "status": " Cancelled "})
	if fe := sourceFieldError(t, func() error {
		_, err := svc.Create(ctx, "DlInvoice", user, invoiceFrom(order, item, 1, "draft"))
		return err
	}()); fe.Code != "source_not_eligible" {
		t.Fatalf("cancelled source: %+v", fe)
	}

	// Un id que no existe en la organización.
	if fe := sourceFieldError(t, func() error {
		_, err := svc.Create(ctx, "DlInvoice", user, invoiceFrom(uuid.NewString(), item, 1, "draft"))
		return err
	}()); fe.Code != "source_not_found" {
		t.Fatalf("missing source: %+v", fe)
	}
	// La venta de otra organización tampoco cuenta como origen.
	if fe := sourceFieldError(t, func() error {
		_, err := svc.Create(ctx, "DlInvoice", newUser(uuid.New()), invoiceFrom(order, item, 1, "draft"))
		return err
	}()); fe.Code != "source_not_found" {
		t.Fatalf("cross-org source: %+v", fe)
	}
}

// Una factura nacida de un origen elegible sigue editándose aunque el origen
// deje de serlo después; cambiarla a otro origen escondido sí se rechaza.
func TestUpdate_ChecksOnlyAChangedSource(t *testing.T) {
	svc, _ := newDocLinesService(t, true)
	user := newUser(uuid.New())
	ctx := context.Background()
	order, item := seedOrder(t, svc, user)
	out, err := svc.Create(ctx, "DlInvoice", user, invoiceFrom(order, item, 1, "draft"))
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.MustParse(fmt.Sprint(out["id"]))
	setOrder(t, svc, user, order, map[string]any{"converted_to": "layaway"})

	upd := invoiceFrom(order, item, 2, "draft")
	delete(upd, "id")
	if _, err := svc.Update(ctx, "DlInvoice", user, id, upd); err != nil {
		t.Fatalf("editing an invoice whose source stopped matching later: %v", err)
	}

	other, otherItem := seedOrder(t, svc, user)
	setOrder(t, svc, user, other, map[string]any{"converted_to": "order"})
	upd = invoiceFrom(other, otherItem, 1, "draft")
	delete(upd, "id")
	_, err = svc.Update(ctx, "DlInvoice", user, id, upd)
	if fe := sourceFieldError(t, err); fe.Code != "source_not_eligible" {
		t.Fatalf("switching to a hidden source: %+v", fe)
	}
}

// El selector del origen es un listado propio del modelo (self-options): su
// columna de ciclo de vida viaja en cada opción, así una regla positiva del
// option_filter (`in` / `equals`) no deja el selector vacío.
func TestSelfOptions_CarryTheLifecycleColumn(t *testing.T) {
	svc, _ := newDocLinesService(t, true)
	svc.optsResolver = noOptionsConfig()
	svc.selfOptions = true
	user := newUser(uuid.New())
	order, _ := seedOrder(t, svc, user)
	setOrder(t, svc, user, order, map[string]any{"status": "sent"})

	res, err := svc.Options(context.Background(), user, OptionsQuery{Model: "DlOrder", Field: "id"})
	if err != nil {
		t.Fatalf("options: %v", err)
	}
	if len(res.Options) != 1 || res.Options[0].Extra["status"] != "sent" {
		t.Fatalf("options = %+v, want status=sent on the option", res.Options)
	}
	if _, leaked := res.Options[0].Extra["converted_to"]; leaked {
		t.Fatal("only the lifecycle column rides a self-listed option")
	}
}
