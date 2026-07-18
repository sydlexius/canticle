package main

import (
	"context"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/joho/godotenv"
	"github.com/sydlexius/canticle/internal/app"
	"github.com/sydlexius/canticle/internal/commands"
	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/musixmatch"
	"github.com/sydlexius/canticle/internal/queue"
)

type appRunner = commands.AppRunner

func main() {
	os.Exit(run())
}

type runOptions struct {
	args       []string
	out        io.Writer
	ctx        context.Context
	loadDotenv func() error
	newFetcher func(token string) musixmatch.Fetcher
	newWriter  func(roots ...string) lyrics.Writer
	newApp     func(fetcher musixmatch.Fetcher, writer lyrics.Writer, inputs *queue.InputsQueue, cooldown int, mode string) commands.AppRunner
}

func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return runWithOptions(runOptions{ctx: ctx})
}

func runWithOptions(opts runOptions) int {
	rawArgs := opts.args
	if rawArgs == nil {
		rawArgs = os.Args[1:]
	}
	out := opts.out
	if out == nil {
		out = os.Stdout
	}
	ctx := opts.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	loadDotenv := opts.loadDotenv
	if loadDotenv == nil {
		loadDotenv = func() error { return godotenv.Load() }
	}
	newFetcher := opts.newFetcher
	if newFetcher == nil {
		newFetcher = func(token string) musixmatch.Fetcher { return musixmatch.NewClient(token) }
	}
	newWriter := opts.newWriter
	if newWriter == nil {
		newWriter = func(roots ...string) lyrics.Writer { return lyrics.NewLRCWriter(roots...) }
	}
	newApp := opts.newApp
	if newApp == nil {
		newApp = func(fetcher musixmatch.Fetcher, writer lyrics.Writer, inputs *queue.InputsQueue, cooldown int, mode string) commands.AppRunner {
			return app.NewApp(fetcher, writer, inputs, cooldown, mode)
		}
	}

	return commands.Run(ctx, rawArgs, out, commands.Deps{
		LoadDotenv: loadDotenv,
		NewFetcher: newFetcher,
		NewWriter:  newWriter,
		NewApp:     newApp,
	})
}
