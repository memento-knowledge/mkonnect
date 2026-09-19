#!/usr/bin/env bash

set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
lookup="$root/scripts/ecr-public-image-digest.sh"
tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT

mkdir "$tmpdir/bin"
cat >"$tmpdir/bin/aws" <<'EOF'
#!/usr/bin/env bash
case ${FAKE_AWS_RESULT:?} in
  found)
    printf '%s\n' '{"imageDetails":[{"imageDigest":"sha256:expected"}]}'
    ;;
  missing)
    printf '%s\n' 'An error occurred (ImageNotFoundException) when calling the DescribeImages operation' >&2
    exit 255
    ;;
  denied)
    printf '%s\n' 'An error occurred (AccessDeniedException) when calling the DescribeImages operation' >&2
    exit 255
    ;;
  *)
    printf 'unknown fake result: %s\n' "$FAKE_AWS_RESULT" >&2
    exit 2
    ;;
esac
EOF
chmod +x "$tmpdir/bin/aws"

run_lookup() {
  PATH="$tmpdir/bin:$PATH" FAKE_AWS_RESULT=$1 bash "$lookup" --repository mkonnect --tag 1.2.3
}

output=$(run_lookup found)
test "$output" = sha256:expected

if run_lookup missing; then
  printf 'missing image lookup unexpectedly succeeded\n' >&2
  exit 1
else
  test $? -eq 3
fi

if run_lookup denied; then
  printf 'access-denied lookup unexpectedly succeeded\n' >&2
  exit 1
else
  test $? -eq 1
fi

printf 'ECR Public image digest tests passed\n'
