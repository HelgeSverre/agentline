package localconfig

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HelgeSverre/agentline/internal/model"
)

func TestLoadUsesDefaultServer(t *testing.T) {
	store := Store{Root: filepath.Join(t.TempDir(), "agentline")}
	config, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if config.ServerURL != DefaultServerURL {
		t.Fatalf("ServerURL = %q, want %q", config.ServerURL, DefaultServerURL)
	}
}

func TestSaveReplacesAtomically(t *testing.T) {
	store := Store{Root: filepath.Join(t.TempDir(), "agentline")}
	if err := store.Save(Config{ServerURL: "https://first.example"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(Config{ServerURL: "https://second.example"}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(store.Root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "config.json" {
		t.Fatalf("save left unexpected files: %v", entryNames(entries))
	}
	config, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if config.ServerURL != "https://second.example" {
		t.Fatalf("ServerURL = %q", config.ServerURL)
	}
}

func TestSaveNeverExposesPartialJSONToConcurrentReaders(t *testing.T) {
	store := Store{Root: filepath.Join(t.TempDir(), "agentline")}
	path := filepath.Join(store.Root, "config.json")
	values := []Config{
		{ServerURL: "https://first.example/" + strings.Repeat("a", 64<<10)},
		{ServerURL: "https://second.example/" + strings.Repeat("b", 64<<10)},
	}
	if err := store.Save(values[0]); err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	errs := make(chan error, 1)
	var readers sync.WaitGroup
	for range 4 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				data, err := os.ReadFile(path)
				if err == nil {
					var got Config
					err = json.Unmarshal(data, &got)
					if err == nil && got != values[0] && got != values[1] {
						err = errors.New("read JSON that was neither complete saved value")
					}
				}
				if err != nil {
					select {
					case errs <- err:
					default:
					}
					return
				}
			}
		}()
	}
	for i := 0; i < 100; i++ {
		if err := store.Save(values[i%len(values)]); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	readers.Wait()
	select {
	case err := <-errs:
		t.Fatalf("concurrent reader observed a partial save: %v", err)
	default:
	}
}

func TestDefaultRootPropagatesHomeDirError(t *testing.T) {
	want := errors.New("home directory unavailable")
	original := userHomeDir
	userHomeDir = func() (string, error) { return "", want }
	t.Cleanup(func() { userHomeDir = original })

	if _, err := DefaultRoot(); !errors.Is(err, want) {
		t.Fatalf("DefaultRoot() error = %v, want %v", err, want)
	}
	if _, err := (Store{}).Load(); !errors.Is(err, want) {
		t.Fatalf("Store.Load() error = %v, want %v", err, want)
	}
}

func TestDefaultRootIsDotConfigAgentline(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root, err := DefaultRoot()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".config", "agentline")
	if root != want {
		t.Fatalf("DefaultRoot() = %q, want %q", root, want)
	}
}

func TestLoadRejectsTrailingJSONGarbage(t *testing.T) {
	store := Store{Root: t.TempDir()}
	if err := os.WriteFile(filepath.Join(store.Root, "config.json"), []byte(`{"server_url":"https://example.com"} garbage`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(); err == nil {
		t.Fatal("Load() accepted trailing JSON garbage")
	}
}

func TestRoomLifecycle(t *testing.T) {
	store := Store{Root: filepath.Join(t.TempDir(), "agentline")}
	if err := os.Mkdir(store.Root, 0o755); err != nil {
		t.Fatal(err)
	}
	want := model.RoomCredential{RoomID: "room-1", RoomName: "alpha", ServerURL: "https://example.com", ParticipantID: "p1", Token: "secret", Cursor: 4}
	if err := store.SaveRoom(want); err != nil {
		t.Fatal(err)
	}

	got, err := store.LoadRoom("")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("LoadRoom() = %#v, want %#v", got, want)
	}
	byName, err := store.LoadRoom("alpha")
	if err != nil || byName != want {
		t.Fatalf("LoadRoom(name) = %#v, %v", byName, err)
	}
	rooms, err := store.ListRooms()
	if err != nil || len(rooms) != 1 || rooms[0] != want {
		t.Fatalf("ListRooms() = %#v, %v", rooms, err)
	}
	if err := store.RemoveRoom(want.RoomID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadRoom(""); !errors.Is(err, ErrRoomNotFound) {
		t.Fatalf("LoadRoom after remove error = %v", err)
	}
}

func TestAdvanceCursorDoesNotChangeExistingDirectoryPermissions(t *testing.T) {
	root := filepath.Join(t.TempDir(), "agentline")
	store := Store{Root: root}
	room := model.RoomCredential{RoomID: "room", RoomName: "name", Token: "secret"}
	if err := store.SaveRoom(room); err != nil {
		t.Fatal(err)
	}
	roomsDir := filepath.Join(root, "rooms")
	if err := os.Chmod(roomsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := store.AdvanceCursor(room.RoomID, 1); err != nil {
		t.Fatal(err)
	}
	assertMode(t, roomsDir, 0o755)
}

func TestLoadRoomRefusesToGuessAmongMultipleRooms(t *testing.T) {
	store := Store{Root: t.TempDir()}
	for _, room := range []model.RoomCredential{{RoomID: "one", RoomName: "same"}, {RoomID: "two", RoomName: "same"}} {
		if err := store.SaveRoom(room); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.LoadRoom(""); !errors.Is(err, ErrRoomAmbiguous) {
		t.Fatalf("empty handle error = %v, want ErrRoomAmbiguous", err)
	}
	if _, err := store.LoadRoom("same"); !errors.Is(err, ErrRoomAmbiguous) {
		t.Fatalf("duplicate name error = %v, want ErrRoomAmbiguous", err)
	}
	got, err := store.LoadRoom("two")
	if err != nil || got.RoomID != "two" {
		t.Fatalf("exact ID resolution = %#v, %v", got, err)
	}
}

func TestRoomOperationsRejectInvalidAndTraversalIDs(t *testing.T) {
	store := Store{Root: t.TempDir()}
	for _, roomID := range []string{"", ".", "..", "../outside", `..\outside`, "nested/room", `nested\room`} {
		t.Run(roomID, func(t *testing.T) {
			if err := store.SaveRoom(model.RoomCredential{RoomID: roomID}); err == nil {
				t.Fatalf("SaveRoom accepted invalid room ID %q", roomID)
			}
			if err := store.RemoveRoom(roomID); err == nil {
				t.Fatalf("RemoveRoom accepted invalid room ID %q", roomID)
			}
		})
	}
}

func TestAdvanceCursorKeepsMaximumAcrossIndependentStores(t *testing.T) {
	root := t.TempDir()
	store := Store{Root: root}
	want := model.RoomCredential{RoomID: "room", RoomName: "name", ServerURL: "https://relay.example", Token: "secret", Cursor: 1}
	if err := store.SaveRoom(want); err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	results := make(chan error, 2)
	for _, sequence := range []int64{10, 5} {
		sequence := sequence
		go func() {
			<-start
			results <- (Store{Root: root}).AdvanceCursor("room", sequence)
		}()
	}
	close(start)
	for range 2 {
		select {
		case err := <-results:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("cursor advancement timed out")
		}
	}
	got, err := store.LoadRoom("room")
	if err != nil {
		t.Fatal(err)
	}
	want.Cursor = 10
	if got != want {
		t.Fatalf("room = %#v, want %#v", got, want)
	}
	stale := want
	stale.Cursor = 3
	stale.RoomName = "updated"
	if err := store.SaveRoom(stale); err != nil {
		t.Fatal(err)
	}
	got, err = store.LoadRoom("room")
	if err != nil || got.Cursor != 10 || got.RoomName != "updated" {
		t.Fatalf("stale SaveRoom regressed cursor or fields: room=%#v error=%v", got, err)
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %o, want %o", path, got, want)
	}
}

func entryNames(entries []os.DirEntry) []string {
	names := make([]string, len(entries))
	for i, entry := range entries {
		names[i] = entry.Name()
	}
	return names
}
