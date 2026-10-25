# Cross-integration test: Cost Center revokes a Keyguard key

A project-scoped Cost Center alert with a tiny limit revokes the Keyguard key of the same
name after one Muse Spark 1.3 call.

**Before you start:** Chrome is open with the Meta Keyguard extension reloaded, and
`./keyguard doctor` passes.

## 1. Turn on Keyguard's Cost Center check

```bash
cd ~/Sites/meta-keyguard
./keyguard cost-center on
```

## 2. Create the key

```bash
./keyguard create kgtest-spark --ttl 1h
```

## 3. Create the alert

The alert is scoped to the project `kgtest-spark` and revokes keys matching `kgtest-spark`.

```bash
cd ~/Sites/meta-costcenter
./muse-cost alert set --project kgtest-spark --monthly-usd 0.01 --revoke-keys kgtest-spark
```

Open the Meta Cost Control Center.

```sh
./muse-cost show
```

## 4. Spend in that project with the key

The workspace folder name becomes the Cost Center project.

```bash
mkdir -p /tmp/kgtest-spark
./keyguard get kgtest-spark | muse exec --api-key-stdin --provider meta \
  --model muse-spark-1.3 --workspace /tmp/kgtest-spark --disable-approval "Say hi"
```

A call usually costs more than a cent. If this prints `ok` instead of `exceeded`, run the
`muse exec` command again.

```bash
~/Sites/meta-costcenter/muse-cost alert status --json \
  | python3 -c "import json,sys; print([a['level'] for a in json.load(sys.stdin) if a['project'] == 'kgtest-spark'])"
```

## 5. Check the result (allow up to a minute)

```bash
cd ~/Sites/meta-keyguard
./keyguard status                  # kgtest-spark is gone; the alert is reported as exceeded
./keyguard log -n 5                # cost-center-revoked ... kgtest-spark
./keyguard create kgtest-spark     # refused while the alert is exceeded
```

To see the same result in the dashboard, run `~/Sites/meta-costcenter/muse-cost show` and
open **Alerts**. The alert reads "Keyguard revoked kgtest-spark at …".

## 6. Clean up

```bash
./keyguard cost-center off
```

Then delete the `kgtest-spark` alert on the dashboard's **Alerts** page.
