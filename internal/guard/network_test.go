package guard

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func fakeDockerSocket(t *testing.T, handler http.HandlerFunc) *SocketDocker {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "d.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(handler)
	srv.Listener = l
	srv.Start()
	t.Cleanup(srv.Close)
	return NewSocketDocker(sock)
}

func TestNetworkEnsureChecksSubnet(t *testing.T) {
	const want = "172.41.1.0/24"
	cases := []struct {
		name, have string
		wantErr    bool
	}{
		{"matching subnet is reused", want, false},
		{"other subnet is refused", "172.18.0.0/16", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := fakeDockerSocket(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
				}
				w.Write([]byte(`{"IPAM":{"Config":[{"Subnet":"` + tc.have + `"}]}}`))
			})
			err := d.NetworkEnsure(context.Background(), "n", want, nil)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), want) {
				t.Errorf("error should name the subnet: %v", err)
			}
		})
	}
}
