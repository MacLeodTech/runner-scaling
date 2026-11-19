package ghapi

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Auth supplies a bearer token for API calls.
type Auth interface {
	Token(ctx context.Context) (string, error)
}

// TokenAuth is a fixed token, e.g. a fine-grained personal access token with Actions: read.
type TokenAuth string

func (t TokenAuth) Token(context.Context) (string, error) { return string(t), nil }

// AppAuth mints short-lived installation tokens for a GitHub App and refreshes them before they expire. The App needs
// the Actions: read repository permission for the calls in this package to work.
type AppAuth struct {
	Base           string // https://api.github.com
	ClientID       string
	InstallationID int64
	Key            *rsa.PrivateKey
	HC             *http.Client
	Now            func() time.Time

	mu      sync.Mutex
	token   string
	expires time.Time
}

// ParseKey reads an RSA private key in PEM form (PKCS#1 or PKCS#8), as GitHub provides it.
func ParseKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	blk, _ := pem.Decode(pemBytes)
	if blk == nil {
		return nil, errors.New("private key is not PEM")
	}
	if k, err := x509.ParsePKCS1PrivateKey(blk.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, fmt.Errorf("private key: %w", err)
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("private key is not RSA")
	}
	return rk, nil
}

func (a *AppAuth) jwt(now time.Time) (string, error) {
	enc := base64.RawURLEncoding
	hdr := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	pl, _ := json.Marshal(map[string]any{"iat": now.Add(-time.Minute).Unix(), "exp": now.Add(9 * time.Minute).Unix(), "iss": a.ClientID})
	msg := hdr + "." + enc.EncodeToString(pl)
	sum := sha256.Sum256([]byte(msg))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.Key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return msg + "." + enc.EncodeToString(sig), nil
}

// Token returns a valid installation token, minting a new one when the cached one is within two minutes of expiry.
func (a *AppAuth) Token(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now
	if a.Now != nil {
		now = a.Now
	}
	if a.token != "" && now().Add(2*time.Minute).Before(a.expires) {
		return a.token, nil
	}
	j, err := a.jwt(now())
	if err != nil {
		return "", err
	}
	hc := a.HC
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second}
	}
	base := strings.TrimRight(a.Base, "/")
	if base == "" {
		base = "https://api.github.com"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/app/installations/"+strconv.FormatInt(a.InstallationID, 10)+"/access_tokens", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+j)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("GitHub App token request: HTTP %d", resp.StatusCode)
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.Token == "" {
		return "", errors.New("GitHub App token request: unexpected answer")
	}
	a.token, a.expires = out.Token, out.ExpiresAt
	return a.token, nil
}
