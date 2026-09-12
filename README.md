# bdpan

Baidu Netdisk CLI with an AWS S3-style file-operation interface.

[中文](#中文) | [English](#english)

## 中文

授权仍使用百度 OAuth；文件操作使用顶层 `ls`、`cp`、`mv`、`rm`、`sync`。
这是高层 CLI 操作习惯的对齐，不是 S3 协议服务，也不实现 `s3api`。

### 构建与授权

需要 Go 1.25 或更高版本：

```bash
make
./output/bdpan --help
./output/bdpan cp --help

# 可选：指定版本；构建 macOS/Linux/Windows 的 amd64/arm64 版本
make VERSION=v0.1.0
make build-all
```

推荐通过环境变量提供凭证，避免进入 shell 历史：

```bash
export BAIDU_APP_ID='你的 AppID'
export BAIDU_APP_KEY='你的 AppKey'
export BAIDU_SECRET_KEY='你的 SecretKey'
./output/bdpan auth device-login

# 或直接使用已有令牌
export BAIDU_ACCESS_TOKEN='你的 Access Token'
```

设备登录输出验证地址、用户码和二维码地址；完成授权后保存令牌。
令牌文件权限为 `0600`，默认位置：

- macOS：`~/Library/Application Support/bdpan/token.json`
- Linux：`$XDG_CONFIG_HOME/bdpan/token.json`，通常为 `~/.config/bdpan/token.json`
- Windows：`%AppData%\bdpan\token.json`

可用 `--token-file ./token.json` 更改位置。刷新和授权码模式保持不变：

```bash
./output/bdpan auth refresh
./output/bdpan auth refresh --refresh-token '刷新令牌'
./output/bdpan auth url --redirect-uri 'https://example.com/callback'
./output/bdpan auth exchange --code '授权码' --redirect-uri 'https://example.com/callback'
```

### 文件操作

远端路径使用 `bd://`：`bd://apps/demo/a.txt` 表示 `/apps/demo/a.txt`，
`bd://` 表示网盘根目录；`apps` 不是 bucket。URI 中的空格、中文、`%` 等按
字面路径处理，不做 URL 解码。实际可读写范围由百度应用权限决定。

下列示例假设已将 `output` 加入 `PATH`：

```bash
bdpan ls
bdpan ls bd://apps/demo/ --recursive --human-readable --summarize

# 上传、下载、远端复制；源在前、目标在后
bdpan cp ./a.txt bd://apps/demo/a.txt
bdpan cp bd://apps/demo/a.txt ./a.txt
bdpan cp bd://apps/demo/a.txt bd://apps/demo/archive/

# 复制目录内容；不复制空目录
bdpan cp ./photos/ bd://apps/demo/photos/ --recursive
bdpan cp bd://apps/demo/photos/ ./photos/ --recursive

# 可选：同时传输最多 4 个文件（默认 1）
bdpan cp ./photos/ bd://apps/demo/photos/ --recursive --concurrency 4
bdpan cp bd://apps/demo/photos/ ./photos/ --recursive --concurrency 4
bdpan sync ./photos/ bd://apps/demo/photos/ --concurrency 4

# 最后匹配的规则生效；* 可匹配跨目录路径
bdpan cp ./reports/ bd://apps/demo/reports/ --recursive \
  --exclude '*' --include '*.txt' --exclude 'private/*'

# 移动成功确认目标后才删除源
bdpan mv ./a.txt bd://apps/demo/a.txt
bdpan rm bd://apps/demo/old.txt
bdpan rm bd://apps/demo/cache/ --recursive --exclude '*.keep'

# 先预览，再同步；删除目标多余文件必须明确启用 --delete
bdpan sync ./reports/ bd://apps/demo/reports/ --delete --dryrun
bdpan sync ./reports/ bd://apps/demo/reports/ --delete
bdpan sync bd://apps/demo/reports/ ./reports/
bdpan sync bd://apps/demo/reports/ bd://apps/demo/archive/

# stdin 上传会先缓存到临时文件；stdout 下载只有文件字节
printf 'hello\n' | bdpan cp - bd://apps/demo/hello.txt --expected-size 6
bdpan cp bd://apps/demo/hello.txt - > hello.txt
```

默认覆盖同名文件，不需要 `--yes`。`--no-overwrite` 跳过已存在的目标。
目标以 `/` 结尾或已存在为目录时，单文件操作会追加源文件名；递归操作复制
源目录的内容，不额外套一层源目录名。禁止本地到本地操作和重叠的远端递归目录。

### 参数

| 命令 | 参数 |
| --- | --- |
| 所有新文件命令 | `--page-size`（默认 1000，范围 1–1000；自动读取全部页） |
| `ls` | `--recursive`、`--human-readable`、`--summarize` |
| `cp` / `mv` / `rm` | `--recursive` |
| `cp` / `mv` / `rm` / `sync` | `--dryrun`、有序重复的 `--include` / `--exclude`、`--quiet`、`--only-show-errors` |
| `cp` / `mv` / `sync` | `--concurrency`（正整数，默认 1）、`--part-concurrency`（1–32，默认 1）、`--no-overwrite`、`--follow-symlinks` / `--no-follow-symlinks`、`--no-progress`、`--progress-frequency`（秒）、`--progress-multiline`、`--case-conflict` |
| `cp` | `--expected-size`（仅 stdin；大小提示，实际大小由临时文件确定） |
| `sync` | `--size-only`、`--exact-timestamps`、`--delete` |

过滤规则相对于源目录，默认为全部包含；仅指定 `--include` 不会排除其他文件。
支持 `*`、`?`、`[abc]`、`[!abc]`。删除阶段对目标的相对路径使用同一组规则。
`sync` 总是递归，不接收 `--recursive`。

`--concurrency N` 控制同时传输的文件数，支持上传、下载和远端复制。
默认 `1` 保持文件级串行；单文件上传/下载分片并发由独立的 `--part-concurrency N` 控制。
并发执行时，进度自动逐行显示，成功动作按完成顺序输出；`--dryrun` 保持稳定顺序。
检测到会继续执行的本地目标大小写冲突时，整批下载会警告并退回串行。
取消会停止分派新文件并等待在途任务退出；`sync --delete` 仍在全部传输成功且无跳过文件后串行执行。

大文件上传会用 `uinfo`（`vip_version=v2`）查询**当前授权账号**：普通用户、
VIP、SVIP 分别使用 4、16、32 MiB 分片；未知身份保守使用 4 MiB，
身份查询失败则报错，不会猜测会员权限。文件不超过 4 MiB 时无需查询身份。
上传域名由每个任务的 `locateupload` 动态获取，过期或网络重试时重新获取；
仅接受可信的 HTTPS 地址，不跟随上传或域名查询的重定向。

SVIP 大文件可以先尝试 4 个并行分片：

```bash
bdpan cp ./video.mp4 bd://apps/demo/video.mp4 --part-concurrency 4
# 同时上传 2 个文件，每个文件最多 4 个并行分片
bdpan cp ./videos/ bd://apps/demo/videos/ --recursive --concurrency 2 --part-concurrency 4
# 单文件分片下载（超过 8 MiB 时使用 Range）
bdpan cp bd://apps/demo/video.mp4 ./video.mp4 --part-concurrency 4
```

总分片请求数上限是两种并发参数的乘积。分片流式读取，不把每个上传请求的完整分片
缓存在内存中；上传分片的网络故障、HTTP 429/5xx 最多尝试 3 次。
上传进度按流式读取的文件字节更新，失败尝试撤回进度，重传流量计入最近约 5 秒的速度；
准备、传输、重试和校验阶段持续更新，服务端确认前不显示 100%。
stdin 仍先串行写入临时文件，再按同样规则并行上传。

传输超时自动管理，不再把普通 API 的 30 秒总超时套到文件传输上。
空闲预算为 `30s + 分片 MiB 数 × 分片并发数 × 1s`，自动限制在 30–120 秒，
每次有文件数据流动都会续期，持续传输不受总时长限制；`--timeout` 仍控制普通 API
请求总时长，并可提高传输空闲预算的下限。父级取消和 Ctrl-C 始终有效。

本地文件下载在 `--part-concurrency > 1` 且文件超过 8 MiB 时使用 8 MiB 分段：
先用第一段确认 Range 支持，再并行写入同一个临时文件的不同偏移。
不支持 Range 时自动回退单连接；严格检查 `206`、`Content-Range`、长度、
可用的 `Content-MD5` 和强 ETag，完成后再次核对远端元数据。
网络中断、超时、HTTP 429/5xx 等可恢复故障仅重试失败分段，最多尝试 3 次；
下载地址过期或重试时重新获取并校验元数据，已完成分段不重下。
默认单连接下载失败时可从头重试；stdout 保持顺序流，已输出字节后不重试。
这些重试仅限本次命令，不提供跨进程断点续传，失败/取消仍会清理临时文件。
SVIP 不代表端到端带宽保证；并发过高也可能增加失败，建议先测 4，再比较 8。

同步先比较大小；大小相同时，上传/远端复制在源时间更新时传输；默认下载在
本地目标时间晚于远端源时间时传输，沿用 AWS CLI 的方向性比较。
普通比较使用秒精度，避免百度秒级时间与本地纳秒时间导致重复上传。
`--exact-timestamps` 仅改变下载：大小相同但时间不完全相等也会传输；
`--size-only` 优先，只比较大小。下载保存远端修改时间。

`--case-conflict` 用于下载，默认 `ignore`；`error` 在执行前终止，
`warn` 警告后继续，`skip` 跳过冲突文件。检查同批目标和现有文件的大小写冲突，
但不模拟各文件系统的 Unicode 规范化规则。忽略冲突可能在大小写不敏感的磁盘上覆盖文件。

### 安全与兼容边界

- `--dryrun` 可以查询元数据，但不写文件、不创建目录、不缓存/读取 stdin，
  也不调用任何远端写 API；向 stdout 下载的 dryrun 不输出文件字节。
- 下载先写同目录临时文件，检查长度及服务端提供的 `Content-MD5`，成功后替换目标；
  失败保留原目标。stdout 是流，失败时已经输出的字节无法撤回。
- 移动在目标确认成功后才删除源，源变化或服务端仅返回异步任务受理时保留源。
  远端移动使用“复制并确认，再删除”，不是事务；删除失败可能留下两份文件。
- `sync --delete` 仅在完整扫描、全部传输成功且没有跳过文件后执行清理；
  扫描失败不执行任何计划操作，传输失败则不清理。这比 AWS 更保守。
- 百度有真实目录，S3 没有：不传输空目录，递归删除/移动不清理遗留的空目录；
  `mb`、`rb` 不会伪装成创建/删除目录。
- 默认跟随本地上传源符号链接；可用 `--no-follow-symlinks` 跳过。
  检测目录环；移动拒绝经过目录符号链接，以免删除链接所指目录中的文件。
  下载不覆盖符号链接或特殊文件，不允许派生路径逃逸目标目录。
- `mb`、`rb`、`presign`、`website` 明确不支持；百度 dlink 不是 S3 预签名 URL。
  ACL、KMS、storage class、S3 对象元数据、AWS profile/region/endpoint 等参数不支持，
  传入会报错，不会静默忽略。保留百度凭证、超时和补全体系。
- 默认串行，可用 `--concurrency` 启用文件级并发、`--part-concurrency` 启用上传/下载分片并发；并发越高，资源占用越高。
  不提供断点续传或持久化清单。大目录扫描和逐文件验证可能较慢；
  百度接口/会员限制、限速、权限及配额仍然适用。

成功动作输出到 stdout：`upload:`、`download:`、`copy:`、`move:`、`delete:`；
预览带 `(dryrun)` 前缀。进度、警告和错误写 stderr；
`--quiet` / `--only-show-errors` 隐藏成功动作和进度，但保留诊断。
进度显示百分比、已传/总大小（KiB/MiB/GiB）、最近约 5 秒的速度及预计剩余时间。
本次仅传输一个文件时不显示文件名；多文件传输在进度前显示不带外层引号的目标文件名。
文件数以过滤和同步比较后的实际传输计划为准，与 `--concurrency` 或
`--part-concurrency` 的值无关。文件名中的换行等控制字符仍会转义显示。
单文件示例：

```text
94.3% | 1.7 GiB/1.8 GiB | 8.2 MiB/s | ETA 13s | transferring
```

多文件进度示例：

```text
课程 01.mp4: 94.3% | 1.7 GiB/1.8 GiB | 8.2 MiB/s | ETA 13s | transferring
```

递归上传保留相对路径和文件名；单文件目标以 `/` 结尾时保留源文件名，
指定完整目标文件名则按该名称保存。stdin 上传的临时文件名不会用作远端文件名。

新文件命令退出码：`0` 成功，`1` 操作失败，`2` 有文件被跳过，
`130` 取消，`252` 参数错误/不支持，`253` 无可用凭证。授权命令行为保持不变。

### 查询与迁移

以下百度专有查询保持原参数和 JSON 输出：

```bash
bdpan info
bdpan quota
bdpan search --key report --dir /apps/demo
bdpan docs --parent-path /apps/demo
bdpan images --parent-path /apps/demo
bdpan meta --path /apps/demo/a.txt
bdpan metas --fsids '[123456789]'
bdpan metas-path --target '["/apps/demo/a.txt"]'
bdpan quota | jq .
```

这是破坏性变更，不保留旧命令别名：

| 旧命令 | 新命令 |
| --- | --- |
| `user info` / `user quota` | `info` / `quota` |
| `file list --dir /apps/demo` | `ls bd://apps/demo/` |
| `file list-all --path /apps/demo --recursion 1` | `ls bd://apps/demo/ --recursive` |
| `file upload --file a.txt --path /apps/demo/a.txt --yes` | `cp a.txt bd://apps/demo/a.txt` |
| `file copy` / `file move` / `file rename`（JSON 参数） | `cp` / `mv`（源、目标位置参数） |
| `file delete --filelist ... --yes` | `rm bd://...` |
| `file search/docs/images/meta/metas/metas-path` | 去掉 `file` 前缀 |
| `file precreate/upload-part/create` | 移除，使用 `cp` 自动分片上传 |

### 全局配置、补全与验证

| 参数 | 环境变量 |
| --- | --- |
| `--app-id` | `BAIDU_APP_ID` |
| `--app-key` | `BAIDU_APP_KEY` |
| `--secret-key` | `BAIDU_SECRET_KEY` |
| `--access-token` | `BAIDU_ACCESS_TOKEN` |
| `--token-file` | 无 |
| `--timeout`（默认 `30s`，普通 API 请求总时长；传输空闲预算下限） | 无 |

非空命令行凭证优先于环境变量。使用已有令牌执行文件操作不需要应用密钥。

```bash
source <(bdpan completion bash)
source <(bdpan completion zsh)
bdpan completion fish | source
# PowerShell: bdpan completion powershell | Out-String | Invoke-Expression

go test ./...
go test -race ./...
go vet ./...
```

测试使用模拟 HTTP，不访问真实网盘。入口位于 `cmd/bdpan`，实现位于 `internal/cli`。

## English

Authorization continues to use Baidu OAuth. File operations use top-level
`ls`, `cp`, `mv`, `rm`, and `sync`, following the high-level AWS S3 CLI interface.
This is neither an S3 protocol gateway nor an implementation of `s3api`.

### Build and authorize

Go 1.25+ is required. Run `make`; the executable is `output/bdpan`.
Use `make VERSION=v0.1.0` to set the version, or `make build-all` to build
macOS/Linux/Windows for amd64/arm64.

```bash
export BAIDU_APP_ID='your AppID'
export BAIDU_APP_KEY='your AppKey'
export BAIDU_SECRET_KEY='your SecretKey'
./output/bdpan auth device-login

# Alternatively, supply an existing token:
export BAIDU_ACCESS_TOKEN='your access token'
./output/bdpan auth refresh
```

Device login displays the verification URL, user code, and QR-code URL, then
saves the token with `0600` permissions. Defaults:

- macOS: `~/Library/Application Support/bdpan/token.json`
- Linux: `$XDG_CONFIG_HOME/bdpan/token.json`, usually `~/.config/bdpan/token.json`
- Windows: `%AppData%\bdpan\token.json`

Override with `--token-file`. The existing `auth url`, `exchange`, `device-code`,
`device-token`, `device-login`, and `refresh` flows and flags are unchanged.

### File operations

`bd://apps/demo/a.txt` maps literally to `/apps/demo/a.txt`; `bd://` is the root.
There are no buckets, and URI paths are not URL-decoded. Baidu application
permissions determine the accessible paths. Examples assume `output` is on `PATH`:

```bash
bdpan ls bd://apps/demo/ --recursive --human-readable --summarize
bdpan cp ./a.txt bd://apps/demo/a.txt
bdpan cp bd://apps/demo/a.txt ./a.txt
bdpan cp bd://apps/demo/a.txt bd://apps/demo/archive/
bdpan cp ./photos/ bd://apps/demo/photos/ --recursive
bdpan cp ./photos/ bd://apps/demo/photos/ --recursive --concurrency 4
bdpan cp bd://apps/demo/photos/ ./photos/ --recursive --concurrency 4
bdpan sync ./photos/ bd://apps/demo/photos/ --concurrency 4
bdpan mv ./a.txt bd://apps/demo/a.txt
bdpan rm bd://apps/demo/cache/ --recursive --exclude '*.keep'

bdpan sync ./reports/ bd://apps/demo/reports/ --delete --dryrun
bdpan sync ./reports/ bd://apps/demo/reports/ --delete
bdpan sync bd://apps/demo/reports/ ./reports/
bdpan sync bd://apps/demo/reports/ bd://apps/demo/archive/

bdpan cp ./reports/ bd://apps/demo/reports/ --recursive \
  --exclude '*' --include '*.txt' --exclude 'private/*'
printf 'hello\n' | bdpan cp - bd://apps/demo/hello.txt --expected-size 6
bdpan cp bd://apps/demo/hello.txt - > hello.txt
```

Arguments are source then destination. Overwrite is the default; `--yes` is
removed. Use `--no-overwrite` to skip existing files. A trailing `/` or existing
destination directory appends the source filename for a single file. Recursive
operations copy directory contents, without another source-directory level.
Local-to-local operations and overlapping recursive remote paths are rejected.

### Flags and synchronization

| Commands | Flags |
| --- | --- |
| All file commands | `--page-size` (default 1000, range 1–1000; all pages are read) |
| `ls` | `--recursive`, `--human-readable`, `--summarize` |
| `cp`, `mv`, `rm` | `--recursive` |
| `cp`, `mv`, `rm`, `sync` | `--dryrun`, ordered repeatable `--include`/`--exclude`, `--quiet`, `--only-show-errors` |
| `cp`, `mv`, `sync` | `--concurrency` (positive integer, default 1), `--part-concurrency` (1–32, default 1), `--no-overwrite`, `--follow-symlinks`/`--no-follow-symlinks`, `--no-progress`, `--progress-frequency` (seconds), `--progress-multiline`, `--case-conflict` |
| `cp` | `--expected-size` (stdin only, advisory; the spool determines actual size) |
| `sync` | `--size-only`, `--exact-timestamps`, `--delete`; always recursive |

Filter paths are source-relative. Everything starts included, and the last
matching rule wins. `--include` alone does not exclude other files. Patterns
support `*` (including `/`), `?`, `[abc]`, and `[!abc]`. Cleanup uses the same
rules on destination-relative paths, never deleting excluded files.

`--concurrency N` bounds concurrent file uploads, downloads, and remote copies.
The default, `1`, keeps file-level execution serial; `--part-concurrency N`
independently bounds upload/download parts within each file. Parallel file progress uses
separate lines and actions appear in completion order; dry runs keep stable order.
Accepted case-colliding local targets trigger a warning and serial execution of
the whole download plan. Cancellation stops dispatch and waits for active tasks
to exit. `sync --delete` remains serial, after all transfers succeed without skips.

Uploads larger than 4 MiB query the **authorized account** using `uinfo`
(`vip_version=v2`): ordinary, VIP, and SVIP accounts use 4, 16, and 32 MiB parts.
Unknown membership types use 4 MiB; failed membership queries return an error.
Smaller files skip this query. Each upload task obtains its HTTPS host through
`locateupload`, refreshing on expiry or network retries; routing and upload
requests do not follow redirects.

Start with four parallel parts for a large SVIP upload:

```bash
bdpan cp ./video.mp4 bd://apps/demo/video.mp4 --part-concurrency 4
bdpan cp ./videos/ bd://apps/demo/videos/ --recursive --concurrency 2 --part-concurrency 4
bdpan cp bd://apps/demo/video.mp4 ./video.mp4 --part-concurrency 4
```

The maximum number of simultaneous part requests is the product of both
concurrency settings. Multipart bodies stream from disk without buffering whole
parts per request. Upload network errors and HTTP 429/5xx get at most three attempts.
Upload progress follows streamed file bytes, rolls back failed attempts, and
includes retransmitted traffic in an approximately five-second rolling rate.
Preparing, transferring, retrying, and verifying stages remain visible; 100% is
reserved for confirmed success. Stdin is still spooled sequentially, then uploaded.

File transfers no longer inherit the ordinary API client's total 30-second
deadline. Their idle budget is calculated as `30s + part MiB × part concurrency × 1s`,
clamped to 30–120 seconds, and renewed as bytes flow. Healthy transfers have no
total-duration limit. `--timeout` still controls API request duration and can raise
the minimum transfer idle budget. Parent cancellation and Ctrl-C remain effective.

With `--part-concurrency > 1`, files larger than 8 MiB download in 8 MiB ranges.
The first part probes Range support; subsequent parts write concurrently at
distinct offsets in the temporary file. Servers ignoring Range fall back to one
stream. Responses must match status 206, Content-Range, length, any supplied
Content-MD5, and the established strong ETag; metadata is checked again at completion.
Interrupted requests, idle timeouts, HTTP 429/5xx, and expired links get bounded
retries (three attempts per segment), refreshing the download URL and verifying
metadata without redownloading completed segments. Serial local downloads may
restart from the beginning; stdout stays sequential and never replays emitted bytes.
Retries are in-process only, not persistent resume across commands; failure or
cancellation still removes the temporary file. SVIP is not an end-to-end throughput
guarantee; try four parts before comparing eight, since excessive concurrency can
increase failures.

Sync transfers missing or different-sized files. For equal sizes, upload and
remote copy transfer a newer source; default downloads transfer when the local
destination is newer than the remote source, following AWS CLI's directional
comparison. Ordinary comparisons use whole seconds to match Netdisk precision.
For downloads, `--exact-timestamps` transfers any timestamp difference;
`--size-only` takes precedence and ignores timestamps. Downloads preserve the
remote modification time.

Download `--case-conflict` defaults to `ignore`. `error` aborts before execution,
`warn` warns and continues, and `skip` skips conflicting files. Checks cover
planned and existing paths, not filesystem-specific Unicode normalization.
Ignoring collisions can overwrite files on case-insensitive filesystems.

### Safety and compatibility limits

- Dry runs read metadata but never create directories/files, spool/read stdin,
  or call remote write APIs. A stdout-download dry run emits no file bytes.
- Downloads stream to a sibling temporary file, verify length and any supplied
  `Content-MD5`, then replace the destination. Failed downloads preserve the
  original. Bytes already streamed to stdout cannot be retracted on failure.
- Moves delete the source only after destination confirmation and a source
  recheck. Remote moves copy, confirm, then delete; they are not atomic.
  Unconfirmed asynchronous tasks leave the source intact. A failed delete can
  leave two copies.
- `sync --delete` cleans up only after a complete scan, successful transfers,
  and no skipped files. A scan failure executes no plan; a transfer failure
  suppresses cleanup. This is intentionally more conservative than AWS.
- Empty directories are not copied. Recursive removal/moves leave empty Baidu
  directories behind. Baidu directories are not S3 buckets.
- Local upload symlinks are followed by default; `--no-follow-symlinks` skips
  them. Directory loops are rejected. Moves refuse directory symlinks to avoid
  deleting their targets' contents. Downloads reject symlink/special-file
  destinations and prevent derived paths escaping the destination directory.
- `mb`, `rb`, `presign`, and `website` explicitly fail as unsupported. A Baidu
  dlink is not an S3 presigned URL. ACL, KMS, storage class, S3 object metadata,
  and AWS profile/region/endpoint flags also fail rather than being ignored.
- Execution defaults to serial; `--concurrency` enables file-level parallelism
  and `--part-concurrency` enables parallel upload/download parts,
  with increased memory and network usage. There is no resume system or persistent
  inventory; listing and per-file confirmation can be slow on large directories.
  Baidu permission, membership, quota, rate, and API limits still apply.

Successful actions go to stdout as `upload:`, `download:`, `copy:`, `move:`,
or `delete:`; previews have a `(dryrun)` prefix. Binary stdout downloads emit
only bytes. Progress, warnings, and errors go to stderr. `--quiet` and
`--only-show-errors` suppress actions/progress, not diagnostics.
Progress shows percentage, transferred/total size in binary units, speed over
roughly the last five seconds, and estimated time remaining. A single-file transfer
omits the filename; multi-file transfers prefix the destination filename without
surrounding quotes. The file count comes from the actual transfer plan after filtering
and sync comparisons, regardless of `--concurrency` or `--part-concurrency`.
Control characters in filenames, such as newlines, remain escaped.
Single-file example:

```text
94.3% | 1.7 GiB/1.8 GiB | 8.2 MiB/s | ETA 13s | transferring
```

Multi-file progress example:

```text
lesson 01.mp4: 94.3% | 1.7 GiB/1.8 GiB | 8.2 MiB/s | ETA 13s | transferring
```

Recursive uploads preserve relative paths and filenames. A single-file
destination ending in `/` preserves the source filename; an explicit destination
filename renames it intentionally. Stdin spool names are not used as remote filenames.

File-operation exit codes: `0` success, `1` failed operation, `2` skipped files,
`130` canceled, `252` invalid/unsupported usage, `253` unavailable credentials.
Authorization behavior remains unchanged.

### Queries and migration

Baidu-specific queries retain their flags and JSON output:

```bash
bdpan info
bdpan quota
bdpan search --key report --dir /apps/demo
bdpan docs --parent-path /apps/demo
bdpan images --parent-path /apps/demo
bdpan meta --path /apps/demo/a.txt
bdpan metas --fsids '[123456789]'
bdpan metas-path --target '["/apps/demo/a.txt"]'
```

This is a breaking change, without legacy aliases:

| Before | After |
| --- | --- |
| `user info`, `user quota` | `info`, `quota` |
| `file list --dir /apps/demo` | `ls bd://apps/demo/` |
| `file list-all --path /apps/demo --recursion 1` | `ls bd://apps/demo/ --recursive` |
| `file upload --file a.txt --path /apps/demo/a.txt --yes` | `cp a.txt bd://apps/demo/a.txt` |
| JSON-based `file copy/move/rename/delete` | Positional `cp/mv/rm` |
| `file search/docs/images/meta/metas/metas-path` | Remove the `file` prefix |
| `file precreate/upload-part/create` | Removed; `cp` handles multipart upload |

### Configuration and development

Global `--app-id`, `--app-key`, `--secret-key`, and `--access-token` use
`BAIDU_APP_ID`, `BAIDU_APP_KEY`, `BAIDU_SECRET_KEY`, and `BAIDU_ACCESS_TOKEN`
as fallbacks. Nonempty flags take precedence. `--token-file` selects the token
file; `--timeout` defaults to `30s` for API requests and sets the minimum
automatically managed transfer idle budget. Existing access tokens do
not require application keys for file operations.

```bash
source <(bdpan completion bash)
source <(bdpan completion zsh)
bdpan completion fish | source
# PowerShell: bdpan completion powershell | Out-String | Invoke-Expression

go test ./...
go test -race ./...
go vet ./...
```

Tests use mock HTTP, never a real Netdisk account. Entry point: `cmd/bdpan`;
implementation: `internal/cli`.
