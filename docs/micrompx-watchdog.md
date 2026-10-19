# Local MicroMPX watchdog

`cmd/micrompx-watchdog` runs **on each MicroMPX decoder Pi**, not on a new middleware host. It uses the same Microsoft Graph client-credentials approach and credential-file fields as `zwfm-encoder` (without its `events` field), but is self-contained in this decoder repository. It does not import, install or start the encoder application. There is no SSH collector, database server, SQLite, Python, CGO or third-party Go module dependency; the watchdog uses only the Go standard library.

It observes and sends notifications only. It never restarts/stops/starts the decoder, sends signals to it, changes its settings, opens a playback device or injects test audio. The example service also has `Restart=no` for the watchdog itself.

Background: [decoder incident and evidence, 19 October 2026](https://github.com/oszuidwest/rpi-umpx-decoder/issues/23).

## What is monitored

- Every ten seconds, bounded `journalctl` reads for `micrompx.service`, including messages about the unit. Match `Buffer empty` and positive `samples silence played` independently of journal priority. Successful FEC recovery and zero-sample silence records do not open incidents.
- Independently read the configured **playback** PCM status under `/proc/asound`: service ownership, stream identity, `RUNNING` state, application sample pointer and hardware playback pointer. A stopped pointer over two consecutive comparisons opens/escalates a critical incident, even if no journal messages are emitted. A journal failure does not disable this kernel check.
- Read-only `systemctl show` checks service activity. A service that is not `active` or an explicitly non-running PCM is critical. Unavailable service/kernel information is unknown, not assumed to be a decoder fault or recovery.
- Optionally ping an existing external HTTPS heartbeat endpoint while the monitoring pipeline is current, the mail queue is not stuck and no mail credential failure remains latched. No new external monitoring host is needed.

ALSA counters show frame movement, **not audible content**. A decoder that keeps writing zeros/repeated data, DAC/analogue faults and RF/transmitter failures may need a physical output or off-air monitor. Ordinary MPX level is not sufficient to establish programme audio: pilot/RDS can remain present. This watchdog does not claim to detect every sub-second dropout with polling; the journal supplies event evidence between PCM polls.

See [Linux ALSA proc status](https://kernel.org/doc/html/v5.19/sound/designs/procfile.html) and [MicroMPX API/status documentation](https://download.thimeo.com/MicroMPX_manual.pdf). The MicroMPX web interface is deliberately not a prerequisite: it was unresponsive while decoding continued during the incident.

## Build and install manually

Requirements: Raspberry Pi OS/Linux with `journalctl`, `systemctl`, procfs and a readable PCM status file. The standard tools are used only in read-only modes. No Python interpreter or libsystemd development headers are required. These steps install a new optional watchdog; they do not modify the decoder configuration or restart it.

Build on a machine with the Go version in `go.mod`:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o micrompx-watchdog ./cmd/micrompx-watchdog
```

Build the watchdog explicitly; it is optional, and `install.sh` does not install or configure it.

On **each decoder Pi**, after transferring that binary and the example files:

```sh
sudo useradd --system --user-group --no-create-home --shell /usr/sbin/nologin micrompx-watchdog
sudo install -m 0755 micrompx-watchdog /usr/local/bin/micrompx-watchdog
sudo install -d -o root -g micrompx-watchdog -m 0750 /etc/micrompx-watchdog
sudo install -o micrompx-watchdog -g micrompx-watchdog -m 0600 \
  deploy/micrompx-watchdog.example.json /etc/micrompx-watchdog/config.json
sudo install -o micrompx-watchdog -g micrompx-watchdog -m 0600 \
  deploy/micrompx-watchdog-m365.example.json /etc/micrompx-watchdog/m365.json
sudo install -m 0644 deploy/micrompx-watchdog.service /etc/systemd/system/micrompx-watchdog.service
```

If the account already exists, inspect/reuse it instead of re-running `useradd`. Configure the location name, primary MPX output's PCM path and mail settings before enabling real mail. Use a **stable ALSA card ID**, not an assumed `card1` number. Select the primary MPX output, not the optional headphone output. Confirm that the status owner belongs to `micrompx.service` and that both counters advance under normal playback. Default `pcm_status` is the usual HiFiBerry DAC+ card ID; confirm it per installation with `cat /proc/asound/cards`.

```sh
sudo -u micrompx-watchdog /usr/local/bin/micrompx-watchdog -check
sudo systemctl daemon-reload
sudo systemctl enable --now micrompx-watchdog.service
sudo journalctl -u micrompx-watchdog.service -f
```

`-check` validates the watchdog configuration and, only when `send_mail` is `true`, the M365 file's shape and permissions; it cannot detect placeholder credentials. It does not poll hardware, write state, fetch an OAuth token or send mail. The service supplies journal-group membership. Test actual access and the service sandbox with `send_mail: false` first. Preview notifications are explicitly logged as **not sent**. Real mail queued before sending was turned off is retained, and does not block previews or the heartbeat, until sending is enabled again. A credential failure already latched by a dead letter still withholds the heartbeat; preview mode cannot clear it. After preview validation, stop **only the watchdog**, change `send_mail` to `true`, check the configuration, and manually start **only the watchdog** again. This does not restart MicroMPX. A currently open preview incident may next notify at its escalation/reminder rather than opening a second incident.

The config and M365 files must be regular files without group or other permission bits (for example 0600), so they must be owned by the service user. Under the example service's `ProtectSystem=strict`, the watchdog cannot write its configuration. Do not copy actual credentials, addresses, heartbeat tokens or raw local evidence into a public issue/PR.

## Configuration

Only `site` is required in preview mode; omitted values take the defaults in the table. Unknown or case-variant fields, duplicate JSON keys, out-of-range bounds and group/other-accessible files are rejected. `null` means the zero value, so it is only accepted where that is valid, such as `send_mail`. There are no automatic restart/reset options.

| Setting | Default | Meaning |
| --- | --- | --- |
| `site` | required | Single-line location name in notifications |
| `pcm_status` | HiFiBerry DAC+ playback status | Existing proc status file; never a `/dev/snd` device |
| `state_dir` | `/var/lib/micrompx-watchdog` | Dedicated private persistent directory, **not RAM disk**; another path needs `ReadWritePaths=` in the unit |
| `email_file` | empty | Required when `send_mail` is `true`; owner-only file using the encoder's Graph config fields |
| `send_mail` | `false` | Explicitly enable real M365 delivery |
| `heartbeat_url` | empty | Optional private HTTPS heartbeat URL; redirects are not followed |
| `poll_seconds` | 10 | Poll period, 10–60 seconds; subprocesses have bounded output and deadlines |
| `critical_count` / `critical_window_seconds` | 3 / 60 | Count **buffer-empty records**, not physical dropout episodes |
| `stalled_checks` | 2 | Consecutive unchanged-pointer comparisons before critical |
| `recovery_seconds` | 900 | Fresh, continuously observed clean interval before recovery |
| `reminder_seconds` | 900 | Minimum reminder interval; no additional reminder while this incident already has queued mail |
| `unknown_seconds` | 60 | Grace before a separate monitoring-unavailable email |
| `retention_days` | 7 | Local evidence age limit, enforced on new batches and hourly |
| `evidence_mib` | 64 | Evidence byte quota; oldest owned batches are pruned to fit |

The first impact opens one incident, with one warning or a bundled critical notification. At least `critical_count` buffer-empty records (three by default) within `critical_window_seconds` escalate it. These are provisional thresholds from one incident, not validated acoustic-duration measurements. Fallback activation is not matched: its log wording needs validation first.

An explicit closed/XRUN/non-running PCM or a service that is not `active` is critical, including while it restarts. The example unit starts after `micrompx.service`, but a PCM that opens late after boot can still raise a critical notice followed by a recovery. A changed stream/owner/boot establishes a fresh baseline. Missing telemetry, a stopped/unavailable journal reader, an unverified cursor overlap, suppressed messages or a truncated/backlogged read blocks recovery. A temporary monitoring gap starts the clean-observation period again. No data is never interpreted as audio recovery.

Journal and kernel files are observed at different instants, not an atomic hardware snapshot. Counter wrap/reopen must not trigger a false freeze. Procfs reads depend on a responsive kernel; a complete Pi/kernel/watchdog hang needs an external dead-man's-switch, as does total network/power failure.

## Continuity and durable evidence

The journal cursor remains opaque. Reads request only `MESSAGE` and the identity fields journalctl always adds, so large fields such as coredump data cannot exceed the line limit; a `MESSAGE` that journalctl encodes as bytes (control characters, invalid UTF-8) is decoded. Each saved bookmark also holds a SHA-256 fingerprint of that entry excluding `__CURSOR`. An inclusive `journalctl --cursor` read must return that overlap entry before new records are accepted as continuous. Cursor aliases can represent the same entry; **cursor string equality is not used as proof**. An absent/mismatched overlap yields unknown continuity and a bounded recent backfill, rather than silently claiming recovery. Fingerprinting is a conservative CLI-based continuity check, not an implementation of `sd_journal_test_cursor`. [systemd cursor documentation](https://www.freedesktop.org/software/systemd/man/257/sd_journal_get_cursor.html).

Without a saved bookmark, or after a continuity gap, only the last minute of journal is read; records from that minute that were already processed are counted again. Stored bookmarks resume from the last committed entry after restart. Reads process at most 512 new records per batch; a partial read is not current. Historical impact becomes a retrospective summary once caught up, not a storm of live alerts. If logs have already rotated/vacuumed or disappeared on reboot, lost history cannot be reconstructed. A gap is logged as a warning; it is mailed only if monitoring stays uncertain for `unknown_seconds`.

Full scoped messages are stored in private, content-addressed evidence batch files. Each batch is synced and renamed durably **before** the state cursor can advance. Cursor, incident and outbox are then committed together using a synced temporary file, atomic rename and directory sync. A crash may leave an unacknowledged batch to replay, but must not acknowledge unwritten evidence. Replayed identical batches are deduplicated. Corrupt/unsupported state is never silently replaced.

An exclusive OS lock prevents two watchdogs sharing the same directory. Each private directory has one `.watchdog.tmp` transaction file name; a file left by an interrupted write is removed and recreated by the next write. Evidence retention only removes owned hash-named batch files inside the dedicated evidence directory; age and quota pruning are logged. This is bounded operational retention, not an indefinite forensic archive. Export incident evidence promptly before retention expires and sanitize it before sharing. Files and directories it creates are 0600 and 0700. Each new journal batch is synced. The compact state is only rewritten when the cursor, incident, outbox or monitoring status changes, not on every quiet poll; the playback baseline and the unknown-monitoring grace period are kept in memory and rebuilt after a restart or a failed state write.

The outbox is capped at 128 entries. Once a mail has failed at least five attempts, a latest HTTP 400, 401, 403 or 404 from either the token or sendMail request moves it into `dead_letters` in `state.json`, allowing later mail to proceed. The error log includes `notification_id`, subject, HTTP status and attempt count. Dead letters retain the mail, final status and failure time; only the last 32 are kept, dropping the oldest. HTTP 408, 429, 5xx, transport errors and timeouts keep retrying indefinitely (at most 256 seconds apart, longer if `Retry-After` asks) and hold back later mail. Preview notifications are unaffected. Full queues or storage failures fail closed without advancing the journal cursor. With a full queue the watchdog keeps running and delivering: the heartbeat stops, and the journal backlog after the saved cursor is processed once mail drains. Without a saved cursor (first start or after a continuity gap) only the last minute is re-read, so older records can be missed while the queue stays full. A failed state or evidence commit during a poll stops the watchdog, allowing the external heartbeat to expire; failed mail-state updates and hourly retention passes are logged and retried. Recover disk/credentials and, after a stop, manually resume the watchdog. Never delete its state to hide a pending incident. State contains no M365 credentials. Local raw log messages can nevertheless contain sensitive operational data.

## M365 and independent failure detection

Use the existing Graph client-credentials approach and shared mailbox, with explicit `Mail.Send` scoping to the required mailbox. Do not assume an existing tenant-wide grant is safe to copy to every Pi. This local design **does distribute a mail credential to each site**: prefer separately revocable credentials/identities, least privilege, owner-only files and monitored expiry/rotation. A second secret on the same app is not a separate permission boundary. No extra application-read permission is requested merely for expiry checks.

Mail runs in an independent worker. Failed notifications remain durable and retry with backoff until accepted or moved to a dead letter under the five-attempt policy above. `Retry-After` seconds and HTTP dates are persisted with the queued mail (bounded to 24 hours). Each attempt has an outer 45-second deadline shared by token acquisition and sending; each HTTP request also has a 30-second timeout. HTTP redirects are never followed. Tokens are acquired per attempt and are never logged or persisted; upstream error bodies are not logged. Turning off real mail does not discard previously queued real notices. Delayed notifications are labelled `[DELAYED]`. Stable IDs help identify duplicates after acceptance followed by a crash; exactly-once email delivery is not promised. Graph `202` means accepted, not guaranteed mailbox delivery. See Microsoft's [client-credentials protocol](https://learn.microsoft.com/en-us/entra/identity-platform/v2-oauth2-client-creds-grant-flow) and [sendMail contract](https://learn.microsoft.com/en-us/graph/api/user-sendmail?view=graph-rest-1.0).

The optional heartbeat measures watchdog/pipeline health, **not whether programme audio is good**. A known decoder incident can therefore still have a healthy heartbeat while a critical mail is queued/sent. A stalled outbox older than five minutes or unavailable observations stop heartbeat pings. Dead-lettering HTTP 401/403 also persists `mail_credential_failed` in `state.json` and withholds the heartbeat, even when the outbox is empty or real mail is disabled. Only a later mail accepted by Graph clears this flag; previews, restarts and other failures cannot clear it. Dead-lettered HTTP 400/404 does not set the flag. Fix the credential or permission, restart only the watchdog so it reloads the M365 file (it is read once at startup; the restart keeps the flag), and wait for a later notification to be accepted before expecting heartbeat pings to resume. The existing unconditional heartbeat cron job must be disabled **manually** if it points to the same endpoint; otherwise it masks watchdog failure. The watchdog never edits cron or an existing decoder service.

A local program cannot email when its Pi has lost power/network access. Reuse the existing external heartbeat service for that case. If its notifications also depend exclusively on the same M365 path, this is not an independent delivery guarantee. Decide explicitly whether another alert route is needed; until then describe deployment as email-based best-effort monitoring, not guaranteed paging.

## Verification before production acceptance

```sh
go test -race ./internal/decodermonitor ./cmd/micrompx-watchdog
go test ./...
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ./cmd/micrompx-watchdog
```

Offline replay of the reviewed evidence ZIP (no network, hardware or mail):

```sh
MICROMPX_EVIDENCE_ZIP=/absolute/path/to/micrompx-evidence-2026-10-19-redacted.zip \
  go test -tags=integration -run TestIncidentEvidenceReplay -v ./internal/decodermonitor
```

Validate the installed systemd version, journal access/retention/rate limits, stable ALSA device identity and pointer progression under the actual service sandbox. Exercise queue persistence, monitor restart, missing cursors, command timeouts and unchanged-pointer snapshots in tests or a spare Pi. Do not freeze or restart an on-air decoder to test monitoring. Check the M365 permission scope and delivery/retry behaviour in a designated test mailbox before enabling production recipients.
