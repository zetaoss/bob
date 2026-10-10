package runbox

import "regexp"

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
