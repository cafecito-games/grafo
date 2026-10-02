package updatecheck

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestGitHubFeedReadsTheLatestTag(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if accept := request.Header.Get("Accept"); accept != "application/vnd.github+json" {
			t.Errorf("Accept = %q, want the GitHub media type", accept)
		}
		_, _ = writer.Write([]byte(`{"tag_name":"v0.4.2","name":"v0.4.2"}`))
	}))
	defer server.Close()

	tag, err := GitHubFeed{URL: server.URL, Client: server.Client()}.LatestRelease(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tag != "v0.4.2" {
		t.Fatalf("tag = %q, want v0.4.2", tag)
	}
}

func TestGitHubFeedRejectsUnusableResponses(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		status int
		body   string
	}{
		{name: "no releases published", status: http.StatusNotFound, body: `{"message":"Not Found"}`},
		{name: "rate limited", status: http.StatusForbidden, body: `{"message":"API rate limit exceeded"}`},
		{name: "server error", status: http.StatusInternalServerError, body: "boom"},
		{name: "not json", status: http.StatusOK, body: "<!DOCTYPE html>"},
		{name: "missing tag", status: http.StatusOK, body: `{"name":"v0.4.2"}`},
		{name: "unusable tag", status: http.StatusOK, body: `{"tag_name":"latest"}`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.WriteHeader(testCase.status)
				_, _ = writer.Write([]byte(testCase.body))
			}))
			defer server.Close()

			tag, err := GitHubFeed{URL: server.URL, Client: server.Client()}.LatestRelease(context.Background())
			if err == nil {
				t.Fatalf("LatestRelease = %q, want an error", tag)
			}
		})
	}
}

// TestGitHubFeedHonorsCancellation covers the unresponsive endpoint: the lookup
// has to end when its context does rather than hold a command open.
func TestGitHubFeedHonorsCancellation(t *testing.T) {
	released := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		<-released
		writer.WriteHeader(http.StatusOK)
	}))
	// Closing the server waits for the blocked handler, so the handler has to be
	// released first: these two deferrals cannot be reordered.
	defer server.Close()
	defer close(released)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := (GitHubFeed{URL: server.URL, Client: server.Client()}).LatestRelease(ctx); err == nil {
		t.Fatal("LatestRelease returned without an error after its context expired")
	}
}
