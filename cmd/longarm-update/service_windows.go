//go:build windows

package main

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"

	"github.com/lubobill1990/longarm/internal/logfile"
	"github.com/lubobill1990/longarm/internal/update"
	"github.com/lubobill1990/longarm/internal/updatesvc"
	"github.com/lubobill1990/longarm/internal/version"
)

func runService() error {
	if !updatesvc.RunningAsSystem() {
		return errors.New("run is the LongarmUpdate service's (LocalSystem): use status / install / auto")
	}
	handled, err := updatesvc.RunAsService(serve)
	if handled {
		return err
	}
	return serve(context.Background()) // started by hand as SYSTEM (psexec -s): in the foreground
}

func serve(ctx context.Context) error {
	cfg, dir, err := updatesvc.WindowsConfig(version.Version, update.PublicKey, nil)
	if err != nil {
		return err
	}
	lw, err := logfile.OpenWriter(filepath.Join(dir, "longarm-update.log"), 1<<20)
	if err != nil {
		return err
	}
	defer lw.Close()
	log := slog.New(slog.NewTextHandler(lw, nil))
	cfg.Log = log
	log.Info("longarm-update starting", "version", version.Version, "release_key", update.Enabled())
	ln, err := updatesvc.ListenPipe()
	if err != nil {
		log.Error("pipe", "err", err)
		return err
	}
	s := updatesvc.New(cfg)
	s.Start(ctx)
	err = updatesvc.Serve(ctx, ln, s, updatesvc.IdentifyPipeClient)
	log.Info("longarm-update stopping")
	return err
}
