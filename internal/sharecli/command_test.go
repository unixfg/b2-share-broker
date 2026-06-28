package sharecli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestUploadHelpersWithTestServers(t *testing.T) {
	var uploadedBody string
	b2Server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Fatalf("method = %s", r.Method)
		}
		if r.Header.Get("Content-Type") != "text/plain; charset=utf-8" {
			t.Fatalf("content-type = %q", r.Header.Get("Content-Type"))
		}
		body, _ := io.ReadAll(r.Body)
		uploadedBody = string(body)
		w.WriteHeader(http.StatusOK)
	}))
	defer b2Server.Close()

	brokerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/uploads":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"uploadUrl":"`+b2Server.URL+`/put","requiredHeaders":{"Content-Type":"text/plain; charset=utf-8"},"uploadToken":"token","publicUrl":"https://public.example.test/key.txt"}`)
		case "/api/uploads/complete":
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"publicUrl":"https://public.example.test/key.txt","verified":true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer brokerServer.Close()

	cfg := commandConfig{
		BrokerURL:  brokerServer.URL,
		HTTPClient: brokerServer.Client(),
	}

	tempFile, err := os.CreateTemp(t.TempDir(), "upload-*.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tempFile.WriteString("hello broker"); err != nil {
		t.Fatal(err)
	}
	if _, err := tempFile.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	defer tempFile.Close()

	upload, err := createUpload(context.Background(), cfg, "access-token", createUploadRequest{
		Filename:    "upload.txt",
		ContentType: "text/plain; charset=utf-8",
		Size:        int64(len("hello broker")),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := putFile(context.Background(), cfg.HTTPClient, upload, tempFile, int64(len("hello broker")), "text/plain; charset=utf-8"); err != nil {
		t.Fatal(err)
	}
	completed, err := completeUpload(context.Background(), cfg, "access-token", upload.UploadToken)
	if err != nil {
		t.Fatal(err)
	}
	if uploadedBody != "hello broker" {
		t.Fatalf("uploaded body = %q", uploadedBody)
	}
	if completed.PublicURL != "https://public.example.test/key.txt" {
		t.Fatalf("public URL = %q", completed.PublicURL)
	}
}

func TestDetectContentType(t *testing.T) {
	tempFile, err := os.CreateTemp(t.TempDir(), "upload-*.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer tempFile.Close()
	if _, err := tempFile.WriteString("hello"); err != nil {
		t.Fatal(err)
	}
	if _, err := tempFile.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	got, err := detectContentType("upload.bin", tempFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "text/plain") {
		t.Fatalf("content type = %q", got)
	}
}
