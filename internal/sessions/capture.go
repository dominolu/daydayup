package sessions

import (
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

type Capture struct {
	store        *Store
	record       Record
	requestPath  string
	response     *bodyWriter
	responsePath string
	closed       bool
}

func (s *Store) Begin(record Record, request []byte, name string) (*Capture, error) {
	if !ValidID(record.ID) || !ValidID(record.SessionID) {
		return nil, errors.New("invalid session or request ID")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if record.StartedAt.IsZero() {
		record.StartedAt = time.Now()
	}
	if record.Title != nil {
		record.Purpose = "title"
	} else {
		if record.Purpose == "" {
			record.Purpose = "model"
		}
		if record.Request.Complete && len(record.UserMessages) == 0 {
			record.UserMessages = MessageHashes(HumanMessages(record.Path, request))
		}
	}
	e := s.entries[record.SessionID]
	if e == nil {
		if len([]rune(name)) > 80 {
			name = string([]rune(name)[:80])
		}
		if name == "" {
			name = record.SessionID
		}
		e = &entry{meta: sessionMeta{ID: record.SessionID, Name: name, CreatedAt: record.StartedAt, PreferredAccounts: map[string]string{}}, records: map[string]Record{}, tools: map[string]ToolEvent{}}
		e.meta.NameInfo = defaultNameInfo(e.meta)
		if err := s.saveMeta(e.meta); err != nil {
			return nil, err
		}
		s.entries[record.SessionID] = e
	}
	if _, exists := e.records[record.ID]; exists {
		return nil, errors.New("duplicate session request ID")
	}
	// Both bodies stream to transient temp files while the request runs; they
	// are imported into the database at Finish (or on restart after a crash).
	requestPath := s.tmpPath(record.SessionID, record.ID, "request")
	requestBody, err := createBody(s.db.AEAD(), requestPath, record.SessionID+"/"+record.ID+"/request")
	if err != nil {
		return nil, err
	}
	_, err = requestBody.Write(request)
	closeErr := requestBody.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, err
	}
	record.Request.Bytes = requestBody.bytes
	responsePath := s.tmpPath(record.SessionID, record.ID, "response")
	responseBody, err := createBody(s.db.AEAD(), responsePath, record.SessionID+"/"+record.ID+"/response")
	if err != nil {
		return nil, err
	}
	record.Outcome = "running"
	record.Attempts = []Attempt{}
	if err = s.saveRecord(record); err != nil {
		_ = responseBody.Close()
		return nil, err
	}
	e.records[record.ID] = record
	if e.seedClaim == record.ID {
		e.seedClaim = ""
	}
	if err := s.reconcileTitlesLocked(e); err != nil {
		slog.Error("session title reconciliation failed", "request_id", record.ID)
	}
	return &Capture{store: s, record: record, requestPath: requestPath, response: responseBody, responsePath: responsePath}, nil
}

func (c *Capture) WriteResponse(p []byte) {
	if c.response.err == nil {
		_, _ = c.response.Write(p)
	}
}

func (c *Capture) AddAttempt(attempt Attempt) {
	c.record.Attempts = append(c.record.Attempts, attempt)
}

// importBody moves one transient body file into durable storage and removes
// the source file. In file-backed mode the transient file is atomically
// renamed into the session archive and only an index row is written; in
// database mode the bytes are imported as a BLOB as before. A missing source
// file is not an error (a previous run may already have imported it).
func (c *Capture) importBody(sessionID, requestID, direction, path string) error {
	if c.store.bodyFiles {
		dst := c.store.bodyPath(sessionID, requestID, direction)
		if dir := filepath.Dir(dst); dir != "" {
			if err := os.MkdirAll(dir, 0700); err != nil {
				return err
			}
		}
		if err := os.Rename(path, dst); err != nil {
			if !os.IsNotExist(err) {
				return err
			}
			// Already moved by a previous attempt: only the index is missing.
		}
		return c.store.ensureBodyIndex(sessionID, requestID, direction)
	}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := c.store.db.SQL().Exec(`INSERT INTO session_bodies(session_id, request_id, direction, body) VALUES(?, ?, ?, ?) ON CONFLICT(session_id, request_id, direction) DO UPDATE SET body = excluded.body`, sessionID, requestID, direction, b); err != nil {
		return err
	}
	return os.Remove(path)
}

func (c *Capture) Finish(result Result) error {
	if c.closed {
		return nil
	}
	c.closed = true
	closeErr := c.response.Close()
	// Bodies first: even a metadata failure must not lose the archive.
	if err := c.importBody(c.record.SessionID, c.record.ID, "request", c.requestPath); err != nil {
		return err
	}
	if err := c.importBody(c.record.SessionID, c.record.ID, "response", c.responsePath); err != nil {
		return err
	}
	r := copyTitle(c.record)
	if r.Title != nil {
		r.Title.Text, r.Title.Error, r.Title.Status = result.GeneratedTitle, result.TitleError, "pending"
	}
	r.Status, r.Outcome, r.Failure = result.Status, result.Outcome, result.Failure
	if result.Provider != "" {
		r.Provider = result.Provider
	}
	r.FinishedAt = time.Now()
	r.DurationMS = r.FinishedAt.Sub(r.StartedAt).Milliseconds()
	r.PoolWaitMS, r.PoolWaits = result.PoolWaitMS, result.PoolWaits
	r.Response, r.ResponseID = result.Response, result.ResponseID
	r.ToolCallIDs = result.ToolCallIDs
	r.TurnStateHash = result.TurnStateHash
	r.Response.Bytes = c.response.bytes
	if closeErr != nil {
		r.Response.Complete = false
		r.CaptureError = "response body could not be fully archived"
	}
	r.UsageComplete = len(r.Attempts) > 0 && r.Response.Complete
	for _, attempt := range r.Attempts {
		r.AccountID = attempt.AccountID
		if !attempt.Usage.Known {
			r.UnknownAttempts++
			r.UsageComplete = false
			continue
		}
		if len(attempt.Usage.Reported) > 0 && (!hasField(attempt.Usage.Reported, "input_tokens") || !hasField(attempt.Usage.Reported, "output_tokens")) {
			r.UsageComplete = false
		}
		r.Usage.Reported = mergeFields(r.Usage.Reported, attempt.Usage.Reported)
		r.Usage.Known = true
		r.Usage.Input += attempt.Usage.Input
		r.Usage.Output += attempt.Usage.Output
		r.Usage.Total += attempt.Usage.Total
		r.Usage.Cached += attempt.Usage.Cached
	}
	r = decorate(r, nil)
	c.store.mu.Lock()
	defer c.store.mu.Unlock()
	e := c.store.entries[r.SessionID]
	if r.Title != nil {
		if latest := e.records[r.ID].Title; latest != nil {
			r.Title.Association, r.Title.LinkedRequestID = latest.Association, latest.LinkedRequestID
		}
	}
	if err := c.store.saveRecord(r); err != nil {
		// Retain the result in memory; on restart the durable running record
		// will explicitly become interrupted rather than claiming success.
		r.CaptureError = "request metadata could not be durably saved"
		e.records[r.ID] = r
		return err
	}
	e.records[r.ID] = r
	if r.Title != nil {
		c.store.applyTitleLocked(e, &r)
		if err := c.store.saveRecord(r); err != nil {
			return err
		}
		e.records[r.ID] = r
	}
	return closeErr
}

func clone[T any](value T) T {
	b, _ := json.Marshal(value)
	var result T
	_ = json.Unmarshal(b, &result)
	return result
}
