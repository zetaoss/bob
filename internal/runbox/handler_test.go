package runbox

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeDocker serves the Docker Engine API calls runbox makes. The exec writes execOut; when
// hang is set it never ends, like a command that runs past its timeout.
type fakeDocker struct {
	mu         sync.Mutex
	calls      []string
	created    containerConfig
	copied     map[string]string
	removed    []string
	hasImage   bool
	execOut    []byte
	hang       bool
	exitCode   int
	pngs       map[string][]byte
	staleList  string
	failCreate bool
}

func frame(stream byte, s string) []byte {
	hdr := make([]byte, 8)
	hdr[0] = stream
	binary.BigEndian.PutUint32(hdr[4:], uint32(len(s)))
	return append(hdr, s...)
}

func (f *fakeDocker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	f.mu.Unlock()
	p := r.URL.Path
	switch {
	case r.Method == http.MethodGet && p == "/containers/json":
		if r.URL.Query().Get("filters") != `{"label":["bob.runbox"]}` {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if f.staleList == "" {
			f.staleList = "[]"
		}
		_, _ = io.WriteString(w, f.staleList)
	case r.Method == http.MethodGet && strings.HasPrefix(p, "/images/"):
		if !f.hasImage {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"No such image"}`)
			return
		}
		_, _ = io.WriteString(w, `{}`)
	case r.Method == http.MethodPost && p == "/images/create":
		if r.URL.Query().Get("tag") == "v1" {
			_, _ = io.WriteString(w, `{"error":"unexpected tag"}`)
			return
		}
		f.hasImage = true
		_, _ = io.WriteString(w, `{"status":"Pulling"}`+"\n"+`{"status":"Done"}`+"\n")
	case r.Method == http.MethodPost && p == "/containers/create" && f.failCreate:
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"message":"no space left on device"}`)
	case r.Method == http.MethodPost && p == "/containers/create":
		_ = json.NewDecoder(r.Body).Decode(&f.created)
		_, _ = io.WriteString(w, `{"Id":"c0123456789abcdef"}`)
	case r.Method == http.MethodPut && p == "/containers/c0123456789abcdef/archive":
		f.copied = map[string]string{}
		tr := tar.NewReader(r.Body)
		for {
			hdr, err := tr.Next()
			if err != nil {
				break
			}
			body, _ := io.ReadAll(tr)
			f.copied[hdr.Name] = string(body)
		}
	case r.Method == http.MethodGet && p == "/containers/c0123456789abcdef/archive":
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		_ = tw.WriteHeader(&tar.Header{Name: "user01/", Typeflag: tar.TypeDir, Mode: 0755})
		for name, data := range f.pngs {
			_ = tw.WriteHeader(&tar.Header{Name: "user01/" + name, Mode: 0644, Size: int64(len(data))})
			_, _ = tw.Write(data)
		}
		_ = tw.Close()
		_, _ = w.Write(buf.Bytes())
	case r.Method == http.MethodPost && p == "/containers/c0123456789abcdef/start":
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && p == "/containers/c0123456789abcdef/exec":
		_, _ = io.WriteString(w, `{"Id":"e1"}`)
	case r.Method == http.MethodPost && p == "/exec/e1/start":
		w.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
		_, _ = w.Write(f.execOut)
		w.(http.Flusher).Flush()
		if f.hang {
			<-r.Context().Done()
		}
	case r.Method == http.MethodGet && p == "/exec/e1/json":
		_ = json.NewEncoder(w).Encode(map[string]int{"ExitCode": f.exitCode})
	case r.Method == http.MethodGet && p == "/containers/c0123456789abcdef/stats":
		f.mu.Lock()
		n := strings.Count(strings.Join(f.calls, "\n"), "/stats")
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"cpu_stats":    map[string]any{"cpu_usage": map[string]any{"total_usage": n * 5000000}},
			"memory_stats": map[string]any{"usage": 2048 * 1024},
		})
	case r.Method == http.MethodDelete && strings.HasPrefix(p, "/containers/"):
		f.mu.Lock()
		f.removed = append(f.removed, strings.TrimPrefix(p, "/containers/"))
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"unexpected `+r.Method+" "+p+`"}`)
	}
}

func newTestHandler(t *testing.T, f *fakeDocker) *Handler {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return &Handler{
		box: &box{docker: &docker{base: srv.URL, client: srv.Client()}, log: slog.New(slog.NewTextHandler(io.Discard, nil))},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func post(h http.Handler, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
	return rec
}

func TestServeHTTP_Lang(t *testing.T) {
	f := &fakeDocker{
		execOut:  append(append(frame(1, "hello\nwor"), frame(2, "warn\n")...), frame(1, "ld")...),
		exitCode: 3,
		pngs:     map[string][]byte{"a.png": []byte("A"), "notes.txt": []byte("x"), "big.png": make([]byte, maxImageSize+1)},
		staleList: `[{"Id":"old0123456789","Names":["/old"],"Created":1,"State":"running"},` +
			`{"Id":"new0123456789","Created":` + jsonNow() + `,"State":"running"}]`,
	}
	h := newTestHandler(t, f)
	rec := post(h, "/lang", `{"lang":"python","files":[{"body":"print('hello')"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var res LangResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.Logs, "|") != "1hello|2warn|1world" || res.Code != 3 || res.CPU != 5000 || res.MEM != 2048 || res.Timedout {
		t.Fatalf("unexpected result: %+v", res)
	}
	if len(res.Images) != 1 || res.Images[0] != "QQ==" {
		t.Fatalf("images: %v", res.Images)
	}
	if f.created.Image != langImage("python") || f.created.WorkingDir != "/home/user01" ||
		f.created.Labels["bob.runbox"] != "1" || !f.created.HostConfig.AutoRemove || f.created.HostConfig.PidsLimit != 100 || f.created.Cmd[0] != "sleep" {
		t.Fatalf("created: %+v", f.created)
	}
	if f.copied["/home/user01/runbox.py"] != "print('hello')" {
		t.Fatalf("copied: %+v", f.copied)
	}
	// The stale container and the run's container are removed; the image was pulled.
	if strings.Join(f.removed, ",") != "old0123456789,c0123456789abcdef" {
		t.Fatalf("removed: %v", f.removed)
	}
	if !strings.Contains(strings.Join(f.calls, "\n"), "POST /images/create") {
		t.Fatalf("image not pulled: %v", f.calls)
	}
}

func TestServeHTTP_LangTimeout(t *testing.T) {
	f := &fakeDocker{hasImage: true, execOut: frame(1, "started\n"), hang: true}
	h := newTestHandler(t, f)
	languages["test-sleep"] = language{command: "sleep 100", timeout: 50 * time.Millisecond}
	defer delete(languages, "test-sleep")

	rec := post(h, "/lang", `{"lang":"test-sleep","files":[{"body":"x"}]}`)
	var res LangResult
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if rec.Code != http.StatusOK || !res.Timedout || strings.Join(res.Logs, "|") != "1started" {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(f.removed) != 1 {
		t.Fatalf("container not removed: %v", f.removed)
	}
}

func TestServeHTTP_Notebook(t *testing.T) {
	nb := `{"cells":[{"outputs":[{"output_type":"stream","name":"stdout","text":["2\n"]}]}]}`
	f := &fakeDocker{hasImage: true, execOut: frame(1, nb+"\n")}
	h := newTestHandler(t, f)
	rec := post(h, "/notebook", `{"lang":"python","sources":["print(2)"]}`)
	want := `{"outputsList":[[{"output_type":"stream","name":"stdout","text":["2\n"]}]],"cpu":5000,"mem":2048,"time":`
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Body.String(), want) {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if f.created.Image != "jmnote/runbox:python-notebook" {
		t.Fatalf("created: %+v", f.created)
	}
}

func TestServeHTTP_Errors(t *testing.T) {
	h := newTestHandler(t, &fakeDocker{hasImage: true})
	cases := []struct {
		method, path, body string
		code               int
		err                string
	}{
		{http.MethodPost, "/lang", `{"lang":"cobol","files":[{"body":"x"}]}`, 400, "invalid language"},
		{http.MethodPost, "/lang", `{"lang":"python"}`, 400, "no files"},
		{http.MethodPost, "/notebook", `{"lang":"python"}`, 400, "no sources"},
		{http.MethodPost, "/lang", `{`, 400, "unexpected EOF"},
		{http.MethodGet, "/lang", ``, 405, "method not allowed"},
		{http.MethodPost, "/browse", `{}`, 404, "not found"},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, strings.NewReader(c.body)))
		var body struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != c.code || body.Error != c.err {
			t.Errorf("%s %s %s: code=%d body=%s", c.method, c.path, c.body, rec.Code, rec.Body.String())
		}
	}
}

func TestServeHTTP_DockerErrorIs500(t *testing.T) {
	f := &fakeDocker{hasImage: true, failCreate: true}
	h := newTestHandler(t, f)
	rec := post(h, "/lang", `{"lang":"python","files":[{"body":"x"}]}`)
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "no space left") {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestImagePull(t *testing.T) {
	h := newTestHandler(t, &fakeDocker{})
	ctx := context.Background()
	// The fake rejects tag v1; a port in the registry host is not a tag (latest is pulled).
	if err := h.box.docker.imagePull(ctx, "localhost:5000/img"); err != nil {
		t.Fatal(err)
	}
	if err := h.box.docker.imagePull(ctx, "example/img:v1"); err == nil || !strings.Contains(err.Error(), "unexpected tag") {
		t.Fatalf("expected pull error, got %v", err)
	}
}

func jsonNow() string {
	b, _ := json.Marshal(time.Now().Unix())
	return string(b)
}
