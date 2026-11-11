#!/usr/bin/env bash
# 构建 linux/amd64 二进制（内嵌前端）并部署到 4ipic，失败自动回滚。
#
# 用法（在 4ipic 分支的干净 checkout/worktree 中执行）：
#   deploy/4ipic/build_and_deploy.sh               # 构建 + 部署
#   deploy/4ipic/build_and_deploy.sh --build-only  # 只构建，产物在 dist-deploy/sub2api
#   SKIP_FRONTEND=1 ...                            # 复用已有的 backend/internal/web/dist
#   BACKUP_NAME=sub2api.0.2.21.official.bak ...    # 自定义服务器端备份文件名
#
# 环境变量（均有默认值，不要把密钥写进仓库）：
#   SSH_KEY      SSH 私钥路径（默认 ~/.ssh/ipic-llm-proxy_key.pem）
#   SSH_HOST     user@host（默认 azureuser@4.190.160.18）
#   REMOTE_DIR   服务器安装目录（默认 /opt/sub2api）
#   SERVICE      systemd 服务名（默认 sub2api）
#   RUN_USER     二进制属主（默认 sub2api）
#   HEALTH_URL   服务器本机健康检查（默认 http://127.0.0.1:8080/health）
#   PUBLIC_URL   公网检查地址（默认 https://4ipic.com/）
#   GO_BIN       go 可执行文件（默认 go）；PNPM_BIN 同理（默认 pnpm）
#   ALLOW_DIRTY  =1 时允许工作区有未提交改动（commit 号会带 -dirty）
set -euo pipefail

SSH_KEY="${SSH_KEY:-$HOME/.ssh/ipic-llm-proxy_key.pem}"
SSH_HOST="${SSH_HOST:-azureuser@4.190.160.18}"
REMOTE_DIR="${REMOTE_DIR:-/opt/sub2api}"
SERVICE="${SERVICE:-sub2api}"
RUN_USER="${RUN_USER:-sub2api}"
HEALTH_URL="${HEALTH_URL:-http://127.0.0.1:8080/health}"
PUBLIC_URL="${PUBLIC_URL:-https://4ipic.com/}"
GO_BIN="${GO_BIN:-go}"
PNPM_BIN="${PNPM_BIN:-pnpm}"

BUILD_ONLY=0
[[ "${1:-}" == "--build-only" ]] && BUILD_ONLY=1

ROOT="$(git rev-parse --show-toplevel)"
cd "$ROOT"

COMMIT="$(git rev-parse HEAD)"
if [[ -n "$(git status --porcelain -- backend frontend)" ]]; then
  if [[ "${ALLOW_DIRTY:-0}" != "1" ]]; then
    echo "backend/frontend 有未提交改动；提交后再部署，或设置 ALLOW_DIRTY=1" >&2
    exit 1
  fi
  COMMIT="${COMMIT}-dirty"
fi
VERSION="$(tr -d '[:space:]' < backend/cmd/server/VERSION)"
BUILD_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
OUT="$ROOT/dist-deploy/sub2api"

if [[ "${SKIP_FRONTEND:-0}" != "1" ]]; then
  echo "==> 构建前端"
  (cd frontend && "$PNPM_BIN" install --frozen-lockfile && "$PNPM_BIN" run build)
fi
[[ -f backend/internal/web/dist/index.html ]] || { echo "缺少前端产物 backend/internal/web/dist" >&2; exit 1; }

echo "==> 构建 linux/amd64 二进制 (version=$VERSION commit=$COMMIT)"
mkdir -p "$(dirname "$OUT")"
(cd backend && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 "$GO_BIN" build -tags=embed \
  -ldflags "-s -w -X main.Version=$VERSION -X main.Commit=$COMMIT -X main.Date=$BUILD_DATE -X main.BuildType=release" \
  -o "$OUT" ./cmd/server)

[[ "$(file "$OUT")" == *'ELF 64-bit LSB executable, x86-64'* ]] || { echo "产物不是 ELF x86-64" >&2; exit 1; }
grep -aq 'assets/index-' "$OUT" || { echo "产物中未找到内嵌前端资源" >&2; exit 1; }
SHA256="$(shasum -a 256 "$OUT" | awk '{print $1}')"
echo "==> 产物: $OUT"
echo "    sha256: $SHA256"

if [[ "$BUILD_ONLY" == "1" ]]; then
  exit 0
fi

SSH=(ssh -i "$SSH_KEY" -o BatchMode=yes "$SSH_HOST")
echo "==> 上传到 $SSH_HOST:/tmp/sub2api.new"
scp -q -i "$SSH_KEY" -o BatchMode=yes "$OUT" "$SSH_HOST:/tmp/sub2api.new"

echo "==> 远程安装"
"${SSH[@]}" bash -s -- "$SHA256" "$COMMIT" "$REMOTE_DIR" "$SERVICE" "$RUN_USER" "$HEALTH_URL" "${BACKUP_NAME:-}" <<'REMOTE'
set -euo pipefail
SHA256="$1"; COMMIT="$2"; REMOTE_DIR="$3"; SERVICE="$4"; RUN_USER="$5"; HEALTH_URL="$6"; BACKUP_NAME="$7"
NEW=/tmp/sub2api.new
BIN="$REMOTE_DIR/sub2api"

echo "$SHA256  $NEW" | sha256sum -c --quiet
chmod +x "$NEW"
newver="$("$NEW" --version 2>&1)"
[[ "$newver" == *"commit: $COMMIT"* ]] || { echo "新二进制 --version 与预期 commit 不符" >&2; exit 1; }

if [[ -z "$BACKUP_NAME" ]]; then
  curver="$("$BIN" --version 2>&1 || true)"
  cur="$(sed -n 's/.*Sub2API \([^ ]*\) (commit: \([0-9a-f]\{9\}\).*/\1-\2/p' <<<"$curver")"
  BACKUP_NAME="sub2api.${cur:-unknown}.bak"
fi
BACKUP="$REMOTE_DIR/$BACKUP_NAME"
[[ -e "$BACKUP" ]] && BACKUP="$BACKUP.$(date +%Y%m%d%H%M%S)"
sudo cp -p "$BIN" "$BACKUP"
echo "备份: $BACKUP"

rollback() {
  echo "!! 验证失败，回滚到 $BACKUP" >&2
  sudo install -o "$RUN_USER" -g "$RUN_USER" -m 755 "$BACKUP" "$BIN"
  sudo systemctl restart "$SERVICE"
  sleep 5
  systemctl is-active "$SERVICE" >&2 || true
  "$BIN" --version 2>&1 | tail -1 >&2
  exit 1
}

sudo install -o "$RUN_USER" -g "$RUN_USER" -m 755 "$NEW" "$BIN"
sudo systemctl restart "$SERVICE"

ok=0
for _ in $(seq 1 30); do
  sleep 2
  if [[ "$(curl -s -o /dev/null -w '%{http_code}' "$HEALTH_URL" || true)" == "200" ]]; then ok=1; break; fi
done
[[ "$ok" == "1" ]] || rollback
systemctl is-active --quiet "$SERVICE" || rollback
PID="$(systemctl show -p MainPID --value "$SERVICE")"
errs="$(sudo journalctl -u "$SERVICE" _PID="$PID" --no-pager | grep -ciE '"level":"(error|fatal)"|\bERROR\b|FATAL|panic' || true)"
if [[ "$errs" != "0" ]]; then
  sudo journalctl -u "$SERVICE" _PID="$PID" --no-pager | grep -iE '"level":"(error|fatal)"|\bERROR\b|FATAL|panic' | tail -20 >&2
  rollback
fi
"$BIN" --version 2>&1 | tail -1
echo "active, health=200, startup errors=0, pid=$PID"
rm -f "$NEW"
echo "BACKUP_PATH=$BACKUP"
REMOTE

code="$(curl -s -o /dev/null -w '%{http_code}' "$PUBLIC_URL" || true)"
echo "==> $PUBLIC_URL -> $code"
if [[ "$code" != "200" ]]; then
  echo "!! 公网检查失败。回滚命令见下方" >&2
fi
cat <<EOF

部署完成: commit=$COMMIT sha256=$SHA256
回滚（BACKUP_PATH 见上方输出）:
  ssh -i $SSH_KEY $SSH_HOST 'sudo install -o $RUN_USER -g $RUN_USER -m 755 <BACKUP_PATH> $REMOTE_DIR/sub2api && sudo systemctl restart $SERVICE'
EOF
[[ "$code" == "200" ]]
