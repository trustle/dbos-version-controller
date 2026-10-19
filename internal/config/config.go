/*
Copyright 2026 Trustle, Inc.

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

// Package config reads the controller settings from the DVC_ environment variables.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"
)

// Defaults for every optional setting.
const (
	DefaultInterval     = 30 * time.Second
	DefaultGrace        = 600 * time.Second
	DefaultManagedBy    = "dbos-version-controller"
	DefaultVersionLabel = "dbos-app-version"
	DefaultLogLevel     = "info"
)

// Config holds the controller settings. Every field reads one DVC_ environment variable.
type Config struct {
	// Namespace holds the WorkerVersionSets and the worker objects. Required.
	Namespace string
	// DatabaseURL is the DBOS system database, read to count outstanding workflows. Required.
	DatabaseURL string
	// Interval is the time between reconciles of one set.
	Interval time.Duration
	// Grace is the default time a drained version stays at zero rows and zero pods before deletion.
	Grace time.Duration
	// DryRun logs every create, patch and delete as "Would ..." and performs no such write.
	DryRun bool
	// ManagedBy is the value of the app.kubernetes.io/managed-by label identifying owned objects.
	ManagedBy string
	// VersionLabel carries the DBOS application version on each managed object.
	VersionLabel string
	// LogLevel is the level of the root logger.
	LogLevel string
}

// Default returns the settings before any environment variable is read.
func Default() Config {
	return Config{
		Interval:     DefaultInterval,
		Grace:        DefaultGrace,
		ManagedBy:    DefaultManagedBy,
		VersionLabel: DefaultVersionLabel,
		LogLevel:     DefaultLogLevel,
	}
}

// FromEnv overlays the DVC_ environment variables onto base, so the environment wins over a flag.
func FromEnv(base Config) (Config, error) {
	cfg := base

	setString(&cfg.Namespace, "DVC_NAMESPACE")
	setString(&cfg.DatabaseURL, "DVC_DATABASE_URL")
	setString(&cfg.ManagedBy, "DVC_MANAGED_BY")
	setString(&cfg.VersionLabel, "DVC_VERSION_LABEL")
	setString(&cfg.LogLevel, "DVC_LOG_LEVEL")

	if err := setSeconds(&cfg.Interval, "DVC_INTERVAL_SECONDS"); err != nil {
		return cfg, err
	}
	if err := setSeconds(&cfg.Grace, "DVC_GRACE_SECONDS"); err != nil {
		return cfg, err
	}
	if raw, ok := os.LookupEnv("DVC_DRY_RUN"); ok {
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return cfg, fmt.Errorf("DVC_DRY_RUN %q is not a boolean: %w", raw, err)
		}
		cfg.DryRun = value
	}

	return cfg, cfg.Validate()
}

// Validate returns why the settings are unusable, or nil when they are usable.
func (c Config) Validate() error {
	if c.Namespace == "" {
		return fmt.Errorf("DVC_NAMESPACE is required")
	}
	if c.DatabaseURL == "" {
		return fmt.Errorf("DVC_DATABASE_URL is required")
	}
	if c.Interval <= 0 {
		return fmt.Errorf("DVC_INTERVAL_SECONDS must be greater than zero")
	}
	if c.Grace < 0 {
		return fmt.Errorf("DVC_GRACE_SECONDS must not be negative")
	}
	// Both go into the label selector that finds the pods holding a version, so an unusable value
	// would leave every count unknown and stop every delete.
	if problems := validation.IsQualifiedName(c.VersionLabel); len(problems) > 0 {
		return fmt.Errorf("DVC_VERSION_LABEL %q is not a valid label key: %s", c.VersionLabel, strings.Join(problems, "; "))
	}
	if problems := validation.IsValidLabelValue(c.ManagedBy); len(problems) > 0 || c.ManagedBy == "" {
		return fmt.Errorf("DVC_MANAGED_BY %q is not a valid label value: %s", c.ManagedBy, strings.Join(problems, "; "))
	}
	return nil
}

func setString(target *string, name string) {
	if raw, ok := os.LookupEnv(name); ok {
		*target = raw
	}
}

func setSeconds(target *time.Duration, name string) error {
	raw, ok := os.LookupEnv(name)
	if !ok {
		return nil
	}
	seconds, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return fmt.Errorf("%s %q is not a number: %w", name, raw, err)
	}
	*target = time.Duration(seconds * float64(time.Second))
	return nil
}
