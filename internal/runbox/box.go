package runbox

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// staleAge is how long a container may live: containers sleep this long, and runbox containers
// older than this are removed before each run.
const staleAge = 300 * time.Second

// containerLabel marks the containers runbox creates. Only these are pruned, so other containers
// on the daemon are left alone.
const containerLabel = "bob.runbox"

const maxImageSize = 100 << 10 // PNG files larger than 100 KiB are not collected

// boxOpts describes one run: Command runs with Shell -c in a fresh container of Image after Files
// are copied in (absolute paths).
type boxOpts struct {
	Image         string
	Command       string
	Shell         string
	Env           []string
	Files         []File
	User          string
	WorkingDir    string
	Timeout       time.Duration
	CollectStats  bool
	CollectImages int // number of PNG files to collect from WorkingDir; 0 collects none
}

type File struct {
	Name string `json:"name"`
	Body string `json:"body"`
}

// Log is one output line; Stream is 1 for stdout and 2 for stderr.
type Log struct {
	Stream int
	Log    string
}

type boxResult struct {
	Logs     []Log
	Code     int
	CPU      int // microseconds of CPU time
	MEM      int // KiB
	Time     int // milliseconds
	Timedout bool
	Images   []string // base64 PNG
}

// streams returns stdout and stderr, one line per log.
func (r *boxResult) streams() (string, string) {
	var stdout, stderr strings.Builder
	for _, l := range r.Logs {
		if l.Stream == 1 {
			stdout.WriteString(l.Log + "\n")
		} else {
			stderr.WriteString(l.Log + "\n")
		}
	}
	return stdout.String(), stderr.String()
}

type box struct {
	docker *docker
	log    *slog.Logger
}

func (b *box) run(ctx context.Context, opts boxOpts) (*boxResult, error) {
	b.pruneStale(ctx)
	if err := b.ensureImage(ctx, opts.Image); err != nil {
		return nil, fmt.Errorf("image: %w", err)
	}
	id, err := b.docker.containerCreate(ctx, containerConfig{
		Image:      opts.Image,
		Cmd:        []string{"sleep", strconv.Itoa(int(staleAge.Seconds()))},
		Env:        opts.Env,
		WorkingDir: opts.WorkingDir,
		User:       opts.User,
		Labels:     map[string]string{containerLabel: "1"},
		HostConfig: hostConfig{AutoRemove: true, PidsLimit: 100},
	})
	if err != nil {
		return nil, fmt.Errorf("create container: %w", err)
	}
	// Remove the container even when the request is canceled.
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if err := b.docker.containerRemove(cleanup, id); err != nil && !isNotFound(err) {
			b.log.Error("runbox: remove container failed", "id", short(id), "err", err)
		}
	}()
	tarball, err := tarFiles(opts.Files)
	if err != nil {
		return nil, err
	}
	if err := b.docker.copyTo(ctx, id, "/", tarball); err != nil {
		return nil, fmt.Errorf("copy files: %w", err)
	}
	if err := b.docker.containerStart(ctx, id); err != nil {
		return nil, fmt.Errorf("start container: %w", err)
	}
	res := &boxResult{}
	if err := b.execute(ctx, id, opts, res); err != nil {
		return nil, fmt.Errorf("execute: %w", err)
	}
	if opts.CollectImages > 0 {
		if err := b.collectImages(ctx, id, opts, res); err != nil {
			return nil, fmt.Errorf("collect images: %w", err)
		}
	}
	return res, nil
}

// pruneStale removes runbox containers left behind (for example by a crashed run).
func (b *box) pruneStale(ctx context.Context) {
	list, err := b.docker.containerList(ctx, containerLabel)
	if err != nil {
		b.log.Error("runbox: list containers failed", "err", err)
		return
	}
	for _, c := range list {
		age := time.Since(time.Unix(c.Created, 0))
		if c.State != "removing" && age <= staleAge {
			continue
		}
		name := ""
		if len(c.Names) > 0 {
			name = c.Names[0]
		}
		if err := b.docker.containerRemove(ctx, c.ID); err != nil && !isNotFound(err) {
			b.log.Error("runbox: remove stale container failed", "id", short(c.ID), "name", name, "age", age.Round(time.Second).String(), "err", err)
			continue
		}
		b.log.Info("runbox: removed stale container", "id", short(c.ID), "name", name, "age", age.Round(time.Second).String())
	}
}

func (b *box) ensureImage(ctx context.Context, image string) error {
	ok, err := b.docker.imageExists(ctx, image)
	if err != nil || ok {
		return err
	}
	b.log.Info("runbox: pulling image", "image", image)
	return b.docker.imagePull(ctx, image)
}

func tarFiles(files []File) (io.Reader, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range files {
		if err := tw.WriteHeader(&tar.Header{Name: f.Name, Mode: 0644, Size: int64(len(f.Body))}); err != nil {
			return nil, err
		}
		if _, err := io.WriteString(tw, f.Body); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return &buf, nil
}

func (b *box) execute(ctx context.Context, id string, opts boxOpts, res *boxResult) error {
	execID, err := b.docker.execCreate(ctx, id, []string{opts.Shell, "-c", opts.Command})
	if err != nil {
		return err
	}
	var startCPU int
	if opts.CollectStats {
		if startCPU, _, err = b.docker.stats(ctx, id); err != nil {
			return err
		}
	}
	stream, err := b.docker.execStart(ctx, execID)
	if err != nil {
		return err
	}
	defer func() { _ = stream.Close() }()

	stdout := &logWriter{stream: 1, logs: &res.Logs}
	stderr := &logWriter{stream: 2, logs: &res.Logs}
	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- demux(stdout, stderr, stream) }()

	timer := time.NewTimer(opts.Timeout)
	defer timer.Stop()
	select {
	case err := <-done:
		if err != nil {
			return err
		}
	case <-timer.C:
		res.Timedout = true
		// Stop reading; the command keeps running until the container is removed.
		_ = stream.Close()
		<-done
	case <-ctx.Done():
		_ = stream.Close()
		<-done
		return ctx.Err()
	}
	res.Time = int(time.Since(start).Milliseconds())
	stdout.flush()
	stderr.flush()

	if opts.CollectStats {
		endCPU, mem, err := b.docker.stats(ctx, id)
		if err != nil {
			return err
		}
		res.CPU = endCPU - startCPU
		res.MEM = mem
	}
	res.Code, err = b.docker.execExitCode(ctx, execID)
	return err
}

func (b *box) collectImages(ctx context.Context, id string, opts boxOpts, res *boxResult) error {
	rc, err := b.docker.copyFrom(ctx, id, opts.WorkingDir)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	tr := tar.NewReader(rc)
	for len(res.Images) < opts.CollectImages {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if hdr.Typeflag != tar.TypeReg || filepath.Ext(hdr.Name) != ".png" || hdr.Size > maxImageSize {
			continue
		}
		raw, err := io.ReadAll(tr)
		if err != nil {
			return err
		}
		res.Images = append(res.Images, base64.StdEncoding.EncodeToString(raw))
	}
	return nil
}

// logWriter splits a stream into lines; flush keeps a last line without a newline.
type logWriter struct {
	stream int
	logs   *[]Log
	buf    string
}

func (w *logWriter) Write(p []byte) (int, error) {
	lines := strings.Split(w.buf+string(p), "\n")
	w.buf = lines[len(lines)-1]
	for _, line := range lines[:len(lines)-1] {
		*w.logs = append(*w.logs, Log{Stream: w.stream, Log: line})
	}
	return len(p), nil
}

func (w *logWriter) flush() {
	if w.buf != "" {
		*w.logs = append(*w.logs, Log{Stream: w.stream, Log: w.buf})
		w.buf = ""
	}
}

func short(id string) string {
	if len(id) > 10 {
		return id[:10]
	}
	return id
}
