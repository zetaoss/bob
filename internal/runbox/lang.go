package runbox

import (
	"path"
	"strings"
	"time"
)

// LangInput is a /lang request: files of one language; Main is the index of the file a language
// may rewrite (php, r).
type LangInput struct {
	Lang  string `json:"lang"`
	Files []File `json:"files"`
	Main  int    `json:"main,omitempty"`
}

// LangResult is the /lang response. Each log is the stream number (1 stdout, 2 stderr) followed by
// the line.
type LangResult struct {
	Logs     []string `json:"logs,omitempty"`
	Code     int      `json:"code,omitempty"`
	CPU      int      `json:"cpu,omitempty"`
	MEM      int      `json:"mem,omitempty"`
	Time     int      `json:"time,omitempty"`
	Timedout bool     `json:"timedout,omitempty"`
	Images   []string `json:"images,omitempty"`
}

const langImagePrefix = "ghcr.io/zetaoss/runcontainers/"

// language is how one language is run. Files without a name are written as fileName.fileExt.
type language struct {
	command    string
	shell      string
	env        []string
	fileDir    string
	fileName   string
	fileExt    string
	timeout    time.Duration
	user       string
	workingDir string
	images     int
	modifyMain func(string) string
}

func texLanguage() language {
	return language{
		command: "touch oblivoir.sty && pdflatex -halt-on-error runbox.tex && convert runbox.pdf -strip p%d.png",
		fileExt: "tex", images: 10, timeout: 30 * time.Second, user: "root",
	}
}

var languages = map[string]language{
	"bash":   {command: "/bin/bash runbox.sh", fileExt: "sh", shell: "bash"},
	"c":      {command: "gcc runbox.c; ./a.out"},
	"cpp":    {command: "g++ runbox.cpp; ./a.out"},
	"csharp": {command: "mcs runbox.cs; mono runbox.exe", fileExt: "cs"},
	"java": {
		command: `javac -d bin -cp "lib/*" src/*; java -cp "bin:lib/*" App`,
		fileDir: "/src", fileName: "App", workingDir: "/demo",
	},
	"kotlin": {command: "kotlinc runbox.kt -include-runtime -d runbox.jar && java -jar runbox.jar", fileExt: "kt", timeout: 40 * time.Second},
	"go": {
		command: "go mod tidy > /dev/null 2>&1; go run runbox.go",
		env:     []string{"TINI_SUBREAPER=1"}, timeout: 30 * time.Second, workingDir: "/go/src/m",
	},
	"latex":      texLanguage(),
	"lua":        {command: "lua runbox.lua"},
	"mysql":      {command: "bash /tmp/entrypoint.sh", fileExt: "sql", timeout: 30 * time.Second},
	"perl":       {command: "perl runbox.pl", fileExt: "pl"},
	"php":        {command: "php runbox.php", modifyMain: phpMain},
	"powershell": {command: "pwsh runbox.ps", fileExt: "ps"},
	"python":     {command: "python runbox.py", fileExt: "py"},
	"r":          {command: "Rscript runbox.r", modifyMain: rMain},
	"ruby":       {command: "ruby runbox.rb", fileExt: "rb"},
	"sqlite3":    {fileExt: "sql"}, // command depends on the source
	"tex":        texLanguage(),
}

// phpMain adds the opening tag and the autoloader to a source without <?php.
func phpMain(source string) string {
	source = strings.TrimLeft(source, " \t\n")
	if !strings.HasPrefix(source, "<?php") {
		source = "<?php\nrequire_once('vendor/autoload.php');\n" + source
	}
	return source
}

// rMain draws plots to PNG files in the working directory.
func rMain(source string) string {
	return "png(width=500,height=400);\n" + source + "\n" + `options(echo=F); invisible(dev.off());system('find . -name "*.pdf" -exec mogrify -density 80 -format png {} \\;',ignore.stdout=T,ignore.stderr=F);`
}

func langBoxOpts(in LangInput) (boxOpts, error) {
	if len(in.Files) == 0 {
		return boxOpts{}, ErrNoFiles
	}
	l, ok := languages[in.Lang]
	if !ok {
		return boxOpts{}, ErrInvalidLanguage
	}
	if l.fileName == "" {
		l.fileName = "runbox"
	}
	if l.fileExt == "" {
		l.fileExt = in.Lang
	}
	if l.shell == "" {
		l.shell = "sh"
	}
	if l.timeout == 0 {
		l.timeout = 10 * time.Second
	}
	if l.workingDir == "" {
		l.workingDir = "/home/user01"
	}
	if in.Lang == "sqlite3" {
		if source := in.Files[0].Body; strings.HasPrefix(source, ".") {
			l.command = "sqlite3 chinook.db " + source
		} else {
			l.command = "sqlite3 -header chinook.db < runbox.sql"
		}
	}

	// Files with the same path are joined with a newline, in order.
	var names []string
	bodies := map[string]string{}
	for i, f := range in.Files {
		name := f.Name
		if name == "" {
			name = l.fileName + "." + l.fileExt
		}
		name = path.Join(l.workingDir, l.fileDir, name)
		body := f.Body
		if i == in.Main && l.modifyMain != nil {
			body = l.modifyMain(body)
		}
		if existing, ok := bodies[name]; ok {
			bodies[name] = existing + "\n" + body
		} else {
			names = append(names, name)
			bodies[name] = body
		}
	}
	files := make([]File, len(names))
	for i, name := range names {
		files[i] = File{Name: name, Body: bodies[name]}
	}

	images := l.images
	if images == 0 {
		images = 2
	}
	return boxOpts{
		Image:         langImagePrefix + in.Lang,
		Command:       l.command,
		Shell:         l.shell,
		Env:           l.env,
		Files:         files,
		User:          l.user,
		WorkingDir:    l.workingDir,
		Timeout:       l.timeout,
		CollectStats:  true,
		CollectImages: images,
	}, nil
}

func toLangResult(r *boxResult) LangResult {
	logs := make([]string, len(r.Logs))
	for i, l := range r.Logs {
		logs[i] = string(rune('0'+l.Stream)) + l.Log
	}
	return LangResult{Logs: logs, Code: r.Code, CPU: r.CPU, MEM: r.MEM, Time: r.Time, Timedout: r.Timedout, Images: r.Images}
}
