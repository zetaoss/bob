// Package runbox runs code blocks from wiki pages in throwaway containers on a Docker daemon (moved
// from the runbox service). Each request gets a fresh container of the language's image.
package runbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"bob/internal/config"
)

// requestError is a bad request; its message is returned as is.
type requestError string

func (e requestError) Error() string { return string(e) }

const (
	ErrNoFiles         requestError = "no files"
	ErrNoSources       requestError = "no sources"
	ErrInvalidLanguage requestError = "invalid language"
)

const maxRequestBytes = 4 << 20

type Handler struct {
	box *box
	log *slog.Logger
}

func NewHandler(cfg config.RunboxConfig, log *slog.Logger) (*Handler, error) {
	d, err := newDocker(cfg)
	if err != nil {
		return nil, err
	}
	return &Handler{box: &box{docker: d, log: log}, log: log}, nil
}

// ServeHTTP answers POST /lang and POST /notebook.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var run func(*http.Request) (any, error)
	switch r.URL.Path {
	case "/lang":
		run = h.lang
	case "/notebook":
		run = h.notebook
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)

	result, err := run(r)
	var reqErr requestError
	switch {
	case errors.As(err, &reqErr):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": reqErr.Error()})
	case err != nil:
		h.log.Error("runbox failed", "path", r.URL.Path, "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": fmt.Sprintf("runbox: %v", err)})
	default:
		writeJSON(w, http.StatusOK, result)
	}
}

func (h *Handler) lang(r *http.Request) (any, error) {
	var in LangInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		return nil, requestError(err.Error())
	}
	opts, err := langBoxOpts(in)
	if err != nil {
		return nil, err
	}
	res, err := h.box.run(r.Context(), opts)
	if err != nil {
		return nil, err
	}
	return toLangResult(res), nil
}

func (h *Handler) notebook(r *http.Request) (any, error) {
	var in NotebookInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		return nil, requestError(err.Error())
	}
	opts, err := notebookBoxOpts(in)
	if err != nil {
		return nil, err
	}
	res, err := h.box.run(r.Context(), opts)
	if err != nil {
		return nil, err
	}
	return toNotebookResult(res)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
