// Copyright 2022 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package sql

import (
	"context"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/testutils/serverutils"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/stretchr/testify/require"
)

// Make sure that running a wire-protocol-level PREPARE of a SQL-level PREPARE
// and SQL-level EXECUTE doesn't cause any problems.
func TestPreparePrepareExecute(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	srv, db, _ := serverutils.StartServer(t, base.TestServerArgs{Insecure: true})
	defer srv.Stopper().Stop(context.Background())
	defer db.Close()

	// Test that preparing an invalid EXECUTE fails at prepare-time.
	_, err := db.Prepare("EXECUTE x(3)")
	require.Contains(t, err.Error(), "no such prepared statement")

	// Test that we can prepare and execute a PREPARE.
	s, err := db.Prepare("PREPARE x AS SELECT $1::int")
	require.NoError(t, err)

	_, err = s.Exec()
	require.NoError(t, err)

	// Make sure we can't send arguments to the PREPARE even though it has a
	// placeholder inside (that placeholder is for the "inner" PREPARE).
	_, err = s.Exec(3)
	require.Contains(t, err.Error(), "expected 0 arguments, got 1")

	// Test that we can prepare and execute the corresponding EXECUTE.
	s, err = db.Prepare("EXECUTE x(3)")
	require.NoError(t, err)

	var output int
	err = s.QueryRow().Scan(&output)
	require.NoError(t, err)
	require.Equal(t, 3, output)

	// Make sure we can't send arguments to the prepared EXECUTE.
	_, err = s.Exec(3)
	require.Contains(t, err.Error(), "expected 0 arguments, got 1")
}

// Makes sure that schema changes can be prepared as the first operation in
// on a connection.
func TestPrepareSchemaChange(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	srv := serverutils.StartServerOnly(t, base.TestServerArgs{Insecure: true})
	defer srv.Stopper().Stop(context.Background())

	newConn := srv.SQLConn(t)
	s, err := newConn.Prepare("PREPARE x AS CREATE SCHEMA sc1;")
	require.NoError(t, err)

	_, err = s.Exec()
	require.NoError(t, err)
}
