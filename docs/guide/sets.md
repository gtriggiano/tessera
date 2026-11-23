---
description: "How to declare the sets of tessera, use them in rules, update their files safely, read them from a Kubernetes ConfigMap, choose what an unavailable set answers, and try a change with tessera eval."
---

# Use sets

A set is a named list of networks, autonomous system numbers or strings that the rules of **tessera** read, kept apart from the rules. This page shows how to declare a set, use it in a rule, update its file while tessera runs, read it from a Kubernetes ConfigMap, choose what a set that tessera cannot keep up to date answers, and try a change before you make it. The [configuration reference](../reference/configuration.md#sets) describes every key.

Sets come after the [enrichments](./enrichments.md) in the [path of a request](./how-tessera-works.md#sets): the [policy rules](./policy-rules.md) look values up in them, such as the client address or its autonomous system.

## Declare a set

Each key of [`sets`](../reference/configuration.md#sets) is the name of a set, with its [type](../reference/configuration.md#sets-name-type) and one source:

- [`values`](../reference/configuration.md#sets-name-values) writes the values in the configuration file. Use it for short lists that change with the configuration, such as the networks of an office.
- [`file`](../reference/configuration.md#sets-name-file) reads them from a file, one value per line. Use it for lists that change on their own, such as networks blocked during an incident, or that another system writes.

```yaml [config.yaml]
sets:
  office:
    type: cidr
    values:
      - 203.0.113.0/24
      - 2001:db8:cafe::/48
  blocked:
    type: cidr
    file: blocked.txt
```

```text [blocked.txt]
# Networks blocked during an incident.
198.51.100.0/24
2001:db8:bad::/48
192.0.2.66          # one address
```

A relative path is relative to the directory of the configuration file. tessera reads every set before it opens its listeners: a missing file, or an invalid line, stops the startup with an error that names the key and the line of the file. Run [`tessera config validate`](../reference/cli.md#tessera-config-validate) to check the configuration and its files without starting.

## Use a set in a rule

A condition reads the set `blocked` as `sets.blocked`, and looks a value up with [`contains`](../reference/expressions.md#sets). The argument has the type of the set: an address, such as `client.ip`, for a `cidr` set, an `int`, such as [`asn.number`](../reference/expressions.md#asn), for an `asn` set, and a `string`, such as `request.host`, for a `string` set.

```yaml [config.yaml]
sets:
  blocked:
    type: cidr
    file: blocked.txt
  office:
    type: cidr
    values: [203.0.113.0/24]
  internal_hosts:
    type: string
    values: [admin.example.com, grafana.example.com]
policy:
  default_action: allow
  rules:
    - name: blocked-networks
      action: deny
      when: sets.blocked.contains(client.ip)
    - name: internal-hosts
      action: deny
      when: sets.internal_hosts.contains(request.host) && !sets.office.contains(client.ip)
```

A decision reads every set as it was when the decision started: a file that changes during a decision does not change it.

## Update the file of a set

`tessera serve` checks the file of each set every [`sets.<name>.refresh`](../reference/configuration.md#sets-name-refresh), and uses its new content from the first check that sees it, without a restart. A check reads the state of the file, and reads its content when the file has changed or was modified too recently to trust its state (see [`sets.<name>.refresh`](../reference/configuration.md#sets-name-refresh)). New content that is invalid changes nothing: tessera keeps the copy in use, counts the check in [`tessera_set_refreshes_total`](../reference/metrics.md#tessera-set-refreshes-total) with `result="rejected"`, and reports it with [`set_load_failed`](../reference/logs.md#set-load-failed).

Write the new content to a temporary file in the same directory, then rename it over the file of the set:

```sh
cp blocked.txt blocked.txt.new
echo '203.0.113.99' >> blocked.txt.new
mv blocked.txt.new blocked.txt
```

A rename replaces the file at once: a check sees either the old file or the new one, whole. Editors that save to a new file and rename it work the same way.

::: warning A file written in place can be read empty or half written
A program that writes the file in place, such as `cp` over `blocked.txt`, a shell redirection `> blocked.txt` or an editor that saves into the same file, first empties the file, then writes it. A check at that moment reads part of the new content, or none:

- a file of 0 bytes is a valid, empty set: tessera applies it until the next check, `refresh` later, and for that time a block list blocks nothing;
- a file cut at the end of a line is valid too: tessera uses the values read until the next check, and the values not yet written are missing from the set;
- a file cut in the middle of a line usually has an invalid last line: the check rejects it, and a later check reads the whole file.

Write a temporary file and rename it over the file of the set, as above.
:::

tessera tells a changed file from the same one by its device, inode, size and modification time, and reads a recently modified file again at each check, so that a rewrite of the same size in place is seen even on a file system that records the modification time to the second. A program that writes new content of the same size in place and then sets the modification time back to an old one hides the change: rename a new file instead.

## Read a set from a Kubernetes ConfigMap

Mount the ConfigMap as a volume, and point [`sets.<name>.file`](../reference/configuration.md#sets-name-file) at the file in the mount:

```yaml [deployment.yaml]
spec:
  containers:
    - name: tessera
      volumeMounts:
        - name: sets
          mountPath: /etc/tessera/sets
          readOnly: true
  volumes:
    - name: sets
      configMap:
        name: tessera-sets
```

```yaml [config.yaml]
sets:
  blocked:
    type: cidr
    file: /etc/tessera/sets/blocked.txt
```

In the volume, each file is a symbolic link to `..data/<name>`, and `..data` a link to a directory of the current version. When the ConfigMap changes, the kubelet writes the new version to a new directory and replaces the link `..data` at once. tessera follows the links, and sees the new file at its next check after the kubelet has updated the volume. A change to another file of the ConfigMap leaves this file with the same content: tessera keeps its copy, and writes no event.

::: warning A `subPath` mount never updates
The kubelet does not update a file mounted with `subPath`: tessera keeps reading the content it had when the container started. Mount the whole ConfigMap, as above.
:::

## Choose what an unavailable set answers

By default, a set read from a file is never unavailable: when its file disappears or becomes invalid, tessera keeps the last copy it loaded, for as long as it runs. [`sets.<name>.max_staleness`](../reference/configuration.md#sets-name-max-staleness) limits the age of that copy: once the last successful check of the file is older than `max_staleness`, the set is unavailable, until a check succeeds again. The age grows only while the checks fail. Choose a value several times `refresh`, and longer than the time you need to repair the file: with `refresh: 10s`, `max_staleness: 5m` makes a set unavailable five minutes after its last successful check.

[`sets.<name>.on_unavailable`](../reference/configuration.md#sets-name-on-unavailable) sets what `contains` answers on an unavailable set:

| `on_unavailable` | `contains` | Use it for |
| --- | --- | --- |
| `fail`, the default | Fails: the condition is indeterminate, with the cause `set_unavailable`. | A list whose loss must not let requests through, such as a block list. |
| `match` | Answers `true`. | A block list for which you prefer to deny everyone rather than let a blocked client through. |
| `no_match` | Answers `false`. | A list whose loss can be ignored, such as networks to watch in a rule in `audit` mode. |

::: danger The default denies the requests that depend on an unavailable set
With `on_unavailable: fail`, a condition that depends on an unavailable set is indeterminate: a `deny` rule whose condition depends on the set denies every request that reaches it, and an `allow` rule whose condition depends on it does not match, so a later rule or the default action decides. Requests that the set would have let through are denied, with `reason=error` in [`decision`](../reference/logs.md#decision) (see [Indeterminate conditions](../reference/expressions.md#indeterminate-conditions)). `&&` and `||` still decide without the set when their other operand is enough.
:::

The metrics and the events warn before a set read from a file becomes unavailable, from the first failed check. The metrics of the checks exist only for the sets read from a file, since a set of `values` is never checked again:

- [`tessera_set_refreshes_total`](../reference/metrics.md#tessera-set-refreshes-total) grows with `result="rejected"` or `result="error"` at each failed check, and [`set_load_failed`](../reference/logs.md#set-load-failed) reports the first one;
- `time() - tessera_set_last_refresh_success_timestamp_seconds` is the age of the copy, which grows towards `max_staleness`;
- [`tessera_set_available`](../reference/metrics.md#tessera-set-available) becomes `0`, and [`set_unavailable`](../reference/logs.md#set-unavailable) is logged, when the set becomes unavailable; [`set_available`](../reference/logs.md#set-available) when it is available again.

For example, alert on these expressions, with an age below the `max_staleness` of your sets:

```text
increase(tessera_set_refreshes_total{result=~"rejected|error"}[5m]) > 0
time() - tessera_set_last_refresh_success_timestamp_seconds > 120
tessera_set_available == 0
```

## Try a change

[`tessera eval`](../reference/cli.md#tessera-eval) shows how the rules decide a request, with the sets of the configuration. `--set` replaces the content of a set for the evaluation, without reading its file, so you can try the content of a file before you write it:

```sh
tessera eval --config config.yaml --set blocked=198.51.100.0/24,203.0.113.99 --ip 203.0.113.99
```

`--set blocked=` makes the set empty. [Sets](../reference/cli.md#sets) in the CLI reference shows the output.
