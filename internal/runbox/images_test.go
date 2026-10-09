package runbox

import "testing"

func TestLangImage(t *testing.T) {
	if len(imageTags) == 0 {
		t.Fatal("tags.yaml is empty")
	}
	for lang := range languages {
		if _, ok := imageTags[lang]; !ok && lang != "tex" {
			t.Errorf("%s is not in tags.yaml", lang)
		}
	}
	if got, want := langImage("tex"), langImagePrefix+"tex:"+imageTags["latex"]; got != want {
		t.Errorf("tex: got %s, want %s", got, want)
	}
	if got := langImage("nope"); got != langImagePrefix+"nope:latest" {
		t.Errorf("unlisted: got %s", got)
	}
}

func TestMustParseTags_Rejects(t *testing.T) {
	for _, in := range []string{"php: [1]", "php: 2026/10/09", "php: latest"} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%q: expected panic", in)
				}
			}()
			mustParseTags([]byte(in))
		}()
	}
}
