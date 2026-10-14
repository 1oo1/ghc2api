package copilot

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testDevice(w http.ResponseWriter, host string) {
	if host == "" {
		host = "github.com"
	}
	testJSON(w, map[string]any{
		"device_code": "private-device-code", "user_code": "ABCD-EFGH",
		"verification_uri": "https://" + host + "/login/device",
		"expires_in":       30, "interval": 1,
	})
}

func TestLoginDeviceFlowSavesValidatedCredential(t *testing.T) {
	for _, host := range []string{"", "acme.ghe.com"} {
		t.Run(host, func(t *testing.T) {
			var polls atomic.Int32
			var events []string
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				events = append(events, r.URL.Path)
				for _, header := range []string{"Editor-Version", "Editor-Plugin-Version", "Copilot-Integration-Id"} {
					if got := r.Header.Get(header); got != "" {
						t.Errorf("login leaked editor header %s = %q", header, got)
					}
				}
				if r.Header.Get("Accept") != "application/json" {
					t.Errorf("login Accept = %q", r.Header.Get("Accept"))
				}
				wantHost := "github.com"
				wantAPI := "api.github.com"
				if host != "" {
					wantHost, wantAPI = host, "api."+host
				}
				switch r.URL.Path {
				case "/login/device/code":
					if r.Header.Get("Test-Original-Host") != wantHost {
						t.Errorf("device host = %q", r.Header.Get("Test-Original-Host"))
					}
					if err := r.ParseForm(); err != nil {
						t.Error(err)
					}
					if r.Method != "POST" || r.Form.Get("client_id") != "Iv1.b507a08c87ecfe98" || r.Form.Get("scope") != "read:user" || r.Header.Get("Authorization") != "" {
						t.Errorf("device form/headers = %v %v", r.Form, r.Header)
					}
					testDevice(w, host)
				case "/login/oauth/access_token":
					_ = r.ParseForm()
					if r.Form.Get("device_code") != "private-device-code" || r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:device_code" || r.Form.Get("client_id") != "Iv1.b507a08c87ecfe98" {
						t.Errorf("token form = %v", r.Form)
					}
					if polls.Add(1) == 1 {
						testJSON(w, map[string]string{"error": "authorization_pending"})
					} else {
						testJSON(w, map[string]string{"access_token": "new-oauth-secret"})
					}
				case "/user", "/copilot_internal/user":
					if r.Header.Get("Test-Original-Host") != wantAPI || r.Header.Get("Authorization") != "token new-oauth-secret" {
						t.Errorf("validation host/header = %v", r.Header)
					}
					if r.URL.Path == "/user" {
						testJSON(w, map[string]string{"login": "alice"})
					} else {
						testJSON(w, map[string]string{"copilot_plan": "individual"})
					}
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
					w.WriteHeader(500)
				}
			})
			path := filepath.Join(t.TempDir(), "credential.json")
			var output bytes.Buffer
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := Login(ctx, host, path, client, &output); err != nil {
				t.Fatal(err)
			}
			c, err := LoadCredential(path)
			if err != nil || c.Token != "new-oauth-secret" || c.User != "alice" || c.Host != host {
				t.Fatalf("saved credential = %+v, %v", c, err)
			}
			if !strings.Contains(output.String(), "ABCD-EFGH") || !strings.Contains(output.String(), "/login/device") || strings.Contains(output.String(), "new-oauth-secret") {
				t.Fatalf("login user output = %q", output.String())
			}
			want := []string{"/login/device/code", "/login/oauth/access_token", "/login/oauth/access_token", "/user", "/copilot_internal/user"}
			if !reflect.DeepEqual(events, want) {
				t.Fatalf("login order = %v, want %v", events, want)
			}
		})
	}
}

func TestLoginFailureNeverWritesCredential(t *testing.T) {
	for _, failure := range []string{"access_denied", "expired_token", "no-subscription", "invalid-user"} {
		t.Run(failure, func(t *testing.T) {
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/login/device/code":
					testDevice(w, "")
				case "/login/oauth/access_token":
					if failure == "access_denied" || failure == "expired_token" {
						testJSON(w, map[string]string{"error": failure})
					} else {
						testJSON(w, map[string]string{"access_token": "secret"})
					}
				case "/user":
					if failure == "invalid-user" {
						w.WriteHeader(401)
					} else {
						testJSON(w, map[string]string{"login": "alice"})
					}
				case "/copilot_internal/user":
					w.WriteHeader(403)
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
				}
			})
			path := filepath.Join(t.TempDir(), "credential.json")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := Login(ctx, "", path, client, io.Discard); err == nil {
				t.Fatal("login should fail")
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed login wrote credential, stat error = %v", err)
			}
		})
	}
}

func TestLoginInvalidHostAndCancellationMakeNoRequests(t *testing.T) {
	var calls atomic.Int32
	client := &http.Client{Transport: testTransport(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("should not be called")
	})}
	path := filepath.Join(t.TempDir(), "credential.json")
	if err := Login(context.Background(), "evil.example", path, client, io.Discard); err == nil {
		t.Fatal("invalid host accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Login(ctx, "", path, client, io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled login error = %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("invalid/cancelled login requests = %d", calls.Load())
	}
}

func TestLoginStopsAfterFiveConsecutivePollFailures(t *testing.T) {
	for _, failure := range []string{"status", "json"} {
		t.Run(failure, func(t *testing.T) {
			var polls atomic.Int32
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/login/device/code" {
					testDevice(w, "")
					return
				}
				if r.URL.Path != "/login/oauth/access_token" {
					t.Errorf("unexpected path %s", r.URL.Path)
				}
				polls.Add(1)
				if failure == "status" {
					w.WriteHeader(503)
				} else {
					_, _ = io.WriteString(w, "not JSON")
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			path := filepath.Join(t.TempDir(), "credential.json")
			err := Login(ctx, "", path, client, io.Discard)
			if err == nil || errors.Is(err, context.DeadlineExceeded) || polls.Load() != 5 {
				t.Fatalf("poll failure = %v; polls=%d", err, polls.Load())
			}
		})
	}
}

func TestLoginSlowDownAndExpiry(t *testing.T) {
	var polls atomic.Int32
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login/device/code":
			testJSON(w, map[string]any{"device_code": "d", "user_code": "U", "expires_in": 2, "interval": 1})
		case "/login/oauth/access_token":
			polls.Add(1)
			testJSON(w, map[string]string{"error": "slow_down"})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "credential.json")
	started := time.Now()
	err := Login(ctx, "", path, client, io.Discard)
	if err == nil || polls.Load() != 1 || time.Since(started) > 3*time.Second {
		t.Fatalf("slow_down expiry = %v; polls=%d elapsed=%s", err, polls.Load(), time.Since(started))
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired flow wrote credential: %v", err)
	}
}

func TestLoginRejectsRedirect(t *testing.T) {
	var leaked atomic.Int32
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Test-Original-Host") != "github.com" {
			leaked.Add(1)
			_, _ = fmt.Fprintln(w, `{}`)
			return
		}
		http.Redirect(w, r, "https://evil.example/device", 302)
	})
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return nil }
	if err := Login(context.Background(), "", filepath.Join(t.TempDir(), "credential.json"), client, io.Discard); err == nil {
		t.Fatal("redirected device flow accepted")
	}
	if leaked.Load() != 0 {
		t.Fatal("login redirect followed")
	}
}
