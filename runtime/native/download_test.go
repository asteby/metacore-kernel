package native

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFetchArtifactVerifiesDigestAfterDownload(t *testing.T) {
	payload := []byte("real native sidecar binary bytes")
	sum := sha256.Sum256(payload)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(payload)
	}))
	defer srv.Close()

	a := Artifact{
		Path:        "backend/native/connector-linux-amd64",
		SBOM:        "backend/native/connector.spdx.json",
		SHA256:      hex.EncodeToString(sum[:]),
		DownloadURL: srv.URL,
	}
	got, err := FetchArtifact(context.Background(), srv.Client(), a)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatal("fetched payload mismatch")
	}
}

func TestFetchArtifactRejectsDigestMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("tampered on the wire"))
	}))
	defer srv.Close()

	a := Artifact{
		Path:        "backend/native/connector-linux-amd64",
		SHA256:      strings.Repeat("a", 64),
		DownloadURL: srv.URL,
	}
	if _, err := FetchArtifact(context.Background(), srv.Client(), a); err == nil {
		t.Fatal("digest mismatch must fail closed")
	}
}

func TestFetchArtifactRejectsOversizedResponse(t *testing.T) {
	old := MaxDownloadBytes
	MaxDownloadBytes = 1024
	defer func() { MaxDownloadBytes = old }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(make([]byte, 4096))
	}))
	defer srv.Close()

	a := Artifact{
		Path:        "backend/native/connector-linux-amd64",
		SHA256:      strings.Repeat("a", 64),
		DownloadURL: srv.URL,
	}
	if _, err := FetchArtifact(context.Background(), srv.Client(), a); err == nil {
		t.Fatal("oversized download must fail closed")
	}
}

func TestFetchArtifactRejectsNonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	a := Artifact{Path: "backend/native/connector-linux-amd64", SHA256: strings.Repeat("a", 64), DownloadURL: srv.URL}
	if _, err := FetchArtifact(context.Background(), srv.Client(), a); err == nil {
		t.Fatal("non-200 status must fail closed")
	}
}

func TestVerifyDownloadedArtifactRequiresDownloadURL(t *testing.T) {
	a := Artifact{SHA256: strings.Repeat("a", 64)}
	if err := VerifyDownloadedArtifact(a, []byte("x")); err == nil {
		t.Fatal("expected error when artifact has no download_url")
	}
}
