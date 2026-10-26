package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/JiaWeiXie/lensdock/internal/update"
)

func expiryDeployment(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	dep := update.ScaffoldM0(filepath.Join(root, "deployment.json"), filepath.Join(root, "state"), filepath.Join(root, "compose.yaml"), filepath.Join(root, "bin"))
	if err := dep.Save(); err != nil {
		t.Fatal(err)
	}
	return dep.File
}

func TestExpiryCLIRejectsInvalidExplicitAgesWithoutSideEffects(t *testing.T) {
	for _, command := range []struct{ name, flag string }{
		{"cleanup", "--older-than-days"},
		{"expiry", "--expire-after-days"},
	} {
		for _, age := range []string{"-1", "-2", "36501"} {
			t.Run(command.name+"/"+age, func(t *testing.T) {
				dep := expiryDeployment(t)
				before, err := os.ReadFile(dep)
				if err != nil {
					t.Fatal(err)
				}
				c, out, _, runner := newUpdateCLI(t, "")
				err = c.dispatch(context.Background(), []string{"update", command.name, "--deployment", dep, command.flag, age})
				var exit *exitError
				if !errors.As(err, &exit) || exit.code != exitUsage {
					t.Fatalf("invalid explicit age must be a usage error: %v", err)
				}
				after, err := os.ReadFile(dep)
				if err != nil {
					t.Fatal(err)
				}
				if string(before) != string(after) || out.Len() != 0 || runner.count() != 0 {
					t.Fatal("invalid age changed policy or attempted cleanup inventory")
				}
			})
		}
	}
}

func TestExpiryCLIDistinguishesOmittedAgeFromExplicitZero(t *testing.T) {
	dep := expiryDeployment(t)
	c, out, _, runner := newUpdateCLI(t, "")
	if err := c.updateExpiry(context.Background(), []string{"--deployment", dep}); err != nil {
		t.Fatal(err)
	}
	var policy struct {
		ExpireAfterDays int `json:"expireAfterDays"`
	}
	if err := json.Unmarshal(out.Bytes(), &policy); err != nil {
		t.Fatal(err)
	}
	if policy.ExpireAfterDays != update.DefaultExpireAfterDays {
		t.Fatalf("omission changed policy: %d", policy.ExpireAfterDays)
	}
	out.Reset()
	if err := c.updateCleanup(context.Background(), []string{"--deployment", dep, "--older-than-days", "0", "--yes"}); err != nil {
		t.Fatal(err)
	}
	var report update.ExpiryReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if !report.Disabled || runner.count() != 0 {
		t.Fatal("explicit zero must disable this cleanup without Docker inventory")
	}
	loaded, err := update.LoadDeployment(dep)
	if err != nil || loaded.ExpireAfter() != update.DefaultExpireAfterDays {
		t.Fatalf("one-off disabled cleanup changed stored policy: %v", err)
	}
	out.Reset()
	if err := c.updateExpiry(context.Background(), []string{"--deployment", dep, "--expire-after-days", "0"}); err != nil {
		t.Fatal(err)
	}
	loaded, err = update.LoadDeployment(dep)
	if err != nil || loaded.ExpireAfter() != 0 {
		t.Fatalf("explicit zero did not disable stored expiry: %v", err)
	}
}
