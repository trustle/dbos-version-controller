/*
Copyright 2026 Trustle, Inc..

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package config

import (
	"testing"
	"time"
)

const databaseURL = "postgresql://user:pass@localhost:5432/dbos"

func TestSettingsParseFromTheEnvironment(t *testing.T) {
	t.Setenv("DVC_NAMESPACE", "dbos-workers-stg")
	t.Setenv("DVC_DATABASE_URL", databaseURL)
	t.Setenv("DVC_INTERVAL_SECONDS", "5")
	t.Setenv("DVC_GRACE_SECONDS", "120")
	t.Setenv("DVC_DRY_RUN", "true")

	settings, err := FromEnv(Default())
	if err != nil {
		t.Fatalf("read the settings: %v", err)
	}

	if settings.Namespace != "dbos-workers-stg" {
		t.Fatalf("the namespace is %q", settings.Namespace)
	}
	if settings.DatabaseURL != databaseURL {
		t.Fatalf("the database URL is %q", settings.DatabaseURL)
	}
	if settings.Interval != 5*time.Second {
		t.Fatalf("the interval is %s", settings.Interval)
	}
	if settings.Grace != 120*time.Second {
		t.Fatalf("the grace period is %s", settings.Grace)
	}
	if !settings.DryRun {
		t.Fatal("dry run is off")
	}
	if settings.ManagedBy != DefaultManagedBy {
		t.Fatalf("the managed-by value is %q", settings.ManagedBy)
	}
	if settings.VersionLabel != DefaultVersionLabel {
		t.Fatalf("the version label is %q", settings.VersionLabel)
	}
}

func TestTheEnvironmentWinsOverAFlag(t *testing.T) {
	t.Setenv("DVC_NAMESPACE", "from-the-environment")
	t.Setenv("DVC_DATABASE_URL", databaseURL)

	base := Default()
	base.Namespace = "from-a-flag"

	settings, err := FromEnv(base)
	if err != nil {
		t.Fatalf("read the settings: %v", err)
	}
	if settings.Namespace != "from-the-environment" {
		t.Fatalf("the namespace is %q", settings.Namespace)
	}
}

func TestTheRequiredSettingsAreRequired(t *testing.T) {
	t.Setenv("DVC_DATABASE_URL", databaseURL)
	if _, err := FromEnv(Default()); err == nil {
		t.Fatal("a missing DVC_NAMESPACE was accepted")
	}

	t.Setenv("DVC_NAMESPACE", "dbos-workers")
	t.Setenv("DVC_DATABASE_URL", "")
	if _, err := FromEnv(Default()); err == nil {
		t.Fatal("a missing DVC_DATABASE_URL was accepted")
	}
}
