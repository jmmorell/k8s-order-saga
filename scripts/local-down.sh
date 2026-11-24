#!/usr/bin/env bash
# Deletes the kind cluster and everything in it.
set -euo pipefail

echo "==> deleting kind cluster order-saga"
kind delete cluster --name order-saga
