# FileStore Uploader

A command-line uploader for [FileStore.me](https://filestore.me):
pick a source folder and a remote destination folder, upload several files in
parallel with progress bars and transfer rates, and resume where you left off
if anything interrupts the transfer.

It replaces FTP upload, which FileStore has retired.

```
  FileStore · 4 files · 80.0 MB · 3 in parallel · → Movies

  file_a.mkv          █████████░░░░░░░░░░░  48%    9.5 MB / 20.0 MB   4.5 MB/s
  file_b.mkv          ██████████░░░░░░░░░░  52%   10.3 MB / 20.0 MB   4.7 MB/s
  file_c.mkv          █████████░░░░░░░░░░░  48%    9.5 MB / 20.0 MB   4.5 MB/s
  TOTAL               ███████░░░░░░░░░░░░░  37%   29.3 MB / 80.0 MB  13.7 MB/s
                      done 0/4 · failed 0 · elapsed 0:02 · ETA 0:03
```

## Install

Grab the archive for your platform from the
[latest release](../../releases/latest), unpack it, and run it — the binary has
no dependencies and no runtime to install. Downloads can be checked against the
`SHA256SUMS` file published with them:

```sh
shasum -a 256 -c SHA256SUMS --ignore-missing
```

### Building from source

Requires Go 1.25 or newer. A `go build` with an older toolchain installed
will fetch the right one automatically.

```sh
git clone https://github.com/matrog/filestore-uploader
cd filestore-uploader
```

```sh
make build              # produces bin/filestore
make install            # copies it to /usr/local/bin
```

Binaries for every platform, Windows included:

```sh
make all                # bin/filestore-{macos-arm64,macos-intel,windows.exe,linux}
```

On Windows you copy just the `.exe`; there is no runtime to install.

## Getting started

```sh
filestore setup         # paste the API key from https://filestore.me/?op=my_account
filestore check         # confirm the key, the account tier and the quota
filestore folders       # show the remote folder tree with ids
```

The key is stored in `~/.filestore.conf` with mode 0600. It can also be
supplied through the `FILESTORE_API_KEY` environment variable.

## Uploading

```sh
filestore up -from ~/Videos -to Movies -n 3 -r -include '*.mkv'
filestore up -to 12345 archive.zip report.pdf
filestore up -from ~/Scans -to "Work/2026" -create -links sent.txt
```

Options and paths can be mixed in any order.

| Option | Effect |
|---|---|
| `-from DIR` | source folder |
| `-to NAME\|ID` | destination: a name, a `Parent/Child` path, or a numeric id |
| `-n N` | parallel uploads (default 3) |
| `-r` | descend into subfolders |
| `-include GLOB` | filter by name, e.g. `'*.mp4'` |
| `-links FILE` | append the resulting links to a file |
| `-create` | create the destination folder when missing |
| `-state FILE` | resume file (default `.filestore-state.jsonl`) |
| `-no-resume` | ignore the state file and upload everything again |
| `-check-remote` | skip files already in the destination (default on) |
| `-retries N` | retries after a network failure (default 2) |
| `-utype TIER` | override the account tier: `prem` or `reg` |
| `-plain` | one line per file, for logs and scripts |

The display adapts to the terminal width, asking the kernel for it rather than
trusting `COLUMNS`, which shells do not export. Long names in numbered series
are shortened in the middle so the part that tells them apart stays visible.
Outside a terminal it switches to plain lines on its own.

## Not uploading twice

Two independent checks keep bytes from being sent for nothing.

**The destination folder is listed once before the transfer starts.** Anything
already there with the same name *and* size is skipped. This catches files put
there by other means, such as the website's own uploader. The destination is
the contract: a file sitting in another folder does not count as present, and
is uploaded.

**Every confirmed file is recorded immediately**, with an `fsync`, so an
interrupted run resumes by re-running the same command:

```sh
filestore up -from /Volumes/Archive -to backups/2026 \
  -state ~/archive-state.jsonl -links ~/archive-links.txt
```

Files are matched by absolute path and size, so a file changed since its upload
is uploaded again. A same-name-but-different-size file on the server is also
uploaded again and reported, because assuming it is the same file could
silently leave a truncated one in place.

When an upload fails, the server is asked whether the file arrived anyway
before anything is re-sent: a connection can die after the server received
everything, and re-sending gigabytes would only create a duplicate.

Links are appended as they arrive, not just at the end, so an interruption
never loses them. Each file is moved into the destination as soon as it
finishes, so a stopped run leaves nothing stranded in the root.

For a transfer of many hours on macOS, keep the machine awake and detach it
from the terminal:

```sh
caffeinate -i filestore up ... -plain > ~/upload.log 2>&1 &
```

The default state file lives in the current directory, so pass an absolute
path with `-state` for a long job.

## Verifying a finished transfer

```sh
filestore files -id 20940
```

Lists the folder's files with sizes and codes. For a split archive this is the
check that matters: every part has to be there for the set to be usable.

## How the upload works

It follows [XFileSharing Pro](https://xfilesharingpro.docs.apiary.io), the
engine behind filestore.me, with three corrections to the published blueprint.
They were each found the hard way, so they are worth recording.

**1. The upload session is not the API key.**
`GET /api/upload/server?key=<KEY>` returns the upload server *and* a single-use
`sess_id` as a top-level field:

```json
{"status":200,"result":"https://sx3.filestore.me/cgi-bin/upload.cgi",
 "sess_id":"io551m9js3sot2de","msg":"OK"}
```

The blueprint does not document that field and implies the API key doubles as
the session. Using the key instead fails with `uploads are not enabled for your
account type` — a misleading message that means the session was not recognised,
so the request counted as anonymous, not that the account is limited.

**2. The account tier travels in the query string.**
The size ceiling depends on `utype`, exactly as the browser sends it:

```
POST https://sxN.filestore.me/cgi-bin/upload.cgi?upload_type=file&utype=prem
```

As a form field it is ignored. Omitted entirely, the upload is anonymous and
capped at 10 MB; as a form field only, the server applies the registered tier
and its 100 MB cap. Either way a large file is refused *after* its whole body
has been sent, with `ERROR: Max filesize limit exceeded!`. The tier comes from
`/api/account/info` — `prem` only when the account really is premium — and
`-utype` overrides it. Below the ceiling a file goes up in a single POST; no
chunking is involved.

**3. Field names and types vary.**
The file code appears as `filecode`, `file_code` or `code` depending on the
endpoint, and numeric fields such as `fld_id` arrive as numbers where the
blueprint documents strings. Reading a single spelling yields empty values and
silently discards whole listings, so every variant is accepted.

One more detail that is not in the blueprint: the multipart body is assembled
by hand so an exact `Content-Length` can be declared. With `io.Pipe` Go would
use chunked encoding, which CGI scripts such as `upload.cgi` often reject.

A size rejection is final, so it is never retried, and once the ceiling is
known it is applied up front to the remaining files instead of sending each one
in full to be refused.

## Development

```sh
go test ./...
go vet ./...
make dist            # release archives in dist/, without touching bin/
```

### Releasing

Pushing a version tag builds every platform and publishes a GitHub Release with
the archives and their checksums attached:

```sh
git tag v1.0.0
git push origin v1.0.0
```

The workflow runs `go vet` and the tests first and stops if either fails, so a
broken build is never published. It can also be started by hand from the
Actions tab. The version reported by `filestore version` is stamped from the
tag at build time.

The tests cover the response shapes observed against the real server, including
non-JSON bodies, mixed types, every file-code spelling, and the errors that
must not be retried.

## License

MIT — see [LICENSE](LICENSE).
