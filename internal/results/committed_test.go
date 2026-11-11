package results

import (
	"bytes"
	"os"
	"testing"
)

// TestCommittedDocsMatchRawResults renders RESULTS.md and the README block
// from the committed raw files and compares them with the committed
// documents, so a hand edit to a number, or a raw file committed without
// `make results`, fails `make check`.
func TestCommittedDocsMatchRawResults(t *testing.T) {
	t.Parallel()
	d, err := Load("../../eval/results")
	if err != nil {
		t.Fatal(err)
	}
	for _, doc := range []struct {
		path, tmpl string
		splice     bool
	}{
		{path: "../../RESULTS.md", tmpl: tmplPath},
		{path: "../../README.md", tmpl: readmeTmplPath, splice: true},
	} {
		body, err := Execute(doc.tmpl, d)
		if err != nil {
			t.Fatal(err)
		}
		committed, err := os.ReadFile(doc.path)
		if err != nil {
			t.Fatal(err)
		}
		want := body
		if doc.splice {
			if want, err = Splice(committed, body); err != nil {
				t.Fatal(err)
			}
		}
		if !bytes.Equal(committed, want) {
			t.Errorf("%s differs from a render of eval/results; run `make results` and commit the result", doc.path)
		}
	}
}
