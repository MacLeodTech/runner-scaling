package dashboard

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"sync"
	"time"
)

// DefaultAuditMax is the size at which the audit log rotates when none is configured.
const DefaultAuditMax = 5 << 20

// AuditEntry is one line of the audit log.
type AuditEntry struct {
	Time   time.Time `json:"time"`
	Event  string    `json:"event"`            // login, login_failed, login_locked, logout, password_changed, ...
	User   string    `json:"user,omitempty"`   // the account the event is about
	By     string    `json:"by,omitempty"`     // the admin who did it, when that is someone else
	IP     string    `json:"ip,omitempty"`     // the TCP peer: behind a proxy this is the proxy
	Fwd    string    `json:"fwd,omitempty"`    // X-Forwarded-For as sent: unverified, shown only as a hint
	Detail string    `json:"detail,omitempty"` // short free text (the role of a new user, why a login failed)
}

// Audit is an append-only JSON-lines file of account and login events. It is bounded: when a write would take the file
// past Max bytes, the file becomes Path+".1" (replacing the previous backup) and a new one starts, so the trail keeps
// between Max and 2*Max bytes of history. A failed write is logged and never fails the request that caused it.
type Audit struct {
	Path string
	Max  int64
	Log  *slog.Logger

	mu sync.Mutex
}

// NewAudit returns an audit log in path. max <= 0 means DefaultAuditMax.
func NewAudit(path string, max int64, log *slog.Logger) *Audit {
	if max <= 0 {
		max = DefaultAuditMax
	}
	return &Audit{Path: path, Max: max, Log: log}
}

// Add appends an entry. Safe to call on a nil *Audit (auditing off).
func (a *Audit) Add(e AuditEntry) {
	if a == nil {
		return
	}
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	b = append(b, '\n')
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.append(b); err != nil && a.Log != nil {
		a.Log.Error("audit log write failed", "file", a.Path, "err", err)
	}
}

func (a *Audit) append(b []byte) error {
	if fi, err := os.Stat(a.Path); err == nil && fi.Size() > 0 && fi.Size()+int64(len(b)) > a.Max {
		if err := os.Rename(a.Path, a.Path+".1"); err != nil {
			return fmt.Errorf("rotate: %w", err)
		}
	}
	f, err := os.OpenFile(a.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Recent returns up to n entries, newest first, reading the backup too when the current file is short.
func (a *Audit) Recent(n int) ([]AuditEntry, error) {
	if a == nil || n <= 0 {
		return []AuditEntry{}, nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	out := []AuditEntry{}
	for _, p := range []string{a.Path, a.Path + ".1"} {
		es, err := readAudit(p)
		if err != nil {
			return nil, err
		}
		for i := len(es) - 1; i >= 0 && len(out) < n; i-- {
			out = append(out, es[i])
		}
		if len(out) >= n {
			break
		}
	}
	return out, nil
}

// readAudit parses a file oldest first. A missing file is empty; a damaged line is skipped, not fatal.
func readAudit(path string) ([]AuditEntry, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []AuditEntry
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var e AuditEntry
		if json.Unmarshal(sc.Bytes(), &e) == nil && e.Event != "" {
			out = append(out, e)
		}
	}
	return out, sc.Err()
}
