# Security policy

## Report a vulnerability

Do not open a public issue, discussion or pull request for a vulnerability.

Use GitHub private vulnerability reporting for this repository:

1. Open <https://github.com/dennisklein/platform-kit/security/advisories/new>.
   You can also go to the **Security** tab and select **Report a vulnerability**.
2. Describe the problem, the affected commit, and the steps to reproduce it.
3. Say what an attacker needs: for example, access to the BMC network, a
   Kubernetes role, or a place on the path between the operator and a BMC.

Only the maintainers can read the report. The discussion and the fix stay
private until an advisory is published.

Do not include real BMC addresses, passwords, serial numbers or other site
data. Use example values.

## What to expect

This project is experimental and has no guaranteed response time. The
maintainers answer in the private report.

## Supported versions

There is no release. Fixes go to the `metal` branch only.

## Scope

pk-metal holds BMC credentials and Talos secrets, and it can power off and
wipe machines. Reports on these subjects are in scope:

- A way around the approval of a HostOperation.
- A leak of BMC credentials, Talos secrets, or media grant tokens.
- A fault in the admission policies or the RBAC that the manifests ship.
- A fault in the media server or the relay.

The [threat model](https://dennisklein.github.io/platform-kit/reference/security/threat-model/)
states what the design protects and what it trusts. A risk that the threat
model already accepts is not a vulnerability, but a report that the threat
model is wrong is welcome.

The virtual lab uses a fixed example password for its simulated BMCs on
purpose. This is not a vulnerability.
