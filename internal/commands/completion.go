package commands

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/library"
)

// CompletionCmd outputs a shell completion script.
type CompletionCmd struct {
	Shell string `arg:"positional,required" help:"shell to generate completion for: bash, zsh, or fish"`
}

// completionSubcommands are the top-level subcommands offered at the first word.
var completionSubcommands = []string{
	"fetch", "serve", "scan", "library", "keys", "secrets", "admin", "config", "queue", "provenance", "realign", "completion",
}

// completionCandidates maps a subcommand to the flags and/or nested subcommands
// offered after it. It is a curated set for completion convenience, not an
// exhaustive mirror of every flag.
var completionCandidates = map[string][]string{
	"fetch":      {"--outdir", "--cooldown", "--depth", "--update", "--upgrade", "--bfs", "--token", "--config", "--album", "--probe", "--isrc", "--duration", "--spotify-id"},
	"serve":      {"--listen", "--outdir", "--token", "--config", "--depth", "--update", "--upgrade", "--bfs", "--embedded-lyrics", "--scan-interval", "--sweep-interval", "--work-interval"},
	"scan":       {"results", "clear", "reconcile", "reconcile-instrumental", "reconcile-instrumental-recalibrate", "reconcile-paths", "reconcile-identity", "reconcile-lrc", "reconcile-marker-provenance", "reconcile-detector-stats", "--config", "--depth", "--update", "--upgrade", "--bfs", "--embedded-lyrics", "--only"},
	"library":    {"add", "list", "remove", "update"},
	"keys":       {"create", "list", "revoke"},
	"secrets":    {"import", "set", "list"},
	"admin":      {"set-password", "--user", "--config"},
	"config":     {"get", "set", "list"},
	"queue":      {"list", "failed", "deferred", "retry", "clear", "recheck"},
	"provenance": {"backfill"},
	"realign":    {"--library", "--yes", "--backup", "--config"},
	"completion": {"bash", "zsh", "fish"},
}

// runCompletion prints a sourceable completion script for the requested shell.
func runCompletion(out io.Writer, args CompletionCmd) int {
	switch strings.ToLower(strings.TrimSpace(args.Shell)) {
	case "bash":
		_, _ = io.WriteString(out, bashCompletion)
	case "zsh":
		_, _ = io.WriteString(out, zshCompletion)
	case "fish":
		_, _ = io.WriteString(out, fishCompletion)
	default:
		_, _ = fmt.Fprintf(out, "unsupported shell %q (want bash, zsh, or fish)\n", args.Shell)
		return 2
	}
	return 0
}

// runComplete is the hidden handler invoked by the generated scripts. words are
// the command-line tokens after the program name, the last being the current
// (possibly empty) partial word. It prints newline-separated candidates.
func runComplete(ctx context.Context, out io.Writer, words []string) int {
	cur := ""
	prior := words
	if len(words) > 0 {
		cur = words[len(words)-1]
		prior = words[:len(words)-1]
	}

	var candidates []string
	switch sub := firstSubcommand(prior); sub {
	case "":
		candidates = append(candidates, completionSubcommands...)
	default:
		candidates = append(candidates, completionCandidates[sub]...)
		// Offer configured library names where a name argument is expected and
		// the user is not partway through typing a flag.
		if (sub == "scan" || sub == "library") && !strings.HasPrefix(cur, "-") {
			candidates = append(candidates, completionLibraryNames(ctx)...)
		}
	}

	sort.Strings(candidates)
	for _, c := range candidates {
		if cur == "" || strings.HasPrefix(c, cur) {
			_, _ = fmt.Fprintln(out, c)
		}
	}
	return 0
}

// firstSubcommand returns the first known top-level subcommand among the
// already-typed words, or "" if none has been entered yet.
func firstSubcommand(prior []string) string {
	known := make(map[string]bool, len(completionSubcommands))
	for _, s := range completionSubcommands {
		known[s] = true
	}
	for _, w := range prior {
		if known[w] {
			return w
		}
	}
	return ""
}

// completionLibraryNames returns configured library names for completion. It
// degrades gracefully: any failure (no config, DB absent, query error) yields
// no names rather than an error. It never creates the database -- a tab-press
// must not have side effects -- so it returns nothing if the DB file is absent.
func completionLibraryNames(ctx context.Context) []string {
	cfg, err := config.Load("")
	if err != nil || cfg.DB.Path == "" {
		return nil
	}
	if _, statErr := os.Stat(cfg.DB.Path); statErr != nil {
		return nil
	}
	// Read-only: completion must not run migrations or otherwise mutate the DB.
	sqlDB, err := db.OpenReadOnly(ctx, cfg.DB.Path)
	if err != nil {
		return nil
	}
	defer func() { _ = sqlDB.Close() }()
	libs, err := library.New(sqlDB).List(ctx)
	if err != nil {
		return nil
	}
	var names []string
	for _, l := range libs {
		if l.Name != "" {
			names = append(names, l.Name)
		}
	}
	return names
}

const bashCompletion = `# canticle bash completion. Source this file (e.g. from ~/.bashrc):
#   source <(canticle completion bash)
_canticle_complete() {
    local cur words
    cur="${COMP_WORDS[COMP_CWORD]}"
    words=("${COMP_WORDS[@]:1:COMP_CWORD}")
    local IFS=$'\n'
    COMPREPLY=( $(canticle __complete "${words[@]}") )
}
complete -F _canticle_complete canticle
`

const zshCompletion = `#compdef canticle
# canticle zsh completion. Source this file (e.g. from ~/.zshrc):
#   source <(canticle completion zsh)
_canticle_complete() {
    local -a completions
    completions=("${(@f)$(canticle __complete "${words[@]:1}")}")
    compadd -- "${completions[@]}"
}
compdef _canticle_complete canticle
`

const fishCompletion = `# canticle fish completion. Save to ~/.config/fish/completions/canticle.fish:
#   canticle completion fish > ~/.config/fish/completions/canticle.fish
function __canticle_complete
    set -l tokens (commandline -opc) (commandline -ct)
    canticle __complete $tokens[2..-1]
end
complete -c canticle -f -a '(__canticle_complete)'
`
