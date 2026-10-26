package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"

	"github.com/JiaWeiXie/lensdock/internal/hostmanager"
	"github.com/JiaWeiXie/lensdock/internal/update"
)

// splitScopes turns a comma separated --scope value into a validated list.
func splitScopes(value string) ([]string, error) {
	var raw []string
	for _, s := range strings.Split(value, ",") {
		if s = strings.TrimSpace(s); s != "" {
			raw = append(raw, s)
		}
	}
	return update.ParseExpiryScopes(raw)
}

func (c *cli) updateCleanup(ctx context.Context, args []string) error {
	fs := c.newFlagSet("update cleanup")
	scope := fs.String("scope", "", "comma separated scopes: "+strings.Join(update.ExpiryScopes, ",")+" (default all)")
	days := fs.Int("older-than-days", 0, "expire items at least this many days old instead of expireAfterDays (0 disables)")
	dry := fs.Bool("dry-run", false, "list what would expire without removing anything")
	yes := fs.Bool("yes", false, "remove without a prompt")
	t, _, err := c.target(fs, args, 0, 0)
	if err != nil {
		return err
	}
	scopes, err := splitScopes(*scope)
	if err != nil {
		return usageErrorf("update cleanup: %v", err)
	}
	var after *int
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "older-than-days" {
			after = days
		}
	})
	if *days < 0 || *days > update.MaxExpireAfterDays {
		return usageErrorf("update cleanup: --older-than-days must be between 0 and %d", update.MaxExpireAfterDays)
	}

	preview, perr := t.eng.ExpireUnused(ctx, update.ExpireOptions{Scopes: scopes, AfterDays: after, DryRun: true})
	if err := c.writeJSONLine(preview); err != nil {
		return err
	}
	switch {
	case *dry:
		return perr
	case preview.Disabled:
		fmt.Fprintln(c.stderr, "age expiry is disabled (0 days): nothing to do")
		return nil
	case preview.WouldExpire == 0:
		return perr
	case perr != nil:
		return fmt.Errorf("refusing to remove anything while the inventory is uncertain: %w", perr)
	}

	if !*yes {
		ok, err := c.confirm(fmt.Sprintf("Remove %d unused old item(s) listed above? This cannot be undone.", preview.WouldExpire), false)
		if err != nil {
			return err
		}
		if !ok {
			return update.ErrNotConfirmed
		}
	}
	if t.owner != nil {
		runErr := c.managedUpdate(ctx, t.owner, hostmanager.UpdateAction{Kind: hostmanager.UpdateExpire, Confirmed: true, Scopes: scopes, AfterDays: after})
		if last, ok, err := t.eng.LastExpiry(); err == nil && ok {
			if err := c.writeJSONLine(last); err != nil {
				return err
			}
		}
		return runErr
	}
	report, err := t.eng.ExpireUnused(ctx, update.ExpireOptions{Scopes: scopes, AfterDays: after})
	if werr := c.writeJSONLine(report); werr != nil {
		return errors.Join(err, werr)
	}
	return err
}

func (c *cli) updateExpiry(ctx context.Context, args []string) error {
	fs := c.newFlagSet("update expiry")
	set := fs.Int("expire-after-days", 0, "store expireAfterDays in deployment.json (0 disables age expiry)")
	t, _, err := c.target(fs, args, 0, 0)
	if err != nil {
		return err
	}
	explicit := false
	fs.Visit(func(f *flag.Flag) { explicit = explicit || f.Name == "expire-after-days" })
	dep := t.eng.Dep
	if *set < 0 || *set > update.MaxExpireAfterDays {
		return usageErrorf("update expiry: --expire-after-days must be between 0 and %d", update.MaxExpireAfterDays)
	}
	if explicit {
		// The write is serialized with updates and other settings writers: a
		// managed deployment is written by its manager's worker under the
		// project and helper locks, a standalone one under the deployment lock.
		if t.owner != nil {
			err = c.managedUpdate(ctx, t.owner, hostmanager.UpdateAction{Kind: hostmanager.UpdatePolicy, Confirmed: true, AfterDays: set})
		} else {
			err = t.eng.SetExpireAfterDays(*set)
		}
		if err != nil {
			return err
		}
		if dep, err = update.LoadDeployment(dep.File); err != nil {
			return err
		}
	}
	out := map[string]any{
		"expireAfterDays":   dep.ExpireAfter(),
		"ageExpiryEnabled":  dep.ExpireAfter() > 0,
		"retainCheckpoints": dep.RetainCheckpoints,
		"autoIntervalHours": int(update.AutoExpireInterval.Hours()),
	}
	if last, ok, err := t.eng.LastExpiry(); err != nil {
		out["lastRunError"] = err.Error()
	} else if ok {
		out["lastRun"] = last
	}
	return c.writeJSONLine(out)
}

// autoExpire runs the automatic expiry after a successful lifecycle command of
// a standalone deployment. A managed deployment is handled by its manager's
// periodic housekeeping instead. Problems are reported, never fatal.
func (c *cli) autoExpire(ctx context.Context, t *updateTarget) {
	if t.owner != nil {
		return
	}
	report, ran, err := t.eng.AutoExpire(ctx)
	switch {
	case err != nil:
		fmt.Fprintf(c.stderr, "warning: automatic expiry of unused old system data: %v\n", err)
	case ran && report.Expired > 0:
		fmt.Fprintf(c.stderr, "expired %d unused old system data item(s)\n", report.Expired)
	}
}
