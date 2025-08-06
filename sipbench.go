package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/novartc/sipbench/arp"
	"github.com/novartc/sipbench/routing"
	"github.com/spf13/cobra"
)

var (
	Version   string
	BuildTime string
	GitCommit string
	GoVersion = runtime.Version()
)

func main() {
	cmd := &cobra.Command{
		Use:     "sipbench",
		Short:   "sipbench",
		Long:    "sipbench",
		Version: fmt.Sprintf("\nVersion: %s\nBuilt: %s\nCommit: %s\nGoVersion: %s\n", Version, BuildTime, GitCommit, GoVersion),
		Run: func(cmd *cobra.Command, args []string) {
			_ = cmd.Help()
		},
	}

	cmd.InitDefaultVersionFlag()
	cmd.AddCommand(newUasCommand(), newUacCommand())
	if err := cmd.Execute(); err != nil {
		panic(err)
	}
}

func startRefreshTimer(d time.Duration) {
	t := time.NewTicker(d)
	for range t.C {
		if err := arp.Reload(); err != nil {
			slog.Error("failed to reload arp table", "error", err)
		}
		if err := routing.Reload(); err != nil {
			slog.Error("failed to reload routing table", "error", err)
		}
	}
}

func waitShutdown(ctx context.Context, cs ...io.Closer) {
	sig := make(chan os.Signal)
	signal.Notify(sig, syscall.SIGTERM, os.Interrupt)

	var reason string
	select {
	case <-ctx.Done():
		reason = ctx.Err().Error()
	case s := <-sig:
		reason = s.String()
	}

	slog.Warn("sipbench starts exiting", slog.String("reason", reason))
	for _, c := range cs {
		_ = c.Close()
	}
}
