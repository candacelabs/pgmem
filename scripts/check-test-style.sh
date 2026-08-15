#!/usr/bin/env bash
# Keep behavior tests on the repository's Ginkgo/Gomega convention.
set -euo pipefail

module_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
violations=()

while IFS= read -r -d '' test_file; do
  test_count="$(grep -Ec '^func Test[[:alnum:]_]*\(t \*testing[.]T\)' "${test_file}" || true)"
  bootstrap_count="$(grep -Ec '^[[:space:]]*(ginkgo[.])?RunSpecs\(t,' "${test_file}" || true)"
  if ((test_count != bootstrap_count)); then
    violations+=(
      "${test_file#"${module_root}/"} (${test_count} Test functions, ${bootstrap_count} RunSpecs bootstraps)"
    )
  fi
done < <(find "${module_root}" -type f -name '*_test.go' -print0)

if ((${#violations[@]} > 0)); then
  printf 'Standard-library behavior tests are not allowed; use Ginkgo/Gomega:\n' >&2
  printf '  %s\n' "${violations[@]}" >&2
  exit 1
fi
