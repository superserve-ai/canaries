package loadrunner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ResidentInventoryEvidence comes from an operator-owned read-only database
// observer. Public list responses omit placement, so local IDs alone cannot
// establish that a retained fleet remains paused on the intended host.
type ResidentInventoryEvidence struct {
	Source         string    `json:"source"`
	ObservedAt     time.Time `json:"observed_at"`
	RunID          string    `json:"run_id"`
	TeamID         string    `json:"team_id"`
	WorkerID       string    `json:"worker_id"`
	Target         string    `json:"target"`
	Host           string    `json:"expected_host"`
	Population     int       `json:"population"`
	Paused         int       `json:"paused"`
	WrongHostCount int       `json:"wrong_host_count"`
	IDsSHA256      string    `json:"ids_sha256"`
}

func (r Runner) residentInventory(ctx context.Context) (ResidentInventoryEvidence, error) {
	var e ResidentInventoryEvidence
	if ctx.Err() != nil {
		return e, ctx.Err()
	}
	var err error
	if r.ReadResidentInventory != nil {
		e, err = r.ReadResidentInventory(ctx)
	} else {
		f, openErr := os.Open(r.Config.Resident.InventoryPath)
		if openErr != nil {
			return e, fmt.Errorf("resident inventory evidence unavailable: %w", openErr)
		}
		defer f.Close()
		info, statErr := f.Stat()
		if statErr != nil {
			return e, statErr
		}
		if info.Size() > 64<<10 {
			return e, errors.New("resident inventory evidence exceeds size limit")
		}
		dec := json.NewDecoder(io.LimitReader(f, 64<<10))
		dec.DisallowUnknownFields()
		err = dec.Decode(&e)
		if err == nil {
			if trailing := dec.Decode(new(any)); trailing != io.EOF {
				err = errors.New("resident inventory evidence must contain one JSON object")
			}
		}
	}
	if err != nil {
		return e, err
	}
	cfg := r.Config
	now := r.now()
	if e.Source != "control-plane-sandbox-read" || e.RunID != cfg.RunID || e.TeamID != cfg.Resident.Preflight.TeamID || e.WorkerID != cfg.WorkerID || e.Target != cfg.Target || e.Host != cfg.Resident.Preflight.ExpectedHost || e.ObservedAt.IsZero() || e.ObservedAt.After(now.Add(5*time.Second)) || now.Sub(e.ObservedAt) > time.Minute || e.Population < 0 || e.Population > cfg.Resident.Population || e.Paused < 0 || e.Paused > e.Population || e.WrongHostCount != 0 || len(e.IDsSHA256) != 64 {
		return e, errors.Join(errResidentUnsafe, errors.New("resident inventory evidence is stale, mismatched, or reports misplaced owners"))
	}
	return e, nil
}

func (r Runner) checkResidentInventory(ctx context.Context, j *residentJournal, population int, after time.Time) (ResidentInventoryEvidence, error) {
	e, err := r.residentInventory(ctx)
	if err != nil {
		return e, err
	}
	if e.ObservedAt.Before(after) {
		return e, errors.New("resident inventory observation predates the stage boundary")
	}
	ids := []string{}
	for _, owner := range j.snapshot() {
		if owner.Slot < r.Config.Resident.Population && !owner.Deleted {
			ids = append(ids, owner.SandboxID)
		}
	}
	digest, err := residentIDsDigest(ids)
	if err != nil {
		return e, err
	}
	if len(ids) != population || e.Population != population || e.Paused != population || e.IDsSHA256 != digest {
		return e, errors.Join(errResidentUnsafe, errors.New("resident fleet changed: authoritative paused population or IDs differ from the retained journal"))
	}
	return e, nil
}

func (r Runner) waitResidentInventory(ctx context.Context, j *residentJournal, population int, after time.Time) (ResidentInventoryEvidence, error) {
	proofCtx, cancel := context.WithTimeout(ctx, r.Config.ActiveTimeout)
	defer cancel()
	for {
		e, err := r.residentInventory(proofCtx)
		if err != nil {
			return e, err
		}
		if !e.ObservedAt.Before(after) {
			return r.checkResidentInventory(proofCtx, j, population, after)
		}
		if err := r.residentWait(proofCtx, r.Config.PollInterval); err != nil {
			return e, err
		}
	}
}

func residentIDsDigest(ids []string) (string, error) {
	normalized := make([]string, len(ids))
	seen := map[string]bool{}
	for i, id := range ids {
		// Public IDs may prefix a UUID with the serving region; database IDs
		// are bare UUIDs. Hash the same canonical bytes in both producers.
		if len(id) < 36 {
			return "", errors.New("resident inventory contains a non-UUID ID")
		}
		parsed, err := uuid.Parse(id[len(id)-36:])
		if err != nil {
			return "", err
		}
		normalized[i] = parsed.String()
		if seen[normalized[i]] {
			return "", errors.New("resident inventory duplicates an ID")
		}
		seen[normalized[i]] = true
	}
	sort.Strings(normalized)
	sum := sha256.Sum256([]byte(strings.Join(normalized, "\n")))
	return hex.EncodeToString(sum[:]), nil
}
