package jit

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// build makes an encoded JIT config from file name -> JSON content.
func build(t *testing.T, files map[string]string) string {
	t.Helper()
	m := map[string]string{}
	for k, v := range files {
		m[k] = b64(v)
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b64(string(raw))
}

const tenant = "https://pipelinesghubeus22.actions.githubusercontent.com/TENANTabc123/"

// Shaped like a real config from GitHub (November 2026), with made-up values.
const goodRunner = `{"AgentId":42,"AgentName":"homelab-1-ab12cd","DisableUpdate":true,"Ephemeral":true,"PoolId":1,
 "PoolName":"Default","RunnerScaleSetId":1,"RunnerScaleSetName":"homelab",
 "ServerUrl":"https://pipelinesghubeus22.actions.githubusercontent.com/TENANTabc123/",
 "ServerUrlV2":"https://broker.actions.githubusercontent.com/","UseV2Flow":true,"WorkFolder":"/tmp/runner"}`

const goodCreds = `{"Scheme":"OAuth","Data":{"ClientId":"x",
 "AuthorizationUrl":"https://pipelinesghubeus22.actions.githubusercontent.com/TENANTabc123/_apis/oauth2/token"}}`

const goodRSA = `{"d":"AA==","dp":"AA==","dq":"AA==","exponent":"AQAB","inverseQ":"AA==","modulus":"AA==","p":"AA==","q":"AA=="}`

var expect = Expect{TenantURL: tenant, RunnerName: "homelab-1-ab12cd", WorkFolder: "/tmp/runner"}

func good(t *testing.T) map[string]string {
	return map[string]string{".runner": goodRunner, ".credentials": goodCreds, ".credentials_rsaparams": goodRSA}
}

func TestValidateAccepts(t *testing.T) {
	enc := build(t, good(t))
	if err := Validate(enc, expect); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	e := expect
	e.TenantURL = strings.TrimSuffix(tenant, "/") // with or without the trailing slash
	if err := Validate(enc, e); err != nil {
		t.Fatalf("tenant without slash rejected: %v", err)
	}
}

func TestNoTenantNamesTheServer(t *testing.T) {
	e := expect
	e.TenantURL = ""
	err := Validate(build(t, good(t)), e)
	if !errors.Is(err, ErrNoTenant) || !strings.Contains(err.Error(), "TENANTabc123") {
		t.Fatalf("got %v", err)
	}
}

func TestValidateRejects(t *testing.T) {
	with := func(file, old, new string) string {
		f := good(t)
		if !strings.Contains(f[file], old) {
			t.Fatalf("fixture has no %q", old)
		}
		f[file] = strings.Replace(f[file], old, new, 1)
		return build(t, f)
	}
	host := "pipelinesghubeus22.actions.githubusercontent.com"
	cases := map[string]string{
		"empty":             "",
		"not base64":        "!!!",
		"not json":          b64("hello"),
		"not strings":       b64(`{".runner": 5}`),
		"no .runner":        build(t, map[string]string{".credentials": goodCreds}),
		"other tenant":      with(".runner", "TENANTabc123", "TENANTevil"),
		"tenant prefix":     with(".runner", "TENANTabc123/", "TENANTabc1234/"),
		"other host":        with(".runner", host, "pipelinesghubeus1.actions.githubusercontent.com"),
		"evil server":       with(".runner", host, "evil.example"),
		"dot segments":      with(".runner", "TENANTabc123/", "TENANTabc123/../TENANTevil/"),
		"encoded path":      with(".runner", "TENANTabc123/", "TENANTabc123/%2e%2e/x/"),
		"token url":         with(".credentials", host+"/TENANTabc123", "evil.example/TENANTabc123"),
		"token other tnt":   with(".credentials", "TENANTabc123", "TENANTevil"),
		"evil broker":       with(".runner", "broker.actions.githubusercontent.com", "broker.evil.example"),
		"broker port":       with(".runner", "broker.actions.githubusercontent.com/", "broker.actions.githubusercontent.com:8443/"),
		"http":              with(".runner", "https://broker", "http://broker"),
		"userinfo":          with(".runner", "https://broker", "https://u:p@broker"),
		"wrong name":        with(".runner", "homelab-1-ab12cd", "homelab-2-ab12cd"),
		"no name":           with(".runner", `"AgentName":"homelab-1-ab12cd",`, ""),
		"workfolder":        with(".runner", `"/tmp/runner"`, `"/"`),
		"url in rsa params": with(".credentials_rsaparams", `"AA=="`, `"https://evil.example/"`),
		"non-json file":     build(t, map[string]string{".runner": goodRunner, "extra": "plain text"}),
		"too big":           strings.Repeat("A", MaxEncodedSize+4),
	}
	for name, enc := range cases {
		t.Run(name, func(t *testing.T) {
			if err := Validate(enc, expect); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func TestBadTenantConfig(t *testing.T) {
	for _, tu := range []string{"http://pipelines.example/x/", "https://pipelines.example/", "https://pipelines.example"} {
		e := expect
		e.TenantURL = tu
		if err := Validate(build(t, good(t)), e); err == nil {
			t.Errorf("tenant %q accepted", tu)
		}
	}
}

func TestShapeHasNoSecrets(t *testing.T) {
	s := Shape(build(t, good(t)))
	for _, secret := range []string{"TENANTabc123", "AQAB", "homelab-1-ab12cd"} {
		if strings.Contains(s, secret) {
			t.Errorf("Shape leaks %q: %s", secret, s)
		}
	}
	if !strings.Contains(s, "AgentName") || !strings.Contains(s, "https://broker.actions.githubusercontent.com") {
		t.Errorf("Shape: %s", s)
	}
}

func TestHostAllowed(t *testing.T) {
	pats := []string{"github.com", ".actions.githubusercontent.com"}
	for host, want := range map[string]bool{
		"github.com": true, "GITHUB.com.": true, "evilgithub.com": false,
		"broker.actions.githubusercontent.com": true, "actions.githubusercontent.com": false,
		"x.actions.githubusercontent.com.evil": false, "": false,
	} {
		if got := HostAllowed(host, pats); got != want {
			t.Errorf("HostAllowed(%q) = %v, want %v", host, got, want)
		}
	}
}
