# hs-hub

Publishes services announced by tailnet nodes as [Headscale](https://github.com/juanfont/headscale) extra DNS records, so every client on the tailnet resolves `reader.example.com` to the tailnet IP of the machine running it, without a DNS server or split DNS.

Adding a service only means rebuilding the machine that runs it. The Headscale host never needs a redeploy.

## How it works

```
 service host (tag:server)                    Headscale host (sky)
┌────────────────────────────┐   tailnet    ┌──────────────────────────────┐
│ Caddy :443  → services     │              │ hs-hub                       │
│ Caddy :7777 /services.json │ ◄─── GET ─── │  1. list peers (LocalAPI)    │
└────────────────────────────┘              │  2. fetch tagged manifests   │
                                            │  3. write records.json ──┐   │
                                            │ headscale  ◄─ watches ───┘   │
                                            │  → MagicDNS on every client  │
                                            └──────────────────────────────┘
```

Every round (default 30s) the hub:

1. Lists the tailnet through the local tailscaled's LocalAPI (`/localapi/v0/status`), including the hub's own node.
2. Fetches `http://<tailnet-ip>:7777/services.json` from every node carrying the configured tag (default `tag:server`).
3. Validates the names and writes A/AAAA records pointing each name at the claiming node's tailnet IPs. The IPs come from tailscaled, never from the manifest.
4. Atomically replaces the records file if its content changed. Headscale watches the file and pushes the records to clients.

Headscale is the membership list, so there's no registration, gossip or agent on the service hosts. A host publishes services by serving a static JSON file.

## Manifest

Each service host serves, on the tailnet only:

```json
{"services": ["reader.example.com", "db.example.com"]}
```

A name is published only if:

- it is a valid hostname (no wildcards), strictly below a `-domain`,
- it is not inside an `-exclude-domain` (use this for the MagicDNS base domain so nodes cannot shadow MagicDNS names),
- it is not `-reserved` (list every public name, above all the Headscale server itself: if a node claimed it, clients would lose their connection to the control server),
- no other node already claimed it. Conflicts go to the node whose host name sorts first, and are logged.

Names are case-insensitive and a trailing dot is ignored.

## Failure behaviour

| Situation | Effect |
| --- | --- |
| Manifest fetch fails (timeout, refused, non-200, bad JSON) | Node's last good names are kept for `-grace` (default 10m), then dropped. |
| Node disappears from the tailnet | Same as above. |
| Node still on the tailnet but loses the tag | Its names are dropped at once. |
| tailscaled unreachable or not running | Nothing changes, and nothing expires, until it is back. |
| Hub restarts | Known nodes are reloaded from `-state`, so a host that is down during the restart keeps its names. |
| Records file deleted or edited by hand | Rewritten on the next round. |

Headscale keeps its last good records if the file ever fails to parse, and re-adds its file watch after a rename.

## Usage

```
hs-hub -output /var/lib/hs-hub/records.json -domain example.com \
       -exclude-domain ts.example.com -reserved hs.example.com
```

| Flag | Default | |
| --- | --- | --- |
| `-output` | (required) | Records file, Headscale's `dns.extra_records_path`. |
| `-domain` | (required) | Published names must be below this domain. Repeatable or comma-separated. |
| `-exclude-domain` | | Never publish names inside this domain. Repeatable. |
| `-reserved` | | Never publish this exact name. Repeatable. |
| `-tag` | `tag:server` | Tag nodes must carry. |
| `-manifest-port` | `7777` | Port of the manifest. |
| `-manifest-path` | `/services.json` | HTTP path of the manifest. |
| `-state` | | File persisting known nodes across restarts. |
| `-interval` | `30s` | Time between rounds. `SIGHUP` triggers one immediately. |
| `-grace` | `10m` | How long names of unreachable nodes are kept. |
| `-fetch-timeout` | `3s` | Timeout per manifest fetch. |
| `-concurrency` | `16` | Parallel manifest fetches. |
| `-socket` | `/run/tailscale/tailscaled.sock` | tailscaled LocalAPI socket. |
| `-once` | | Run a single round and exit. |
| `-log-level` | `info` | `debug`, `info`, `warn` or `error`. |

### Tailnet policy

The hub only sees nodes that the tailnet policy lets it reach, so allow the hub's node to reach the manifest port:

```hujson
"tagOwners": {
  "tag:hub":    ["group:admin"],
  "tag:server": ["group:admin"],
},
"acls": [
  { "action": "accept", "src": ["tag:hub"], "dst": ["tag:server:7777"] },
],
```

## NixOS

The module runs the hub as a hardened service, creates an empty records file at boot so Headscale can start first, and, when Headscale runs on the same host, sets its `dns.extra_records_path` and excludes its MagicDNS `base_domain`.

```nix
{ config, lib, ... }:
{
  imports = [ inputs.hs-hub.nixosModules.default ];   # or "${pins.hs-hub}/nix/module.nix"

  services.hs-hub = {
    enable = true;
    domains = [ "example.com" ];
    # Public names served by this host must never be shadowed on the tailnet.
    reserved = lib.attrNames config.services.caddy.virtualHosts;
  };
}
```

With an impermanent root, persist `/var/lib/hs-hub`.

### Publishing services

A service host serves the manifest on the tailnet, for example from Caddy:

```nix
let
  manifest = builtins.toJSON { services = [ "reader.example.com" "db.example.com" ]; };
in
{
  services.caddy.virtualHosts."http://:7777".extraConfig = ''
    handle /services.json {
      header Content-Type application/json
      respond `${manifest}` 200
    }
    handle {
      respond 404
    }
  '';
  networking.firewall.interfaces.tailscale0.allowedTCPPorts = [ 7777 ];
}
```

## Development

```
nix develop
go test ./...
CGO_ENABLED=1 go test -race ./...
nix build
```

The hub depends only on the Go standard library. It talks to tailscaled's LocalAPI socket directly instead of importing `tailscale.com`.
