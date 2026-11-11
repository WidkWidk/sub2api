# 4ipic 部署分支

`4ipic` 分支 = 上游 `Wei-Shaw/sub2api` 的 `main`（或指定 tag）+ 下面这些本地 patch，用于 4ipic.com 生产部署。
**不要合并到 `main`。**

## Patch 列表

分支上相对上游基线多出的提交就是 patch（`git log --oneline upstream/main..4ipic`）：

| Patch | 来源 |
| --- | --- |
| fix(antigravity): forward thinking config for Gemini via OpenAI-compatible endpoints | `widkwidk-antigravity-gemini-compat-thinking` |
| fix(antigravity): make Gemini stream errors retryable for go-genai clients | PR #1 |
| fix(antigravity): fail over to another account after upstream network errors | PR #1 |
| chore(4ipic): branch maintenance and deploy scripts | 本目录 |

上游合入某个 patch 之后，`git rebase` 会按 patch-id 自动跳过它。如果上游合入的版本有改动，跳不过去，就在冲突里选上游的实现。

## 更新到上游最新

建议在独立的 worktree 里操作：

```bash
git worktree add ../sub2api-4ipic 4ipic && cd ../sub2api-4ipic
deploy/4ipic/rebase.sh            # 基线 = upstream/main；可传 tag，例如 v0.2.22
# 出现冲突：按 patch 意图解决 → git add → git rebase --continue → deploy/4ipic/rebase.sh --test-only
git push --force-with-lease origin 4ipic   # 或者 PUSH=1 deploy/4ipic/rebase.sh
```

脚本会先把旧的分支头保存到 `refs/4ipic-backup/<时间>`，然后列出新基线引入的 migrations。部署前请确认这些 migrations 能安全执行；服务启动时会自动执行它们，而回滚二进制不会撤销 migration。

## 构建与部署

需要的工具：Go（版本见 `backend/go.mod`）、Node 24、pnpm，以及可以 SSH 登录服务器的私钥（私钥不进仓库）。

```bash
deploy/4ipic/build_and_deploy.sh --build-only     # 只构建，产物在 dist-deploy/sub2api，并打印 sha256
deploy/4ipic/build_and_deploy.sh                  # 构建 → scp → 备份 → 安装 → 重启 → 验证
BACKUP_NAME=sub2api.0.2.21.official.bak deploy/4ipic/build_and_deploy.sh
```

- 版本号取自 `backend/cmd/server/VERSION`，commit 是当前的 HEAD。
- 备份默认存为 `/opt/sub2api/sub2api.<版本>-<commit9>.bak`，同名文件已存在时会追加时间戳。
- 验证内容：`/health` 返回 200、服务处于 active、新进程的日志里没有 ERROR/FATAL/panic。任何一项失败都会自动回滚到备份并重启。
- 最后会检查公网 `PUBLIC_URL` 是否返回 200，并打印回滚命令。
- 部署后建议再手动跑一次端到端检查：`agy -p "reply ok"`。

可以通过环境变量覆盖的配置：`SSH_KEY`、`SSH_HOST`、`REMOTE_DIR`、`SERVICE`、`HEALTH_URL`、`PUBLIC_URL`、`GO_BIN`、`PNPM_BIN`、`SKIP_FRONTEND=1`、`ALLOW_DIRTY=1`。

手动回滚：

```bash
ssh -i ~/.ssh/ipic-llm-proxy_key.pem azureuser@4.190.160.18 \
  'sudo install -o sub2api -g sub2api -m 755 /opt/sub2api/<备份文件> /opt/sub2api/sub2api && sudo systemctl restart sub2api'
```
