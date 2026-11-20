package onboard

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

var serverURLRE = regexp.MustCompile(`no tenant_url pinned: this config's ServerUrl is \\?"(https://[^"\\]+)\\?"`)

// FindTenant returns the tenant URL the guard asked you to pin, from its log. Several different URLs in one log is
// an error: pinning is a trust decision and must not guess.
func FindTenant(logs string) (string, error) {
	seen := map[string]bool{}
	var last string
	for _, m := range serverURLRE.FindAllStringSubmatch(logs, -1) {
		seen[m[1]] = true
		last = m[1]
	}
	switch len(seen) {
	case 0:
		return "", errors.New("the guard has not refused a registration yet: run a job on the pool first (QUICKSTART step 6), then run this again")
	case 1:
		return last, ValidTenant(last)
	}
	return "", fmt.Errorf("the guard saw %d different tenant URLs; look at them yourself and set tenant_url by hand", len(seen))
}

// ValidTenant checks the URL has the shape of an Actions tenant URL.
func ValidTenant(s string) error {
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" || !strings.HasSuffix(u.Hostname(), ".actions.githubusercontent.com") || strings.Trim(u.Path, "/") == "" {
		return fmt.Errorf("%q does not look like https://<host>.actions.githubusercontent.com/<tenant>/", s)
	}
	return nil
}

var tenantLineRE = regexp.MustCompile(`(?m)^#?\s*tenant_url:.*$`)

// SetTenant writes tenant_url into a guard config, replacing the (commented) line the examples carry.
func SetTenant(cfg, tenant string) (string, error) {
	if err := ValidTenant(tenant); err != nil {
		return "", err
	}
	line := "tenant_url: " + tenant
	if tenantLineRE.MatchString(cfg) {
		return tenantLineRE.ReplaceAllLiteralString(cfg, line), nil
	}
	return strings.Replace(cfg, "github_url:", line+"\ngithub_url:", 1), nil
}
