package downloader

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

const taskVersion = 1

type Task struct {
	Version      int         `json:"version"`
	URL          string      `json:"url"`
	Output       string      `json:"output"`
	TotalSize    int64       `json:"total_size"`
	AcceptRanges bool        `json:"accept_ranges"`
	ETag         string      `json:"etag,omitempty"`
	LastModified string      `json:"last_modified,omitempty"`
	Workers      int         `json:"workers"`
	TempDir      string      `json:"temp_dir"`
	Ranges       []RangeTask `json:"ranges"`
	Completed    bool        `json:"completed"`
	CreatedAt    time.Time   `json:"created_at"`
	UpdatedAt    time.Time   `json:"updated_at"`
}

type RangeTask struct {
	Start        int64  `json:"start"`
	End          int64  `json:"end"`
	Done         bool   `json:"done"`
	PartPath     string `json:"part_path"`
	BytesWritten int64  `json:"bytes_written"`
}

func DefaultTaskPath(output string) string {
	return TaskPath("", output)
}

func TaskPath(raw, output string) string {
	return filepath.Join(TempRoot(), taskKey(raw, output)+".task")
}

func TempDir(raw, output string) string {
	return filepath.Join(TempRoot(), taskKey(raw, output))
}

func TempRoot() string {
	return filepath.Join(os.TempDir(), "gofetch")
}

func taskKey(raw, output string) string {
	abs, err := filepath.Abs(output)
	if err != nil {
		abs = output
	}
	sum := sha256.Sum256([]byte(raw + "\n" + abs))
	return hex.EncodeToString(sum[:])[:16]
}

func LoadTask(path string) (*Task, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var task Task
	if err := json.Unmarshal(data, &task); err != nil {
		return nil, err
	}
	if task.Version != taskVersion {
		return nil, errors.New("unsupported task file version")
	}
	// Normalize TempDir and PartPaths to the current machine's temp root.
	// The hash component is machine-independent; only the OS temp prefix changes.
	if task.TempDir != "" {
		hash := filepath.Base(task.TempDir)
		expected := filepath.Join(os.TempDir(), "gofetch", hash)
		if task.TempDir != expected {
			for i := range task.Ranges {
				task.Ranges[i].PartPath = filepath.Join(expected, filepath.Base(task.Ranges[i].PartPath))
			}
			task.TempDir = expected
		}
	}
	return &task, nil
}

func SaveTask(path string, task *Task) error {
	task.UpdatedAt = time.Now().UTC()
	if task.CreatedAt.IsZero() {
		task.CreatedAt = task.UpdatedAt
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil && filepath.Dir(path) != "." {
		return err
	}
	data, err := json.MarshalIndent(task, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func validatorsChanged(task *Task, probe ProbeInfo) bool {
	if task.TotalSize > 0 && probe.TotalSize > 0 && task.TotalSize != probe.TotalSize {
		return true
	}
	if task.ETag != "" && probe.ETag != "" && task.ETag != probe.ETag {
		return true
	}
	if task.LastModified != "" && probe.LastModified != "" && task.LastModified != probe.LastModified {
		return true
	}
	return false
}
