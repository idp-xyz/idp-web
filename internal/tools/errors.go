package tools

import (
	"context"
	"errors"
	"fmt"
	"time"

	"idp-web/internal/addr"
	"idp-web/internal/fetch"
	"idp-web/internal/refs"
	"idp-web/internal/search"
	"idp-web/internal/tenants"
)

// The sentinel errors are what a model is told, so each is a sentence it can act on. Anything else that goes wrong
// is logged and the model gets a fixed sentence (ADR-0001 D3: internals stay in the log).
var (
	ErrNotAvailable = errors.New("web search is not available: this service has no search provider configured")
	ErrInvalid      = errors.New("the arguments are not valid")
	ErrNoCaller     = errors.New("no caller identity: this server must be registered with identity=forward")
	ErrRateLimited  = errors.New("too many calls in a short time")
	// ErrQuotaExceeded: the tenant's daily limit is used up (ADR-0001 D28).
	ErrQuotaExceeded = errors.New("the daily limit is used up")
	// ErrQuotaUnavailable: the limit could not be checked, so the call is not made. Never "allowed without counting".
	ErrQuotaUnavailable = errors.New("the daily limit could not be checked")
	// ErrNumberingUnavailable: the database that numbers the sources did not answer (ADR-0001 D29).
	ErrNumberingUnavailable = errors.New("the sources could not be numbered")
)

// quotaExceeded says which daily limit is used up.
type quotaExceeded struct{ what string }

func (e *quotaExceeded) Error() string {
	return fmt.Sprintf("the daily limit of %s for your organisation is used up; it resets at 00:00 UTC: do not try again today, and answer with what you have", e.what)
}
func (e *quotaExceeded) Is(target error) bool { return target == ErrQuotaExceeded }

// quotaUnavailable says which daily limit could not be checked.
type quotaUnavailable struct{ what string }

func (e *quotaUnavailable) Error() string {
	return fmt.Sprintf("the daily limit of %s could not be checked just now, so the call was not made: try again in a little while, or answer with what you have", e.what)
}
func (e *quotaUnavailable) Is(target error) bool { return target == ErrQuotaUnavailable }

// rateLimited carries when the caller may try again.
type rateLimited struct {
	what string
	wait time.Duration
}

func (e *rateLimited) Error() string {
	secs := int((e.wait + time.Second - 1) / time.Second)
	return fmt.Sprintf("too many %s in a short time: try again in %d seconds, and meanwhile answer with what you have", e.what, secs)
}
func (e *rateLimited) Is(target error) bool { return target == ErrRateLimited }

// Kinds of outcome other than success, for the audit line's decision.
const (
	decisionRefused = "refused" // a rule said no: the call never reached the web
	decisionFailed  = "failed"  // the web or the search service did not deliver
)

// outcome is how a failure is told to the model (text), counted (reason) and audited (decision).
type outcome struct {
	text     string
	reason   string
	decision string
}

type mapping struct {
	target   error
	reason   string
	decision string
	// text is what the model reads; empty means the error's own message, which is already a sentence for the model
	text string
}

const needsScriptText = "the content of this page is produced by script and cannot be read now: use another source, or the excerpt of the search result"

const search1 = "search first and use the address and ref exactly as listed"

// mappings is the table from each sentinel of fetch, refs, tenants, search and addr to the sentence the model reads,
// in the order they are tried. TestEverySentinelHasAFixedSentence walks the sentinels of the packages and fails when
// one is missing.
var mappings = []mapping{
	{ErrInvalid, "invalid_arguments", decisionRefused, ""},
	{ErrNoCaller, "no_caller", decisionRefused, ""},
	{ErrNotAvailable, "unavailable", decisionFailed, ""},
	{ErrRateLimited, "rate_limited", decisionRefused, ""},
	{ErrQuotaExceeded, "quota_exceeded", decisionRefused, ""},
	{ErrQuotaUnavailable, "quota_unavailable", decisionRefused, ""},
	{ErrNumberingUnavailable, "numbering_unavailable", decisionFailed, "the sources of this answer could not be numbered just now: try again in a little while, or answer with what you have"},
	{refs.ErrMalformed, "ref_malformed", decisionRefused, "that address did not come from a search result, or its ref is not the one listed: " + search1 + "; other addresses cannot be read"},
	{refs.ErrNotForIt, "ref_mismatch", decisionRefused, "that address did not come from a search result: " + search1 + "; other addresses cannot be read"},
	{refs.ErrExpired, "ref_expired", decisionRefused, "the ref has expired: search again to get a fresh address and ref"},
	{tenants.ErrDisabled, "tenant_disabled", decisionRefused, "web search and reading are not enabled for your organisation: tell the member, and do not try again"},
	{tenants.ErrOpenDisabled, "open_url_disabled", decisionRefused, ""},
	{tenants.ErrExcluded, "domain_excluded", decisionRefused, "reading from that domain is not allowed for your organisation: use another source"},
	{tenants.ErrNotIncluded, "domain_not_included", decisionRefused, "that domain is not among those your organisation allows: use another source"},
	{search.ErrBusy, "provider_busy", decisionFailed, "the search service is busy: try again in a little while, or answer with what you have"},
	{search.ErrUnavailable, "provider_error", decisionFailed, "the search failed: try again with other words, or answer with what you have"},
	// the address rules (addr) and the reader (fetch): their messages are written for the model and never name internals
	// the more specific row comes first: addr.ErrNotPublic also satisfies errors.Is(err, addr.ErrRefused), so with the
	// rows the other way round no address was ever recorded as not public and an operator reading the audit could not
	// tell a malformed address from one that points inside. Keep any row whose error wraps another's above it.
	{fetch.ErrNotPublic, "address_not_public", decisionRefused, ""},
	{addr.ErrRefused, "address_refused", decisionRefused, ""},
	{fetch.ErrRobots, "robots", decisionRefused, ""},
	{fetch.ErrTooLarge, "too_large", decisionFailed, ""},
	{fetch.ErrTooSlow, "too_slow", decisionFailed, ""},
	{fetch.ErrType, "content_type", decisionFailed, ""},
	// the PDF reader (ADR-0001 D31): each of its refusals has a reason of its own, so that an operator reading the audit
	// or the counter can tell a scan from a damaged file from a reader that is not there; the sentences are fixed here
	// because the reader's own errors carry the parser's words for the log, which the model must not read
	{fetch.ErrPDFNoText, "pdf_no_text_layer", decisionFailed, "the PDF has no text layer (it is a scan or pictures only) and this service does not do character recognition: use another source, or the excerpt of the search result"},
	{fetch.ErrPDFEncrypted, "pdf_encrypted", decisionFailed, "the PDF is protected with a password: use another source, or the excerpt of the search result"},
	{fetch.ErrPDFMalformed, "pdf_malformed", decisionFailed, "the PDF is damaged, or not a valid PDF: use another source, or the excerpt of the search result"},
	{fetch.ErrPDFTooLarge, "pdf_too_large", decisionFailed, "the PDF holds more data than can be read: use another source, or the excerpt of the search result"},
	{fetch.ErrPDFTooSlow, "pdf_too_slow", decisionFailed, "the PDF could not be read within the time allowed: use another source, or the excerpt of the search result"},
	{fetch.ErrPDFUnavailable, "pdf_reader_unavailable", decisionFailed, "reading PDFs is not available in this service: use another source, or the excerpt of the search result"},
	{fetch.ErrPDFBusy, "pdf_busy", decisionFailed, "the PDF reader is busy with other documents: try again in a little while, or use another source"},
	{fetch.ErrEncoding, "encoding", decisionFailed, ""},
	{fetch.ErrBotCheck, "bot_check", decisionFailed, ""},
	{fetch.ErrNeedsScript, "needs_script", decisionFailed, needsScriptText},
	// the site refused in words, or the renderer said so (D33): a reason that may pass, so another source is the remedy
	{fetch.ErrBlockedBySite, "blocked_by_site", decisionFailed, "the site refused to show this page to an automated reader: use another source"},
	{fetch.ErrStatus, "http_status", decisionFailed, ""},
	{fetch.ErrTooManyRedirects, "redirects", decisionFailed, ""},
	{fetch.ErrConnect, "connect", decisionFailed, ""},
	{fetch.ErrUnreadable, "unreadable", decisionFailed, ""},
}

// classify finds the sentence for err. An error that is in no row is an internal one: the model gets one generic
// sentence, and the caller logs the detail.
func classify(err error) (outcome, bool) {
	// a page that needed the renderer and did not get it: the model reads the needs_script sentence, whatever the reason
	// was, and the audit and the counter get the renderer's code (render_busy, render_failed, ...) (D33)
	var rf *renderFailure
	if errors.As(err, &rf) {
		return outcome{needsScriptText, rf.code, decisionFailed}, true
	}
	for _, m := range mappings {
		if errors.Is(err, m.target) {
			text := m.text
			if text == "" {
				text = err.Error()
				if m.decision == decisionFailed && !errors.Is(err, ErrNotAvailable) {
					text += ": use another source"
				}
			}
			return outcome{text, m.reason, m.decision}, true
		}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return outcome{genericText, "canceled", decisionFailed}, false
	}
	return outcome{genericText, "internal", decisionFailed}, false
}

const genericText = "the web tool failed to complete the request"

// renderFailure is a read that the plain reader answered with ErrNeedsScript (or that was to be rendered first) and
// that the renderer did not turn into a page. It is ErrNeedsScript for everyone who asks, and carries why in code.
type renderFailure struct{ code string }

func (e *renderFailure) Error() string {
	return fetch.ErrNeedsScript.Error() + " (the renderer: " + e.code + ")"
}
func (e *renderFailure) Is(target error) bool { return target == fetch.ErrNeedsScript }
