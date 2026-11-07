package hook

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/harness"
)

type echoEngine struct{ token string }

func (e echoEngine) Accept(token string) bool { return token == e.token }

func (e echoEngine) Serve(c io.ReadWriteCloser) error {
	defer c.Close()
	r := bufio.NewReader(c)
	line, err := r.ReadString('\n')
	if err != nil {
		return err
	}
	_, err = io.WriteString(c, "echo "+line)
	return err
}

// The engine route upgrades a connection with the right token and carries lines both ways,
// past the server's post timeouts; any other token is 404, a plain post 426.
func TestEngineRoute(t *testing.T) {
	sock := filepath.Join(shortDir(t), "hook.sock")
	l, err := control.Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(newTestPanes(), func(string, harness.Envelope) string { return "" }, t.Logf)
	token := strings.Repeat("ab", 32)
	s.SetEngine(echoEngine{token: token})
	go s.Serve(l)
	t.Cleanup(func() { s.Close() })

	dial := func(path string, upgrade bool) (net.Conn, *bufio.Reader, *http.Response) {
		t.Helper()
		c, err := net.Dial("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		req := "POST " + path + " HTTP/1.1\r\nHost: hab\r\nContent-Length: 0\r\n"
		if upgrade {
			req += "Connection: Upgrade\r\nUpgrade: " + EngineProtocol + "\r\n"
		}
		io.WriteString(c, req+"\r\n")
		r := bufio.NewReader(c)
		resp, err := http.ReadResponse(r, nil)
		if err != nil {
			t.Fatal(err)
		}
		return c, r, resp
	}
	c, _, resp := dial(EnginePath+strings.Repeat("cd", 32), true)
	c.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("a wrong token: %d", resp.StatusCode)
	}
	c, _, resp = dial(EnginePath+token, false)
	c.Close()
	if resp.StatusCode != http.StatusUpgradeRequired {
		t.Fatalf("no upgrade: %d", resp.StatusCode)
	}
	c, r, resp := dial(EnginePath+token, true)
	defer c.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade: %d", resp.StatusCode)
	}
	time.Sleep(11 * time.Second / 10) // a pause is fine: the post timeouts do not apply
	io.WriteString(c, `{"type":"ready"}`+"\n")
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	got, err := r.ReadString('\n')
	if err != nil || got != `echo {"type":"ready"}`+"\n" {
		t.Fatalf("%q %v", got, err)
	}
}
