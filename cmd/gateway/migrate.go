package main

import (
	"context"
	"errors"
	"flag"
	"path/filepath"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/feedback"
	"github.com/JiaWeiXie/lensdock/internal/storage"
)

func migrate(args []string) error {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	root := fs.String("data-dir", "/data", "Gateway data directory containing the cloned db volume")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected migration argument")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db, err := storage.OpenDB(filepath.Join(*root, "db", "lensdock.db"))
	if err != nil {
		return err
	}
	defer db.Close()
	if err := storage.Migrate(ctx, db); err != nil {
		return err
	}
	if err := feedback.Migrate(ctx, db); err != nil {
		return err
	}
	return db.Checkpoint(ctx)
}
