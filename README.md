# log-tools

`log-tools` 是一个面向 Linux/Unix 日志场景的 HTTP 检索服务。它封装
`grep` 和 `zgrep`，对外提供统一的搜索接口，用来在指定日志目录中按关键字
检索普通日志和压缩日志。

## 功能

- 启动时只需要指定日志根目录 `--log-dir`
- 通过 `POST /search` 接收关键字、时间范围、最大返回行数等参数
- `time_range` 基于文件修改时间（mtime）筛选目标文件，不再依赖文件名格式
- 搜索范围限制为日志根目录以及一级子目录，超过一级的更深层目录不会扫描
- 只允许一小部分安全的 grep 标志，避免把接口变成任意命令执行入口
- 搜索命令带超时控制，并返回实际执行的命令字符串，便于排查
- 支持 `SIGINT` / `SIGTERM` 优雅停机

## 构建

```bash
make build
```

或直接使用 Go：

```bash
go build -o log-tools .
```

## 运行

```bash
./log-tools --log-dir /var/log/myapp --addr :9999
```

启动参数：

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `--log-dir` | 必填 | 要检索的日志根目录 |
| `--addr` | `:9999` | HTTP 监听地址 |

## API

### `GET /health`

健康检查：

```json
{
  "status": "ok",
  "log_dir": "/var/log/myapp"
}
```

### `POST /search`

执行日志检索。

请求体：

| 字段 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `keyword` | string | 是 | 传给 `grep` / `zgrep` 的检索模式 |
| `time_range` | string | 否 | 相对时间范围，如 `"30m"`、`"1h"`、`"1d"`；只搜索修改时间在该范围内、且位于根目录或一级子目录中的文件 |
| `max_lines` | int | 否 | 最大返回行数，对应 `grep -m` |
| `tool` | string | 否 | `"grep"` 或 `"zgrep"`，默认 `"grep"` |
| `extra_flags` | []string | 否 | 额外安全标志，见下方白名单 |

允许的 `extra_flags`：

- `-i` / `--ignore-case`
- `-n` / `--line-number`
- `-c` / `--count`
- `-l` / `--files-with-matches`
- `-v` / `--invert-match`
- `-w` / `--word-regexp`
- `-x` / `--line-regexp`

成功响应：

```json
{
  "lines": [
    "/var/log/myapp/app.log:2026-04-02 10:00:05 ERROR disk full"
  ],
  "count": 1,
  "command": "grep -m 100 -- ERROR /var/log/myapp/app.log /var/log/myapp/api/app.log"
}
```

失败时会返回：

```json
{
  "error": "..."
}
```

## 示例

搜索最近 1 小时内修改过的日志文件中的 `ERROR`，最多返回 100 行：

```bash
curl -s -X POST http://localhost:9999/search \
  -H "Content-Type: application/json" \
  -d '{"keyword":"ERROR","time_range":"1h","max_lines":100}'
```

忽略大小写搜索最近 1 天内修改过的日志文件中的 `timeout`：

```bash
curl -s -X POST http://localhost:9999/search \
  -H "Content-Type: application/json" \
  -d '{"keyword":"timeout","time_range":"1d","extra_flags":["-i"]}'
```

在最近 7 天内修改过的压缩日志中搜索 `critical`：

```bash
curl -s -X POST http://localhost:9999/search \
  -H "Content-Type: application/json" \
  -d '{"keyword":"critical","tool":"zgrep","time_range":"7d"}'
```

## 测试

```bash
make test
```
