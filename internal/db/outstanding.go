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

// Package db counts the unfinished workflows of one DBOS application version.
package db

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"time"

	// Register the pgx driver under the name "pgx".
	_ "github.com/jackc/pgx/v5/stdlib"
)

// queryTimeout bounds both the connection attempt and the statement.
const queryTimeout = 10 * time.Second

// NewOutstandingCounter returns a function counting the unfinished workflows of one application
// version.
//
// Each call opens one short connection and closes it again. The controller never starts DBOS, so
// a crashing worker version cannot take the controller down with it.
func NewOutstandingCounter(databaseURL, query string) func(ctx context.Context, version string) (int64, error) {
	dsn := withTimeouts(databaseURL)

	return func(ctx context.Context, version string) (int64, error) {
		ctx, cancel := context.WithTimeout(ctx, queryTimeout)
		defer cancel()

		database, err := sql.Open("pgx", dsn)
		if err != nil {
			return 0, fmt.Errorf("open the DBOS system database: %w", err)
		}
		defer func() { _ = database.Close() }()

		var count int64
		if err := database.QueryRowContext(ctx, query, version).Scan(&count); err != nil {
			return 0, fmt.Errorf("count outstanding workflows of %s: %w", version, err)
		}
		return count, nil
	}
}

// withTimeouts adds a connect timeout and a statement timeout to a database URL, leaving any the
// caller already set alone.
func withTimeouts(databaseURL string) string {
	parsed, err := url.Parse(databaseURL)
	if err != nil {
		return databaseURL
	}
	values := parsed.Query()
	if values.Get("connect_timeout") == "" {
		values.Set("connect_timeout", fmt.Sprintf("%d", int(queryTimeout.Seconds())))
	}
	if values.Get("options") == "" {
		values.Set("options", fmt.Sprintf("-c statement_timeout=%d", queryTimeout.Milliseconds()))
	}
	parsed.RawQuery = values.Encode()
	return parsed.String()
}
