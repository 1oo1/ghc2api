package copilot

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAcceptTermsDoesNotReuseStalePersistentModelCache(t *testing.T) {
	enabledModels := strings.Replace(
		testModels,
		`"policy":{"state":"disabled","terms":"Accept terms"}`,
		`"policy":{"state":"enabled"}`,
		1,
	)
	if enabledModels == testModels {
		t.Fatal("enabled model fixture was not updated")
	}

	var modelCalls, acceptCalls atomic.Int32
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models":
			switch modelCalls.Add(1) {
			case 1:
				_, _ = io.WriteString(w, testModels)
			case 2:
				_, _ = io.WriteString(w, enabledModels)
			default:
				t.Errorf("unexpected extra model request")
				w.WriteHeader(http.StatusInternalServerError)
			}
		case "/models/terms-model/policy":
			acceptCalls.Add(1)
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read accept-terms request: %v", err)
			}
			if r.Method != http.MethodPost || !bytes.Equal(bytes.TrimSpace(body), []byte(`{"state":"enabled"}`)) {
				t.Errorf("accept-terms request = %s %s", r.Method, body)
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	config := Config{
		Token: "github-secret", CacheDir: t.TempDir(), ModelsCacheTTL: time.Hour, Client: client,
	}
	current := session{Token: "session-secret", API: "https://api.githubcopilot.com"}

	firstService := testService(t, config)
	initial, err := firstService.getModels(context.Background(), current)
	if err != nil {
		t.Fatalf("initial getModels: %v", err)
	}
	if !initial.Waiting["terms-model"] {
		t.Fatal("terms-model should require accepting terms before acceptance")
	}
	if _, err := os.Stat(firstService.cacheFile("models")); err != nil {
		t.Fatalf("initial model cache was not persisted: %v", err)
	}

	firstService.acceptTerms(context.Background(), current, "terms-model")
	if got := acceptCalls.Load(); got != 1 {
		t.Fatalf("accept-terms calls = %d, want 1", got)
	}

	secondService := testService(t, config)
	refreshed, err := secondService.getModels(context.Background(), current)
	if err != nil {
		t.Fatalf("getModels after accepting terms: %v", err)
	}
	if got := modelCalls.Load(); got != 2 {
		t.Fatalf("model upstream calls = %d, want a refresh after accepting terms", got)
	}

	picked := false
	for _, id := range refreshed.Picks {
		if id == "terms-model" {
			picked = true
			break
		}
	}
	if !picked {
		t.Fatalf("terms-model missing from refreshed Picks: %v", refreshed.Picks)
	}
	if refreshed.Waiting["terms-model"] {
		t.Fatal("terms-model is still waiting for terms after the refreshed enabled model list")
	}
}
