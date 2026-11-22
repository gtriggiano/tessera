---
description: "The expression language of the rule conditions: attributes and their types, functions, sets, operators, indeterminate conditions, errors and limits."
---

# Expression language reference

**tessera** reads the condition of each rule, [`policy.rules[].when`](./configuration.md#policy-rules-when), as an expression of the [Common Expression Language](https://cel.dev/) (CEL). This page lists the attributes that an expression reads, the functions and operators it can use, the sets it can look values up in, what happens when it cannot be evaluated, and its limits. [Write rules](../guide/write-rules.md) shows how to combine conditions into a policy.

## Conditions

A condition is an expression whose result is a bool:

```text
request.host == "api.example.com" && client.ip.inCidr("192.0.2.0/24")
```

tessera compiles and type-checks every condition when it reads the configuration, so that `tessera config validate` and `tessera serve` stop on an unknown attribute, a wrong type or a result that is not a bool, at its position in the file (see [`policy.rules[].when`](./configuration.md#policy-rules-when)). The expression language is CEL as its [language definition](https://github.com/google/cel-spec/blob/master/doc/langdef.md) describes, with the attributes and functions of this page; no extension library of CEL is available.

## Attributes

| Attribute | Type | Content |
| --- | --- | --- |
| `request.host` | `string` | The host of the request, from its `:authority` or `Host` header, in lowercase and without the port. |
| `request.method` | `string` | The method, in uppercase: `GET`, `POST`. |
| `request.path` | `string` | The path, up to the first `?`. |
| `request.query` | `string` | What follows the first `?` of the path, without it; empty when there is none. |
| `request.scheme` | `string` | The scheme, in lowercase: `http` or `https`. |
| `request.headers` | `map(string, string)` | The headers, by name in lowercase. |
| `request.id` | `string` | The value of the `x-request-id` header, empty without it. |
| `client.ip` | `tessera.IP` | The client address, read from the source that [`client_ip`](./configuration.md#client-ip) declares. Absent when that source gives no address. |
| `sets.<name>` | `tessera.CIDRSet`, `tessera.ASNSet` or `tessera.StringSet` | The set `<name>` that [`sets`](./configuration.md#sets) declares, of the type of its values. It is used only as `sets.<name>.contains(value)`: see [Sets](#sets). |

tessera reads the request as Envoy describes it, and changes it only as follows:

- **Host.** A port is a `:` followed by digits at the end: `api.example.com:8443` becomes `api.example.com`. Anything else after the last `:` is not a port and stays, so that the host matches no host written in its usual form. An IPv6 address keeps its brackets: `[2001:db8::1]`. A trailing dot stays: `www.example.com.` is not `www.example.com`. Only the letters `A` to `Z` change case.
- **Method.** Only the letters `a` to `z` change case.
- **Path and query.** The path is not decoded and not normalized: tessera sees `/a/../b` and `%2F` as Envoy sends them. Let Envoy normalize the path, as [Request paths](./grpc-api.md#request-paths) describes.
- **Headers.** Envoy joins the values of a repeated header with a comma, without a space: two `x-tag` headers `a` and `b` give `request.headers["x-tag"] == "a,b"`. The map holds the pseudo-headers too, such as `:authority` and `:path`. Values keep their case.
- **Request ID.** `request.id` is the value of the `x-request-id` header. Envoy generates one for a request that has none, unless its `generate_request_id` is `false`, and by default replaces the one that a client sends on a request from outside: its `preserve_external_request_id` keeps it.
- **Text.** A byte that is not valid UTF-8 becomes `!`, in every attribute, as Envoy does in the headers it sends.

`client.ip` is the only attribute that can be absent: `has(client.ip)` is `false` when the request has no client address, and reading `client.ip` is then an error, never an empty value (see [Indeterminate conditions](#indeterminate-conditions)).

## Types

| Type | Values |
| --- | --- |
| `bool`, `int`, `uint`, `double`, `string`, `bytes` | The types of CEL. An `int` is a 64-bit signed integer. |
| `list`, `map` | Written as `["GET", "HEAD"]` and `{"a": 1}`. `request.headers` is a `map(string, string)`. |
| `tessera.IP` | An IPv4 or IPv6 address. An IPv4-mapped IPv6 address, such as `::ffff:192.0.2.10`, is the IPv4 address `192.0.2.10`. Two addresses compare with `==` and `!=`; nothing else compares or converts them, but the functions below. |
| `tessera.CIDRSet`, `tessera.ASNSet`, `tessera.StringSet` | A set of networks, of autonomous system numbers, or of strings: a set of type `cidr`, `asn` or `string`. Only `contains` reads it. |

## Functions

tessera adds three functions on addresses:

| Function | Result | Meaning |
| --- | --- | --- |
| `client.ip.inCidr("192.0.2.0/24")` | `bool` | Whether the address is in the network. |
| `client.ip.family()` | `int` | `4` for an IPv4 address, `6` for an IPv6 address. |
| `string(client.ip)` | `string` | The address in its canonical form: `192.0.2.10`, `2001:db8::1`. |

The network of `inCidr` is a string literal, which tessera checks when it compiles the condition: an address and a prefix length, such as `192.0.2.0/24` or `2001:db8::/32`, with no bit set after the prefix length (`192.0.2.1/24` is an error) and an IPv4 network in IPv4 notation (`::ffff:192.0.2.0/120` is an error). An argument that is not a literal, such as `request.headers["x-net"]`, is an error. An IPv4 address is never in an IPv6 network, and the reverse: `client.ip.inCidr("::/0")` is `false` for `192.0.2.10`.

The standard functions and macros of CEL are available, among them:

| Function | Meaning |
| --- | --- |
| `s.startsWith(t)`, `s.endsWith(t)`, `s.contains(t)` | Prefix, suffix, substring. Case-sensitive. |
| `s.matches(re)` | Whether the regular expression `re`, in [RE2 syntax](https://github.com/google/re2/wiki/Syntax), matches part of `s`: anchor it with `^` and `$` to match all of it. `(?i)` makes it case-insensitive. |
| `size(x)` | The number of characters of a string, of items of a list, or of entries of a map. |
| `x in list`, `k in map` | Whether a list holds `x`, whether a map has the key `k`. |
| `has(x.f)` | Whether the field or map key `f` is set, without reading it. |
| `list.all(v, cond)`, `list.exists(v, cond)`, `list.exists_one(v, cond)`, `list.map(v, expr)`, `list.filter(v, cond)` | The macros over the items of a list, or the keys of a map. |
| `int(x)`, `uint(x)`, `double(x)`, `string(x)`, `bytes(x)`, `bool(x)` | Conversions. |

## Sets

`sets.<name>.contains(value)` is `true` when the set `<name>`, declared in [`sets`](./configuration.md#sets), holds `value`. The type of `value` follows the type of the set:

| Set | Example | `value` | `contains` is `true` when |
| --- | --- | --- | --- |
| `cidr` | `sets.office.contains(client.ip)` | A `tessera.IP` | The address is in a network of the set. An IPv4 address is never in an IPv6 network. |
| `asn` | `sets.partner_networks.contains(64496)` | An `int` | The set holds the number. An `int` outside `1` to `4294967295` is never in the set. |
| `string` | `sets.partners.contains(request.host)` | A `string` | The set holds the string, compared exactly, case included. |

```text
request.path.startsWith("/admin") && !sets.office.contains(client.ip)
```

A set is not a value: `sets.<name>` alone, `sets`, `has(sets.<name>)`, a comparison of sets and a list of sets are errors. So are an argument of another type, such as `sets.office.contains("192.0.2.10")` for a `cidr` set, and a set that [`sets`](./configuration.md#sets) does not declare (see [Errors](#errors)). `s.contains(t)` on strings stays the standard function.

All the conditions of one decision read the sets as they were when it started: a set that changes meanwhile changes nothing for it. When a set is unavailable, `contains` answers what its [`on_unavailable`](./configuration.md#sets-name-on-unavailable) says, whatever its argument: with `fail`, the default, the condition is [indeterminate](#indeterminate-conditions).

## Operators

From the highest precedence to the lowest:

| Operators | Meaning |
| --- | --- |
| `.`, `[]`, `()` | Field, index or key, call. |
| `!`, `-` | Not, negation. |
| `*`, `/`, `%` | Multiplication, division, remainder. |
| `+`, `-` | Addition and concatenation, subtraction. |
| `==`, `!=`, `<`, `<=`, `>`, `>=`, `in` | Comparison and membership. |
| `&&` | And. |
| `\|\|` | Or. |
| `? :` | Conditional: `cond ? a : b`. |

## Indeterminate conditions

The evaluation of a condition fails when it:

- reads `client.ip` and the request has no client address;
- looks a value up in a set that is unavailable and whose [`on_unavailable`](./configuration.md#sets-name-on-unavailable) is `fail`, which tessera reports with the cause [`set_unavailable`](./metrics.md#tessera-rule-evaluation-errors-total);
- reads a key that a map does not have, as `request.headers["authorization"]` does when the request has no `Authorization` header;
- fails a conversion (`int("ten")`), overflows an integer, or divides by zero;
- exceeds the [cost limit](#limits).

`&&` and `||` absorb a failure when their other operand decides, whatever its side: `true || x` and `x || true` are `true`, `false && x` and `x && false` are `false`, even when `x` fails. A condition whose result depends on a failure is indeterminate, and it counts against allowing:

| Rule | With an indeterminate condition |
| --- | --- |
| `action: deny` | The rule matches: the request is denied. |
| `action: allow` | The rule does not match: tessera evaluates the next rule. |
| `mode: audit` | The rule decides nothing: tessera evaluates the next rule. |

So a missing datum can make tessera deny a request that it would have allowed with the datum, and never allow a request that it would have denied. To decide otherwise when the client address is missing, write a rule with `has(client.ip)` before the rules that read it: see [Handle a missing client address](../guide/write-rules.md#handle-a-missing-client-address).

The cost limit is not absorbed: an evaluation that exceeds it stops, and the condition is indeterminate, also on one side of `||` or `&&`. A failure inside tessera, which it recovers, makes the condition indeterminate too.

## Errors

A condition that does not compile stops `tessera config validate` and `tessera serve` with its position, a line and a column counted in characters:

| Expression | Error |
| --- | --- |
| `request.hots == "x"` | `undefined field 'hots'` |
| `geo.country == "IT"` | `undeclared reference to 'geo'` |
| `request.host == 443` | `found no matching overload for '_==_' applied to '(string, int)'` |
| `request.path` | `the condition must be a bool, not string` |
| `client.ip.inCidr("192.0.2.1/24")` | `invalid network: the network has bits set after the prefix length: write its first address, such as 192.0.2.0/24` |
| `sets.office.contains("192.0.2.10")`, `office` of type `cidr` | `found no matching overload for 'contains' applied to 'tessera.CIDRSet.(string)'` |
| `sets.ofice.contains(client.ip)` | `undefined field 'ofice'` |
| `has(sets.office)` | `a set can be used only as the target of contains, as in sets.<name>.contains(value)` |
| `request.path.matches("[a-")` | `invalid regular expression: missing closing ]` |
| `request.host == "x" &&` | `syntax error in the expression` |

An error message never quotes a literal of the expression, such as a string that it compares.

## Pitfalls

- **A missing header is an error.** `request.headers["x-api-key"] == "k-1"` fails when the request has no `x-api-key`: in a `deny` rule it denies, in an `allow` rule it does not match. Test the key first: `"x-api-key" in request.headers && request.headers["x-api-key"] == "k-1"`.
- **`!` binds tighter than a comparison.** `!int(request.headers["x-count"]) > 5` reads as `(!int(...)) > 5`, which does not compile. Write `!(int(request.headers["x-count"]) > 5)`.
- **A repeated header is one value.** `request.headers["x-tag"] == "a"` is `false` when the request has two `x-tag` headers: the value is `a,b`.
- **YAML reads some expressions first.** An expression that starts with a character that YAML reserves, such as `!` or `"`, or that contains `: ` or ` #`, needs quotes or a literal block (`|`): see the [types of the configuration file](./configuration.md#format). A literal block is always safe.

## Limits

| Limit | Value |
| --- | --- |
| Length of an expression | 8192 characters |
| Cost of one evaluation | 10000 units of the cost model of CEL |
| Nesting of macros | A macro over a list or a map (`all`, `exists`, `exists_one`, `map`, `filter`) cannot contain another, nor iterate over the result of another: `l.map(x, f(x)).filter(y, g(y))` is an error. |

- **Cost.** CEL counts a cost for each step of an evaluation, larger for steps that read more data, such as a regular expression over a long header or a macro over many headers. `contains` on a set costs one unit, whatever the size of the set and of its argument. A condition made of comparisons and a few function calls stays far below the limit. An evaluation that reaches it stops, and the condition is [indeterminate](#indeterminate-conditions).
- **Regular expressions** are RE2, whose time grows linearly with the text. tessera compiles a pattern written as a literal when it reads the configuration, and an invalid one is an error.
