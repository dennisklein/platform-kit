{{- /*
Vendored from crd-ref-docs' default markdown templates, with one change: the
resources come first, each as its own section, and everything else is gathered
under a heading below them.

Upstream renders every type at the same level in one alphabetical run, so a spec
struct, an enum and an actual resource look alike and sit next to each other.
Only the kinds are resources; the rest exist because some resource field has a
shape. Splitting them puts the things you can `kubectl get` in the sidebar and
keeps the several dozen supporting types out of it.
*/ -}}
{{- define "gvDetails" -}}
{{- $gv := . -}}

## {{ $gv.GroupVersionString }}

{{ $gv.Doc }}

{{- if $gv.Kinds }}
**Resources:** {{ range $i, $k := $gv.SortedKinds }}{{ if $i }} · {{ end }}{{ $gv.TypeForKind $k | markdownRenderTypeLink }}{{ end }}
{{ end }}

{{ range $gv.SortedKinds }}
{{ template "type" ($gv.TypeForKind .) }}
{{ end }}

### Supporting types

The shapes the resources above are made of. These are fields of a resource, not
resources themselves — nothing here is created or reconciled on its own.

{{ range $gv.SortedTypes }}
{{- if not .GVK }}
{{ template "type" . }}
{{- end }}
{{ end }}

{{- end -}}
