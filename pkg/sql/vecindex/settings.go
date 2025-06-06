// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package vecindex

import (
	"context"

	"github.com/cockroachdb/cockroach/pkg/clusterversion"
	"github.com/cockroachdb/cockroach/pkg/settings"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgcode"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgerror"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/eval"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/cockroach/pkg/sql/vecindex/cspann"
	"github.com/cockroachdb/cockroach/pkg/sql/vecindex/vecpb"
	"github.com/cockroachdb/cockroach/pkg/util/errorutil/unimplemented"
)

// DeterministicFixupsSetting, if true, makes all background index operations
// deterministic by:
//  1. Using a fixed pseudo-random seed for random operations.
//  2. Using a single background worker that processes fixups.
//  3. Synchronously running the worker only at prescribed times, e.g. before
//     running SearchForInsert or SearchForDelete.
var DeterministicFixupsSetting = settings.RegisterBoolSetting(
	settings.ApplicationLevel,
	"sql.vecindex.deterministic_fixups.enabled",
	"set to true to make all background index operations deterministic, for testing",
	false,
	settings.WithVisibility(settings.Reserved),
)

// StalledOpTimeoutSetting specifies how long a split/merge operation can remain
// in its current state before another fixup worker may attempt to assist. If
// this is set too high, then a fixup can get stuck for too long. If it is set
// too low, then multiple workers can assist at the same time, resulting in
// duplicate work.
// TODO(andyk): Consider making this more dynamic, e.g. with
// livenesspb.NodeVitalityInterface.
var StalledOpTimeoutSetting = settings.RegisterDurationSetting(
	settings.ApplicationLevel,
	"sql.vecindex.stalled_op.timeout",
	"amount of time before other vector index workers will assist with a stalled background fixup",
	cspann.DefaultStalledOpTimeout,
	settings.WithPublic,
)

// VectorIndexEnabled is used to enable and disable vector indexes.
var VectorIndexEnabled = settings.RegisterBoolSetting(
	settings.ApplicationLevel,
	"feature.vector_index.enabled",
	"set to true to enable vector indexes, false to disable; default is false",
	false,
	settings.WithPublic)

// CheckEnabled returns an error if the feature.vector_index.enabled cluster
// setting is false.
func CheckEnabled(sv *settings.Values) error {
	if !VectorIndexEnabled.Get(sv) {
		return pgerror.Newf(pgcode.FeatureNotSupported,
			"vector indexes are not enabled; enable with the feature.vector_index.enabled cluster setting")
	}
	return nil
}

// MakeVecConfig constructs a new VecConfig that's compatible with the given
// type.
func MakeVecConfig(
	ctx context.Context, evalCtx *eval.Context, typ *types.T, opClass tree.Name,
) (vecpb.Config, error) {
	// Dimensions are derived from the vector type. By default, use Givens
	// rotations to mix input vectors.
	config := vecpb.Config{Dims: typ.Width(), RotAlgorithm: vecpb.RotGivens}
	if DeterministicFixupsSetting.Get(&evalCtx.Settings.SV) {
		// Set well-known seed and deterministic fixups.
		config.Seed = 42
		config.IsDeterministic = true
	} else {
		// Use random seed.
		config.Seed = evalCtx.GetRNG().Int63()
	}

	// Set the distance metric used by the index.
	switch opClass {
	case "vector_l2_ops", "":
		// vector_l2_ops is the default operator class. This allows users to omit
		// the operator class in index definitions.
	case "vector_ip_ops":
		config.DistanceMetric = vecpb.InnerProductDistance
	case "vector_cosine_ops":
		config.DistanceMetric = vecpb.CosineDistance
	case "vector_l1_ops", "bit_hamming_ops", "bit_jaccard_ops":
		return vecpb.Config{},
			unimplemented.NewWithIssuef(144016, "operator class %v is not supported", opClass)
	default:
		return vecpb.Config{}, pgerror.Newf(
			pgcode.UndefinedObject, "operator class %q does not exist", opClass)
	}

	if config.DistanceMetric != vecpb.L2SquaredDistance {
		if !evalCtx.Settings.Version.ActiveVersion(ctx).AtLeast(clusterversion.V25_3.Version()) {
			return vecpb.Config{}, pgerror.Newf(pgcode.FeatureNotSupported,
				"cannot use %s until finalizing on 25.3", opClass)
		}
	}

	return config, nil
}
