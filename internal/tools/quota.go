package tools

import (
	"context"
	"time"

	"idp-web/internal/mcp"
	"idp-web/internal/tenants"
)

// take counts one search or one page read against the tenant's daily quota (ADR-0001 D28), and is called immediately
// before the call goes out, after every check that can refuse it (arguments, tenant, ref, address rules, the sliding
// window). kind is state.Searches or state.Reads.
//
//   - A tenant with no daily limit for the kind never touches the database here: a deployment that sets no quota keeps
//     working while the database is down.
//   - A limit of 0 allows none, and needs no database to say so.
//   - Otherwise the decision is the database's single conditional statement. If it cannot be had (no database
//     configured, a timeout, any error) the call is refused with ErrQuotaUnavailable: never allowed without counting.
//
// The returned release gives the unit back; it is for a read that turned out to send nothing, and does nothing when no
// unit was taken. A release that fails leaves the unit taken, which counts too much and never too little.
func (a *api) take(ctx context.Context, pol tenants.Policy, caller mcp.Caller, kind, what string) (release func(), err error) {
	noop := func() {}
	limit, limited := pol.Daily(kind)
	if !limited {
		return noop, nil
	}
	if limit <= 0 {
		return noop, &quotaExceeded{what}
	}
	if a.cfg.State == nil {
		a.log.Error("a daily limit is set but there is no database", "tenant", caller.Tenant)
		return noop, &quotaUnavailable{what}
	}
	now := a.cfg.Now()
	ok, err := a.cfg.State.Take(ctx, caller.Tenant, kind, now, limit)
	if err != nil {
		a.log.Error("the daily quota could not be checked", "tenant", caller.Tenant, "kind", kind, "err", err)
		return noop, &quotaUnavailable{what}
	}
	if !ok {
		return noop, &quotaExceeded{what}
	}
	return func() {
		// not the call's context: a call that was cancelled still has to give its unit back
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		if err := a.cfg.State.Release(c, caller.Tenant, kind, now); err != nil {
			a.log.Error("a unit of the daily quota could not be given back", "tenant", caller.Tenant, "kind", kind, "err", err)
		}
	}, nil
}
