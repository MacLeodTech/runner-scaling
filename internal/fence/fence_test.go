package fence

import (
	"net"
	"net/netip"
	"os"
	"testing"
	"time"
)

// These tests change the kernel's firewall. They run only when FENCE_TEST=1 and must run in a network namespace of
// their own (a throwaway container with NET_ADMIN, not the host's network):
//
//	go test -c -o /tmp/fence.test ./internal/fence && docker run --rm --cap-add NET_ADMIN -v /tmp/fence.test:/t:ro -e FENCE_TEST=1 alpine /t -test.v
func needKernel(t *testing.T) {
	t.Helper()
	if os.Getenv("FENCE_TEST") != "1" {
		t.Skip("set FENCE_TEST=1 inside a disposable network namespace")
	}
}

func TestNormalize(t *testing.T) {
	good := Spec{Src4: []netip.Prefix{netip.MustParsePrefix("172.30.0.0/16")}}
	if err := good.Normalize(); err != nil {
		t.Fatal(err)
	}
	for name, s := range map[string]Spec{
		"empty":       {},
		"unmasked":    {Src4: []netip.Prefix{netip.MustParsePrefix("172.30.1.5/16")}},
		"too wide":    {Src4: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}},
		"v6 as v4":    {Src4: []netip.Prefix{netip.MustParsePrefix("fd00::/64")}},
		"v4 as v6":    {Src6: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}},
		"wide v6":     {Src6: []netip.Prefix{netip.MustParsePrefix("::/0")}},
		"bad block":   {Src4: good.Src4, Block4: []netip.Prefix{netip.MustParsePrefix("fc00::/7")}},
		"bad block 6": {Src4: good.Src4, Block6: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}},
	} {
		if err := s.Normalize(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestRulesCount(t *testing.T) {
	s := Spec{Src4: []netip.Prefix{netip.MustParsePrefix("172.30.0.0/16")}, Src6: []netip.Prefix{netip.MustParsePrefix("fd00:a::/64")}}
	if err := s.Normalize(); err != nil {
		t.Fatal(err)
	}
	if got, want := len(s.rules()), len(DefaultBlock4)+1+len(DefaultBlock6)+1; got != want {
		t.Fatalf("rules = %d, want %d", got, want)
	}
}

func TestApplyVerifyRepair(t *testing.T) {
	needKernel(t)
	s := Spec{Src4: []netip.Prefix{netip.MustParsePrefix("172.30.0.0/16")}, Src6: []netip.Prefix{netip.MustParsePrefix("fd00:a::/64")}}
	if err := s.Normalize(); err != nil {
		t.Fatal(err)
	}
	_ = Remove()
	if st, err := s.Verify(); err != nil || st.OK {
		t.Fatalf("before apply: %+v %v", st, err)
	}
	if err := s.Apply(); err != nil {
		t.Fatal(err)
	}
	if st, err := s.Verify(); err != nil || !st.OK {
		t.Fatalf("after apply: %+v %v", st, err)
	}
	if err := s.Apply(); err != nil { // replacing an existing table works
		t.Fatal(err)
	}
	if st, _ := s.Verify(); !st.OK {
		t.Fatalf("after re-apply: %+v", st)
	}
	// A different spec is not "OK" against the installed table.
	other := Spec{Src4: []netip.Prefix{netip.MustParsePrefix("172.31.0.0/16")}}
	_ = other.Normalize()
	if st, _ := other.Verify(); st.OK {
		t.Fatal("a different spec verified against the installed rules")
	}
	if err := Remove(); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.Verify(); st.OK {
		t.Fatal("verified with the table removed")
	}
	if err := Remove(); err != nil { // removing nothing is fine
		t.Fatal(err)
	}
}

// Traffic from a fenced source to the host is dropped, replies to what the host started are not, and removing the
// table restores everything. 127.0.0.0/8 stands in for a runner network: it reaches the input hook without routing.
func TestInputFunctional(t *testing.T) {
	needKernel(t)
	s := Spec{Src4: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}}
	if err := s.Normalize(); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	dial := func() error {
		c, err := net.DialTimeout("tcp4", l.Addr().String(), 800*time.Millisecond)
		if err == nil {
			c.Close()
		}
		return err
	}
	_ = Remove()
	if err := dial(); err != nil {
		t.Fatalf("without the fence: %v", err)
	}
	if err := s.Apply(); err != nil {
		t.Fatal(err)
	}
	if err := dial(); err == nil {
		t.Fatal("connection from a fenced source reached the host")
	}
	st, _ := s.Verify()
	if !st.OK || st.DroppedInput == 0 {
		t.Fatalf("status %+v: want OK and dropped input packets counted", st)
	}
	if err := Remove(); err != nil {
		t.Fatal(err)
	}
	if err := dial(); err != nil {
		t.Fatalf("after removal: %v", err)
	}
}
