package commands

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/sydlexius/mxlrcgo-svc/internal/config"
	"github.com/sydlexius/mxlrcgo-svc/internal/db"
	"github.com/sydlexius/mxlrcgo-svc/internal/library"
)

// CompletionCmd outputs a shell completion script.
type CompletionCmd struct {
	Shell string `arg:"positional,required" help:"shell to generate completion for: bash, zsh, or fish"`
}

// completionSubcommands are the top-level subcommands offered at the first word.
var completionSubcommands = []string{
	"fetch", "serve", "scan", "library", "keys", "config", "queue", "completion",
}

// completionCandidates maps a subcommand to the flags and/or nested subcommands
// offered after it. It is a curated set for completion convenience, not an
// exhaustive mirror of every flag.
var completionCandidates = map[string][]string{
	"fetch":      {"--outdir", "--cooldown", "--depth", "--update", "--upgrade", "--bfs", "--token", "--config", "--album", "--probe"},
	"serve":      {"--listen", "--outdir", "--token", "--config", "--depth", "--update", "--upgrade", "--bfs", "--embedded-lyrics", "--scan-interval", "--work-interval"},
	"scan":       {"results", "clear", "--config", "--depth", "--update", "--upgrade", "--bfs", "--embedded-lyrics", "--only"},
	"library":    {"add", "list", "remove", "update"},
	"keys":       {"create", "list", "revoke"},
	"config":     {"get", "set", "list"},
	"queue":      {"list", "failed", "deferred", "retry", "clear", "recheck"},
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

const bashCompletion = `# mxlrcgo-svc bash completion. Source this file (e.g. from ~/.bashrc):
#   source <(mxlrcgo-svc completion bash)
_mxlrcgo_svc_complete() {
    local cur words
    cur="${COMP_WORDS[COMP_CWORD]}"
    words=("${COMP_WORDS[@]:1:COMP_CWORD}")
    local IFS=$'\n'
    COMPREPLY=( $(mxlrcgo-svc __complete "${words[@]}") )
}
complete -F _mxlrcgo_svc_complete mxlrcgo-svc
`

const zshCompletion = `#compdef mxlrcgo-svc
# mxlrcgo-svc zsh completion. Source this file (e.g. from ~/.zshrc):
#   source <(mxlrcgo-svc completion zsh)
_mxlrcgo_svc_complete() {
    local -a completions
    completions=("${(@f)$(mxlrcgo-svc __complete "${words[@]:1}")}")
    compadd -- "${completions[@]}"
}
compdef _mxlrcgo_svc_complete mxlrcgo-svc
`

const fishCompletion = `# mxlrcgo-svc fish completion. Save to ~/.config/fish/completions/mxlrcgo-svc.fish:
#   mxlrcgo-svc completion fish > ~/.config/fish/completions/mxlrcgo-svc.fish
function __mxlrcgo_svc_complete
    set -l tokens (commandline -opc) (commandline -ct)
    mxlrcgo-svc __complete $tokens[2..-1]
end
complete -c mxlrcgo-svc -f -a '(__mxlrcgo_svc_complete)'
`
