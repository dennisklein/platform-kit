// Package render turns a TalosCluster, one of its TalosNodes and the cluster's
// secrets bundle into that node's Talos machine configuration, using Talos' own
// generator and document types from pkg/machinery. It also mints the admin
// talosconfig and kubeconfig from the same bundle, and addresses the image
// factory that serves the installer a rendered config names.
//
// # What Talos 1.14 expects
//
// These findings come from reading pkg/machinery v1.14.6, and they decide the
// shape of the os.pk.io kinds:
//
//   - The configuration is a stream of YAML documents. generate.NewInput with
//     a v1.14 version contract (config.ParseContractFromVersion) already emits
//     the multi-document form: the v1alpha1 document keeps only machine.type,
//     token, CA, certSANs and features plus cluster.token and the PKI Talos
//     itself needs (etcd, ...); Kubernetes moved out to its own documents
//     (KubeClusterConfig, KubeNodeConfig, KubeletConfig, KubeNetworkConfig,
//     KubeAPIServerConfig, KubeFlannelCNIConfig, KubePrismConfig, ...), as did
//     discovery (DiscoveryServiceConfig, DiscoveryIdentityConfig), host DNS
//     (ResolverConfig), the hostname (HostnameConfig), registries
//     (RegistryMirrorConfig, RegistryTLSConfig), sysctls (SysctlConfig) and
//     the ephemeral volume (VolumeConfig, FilesystemTrimConfig). The v1alpha1
//     fields they replace (machine.network, machine.install, machine.kubelet,
//     machine.registries, machine.sysctls, cluster.network, cluster.apiServer,
//     ...) are deprecated, and a document conflicts with its legacy field when
//     both are set.
//   - machine.install is replaced by UnattendedInstallConfig: installer.image
//     plus provisioning.diskSelector.match, a CEL expression over `disk` in
//     the DiskLocator environment (disk.serial, disk.model, disk.wwid,
//     disk.bus_path, disk.dev_path, disk.size, glob(...)). The old
//     diskSelector struct is gone, and generate only emits the document when
//     given a device path, so this package builds it itself from the node's
//     selector.
//   - Networking is entirely document based: LinkAliasConfig names a physical
//     link by a CEL selector over its link status (LinkMatch: the permanent
//     MAC, or the current MAC of a physical link that reports no permanent
//     one; Talos reads permanent_addr from ethtool and has no fallback), and every
//     other document refers to links by that alias. LinkConfig carries static
//     addresses, routes and MTU; DHCPv4Config turns DHCP on for one link; the
//     first link or DHCP document switches off Talos' default DHCP on every
//     link. Layer2VIPConfig (name = the VIP, link = the alias) replaces the
//     per-interface vip field. BondConfig and VLANConfig exist but are not
//     modelled here; patches reach them.
//   - A custom CNI is KubeExternalManifestConfig documents instead of
//     Flannel; no CNI at all means deleting the KubeFlannelCNIConfig document.
//   - configpatcher.LoadPatch reads multi-document strategic merge patches
//     (documents merge by apiVersion, kind and name; `$patch: delete`
//     removes one). JSON6902 patches still load but only address the
//     v1alpha1 document, so they are refused here.
//   - Validation runs client side with Container.ValidateAsClient and a
//     runtime mode; metal mode requires an install, container mode does not.
//   - The secrets bundle `talosctl gen secrets` writes decodes into
//     secrets.Bundle and validates against the contract. The Talos client
//     certificate comes from Bundle.GenerateTalosAPIClientCertificateWithTTL;
//     the Kubernetes admin certificate is not in machinery (Talos mints it
//     inside the node), so Kubeconfig mints it from the bundle's Kubernetes
//     CA with the same subject Talos uses (CN admin, O system:masters).
//   - talos.config=metal-iso is still honoured by the metal platform in
//     v1.14: it reads config.yaml from a volume labelled metal-iso. The kernel
//     argument rides the schematic, because Talos takes the installed kernel
//     command line from the installer image and refuses extra kernel
//     arguments in the install configuration.
package render
