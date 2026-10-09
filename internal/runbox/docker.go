package runbox

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"bob/internal/config"
)

// docker calls the subset of the Docker Engine API that runbox needs. Paths are unversioned, so
// the daemon's current API version is used.
type docker struct {
	base   string // scheme://host, e.g. https://docker:2376 or http://docker (unix socket)
	client *http.Client
}

func newDocker(cfg config.RunboxConfig) (*docker, error) {
	u, err := url.Parse(cfg.DockerHost)
	if err != nil {
		return nil, fmt.Errorf("parse dockerHost: %w", err)
	}
	// HTTP/1.1 only: exec output is streamed on a connection the daemon takes over.
	tr := &http.Transport{
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	switch u.Scheme {
	case "unix":
		path := u.Path
		tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		}
		return &docker{base: "http://docker", client: &http.Client{Transport: tr}}, nil
	case "tcp":
		tlsCfg, err := clientTLS(cfg)
		if err != nil {
			return nil, err
		}
		tr.TLSClientConfig = tlsCfg
		tr.DialContext = (&net.Dialer{Timeout: 10 * time.Second}).DialContext
		return &docker{base: "https://" + u.Host, client: &http.Client{Transport: tr}}, nil
	default:
		return nil, fmt.Errorf("dockerHost must be tcp:// or unix://, got %q", cfg.DockerHost)
	}
}

func clientTLS(cfg config.RunboxConfig) (*tls.Config, error) {
	cert, err := tls.X509KeyPair([]byte(cfg.ClientCert), []byte(cfg.ClientKey))
	if err != nil {
		return nil, fmt.Errorf("load runbox client certificate: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(cfg.CACert)) {
		return nil, errors.New("runbox caCert has no PEM certificate")
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		MinVersion:   tls.VersionTLS12,
		NextProtos:   []string{"http/1.1"},
	}, nil
}

// apiError is a non-2xx response; Docker puts the reason in {"message": ...}.
type apiError struct {
	Status  int
	Message string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("docker %d: %s", e.Status, e.Message)
}

func isNotFound(err error) bool {
	var ae *apiError
	return errors.As(err, &ae) && ae.Status == http.StatusNotFound
}

// do sends a request and returns the response for a 2xx status; the caller closes the body.
func (d *docker) do(ctx context.Context, method, path string, query url.Values, body io.Reader, contentType string) (*http.Response, error) {
	u := d.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		defer func() { _ = resp.Body.Close() }()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		var msg struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(raw, &msg) != nil || msg.Message == "" {
			msg.Message = strings.TrimSpace(string(raw))
		}
		return nil, &apiError{Status: resp.StatusCode, Message: msg.Message}
	}
	return resp, nil
}

// call sends JSON (when in is not nil) and decodes the JSON response into out (when not nil).
func (d *docker) call(ctx context.Context, method, path string, query url.Values, in, out any) error {
	var body io.Reader
	contentType := ""
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
		contentType = "application/json"
	}
	resp, err := d.do(ctx, method, path, query, body, contentType)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if out == nil {
		_, err = io.Copy(io.Discard, resp.Body)
		return err
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

type containerSummary struct {
	ID      string   `json:"Id"`
	Names   []string `json:"Names"`
	Created int64    `json:"Created"`
	State   string   `json:"State"`
}

// containerList lists containers (running or not) that have the label.
func (d *docker) containerList(ctx context.Context, label string) ([]containerSummary, error) {
	filters, _ := json.Marshal(map[string][]string{"label": {label}})
	var out []containerSummary
	err := d.call(ctx, http.MethodGet, "/containers/json", url.Values{"all": {"1"}, "filters": {string(filters)}}, nil, &out)
	return out, err
}

func (d *docker) containerRemove(ctx context.Context, id string) error {
	return d.call(ctx, http.MethodDelete, "/containers/"+id, url.Values{"force": {"1"}}, nil, nil)
}

func (d *docker) imageExists(ctx context.Context, name string) (bool, error) {
	err := d.call(ctx, http.MethodGet, "/images/"+name+"/json", nil, nil, nil)
	if isNotFound(err) {
		return false, nil
	}
	return err == nil, err
}

// imagePull pulls one tag (latest when the name has none) and waits until it is done.
func (d *docker) imagePull(ctx context.Context, name string) error {
	repo, tag := name, "latest"
	if i := strings.LastIndex(name, ":"); i > strings.LastIndex(name, "/") {
		repo, tag = name[:i], name[i+1:]
	}
	resp, err := d.do(ctx, http.MethodPost, "/images/create", url.Values{"fromImage": {repo}, "tag": {tag}}, nil, "")
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	// The progress stream reports a failed pull as {"error": ...} with status 200.
	dec := json.NewDecoder(resp.Body)
	for {
		var msg struct {
			Error string `json:"error"`
		}
		if err := dec.Decode(&msg); err == io.EOF {
			return nil
		} else if err != nil {
			return err
		}
		if msg.Error != "" {
			return fmt.Errorf("pull %s: %s", name, msg.Error)
		}
	}
}

type containerConfig struct {
	Image      string            `json:"Image"`
	Cmd        []string          `json:"Cmd"`
	Env        []string          `json:"Env,omitempty"`
	WorkingDir string            `json:"WorkingDir,omitempty"`
	User       string            `json:"User,omitempty"`
	Labels     map[string]string `json:"Labels,omitempty"`
	HostConfig hostConfig        `json:"HostConfig"`
}

type hostConfig struct {
	AutoRemove bool  `json:"AutoRemove"`
	PidsLimit  int64 `json:"PidsLimit,omitempty"`
}

func (d *docker) containerCreate(ctx context.Context, cfg containerConfig) (string, error) {
	var out struct {
		ID string `json:"Id"`
	}
	err := d.call(ctx, http.MethodPost, "/containers/create", nil, cfg, &out)
	return out.ID, err
}

func (d *docker) containerStart(ctx context.Context, id string) error {
	return d.call(ctx, http.MethodPost, "/containers/"+id+"/start", nil, nil, nil)
}

// copyTo extracts a tar archive into the container at dir.
func (d *docker) copyTo(ctx context.Context, id, dir string, tarball io.Reader) error {
	resp, err := d.do(ctx, http.MethodPut, "/containers/"+id+"/archive", url.Values{"path": {dir}}, tarball, "application/x-tar")
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// copyFrom returns a tar archive of path in the container; the caller closes it.
func (d *docker) copyFrom(ctx context.Context, id, path string) (io.ReadCloser, error) {
	resp, err := d.do(ctx, http.MethodGet, "/containers/"+id+"/archive", url.Values{"path": {path}}, nil, "")
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

func (d *docker) execCreate(ctx context.Context, id string, cmd []string) (string, error) {
	var out struct {
		ID string `json:"Id"`
	}
	in := map[string]any{"AttachStdout": true, "AttachStderr": true, "Cmd": cmd}
	err := d.call(ctx, http.MethodPost, "/containers/"+id+"/exec", nil, in, &out)
	return out.ID, err
}

// execStart starts the exec and returns its multiplexed stdout/stderr stream, which ends when the
// command exits. Without an Upgrade header the daemon answers 200 and streams until it closes the
// connection, so a plain HTTP client can read it.
func (d *docker) execStart(ctx context.Context, execID string) (io.ReadCloser, error) {
	raw, _ := json.Marshal(map[string]bool{"Detach": false, "Tty": false})
	resp, err := d.do(ctx, http.MethodPost, "/exec/"+execID+"/start", nil, bytes.NewReader(raw), "application/json")
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

func (d *docker) execExitCode(ctx context.Context, execID string) (int, error) {
	var out struct {
		ExitCode int `json:"ExitCode"`
	}
	err := d.call(ctx, http.MethodGet, "/exec/"+execID+"/json", nil, nil, &out)
	return out.ExitCode, err
}

// stats returns the container's total CPU time (microseconds) and memory usage (KiB).
func (d *docker) stats(ctx context.Context, id string) (cpu, mem int, err error) {
	var out struct {
		CPUStats struct {
			CPUUsage struct {
				TotalUsage uint64 `json:"total_usage"`
			} `json:"cpu_usage"`
		} `json:"cpu_stats"`
		MemoryStats struct {
			Usage uint64 `json:"usage"`
		} `json:"memory_stats"`
	}
	err = d.call(ctx, http.MethodGet, "/containers/"+id+"/stats", url.Values{"stream": {"0"}, "one-shot": {"1"}}, nil, &out)
	return int(out.CPUStats.CPUUsage.TotalUsage / 1000), int(out.MemoryStats.Usage / 1024), err
}

// demux splits Docker's multiplexed stream (8-byte header: stream, 0, 0, 0, big-endian size) into
// stdout and stderr.
func demux(stdout, stderr io.Writer, r io.Reader) error {
	var hdr [8]byte
	for {
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		size := int64(hdr[4])<<24 | int64(hdr[5])<<16 | int64(hdr[6])<<8 | int64(hdr[7])
		var w io.Writer
		switch hdr[0] {
		case 1:
			w = stdout
		case 2:
			w = stderr
		default:
			w = io.Discard
		}
		if _, err := io.CopyN(w, r, size); err != nil {
			return err
		}
	}
}
