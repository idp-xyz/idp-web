package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"idp-web/internal/refs"
	"idp-web/internal/search"
	"idp-web/internal/tenants"
	"idp-web/internal/tools"
)

// setup is what the service needs besides its listeners: the search provider, the ref keys, the tenants and the
// limits. A missing or invalid one is a configuration error (exit status 2), so a running service always has all of
// them; /readyz still asks (ADR-0001 D24).
type setup struct {
	provider     search.Provider
	providerName string
	keys         *refs.Keys
	tenants      *tenants.Set
	limits       tools.Limits
}

// missing names what is not loaded, for /readyz.
func (s *setup) missing() []string {
	var m []string
	if s == nil || s.provider == nil {
		m = append(m, "search provider")
	}
	if s == nil || s.keys == nil {
		m = append(m, "ref key")
	}
	if s == nil || s.tenants == nil {
		m = append(m, "tenants file")
	}
	return m
}

func readSecret(name, path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("%s is not set", name)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	k := strings.TrimSpace(string(b))
	if k == "" {
		return "", fmt.Errorf("%s: the file is empty", name)
	}
	return k, nil
}

func positive(getenv func(string) string, name string, def int) (int, error) {
	v := getenv(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("%s must be a positive whole number, not %q", name, v)
	}
	return n, nil
}

// loadSetup reads the settings of the search service, the ref keys, the tenants and the limits from the environment.
// Keys come from files, never from the environment itself, where a process listing or a crash report would show them.
func loadSetup(getenv func(string) string, log *slog.Logger) (*setup, error) {
	s := &setup{}
	var err error
	if s.keys, err = refs.Load(getenv("IDP_WEB_REF_KEY_FILE")); err != nil {
		if getenv("IDP_WEB_REF_KEY_FILE") == "" {
			return nil, errors.New("IDP_WEB_REF_KEY_FILE is not set: refs cannot be made without a key of at least 32 bytes")
		}
		return nil, fmt.Errorf("IDP_WEB_REF_KEY_FILE: %w", err)
	}
	if getenv("IDP_WEB_TENANTS_FILE") == "" {
		return nil, errors.New("IDP_WEB_TENANTS_FILE is not set: without it no tenant may use the tools")
	}
	if s.tenants, err = tenants.Load(getenv("IDP_WEB_TENANTS_FILE")); err != nil {
		return nil, fmt.Errorf("IDP_WEB_TENANTS_FILE: %w", err)
	}
	s.providerName = getenv("IDP_WEB_PROVIDER")
	switch s.providerName {
	case "":
		return nil, errors.New("IDP_WEB_PROVIDER is not set: fixture, exa or brave")
	case "fixture":
		if getenv("IDP_WEB_FIXTURE_FILE") == "" {
			return nil, errors.New("IDP_WEB_FIXTURE_FILE is not set")
		}
		if s.provider, err = search.LoadFixture(getenv("IDP_WEB_FIXTURE_FILE")); err != nil {
			return nil, fmt.Errorf("IDP_WEB_FIXTURE_FILE: %w", err)
		}
	case "exa":
		key, err := readSecret("IDP_WEB_EXA_KEY_FILE", getenv("IDP_WEB_EXA_KEY_FILE"))
		if err != nil {
			return nil, err
		}
		base := getenv("IDP_WEB_EXA_BASE_URL")
		if u, perr := url.Parse(base); base != "" && (perr != nil || u.Scheme != "https") {
			return nil, fmt.Errorf("IDP_WEB_EXA_BASE_URL must be an https address, not %q: the API key travels to it", base)
		}
		if s.provider, err = search.NewExa(base, key, getenv("IDP_WEB_EXA_TYPE"), nil, log); err != nil {
			return nil, fmt.Errorf("IDP_WEB_EXA_*: %w", err)
		}
	case "brave":
		key, err := readSecret("IDP_WEB_BRAVE_KEY_FILE", getenv("IDP_WEB_BRAVE_KEY_FILE"))
		if err != nil {
			return nil, err
		}
		if s.provider, err = search.NewBrave("", key, nil, log); err != nil {
			return nil, fmt.Errorf("IDP_WEB_BRAVE_KEY_FILE: %w", err)
		}
	default:
		return nil, fmt.Errorf("IDP_WEB_PROVIDER %q is not fixture, exa or brave", s.providerName)
	}
	d := tools.DefaultLimits
	if s.limits.Searches, err = positive(getenv, "IDP_WEB_LIMIT_SEARCHES", d.Searches); err != nil {
		return nil, err
	}
	if s.limits.Reads, err = positive(getenv, "IDP_WEB_LIMIT_READS", d.Reads); err != nil {
		return nil, err
	}
	secs, err := positive(getenv, "IDP_WEB_LIMIT_WINDOW_S", int(d.Window/time.Second))
	if err != nil {
		return nil, err
	}
	s.limits.Window = time.Duration(secs) * time.Second
	return s, nil
}
