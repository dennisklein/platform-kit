# The relay, and its secret

`pk-metal relay` runs on a host that can reach the BMCs when the machine that
runs `pk-metal boot` cannot. BMCs download boot media from the relay's
**front** address; the relay hands each download to `pk-metal boot` through
its **tunnel** address, and boot's Redfish calls go out to the BMCs the same
way. The relay terminates nothing: TLS ends at the media server inside
`pk-metal boot`, so the relay only ever sees ciphertext.

The relay is not wired into the operator today. It serves day 0: the first
machines of a site, before there is a cluster.

## Why the tunnel needs a secret

Splicing ciphertext does not protect the media on its own. BMCs do not check
the media server's certificate, so whoever holds the relay's control
connection receives the BMCs' media connections, can present any
certificate, and then:

- reads the grant's user and password (Supermicro sends them as HTTP Basic,
  the iBMC in the URL) and fetches the machine config, with the cluster's
  keys, through the front; or
- serves an ISO of its own, which the machine boots.

The tunnel's DIAL verb also opens TCP connections from the relay into the
BMC network for whoever asks. So every connection to the tunnel port proves
that it holds the relay's shared secret before the relay does anything for
it:

```text
relay:  PK-RELAY/1 <nonce>                    a fresh 128-bit nonce per connection
peer:   REGISTER mac=<hmac>                   HMAC-SHA256(secret, nonce, verb, argument)
        ACCEPT <id> mac=<hmac>
        DIAL <host:port> mac=<hmac>
```

The MAC covers the verb, its argument and the connection's nonce: a line seen
on the wire works neither on another connection nor with another target, and
the secret itself never crosses the wire. A line without a valid MAC gets
`ERR unauthenticated` and the connection is closed. A wrong secret therefore
cannot take the control connection away from the operator that holds it.

The secret proves who connects. It does not encrypt the tunnel, which is
why the tunnel stays on loopback and is reached over `ssh -L`.

## Day 0

On the host that reaches the BMCs:

```sh
pk-metal relay --front 10.0.0.2:8443 --secret-file ~/.config/pk-metal/relay.secret
```

When the file does not exist, the relay writes a new secret there, readable
by its owner only, and says so. An existing file is never replaced, so a
secret already handed out keeps working after a restart. Instead of a file,
the secret can come from `$PK_METAL_RELAY_SECRET` (at least 32 characters).

On the workstation, with the tunnel forwarded and the same secret:

```sh
ssh -L 9443:127.0.0.1:9443 relay-host
scp relay-host:.config/pk-metal/relay.secret ./relay.secret
PK_METAL_BMC_PASSWORD=... pk-metal boot --relay 127.0.0.1:9443 \
    --relay-secret-file ./relay.secret --config node-1.yaml --node node-1 https://10.0.0.11
```

`pk-metal boot` only reads a secret; it never makes one up.

## Steady state (not wired yet)

When the relay runs as a pod on the BMC network, its tunnel crosses the pod
network instead of an ssh connection. The design for that day:

- The secret is a Secret in the operator's namespace, created by a person (or
  by the same `--secret-file` logic writing into a mounted file), and mounted
  into both the relay pod and the manager. The manager never generates one.
- A NetworkPolicy lets only the manager's pods reach the relay's tunnel port.
- Because the secret authenticates but does not encrypt, a party on the pod
  network path could still splice into an authenticated connection. Mutual
  TLS on the tunnel port closes that gap and is the next step before the
  relay carries steady-state traffic.
