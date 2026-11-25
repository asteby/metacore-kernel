package contract

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestChannelFixturesFields(t *testing.T) {
	var ev DeskIngestEvent
	if err := json.Unmarshal(readFixture(t, "desk_case_opened_channel.json"), &ev); err != nil {
		t.Fatal(err)
	}
	c := ev.Case
	if c.CaseID == "" || c.Channel == nil || c.Requester == nil || c.InstanceContext == nil {
		t.Fatalf("channel fields missing: %+v", c)
	}
	if c.Channel.Kind != ChannelKindOrg || !strings.HasPrefix(c.Channel.PublicID, "sch_") {
		t.Errorf("channel: %+v", c.Channel)
	}
	if c.Requester.Kind != RequesterVisitor || !c.Requester.Verified || c.Requester.Email == "" {
		t.Errorf("requester: %+v", c.Requester)
	}
	if c.InstanceContext.Plan == "" || c.InstanceContext.LastHeartbeat == "" {
		t.Errorf("instance context: %+v", c.InstanceContext)
	}
	if _, err := time.Parse(time.RFC3339, c.InstanceContext.LastHeartbeat); err != nil {
		t.Error(err)
	}
}

// A v2 receiver that predates channels decodes the new fixtures, ignoring
// the fields it does not know; the current types accept the old fixtures.
func TestOldReceiverIgnoresChannelFields(t *testing.T) {
	var ev struct {
		Event string `json:"event"`
		Case  struct {
			Folio           string `json:"folio"`
			InstanceContext *struct {
				InstanceID string `json:"instance_id"`
			} `json:"instance_context"`
		} `json:"case"`
	}
	if err := json.Unmarshal(readFixture(t, "desk_case_opened_channel.json"), &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Event != EventCaseOpened || ev.Case.Folio != "TIENDA-000007" || ev.Case.InstanceContext == nil {
		t.Fatalf("%+v", ev)
	}
	var cur DeskIngestEvent
	if err := json.Unmarshal(readFixture(t, "desk_case_opened.json"), &cur); err != nil {
		t.Fatal(err)
	}
	if cur.Case.Channel != nil || cur.Case.Requester != nil || cur.Case.CaseID != "" {
		t.Fatalf("old fixture grew channel fields: %+v", cur.Case)
	}
	out, _ := json.Marshal(cur.Case)
	for _, k := range []string{`"case_id"`, `"channel"`, `"requester"`} {
		if bytes.Contains(out, []byte(k)) {
			t.Errorf("v2 case re-encoded with %s", k)
		}
	}
}

func TestProjectEventTable(t *testing.T) {
	ts := int64(1764065200)
	at := time.Unix(ts, 0).UTC().Format(time.RFC3339)
	att := []Attachment{{URL: "https://x.example/a.png", Name: "a.png"}}
	cases := []struct {
		name   string
		ev     CallbackEvent
		want   MessageView
		status string
		known  bool
	}{
		{"desk_message", CallbackEvent{Event: EventDeskMessage, Body: "hi", AuthorName: "Sam", MessageID: "dm_1", DeliveryID: "d1", Timestamp: ts, Attachments: att},
			MessageView{ID: "dm_1", AuthorType: AuthorAgent, AuthorName: "Sam", Body: "hi", CreatedAt: at, Attachments: att}, "", true},
		{"issue_commented", CallbackEvent{Event: EventIssueCommented, Comment: "note", DeliveryID: "d2", Timestamp: ts},
			MessageView{ID: "d2", AuthorType: AuthorAgent, Body: "note", CreatedAt: at}, "", true},
		{"ai_action", CallbackEvent{Event: EventAIAction, Body: "done", AuthorName: "Aby", DeliveryID: "d3", Timestamp: ts},
			MessageView{ID: "d3", AuthorType: AuthorAI, AuthorName: "Aby", Body: "done", CreatedAt: at}, "", true},
		{"desk_resolved", CallbackEvent{Event: EventDeskResolved, Solution: "fixed", DeliveryID: "d4", Timestamp: ts},
			MessageView{ID: "d4", AuthorType: AuthorSystem, Body: "fixed", CreatedAt: at}, CaseStatusResolved, true},
		{"issue_closed", CallbackEvent{Event: EventIssueClosed, Solution: "shipped", DeliveryID: "d5", Timestamp: ts},
			MessageView{ID: "d5", AuthorType: AuthorSystem, Body: "shipped", CreatedAt: at}, CaseStatusClosed, true},
		{"desk_reopened", CallbackEvent{Event: EventDeskReopened, DeliveryID: "d6", Timestamp: ts},
			MessageView{ID: "d6", AuthorType: AuthorSystem, CreatedAt: at}, CaseStatusOpen, true},
		{"visitor_erased", CallbackEvent{Event: EventVisitorErased, DeliveryID: "d7", Timestamp: ts}, MessageView{}, "", true},
		{"unknown", CallbackEvent{Event: "future_event", DeliveryID: "d8"}, MessageView{}, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, status, known := ProjectEvent(c.ev)
			if !reflect.DeepEqual(got, c.want) || status != c.status || known != c.known {
				t.Fatalf("got %+v %q %v, want %+v %q %v", got, status, known, c.want, c.status, c.known)
			}
		})
	}
}

// Every callback fixture projects to a known event with a valid message id.
func TestProjectEventFixtures(t *testing.T) {
	for _, f := range []string{"callback_issue_commented.json", "callback_issue_closed.json", "callback_desk_message.json", "callback_desk_resolved.json", "callback_ai_action.json"} {
		var ev CallbackEvent
		if err := json.Unmarshal(readFixture(t, f), &ev); err != nil {
			t.Fatal(err)
		}
		m, _, known := ProjectEvent(ev)
		if !known || !ValidMessageID(m.ID) || m.CreatedAt == "" {
			t.Errorf("%s: %+v known=%v", f, m, known)
		}
	}
}

func TestDeskMessageView(t *testing.T) {
	var ev DeskIngestEvent
	if err := json.Unmarshal(readFixture(t, "desk_message.json"), &ev); err != nil {
		t.Fatal(err)
	}
	v := ev.Message.View()
	if v.ID != "cm_01HZX" || v.AuthorType != AuthorReporter || v.CreatedAt != "2026-11-25T10:05:00Z" {
		t.Fatalf("%+v", v)
	}
}

func TestEmbedSessionClaims(t *testing.T) {
	var c EmbedSessionClaims
	if err := json.Unmarshal(readFixture(t, "embed_session_claims.json"), &c); err != nil {
		t.Fatal(err)
	}
	if EmbedSessionPurpose != "support.embed.session" || IdentityAudience != "support.embed" {
		t.Fatal("purpose constants changed")
	}
	nbf := time.Unix(c.NotBefore, 0)
	for _, tc := range []struct {
		at   time.Time
		want bool
	}{
		{nbf.Add(-time.Second), false}, {nbf, true}, {time.Unix(c.Expires-1, 0), true}, {time.Unix(c.Expires, 0), false},
	} {
		if got := c.ActiveAt(tc.at); got != tc.want {
			t.Errorf("ActiveAt(%v) = %v", tc.at, got)
		}
	}
	if (EmbedSessionClaims{}).ActiveAt(time.Now()) {
		t.Error("zero claims must not be active")
	}
}

func TestPublicProfileValidate(t *testing.T) {
	var ok PublicProfile
	if err := json.Unmarshal(readFixture(t, "public_profile.json"), &ok); err != nil {
		t.Fatal(err)
	}
	if err := ok.Validate(); err != nil {
		t.Fatalf("fixture must validate: %v", err)
	}
	if err := (PublicProfile{}).Validate(); err != nil {
		t.Fatalf("empty profile: %v", err)
	}
	rep := func(n int) string { return strings.Repeat("a", n) }
	cases := []struct {
		name   string
		mut    func(p *PublicProfile)
		field  string
		reason string
	}{
		{"color name", func(p *PublicProfile) { p.BrandColor = "red" }, "brand_color", ProfileReasonInvalid},
		{"color short", func(p *PublicProfile) { p.BrandColor = "#fff" }, "brand_color", ProfileReasonInvalid},
		{"appearance", func(p *PublicProfile) { p.Appearance = "sepia" }, "appearance", ProfileReasonInvalid},
		{"position", func(p *PublicProfile) { p.Position = "top" }, "position", ProfileReasonInvalid},
		{"logo http", func(p *PublicProfile) { p.LogoURL = "http://x.example/l.png" }, "logo_url", ProfileReasonNotHTTPS},
		{"logo js", func(p *PublicProfile) { p.LogoURL = "javascript:alert(1)" }, "logo_url", ProfileReasonInvalid},
		{"logo creds", func(p *PublicProfile) { p.LogoURL = "https://u:p@x.example/l.png" }, "logo_url", ProfileReasonInvalid},
		{"logo no host", func(p *PublicProfile) { p.LogoURL = "https:///l.png" }, "logo_url", ProfileReasonInvalid},
		{"locale", func(p *PublicProfile) { p.DefaultLocale = "es_MX" }, "default_locale", ProfileReasonInvalid},
		{"launcher long", func(p *PublicProfile) { p.Texts = map[string]ProfileTexts{"en": {Launcher: rep(61)}} }, "texts.en.launcher", ProfileReasonTooLong},
		{"greeting long", func(p *PublicProfile) { p.Texts = map[string]ProfileTexts{"en": {Greeting: rep(501)}} }, "texts.en.greeting", ProfileReasonTooLong},
		{"offline long", func(p *PublicProfile) { p.Texts = map[string]ProfileTexts{"en": {Offline: rep(501)}} }, "texts.en.offline", ProfileReasonTooLong},
		{"bad locale key", func(p *PublicProfile) { p.Texts = map[string]ProfileTexts{"EN!": {}} }, "texts.EN!", ProfileReasonInvalid},
		{"contact email", func(p *PublicProfile) { p.FallbackContact = &FallbackContact{Email: "not an email"} }, "fallback_contact.email", ProfileReasonInvalid},
		{"contact url http", func(p *PublicProfile) { p.FallbackContact = &FallbackContact{URL: "http://x.example"} }, "fallback_contact.url", ProfileReasonNotHTTPS},
		{"display mode", func(p *PublicProfile) { p.DisplayMode = "modal" }, "display_mode", ProfileReasonInvalid},
		{"target tag injection", func(p *PublicProfile) { p.InlineTarget = "#a<script>" }, "inline_target", ProfileReasonInvalid},
		{"target quote", func(p *PublicProfile) { p.InlineTarget = `[x="y"]` }, "inline_target", ProfileReasonInvalid},
		{"target js", func(p *PublicProfile) { p.InlineTarget = "JavaScript:alert" }, "inline_target", ProfileReasonInvalid},
		{"target semicolon", func(p *PublicProfile) { p.InlineTarget = "a;b" }, "inline_target", ProfileReasonInvalid},
		{"target backslash", func(p *PublicProfile) { p.InlineTarget = `#a\3c` }, "inline_target", ProfileReasonInvalid},
		{"target long", func(p *PublicProfile) { p.InlineTarget = "#" + rep(ProfileMaxInlineTarget) }, "inline_target", ProfileReasonTooLong},
		{"height low", func(p *PublicProfile) { p.InlineHeight = 319 }, "inline_height", ProfileReasonInvalid},
		{"height high", func(p *PublicProfile) { p.InlineHeight = 1201 }, "inline_height", ProfileReasonInvalid},
		{"height negative", func(p *PublicProfile) { p.InlineHeight = -1 }, "inline_height", ProfileReasonInvalid},
		{"width low", func(p *PublicProfile) { p.PanelWidth = 279 }, "panel_width", ProfileReasonInvalid},
		{"width high", func(p *PublicProfile) { p.PanelWidth = 721 }, "panel_width", ProfileReasonInvalid},
		{"offset high", func(p *PublicProfile) { p.Offset = 121 }, "offset", ProfileReasonInvalid},
		{"offset negative", func(p *PublicProfile) { p.Offset = -1 }, "offset", ProfileReasonInvalid},
		{"shape", func(p *PublicProfile) { p.LauncherShape = "star" }, "launcher_shape", ProfileReasonInvalid},
		{"label long", func(p *PublicProfile) { p.LauncherLabel = rep(61) }, "launcher_label", ProfileReasonTooLong},
		{"too many locales", func(p *PublicProfile) {
			p.Texts = map[string]ProfileTexts{}
			for i := 0; i <= ProfileMaxLocales; i++ {
				p.Texts["l"+string(rune('a'+i%26))+string(rune('a'+i/26))] = ProfileTexts{}
			}
		}, "texts", ProfileReasonTooMany},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := ok
			c.mut(&p)
			errs, isList := p.Validate().(ProfileErrors)
			if !isList {
				t.Fatalf("want ProfileErrors")
			}
			found := false
			for _, e := range errs {
				if e.Field == c.field && e.Reason == c.reason {
					found = true
				}
			}
			if !found {
				t.Fatalf("want %s/%s in %v", c.field, c.reason, errs)
			}
		})
	}
	// Total size limit: ProfileMaxLocales locales at max length exceed 16 KiB.
	big := PublicProfile{Texts: map[string]ProfileTexts{}}
	for i := 0; i < ProfileMaxLocales; i++ {
		big.Texts["l"+string(rune('a'+i))] = ProfileTexts{Launcher: rep(60), Greeting: rep(500), Offline: rep(500)}
	}
	if err := big.Validate(); err == nil || !strings.Contains(err.Error(), "profile: too_long") {
		t.Fatalf("size limit: %v", err)
	}
}

func TestVisitorErasedAndErrors(t *testing.T) {
	if EventVisitorErased != "visitor_erased" || !inList(DeskEvents, EventVisitorErased) || IsCallbackEvent(EventVisitorErased) {
		t.Fatal("visitor_erased is a desk event only")
	}
	var e ErrorResponse
	if err := json.Unmarshal(readFixture(t, "error_channel_inactive.json"), &e); err != nil {
		t.Fatal(err)
	}
	if e.Error.Reason != ReasonChannelOwner || StatusForCode(e.Error.Code) != 403 {
		t.Fatalf("%+v", e)
	}
}

func TestPublicProfileDisplayModes(t *testing.T) {
	for _, name := range []string{"public_profile_inline.json", "public_profile_push.json"} {
		var p PublicProfile
		if err := json.Unmarshal(readFixture(t, name), &p); err != nil {
			t.Fatal(err)
		}
		if err := p.Validate(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	for _, m := range []string{"", DisplayFloating, DisplayInline, DisplayPush} {
		if err := (PublicProfile{DisplayMode: m}).Validate(); err != nil {
			t.Fatalf("mode %q: %v", m, err)
		}
	}
	for _, sel := range []string{"#help", ".a .b > div", "main[data-slot=support]", "div:not(.x), #y"} {
		if err := (PublicProfile{InlineTarget: sel}).Validate(); err != nil {
			t.Fatalf("selector %q: %v", sel, err)
		}
	}
	edge := PublicProfile{InlineHeight: ProfileMaxInlineHeight, PanelWidth: ProfileMinPanelWidth, Offset: ProfileMaxOffset}
	if err := edge.Validate(); err != nil {
		t.Fatal(err)
	}
	// A profile from before the fields existed is valid and re-encodes without
	// any of the new keys.
	var p PublicProfile
	if err := json.Unmarshal(readFixture(t, "public_profile.json"), &p); err != nil {
		t.Fatal(err)
	}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(p)
	for _, k := range []string{"display_mode", "inline_target", "inline_height", "panel_width", "offset", "launcher_shape", "launcher_label"} {
		if strings.Contains(string(out), k) {
			t.Fatalf("old profile re-encoded with %s", k)
		}
	}
}
