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

package db

import (
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestTheStatementTimeoutSurvivesTheDriverParser(t *testing.T) {
	config, err := pgconn.ParseConfig(withTimeouts("postgresql://user@localhost:5432/dbos"))
	if err != nil {
		t.Fatalf("parse the DSN: %v", err)
	}

	if got := config.RuntimeParams["options"]; got != "-c statement_timeout=10000" {
		t.Fatalf("the options runtime parameter is %q", got)
	}
	if config.ConnectTimeout.Seconds() != 10 {
		t.Fatalf("the connect timeout is %v", config.ConnectTimeout)
	}
}

func TestExistingTimeoutsAreLeftAlone(t *testing.T) {
	dsn := withTimeouts("postgresql://user@localhost:5432/dbos?connect_timeout=3&options=-c%20statement_timeout=1")

	config, err := pgconn.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse the DSN: %v", err)
	}
	if got := config.RuntimeParams["options"]; got != "-c statement_timeout=1" {
		t.Fatalf("the options runtime parameter is %q", got)
	}
	if config.ConnectTimeout.Seconds() != 3 {
		t.Fatalf("the connect timeout is %v", config.ConnectTimeout)
	}
}

func TestAKeywordValueConnectionStringSurvivesTheDriverParser(t *testing.T) {
	dsn := withTimeouts("host=pg port=5432 user=dbos dbname=dbos")

	config, err := pgconn.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse the DSN %q: %v", dsn, err)
	}
	if config.Host != "pg" {
		t.Fatalf("the host is %q", config.Host)
	}
	if config.User != "dbos" {
		t.Fatalf("the user is %q", config.User)
	}
	if got := config.RuntimeParams["options"]; got != "-c statement_timeout=10000" {
		t.Fatalf("the options runtime parameter is %q", got)
	}
	if config.ConnectTimeout.Seconds() != 10 {
		t.Fatalf("the connect timeout is %v", config.ConnectTimeout)
	}
}

func TestExistingKeywordValueTimeoutsAreLeftAlone(t *testing.T) {
	dsn := withTimeouts("host=pg user=dbos connect_timeout=3 options='-c statement_timeout=1'")

	config, err := pgconn.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse the DSN %q: %v", dsn, err)
	}
	if got := config.RuntimeParams["options"]; got != "-c statement_timeout=1" {
		t.Fatalf("the options runtime parameter is %q", got)
	}
	if config.ConnectTimeout.Seconds() != 3 {
		t.Fatalf("the connect timeout is %v", config.ConnectTimeout)
	}
}
