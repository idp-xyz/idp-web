package extract

import (
	"strings"
	"testing"

	"golang.org/x/net/html"
)

func mustParse(t *testing.T, s string) *html.Node {
	t.Helper()
	doc, err := html.Parse(strings.NewReader(s))
	if err != nil {
		t.Fatal(err)
	}
	return doc
}
