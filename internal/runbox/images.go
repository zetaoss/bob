package runbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"time"

	"bob/internal/config"
)

// runcontainersVersion is the github.com/zetaoss/runcontainers release whose images bob runs: each release
// publishes every image as <lang>:<version> (tex from the latex image). Set it to a newer release to run
// newer images; the images are then fixed by the bob release.
const runcontainersVersion = "v0.3.0"

var versionRE = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)

// langImage is the runcontainers image of a language at runcontainersVersion.
func langImage(lang string) string {
	return langImagePrefix + lang + ":" + runcontainersVersion
}

// RuncontainersVersion returns the runcontainers release bob runs, for the startup log.
func RuncontainersVersion() string {
	return runcontainersVersion
}

// Images returns every image bob runs: each language (tex included) and each notebook language.
func Images() []string {
	images := make([]string, 0, len(languages)+len(kernels))
	for lang := range languages {
		images = append(images, langImage(lang))
	}
	for lang := range kernels {
		images = append(images, langImage(lang+"-notebook"))
	}
	slices.Sort(images)
	return images
}

// PullImages pulls the images that the Docker host does not have yet, one by one, so that a first run
// does not wait for a pull. It tries every image and returns the failures joined.
func PullImages(ctx context.Context, cfg config.RunboxConfig, log *slog.Logger) error {
	d, err := newDocker(cfg)
	if err != nil {
		return err
	}
	b := &box{docker: d, log: log}
	var errs []error
	for _, image := range Images() {
		start := time.Now()
		if err := b.ensureImage(ctx, image); err != nil {
			log.Error("runbox: pull failed", "image", image, "err", err)
			errs = append(errs, fmt.Errorf("%s: %w", image, err))
			continue
		}
		log.Info("runbox: image ready", "image", image, "seconds", int(time.Since(start).Seconds()))
	}
	return errors.Join(errs...)
}
