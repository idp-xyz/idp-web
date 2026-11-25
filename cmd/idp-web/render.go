package main

import (
	"fmt"
	"log/slog"
	"os"

	"idp-web/internal/render"
	"idp-web/internal/tenants"
	"idp-web/internal/tools"
)

// startRenderer prepares calling the renderer (idp-browser) for the pages that need script (ADR-0001 D33).
//
//	IDP_WEB_RENDER_URL         the render endpoint, for example http://idp-browser.idp-browser.svc.cluster.local:8590/render;
//	                           also the audience of the signed requests, exactly as written. Unset: nothing is rendered
//	IDP_WEB_RENDER_KEY_FILE    idp-web's P-256 private key (PEM) whose public half the renderer holds. Required with the URL
//	IDP_WEB_RENDER_TIMEOUT_MS  how long the renderer may take for a page (default 15000)
//	IDP_WEB_RENDER_MAX_BYTES   the most HTML it may hand back (default 5 MiB, at most 20 MiB)
//
// With the URL unset it returns nil and the service behaves as it did before the renderer; the other settings are then
// ignored with a warning. A URL without a usable key is an error: a renderer that every request would be refused by is
// not a configuration to start with. It does not call the renderer: one that is down at start is no reason not to start.
func startRenderer(getenv func(string) string, ts *tenants.Set, log *slog.Logger) (tools.Renderer, error) {
	url := getenv("IDP_WEB_RENDER_URL")
	if url == "" {
		for _, k := range []string{"IDP_WEB_RENDER_KEY_FILE", "IDP_WEB_RENDER_TIMEOUT_MS", "IDP_WEB_RENDER_MAX_BYTES"} {
			if getenv(k) != "" {
				log.Warn(k + " is set without IDP_WEB_RENDER_URL: it is ignored, no page is rendered")
			}
		}
		if names := ts.AlwaysRenders(); len(names) > 0 {
			log.Warn("alwaysRender is set in the tenants file but no renderer is configured (IDP_WEB_RENDER_URL): it does nothing", "tenants", names)
		}
		log.Info("rendering is off (IDP_WEB_RENDER_URL is not set)")
		return nil, nil
	}
	if _, err := render.ParseURL(url); err != nil {
		return nil, fmt.Errorf("IDP_WEB_RENDER_URL: %w", err)
	}
	if getenv("IDP_WEB_RENDER_KEY_FILE") == "" {
		return nil, fmt.Errorf("IDP_WEB_RENDER_KEY_FILE is not set: IDP_WEB_RENDER_URL needs the key that signs the requests")
	}
	pem, err := os.ReadFile(getenv("IDP_WEB_RENDER_KEY_FILE"))
	if err != nil {
		return nil, fmt.Errorf("IDP_WEB_RENDER_KEY_FILE: %w", err)
	}
	signer, err := render.NewSigner(pem)
	if err != nil {
		return nil, fmt.Errorf("IDP_WEB_RENDER_KEY_FILE: %w", err)
	}
	ms, err := positive(getenv, "IDP_WEB_RENDER_TIMEOUT_MS", render.DefaultTimeoutMS)
	if err != nil {
		return nil, err
	}
	maxBytes, err := positive(getenv, "IDP_WEB_RENDER_MAX_BYTES", render.DefaultMaxBytes)
	if err != nil {
		return nil, err
	}
	c, err := render.New(render.Config{URL: url, Signer: signer, TimeoutMS: ms, MaxBytes: maxBytes})
	if err != nil {
		return nil, fmt.Errorf("IDP_WEB_RENDER_*: %w", err)
	}
	log.Info("rendering pages that need script", "url", url, "keyId", signer.KeyID(), "timeoutMs", ms, "maxBytes", maxBytes, "alwaysRender", ts.AlwaysRenders())
	return c, nil
}
