# nginx-log-geo-enricher

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)
[![Go Version](https://img.shields.io/badge/go-%3E%3D1.26-blue.svg)](https://go.dev/)

[中文版](./README.zh.md) | [English](./README.md)

Enrich raw JSON nginx access logs in real time without relying on nginx modules. The tool tails the source log, looks up geolocation data by client IP, appends it to each JSON log entry, and writes the enriched JSON logs to an output file.

## Features

- **Real-time tailing** - Continuously tails log files like `tail -F`, with automatic switching after logrotate rotation
- **IP geolocation lookup** - Uses the [ip2region](https://github.com/lionsoul2014/ip2region) database, with IPv4 and IPv6 support
- **Synchronized output rotation** - When the input log rotates by date, such as `accesslog.log-20260630`, the output file rotates with the same date suffix
- **Rotated file retention** - Optionally removes expired rotated output files by retention days to prevent unbounded disk growth; disabled by default
- **Checkpoint resume** - Resumes from the previous position after restart, avoiding data loss and duplicate processing within a small tolerance
- **Controlled memory usage** - Caps the IP lookup cache at 1 million entries and clears it automatically when exceeded to avoid OOM under abnormal traffic
- **Graceful shutdown** - Handles SIGINT/SIGTERM and drains buffered data before exiting
- **Fault tolerance** - Writes failed line parses to a separate error log without blocking the main pipeline; checkpoint progress is not advanced if flush fails
- **Error isolation** - Records raw lines that fail JSON parsing in `*_err.log` to keep normal logs clean
- **Field value cleanup** - Optionally removes interfering characters from selected field values, such as brackets in `X-Forwarded-For`, to avoid tokenization issues in downstream log platforms

## Quick Start

### Build

```bash
go build -o nginx-log-geo-enricher main.go
```

### Run

```bash
./nginx-log-geo-enricher \
  -input  /alidata/logs/nginx/accesslog.log \
  -output /alidata/logs/nginx/accesslog_geo.log \
  -v4db   /data/ip2region_v4.xdb \
  -v6db   /data/ip2region_v6.xdb \
  -field  remote \
  -geofield geo
```

## Command-line Options

| Option | Default | Description |
|--------|---------|-------------|
| `-input` | `/alidata/logs/nginx/accesslog.log` | Path to the nginx access log file |
| `-output` | `/alidata/logs/nginx/accesslog_geo.log` | Output log path with geolocation data |
| `-v4db` | ip2region_v4.xdb | IPv4 database path |
| `-v6db` | ip2region_v6.xdb | IPv6 database path |
| `-field` | `remote` | JSON field containing the IP address |
| `-geofield` | `geo` | Field name used to store geolocation data in the output JSON |
| `-checkpoint` | `<output directory>/.nginx-geo-enricher.checkpoint` | Checkpoint file path |
| `-clean-fields` | Empty; disabled | Comma-separated field names whose values should be cleaned, such as `real_ip,x_forwarded_for` |
| `-clean-chars` | `[]` | Character set to remove |
| `-retention-days` | `0`; no cleanup | Retention days for rotated output files. Values `<=0` disable cleanup |

## How It Works

```text
  accesslog.log --tail -F--> Parse JSON --> Extract IP --> ip2region lookup --> Append geo field --> accesslog_geo.log
                                         |
                                         `-- Parse failure --> Record _error/_raw --> accesslog_geo_err.log
```

1. Continuously reads the input file in `tail -F` mode, including automatic switching after logrotate rotation.
2. Parses each line as JSON and extracts the configured IP field.
3. Looks up the province, city, and ISP for the IP through ip2region.
4. Appends the geolocation data to the JSON object and writes it to the output file.
5. For lines that fail JSON parsing, writes `_error` with the failure reason and `_raw` with the original content to the error log.
6. Flushes output and advances the checkpoint every 500 processed lines or when the channel is idle.

### Geolocation Field Format

```text
IPv4: continent|country|province|city|district|ISP -> outputs "province city ISP"
IPv6: country|province|city|district|ISP|country code -> outputs "province city ISP"
```

- Private, loopback, or link-local addresses -> `内网IP`
- Invalid IP format -> `非法IP`
- Lookup failure -> `查询失败:<reason>`
- Missing IP field -> `无IP字段`

### Input JSON Example

```json
{"remote": "120.229.45.112", "time": "2026-06-30T10:00:00Z", "status": 200}
```

### Output JSON Example

```json
{"geo":"广东深圳移动","remote":"120.229.45.112","status":200,"time":"2026-06-30T10:00:00Z"}
```

### Error Log Format

Lines that fail JSON parsing are not discarded. They are written to `*_err.log` in the same directory as the output file, such as `accesslog_geo_err.log`, with the error reason and original line content:

```json
{"_error":"JSON parse failed: invalid character ...","_raw":"original log content"}
```

> The error log also participates in checkpoint flushing and log rotation, sharing the same lifecycle as the normal output file.

## Field Value Cleanup

Some fields, such as `http_x_forwarded_for`, may contain maliciously injected bracketed values:

```json
{"real_ip": "[ranip], 36.137.176.36, 118.178.15.113"}
```

When uploaded to log platforms such as SLS, brackets may cause tokenization issues. After cleanup is enabled, the configured characters are automatically removed from selected fields.

### Usage

```bash
# Clean the real_ip field and remove the default square brackets
./nginx-log-geo-enricher -clean-fields real_ip

# Clean multiple fields and customize the characters to remove
./nginx-log-geo-enricher -clean-fields "real_ip,http_x_forwarded_for" -clean-chars "[] "
```

### Result

```text
Before cleanup: {"real_ip": "[ranip], 36.137.176.36", "geo": "广东深圳移动"}
After cleanup:  {"real_ip": "ranip, 36.137.176.36", "geo": "广东深圳移动"}
```

> **Note**: Cleanup is disabled when `-clean-fields` is empty, with zero overhead and full backward compatibility. Cleanup only applies to string fields; non-string fields are skipped.

## Log Rotation Behavior

### Input File Rotation

The program detects logrotate rotation by inode and automatically switches to the new file from the beginning.

### Synchronized Output Rotation

When input rotation is detected, the output file is rotated at the same time:

```text
accesslog.log-20260630          (original input file renamed by logrotate)
accesslog.log                   (new input file)

accesslog_geo.log-20260630      (output file renamed with the same date)
accesslog_geo.log               (new output file)
accesslog_geo_err.log-20260630  (error log rotated at the same time)
accesslog_geo_err.log           (new error log file)
```

If rotation happens multiple times on the same day, which is rare, a numeric suffix such as `.1` or `.2` is appended automatically to avoid overwriting existing files.

### Rotated File Retention Cleanup

Use `-retention-days` to automatically clean up expired rotated output files and prevent unbounded disk growth:

```bash
# Keep only rotated output files from the last 7 days
./nginx-log-geo-enricher -retention-days 7
```

Cleanup behavior:

- **Trigger timing**: Runs once on startup, and again after each output file rotation.
- **Cleanup scope**: Only removes rotated files corresponding to the main output file and error output file, such as `accesslog_geo.log-*` and `accesslog_geo_err.log-*`. Files currently being written are not affected.
- **Retention rule**: Uses the date suffix in rotated filenames, `YYYYMMDD`. Files with dates `>= today - retention-days + 1` are kept; older files are removed. Files without a valid date suffix are skipped and will not be removed accidentally.
- **Default behavior**: When `-retention-days` is omitted or set to `<=0`, cleanup is completely disabled, with zero overhead and full backward compatibility.

> **Note**: Cleanup is based on the date in the filename rather than file modification time. Rotated files generated by this program use the controlled `YYYYMMDD` format. Compressed historical files with date suffixes, such as `accesslog_geo.log-20260630.gz`, are also included in date-based cleanup.

## Checkpoint Recovery

On exit, the program saves a checkpoint containing the input file inode and read offset. After restart, behavior depends on checkpoint state:

| Condition | Behavior |
|-----------|----------|
| Inode matches | Continue reading from the previous offset without loss or duplication |
| Inode does not match because rotation happened during downtime | Locate the old file by inode, read the remaining data, then start from the beginning of the new file |
| Old file has been deleted | Tail the new file directly; a small amount of data may be lost |
| No checkpoint file | Start from the end of the file and process only new data |

## Dependencies

- Go >= 1.26
- [ip2region v2.0](https://github.com/lionsoul2014/ip2region) - xdb-format IP address database

## License

MIT
