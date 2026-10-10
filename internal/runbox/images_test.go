package runbox

import (
	"slices"
	"testing"
)

func TestLangImage(t *testing.T) {
	if !versionRE.MatchString(runcontainersVersion) {
		t.Fatalf("runcontainersVersion %q is not vX.Y.Z", runcontainersVersion)
	}
	if got, want := langImage("tex"), "ghcr.io/zetaoss/runcontainers/tex:"+runcontainersVersion; got != want {
		t.Errorf("tex: got %s, want %s", got, want)
	}
}

func TestImages(t *testing.T) {
	images := Images()
	if len(images) != len(languages)+len(kernels) {
		t.Fatalf("got %d images, want %d", len(images), len(languages)+len(kernels))
	}
	for _, want := range []string{"tex", "python", "python-notebook", "r-notebook"} {
		if !slices.Contains(images, langImage(want)) {
			t.Errorf("%s is missing from %v", want, images)
		}
	}
}
