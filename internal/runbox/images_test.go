package runbox

import "testing"

func TestLangImage(t *testing.T) {
	if !versionRE.MatchString(runcontainersVersion) {
		t.Fatalf("runcontainersVersion %q is not vX.Y.Z", runcontainersVersion)
	}
	if got, want := langImage("tex"), "ghcr.io/zetaoss/runcontainers/tex:"+runcontainersVersion; got != want {
		t.Errorf("tex: got %s, want %s", got, want)
	}
}
