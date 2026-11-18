package dashboard

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Account roles. An admin can manage accounts; a user can only view the dashboard.
const (
	RoleAdmin = "admin"
	RoleUser  = "user"
)

const (
	minPassword  = 8
	maxPassword  = 128
	generatedLen = 16
	// no 0/O, 1/l/I: a password read out or copied by hand must survive
	passwordAlphabet = "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
)

// pbkdf2Iter is OWASP's figure for PBKDF2-HMAC-SHA256. Each hash records its own count, so it can change later.
// Tests lower it; nothing else does.
var pbkdf2Iter = 600_000

var usernameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,31}$`)

// Account is one login. Hash is "pbkdf2-sha256$iterations$salt$key" (base64).
type Account struct {
	Name       string    `json:"name"`
	Hash       string    `json:"hash"`
	Role       string    `json:"role"`
	MustChange bool      `json:"must_change"` // the password was set by someone else: change it at the next login
	Created    time.Time `json:"created"`
	LastLogin  time.Time `json:"last_login,omitzero"`
}

// Accounts is the user database: a small JSON file, written atomically with mode 0600.
type Accounts struct {
	path string
	mu   sync.Mutex
	m    map[string]*Account
}

// OpenAccounts loads the file, creating it with the default admin:admin account (which must change its password at
// first login) when it does not exist yet. The second result is true when that default account was just created.
func OpenAccounts(path string) (*Accounts, bool, error) {
	a := &Accounts{path: path, m: map[string]*Account{}}
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		h, err := hashPassword("admin")
		if err != nil {
			return nil, false, err
		}
		a.m["admin"] = &Account{Name: "admin", Hash: h, Role: RoleAdmin, MustChange: true, Created: time.Now().UTC()}
		if err := a.save(); err != nil {
			return nil, false, fmt.Errorf("creating %s: %w", path, err)
		}
		return a, true, nil
	case err != nil:
		return nil, false, err
	}
	var list []*Account
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, false, fmt.Errorf("%s: %w", path, err)
	}
	for _, u := range list {
		a.m[u.Name] = u
	}
	if !slices.ContainsFunc(list, func(u *Account) bool { return u.Role == RoleAdmin }) {
		return nil, false, fmt.Errorf("%s has no admin account; delete the file to start over with admin:admin", path)
	}
	return a, false, nil
}

// save writes the file; the caller holds mu.
func (a *Accounts) save() error {
	list := make([]*Account, 0, len(a.m))
	for _, u := range a.m {
		list = append(list, u)
	}
	slices.SortFunc(list, func(x, y *Account) int { return strings.Compare(x.Name, y.Name) })
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(a.path), ".users-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), a.path)
}

var dummyHash, _ = hashPassword("not-a-real-password")

// Authenticate checks a password. Unknown names cost the same as wrong passwords.
func (a *Accounts) Authenticate(name, password string) (Account, bool) {
	a.mu.Lock()
	u := a.m[name]
	a.mu.Unlock()
	if u == nil {
		verifyPassword(dummyHash, password)
		return Account{}, false
	}
	if !verifyPassword(u.Hash, password) {
		return Account{}, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if cur := a.m[name]; cur != nil && cur.Hash == u.Hash {
		cur.LastLogin = time.Now().UTC()
		_ = a.save() // best effort: last-login is informational
		return *cur, true
	}
	return Account{}, false // changed while we were hashing
}

// Get returns a copy of the account.
func (a *Accounts) Get(name string) (Account, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if u := a.m[name]; u != nil {
		return *u, true
	}
	return Account{}, false
}

// List returns every account, sorted by name.
func (a *Accounts) List() []Account {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]Account, 0, len(a.m))
	for _, u := range a.m {
		out = append(out, *u)
	}
	slices.SortFunc(out, func(x, y Account) int { return strings.Compare(x.Name, y.Name) })
	return out
}

// ErrPolicy is a password or name the rules reject; its text is safe to show.
var ErrPolicy = errors.New("policy")

func policyErr(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrPolicy, fmt.Sprintf(format, args...))
}

// CheckPassword applies the password rules.
func CheckPassword(name, pw string) error {
	switch {
	case len(pw) < minPassword:
		return policyErr("the password needs at least %d characters", minPassword)
	case len(pw) > maxPassword:
		return policyErr("the password is longer than %d characters", maxPassword)
	case strings.EqualFold(pw, name), strings.EqualFold(pw, "admin"), strings.EqualFold(pw, "password"):
		return policyErr("that password is too easy to guess")
	}
	return nil
}

// SetPassword is the user choosing their own password: it clears must_change.
func (a *Accounts) SetPassword(name, pw string) error {
	if err := CheckPassword(name, pw); err != nil {
		return err
	}
	h, err := hashPassword(pw)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	u := a.m[name]
	if u == nil {
		return policyErr("no such user")
	}
	u.Hash, u.MustChange = h, false
	return a.save()
}

// Add creates an account with a generated password the user must change at first login, and returns that password.
func (a *Accounts) Add(name, role string) (string, error) {
	if !usernameRE.MatchString(name) {
		return "", policyErr("a username is 1-32 lowercase letters, digits, . _ -, starting with a letter or digit")
	}
	if role != RoleAdmin && role != RoleUser {
		return "", policyErr("role must be admin or user")
	}
	pw, h, err := newGenerated()
	if err != nil {
		return "", err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.m[name] != nil {
		return "", policyErr("that username is taken")
	}
	a.m[name] = &Account{Name: name, Hash: h, Role: role, MustChange: true, Created: time.Now().UTC()}
	if err := a.save(); err != nil {
		delete(a.m, name)
		return "", err
	}
	return pw, nil
}

// Reset gives an existing account a new generated password, to be changed at its next login.
func (a *Accounts) Reset(name string) (string, error) {
	pw, h, err := newGenerated()
	if err != nil {
		return "", err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	u := a.m[name]
	if u == nil {
		return "", policyErr("no such user")
	}
	u.Hash, u.MustChange = h, true
	return pw, a.save()
}

// Delete removes an account. The last admin cannot be removed.
func (a *Accounts) Delete(name string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	u := a.m[name]
	if u == nil {
		return policyErr("no such user")
	}
	if u.Role == RoleAdmin {
		n := 0
		for _, o := range a.m {
			if o.Role == RoleAdmin {
				n++
			}
		}
		if n < 2 {
			return policyErr("that is the last admin")
		}
	}
	delete(a.m, name)
	if err := a.save(); err != nil {
		a.m[name] = u
		return err
	}
	return nil
}

func newGenerated() (pw, hash string, err error) {
	b := make([]byte, generatedLen)
	if _, err = rand.Read(b); err != nil {
		return "", "", err
	}
	out := make([]byte, generatedLen)
	for i, v := range b { // 256 % 54 != 0: reject the biased tail by redrawing
		for int(v) >= 256-256%len(passwordAlphabet) {
			var r [1]byte
			if _, err = rand.Read(r[:]); err != nil {
				return "", "", err
			}
			v = r[0]
		}
		out[i] = passwordAlphabet[int(v)%len(passwordAlphabet)]
	}
	pw = string(out)
	hash, err = hashPassword(pw)
	return pw, hash, err
}

func hashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	k, err := pbkdf2.Key(sha256.New, pw, salt, pbkdf2Iter, 32)
	if err != nil {
		return "", err
	}
	e := base64.RawStdEncoding
	return "pbkdf2-sha256$" + strconv.Itoa(pbkdf2Iter) + "$" + e.EncodeToString(salt) + "$" + e.EncodeToString(k), nil
}

func verifyPassword(hash, pw string) bool {
	p := strings.Split(hash, "$")
	if len(p) != 4 || p[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err := strconv.Atoi(p[1])
	if err != nil || iter < 1 || iter > 10_000_000 {
		return false
	}
	e := base64.RawStdEncoding
	salt, err1 := e.DecodeString(p[2])
	want, err2 := e.DecodeString(p[3])
	if err1 != nil || err2 != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, pw, salt, iter, len(want))
	return err == nil && subtle.ConstantTimeCompare(got, want) == 1
}
