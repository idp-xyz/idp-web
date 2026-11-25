package fetch

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/html/charset"

	"idp-web/internal/addr"
	"idp-web/internal/extract"
)

// textTypes are the media types that are read as text (ADR-0001 D10); PDF is read by the PDF reader (D31), everything
// else is refused as "not read".
var textTypes = map[string]bool{"text/html": true, "application/xhtml+xml": true, "text/plain": true}

func mediaType(h http.Header) string {
	ct := h.Get("Content-Type")
	if mt, _, err := mime.ParseMediaType(ct); err == nil {
		return mt
	}
	return strings.ToLower(strings.TrimSpace(strings.SplitN(ct, ";", 2)[0]))
}

// page turns the final response into a Page.
func (r *Reader) page(ctx context.Context, resp *http.Response, final addr.Address) (Page, error) {
	finalURL := final.URL()
	kind := mediaType(resp.Header)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return Page{}, r.errorAnswer(resp, kind)
	}
	// What the page is comes from its bytes where they say so (ADR-0001 D31). A body that starts with %PDF- is a PDF
	// whatever the server called it: servers send PDFs as application/octet-stream, as text/plain, with no type at all,
	// and a reader that believed the label would show the model a page of PDF syntax or refuse a document it could read.
	// The other way round, a body labelled application/pdf that is an HTML page (an error or a login page, which is what
	// it nearly always is) is read as the HTML page it is; any other body under that label is a damaged PDF.
	// Only the types that can hold a PDF or a page are read for this: the rest are refused as before, unread.
	isPDFLabel := kind == "application/pdf" || kind == "application/x-pdf"
	sniffed := kind == "" || kind == "application/octet-stream" || kind == "binary/octet-stream"
	if !textTypes[kind] && !isPDFLabel && !sniffed {
		return Page{}, wrap(ErrType, "content type %s is not read", kind)
	}
	if strings.EqualFold(resp.Header.Get("Cf-Mitigated"), "challenge") {
		return Page{}, ErrBotCheck
	}
	if resp.ContentLength > r.maxBytes {
		return Page{}, ErrTooLarge
	}
	body, err := readBody(resp, r.maxBytes, false)
	if err != nil {
		return Page{}, r.bodyError(err)
	}
	if bytes.HasPrefix(body, []byte("%PDF-")) {
		return r.pdfPage(ctx, body, finalURL)
	}
	if isPDFLabel {
		if !looksLikeHTML(body) {
			r.log.Info("a body labelled PDF is not one", "bytes", len(body))
			return Page{}, wrap(ErrPDFMalformed, "the answer is labelled as a PDF but is not one")
		}
		kind = "text/html"
	} else if sniffed {
		if kind == "" {
			kind = "(none)"
		}
		return Page{}, wrap(ErrType, "content type %s is not read", kind)
	}
	if kind == "text/html" || kind == "application/xhtml+xml" {
		if fn, _ := ctx.Value(htmlKey{}).(func()); fn != nil {
			fn()
		}
	}
	text := decodeText(body, resp.Header.Get("Content-Type"))
	if kind == "text/plain" {
		return Page{FinalURL: finalURL, Text: strings.TrimSpace(text), Bytes: len(body), Type: kind, Engine: extract.Plain}, nil
	}
	res, err := extract.Extract([]byte(text), finalURL, extract.Options{Engine: r.cfg.Engine})
	if err != nil {
		r.log.Info("extraction failed", "err", err)
		return Page{}, ErrUnreadable
	}
	if rule, err := notThePage(text, res); err != nil {
		if rule != "" {
			r.log.Info("the page is not the page", "rule", rule) // which sign found it, for whoever reads the log (D27, D33)
		}
		return Page{}, err
	}
	return Page{FinalURL: finalURL, Title: res.Title, Text: res.Markdown, Bytes: len(body), Type: kind, Engine: res.Engine}, nil
}

// notThePage reports the pages that extraction read correctly and that are still not the article: a challenge (D12)
// or a frame whose article script writes (D27). The bot check is asked first and keeps its reason: a challenge page is
// also short and full of script, and "the site took you for a robot" is what the model must learn, because the
// remedy is a different source for a reason that may pass, where a page that needs script will not pass by itself.
// Both are asked of res, the text after the fallback of D5, so a page the plain reader rescued is judged by what the
// model would get.
// The first result is the name of the rule that found a page that needs script, or the marker of a refusal, "" otherwise.
func notThePage(rawHTML string, res extract.Result) (rule string, err error) {
	if isBotCheck(rawHTML, res.Title, res.Markdown) {
		return "", ErrBotCheck
	}
	if sig := refusalSignal(res.Markdown); sig != "" {
		return "refusal: " + sig, ErrBlockedBySite
	}
	if rule := needsScript(rawHTML, res.Markdown); rule != "" {
		return rule, ErrNeedsScript
	}
	return "", nil
}

// bodyError keeps the errors readBody means for the model and turns the rest (a connection that broke while the body
// was coming, a deadline) into sentences that say nothing of the inside; Read maps the deadline to ErrTooSlow.
func (r *Reader) bodyError(err error) error {
	for _, k := range []error{ErrTooLarge, ErrEncoding} {
		if errors.Is(err, k) {
			return err
		}
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return err
	}
	r.log.Info("the body could not be read", "err", err)
	return ErrConnect
}

// errorAnswer is what an answer that is not a success becomes. Challenge pages often come with 403, 429 or 503 (and
// the AWS WAF one with 202, which page() reads as a page), so the body of those is looked at before the status is
// reported: "HTTP 403" from a site that wanted a CAPTCHA solved is not what happened.
func (r *Reader) errorAnswer(resp *http.Response, kind string) error {
	if strings.EqualFold(resp.Header.Get("Cf-Mitigated"), "challenge") {
		return ErrBotCheck
	}
	switch resp.StatusCode {
	case http.StatusForbidden, http.StatusTooManyRequests, http.StatusServiceUnavailable:
		if kind == "text/html" || kind == "application/xhtml+xml" {
			if body, err := readBody(resp, maxErrorPage, true); err == nil {
				text := decodeText(body, resp.Header.Get("Content-Type"))
				if res, err := extract.Extract([]byte(text), "", extract.Options{Engine: extract.Plain}); err == nil && isBotCheck(text, res.Title, res.Markdown) {
					return ErrBotCheck
				} else if err == nil && refusalSignal(res.Markdown) != "" {
					return ErrBlockedBySite
				}
			}
		}
	}
	return wrap(ErrStatus, "the site answered with HTTP status %d", resp.StatusCode)
}

// readBody reads the body, undoes the Content-Encoding and applies limit to both the encoded and the decoded size
// (ADR-0001 D10): a few kilobytes of gzip cannot expand past it, because the decoder is read through a limit and never
// to its end. With truncate, a body over the limit is cut instead of refused (robots.txt: RFC 9309 2.5 asks for the
// first 500 KiB to be read, not for the file to be refused).
func readBody(resp *http.Response, limit int64, truncate bool) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	cut := int64(len(raw)) > limit
	if cut {
		if !truncate {
			return nil, ErrTooLarge
		}
		raw = raw[:limit]
	}
	enc := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
	var out []byte
	switch enc {
	case "", "identity":
		return raw, nil
	case "gzip", "x-gzip":
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, wrap(ErrEncoding, "the content encoding gzip could not be decoded")
		}
		out, err = limited(zr, limit, truncate || cut)
		if err != nil {
			return nil, err
		}
	case "deflate":
		// deflate is meant to be zlib-wrapped, but some servers send the bare stream; the reference tries both
		zr, err := zlib.NewReader(bytes.NewReader(raw))
		if err == nil {
			out, err = limited(zr, limit, truncate || cut)
		}
		if err != nil {
			if errors.Is(err, ErrTooLarge) {
				return nil, err
			}
			out, err = limited(flate.NewReader(bytes.NewReader(raw)), limit, truncate || cut)
			if err != nil {
				if errors.Is(err, ErrTooLarge) {
					return nil, err
				}
				return nil, wrap(ErrEncoding, "the content encoding deflate could not be decoded")
			}
		}
	default:
		return nil, wrap(ErrEncoding, "content encoding %s is not read", sanitize(enc))
	}
	return out, nil
}

// limited reads at most limit bytes of the decoded stream. More than that is a refusal, or with truncate a cut. A
// stream that ends early because its (truncated) source did is accepted in truncate mode.
func limited(r io.Reader, limit int64, truncate bool) ([]byte, error) {
	out, err := io.ReadAll(io.LimitReader(r, limit+1))
	if int64(len(out)) > limit {
		if !truncate {
			return nil, ErrTooLarge
		}
		return out[:limit], nil
	}
	if err != nil && !(truncate && errors.Is(err, io.ErrUnexpectedEOF)) {
		return nil, wrap(ErrEncoding, "the content encoding could not be decoded")
	}
	return out, nil
}

// sanitize keeps a header value that is echoed to the model short and printable.
func sanitize(s string) string {
	if len(s) > 40 {
		s = s[:40]
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e {
			return '?'
		}
		return r
	}, s)
}

// decodeText decodes the body to UTF-8 by the charset of the Content-Type header, a byte order mark or the document's
// meta tag, in the order of the HTML standard (x/net/html/charset). When nothing declares one and the bytes are valid
// UTF-8, they are taken as UTF-8: the library's fallback for an undeclared page is windows-1252, which would garble
// the many modern pages that declare nothing.
func decodeText(body []byte, contentType string) string {
	enc, _, certain := charset.DetermineEncoding(body, contentType)
	if !certain && utf8.Valid(body) {
		return strings.TrimPrefix(string(body), "\xef\xbb\xbf")
	}
	out, err := enc.NewDecoder().Bytes(body)
	if err != nil {
		out = body
	}
	return strings.TrimPrefix(strings.ToValidUTF8(string(out), "�"), "\xef\xbb\xbf")
}

// looksLikeHTML says whether a body that is not a PDF starts like an HTML page, after a byte order mark and white
// space.
func looksLikeHTML(body []byte) bool {
	b := bytes.TrimLeft(bytes.TrimPrefix(body, []byte("\xef\xbb\xbf")), " \t\r\n")
	if len(b) > 64 {
		b = b[:64]
	}
	b = bytes.ToLower(b)
	for _, p := range []string{"<!doctype html", "<html", "<head", "<body"} {
		if bytes.HasPrefix(b, []byte(p)) {
			return true
		}
	}
	return false
}

// pdfPage hands a downloaded PDF to the PDF reader, which runs it in a process of its own with limits (D31).
func (r *Reader) pdfPage(ctx context.Context, body []byte, finalURL string) (Page, error) {
	if r.cfg.PDF == nil {
		return Page{}, ErrPDFUnavailable
	}
	out, err := r.cfg.PDF.Read(ctx, body)
	if err != nil {
		return Page{}, err
	}
	title := out.Title
	if title == "" {
		title = pdfNameOf(finalURL)
	}
	return Page{FinalURL: finalURL, Title: title, Text: out.Text, Bytes: len(body), Type: "application/pdf", Engine: "pdf",
		Pages: out.Pages, PagesRead: out.PagesRead}, nil
}

// pdfNameOf is the last part of the address' path, which is what a document without a title is called.
func pdfNameOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	if name := path.Base(u.Path); name != "." && name != "/" && name != "" {
		return name
	}
	return u.Host
}
