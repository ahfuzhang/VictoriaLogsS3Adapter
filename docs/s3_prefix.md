# `-s3.prefix` Prefix Template

Both [victoria_logs_s3_backup](victoria_logs_s3_backup.md) and [victoria_logs_s3_restore_svr](victoria_logs_s3_restore_svr.md) use `-s3.prefix` to specify the prefix of S3 object keys. The prefix is a template that supports two kinds of placeholders:

| Placeholder | Meaning |
|---|---|
| `{VAR}` | Replaced with the value of environment variable `VAR`. If the variable is unset or empty, the program fails at startup. |
| `${time format}` | Replaced with the time the backup started (UTC). See below for how to write the time format. |

After expansion, leading and trailing `/` are removed and consecutive `/` are merged into one. The full key of a backup object is `<expanded prefix>/<YYYYMMDD>.tar`, where `YYYYMMDD` in the file name is the date (UTC day) of the partition being backed up.

## Time format

The content of `${...}` is a **free combination** of the following 6 fields:

| Field | Meaning | Output |
|---|---|---|
| `YYYY` | Year | 4 digits, e.g. `2026` |
| `MM` | Month | 2 digits, `01`–`12` |
| `DD` | Day | 2 digits, `01`–`31` |
| `HH` | Hour (24-hour clock) | 2 digits, `00`–`23` |
| `MI` | Minute | 2 digits, `00`–`59` |
| `SS` | Second | 2 digits, `00`–`59` |

Rules:

* Fields can be in any order, and you can use only some of them, e.g. `${YYYYMMDD}`, `${DD-MM-YYYY_HH_MI}`, `${HHMI}`.
* Fields can be separated by `-`, `_`, `.`, or concatenated directly with no separator.
* Fields are case-sensitive and must be uppercase.
* **`MM` is month; write minutes as `MI`.** For example, `${YYYYMMDD_HHMISS}` means "year-month-day_hour-minute-second"; in `${YYYYMMDD_HHMMSS}`, `MM` appears twice, which is an error.
* Each field may appear at most once within one `${...}`, and there must be at least one field.
* Time is always expanded in **UTC**, using the time the current backup started, not the date of the partition being backed up.
* A template may contain multiple `${...}`. To organize by date directories, use multiple placeholders, e.g. `${YYYY}/${MM}/${DD}`. `/` cannot be written inside `${...}`.

## Examples

Assume `namespace=ns1`, `env=prod`, the backup started at `2026-11-05 00:15:30 UTC`, and the partition being backed up is `20261104`:

| `-s3.prefix` | Generated object key |
|---|---|
| `victoria_logs/{namespace}/{env}` | `victoria_logs/ns1/prod/20261104.tar` |
| `victoria_logs/{env}/${YYYYMMDD_HHMISS}` | `victoria_logs/prod/20261105_001530/20261104.tar` |
| `victoria_logs/{env}/${DD-MM-YYYY_HH_MI}` | `victoria_logs/prod/05-11-2026_00_15/20261104.tar` |
| `victoria_logs/{env}/${YYYYMMDD}` | `victoria_logs/prod/20261105/20261104.tar` |
| `victoria_logs/{env}/${YYYY}/${MM}/bk_${DD.HH}` | `victoria_logs/prod/2026/11/bk_05.00/20261104.tar` |

On the command line, wrap the template in single quotes so the shell doesn't expand `${...}` as a shell variable:

```bash
victoria_logs_s3_backup \
  -s3.prefix='victoria_logs/{namespace}/{env}/${YYYYMMDD_HHMISS}' \
  ...
```

## Format errors

If the content of `${...}` violates the rules above, both tools report an error at startup and exit (exit code 1) without starting a backup or serving. For example:

| Usage | Reason for the error |
|---|---|
| `${YYYYMMDD_HHMMSS}` | `MM` appears twice; write minutes as `MI` |
| `${yyyymmdd}` | Fields must be uppercase |
| `${YYYY-MM-DDTHH}` | `T` is not a valid field or separator |
| `${YYYY/MM/DD}` | `/` cannot be written inside `${...}`; write `${YYYY}/${MM}/${DD}` |
| `${namespace}` | Not a time format; environment variables are written `{namespace}` (no `$`) |
| `${-}`, `${}` | No time fields at all |

Example error message:

```
invalid -s3.prefix: invalid time placeholder ${YYYYMMDD_HHMMSS} in template "a/${YYYYMMDD_HHMMSS}": MM (month) is used more than once (MM means month; write minutes as MI)
```

## How backups are located during restore

`victoria_logs_s3_restore_svr` must use the **same** `-s3.prefix` as the backup.

* If the template has no time placeholder: it reads `<expanded prefix>/<YYYYMMDD>.tar` directly.
* If the template has time placeholders: it first lists all objects under the fixed prefix before the first time placeholder, keeps the objects whose key matches the whole template (time parts match numbers of the corresponding number of digits according to the format), and then picks the most recently modified one. So if a day's partition was backed up multiple times, the restore uses the latest copy; other objects that share the fixed prefix but have a different format are ignored.
