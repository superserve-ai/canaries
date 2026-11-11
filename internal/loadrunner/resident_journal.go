package loadrunner

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const residentJournalMaxBytes = 512 << 20

type residentEntry struct {
	Slot      int    `json:"slot"`
	Name      string `json:"name,omitempty"`
	SandboxID string `json:"sandbox_id,omitempty"`
	Deleted   bool   `json:"deleted,omitempty"`
}

type residentJournalRecord struct {
	Kind       string        `json:"kind"`
	Entry      residentEntry `json:"entry,omitempty"`
	RunID      string        `json:"run_id,omitempty"`
	TeamID     string        `json:"team_id,omitempty"`
	WorkerID   string        `json:"worker_id,omitempty"`
	Target     string        `json:"target,omitempty"`
	Host       string        `json:"host,omitempty"`
	Template   string        `json:"template,omitempty"`
	Population int           `json:"population,omitempty"`
	CreatedAt  time.Time     `json:"created_at,omitempty"`
	ExpiresAt  time.Time     `json:"expires_at,omitempty"`
}

type residentJournal struct {
	mu        sync.Mutex
	file      *os.File
	header    residentJournalRecord
	entries   map[int]residentEntry
	completed bool
	bytes     int64
}

func openResidentJournal(cfg Config, now time.Time) (*residentJournal, error) {
	path := cfg.Resident.JournalPath
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		return nil, err
	}
	j := &residentJournal{file: f, entries: map[int]residentEntry{}}
	fail := func(err error) (*residentJournal, error) { _ = f.Close(); return nil, err }
	stat, err := f.Stat()
	if err != nil {
		return fail(err)
	}
	j.bytes = stat.Size()
	if j.bytes > residentJournalMaxBytes {
		return fail(errors.New("resident journal exceeds size limit"))
	}
	scanner := bufio.NewScanner(io.LimitReader(f, residentJournalMaxBytes+1))
	scanner.Buffer(make([]byte, 4096), 64<<10)
	seenHeader := false
	for scanner.Scan() {
		var rec residentJournalRecord
		if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
			return fail(errors.New("resident journal is corrupt; no resource mutation permitted"))
		}
		if !seenHeader {
			if rec.Kind != "header" {
				return fail(errors.New("resident journal lacks ownership header"))
			}
			j.header = rec
			seenHeader = true
			continue
		}
		switch rec.Kind {
		case "entry":
			e := rec.Entry
			if e.Slot < 0 || e.Slot >= MaxResidentPopulation+MaxOperations || e.Name == "" {
				return fail(errors.New("resident journal entry is invalid"))
			}
			if old, ok := j.entries[e.Slot]; ok && (old.Name != e.Name || (old.SandboxID != "" && old.SandboxID != e.SandboxID) || old.Deleted && !e.Deleted) {
				return fail(errors.New("resident journal rewrites resource identity"))
			}
			j.entries[e.Slot] = e
		case "complete":
			j.completed = true
		default:
			return fail(errors.New("unknown resident journal record"))
		}
	}
	if err := scanner.Err(); err != nil {
		return fail(err)
	}
	if !seenHeader {
		j.header = residentJournalRecord{Kind: "header", RunID: cfg.RunID, TeamID: cfg.Resident.Preflight.TeamID, WorkerID: cfg.WorkerID, Target: cfg.Target, Host: cfg.Resident.Preflight.ExpectedHost, Template: cfg.Template, Population: cfg.Resident.Population, CreatedAt: now.UTC(), ExpiresAt: now.UTC().Add(cfg.ResourceTTL)}
		if err := j.appendLocked(j.header); err != nil {
			return fail(err)
		}
		// The initial file name must survive a host restart before any creates.
		d, err := os.Open(filepath.Dir(path))
		if err != nil {
			return fail(err)
		}
		err = d.Sync()
		_ = d.Close()
		if err != nil {
			return fail(err)
		}
	}
	h := j.header
	if h.RunID != cfg.RunID || h.TeamID != cfg.Resident.Preflight.TeamID || h.WorkerID != cfg.WorkerID || h.Target != cfg.Target || h.Host != cfg.Resident.Preflight.ExpectedHost || h.Template != cfg.Template || h.Population != cfg.Resident.Population {
		return fail(errors.New("resident journal belongs to another run, worker, host, template or population"))
	}
	if cfg.Resident.Mode != "cleanup" && (j.completed || !h.ExpiresAt.After(now.Add(cfg.RunTimeout+cfg.DeleteTimeout))) {
		return fail(errors.New("resident journal completed or retention cannot cover the remaining run; use cleanup mode"))
	}
	return j, nil
}

func (j *residentJournal) appendLocked(rec residentJournalRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if j.bytes+int64(len(data)) > residentJournalMaxBytes {
		return errors.New("resident journal exceeds size limit")
	}
	n, err := j.file.Write(data)
	j.bytes += int64(n)
	if err != nil {
		return err
	}
	if n != len(data) {
		return io.ErrShortWrite
	}
	return j.file.Sync()
}

func (j *residentJournal) save(e residentEntry) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if old, ok := j.entries[e.Slot]; ok && (old.Name != e.Name || old.SandboxID != "" && old.SandboxID != e.SandboxID || old.Deleted && !e.Deleted) {
		return errors.New("resident resource identity changed")
	}
	// Retain a known ID for immediate cleanup even if persisting it fails. The
	// earlier durable intent permits metadata-based recovery after a restart.
	j.entries[e.Slot] = e
	return j.appendLocked(residentJournalRecord{Kind: "entry", Entry: e})
}

func (j *residentJournal) entry(slot int) (residentEntry, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	e, ok := j.entries[slot]
	return e, ok
}
func (j *residentJournal) snapshot() []residentEntry {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]residentEntry, 0, len(j.entries))
	for _, e := range j.entries {
		out = append(out, e)
	}
	return out
}
func (j *residentJournal) complete() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.appendLocked(residentJournalRecord{Kind: "complete"}); err != nil {
		return err
	}
	j.completed = true
	return nil
}
func (j *residentJournal) close() error { return j.file.Close() }

func residentJournalError(stage string, err error) error {
	return fmt.Errorf("resident %s: %w", stage, err)
}
