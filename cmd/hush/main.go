// hush soft-blocks Signal conversations. See README.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/psyclyx/hush/internal/config"
	sig "github.com/psyclyx/hush/internal/signal"
	"github.com/psyclyx/hush/internal/softblock"
)

func main() {
	path := flag.String("config", "/etc/hush/config.json", "config file")
	check := flag.Bool("check", false, "validate the config, print a sample auto-reply, and exit")
	flag.Parse()

	cfg, err := config.Load(*path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if *check {
		until := time.Now().Add(72 * time.Hour)
		b := &softblock.Block{Name: "Example", Reason: "taking a few days", Until: &until}
		reply, err := softblock.Render(cfg, b)
		if err == nil {
			var esc string
			esc, err = softblock.RenderEscape(cfg, b)
			fmt.Printf("auto-reply:\n%s\n\nescape notice:\n%s\n", reply, esc)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client, err := sig.New(cfg.SignalCli, cfg.Account)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	d, err := softblock.New(cfg, client)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	reload := make(chan *config.Config)
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for range hup {
			c, err := config.Load(*path)
			if err != nil {
				slog.Error("reload failed; keeping old config", "err", err)
				continue
			}
			reload <- c
		}
	}()

	go client.Run(ctx)
	slog.Info("hush running")
	d.Run(ctx, client.Received, reload)
}
