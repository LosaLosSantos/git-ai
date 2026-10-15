package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/feder-cr/open_scion/tools/graft-agents/superfan/chat"
)

// defaultChatListen is loopback so that the endpoints are not exposed by
// default; put a reverse proxy with TLS in front of them.
const defaultChatListen = "127.0.0.1:8090"

// chatDrainTimeout bounds how long shutdown waits for answers in progress.
const chatDrainTimeout = 30 * time.Second

// runChat implements "graft-superfan chat serve" and "graft-superfan chat ask".
// See doc/graft/SUPERFAN-CHAT.md.
func runChat(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "graft-superfan: chat needs a command: serve or ask")
		return exitUsage
	}
	switch args[0] {
	case "serve":
		return runChatServe(ctx, args[1:], stderr)
	case "ask":
		return runChatAsk(ctx, args[1:], stdout, stderr)
	}
	fmt.Fprintf(stderr, "graft-superfan: unknown chat command %q (serve, ask)\n", args[0])
	return exitUsage
}

// runChatServe serves the Slack endpoints until interrupted.
func runChatServe(ctx context.Context, args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("graft-superfan chat serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", defaultChatListen, "address to serve the Slack endpoints on")
	configPath := fs.String("config", "", "chat configuration (JSON)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "graft-superfan: unexpected arguments: %s\n", strings.Join(fs.Args(), " "))
		return exitUsage
	}
	if *configPath == "" {
		fmt.Fprintln(stderr, "graft-superfan: chat serve needs --config")
		return exitUsage
	}
	cfg, err := chat.LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "graft-superfan: %v\n", err)
		return exitError
	}
	engine, err := chat.NewEngine(chat.Options{Config: cfg})
	if err != nil {
		fmt.Fprintf(stderr, "graft-superfan: %v\n", err)
		return exitError
	}
	secret, token, err := chat.LoadSlackSecrets(cfg, os.Getenv)
	if err != nil {
		fmt.Fprintf(stderr, "graft-superfan: %v\n", err)
		return exitError
	}
	logger := slog.New(slog.NewTextHandler(stderr, nil))
	if token == "" {
		logger.Warn("no Slack bot token: app_mention events will not be answered")
	}
	srv, err := chat.NewServer(chat.ServerOptions{
		Engine:        engine,
		SigningSecret: secret,
		BotToken:      token,
		APIBase:       cfg.SlackAPIBase(),
		Logger:        logger,
	})
	if err != nil {
		fmt.Fprintf(stderr, "graft-superfan: %v\n", err)
		return exitError
	}
	logger.Info("graft-superfan chat serving Slack", "listen", *listen)
	if err := chat.Serve(ctx, *listen, srv.Handler()); err != nil {
		fmt.Fprintf(stderr, "graft-superfan: %v\n", err)
		return exitError
	}
	// Answers already acknowledged to Slack are finished before the process
	// exits, within a bound so that a stuck provider cannot block shutdown.
	if !srv.Drain(chatDrainTimeout) {
		logger.Warn("shutdown: answers still running after drain timeout", "timeout", chatDrainTimeout.String())
		return exitError
	}
	return exitOK
}

// runChatAsk answers one question on the command line with the same engine.
// Flags must come before the question.
func runChatAsk(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("graft-superfan chat ask", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "chat configuration (JSON); without it no source is read")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	question := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if question == "" {
		fmt.Fprintln(stderr, `graft-superfan: chat ask needs a question, for example: graft-superfan chat ask "what changed?"`)
		return exitUsage
	}
	cfg, err := loadChatConfig(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "graft-superfan: %v\n", err)
		return exitError
	}
	engine, err := chat.NewEngine(chat.Options{Config: cfg})
	if err != nil {
		fmt.Fprintf(stderr, "graft-superfan: %v\n", err)
		return exitError
	}
	answer, err := engine.Answer(ctx, question)
	if err != nil {
		fmt.Fprintf(stderr, "graft-superfan: %v\n", err)
		return exitError
	}
	fmt.Fprintln(stdout, answer)
	return exitOK
}

// loadChatConfig reads the chat configuration, or the empty configuration when
// path is empty.
func loadChatConfig(path string) (*chat.Config, error) {
	if path == "" {
		return chat.ParseConfig([]byte("{}"))
	}
	return chat.LoadConfig(path)
}
