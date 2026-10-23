package webhookin

import (
	"encoding/base64"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

func TestBuildPayloadRawBodyAliasUTF8(t *testing.T) {
	body := "{ \"b\": 1,  \"a\":\"<x>\\u00e9\" }\n"
	p := BuildPayload(Route{Key: "k"}, uuid.New(), Inbound{Method: http.MethodPost, Body: []byte(body)})
	if got, ok := p[PayloadRawBody].(string); !ok || got != body {
		t.Fatalf("raw_body = %#v, want exact %q", p[PayloadRawBody], body)
	}
	dec, err := base64.StdEncoding.DecodeString(p[PayloadRawBodyB64].(string))
	if err != nil || string(dec) != body {
		t.Fatalf("raw_body_b64 mismatch: %q %v", dec, err)
	}
}

func TestBuildPayloadRawBodyOmittedWhenNotUTF8(t *testing.T) {
	body := []byte{0xff, 0xfe, 0x00, 0x80, 'a'}
	p := BuildPayload(Route{Key: "k"}, uuid.New(), Inbound{Method: http.MethodPost, Body: body})
	if _, ok := p[PayloadRawBody]; ok {
		t.Fatal("raw_body must be absent for non-UTF-8 body")
	}
	dec, err := base64.StdEncoding.DecodeString(p[PayloadRawBodyB64].(string))
	if err != nil || string(dec) != string(body) {
		t.Fatalf("raw_body_b64 mismatch: %v %v", dec, err)
	}
}
