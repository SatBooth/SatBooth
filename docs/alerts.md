# Alerts

Every log line whose message starts with `ALERT:` is a condition a person must act on (the rules for each payment method list them,
rail by rail). With an `alerts` block, SatBooth also queues each one and posts it to the operator's webhook, so a monitoring
system can page someone. The log line is always written, unchanged, whatever the queue does. Without the block,
nothing is queued and the lines are only logged. Records logged before the store opens and the pair stamp passes are
only logged.

    "alerts": { "url": "https://monitor.example.com/satbooth-alert", "secret_file": "/etc/satbooth/alerts.secret" }

The block follows the rules of a `webhooks` entry. Its URL must differ from every endpoint's URL, and its secret from
every endpoint's secret as an HMAC key (a secret that is an endpoint's secret followed by zero bytes is the same key,
and is refused too): whoever runs the alert receiver must not be able to sign merchant webhooks. It is an outbound
destination of the public host: the egress firewall must allow the host of `alerts.url`. SatBooth posts JSON with the
`SatBooth-Signature` header, signed with the alert secret as webhooks are (see [Webhooks](webhooks.md)):

    {"id":"<32 hex digits>","type":"alert","message":"ALERT: ...","fields":{},"dropped":0,"repeats":1,"created_at":1700000000}

- **`fields`** holds only the line's attributes that are on a fixed list of keys, the keys the `ALERT` lines log, and
  have the key's shape:
  - ids of 32 lowercase hex digits: `order`, `order_id`, `refund_id`, `event`;
  - integers from 0 to 2^53: `count`, `step`, `from_height`, `to_height`, `unused`, `passes`;
  - a signed integer from −2^31 to 2^31−1: `code` (an RPC error code);
  - one word of `[a-z][a-z0-9_]*`, 1 to 40 characters: `reason`, `method`, `result`, `low`, `high`;
  - a webhook endpoint's name, 1 to 64 characters of `[A-Za-z0-9._-]`: `endpoint`;
  - exactly 3 uppercase letters: `asset`, `currency`.

  Integers are JSON numbers, everything else strings, and the keys are sorted. Any other attribute (`providers` among
  them), or a value of the wrong shape, is dropped, and the number dropped is the payload's top-level `dropped`. A
  payload never carries a key, secret, address, invoice, amount or text from a peer.
- **Repeats are coalesced** by hour. An alert's id is the SHA-256 of its message, its fields and its UTC hour, cut to
  32 hex digits, so the same alert in the same hour is one alert, and the next hour is a new one. `repeats` counts
  every occurrence in the hour, the first one included, at most 100. An alert already delivered is not sent again
  when it repeats: the payload carries `repeats` as it was when it was sent. Use `id` to deduplicate.
- **A refused start's alerts are kept.** An alert logged during a start that is then refused (after the pair stamp
  passed) is stored, and delivered within its 24-hour retry window by a later start that runs. If SatBooth stays down
  longer than that, the alert still gets one last attempt when SatBooth next runs, up to 8 days (24 hours plus 7 days)
  after it was logged, and the give-up line if that attempt fails; an alert older than that is not sent.
- **Delivery** follows the webhooks' retry policy (10 seconds, doubling to 1 hour, inside 24 hours). An alert given up
  is logged as `alert delivery abandoned after 24 hours`.
- **A webhook endpoint's abandoned log line is not lost to a crash.** The line `ALERT: webhook delivery abandoned after
  24 hours` is logged at least once for every event abandoned for an endpoint, also across a crash or a restart (it can
  repeat once after a crash), and once per run. See [webhooks](webhooks.md). What is durable is the log line. The copy
  for the operator's alert webhook follows the queue's rules below: it is queued in memory, so a crash right after the
  line, or a full queue, can lose it (a repeat in another hour is a separate alert). The alert deliverer's own give-up
  line is not made durable: losing it never loses an alert, only the line that says the operator's receiver was down for
  24 hours.
- **At most 1,000 alerts are stored per UTC day.** The count is kept in the store, so a restart does not reset it.
  Past that, SatBooth logs `alert queue full; alerts are logged only until tomorrow` once a day.
- **A full in-memory queue loses an alert, not a log line.** The handler offers each alert to a channel of 256
  entries that one goroutine drains into the store. When the channel is full, the alert is counted and SatBooth logs
  `alert queue overflow` with the count: this line is the only sign that alerts were lost.
- **The alert machinery never raises an alert.** Everything the alert queue and its deliverer log, including a failed
  alert write and the give-up line, carries no `ALERT:` prefix.
