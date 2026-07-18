package commands

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/webauth"
)

// runAdmin dispatches the admin subcommand tree.
func runAdmin(ctx context.Context, out io.Writer, stdin io.Reader, args AdminCmd) int {
	switch {
	case args.SetPassword != nil:
		return runAdminSetPassword(ctx, out, stdin, *args.SetPassword)
	default:
		fmt.Fprintln(out, "usage: canticle admin set-password --user <name>") //nolint:errcheck // reason: usage text to a caller-supplied writer; a write failure has no recovery path
		return 2
	}
}

// runAdminSetPassword sets an existing web-UI admin's password (#545).
//
// The password is read from stdin, never a flag: a --password flag would put the
// credential in shell history and in the process table, where any other user on
// the host can read it. Piping is the supported form:
//
//	canticle admin set-password --user jesse < newpass.txt
//	printf '%s' "$NEW" | canticle admin set-password --user jesse
//
// A trailing newline is stripped so the common `echo` and heredoc forms do not
// silently set a password with a newline on the end -- a failure that would only
// surface later as "the password I set does not work".
func runAdminSetPassword(ctx context.Context, out io.Writer, stdin io.Reader, args AdminSetPasswordCmd) int {
	user := strings.TrimSpace(args.User)
	if user == "" {
		slog.Error("admin set-password: --user is required")
		return 2
	}

	password, err := readPasswordFromStdin(stdin)
	if err != nil {
		slog.Error("admin set-password: read password", "error", err)
		return 1
	}
	if password == "" {
		slog.Error("admin set-password: no password supplied on stdin")
		return 2
	}

	cfg, err := config.Load(args.ConfigPath)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		return 1
	}
	sqlDB, err := db.Open(ctx, cfg.DB.Path)
	if err != nil {
		slog.Error("failed to open database", "error", err)
		return 1
	}
	defer sqlDB.Close() //nolint:errcheck // best-effort close on shutdown

	svc := webauth.NewService(webauth.NewSQLUserStore(sqlDB), webauth.NewSQLSessionStore(sqlDB))
	if err := svc.SetPassword(ctx, user, password); err != nil {
		switch {
		case errors.Is(err, webauth.ErrUserNotFound):
			slog.Error("admin set-password: no such user", "user", user)
		case errors.Is(err, webauth.ErrPasswordTooShort):
			slog.Error("admin set-password: password too short", "minimum", webauth.MinPasswordLength)
		default:
			slog.Error("admin set-password failed", "error", err)
		}
		return 1
	}

	// Never echo the password. Naming the user and the session revocation is the
	// useful confirmation: the operator needs to know existing logins are dead.
	fmt.Fprintf(out, "password updated for %q; existing sessions revoked\n", user) //nolint:errcheck // reason: confirmation text; the rotation already succeeded and a write failure must not fail the command
	return 0
}

// readPasswordFromStdin reads a single line, tolerating a missing trailing
// newline (the `printf` form) and stripping a present one (the `echo` and
// heredoc forms).
func readPasswordFromStdin(stdin io.Reader) (string, error) {
	r := bufio.NewReader(stdin)
	line, err := r.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}
