package localserver

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/HelgeSverre/agentline/internal/localconfig"
	"github.com/gofrs/flock"
)

const (
	stateFile          = "local-server.json"
	managementStatus   = "/_agentline/local/status"
	managementShutdown = "/_agentline/local/shutdown"
	startupTimeout     = 10 * time.Second
	shutdownTimeout    = 10 * time.Second
	requestTimeout     = time.Second
)

type Manager struct {
	Config     localconfig.Store
	Executable string
}

type state struct {
	URL      string `json:"url"`
	Instance string `json:"instance"`
}

func (m Manager) Ensure(ctx context.Context) (string, error) {
	root, err := m.root()
	if err != nil {
		return "", err
	}
	unlock, err := lock(ctx, root)
	if err != nil {
		return "", err
	}
	defer unlock()

	path := filepath.Join(root, stateFile)
	current, err := readState(path)
	if err == nil && owned(ctx, current) {
		return current.URL, nil
	}
	// An unreachable state is stale. Never send shutdown unless ownership was proven.
	if err := ctx.Err(); err != nil {
		return "", err
	}
	_ = os.Remove(path)

	executable, err := filepath.Abs(m.Executable)
	if err != nil || m.Executable == "" {
		return "", fmt.Errorf("find agentline executable")
	}
	instance, err := nonce()
	if err != nil {
		return "", err
	}
	log, err := os.OpenFile(filepath.Join(root, "local-server.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return "", err
	}
	defer log.Close()
	reader, writer, err := os.Pipe()
	if err != nil {
		return "", err
	}
	defer reader.Close()
	command := exec.Command(executable, "--json", "server", "--listen", "127.0.0.1:0", "--data", filepath.Join(root, "local.db"), "--local-instance", instance)
	command.Stdout, command.Stderr = writer, log
	if err := command.Start(); err != nil {
		writer.Close()
		return "", fmt.Errorf("start local relay: %w", err)
	}
	writer.Close()
	succeeded := false
	defer func() {
		if !succeeded {
			_ = command.Process.Kill()
			_, _ = command.Process.Wait()
		}
	}()

	var ready struct {
		URL string `json:"public_url"`
	}
	decode := make(chan error, 1)
	go func() { decode <- json.NewDecoder(reader).Decode(&ready) }()
	deadline := time.NewTimer(startupTimeout)
	defer deadline.Stop()
	select {
	case err := <-decode:
		if err != nil {
			return "", fmt.Errorf("local relay readiness: %w", err)
		}
	case <-ctx.Done():
		return "", ctx.Err()
	case <-deadline.C:
		return "", errors.New("timed out waiting for local relay")
	}
	reader.Close()
	if ready.URL == "" {
		return "", errors.New("local relay returned an empty URL")
	}
	current = state{URL: ready.URL, Instance: instance}
	for !owned(ctx, current) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		select {
		case <-deadline.C:
			return "", errors.New("local relay did not become healthy")
		case <-time.After(25 * time.Millisecond):
		}
	}
	if err := writeState(path, current); err != nil {
		return "", err
	}
	if err := command.Process.Release(); err != nil {
		return "", err
	}
	succeeded = true
	return ready.URL, nil
}

func (m Manager) Stop(ctx context.Context) error {
	root, err := m.root()
	if err != nil {
		return err
	}
	unlock, err := lock(ctx, root)
	if err != nil {
		return err
	}
	defer unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	path := filepath.Join(root, stateFile)
	current, err := readState(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !owned(ctx, current) {
		if err := ctx.Err(); err != nil {
			return err
		}
		return os.Remove(path)
	}
	requestCtx, cancelRequest := context.WithTimeout(ctx, requestTimeout)
	request, err := managementRequest(requestCtx, http.MethodPost, current, managementShutdown)
	if err != nil {
		cancelRequest()
		return err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		cancelRequest()
		return fmt.Errorf("stop local relay: %w", err)
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	cancelRequest()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("stop local relay: HTTP %d", response.StatusCode)
	}
	waitCtx, cancelWait := context.WithTimeout(ctx, shutdownTimeout)
	defer cancelWait()
	for relayReachable(waitCtx, current) {
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("waiting for local relay shutdown: %w", waitCtx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
	if err := waitCtx.Err(); err != nil {
		return fmt.Errorf("waiting for local relay shutdown: %w", err)
	}
	return os.Remove(path)
}

// ManagementHandler adds private lifecycle endpoints. Callers must only use it
// for a loopback listener with a nonempty random token.
func ManagementHandler(token string, next http.Handler, shutdown func()) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != managementStatus && r.URL.Path != managementShutdown {
			next.ServeHTTP(w, r)
			return
		}
		provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token == "" || len(provided) != len(token) || subtle.ConstantTimeCompare([]byte(provided), []byte(token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.URL.Path == managementStatus && r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "running"})
			return
		}
		if r.URL.Path == managementShutdown && r.Method == http.MethodPost {
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "stopping"})
			go shutdown()
			return
		}
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	})
}

func owned(ctx context.Context, value state) bool {
	request, err := managementRequest(ctx, http.MethodGet, value, managementStatus)
	if err != nil {
		return false
	}
	checkCtx, cancel := context.WithTimeout(request.Context(), 500*time.Millisecond)
	defer cancel()
	response, err := http.DefaultClient.Do(request.WithContext(checkCtx))
	if err != nil {
		return false
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	return response.StatusCode == http.StatusOK
}

func relayReachable(ctx context.Context, value state) bool {
	request, err := managementRequest(ctx, http.MethodGet, value, managementStatus)
	if err != nil {
		return false
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return false
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	return true
}

func managementRequest(ctx context.Context, method string, value state, path string) (*http.Request, error) {
	request, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(value.URL, "/")+path, nil)
	if err == nil {
		request.Header.Set("Authorization", "Bearer "+value.Instance)
	}
	return request, err
}

func lock(ctx context.Context, root string) (func(), error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	fileLock := flock.New(filepath.Join(root, "local-server.lock"))
	locked, err := fileLock.TryLockContext(ctx, 25*time.Millisecond)
	if err != nil {
		return nil, err
	}
	if !locked {
		return nil, ctx.Err()
	}
	return func() { _ = fileLock.Unlock() }, nil
}

func (m Manager) root() (string, error) {
	if m.Config.Root != "" {
		return m.Config.Root, nil
	}
	return localconfig.DefaultRoot()
}

func nonce() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func readState(path string) (state, error) {
	var value state
	data, err := os.ReadFile(path)
	if err != nil {
		return value, err
	}
	if err := json.Unmarshal(data, &value); err != nil {
		return value, fmt.Errorf("read local relay state: %w", err)
	}
	return value, nil
}

func writeState(path string, value state) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".local-server-*.tmp")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
