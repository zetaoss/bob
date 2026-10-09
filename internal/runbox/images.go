package runbox

import (
	_ "embed"
	"fmt"
	"regexp"

	"go.yaml.in/yaml/v3"
)

// tags.yaml is a copy of tags.yaml in github.com/zetaoss/runcontainers: the published tag of each
// image. Copy it from there to run newer images; the tags are then fixed by the bob release.
//
//go:embed tags.yaml
var tagsYAML []byte

var imageTags = mustParseTags(tagsYAML)

var tagRE = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)

func mustParseTags(b []byte) map[string]string {
	tags := map[string]string{}
	if err := yaml.Unmarshal(b, &tags); err != nil {
		panic(fmt.Sprintf("runbox tags.yaml: %v", err))
	}
	for lang, tag := range tags {
		if !tagRE.MatchString(tag) {
			panic(fmt.Sprintf("runbox tags.yaml: %s: invalid tag %q", lang, tag))
		}
	}
	return tags
}

// langImage is the runcontainers image of a language at its tag in tags.yaml (latest if not listed).
// tex is published from the latex image.
func langImage(lang string) string {
	tag, ok := imageTags[lang]
	if !ok && lang == "tex" {
		tag, ok = imageTags["latex"]
	}
	if !ok {
		tag = "latest"
	}
	return langImagePrefix + lang + ":" + tag
}

// ImageTags returns the image tags bob runs, for the startup log.
func ImageTags() map[string]string {
	return imageTags
}
