package overlays

// JSON Schemas of the third-party kinds: subsets of the CRD and core schemas
// with UI hints (x-pk-ui) and friendly messages (x-pk-message).

const durationPattern = `^([0-9]+(\\.[0-9]+)?(ms|s|m|h))+$`

const duration = `{"type": "string", "pattern": "` + durationPattern + `", "x-pk-message": "must be a duration, for example 5m", "x-pk-ui": {"widget": "duration"}}`

const metadata = `{
  "type": "object",
  "required": ["name"],
  "properties": {
    "name": {"type": "string", "title": "Name", "pattern": "^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$", "x-pk-message": "must be a lower-case DNS name, for example cilium", "x-pk-ui": {"label": "Name", "order": 0}},
    "namespace": {"type": "string", "title": "Namespace", "pattern": "^[a-z0-9]([-a-z0-9]*[a-z0-9])?$", "x-pk-ui": {"label": "Namespace", "order": 1}},
    "labels": {"type": "object", "additionalProperties": {"type": "string"}, "x-pk-ui": {"label": "Labels", "order": 2}},
    "annotations": {"type": "object", "additionalProperties": {"type": "string"}}
  }
}`

const sops = `{"type": "object", "title": "SOPS metadata", "description": "Encryption metadata of SOPS. The bench never shows the encrypted values.", "x-pk-truth": "generated", "x-pk-ui": {"label": "SOPS", "group": "Encryption", "order": 99}}`

const sourceRef = `{"type": "object", "required": ["kind", "name"], "properties": {
  "kind": {"type": "string", "enum": ["HelmRepository", "GitRepository", "OCIRepository", "Bucket", "HelmChart"]},
  "name": {"type": "string"},
  "namespace": {"type": "string"}
}}`

const dependsOn = `{"type": "array", "description": "Objects that must be ready first", "items": {"type": "object", "required": ["name"], "properties": {"name": {"type": "string"}, "namespace": {"type": "string"}}}}`

func root(apiVersion, kind, description, spec string, required string) string {
	return `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "title": "` + kind + `",
  "description": "` + description + `",
  "type": "object",
  "x-pk-kind": {"apiVersion": "` + apiVersion + `", "kind": "` + kind + `"},
  "required": [` + required + `],
  "properties": {
    "apiVersion": {"type": "string", "enum": ["` + apiVersion + `"]},
    "kind": {"type": "string", "enum": ["` + kind + `"]},
    "metadata": ` + metadata + `,
    "spec": ` + spec + `,
    "sops": ` + sops + `,
    "status": {"type": "object", "x-pk-truth": "generated"}
  }
}`
}

var helmReleaseSchema = root("helm.toolkit.fluxcd.io/v2", "HelmRelease",
	"Flux HelmRelease: a Helm chart with values, reconciled by the helm-controller. From the CRD schema helm.toolkit.fluxcd.io/v2.", `{
  "type": "object",
  "required": ["interval"],
  "additionalProperties": false,
  "x-pk-ui": {"label": "Spec", "order": 2},
  "properties": {
    "interval": {"type": "string", "pattern": "`+durationPattern+`", "x-pk-message": "must be a duration, for example 5m", "description": "Interval at which to reconcile the Helm release", "x-pk-ui": {"widget": "duration", "label": "Interval", "order": 1}},
    "chart": {"type": "object", "required": ["spec"], "description": "Chart template", "x-pk-ui": {"label": "Chart", "widget": "chart", "order": 2}, "properties": {
      "spec": {"type": "object", "required": ["chart", "sourceRef"], "properties": {
        "chart": {"type": "string", "x-pk-ui": {"label": "Chart"}},
        "version": {"type": "string", "x-pk-ui": {"label": "Version"}},
        "sourceRef": `+sourceRef+`,
        "interval": `+duration+`,
        "reconcileStrategy": {"type": "string", "enum": ["ChartVersion", "Revision"]},
        "valuesFiles": {"type": "array", "items": {"type": "string"}}
      }}
    }},
    "chartRef": `+sourceRef+`,
    "dependsOn": `+dependsOn+`,
    "driftDetection": {"type": "object", "description": "Detect and correct drift", "properties": {"mode": {"type": "string", "enum": ["enabled", "warn", "disabled"]}, "ignore": {"type": "array"}}},
    "install": {"type": "object", "description": "Install options"},
    "upgrade": {"type": "object", "description": "Upgrade options"},
    "rollback": {"type": "object"},
    "uninstall": {"type": "object"},
    "test": {"type": "object"},
    "kubeConfig": {"type": "object"},
    "maxHistory": {"type": "integer", "minimum": 0},
    "persistentClient": {"type": "boolean"},
    "postRenderers": {"type": "array"},
    "releaseName": {"type": "string", "maxLength": 53},
    "serviceAccountName": {"type": "string"},
    "storageNamespace": {"type": "string"},
    "suspend": {"type": "boolean", "description": "Stop reconciling"},
    "targetNamespace": {"type": "string"},
    "timeout": `+duration+`,
    "commonMetadata": {"type": "object"},
    "valuesFrom": {"type": "array", "items": {"type": "object", "required": ["kind", "name"], "properties": {"kind": {"type": "string", "enum": ["Secret", "ConfigMap"]}, "name": {"type": "string"}, "valuesKey": {"type": "string"}, "targetPath": {"type": "string"}, "optional": {"type": "boolean"}}}},
    "values": {"type": "object", "description": "Values for the chart", "x-pk-ui": {"label": "Values", "order": 10}, "properties": {
      "bgpControlPlane": {"type": "object", "properties": {"enabled": {"type": "boolean", "x-pk-ui": {"label": "BGP control plane", "order": 11}}}},
      "hubble": {"type": "object", "properties": {"ui": {"type": "object", "properties": {"auth": {"type": "object", "properties": {
        "token": {"type": "string", "x-pk-secret": true, "x-pk-ui": {"label": "Hubble UI token", "order": 12}}
      }}}}}}
    }}
  }
}`, `"apiVersion", "kind", "metadata", "spec"`)

var fluxKustomizationSchema = root("kustomize.toolkit.fluxcd.io/v1", "Kustomization",
	"Flux Kustomization: applies one path of the state repo to a cluster. From the CRD schema kustomize.toolkit.fluxcd.io/v1.", `{
  "type": "object",
  "required": ["interval", "prune", "sourceRef"],
  "additionalProperties": false,
  "properties": {
    "interval": {"type": "string", "pattern": "`+durationPattern+`", "x-pk-message": "must be a duration, for example 5m", "x-pk-ui": {"widget": "duration", "label": "Interval", "order": 1}},
    "path": {"type": "string", "pattern": "^\\./", "x-pk-message": "must be a path in the repo that starts with ./", "x-pk-ui": {"label": "Path", "order": 2}},
    "prune": {"type": "boolean", "x-pk-ui": {"label": "Prune", "order": 3}},
    "sourceRef": `+sourceRef+`,
    "dependsOn": `+dependsOn+`,
    "decryption": {"type": "object", "properties": {"provider": {"type": "string", "enum": ["sops"]}, "secretRef": {"type": "object", "properties": {"name": {"type": "string"}}}}},
    "targetNamespace": {"type": "string"},
    "postBuild": {"type": "object"},
    "timeout": `+duration+`,
    "retryInterval": `+duration+`,
    "wait": {"type": "boolean"},
    "force": {"type": "boolean"},
    "suspend": {"type": "boolean"},
    "healthChecks": {"type": "array"},
    "healthCheckExprs": {"type": "array"},
    "patches": {"type": "array"},
    "images": {"type": "array"},
    "components": {"type": "array", "items": {"type": "string"}},
    "commonMetadata": {"type": "object"},
    "namePrefix": {"type": "string"},
    "nameSuffix": {"type": "string"},
    "kubeConfig": {"type": "object"},
    "serviceAccountName": {"type": "string"},
    "deletionPolicy": {"type": "string", "enum": ["MirrorPrune", "Delete", "WaitForTermination", "Orphan"]}
  }
}`, `"apiVersion", "kind", "metadata", "spec"`)

var bgpSchema = root("cilium.io/v2alpha1", "CiliumBGPPeeringPolicy",
	"Cilium BGP peering policy: which nodes peer with which routers. From the CRD schema cilium.io/v2alpha1.", `{
  "type": "object",
  "required": ["virtualRouters"],
  "additionalProperties": false,
  "properties": {
    "nodeSelector": {"type": "object", "description": "Nodes that use this policy", "x-pk-ui": {"label": "Nodes", "widget": "node-selector", "order": 1}, "properties": {
      "matchLabels": {"type": "object", "additionalProperties": {"type": "string"}},
      "matchExpressions": {"type": "array", "items": {"type": "object", "required": ["key", "operator"], "properties": {
        "key": {"type": "string"},
        "operator": {"type": "string", "enum": ["In", "NotIn", "Exists", "DoesNotExist"]},
        "values": {"type": "array", "items": {"type": "string", "x-pk-ref": {"kind": "Node"}}}
      }}}
    }},
    "virtualRouters": {"type": "array", "minItems": 1, "x-pk-ui": {"label": "Virtual routers", "order": 2}, "items": {"type": "object", "required": ["localASN", "neighbors"], "properties": {
      "localASN": {"type": "integer", "minimum": 1, "maximum": 4294967295, "x-pk-ui": {"label": "Local ASN"}},
      "exportPodCIDR": {"type": "boolean"},
      "podIPPoolSelector": {"type": "object"},
      "serviceSelector": {"type": "object"},
      "serviceAdvertisements": {"type": "array"},
      "neighbors": {"type": "array", "x-pk-ui": {"label": "BGP peers"}, "items": {"type": "object", "required": ["peerAddress", "peerASN"], "properties": {
        "peerAddress": {"type": "string", "pattern": "^[0-9a-fA-F.:]+/[0-9]{1,3}$", "x-pk-message": "must be an address with a prefix length, for example 10.6.0.1/32", "x-pk-ui": {"label": "Peer address"}},
        "peerASN": {"type": "integer", "minimum": 1, "maximum": 4294967295, "x-pk-ui": {"label": "Peer ASN"}},
        "peerPort": {"type": "integer", "minimum": 1, "maximum": 65535},
        "eBGPMultihopTTL": {"type": "integer", "minimum": 1, "maximum": 255},
        "connectRetryTimeSeconds": {"type": "integer", "minimum": 1},
        "holdTimeSeconds": {"type": "integer", "minimum": 3},
        "keepAliveTimeSeconds": {"type": "integer", "minimum": 1},
        "authSecretRef": {"type": "string"},
        "gracefulRestart": {"type": "object"},
        "families": {"type": "array"},
        "advertisedPathAttributes": {"type": "array"}
      }}}
    }}}
  }
}`, `"apiVersion", "kind", "metadata", "spec"`)

var deploymentSchema = root("apps/v1", "Deployment", "Kubernetes Deployment (light schema).", `{
  "type": "object",
  "required": ["selector", "template"],
  "properties": {
    "replicas": {"type": "integer", "minimum": 0, "x-pk-ui": {"label": "Replicas", "order": 1}},
    "selector": {"type": "object", "x-pk-ui": {"label": "Selector", "order": 2}},
    "strategy": {"type": "object"},
    "template": {"type": "object", "properties": {
      "metadata": {"type": "object", "x-pk-ui": {"label": "Pod metadata", "order": 5}},
      "spec": {"type": "object", "required": ["containers"], "properties": {
        "containers": {"type": "array", "minItems": 1, "x-pk-ui": {"label": "Containers", "order": 4}, "items": {"type": "object", "required": ["name", "image"], "properties": {
          "name": {"type": "string"},
          "image": {"type": "string", "pattern": "^[^\\s]+$", "x-pk-ui": {"label": "Image"}},
          "ports": {"type": "array", "items": {"type": "object", "properties": {"containerPort": {"type": "integer", "minimum": 1, "maximum": 65535}, "name": {"type": "string", "maxLength": 15}}}},
          "resources": {"type": "object"}
        }}}
      }}
    }}
  }
}`, `"apiVersion", "kind", "metadata", "spec"`)

var serviceSchema = root("v1", "Service", "Kubernetes Service (light schema).", `{
  "type": "object",
  "properties": {
    "type": {"type": "string", "enum": ["ClusterIP", "NodePort", "LoadBalancer", "ExternalName"]},
    "selector": {"type": "object", "additionalProperties": {"type": "string"}},
    "ports": {"type": "array", "items": {"type": "object", "required": ["port"], "properties": {
      "name": {"type": "string"},
      "port": {"type": "integer", "minimum": 1, "maximum": 65535},
      "targetPort": {"type": ["integer", "string"]},
      "protocol": {"type": "string", "enum": ["TCP", "UDP", "SCTP"]}
    }}}
  }
}`, `"apiVersion", "kind", "metadata"`)

var namespaceSchema = root("v1", "Namespace", "Kubernetes Namespace (light schema).", `{"type": "object"}`, `"apiVersion", "kind", "metadata"`)

var helmRepositorySchema = root("source.toolkit.fluxcd.io/v1", "HelmRepository", "Flux HelmRepository: a Helm chart repository.", `{
  "type": "object",
  "required": ["url"],
  "properties": {
    "url": {"type": "string", "pattern": "^(https?|oci)://", "x-pk-message": "must be an http, https or oci URL"},
    "interval": `+duration+`,
    "type": {"type": "string", "enum": ["default", "oci"]}
  }
}`, `"apiVersion", "kind", "metadata", "spec"`)

var gitRepositorySchema = root("source.toolkit.fluxcd.io/v1", "GitRepository", "Flux GitRepository: the state repo as a source.", `{
  "type": "object",
  "required": ["url"],
  "properties": {
    "url": {"type": "string"},
    "interval": `+duration+`,
    "ref": {"type": "object"}
  }
}`, `"apiVersion", "kind", "metadata", "spec"`)

const talosSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "title": "TalosConfig",
  "description": "talhelper configuration of a cluster (shared parts; nodes come from the inventory).",
  "type": "object",
  "x-pk-kind": {"apiVersion": "talhelper.siderolabs.dev/v1", "kind": "TalosConfig"},
  "required": ["clusterName", "talosVersion"],
  "properties": {
    "clusterName": {"type": "string", "x-pk-ref": {"kind": "Cluster"}, "x-pk-ui": {"label": "Cluster", "order": 1}},
    "talosVersion": {"type": "string", "pattern": "^v[0-9]+\\.[0-9]+\\.[0-9]+$", "x-pk-message": "must be a Talos version, for example v1.10.6", "x-pk-ui": {"label": "Talos", "order": 2}},
    "kubernetesVersion": {"type": "string", "pattern": "^v[0-9]+\\.[0-9]+\\.[0-9]+$", "x-pk-message": "must be a Kubernetes version, for example v1.33.4", "x-pk-ui": {"label": "Kubernetes", "order": 3}},
    "endpoint": {"type": "string", "format": "uri", "x-pk-ui": {"label": "Endpoint", "order": 4}},
    "nodes": {"type": "array"},
    "controlPlane": {"type": "object"},
    "worker": {"type": "object"},
    "patches": {"type": "array"}
  }
}`
