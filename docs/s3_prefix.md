# `-s3.prefix` 前缀模板

[victoria_logs_s3_backup](victoria_logs_s3_backup.md) 和 [victoria_logs_s3_restore_svr](victoria_logs_s3_restore_svr.md) 都通过 `-s3.prefix` 指定 S3 对象 key 的前缀。前缀是一个模板，支持两种占位符：

| 占位符 | 含义 |
|---|---|
| `{VAR}` | 替换为环境变量 `VAR` 的值。变量未设置或为空时，程序启动报错。 |
| `${时间格式}` | 替换为备份开始的时间（UTC），时间格式的写法见下文。 |

展开后会去掉首尾的 `/`，连续的 `/` 会合并为一个。备份对象的完整 key 为 `<展开后的前缀>/<YYYYMMDD>.tar`，其中文件名里的 `YYYYMMDD` 是被备份的分区日期（UTC 日）。

## 时间格式

`${...}` 里由下面 6 个字段**自由组合**而成：

| 字段 | 含义 | 输出 |
|---|---|---|
| `YYYY` | 年 | 4 位，例如 `2026` |
| `MM` | 月 | 2 位，`01`–`12` |
| `DD` | 日 | 2 位，`01`–`31` |
| `HH` | 时（24 小时制） | 2 位，`00`–`23` |
| `MI` | 分 | 2 位，`00`–`59` |
| `SS` | 秒 | 2 位，`00`–`59` |

规则：

* 字段的顺序随意，也可以只用其中几个，例如 `${YYYYMMDD}`、`${DD-MM-YYYY_HH_MI}`、`${HHMI}`。
* 字段之间可以用 `-`、`_`、`.` 分隔，也可以不加分隔符直接相连。
* 字段区分大小写，必须大写。
* **`MM` 是月，分钟要写成 `MI`。** 例如 `${YYYYMMDD_HHMISS}` 是“年月日_时分秒”；`${YYYYMMDD_HHMMSS}` 中 `MM` 出现了两次，会报错。
* 同一个 `${...}` 里每个字段最多出现一次，并且至少要有一个字段。
* 时间一律按 **UTC** 展开，取的是本次备份开始的时间，不是被备份分区的日期。
* 一个模板里可以出现多个 `${...}`。需要按日期分目录时，就写成多个占位符，例如 `${YYYY}/${MM}/${DD}`。`/` 不能写在 `${...}` 里面。

## 示例

假设 `namespace=ns1`、`env=prod`，备份开始于 `2026-11-05 00:15:30 UTC`，备份的分区是 `20261104`：

| `-s3.prefix` | 生成的对象 key |
|---|---|
| `victoria_logs/{namespace}/{env}` | `victoria_logs/ns1/prod/20261104.tar` |
| `victoria_logs/{env}/${YYYYMMDD_HHMISS}` | `victoria_logs/prod/20261105_001530/20261104.tar` |
| `victoria_logs/{env}/${DD-MM-YYYY_HH_MI}` | `victoria_logs/prod/05-11-2026_00_15/20261104.tar` |
| `victoria_logs/{env}/${YYYYMMDD}` | `victoria_logs/prod/20261105/20261104.tar` |
| `victoria_logs/{env}/${YYYY}/${MM}/bk_${DD.HH}` | `victoria_logs/prod/2026/11/bk_05.00/20261104.tar` |

命令行中请用单引号把模板括起来，防止 shell 把 `${...}` 当作 shell 变量展开：

```bash
victoria_logs_s3_backup \
  -s3.prefix='victoria_logs/{namespace}/{env}/${YYYYMMDD_HHMISS}' \
  ...
```

## 格式错误

只要 `${...}` 里的内容不符合上面的规则，两个工具都会在启动时报错并退出（退出码 1），不会开始备份或提供服务。例如：

| 写法 | 错误原因 |
|---|---|
| `${YYYYMMDD_HHMMSS}` | `MM` 出现两次；分钟应写成 `MI` |
| `${yyyymmdd}` | 字段必须大写 |
| `${YYYY-MM-DDTHH}` | `T` 不是合法的字段或分隔符 |
| `${YYYY/MM/DD}` | `/` 不能写在 `${...}` 里，请写成 `${YYYY}/${MM}/${DD}` |
| `${namespace}` | 不是时间格式；环境变量应写成 `{namespace}`（没有 `$`） |
| `${-}`、`${}` | 没有任何时间字段 |

错误信息示例：

```
invalid -s3.prefix: invalid time placeholder ${YYYYMMDD_HHMMSS} in template "a/${YYYYMMDD_HHMMSS}": MM (month) is used more than once (MM means month; write minutes as MI)
```

## 恢复时如何查找备份

`victoria_logs_s3_restore_svr` 必须使用与备份时**相同的** `-s3.prefix`。

* 模板里没有时间占位符：直接读取 `<展开后的前缀>/<YYYYMMDD>.tar`。
* 模板里有时间占位符：先列出第一个时间占位符之前的固定前缀下的所有对象，筛出 key 与整个模板匹配的对象（时间部分按格式匹配相应位数的数字），再取其中最后修改的那个。因此同一天分区备份过多次时，恢复使用最新的一份；共用固定前缀、但格式不同的其他对象会被忽略。
