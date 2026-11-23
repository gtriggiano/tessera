---
description: "The expression language of the rule conditions, the upstream headers and the decision fields: attributes and their types, functions, sets, enrichments, operators, indeterminate conditions, errors and limits."
---

# Expression language reference

**tessera** reads three kinds of expressions of the [Common Expression Language](https://cel.dev/) (CEL): the condition of each rule, [`policy.rules[].when`](./configuration.md#policy-rules-when), the value of each upstream header, [`upstream.headers`](./configuration.md#upstream-headers), and the value of each decision field, [`log.decision_fields`](./configuration.md#log-decision-fields). This page lists the attributes that an expression reads, the functions and operators it can use, the sets it can look values up in, the enrichments that describe the client address, what happens when it cannot be evaluated, and its limits. [Write policy rules](../guide/policy-rules.md) shows how to combine conditions into a policy.

| Expression | Result | Evaluated | When it fails |
| --- | --- | --- | --- |
| [A condition](#conditions) | `bool` | For each rule, in order, until one decides. | The condition is [indeterminate](#indeterminate-conditions). |
| [An upstream header](#upstream-headers) | `string` | After the decision, for a request that Envoy sends on. | The header is removed. |
| [A decision field](#decision-fields) | `string` | After the decision, for a `decision` event that tessera writes. | The field is empty. |

The three read the same attributes, except what a [decision field](#decision-fields) cannot read, with the same functions, limits and errors, and all the expressions of one decision read the same request, the same copies of the sets and of the databases, and the same records of the client address.

## Conditions

A condition is an expression whose result is a bool:

```text
request.host == "api.example.com" && client.ip.inCidr("192.0.2.0/24")
```

tessera compiles and type-checks every condition when it reads the configuration, so that `tessera config validate` and `tessera serve` stop on an unknown attribute, a wrong type or a result that is not a bool, at its position in the file (see [`policy.rules[].when`](./configuration.md#policy-rules-when)). The expression language is CEL as its [language definition](https://github.com/google/cel-spec/blob/master/doc/langdef.md) describes, with the attributes and functions of this page; no extension library of CEL is available.

## Upstream headers

The value of an upstream header is an expression whose result is a string, which tessera compiles and type-checks as a condition: a result of another type is an error, `the expression must be a string, not bool` for a comparison. Convert a number with `string()`, and choose between two values with `? :`:

```text
asn.found ? string(asn.number) : ""
```

tessera evaluates the upstream headers after the decision, for every request that Envoy sends on: an allowed request, and a denied request that [`policy.mode: audit`](./configuration.md#policy-mode) lets through. An expression that gives an empty string removes the header. An expression that fails, for one of the reasons of [Indeterminate conditions](#indeterminate-conditions), removes it too, and [`tessera_upstream_header_errors_total`](./metrics.md#tessera-upstream-header-errors-total) counts it with the same causes: a failure never changes the decision. An upstream header can read every attribute, the query and the headers of the request included.

## Decision fields

The value of a decision field is an expression whose result is a string, as for an upstream header. tessera evaluates it after the decision, only for a [`decision`](./logs.md#decision) event that it writes, and writes an empty value when it fails.

A decision field is written to the logs, so it cannot read what tessera never logs (see [What is never logged](./logs.md#what-is-never-logged)): the query, a [secret header](./configuration.md#secret-headers), in any spelling and case, and the map `request.headers` otherwise than by a literal name, which leaves every other header unread. Each such read is a configuration error at the position of `request`:

| Expression | Error |
| --- | --- |
| `request.query`, `has(request.query)` | `a decision field cannot read request.query` |
| `request.headers["authorization"]`, `request.headers.Cookie`, `"proxy-authorization" in request.headers`, and the same for a header of `secret_headers` | `a decision field cannot read a secret header` |
| `request.headers.exists(h, h.startsWith("x-"))`, `request.headers[request.host]`, `size(request.headers)` | `a decision field can read request.headers only by a literal name, such as request.headers["user-agent"]` |
| `[request][0].host` | `a decision field can read request only through its attributes, such as request.host` |

A header named by a literal reads as in a condition: `request.headers["user-agent"]`, `request.headers.accept`, `"x-tag" in request.headers` and `has(request.headers.accept)`.

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
| `geo` | `tessera.Geo` | The enrichment that says where the client address is, from the database of [`enrichment.geo`](./configuration.md#enrichment-geo), read through its fields `geo.found`, `geo.country`, `geo.continent`, `geo.subdivisions`, `geo.city`, `geo.postal_code`, `geo.timezone` and `geo.location`: see [`geo`](#geo). |
| `asn` | `tessera.ASN` | The enrichment that says which autonomous system the client address is in, from the database of [`enrichment.asn`](./configuration.md#enrichment-asn), read through its fields `asn.found`, `asn.number` and `asn.organization`: see [`asn`](#asn). |

tessera reads the request as Envoy describes it, and changes it only as follows:

- **Host.** A port is a `:` followed by digits at the end: `api.example.com:8443` becomes `api.example.com`. Anything else after the last `:` is not a port and stays, so that the host matches no host written in its usual form. An IPv6 address keeps its brackets: `[2001:db8::1]`. A trailing dot stays: `www.example.com.` is not `www.example.com`. Only the letters `A` to `Z` change case.
- **Method.** Only the letters `a` to `z` change case.
- **Path and query.** The path is not decoded and not normalized: tessera sees `/a/../b` and `%2F` as Envoy sends them. Let Envoy normalize the path, as [Request paths](./grpc-api.md#request-paths) describes.
- **Headers.** Envoy joins the values of a repeated header with a comma, without a space: two `x-tag` headers `a` and `b` give `request.headers["x-tag"] == "a,b"`. The map holds the pseudo-headers too, such as `:authority` and `:path`. Values keep their case.
- **Request ID.** `request.id` is the value of the `x-request-id` header. Envoy generates one for a request that has none, unless its `generate_request_id` is `false`, and by default replaces the one that a client sends on a request from outside: its `preserve_external_request_id` keeps it.
- **Text.** A byte that is not valid UTF-8 becomes `!`, in every attribute, as Envoy does in the headers it sends.

`client.ip` and `geo.location` are the only attributes that can be absent: `has(client.ip)` is `false` when the request has no client address, and reading `client.ip` is then an error, never an empty value (see [Indeterminate conditions](#indeterminate-conditions)). Without a client address, reading `geo` or `asn` is an error too, `has(geo.location)` included.

## Types

| Type | Values |
| --- | --- |
| `bool`, `int`, `uint`, `double`, `string`, `bytes` | The types of CEL. An `int` is a 64-bit signed integer. |
| `list`, `map` | Written as `["GET", "HEAD"]` and `{"a": 1}`. `request.headers` is a `map(string, string)`. |
| `tessera.IP` | An IPv4 or IPv6 address. An IPv4-mapped IPv6 address, such as `::ffff:192.0.2.10`, is the IPv4 address `192.0.2.10`. Two addresses compare with `==` and `!=`; nothing else compares or converts them, but the functions below. |
| `tessera.CIDRSet`, `tessera.ASNSet`, `tessera.StringSet` | A set of networks, of autonomous system numbers, or of strings: a set of type `cidr`, `asn` or `string`. Only `contains` reads it. |
| `tessera.Geo`, `tessera.ASN` | The values of `geo` and `asn`, read through their fields. A value compares equal only to itself: `geo == geo` is `true` in a decision, and no other value equals it. |
| `tessera.GeoCountry`, `tessera.GeoContinent`, `tessera.GeoSubdivision`, `tessera.GeoPoint` | The country, the continent, a subdivision and the location of [`geo`](#geo), read through their fields. Two values of one of these types are equal when all their fields are equal: `==`, `!=` and `in` compare them field by field, and so does `==` between two lists of them. |

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

All the expressions of one decision, its upstream headers and decision fields included, read the sets as they were when it started: a set that changes meanwhile changes nothing for it. When a set is unavailable, `contains` answers what its [`on_unavailable`](./configuration.md#sets-name-on-unavailable) says, whatever its argument: with `fail`, the default, the condition is [indeterminate](#indeterminate-conditions).

## Enrichments

`geo` and `asn` are the enrichments of the client address: attributes that tessera derives from it, with what a database in the MaxMind DB format says of it. [`enrichment.geo`](./configuration.md#enrichment-geo) and [`enrichment.asn`](./configuration.md#enrichment-asn) configure them. Without its configuration, an enrichment does not exist, and a condition that reads it is a configuration error: `geo is not available: the configuration has no enrichment.geo`.

```text
geo.country.code in ["IT", "FR"] || asn.number == 64496
```

### `geo`

| Attribute | Type | Content |
| --- | --- | --- |
| `geo.found` | `bool` | Whether the database has the address. |
| `geo.country` | `tessera.GeoCountry` | The country: `country` in the database, with the three fields below. |
| `geo.country.code` | `string` | The ISO 3166-1 alpha-2 code of the country, such as `IT`: `country.iso_code`. |
| `geo.country.name` | `string` | The name of the country in English, such as `Italy`: `country.names.en`. The database writes some names with an article, such as `The Netherlands`. |
| `geo.country.in_eu` | `bool` | Whether the country is in the European Union: `country.is_in_european_union`. |
| `geo.continent` | `tessera.GeoContinent` | The continent: `continent`, with the two fields below. |
| `geo.continent.code` | `string` | The code of the continent, `AF`, `AN`, `AS`, `EU`, `NA`, `OC` or `SA`: `continent.code`. |
| `geo.continent.name` | `string` | The name of the continent in English, `Africa`, `Antarctica`, `Asia`, `Europe`, `North America`, `Oceania` or `South America`: `continent.names.en`. |
| `geo.subdivisions` | `list(tessera.GeoSubdivision)` | The subdivisions of the country where the address is, all of them, in the order of `subdivisions` in the database: from the largest to the smallest, one or two in practice, such as the region of Lazio and the province of Rome. An empty list when the record has none. |
| `geo.subdivisions[i].code` | `string` | The code of the subdivision as the database has it, without the code of the country, such as `62` for Lazio, `RM` for Rome, `ENG` for England or `CA` for California: `iso_code`. |
| `geo.subdivisions[i].name` | `string` | The name of the subdivision in English, such as `Lazio`: `names.en`. |
| `geo.city` | `string` | The name of the city in English: `city.names.en`. |
| `geo.postal_code` | `string` | The postal code as the database has it: `postal.code`. In some countries it is only the first part of the code, such as `EC4N` in the United Kingdom, `T2R` in Canada or `P72` in Ireland. |
| `geo.timezone` | `string` | The IANA time zone, such as `Europe/Rome`: `location.time_zone`. |
| `geo.location` | `tessera.GeoPoint` | The location: `geo.location.lat` and `geo.location.lon`, its latitude and longitude as `double` values in degrees, from `location.latitude` and `location.longitude`. Absent when the record does not have both. |

- An address that the database does not have is not an error: `geo.found` is `false`, the strings are empty, `geo.country.in_eu` is `false`, `geo.subdivisions` is an empty list, and `geo.location` is absent. A comparison such as `geo.country.code == "IT"` is then `false`: to decide on such addresses, write a rule on `!geo.found`.
- A record can have the address without a country, as when the database only has the country where the network is registered: `geo.found` is `true`, `geo.country.code` and `geo.country.name` are empty, and `geo.country.in_eu` is `false`.
- A Country database has no subdivisions, city, postal code, time zone or location: `geo.subdivisions` is empty, those strings are empty and `geo.location` is absent.
- A string that the record does not have, such as the name of a subdivision or the postal code, is empty.
- `has(geo.location)` tests whether the location is there. Reading `geo.location`, or `geo.location.lat`, when it is absent fails, as a missing header does: write `has(geo.location) && geo.location.lat > 45.0`.
- An IPv4-mapped IPv6 address is looked up as the IPv4 address it maps. An IPv6 address is never in a database of IPv4 addresses only.

The code of a subdivision is unique only in its country: in GeoLite2 City, `62` is the code of a first-level subdivision in `IT`, `KZ`, `MT`, `TH` and `TR`. Test the country together with the subdivision:

```text
geo.country.code == "IT" && geo.subdivisions.exists(s, s.code == "RM")
```

`exists` tests every level of the list, so the condition does not depend on the position of the province in it. To read one level, test the size of the list first: `size(geo.subdivisions) > 1 && geo.subdivisions[1].code == "RM"`. An index beyond the end of the list fails, as a missing header does.

### `asn`

| Attribute | Type | Content |
| --- | --- | --- |
| `asn.found` | `bool` | Whether the database has the address. |
| `asn.number` | `int` | The autonomous system number: `autonomous_system_number` in the database; `0` when the database does not have the address. |
| `asn.organization` | `string` | The organization of the autonomous system: `autonomous_system_organization`. |

`asn.number` reads as the argument of a set of type `asn`: `sets.hosting.contains(asn.number)`. A set of type `asn` never holds `0`.

### When tessera computes an enrichment

tessera computes an enrichment, by looking the client address up in its database, only when an expression reads `geo` or `asn`, at the first read in a decision: a request that a rule decides before any rule reads them, and whose upstream headers and decision fields do not read them, looks nothing up. The lookup reads the whole record once, and every later read in the same decision, by another field, another rule, an upstream header or a decision field, reuses it. Each decision reads the databases as they were when it started: a database that tessera replaces meanwhile changes nothing for it, its upstream headers and decision fields included.

Without a client address, reading `geo` or `asn` fails, `geo.found`, `has(geo.location)` and `size(geo.subdivisions)` included, with the cause `client_ip_missing`: `!has(geo.location)` cannot let a `deny` rule open. A record that tessera cannot read, such as one whose fields have other types than in the schema, fails the read with the cause `database_error`: tessera never takes it for an address that the database does not have. See [Indeterminate conditions](#indeterminate-conditions).

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

- reads `client.ip`, `geo` or `asn`, and the request has no client address, which tessera reports with the cause [`client_ip_missing`](./metrics.md#tessera-rule-evaluation-errors-total);
- looks a value up in a set that is unavailable and whose [`on_unavailable`](./configuration.md#sets-name-on-unavailable) is `fail`, which tessera reports with the cause `set_unavailable`;
- reads `geo` or `asn`, and tessera cannot read the record of the client address in the database, which tessera reports with the cause `database_error`;
- reads a key that a map does not have, as `request.headers["authorization"]` does when the request has no `Authorization` header, `geo.location` when the record has no location, or an index beyond the end of a list, as `geo.subdivisions[1]` does when the record has one subdivision;
- fails a conversion (`int("ten")`), overflows an integer, or divides by zero;
- exceeds the [cost limit](#limits).

`&&` and `||` absorb a failure when their other operand decides, whatever its side: `true || x` and `x || true` are `true`, `false && x` and `x && false` are `false`, even when `x` fails. A condition whose result depends on a failure is indeterminate, and it counts against allowing:

| Rule | With an indeterminate condition |
| --- | --- |
| `action: deny` | The rule matches: the request is denied. |
| `action: allow` | The rule does not match: tessera evaluates the next rule. |
| `mode: audit` | The rule decides nothing: tessera evaluates the next rule. |

So a missing datum can make tessera deny a request that it would have allowed with the datum, and never allow a request that it would have denied. To decide otherwise when the client address is missing, write a rule with `has(client.ip)` before the rules that read it: see [Handle a missing client address](../guide/policy-rules.md#handle-a-missing-client-address).

The cost limit is not absorbed: an evaluation that exceeds it stops, and the condition is indeterminate, also on one side of `||` or `&&`. A failure inside tessera, which it recovers, makes the condition indeterminate too.

## Errors

An expression that does not compile stops `tessera config validate` and `tessera serve` with its position, a line and a column counted in characters:

| Expression | Error |
| --- | --- |
| `request.hots == "x"` | `undefined field 'hots'` |
| `geo.country.code == "IT"`, without [`enrichment.geo`](./configuration.md#enrichment-geo) | `geo is not available: the configuration has no enrichment.geo` |
| `geo.region == "62"` | `undefined field 'region'` |
| `geo.location.lat == 41` | `found no matching overload for '_==_' applied to '(double, int)'` |
| `"RM" in geo.subdivisions` | `found no matching overload for '@in' applied to '(string, list(tessera.GeoSubdivision))'` |
| `request.host == 443` | `found no matching overload for '_==_' applied to '(string, int)'` |
| `request.path` | `the condition must be a bool, not string` |
| `request.path == "/"`, as an upstream header or a decision field | `the expression must be a string, not bool` |
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
- **A location can be absent.** `geo.location.lat > 45.0` fails for a record without a location. Test it first: `has(geo.location) && geo.location.lat > 45.0`.
- **A subdivision code needs its country.** `geo.subdivisions.exists(s, s.code == "62")` matches a subdivision `62` of any country: write `geo.country.code == "IT" && geo.subdivisions.exists(s, s.code == "62")`.
- **A coordinate is a `double`.** `geo.location.lat > 45` does not compile: write `45.0`.
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
