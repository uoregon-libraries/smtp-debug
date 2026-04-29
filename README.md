# SMTP Debug

A single-binary Go SMTP interceptor for debugging. It speaks just enough of the
protocol to accept mail from a client and optionally dump each message to a
semantic HTML file. It is a debug aid, not a real MTA. It's been written almost
exclusively by Claude Code.

Everything lives in `cmd/smtp-debug/main.go` as `package main`. There is no
library layer, there are no internal packages. It's super simple and arguably
still overengineered (putting the app under `cmd` was my idea, not AI's, and I
don't think I can defend that decision).

This project seems like kind of a terrible idea, but for us it's been very
handy to test emails in staging environments where end users need to verify
that what's going to be sent is what they want. Set up staging as if it's prod,
load prod users and data, and point your app to this instead of the real SMTP
server. Make sure you expose the SMTP HTML output to end users in some way.

If you're an expert in the SMTP protocol, you should probably build your own
thing, not use this. Otherwise, though, it can be pretty useful for testing
pretty much any email-enabled app.

## Build

Simply run `make`. This produces `bin/smtp-debug`.

## Usage

```
./bin/smtp-debug -port 2525 -out ./out
```

Flags:

- `-port`: port to listen on. Defaults to `25` (privileged).
- `-out`: directory to write captured messages to. Optional; if omitted,
  traffic is logged to stdout but no files are written.

Captured messages are written as `YYYY-MM-DD_HH-MM-SS_conn{N}_msg{M}.html`,
with `msgSeq` incrementing per message within a single connection.

## Tests

Claude has been kind enough to write some fairly comprehensive tests for us.
They have *not* been vetted, as this is truly a staging-only service, but they
appear at a glance to test very realistic SMTP communications.

```
go test ./...
```
