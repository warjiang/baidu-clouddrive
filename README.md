# bdpan

Baidu Netdisk Open Platform command-line client.

[中文](#中文) | [English](#english)

## 中文

`bdpan` 是一个基于百度网盘开放平台 API 的命令行工具，支持 OAuth
授权、用户信息查询、文件检索、文件管理和分片上传。

### 构建

需要 Go 1.25 或更高版本。

```bash
make
```

构建完成后，当前平台的可执行文件位于：

```text
./output/bdpan
```

查看版本和帮助：

```bash
./output/bdpan --version
./output/bdpan --help
./output/bdpan file --help
./output/bdpan file upload --help
```

指定构建版本：

```bash
make VERSION=v0.1.0
```

构建 macOS、Linux 和 Windows 的 amd64/arm64 版本：

```bash
make build-all
```

### 配置凭证

推荐通过环境变量配置百度开放平台应用凭证，避免敏感信息进入 shell
历史记录：

```bash
export BAIDU_APP_ID='你的 AppID'
export BAIDU_APP_KEY='你的 AppKey'
export BAIDU_SECRET_KEY='你的 SecretKey'
```

所有凭证也可以通过全局参数传入：

```bash
./output/bdpan \
  --app-id '...' \
  --app-key '...' \
  --secret-key '...' \
  auth device-login
```

### 获取访问令牌

推荐使用设备码登录：

```bash
./output/bdpan auth device-login
```

命令会输出验证地址、用户码和二维码地址。完成浏览器授权后，访问令牌会自动
保存到用户配置目录：

- macOS：`~/Library/Application Support/bdpan/token.json`
- Linux：`$XDG_CONFIG_HOME/bdpan/token.json`，通常为
  `~/.config/bdpan/token.json`
- Windows：`%AppData%\bdpan\token.json`

令牌文件权限为 `0600`。可以通过 `--token-file` 指定其他位置：

```bash
./output/bdpan --token-file ./token.json auth device-login
```

也可以直接设置访问令牌，此时不会读取令牌文件：

```bash
export BAIDU_ACCESS_TOKEN='你的 Access Token'
```

刷新已保存的令牌：

```bash
./output/bdpan auth refresh
```

或者显式提供刷新令牌：

```bash
./output/bdpan auth refresh --refresh-token '你的 Refresh Token'
```

授权码模式也可用：

```bash
./output/bdpan auth url \
  --redirect-uri 'https://example.com/callback'

./output/bdpan auth exchange \
  --code '授权码' \
  --redirect-uri 'https://example.com/callback'
```

### 常用命令

| 功能 | 命令 |
| --- | --- |
| 查询用户信息 | `./output/bdpan user info` |
| 查询网盘容量 | `./output/bdpan user quota` |
| 列出目录 | `./output/bdpan file list --dir /` |
| 搜索文件 | `./output/bdpan file search --key report --dir /` |
| 递归列出文件 | `./output/bdpan file list-all --path /apps/your-app --recursion 1` |
| 查询单个文件元数据 | `./output/bdpan file meta --path /apps/your-app/a.txt` |
| 按 fs_id 查询元数据 | `./output/bdpan file metas --fsids '[123456789]'` |
| 按路径批量查询元数据 | `./output/bdpan file metas-path --target '["/apps/your-app/a.txt"]'` |

API 响应以 JSON 输出到标准输出，可以直接交给 `jq`：

```bash
./output/bdpan file list --dir / | jq '.list'
```

### 上传文件

普通上传应使用 `file upload`。它会自动完成预创建、分片上传和文件创建：

```bash
./output/bdpan file upload \
  --file ./a.txt \
  --path /apps/your-app/a.txt \
  --yes
```

同名文件处理方式由 `--rtype` 控制：

| 值 | 行为 |
| --- | --- |
| `1` | 自动重命名 |
| `2` | 内容不同时自动重命名 |
| `3` | 覆盖 |

`file precreate`、`file upload-part` 和 `file create` 是手工控制分片上传流程的
底层命令。通常不需要直接使用，可通过以下命令查看参数：

```bash
./output/bdpan file precreate --help
./output/bdpan file upload-part --help
./output/bdpan file create --help
```

### 文件管理

以下命令会修改网盘内容，因此必须显式传入 `--yes`：

```text
file copy
file move
file rename
file delete
file upload
file precreate
file upload-part
file create
```

`copy`、`move`、`rename` 和 `delete` 通过 `--filelist` 接收百度文件管理 API
要求的 JSON 数组：

```bash
./output/bdpan file delete \
  --filelist '[{"path":"/apps/your-app/old.txt"}]' \
  --yes
```

执行前可以查看具体参数：

```bash
./output/bdpan file copy --help
./output/bdpan file move --help
./output/bdpan file rename --help
./output/bdpan file delete --help
```

### 全局参数

| 参数 | 环境变量 | 说明 |
| --- | --- | --- |
| `--app-id` | `BAIDU_APP_ID` | 应用 AppID |
| `--app-key` | `BAIDU_APP_KEY` | 应用 AppKey |
| `--secret-key` | `BAIDU_SECRET_KEY` | 应用 SecretKey |
| `--access-token` | `BAIDU_ACCESS_TOKEN` | 直接使用访问令牌 |
| `--token-file` | 无 | 指定令牌文件 |
| `--timeout` | 无 | HTTP 超时时间，默认 `30s` |

命令行参数优先于环境变量。错误信息写入标准错误，API JSON 写入标准输出。
按下 Ctrl+C 会取消正在进行的请求。

### Shell 自动补全

```bash
# Bash
source <(./output/bdpan completion bash)

# Zsh
source <(./output/bdpan completion zsh)

# Fish
./output/bdpan completion fish | source

# PowerShell
./output/bdpan completion powershell | Out-String | Invoke-Expression
```

### 开发

```bash
make test
go vet ./cmd/bdpan ./internal/cli
```

项目入口位于 `cmd/bdpan`，命令实现位于 `internal/cli`。

## English

`bdpan` is a command-line client for the Baidu Netdisk Open Platform API. It
supports OAuth authorization, user and quota queries, file search and
management, and multipart uploads.

### Build

Go 1.25 or later is required.

```bash
make
```

The executable for the current platform is written to:

```text
./output/bdpan
```

Check the version and available commands:

```bash
./output/bdpan --version
./output/bdpan --help
./output/bdpan file --help
./output/bdpan file upload --help
```

Set a build version:

```bash
make VERSION=v0.1.0
```

Build amd64 and arm64 binaries for macOS, Linux, and Windows:

```bash
make build-all
```

### Configure credentials

Environment variables are recommended so secrets do not remain in shell
history:

```bash
export BAIDU_APP_ID='your AppID'
export BAIDU_APP_KEY='your AppKey'
export BAIDU_SECRET_KEY='your SecretKey'
```

Credentials can also be passed as global flags:

```bash
./output/bdpan \
  --app-id '...' \
  --app-key '...' \
  --secret-key '...' \
  auth device-login
```

### Obtain an access token

Device-code login is the recommended flow:

```bash
./output/bdpan auth device-login
```

The command prints a verification URL, user code, and QR-code URL. After
authorization, the token is saved under the user configuration directory:

- macOS: `~/Library/Application Support/bdpan/token.json`
- Linux: `$XDG_CONFIG_HOME/bdpan/token.json`, usually
  `~/.config/bdpan/token.json`
- Windows: `%AppData%\bdpan\token.json`

The token file is created with `0600` permissions. Use `--token-file` to choose
another location:

```bash
./output/bdpan --token-file ./token.json auth device-login
```

Alternatively, provide an access token directly:

```bash
export BAIDU_ACCESS_TOKEN='your access token'
```

Refresh a saved token:

```bash
./output/bdpan auth refresh
```

Or provide the refresh token explicitly:

```bash
./output/bdpan auth refresh --refresh-token 'your refresh token'
```

The authorization-code flow is also available:

```bash
./output/bdpan auth url \
  --redirect-uri 'https://example.com/callback'

./output/bdpan auth exchange \
  --code 'authorization code' \
  --redirect-uri 'https://example.com/callback'
```

### Common commands

| Task | Command |
| --- | --- |
| Show user information | `./output/bdpan user info` |
| Show quota information | `./output/bdpan user quota` |
| List a directory | `./output/bdpan file list --dir /` |
| Search for files | `./output/bdpan file search --key report --dir /` |
| List files recursively | `./output/bdpan file list-all --path /apps/your-app --recursion 1` |
| Get file metadata | `./output/bdpan file meta --path /apps/your-app/a.txt` |
| Get metadata by fs_id | `./output/bdpan file metas --fsids '[123456789]'` |
| Get metadata by path | `./output/bdpan file metas-path --target '["/apps/your-app/a.txt"]'` |

API responses are written as JSON to standard output and can be piped into
tools such as `jq`:

```bash
./output/bdpan file list --dir / | jq '.list'
```

### Upload files

Use `file upload` for normal uploads. It performs precreation, multipart
upload, and file creation automatically:

```bash
./output/bdpan file upload \
  --file ./a.txt \
  --path /apps/your-app/a.txt \
  --yes
```

Use `--rtype` to control name conflicts:

| Value | Behavior |
| --- | --- |
| `1` | Rename automatically |
| `2` | Rename when the content differs |
| `3` | Overwrite |

`file precreate`, `file upload-part`, and `file create` expose the low-level
multipart upload flow. They are normally unnecessary:

```bash
./output/bdpan file precreate --help
./output/bdpan file upload-part --help
./output/bdpan file create --help
```

### Manage files

Commands that modify Netdisk content require an explicit `--yes`:

```text
file copy
file move
file rename
file delete
file upload
file precreate
file upload-part
file create
```

The `copy`, `move`, `rename`, and `delete` commands accept the JSON array
required by Baidu's file-management API through `--filelist`:

```bash
./output/bdpan file delete \
  --filelist '[{"path":"/apps/your-app/old.txt"}]' \
  --yes
```

Inspect each command before running a modifying operation:

```bash
./output/bdpan file copy --help
./output/bdpan file move --help
./output/bdpan file rename --help
./output/bdpan file delete --help
```

### Global flags

| Flag | Environment variable | Description |
| --- | --- | --- |
| `--app-id` | `BAIDU_APP_ID` | Application AppID |
| `--app-key` | `BAIDU_APP_KEY` | Application AppKey |
| `--secret-key` | `BAIDU_SECRET_KEY` | Application SecretKey |
| `--access-token` | `BAIDU_ACCESS_TOKEN` | Use an access token directly |
| `--token-file` | None | Select a token file |
| `--timeout` | None | HTTP timeout, default `30s` |

Command-line flags take precedence over environment variables. Errors are
written to standard error, while API JSON is written to standard output.
Pressing Ctrl+C cancels an in-progress request.

### Shell completion

```bash
# Bash
source <(./output/bdpan completion bash)

# Zsh
source <(./output/bdpan completion zsh)

# Fish
./output/bdpan completion fish | source

# PowerShell
./output/bdpan completion powershell | Out-String | Invoke-Expression
```

### Development

```bash
make test
go vet ./cmd/bdpan ./internal/cli
```

The executable entry point is in `cmd/bdpan`; the command implementation is
in `internal/cli`.
