# Give every heading the anchor the generator links to.
#
# crd-ref-docs writes GitHub-style anchors, which drop punctuation:
# "metal.pk.io/v1alpha1" becomes #metalravenhpcgsidev1alpha1. Pandoc
# keeps periods, so the same heading renders as id="metal.pk.iov1alpha1"
# and every generated link to a heading with a dot in it — the API group headings —
# lands nowhere. Attaching the generator's own anchor as an explicit id makes the
# document agree with itself, for these links and any later ones.
BEGIN { fence = 0 }

# Headings inside fenced code are text, not headings.
/^```/ { fence = !fence }

!fence && /^#+[ \t]+/ {
    text = $0
    sub(/^#+[ \t]+/, "", text)
    # An explicit id already present wins — never fight the author.
    if (text ~ /\{#/) { print; next }

    id = tolower(text)
    gsub(/\t/, " ", id)
    gsub(/[^a-z0-9 _-]/, "", id)
    sub(/^ +/, "", id)
    sub(/ +$/, "", id)
    gsub(/ +/, "-", id)
    if (id == "") { print; next }

    # Two headings can share a name — one "Supporting types" per API group. Only
    # the first may claim the plain anchor; forcing it on both would emit
    # duplicate ids. Later ones fall back to Pandoc's own de-duplication, which
    # is safe because the generated links only ever point at type names, and
    # those are unique.
    if (id in seen) { print; next }
    seen[id] = 1

    print $0 " {#" id "}"
    next
}

{ print }
