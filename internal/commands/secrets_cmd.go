package commands

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/sydlexius/mxlrcgo-svc/internal/config"
	"github.com/sydlexius/mxlrcgo-svc/internal/db"
	"github.com/sydlexius/mxlrcgo-svc/internal/secrets"
)

// validSecretNames lists the secret names accepted by `secrets set`. v1 wires
// only these two; the store table is general so this list grows as needed.
var validSecretNames = []string{secrets.NameMusixmatchToken, secrets.NameWebhookAPIKey}

// runSecrets dispatches the `secrets import|set|list` subcommands. Each opens the
// config + DB and builds the encrypted store via resolveSecretStore. Store-init
// errors are fatal. No subcommand ever echoes or logs a secret value.
func runSecrets(ctx context.Context, out io.Writer, args SecretsCmd) int {
	path := secretsConfigPath(args)
	cfg, err := config.Load(path)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		return 1
	}
	sqlDB, err := db.Open(ctx, cfg.DB.Path)
	if err != nil {
		slog.Error("failed to open database", "error", err)
		return 1
	}
	defer func() { _ = sqlDB.Close() }()

	store, err := resolveSecretStore(cfg, sqlDB)
	if err != nil {
		slog.Error("failed to initialize secret store", "error", err)
		return 1
	}

	switch {
	case args.Import != nil:
		return runSecretsImport(ctx, out, cfg, store, *args.Import)
	case args.Set != nil:
		return runSecretsSet(ctx, out, store, *args.Set, os.Stdin)
	case args.List != nil:
		return runSecretsList(ctx, out, store)
	default:
		_, _ = fmt.Fprintln(out, "missing secrets subcommand")
		return 2
	}
}

// runSecretsImport encrypts the currently effective plaintext secret(s) into the
// store, resolving the normal precedence (CLI/env/TOML, already folded into cfg)
// but SKIPPING the DB tier as a source so it never reads-then-writes the DB onto
// itself. With neither --token nor --webhook it imports both currently-set
// secrets; a target with no effective higher-tier value is skipped with a clear
// message rather than writing an empty secret. store.Set upserts, so re-running
// is idempotent. The secret value is never echoed.
func runSecretsImport(ctx context.Context, out io.Writer, cfg config.Config, store secrets.Store, args SecretsImportCmd) int {
	importToken := args.Token || (!args.Token && !args.Webhook)
	importWebhook := args.Webhook || (!args.Token && !args.Webhook)

	imported := 0
	if importToken {
		token := strings.TrimSpace(cfg.API.Token)
		if token == "" {
			_, _ = fmt.Fprintf(out, "skipping %s: no effective value from CLI/env/config\n", secrets.NameMusixmatchToken)
		} else {
			if err := store.Set(ctx, secrets.NameMusixmatchToken, token); err != nil {
				slog.Error("failed to import musixmatch token", "error", err)
				return 1
			}
			_, _ = fmt.Fprintf(out, "imported %s\n", secrets.NameMusixmatchToken)
			imported++
		}
	}
	if importWebhook {
		webhook := firstNonEmpty(cfg.Server.WebhookAPIKeys)
		if webhook == "" {
			_, _ = fmt.Fprintf(out, "skipping %s: no effective value from CLI/env/config\n", secrets.NameWebhookAPIKey)
		} else {
			if err := store.Set(ctx, secrets.NameWebhookAPIKey, webhook); err != nil {
				slog.Error("failed to import webhook API key", "error", err)
				return 1
			}
			_, _ = fmt.Fprintf(out, "imported %s\n", secrets.NameWebhookAPIKey)
			imported++
		}
	}

	if imported > 0 {
		_, _ = fmt.Fprintln(out, "secret(s) are now encrypted at rest in the DB. Remove the now-redundant plaintext from config.toml / your compose env so it is no longer stored in the clear.")
	}
	return 0
}

// runSecretsSet sets one named secret from stdin (prompt or pipe), never from
// argv. A value passed positionally is rejected because argv lands in shell
// history and ps. The name must be one of validSecretNames. The value is never
// echoed or logged.
func runSecretsSet(ctx context.Context, out io.Writer, store secrets.Store, args SecretsSetCmd, stdin *os.File) int {
	if strings.TrimSpace(args.Value) != "" {
		_, _ = fmt.Fprintln(os.Stderr, "refusing to read the secret value from the command line (it lands in shell history and ps); provide it via stdin: pipe it in, or run without the value and enter it at the prompt")
		return 2
	}
	name := strings.TrimSpace(args.Name)
	if !isValidSecretName(name) {
		_, _ = fmt.Fprintf(os.Stderr, "unknown secret name %q; valid names: %s\n", name, strings.Join(validSecretNames, ", "))
		return 2
	}

	// Prompt only when stdin is an interactive terminal; for a pipe, read silently.
	// golang.org/x/term is not a dependency, so the typed value is not hidden from
	// the terminal; the prompt warns the operator. The value is read from stdin,
	// never argv, which is the property that matters (no shell-history/ps exposure).
	//
	// TTY path: read one line (Enter-terminated). A single ReadString('\n') is the
	// correct interactive UX -- the user presses Enter and we proceed immediately.
	//
	// Pipe path: read the full payload with io.ReadAll so a multi-line or
	// newline-free value is not silently truncated at the first '\n'.
	var value string
	if isTerminal(stdin) {
		_, _ = fmt.Fprintf(os.Stderr, "Enter value for %s (input is read from this terminal and not stored in shell history): ", name)
		reader := bufio.NewReader(stdin)
		line, err := reader.ReadString('\n')
		if err != nil && err != io.EOF {
			slog.Error("failed to read secret value from stdin", "error", err)
			return 1
		}
		value = strings.TrimRight(line, "\r\n")
	} else {
		data, err := io.ReadAll(stdin)
		if err != nil {
			slog.Error("failed to read secret value from stdin", "error", err)
			return 1
		}
		value = strings.TrimRight(string(data), "\r\n")
	}
	if value == "" {
		_, _ = fmt.Fprintln(os.Stderr, "no value provided on stdin; aborting")
		return 2
	}
	if err := store.Set(ctx, name, value); err != nil {
		slog.Error("failed to set secret", "error", err)
		return 1
	}
	_, _ = fmt.Fprintf(out, "set %s\n", name)
	return 0
}

// runSecretsList prints stored secret names and updated_at only, never values.
func runSecretsList(ctx context.Context, out io.Writer, store secrets.Store) int {
	infos, err := store.List(ctx)
	if err != nil {
		slog.Error("failed to list secrets", "error", err)
		return 1
	}
	if len(infos) == 0 {
		_, _ = fmt.Fprintln(out, "no secrets stored")
		return 0
	}
	for _, info := range infos {
		_, _ = fmt.Fprintf(out, "%s\t%s\n", info.Name, info.UpdatedAt)
	}
	return 0
}

func secretsConfigPath(args SecretsCmd) string {
	switch {
	case args.Import != nil:
		return args.Import.ConfigPath
	case args.Set != nil:
		return args.Set.ConfigPath
	case args.List != nil:
		return args.List.ConfigPath
	default:
		return ""
	}
}

func isValidSecretName(name string) bool {
	for _, v := range validSecretNames {
		if v == name {
			return true
		}
	}
	return false
}

func firstNonEmpty(values []string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// isTerminal reports whether f is an interactive character device (a TTY). It
// avoids a golang.org/x/term dependency by inspecting the file mode.
func isTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
