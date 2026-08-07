package localserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HelgeSverre/agentline/internal/localconfig"
)

func TestEnsureStartsOnFreePortReusesAndStops(t *testing.T) {
	m := testManager(t)
	url, err := m.Ensure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Stop(context.Background()) })
	if url == "http://127.0.0.1:8080" {
		t.Fatal("local relay used the fixed legacy port")
	}
	response, err := http.Get(url + "/healthz")
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("health check: response=%v err=%v", response, err)
	}
	response.Body.Close()

	reused, err := m.Ensure(context.Background())
	if err != nil || reused != url {
		t.Fatalf("reuse = %q, %v; want %q", reused, err, url)
	}
	if err := m.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := http.Get(url + "/healthz"); err == nil {
		t.Fatal("relay still accepts requests after Stop")
	}
}

func TestEnsureRecoversStaleState(t *testing.T) {
	m := testManager(t)
	writeTestState(t, m, state{URL: "http://127.0.0.1:1", Instance: "stale"})
	url, err := m.Ensure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Stop(context.Background()) })
	if url == "http://127.0.0.1:1" {
		t.Fatal("stale relay was reused")
	}
}

func TestStopDoesNotStopServerWithDifferentIdentity(t *testing.T) {
	m := testManager(t)
	var stopped atomic.Bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == managementStatus {
			http.Error(w, "forbidden", http.StatusUnauthorized)
			return
		}
		stopped.Store(true)
	}))
	defer other.Close()
	writeTestState(t, m, state{URL: other.URL, Instance: "not-this-server"})
	if err := m.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if stopped.Load() {
		t.Fatal("unrelated server received shutdown")
	}
}

func TestManagementHandlerRequiresBearerToken(t *testing.T) {
	var stopped atomic.Bool
	h := ManagementHandler("secret", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }), func() { stopped.Store(true) })
	server := httptest.NewServer(h)
	defer server.Close()
	for _, token := range []string{"", "wrong"} {
		req, _ := http.NewRequest(http.MethodPost, server.URL+managementShutdown, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		response, err := http.DefaultClient.Do(req)
		if err != nil || response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("token %q: response=%v err=%v", token, response, err)
		}
		response.Body.Close()
	}
	if stopped.Load() {
		t.Fatal("unauthenticated shutdown ran")
	}
}

func TestCanceledStopPreservesHealthyRelayState(t *testing.T) {
	m := testManager(t)
	server := httptest.NewServer(ManagementHandler("secret", http.NotFoundHandler(), func() {}))
	defer server.Close()
	writeTestState(t, m, state{URL: server.URL, Instance: "secret"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.Stop(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Stop error = %v, want context canceled", err)
	}
	if _, err := os.Stat(filepath.Join(m.Config.Root, stateFile)); err != nil {
		t.Fatalf("healthy relay state was removed: %v", err)
	}
}

func TestStopTimesOutAndPreservesStateWhileRelayRemainsReachable(t *testing.T) {
	m := testManager(t)
	server := httptest.NewServer(ManagementHandler("secret", http.NotFoundHandler(), func() {}))
	defer server.Close()
	writeTestState(t, m, state{URL: server.URL, Instance: "secret"})
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if err := m.Stop(ctx); err == nil || !strings.Contains(err.Error(), "waiting for local relay shutdown") {
		t.Fatalf("Stop error = %v, want shutdown timeout", err)
	}
	if _, err := os.Stat(filepath.Join(m.Config.Root, stateFile)); err != nil {
		t.Fatalf("relay state was removed after unobserved shutdown: %v", err)
	}
}

func TestLockSerializesAndHonorsCancellation(t *testing.T) {
	root := t.TempDir()
	unlock, err := lock(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := lock(ctx, root); !errors.Is(err, context.Canceled) {
		t.Fatalf("contending lock error = %v, want context canceled", err)
	}
	unlock()
	secondUnlock, err := lock(context.Background(), root)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	secondUnlock()
}

func TestLockReleasedWhenOwnerProcessExits(t *testing.T) {
	if os.Getenv("AGENTLINE_LOCK_HELPER") == "1" {
		unlock, err := lock(context.Background(), os.Getenv("AGENTLINE_LOCK_ROOT"))
		if err != nil {
			os.Exit(2)
		}
		_ = unlock
		if err := os.WriteFile(os.Getenv("AGENTLINE_LOCK_READY"), nil, 0o600); err != nil {
			os.Exit(3)
		}
		os.Exit(0)
	}
	root := t.TempDir()
	ready := filepath.Join(root, "ready")
	command := exec.Command(os.Args[0], "-test.run=^TestLockReleasedWhenOwnerProcessExits$")
	command.Env = append(os.Environ(), "AGENTLINE_LOCK_HELPER=1", "AGENTLINE_LOCK_ROOT="+root, "AGENTLINE_LOCK_READY="+ready)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("lock helper: %v\n%s", err, output)
	}
	if _, err := os.Stat(ready); err != nil {
		t.Fatalf("helper did not acquire lock: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	unlock, err := lock(ctx, root)
	if err != nil {
		t.Fatalf("lock after owner exit: %v", err)
	}
	unlock()
}

func TestCreateLocalUsesManagedRelay(t *testing.T) {
	m := testManager(t)
	home := t.TempDir()
	command := exec.Command(m.Executable, "--json", "create", "--local", "--name", "alice")
	command.Env = append(os.Environ(), "HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, ".config"))
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("create --local: %v\n%s", err, output)
	}
	var created struct {
		InviteURL string `json:"invite_url"`
	}
	if err := json.Unmarshal(output, &created); err != nil {
		t.Fatalf("create output: %v\n%s", err, output)
	}
	if !strings.HasPrefix(created.InviteURL, "http://127.0.0.1:") {
		t.Fatalf("invite URL = %q", created.InviteURL)
	}
	configRoot := filepath.Join(home, ".config", "agentline")
	if runtime.GOOS == "darwin" {
		configRoot = filepath.Join(home, "Library", "Application Support", "agentline")
	}
	manager := Manager{Config: localconfig.Store{Root: configRoot}, Executable: m.Executable}
	t.Cleanup(func() { _ = manager.Stop(context.Background()) })
}

func testManager(t *testing.T) Manager {
	t.Helper()
	executable := filepath.Join(t.TempDir(), "agentline")
	command := exec.Command("go", "build", "-o", executable, "./cmd/agentline")
	command.Dir = filepath.Join("..", "..")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build helper: %v\n%s", err, output)
	}
	return Manager{Config: localconfig.Store{Root: t.TempDir()}, Executable: executable}
}

func writeTestState(t *testing.T, m Manager, value state) {
	t.Helper()
	if err := os.MkdirAll(m.Config.Root, 0o700); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(value)
	if err := os.WriteFile(filepath.Join(m.Config.Root, stateFile), data, 0o600); err != nil {
		t.Fatal(err)
	}
}
