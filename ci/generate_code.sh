#!/usr/bin/env bash
set -Eeuxo pipefail

# Run all code generators we use in this repository.
# It is used to check for mismatches in GitHub Actions workflow.

# go projects
(cd components/notebook-controller; make manifests generate fmt)
(cd components/odh-notebook-controller; make manifests generate fmt)

# component metadata yaml file
(cd components/notebook-controller; bash generate-metadata-yaml.sh -o config/component_metadata.yaml)
