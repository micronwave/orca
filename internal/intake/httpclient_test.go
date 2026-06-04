package intake

import (
	"net/http"
	"testing"
	"time"
)

func TestFetcherHTTPClient_DefaultTimeout(t *testing.T) {
	f := &Fetcher{}

	client := f.httpClient()
	if client == nil {
		t.Fatal("httpClient returned nil")
	}
	if client == http.DefaultClient {
		t.Fatal("httpClient reused http.DefaultClient; want dedicated timeout client")
	}
	if client.Timeout != 30*time.Second {
		t.Fatalf("httpClient timeout = %v, want %v", client.Timeout, 30*time.Second)
	}
}

func TestFetcherHTTPClient_UsesConfiguredClient(t *testing.T) {
	custom := &http.Client{Timeout: 5 * time.Second}
	f := &Fetcher{Client: custom}

	if got := f.httpClient(); got != custom {
		t.Fatalf("httpClient returned %p, want configured client %p", got, custom)
	}
}
