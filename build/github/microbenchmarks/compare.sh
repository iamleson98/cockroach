#!/bin/bash

# Copyright 2025 The Cockroach Authors.
#
# Use of this software is governed by the CockroachDB Software License
# included in the /LICENSE file.

set -euxo pipefail

working_dir=$(mktemp -d)
output_dir=$(mktemp -d)
storage_bucket="$BUCKET"
shas=("$BASE_SHA" "$HEAD_SHA")

# Disable parallel uploads, as it requires specific permissions
gcloud config set storage/parallel_composite_upload_enabled False

# Retrieve outputs from the runs
for sha in "${shas[@]}"; do
  mkdir -p "${working_dir}/${sha}/artifacts"
  gcloud storage cp -r "gs://${storage_bucket}/artifacts/${sha}/${BUILD_ID}/*" "${working_dir}/${sha}/artifacts/"
done

# Retrieve token (with logging disabled)
set +x
GITHUB_TOKEN=$(gcloud secrets versions access 1 --secret=cockroach-microbench-ga-token)
export GITHUB_TOKEN
set -x

# Compare the microbenchmarks
./build/github/microbenchmarks/util.sh compare \
  --working-dir="$working_dir" \
  --summary="$output_dir/summary.json" \
  --github-summary="$output_dir/summary.md" \
  --build-id="$BUILD_ID" \
  --old="$BASE_SHA" \
  --new="$HEAD_SHA" \
  --post

# Clear the token
unset GITHUB_TOKEN

cat "$output_dir/summary.md" > "$GITHUB_STEP_SUMMARY"

# Copy comparison results to GCS
gcloud storage cp -r "$output_dir/*" "gs://${storage_bucket}/results/${HEAD_SHA}/${BUILD_ID}"
