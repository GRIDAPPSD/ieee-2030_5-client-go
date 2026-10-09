# Manual DERMS test

Two scripts run a simulated aggregator with two managed devices, and a
standalone device, against a local `sep2server`. You drive a flow
reservation from the command line and watch the DERMS tab.

| Script | Does |
|---|---|
| `start-aggregator.sh [start]` | checks the certificate files, fetches the CA, writes the aggregator's config, pairs the two managed devices, runs the aggregator in the foreground |
| `start-aggregator.sh reserve [--energy-wh N] [--power-w N] [--start-in-s N] [--duration-s N]` | writes the request file and signals the running aggregator to read it, with the given values in place of the config defaults (6000 Wh, 3000 W, start in 2400 s, 3600 s long). Energy is signed: positive charges the battery, negative discharges it, zero is refused. The script reports that the file was read, not that the request was accepted; the aggregator log says whether it was. A new `reserve` replaces the request being followed and withdraws it |
| `start-aggregator.sh withdraw` | asks the aggregator to withdraw the request it is following |
| `start-aggregator.sh unpair` | removes both management pairs; a pair already gone is fine. It deletes the pair for each managed device whoever the manager is, so it also removes a pair another manager created |
| `start-device.sh` | runs the standalone device (`solo-pv-1` by default; `DEVICE=` picks another that has a config in `sim/derms/`) |

Both stop on Ctrl-C. Nothing is written in the checkout: the built
`inverterclient`, the generated config, the CA, the request file and the pid
file all go in `DERMS_DIR` (default `~/derms-certs`, created private; a
directory inside the checkout is refused, and so is an existing one that you
do not own or that is writable by group or others).

| Variable | Default | Meaning |
|---|---|---|
| `DERMS_DIR` | `~/derms-certs` | certificates, generated files |
| `SEP2_URL` | `https://127.0.0.1:8443` | protocol listener |
| `ADMIN_URL` | `http://127.0.0.1:8444` | admin API; plain `http` is accepted only on `127.0.0.1`, `localhost` or `[::1]`, any other host needs `https` |
| `SEP2_ADMIN_KEY` | none, required | the server's admin key, sent as a Bearer header on stdin to curl and never printed. `start` and `unpair` refuse to run without it. `make run` in the server checkout sets it to `admin`, so `export SEP2_ADMIN_KEY=admin`. A key containing a double quote, backslash or newline is refused. `SEP2_ADMIN_UI_KEY` is not read |
| `PICKUP_WAIT_S` | 10 | how long `reserve` waits for the aggregator to take the request |

Needs `go`, `openssl`, `curl` and `jq`.

## Recipe 1: add an end device in the UI and save what a client needs

Do this four times, for `agg-1`, `agg-bat-1`, `agg-pv-1` and `solo-pv-1`.

1. Start the server with a short answer deadline, from the server checkout:
   `export SEP2_FLOW_RESERVATION_DEADLINE_SECONDS=1800` then `make run`.
2. Open the admin UI (`http://127.0.0.1:8444/ui`) and log in.
3. Certificates tab: Hardware Serial is the device name; PEN OID
   `1.3.6.1.4.1.40732.99`; Generate Device Cert.
4. Save the certificate and Save the private key. The files are named
   `<serial>.crt` and `<serial>.key`.
5. Devices tab, Add End Device: Description is the name, PIN `111115`, Add
   Device.
6. Move the files into `DERMS_DIR` (`mkdir -p -m 700 ~/derms-certs`).

The aggregator needs `agg-1.crt`, `agg-1.key`, `agg-bat-1.crt` and
`agg-pv-1.crt`; the two managed devices' keys are never used, their
certificates give the LFDI (first 40 hex digits of the SHA-256 of the DER
certificate, upper case). The standalone device needs `solo-pv-1.crt`,
`solo-pv-1.key` and `ca.crt`. `start-aggregator.sh` fetches the CA from the
running server into `DERMS_DIR/ca.crt`; for `start-device.sh` alone, use the
Certificates tab's Download CA certificate.

## The test

The "should show" column is the expected result from the design, not yet
checked against a running server.

| Step | You do | The DERMS tab should show |
|---|---|---|
| 1 | Recipe 1 four times | Devices: four EndDevices. Fleet: none |
| 2 | `start-aggregator.sh` | Fleet: one row for `agg-1`. The fleet counts the aggregator's own EndDevice, so it shows 3 devices, not 2 |
| 3 | `start-device.sh` in a second terminal | Fleet unchanged; `solo-pv-1` is reporting |
| 4 | `start-aggregator.sh reserve` in a third terminal, then read the aggregator log | Queue: one request, 6000 Wh, 3000 W. The log shows it was posted; a request the client refuses (a start too soon) is logged there and the script still reports the file as read |
| 5 | Grant request 1 at 4000 Wh and 2000 W, then wait for its start | The aggregator logs the grant; inside the interval `agg-bat-1` reports about 2000 W charging and the fleet power moves by that amount; delivered energy stops at 4000 Wh |
| 6 | Revise the grant, then cancel it | The battery follows the revision from its start; on cancel it returns to its baseline within one tick |
| 7 | `start-aggregator.sh reserve --energy-wh 4000 --power-w 2000` | A new request with those values. The aggregator withdraws the request it was following, so only answer one request before posting the next |
| 8 | Deny the request from step 7 | The aggregator logs a denial; nothing moves |
| 9 | Dispatch a 1000 W limit to `agg-pv-1` | Responses received, then started; reported power drops to 1000 W when the baseline is above it; completed at the end |
| 10 | Dispatch a 1000 W limit to `solo-pv-1` | The same, from the standalone process |
| 11 | `start-aggregator.sh reserve` for a fresh request, then `start-aggregator.sh withdraw` while it is pending | The aggregator logs the withdrawal of the request it is following. Withdraw needs a request pending, so post one first |
| 12 | Ctrl-C both scripts, then `start-aggregator.sh unpair` | Devices go idle; no `inverterclient` is left running |

### The trap

The server holds a pending request until its answer deadline but never past
the requested start. A request's start must therefore be later than the
server's answer deadline, or the server's fallback answers first. Step 1 of
Recipe 1 sets the deadline to 1800 s and the default `--start-in-s` is 2400.
If you lower the start, keep it above the deadline.

### When something does not happen

* `reserve` says "not picked up": the aggregator did not read the request
  file within `PICKUP_WAIT_S`. The binary may predate the reserve handler;
  stop the aggregator and run `start-aggregator.sh start` again, which
  rebuilds it.
* `reserve` or `withdraw` says "stale pid file", "not inverterclient" or
  "not the aggregator": the aggregator is not running, the pid was reused,
  or the process is another `inverterclient` such as the standalone device.
  Start the aggregator again.
* `start` or `unpair` says `SEP2_ADMIN_KEY` is not set: export the server's
  admin key (`admin` under `make run`).
* `start` stops with 409: another manager already holds that device. Run
  `unpair` from that manager first (which removes that manager's pair for
  the device, whoever it is).
