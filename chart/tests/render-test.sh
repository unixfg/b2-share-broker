#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
chart_dir="$(cd "${script_dir}/.." && pwd)"
render_dir="$(mktemp -d "${TMPDIR:-/tmp}/b2-share-broker-chart-test.XXXXXX")"
trap 'rm -rf "${render_dir}"' EXIT

fail() {
  echo "render test failed: $*" >&2
  exit 1
}

render() {
  helm template policy-test "${chart_dir}" --namespace policy-test "$@"
}

policy_count() {
  grep -c '^kind: NetworkPolicy$' "$1" || true
}

assert_policy_count() {
  local manifest="$1"
  local expected="$2"
  local actual
  actual="$(policy_count "${manifest}")"
  [[ "${actual}" == "${expected}" ]] ||
    fail "expected ${expected} NetworkPolicies in ${manifest}, found ${actual}"
}

assert_contains() {
  local manifest="$1"
  local expected="$2"
  grep -Fq -- "${expected}" "${manifest}" ||
    fail "expected '${expected}' in ${manifest}"
}

assert_absent() {
  local manifest="$1"
  local unexpected="$2"
  if grep -Fq -- "${unexpected}" "${manifest}"; then
    fail "did not expect '${unexpected}' in ${manifest}"
  fi
}

assert_occurrence_count() {
  local manifest="$1"
  local expected="$2"
  local count="$3"
  local actual
  actual="$(grep -Fc -- "${expected}" "${manifest}" || true)"
  [[ "${actual}" == "${count}" ]] ||
    fail "expected '${expected}' ${count} times in ${manifest}, found ${actual}"
}

assert_cluster_only_cnpg_peer() {
  local manifest="$1"
  local cluster="$2"
  awk -v cluster="${cluster}" '
    $0 == "        - podSelector:" {
      capture = 1
      block = $0 "\n"
      next
    }
    capture {
      block = block $0 "\n"
    }
    capture && $0 == "      ports:" {
      expected = "        - podSelector:\n" \
        "            matchLabels:\n" \
        "              cnpg.io/cluster: " cluster "\n" \
        "      ports:\n"
      if (block == expected) {
        found = 1
      }
      capture = 0
    }
    END { if (!found) exit 1 }
  ' "${manifest}" ||
    fail "expected a cluster-only CNPG peer selector for ${cluster}"
}

extract_policy() {
  local manifest="$1"
  local policy_name="$2"
  local output="$3"
  awk -v target="${policy_name}" '
    BEGIN { RS = "---\n"; ORS = "" }
    $0 ~ /\nkind: NetworkPolicy\n/ && $0 ~ ("\n  name: " target "\n") {
      print $0
      found = 1
    }
    END { if (!found) exit 1 }
  ' "${manifest}" > "${output}" ||
    fail "could not extract NetworkPolicy ${policy_name}"
}

default_render="${render_dir}/default.yaml"
enabled_render="${render_dir}/enabled.yaml"
without_processor_render="${render_dir}/without-processor.yaml"
without_cnpg_render="${render_dir}/without-cnpg.yaml"
api_only_render="${render_dir}/api-only.yaml"
missing_selector_log="${render_dir}/missing-selector.log"

render > "${default_render}"
assert_policy_count "${default_render}" 0

render -f "${script_dir}/networkpolicy-values.yaml" > "${enabled_render}"
assert_policy_count "${enabled_render}" 3
assert_absent "${enabled_render}" "    - Egress"
assert_absent "${enabled_render}" "  egress:"

api_policy="${render_dir}/api-policy.yaml"
processor_policy="${render_dir}/processor-policy.yaml"
cnpg_policy="${render_dir}/cnpg-policy.yaml"
extract_policy "${enabled_render}" b2-share-broker-api-ingress "${api_policy}"
extract_policy "${enabled_render}" b2-share-processor-ingress "${processor_policy}"
extract_policy "${enabled_render}" b2-share-broker-pg-ingress "${cnpg_policy}"

assert_contains "${api_policy}" "app.kubernetes.io/instance: policy-test"
assert_contains "${api_policy}" "app.kubernetes.io/component: api"
assert_contains "${api_policy}" "kubernetes.io/metadata.name: test-ingress"
assert_contains "${api_policy}" "app.kubernetes.io/name: test-traefik"
assert_contains "${api_policy}" "app.kubernetes.io/instance: test-traefik-release"
assert_contains "${api_policy}" "kubernetes.io/metadata.name: test-gatus"
assert_contains "${api_policy}" "app.kubernetes.io/instance: test-gatus-release"
assert_contains "${api_policy}" "port: 8080"

assert_contains "${processor_policy}" "app.kubernetes.io/instance: policy-test"
assert_contains "${processor_policy}" "app.kubernetes.io/component: processor"
assert_contains "${processor_policy}" "kubernetes.io/metadata.name: test-ingress"
assert_contains "${processor_policy}" "app.kubernetes.io/instance: test-traefik-release"
assert_contains "${processor_policy}" "port: 8080"
assert_absent "${processor_policy}" "test-gatus"

# The target remains instance-only, but the peer must admit every workload in
# this CNPG cluster, including join jobs with jobRole and cluster instance labels.
assert_cluster_only_cnpg_peer "${cnpg_policy}" b2-share-broker-pg
assert_occurrence_count "${cnpg_policy}" "cnpg.io/cluster: b2-share-broker-pg" 2
assert_occurrence_count "${cnpg_policy}" "cnpg.io/podRole: instance" 1
assert_occurrence_count "${cnpg_policy}" "app.kubernetes.io/instance: policy-test" 3
assert_absent "${cnpg_policy}" "cnpg.io/cluster: unrelated-pg"
assert_contains "${cnpg_policy}" "app.kubernetes.io/component: api"
assert_contains "${cnpg_policy}" "app.kubernetes.io/component: processor"
assert_contains "${cnpg_policy}" "kubernetes.io/metadata.name: test-cnpg-system"
assert_contains "${cnpg_policy}" "app.kubernetes.io/instance: test-cnpg-operator"
assert_contains "${cnpg_policy}" "kubernetes.io/metadata.name: test-monitoring"
assert_contains "${cnpg_policy}" "app.kubernetes.io/instance: test-prometheus-release"
assert_contains "${cnpg_policy}" "port: 5432"
assert_contains "${cnpg_policy}" "port: 8000"
assert_contains "${cnpg_policy}" "port: 9187"

render -f "${script_dir}/networkpolicy-values.yaml" \
  --set processor.enabled=false > "${without_processor_render}"
assert_policy_count "${without_processor_render}" 2
assert_absent "${without_processor_render}" "name: b2-share-processor-ingress"
extract_policy "${without_processor_render}" b2-share-broker-pg-ingress "${cnpg_policy}"
assert_absent "${cnpg_policy}" "app.kubernetes.io/component: processor"

render -f "${script_dir}/networkpolicy-values.yaml" \
  --set cnpg.enabled=false > "${without_cnpg_render}"
assert_policy_count "${without_cnpg_render}" 2
assert_absent "${without_cnpg_render}" "name: b2-share-broker-pg-ingress"

render -f "${script_dir}/networkpolicy-values.yaml" \
  --set processor.enabled=false \
  --set cnpg.enabled=false > "${api_only_render}"
assert_policy_count "${api_only_render}" 1
assert_contains "${api_only_render}" "name: b2-share-broker-api-ingress"

if render --set networkPolicy.enabled=true \
  --set networkPolicy.trustedSources.gatus.podSelector=null \
  > "${missing_selector_log}" 2>&1; then
  fail "expected an enabled policy with an empty trusted selector to fail"
fi
assert_contains "${missing_selector_log}" \
  "networkPolicy.trustedSources.gatus.podSelector is required"

echo "chart render tests passed"
