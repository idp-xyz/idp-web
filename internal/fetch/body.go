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
	"strings"
	"unicode/utf8"

	"golang.org/x/net/html/charset"

	"idp-web/internal/addr"
	"idp-web/internal/extract"
)

// textTypes are the media types that are read (ADR-0001 D10). PDF is refused with a message of its own, everything
// else as "not read".
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
	if kind == "application/pdf" {
		return Page{}, ErrPDF
	}
	if !textTypes[kind] {
		if kind == "" {
			kind = "(none)"
		}
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
	text := decodeText(body, resp.Header.Get("Content-Type"))
	if kind == "text/plain" {
		return Page{FinalURL: finalURL, Text: strings.TrimSpace(text), Bytes: len(body), Type: kind, Engine: extract.Plain}, nil
	}
	res, err := extract.Extract([]byte(text), finalURL, extract.Options{Engine: r.cfg.Engine})
	if err != nil {
		r.log.Info("extraction failed", "err", err)
		return Page{}, ErrUnreadable
	}
	if isBotCheck(text, res.Title, res.Markdown) {
		return Page{}, ErrBotCheck
	}
	return Page{FinalURL: finalURL, Title: res.Title, Text: res.Markdown, Bytes: len(body), Type: kind, Engine: res.Engine}, nil
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
