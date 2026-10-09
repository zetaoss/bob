package runbox

import (
	"encoding/json"
	"fmt"
	"time"
)

// NotebookInput is a /notebook request: one code cell per source.
type NotebookInput struct {
	Lang    string   `json:"lang"`
	Sources []string `json:"sources"`
}

// NotebookResult is the /notebook response: the outputs of each cell, in order.
type NotebookResult struct {
	OutputsList [][]Output `json:"outputsList"`
	CPU         int        `json:"cpu"`
	MEM         int        `json:"mem"`
	Time        int        `json:"time"`
	Timedout    bool       `json:"timedout"`
}

// Output is a Jupyter (nbformat 4) cell output.
type Output struct {
	OutputType     string          `json:"output_type"`
	Data           map[string]any  `json:"data,omitempty"`
	Metadata       *map[string]any `json:"metadata,omitempty"`
	ExecutionCount *int            `json:"execution_count,omitempty"`
	Name           string          `json:"name,omitempty"`
	Text           []string        `json:"text,omitempty"`
	Ename          string          `json:"ename,omitempty"`
	Evalue         string          `json:"evalue,omitempty"`
	Traceback      []string        `json:"traceback,omitempty"`
}

const notebookPath = "/tmp/runbox.ipynb"

var kernels = map[string]struct{ kernel, language string }{
	"python": {"python3", "python"},
	"r":      {"ir", "R"},
}

func notebookBoxOpts(in NotebookInput) (boxOpts, error) {
	k, ok := kernels[in.Lang]
	if !ok {
		return boxOpts{}, ErrInvalidLanguage
	}
	if len(in.Sources) == 0 {
		return boxOpts{}, ErrNoSources
	}
	type cell struct {
		CellType       string         `json:"cell_type"`
		Metadata       map[string]any `json:"metadata"`
		Source         []string       `json:"source"`
		Outputs        []Output       `json:"outputs"`
		ExecutionCount int            `json:"execution_count"`
	}
	cells := make([]cell, len(in.Sources))
	for i, s := range in.Sources {
		cells[i] = cell{CellType: "code", Metadata: map[string]any{}, Source: []string{s}, Outputs: []Output{}}
	}
	nb := map[string]any{
		"metadata": map[string]any{
			"kernelspec":    map[string]string{"name": k.kernel, "display_name": ""},
			"language_info": map[string]string{"name": k.language},
		},
		"nbformat_minor": 4,
		"nbformat":       4,
		"cells":          cells,
	}
	body, err := json.Marshal(nb)
	if err != nil {
		return boxOpts{}, err
	}
	return boxOpts{
		Image:        "jmnote/runbox:" + in.Lang + "-notebook",
		Command:      "jupyter nbconvert --execute --to notebook --allow-errors --stdout " + notebookPath,
		Shell:        "sh",
		Files:        []File{{Name: notebookPath, Body: string(body)}},
		WorkingDir:   "/tmp",
		Timeout:      60 * time.Second,
		CollectStats: true,
	}, nil
}

// toNotebookResult reads the executed notebook that nbconvert prints to stdout.
func toNotebookResult(r *boxResult) (*NotebookResult, error) {
	stdout, _ := r.streams()
	var nb struct {
		Cells []struct {
			Outputs []Output `json:"outputs"`
		} `json:"cells"`
	}
	if err := json.Unmarshal([]byte(stdout), &nb); err != nil {
		return nil, fmt.Errorf("read executed notebook: %w", err)
	}
	list := make([][]Output, len(nb.Cells))
	for i, c := range nb.Cells {
		list[i] = c.Outputs
		if list[i] == nil {
			list[i] = []Output{}
		}
	}
	return &NotebookResult{OutputsList: list, CPU: r.CPU, MEM: r.MEM, Time: r.Time, Timedout: r.Timedout}, nil
}
