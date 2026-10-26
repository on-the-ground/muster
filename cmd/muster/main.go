package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/on-the-ground/muster/internal/contract"
	"github.com/on-the-ground/muster/internal/proxy"
	"github.com/on-the-ground/muster/internal/server"
	"github.com/on-the-ground/muster/internal/worker"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "muster:", err)
		os.Exit(1)
	}
}

func run() error {
	manifest := flag.String("manifest", "", "operator-owned manifest JSON (required)")
	evidence := flag.String("evidence", "", "private JSONL evidence file (required)")
	check := flag.Bool("check", false, "validate manifest and backend capabilities, then exit")
	flag.Parse()
	if *manifest == "" || flag.NArg() != 0 || (!*check && *evidence == "") {
		return errors.New("use -manifest FILE and -evidence FILE, or -manifest FILE -check")
	}
	c, err := contract.Load(*manifest)
	if err != nil {
		return fmt.Errorf("load manifest: %w", err)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	backends := map[string]worker.Backend{}
	for _, t := range c.Tools {
		id := t.Definition.Binding.Backend
		if backends[id] != nil {
			continue
		}
		b, err := worker.New(c.Backends[id])
		if err != nil {
			return fmt.Errorf("backend %s: %w", id, err)
		}
		if err := b.Check(ctx); err != nil {
			return fmt.Errorf("backend %s: %w", id, err)
		}
		backends[id] = b
	}
	if *check {
		fmt.Fprintln(os.Stderr, "manifest and backend capabilities valid; authentication untested")
		return nil
	}
	// Evidence must be an operator-chosen private regular file, never stdout.
	f, err := os.OpenFile(*evidence, os.O_WRONLY|os.O_CREATE|os.O_APPEND|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return errors.New("cannot open evidence file")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return errors.New("evidence must be a private regular file (mode 0600)")
	}
	p, err := proxy.New(c, backends, proxy.NewJournal(f))
	if err != nil {
		return err
	}
	defer p.Close()
	s := server.New(c, p)
	return s.Run(ctx, &mcp.IOTransport{Reader: server.NewGuardedReader(os.Stdin), Writer: stdoutWriter{}, MaxLineLength: 4 << 20})
}

type stdoutWriter struct{}

func (stdoutWriter) Write(p []byte) (int, error) { return os.Stdout.Write(p) }
func (stdoutWriter) Close() error                { return nil }
