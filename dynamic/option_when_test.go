package dynamic

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/manifest"
)

// Forma de pago «99» solo con método PPD, y con PPD solo «99»: la regla vive en
// el `when` de las opciones y el servidor la aplica al guardar, no solo el SDK
// al pintar el select.
func dlPaymentFields() []manifest.FieldDef {
	ppd := &manifest.OptionCondition{Field: "metodo", In: []string{"PPD"}}
	notPPD := &manifest.OptionCondition{Field: "metodo", NotIn: []string{"PPD"}}
	return []manifest.FieldDef{
		{Key: "metodo", Type: "select", Options: []manifest.Option{{Value: "PUE"}, {Value: "PPD"}}},
		{Key: "forma", Type: "select", Options: []manifest.Option{
			{Value: "01", When: notPPD}, {Value: "03", When: notPPD}, {Value: "99", When: ppd},
		}},
	}
}

func TestOptionWhenAllows(t *testing.T) {
	forma := dlPaymentFields()[1]
	vals := func(m map[string]any) func(string) any { return func(k string) any { return m[k] } }
	cases := []struct {
		metodo, forma string
		ok            bool
	}{
		{"PUE", "01", true},
		{"PUE", "99", false},
		{"PPD", "99", true},
		{"PPD", "03", false},
		{"", "99", false}, // sin método no hay PPD
	}
	for _, c := range cases {
		allowed, ok := optionWhenAllows(forma, c.forma, vals(map[string]any{"metodo": c.metodo}))
		if ok != c.ok {
			t.Errorf("metodo=%q forma=%q ok=%v, want %v (allowed %v)", c.metodo, c.forma, ok, c.ok, allowed)
		}
	}
	if allowed, _ := optionWhenAllows(forma, "03", vals(map[string]any{"metodo": "PPD"})); fmt.Sprint(allowed) != "[99]" {
		t.Errorf("allowed with PPD = %v, want [99]", allowed)
	}
	// Fuera de catálogo no es asunto de esta regla (optionAllows ya lo rechaza).
	if _, ok := optionWhenAllows(forma, "zz", vals(map[string]any{"metodo": "PPD"})); !ok {
		t.Error("an off-catalog value must be left to optionAllows")
	}
}

func TestLookupInput_DottedJSONColumn(t *testing.T) {
	flat := map[string]any{"fiscal_data.metodo_pago": "PPD"}
	nested := map[string]any{"fiscal_data": map[string]any{"metodo_pago": "PUE"}}
	text := map[string]any{"fiscal_data": `{"metodo_pago":"PPD"}`}
	for _, c := range []struct {
		in   map[string]any
		want string
	}{{flat, "PPD"}, {nested, "PUE"}, {text, "PPD"}} {
		v, ok := lookupInput(c.in, "fiscal_data.metodo_pago")
		if !ok || v != c.want {
			t.Errorf("lookupInput(%v) = %v,%v want %s", c.in, v, ok, c.want)
		}
	}
	if _, ok := lookupInput(map[string]any{}, "fiscal_data.metodo_pago"); ok {
		t.Error("absent key must not be found")
	}
}

func TestValidateActionPayload_OptionWhen(t *testing.T) {
	svc := &Service{}
	def := &manifest.ActionDef{Key: "stamp", Fields: dlPaymentFields()}
	err := svc.validateActionPayload(def, map[string]any{"metodo": "PUE", "forma": "99"})
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("PUE + 99 accepted: %v", err)
	}
	if got := ve.Fields["forma"]; len(got) != 1 || got[0].Code != codeInvalidOption {
		t.Fatalf("forma issues = %+v", ve.Fields)
	}
	if err := svc.validateActionPayload(def, map[string]any{"metodo": "PPD", "forma": "99"}); err != nil {
		t.Fatalf("PPD + 99 rejected: %v", err)
	}
}

func TestCreateUpdate_DocumentFormOptionWhen(t *testing.T) {
	svc, _ := newDocLinesService(t, true)
	user := newUser(uuid.New())
	ctx := context.Background()
	order, item := seedOrder(t, svc, user)

	bad := invoiceFrom(order, item, 1, "draft")
	bad["metodo"], bad["forma"] = "PPD", "01"
	_, err := svc.Create(ctx, "DlInvoice", user, bad)
	var ve *ValidationError
	if !errors.As(err, &ve) || len(ve.Fields["forma"]) != 1 || ve.Fields["forma"][0].Code != codeInvalidOption {
		t.Fatalf("PPD + 01 on create: err=%v", err)
	}

	good := invoiceFrom(order, item, 1, "draft")
	good["metodo"], good["forma"] = "PPD", "99"
	out, err := svc.Create(ctx, "DlInvoice", user, good)
	if err != nil {
		t.Fatalf("PPD + 99 rejected: %v", err)
	}
	id := uuid.MustParse(fmt.Sprint(out["id"]))

	// PATCH que solo cambia el método: la forma persistida (99) ya no aplica.
	if _, err := svc.Update(ctx, "DlInvoice", user, id, map[string]any{"metodo": "PUE"}); !errors.As(err, &ve) {
		t.Fatalf("PUE over a persisted 99 accepted: %v", err)
	}
	if _, err := svc.Update(ctx, "DlInvoice", user, id, map[string]any{"metodo": "PUE", "forma": "03"}); err != nil {
		t.Fatalf("PUE + 03 rejected: %v", err)
	}
}
