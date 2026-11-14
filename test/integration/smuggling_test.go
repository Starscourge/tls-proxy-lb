package integration

import (
	"bufio"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// smugglingCase is one file of testdata/smuggling: comment lines starting
// with "#", one of them "want: STATUS" (or, in ok/, "paths: P..."), and
// then the payload as a Go string body, so "\r\n" stands for CRLF and
// every byte is visible.
type smugglingCase struct {
	name    string
	want    int
	paths   []string
	payload []byte
}

func loadSmuggling(t *testing.T, dir string) []smugglingCase {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.txt"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no cases in %s: %v", dir, err)
	}
	var out []smugglingCase
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		c := smugglingCase{name: strings.TrimSuffix(filepath.Base(f), ".txt")}
		var body []string
		for line := range strings.Lines(string(data)) {
			line = strings.TrimSuffix(line, "\n")
			switch {
			case strings.HasPrefix(line, "# want: "):
				c.want, _ = strconv.Atoi(strings.TrimPrefix(line, "# want: "))
			case strings.HasPrefix(line, "# paths: "):
				c.paths = strings.Fields(strings.TrimPrefix(line, "# paths: "))
			case strings.HasPrefix(line, "#"):
			default:
				body = append(body, line)
			}
		}
		s, err := strconv.Unquote(`"` + strings.Join(body, "") + `"`)
		if err != nil {
			t.Fatalf("%s: payload: %v", f, err)
		}
		c.payload = []byte(s)
		out = append(out, c)
	}
	return out
}

// Done when: a corpus of smuggling payloads, every case refused, and the
// backend's request log never shows a smuggled second request.
//
// Each payload in testdata/smuggling is one request built to make two
// parsers disagree about where it ends, followed by "GET /smuggled-<case>",
// which a backend that disagreed with the proxy would read as a second
// request. Each must get a 4xx and a closed connection, and nothing whose
// path starts with /smuggled may reach the backend. The cases in ok/ are
// well-formed and must be served, every request in them, so the check
// cannot pass by refusing everything.
func TestSmugglingCorpus(t *testing.T) {
	ForEachScheme(t, func(t *testing.T, scheme string) {
		b := StartBackend(t, "b1", BackendOptions{Quiet: true})
		p := StartProxy(t, b.Addr, ProxyOptions{Scheme: scheme, Logger: discard()})

		for _, c := range loadSmuggling(t, "testdata/smuggling") {
			t.Run(c.name, func(t *testing.T) {
				conn := p.Dial(t, "a.test", nil)
				conn.SetDeadline(time.Now().Add(10 * time.Second))
				if _, err := conn.Write(c.payload); err != nil {
					t.Fatal(err)
				}
				br := bufio.NewReader(conn)
				resp, err := http.ReadResponse(br, nil)
				if err != nil {
					t.Fatalf("no response: %v", err)
				}
				io.Copy(io.Discard, resp.Body)
				if resp.StatusCode != c.want {
					t.Errorf("status %d, want %d", resp.StatusCode, c.want)
				}
				if !resp.Close {
					t.Error("refusal did not say Connection: close")
				}
				if _, err := br.ReadByte(); err == nil {
					t.Error("connection still open after the refusal: read another byte")
				}
			})
		}
		for _, r := range b.Requests() {
			if strings.HasPrefix(r.Path, "/smuggled") {
				t.Errorf("the backend saw a smuggled request: %s %s", r.Method, r.Path)
			}
		}
		b.ResetRequests()

		for _, c := range loadSmuggling(t, "testdata/smuggling/ok") {
			t.Run("ok/"+c.name, func(t *testing.T) {
				b.ResetRequests()
				conn := p.Dial(t, "a.test", nil)
				conn.SetDeadline(time.Now().Add(10 * time.Second))
				if _, err := conn.Write(c.payload); err != nil {
					t.Fatal(err)
				}
				br := bufio.NewReader(conn)
				for i := range c.paths {
					resp, err := http.ReadResponse(br, nil)
					if err != nil {
						t.Fatalf("response %d: %v", i, err)
					}
					io.Copy(io.Discard, resp.Body)
					if resp.StatusCode != 200 {
						t.Fatalf("response %d: status %d", i, resp.StatusCode)
					}
				}
				var got []string
				for _, r := range b.Requests() {
					got = append(got, r.Path)
				}
				if strings.Join(got, " ") != strings.Join(c.paths, " ") {
					t.Fatalf("backend saw %v, want %v", got, c.paths)
				}
			})
		}
	})
}
