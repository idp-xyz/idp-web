package tools

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"idp-web/internal/addr"
	"idp-web/internal/fetch"
	"idp-web/internal/render"
	"idp-web/internal/tenants"
)

// Renderer is the client of the renderer (internal/render.Client). Nil: pages are not rendered, and everything behaves
// as it did before the renderer (ADR-0001 D33).
type Renderer interface {
	Render(ctx context.Context, rawURL string) (render.Result, error)
}

// Rendering is what the reader (internal/fetch.Reader) offers the renderer's path: the checks that come before an
// address is sent anywhere (address rules, the tenant's lists, robots.txt), and the extraction of a rendered page, the
// same one a fetched page goes through. A Reader that is not a Rendering switches rendering off.
type Rendering interface {
	Check(ctx context.Context, rawURL string) error
	FromRendered(finalURL, html string) (fetch.Page, error)
}

// Outcomes of a render, for the counter and the audit line ("ok", the renderer's codes, and these).
const (
	renderOK      = "ok"
	renderThin    = "thin"    // rendered, and still a frame by D27's rules
	renderRefused = "refused" // rendered, and the address it ended at is not one this tenant may read
)

// loadPage gets the page for u. The plain reader goes first, always: it makes the checks (address rules, the tenant's
// lists, robots.txt), reads PDFs and other files itself, and says what the answer was. The renderer is asked, once, only
// when the answer was an HTML page (fetch.WithHTML: text/html or application/xhtml+xml, status 2xx; a browser given a
// file answers render_failed, so a PDF is never sent to it) and either the plain reader judged it needs script (D27) or
// the tenant lists the host (alwaysRender: the rendered text replaces the fetched). When the renderer does not give a
// page the plain result stands: its page for a listed host, its needs_script with the renderer's reason otherwise. A
// renderer that is down changes nothing else. sent says that a request went out to the web, by either path, which is
// what counts a read (D28).
func (a *api) loadPage(ctx, rctx context.Context, host, u string, pol tenants.Policy, au *audit, sent *atomic.Bool) (fetch.Page, error) {
	if a.renderer == nil {
		return a.cfg.Reader.Read(rctx, u)
	}
	var html atomic.Bool
	page, err := a.cfg.Reader.Read(fetch.WithHTML(rctx, func() { html.Store(true) }), u)
	needs := err != nil && errors.Is(err, fetch.ErrNeedsScript)
	if !html.Load() || !(needs || (err == nil && pol.RendersFirst(host))) {
		return page, err
	}
	rendered, rerr := a.renderOnce(ctx, rctx, u, pol, au, sent)
	if rerr == nil {
		return rendered, nil
	}
	var rf *renderFailure
	if err == nil && (errors.As(rerr, &rf) || errors.Is(rerr, fetch.ErrBlockedBySite)) {
		return page, nil // a listed host the renderer could not do: the page as fetched is still a page
	}
	return fetch.Page{}, rerr
}

// renderOnce asks the renderer for u and turns the answer into a page of the same kind a fetched one is. Its errors are
// the sentinels the table of errors.go knows: a *renderFailure for what makes the page "needs script" with a reason of
// its own, ErrBlockedBySite when the site refused, the rules' own errors when the page ended somewhere this tenant may not read.
func (a *api) renderOnce(ctx, rctx context.Context, u string, pol tenants.Policy, au *audit, sent *atomic.Bool) (fetch.Page, error) {
	start := a.cfg.Now()
	done := func(outcome string, ms int64) {
		d := a.cfg.Now().Sub(start)
		if ms == 0 {
			ms = d.Milliseconds()
		}
		au.render, au.renderMS = outcome, ms
		a.met.Rendered(outcome, d)
	}
	res, err := a.renderer.Render(ctx, u)
	if err != nil {
		var re *render.Error
		if !errors.As(err, &re) {
			return fetch.Page{}, err // the call was cancelled
		}
		if re.Sent {
			sent.Store(true)
		}
		done(re.Code, 0)
		a.log.Info("the renderer did not give a page", "code", re.Code, "msg", re.Message)
		if re.Code == render.BlockedBySite {
			return fetch.Page{}, fmt.Errorf("%w (the renderer saw it)", fetch.ErrBlockedBySite)
		}
		return fetch.Page{}, &renderFailure{code: re.Code}
	}
	sent.Store(true)
	// The browser followed redirects of its own, and a script may have navigated: the address it ended at passes the same
	// rules as the address that was asked for, before a word of its page is read (robots.txt, the tenant's lists). A page
	// that ended somewhere the tenant may not read is refused, and nothing of it is kept.
	t, err := addr.TargetWith(res.FinalURL, addr.Options{TestOrigins: a.cfg.TestOrigins})
	if err == nil {
		err = pol.AllowHost(t.Host)
	}
	if err == nil && t.URL() != u {
		err = a.rendering.Check(rctx, t.URL())
	}
	if err != nil {
		done(renderRefused, res.MS)
		return fetch.Page{}, err
	}
	if res.Status >= 400 {
		done("http_status", res.MS)
		return fetch.Page{}, fetch.StatusError(res.Status)
	}
	page, err := a.rendering.FromRendered(t.URL(), res.HTML)
	switch {
	case err == nil:
		done(renderOK, res.MS)
		return page, nil
	case errors.Is(err, fetch.ErrNeedsScript):
		done(renderThin, res.MS)
		return fetch.Page{}, &renderFailure{code: render.RenderFailed}
	case errors.Is(err, fetch.ErrBotCheck), errors.Is(err, fetch.ErrBlockedBySite):
		done(render.BlockedBySite, res.MS)
		return fetch.Page{}, fmt.Errorf("%w (after rendering)", fetch.ErrBlockedBySite)
	}
	done(render.RenderFailed, res.MS)
	return fetch.Page{}, err
}
