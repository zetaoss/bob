package runbox

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestLangBoxOpts_Defaults(t *testing.T) {
	opts, err := langBoxOpts(LangInput{Lang: "python", Files: []File{{Body: "print(1)"}}})
	if err != nil {
		t.Fatal(err)
	}
	if opts.Image != "ghcr.io/zetaoss/runcontainers/python:"+imageTags["python"] || opts.Command != "python runbox.py" || opts.Shell != "sh" ||
		opts.WorkingDir != "/home/user01" || opts.Timeout != 10*time.Second || opts.CollectImages != 2 || !opts.CollectStats {
		t.Fatalf("unexpected opts: %+v", opts)
	}
	if len(opts.Files) != 1 || opts.Files[0] != (File{Name: "/home/user01/runbox.py", Body: "print(1)"}) {
		t.Fatalf("unexpected files: %+v", opts.Files)
	}
}

func TestLangBoxOpts_Languages(t *testing.T) {
	java, _ := langBoxOpts(LangInput{Lang: "java", Files: []File{{Body: "class App {}"}, {Name: "Util.java", Body: "class Util {}"}}})
	if java.WorkingDir != "/demo" || java.Files[0].Name != "/demo/src/App.java" || java.Files[1].Name != "/demo/src/Util.java" {
		t.Fatalf("java: %+v", java)
	}
	goOpts, _ := langBoxOpts(LangInput{Lang: "go", Files: []File{{Body: "package main"}}})
	if goOpts.Files[0].Name != "/go/src/m/runbox.go" || goOpts.Env[0] != "TINI_SUBREAPER=1" || goOpts.Timeout != 30*time.Second {
		t.Fatalf("go: %+v", goOpts)
	}
	tex, _ := langBoxOpts(LangInput{Lang: "latex", Files: []File{{Body: `\documentclass{article}`}}})
	if tex.Files[0].Name != "/home/user01/runbox.tex" || tex.CollectImages != 10 || tex.User != "root" {
		t.Fatalf("latex: %+v", tex)
	}
	bash, _ := langBoxOpts(LangInput{Lang: "bash", Files: []File{{Body: "echo"}}})
	if bash.Shell != "bash" || bash.Files[0].Name != "/home/user01/runbox.sh" {
		t.Fatalf("bash: %+v", bash)
	}
}

func TestLangBoxOpts_ModifyMain(t *testing.T) {
	php, _ := langBoxOpts(LangInput{Lang: "php", Files: []File{{Body: "\n echo 1;"}}})
	if php.Files[0].Body != "<?php\nrequire_once('vendor/autoload.php');\necho 1;" {
		t.Fatalf("php: %q", php.Files[0].Body)
	}
	short, _ := langBoxOpts(LangInput{Lang: "php", Files: []File{{Body: "1"}}})
	if !strings.HasSuffix(short.Files[0].Body, "\n1") {
		t.Fatalf("php short source: %q", short.Files[0].Body)
	}
	tagged, _ := langBoxOpts(LangInput{Lang: "php", Files: []File{{Body: "<?php echo 1;"}}})
	if tagged.Files[0].Body != "<?php echo 1;" {
		t.Fatalf("php tagged: %q", tagged.Files[0].Body)
	}
	// Only the main file is modified; files with the same path are joined.
	r, _ := langBoxOpts(LangInput{Lang: "r", Main: 1, Files: []File{{Body: "a <- 1"}, {Body: "plot(a)"}}})
	if len(r.Files) != 1 || !strings.HasPrefix(r.Files[0].Body, "a <- 1\npng(width=500,height=400);\nplot(a)\n") {
		t.Fatalf("r: %+v", r.Files)
	}
}

func TestLangBoxOpts_Sqlite3(t *testing.T) {
	dot, _ := langBoxOpts(LangInput{Lang: "sqlite3", Files: []File{{Body: ".tables"}}})
	if dot.Command != "sqlite3 chinook.db .tables" {
		t.Fatalf("dot command: %q", dot.Command)
	}
	sql, _ := langBoxOpts(LangInput{Lang: "sqlite3", Files: []File{{Body: "SELECT 1;"}}})
	if sql.Command != "sqlite3 -header chinook.db < runbox.sql" || sql.Files[0].Name != "/home/user01/runbox.sql" {
		t.Fatalf("sql: %+v", sql)
	}
}

func TestLangBoxOpts_Errors(t *testing.T) {
	if _, err := langBoxOpts(LangInput{Lang: "python"}); err != ErrNoFiles {
		t.Fatalf("no files: %v", err)
	}
	if _, err := langBoxOpts(LangInput{Lang: "cobol", Files: []File{{Body: "x"}}}); err != ErrInvalidLanguage {
		t.Fatalf("invalid language: %v", err)
	}
}

func TestToLangResult(t *testing.T) {
	res := toLangResult(&boxResult{Logs: []Log{{1, "out"}, {2, "err"}}, Code: 1, Time: 5})
	raw, _ := json.Marshal(res)
	if string(raw) != `{"logs":["1out","2err"],"code":1,"time":5}` {
		t.Fatalf("got %s", raw)
	}
}

func TestLogWriter(t *testing.T) {
	var logs []Log
	w := &logWriter{stream: 1, logs: &logs}
	_, _ = w.Write([]byte("a\nb"))
	_, _ = w.Write([]byte("c\n\nd"))
	w.flush()
	want := []string{"a", "bc", "", "d"}
	if len(logs) != len(want) {
		t.Fatalf("got %+v", logs)
	}
	for i, l := range logs {
		if l.Log != want[i] || l.Stream != 1 {
			t.Fatalf("got %+v", logs)
		}
	}
}

func TestNotebookBoxOpts(t *testing.T) {
	opts, err := notebookBoxOpts(NotebookInput{Lang: "python", Sources: []string{"1+1", "print(2)"}})
	if err != nil {
		t.Fatal(err)
	}
	if opts.Image != "jmnote/runbox:python-notebook" || opts.Files[0].Name != "/tmp/runbox.ipynb" || opts.CollectImages != 0 {
		t.Fatalf("unexpected opts: %+v", opts)
	}
	var nb struct {
		Metadata struct {
			Kernelspec   map[string]string `json:"kernelspec"`
			LanguageInfo map[string]string `json:"language_info"`
		} `json:"metadata"`
		Nbformat int `json:"nbformat"`
		Cells    []struct {
			CellType string   `json:"cell_type"`
			Source   []string `json:"source"`
			Outputs  []any    `json:"outputs"`
		} `json:"cells"`
	}
	if err := json.Unmarshal([]byte(opts.Files[0].Body), &nb); err != nil {
		t.Fatal(err)
	}
	if nb.Metadata.Kernelspec["name"] != "python3" || nb.Metadata.LanguageInfo["name"] != "python" || nb.Nbformat != 4 ||
		len(nb.Cells) != 2 || nb.Cells[1].Source[0] != "print(2)" || nb.Cells[0].CellType != "code" || nb.Cells[0].Outputs == nil {
		t.Fatalf("unexpected notebook: %s", opts.Files[0].Body)
	}

	if _, err := notebookBoxOpts(NotebookInput{Lang: "go", Sources: []string{"x"}}); err != ErrInvalidLanguage {
		t.Fatalf("invalid language: %v", err)
	}
	if _, err := notebookBoxOpts(NotebookInput{Lang: "r"}); err != ErrNoSources {
		t.Fatalf("no sources: %v", err)
	}
}

func TestToNotebookResult(t *testing.T) {
	executed := `{"cells":[{"cell_type":"code","outputs":[{"output_type":"execute_result","data":{"text/plain":["2"]},"metadata":{},"execution_count":1}]},` +
		`{"cell_type":"code","outputs":[]}]}`
	var logs []Log
	for _, line := range strings.Split(executed, "\n") {
		logs = append(logs, Log{Stream: 1, Log: line})
	}
	res, err := toNotebookResult(&boxResult{Logs: append(logs, Log{Stream: 2, Log: "warning"}), CPU: 3, MEM: 4, Time: 5})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(res)
	want := `{"outputsList":[[{"output_type":"execute_result","data":{"text/plain":["2"]},"metadata":{},"execution_count":1}],[]],"cpu":3,"mem":4,"time":5,"timedout":false}`
	if string(raw) != want {
		t.Fatalf("got  %s\nwant %s", raw, want)
	}
}
