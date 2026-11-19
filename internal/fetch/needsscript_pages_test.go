package fetch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"

	"idp-web/internal/extract"
)

// TestMeasureNeedsScriptOnLocalPages runs the detection on every page of a directory and says which are flagged and by
// which rule (ADR-0001 D27). It is how the rules were measured on the platform's evaluation pages, which are third-party
// pages and are not in this repository; it is skipped without IDP_WEB_PAGES_DIR.
//
//	IDP_WEB_PAGES_DIR=.../e4-real/pages [IDP_WEB_PAGES_PREFIX=e4r-] go test ./internal/fetch -run MeasureNeedsScript -v
//
// A page is <name>.raw (the bytes as fetched) with an optional <name>.json that holds "url" and "type" (the content
// type, for the character set), as tools/web_eval/e4real.py stores them; a bare .html file is read as it is. The pages
// go through the same steps as a fetched page (decoding, extraction with the fallback, then notThePage). The test fails
// only if a page cannot be read; judging the flagged pages is for the person who reads the output.
func TestMeasureNeedsScriptOnLocalPages(t *testing.T) {
	dir := os.Getenv("IDP_WEB_PAGES_DIR")
	if dir == "" {
		t.Skip("IDP_WEB_PAGES_DIR is not set")
	}
	prefix := os.Getenv("IDP_WEB_PAGES_PREFIX")
	var files []string
	for _, pat := range []string{"*.raw", "*.html"} {
		m, err := filepath.Glob(filepath.Join(dir, pat))
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, m...)
	}
	sort.Strings(files)
	n, needs, bots := 0, 0, 0
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), filepath.Ext(f))
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var meta struct{ URL, Type string }
		if b, err := os.ReadFile(strings.TrimSuffix(f, filepath.Ext(f)) + ".json"); err == nil {
			_ = json.Unmarshal(b, &meta)
		}
		text := decodeText(raw, meta.Type)
		res, err := extract.Extract([]byte(text), meta.URL, extract.Options{})
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		n++
		rule, verdict := notThePage(text, res)
		switch {
		case rule != "":
			needs++
			t.Logf("FLAGGED %-16s rule=%-14s text=%d bytes, %d characters, engine %s", name, rule, len(res.Markdown), utf8.RuneCountInString(res.Markdown), res.Engine)
		case verdict != nil:
			bots++
			t.Logf("BOT-CHECK %-14s text=%d bytes", name, len(res.Markdown))
		}
	}
	t.Logf("%d pages in %s: %d need script, %d are bot checks", n, dir, needs, bots)
}
