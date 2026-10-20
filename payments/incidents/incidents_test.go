package incidents

import (
	"testing"

	generic "github.com/asteby/metacore-kernel/incidents"
	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestFacadeFixesDomainAndMapsFields(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:pinc?mode=memory&cache=shared"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	s, _ := db.DB()
	s.SetMaxOpenConns(1)
	defer s.Close()
	st, err := New(t.Context(), db, Config{})
	if err != nil {
		t.Fatal(err)
	}
	org := uuid.New()
	nid := uuid.New()
	inc, created, err := st.Open(t.Context(), Input{Org: org, Provider: "mercadopago", Kind: KindDeadLetter, PaymentRef: "123",
		OrderRef: "ord-1", Amount: "10.00", Currency: "MXN", NotificationID: &nid})
	if err != nil || !created {
		t.Fatal(err)
	}
	if inc.Domain != "payments" || inc.Source != "mercadopago" || inc.ResourceType != "payment" || inc.ResourceID != "123" ||
		inc.Fingerprint != "payments.dead_letter:mercadopago" || len(inc.EvidenceIDs) != 1 || inc.EvidenceIDs[0] != nid.String() ||
		string(inc.Meta) != `{"amount":"10.00","currency":"MXN","order_ref":"ord-1"}` {
		t.Fatalf("mapping: %+v", inc)
	}
	// Another domain's row is invisible through the facade.
	other, _, _ := st.Generic().Open(t.Context(), generic.Input{Org: org, Domain: generic.DomainInventory, Kind: "inventory.low"})
	if _, err := st.Get(t.Context(), org, other.ID); err == nil {
		t.Error("facade must not return other domains")
	}
	rows, total, _ := st.List(t.Context(), Filter{Org: org, OpenOnly: true})
	if total != 1 || rows[0].ID != inc.ID {
		t.Errorf("List must be payments only: %+v", rows)
	}
	if n, _ := st.CloseByFingerprint(t.Context(), org, "payments.dead_letter:mercadopago", "ok"); n != 1 {
		t.Error("close by fingerprint")
	}
	if EventOpened != "kernel.payments.incident_opened" {
		t.Errorf("event: %s", EventOpened)
	}
}
