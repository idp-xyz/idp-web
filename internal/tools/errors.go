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
)

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

const search1 = "search first and use the address and ref exactly as listed"

// mappings is the table from each sentinel of fetch, refs, tenants, search and addr to the sentence the model reads,
// in the order they are tried. TestEverySentinelHasAFixedSentence walks the sentinels of the packages and fails when
// one is missing.
var mappings = []mapping{
	{ErrInvalid, "invalid_arguments", decisionRefused, ""},
	{ErrNoCaller, "no_caller", decisionRefused, ""},
	{ErrNotAvailable, "unavailable", decisionFailed, ""},
	{ErrRateLimited, "rate_limited", decisionRefused, ""},
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
	{addr.ErrRefused, "address_refused", decisionRefused, ""},
	{fetch.ErrNotPublic, "address_not_public", decisionRefused, ""},
	{fetch.ErrRobots, "robots", decisionRefused, ""},
	{fetch.ErrTooLarge, "too_large", decisionFailed, ""},
	{fetch.ErrTooSlow, "too_slow", decisionFailed, ""},
	{fetch.ErrType, "content_type", decisionFailed, ""},
	{fetch.ErrPDF, "pdf", decisionFailed, ""},
	{fetch.ErrEncoding, "encoding", decisionFailed, ""},
	{fetch.ErrBotCheck, "bot_check", decisionFailed, ""},
	{fetch.ErrStatus, "http_status", decisionFailed, ""},
	{fetch.ErrTooManyRedirects, "redirects", decisionFailed, ""},
	{fetch.ErrConnect, "connect", decisionFailed, ""},
	{fetch.ErrUnreadable, "unreadable", decisionFailed, ""},
}

// classify finds the sentence for err. An error that is in no row is an internal one: the model gets one generic
// sentence, and the caller logs the detail.
func classify(err error) (outcome, bool) {
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
