package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"idp-web/internal/state"
)

// database runs the two commands that prepare the database, as the siblings idp-mcp and idp-host do: migrate (apply the
// embedded migrations as the schema owner) and ensure-login USER (create or update the login role the service runs as).
// args[0] is the command. It returns the exit status.
func database(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	owner, err := secret(getenv, "IDP_WEB_MIGRATION_DATABASE_URL")
	if err == nil && owner == "" {
		err = errors.New("IDP_WEB_MIGRATION_DATABASE_URL is not set: the schema owner's URL (or IDP_WEB_MIGRATION_DATABASE_URL_FILE)")
	}
	if err == nil {
		switch args[0] {
		case "migrate":
			var applied []string
			if applied, err = state.Migrate(ctx, owner); err == nil {
				fmt.Fprintf(stdout, "OK: %d migration(s) applied %v\n", len(applied), applied)
			}
		case "ensure-login":
			var password string
			if len(args) != 2 {
				err = errors.New("usage: ensure-login USER (password in IDP_WEB_APP_DB_PASSWORD or IDP_WEB_APP_DB_PASSWORD_FILE)")
			} else if password, err = secret(getenv, "IDP_WEB_APP_DB_PASSWORD"); err == nil && password == "" {
				err = errors.New("IDP_WEB_APP_DB_PASSWORD is not set (or IDP_WEB_APP_DB_PASSWORD_FILE)")
			}
			if err == nil {
				if err = state.EnsureLogin(ctx, owner, args[1], password); err == nil {
					fmt.Fprintf(stdout, "OK: login role %s is a member of idp_web_app\n", args[1])
				}
			}
		}
	}
	if err != nil {
		fmt.Fprintln(stderr, "idp-web", args[0]+":", err)
		return 1
	}
	return 0
}
