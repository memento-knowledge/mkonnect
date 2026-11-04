#!/usr/bin/env bash

set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
reconcile="$root/scripts/reconcile-image-tags.sh"
tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT

release_id=20260919.0
release_commit=0123456789abcdef0123456789abcdef01234567
registry=registry.example.invalid/alias
repository=memento-example
version_tag=$release_id
commit_tag="sha-$release_commit"

# The stubs model a registry as one file per tag, "<digest> <version label> <revision label>",
# and the local Docker image store the same way. Every call is appended to a log so the tests
# can assert what was (and was not) run.
mkdir "$tmpdir/bin"
cat >"$tmpdir/bin/aws" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
ORIGINAL_ARGS="$*"
repository=
tag=
while (($# > 0)); do
  case $1 in
    --repository-name) repository=$2; shift 2 ;;
    --image-ids) tag=${2#imageTag=}; shift 2 ;;
    *) shift ;;
  esac
done
printf 'aws %s\n' "$ORIGINAL_ARGS" >>"$FAKE_LOG"
if [[ -n ${FAKE_AWS_DENY-} ]]; then
  printf '%s\n' 'An error occurred (AccessDeniedException) when calling the DescribeImages operation' >&2
  exit 255
fi
record="$FAKE_REGISTRY/$repository/$tag"
if [[ -f $record ]]; then
  read -r digest _ <"$record"
  printf '{"imageDetails":[{"imageDigest":"%s"}]}\n' "$digest"
else
  printf '%s\n' 'An error occurred (ImageNotFoundException) when calling the DescribeImages operation' >&2
  exit 255
fi
EOF
cat >"$tmpdir/bin/docker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"$FAKE_LOG"

# registry.example.invalid/alias/<repository>:<tag> -> <repository>/<tag>
registry_path() {
  local ref=${1#*/}
  ref=${ref#*/}
  printf '%s/%s\n' "${ref%%:*}" "${ref#*:}"
}
local_path() { printf '%s/%s\n' "$FAKE_LOCAL" "$(printf '%s' "$1" | tr '/:' '__')"; }

digest_of() {
  if command -v sha256sum >/dev/null; then
    printf '%s' "$1" | sha256sum | awk '{ print "sha256:" $1 }'
  else
    printf '%s' "$1" | shasum -a 256 | awk '{ print "sha256:" $1 }'
  fi
}

case $1 in
  build)
    shift
    version=
    revision=
    refs=()
    dockerfile=
    while (($# > 1)); do
      case $1 in
        --file) dockerfile=$2; shift 2 ;;
        --build-arg)
          case $2 in
            VERSION=*) version=${2#VERSION=} ;;
            REVISION=*) revision=${2#REVISION=} ;;
          esac
          shift 2
          ;;
        --tag) refs+=("$2"); shift 2 ;;
        *) shift ;;
      esac
    done
    if [[ -n ${FAKE_BUILD_FAIL-} ]]; then
      printf 'simulated build failure\n' >&2
      exit 1
    fi
    digest=$(digest_of "$dockerfile|$version|$revision|${FAKE_BUILD_SALT-}")
    for ref in "${refs[@]}"; do
      printf '%s %s %s\n' "$digest" "$version" "$revision" >"$(local_path "$ref")"
    done
    ;;
  push)
    if [[ -n ${FAKE_PUSH_FAIL_TAG-} && $2 == *":$FAKE_PUSH_FAIL_TAG" ]]; then
      printf 'simulated push failure\n' >&2
      exit 1
    fi
    path="$FAKE_REGISTRY/$(registry_path "$2")"
    mkdir -p "$(dirname "$path")"
    cp "$(local_path "$2")" "$path"
    ;;
  pull)
    path="$FAKE_REGISTRY/$(registry_path "$2")"
    [[ -f $path ]] || { printf 'not found: %s\n' "$2" >&2; exit 1; }
    cp "$path" "$(local_path "$2")"
    ;;
  inspect)
    read -r _ version revision <"$(local_path "$2")"
    case $4 in
      *org.opencontainers.image.version*) printf '%s\n' "$version" ;;
      *org.opencontainers.image.revision*) printf '%s\n' "$revision" ;;
      *) exit 2 ;;
    esac
    ;;
  buildx)
    # buildx imagetools create --prefer-index=false --tag <new ref> <repo>@<digest>
    new_ref=$6
    source=$7
    source_repo=${source%@*}
    source_digest=${source#*@}
    repo_dir="$FAKE_REGISTRY/${source_repo##*/}"
    for candidate in "$repo_dir"/*; do
      read -r digest _ <"$candidate"
      if [[ $digest == "$source_digest" ]]; then
        cp "$candidate" "$FAKE_REGISTRY/$(registry_path "$new_ref")"
        exit 0
      fi
    done
    printf 'no such source digest\n' >&2
    exit 1
    ;;
  *)
    printf 'unexpected docker command: %s\n' "$*" >&2
    exit 2
    ;;
esac
EOF
chmod +x "$tmpdir/bin/aws" "$tmpdir/bin/docker"

test_count=0
# Extra reconcile.sh arguments for the current case (for example --version-tag-last).
extra_args=()
image="$registry/$repository"

# The exact calls the API connector release has always made, in order. aws_call and the push
# expectations below pin the original behaviour: version tag first, then the commit tag.
aws_call() {
  printf 'aws ecr-public describe-images --region us-east-1 --repository-name %s --image-ids imageTag=%s --output json\n' "$repository" "$1"
}
build_call() {
  printf 'build --file %s/Dockerfile.example --build-arg VERSION=%s --build-arg REVISION=%s --tag %s:%s --tag %s:%s %s\n' \
    "$case_dir" "$release_id" "$release_commit" "$image" "$version_tag" "$image" "$commit_tag" "$case_dir"
}

new_case() {
  case_dir="$tmpdir/$1"
  mkdir -p "$case_dir/registry/$repository" "$case_dir/local"
  : >"$case_dir/log"
  : >"$case_dir/Dockerfile.example"
}

# seed <tag> <digest> <version label> <revision label>
seed() {
  printf '%s %s %s\n' "$2" "$3" "$4" >"$case_dir/registry/$repository/$1"
}

run_reconcile() {
  FAKE_REGISTRY="$case_dir/registry" FAKE_LOCAL="$case_dir/local" FAKE_LOG="$case_dir/log" \
    PATH="$tmpdir/bin:$PATH" \
    bash "$reconcile" \
    --registry "$registry" \
    --repository "$repository" \
    --dockerfile "$case_dir/Dockerfile.example" \
    --release-id "$release_id" \
    --release-commit "$release_commit" \
    --output "$case_dir/out.env" \
    --context "$case_dir" \
    ${extra_args[@]+"${extra_args[@]}"}
}

expect_success() {
  run_reconcile >"$case_dir/stdout" 2>"$case_dir/stderr" || {
    printf 'expected %s to succeed:\n' "$case_dir" >&2
    cat "$case_dir/stderr" >&2
    exit 1
  }
  test_count=$((test_count + 1))
}

expect_failure() {
  if run_reconcile >"$case_dir/stdout" 2>"$case_dir/stderr"; then
    printf 'expected %s to fail\n' "$case_dir" >&2
    exit 1
  fi
  test ! -e "$case_dir/out.env"
  test_count=$((test_count + 1))
}

registry_has() { test -f "$case_dir/registry/$repository/$1"; }
registry_lacks() { test ! -e "$case_dir/registry/$repository/$1"; }
log_has() { grep -Eq -- "$1" "$case_dir/log"; }
log_lacks() { ! grep -Eq -- "$1" "$case_dir/log"; }
registry_digest() { awk '{ print $1 }' "$case_dir/registry/$repository/$1"; }

# 1. Neither tag exists: build once, push both, report the shared digest.
new_case fresh
expect_success
registry_has "$version_tag"
registry_has "$commit_tag"
test "$(registry_digest "$version_tag")" = "$(registry_digest "$commit_tag")"
grep -qx "image=$registry/$repository" "$case_dir/out.env"
grep -qx "image_digest=$(registry_digest "$version_tag")" "$case_dir/out.env"
test "$(grep -c '^build ' "$case_dir/log")" -eq 1
log_has "^build --file $case_dir/Dockerfile.example --build-arg VERSION=$release_id --build-arg REVISION=$release_commit "
# Default order, identical to the API connector's original behaviour: the release tag is pushed
# first, then the commit tag, between the lookups before and after. The whole call sequence is
# compared, not just the pushes.
{
  aws_call "$version_tag"
  aws_call "$commit_tag"
  build_call
  printf 'push %s:%s\n' "$image" "$version_tag"
  printf 'push %s:%s\n' "$image" "$commit_tag"
  aws_call "$version_tag"
  aws_call "$commit_tag"
} >"$case_dir/expected-calls"
cmp "$case_dir/expected-calls" "$case_dir/log"
# The registry only ever contains the two release tags.
test "$(find "$case_dir/registry/$repository" -type f | wc -l | tr -d ' ')" -eq 2

# 2. Re-running after success changes nothing and rebuilds nothing.
cp "$case_dir/out.env" "$case_dir/first.env"
: >"$case_dir/log"
rm "$case_dir/out.env"
expect_success
cmp "$case_dir/first.env" "$case_dir/out.env"
log_lacks '^(build|push|buildx) '

# 3. Only the release tag exists with matching labels: add the commit tag, no rebuild.
new_case version-only
seed "$version_tag" sha256:aaaa "$release_id" "$release_commit"
expect_success
registry_has "$commit_tag"
test "$(registry_digest "$commit_tag")" = sha256:aaaa
grep -qx 'image_digest=sha256:aaaa' "$case_dir/out.env"
log_lacks '^(build|push) '
log_has "^buildx imagetools create --prefer-index=false --tag $registry/$repository:$commit_tag $registry/$repository@sha256:aaaa"

# 4. Only the commit tag exists with matching labels: add the release tag, no rebuild.
new_case commit-only
seed "$commit_tag" sha256:bbbb "$release_id" "$release_commit"
expect_success
registry_has "$version_tag"
test "$(registry_digest "$version_tag")" = sha256:bbbb
log_lacks '^(build|push) '

# 5. Both tags exist and agree: nothing to do.
new_case complete
seed "$version_tag" sha256:cccc "$release_id" "$release_commit"
seed "$commit_tag" sha256:cccc "$release_id" "$release_commit"
expect_success
grep -qx 'image_digest=sha256:cccc' "$case_dir/out.env"
log_lacks '^(build|push|buildx) '

# 6. Both tags exist but point at different images: conflict, nothing changes.
new_case digest-conflict
seed "$version_tag" sha256:dddd "$release_id" "$release_commit"
seed "$commit_tag" sha256:eeee "$release_id" "$release_commit"
expect_failure
log_lacks '^(build|push|buildx) '
test "$(registry_digest "$version_tag")" = sha256:dddd
test "$(registry_digest "$commit_tag")" = sha256:eeee

# 7. An existing tag carries a different revision or version label: conflict, no new tag.
new_case wrong-revision
seed "$version_tag" sha256:ffff "$release_id" 1111111111111111111111111111111111111111
expect_failure
registry_lacks "$commit_tag"
log_lacks '^(build|push|buildx) '

new_case wrong-version
seed "$commit_tag" sha256:ffff 20260101.0 "$release_commit"
expect_failure
registry_lacks "$version_tag"
log_lacks '^(build|push|buildx) '

new_case wrong-labels-both-present
seed "$version_tag" sha256:abab "$release_id" "$release_commit"
seed "$commit_tag" sha256:abab "$release_id" 2222222222222222222222222222222222222222
expect_failure
log_lacks '^(build|push|buildx) '

# 8. A failed build publishes nothing at all.
new_case build-failure
FAKE_BUILD_FAIL=1 expect_failure
registry_lacks "$version_tag"
registry_lacks "$commit_tag"
log_lacks '^push '
unset FAKE_BUILD_FAIL

# 9. Default order (API connector): the release tag is pushed first, so a failed push of the
# commit tag leaves a complete release tag behind, and a rerun completes the release from it.
new_case default-order-commit-push-failure
FAKE_PUSH_FAIL_TAG=$commit_tag expect_failure
registry_has "$version_tag"
registry_lacks "$commit_tag"
unset FAKE_PUSH_FAIL_TAG
: >"$case_dir/log"
expect_success
registry_has "$commit_tag"
test "$(registry_digest "$version_tag")" = "$(registry_digest "$commit_tag")"
log_lacks '^build '
log_lacks '^push '

new_case default-order-version-push-failure
FAKE_PUSH_FAIL_TAG=$version_tag expect_failure
registry_lacks "$version_tag"
registry_lacks "$commit_tag"
unset FAKE_PUSH_FAIL_TAG

# 10. --version-tag-last (git-sync image): the commit tag is pushed first and the release tag
# last, so no push failure can leave a release tag behind.
extra_args=(--version-tag-last)
new_case last-order-fresh
expect_success
{
  aws_call "$version_tag"
  aws_call "$commit_tag"
  build_call
  printf 'push %s:%s\n' "$image" "$commit_tag"
  printf 'push %s:%s\n' "$image" "$version_tag"
  aws_call "$version_tag"
  aws_call "$commit_tag"
} >"$case_dir/expected-calls"
cmp "$case_dir/expected-calls" "$case_dir/log"
grep -qx "image_digest=$(registry_digest "$version_tag")" "$case_dir/out.env"

new_case last-order-commit-push-failure
FAKE_PUSH_FAIL_TAG=$commit_tag expect_failure
registry_lacks "$version_tag"
registry_lacks "$commit_tag"
log_lacks "^push .*:$version_tag\$"
unset FAKE_PUSH_FAIL_TAG

new_case last-order-version-push-failure
FAKE_PUSH_FAIL_TAG=$version_tag expect_failure
registry_lacks "$version_tag"
registry_has "$commit_tag"
unset FAKE_PUSH_FAIL_TAG
: >"$case_dir/log"
expect_success
registry_has "$version_tag"
test "$(registry_digest "$version_tag")" = "$(registry_digest "$commit_tag")"
log_lacks '^build '
log_lacks '^push '

new_case last-order-build-failure
FAKE_BUILD_FAIL=1 expect_failure
registry_lacks "$version_tag"
registry_lacks "$commit_tag"
log_lacks '^push '
unset FAKE_BUILD_FAIL

# 11. Recovery from a partial or complete release makes the same calls in either order, and
# never builds or pushes.
for mode in default last; do
  if [ "$mode" = last ]; then extra_args=(--version-tag-last); else extra_args=(); fi

  new_case "recover-version-only-$mode"
  seed "$version_tag" sha256:aaaa "$release_id" "$release_commit"
  expect_success
  {
    aws_call "$version_tag"
    aws_call "$commit_tag"
    printf 'pull %s:%s\n' "$image" "$version_tag"
    printf 'inspect %s:%s --format {{ index .Config.Labels "org.opencontainers.image.version" }}\n' "$image" "$version_tag"
    printf 'inspect %s:%s --format {{ index .Config.Labels "org.opencontainers.image.revision" }}\n' "$image" "$version_tag"
    printf 'buildx imagetools create --prefer-index=false --tag %s:%s %s@sha256:aaaa\n' "$image" "$commit_tag" "$image"
    aws_call "$version_tag"
    aws_call "$commit_tag"
  } >"$case_dir/expected-calls"
  cmp "$case_dir/expected-calls" "$case_dir/log"

  new_case "recover-commit-only-$mode"
  seed "$commit_tag" sha256:bbbb "$release_id" "$release_commit"
  expect_success
  {
    aws_call "$version_tag"
    aws_call "$commit_tag"
    printf 'pull %s:%s\n' "$image" "$commit_tag"
    printf 'inspect %s:%s --format {{ index .Config.Labels "org.opencontainers.image.version" }}\n' "$image" "$commit_tag"
    printf 'inspect %s:%s --format {{ index .Config.Labels "org.opencontainers.image.revision" }}\n' "$image" "$commit_tag"
    printf 'buildx imagetools create --prefer-index=false --tag %s:%s %s@sha256:bbbb\n' "$image" "$version_tag" "$image"
    aws_call "$version_tag"
    aws_call "$commit_tag"
  } >"$case_dir/expected-calls"
  cmp "$case_dir/expected-calls" "$case_dir/log"

  new_case "recover-conflict-$mode"
  seed "$version_tag" sha256:dddd "$release_id" "$release_commit"
  seed "$commit_tag" sha256:eeee "$release_id" "$release_commit"
  expect_failure
  log_lacks '^(build|push|buildx) '
done
extra_args=()

# 12. A registry error other than "not found" stops the run before anything is built or pushed.
new_case registry-denied
FAKE_AWS_DENY=1 expect_failure
log_lacks '^(build|push|buildx|pull) '
unset FAKE_AWS_DENY

# 13. Usage errors and a missing Dockerfile are rejected.
if bash "$reconcile" --repository "$repository" 2>/dev/null; then
  printf 'incomplete arguments unexpectedly accepted\n' >&2
  exit 1
fi
if bash "$reconcile" --registry "$registry" --repository "$repository" --dockerfile "$tmpdir/missing" \
  --release-id "$release_id" --release-commit "$release_commit" --output "$tmpdir/unused.env" 2>/dev/null; then
  printf 'missing Dockerfile unexpectedly accepted\n' >&2
  exit 1
fi

printf 'image tag reconcile tests passed (%s cases)\n' "$test_count"
