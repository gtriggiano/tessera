---
description: "What an enrichment is, and how to get the MaxMind databases that geo and asn read, keep them up to date with geoipupdate in Docker, in Kubernetes or in an image, configure the enrichments, check that tessera loads every release, use them in rules, and recognize the client software with ua."
---

# Enrich requests

An enrichment is a group of attributes that **tessera** derives from the request, such as the country or the autonomous system of the client address, or the browser that the `user-agent` header names. Every expression can read them, in a condition, an upstream header or a decision field, and tessera computes them only when an expression reads them, once per decision. This page describes the enrichments that tessera has today, `geo`, `asn` and `ua`. It shows how to get the files that `geo` and `asn` read from MaxMind, keep them up to date with MaxMind's `geoipupdate` in Docker, in Kubernetes or in an image, configure tessera to read them, check that it loads every new release, and use them in rules; then how to recognize the client software with `ua`, which reads no file.

Enrichments come after the client address in the [path of a request](./how-tessera-works.md#enrichments): `geo` and `asn` describe the address that [Set the client address](./client-address.md) chooses, `ua` the software that sent the request, and the [policy rules](./policy-rules.md) read them next to the request and the [sets](./sets.md).

## The enrichments

| Enrichment | What it says of the client address | Configured by |
| --- | --- | --- |
| [`geo`](../reference/expressions.md#geo) | Its country, continent, subdivisions, city, postal code, time zone and location. | [`enrichment.geo`](../reference/configuration.md#enrichment-geo) |
| [`asn`](../reference/expressions.md#asn) | Its autonomous system: its number and its organization. | [`enrichment.asn`](../reference/configuration.md#enrichment-asn) |

`ua` says what the `user-agent` header says of the client software:

| Enrichment | What it says of the client software | Configured by |
| --- | --- | --- |
| [`ua`](../reference/expressions.md#ua) | Its browser and operating system, with their versions, the kind of device, and whether it is a bot, with the name and the description of the bot. | [`enrichment.user_agent`](../reference/configuration.md#enrichment-user-agent) |

tessera reads `geo` and `asn` from files in the MaxMind DB format, which it loads into memory before it opens its listeners, and loads again when they change. `ua` reads no file: tessera analyzes the header of the request (see [Recognize the client software](#recognize-the-client-software)). An enrichment exists only when its block of `enrichment` configures it: an expression that reads one that the configuration does not have is a configuration error. The [configuration reference](../reference/configuration.md#enrichment) describes every key.

## Choose the databases

[`enrichment.geo`](../reference/configuration.md#enrichment-geo) reads a database with the schema of GeoIP2 or GeoLite2 City or Country, and [`enrichment.asn`](../reference/configuration.md#enrichment-asn) one with the schema of GeoLite2 ASN. MaxMind publishes the GeoLite2 databases free of charge, and the GeoIP2 databases for its customers. A database of another provider with the same schema works too.

| Database | Edition ID | What tessera reads |
| --- | --- | --- |
| GeoLite2 City | `GeoLite2-City` | With `enrichment.geo`: every attribute of `geo`. |
| GeoLite2 Country | `GeoLite2-Country` | With `enrichment.geo`: `geo.found`, `geo.country` and `geo.continent`. The subdivisions, the city, the postal code, the time zone and the location are empty or absent. |
| GeoLite2 ASN | `GeoLite2-ASN` | With `enrichment.asn`: every attribute of `asn`. |

The edition ID names the database for `geoipupdate`, and the file that it writes: `GeoLite2-City.mmdb`. With a GeoIP2 subscription, use the edition IDs of your databases, such as `GeoIP2-City`, in place of the GeoLite ones.

MaxMind releases the GeoLite2 City and Country databases every Tuesday and Friday, and the GeoLite2 ASN database every weekday (see [Download and update databases](https://support.maxmind.com/knowledge-base/articles/download-and-update-maxmind-databases)).

::: warning Read the license of the data
The GeoLite2 databases come under the [GeoLite End User License Agreement](https://www.maxmind.com/en/geolite/eula). Among its terms, you attribute your use of the data to MaxMind, you use each updated database promptly, and you destroy the older versions within 30 days of a release. MaxMind does not guarantee the accuracy of the data: a rule on a country or an autonomous system decides on what the database says, which can be wrong for an address.
:::

## Get an account and a license key

1. Sign up for a GeoLite account from the [GeoLite page](https://www.maxmind.com/en/geolite-free-ip-geolocation-data) of MaxMind.
2. In your account, [generate a license key](https://support.maxmind.com/knowledge-base/articles/generate-a-maxmind-license-key), and copy it: MaxMind shows it only once.
3. Note your [account ID](https://support.maxmind.com/knowledge-base/articles/find-your-maxmind-account-id).

`geoipupdate` downloads the databases with the account ID and the license key (see [Updating databases](https://dev.maxmind.com/geoip/updating-databases)). A GeoLite account can download 30 databases a day, and the requests that check for a new release do not count (see [Download and update databases](https://support.maxmind.com/knowledge-base/articles/download-and-update-maxmind-databases)).

::: danger Keep the license key secret
The license key authenticates the downloads of your account, as a password does. Keep it in the secrets of your platform, as the examples below do, never in the configuration of tessera, in an image or in a repository. tessera never reads it.
:::

## Keep the databases up to date

[`geoipupdate`](https://github.com/maxmind/geoipupdate) is the program of MaxMind that downloads the databases of your account and their new releases. It writes each database to a file named after its edition ID, such as `GeoLite2-City.mmdb`: it writes a new release to a temporary file in the same directory, then renames it over the file (see [its source](https://github.com/maxmind/geoipupdate/blob/v8.0.0/internal/geoipupdate/database/local_file_writer.go)).

That is how tessera expects a database to change. `tessera serve` checks the file of each database every [`enrichment.geo.refresh`](../reference/configuration.md#enrichment-geo-refresh) or [`enrichment.asn.refresh`](../reference/configuration.md#enrichment-asn-refresh), and the decisions after the first check that sees a new release read it, without a restart. A file that is not a valid database changes nothing: tessera keeps the copy in use.

::: warning Mount the directory, not the file
Give tessera the directory where `geoipupdate` writes. A mount of a single file, such as a Docker `--volume` of the file or a Kubernetes `subPath` mount, does not follow a new file renamed over it: tessera never sees the new releases.
:::

### In Docker

Run `geoipupdate` from its [Docker image](https://github.com/maxmind/geoipupdate/blob/v8.0.0/doc/docker.md) next to tessera, with a volume that both containers mount: `geoipupdate` writes to it, and tessera reads it.

```yaml [compose.yaml]
services:
  geoipupdate:
    image: ghcr.io/maxmind/geoipupdate:v8.0.0
    restart: unless-stopped
    environment:
      GEOIPUPDATE_ACCOUNT_ID_FILE: /run/secrets/GEOIPUPDATE_ACCOUNT_ID
      GEOIPUPDATE_LICENSE_KEY_FILE: /run/secrets/GEOIPUPDATE_LICENSE_KEY
      GEOIPUPDATE_EDITION_IDS: GeoLite2-ASN GeoLite2-City
      GEOIPUPDATE_FREQUENCY: "24"
    secrets:
      - GEOIPUPDATE_ACCOUNT_ID
      - GEOIPUPDATE_LICENSE_KEY
    volumes:
      - geoip:/usr/share/GeoIP
  tessera:
    image: ghcr.io/gtriggiano/tessera:{{VERSION}}
    restart: unless-stopped
    read_only: true
    volumes:
      - ./config.yaml:/etc/tessera/config.yaml:ro
      - geoip:/var/lib/geoip:ro
secrets:
  GEOIPUPDATE_ACCOUNT_ID:
    file: ./secrets/GEOIPUPDATE_ACCOUNT_ID.txt
  GEOIPUPDATE_LICENSE_KEY:
    file: ./secrets/GEOIPUPDATE_LICENSE_KEY.txt
volumes:
  geoip:
```

- `geoipupdate` writes to `/usr/share/GeoIP` in its container, the volume `geoip`, which tessera mounts read-only at `/var/lib/geoip`.
- `GEOIPUPDATE_FREQUENCY` is the number of hours between two runs of `geoipupdate`: with `24`, it checks for new releases once a day.
- The files `secrets/GEOIPUPDATE_ACCOUNT_ID.txt` and `secrets/GEOIPUPDATE_LICENSE_KEY.txt` hold your account ID and your license key.
- tessera listens for Envoy on port `9001` of the network of the project, at `tessera:9001`: add Envoy to the project, or publish the port, as your setup needs.

tessera does not start without its databases: download them once before you start the project. Without `GEOIPUPDATE_FREQUENCY`, or with `0`, `geoipupdate` runs once and exits:

```sh
docker compose run --rm -e GEOIPUPDATE_FREQUENCY=0 geoipupdate
docker compose run --rm tessera config validate
docker compose up -d
```

`tessera config validate` reads the configuration and the databases as `tessera serve` does, and prints `configuration valid`.

### In Kubernetes

Share an `emptyDir` volume between tessera and two containers of `geoipupdate`: an init container that downloads the databases once, before tessera starts, and a container that downloads every new release. Keep the account ID and the license key in a Secret, which the containers of `geoipupdate` read as files:

```sh
kubectl create secret generic maxmind --from-file=account-id=account-id.txt --from-file=license-key=license-key.txt
```

```yaml [deployment.yaml]
apiVersion: apps/v1
kind: Deployment
metadata:
  name: tessera
spec:
  replicas: 2
  selector:
    matchLabels:
      app: tessera
  template:
    metadata:
      labels:
        app: tessera
    spec:
      initContainers:
        - name: geoipupdate-first
          image: ghcr.io/maxmind/geoipupdate:v8.0.0
          env:
            - name: GEOIPUPDATE_ACCOUNT_ID_FILE
              value: /run/secrets/maxmind/account-id
            - name: GEOIPUPDATE_LICENSE_KEY_FILE
              value: /run/secrets/maxmind/license-key
            - name: GEOIPUPDATE_EDITION_IDS
              value: GeoLite2-ASN GeoLite2-City
          volumeMounts:
            - name: maxmind
              mountPath: /run/secrets/maxmind
              readOnly: true
            - name: geoip
              mountPath: /usr/share/GeoIP
      containers:
        - name: tessera
          image: ghcr.io/gtriggiano/tessera:{{VERSION}}
          ports:
            - containerPort: 9001
            - containerPort: 9090
          volumeMounts:
            - name: config
              mountPath: /etc/tessera
              readOnly: true
            - name: geoip
              mountPath: /var/lib/geoip
              readOnly: true
        - name: geoipupdate
          image: ghcr.io/maxmind/geoipupdate:v8.0.0
          env:
            - name: GEOIPUPDATE_ACCOUNT_ID_FILE
              value: /run/secrets/maxmind/account-id
            - name: GEOIPUPDATE_LICENSE_KEY_FILE
              value: /run/secrets/maxmind/license-key
            - name: GEOIPUPDATE_EDITION_IDS
              value: GeoLite2-ASN GeoLite2-City
            - name: GEOIPUPDATE_FREQUENCY
              value: "24"
          volumeMounts:
            - name: maxmind
              mountPath: /run/secrets/maxmind
              readOnly: true
            - name: geoip
              mountPath: /usr/share/GeoIP
      volumes:
        - name: config
          configMap:
            name: tessera-config
        - name: maxmind
          secret:
            secretName: maxmind
        - name: geoip
          emptyDir: {}
```

The ConfigMap `tessera-config` holds the configuration of tessera as `config.yaml`: the image reads `/etc/tessera/config.yaml`.

Each pod downloads the databases in its init container when it starts, and each of these downloads counts towards the daily limit of your account. With many replicas, or pods that restart often, let one CronJob of `geoipupdate` write to a PersistentVolumeClaim that every pod mounts read-only, if your cluster has a storage class with the access mode `ReadWriteMany`:

```yaml [cronjob.yaml]
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: geoip
spec:
  accessModes: [ReadWriteMany]
  resources:
    requests:
      storage: 1Gi
---
apiVersion: batch/v1
kind: CronJob
metadata:
  name: geoipupdate
spec:
  schedule: "0 6 * * *"
  concurrencyPolicy: Forbid
  jobTemplate:
    spec:
      template:
        spec:
          restartPolicy: OnFailure
          containers:
            - name: geoipupdate
              image: ghcr.io/maxmind/geoipupdate:v8.0.0
              env:
                - name: GEOIPUPDATE_ACCOUNT_ID_FILE
                  value: /run/secrets/maxmind/account-id
                - name: GEOIPUPDATE_LICENSE_KEY_FILE
                  value: /run/secrets/maxmind/license-key
                - name: GEOIPUPDATE_EDITION_IDS
                  value: GeoLite2-ASN GeoLite2-City
              volumeMounts:
                - name: maxmind
                  mountPath: /run/secrets/maxmind
                  readOnly: true
                - name: geoip
                  mountPath: /usr/share/GeoIP
          volumes:
            - name: maxmind
              secret:
                secretName: maxmind
            - name: geoip
              persistentVolumeClaim:
                claimName: geoip
```

Run the job once before you deploy tessera, with `kubectl create job --from=cronjob/geoipupdate geoipupdate-first`. Then, in the Deployment above, leave out the init container, the container of `geoipupdate` and the volume `maxmind`, and replace the `emptyDir` of the volume `geoip` with the claim, read-only:

```yaml [deployment.yaml]
- name: geoip
  persistentVolumeClaim:
    claimName: geoip
    readOnly: true
```

::: warning No ConfigMap, no subPath
A ConfigMap cannot hold the databases: its data cannot exceed 1 MiB (see [ConfigMaps](https://kubernetes.io/docs/concepts/configuration/configmap/)). And mount the volume of the databases whole: a `subPath` mount of a database file never shows the new releases.
:::

### In an image

You can also build an image of tessera with the databases inside, and roll out a new image for each release that you take:

```dockerfile [Dockerfile]
FROM ghcr.io/gtriggiano/tessera:{{VERSION}}
COPY GeoLite2-City.mmdb GeoLite2-ASN.mmdb /var/lib/geoip/
```

tessera runs as the user `65532`: the files must be readable by every user, as `geoipupdate` writes them. tessera reads the databases of the image when it starts, and a new release reaches it with the next rollout. The license of the GeoLite2 databases asks you to use each updated database promptly, and to destroy the older ones within 30 days of a release: build and roll out a new image at least as often.

## Configure the enrichments

Point [`enrichment.geo.database`](../reference/configuration.md#enrichment-geo-database) and [`enrichment.asn.database`](../reference/configuration.md#enrichment-asn-database) at the files in the directory that tessera mounts:

```yaml [config.yaml]
enrichment:
  geo:
    database: /var/lib/geoip/GeoLite2-City.mmdb
  asn:
    database: /var/lib/geoip/GeoLite2-ASN.mmdb
policy:
  default_action: allow
  rules:
    - name: hosting-networks
      action: deny
      when: asn.number in [64500, 64501]
    - name: outside-europe
      action: deny
      when: geo.found && geo.continent.code != "EU"
```

- tessera reads each database whole into memory: see [`enrichment.geo.database`](../reference/configuration.md#enrichment-geo-database) for the formats that it reads and the memory that a database takes.
- `geo` and `asn` look up the client address, `client.ip`. Behind a load balancer or a CDN, declare where tessera reads it: see [Set the client address](./client-address.md).
- A database that no rule, upstream header or decision field reads is loaded all the same, and [`tessera config validate`](../reference/cli.md#tessera-config-validate) warns about it.

## Check that tessera loads the databases

tessera reads each database before it opens its listeners. A file that is missing, cannot be read or is not a valid database stops `tessera serve` with status 2 and an error that names the key (see [`enrichment`](../reference/configuration.md#enrichment)), such as:

```text
tessera: enrichment.geo.database (line 3, column 15): the file does not exist
```

Once it runs, tessera writes a [`database_loaded`](../reference/logs.md#database-loaded) event for each database, with its type and its build time, at startup and for each new release that a check finds:

```text
ts=2026-11-23T00:06:15.659Z level=info msg=database_loaded database=geo database_type=GeoLite2-City build_time=2026-11-17T10:00:00Z duration_ms=58.112
ts=2026-11-23T00:06:15.659Z level=info msg=database_loaded database=asn database_type=GeoLite2-ASN build_time=2026-11-17T10:00:00Z duration_ms=10.294
```

A check that fails, because the file disappeared or is not a valid database, writes [`database_load_failed`](../reference/logs.md#database-load-failed) once, and tessera keeps the copy in use. The [metrics of the enrichments](../reference/metrics.md#enrichments) follow the same checks:

- [`tessera_enrichment_database_refreshes_total`](../reference/metrics.md#tessera-enrichment-database-refreshes-total) counts each check by its result;
- `time() - tessera_enrichment_database_build_timestamp_seconds` is the age of the data that tessera reads, from the build time of the database (see [`tessera_enrichment_database_build_timestamp_seconds`](../reference/metrics.md#tessera-enrichment-database-build-timestamp-seconds)).

For example, alert on these expressions: MaxMind releases each GeoLite2 database at least twice a week, so a database older than a week means that its updates stopped.

```text
increase(tessera_enrichment_database_refreshes_total{result=~"rejected|error"}[1h]) > 0
time() - tessera_enrichment_database_build_timestamp_seconds > 7 * 86400
```

## Use the enrichments

### In rules

A condition of the [policy rules](./policy-rules.md) reads the record of the client address through the attributes of [`geo`](../reference/expressions.md#geo) and [`asn`](../reference/expressions.md#asn), [sets](./sets.md) of type `asn` hold AS numbers, and sets of type `area` hold geographic areas, in which a rule looks up `geo.location` (see [Match a location in an area](./sets.md#match-a-location-in-an-area)):

```yaml [config.yaml]
enrichment:
  geo:
    database: /var/lib/geoip/GeoLite2-City.mmdb
  asn:
    database: /var/lib/geoip/GeoLite2-ASN.mmdb
sets:
  hosting:
    type: asn
    values: [64500, 64501]
policy:
  default_action: deny
  rules:
    - name: hosting-networks
      description: Networks of hosting providers.
      action: deny
      when: sets.hosting.contains(asn.number)
    - name: unknown-addresses
      description: Addresses that the database does not locate.
      action: deny
      mode: audit
      when: '!geo.found'
    - name: lazio
      description: The region of the office, Lazio in Italy.
      action: allow
      when: geo.country.code == "IT" && geo.subdivisions.exists(s, s.code == "62")
    - name: european-union
      action: allow
      when: geo.country.in_eu
```

[`geo`](../reference/expressions.md#geo) describes what the attributes hold for an address that the database does not have, and [When tessera computes an enrichment](../reference/expressions.md#when-tessera-computes-an-enrichment) what happens without a client address and when a lookup fails. To decide on the addresses that the database does not locate, write a rule on `!geo.found`, in `audit` mode first, as above. A rule on a region tests the country too, as `lazio` does: the database writes the code of a subdivision without its country, and other countries use the same codes (see [`geo`](../reference/expressions.md#geo)).

### In upstream headers and decision fields

An upstream header or a decision field can carry what an enrichment says of the client: the upstream receives the country in `x-client-country`, and the [`decision`](../reference/logs.md#decision) events have a field `country`.

```yaml [config.yaml]
enrichment:
  geo:
    database: /var/lib/geoip/GeoLite2-City.mmdb
log:
  decision_fields:
    country: geo.country.code
upstream:
  headers:
    x-client-country: geo.country.code
```

[Send headers to the upstream](./responses-and-logs.md#send-headers-to-the-upstream) shows what the upstream can trust in such a header, and [Log the decisions](./responses-and-logs.md#log-the-decisions) what a decision field can read.

## Recognize the client software

The `user-agent` header names the software that sent the request: a browser on a phone, a crawler, a script. [`ua`](../reference/expressions.md#ua) reads it, once [`enrichment.user_agent`](../reference/configuration.md#enrichment-user-agent) turns the analysis on. It has no file to get or to keep up to date: an empty block turns it on with its defaults.

```yaml [config.yaml]
enrichment:
  user_agent: {}
sets:
  blocked_bots:
    type: string
    values: [OpenAI GPT, Claude Bot, Bytespider]
policy:
  default_action: allow
  rules:
    - name: blocked-bots
      description: Bots that the site does not serve.
      action: deny
      when: sets.blocked_bots.contains(ua.bot.name)
    - name: checkout-bots
      description: Crawlers and scripts on the checkout pages.
      action: deny
      when: request.path.startsWith("/checkout") && ua.is_bot
log:
  decision_fields:
    bot: ua.bot.name
upstream:
  headers:
    x-client-device: ua.device
```

- tessera analyzes the header when an expression reads `ua`, and keeps the records of recent headers in a cache that every decision shares, whose number of entries [`enrichment.user_agent.cache_entries`](../reference/configuration.md#enrichment-user-agent-cache-entries) sets. Raise it when the share of lookups that find their header in the cache stays low (see [`tessera_enrichment_user_agent_cache_lookups_total`](../reference/metrics.md#tessera-enrichment-user-agent-cache-lookups-total)).
- A request without the header, or with one that tessera does not recognize, is not an error: the names and the versions are empty, `ua.device` is `unknown` and `ua.is_bot` is `false`. A rule that denies bots does not deny it: to decide on requests without the header, test it, as `!("user-agent" in request.headers)` does.
- The names that `ua.browser.name`, `ua.os.name` and `ua.bot.name` take are in the [expressions reference](../reference/expressions.md#ua), with how tessera recognizes a bot and where `ua.bot.description` comes from.
- To decide on a group of bots, put their names in a `string` [set](./sets.md), such as `blocked_bots` above, and look `ua.bot.name` up in it: take the names from the [bot names reference](../reference/bot-names.md), which lists every name that tessera gives, with its description.

::: warning Do not trust the header
A client writes its own `user-agent` header, and can write anything, such as the header of a browser or of a well-known crawler. Use `ua` to treat the clients that say what they are, such as crawlers and scripts, and not to let a client through because of its header: to allow a crawler, check its address too, such as with a [set](./sets.md) of the networks that its operator publishes.
:::

## Try a change

[`tessera eval`](../reference/cli.md#enrichments) reads the databases of the configuration, and shows the attributes of `geo`, `asn` and `ua` that the rules read for an address and a `user-agent` header, and the upstream headers of an allowed request:

```sh
tessera eval --config config.yaml --ip 192.0.2.10 --header 'user-agent: curl/8.7.1'
```

In the Docker project above, run it in the image: `docker compose run --rm tessera eval --ip 192.0.2.10`.
