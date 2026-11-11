#!/usr/bin/env bash
# 把 4ipic 分支上的本地 patch 重放到新的 upstream 基线上，并跑 service/handler 测试。
#
# 用法（在 4ipic 分支的干净 checkout/worktree 中执行）：
#   deploy/4ipic/rebase.sh                 # 基线 = upstream/main
#   deploy/4ipic/rebase.sh v0.2.22         # 基线 = 指定 tag/commit
#   SKIP_TESTS=1 deploy/4ipic/rebase.sh    # 只 rebase 不跑测试
#   PUSH=1 deploy/4ipic/rebase.sh          # 测试通过后 push --force-with-lease 到 origin/4ipic
#   deploy/4ipic/rebase.sh --test-only     # 不 rebase，只跑测试（解决冲突后使用）
#
# 环境变量：
#   UPSTREAM_REMOTE  upstream remote 名（默认 upstream，不存在时自动添加 Wei-Shaw/sub2api）
#   BRANCH           维护的分支名（默认 4ipic）
#   GO_BIN           go 可执行文件（默认 PATH 中的 go）
set -euo pipefail

UPSTREAM_REMOTE="${UPSTREAM_REMOTE:-upstream}"
UPSTREAM_URL="${UPSTREAM_URL:-https://github.com/Wei-Shaw/sub2api.git}"
BRANCH="${BRANCH:-4ipic}"
GO_BIN="${GO_BIN:-go}"

ROOT="$(git rev-parse --show-toplevel)"
cd "$ROOT"

run_tests() {
  echo "==> go vet + go test (service/handler)"
  (cd backend && "$GO_BIN" vet ./internal/service/ ./internal/handler/ &&
    "$GO_BIN" test ./internal/service/ ./internal/handler/... -count=1)
}

if [[ "${1:-}" == "--test-only" ]]; then
  run_tests
  exit 0
fi

current="$(git rev-parse --abbrev-ref HEAD)"
if [[ "$current" != "$BRANCH" ]]; then
  echo "当前分支是 $current，请先 checkout $BRANCH（建议用独立 worktree）" >&2
  exit 1
fi
if [[ -n "$(git status --porcelain)" ]]; then
  echo "工作区不干净，请先提交或清理" >&2
  exit 1
fi

if ! git remote get-url "$UPSTREAM_REMOTE" >/dev/null 2>&1; then
  git remote add "$UPSTREAM_REMOTE" "$UPSTREAM_URL"
fi
echo "==> fetch $UPSTREAM_REMOTE / origin"
git fetch --quiet "$UPSTREAM_REMOTE" --tags
git fetch --quiet origin

NEW_BASE_REF="${1:-$UPSTREAM_REMOTE/main}"
NEW_BASE="$(git rev-parse --verify "${NEW_BASE_REF}^{commit}")"
# 旧基线 = 当前分支与 upstream/main 的分叉点；其后的提交即我们的 patch。
OLD_BASE="$(git merge-base HEAD "$UPSTREAM_REMOTE/main")"

echo "==> 旧基线: $(git log --oneline -1 "$OLD_BASE")"
echo "==> 新基线: $(git log --oneline -1 "$NEW_BASE") ($NEW_BASE_REF)"
echo "==> 待重放的 patch:"
git log --oneline --reverse "$OLD_BASE..HEAD"

echo "==> 新基线相对旧基线新增/修改的 migrations（部署前确认）:"
git diff --stat "$OLD_BASE" "$NEW_BASE" -- backend/migrations | tail -20 || true

BACKUP_REF="refs/4ipic-backup/$(date +%Y%m%d%H%M%S)"
git update-ref "$BACKUP_REF" HEAD
echo "==> 旧分支头已备份到 $BACKUP_REF"

if ! git rebase --onto "$NEW_BASE" "$OLD_BASE" "$BRANCH"; then
  cat >&2 <<EOF

rebase 出现冲突。请按各 patch 的意图解决后：
  git add <files> && git rebase --continue
完成后运行：deploy/4ipic/rebase.sh --test-only
放弃本次 rebase：git rebase --abort （旧分支头在 $BACKUP_REF）
EOF
  exit 1
fi

echo "==> rebase 后的 patch:"
git log --oneline --reverse "$NEW_BASE..HEAD"

if [[ "${SKIP_TESTS:-0}" != "1" ]]; then
  run_tests
fi

if [[ "${PUSH:-0}" == "1" ]]; then
  git push --force-with-lease origin "$BRANCH"
else
  echo "==> 未 push。确认无误后：git push --force-with-lease origin $BRANCH"
fi
