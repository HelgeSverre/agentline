package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCreateAndClaimUseRelayShapes(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/rooms", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["name"] != "room" || body["creator_name"] != "alice" || body["ttl_seconds"] != float64(3600) {
			t.Fatalf("body = %#v", body)
		}
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"room":{"id":"r1","name":"room"},"participant":{"id":"p1"},"participant_token":"secret","invite_token":"invite","invite_url":"https://relay.test/join/invite"}`))
	})
	mux.HandleFunc("POST /api/invites/invite/claim", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"room":{"id":"r1","name":"room"},"participant":{"id":"p2"},"participant_token":"joined"}`))
	})
	s := httptest.NewServer(mux)
	defer s.Close()
	c := New(s.URL, "", s.Client())
	created, err := c.CreateRoom(context.Background(), "room", "alice", time.Hour, nil)
	if err != nil || created.ParticipantToken != "secret" || created.InviteURL == "" {
		t.Fatalf("created=%+v err=%v", created, err)
	}
	claimed, err := c.ClaimInvite(context.Background(), s.URL+"/join/invite", "bob")
	if err != nil || claimed.ParticipantToken != "joined" {
		t.Fatalf("claimed=%+v err=%v", claimed, err)
	}
}

func TestSendGeneratesOneIDAndRetriesServerError(t *testing.T) {
	var attempts atomic.Int32
	var firstID string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Fatal("missing bearer")
		}
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if firstID == "" {
			firstID = body["id"]
		}
		if body["id"] == "" || body["id"] != firstID {
			t.Fatalf("ids changed: %#v", body)
		}
		if attempts.Add(1) == 1 {
			http.Error(w, "oops", 500)
			return
		}
		w.Write([]byte(`{"id":"` + firstID + `","room_id":"r1","sequence":1,"kind":"message"}`))
	}))
	defer s.Close()
	c := New(s.URL, "token", s.Client())
	c.Backoff = time.Millisecond
	message, err := c.Send(context.Background(), "r1", "", "hello", "", "")
	if err != nil || message.ID != firstID || attempts.Load() != 2 {
		t.Fatalf("message=%+v attempts=%d err=%v", message, attempts.Load(), err)
	}
}

func TestErrorIsTypedAndSafe(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"code":"unauthorized","message":"A valid participant credential is required."}}`))
	}))
	defer s.Close()
	_, err := New(s.URL, "super-secret", s.Client()).Room(context.Background(), "room")
	api, ok := err.(*Error)
	if !ok || api.Code != "unauthorized" || strings.Contains(err.Error(), "super-secret") {
		t.Fatalf("error = %#v", err)
	}
}

func TestInviteTokenRejectsWrongPath(t *testing.T) {
	invalid := []string{
		"https://relay.test/not-join/token",
		"ftp://relay.test/join/token",
		"https:///join/token",
		"https://user@relay.test/join/token",
		"https://relay.test/join/token/extra",
		"https://relay.test/join/token?x=1",
		"https://relay.test/join/token#fragment",
		"/join/token",
	}
	for _, invite := range invalid {
		if _, err := InviteToken(invite); err == nil {
			t.Errorf("InviteToken(%q) succeeded", invite)
		}
	}
}

func TestValidateOrigin(t *testing.T) {
	for _, origin := range []string{"https://relay.test", "http://127.0.0.1:8080", "https://[2001:db8::1]", "https://[2001:db8::1]:8443"} {
		if err := ValidateOrigin(origin); err != nil {
			t.Errorf("ValidateOrigin(%q): %v", origin, err)
		}
	}
	for _, origin := range []string{"relay.test", "ftp://relay.test", "https:///missing", "https://user@relay.test", "https://relay.test/path", "https://relay.test?x=1", "https://relay.test/#x", "https://relay.test:", "https://relay.test:0", "https://relay.test:65536", "https://relay.test:abc"} {
		if err := ValidateOrigin(origin); err == nil {
			t.Errorf("ValidateOrigin(%q) succeeded", origin)
		}
	}
}

func TestInviteTokenRejectsMalformedAndOutOfRangePorts(t *testing.T) {
	for _, invite := range []string{"https://relay.test:/join/token", "https://relay.test:0/join/token", "https://relay.test:65536/join/token", "https://relay.test:abc/join/token"} {
		if _, err := InviteToken(invite); err == nil {
			t.Errorf("InviteToken(%q) succeeded", invite)
		}
	}
}

func TestInviteTokenAcceptsBracketedIPv6Origins(t *testing.T) {
	for _, invite := range []string{"https://[2001:db8::1]/join/token", "https://[2001:db8::1]:8443/join/token"} {
		token, err := InviteToken(invite)
		if err != nil || token != "token" {
			t.Errorf("InviteToken(%q) = %q, %v", invite, token, err)
		}
	}
}

func TestWaitDoesNotRetryLongPoll(t *testing.T) {
	var attempts atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		http.Error(w, "temporary", http.StatusServiceUnavailable)
	}))
	defer s.Close()
	c := New(s.URL, "token", s.Client())
	c.Backoff = time.Millisecond
	_, err := c.Wait(context.Background(), "room", 0, time.Second)
	if err == nil || attempts.Load() != 1 {
		t.Fatalf("attempts=%d err=%v", attempts.Load(), err)
	}
}

func TestReadRetryBoundsAndMalformedResponses(t *testing.T) {
	t.Run("network errors stop after three attempts", func(t *testing.T) {
		var attempts atomic.Int32
		transport := roundTripperFunc(func(*http.Request) (*http.Response, error) {
			attempts.Add(1)
			return nil, errors.New("network down")
		})
		c := New("https://relay.test", "token", &http.Client{Transport: transport})
		c.Backoff = time.Millisecond
		_, err := c.Read(context.Background(), "room", 0)
		if err == nil || attempts.Load() != 3 {
			t.Fatalf("attempts=%d err=%v", attempts.Load(), err)
		}
	})

	t.Run("cancellation stops retry backoff", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		var attempts atomic.Int32
		transport := roundTripperFunc(func(*http.Request) (*http.Response, error) {
			attempts.Add(1)
			cancel()
			return nil, errors.New("network down")
		})
		c := New("https://relay.test", "token", &http.Client{Transport: transport})
		c.Backoff = time.Hour
		_, err := c.Read(ctx, "room", 0)
		if !errors.Is(err, context.Canceled) || attempts.Load() != 1 {
			t.Fatalf("attempts=%d err=%v", attempts.Load(), err)
		}
	})

	t.Run("malformed success JSON is an error", func(t *testing.T) {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"messages":`))
		}))
		defer s.Close()
		if _, err := New(s.URL, "token", s.Client()).Read(context.Background(), "room", 0); err == nil {
			t.Fatal("malformed JSON succeeded")
		}
	})
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}
