#!/usr/bin/env bash
# Bash is required because this self-contained integration test builds scratch Git repositories.
set -euo pipefail
script="$(cd "$(dirname "$0")" && pwd)/release-tag.sh"
dir="$(mktemp -d)"
trap 'rm -rf "$dir"' EXIT

failures=0
check() {
  local name="$1" want="$2" output rc
  shift 2
  if output="$("$@" 2>&1)"; then rc=0; else rc=$?; fi
  if [[ "$rc" -eq "$want" ]]; then
    printf 'ok   %s\n' "$name"
  else
    printf 'FAIL %s: rc=%s (want %s)\n%s\n' "$name" "$rc" "$want" "$output"
    failures=$((failures + 1))
  fi
}

new_repo() {
  local path="$1" version="$2"
  mkdir -p "$path/.github"
  git -C "$path" init -q -b main
  git -C "$path" config user.name test
  git -C "$path" config user.email test@example.invalid
  printf '%s\n' "$version" > "$path/.github/rigel-version"
  git -C "$path" add .github/rigel-version
  git -C "$path" commit -qm "set $version"
}
run_in_repo() {
  local repo="$1"
  shift
  (cd "$repo" && "$@")
}

mkdir -p "$dir/bin"
cat > "$dir/bin/gh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
case "$*" in
  *"git/refs --method POST"*)
    printf '%s\n' "$*" > "$GH_CALL_FILE"
    if [[ "${GH_RACE:-}" == same ]]; then
      echo 'HTTP 422: Reference already exists' >&2
      exit 1
    elif [[ "${GH_RACE:-}" == different ]]; then
      echo 'HTTP 422: Reference already exists' >&2
      exit 1
    fi
    echo created
    ;;
  *"git/ref/tags/v"*)
    echo "$GH_EXISTING_SHA"
    ;;
  *)
    echo "unexpected gh call: $*" >&2
    exit 2
    ;;
esac
EOF
chmod +x "$dir/bin/gh"
export PATH="$dir/bin:$PATH"

mkdir -p "$dir/mint"
new_repo "$dir/mint" 0.8.0
base="$(git -C "$dir/mint" rev-parse HEAD)"
printf '0.9.0\n' > "$dir/mint/.github/rigel-version"
git -C "$dir/mint" add .github/rigel-version
git -C "$dir/mint" commit -qm 'bump to 0.9.0'
target="$(git -C "$dir/mint" rev-parse HEAD)"
printf 'vendor change\n' > "$dir/mint/vendor.txt"
git -C "$dir/mint" add vendor.txt
git -C "$dir/mint" commit -qm 'later change'
tip="$(git -C "$dir/mint" rev-parse HEAD)"
git -C "$dir/mint" tag v0.8.0 "$base"
printf '0.9.0\n' > "$dir/mint/.github/rigel-version"
GH_CALL_FILE="$dir/gh-call" RELEASE_SHA="$tip" RELEASE_REPOSITORY=RigelBuild/terraform-provider-googleworkspace \
  check 'mints version tag at oldest commit setting version' 0 run_in_repo "$dir/mint" "$script" mint
if ! grep -F "sha=$target" "$dir/gh-call" >/dev/null; then
  echo 'FAIL mint target was not the version-bump commit'
  failures=$((failures + 1))
else
  echo 'ok   mint target is version-bump commit, not later tip'
fi

rm -f "$dir/gh-call"
git -C "$dir/mint" tag v0.9.0 "$base"
GH_CALL_FILE="$dir/gh-call" RELEASE_SHA="$tip" RELEASE_REPOSITORY=RigelBuild/terraform-provider-googleworkspace \
  check 'existing tag anywhere is an idempotent no-op' 0 run_in_repo "$dir/mint" "$script" mint
if [[ -e "$dir/gh-call" ]]; then
  echo 'FAIL existing tag called GitHub API'
  failures=$((failures + 1))
else
  echo 'ok   existing tag skipped GitHub API'
fi

git -C "$dir/mint" tag -d v0.9.0 >/dev/null
GH_CALL_FILE="$dir/gh-call" GH_RACE=same GH_EXISTING_SHA="$target" RELEASE_SHA="$tip" RELEASE_REPOSITORY=RigelBuild/terraform-provider-googleworkspace \
  check '422 race at expected target succeeds' 0 run_in_repo "$dir/mint" "$script" mint
GH_CALL_FILE="$dir/gh-call" GH_RACE=different GH_EXISTING_SHA="$base" RELEASE_SHA="$tip" RELEASE_REPOSITORY=RigelBuild/terraform-provider-googleworkspace \
  check '422 race at a different target fails' 1 run_in_repo "$dir/mint" "$script" mint

mkdir -p "$dir/check"
new_repo "$dir/check" 0.9.0
check_base="$(git -C "$dir/check" rev-parse HEAD)"
git -C "$dir/check" tag v0.9.0 "$check_base"
git -C "$dir/check" rm -q .github/rigel-version
git -C "$dir/check" commit -qm 'remove seeded version source'
base_without_source="$(git -C "$dir/check" rev-parse HEAD)"
mkdir -p "$dir/check/.github"
printf '0.9.0\n' > "$dir/check/.github/rigel-version"
git -C "$dir/check" add .github/rigel-version
git -C "$dir/check" commit -qm 'seed existing tagged version'
check_head="$(git -C "$dir/check" rev-parse HEAD)"
check 'introducing source equal to latest tag passes' 0 run_in_repo "$dir/check" env BASE_SHA="$base_without_source" HEAD_SHA="$check_head" "$script" check
printf '0.8.5\n' > "$dir/check/.github/rigel-version"
git -C "$dir/check" add .github/rigel-version
git -C "$dir/check" commit -qm 'seed version below latest tag'
check_head="$(git -C "$dir/check" rev-parse HEAD)"
check 'introducing source below latest tag fails' 1 run_in_repo "$dir/check" env BASE_SHA="$base_without_source" HEAD_SHA="$check_head" "$script" check
printf '1.0.0\n' > "$dir/check/.github/rigel-version"
git -C "$dir/check" add .github/rigel-version
git -C "$dir/check" commit -qm 'bump version after source exists'
check_head="$(git -C "$dir/check" rev-parse HEAD)"
check 'later version above latest tag passes' 0 run_in_repo "$dir/check" env BASE_SHA="$base_without_source" HEAD_SHA="$check_head" "$script" check
printf '0.9.0\n' > "$dir/check/.github/rigel-version"
git -C "$dir/check" add .github/rigel-version
git -C "$dir/check" commit -qm 'reuse latest version after source exists'
next_head="$(git -C "$dir/check" rev-parse HEAD)"
check 'later change cannot reuse latest version' 1 run_in_repo "$dir/check" env BASE_SHA="$check_head" HEAD_SHA="$next_head" "$script" check

mkdir -p "$dir/resolve"
new_repo "$dir/resolve" 1.0.0
resolve_sha="$(git -C "$dir/resolve" rev-parse HEAD)"
git -C "$dir/resolve" update-ref refs/remotes/origin/main "$resolve_sha"
check 'release tag on main matching version resolves' 0 run_in_repo "$dir/resolve" "$script" resolve v1.0.0 "$resolve_sha"
check 'tag that does not match version is rejected' 1 run_in_repo "$dir/resolve" "$script" resolve v0.9.0 "$resolve_sha"


if [[ "$failures" -ne 0 ]]; then
  exit 1
fi
