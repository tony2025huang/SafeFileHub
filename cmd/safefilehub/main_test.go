package main

import (
	"testing"

	"github.com/example/safefilehub/internal/config"
)

func TestParseMaintenanceOptionsDefaultsToEnabledBoundedRecovery(t *testing.T) {
	opts, err := parseMaintenanceOptions(nil, config.Default())
	if err != nil {
		t.Fatal(err)
	}
	if !opts.recoverOnStart || opts.recoverOnly || opts.dryRun || opts.limit != 64 {
		t.Fatalf("defaults = %#v", opts)
	}
}

func TestParseMaintenanceOptionsSupportsRecoverOnlyDryRun(t *testing.T) {
	opts, err := parseMaintenanceOptions([]string{"-recover-only", "-recover-dry-run", "-recover-limit=7"}, config.Default())
	if err != nil {
		t.Fatal(err)
	}
	if !opts.recoverOnStart || !opts.recoverOnly || !opts.dryRun || opts.limit != 7 {
		t.Fatalf("options = %#v", opts)
	}
}

func TestParseMaintenanceOptionsRejectsUnboundedOrConflictingRequests(t *testing.T) {
	for _, args := range [][]string{
		{"-recover-limit=0"},
		{"-recover-limit=65"},
		{"-recover-only", "-recover-on-start=false"},
	} {
		if _, err := parseMaintenanceOptions(args, config.Default()); err == nil {
			t.Fatalf("parseMaintenanceOptions(%q) succeeded", args)
		}
	}
}
