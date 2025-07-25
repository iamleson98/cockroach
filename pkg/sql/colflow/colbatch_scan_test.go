// Copyright 2016 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

// Note that this file is not in pkg/sql/colexec because it instantiates a
// server, and if it were moved into sql/colexec, that would create a cycle
// with pkg/server.

package colflow_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/kv"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/desctestutils"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/fetchpb"
	"github.com/cockroachdb/cockroach/pkg/sql/colexec/colbuilder"
	"github.com/cockroachdb/cockroach/pkg/sql/colexec/colexecargs"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfra"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfrapb"
	"github.com/cockroachdb/cockroach/pkg/sql/rowenc"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/eval"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/cockroach/pkg/testutils/serverutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/sqlutils"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
)

// TestColBatchScanMeta makes sure that the ColBatchScan propagates the leaf
// txn final state metadata which is necessary to notify the kvCoordSender
// about the spans that have been read.
func TestColBatchScanMeta(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()
	srv, sqlDB, kvDB := serverutils.StartServer(t, base.TestServerArgs{})
	defer srv.Stopper().Stop(ctx)
	s := srv.ApplicationLayer()

	sqlutils.CreateTable(t, sqlDB, "t",
		"num INT PRIMARY KEY",
		3, /* numRows */
		sqlutils.ToRowFn(sqlutils.RowIdxFn))

	td := desctestutils.TestingGetPublicTableDescriptor(kvDB, s.Codec(), "test", "t")

	st := s.ClusterSettings()
	evalCtx := eval.MakeTestingEvalContext(st)
	defer evalCtx.Stop(ctx)
	var monitorRegistry colexecargs.MonitorRegistry
	defer monitorRegistry.Close(ctx)
	var closerRegistry colexecargs.CloserRegistry
	defer closerRegistry.Close(ctx)

	rootTxn := kv.NewTxn(ctx, s.DB(), s.DistSQLPlanningNodeID())
	leafInputState, err := rootTxn.GetLeafTxnInputState(ctx, nil /* readsTree */)
	if err != nil {
		t.Fatal(err)
	}
	leafTxn := kv.NewLeafTxn(ctx, s.DB(), s.DistSQLPlanningNodeID(), leafInputState, nil /* header */)
	flowCtx := execinfra.FlowCtx{
		EvalCtx: &evalCtx,
		Mon:     evalCtx.TestingMon,
		Cfg: &execinfra.ServerConfig{
			Settings: st,
		},
		Txn:    leafTxn,
		Local:  true,
		NodeID: evalCtx.NodeID,
	}
	var fetchSpec fetchpb.IndexFetchSpec
	if err := rowenc.InitIndexFetchSpec(
		&fetchSpec, s.Codec(), td, td.GetPrimaryIndex(),
		[]descpb.ColumnID{td.PublicColumns()[0].GetID()},
	); err != nil {
		t.Fatal(err)
	}
	spec := execinfrapb.ProcessorSpec{
		Core: execinfrapb.ProcessorCoreUnion{
			TableReader: &execinfrapb.TableReaderSpec{
				FetchSpec: fetchSpec,
				Spans: []roachpb.Span{
					td.PrimaryIndexSpan(s.Codec()),
				},
			}},
		ResultTypes: types.OneIntCol,
	}

	args := &colexecargs.NewColOperatorArgs{
		Spec:            &spec,
		MonitorRegistry: &monitorRegistry,
		CloserRegistry:  &closerRegistry,
	}
	res, err := colbuilder.NewColOperator(ctx, &flowCtx, args)
	if err != nil {
		t.Fatal(err)
	}
	tr := res.Root
	tr.Init(ctx)
	meta := res.MetadataSources[0].DrainMeta()
	var txnFinalStateSeen bool
	for _, m := range meta {
		if m.LeafTxnFinalState != nil {
			txnFinalStateSeen = true
			break
		}
	}
	if !txnFinalStateSeen {
		t.Fatal("missing txn final state")
	}
}

func BenchmarkColBatchScan(b *testing.B) {
	defer leaktest.AfterTest(b)()
	logScope := log.Scope(b)
	defer logScope.Close(b)
	ctx := context.Background()

	srv, sqlDB, kvDB := serverutils.StartServer(b, base.TestServerArgs{})
	defer srv.Stopper().Stop(ctx)
	s := srv.ApplicationLayer()

	const numCols = 2
	for _, numRows := range []int{1 << 4, 1 << 8, 1 << 12, 1 << 16} {
		tableName := fmt.Sprintf("t%d", numRows)
		sqlutils.CreateTable(
			b, sqlDB, tableName,
			"k INT PRIMARY KEY, v INT",
			numRows,
			sqlutils.ToRowFn(sqlutils.RowIdxFn, sqlutils.RowModuloFn(42)),
		)
		tableDesc := desctestutils.TestingGetPublicTableDescriptor(kvDB, s.Codec(), "test", tableName)
		b.Run(fmt.Sprintf("rows=%d", numRows), func(b *testing.B) {
			span := tableDesc.PrimaryIndexSpan(s.Codec())
			var fetchSpec fetchpb.IndexFetchSpec
			if err := rowenc.InitIndexFetchSpec(
				&fetchSpec, s.Codec(), tableDesc, tableDesc.GetPrimaryIndex(),
				[]descpb.ColumnID{tableDesc.PublicColumns()[0].GetID(), tableDesc.PublicColumns()[1].GetID()},
			); err != nil {
				b.Fatal(err)
			}
			spec := execinfrapb.ProcessorSpec{
				Core: execinfrapb.ProcessorCoreUnion{
					TableReader: &execinfrapb.TableReaderSpec{
						FetchSpec: fetchSpec,
						// Spans will be set below.
					}},
				ResultTypes: types.TwoIntCols,
			}

			evalCtx := eval.MakeTestingEvalContext(s.ClusterSettings())
			defer evalCtx.Stop(ctx)
			var monitorRegistry colexecargs.MonitorRegistry
			var closerRegistry colexecargs.CloserRegistry
			afterEachRun := func() {
				closerRegistry.BenchmarkReset(ctx)
				monitorRegistry.BenchmarkReset(ctx)
			}

			flowCtx := execinfra.FlowCtx{
				EvalCtx: &evalCtx,
				Mon:     evalCtx.TestingMon,
				Cfg:     &execinfra.ServerConfig{Settings: s.ClusterSettings()},
				Txn:     kv.NewTxn(ctx, s.DB(), s.DistSQLPlanningNodeID()),
				NodeID:  evalCtx.NodeID,
			}

			b.SetBytes(int64(numRows * numCols * 8))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				// We have to set the spans on each iteration since the
				// txnKVFetcher reuses the passed-in slice and destructively
				// modifies it.
				spec.Core.TableReader.Spans = []roachpb.Span{span}
				args := &colexecargs.NewColOperatorArgs{
					Spec:            &spec,
					MonitorRegistry: &monitorRegistry,
					CloserRegistry:  &closerRegistry,
				}
				res, err := colbuilder.NewColOperator(ctx, &flowCtx, args)
				if err != nil {
					b.Fatal(err)
				}
				tr := res.Root
				b.StartTimer()
				tr.Init(ctx)
				for {
					bat := tr.Next()
					if bat.Length() == 0 {
						break
					}
				}
				b.StopTimer()
				afterEachRun()
			}
		})
	}
}
