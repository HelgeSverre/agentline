package localconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/HelgeSverre/agentline/internal/model"
	"github.com/gofrs/flock"
)

const DefaultServerURL = "https://agentline.dev"

var userHomeDir = os.UserHomeDir

var (
	ErrRoomNotFound  = errors.New("room not found")
	ErrRoomAmbiguous = errors.New("room handle is ambiguous")
)

type Config struct {
	ServerURL string `json:"server_url"`
}

type Store struct {
	Root string
}

func (s Store) Load() (Config, error) {
	root, err := s.root()
	if err != nil {
		return Config{}, err
	}
	var config Config
	err = readJSON(filepath.Join(root, "config.json"), &config)
	if errors.Is(err, os.ErrNotExist) {
		return Config{ServerURL: DefaultServerURL}, nil
	}
	if err != nil {
		return Config{}, err
	}
	if config.ServerURL == "" {
		config.ServerURL = DefaultServerURL
	}
	return config, nil
}

func (s Store) Save(config Config) error {
	root, err := s.root()
	if err != nil {
		return err
	}
	return writeJSON(filepath.Join(root, "config.json"), config)
}

func (s Store) SaveRoom(room model.RoomCredential) error {
	if !validRoomID(room.RoomID) {
		return fmt.Errorf("invalid room ID %q", room.RoomID)
	}
	root, err := s.root()
	if err != nil {
		return err
	}
	return s.withRoomLock(root, room.RoomID, func(path string) error {
		var current model.RoomCredential
		if err := readJSON(path, &current); err == nil && current.Cursor > room.Cursor {
			room.Cursor = current.Cursor
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return writeJSON(path, room)
	})
}

// AdvanceCursor atomically persists the greatest observed sequence while
// preserving credential fields written by another process.
func (s Store) AdvanceCursor(roomID string, sequence int64) error {
	if !validRoomID(roomID) {
		return fmt.Errorf("invalid room ID %q", roomID)
	}
	root, err := s.root()
	if err != nil {
		return err
	}
	return s.withExistingRoomLock(root, roomID, func(path string) error {
		var room model.RoomCredential
		if err := readJSON(path, &room); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return ErrRoomNotFound
			}
			return err
		}
		if sequence <= room.Cursor {
			return nil
		}
		room.Cursor = sequence
		return writeExistingJSON(path, room)
	})
}

func (s Store) LoadRoom(handle string) (model.RoomCredential, error) {
	rooms, err := s.ListRooms()
	if err != nil {
		return model.RoomCredential{}, err
	}
	if handle == "" {
		switch len(rooms) {
		case 0:
			return model.RoomCredential{}, ErrRoomNotFound
		case 1:
			return rooms[0], nil
		default:
			return model.RoomCredential{}, ErrRoomAmbiguous
		}
	}

	var matches []model.RoomCredential
	for _, room := range rooms {
		if room.RoomID == handle {
			return room, nil
		}
		if room.RoomName == handle {
			matches = append(matches, room)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		return model.RoomCredential{}, ErrRoomAmbiguous
	}
	return model.RoomCredential{}, ErrRoomNotFound
}

func (s Store) ListRooms() ([]model.RoomCredential, error) {
	root, err := s.root()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(root, "rooms")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list rooms: %w", err)
	}
	rooms := make([]model.RoomCredential, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		var room model.RoomCredential
		if err := readJSON(filepath.Join(dir, entry.Name()), &room); err != nil {
			return nil, fmt.Errorf("load room %s: %w", entry.Name(), err)
		}
		rooms = append(rooms, room)
	}
	sort.Slice(rooms, func(i, j int) bool { return rooms[i].RoomID < rooms[j].RoomID })
	return rooms, nil
}

func (s Store) RemoveRoom(roomID string) error {
	if !validRoomID(roomID) {
		return fmt.Errorf("invalid room ID %q", roomID)
	}
	root, err := s.root()
	if err != nil {
		return err
	}
	err = s.withRoomLock(root, roomID, func(path string) error { return os.Remove(path) })
	if errors.Is(err, os.ErrNotExist) {
		return ErrRoomNotFound
	}
	if err != nil {
		return fmt.Errorf("remove room: %w", err)
	}
	return nil
}

func (s Store) withRoomLock(root, roomID string, action func(string) error) error {
	dir := filepath.Join(root, "rooms")
	if err := ensureDir(dir); err != nil {
		return err
	}
	return lockRoom(dir, roomID, action)
}

// withExistingRoomLock does not create or change the credential directory.
func (s Store) withExistingRoomLock(root, roomID string, action func(string) error) error {
	return lockRoom(filepath.Join(root, "rooms"), roomID, action)
}

func lockRoom(dir, roomID string, action func(string) error) error {
	lock := flock.New(filepath.Join(dir, roomID+".lock"))
	if err := lock.Lock(); err != nil {
		return fmt.Errorf("lock room: %w", err)
	}
	defer lock.Unlock()
	return action(filepath.Join(dir, roomID+".json"))
}

// DefaultRoot always stores configuration below $HOME/.config/agentline,
// regardless of the platform's user data directory convention (in particular
// macOS's ~/Library/Application Support).
func DefaultRoot() (string, error) {
	home, err := userHomeDir()
	if err != nil {
		return "", fmt.Errorf("find user home directory: %w", err)
	}
	return filepath.Join(home, ".config", "agentline"), nil
}

func (s Store) root() (string, error) {
	if s.Root != "" {
		return s.Root, nil
	}
	return DefaultRoot()
}

func validRoomID(roomID string) bool {
	return roomID != "" && roomID != "." && roomID != ".." && !strings.ContainsAny(roomID, `/\`)
}

func readJSON(path string, value any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	if err := decoder.Decode(value); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return fmt.Errorf("decode %s: trailing data: %w", path, err)
	}
	return nil
}

func writeJSON(path string, value any) (err error) {
	dir := filepath.Dir(path)
	if err := ensureDir(dir); err != nil {
		return err
	}
	return writeJSONInExistingDir(path, value)
}

// writeExistingJSON atomically replaces a file without modifying its parent
// directory. This is used by routine room operations in restricted sandboxes.
func writeExistingJSON(path string, value any) error {
	return writeJSONInExistingDir(path, value)
}

func writeJSONInExistingDir(path string, value any) (err error) {
	dir := filepath.Dir(path)
	file, err := os.CreateTemp(dir, ".agentline-*")
	if err != nil {
		return fmt.Errorf("create temporary config: %w", err)
	}
	temporary := file.Name()
	defer func() {
		file.Close()
		if err != nil {
			os.Remove(temporary)
		}
	}()
	if err = json.NewEncoder(file).Encode(value); err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	if err = file.Sync(); err != nil {
		return fmt.Errorf("sync config: %w", err)
	}
	if err = file.Close(); err != nil {
		return fmt.Errorf("close config: %w", err)
	}
	if err = replaceFile(temporary, path); err != nil {
		return fmt.Errorf("replace config: %w", err)
	}
	return nil
}

func ensureDir(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	return nil
}
