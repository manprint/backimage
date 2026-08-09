package registry

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Checkpoint is the resumable state of one backup.
type Checkpoint struct {
	ID        string          `json:"id"`
	Ref       string          `json:"ref"`
	CreatedAt time.Time       `json:"createdAt"`
	DoneBlobs []string        `json:"doneBlobs"`
	Manifest  json.RawMessage `json:"manifest,omitempty"` // manifest.json from the backup plan
}

// CheckpointStore records which blobs are already on the registry.
type CheckpointStore interface {
	Load(id string) (*Checkpoint, error)
	Save(c *Checkpoint) error
	Delete(id string) error
}

// ErrCheckpointNotFound is returned by Load for absent checkpoints.
var ErrCheckpointNotFound = errors.New("checkpoint not found")

// NewCheckpointStore returns a checkpoints store rooted at dir
// (default $XDG_CACHE_HOME/backimage/checkpoints). One JSON file per id.
func NewCheckpointStore(dir string) CheckpointStore {
	if dir == "" {
		base := os.Getenv("XDG_CACHE_HOME")
		if base == "" {
			if home, err := os.UserHomeDir(); err == nil {
				base = filepath.Join(home, ".cache")
			}
		}
		dir = filepath.Join(base, "backimage", "checkpoints")
	}
	return &fileCheckpointStore{dir: dir}
}

type fileCheckpointStore struct {
	dir string
}

func (s *fileCheckpointStore) path(id string) (string, error) {
	if id == "" {
		return "", errors.New("checkpoint id is empty")
	}
	for _, r := range id {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') &&
			(r < '0' || r > '9') && r != '-' && r != '_' {
			return "", fmt.Errorf("invalid checkpoint id %q", id)
		}
	}
	return filepath.Join(s.dir, id+".json"), nil
}

func (s *fileCheckpointStore) Load(id string) (*Checkpoint, error) {
	path, err := s.path(id)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrCheckpointNotFound
		}
		return nil, fmt.Errorf("reading checkpoint %s: %w", id, err)
	}
	var c Checkpoint
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parsing checkpoint %s: %w", id, err)
	}
	return &c, nil
}

func (s *fileCheckpointStore) Save(c *Checkpoint) error {
	if c == nil {
		return errors.New("checkpoint is nil")
	}
	path, err := s.path(c.ID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("creating checkpoint dir: %w", err)
	}
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	dir := s.dir
	tmp, err := os.CreateTemp(dir, ".ckpt-*")
	if err != nil {
		return fmt.Errorf("checkpoint temp file: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("checkpoint rename: %w", err)
	}
	return nil
}

func (s *fileCheckpointStore) Delete(id string) error {
	path, err := s.path(id)
	if err != nil {
		return err
	}
	err = os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
