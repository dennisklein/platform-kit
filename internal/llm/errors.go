package llm

import "errors"

// ErrNoEmbeddings is returned by Embed without an embedding model.
var ErrNoEmbeddings = errors.New("llm: no embedding model configured")
