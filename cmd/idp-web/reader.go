package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"idp-web/internal/fetch"
	"idp-web/internal/pdfread"
)

// parseTestOrigins reads IDP_WEB_TEST_ORIGINS: comma-separated host:port pairs, each an exact origin that the reader
// may contact although it is not a public address or not on port 80 or 443 (ADR-0001 D14). It is for tests only; the
// service logs a warning at start when it is set. The pairs come out in the form addr.Address.HostPort gives (a lower
// case name or a canonical IP, no brackets in the host part), because that is what they are compared with. A wildcard,
// a missing port or an empty host is an error: a loose entry would exempt more than was meant.
func parseTestOrigins(s string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, item := range strings.Split(s, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		host, port, err := net.SplitHostPort(item)
		if err != nil || host == "" {
			return nil, fmt.Errorf("%q is not host:port", item)
		}
		p, err := strconv.Atoi(port)
		if err != nil || p < 1 || p > 65535 {
			return nil, fmt.Errorf("%q has no valid port", item)
		}
		if strings.ContainsAny(host, "*?%") {
			return nil, fmt.Errorf("%q: only exact hosts are accepted", item)
		}
		if ip, err := netip.ParseAddr(host); err == nil {
			host = ip.String()
		} else {
			host = strings.ToLower(host)
		}
		out[net.JoinHostPort(host, strconv.Itoa(p))] = true
	}
	return out, nil
}

// newReader is the page reader of the service, configured from the environment.
func newReader(log *slog.Logger, origins map[string]bool, pdf fetch.PDFReader) *fetch.Reader {
	return fetch.New(fetch.Config{TestOrigins: origins, Version: version, Log: log, PDF: pdf})
}

// startPDFReader prepares reading PDFs (ADR-0001 D31) and returns nil, with the reason in the log, when it cannot be
// offered: the service then answers a PDF with "reading PDFs is not available" instead of failing obscurely on every
// call. IDP_WEB_PDF=off turns it off on purpose. The reader is a command of its own, IDP_WEB_PDF_READER or idp-web-pdf
// next to this executable; before the service takes calls it is run on a document built here, which proves that it
// starts, that it can apply its limits and that it reads. IDP_WEB_PDF_CONCURRENCY (default 2) is how many documents are
// read at once; a call finding all the slots taken waits two seconds for one and then says the reader is busy.
func startPDFReader(getenv func(string) string, log *slog.Logger) (fetch.PDFReader, error) {
	switch getenv("IDP_WEB_PDF") {
	case "", "on":
	case "off":
		log.Info("reading PDFs is switched off (IDP_WEB_PDF=off)")
		return nil, nil
	default:
		return nil, fmt.Errorf("IDP_WEB_PDF must be on or off, not %q", getenv("IDP_WEB_PDF"))
	}
	n, err := positive(getenv, "IDP_WEB_PDF_CONCURRENCY", 2)
	if err != nil {
		return nil, err
	}
	path, err := pdfread.FindCommand(getenv("IDP_WEB_PDF_READER"))
	if err != nil {
		log.Error("reading PDFs is not available: the PDF reader command was not found", "err", err)
		return nil, nil
	}
	run := pdfread.NewRunner(path, n, log)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := run.Check(ctx); err != nil {
		log.Error("reading PDFs is not available: the PDF reader failed its self test", "path", path, "err", err)
		return nil, nil
	}
	log.Info("reading PDFs", "reader", path, "concurrency", n)
	return run, nil
}

// checkTLS is `idp-web -check-tls URL`: it reads one page through the service's own reader and prints what came back.
// It exists to show, from inside the image (which has no CA certificates of its own, ADR-0001 D13), that an HTTPS
// handshake verifies. The exit status is 0 when the page was read, 1 when it was not and 2 for a bad argument.
func checkTLS(ctx context.Context, args []string, out, errw io.Writer, log *slog.Logger) int {
	if len(args) != 1 || !strings.HasPrefix(args[0], "https://") {
		fmt.Fprintln(errw, "usage: idp-web -check-tls https://host/")
		return 2
	}
	page, err := newReader(log, nil, nil).Read(ctx, args[0])
	if err != nil {
		fmt.Fprintln(out, "failed:", err)
		return 1
	}
	fmt.Fprintf(out, "ok: read %s over TLS, %d bytes, type %s, title %q, engine %s\n", page.FinalURL, page.Bytes, page.Type, strings.Join(strings.Fields(page.Title), " "), page.Engine)
	return 0
}
